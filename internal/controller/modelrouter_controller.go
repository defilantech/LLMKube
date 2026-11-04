/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"fmt"
	"math"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

// Phase and condition constants specific to ModelRouter. Names that collide
// with model_controller.go (PhaseReady, PhaseFailed, ConditionAvailable,
// ConditionDegraded) are reused from there; only the new ones are declared
// here.
const (
	ModelRouterPhasePending      = "Pending"
	ModelRouterPhaseProvisioning = "Provisioning"
	ModelRouterPhaseDegraded     = "Degraded"

	ModelRouterConditionValidated       = "Validated"
	ModelRouterConditionBackendsReady   = "BackendsReady"
	ModelRouterConditionBudgetReporting = "BudgetReporting"

	ModelRouterReasonSpecValid   = "SpecValid"
	ModelRouterReasonSpecInvalid = "SpecInvalid"
	// ModelRouterReasonClassifierNotImplemented marks a Proxy router whose
	// policy.classification.mode asks for a content classifier (detector,
	// hybrid) the router-proxy does not have yet (#441). It replaces
	// SpecInvalid only when that is the spec's sole problem.
	ModelRouterReasonClassifierNotImplemented = "ClassifierNotImplemented"
	ModelRouterReasonCompileFailed            = "CompileFailed"
	ModelRouterReasonReconcileFailed          = "ReconcileFailed"
	// ModelRouterReasonRouterProxyNameTaken marks a router whose generated
	// router-proxy name is held by an object LLMKube did not create (any of
	// the seven proxy objects: ConfigMap, Deployment, both Services,
	// ServiceAccount, Role, RoleBinding). Adopting such an object would
	// overwrite the owner's spec and, via the controller reference, delete it
	// when this router goes away.
	ModelRouterReasonRouterProxyNameTaken = "RouterProxyNameTaken"
	ModelRouterReasonBackendsResolved     = "BackendsResolved"
	ModelRouterReasonBackendsUnresolved   = "BackendsUnresolved"
	ModelRouterReasonDeploymentReady      = "DeploymentReady"
	ModelRouterReasonDeploymentNotReady   = "DeploymentNotReady"
	ModelRouterReasonDeploymentDegraded   = "DeploymentDegraded"
	ModelRouterReasonBudgetReported       = "BudgetReported"
	ModelRouterReasonBudgetUnreachable    = "BudgetUnreachable"

	modelRouterControllerName = "modelrouter"
)

// ModelRouterReconciler reconciles the ModelRouter CRD. It validates the
// spec, compiles the routing config, and creates / updates the ConfigMap,
// Deployment, and Service that constitute the data plane.
type ModelRouterReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Recorder surfaces configuration that is accepted but ignored, such as
	// session affinity on a RoundRobin backend, as Kubernetes Events. Optional.
	Recorder events.EventRecorder

	// RouterProxyImage is the default container image for the
	// router-proxy. Per-ModelRouter overrides in spec.proxy.image take
	// precedence. Wired in cmd/main.go from --router-proxy-image.
	RouterProxyImage string

	// DefaultLiteLLMURL is the cluster-wide default LiteLLM proxy URL
	// the reconciler injects into External backends whose
	// provider=litellm and URL is empty. Operators set this once via
	// --default-litellm-url so application teams can declare
	// `external: {provider: litellm, model: ...}` without repeating
	// the LiteLLM Service URL on every ModelRouter. Empty string
	// means "no default — the user must specify URL explicitly".
	DefaultLiteLLMURL string

	// EmitScrapeAnnotations, when true, adds the prometheus.io/scrape, /path
	// and /port annotations to router-proxy pods, pointing at the proxy's
	// metrics listener, as InferenceServiceReconciler does for inference pods.
	// Set via --emit-scrape-annotations; default false.
	EmitScrapeAnnotations bool

	// BudgetStatusURL overrides how the reconciler derives the proxy's budget
	// admin URL from a ModelRouter. Nil means routerProxyBudgetEndpoint, the
	// in-cluster Service address. Tests inject it to point at a stub proxy.
	BudgetStatusURL func(*inferencev1alpha1.ModelRouter) string
}

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=modelrouters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=modelrouters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=modelrouters/finalizers,verbs=update
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=modelpools,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

func (r *ModelRouterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reconcileStart := time.Now()
	defer func() {
		llmkubemetrics.ReconcileDuration.WithLabelValues(modelRouterControllerName).Observe(time.Since(reconcileStart).Seconds())
	}()

	logger := log.FromContext(ctx)
	logger.Info("Reconciling ModelRouter", "name", req.Name, "namespace", req.Namespace)

	mr := &inferencev1alpha1.ModelRouter{}
	if err := r.Get(ctx, req.NamespacedName, mr); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to get ModelRouter")
		return ctrl.Result{}, err
	}

	valErrors := validateModelRouter(mr)
	if len(valErrors) > 0 {
		return ctrl.Result{}, r.recordValidationFailure(ctx, mr, valErrors)
	}

	// Every mode except Proxy (empty means Proxy; the CRD defaults it) is
	// compiled onto a gateway data plane by a sibling reconciler, so the
	// router-proxy is not part of those modes. A router that previously ran
	// in Proxy mode leaves its router-proxy objects behind as orphans of the
	// switch: remove the ones this router controls, then return without
	// provisioning and without writing Proxy-mode status - status in these
	// modes belongs to the Gateway and AgentGateway reconcilers.
	if mr.Spec.DataPlane != inferencev1alpha1.ModelRouterDataPlaneProxy {
		if err := r.removeRouterProxyObjects(ctx, mr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	compiled, err := r.compileRouterConfig(ctx, mr)
	if err != nil {
		return ctrl.Result{}, r.recordCompileFailure(ctx, mr, err)
	}

	if err := r.reconcileRouterConfigMap(ctx, mr, compiled); err != nil {
		return r.recordReconcileFailure(ctx, mr, compiled, "ConfigMap", err)
	}
	if err := r.reconcileRouterActivationRBAC(ctx, mr, compiled.HasPools); err != nil {
		return r.recordReconcileFailure(ctx, mr, compiled, "ActivationRBAC", err)
	}
	if err := r.reconcileRouterDeployment(ctx, mr, compiled.Hash, compiled.HasPools); err != nil {
		return r.recordReconcileFailure(ctx, mr, compiled, "Deployment", err)
	}
	if err := r.reconcileRouterService(ctx, mr); err != nil {
		return r.recordReconcileFailure(ctx, mr, compiled, "Service", err)
	}

	deployReady, deployMessage, err := r.fetchDeploymentReadiness(ctx, mr)
	if err != nil {
		return r.recordReconcileFailure(ctx, mr, compiled, "DeploymentReadiness", err)
	}

	if err := r.recordSuccess(ctx, mr, compiled, deployReady, deployMessage); err != nil {
		return ctrl.Result{}, err
	}
	// The proxy holds the only copy of the rolling-window budget counters, so
	// a router that declares budgets needs a timer to refresh its status even
	// when no watched object changes.
	if routerHasBudgets(mr) {
		return ctrl.Result{RequeueAfter: modelRouterBudgetPollInterval}, nil
	}
	return ctrl.Result{}, nil
}

// removeRouterProxyObjects deletes the router-proxy objects this ModelRouter
// controls, for a router that is not in Proxy mode. The proxy path creates
// seven objects across six kinds, all named routerProxyResourceName(mr.Name)
// in the router's namespace except the admin Service, which appends "-admin",
// and the data-plane Service when spec.proxy.serviceName names it (both its
// generated and its current name are swept): the ConfigMap
// (router_config_builder.go), the Deployment (router_deployment_builder.go),
// the two Services (router_service_builder.go), and the ServiceAccount, Role,
// and RoleBinding (router_activation_rbac.go, only when the router had pools
// or an endpoint-resolution backend). A Service left under a name the spec no
// longer carries (serviceName changed in the same edit that left Proxy mode)
// is not found by name; its owner reference still garbage-collects it with
// the router.
//
// Each object is fetched by its known name, never via a List: a non-Proxy
// router pays a few cached Gets per reconcile in steady state, and a name
// this router does not own resolves to NotFound or to someone else's object.
// The Gets go through the manager's shared cache. This sweep and the
// activation upserts in router_activation_rbac.go are this controller's only
// cached reads of ServiceAccount, Role, and RoleBinding, so on a cluster
// that never provisioned a pooled Proxy router this sweep is what first
// starts those informers. A same-name object this router does not control is
// left alone and never adopted: deleting it would destroy another owner's
// workload, and adopting it would hand the operator objects it did not
// create.
func (r *ModelRouterReconciler) removeRouterProxyObjects(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) error {
	name := routerProxyResourceName(mr.Name)
	if err := r.deleteIfControlled(ctx, mr, name, &corev1.ConfigMap{}, "ConfigMap"); err != nil {
		return err
	}
	if err := r.deleteIfControlled(ctx, mr, name, &appsv1.Deployment{}, "Deployment"); err != nil {
		return err
	}
	if err := r.deleteIfControlled(ctx, mr, name, &corev1.Service{}, "Service"); err != nil {
		return err
	}
	if svcName := routerProxyServiceName(mr); svcName != name {
		if err := r.deleteIfControlled(ctx, mr, svcName, &corev1.Service{}, "Service"); err != nil {
			return err
		}
	}
	if err := r.deleteIfControlled(ctx, mr, routerProxyAdminResourceName(mr.Name), &corev1.Service{}, "admin Service"); err != nil {
		return err
	}
	if err := r.deleteIfControlled(ctx, mr, name, &corev1.ServiceAccount{}, "ServiceAccount"); err != nil {
		return err
	}
	if err := r.deleteIfControlled(ctx, mr, name, &rbacv1.Role{}, "Role"); err != nil {
		return err
	}
	return r.deleteIfControlled(ctx, mr, name, &rbacv1.RoleBinding{}, "RoleBinding")
}

// deleteIfControlled fetches one candidate object by name and deletes it only
// when the ModelRouter is its managing controller. NotFound on the Get or on
// the Delete is success: the steady state for a router that was never in
// Proxy mode is a run of NotFound Gets, and a concurrent reconcile may delete
// the object between this reconcile's Get and Delete.
func (r *ModelRouterReconciler) deleteIfControlled(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	name string,
	obj client.Object,
	kind string,
) error {
	key := types.NamespacedName{Name: name, Namespace: mr.Namespace}
	if err := r.Get(ctx, key, obj); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get router-proxy %s: %w", kind, err)
	}
	if !metav1.IsControlledBy(obj, mr) {
		return nil
	}
	if err := r.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete router-proxy %s: %w", kind, err)
	}
	return nil
}

// recordValidationFailure writes the validated=false branch of the
// status. Used when validateModelRouter rejects the spec. It writes status
// only: the router-proxy objects keep the last config that compiled.
func (r *ModelRouterReconciler) recordValidationFailure(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	valErrors []ModelRouterValidationError,
) error {
	desired := mr.DeepCopy()
	now := metav1.Now()
	desired.Status.LastUpdated = &now
	apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
		Type:    ModelRouterConditionValidated,
		Status:  metav1.ConditionFalse,
		Reason:  validationFailureReason(valErrors),
		Message: formatValidationErrors(valErrors),
	})
	desired.Status.Phase = PhaseFailed
	desired.Status.ActiveRules = 0
	desired.Status.Backends = nil
	return r.patchStatus(ctx, mr, desired)
}

// recordCompileFailure writes the configmap-compile-failed branch.
func (r *ModelRouterReconciler) recordCompileFailure(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	err error,
) error {
	desired := mr.DeepCopy()
	now := metav1.Now()
	desired.Status.LastUpdated = &now
	apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
		Type:    ModelRouterConditionValidated,
		Status:  metav1.ConditionTrue,
		Reason:  ModelRouterReasonSpecValid,
		Message: "spec passed static validation",
	})
	apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
		Type:    ConditionAvailable,
		Status:  metav1.ConditionFalse,
		Reason:  ModelRouterReasonCompileFailed,
		Message: err.Error(),
	})
	desired.Status.Phase = PhaseFailed
	return r.patchStatus(ctx, mr, desired)
}

// recordReconcileFailure writes the data-plane-reconcile-failed branch.
// We keep Validated=True (spec was good) but flip Available=False with a
// message identifying which child resource broke. As before, the returned
// error is only the status patch's: the condition carries the failure
// evidence and its write triggers the next reconcile, so the underlying
// error is not returned.
//
// A routerProxyNameTakenError is a refusal, not a reconcile failure: the
// condition carries the dedicated RouterProxyNameTaken reason and the result
// requeues on routerProxyNameTakenRequeueAfter, because the refusal depends
// on cluster state outside the spec and nothing watches the foreign object.
func (r *ModelRouterReconciler) recordReconcileFailure(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	compiled *compiledConfig,
	resourceKind string,
	err error,
) (ctrl.Result, error) {
	desired := mr.DeepCopy()
	now := metav1.Now()
	desired.Status.LastUpdated = &now
	reason := ModelRouterReasonReconcileFailed
	if routerProxyNameTaken(err) != nil {
		reason = ModelRouterReasonRouterProxyNameTaken
	}
	apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
		Type:    ModelRouterConditionValidated,
		Status:  metav1.ConditionTrue,
		Reason:  ModelRouterReasonSpecValid,
		Message: "spec passed static validation",
	})
	apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
		Type:    ConditionAvailable,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: resourceKind + ": " + err.Error(),
	})
	if compiled != nil {
		desired.Status.Backends = compiled.Backends
	}
	desired.Status.Phase = PhaseFailed
	if perr := r.patchStatus(ctx, mr, desired); perr != nil {
		return ctrl.Result{}, perr
	}
	if reason == ModelRouterReasonRouterProxyNameTaken {
		return ctrl.Result{RequeueAfter: routerProxyNameTakenRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

// recordSuccess writes the happy-path status when the data plane is at
// least provisioning correctly. Phase ranges over Provisioning / Ready
// / Degraded depending on backend health and deployment readiness.
func (r *ModelRouterReconciler) recordSuccess(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	compiled *compiledConfig,
	deployReady bool,
	deployMessage string,
) error {
	desired := mr.DeepCopy()
	now := metav1.Now()
	desired.Status.LastUpdated = &now
	desired.Status.Endpoint = routerProxyEndpoint(mr)
	desired.Status.ActiveRules = safeInt32(len(mr.Spec.Rules))
	desired.Status.Backends = compiled.Backends

	// Budget utilization is per-budget state the proxy process holds, so it is
	// polled rather than derived. A proxy that cannot answer leaves the
	// previous snapshot in place (desired is a deep copy of the observed
	// object) and reports why: a status that quietly reads zero is worse than
	// a stale one.
	if routerHasBudgets(mr) {
		pollCtx, cancel := context.WithTimeout(ctx, budgetProbeTimeout)
		usage, pollErr := probeBudgetUtilization(pollCtx, r.budgetStatusURL(mr))
		cancel()
		if pollErr != nil {
			apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
				Type:    ModelRouterConditionBudgetReporting,
				Status:  metav1.ConditionFalse,
				Reason:  ModelRouterReasonBudgetUnreachable,
				Message: pollErr.Error(),
			})
		} else {
			desired.Status.BudgetUtilization = budgetStatusFromUsage(usage)
			apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
				Type:    ModelRouterConditionBudgetReporting,
				Status:  metav1.ConditionTrue,
				Reason:  ModelRouterReasonBudgetReported,
				Message: fmt.Sprintf("%d budgets reported", len(desired.Status.BudgetUtilization)),
			})
		}
	}

	apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
		Type:    ModelRouterConditionValidated,
		Status:  metav1.ConditionTrue,
		Reason:  ModelRouterReasonSpecValid,
		Message: "spec passed static validation",
	})

	backendsReady, reason, backendsMessage := backendsReadyCondition(compiled)
	apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
		Type:    ModelRouterConditionBackendsReady,
		Status:  conditionBool(backendsReady),
		Reason:  reason,
		Message: backendsMessage,
	})

	apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
		Type:    ConditionAvailable,
		Status:  conditionBool(deployReady),
		Reason:  conditionPick(deployReady, ModelRouterReasonDeploymentReady, ModelRouterReasonDeploymentNotReady),
		Message: deployMessage,
	})

	switch {
	case deployReady && backendsReady:
		desired.Status.Phase = PhaseReady
	case deployReady && !backendsReady:
		desired.Status.Phase = ModelRouterPhaseDegraded
	default:
		desired.Status.Phase = ModelRouterPhaseProvisioning
	}
	return r.patchStatus(ctx, mr, desired)
}

func (r *ModelRouterReconciler) patchStatus(
	ctx context.Context,
	original, desired *inferencev1alpha1.ModelRouter,
) error {
	if err := r.Status().Patch(ctx, desired, client.MergeFrom(original)); err != nil {
		log.FromContext(ctx).Error(err, "failed to update ModelRouter status")
		return err
	}
	return nil
}

// fetchDeploymentReadiness reads the proxy Deployment's current replica
// status and returns a (ready, message, err) triple. Returns (false,
// "Deployment not yet created", nil) when the Deployment does not exist
// (the create just queued and we'll re-reconcile when the cache observes
// it).
func (r *ModelRouterReconciler) fetchDeploymentReadiness(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) (bool, string, error) {
	dep := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      routerProxyResourceName(mr.Name),
		Namespace: mr.Namespace,
	}, dep)
	switch {
	case errors.IsNotFound(err):
		return false, "Deployment not yet observed", nil
	case err != nil:
		return false, "", err
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	if dep.Status.ReadyReplicas >= desired && desired > 0 {
		return true, "Deployment is fully Ready", nil
	}
	return false, "Waiting for proxy pods to become Ready", nil
}

// summarizeBackends returns (allHealthy, message) for the per-backend
// statuses produced by compileRouterConfig.
func summarizeBackends(backends []inferencev1alpha1.BackendStatus) (bool, string) {
	if len(backends) == 0 {
		return false, "no backends declared"
	}
	healthy := 0
	for _, b := range backends {
		if b.Healthy {
			healthy++
		}
	}
	if healthy == len(backends) {
		return true, "all backends resolved"
	}
	return false, "some backends unresolved (see status.backends for details)"
}

// backendsReadyCondition derives the BackendsReady condition from the compile.
// A keyed backend wins the reason: it names the cause and the fix.
func backendsReadyCondition(compiled *compiledConfig) (bool, string, string) {
	if len(compiled.KeyedBackends) > 0 || len(compiled.KeyedPoolBackends) > 0 {
		return false, ModelRouterReasonBackendAPIKeyUnsupported, keyedVerdictMessage(compiled.KeyedBackends, compiled.KeyedPoolBackends)
	}
	ready, msg := summarizeBackends(compiled.Backends)
	return ready, conditionPick(ready, ModelRouterReasonBackendsResolved, ModelRouterReasonBackendsUnresolved), msg
}

func conditionBool(ok bool) metav1.ConditionStatus {
	if ok {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func conditionPick(ok bool, whenTrue, whenFalse string) string {
	if ok {
		return whenTrue
	}
	return whenFalse
}

// safeInt32 narrows an int to int32, clamping at math.MaxInt32 when needed,
// for populating Status fields whose CRD type is int32. The bounds check
// above the conversion makes the narrow safe; gosec G115 cannot see across
// the branch so we suppress on that single line.
func safeInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n) //nolint:gosec // bounds-checked above
}

// SetupWithManager wires up the ModelRouter primary watch, the owned
// child resources (Deployment, Service, ConfigMap), and the secondary
// watch on InferenceService so a router whose backend turns Ready (or
// goes away) is re-reconciled.
func (r *ModelRouterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.ModelRouter{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Watches(
			&inferencev1alpha1.InferenceService{},
			handler.EnqueueRequestsFromMapFunc(r.findModelRoutersForInferenceService),
		).
		Watches(
			&inferencev1alpha1.ModelPool{},
			handler.EnqueueRequestsFromMapFunc(r.findModelRoutersForModelPool),
		).
		Watches(
			&discoveryv1.EndpointSlice{},
			handler.EnqueueRequestsFromMapFunc(r.findModelRoutersForEndpointSlice),
		).
		Named(modelRouterControllerName).
		Complete(r)
}

// findModelRoutersForEndpointSlice re-reconciles every ModelRouter in a changed
// EndpointSlice's namespace that has at least one endpoint-resolution backend.
// The endpoint list is part of the compiled config and therefore of its hash,
// so pod churn must rebuild the config and re-roll the proxy. The mapping is
// namespace-wide rather than joined back to the owning InferenceService: a
// slice carries only the Service name, and a router with endpoint backends is
// cheap to recompile.
func (r *ModelRouterReconciler) findModelRoutersForEndpointSlice(ctx context.Context, obj client.Object) []reconcile.Request {
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return nil
	}
	routerList := &inferencev1alpha1.ModelRouterList{}
	if err := r.List(ctx, routerList, client.InNamespace(slice.Namespace)); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range routerList.Items {
		mr := &routerList.Items[i]
		if !routerHasEndpointBackend(mr) {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
		})
	}
	return requests
}

// routerHasEndpointBackend reports whether any of the router's local backends
// resolves to pod endpoints rather than the Service DNS name.
func routerHasEndpointBackend(mr *inferencev1alpha1.ModelRouter) bool {
	for i := range mr.Spec.Backends {
		b := &mr.Spec.Backends[i]
		if b.InferenceServiceRef != nil && b.Resolution == inferencev1alpha1.RouterBackendResolutionEndpoint {
			return true
		}
	}
	return false
}

// findModelRoutersForModelPool re-reconciles every ModelRouter in a changed
// pool's namespace whose backends include one of the pool's members. A pool
// edit (member set, minResidency) changes the compiled BackendPool the proxy
// sees, so the router config must be rebuilt.
func (r *ModelRouterReconciler) findModelRoutersForModelPool(ctx context.Context, obj client.Object) []reconcile.Request {
	pool, ok := obj.(*inferencev1alpha1.ModelPool)
	if !ok {
		return nil
	}
	routerList := &inferencev1alpha1.ModelRouterList{}
	if err := r.List(ctx, routerList, client.InNamespace(pool.Namespace)); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range routerList.Items {
		mr := &routerList.Items[i]
		for _, m := range pool.Spec.Members {
			if routerReferencesInferenceService(mr, m.InferenceServiceRef.Name) {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
				})
				break
			}
		}
	}
	return requests
}

// findModelRoutersForInferenceService returns reconcile requests for every
// ModelRouter in the same namespace that references the given
// InferenceService by name. Mirrors the pattern used by
// InferenceServiceReconciler.findInferenceServicesForModel.
func (r *ModelRouterReconciler) findModelRoutersForInferenceService(ctx context.Context, obj client.Object) []reconcile.Request {
	isvc, ok := obj.(*inferencev1alpha1.InferenceService)
	if !ok {
		return nil
	}

	routerList := &inferencev1alpha1.ModelRouterList{}
	if err := r.List(ctx, routerList, client.InNamespace(isvc.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, mr := range routerList.Items {
		if routerReferencesInferenceService(&mr, isvc.Name) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      mr.Name,
					Namespace: mr.Namespace,
				},
			})
		}
	}
	return requests
}

// routerReferencesInferenceService reports whether the router has a backend
// whose InferenceServiceRef names the given service. Pulled out as a free
// function so it's trivially unit-testable.
func routerReferencesInferenceService(mr *inferencev1alpha1.ModelRouter, isvcName string) bool {
	for _, b := range mr.Spec.Backends {
		if b.InferenceServiceRef != nil && b.InferenceServiceRef.Name == isvcName {
			return true
		}
	}
	return false
}
