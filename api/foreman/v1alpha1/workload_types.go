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

// WorkloadPhase is the lifecycle of a planner-driven batch.
// +kubebuilder:validation:Enum=Planning;Planned;Dispatched;Completed;Failed
type WorkloadPhase string

const (
	WorkloadPhasePlanning   WorkloadPhase = "Planning"
	WorkloadPhasePlanned    WorkloadPhase = "Planned"
	WorkloadPhaseDispatched WorkloadPhase = "Dispatched"
	WorkloadPhaseCompleted  WorkloadPhase = "Completed"
	WorkloadPhaseFailed     WorkloadPhase = "Failed"
)

// WorkloadSpec captures a high-level intent that the WorkloadReconciler
// decomposes into a set of AgenticTask objects.
//
// v0.1 supports three modes; the reconciler picks based on which fields are
// set (in this precedence order):
//
//  1. Explicit pipeline (Pipeline non-empty): emit each PipelineStep as
//     one AgenticTask owner-ref'd to this Workload. The reconciler
//     rewrites step-local DependsOn names to absolute task names. Used
//     for full control over a hand-authored pipeline.
//  2. Issue-batch shortcut (Issues non-empty + CoderAgentRef +
//     VerifierAgentRef set): emit one code+verify pair per issue. The
//     most common shape for v0.1.
//  3. LLM-planner (Intent only, no Pipeline, no Issues): deferred to v0.2.
//     v0.1 marks the Workload Failed with reason NoPlannerOrPipeline.
type WorkloadSpec struct {
	// Intent is the natural-language description of what to do.
	// Example: "fix all open bugs in defilantech/LLMKube tagged size/small".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Intent string `json:"intent"`

	// Repo is the GitHub repo in "owner/name" form that Intent applies to.
	// Required for issue-fix workloads; supplies the Payload.Repo for each
	// generated AgenticTask in issue-batch mode. Ignored in explicit
	// Pipeline mode (each step carries its own Payload.Repo).
	// +optional
	Repo string `json:"repo,omitempty"`

	// MaxTasks caps how many AgenticTasks may be emitted. Zero means no
	// limit. Applied after pipeline expansion: if Issues has 20 entries and
	// MaxTasks is 10, only the first 10 issues are processed and a
	// Truncated condition is set.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxTasks int32 `json:"maxTasks,omitempty"`

	// PlannerModel selects the frontier model the planner should call.
	// Empty uses the operator's default (Anthropic Claude). v0.1 stores
	// this verbatim but does not consume it (planner is deferred to v0.2);
	// the value round-trips through status.plannerModel for visibility.
	// +optional
	PlannerModel string `json:"plannerModel,omitempty"`

	// Pipeline, when set, bypasses the planner and emits AgenticTasks
	// verbatim from this list. Each step becomes one AgenticTask
	// owner-ref'd to this Workload, named "<workload-name>-<step.name>".
	// Pipeline takes precedence over Issues + agent-ref shortcuts when
	// both are set. Used for hand-authored pipelines and re-runs.
	// +optional
	Pipeline []PipelineStep `json:"pipeline,omitempty"`

	// Issues, when set and Pipeline is empty, drives the issue-batch
	// shortcut. The reconciler emits one code/verify pair per issue
	// number using CoderAgentRef + VerifierAgentRef. Repo + Branch + Issue
	// fields are populated from the Workload's spec.repo and the issue
	// numbers themselves; downstream verify tasks DependOn the upstream
	// code task automatically.
	// +optional
	Issues []int32 `json:"issues,omitempty"`

	// CoderAgentRef is required when Issues is set: names the same-
	// namespace Agent the code-<N> steps reference. Ignored in explicit
	// Pipeline mode.
	// +optional
	CoderAgentRef *corev1.LocalObjectReference `json:"coderAgentRef,omitempty"`

	// VerifierAgentRef is required when Issues is set: names the same-
	// namespace Agent the verify-<N> steps reference. Ignored in explicit
	// Pipeline mode.
	// +optional
	VerifierAgentRef *corev1.LocalObjectReference `json:"verifierAgentRef,omitempty"`
}

// PipelineStep is one step in an explicit Workload pipeline. Each step
// becomes one AgenticTask owner-ref'd to the Workload.
//
// Names are scoped to the Workload: the reconciler renders the resulting
// AgenticTask name as "<workload-name>-<step.name>" so two Workloads in
// the same namespace can reuse step names without colliding. DependsOn
// references use the step-local Name; the reconciler resolves these to
// absolute task names at render time.
type PipelineStep struct {
	// Name identifies the step within this Workload's pipeline. Becomes
	// the suffix of the rendered AgenticTask name and the target ID for
	// other steps' DependsOn entries.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*$`
	Name string `json:"name"`

	// Kind selects the AgenticTask kind to emit.
	// +kubebuilder:validation:Required
	Kind AgenticTaskKind `json:"kind"`

	// AgentRef names the same-namespace Agent that runs this step.
	// +kubebuilder:validation:Required
	AgentRef corev1.LocalObjectReference `json:"agentRef"`

	// Payload mirrors AgenticTaskSpec.Payload exactly.
	// +kubebuilder:validation:Required
	Payload AgenticTaskPayload `json:"payload"`

	// DependsOn lists other PipelineStep.Name values within this Workload
	// whose AgenticTasks must reach Succeeded before this step's task is
	// dispatched. The reconciler rewrites these to absolute task names
	// when rendering.
	// +optional
	DependsOn []string `json:"dependsOn,omitempty"`

	// TimeoutSeconds bounds the agent's run time on this step. Zero uses
	// the operator's default (2700 seconds).
	// +kubebuilder:validation:Minimum=0
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// Priority is a scheduler hint when multiple tasks are Pending.
	// Higher values dispatch first. v0.1 is FIFO and ignores priority.
	// +optional
	Priority int32 `json:"priority,omitempty"`
}

// WorkloadStatus reflects the observed state of the workload.
type WorkloadStatus struct {
	// Phase is the lifecycle state.
	// +optional
	Phase WorkloadPhase `json:"phase,omitempty"`

	// Tasks lists the AgenticTask objects the planner emitted. They are
	// owner-ref'd to this Workload so they cascade-delete with it.
	// +optional
	Tasks []corev1.ObjectReference `json:"tasks,omitempty"`

	// SucceededTasks counts child tasks in phase Succeeded.
	// +optional
	SucceededTasks int32 `json:"succeededTasks,omitempty"`

	// FailedTasks counts child tasks in phase Failed.
	// +optional
	FailedTasks int32 `json:"failedTasks,omitempty"`

	// PlannerModel records which frontier model the planner actually used
	// for this workload. Set after the planner runs.
	// +optional
	PlannerModel string `json:"plannerModel,omitempty"`

	// Conditions track standard signals: Planned, Dispatched, Completed.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=wl
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.repo`
// +kubebuilder:printcolumn:name="Tasks",type=integer,JSONPath=`.status.succeededTasks`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failedTasks`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Workload is the v0.1 entrypoint to Foreman. A user creates a Workload with
// a high-level Intent plus either an explicit Pipeline (full control) or an
// Issues list + Coder/Verifier agent refs (the common shortcut). The
// WorkloadReconciler emits one AgenticTask per pipeline step, owner-ref'd
// to the Workload, and the scheduler then dispatches them across the fleet.
//
// In v0.1 the reconciler is a deterministic stub: it does not call an LLM
// to plan. v0.2 will add an LLM-driven planner for the Intent-only mode.
type Workload struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the user-supplied intent.
	Spec WorkloadSpec `json:"spec"`

	// status is the planner's and scheduler's observed view.
	// +optional
	Status WorkloadStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadList is a list of Workloads.
type WorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workload `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workload{}, &WorkloadList{})
}
