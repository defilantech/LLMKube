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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

const (
	gpuQuotaControllerName = "gpuquota"
)

// GPUQuotaReconciler reconciles the GPUQuota CRD. It aggregates GPU usage
// from InferenceServices in the quota's scope and writes the total to
// Status.UsedGPUCount. This is a status-only reconciler: it never rejects
// anything or owns external resources.
type GPUQuotaReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=gpuquotas,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=gpuquotas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *GPUQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reconcileStart := time.Now()
	defer func() {
		llmkubemetrics.ReconcileDuration.WithLabelValues(gpuQuotaControllerName).
			Observe(time.Since(reconcileStart).Seconds())
	}()

	logger := log.FromContext(ctx)
	logger.Info("Reconciling GPUQuota", "name", req.Name, "namespace", req.Namespace)

	gq := &inferencev1alpha1.GPUQuota{}
	if err := r.Get(ctx, req.NamespacedName, gq); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to get GPUQuota")
		return ctrl.Result{}, err
	}

	// Resolve scope: which namespaces are in-scope for this quota?
	var inScopeNamespaces []string
	switch {
	case gq.Spec.NamespaceRef != "":
		// NamespaceRef set: single namespace scope.
		inScopeNamespaces = []string{gq.Spec.NamespaceRef}
	case gq.Spec.Selector != nil:
		// Selector set: label-selector scope across multiple namespaces.
		sel, err := metav1.LabelSelectorAsSelector(gq.Spec.Selector)
		if err != nil {
			logger.Error(err, "failed to convert GPUQuota selector")
			return ctrl.Result{}, err
		}
		nsList := &corev1.NamespaceList{}
		if err := r.List(ctx, nsList, client.MatchingLabelsSelector{Selector: sel}); err != nil {
			logger.Error(err, "failed to list namespaces for selector")
			return ctrl.Result{}, err
		}
		for _, ns := range nsList.Items {
			inScopeNamespaces = append(inScopeNamespaces, ns.Name)
		}
	default:
		// Both/neither set: no in-scope namespaces. The CEL rule already
		// enforces exactly-one, so this is defensive.
		return ctrl.Result{}, nil
	}

	// Aggregate GPU usage from InferenceServices in the in-scope namespaces.
	var usedGPUCount int32
	for _, nsName := range inScopeNamespaces {
		isvcList := &inferencev1alpha1.InferenceServiceList{}
		if err := r.List(ctx, isvcList, client.InNamespace(nsName)); err != nil {
			logger.Error(err, "failed to list InferenceServices", "namespace", nsName)
			return ctrl.Result{}, err
		}
		for _, isvc := range isvcList.Items {
			// GPU count per pod: nil resources means 0.
			gpuPerPod := int32(0)
			if isvc.Spec.Resources != nil && isvc.Spec.Resources.GPU > 0 {
				gpuPerPod = isvc.Spec.Resources.GPU
			}
			// Replicas: nil means 1.
			replicas := int32(1)
			if isvc.Spec.Replicas != nil {
				replicas = *isvc.Spec.Replicas
			}
			usedGPUCount += gpuPerPod * replicas
		}
	}

	// VRAM aggregation is a DOCUMENTED, INTENTIONAL DEFERRAL: InferenceService
	// exposes no VRAM field, so UsedVRAMBytes stays 0 until either
	// InferenceService gains a VRAM field or it is derived from the Model size;
	// the admission webhook already treats vramBytes 0 as "no cap", so the
	// VRAM half of the quota is simply inert until then.
	usedVRAMBytes := int64(0)

	// Write status.
	desired := gq.DeepCopy()
	desired.Status.UsedGPUCount = usedGPUCount
	desired.Status.UsedVRAMBytes = usedVRAMBytes

	if err := r.Status().Patch(ctx, desired, client.MergeFrom(gq)); err != nil {
		logger.Error(err, "failed to update GPUQuota status")
		return ctrl.Result{}, err
	}

	// Publish per-quota usage and cap gauges (#416) so the multi-tenancy
	// dashboard can plot utilization = used / limit.
	llmkubemetrics.GPUQuotaUsedGPUCount.WithLabelValues(gq.Name, gq.Namespace).Set(float64(usedGPUCount))
	llmkubemetrics.GPUQuotaGPUCountLimit.WithLabelValues(gq.Name, gq.Namespace).Set(float64(gq.Spec.GPUCount))

	return ctrl.Result{}, nil
}

// SetupWithManager wires up the GPUQuota primary watch and the secondary
// watch on InferenceService so a changed InferenceService triggers
// re-aggregation of all GPUQuotas whose scope covers the changed service's
// namespace.
func (r *GPUQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.GPUQuota{}).
		Watches(
			&inferencev1alpha1.InferenceService{},
			handler.EnqueueRequestsFromMapFunc(r.findGPUQuotasForInferenceService),
		).
		Named(gpuQuotaControllerName).
		Complete(r)
}

// findGPUQuotasForInferenceService returns reconcile requests for every
// GPUQuota whose scope covers the given InferenceService's namespace.
// Mirrors the pattern used by ModelRouterReconciler.findModelRoutersForInferenceService.
func (r *GPUQuotaReconciler) findGPUQuotasForInferenceService(ctx context.Context, obj client.Object) []reconcile.Request {
	isvc, ok := obj.(*inferencev1alpha1.InferenceService)
	if !ok {
		return nil
	}

	gqList := &inferencev1alpha1.GPUQuotaList{}
	if err := r.List(ctx, gqList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, gq := range gqList.Items {
		if gpuQuotaCoversNamespace(r.Client, &gq, isvc.Namespace) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      gq.Name,
					Namespace: gq.Namespace,
				},
			})
		}
	}
	return requests
}

// gpuQuotaCoversNamespace reports whether the GPUQuota's scope includes the
// given namespace. A quota covers a namespace when:
//   - spec.namespaceRef matches the namespace name, or
//   - spec.selector matches the namespace's labels.
func gpuQuotaCoversNamespace(r client.Client, gq *inferencev1alpha1.GPUQuota, nsName string) bool {
	switch {
	case gq.Spec.NamespaceRef != "":
		return gq.Spec.NamespaceRef == nsName
	case gq.Spec.Selector != nil:
		sel, err := metav1.LabelSelectorAsSelector(gq.Spec.Selector)
		if err != nil {
			return false
		}
		ns := &corev1.Namespace{}
		if err := r.Get(context.Background(), types.NamespacedName{Name: nsName}, ns); err != nil {
			return false
		}
		return sel.Matches(labels.Set(ns.Labels))
	}
	return false
}
