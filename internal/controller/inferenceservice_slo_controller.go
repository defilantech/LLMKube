/*
Copyright 2026.

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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	sloControllerName = "inferenceservice-slo"

	// SLOConditionReady is the InferenceService status condition the SLO
	// reconciler owns. True once the Pyrra resource is reconciled; False
	// (with a reason) when the integration is off, Pyrra is not installed,
	// the indicator has no data source on this runtime, or reconciliation
	// failed.
	SLOConditionReady = "SLOReady"

	// SLOConditionDataSource warns when the SLO is rendered but cluster
	// Prometheus has no scrape target for the service (Metal-backed
	// InferenceServices run off-cluster), so the error budget will show no
	// data until a metrics path for off-cluster runtimes exists.
	SLOConditionDataSource = "SLODataSourceAvailable"

	sloReasonCreated     = "SLOCreated"
	sloReasonDisabled    = "IntegrationDisabled"
	sloReasonCRDsMissing = "PyrraNotInstalled"
	sloReasonUnsupported = "IndicatorUnsupportedForRuntime"
	sloReasonReconcile   = "ReconcileFailed"

	sloReasonOffCluster = "OffClusterRuntime"
	sloReasonInCluster  = "InClusterRuntime"
)

// InferenceServiceSLOReconciler renders and lifecycle-binds a Pyrra
// ServiceLevelObjective for each InferenceService with spec.slo set. It is
// intentionally separate from the core InferenceServiceReconciler (same
// rationale as the gateway reconciler): the integration stays cleanly
// optional, and a cluster without the pyrra.dev CRD runs the rest of the
// operator unaffected.
type InferenceServiceSLOReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Enabled mirrors the --enable-pyrra-slo operator flag (Helm:
	// pyrra.enabled). When false the reconciler only reports the
	// IntegrationDisabled condition for opted-in services.
	Enabled bool

	detectorOnce sync.Once
	detector     *crdDetector
}

// +kubebuilder:rbac:groups=pyrra.dev,resources=servicelevelobjectives,verbs=get;list;watch;create;update;patch;delete

// Reconcile renders the Pyrra resource for an InferenceService with spec.slo,
// or no-ops cleanly when the field is unset, the integration is disabled, or
// the pyrra.dev CRD is not installed.
func (r *InferenceServiceSLOReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName(sloControllerName)

	isvc := &inferencev1alpha1.InferenceService{}
	if err := r.Get(ctx, req.NamespacedName, isvc); err != nil {
		if apierrors.IsNotFound(err) {
			// Owner-ref GC removes the rendered SLO with the ISvc.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if isvc.Spec.SLO == nil {
		// spec.slo removed (or never set): delete anything we rendered
		// earlier. Owner refs only GC on ISvc deletion, not field removal.
		return ctrl.Result{}, r.cleanupStaleSLOs(ctx, isvc, "" /* keep nothing */, log)
	}

	if !r.Enabled {
		return ctrl.Result{}, r.setSLOCondition(ctx, isvc, metav1.ConditionFalse, sloReasonDisabled,
			"spec.slo is set but the Pyrra integration is disabled; enable it with pyrra.enabled=true (operator flag --enable-pyrra-slo)")
	}

	present, err := r.pyrraCRDPresent(log)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !present {
		return ctrl.Result{}, r.setSLOCondition(ctx, isvc, metav1.ConditionFalse, sloReasonCRDsMissing,
			"Pyrra ServiceLevelObjective CRD is not installed; see https://github.com/pyrra-dev/pyrra")
	}

	if !sloIndicatorSupported(isvc.Spec.Runtime, isvc.Spec.SLO.Indicator) {
		// Also drop anything rendered under a previously-supported spec so
		// no stale SLO keeps alerting.
		if err := r.cleanupStaleSLOs(ctx, isvc, "", log); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.setSLOCondition(ctx, isvc, metav1.ConditionFalse, sloReasonUnsupported,
			fmt.Sprintf("indicator %q has no metric source on runtime %q (see docs/observability/slo.md)",
				isvc.Spec.SLO.Indicator, isvc.Spec.Runtime))
	}

	desired := newServiceLevelObjective(isvc)
	if err := r.applySLO(ctx, isvc, desired); err != nil {
		_ = r.setSLOCondition(ctx, isvc, metav1.ConditionFalse, sloReasonReconcile, err.Error())
		return ctrl.Result{}, err
	}
	if err := r.cleanupStaleSLOs(ctx, isvc, desired.GetName(), log); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.setSLOReady(ctx, isvc)
}

// applySLO creates-or-updates the rendered resource, owner-referenced to the
// InferenceService, overwriting spec drift while preserving server-managed
// metadata (same shape as the gateway applyResource).
func (r *InferenceServiceSLOReconciler) applySLO(
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
		live.SetLabels(desired.GetLabels())
		return setControllerReferenceUnblocked(isvc, live, r.Scheme)
	})
	return err
}

// cleanupStaleSLOs deletes every SLO labeled for this InferenceService except
// keep (empty keep deletes all). Handles spec.slo removal, renames, and
// indicator switches. No-ops when the pyrra.dev CRD is absent (nothing could
// have been created, or the CRD deletion already cascaded).
func (r *InferenceServiceSLOReconciler) cleanupStaleSLOs(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	keep string,
	log logr.Logger,
) error {
	present, err := r.pyrraCRDPresent(log)
	if err != nil || !present {
		return err
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(pyrraSLOListGVK())
	if err := r.List(ctx, list,
		client.InNamespace(isvc.Namespace),
		client.MatchingLabels{sloISvcLabel: isvc.Name}); err != nil {
		return err
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetName() == keep {
			continue
		}
		if err := r.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		log.Info("deleted stale ServiceLevelObjective", "name", item.GetName())
	}
	return nil
}

// pyrraCRDPresent reports whether the pyrra.dev SLO CRD is registered,
// delegating to the shared crdDetector (positive detection cached; re-checks
// while absent so Pyrra installed after operator startup is picked up).
func (r *InferenceServiceSLOReconciler) pyrraCRDPresent(log logr.Logger) (bool, error) {
	r.detectorOnce.Do(func() {
		r.detector = newCRDDetector([]schema.GroupVersionKind{pyrraSLOGVK()})
	})
	return r.detector.Present(r.Client, log)
}

// setSLOReady writes SLOReady=True plus the data-source warning condition for
// off-cluster (Metal) services, whose series never reach cluster Prometheus.
func (r *InferenceServiceSLOReconciler) setSLOReady(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
) error {
	patch := client.MergeFrom(isvc.DeepCopy())
	apimeta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:    SLOConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  sloReasonCreated,
		Message: fmt.Sprintf("Pyrra ServiceLevelObjective %q reconciled", sloResourceName(isvc)),
	})

	dataSource := metav1.Condition{
		Type:    SLOConditionDataSource,
		Status:  metav1.ConditionTrue,
		Reason:  sloReasonInCluster,
		Message: "serving pods are scraped by cluster Prometheus",
	}
	model := &inferencev1alpha1.Model{}
	err := r.Get(ctx, types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Spec.ModelRef}, model)
	if err == nil && isMetalModel(model) {
		dataSource.Status = metav1.ConditionFalse
		dataSource.Reason = sloReasonOffCluster
		dataSource.Message = "Metal-backed services run off-cluster; cluster Prometheus has no scrape target, so this SLO will show no data"
	}
	// A missing Model is the core reconciler's problem to report; default to
	// the in-cluster assumption here rather than failing the SLO reconcile.
	apimeta.SetStatusCondition(&isvc.Status.Conditions, dataSource)

	return r.Status().Patch(ctx, isvc, patch)
}

// setSLOCondition writes a non-ready SLOReady condition.
// nolint:unparam // status is parameterized to mirror setSLOReady's Condition shape even though every current call site passes metav1.ConditionFalse
func (r *InferenceServiceSLOReconciler) setSLOCondition(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	patch := client.MergeFrom(isvc.DeepCopy())
	apimeta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:    SLOConditionReady,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	return r.Status().Patch(ctx, isvc, patch)
}

// SetupWithManager wires the SLO reconciler to watch InferenceServices. Like
// the gateway reconciler, we do not Owns() the rendered resource: the
// pyrra.dev CRD may be absent, and an Owns watch on an unregistered kind
// fails manager startup. The primary watch plus CreateOrUpdate drift
// correction is sufficient.
func (r *InferenceServiceSLOReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.InferenceService{}).
		Named(sloControllerName).
		Complete(r)
}
