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
// decomposes (via a frontier model) into a set of AgenticTask objects.
type WorkloadSpec struct {
	// Intent is the natural-language description of what to do.
	// Example: "fix all open bugs in defilantech/LLMKube tagged size/small".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Intent string `json:"intent"`

	// Repo is the GitHub repo in "owner/name" form that Intent applies to.
	// Required for issue-fix workloads; the planner reads its open issues.
	// +optional
	Repo string `json:"repo,omitempty"`

	// MaxTasks caps how many AgenticTasks the planner may emit. Zero means
	// no limit; the planner picks. Use this as a safety belt on the first
	// runs against a new repo or intent.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxTasks int32 `json:"maxTasks,omitempty"`

	// PlannerModel selects the frontier model the planner should call.
	// Empty uses the operator's default (Anthropic Claude). The value is
	// a free-form identifier the planner adapter interprets, e.g.
	// "anthropic/claude-opus-4-7", "anthropic/claude-sonnet-4-6".
	// +optional
	PlannerModel string `json:"plannerModel,omitempty"`
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
// a high-level intent ("fix open bugs"); the WorkloadReconciler calls a
// frontier model to decompose it into a set of AgenticTask objects, which
// the scheduler then dispatches across the fleet.
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
