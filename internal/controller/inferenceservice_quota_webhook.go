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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/defilantech/llmkube/internal/webhook/quota"
)

// +kubebuilder:webhook:path=/validate-inference-llmkube-dev-v1alpha1-inferenceservice-quota,mutating=false,failurePolicy=fail,sideEffects=None,groups=inference.llmkube.dev,resources=inferenceservices,verbs=create;update,versions=v1alpha1,name=vinferenceservicequota.inference.llmkube.dev,admissionReviewVersions=v1

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=gpuquotas,verbs=get;list;watch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// InferenceServiceQuotaValidator validates InferenceService CRs against
// GPUQuota admission rules. It rejects an InferenceService whose GPU
// allocation would exceed an applicable quota.
//
// NOTE: This webhook does NOT update GPUQuota.Status.AdmissionDenials.
// A validating webhook is sideEffects=None, so writing status from it
// would violate the admission webhook contract. The denial counter is
// a documented follow-up (a metric or a reconciler-observed counter).
type InferenceServiceQuotaValidator struct {
	Client client.Client
}

var _ admission.Validator[*inferencev1alpha1.InferenceService] = &InferenceServiceQuotaValidator{}

// SetupInferenceServiceQuotaWebhookWithManager registers the InferenceService
// GPUQuota validating webhook.
func SetupInferenceServiceQuotaWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &inferencev1alpha1.InferenceService{}).
		WithValidator(&InferenceServiceQuotaValidator{Client: mgr.GetClient()}).
		Complete()
}

// ValidateCreate validates an InferenceService on creation.
func (v *InferenceServiceQuotaValidator) ValidateCreate(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (admission.Warnings, error) {
	log.FromContext(ctx).V(1).Info("validating InferenceService create against GPUQuota", "name", isvc.Name, "namespace", isvc.Namespace)
	return nil, v.validate(ctx, isvc)
}

// ValidateUpdate validates an InferenceService on update.
func (v *InferenceServiceQuotaValidator) ValidateUpdate(ctx context.Context, oldISvc, isvc *inferencev1alpha1.InferenceService) (admission.Warnings, error) {
	log.FromContext(ctx).V(1).Info("validating InferenceService update against GPUQuota", "name", isvc.Name, "namespace", isvc.Namespace)
	return nil, v.validate(ctx, isvc)
}

// ValidateDelete is a no-op: deleting an InferenceService is always allowed.
func (v *InferenceServiceQuotaValidator) ValidateDelete(_ context.Context, _ *inferencev1alpha1.InferenceService) (admission.Warnings, error) {
	return nil, nil
}

// validate checks the InferenceService against all applicable GPUQuotas and
// returns an error if any quota denies the admission.
func (v *InferenceServiceQuotaValidator) validate(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	quotas, err := v.listApplicableQuotas(ctx, isvc)
	if err != nil {
		return fmt.Errorf("listing applicable GPUQuotas: %w", err)
	}

	for _, q := range quotas {
		allow, reason := v.decide(ctx, q, isvc)
		if !allow {
			// Record the denial as a metric (#416): a sideEffects=None
			// validating webhook cannot mutate the GPUQuota status counter,
			// so the per-quota denial count is surfaced here instead.
			llmkubemetrics.GPUQuotaAdmissionDenialsTotal.WithLabelValues(q.Name, q.Namespace).Inc()
			return fmt.Errorf("GPUQuota %q denied: %s", q.Name, reason)
		}
	}

	return nil
}

// listApplicableQuotas returns all GPUQuotas whose scope covers the given
// InferenceService's namespace. A quota covers a namespace when:
//   - NamespaceRef == the namespace, OR
//   - Selector matches the Namespace's labels.
func (v *InferenceServiceQuotaValidator) listApplicableQuotas(ctx context.Context, isvc *inferencev1alpha1.InferenceService) ([]inferencev1alpha1.GPUQuota, error) {
	var quotaList inferencev1alpha1.GPUQuotaList
	if err := v.Client.List(ctx, &quotaList); err != nil {
		return nil, err
	}

	var ns corev1.Namespace
	if err := v.Client.Get(ctx, types.NamespacedName{Name: isvc.Namespace}, &ns); err != nil {
		return nil, err
	}

	nsLabels := labels.Set(ns.Labels)

	var applicable []inferencev1alpha1.GPUQuota
	for _, q := range quotaList.Items {
		if q.Spec.NamespaceRef == isvc.Namespace {
			applicable = append(applicable, q)
			continue
		}
		if q.Spec.Selector != nil {
			selector, err := metav1.LabelSelectorAsSelector(q.Spec.Selector)
			if err != nil {
				// Malformed selector: skip this quota rather than failing the
				// entire validation. The reconciler will surface this as a
				// status condition.
				continue
			}
			if selector.Matches(nsLabels) {
				applicable = append(applicable, q)
			}
		}
	}

	return applicable, nil
}

// decide evaluates a single InferenceService against a single GPUQuota using
// the pure quota.Decide function. It computes current usage by listing
// InferenceServices in the quota's scope and summing their GPU allocations.
func (v *InferenceServiceQuotaValidator) decide(ctx context.Context, q inferencev1alpha1.GPUQuota, isvc *inferencev1alpha1.InferenceService) (bool, string) {
	currentUsage, err := v.currentUsage(ctx, q, isvc)
	if err != nil {
		// If we cannot read current usage, deny to be safe.
		return false, fmt.Sprintf("failed to read current usage: %v", err)
	}

	incoming := quota.Incoming{
		GPUCount:  gpuCount(isvc),
		VRAMBytes: 0, // InferenceService has no VRAM field (mirrors #1093).
		Priority:  isvc.Spec.Priority,
	}

	// CostBudgetRef integration is a documented follow-up; for now we always
	// pass costBudgetBreached=false.
	return quota.Decide(q.Spec, currentUsage, incoming, false)
}

// currentUsage lists InferenceServices in the quota's scope and sums their
// GPU allocations. When called for an update, the incoming object is excluded
// from the sum (it is already stored).
func (v *InferenceServiceQuotaValidator) currentUsage(ctx context.Context, q inferencev1alpha1.GPUQuota, isvc *inferencev1alpha1.InferenceService) (quota.Usage, error) {
	var isvcList inferencev1alpha1.InferenceServiceList

	if q.Spec.NamespaceRef != "" {
		// Namespace-scoped quota: only list ISVCs in that namespace.
		if err := v.Client.List(ctx, &isvcList, client.InNamespace(q.Spec.NamespaceRef)); err != nil {
			return quota.Usage{}, err
		}
	} else if q.Spec.Selector != nil {
		// Selector-scoped quota: list ISVCs across all namespaces.
		if err := v.Client.List(ctx, &isvcList); err != nil {
			return quota.Usage{}, err
		}
	} else {
		// No scope defined (should not happen due to CRD validation, but be safe).
		return quota.Usage{}, nil
	}

	var usage quota.Usage
	for _, i := range isvcList.Items {
		// Exclude the incoming object from the sum (it is already stored on
		// update, so including it would double-count).
		if i.Namespace == isvc.Namespace && i.Name == isvc.Name {
			continue
		}
		usage.GPUCount += gpuCount(&i)
	}

	return usage, nil
}

// gpuCount returns the GPU count for an InferenceService, defaulting to 0
// when resources or resources.gpu is nil. It multiplies by replicas
// (defaulting to 1 if nil) to account for total GPU usage across all pods.
func gpuCount(isvc *inferencev1alpha1.InferenceService) int32 {
	if isvc.Spec.Resources == nil {
		return 0
	}
	replicas := int32(1)
	if isvc.Spec.Replicas != nil {
		replicas = *isvc.Spec.Replicas
	}
	return isvc.Spec.Resources.GPU * replicas
}
