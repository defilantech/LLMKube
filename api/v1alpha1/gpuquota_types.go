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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GPUQuotaSpec defines the desired state of GPUQuota. Exactly one of
// Selector or NamespaceRef must be set; they are mutually exclusive.
// Selector targets pods by label across one or more namespaces;
// NamespaceRef pins the quota to a single namespace.
// +kubebuilder:validation:XValidation:rule="has(self.selector) != has(self.namespaceRef)",message="exactly one of selector or namespaceRef must be set"
type GPUQuotaSpec struct {
	// Selector matches pods by label across one or more namespaces.
	// Mutually exclusive with NamespaceRef.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`

	// NamespaceRef pins this quota to a single namespace.
	// Mutually exclusive with Selector.
	// +optional
	NamespaceRef string `json:"namespaceRef,omitempty"`

	// GPUCount is the maximum number of GPUs this quota allows.
	// +kubebuilder:validation:Minimum=0
	GPUCount int32 `json:"gpuCount"`

	// VRAMBytes is the maximum total VRAM (bytes) this quota allows.
	// Zero means no VRAM cap.
	// +kubebuilder:validation:Minimum=0
	// +optional
	VRAMBytes int64 `json:"vramBytes,omitempty"`

	// MinPriority is the minimum scheduling priority a pod must declare
	// to be admitted under this quota. Pods with a lower priority are
	// denied regardless of available headroom.
	// +kubebuilder:validation:Enum=critical;high;normal;low;batch
	// +optional
	MinPriority string `json:"minPriority,omitempty"`

	// CostBudgetRef is the name of a CostBudget resource (future CRD)
	// that caps the dollar cost of GPU usage under this quota.
	// +optional
	CostBudgetRef string `json:"costBudgetRef,omitempty"`
}

// GPUQuotaStatus defines the observed state of GPUQuota.
type GPUQuotaStatus struct {
	// UsedGPUCount is the number of GPUs currently allocated under this quota.
	// +optional
	UsedGPUCount int32 `json:"usedGPUCount"`

	// UsedVRAMBytes is the total VRAM (bytes) currently allocated under this quota.
	// +optional
	UsedVRAMBytes int64 `json:"usedVRAMBytes"`

	// AdmissionDenials is the cumulative count of pods denied by this quota.
	// +optional
	AdmissionDenials int64 `json:"admissionDenials"`

	// LastDenial is the timestamp of the most recent admission denial.
	// +optional
	LastDenial *metav1.Time `json:"lastDenial,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="GPU Count",type=integer,JSONPath=`.spec.gpuCount`
// +kubebuilder:printcolumn:name="Used GPUs",type=integer,JSONPath=`.status.usedGPUCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:path=gpuquotas,shortName=gq

// GPUQuota is the Schema for the gpuquotas API. It declares a multi-tenant
// GPU budget that admission logic enforces against InferenceService pods.
type GPUQuota struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPUQuotaSpec   `json:"spec"`
	Status GPUQuotaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GPUQuotaList contains a list of GPUQuota.
type GPUQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUQuota `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GPUQuota{}, &GPUQuotaList{})
}
