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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelPoolSwapPolicy selects how a ModelPool decides which member owns the
// shared GPU slot when demand crosses model boundaries.
// +kubebuilder:validation:Enum=sticky
type ModelPoolSwapPolicy string

const (
	// ModelPoolSwapPolicySticky keeps whichever member is resident resident
	// until a different member is requested. There is no automatic restore of
	// a default member. This is the default and, in v1, the only policy. A
	// priority-based reclaim policy is tracked as a follow-up (PR2).
	ModelPoolSwapPolicySticky ModelPoolSwapPolicy = "sticky"
)

// ModelPool status phases.
const (
	// ModelPoolPhasePending means the pool has been accepted but no member is
	// resident yet (cold pool, or the first activation is in flight).
	ModelPoolPhasePending = "Pending"

	// ModelPoolPhaseReady means exactly one member is resident and serving.
	ModelPoolPhaseReady = "Ready"

	// ModelPoolPhaseSwapping means the pool is mid-swap: the incumbent is
	// draining/unloading before the target loads. No client request is
	// rejected during this window; the router holds cross-model requests open.
	ModelPoolPhaseSwapping = "Swapping"

	// ModelPoolPhaseDegraded means the pool cannot satisfy its invariant, for
	// example a member InferenceService is missing or references are invalid.
	ModelPoolPhaseDegraded = "Degraded"
)

// ModelPool condition types.
const (
	// ConditionModelPoolSlotAllocated is True when exactly one member owns the
	// shared GPU slot and the rest are held at Stopped.
	ConditionModelPoolSlotAllocated = "SlotAllocated"

	// ConditionModelPoolMembersValid is True when every member reference
	// resolves to an existing InferenceService in the pool namespace.
	ConditionModelPoolMembersValid = "MembersValid"

	// ConditionModelPoolSwapDeferred is True while a swap is held because the
	// incumbent is still serving (ReasonPodsBusy) or its idleness cannot be
	// established (ReasonIdleCheckFailed). It mirrors the InferenceService
	// rollout RolloutDeferred condition; unlike a rollout there is no timeout
	// that force-unloads the incumbent, so a deferred swap stays deferred until
	// the incumbent is genuinely idle.
	ConditionModelPoolSwapDeferred = "SwapDeferred"

	// ConditionModelPoolMetalSupported is False when a member is metal-backed.
	// Metal hosts have no device-plugin GPU gating, so unload-before-load would
	// have to be enforced by the metal-agent; that is a follow-up and v1 is
	// k8s-GPU-only. The pool refuses to manage replicas in this state.
	ConditionModelPoolMetalSupported = "MetalSupported"
)

// ModelPoolMember names one InferenceService that shares the pool's GPU slot.
type ModelPoolMember struct {
	// InferenceServiceRef references the member InferenceService by name. The
	// InferenceService must live in the same namespace as the ModelPool.
	// +kubebuilder:validation:Required
	InferenceServiceRef corev1.LocalObjectReference `json:"inferenceServiceRef"`
}

// ModelPoolSpec defines the desired state of a ModelPool.
//
// A ModelPool names a set of member InferenceServices that share a single
// exclusive GPU slot on one node. At most one member is Ready at a time; the
// controller drains and unloads the incumbent (freeing VRAM) before the next
// member loads. The router-proxy fronts the members and activates a stopped
// member on demand, so the pool presents many models behind one slot with a
// sticky, anti-thrash swap policy.
type ModelPoolSpec struct {
	// NodeSelector pins the shared slot to a node (or node class). All members
	// are expected to schedule onto the same node so they contend for the same
	// device memory. Mirrors a standard Kubernetes node selector.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// GPU is the number of GPUs the shared slot occupies. Informational for the
	// pool invariant (members declare their own resource requests); recorded so
	// operators can see the slot size at a glance.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	GPU int32 `json:"gpu,omitempty"`

	// SwapPolicy selects how the slot owner is chosen when demand crosses model
	// boundaries. "sticky" (default) keeps the incumbent until a different
	// member is requested. It is the only policy in v1; a priority-based reclaim
	// policy is a planned follow-up.
	// +kubebuilder:default=sticky
	// +optional
	SwapPolicy ModelPoolSwapPolicy `json:"swapPolicy,omitempty"`

	// Members lists the InferenceServices that share the pool's GPU slot. Each
	// member is an ordinary single-model InferenceService with its own image,
	// context size, and runtime.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	Members []ModelPoolMember `json:"members"`

	// Default names the member to warm on a cold pool (before any request has
	// picked an owner). When empty, the first request to arrive selects the
	// owner. Must match one of the members' InferenceServiceRef names.
	// +optional
	Default string `json:"default,omitempty"`
}

// ModelPoolMemberStatus reports the observed state of one pool member.
type ModelPoolMemberStatus struct {
	// Name is the member InferenceService name.
	Name string `json:"name"`

	// Phase mirrors the member InferenceService status.phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Resident is true when this member currently owns the shared GPU slot.
	// +optional
	Resident bool `json:"resident,omitempty"`
}

// ModelPoolStatus defines the observed state of a ModelPool.
type ModelPoolStatus struct {
	// Phase is the pool lifecycle phase: Pending, Ready, Swapping, or Degraded.
	// +kubebuilder:validation:Enum=Pending;Ready;Swapping;Degraded
	// +optional
	Phase string `json:"phase,omitempty"`

	// ResidentMember is the member that currently owns the shared GPU slot, or
	// empty when the pool is cold (no member resident).
	// +optional
	ResidentMember string `json:"residentMember,omitempty"`

	// PendingMember is the member being loaded during a swap, or empty when the
	// pool is not mid-swap.
	// +optional
	PendingMember string `json:"pendingMember,omitempty"`

	// Members reports the observed state of each pool member.
	// +listType=map
	// +listMapKey=name
	// +optional
	Members []ModelPoolMemberStatus `json:"members,omitempty"`

	// ObservedGeneration is the spec generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the current state of the ModelPool resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mpool
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=`.spec.swapPolicy`
// +kubebuilder:printcolumn:name="Resident",type=string,JSONPath=`.status.residentMember`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelPool is the Schema for the modelpools API.
type ModelPool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of ModelPool
	// +required
	Spec ModelPoolSpec `json:"spec"`

	// status defines the observed state of ModelPool
	// +optional
	Status ModelPoolStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ModelPoolList contains a list of ModelPool.
type ModelPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelPool{}, &ModelPoolList{})
}
