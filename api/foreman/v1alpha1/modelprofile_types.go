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

// ModelProfileSpec is reusable, per-model harness tuning layered onto an
// Agent at task dispatch via Agent.spec.modelProfileRef. It carries a
// system-prompt addendum, stuck-loop-detection overrides, and a
// forcing-phase read restriction, so model-specific behavioral tuning can
// be defined once and curated (e.g. an "ornith" profile) instead of being
// hand-set on every Agent.
type ModelProfileSpec struct {
	// SystemPromptAddendum is appended (never substituted) to the
	// referencing Agent's systemPrompt at dispatch, separated by a blank
	// line. Use it to damp model-specific artifacts (e.g. an over-gating
	// model that stalls on fully-authorized work). Empty is a no-op.
	// +optional
	SystemPromptAddendum string `json:"systemPromptAddendum,omitempty"`

	// StuckLoopDetection overrides the stuck-loop detector thresholds, but
	// only for Agents that reference this profile AND do not set their own
	// spec.stuckLoopDetection (the Agent's own block wins wholesale).
	// Non-zero fields override the harness default; zero fields inherit it.
	// +optional
	StuckLoopDetection *StuckLoopDetectionSpec `json:"stuckLoopDetection,omitempty"`

	// RestrictReadsInForcingPhase, when true, makes the EditFreeStreak
	// forcing phase also drop read_file from the advertised tool set (it
	// normally drops only grep/bash). This forces a thrash-prone model
	// that re-reads instead of editing to actually edit. Default false
	// preserves the standard forcing-phase behavior.
	// +optional
	RestrictReadsInForcingPhase bool `json:"restrictReadsInForcingPhase,omitempty"`
}

// ModelProfileStatus is the observed state. The MVP has no reconciler;
// status is reserved for a future controller.
type ModelProfileStatus struct {
	// ObservedGeneration is the most recent generation observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=mp
// +kubebuilder:printcolumn:name="Reads-Restricted",type=boolean,JSONPath=`.spec.restrictReadsInForcingPhase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelProfile is reusable, cluster-scoped per-model harness tuning
// referenced by Agents via spec.modelProfileRef. Cluster-scoped because a
// profile is fleet-wide curated knowledge, not namespace-local state.
type ModelProfile struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the per-model tuning.
	Spec ModelProfileSpec `json:"spec"`

	// status is reserved; the MVP has no reconciler.
	// +optional
	Status ModelProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelProfileList is a list of ModelProfiles.
type ModelProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelProfile{}, &ModelProfileList{})
}
