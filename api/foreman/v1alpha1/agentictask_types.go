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
// +kubebuilder:validation:Enum=issue-fix;verify;review;freeform;integrate;reconcile
type AgenticTaskKind string

const (
	// AgenticTaskKindIssueFix runs an agent against a GitHub issue: read the
	// issue, edit the repo, run the verification, commit (DCO), push a branch.
	AgenticTaskKindIssueFix AgenticTaskKind = "issue-fix"
	// AgenticTaskKindVerify runs the project's gate (fmt/vet/lint/test +
	// codegen sync) against a pushed branch. Typically scheduled by the
	// controller as a child of a Succeeded issue-fix task.
	AgenticTaskKindVerify AgenticTaskKind = "verify"
	// AgenticTaskKindReview runs a reviewer Agent against the diff that a
	// Succeeded issue-fix produced. v0.2 emits one review task per entry
	// in WorkloadSpec.ReviewerAgentRefs, all parallel, all depending on
	// the upstream verify task reaching GATE-PASS. The rollup from #548
	// treats verdict=NO-GO as "Succeeded but incomplete," so any reviewer
	// NO-GO marks the parent Workload review-failed. When
	// WorkloadSpec.EscalationReviewerAgentRefs is set (#546), a second
	// reviewer tier is emitted per issue only after the base reviewers are
	// terminal with at least one NO-GO.
	AgenticTaskKindReview AgenticTaskKind = "review"
	// AgenticTaskKindFreeform passes an arbitrary prompt to a named agent.
	AgenticTaskKindFreeform AgenticTaskKind = "freeform"
	// AgenticTaskKindIntegrate unions the disjoint slice branches of a sliced
	// Workload onto the current base on a fresh integration branch, gated by a
	// build. Scheduled after all slice issue-fix tasks Succeed; its payload
	// carries the slice branches (payload.slices[].branch) and the base
	// (payload.baseBranch). Part of Sliced Workloads (#1033).
	AgenticTaskKindIntegrate AgenticTaskKind = "integrate"
	// AgenticTaskKindReconcile checks the integrated union against the slice
	// plan's pinned shared identifiers, catching cross-slice interface drift a
	// build cannot see. A pinned-missing drift is authoritative (GATE-FAIL); an
	// llm-flagged-only drift is advisory. Its payload carries the pins
	// (payload.sharedIdentifiers), the slices' files (payload.slices[].files),
	// and the contract (payload.contract). Part of Sliced Workloads (#1033).
	AgenticTaskKindReconcile AgenticTaskKind = "reconcile"
)

// BranchStrategy controls how the executor cuts an issue-fix task's working
// branch relative to the current base, so a re-dispatch of the same issue can
// never revert already-merged work.
// +kubebuilder:validation:Enum=reset;rebase
type BranchStrategy string

const (
	// BranchStrategyReset cuts the working branch fresh from the CURRENT base
	// tip, ignoring any prior attempt (payload.reviseFromBranch). This is the
	// default: a fresh issue-fix, retry, or repair re-dispatch redoes the work
	// against latest base, so a stale prior branch can never drift from base and
	// revert merged commits.
	BranchStrategyReset BranchStrategy = "reset"
	// BranchStrategyRebase restores the prior attempt (payload.reviseFromBranch)
	// and rebases it onto the CURRENT base, so an in-review PR revision carries
	// its earlier commits forward on top of merged work instead of reverting it.
	BranchStrategyRebase BranchStrategy = "rebase"
)

// AgenticTaskFailureReason categorizes WHY a task did not reach a
// "succeeded-on-target" outcome. Distinct from AgenticTaskVerdict
// (which carries the externally-meaningful WHAT: GO / NO-GO /
// GATE-PASS / GATE-FAIL / INCOMPLETE / GATE-ERROR). Together they let
// downstream consumers route differently on different failure modes:
// retry the recoverable ones in place, escalate the role-discipline
// ones, alert on infrastructure ones.
//
// Reason coexists with Verdict: a task can be Phase=Succeeded +
// Verdict=GATE-FAIL + FailureReason=GateFailed, or Phase=Failed +
// FailureReason=InfrastructureError. Empty reason on a successful
// task is normal.
//
// v0.3 #559 introduces the enum + emission; per-reason retry policy
// on AgenticTaskSpec and retry-with-correction in the loop are
// follow-up work that consumes this signal.
// +kubebuilder:validation:Enum=AgentNotFound;InferenceServiceUnavailable;AuthUnavailable;GitRemoteNotConfigured;CloneFailed;ModelMisunderstood;ToolFailed;MaxTurnsExhausted;ConstraintViolated;Timeout;InfrastructureError;GateFailed;GateError;ModelReportedError
type AgenticTaskFailureReason string

const (
	// Pre-loop executor failures (the task never reached the model loop):

	// FailureAgentNotFound: the Agent referenced by spec.agentRef was
	// not present in the task's namespace at executor time. Usually a
	// race between scheduling and execution; not retryable in place.
	FailureAgentNotFound AgenticTaskFailureReason = "AgentNotFound"

	// FailureInferenceServiceUnavailable: the Agent's InferenceServiceRef
	// could not be resolved to a usable endpoint. The metal-agent may
	// not have spawned llama-server yet, or the host-override port
	// lookup failed. Retryable; #540's live-Endpoints path catches
	// most transient cases automatically.
	FailureInferenceServiceUnavailable AgenticTaskFailureReason = "InferenceServiceUnavailable"

	// FailureAuthUnavailable: GitHub auth could not be built (no
	// GITHUB_TOKEN, no ~/.config/foreman/github-token). Operator
	// config error; not retryable.
	FailureAuthUnavailable AgenticTaskFailureReason = "AuthUnavailable"

	// FailureGitRemoteNotConfigured: foreman-agent was started without
	// --git-remote-url and a coder-shaped task arrived. Operator
	// config error; not retryable.
	FailureGitRemoteNotConfigured AgenticTaskFailureReason = "GitRemoteNotConfigured"

	// FailureCloneFailed: git clone of the workspace failed. Often
	// network or auth; sometimes a missing branch. Retryable for
	// transient cases.
	FailureCloneFailed AgenticTaskFailureReason = "CloneFailed"

	// In-loop failures (the model loop ran but did not reach
	// submit_result with a successful verdict):

	// FailureModelMisunderstood: model emitted a syntactically valid
	// turn that the loop could not act on. Examples: assistant message
	// with no tool_calls; tool call referencing a tool name unknown to
	// the system (typo / hallucinated tool). Often recoverable with a
	// corrective system message + retry (deferred to v0.3 follow-up).
	FailureModelMisunderstood AgenticTaskFailureReason = "ModelMisunderstood"

	// FailureToolFailed: a tool dispatch returned a structured error
	// (str_replace context not found, bash non-zero exit, etc.). The
	// loop surfaces these as tool messages today; this reason fires
	// when a tool error escalates to terminal (every available retry
	// exhausted in a future loop revision).
	FailureToolFailed AgenticTaskFailureReason = "ToolFailed"

	// FailureMaxTurnsExhausted: the loop hit Agent.spec.MaxTurns
	// without the model calling submit_result. The model effectively
	// gave up. Not retryable without intervention (different
	// MaxTurns or different prompt).
	FailureMaxTurnsExhausted AgenticTaskFailureReason = "MaxTurnsExhausted"

	// FailureConstraintViolated: the model called a tool excluded by
	// the Agent's spec.tools whitelist (#561's
	// tools.ErrToolNotInWhitelist). Indicates a role-discipline
	// violation. Should escalate rather than retry blindly.
	FailureConstraintViolated AgenticTaskFailureReason = "ConstraintViolated"

	// FailureTimeout: wall-clock budget exceeded. The task's
	// spec.timeoutSeconds elapsed, or the per-turn
	// requestTimeoutSeconds fired and no retry recovered.
	FailureTimeout AgenticTaskFailureReason = "Timeout"

	// FailureInfrastructureError: a non-model failure: apiserver
	// unreachable, llama-server crashed, the OAI client got a 5xx,
	// transcript ConfigMap write failed. Typically retryable.
	FailureInfrastructureError AgenticTaskFailureReason = "InfrastructureError"

	// Gate-specific failures (deterministic verify path):

	// FailureGateFailed: the gate Job ran cleanly but at least one
	// check (fmt / vet / lint / test / codegen-sync) failed. The
	// coder's diff didn't meet quality bar; cascade-on-verdict from
	// #548 short-circuits the downstream reviewer task.
	FailureGateFailed AgenticTaskFailureReason = "GateFailed"

	// FailureGateError: the gate Job itself failed to execute
	// (image pull error, PVC issue, timeout before any check ran).
	// Infrastructure rather than diff-quality; retryable.
	FailureGateError AgenticTaskFailureReason = "GateError"

	// FailureModelReportedError means the model itself reported it could
	// not complete the task via verdict ERROR (a reviewer's
	// could-not-review, a coder's unrecoverable-error). Distinguishes
	// model-reported inability from harness-detected failures; the stored
	// verdict is INCOMPLETE.
	//
	// Maps from the model-facing "ERROR" verdict in submit_result, which
	// the CRD intentionally does not store as a verdict (issue #649;
	// INCOMPLETE-as-could-not-review per #644).
	FailureModelReportedError AgenticTaskFailureReason = "ModelReportedError"
)

// AgenticTaskAccelerator pins which accelerator family a task needs from the
// node that runs it. "any" lets the scheduler pick from any Ready FleetNode.
// "vulkan" is the AMD/Vulkan tier (e.g. Strix Halo gfx1151).
// +kubebuilder:validation:Enum=metal;cuda;vulkan;any
type AgenticTaskAccelerator string

// RequiredCapability tells the scheduler which FleetNodes can serve this task.
// The scheduler matches each field against FleetNode.status.capability;
// unset fields are unconstrained.
type RequiredCapability struct {
	// MinRAMGB is the minimum available RAM the node must advertise.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinRAMGB int32 `json:"minRAMGB,omitempty"`

	// RequiresModelInstalled, when true, scopes scheduling to FleetNodes
	// that already have this Agent's model resident (the model name,
	// resolved from Agent.spec.model or Agent.spec.inferenceServiceRef,
	// must appear in the node's status.capability.installedModels). In
	// that mode the minRAMGB gate is intentionally ignored: the model is
	// already loaded, so the agent loop needs ~0 additional RAM. This is
	// the warm-driver path reviewer-class Agents always take; minRAMGB is
	// correct only for the cold-load path. When false (default), minRAMGB
	// is enforced as before.
	// +optional
	RequiresModelInstalled bool `json:"requiresModelInstalled,omitempty"`

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

	// Roles filters to FleetNodes whose spec.roles include every named
	// role. Matches against FleetNodeSpec.Roles (the --roles flag on the
	// foreman-agent). Used to route the gate Agent's tasks to nodes that
	// advertise themselves as verifiers without coupling to per-node
	// labels.
	// +optional
	Roles []string `json:"roles,omitempty"`
}

// GateAdvisory is a non-blocking finding from the coder-side gate that the
// reviewer should confirm or dismiss. It records which check raised the
// suspicion and a human-readable explanation of what was found.
type GateAdvisory struct {
	// Check is the name of the gate check that produced the finding
	// (e.g. "grounding-breadth", "scope-overlap").
	// +optional
	Check string `json:"check,omitempty"`

	// Detail is the human-readable description of the advisory
	// (e.g. "cites dcgm_gpu_utilization (unknown symbol)").
	// +optional
	Detail string `json:"detail,omitempty"`
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

	// BaseBranch is the upstream base ref the coder branch is cut from
	// (and the verify gate's bite check diffs against). On an issue-fix task
	// the executor fetches this ref from the upstream repo (payload.repo) and
	// branches off it, so a stale fork default branch does not produce a
	// stale-base branch. Optional; defaults to "main".
	// +optional
	BaseBranch string `json:"baseBranch,omitempty"`

	// OpenPullRequest instructs the reviewer-role executor to open a
	// pull request for the task's branch when its review verdict is GO
	// (idempotent: an existing PR for the branch is reused). Stamped
	// from Workload.spec.openPullRequest onto review steps in
	// issue-batch mode; explicit pipelines set it per step. The PR is
	// the Workload's artifact — without this the pipeline ends at a
	// reviewed-but-unopened branch (#937).
	// +optional
	OpenPullRequest bool `json:"openPullRequest,omitempty"`

	// AllowOverwrite permits the coder to replace a stale remote ref for
	// its own branch (force-with-lease pinned to the observed SHA) when a
	// plain push is rejected non-fast-forward. Opt-in, per the design
	// decision in #573: silently overwriting published branches is the
	// wrong default for a system that leaves an audit trail, but
	// automated retry systems that delete-and-recreate Workloads derive
	// the same branch name every run and are otherwise permanently
	// wedged by their predecessor's ref (#934). Stamped from
	// Workload.spec.allowOverwrite in issue-batch mode; explicit
	// pipelines set it per step.
	// +optional
	AllowOverwrite bool `json:"allowOverwrite,omitempty"`

	// BranchPrefix overrides the branch name prefix on issue-fix tasks
	// (default derived from the issue's labels via conventional commit
	// prefixes: fix/, feat/, chore/, etc.).
	// +optional
	BranchPrefix string `json:"branchPrefix,omitempty"`

	// ReviseFromBranch names a ref on the push remote that the executor
	// cuts the working branch FROM instead of from BaseBranch, so a
	// revision task starts with its prior attempt's files present
	// (#951). Issue-fix only. The WorkloadReconciler stamps this on fix-
	// iteration coder tasks (the prior attempt lives at the task's own
	// branch name on the push remote). The executor owns the git restore
	// — no prompt-driven fetch/checkout. If the ref does not exist on
	// the remote (e.g. pruned, or the prior attempt never pushed), the
	// executor logs and falls back to branching from BaseBranch rather
	// than failing.
	// +optional
	ReviseFromBranch string `json:"reviseFromBranch,omitempty"`

	// BranchStrategy selects how the working branch is cut relative to the
	// current base (see BranchStrategy). Defaults to "reset": cut fresh from the
	// current base and ignore any prior attempt (reviseFromBranch), so a retry
	// or repair re-dispatch cannot revert merged work. Set "rebase" for the
	// in-review PR revision path, where the prior attempt is restored and
	// rebased onto the current base. Issue-fix only.
	// +kubebuilder:default=reset
	// +optional
	BranchStrategy BranchStrategy `json:"branchStrategy,omitempty"`

	// Prompt is the agent input. Required for freeform.
	// +optional
	Prompt string `json:"prompt,omitempty"`

	// PromptPrefix is prepended to the assembled user prompt for an
	// issue-fix task, BEFORE the fetched issue body. Unlike Prompt
	// (which, when set, suppresses the issue-body fetch), PromptPrefix
	// leaves the issue-body fetch intact, so the agent sees both this
	// hint and the issue's acceptance criteria. Used by the coder
	// escalation tier to carry the prior model's diagnosis.
	// +optional
	PromptPrefix string `json:"promptPrefix,omitempty"`

	// Agent is the named agent to invoke. Required for freeform; defaults
	// to "issue-fixer" for issue-fix and "verify" for verify.
	// +optional
	Agent string `json:"agent,omitempty"`

	// GateAdvisories holds non-blocking findings copied from the upstream
	// coder task's result, surfaced to a reviewer for confirmation or
	// dismissal. The WorkloadReconciler populates this field on review-kind
	// tasks once the coder task has a terminal result with advisories in
	// status.result.extra["gateAdvisories"]. Empty when the coder task
	// produced no advisories or has not yet completed.
	// +optional
	GateAdvisories []GateAdvisory `json:"gateAdvisories,omitempty"`

	// Slices are the disjoint slices of a sliced Workload. The integrate task
	// unions their branches; the reconcile task checks pins against their
	// files. Set on integrate and reconcile tasks. Part of #1033.
	// +optional
	Slices []SliceRef `json:"slices,omitempty"`

	// Contract is the slice plan's prose contract, handed to a reconcile
	// task's LLM sweep as the description of the intended shared interfaces.
	// Reconcile only. Part of #1033.
	// +optional
	Contract string `json:"contract,omitempty"`

	// SharedIdentifiers pins the exact cross-slice strings a reconcile task
	// verifies against the integrated union. Reconcile only. Part of #1033.
	// +optional
	SharedIdentifiers []SharedIdentifier `json:"sharedIdentifiers,omitempty"`
}

// SliceRef describes one disjoint slice of a sliced Workload. The integrate
// task unions each slice's Branch; the reconcile task verifies pinned
// identifiers appear in each slice's Files. Part of Sliced Workloads (#1033).
type SliceRef struct {
	// Name is the slice's name within the plan.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Branch is the git ref the slice's coder pushed. Required for the
	// integrate task, which applies each slice branch's diff onto the base.
	// +optional
	Branch string `json:"branch,omitempty"`

	// Files are the repo-relative paths this slice owns, so a reconcile task
	// looks for each pinned identifier in the right slice.
	// +optional
	Files []string `json:"files,omitempty"`
}

// SharedIdentifier pins one exact string that crosses a slice boundary (a
// metric name, config key, or CRD field). The reconcile task asserts the ID
// appears verbatim in the DefinedBy slice and every ReferencedBy slice; a
// missing pin is an authoritative drift. Part of Sliced Workloads (#1033).
type SharedIdentifier struct {
	// ID is the exact string every listed slice must contain verbatim.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`

	// DefinedBy is the slice name that must produce the identifier.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	DefinedBy string `json:"definedBy"`

	// ReferencedBy are the slice names that must consume the identifier.
	// +optional
	ReferencedBy []string `json:"referencedBy,omitempty"`
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

	// GateProfile declares the gate commands, container image, and source
	// file extensions for the task's language. Consumed in a later slice
	// by the gate executor; unset means the "go" preset (current behavior).
	// +optional
	GateProfile *GateProfile `json:"gateProfile,omitempty"`

	// MCPEnabled is the reconciler-propagated effective Workload opt-out
	// that reaches the executor without a live Workload GET. Three-valued
	// via the *bool: nil or true means MCP is allowed (subject to the
	// referenced Agent's own spec.mcp config); false disables MCP for
	// this run, for benchmark control runs that must be comparable to a
	// no-MCP baseline. The WorkloadReconciler stamps this from
	// Workload.spec.mcpEnabled onto every child task it emits; hand-
	// authored AgenticTasks may set it directly.
	// +optional
	MCPEnabled *bool `json:"mcpEnabled,omitempty"`

	// VerdictPolicy is the reconciler-propagated effective Workload
	// work-class self-certification policy (proposal 1075, section 3.2)
	// that reaches the executor without a live Workload GET. A nil
	// policy, or one with an empty SelfGO, resolves to
	// VerdictPolicy.Resolve's default. The WorkloadReconciler stamps
	// this from Workload.spec.verdictPolicy onto every child task it
	// emits, the same way it propagates MCPEnabled; hand-authored
	// AgenticTasks may set it directly.
	// +optional
	VerdictPolicy *VerdictPolicy `json:"verdictPolicy,omitempty"`
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
// +kubebuilder:validation:Enum=GO;NO-GO;INCOMPLETE;GATE-PASS;GATE-FAIL;GATE-ERROR;Skipped
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
	// AgenticTaskVerdictSkipped marks a task the executor never ran
	// because its dependency terminated as a terminal non-failure the
	// downstream work couldn't meaningfully act on — currently: the dep
	// ended NO-GO + extra.outcome="ALREADY-RESOLVED" (#970). Set by the
	// cascade-fail path in agentictask_controller.go when
	// cascadeSkipIfDepAlreadyResolved sees an ALREADY-RESOLVED dep; the
	// rollup excludes Skipped tasks from all five buckets
	// (succeeded, incomplete, failed, inFlight, alreadyResolved) so
	// they don't pin the Workload to Failed. Phase=Succeeded with
	// Skipped is the only terminal shape the cascade path writes.
	AgenticTaskVerdictSkipped AgenticTaskVerdict = "Skipped"
)

// SucceededOnTarget reports whether the task is in a terminal Succeeded
// phase AND its verdict is a positive outcome that produced usable
// downstream artifacts (GO for LLM-driven Agents, GATE-PASS for the
// deterministic gate Agent).
//
// This is the correct gate for downstream behavior. A task can end with
// Phase=Succeeded + Verdict=INCOMPLETE (e.g. MaxTurnsExhausted,
// LoopContractViolation, AssistantHallucinatedFinish) when the executor
// reached terminal state cleanly but the agent loop did not produce a
// usable result. The dependents of such a task must NOT proceed against
// nonexistent output (a verify task that tries to clone a branch the
// coder never pushed crashes on GATE-FAIL for the wrong reason), and
// the Workload status rollup must NOT count it as a win.
//
// Fixes defilantech/LLMKube#541: cascadeFailIfDepFailed and the Workload
// rollup previously gated on Phase alone, leaking INCOMPLETE coder
// tasks through.
func (t *AgenticTask) SucceededOnTarget() bool {
	if t == nil || t.Status.Phase != AgenticTaskPhaseSucceeded {
		return false
	}
	switch t.Status.Verdict {
	case AgenticTaskVerdictGo, AgenticTaskVerdictGatePass:
		return true
	}
	return false
}

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

	// FailureReason categorizes WHY a task did not reach a "succeeded
	// on target" outcome. Distinct from Verdict (which carries the
	// externally-meaningful WHAT). Empty on a successful task. See
	// AgenticTaskFailureReason for the full enum + per-reason semantics.
	//
	// v0.3 #559: introduces the structured reason so downstream
	// consumers (the Workload reconciler's rollup, future retry
	// policy, batch-level metrics) can route on a typed value rather
	// than mining the Result.Extra map.
	// +optional
	FailureReason AgenticTaskFailureReason `json:"failureReason,omitempty"`

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
