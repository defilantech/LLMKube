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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubpr"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

// RunTask is the standalone single-task runner: it executes ONE
// AgenticTask to completion against a remote model and returns a
// structured result. It is the body that runs inside the Foreman coder
// Job (#620); the same logic runs in-process from the AgenticTaskWatcher
// via NativeAgentLoopExecutor.Execute.
//
// RunTask deliberately reuses NativeAgentLoopExecutor.Execute rather
// than re-plumbing the agent loop: Execute already resolves the Agent +
// model endpoint, prepares the workspace, clones the branch, runs the
// loop, and on a GO verdict commits + pushes. RunTask layers two things
// on top of that:
//
//  1. It loads the AgenticTask from the API (Execute takes an already-
//     loaded *AgenticTask; the watcher hands it one, the Job must fetch
//     it by NamespacedName first).
//  2. It emits the structured result as JSON on cfg.Stdout followed by a
//     sentinel line (RunTaskSentinelGo / NoGo / Incomplete / Error) so a
//     Job poller can read the outcome from the Pod log, mirroring the
//     gate Job's "GATE PASS" / "GATE FAIL" contract
//     (pkg/foreman/agent/tools/gate_job_template.yaml).
//
// A genuine system failure (apiserver unreachable, task not found, the
// loop's transport erroring out) is returned as a non-nil error so the
// Job exits non-zero. A data-shaped failure (model said NO-GO, max turns
// exhausted, push rejected) is NOT an error: it is a real, structured
// outcome carried in the returned RunTaskResult and emitted on the
// stream. The caller (cmd/foreman-agent run-task) exits non-zero only on
// a returned error or an ERROR sentinel, not on a NO-GO verdict.

// Sentinel + prefix strings the coder Job poller matches on. The
// RunTaskResultPrefix line carries the full RunTaskResult JSON; the
// sentinel line is a single token a substring scan can find without
// parsing JSON, exactly as the gate Job emits "GATE PASS" / "GATE FAIL".
const (
	// RunTaskResultPrefix prefixes the single line carrying the
	// marshaled RunTaskResult JSON.
	RunTaskResultPrefix = "FOREMAN-RESULT: "

	// RunTaskSentinelGo is printed when the task produced a committed,
	// pushed branch (verdict GO).
	RunTaskSentinelGo = "FOREMAN GO"

	// RunTaskSentinelNoGo is printed when the model legitimately
	// declined / produced no usable diff (verdict NO-GO).
	RunTaskSentinelNoGo = "FOREMAN NO-GO"

	// RunTaskSentinelIncomplete is printed when the loop did not reach a
	// terminal verdict (max turns, model misunderstood, timeout).
	RunTaskSentinelIncomplete = "FOREMAN INCOMPLETE"

	// RunTaskSentinelError is printed when the runner itself failed
	// before or during execution (returned as a Go error). The poller
	// treats this as a non-retryable run failure distinct from a NO-GO.
	RunTaskSentinelError = "FOREMAN ERROR"
)

// RunTaskConfig is everything RunTask needs to execute one task. The
// fields mirror NativeAgentLoopExecutor's so cmd/foreman-agent can wire
// the same flags into both the watcher (in-process) and the Job
// (run-task) modes.
type RunTaskConfig struct {
	// Client resolves the AgenticTask, its Agent, the InferenceService /
	// provider Secret, and writes the transcript ConfigMap. Required.
	Client client.Client

	// Task is the AgenticTask to run, by namespace + name. Required.
	Task types.NamespacedName

	// WorkspaceDir is the parent dir for the per-task clone workspace.
	// Maps to NativeAgentLoopExecutor.WorkspaceRoot.
	WorkspaceDir string

	// GitRemoteURL is the URL to clone from and push to. Required for
	// coder tasks that commit + push.
	GitRemoteURL string

	// CommitAuthor / CommitCommitter are the git identities the produced
	// commit carries.
	CommitAuthor    repo.Identity
	CommitCommitter repo.Identity

	// InferenceBaseURLOverride / InferenceBaseURLHostOverride pass
	// through to the executor's endpoint resolution. See
	// NativeAgentLoopExecutor for the precedence rules.
	InferenceBaseURLOverride     string
	InferenceBaseURLHostOverride string

	// KeepWorkspace preserves the clone workspace after the run.
	KeepWorkspace bool

	// RegistryFactory builds the tool registry for the workspace +
	// Agent. Required (the executor refuses to run without one). See
	// NativeAgentLoopExecutor.RegistryFactory for the ctx +
	// workloadMCPEnabled params this mirrors.
	RegistryFactory func(
		ctx context.Context, workspace string, agent *foremanv1alpha1.Agent, workloadMCPEnabled bool,
	) (ToolRegistry, error)

	// AuthFactory builds GitHub auth. nil falls back to the executor's
	// default (env / file token lookup).
	AuthFactory func() (*repo.Auth, error)

	// PREnsurer opens the PR on a review GO (#937). nil disables.
	PREnsurer githubpr.Ensurer

	// IssueFetcher pulls the GitHub issue body for issue-fix tasks with
	// an empty payload prompt. Optional; nil preserves the empty-body
	// behavior.
	IssueFetcher githubissue.Fetcher

	// UpstreamURLForRepo overrides how the task's payload.repo slug
	// resolves to the upstream project's git URL (mirrors
	// NativeAgentLoopExecutor.UpstreamURLForRepo). nil falls back to the
	// executor's default (github.com/<repo slug>.git). Tests use this to
	// point the base-branch fetch at a local fixture repo instead of a
	// real network host.
	UpstreamURLForRepo func(repoSlug string) string

	// Stdout is where the result JSON + sentinel line are written.
	// Defaults to os.Stdout when nil so the Job's Pod log carries them.
	Stdout io.Writer
}

// RunTaskResult is the structured outcome of a single task run. It is
// both the Go return value and the JSON payload emitted on the result
// line. The Result field carries the full executor envelope (verdict,
// failure reason, transcript pointer, etc.); the flat fields hoist the
// values a Job poller most often wants without re-deriving them from
// Result.Extra.
type RunTaskResult struct {
	// Verdict is the final outcome category (GO / NO-GO / INCOMPLETE /
	// GATE-*). String form of foremanv1alpha1.AgenticTaskVerdict.
	Verdict string `json:"verdict"`

	// Summary is the one-line "what happened".
	Summary string `json:"summary"`

	// CommitMessage is the message the model supplied for the commit on
	// a GO verdict. Empty for non-GO outcomes.
	CommitMessage string `json:"commitMessage,omitempty"`

	// Branch is the branch the run targeted (pushed on GO, intended on
	// non-GO).
	Branch string `json:"branch,omitempty"`

	// CommitSHA is the head commit pushed on a GO verdict. Empty
	// otherwise.
	CommitSHA string `json:"commitSHA,omitempty"`

	// Result is the full executor Result envelope.
	Result *Result `json:"result,omitempty"`
}

// RunTask loads the AgenticTask named by cfg.Task, runs it to
// completion via NativeAgentLoopExecutor.Execute (the same loop the
// in-process watcher uses), emits the structured result + a sentinel
// line on cfg.Stdout, and returns the structured result.
//
// Returns a non-nil error only for system / execution failures (task
// not found, apiserver unreachable, loop transport error). Data-shaped
// outcomes (NO-GO, INCOMPLETE) come back with err == nil and a populated
// RunTaskResult.
func RunTask(ctx context.Context, cfg RunTaskConfig) (RunTaskResult, error) {
	log := logf.FromContext(ctx).WithName("run-task").WithValues(
		"task", cfg.Task.Name, "ns", cfg.Task.Namespace,
	)
	out := cfg.Stdout
	if out == nil {
		out = os.Stdout
	}

	var task foremanv1alpha1.AgenticTask
	if err := cfg.Client.Get(ctx, cfg.Task, &task); err != nil {
		emitSentinel(out, RunTaskSentinelError, "")
		return RunTaskResult{}, fmt.Errorf("get AgenticTask %s: %w", cfg.Task, err)
	}

	exec := &NativeAgentLoopExecutor{
		Client:                       cfg.Client,
		WorkspaceRoot:                cfg.WorkspaceDir,
		GitRemoteURL:                 cfg.GitRemoteURL,
		InferenceBaseURLOverride:     cfg.InferenceBaseURLOverride,
		InferenceBaseURLHostOverride: cfg.InferenceBaseURLHostOverride,
		CommitAuthor:                 cfg.CommitAuthor,
		CommitCommitter:              cfg.CommitCommitter,
		KeepWorkspace:                cfg.KeepWorkspace,
		RegistryFactory:              cfg.RegistryFactory,
		AuthFactory:                  cfg.AuthFactory,
		IssueFetcher:                 cfg.IssueFetcher,
		PREnsurer:                    cfg.PREnsurer,
		UpstreamURLForRepo:           cfg.UpstreamURLForRepo,
	}

	res, execErr := exec.Execute(ctx, &task)
	if execErr != nil {
		// System / transport failure. Mirror the executor's contract:
		// these are the cases the watcher records as ExecutorError.
		emitSentinel(out, RunTaskSentinelError, execErr.Error())
		return RunTaskResult{}, fmt.Errorf("execute task %s: %w", cfg.Task, execErr)
	}

	rt := runTaskResultFromResult(res)
	emitResult(out, rt, log)
	return rt, nil
}

// runTaskResultFromResult hoists the flat RunTaskResult fields out of
// the executor's Result envelope. Branch + commitSHA live in Result.Extra
// under different keys depending on which terminal path the executor
// took (GO sets "branch"/"commitSHA"; non-GO paths set "intendedBranch"),
// so we coalesce them here rather than making the Job poller know the
// executor's internal Extra schema.
func runTaskResultFromResult(res *Result) RunTaskResult {
	rt := RunTaskResult{Result: res}
	if res == nil {
		return rt
	}
	rt.Verdict = string(res.Verdict)
	rt.Summary = res.Summary
	if res.Extra != nil {
		rt.Branch = firstStringField(res.Extra, "branch", "intendedBranch")
		rt.CommitSHA = stringField(res.Extra, "commitSHA")
		rt.CommitMessage = stringField(res.Extra, "commitMessage")
	}
	return rt
}

// stringField returns res.Extra[key] as a string, or "" if absent / not
// a string.
func stringField(extra map[string]any, key string) string {
	if v, ok := extra[key].(string); ok {
		return v
	}
	return ""
}

// firstStringField returns the first non-empty string value among the
// given keys.
func firstStringField(extra map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := stringField(extra, k); v != "" {
			return v
		}
	}
	return ""
}

// emitResult writes the result JSON line and the matching sentinel line
// to w. Marshal failure is non-fatal: we still emit the sentinel so the
// poller has the verdict even if the JSON line is missing.
func emitResult(w io.Writer, rt RunTaskResult, log interface{ Error(error, string, ...any) }) {
	b, err := json.Marshal(rt)
	if err != nil {
		if log != nil {
			log.Error(err, "marshal RunTaskResult; emitting sentinel only")
		}
	} else {
		_, _ = fmt.Fprintf(w, "%s%s\n", RunTaskResultPrefix, string(b))
	}
	emitSentinel(w, sentinelForVerdict(rt.Verdict), rt.Summary)
}

// sentinelForVerdict maps a verdict string onto the sentinel token the
// Job poller scans for. GO -> GO; everything else that reached a model
// decision -> NO-GO; INCOMPLETE -> INCOMPLETE; anything unrecognized
// (including empty) -> INCOMPLETE, the conservative "did not clearly
// succeed" bucket.
func sentinelForVerdict(verdict string) string {
	switch foremanv1alpha1.AgenticTaskVerdict(verdict) {
	case foremanv1alpha1.AgenticTaskVerdictGo, foremanv1alpha1.AgenticTaskVerdictGatePass:
		return RunTaskSentinelGo
	case foremanv1alpha1.AgenticTaskVerdictNoGo,
		foremanv1alpha1.AgenticTaskVerdictGateFail:
		return RunTaskSentinelNoGo
	case foremanv1alpha1.AgenticTaskVerdictGateError:
		return RunTaskSentinelError
	default:
		// INCOMPLETE and any unknown value.
		return RunTaskSentinelIncomplete
	}
}

// emitSentinel writes a final sentinel line. The message tail is
// optional context; the poller matches on the leading token.
func emitSentinel(w io.Writer, sentinel, message string) {
	if message != "" {
		_, _ = fmt.Fprintf(w, "%s: %s\n", sentinel, message)
		return
	}
	_, _ = fmt.Fprintln(w, sentinel)
}
