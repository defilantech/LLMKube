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
	// pins this to a verifier-tagged node for cross-arch coverage. No LLM; the
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

// AgentProvider names the model-serving backend the executor dispatches
// to. v0.2 introduces this enum so reviewer Agents can route to an
// OpenAI-compatible cloud proxy (typically a LiteLLM gateway hitting
// Anthropic / OpenAI / Bedrock) without changing the agent loop. The
// default "local" preserves the v0.1 behavior of resolving an
// in-cluster InferenceService.
//
// Air-gapped orgs MUST keep cloud-proxy Agents out of dispatch; both
// the operator-level kill switch (--allow-cloud-providers flag, chart
// value foreman.allowCloudProviders) and the per-Workload opt-in
// (Workload.spec.allowCloudReviewers) gate this.
// +kubebuilder:validation:Enum=local;cloud-proxy
type AgentProvider string

const (
	// AgentProviderLocal (default) dispatches via the Agent's
	// InferenceServiceRef. The v0.1 path; nothing leaves the cluster.
	AgentProviderLocal AgentProvider = "local"
	// AgentProviderCloudProxy dispatches via providerConfig.baseURL to
	// an OpenAI-compatible HTTP endpoint. The endpoint is typically a
	// LiteLLM proxy (e.g. foundation-router:4000) that translates to
	// Anthropic / OpenAI / Bedrock. Data leaves the cluster on every
	// call; subject to the operator + workload sovereignty toggles.
	AgentProviderCloudProxy AgentProvider = "cloud-proxy"
)

// ProviderConfig configures a non-local AgentProvider. Required when
// AgentSpec.Provider is "cloud-proxy"; ignored when Provider is "local"
// or unset.
type ProviderConfig struct {
	// BaseURL is the OpenAI-compatible HTTP endpoint the executor
	// dispatches chat-completions requests to. The /chat/completions
	// path is appended; supply the /v1 prefix (e.g.
	// "http://foundation-router.lan:4000/v1"). Required for
	// cloud-proxy.
	// +kubebuilder:validation:MinLength=1
	// +optional
	BaseURL string `json:"baseURL,omitempty"`

	// Model is the identifier the proxy expects in the request body
	// (e.g. "claude-sonnet-4-6", "gpt-4o", "anthropic/claude-sonnet-4-6"
	// when LiteLLM is in front). Required for cloud-proxy; overrides
	// AgentSpec.Model on the wire while AgentSpec.Model remains the
	// human-readable handle.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Model string `json:"model,omitempty"`

	// APIKeySecretRef references a Secret carrying the bearer token
	// the executor sends as the Authorization header. Optional: when
	// nil, the proxy is dialed without auth (LAN-only LiteLLM behind
	// a network policy is a common case).
	// +optional
	APIKeySecretRef *corev1.SecretKeySelector `json:"apiKeySecretRef,omitempty"`
}

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

	// Provider selects the model-serving backend. Default "local" keeps
	// the v0.1 behavior (resolve InferenceServiceRef). v0.2 adds
	// "cloud-proxy" for OpenAI-compatible HTTP endpoints (typically a
	// LiteLLM gateway); see ProviderConfig. Cloud-proxy Agents are
	// subject to the foreman-operator's --allow-cloud-providers kill
	// switch and to per-Workload spec.allowCloudReviewers gating.
	// +kubebuilder:default=local
	// +optional
	Provider AgentProvider `json:"provider,omitempty"`

	// ProviderConfig configures non-local providers. Required when
	// Provider is "cloud-proxy"; ignored for "local".
	// +optional
	ProviderConfig *ProviderConfig `json:"providerConfig,omitempty"`

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
	//
	// Optional: an Agent with an empty InferenceServiceRef is a
	// *deterministic* Agent (no LLM in the loop). The executor skips
	// model dispatch and runs the agent's first non-terminal tool
	// directly. Used for the gate role (verify role), which only needs
	// to submit a Kubernetes Job and read the verdict.
	// +optional
	InferenceServiceRef corev1.LocalObjectReference `json:"inferenceServiceRef,omitempty"`

	// SystemPrompt is the literal system message the Agent sees on every
	// run. Inline (no ConfigMap indirection) for v0.1; ConfigMap
	// indirection is a non-breaking v0.2 addition.
	//
	// Optional: only meaningful when InferenceServiceRef is set. A
	// deterministic Agent (empty InferenceServiceRef) has no LLM to
	// read this; the executor ignores the field in that path.
	// +optional
	SystemPrompt string `json:"systemPrompt,omitempty"`

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

	// ContextWindowTokens is the soft token budget for the wire payload
	// the loop sends on every turn. When the running message list
	// approximately exceeds this size, older tool result messages are
	// masked to a one-line header until the budget is met. The persisted
	// transcript ConfigMap still captures the FULL (unmasked) trajectory
	// for review. Zero uses the executor default (32768 tokens).
	//
	// v0.3 #558: observation masking. The token estimate is an
	// approximation (~chars/4); precise tokenization is not required
	// for the masking decision. Pairs with ObservationWindowTurns.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ContextWindowTokens int32 `json:"contextWindowTokens,omitempty"`

	// ObservationWindowTurns is the number of most-recent tool result
	// messages kept in full before older ones are masked, regardless of
	// the token budget. Acts as a floor (the model always sees this
	// many recent tool outputs verbatim). Zero uses the executor
	// default (3).
	//
	// v0.3 #558: observation masking. Pairs with ContextWindowTokens;
	// the floor wins over the budget when they conflict.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	// +optional
	ObservationWindowTurns int32 `json:"observationWindowTurns,omitempty"`

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
