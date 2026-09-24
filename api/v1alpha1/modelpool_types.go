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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelPoolSwapPolicy selects how a ModelPool decides which member owns the
// shared GPU slot when demand crosses model boundaries.
// +kubebuilder:validation:Enum=sticky;reclaim;reclaim-down
type ModelPoolSwapPolicy string

const (
	// ModelPoolSwapPolicySticky keeps whichever member is resident resident
	// until a different member is requested. There is no automatic restore of
	// a default member. This is the default policy.
	ModelPoolSwapPolicySticky ModelPoolSwapPolicy = "sticky"

	// ModelPoolSwapPolicyReclaim behaves like sticky for cross-model demand but
	// additionally returns the slot to spec.default once the resident
	// non-default member has been continuously idle for spec.reclaimAfter. It
	// lets a background default model share one GPU with an on-demand member:
	// the on-demand member's requests swap it in, an interactive workload rides
	// whichever member is warm (with router poolActivation IfIdle), and the slot
	// returns to the default on its own once the burst subsides, without any
	// workload forcing a disruptive swap. With no spec.default it behaves like
	// sticky.
	ModelPoolSwapPolicyReclaim ModelPoolSwapPolicy = "reclaim"

	// ModelPoolSwapPolicyReclaimDown behaves like sticky for cross-model demand
	// but releases the slot once the resident member has been continuously idle
	// for spec.reclaimAfter: the resident is drained and every member is held at
	// zero replicas until the next request activates one. It frees the GPU for
	// other tenants of a capacity-constrained cluster. It is an explicit opt-in,
	// distinct from reclaim with no default (which keeps the resident), and
	// cannot be combined with spec.default, which would warm the released slot
	// straight back up.
	ModelPoolSwapPolicyReclaimDown ModelPoolSwapPolicy = "reclaim-down"
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

	// ConditionModelPoolExternallyScaled is True when a non-resident member was
	// scaled up by a field manager other than LLMKube's own (#2302), typically a
	// GitOps tool restoring spec.replicas on every sync. The scale-up still
	// activates the member; the condition names the member and the manager, and
	// clears after a quiet period with no further outside scale-up.
	ConditionModelPoolExternallyScaled = "ExternallyScaled"
)

// Field managers LLMKube writes a pool member's spec.replicas under. The pool
// reads a member's managedFields to tell its own and the router's scale-ups
// from anyone else's, so both writers pass these names explicitly rather than
// inheriting a field manager from the binary's user agent.
const (
	// ModelPoolFieldManager is the ModelPool controller's drain and scale-up.
	ModelPoolFieldManager = "llmkube-modelpool"

	// RouterProxyFieldManager is the router-proxy's on-demand activation.
	RouterProxyFieldManager = "llmkube-router-proxy"
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
//
// +kubebuilder:validation:XValidation:rule="!(has(self.swapPolicy) && self.swapPolicy == 'reclaim-down' && has(self.default) && size(self.default) > 0)",message="swapPolicy reclaim-down releases the slot to zero and cannot be combined with default, which would warm it straight back up; use swapPolicy reclaim to return the slot to default instead"
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
	// member is requested. "reclaim" additionally returns the slot to
	// spec.default after the resident non-default member has been idle for
	// spec.reclaimAfter. "reclaim-down" instead releases the slot (every member
	// at zero replicas) after the resident member has been idle for
	// spec.reclaimAfter; it requires spec.default to be empty.
	// +kubebuilder:default=sticky
	// +optional
	SwapPolicy ModelPoolSwapPolicy `json:"swapPolicy,omitempty"`

	// Members lists the InferenceServices that share the pool's GPU slot. Each
	// member is an ordinary single-model InferenceService with its own image,
	// context size, and runtime. The pool owns each member's spec.replicas:
	// leave it out of GitOps manifests, or every sync that restores it is read
	// as a request for that member (docs/operations/gpu-sharing.md, "GitOps").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	Members []ModelPoolMember `json:"members"`

	// Default names the member to warm on a cold pool (before any request has
	// picked an owner). When empty, the first request to arrive selects the
	// owner. Must match one of the members' InferenceServiceRef names.
	// +optional
	Default string `json:"default,omitempty"`

	// SwapBudget bounds how long the router holds a cross-model request open
	// while it drains the incumbent and cold-loads the target member. It is
	// deliberately decoupled from the router's response-header (generation)
	// timeout: a large model can take minutes to load without forcing operators
	// to loosen the per-request generation cap for every request. When the
	// budget elapses before the target becomes resident, the held request is
	// failed with 503 + Retry-After (the caller can retry; the now-warm member
	// serves the next request). Defaults to 300s.
	// +kubebuilder:default="300s"
	// +optional
	SwapBudget *metav1.Duration `json:"swapBudget,omitempty"`

	// ReclaimAfter is how long the resident non-default member must be
	// continuously idle before the slot is reclaimed for spec.default. Only
	// consulted when SwapPolicy is "reclaim" and Default is set, or when
	// SwapPolicy is "reclaim-down", where it is how long the resident member must
	// be idle before the slot is released to zero. It MUST be set
	// comfortably longer than the on-demand member's typical inter-request gap:
	// a value shorter than that gap trades cross-model request coalescing for
	// load-side thrash, reclaiming the default only to swap it straight back
	// out on the next on-demand request. Defaults to 300s.
	// +kubebuilder:default="300s"
	// +optional
	ReclaimAfter *metav1.Duration `json:"reclaimAfter,omitempty"`
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

// ModelPoolExternalScale names a member scale-up made outside LLMKube.
type ModelPoolExternalScale struct {
	// Member is the member InferenceService that was scaled up.
	Member string `json:"member"`

	// Manager is the field manager that owns the member's spec.replicas, as
	// recorded in its managedFields (for example kustomize-controller or
	// kubectl).
	Manager string `json:"manager"`

	// MemberGeneration is the member's metadata.generation when the scale-up
	// was observed. With Member and Manager it identifies one write.
	// +optional
	MemberGeneration int64 `json:"memberGeneration,omitempty"`

	// ObservedAt is when the pool first observed this scale-up.
	ObservedAt metav1.Time `json:"observedAt"`
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

	// ResidentIdleSince is when the resident non-default member was first
	// observed idle under the "reclaim" or "reclaim-down" swap policy. It is
	// cleared when the member serves a request again, when spec.default becomes
	// resident, or when the slot is released, and is used to time the reclaim of
	// the slot back to spec.default (or its release to zero).
	// +optional
	ResidentIdleSince *metav1.Time `json:"residentIdleSince,omitempty"`

	// ExternalScale records the last scale-up of a non-resident member by a
	// field manager outside LLMKube. It identifies the write already reported,
	// so the same scale-up seen on a later pass is not reported again, and times
	// the ExternallyScaled condition's quiet period.
	// +optional
	ExternalScale *ModelPoolExternalScale `json:"externalScale,omitempty"`

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
