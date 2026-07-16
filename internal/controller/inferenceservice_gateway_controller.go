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
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	gatewayControllerName = "inferenceservice-gateway"

	// GatewayConditionReady is the InferenceService status condition type the
	// gateway reconciler owns. True once the three gateway resources are
	// reconciled; False (with a reason) when the integration is disabled
	// because the aigw CRDs are absent, or when reconciliation fails.
	GatewayConditionReady = "GatewayReady"

	// Gateway condition reasons.
	gatewayReasonExposed     = "Exposed"
	gatewayReasonCRDsMissing = "GatewayCRDsNotInstalled"
	gatewayReasonReconcile   = "ReconcileFailed"
)

// InferenceServiceGatewayReconciler generates and lifecycle-binds the Envoy AI
// Gateway resources (Backend + AIServiceBackend + AIGatewayRoute) for each
// opted-in InferenceService. It is intentionally separate from the core
// InferenceServiceReconciler so the integration stays cleanly optional and
// feature-flaggable, and so a cluster without the aigw CRDs runs the rest of
// the operator unaffected.
type InferenceServiceGatewayReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// detector is the shared CRD-presence gate. Lazily initialized on first
	// reconcile (so a zero-value reconciler, as built in cmd/main.go, works) and
	// reused thereafter. It caches a positive detection and re-checks while
	// absent so a gateway installed after startup is picked up without a
	// restart.
	detectorOnce sync.Once
	detector     *crdDetector
}

// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=backends,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=aigateway.envoyproxy.io,resources=aiservicebackends,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=aigateway.envoyproxy.io,resources=aigatewayroutes,verbs=get;list;watch;create;update;patch;delete

// Reconcile generates the gateway resources for an opted-in InferenceService,
// or no-ops cleanly when the InferenceService did not opt in or when the aigw
// CRDs are not installed.
func (r *InferenceServiceGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName(gatewayControllerName)

	isvc := &inferencev1alpha1.InferenceService{}
	if err := r.Get(ctx, req.NamespacedName, isvc); err != nil {
		if apierrors.IsNotFound(err) {
			// Owner-ref GC removes the generated resources when the
			// InferenceService is deleted; nothing to do here.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Not opted in: nothing to generate. (Opt-OUT after a prior opt-in is a
	// documented follow-up; on toggling enabled=false the generated resources
	// linger until the InferenceService is deleted. See proposal 4.7.)
	if !gatewayEnabled(isvc) {
		return ctrl.Result{}, nil
	}

	// CRD-presence gate. When the aigw CRDs are absent we never create
	// resources and never requeue in a hot loop; we set a clear condition so an
	// opted-in user sees why nothing happened. A transient discovery error
	// (not a missing kind) is returned so we requeue rather than disabling.
	present, err := r.gatewayCRDsPresent(log)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !present {
		return ctrl.Result{}, r.setGatewayCondition(ctx, isvc, metav1.ConditionFalse, gatewayReasonCRDsMissing,
			"Envoy AI Gateway CRDs are not installed; gateway exposure is disabled")
	}

	if err := r.reconcileGatewayResources(ctx, isvc); err != nil {
		// Surface the failure on status, then return the error to requeue with
		// the manager's backoff.
		_ = r.setGatewayCondition(ctx, isvc, metav1.ConditionFalse, gatewayReasonReconcile, err.Error())
		return ctrl.Result{}, err
	}

	if err := r.setGatewayReady(ctx, isvc); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileGatewayResources creates-or-updates the Backend, AIServiceBackend,
// and AIGatewayRoute, each owner-referenced to the InferenceService.
func (r *InferenceServiceGatewayReconciler) reconcileGatewayResources(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
) error {
	resources := []struct {
		kind string
		obj  *unstructured.Unstructured
	}{
		{gatewayBackendKind, newBackend(isvc)},
		{aiServiceBackendKind, newAIServiceBackend(isvc)},
		{aiGatewayRouteKind, newAIGatewayRoute(isvc)},
	}
	for _, res := range resources {
		if err := r.applyResource(ctx, isvc, res.obj); err != nil {
			return fmt.Errorf("%s: %w", res.kind, err)
		}
	}
	return nil
}

// applyResource owner-references desired to the InferenceService and
// creates-or-updates it. The desired spec is captured before CreateOrUpdate so
// the mutate function (which sees the live object on update) overwrites spec to
// correct any drift while preserving server-managed metadata.
func (r *InferenceServiceGatewayReconciler) applyResource(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	desired *unstructured.Unstructured,
) error {
	desiredSpec, _, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil {
		return err
	}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(desired.GroupVersionKind())
	live.SetName(desired.GetName())
	live.SetNamespace(desired.GetNamespace())

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, live, func() error {
		live.Object["spec"] = desiredSpec
		return setControllerReferenceUnblocked(isvc, live, r.Scheme)
	})
	return err
}

// gatewayCRDsPresent reports whether the three aigw CRDs slice 1 generates are
// registered, delegating to the shared crdDetector. A positive result is
// cached; while absent it re-checks on every call so a gateway installed after
// startup is picked up without an operator restart. A transient discovery error
// (not a missing kind) is returned so the caller requeues instead of caching a
// false negative. The disabled message is logged once.
func (r *InferenceServiceGatewayReconciler) gatewayCRDsPresent(log logr.Logger) (bool, error) {
	r.detectorOnce.Do(func() {
		r.detector = newCRDDetector("gateway", []schema.GroupVersionKind{
			backendGVK(), aiServiceBackendGVK(), aiGatewayRouteGVK(),
		})
	})
	return r.detector.Present(r.Client, log)
}

// setGatewayReady writes the success status: GatewayReady=True plus
// status.gateway with the resolved model name.
func (r *InferenceServiceGatewayReconciler) setGatewayReady(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
) error {
	patch := client.MergeFrom(isvc.DeepCopy())
	isvc.Status.Gateway = &inferencev1alpha1.GatewayStatus{
		RouteReady: true,
		ModelName:  resolveGatewayModelName(isvc),
	}
	apimeta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:    GatewayConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  gatewayReasonExposed,
		Message: fmt.Sprintf("model %q exposed via gateway %s", resolveGatewayModelName(isvc), gatewaySpec(isvc).GatewayRef.Name),
	})
	return r.Status().Patch(ctx, isvc, patch)
}

// setGatewayCondition writes a non-ready GatewayReady condition. On the
// CRDs-missing path it also clears any stale RouteReady so status reflects
// reality.
func (r *InferenceServiceGatewayReconciler) setGatewayCondition(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	patch := client.MergeFrom(isvc.DeepCopy())
	if isvc.Status.Gateway == nil {
		isvc.Status.Gateway = &inferencev1alpha1.GatewayStatus{}
	}
	isvc.Status.Gateway.RouteReady = false
	apimeta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:    GatewayConditionReady,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	return r.Status().Patch(ctx, isvc, patch)
}

// SetupWithManager wires the gateway reconciler to watch InferenceServices.
//
// We intentionally do not Owns() the generated resources: the operator may run
// in a cluster where the aigw CRDs are absent, and registering an Owns watch on
// an unregistered kind fails manager startup. The InferenceService primary
// watch plus CreateOrUpdate's drift correction is sufficient for the MVP.
func (r *InferenceServiceGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.InferenceService{}).
		Named(gatewayControllerName).
		Complete(r)
}
