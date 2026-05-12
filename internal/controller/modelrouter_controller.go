/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"math"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

	ModelRouterConditionValidated     = "Validated"
	ModelRouterConditionBackendsReady = "BackendsReady"

	ModelRouterReasonSpecValid          = "SpecValid"
	ModelRouterReasonSpecInvalid        = "SpecInvalid"
	ModelRouterReasonCompileFailed      = "CompileFailed"
	ModelRouterReasonReconcileFailed    = "ReconcileFailed"
	ModelRouterReasonBackendsResolved   = "BackendsResolved"
	ModelRouterReasonBackendsUnresolved = "BackendsUnresolved"
	ModelRouterReasonDeploymentReady    = "DeploymentReady"
	ModelRouterReasonDeploymentNotReady = "DeploymentNotReady"
	ModelRouterReasonDeploymentDegraded = "DeploymentDegraded"

	modelRouterControllerName = "modelrouter"
)

// ModelRouterReconciler reconciles the ModelRouter CRD. It validates the
// spec, compiles the routing config, and creates / updates the ConfigMap,
// Deployment, and Service that constitute the data plane.
type ModelRouterReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// RouterProxyImage is the default container image for the
	// router-proxy. Per-ModelRouter overrides in spec.proxy.image take
	// precedence. Wired in cmd/main.go from --router-proxy-image.
	RouterProxyImage string
}

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=modelrouters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=modelrouters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=modelrouters/finalizers,verbs=update
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

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

	compiled, err := r.compileRouterConfig(ctx, mr)
	if err != nil {
		return ctrl.Result{}, r.recordCompileFailure(ctx, mr, err)
	}

	if err := r.reconcileRouterConfigMap(ctx, mr, compiled); err != nil {
		return ctrl.Result{}, r.recordReconcileFailure(ctx, mr, compiled, "ConfigMap", err)
	}
	if err := r.reconcileRouterDeployment(ctx, mr, compiled.Hash); err != nil {
		return ctrl.Result{}, r.recordReconcileFailure(ctx, mr, compiled, "Deployment", err)
	}
	if err := r.reconcileRouterService(ctx, mr); err != nil {
		return ctrl.Result{}, r.recordReconcileFailure(ctx, mr, compiled, "Service", err)
	}

	deployReady, deployMessage, err := r.fetchDeploymentReadiness(ctx, mr)
	if err != nil {
		return ctrl.Result{}, r.recordReconcileFailure(ctx, mr, compiled, "DeploymentReadiness", err)
	}

	return ctrl.Result{}, r.recordSuccess(ctx, mr, compiled, deployReady, deployMessage)
}

// recordValidationFailure writes the validated=false branch of the
// status. Used when validateModelRouter rejects the spec.
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
		Reason:  ModelRouterReasonSpecInvalid,
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
// message identifying which child resource broke.
func (r *ModelRouterReconciler) recordReconcileFailure(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	compiled *compiledConfig,
	resourceKind string,
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
		Reason:  ModelRouterReasonReconcileFailed,
		Message: resourceKind + ": " + err.Error(),
	})
	if compiled != nil {
		desired.Status.Backends = compiled.Backends
	}
	desired.Status.Phase = PhaseFailed
	return r.patchStatus(ctx, mr, desired)
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

	apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
		Type:    ModelRouterConditionValidated,
		Status:  metav1.ConditionTrue,
		Reason:  ModelRouterReasonSpecValid,
		Message: "spec passed static validation",
	})

	backendsReady, backendsMessage := summarizeBackends(compiled.Backends)
	apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
		Type:    ModelRouterConditionBackendsReady,
		Status:  conditionBool(backendsReady),
		Reason:  conditionPick(backendsReady, ModelRouterReasonBackendsResolved, ModelRouterReasonBackendsUnresolved),
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
		Named(modelRouterControllerName).
		Complete(r)
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
