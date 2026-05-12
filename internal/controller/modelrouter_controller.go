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

	ModelRouterConditionValidated     = "Validated"
	ModelRouterConditionBackendsReady = "BackendsReady"

	ModelRouterReasonSpecValid   = "SpecValid"
	ModelRouterReasonSpecInvalid = "SpecInvalid"

	modelRouterControllerName = "modelrouter"
)

// ModelRouterReconciler reconciles the ModelRouter CRD. In Phase 1 (this
// commit) the reconciler validates the spec and writes a Validated
// condition. No data plane is managed yet; the proxy Deployment, Service,
// and ConfigMap land in #428.
type ModelRouterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=modelrouters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=modelrouters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=modelrouters/finalizers,verbs=update
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices,verbs=get;list;watch

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
	if err := r.updateStatus(ctx, mr, valErrors); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// updateStatus patches the ModelRouter status with the Validated condition
// and an appropriate phase. Other conditions (BackendsReady, Available,
// Degraded) will be set by the data-plane reconciler in #428.
func (r *ModelRouterReconciler) updateStatus(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	valErrors []ModelRouterValidationError,
) error {
	logger := log.FromContext(ctx)

	desired := mr.DeepCopy()
	now := metav1.Now()
	desired.Status.LastUpdated = &now

	if len(valErrors) == 0 {
		apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
			Type:    ModelRouterConditionValidated,
			Status:  metav1.ConditionTrue,
			Reason:  ModelRouterReasonSpecValid,
			Message: "spec passed static validation",
		})
		// Phase 1: no data plane yet, so a valid spec stays Pending.
		// #428 will advance this to Provisioning / Ready / Degraded.
		desired.Status.Phase = ModelRouterPhasePending
		desired.Status.ActiveRules = safeInt32(len(desired.Spec.Rules))
	} else {
		apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
			Type:    ModelRouterConditionValidated,
			Status:  metav1.ConditionFalse,
			Reason:  ModelRouterReasonSpecInvalid,
			Message: formatValidationErrors(valErrors),
		})
		desired.Status.Phase = PhaseFailed
		desired.Status.ActiveRules = 0
	}

	if err := r.Status().Patch(ctx, desired, client.MergeFrom(mr)); err != nil {
		logger.Error(err, "failed to update ModelRouter status")
		return err
	}
	return nil
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

// SetupWithManager wires up the ModelRouter primary watch plus a secondary
// watch on InferenceService so a router whose backend turns Ready (or goes
// away) is re-reconciled and re-validated.
func (r *ModelRouterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.ModelRouter{}).
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
