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
	"k8s.io/apimachinery/pkg/runtime"
)

// AgenticTaskKind is the unit of work the task performs. Each kind has a
// payload shape, scheduler routing, and lifecycle.
// +kubebuilder:validation:Enum=issue-fix;verify;freeform
type AgenticTaskKind string

const (
	// AgenticTaskKindIssueFix runs an agent against a GitHub issue: read the
	// issue, edit the repo, run the verification, commit (DCO), push a branch.
	AgenticTaskKindIssueFix AgenticTaskKind = "issue-fix"
	// AgenticTaskKindVerify runs the project's gate (fmt/vet/lint/test +
	// codegen sync) against a pushed branch. Typically scheduled by the
	// controller as a child of a Succeeded issue-fix task.
	AgenticTaskKindVerify AgenticTaskKind = "verify"
	// AgenticTaskKindFreeform passes an arbitrary prompt to a named agent.
	AgenticTaskKindFreeform AgenticTaskKind = "freeform"
)

// AgenticTaskAccelerator pins which accelerator family a task needs from the
// node that runs it. "any" lets the scheduler pick from any Ready FleetNode.
// +kubebuilder:validation:Enum=metal;cuda;any
type AgenticTaskAccelerator string

// RequiredCapability tells the scheduler which FleetNodes can serve this task.
// The scheduler matches each field against FleetNode.status.capability;
// unset fields are unconstrained.
type RequiredCapability struct {
	// MinRAMGB is the minimum available RAM the node must advertise.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinRAMGB int32 `json:"minRAMGB,omitempty"`

	// MinContextTokens is the minimum context window the node's installed
	// model must support. Set to 0 to leave unconstrained.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinContextTokens int32 `json:"minContextTokens,omitempty"`

	// Accelerator selects an accelerator family. "any" matches any node.
	// +kubebuilder:default=any
	// +optional
	Accelerator AgenticTaskAccelerator `json:"accelerator,omitempty"`

	// NodeSelector is a hard pin: only FleetNodes whose labels match every
	// key are eligible. Used for tasks that must run on a specific node
	// (e.g. verify tasks targeting the gate runner).
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// AgenticTaskPayload is the kind-discriminated work spec. Each field is only
// meaningful for the kinds named in its description.
type AgenticTaskPayload struct {
	// Repo is the "owner/name" GitHub repo. Required for issue-fix and verify.
	// +optional
	Repo string `json:"repo,omitempty"`

	// Issue is the GitHub issue number. Required for issue-fix.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Issue int32 `json:"issue,omitempty"`

	// Branch is the existing branch to gate. Required for verify.
	// +optional
	Branch string `json:"branch,omitempty"`

	// BranchPrefix overrides the branch name prefix on issue-fix tasks
	// (default derived from the issue's labels via conventional commit
	// prefixes: fix/, feat/, chore/, etc.).
	// +optional
	BranchPrefix string `json:"branchPrefix,omitempty"`

	// Prompt is the agent input. Required for freeform.
	// +optional
	Prompt string `json:"prompt,omitempty"`

	// Agent is the named agent to invoke. Required for freeform; defaults
	// to "issue-fixer" for issue-fix and "verify" for verify.
	// +optional
	Agent string `json:"agent,omitempty"`
}

// AgenticTaskSpec defines the desired state of an AgenticTask.
type AgenticTaskSpec struct {
	// Kind selects the work type and payload shape.
	// +kubebuilder:validation:Required
	Kind AgenticTaskKind `json:"kind"`

	// ModelRef names the Model the agent should use. Optional; the
	// scheduler can pick a default based on RequiredCapability.
	// +optional
	ModelRef string `json:"modelRef,omitempty"`

	// AgentRef references an Agent (in the same namespace) that runs
	// this step. When set, the scheduler resolves the Agent and uses
	// Agent.spec.requiredCapability for capability matching, ignoring
	// this task's own RequiredCapability. Empty preserves the M2 path
	// in which the task's RequiredCapability is authoritative.
	// +optional
	AgentRef *corev1.LocalObjectReference `json:"agentRef,omitempty"`

	// RequiredCapability filters which FleetNodes can serve this task.
	// Ignored when AgentRef is set (the Agent's RequiredCapability wins).
	// +optional
	RequiredCapability RequiredCapability `json:"requiredCapability,omitempty"`

	// Payload is the kind-discriminated work spec.
	// +kubebuilder:validation:Required
	Payload AgenticTaskPayload `json:"payload"`

	// TimeoutSeconds bounds the agent's run time. Zero uses the operator's
	// default (2700, matching the autofix pipeline's value).
	// +kubebuilder:validation:Minimum=0
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// DependsOn lists AgenticTasks (by name in the same namespace) that
	// must reach Succeeded before this task is dispatched. v0.1 uses this
	// only to chain verify tasks behind their parent issue-fix.
	// +optional
	DependsOn []string `json:"dependsOn,omitempty"`

	// Priority is a hint for the scheduler when many tasks are Pending.
	// Higher values dispatch first. v0.1 is FIFO and ignores priority.
	// +optional
	Priority int32 `json:"priority,omitempty"`
}

// AgenticTaskPhase is the lifecycle state of a task.
// +kubebuilder:validation:Enum=Pending;Scheduled;Running;Verifying;Succeeded;Failed
type AgenticTaskPhase string

const (
	AgenticTaskPhasePending   AgenticTaskPhase = "Pending"
	AgenticTaskPhaseScheduled AgenticTaskPhase = "Scheduled"
	AgenticTaskPhaseRunning   AgenticTaskPhase = "Running"
	AgenticTaskPhaseVerifying AgenticTaskPhase = "Verifying"
	AgenticTaskPhaseSucceeded AgenticTaskPhase = "Succeeded"
	AgenticTaskPhaseFailed    AgenticTaskPhase = "Failed"
)

// AgenticTaskVerdict is the final outcome category, distinct from Phase.
// A task can be Succeeded with a NO-GO verdict (the agent legitimately
// declined to fix the issue) or Failed with no verdict at all (the run
// timed out before producing a verdict).
// +kubebuilder:validation:Enum=GO;NO-GO;INCOMPLETE;GATE-PASS;GATE-FAIL;GATE-ERROR
type AgenticTaskVerdict string

const (
	// AgenticTaskVerdictGo signals the agent finished and produced a
	// change it stands behind: edit applied, branch pushed, ready for
	// downstream gating.
	AgenticTaskVerdictGo AgenticTaskVerdict = "GO"
	// AgenticTaskVerdictNoGo signals the agent legitimately declined to
	// produce a change (e.g. the issue is already fixed, or the scope is
	// out of reach for this agent kind). Distinct from Failed.
	AgenticTaskVerdictNoGo AgenticTaskVerdict = "NO-GO"
	// AgenticTaskVerdictIncomplete signals the agent did not produce a
	// terminal verdict before its run ended (timeout, mid-loop crash,
	// upstream cascade-fail).
	AgenticTaskVerdictIncomplete AgenticTaskVerdict = "INCOMPLETE"
	// AgenticTaskVerdictGatePass is the gate agent's positive outcome:
	// every check (fmt/vet/lint/test) passed.
	AgenticTaskVerdictGatePass AgenticTaskVerdict = "GATE-PASS"
	// AgenticTaskVerdictGateFail is the gate agent's negative outcome:
	// at least one check failed but the gate itself ran cleanly.
	AgenticTaskVerdictGateFail AgenticTaskVerdict = "GATE-FAIL"
	// AgenticTaskVerdictGateError signals the gate runner itself failed
	// to execute (infrastructure issue), distinct from a check failure.
	AgenticTaskVerdictGateError AgenticTaskVerdict = "GATE-ERROR"
)

// AgenticTaskStatus defines the observed state of an AgenticTask.
type AgenticTaskStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase AgenticTaskPhase `json:"phase,omitempty"`

	// AssignedNode is the FleetNode.metadata.name the scheduler routed to.
	// +optional
	AssignedNode string `json:"assignedNode,omitempty"`

	// ClaimedAt is when the FleetAgent on AssignedNode claimed the task.
	// +optional
	ClaimedAt *metav1.Time `json:"claimedAt,omitempty"`

	// StartedAt is when the executor began work.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// FinishedAt is when the executor produced a verdict (success or fail).
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Verdict is the final outcome category.
	// +optional
	Verdict AgenticTaskVerdict `json:"verdict,omitempty"`

	// Result is the structured JSON the agent emitted, validated against the
	// foreman.v1 schema. Opaque to the API server.
	// +optional
	Result *runtime.RawExtension `json:"result,omitempty"`

	// Branch is the pushed branch, set on a successful issue-fix.
	// +optional
	Branch string `json:"branch,omitempty"`

	// CommitSHA is the head commit of Branch.
	// +optional
	CommitSHA string `json:"commitSHA,omitempty"`

	// TranscriptRef points to where the agent's full transcript was stored
	// (typically a ConfigMap in the operator's namespace).
	// +optional
	TranscriptRef string `json:"transcriptRef,omitempty"`

	// Conditions represent the current state of the task. Standard types:
	// Scheduled, Running, Completed, Failed.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=at
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.assignedNode`
// +kubebuilder:printcolumn:name="Verdict",type=string,JSONPath=`.status.verdict`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgenticTask is the unit of dispatchable agentic work. The Foreman scheduler
// matches each Pending task to a FleetNode whose advertised capability
// satisfies the task's RequiredCapability, then a FleetAgent on that node
// picks up the task and runs the matching executor.
type AgenticTask struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the desired task definition.
	Spec AgenticTaskSpec `json:"spec"`

	// status reflects the observed state, updated by the scheduler and the
	// assigned FleetAgent.
	// +optional
	Status AgenticTaskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgenticTaskList is a list of AgenticTasks.
type AgenticTaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgenticTask `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgenticTask{}, &AgenticTaskList{})
}
