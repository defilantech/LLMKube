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

// LoRAAdapterSpec defines a single SGLang LoRA adapter that should be
// loaded into a target InferenceService without restarting its pod. The
// controller translates this resource into a POST against SGLang's
// /load_lora_adapter HTTP endpoint (singular `lora_adapter`, no /v1
// prefix, as of SGLang v0.5.15) and a finalizer-driven
// /unload_lora_adapter on delete. See
// https://github.com/sgl-project/sglang/blob/v0.5.15/python/sglang/srt/entrypoints/http_server.py
// for the wire format.
type LoRAAdapterSpec struct {
	// InferenceServiceRef names the InferenceService this adapter should
	// be loaded into. The reconciler resolves it; if it points at a
	// non-sglang runtime, Available=False with reason RuntimeMismatch.
	// +kubebuilder:validation:Required
	InferenceServiceRef LocalInferenceServiceReference `json:"inferenceServiceRef"`

	// Name is the SGLang-side adapter handle. Operators quote this in
	// inference requests to pick the adapter. Must be unique within the
	// target InferenceService's loaded set; the SGLang HTTP API rejects
	// duplicates with 409.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]*$`
	// +required
	Name string `json:"name"`

	// Path is the path on disk inside the SGLang container where the
	// adapter weights are mounted. SGLang reads from this path when
	// /load_lora_adapter is invoked, so it must be reachable from the
	// inference pod (typically via a PVC exposed in
	// InferenceService.spec.extraVolumes) at the path the operator
	// chose. The controller does not auto-mount.
	// +kubebuilder:validation:MinLength=1
	// +required
	Path string `json:"path"`
}

// LocalInferenceServiceReference is a namespaced pointer at an
// InferenceService. Kept narrow so it can never accidentally refer to a
// cluster-scoped resource or hang a cross-cluster reference.
type LocalInferenceServiceReference struct {
	// Name is the name of the target InferenceService. Must exist in
	// Namespace (or be omitted to default to this resource's namespace).
	// +kubebuilder:validation:Required
	// +required
	Name string `json:"name"`

	// Namespace is the namespace of the target InferenceService. When
	// omitted, defaults to the LoRAAdapter's namespace. Cross-namespace
	// references are allowed; the controller only requires that the
	// operator has RBAC to GET the referenced InferenceService.
	// +optional
	Namespace *string `json:"namespace,omitempty"`
}

// LoRAAdapterStatus reports observed load state for an adapter.
type LoRAAdapterStatus struct {
	// LoadedPath is the path SGLang has accepted for this adapter. Set
	// after a successful POST /load_lora_adapter response. Empty before
	// the first successful load.
	// +optional
	LoadedPath string `json:"loadedPath,omitempty"`

	// LastLoadedAt is when the adapter was last successfully loaded
	// into SGLang. Re-loads (after a SGLang pod restart, for example)
	// update this field.
	// +optional
	LastLoadedAt *metav1.Time `json:"lastLoadedAt,omitempty"`

	// conditions represent the current state of the LoRAAdapter
	// resource.
	//
	// Standard condition types include:
	// - "Available": the spec is well-formed and target InferenceService
	//   exists with runtime == sglang.
	// - "Loaded": the adapter has been POSTed to SGLang and SGLang
	//   reported success (or, on teardown, acknowledged the unload).
	// - "Error": a reconcile step failed; the message carries the
	//   underlying reason (runtime mismatch, HTTP failure, etc.).
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.inferenceServiceRef.name`
// +kubebuilder:printcolumn:name="Adapter",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Loaded",type=string,JSONPath=`.status.conditions[?(@.type=="Loaded")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:shortName=lora

// LoRAAdapter is the Schema for the loraadapters API.
type LoRAAdapter struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of LoRAAdapter
	// +required
	Spec LoRAAdapterSpec `json:"spec"`

	// status defines the observed state of LoRAAdapter
	// +optional
	Status LoRAAdapterStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// LoRAAdapterList contains a list of LoRAAdapter.
type LoRAAdapterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LoRAAdapter `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LoRAAdapter{}, &LoRAAdapterList{})
}
