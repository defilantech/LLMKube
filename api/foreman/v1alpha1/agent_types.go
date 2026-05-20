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

// AgentRole names the pipeline role this Agent fulfills. Free-form in
// kubebuilder validation terms (an enum), but extensible: v0.2 may add
// "judge" or "planner-helper" without breaking the v0.1 set.
// +kubebuilder:validation:Enum=coder;verifier;reviewer;planner
type AgentRole string

const (
	// AgentRoleCoder writes the change: reads the issue, edits the repo,
	// commits, pushes. Currently runs on the M5 Max in v0.1.
	AgentRoleCoder AgentRole = "coder"
	// AgentRoleVerifier runs the project's gate (fmt/vet/lint/test). v0.1
	// pins this to ShadowStack for cross-arch coverage. No LLM; the
	// run_gate_job tool drives the work end-to-end (M4).
	AgentRoleVerifier AgentRole = "verifier"
	// AgentRoleReviewer reads the diff + gate verdict and emits a
	// structured review (approve / request-changes + rationale). v0.1
	// pins this to the Mac Studio (M5).
	AgentRoleReviewer AgentRole = "reviewer"
	// AgentRolePlanner decomposes a Workload intent into a pipeline of
	// AgenticTasks. v0.1 (M6) uses a frontier model; the Agent shape is
	// the same so future on-prem planners drop in unchanged.
	AgentRolePlanner AgentRole = "planner"
)

// AgentSpec is the reusable role definition referenced by AgenticTasks
// via spec.agentRef. An Agent bundles the system prompt, tool whitelist,
// model endpoint, and required host capability for one pipeline step.
// The Agent itself owns no per-task state.
type AgentSpec struct {
	// Role discriminates the pipeline step this Agent fulfills. Currently
	// informational (used for observability + future learning routing);
	// the scheduler does not branch on it in v0.1.
	// +kubebuilder:validation:Required
	Role AgentRole `json:"role"`

	// Model is a free-form identifier for the model this Agent expects
	// the referenced InferenceService to serve. Cosmetic in v0.1 (the
	// runtime endpoint comes from InferenceServiceRef); v0.2 will use it
	// to validate the InferenceService is actually serving the expected
	// model before the agent loop dispatches.
	// +optional
	Model string `json:"model,omitempty"`

	// InferenceServiceRef names the LLMKube InferenceService in the same
	// namespace that serves this Agent's model. The foreman-agent
	// resolves it to a base URL ("http://<svc>.<ns>.svc:<port>/v1") at
	// task time. v0.1 uses inferenceServiceRef exclusively; v0.2 may
	// introduce an external-provider URL form.
	// +kubebuilder:validation:Required
	InferenceServiceRef corev1.LocalObjectReference `json:"inferenceServiceRef"`

	// SystemPrompt is the literal system message the Agent sees on every
	// run. Inline (no ConfigMap indirection) for v0.1; ConfigMap
	// indirection is a non-breaking v0.2 addition. Required and non-empty.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SystemPrompt string `json:"systemPrompt"`

	// Temperature is the sampling temperature passed verbatim on each
	// chat-completions request. String-typed to dodge float-on-CRD
	// complications and to allow exact roundtrip; parsed as a float at
	// loop start. Expected range "0.0" through "2.0"; the loop rejects
	// anything outside.
	// +kubebuilder:validation:Pattern=`^[0-2](\.[0-9]+)?$`
	// +optional
	Temperature *string `json:"temperature,omitempty"`

	// MaxTurns caps the agent loop's iterations before it gives up. Each
	// turn is one chat-completions call plus its tool dispatches.
	// +kubebuilder:default=50
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=500
	// +optional
	MaxTurns int32 `json:"maxTurns,omitempty"`

	// MaxRetries bounds how many times the loop retries a single turn on
	// recoverable errors (notably llama.cpp #22072 truncated tool_call
	// argument JSON). Bounded exponential backoff with jitter.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	// +optional
	MaxRetries int32 `json:"maxRetries,omitempty"`

	// RequestTimeoutSeconds bounds a single chat-completions HTTP
	// request. Long-context decode on a local model can be slow; default
	// is generous.
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=1
	// +optional
	RequestTimeoutSeconds int32 `json:"requestTimeoutSeconds,omitempty"`

	// BashTimeoutSeconds bounds a single bash tool invocation. The bash
	// tool runs under "sh -c" with cwd pinned to the workspace and a
	// scrubbed environment; this cap stops a runaway test or grep from
	// stalling the whole agent loop.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=1
	// +optional
	BashTimeoutSeconds int32 `json:"bashTimeoutSeconds,omitempty"`

	// Tools is the tool whitelist surfaced to the model on every turn.
	// Unknown names are rejected at agent startup so a typo in an Agent
	// CR fails loud rather than silently disabling a tool.
	// +kubebuilder:validation:MinItems=1
	Tools []string `json:"tools"`

	// RequiredCapability filters which FleetNodes can serve this Agent.
	// When an AgenticTask references this Agent via spec.agentRef, the
	// scheduler uses this RequiredCapability and ignores the task's own
	// spec.requiredCapability. Single source of truth.
	// +optional
	RequiredCapability RequiredCapability `json:"requiredCapability,omitempty"`
}

// AgentStatus is the reconciler's view of the Agent's readiness. M3 keeps
// the reconciler as a stub (the two condition writers stay at scheduler +
// watcher); v0.2 will promote validation results to a Ready condition.
type AgentStatus struct {
	// ObservedGeneration is the most recent .metadata.generation the
	// reconciler has processed. Lets clients tell at a glance whether a
	// spec edit has been observed yet.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions track standard signals. M3 reserves "Ready" and
	// "Validated"; the reconciler does not write either in M3.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ag
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`
// +kubebuilder:printcolumn:name="InferenceService",type=string,JSONPath=`.spec.inferenceServiceRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Agent is the reusable role definition referenced by AgenticTasks via
// spec.agentRef. An Agent bundles the system prompt, tool whitelist,
// model endpoint, and required host capability for one pipeline step
// (coder, verifier, reviewer, planner). Multiple tasks can reference
// the same Agent; the Agent itself owns no per-task state.
//
// Namespaced: a transcript ConfigMap produced by a task referencing this
// Agent can be owner-ref'd cleanly, and namespaces can declare their own
// role-specialized variants without name collisions.
type Agent struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the role definition.
	Spec AgentSpec `json:"spec"`

	// status is the reconciler's observed view. M3 reconciler is a stub
	// and does not write to it; status fields are reserved for v0.2.
	// +optional
	Status AgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentList is a list of Agents.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}
