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
//     most common shape for v0.1. When ReviewerAgentRefs is also set
//     (v0.2), each issue additionally fans out one parallel review
//     task per listed reviewer Agent, depending on the verify task.
//     When EscalationReviewerAgentRefs is also set, a second reviewer
//     tier is emitted per issue only after a base reviewer returns
//     NO-GO.
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

	// ReviewerAgentRefs is the optional v0.2 third pipeline stage: each
	// listed Agent runs a review task against the diff the coder
	// produced, in parallel, all depending on the upstream verify task
	// reaching GATE-PASS. The rendered task names are
	// "<workload>-review-<N>-<i>" where N is the issue number and i is
	// the index into this slice; the index keeps DNS-1123 lengths
	// bounded across long agent names.
	//
	// Aggregation: any reviewer emitting verdict=NO-GO leaves that task
	// Phase=Succeeded but not "succeeded on target," so the Workload
	// rollup from #548 marks it under IncompleteTasks and the Workload
	// reaches Phase=Failed with reason ChildrenIncomplete.
	//
	// v0.2: entries whose Agent has spec.provider="cloud-proxy"
	// are gated by AllowCloudReviewers (below) and by the operator-
	// level --allow-cloud-providers kill switch. When either gate
	// blocks, the WorkloadReconciler omits the cloud reviewer's task
	// and surfaces a CloudReviewersSuppressed condition naming the
	// skipped Agents; local reviewers in the same list still run.
	//
	// Leave empty to keep the v0.1 two-step (code + verify) pipeline.
	// Multi-strategy reviewing (validator / falsification / thinking-
	// mode + optionally a cloud frontier reviewer) is achieved by
	// listing multiple Agent CRs with different system prompts and
	// providers.
	// +optional
	ReviewerAgentRefs []corev1.LocalObjectReference `json:"reviewerAgentRefs,omitempty"`

	// EscalationReviewerAgentRefs is the optional second reviewer tier
	// (v0.2, #546). For each issue N in issue-batch mode, the
	// WorkloadReconciler emits one review task per listed Agent
	// (rendered name "<workload>-escalate-<N>-<j>") ONLY after every
	// base reviewer task for that issue (the ReviewerAgentRefs fan-out)
	// is terminal AND at least one
	// returned verdict=NO-GO. This bounds the cost of an expensive
	// reviewer (typically a larger local model; a cloud-proxy Agent is
	// allowed but stays behind the same sovereignty gates as base
	// reviewers) to the small fraction of branches a cheap reviewer
	// already flagged.
	//
	// Escalation verdicts are advisory in v0.2: the
	// EscalationTriggered condition records that the tier fired, the
	// verdict itself is read from the escalation AgenticTask, and the
	// base NO-GO still rolls the Workload to Failed so a human reviews
	// both verdicts. Note that adding escalation refs to an
	// already-Failed issue-batch Workload re-opens it: the next
	// reconcile emits the escalation tier against the recorded NO-GO.
	// Escalation tasks never trigger further escalation. Requires
	// ReviewerAgentRefs to be non-empty; ignored in explicit Pipeline
	// mode. Counts against MaxTasks.
	// +optional
	EscalationReviewerAgentRefs []corev1.LocalObjectReference `json:"escalationReviewerAgentRefs,omitempty"`

	// OpenPullRequest controls whether a review-GO opens a pull request
	// for the Workload's branch (the pipeline's end artifact). Three-
	// valued via the *bool: nil (unset) means true in issue-batch mode —
	// a Workload built from Issues exists to produce a PR — while *false
	// opts a Workload out (branch-only consumers). Explicit pipelines
	// carry the flag per step and ignore this field. Idempotent at the
	// executor: an existing PR for the branch is reused, never
	// duplicated (#937).
	// +optional
	OpenPullRequest *bool `json:"openPullRequest,omitempty"`

	// EscalationCoderAgentRef is the optional coder escalation tier: a
	// single, typically larger/denser coder Agent that re-attempts an
	// issue when the base CoderAgentRef fails at its model's ceiling.
	// Issue-batch mode only. For each issue N, if the base code-<N> task
	// is terminal with a capability failure (verdict NO-GO from the
	// model, or outcome CODER-GATE-FAILED), the WorkloadReconciler emits
	// an escalation code+verify(+review) set (code-<N>-esc /
	// verify-<N>-esc / review-<N>-esc-<i>) against this Agent, on a fresh
	// branch foreman/<w>/issue-<N>-esc, with the failed model's summary
	// passed as a prompt hint.
	//
	// Singular (unlike EscalationReviewerAgentRefs, which fans out N
	// reviewers in parallel): coders are sequential, so N parallel
	// escalation coders would produce N competing branches. Exactly one
	// escalation tier; an escalation task that itself fails is terminal.
	//
	// Does NOT trigger on STUCK-LOOP-DETECTED, a model-decided
	// INCOMPLETE, or ERROR: those are scope/harness failures a larger,
	// slower model will not fix. Leave unset to disable escalation.
	// When EscalateOnFailure is set to true, the escalation coder also
	// triggers on STUCK-LOOP-DETECTED, INCOMPLETE, and ERROR outcomes
	// from the base coder (in addition to the default NO-GO capability
	// failures).
	// +optional
	EscalationCoderAgentRef *corev1.LocalObjectReference `json:"escalationCoderAgentRef,omitempty"`

	// EscalateOnFailure, when true, additionally re-attempts the issue on
	// EscalationCoderAgentRef for a terminal STUCK-LOOP-DETECTED, INCOMPLETE,
	// or ERROR from the base coder (not just a capability NO-GO). Requires
	// EscalationCoderAgentRef to be set. Default false preserves the
	// NO-GO-only behavior. ALREADY-RESOLVED and NEEDS-VERIFICATION never
	// escalate.
	// +optional
	EscalateOnFailure *bool `json:"escalateOnFailure,omitempty"`

	// AllowOverwrite lets this Workload's coder replace a stale remote
	// ref for its own foreman/* branch (force-with-lease compare-and-swap)
	// instead of failing non-fast-forward. Opt-in — #573 deliberately
	// rejected force-on-collision as a default; retry systems that
	// re-run Workloads under the same name set this on the re-run, where
	// the stale ref is their own previous attempt. Copied onto every
	// synthesized issue-batch task payload.
	// +optional
	AllowOverwrite bool `json:"allowOverwrite,omitempty"`

	// MaxReviewIterations bounds how many fix iterations the
	// WorkloadReconciler may append per issue after a reviewer NO-GO
	// (#946). On a NO-GO, instead of failing the Workload, the
	// reconciler emits a new coder task ("code-<N>-r<k>", using
	// RevisionCoderAgentRef when set and CoderAgentRef otherwise, same
	// branch, payload.reviseFromBranch naming that branch so the
	// executor restores the prior attempt (#951), and
	// payload.allowOverwrite=true) whose payload.prompt carries the
	// reviewer's structured findings and summary, then chains a fresh
	// verify + reviewer fan-out behind it ("verify-<N>-r<k>",
	// "review-<N>-<i>-r<k>"). Superseded
	// iterations stop counting toward the rollup; the Workload
	// completes when the final iteration's reviewers say GO and
	// fails (today's behavior) when the last allowed iteration is
	// still NO-GO.
	//
	// nil (unset) defaults to 1 iteration. An explicit 0 disables
	// iteration and restores the fail-on-first-NO-GO behavior.
	// Issue-batch mode only; ignored in explicit Pipeline mode.
	// Iteration tasks count against MaxTasks.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxReviewIterations *int32 `json:"maxReviewIterations,omitempty"`

	// RevisionCoderAgentRef optionally names the same-namespace Agent
	// the fix-iteration coder steps ("code-<N>-r<k>") reference instead
	// of CoderAgentRef. A revision task amends its restored prior
	// attempt (payload.reviseFromBranch, #951) rather than building a
	// fix from scratch, and the issue-fix Agent's forcing profile is
	// tuned for the latter — #951's live validation showed a revision
	// task collapsing under the issue-fix profile's forcing windows.
	// When unset, iteration coder steps fall back to CoderAgentRef and
	// the reconciler emits a Warning event on the Workload (reason
	// RevisionUnderIssueFixProfile). Issue-batch mode only; ignored in
	// explicit Pipeline mode.
	// +optional
	RevisionCoderAgentRef *corev1.LocalObjectReference `json:"revisionCoderAgentRef,omitempty"`

	// AllowCloudReviewers gates whether reviewer Agents whose
	// spec.provider is "cloud-proxy" (or any non-"local" value) may be
	// dispatched for this Workload. Three-valued via the *bool:
	//
	//   - nil (unset): treated as true; cloud reviewers run as long as
	//     the operator-level kill switch also allows.
	//   - *true: cloud reviewers run (subject to the operator gate).
	//   - *false: cloud reviewers are suppressed for this Workload,
	//     even if the operator allows them. The WorkloadReconciler
	//     omits the cloud reviewer's task and records a
	//     CloudReviewersSuppressed condition. Local reviewers and
	//     coder/verifier tasks are unaffected.
	//
	// The independent operator-level kill switch
	// (--allow-cloud-providers) is the cluster-wide hard stop for
	// air-gapped or compliance-restricted environments. Both gates
	// must allow for a cloud reviewer to dispatch.
	// +optional
	AllowCloudReviewers *bool `json:"allowCloudReviewers,omitempty"`

	// GateProfile is the default gate profile applied to every AgenticTask
	// this Workload decomposes into (issue-batch, explicit pipeline, and
	// escalation steps alike). It sets the verify gate's language, image,
	// and commands, and drives the coder's in-loop self-gate. A PipelineStep
	// may override it per step (see PipelineStep.GateProfile); an unset
	// profile on both resolves to the "go" preset, i.e. current behavior.
	//
	// Without this field, every Workload-decomposed task falls back to the
	// Go gate, so a Workload-driven source (a work-queue bridge) cannot run
	// Foreman on non-Go repositories even though the language presets exist.
	// +optional
	GateProfile *GateProfile `json:"gateProfile,omitempty"`

	// MCPEnabled is a benchmark opt-out for MCP tool access. Three-valued
	// via the *bool: nil or true means MCP is allowed for Agents in this
	// Workload that have spec.mcp configured; false disables MCP for
	// every child task regardless of the referenced Agent's own MCP
	// config, for control runs that must be comparable to a no-MCP
	// baseline. The WorkloadReconciler propagates this value onto each
	// child AgenticTask.spec.mcpEnabled.
	// +optional
	MCPEnabled *bool `json:"mcpEnabled,omitempty"`

	// VerdictPolicy controls which work classes a coder GO may
	// self-certify for every AgenticTask this Workload decomposes into
	// (proposal 1075, section 3.2). Unset resolves to
	// VerdictPolicy.Resolve's default (code-fix, docs, packaging,
	// config); ci-policy and release-policy stay human-sign-off-only
	// unless an operator opts them in here. The WorkloadReconciler
	// propagates this value onto each child AgenticTask.spec.verdictPolicy
	// the same way it propagates MCPEnabled, so the executor reads the
	// policy without a live Workload GET.
	// +optional
	VerdictPolicy *VerdictPolicy `json:"verdictPolicy,omitempty"`
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

	// GateProfile overrides the Workload-level GateProfile for this step
	// only, so a mixed-language pipeline can gate each step differently.
	// Unset falls back to WorkloadSpec.GateProfile, then to the "go" preset.
	// +optional
	GateProfile *GateProfile `json:"gateProfile,omitempty"`
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

	// SucceededTasks counts child tasks that reached a positive terminal
	// outcome (Phase=Succeeded AND verdict in {GO, GATE-PASS}). A child
	// in Phase=Succeeded with verdict INCOMPLETE / NO-GO / GATE-FAIL /
	// GATE-ERROR is NOT counted here; it lands in IncompleteTasks.
	// +optional
	SucceededTasks int32 `json:"succeededTasks,omitempty"`

	// FailedTasks counts child tasks in phase Failed.
	// +optional
	FailedTasks int32 `json:"failedTasks,omitempty"`

	// IncompleteTasks counts child tasks that reached Phase=Succeeded
	// but did not produce a usable artifact (verdict in {INCOMPLETE,
	// NO-GO, GATE-FAIL, GATE-ERROR}). These are surfaced separately
	// from FailedTasks so callers can distinguish "the runtime errored"
	// from "the agent legitimately gave up or the gate Job said the
	// branch didn't pass the checks."
	// +optional
	IncompleteTasks int32 `json:"incompleteTasks,omitempty"`

	// ReviewIterations counts the fix iterations the reconciler has
	// emitted after reviewer NO-GO verdicts (#946), summed across
	// issues. Zero (or absent) means no reviewer ever bounced a
	// branch back to the coder; operators watch this to distinguish
	// convergence (small counts) from thrash (counts near
	// issues * maxReviewIterations).
	// +optional
	ReviewIterations int32 `json:"reviewIterations,omitempty"`

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
