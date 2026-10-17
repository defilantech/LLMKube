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
	"errors"
	"fmt"
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
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
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
	// AllowedHostPathRoots is the operator-configured allowlist of absolute
	// path prefixes under which local (/abs and file://) model sources — and
	// therefore the HostPathVolumeSource they generate — are permitted. Empty
	// (the secure default) disables all local/hostPath sources; see
	// validateLocalSourceAllowed and GHSA-jw3m-8q7m-f35r.
	AllowedHostPathRoots []string
	// DefaultFSGroup is applied to the rendered PodSecurityContext when the
	// user has not supplied one. Values <= 0 disable the default (recommended
	// on OpenShift, where the restricted-v2 SCC injects fsGroup from the
	// namespace's allocated range). Set via --default-fsgroup; default 102.
	DefaultFSGroup int64
	// DRIRenderGID is the node-local GID of the DRI render group
	// (/dev/dri/renderD128) added to a Vulkan InferenceService pod's
	// supplementalGroups so the container can open the render node; without it
	// llama.cpp fails open and serves from CPU at roughly half speed with no
	// status condition (#1560). The GID is node-local and not knowable at
	// admission, so it comes from the operator's --dri-render-gid flag. Values
	// <= 0 disable the default (e.g. on OpenShift); the field applies only on
	// the Vulkan path and never to a user-supplied Spec.PodSecurityContext.
	// Set via --dri-render-gid; defaults to 0 (disabled) because `render` is
	// dynamically allocated and has no portable value (#1572).
	DRIRenderGID int64
	// EmitScrapeAnnotations, when true, adds Prometheus annotation-discovery
	// hints (prometheus.io/scrape, /path, /port) to every inference Pod, with
	// port set to the resolved endpoint port, so annotation-based scrapers
	// (e.g. Grafana Alloy) discover /metrics without a per-InferenceService
	// podAnnotations block or a PodMonitor. User-supplied podAnnotations win.
	// Set via --emit-scrape-annotations; default false.
	EmitScrapeAnnotations bool
	// HTTPClient overrides the HTTP client used for idle checks. When nil, a
	// default client with a 5-second timeout is created per request. Primarily
	// useful in tests to capture requests or control timeouts.
	HTTPClient *http.Client
	// RuntimeImageOverrides remaps a runtime backend's default image
	// fleet-wide (keys: llamacpp, vllm, sglang, tgi). Set via
	// --runtime-images (chart: runtimeImages.*) for air-gapped/mirrored
	// registries; wins over the built-in vendor divergences, loses to an
	// explicit spec.image. Empty means stock defaults.
	RuntimeImageOverrides map[string]string
	// RolloutIdleBaseURL overrides the base URL used for idle endpoint checks.
	// When non-empty, this URL is used directly instead of constructing a
	// cluster-local service DNS name. Primarily useful in tests where cluster
	// DNS is unavailable.
	RolloutIdleBaseURL string
	// GPUSharingSharedPool is the node selector for the shared-GPU pool
	// (time-sliced / co-located devices). Shared pods carry it so they never
	// land on exclusive nodes: both pool kinds advertise the same extended
	// resource (e.g. nvidia.com/gpu), so only the label separates them. Set
	// via --gpu-sharing-shared-pool-selector (chart: gpuSharing.pools.shared).
	// Empty means no shared pool exists and gpuSharing mode shared is
	// rejected at reconcile time.
	GPUSharingSharedPool map[string]string
	// PodLogReader reads a serving pod's container log so the operator can
	// surface the engine's offload result on status.acceleration (#1385).
	// Nil leaves status.acceleration unset (the read is advisory and best-
	// effort); the production operator wires a kubernetes-clientset-backed
	// reader.
	PodLogReader PodLogReader

	// RelayImage is the image for Metal relay pods (router-proxy in --relay
	// mode). Wired from --router-proxy-image; empty uses the router-proxy default.
	RelayImage string

	// ReconcileDecorator wraps the reconciler at the point SetupWithManager
	// hands it to the controller builder, so the wrapper is what the
	// controller calls. cmd/main.go sets it to the reconcile deadline; nil
	// leaves the reconcile path untouched.
	ReconcileDecorator func(reconcile.Reconciler) reconcile.Reconciler
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
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=core,resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=core,resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaimtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

func (r *InferenceServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reconcileStart := time.Now()
	inferenceService := &inferencev1alpha1.InferenceService{}
	// Assigned by getModelForInferenceService below; declared here so the
	// deferred publish can label the info series with the Model's accelerator.
	var model *inferencev1alpha1.Model
	defer func() {
		llmkubemetrics.ReconcileDuration.WithLabelValues("inferenceservice").Observe(time.Since(reconcileStart).Seconds())
		// Republish from observed state: a pass that returns before
		// updateStatusWithSchedulingInfo publishes nothing at the call site.
		// A live cluster hit this via #1225 — two Ready, serving services
		// failed reconcileDeployment every pass and exported no series at all.
		publishInferenceServiceState(inferenceService, model)
	}()

	log := logf.FromContext(ctx)

	if err := r.Get(ctx, req.NamespacedName, inferenceService); err != nil {
		if apierrors.IsNotFound(err) {
			llmkubemetrics.DeleteInferenceServiceSeries(req.Name, req.Namespace)
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

	draftModel, draftReady, draftResult, err := r.getDraftModelForInferenceService(ctx, inferenceService)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !draftReady {
		return *draftResult, nil
	}

	// Host-path allowlist gate (GHSA-jw3m-8q7m-f35r): a local model source
	// outside the operator-configured roots must never produce a
	// HostPathVolumeSource in a generated pod. Block the InferenceService
	// before any Deployment is built. This guards independently of the Model
	// controller's own gate because a Model may carry a sticky Ready status
	// from before the allowlist was introduced (or tightened).
	if valErr := validateLocalSourceAllowed(model.Spec.Source, r.AllowedHostPathRoots); valErr != nil {
		log.Error(valErr, "rejected local model source by host-path allowlist", "model", model.Name, "source", model.Spec.Source)
		blockResult, updateErr := r.updateStatusWithSchedulingInfo(ctx, inferenceService, PhaseFailed, modelReady, 0, inferenceService.Status.Replicas, 0, "",
			valErr.Error(), nil)
		return blockResult, updateErr
	}

	desiredReplicas := int32(1)
	if inferenceService.Spec.Replicas != nil {
		desiredReplicas = *inferenceService.Spec.Replicas
	}

	// Suspend (spec.suspend) scales the workload to zero without touching
	// spec.replicas, so unsuspend restores the user's desired count. The
	// forced zero flows through reconcileDeployment (Deployment replicas),
	// the metal-agent state, and status.desiredReplicas.
	if inferenceService.Spec.Suspend {
		desiredReplicas = 0
	}

	if modelNeedsCachePVC(model, inferenceService, r.ModelCachePath) {
		if err := r.ensureModelCachePVC(ctx, inferenceService); err != nil {
			log.Error(err, "Failed to ensure model cache PVC exists", "namespace", inferenceService.Namespace)
			return r.updateStatusWithSchedulingInfo(ctx, inferenceService, PhaseFailed, modelReady, 0, inferenceService.Status.Replicas, desiredReplicas, "",
				fmt.Sprintf("Failed to ensure model cache PVC: %v", err), nil)
		}
	}

	r.warnIgnoredModelCacheClaim(inferenceService, model)
	r.warnUnboundedEphemeralCache(inferenceService)

	isMetal := isMetalModel(model)

	r.emitSpecAdvisoryEvents(inferenceService, model)

	deployment, counts, metalSnap, result, err := r.reconcileWorkload(ctx, inferenceService, model, draftModel, desiredReplicas, modelReady, isMetal)
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

	// A name-taken refusal is soft on purpose: the HPA is optional, the
	// AutoscalingReady condition is persisted by the status write below, and
	// the refusal's requeue is merged into the final result at the end. A
	// real error still aborts the reconcile.
	hpaRequeue, err := r.reconcileHPA(ctx, inferenceService, inferenceService.Name, isMetal)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Post-hoc crash diagnosis before the status write persists conditions.
	// Metal services have no pods to diagnose (the agent runs the server
	// natively), so the pod scan is skipped there.
	if !isMetal {
		r.reconcileDriverCompatCondition(ctx, inferenceService)
		// Sibling diagnosis for the containers that stage weights. Scans init
		// containers only, as the driver diagnosis scans the runtime container
		// only, so one failure never collects two contradictory diagnoses.
		r.reconcileModelTransferCondition(ctx, inferenceService)
	}

	endpoint := r.constructEndpoint(inferenceService, service)
	phase, schedulingInfo := r.determinePhase(ctx, inferenceService, counts.Ready, desiredReplicas, isMetal, deployment, metalSnap)

	// Surface the engine's offload result before the status write persists it
	// with the phase. Deployment path only: the metal path runs the engine
	// natively with no serving pod to read.
	if !isMetal {
		r.reconcileAccelerationStatus(ctx, inferenceService)
	}

	// Clear the scheduling diagnosis this controller wrote once the service is
	// actually serving. determinePhase returns a nil SchedulingInfo at Ready,
	// and the status builder deliberately no-ops on nil so it cannot clobber
	// agent-written scheduling fields (#643). That preserve is correct for the
	// metal path, where the agent owns these fields and clears them itself
	// (#777), but it also froze the controller's own InsufficientGPU /
	// UnbindableModelCache: a recovered service kept advertising a resource it
	// was no longer waiting on (#1632). This block handles the deployment
	// path; the metal counterpart is clearControllerMetalScheduling.
	if !isMetal && phase == PhaseReady {
		inferenceService.Status.SchedulingStatus = ""
		inferenceService.Status.SchedulingMessage = ""
		inferenceService.Status.WaitingFor = ""
	}

	finalResult, statusErr := r.updateStatusWithSchedulingInfo(ctx, inferenceService, phase, modelReady, counts.Ready, counts.Observed, desiredReplicas, endpoint, "", schedulingInfo)
	if statusErr != nil {
		return finalResult, statusErr
	}

	lifetimeRequeue, err := r.reconcilePodLifetime(ctx, inferenceService, isMetal, time.Now())
	if err != nil {
		return finalResult, err
	}
	finalResult.RequeueAfter = earliestPositive(finalResult.RequeueAfter, lifetimeRequeue)
	finalResult.RequeueAfter = earliestPositive(finalResult.RequeueAfter, hpaRequeue)
	// A Ready multiNode group with a failure count comes back once it has
	// stayed Ready long enough for the count to be forgotten (#1856).
	finalResult.RequeueAfter = earliestPositive(finalResult.RequeueAfter, multiNodeRecreateResetRequeue(inferenceService, time.Now()))

	// When a rollout is deferred pending idle, reconcileRolloutPolicy set
	// RolloutDeferred=True (persisted by the status update above). Drive a
	// recheck so the controller notices when the backend goes idle or the
	// idleTimeoutSeconds budget is spent. Metal-backed services never defer
	// (no Deployment), so this does not conflict with the metal requeue below.
	if cond := meta.FindStatusCondition(inferenceService.Status.Conditions, ConditionRolloutDeferred); cond != nil && cond.Status == metav1.ConditionTrue {
		finalResult.RequeueAfter = earliestPositive(finalResult.RequeueAfter, inferencev1alpha1.DefaultIdleCheckInterval)
	}

	// On the metal path a heartbeat going stale generates no watch event, so
	// force a periodic requeue when the Endpoints carry the annotation.
	if isMetal {
		if requeue := metalHeartbeatRequeueDuration(metalSnap); requeue > 0 {
			finalResult.RequeueAfter = earliestPositive(finalResult.RequeueAfter, requeue)
		}
	}

	return finalResult, nil
}

// earliestPositive merges the requeue timers Reconcile collects without
// allowing a zero timer (which means "no requeue") to win a plain minimum.
func earliestPositive(values ...time.Duration) time.Duration {
	var earliest time.Duration
	for _, value := range values {
		if value > 0 && (earliest == 0 || value < earliest) {
			earliest = value
		}
	}
	return earliest
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
			result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, false, 0, isvc.Status.Replicas, 0, "", "Model not found", nil)
			return nil, false, &result, updateErr
		}
		log.Error(err, "Failed to get Model")
		return nil, false, nil, err
	}

	modelReady := model.Status.Phase == PhaseReady
	if !modelReady {
		log.Info("Model not ready yet", "model", model.Name, "phase", model.Status.Phase)
		result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, "Pending", false, 0, isvc.Status.Replicas, 0, "", "Waiting for Model to be Ready", nil)
		return nil, false, &result, updateErr
	}

	return model, modelReady, nil, nil
}

// draftModelRefFor returns the name of the draft Model this InferenceService
// actually resolves, or "" when it configures none (no speculativeDecoding
// block, a spec type that carries its own speculation, or an empty ref).
//
// Single source of truth for the resolver below and for the Model watch
// mapping. They MUST agree: the resolver fails closed on a draft that is not
// yet Ready, so a mapping that does not enqueue on that same draft leaves the
// service Pending until the next full resync.
func draftModelRefFor(isvc *inferencev1alpha1.InferenceService) string {
	if isvc == nil {
		return ""
	}
	sd := isvc.Spec.SpeculativeDecoding
	if sd == nil || !specTypeNeedsDraftWeights(sd.Type) {
		return ""
	}
	return sd.DraftModelRef
}

// getDraftModelForInferenceService resolves spec.speculativeDecoding.draftModelRef
// the same way getModelForInferenceService resolves spec.modelRef, and fails
// closed for the same reasons: a draft that is missing or not Ready must block
// serving rather than silently degrade to unspeculated decoding. Silent
// performance loss is the failure mode we are least able to notice; on GB10
// the difference is 18% of decode throughput (#1423).
//
// Returns (nil, true, nil, nil) when the service configures no draft weights,
// which is the common case.
func (r *InferenceServiceReconciler) getDraftModelForInferenceService(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService,
) (*inferencev1alpha1.Model, bool, *ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ref := draftModelRefFor(isvc)
	if ref == "" {
		return nil, true, nil, nil
	}

	draft := &inferencev1alpha1.Model{}
	name := types.NamespacedName{Name: ref, Namespace: isvc.Namespace}
	if err := r.Get(ctx, name, draft); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Referenced draft Model not found", "draftModel", ref)
			result, updateErr := r.updateStatusWithSchedulingInfo(
				ctx, isvc, PhaseFailed, false, 0, isvc.Status.Replicas, 0, "",
				fmt.Sprintf("Draft model %q not found", ref), nil)
			return nil, false, &result, updateErr
		}
		log.Error(err, "Failed to get draft Model")
		return nil, false, nil, err
	}

	if draft.Status.Phase != PhaseReady {
		log.Info("Draft Model not ready yet", "draftModel", draft.Name, "phase", draft.Status.Phase)
		result, updateErr := r.updateStatusWithSchedulingInfo(
			ctx, isvc, "Pending", false, 0, isvc.Status.Replicas, 0, "",
			fmt.Sprintf("Waiting for draft model %q to be Ready", draft.Name), nil)
		return nil, false, &result, updateErr
	}

	return draft, true, nil, nil
}

// reconcileMetal serves a Metal InferenceService. There is no inference
// Deployment: the host metal-agent runs the engine natively and registers
// EndpointSlices once the model is fetched and the server is healthy, so ready
// replicas come from those slices rather than desiredReplicas, otherwise Phase
// reports Ready before the agent has done anything (issue #374).
//
// In relay mode the agent exposes only its authenticated ingress
// ("<isvc>-agent") and traffic reaches it through the controller's relay
// Deployment, so a replica counts as ready only while the relay is available
// too. In legacy mode the agent owns "<isvc>"; any relay this InferenceService
// ran is torn down and "<isvc>" is left to the agent (hand-back).
//
// A relay write that loses an optimistic-lock race (the Deployment controller
// updated the object between our Get and Update) is not a failure: it returns
// a short requeue result with no error and no RelayReconcileFailed event, and
// the next reconcile works from the fresh object.
//
// A relay token Secret the controller did not create (no managed-by label)
// stops relay mode for the namespace: reconcileMetal records a
// RelaySecretNotManaged Warning Event and returns the
// *relaySecretNotManagedError before any relay Deployment is created or
// updated; reconcileDeployment turns it into status and a timed requeue.
func (r *InferenceServiceReconciler) reconcileMetal(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (int32, *metalSnapshot, *ctrl.Result, error) {
	mode, pin, err := r.decideMetalMode(ctx, isvc)
	if err != nil {
		r.relayEvent(isvc, corev1.EventTypeWarning, EventRelayReconcileFailed, "Deciding the metal serving mode failed: %v", err)
		return 0, nil, nil, err
	}

	if mode == metalModeRelay {
		snap := r.metalEndpointSnapshot(ctx, isvc, agentServiceName(isvc.Name))
		if snap.Kind == metalHBStale {
			r.withdrawStaleMetalEndpoints(ctx, snap.staleSlices)
		}
		available, err := r.reconcileRelay(ctx, isvc, pin)
		if apierrors.IsConflict(err) {
			logf.FromContext(ctx).V(1).Info("Relay object changed under the reconcile; requeueing", "reason", err.Error())
			return 0, nil, &ctrl.Result{RequeueAfter: relayConflictRequeueAfter}, nil
		}
		var unmanaged *relaySecretNotManagedError
		if errors.As(err, &unmanaged) {
			// Not a transient failure: the caller records it on status and
			// requeues on a timer instead of backing off on an error.
			r.relayEvent(isvc, corev1.EventTypeWarning, EventRelaySecretNotManaged, "%s", unmanaged.Error())
			return 0, nil, nil, err
		}
		if err != nil {
			r.relayEvent(isvc, corev1.EventTypeWarning, EventRelayReconcileFailed, "Reconciling the metal relay failed: %v", err)
			return 0, nil, nil, err
		}
		clearRelaySecretNotManaged(isvc)
		if !available {
			return 0, snap, nil, nil
		}
		return snap.ReadyReplicas, snap, nil, nil
	}

	if err := r.teardownRelay(ctx, isvc); err != nil {
		r.relayEvent(isvc, corev1.EventTypeWarning, EventRelayReconcileFailed, "Removing the metal relay failed: %v", err)
		return 0, nil, nil, err
	}
	clearRelaySecretNotManaged(isvc)
	snap := r.metalEndpointSnapshot(ctx, isvc, sanitizeDNSName(isvc.Name))
	if snap.Kind == metalHBStale {
		r.withdrawStaleMetalEndpoints(ctx, snap.staleSlices)
	}
	return snap.ReadyReplicas, snap, nil, nil
}

// clearRelaySecretNotManaged drops the RelaySecretNotManaged diagnosis this
// controller wrote once the relay Secret is usable again (or relay mode no
// longer applies). The status writer preserves scheduling fields when a pass
// returns no SchedulingInfo, which a Ready metal service does, so without
// this a recovered service would keep advertising the refusal.
func clearRelaySecretNotManaged(isvc *inferencev1alpha1.InferenceService) {
	if isvc.Status.SchedulingStatus != inferencev1alpha1.ReasonRelaySecretNotManaged {
		return
	}
	isvc.Status.SchedulingStatus = ""
	isvc.Status.SchedulingMessage = ""
	isvc.Status.WaitingFor = ""
}

// replicaCounts carries the two replica observations a reconcile feeds the
// status writer: Ready gates the phase, Observed is what /scale must expose so
// an HPA reads a truthful current count.
type replicaCounts struct {
	Ready    int32
	Observed int32
}

// deploymentNameTakenRequeueAfter is the bounded requeue for a name-taken
// refusal. The refusal depends on cluster state outside the service's spec:
// nothing maps a foreign Deployment's events to this reconcile (Owns maps
// only owned objects), so deleting or renaming the foreign object takes
// effect only on this timer, without an unrelated event. Same cadence as the
// router-proxy and HPA refusals
// (routerProxyNameTakenRequeueAfter, hpaNameTakenRequeueAfter).
const deploymentNameTakenRequeueAfter = 2 * time.Minute

// deploymentNameTakenError reports a Deployment whose generated name is held
// by an existing object this InferenceService does not control.
// reconcileDeployment turns it into the DeploymentNameTaken scheduling status
// and a bounded requeue instead of a reconcile error.
type deploymentNameTakenError struct {
	ns, name, owner string
	// previous is set when the controller is an InferenceService with this
	// service's name but another UID: a deleted predecessor whose Deployment
	// garbage collection has not removed yet (a GitOps delete-and-recreate).
	previous bool
}

func (e *deploymentNameTakenError) Error() string {
	if e.previous {
		return fmt.Sprintf("Deployment %s/%s is still controlled by a previous InferenceService named %s (uid %s) that was deleted; "+
			"it is removed when garbage collection finishes, and this service is reconciled again then", e.ns, e.name, e.name, e.owner)
	}
	if e.owner != "" {
		return fmt.Sprintf("Deployment %s/%s is already controlled by another InferenceService (uid %s); "+
			"refusing to overwrite or recreate it; rename one of the services", e.ns, e.name, e.owner)
	}
	return fmt.Sprintf("Deployment %s/%s already exists and is not controlled by this InferenceService; "+
		"refusing to overwrite or recreate it (LLMKube did not create it); delete or rename the object, or the service",
		e.ns, e.name)
}

// ensureDeploymentOwned is the ownership gate for the workload Deployment
// writes in reconcileDeployment: the selector-migration delete-and-recreate
// and the in-place spec replace. Write only what this InferenceService
// controls: a Deployment with the generated name but no controller reference,
// a foreign reference, or another InferenceService's reference is refused,
// never adopted, never overwritten, and never deleted. A refused service
// reports Failed, or Suspended/Stopped when the service is deliberately off,
// with the DeploymentNameTaken marker; the multiNode and metal paths run no
// Deployment and clear a stale marker instead. The relay Deployment
// (teardownRelay) and the router-proxy Deployment carry their own gates; the
// Service upsert in reconcileService is not gated yet (#2086).
func ensureDeploymentOwned(isvc *inferencev1alpha1.InferenceService, existing *appsv1.Deployment) error {
	if metav1.IsControlledBy(existing, isvc) {
		return nil
	}
	taken := &deploymentNameTakenError{ns: existing.GetNamespace(), name: existing.GetName()}
	if ref := metav1.GetControllerOf(existing); ref != nil && ref.Kind == "InferenceService" {
		// The owner ref's APIVersion is "<group>/<version>" (or bare version
		// for the core group); match the group so a same-Kind controller in
		// another API group is not misreported as another InferenceService.
		if strings.SplitN(ref.APIVersion, "/", 2)[0] == inferencev1alpha1.GroupVersion.Group {
			taken.owner = string(ref.UID)
			taken.previous = ref.Name == isvc.Name
		}
	}
	return taken
}

// clearDeploymentNameTaken drops the DeploymentNameTaken diagnosis this
// controller wrote once the name is usable again. The status writer preserves
// scheduling fields when a pass returns no SchedulingInfo, so without this a
// recovered service would keep advertising the refusal.
func clearDeploymentNameTaken(isvc *inferencev1alpha1.InferenceService) {
	if isvc.Status.SchedulingStatus != inferencev1alpha1.ReasonDeploymentNameTaken {
		return
	}
	isvc.Status.SchedulingStatus = ""
	isvc.Status.SchedulingMessage = ""
	isvc.Status.WaitingFor = ""
}

// refuseDeploymentNameTaken records a name-taken refusal and returns the stop
// result every refusal path returns: a Warning event once per episode, the
// marker on the scheduling status, and the bounded requeue. The reported phase
// follows the precedence determinePhase uses: a suspended or scaled-to-zero
// service is deliberately off, so the refusal reports Suspended or Stopped
// rather than overriding that intent with Failed; the marker bites on
// unsuspend or scale-up.
//
// The sequence stops here, so reconcileHPA would never run: an HPA this
// service owns would keep scaling whatever holds the generated name, which
// after the refusal is the foreign object. Delete it (the #2010 gate keeps
// HPAs at this name operator-owned or refused, never adopted) and report the
// removal on AutoscalingReady=False; a foreign HPA is left alone and reported
// as HPANameTaken, since reconcileHPA cannot report it itself. The multiNode
// path never reaches this helper (it runs no Deployment); its own HPA-target
// gap is tracked in #2094.
func (r *InferenceServiceReconciler) refuseDeploymentNameTaken(ctx context.Context, isvc *inferencev1alpha1.InferenceService, nameTakenPreviously bool, modelReady bool, desiredReplicas int32, taken error) (*ctrl.Result, error) {
	log := logf.FromContext(ctx)
	// The event fires before the status write, so "once per episode" assumes
	// that write persisted; a write conflict re-fires the event on the next
	// bounded requeue for the same episode.
	log.Info("Refusing to overwrite or recreate a Deployment this InferenceService does not control", "error", taken)
	if !nameTakenPreviously && r.Recorder != nil {
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning,
			inferencev1alpha1.ReasonDeploymentNameTaken, "Reconcile", "%s", taken.Error())
	}

	if isvc.Spec.Autoscaling != nil {
		hpa := &autoscalingv2.HorizontalPodAutoscaler{}
		getErr := r.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace}, hpa)
		switch {
		case getErr == nil && metav1.IsControlledBy(hpa, isvc):
			if delErr := r.Delete(ctx, hpa); delErr != nil && !apierrors.IsNotFound(delErr) {
				// Transient: the bounded requeue brings the next pass, which
				// retries the delete. The refusal below still lands, and the
				// HPA's own condition stays until the delete lands.
				log.Error(delErr, "Failed to delete the operator-owned HPA scaling a refused name", "hpa", isvc.Name)
			} else {
				setHPARemovedForNameTaken(isvc)
			}
		case getErr == nil:
			// A foreign HPA holds the generated name. Report it the way
			// reconcileHPA would have (#2010): this refusal stops the
			// sequence before reconcileHPA runs, so the condition would
			// otherwise stay stale or absent for the whole episode. The HPA
			// object itself is left alone.
			if takenHPA := ensureHPAOwned(isvc, hpa); takenHPA != nil {
				setHPANameTakenCondition(isvc, takenHPA)
			}
		case apierrors.IsNotFound(getErr):
			setHPARemovedForNameTaken(isvc)
		case getErr != nil:
			// The HPA's state cannot be verified or corrected this pass, so
			// a prior True/HPAManaged must not stand: report the unknown
			// state and let the bounded requeue retry the read.
			log.Error(getErr, "Failed to get the HPA while refusing a name-taken Deployment", "hpa", isvc.Name)
			meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
				Type:               ConditionAutoscalingReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: isvc.Generation,
				Reason:             ReasonHPAStateUnknown,
				Message:            fmt.Sprintf("could not read HorizontalPodAutoscaler %s/%s while a foreign Deployment holds the generated name; its state is unknown and the reconcile will retry", isvc.Namespace, isvc.Name),
			})
		}
	} else {
		// The refusal stops the sequence before reconcileHPA, which owns the
		// condition's removal when autoscaling is not configured; do it here
		// so a mode switch while the name is taken cannot strand a stale
		// AutoscalingReady.
		removeAutoscalingReadyCondition(isvc)
	}

	// Ready is forced to zero (this service has no workload of its own to
	// count) while Observed preserves the last persisted count rather than
	// reading the foreign object's status; see updateStatusWithSchedulingInfo.
	phase := PhaseFailed
	if isvc.Spec.Suspend {
		phase = PhaseSuspended
	} else if desiredReplicas == 0 {
		phase = PhaseStopped
	}
	res, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, phase, modelReady,
		0, isvc.Status.Replicas, desiredReplicas, "", taken.Error(),
		&SchedulingInfo{Status: inferencev1alpha1.ReasonDeploymentNameTaken, Message: taken.Error()})
	res.RequeueAfter = earliestPositive(res.RequeueAfter, deploymentNameTakenRequeueAfter)
	return &res, updateErr
}

// reconcileDeployment coordinates several independent concerns (metal snapshot,
// rollout-policy idle gating, HPA replica ownership, and retry-on-conflict update
// for concurrent ModelPool replica scaling), which keeps it over the gocyclo
// threshold; splitting it further would fragment one linear reconcile step.
//
//nolint:gocyclo
func (r *InferenceServiceReconciler) reconcileDeployment(ctx context.Context, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, draftModel *inferencev1alpha1.Model, desiredReplicas int32, modelReady bool, isMetal bool) (*appsv1.Deployment, replicaCounts, *metalSnapshot, *ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// A previous pass may have reported a Deployment name-taken refusal.
	// Captured before anything can change it so the Warning event fires once
	// per episode, not once per bounded requeue. The marker itself is cleared
	// only where the name's true state is known: on the metal path (no
	// Deployment is wanted) and just before the Deployment Get below, whose
	// guard re-sets it when the object at the name is foreign. Clearing only
	// at those points keeps the marker visible across the fatal spec-fault
	// returns between here and the Get, which persist whatever scheduling
	// fields the in-memory object carries (#643).
	nameTakenPreviously := isvc.Status.SchedulingStatus == inferencev1alpha1.ReasonDeploymentNameTaken

	if isMetal {
		// A metal service wants no Deployment, so a marker left by a
		// deployment-path pass is stale; drop it before the metal status
		// writes persist it.
		clearDeploymentNameTaken(isvc)
		ready, snap, result, err := r.reconcileMetal(ctx, isvc)
		var unmanaged *relaySecretNotManagedError
		if errors.As(err, &unmanaged) {
			log.Info("Relay token Secret was not created by the controller; not serving through a relay", "secret", unmanaged.key.String())
			res, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseCreating, modelReady, 0, 0, desiredReplicas, isvc.Status.Endpoint, "",
				&SchedulingInfo{Status: inferencev1alpha1.ReasonRelaySecretNotManaged, Message: unmanaged.Error()})
			res.RequeueAfter = earliestPositive(res.RequeueAfter, relaySecretNotManagedRequeueAfter)
			return nil, replicaCounts{}, nil, &res, updateErr
		}
		if err != nil || result != nil {
			return nil, replicaCounts{}, nil, result, err
		}
		log.Info("Metal accelerator detected, skipping inference Deployment creation",
			"readyEndpoints", ready, "desiredReplicas", desiredReplicas)
		return nil, replicaCounts{Ready: ready, Observed: ready}, snap, nil, nil
	}

	// A metal-agent-only runtime off Metal is fatal: resolveBackend would
	// build a llama.cpp Deployment for a CR that asked for something else.
	// The validating webhook rejects this at admission when enabled; this
	// backstop covers default installs.
	if err := validateRuntimePlacement(isvc, model); err != nil {
		log.Info("Rejecting InferenceService with a metal-only runtime off Metal", "reason", err.Error())
		result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, isvc.Status.Replicas, desiredReplicas, "", fmt.Sprintf("Invalid runtime: %v", err), nil)
		return nil, replicaCounts{}, nil, &result, updateErr
	}

	// Surface non-fatal runtime spec problems as a status condition before we
	// build the Deployment. A failure here never blocks reconciliation — the
	// Deployment is still produced with the offending flags silently skipped
	// (see VLLMBackend.BuildArgs, SGLangBackend.BuildArgs).
	r.reconcileVLLMSpecCondition(isvc)
	r.reconcileSGLangSpecCondition(isvc)

	// gpuSharing, by contrast, is fatal when invalid or unsatisfiable:
	// building the Deployment anyway would either request an extended
	// resource no node advertises (pod Pending forever) or land a shared
	// workload on an exclusive node. Fail the reconcile with an actionable
	// message; admission-time rejection is the follow-up (#1196 story 5).
	if _, err := resolveGPUSharing(isvc, model, r.GPUSharingSharedPool); err != nil {
		log.Info("Rejecting InferenceService with invalid gpuSharing spec", "reason", err.Error())
		result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, isvc.Status.Replicas, desiredReplicas, "", fmt.Sprintf("Invalid gpuSharing: %v", err), nil)
		return nil, replicaCounts{}, nil, &result, updateErr
	}

	// Explicit parallelism that cannot fit the pod's GPUs is equally fatal:
	// the engine crash-loops probing for devices that do not exist (#1320).
	// The quota webhook rejects this at admission when multitenancy is
	// enabled; this backstop covers default installs.
	if err := parallelismExceedsGPUCount(isvc, model); err != nil {
		log.Info("Rejecting InferenceService with unsatisfiable parallelism", "reason", err.Error())
		result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, isvc.Status.Replicas, desiredReplicas, "", fmt.Sprintf("Invalid parallelism: %v", err), nil)
		return nil, replicaCounts{}, nil, &result, updateErr
	}

	// The HF mirror host, resolved per reconcile from each Model's
	// sourceSecretRef (#1900). A Model on the host named by HF_ENDPOINT gets
	// the bearer token; a nil draftModel (the helper is nil-safe) names none.
	hfEndpoint := hfEndpointFromSecret(ctx, r.Client, model)
	// A runtime that downloads the Model itself gets the mirror too, unless
	// its token lives in another Secret; say so rather than silently going to
	// huggingface.co.
	if _, conflict := runtimeHFMirrorEnv(resolveBackend(isvc), isvc, model, hfEndpoint); conflict && r.Recorder != nil {
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, ReasonHFMirrorNotApplied, "Reconcile",
			"%s", hfMirrorNotAppliedMessage(model))
	}
	deployment := r.constructDeployment(isvc, model, draftModel, desiredReplicas,
		hfEndpoint, hfEndpointFromSecret(ctx, r.Client, draftModel))
	if err := setControllerReferenceUnblocked(isvc, deployment, r.Scheme); err != nil {
		log.Error(err, "Failed to set controller reference for Deployment")
		return nil, replicaCounts{}, nil, nil, err
	}

	caMissing, err := r.reconcileCACertConfigMap(ctx, isvc, &deployment.Spec.Template.Spec)
	if err != nil {
		return nil, replicaCounts{}, nil, nil, err
	}

	// The Get below decides the name's state: drop a stale marker here so
	// the create, migration, and in-place paths all persist it cleared, and
	// the guard re-sets it when the object at the name is foreign.
	clearDeploymentNameTaken(isvc)

	existingDeployment := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, existingDeployment)
	if err != nil && apierrors.IsNotFound(err) {
		// Hold the create rather than schedule a pod that can never mount
		// its CA volume and would sit on a GPU in ContainerCreating. An
		// existing Deployment is left to reconcile normally: its running
		// pods already have the volume, and the condition explains any
		// replacement pod that wedges.
		if caMissing && desiredReplicas > 0 {
			log.Info("CA ConfigMap missing in the service namespace; not creating the Deployment",
				"configMap", r.CACertConfigMap, "namespace", isvc.Namespace)
			result, updateErr := r.waitForCACertConfigMap(ctx, isvc, modelReady, desiredReplicas)
			return nil, replicaCounts{}, nil, result, updateErr
		}
		log.Info("Creating new Deployment", "name", deployment.Name)
		// Stamp desired-template hash on new deployment for change detection.
		if deployment.Annotations == nil {
			deployment.Annotations = make(map[string]string)
		}
		tmplHash := desiredTemplateHash(deployment.Spec.Template)
		deployment.Annotations[AnnotationDesiredTemplateHash] = tmplHash
		if err := r.Create(ctx, deployment); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// A foreign object took the name between the Get above and
				// this Create: the same race the gate closes, reached from
				// the other side. Route it through the refusal instead of
				// wedging on an error no requeue would clear (nothing maps
				// the foreign object's events here). If the object at the
				// name is this service's own (a concurrent reconcile created
				// it), return it and let the next pass take the update path.
				raced := &appsv1.Deployment{}
				if getErr := r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, raced); getErr != nil {
					return nil, replicaCounts{}, nil, nil, getErr
				}
				if guardErr := ensureDeploymentOwned(isvc, raced); guardErr != nil {
					res, updateErr := r.refuseDeploymentNameTaken(ctx, isvc, nameTakenPreviously, modelReady, desiredReplicas, guardErr)
					return nil, replicaCounts{}, nil, res, updateErr
				}
				// Ours: a concurrent reconcile created it; return the object
				// that actually exists so the next pass takes the update path.
				return raced, replicaCounts{}, nil, nil, nil
			}
			log.Error(err, "Failed to create Deployment")
			result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, isvc.Status.Replicas, desiredReplicas, "", "Failed to create Deployment", nil)
			return nil, replicaCounts{}, nil, &result, updateErr
		}
		return deployment, replicaCounts{}, nil, nil, nil
	} else if err != nil {
		log.Error(err, "Failed to get Deployment")
		return nil, replicaCounts{}, nil, nil, err
	}

	// The gate sits above the selector-migration branch so one check covers
	// both writes: the in-place spec replace below and the delete-and-recreate
	// migrate path. A refusal must precede every write to the existing object.
	if err := ensureDeploymentOwned(isvc, existingDeployment); err != nil {
		res, updateErr := r.refuseDeploymentNameTaken(ctx, isvc, nameTakenPreviously, modelReady, desiredReplicas, err)
		return nil, replicaCounts{}, nil, res, updateErr
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
			return nil, replicaCounts{}, nil, nil, err
		}
		if err := r.Create(ctx, deployment); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// The old Deployment is still terminating; recreate on the
				// next reconcile once its deletion has propagated.
				return nil, replicaCounts{}, nil, &ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			log.Error(err, "Failed to recreate Deployment after selector change")
			return nil, replicaCounts{}, nil, nil, err
		}
		return deployment, replicaCounts{}, nil, nil, nil
	}

	// Snapshot externally-set template metadata before the wholesale
	// Spec replace; restore it afterward so sidecar-injector
	// annotations, `kubectl rollout restart`'s restartedAt, and GitOps
	// sync labels survive operator reconciles. Operator-owned keys
	// still win on collision. Same fix as the router-proxy reconciler
	// in router_deployment_builder.go; see #456.
	existingTemplateLabels := existingDeployment.Spec.Template.Labels
	existingTemplateAnnotations := existingDeployment.Spec.Template.Annotations

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

	// Use annotation-based hash comparison to detect real changes. A missing
	// stored hash (a legacy Deployment that predates the annotation) is
	// treated as changed: draining unnecessarily costs a little latency, while
	// skipping a needed drain drops in-flight requests. After the first
	// reconcile the annotation is stamped and the hash path takes over.
	desiredHash := desiredTemplateHash(desiredPodTemplate)
	storedHash := ""
	if existingDeployment.Annotations != nil {
		storedHash = existingDeployment.Annotations[AnnotationDesiredTemplateHash]
	}
	templateChanged := true
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
		rollResult, err := r.reconcileRolloutPolicy(ctx, isvc, svc)
		if err != nil {
			log.Error(err, "Failed to reconcile rollout policy")
			return nil, replicaCounts{}, nil, nil, err
		}
		if rollResult.RequeueAfter > 0 {
			// Rollout deferred until idle: hold the pod-template Update so the
			// live Deployment keeps serving the old template, but fall through
			// to Service/HPA/status by returning without a result. The
			// RolloutDeferred=True condition (set by reconcileRolloutPolicy)
			// drives a requeue in Reconcile.
			return existingDeployment, replicaCounts{
				Ready:    existingDeployment.Status.ReadyReplicas,
				Observed: existingDeployment.Status.Replicas,
			}, nil, nil, nil
		}
	} else if !isMetal && !templateChanged {
		// Template no longer differs — clear any stale RolloutDeferred condition.
		if meta.FindStatusCondition(isvc.Status.Conditions, ConditionRolloutDeferred) != nil {
			meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionRolloutDeferred)
			if updateErr := r.Status().Update(ctx, isvc); updateErr != nil {
				log.Error(updateErr, "Failed to clear stale RolloutDeferred condition")
				return nil, replicaCounts{}, nil, nil, updateErr
			}
		}
	}

	// Apply the desired spec onto the live Deployment and Update. A ModelPool
	// can be scaling this InferenceService's replicas concurrently, which
	// advances the Deployment's resourceVersion between our Get and Update and
	// would otherwise surface as spurious "Failed to update Deployment"
	// optimistic-lock conflicts during a swap. Retry on conflict with a fresh
	// read so the swap-driven replica churn is absorbed silently. Each attempt
	// re-derives the preserved external template metadata and the HPA-owned
	// replica count from the current object so the retry stays self-consistent.
	readyReplicas := existingDeployment.Status.ReadyReplicas
	observedReplicas := existingDeployment.Status.Replicas
	updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		liveTemplateLabels := existingDeployment.Spec.Template.Labels
		liveTemplateAnnotations := existingDeployment.Spec.Template.Annotations
		liveReplicas := existingDeployment.Spec.Replicas

		existingDeployment.Spec = deployment.Spec
		existingDeployment.Spec.Template.Labels = mergePreservingExternal(
			liveTemplateLabels, deployment.Spec.Template.Labels)
		existingDeployment.Spec.Template.Annotations = mergePreservingExternal(
			liveTemplateAnnotations, deployment.Spec.Template.Annotations)
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
		// the HPA on every reconcile. Suspension overrides this: the HPA is
		// deleted while suspended, and the forced zero must land.
		if isvc.Spec.Autoscaling != nil && !isvc.Spec.Suspend {
			existingDeployment.Spec.Replicas = liveReplicas
		}
		if err := r.Update(ctx, existingDeployment); err != nil {
			if apierrors.IsConflict(err) {
				// Re-read the newer object for the next attempt.
				if getErr := r.Get(ctx, types.NamespacedName{
					Name: deployment.Name, Namespace: deployment.Namespace,
				}, existingDeployment); getErr != nil {
					return getErr
				}
			}
			return err
		}
		readyReplicas = existingDeployment.Status.ReadyReplicas
		observedReplicas = existingDeployment.Status.Replicas
		return nil
	})
	if updateErr != nil {
		log.Error(updateErr, "Failed to update Deployment")
		return nil, replicaCounts{}, nil, nil, updateErr
	}

	return deployment, replicaCounts{Ready: readyReplicas, Observed: observedReplicas}, nil, nil, nil
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
			result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, isvc.Status.Replicas, desiredReplicas, "", "Failed to create Service", nil)
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
		// Empty is safe to send: the apiserver re-defaults it to Cluster on
		// NodePort/LoadBalancer, so removing the field reverts to the default.
		existingService.Spec.ExternalTrafficPolicy = service.Spec.ExternalTrafficPolicy
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
	// staleSlices holds the listed slices when Kind == metalHBStale so the
	// reconciler can withdraw their endpoints without a second List.
	staleSlices []discoveryv1.EndpointSlice
}

// metalEndpointSnapshot lists the EndpointSlices of the Service name (the
// InferenceService's own "<isvc>" in legacy mode, "<isvc>-agent" in relay
// mode) and returns a metalSnapshot summarising both the ready-replica count
// and the heartbeat state. It is the single source of truth for the metal path: it makes the
// one List call, and metalHeartbeatRequeueDuration then derives the requeue
// interval from the snapshot it returns without calling the API again.
//
// Slices are listed by the well-known kubernetes.io/service-name label rather
// than fetched by name because there can be more than one: the metal-agent
// produces one directly, and Kubernetes' EndpointSliceMirroring controller may
// produce another from a legacy Endpoints object during a mid-upgrade window.
// When multiple slices carry a heartbeat annotation we classify against the
// freshest one. Mirrored slices have no heartbeat annotation, so an upgrade
// window degrades gracefully to the legacy-exempt path.
func (r *InferenceServiceReconciler) metalEndpointSnapshot(ctx context.Context, isvc *inferencev1alpha1.InferenceService, name string) *metalSnapshot {
	log := logf.FromContext(ctx)
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
			return &metalSnapshot{Kind: metalHBStale, RawHeartbeat: rawHeartbeat, staleSlices: slices.Items}
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

// withdrawStaleMetalEndpoints marks the endpoints of metal-agent slices whose
// heartbeat has expired as not ready and not serving, so kube-proxy stops
// routing Service traffic to a host that stopped re-asserting them (agent dead,
// Mac asleep or offline). Status already treats a stale heartbeat as zero
// ready replicas; without this the slice itself kept Ready=true and direct
// Service clients were sent to a closed port indefinitely (#1918).
//
// Only a slice that carries the metal-agent managed-by label and its own
// expired heartbeat is touched; mirrored or foreign slices are left alone.
// The patch carries an optimistic lock: the agent re-registers with a full
// Update that refreshes the heartbeat, so if it came back between our List and
// this Patch the write fails with a conflict instead of clobbering its
// Ready=true, and the next reconcile sees a fresh heartbeat. Once withdrawn,
// the slice is skipped, so the EndpointSlice watch this write triggers does
// not loop. Failures are logged, not returned: status is already correct and
// the stale-heartbeat requeue retries.
func (r *InferenceServiceReconciler) withdrawStaleMetalEndpoints(ctx context.Context, slices []discoveryv1.EndpointSlice) {
	log := logf.FromContext(ctx)
	for i := range slices {
		slice := &slices[i]
		if !metalSliceNeedsWithdrawal(slice, time.Now()) {
			continue
		}
		patch := client.MergeFromWithOptions(slice.DeepCopy(), client.MergeFromWithOptimisticLock{})
		for j := range slice.Endpoints {
			slice.Endpoints[j].Conditions.Ready = ptr.To(false)
			slice.Endpoints[j].Conditions.Serving = ptr.To(false)
		}
		if err := r.Patch(ctx, slice, patch); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				log.V(1).Info("Metal EndpointSlice changed before stale withdrawal; re-evaluating next reconcile",
					"endpointSlice", slice.Name, "reason", err.Error())
				continue
			}
			log.Error(err, "Failed to withdraw stale metal EndpointSlice", "endpointSlice", slice.Name)
			continue
		}
		log.Info("Withdrew stale metal EndpointSlice endpoints so kube-proxy stops routing to the host",
			"endpointSlice", slice.Name)
	}
}

// metalSliceNeedsWithdrawal reports whether slice is an agent-managed metal
// slice whose own heartbeat has expired while some endpoint still advertises
// ready or serving (a nil condition counts as true, per EndpointSlice
// convention).
func metalSliceNeedsWithdrawal(slice *discoveryv1.EndpointSlice, now time.Time) bool {
	if slice.Labels["llmkube.ai/managed-by"] != "metal-agent" {
		return false
	}
	ts, err := time.Parse(time.RFC3339, slice.Annotations[inferencev1alpha1.AnnotationAgentHeartbeat])
	if err != nil || now.Sub(ts) <= inferencev1alpha1.DefaultAgentHeartbeatTimeout {
		return false
	}
	for _, ep := range slice.Endpoints {
		c := ep.Conditions
		if c.Ready == nil || *c.Ready || c.Serving == nil || *c.Serving {
			return true
		}
	}
	return false
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
	if !isHFRepoSource(model.Spec.Source) || hasMultiFileStaging(model) {
		return false
	}
	return !needsSkipModelInit(isvc)
}

// emitSpecAdvisoryEvents emits the non-blocking spec-hygiene warnings in one
// place: config that reconciliation tolerates but that predictably surprises
// the operator later (host-RAM surprises, redundant init downloads, a
// cpu-offload flag with an open upstream reliability report). Extracted from
// Reconcile so each new advisory does not grow its cyclomatic complexity.
func (r *InferenceServiceReconciler) emitSpecAdvisoryEvents(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) {
	if r.Recorder == nil {
		return
	}
	if needsOffloadMemoryWarning(isvc) {
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, "MissingMemoryRequest", "Reconcile",
			"CPU/KV offloading is enabled but resources.memory/hostMemory is not set; hybrid pods consume significant host RAM")
	}
	if shouldWarnMissingSkipModelInit(model, isvc) {
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, "MissingSkipModelInit", "Reconcile",
			"Model source is a HuggingFace repo ID (resolved by the runtime at startup); set spec.skipModelInit=true so the init container does not run")
	}
	if needsCPUOffloadNoopWarning(isvc) {
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, "CPUOffloadUnverified", "Reconcile",
			cpuOffloadIneffectiveMessage)
	}
}

// InferenceServiceReconcileConcurrency is how many InferenceServices
// reconcile at once. The controller-runtime default is 1, which makes a
// single stuck reconcile a head-of-line block for every InferenceService in
// the cluster: in the #2069 stall, one wedged reconcile froze all of them
// behind the single worker while the pod stayed healthy.
//
// Safe to raise here: controller-runtime never reconciles the same object
// concurrently, and InferenceServiceReconciler carries no mutable shared
// state (its fields are configuration set once at startup; a package-wide
// audit of its reconcile-path methods found no receiver-field writes).
const InferenceServiceReconcileConcurrency = 4

func (r *InferenceServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.InferenceService{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: InferenceServiceReconcileConcurrency}).
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
		)
	// A service blocked on a missing CA ConfigMap recovers on the ConfigMap's
	// own create event instead of a timer. The ModelRouter controller already
	// owns ConfigMaps, so this shares an informer the manager runs anyway.
	if r.CACertConfigMap != "" {
		b = b.Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.findInferenceServicesForCACertConfigMap),
		)
	}
	rec := reconcile.Reconciler(r)
	if r.ReconcileDecorator != nil {
		rec = r.ReconcileDecorator(rec)
	}
	return b.Named("inferenceservice").Complete(rec)
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
	reqs := []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: name, Namespace: obj.GetNamespace()},
	}}
	// A relay-mode agent writes "<isvc>-agent"; also enqueue "<isvc>". The
	// unstripped name stays enqueued because an InferenceService may itself be
	// named "x-agent" with a legacy slice, and reconciling a name that has no
	// InferenceService is a no-op.
	if labels["llmkube.ai/managed-by"] == "metal-agent" {
		if isvc, ok := strings.CutSuffix(svcName, inferencev1alpha1.MetalAgentServiceSuffix); ok && isvc != "" {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvc, Namespace: obj.GetNamespace()},
			})
		}
	}
	return reqs
}

// reconcileWorkload picks the serving shape. A spec.multiNode group replaces
// the Deployment: one pinned pod per member, owned by the service, recreated
// as a unit (#1423). Everything else is the Deployment (or, on Metal, the
// agent-owned endpoint) path.
func (r *InferenceServiceReconciler) reconcileWorkload(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	model, draftModel *inferencev1alpha1.Model,
	desiredReplicas int32,
	modelReady, isMetal bool,
) (*appsv1.Deployment, replicaCounts, *metalSnapshot, *ctrl.Result, error) {
	// A relay only serves Metal. After a model switch off Metal (for
	// example to CUDA) the "<isvc>-relay" Deployment would otherwise linger.
	if !isMetal {
		if err := r.teardownRelay(ctx, isvc); err != nil {
			return nil, replicaCounts{}, nil, nil, err
		}
	}
	if isvc.Spec.MultiNode != nil {
		// A multiNode group runs no Deployment, so a marker left by a
		// deployment-path pass is stale; drop it before the group's status
		// writes persist it.
		clearDeploymentNameTaken(isvc)
		readyReplicas, result, err := r.reconcileMultiNodeGroup(ctx, isvc, model, draftModel, desiredReplicas, modelReady)
		// A multiNode group has no Deployment to read an observed total from,
		// so the ready member count stands in for both.
		return nil, replicaCounts{Ready: readyReplicas, Observed: readyReplicas}, nil, result, err
	}
	return r.reconcileDeployment(ctx, isvc, model, draftModel, desiredReplicas, modelReady, isMetal)
}

func (r *InferenceServiceReconciler) findInferenceServiceForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	pod := obj.(*corev1.Pod)

	serviceName, ok := pod.Labels["inference.llmkube.dev/service"]
	if !ok {
		// Multi-node workers drop the service label so the Service never
		// selects them; the group label still maps them to their owner.
		serviceName, ok = pod.Labels[LabelMultiNodeGroup]
	}
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
		// The draft ref matters as much as the target ref: the draft gate in
		// getDraftModelForInferenceService fails closed, so a service whose
		// draft is still downloading is Pending until something enqueues it.
		// Without this arm nothing does, and the ~10h default resync is the
		// only thing that eventually would (#1528).
		if isvc.Spec.ModelRef == model.Name || draftModelRefFor(&isvc) == model.Name {
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
