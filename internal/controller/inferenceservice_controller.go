/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"net/http"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

type InferenceServiceReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	Recorder             events.EventRecorder
	ModelCachePath       string
	ModelCacheSize       string
	ModelCacheClass      string
	ModelCacheAccessMode string
	// ModelCacheMode selects how model cache PVCs are provisioned:
	// ModelCacheModeShared (default) uses the single cluster-wide
	// llmkube-model-cache PVC that the operator mounts and all InferenceServices
	// share (cross-isvc dedup, cache list works; needs an RWX class on multi-node
	// clusters); ModelCacheModePerService gives each InferenceService its own RWO,
	// WaitForFirstConsumer PVC that binds on the serving node (#728), the opt-in
	// escape hatch for multi-node clusters without RWX. An empty value is treated
	// as shared (see resolveCacheMode).
	ModelCacheMode     string
	CACertConfigMap    string
	InitContainerImage string
	// DefaultFSGroup is applied to the rendered PodSecurityContext when the
	// user has not supplied one. Values <= 0 disable the default (recommended
	// on OpenShift, where the restricted-v2 SCC injects fsGroup from the
	// namespace's allocated range). Set via --default-fsgroup; default 102.
	DefaultFSGroup int64
	// HTTPClient overrides the HTTP client used for idle checks. When nil, a
	// default client with a 5-second timeout is created per request. Primarily
	// useful in tests to capture requests or control timeouts.
	HTTPClient *http.Client
	// RolloutIdleBaseURL overrides the base URL used for idle endpoint checks.
	// When non-empty, this URL is used directly instead of constructing a
	// cluster-local service DNS name. Primarily useful in tests where cluster
	// DNS is unavailable.
	RolloutIdleBaseURL string
}

func sanitizeDNSName(name string) string {
	return strings.ReplaceAll(name, ".", "-")
}

func boolPtr(b bool) *bool { return &b }

// int64Ptr returns a pointer to the given int64 value. Used to construct
// corev1 fields that take *int64 (e.g. SecurityContext.RunAsUser,
// PodSecurityContext.FSGroup) from a plain int64 literal.
func int64Ptr(v int64) *int64 { return &v }

// initContainerSecurityContext returns the SecurityContext applied to the
// model downloader init container. It inherits runAsUser/runAsGroup from the
// pod-level Spec.PodSecurityContext when the user supplied one. The default
// FSGroup on the pod (see inferPodSecurityContext) is sufficient for the
// standard curlimages/curl init image; explicit RunAsUser is only needed if
// the operator overrides --init-container-image with one whose default user
// differs from curl_user.
func initContainerSecurityContext(isvc *inferencev1alpha1.InferenceService) *corev1.SecurityContext {
	sc := &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}

	// Inherit runAsUser/runAsGroup from podSecurityContext if specified
	if isvc != nil && isvc.Spec.PodSecurityContext != nil {
		if isvc.Spec.PodSecurityContext.RunAsUser != nil {
			sc.RunAsUser = isvc.Spec.PodSecurityContext.RunAsUser
		}
		if isvc.Spec.PodSecurityContext.RunAsGroup != nil {
			sc.RunAsGroup = isvc.Spec.PodSecurityContext.RunAsGroup
		}
	}

	return sc
}

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaimtemplates,verbs=get;list;watch

func (r *InferenceServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reconcileStart := time.Now()
	defer func() {
		llmkubemetrics.ReconcileDuration.WithLabelValues("inferenceservice").Observe(time.Since(reconcileStart).Seconds())
	}()

	log := logf.FromContext(ctx)

	inferenceService := &inferencev1alpha1.InferenceService{}
	if err := r.Get(ctx, req.NamespacedName, inferenceService); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get InferenceService")
		return ctrl.Result{}, err
	}

	model, modelReady, result, err := r.getModelForInferenceService(ctx, inferenceService)
	if err != nil || result != nil {
		if result != nil {
			return *result, err
		}
		return ctrl.Result{}, err
	}

	desiredReplicas := int32(1)
	if inferenceService.Spec.Replicas != nil {
		desiredReplicas = *inferenceService.Spec.Replicas
	}

	if model.Status.CacheKey != "" && r.ModelCachePath != "" {
		if err := r.ensureModelCachePVC(ctx, inferenceService); err != nil {
			log.Error(err, "Failed to ensure model cache PVC exists", "namespace", inferenceService.Namespace)
			return r.updateStatusWithSchedulingInfo(ctx, inferenceService, PhaseFailed, modelReady, 0, desiredReplicas, "", "Failed to create model cache PVC", nil)
		}
	}

	isMetal := isMetalModel(model)

	if r.Recorder != nil && needsOffloadMemoryWarning(inferenceService) {
		r.Recorder.Eventf(inferenceService, nil, corev1.EventTypeWarning, "MissingMemoryRequest", "Reconcile",
			"CPU/KV offloading is enabled but resources.memory/hostMemory is not set; hybrid pods consume significant host RAM")
	}

	if r.Recorder != nil && shouldWarnMissingSkipModelInit(model, inferenceService) {
		r.Recorder.Eventf(inferenceService, nil, corev1.EventTypeWarning, "MissingSkipModelInit", "Reconcile",
			"Model source is a HuggingFace repo ID (resolved by the runtime at startup); set spec.skipModelInit=true so the init container does not run")
	}

	deployment, readyReplicas, metalSnap, result, err := r.reconcileDeployment(ctx, inferenceService, model, desiredReplicas, modelReady, isMetal)
	if err != nil || result != nil {
		if result != nil {
			return *result, err
		}
		return ctrl.Result{}, err
	}

	service, result, err := r.reconcileService(ctx, inferenceService, modelReady, desiredReplicas, isMetal)
	if err != nil || result != nil {
		if result != nil {
			return *result, err
		}
		return ctrl.Result{}, err
	}

	if err := r.reconcileHPA(ctx, inferenceService, inferenceService.Name, isMetal); err != nil {
		return ctrl.Result{}, err
	}

	endpoint := r.constructEndpoint(inferenceService, service)
	phase, schedulingInfo := r.determinePhase(ctx, inferenceService, readyReplicas, desiredReplicas, isMetal, deployment, metalSnap)

	finalResult, statusErr := r.updateStatusWithSchedulingInfo(ctx, inferenceService, phase, modelReady, readyReplicas, desiredReplicas, endpoint, "", schedulingInfo)
	if statusErr != nil {
		return finalResult, statusErr
	}

	// On the metal path a heartbeat going stale generates no watch event, so
	// force a periodic requeue when the Endpoints carry the annotation.
	if isMetal {
		if requeue := metalHeartbeatRequeueDuration(metalSnap); requeue > 0 {
			finalResult.RequeueAfter = requeue
		}
	}

	return finalResult, nil
}

func (r *InferenceServiceReconciler) getModelForInferenceService(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (*inferencev1alpha1.Model, bool, *ctrl.Result, error) {
	log := logf.FromContext(ctx)

	model := &inferencev1alpha1.Model{}
	modelName := types.NamespacedName{
		Name:      isvc.Spec.ModelRef,
		Namespace: isvc.Namespace,
	}
	if err := r.Get(ctx, modelName, model); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Referenced Model not found", "model", isvc.Spec.ModelRef)
			result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, false, 0, 0, "", "Model not found", nil)
			return nil, false, &result, updateErr
		}
		log.Error(err, "Failed to get Model")
		return nil, false, nil, err
	}

	modelReady := model.Status.Phase == PhaseReady
	if !modelReady {
		log.Info("Model not ready yet", "model", model.Name, "phase", model.Status.Phase)
		result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, "Pending", false, 0, 0, "", "Waiting for Model to be Ready", nil)
		return nil, false, &result, updateErr
	}

	return model, modelReady, nil, nil
}

func (r *InferenceServiceReconciler) reconcileDeployment(ctx context.Context, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, desiredReplicas int32, modelReady bool, isMetal bool) (*appsv1.Deployment, int32, *metalSnapshot, *ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if isMetal {
		// No Deployment for metal: the host metal-agent runs llama-server natively
		// and registers the InferenceService's Endpoints once the model is fetched
		// and the server is healthy. Derive readyReplicas from the Endpoints rather
		// than blindly returning desiredReplicas, otherwise Phase reports Ready
		// before the agent has done anything (issue #374).
		snap := r.metalEndpointSnapshot(ctx, isvc)
		log.Info("Metal accelerator detected, skipping Deployment creation",
			"readyEndpoints", snap.ReadyReplicas, "desiredReplicas", desiredReplicas)
		return nil, snap.ReadyReplicas, snap, nil, nil
	}

	// Surface non-fatal vLLM spec problems as a status condition before we
	// build the Deployment. A failure here never blocks reconciliation — the
	// Deployment is still produced with the offending flags silently skipped
	// (see VLLMBackend.BuildArgs).
	r.reconcileVLLMSpecCondition(isvc)

	deployment := r.constructDeployment(isvc, model, desiredReplicas)
	if err := setControllerReferenceUnblocked(isvc, deployment, r.Scheme); err != nil {
		log.Error(err, "Failed to set controller reference for Deployment")
		return nil, 0, nil, nil, err
	}

	existingDeployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, existingDeployment)
	if err != nil && apierrors.IsNotFound(err) {
		log.Info("Creating new Deployment", "name", deployment.Name)
		// Stamp desired-template hash on new deployment for change detection.
		if deployment.Annotations == nil {
			deployment.Annotations = make(map[string]string)
		}
		tmplHash := desiredTemplateHash(deployment.Spec.Template)
		deployment.Annotations[AnnotationDesiredTemplateHash] = tmplHash
		if err := r.Create(ctx, deployment); err != nil {
			log.Error(err, "Failed to create Deployment")
			result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, desiredReplicas, "", "Failed to create Deployment", nil)
			return nil, 0, nil, &result, updateErr
		}
		return deployment, 0, nil, nil, nil
	} else if err != nil {
		log.Error(err, "Failed to get Deployment")
		return nil, 0, nil, nil, err
	}

	// Deployment.spec.selector is immutable. A Deployment created by an older
	// operator version can carry a smaller selector than we now generate
	// (pre-0.8 used {app: <name>}; we now also add inference.llmkube.dev/service).
	// An in-place Update then fails permanently with "field is immutable" and
	// the controller hot-loops (#606). Migrate by recreating the Deployment
	// with the correct selector. This is a one-time, logged recreate; the
	// brief pod churn is unavoidable for an immutable-selector change.
	if !apiequality.Semantic.DeepEqual(existingDeployment.Spec.Selector, deployment.Spec.Selector) {
		log.Info("Deployment selector changed; recreating (selector is immutable)",
			"name", deployment.Name,
			"oldSelector", existingDeployment.Spec.Selector,
			"newSelector", deployment.Spec.Selector)
		if err := r.Delete(ctx, existingDeployment); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to delete stale Deployment for selector migration")
			return nil, 0, nil, nil, err
		}
		if err := r.Create(ctx, deployment); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// The old Deployment is still terminating; recreate on the
				// next reconcile once its deletion has propagated.
				return nil, 0, nil, &ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			log.Error(err, "Failed to recreate Deployment after selector change")
			return nil, 0, nil, nil, err
		}
		return deployment, 0, nil, nil, nil
	}

	// Snapshot externally-set template metadata before the wholesale
	// Spec replace; restore it afterward so sidecar-injector
	// annotations, `kubectl rollout restart`'s restartedAt, and GitOps
	// sync labels survive operator reconciles. Operator-owned keys
	// still win on collision. Same fix as the router-proxy reconciler
	// in router_deployment_builder.go; see #456.
	existingTemplateLabels := existingDeployment.Spec.Template.Labels
	existingTemplateAnnotations := existingDeployment.Spec.Template.Annotations
	existingReplicas := existingDeployment.Spec.Replicas

	// Compute the desired merged template so we can compare against the
	// live Deployment before gating on idle checks.
	desiredTemplateLabels := mergePreservingExternal(
		existingTemplateLabels,
		deployment.Spec.Template.Labels,
	)
	desiredTemplateAnnotations := mergePreservingExternal(
		existingTemplateAnnotations,
		deployment.Spec.Template.Annotations,
	)

	// Check RolloutPolicy before applying pod-template update. Only gate
	// when the pod template (including merged labels/annotations) actually
	// differs from the live Deployment — otherwise we'd block every reconcile
	// on a service with waitForIdle even though nothing needs updating.
	desiredPodTemplate := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      desiredTemplateLabels,
			Annotations: desiredTemplateAnnotations,
		},
		Spec: deployment.Spec.Template.Spec,
	}

	// Use annotation-based hash comparison to avoid false positives from
	// API-server-applied defaults that differ between in-memory and persisted objects.
	desiredHash := desiredTemplateHash(desiredPodTemplate)
	storedHash := ""
	if existingDeployment.Annotations != nil {
		storedHash = existingDeployment.Annotations[AnnotationDesiredTemplateHash]
	}
	templateChanged := podTemplatesDiffer(existingDeployment.Spec.Template, desiredPodTemplate)
	if storedHash != "" {
		templateChanged = desiredHash != storedHash
	}

	if !isMetal && templateChanged {
		svcName := sanitizeDNSName(isvc.Name)
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      svcName,
				Namespace: isvc.Namespace,
			},
		}
		if rollResult, err := r.reconcileRolloutPolicy(ctx, isvc, svc); err != nil {
			log.Error(err, "Failed to reconcile rollout policy")
			return nil, 0, nil, nil, err
		} else if rollResult.RequeueAfter > 0 {
			return existingDeployment, existingDeployment.Status.ReadyReplicas, nil, &rollResult, nil
		}
	} else if !isMetal && !templateChanged {
		// Template no longer differs — clear any stale RolloutDeferred condition.
		if meta.FindStatusCondition(isvc.Status.Conditions, ConditionRolloutDeferred) != nil {
			meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionRolloutDeferred)
			if updateErr := r.Status().Update(ctx, isvc); updateErr != nil {
				log.Error(updateErr, "Failed to clear stale RolloutDeferred condition")
				return nil, 0, nil, nil, updateErr
			}
		}
	}

	existingDeployment.Spec = deployment.Spec
	existingDeployment.Spec.Template.Labels = desiredTemplateLabels
	existingDeployment.Spec.Template.Annotations = desiredTemplateAnnotations
	// Stamp the desired-template hash so subsequent reconciles can detect
	// real changes without false positives from API-server defaulting.
	if existingDeployment.Annotations == nil {
		existingDeployment.Annotations = make(map[string]string)
	}
	existingDeployment.Annotations[AnnotationDesiredTemplateHash] = desiredHash
	// When autoscaling is enabled the HPA owns the replica count: preserve
	// the live value rather than overwriting it with the operator's desired
	// count. Setting it to nil does not work here: a plain Update with a nil
	// replicas field is defaulted back to 1 by the API server, which fights
	// the HPA on every reconcile.
	if isvc.Spec.Autoscaling != nil {
		existingDeployment.Spec.Replicas = existingReplicas
	}
	if err := r.Update(ctx, existingDeployment); err != nil {
		log.Error(err, "Failed to update Deployment")
		return nil, 0, nil, nil, err
	}

	return deployment, existingDeployment.Status.ReadyReplicas, nil, nil, nil
}

func (r *InferenceServiceReconciler) reconcileService(ctx context.Context, isvc *inferencev1alpha1.InferenceService, modelReady bool, desiredReplicas int32, isMetal bool) (*corev1.Service, *ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if isMetal {
		log.Info("Metal accelerator detected, skipping Service creation (managed by Metal Agent)")
		// Return a minimal Service object so constructEndpoint can still build
		// the endpoint URL from the sanitized name.
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sanitizeDNSName(isvc.Name),
				Namespace: isvc.Namespace,
			},
		}, nil, nil
	}

	service := r.constructService(isvc)
	if err := setControllerReferenceUnblocked(isvc, service, r.Scheme); err != nil {
		log.Error(err, "Failed to set controller reference for Service")
		return nil, nil, err
	}

	existingService := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, existingService)
	if err != nil && apierrors.IsNotFound(err) {
		log.Info("Creating new Service", "name", service.Name)
		if err := r.Create(ctx, service); err != nil {
			log.Error(err, "Failed to create Service")
			result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, desiredReplicas, "", "Failed to create Service", nil)
			return nil, &result, updateErr
		}
	} else if err != nil {
		log.Error(err, "Failed to get Service")
		return nil, nil, err
	} else {
		// Service exists — sync mutable fields from the desired spec.
		// Preserve the allocated nodePort when the type is not changing,
		// and always preserve ClusterIP (immutable).
		existingService.Spec.Type = service.Spec.Type
		existingService.Spec.Ports = service.Spec.Ports
		existingService.Spec.Selector = service.Spec.Selector
		existingService.Labels = service.Labels
		if err := r.Update(ctx, existingService); err != nil {
			log.Error(err, "Failed to update Service")
			return nil, nil, err
		}
	}

	return service, nil, nil
}

// metalHeartbeatKind classifies the heartbeat annotation on a metal
// EndpointSlice so that callers can produce appropriately differentiated status
// messages without re-listing the slices.
type metalHeartbeatKind int

const (
	// metalHBNone: no EndpointSlice exists yet (agent has not registered).
	metalHBNone metalHeartbeatKind = iota
	// metalHBLegacy: slices exist but carry no heartbeat annotation (older agent).
	metalHBLegacy
	// metalHBFresh: slices exist with a current, valid heartbeat.
	metalHBFresh
	// metalHBStale: slices exist but the heartbeat timestamp has expired.
	metalHBStale
	// metalHBUnparseable: slices exist but the heartbeat annotation cannot be parsed.
	metalHBUnparseable
)

// metalSnapshot captures the result of a single EndpointSlice list for the
// metal path. It is produced by metalEndpointSnapshot and consumed by
// determinePhase and metalHeartbeatRequeueDuration, eliminating the second Get
// that the old metalHeartbeatRequeue performed.
type metalSnapshot struct {
	ReadyReplicas int32
	Kind          metalHeartbeatKind
	// RawHeartbeat is the annotation value verbatim (empty when Kind == metalHBNone).
	RawHeartbeat string
	// ParseErr is set when Kind == metalHBUnparseable.
	ParseErr error
}

// metalEndpointSnapshot lists the EndpointSlices for isvc and returns a
// metalSnapshot summarising both the ready-replica count and the heartbeat
// state. It is the single source of truth for the metal path; both
// metalReadyEndpoints and metalHeartbeatRequeueDuration are thin wrappers
// around it.
//
// Slices are listed by the well-known kubernetes.io/service-name label rather
// than fetched by name because there can be more than one: the metal-agent
// produces one directly, and Kubernetes' EndpointSliceMirroring controller may
// produce another from a legacy Endpoints object during a mid-upgrade window.
// When multiple slices carry a heartbeat annotation we classify against the
// freshest one. Mirrored slices have no heartbeat annotation, so an upgrade
// window degrades gracefully to the legacy-exempt path.
func (r *InferenceServiceReconciler) metalEndpointSnapshot(ctx context.Context, isvc *inferencev1alpha1.InferenceService) *metalSnapshot {
	log := logf.FromContext(ctx)
	name := sanitizeDNSName(isvc.Name)
	slices := &discoveryv1.EndpointSliceList{}
	err := r.List(ctx, slices,
		client.InNamespace(isvc.Namespace),
		client.MatchingLabels{"kubernetes.io/service-name": name},
	)
	if err != nil {
		log.Error(err, "Failed to list EndpointSlices for metal accelerator", "name", name)
		return &metalSnapshot{Kind: metalHBNone}
	}
	if len(slices.Items) == 0 {
		return &metalSnapshot{Kind: metalHBNone}
	}

	// Pick the freshest heartbeat across all slices. A slice without the
	// annotation contributes to the count but not to heartbeat freshness.
	var (
		rawHeartbeat   string
		hasAnnotation  bool
		freshestTS     time.Time
		freshestParsed bool
		parseErr       error
		parseErrRaw    string
	)
	for i := range slices.Items {
		raw, ok := slices.Items[i].Annotations[inferencev1alpha1.AnnotationAgentHeartbeat]
		if !ok {
			continue
		}
		hasAnnotation = true
		ts, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			// Remember the first unparseable value but keep scanning: a
			// sibling slice may carry a parseable, fresher heartbeat.
			if parseErr == nil {
				parseErr = perr
				parseErrRaw = raw
			}
			continue
		}
		if !freshestParsed || ts.After(freshestTS) {
			freshestTS = ts
			freshestParsed = true
			rawHeartbeat = raw
		}
	}

	switch {
	case freshestParsed:
		// At least one parseable heartbeat: classify against the freshest.
		age := time.Since(freshestTS)
		if age > inferencev1alpha1.DefaultAgentHeartbeatTimeout {
			log.Info("Metal endpoint heartbeat stale; treating as not ready",
				"name", name, "heartbeat", rawHeartbeat, "age", age.Round(time.Second), "timeout", inferencev1alpha1.DefaultAgentHeartbeatTimeout)
			return &metalSnapshot{Kind: metalHBStale, RawHeartbeat: rawHeartbeat}
		}
	case hasAnnotation:
		// Every heartbeat annotation present was unparseable.
		log.Info("Metal endpoint heartbeat annotation unparseable; treating as not ready",
			"name", name, "heartbeat", parseErrRaw, "parseError", parseErr)
		return &metalSnapshot{Kind: metalHBUnparseable, RawHeartbeat: parseErrRaw, ParseErr: parseErr}
	}
	// No annotation on any slice: legacy agent without heartbeats; fall
	// through and count.

	var ready int32
	for i := range slices.Items {
		for j := range slices.Items[i].Endpoints {
			cond := slices.Items[i].Endpoints[j].Conditions.Ready
			if cond == nil || *cond {
				// Ready==nil is treated as ready, matching the EndpointSlice
				// convention that an absent condition means "ready".
				ready += int32(len(slices.Items[i].Endpoints[j].Addresses)) //nolint:gosec // one address per metal endpoint; bounded
			}
		}
	}

	kind := metalHBLegacy
	if hasAnnotation {
		kind = metalHBFresh
	}
	return &metalSnapshot{ReadyReplicas: ready, Kind: kind, RawHeartbeat: rawHeartbeat}
}

// metalReadyEndpoints is a convenience wrapper that returns only the
// ready-replica count for callers that do not need the full snapshot (e.g.,
// unit tests). Production reconcile paths use metalEndpointSnapshot directly.
func (r *InferenceServiceReconciler) metalReadyEndpoints(ctx context.Context, isvc *inferencev1alpha1.InferenceService) int32 {
	return r.metalEndpointSnapshot(ctx, isvc).ReadyReplicas
}

// metalHeartbeatRequeueDuration returns the RequeueAfter duration to use after
// a successful metal reconcile. It operates on the already-fetched metalSnapshot
// so no additional API call is needed.
//
// When the slices carry a heartbeat annotation (fresh or stale) the
// reconciler must periodically re-check staleness because no watch event fires
// when a heartbeat simply ages out. We return half the timeout so we notice
// expiry within one extra interval. Legacy agents (no annotation) and
// not-yet-registered services (no slices) need no forced requeue.
func metalHeartbeatRequeueDuration(snap *metalSnapshot) time.Duration {
	if snap == nil {
		return 0
	}
	switch snap.Kind {
	case metalHBFresh, metalHBStale:
		return inferencev1alpha1.DefaultAgentHeartbeatTimeout / 2
	default:
		return 0
	}
}

func needsSkipModelInit(isvc *inferencev1alpha1.InferenceService) bool {
	return isvc.Spec.SkipModelInit != nil && *isvc.Spec.SkipModelInit
}

// shouldWarnMissingSkipModelInit reports whether the InferenceService should
// receive a `MissingSkipModelInit` warning event. The warning fires when:
//
//   - The Model is Ready (so we know its source type matters), AND
//   - The Model's source is resolved entirely by the runtime — currently
//     only HuggingFace repo IDs, which vLLM/llama.cpp fetch directly via
//     HF_TOKEN at workload startup, AND
//   - The InferenceService still has the init container enabled
//     (skipModelInit is unset or false).
//
// In that combination the init container has nothing to fetch and the Pod
// will fail to start. HTTP(S) sources also have Status.Path == "" after
// issue #363 — the controller defers their fetch to the workload init
// container — but those DO need the init container (it is exactly how the
// per-namespace cache PVC gets populated), so the warning must NOT fire for
// them.
func shouldWarnMissingSkipModelInit(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService) bool {
	if model.Status.Phase != PhaseReady {
		return false
	}
	if !isHFRepoSource(model.Spec.Source) {
		return false
	}
	return !needsSkipModelInit(isvc)
}

func (r *InferenceServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.InferenceService{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.findInferenceServiceForPod),
		).
		Watches(
			&inferencev1alpha1.Model{},
			handler.EnqueueRequestsFromMapFunc(r.findInferenceServicesForModel),
		).
		Watches(
			&discoveryv1.EndpointSlice{},
			handler.EnqueueRequestsFromMapFunc(r.findInferenceServiceForEndpoints),
		).
		Named("inferenceservice").
		Complete(r)
}

// findInferenceServiceForEndpoints enqueues the InferenceService that an
// EndpointSlice belongs to. On the Metal path there is no Deployment or Pod, so
// the Pod watch never fires; the metal-agent signals readiness by creating an
// EndpointSlice named after the InferenceService. Without this watch a Metal
// service stays Creating until the next periodic resync.
//
// The InferenceService name is derived from the kubernetes.io/service-name
// label (== sanitizeDNSName(isvc.Name)) rather than the slice's own name,
// because a slice produced by the EndpointSliceMirroring controller during an
// upgrade window has a generated name (<service>-<hash>) and only carries the
// service-name label, not our managed-by label. Either of those labels is
// enough to recognise a metal slice and route it back to its service.
func (r *InferenceServiceReconciler) findInferenceServiceForEndpoints(ctx context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	svcName := labels["kubernetes.io/service-name"]
	if labels["llmkube.ai/managed-by"] != "metal-agent" && svcName == "" {
		return nil
	}
	// Prefer the service-name label (covers both our own and mirrored slices);
	// fall back to the object name for an agent slice that predates the label.
	name := svcName
	if name == "" {
		name = obj.GetName()
	}
	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Name:      name,
				Namespace: obj.GetNamespace(),
			},
		},
	}
}

func (r *InferenceServiceReconciler) findInferenceServiceForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	pod := obj.(*corev1.Pod)

	serviceName, ok := pod.Labels["inference.llmkube.dev/service"]
	if !ok {
		return nil
	}

	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Name:      serviceName,
				Namespace: pod.Namespace,
			},
		},
	}
}

func (r *InferenceServiceReconciler) findInferenceServicesForModel(ctx context.Context, obj client.Object) []reconcile.Request {
	model := obj.(*inferencev1alpha1.Model)

	inferenceServiceList := &inferencev1alpha1.InferenceServiceList{}
	if err := r.List(ctx, inferenceServiceList, client.InNamespace(model.Namespace)); err != nil {
		return []reconcile.Request{}
	}

	var requests []reconcile.Request
	for _, isvc := range inferenceServiceList.Items {
		if isvc.Spec.ModelRef == model.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      isvc.Name,
					Namespace: isvc.Namespace,
				},
			})
		}
	}

	return requests
}
