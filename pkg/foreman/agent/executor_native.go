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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

// NativeAgentLoopExecutor is the M3 production executor. It resolves an
// Agent CR + InferenceService endpoint, prepares a workspace by cloning
// the configured remote, builds a tool registry pinned to that
// workspace, runs the native agent loop, persists the transcript to a
// ConfigMap owner-ref'd to the task, and on a GO verdict commits +
// pushes the result branch to the fork.
//
// The executor never panics on a misconfigured Agent / InferenceService
// / git auth: it converts everything to a Result envelope so the
// watcher's terminal-status patch always carries something meaningful.
// Genuine system failures (apiserver unreachable, ctx cancelled mid-run)
// are returned as errors so the watcher can flag them distinctly.
type NativeAgentLoopExecutor struct {
	// Client is the Kubernetes client used to resolve Agent +
	// InferenceService refs and to write the transcript ConfigMap.
	Client client.Client

	// WorkspaceRoot is the parent dir for per-task clone workspaces.
	// Defaults to $HOME/foreman-workspaces.
	WorkspaceRoot string

	// GitRemoteURL is the git URL to clone from and push to. v0.1 clones
	// the fork directly (Option A from the M3 plan) so the same URL
	// serves both directions; v0.2 may separate clone-from-upstream +
	// push-to-fork.
	GitRemoteURL string

	// InferenceBaseURLOverride bypasses InferenceService resolution and
	// dispatches OAI requests to this URL instead. Required when the
	// foreman-agent runs outside the cluster (e.g. on the M5 Max host
	// where in-cluster DNS does not resolve). Must include the /v1
	// suffix; the OAI client appends /chat/completions.
	InferenceBaseURLOverride string

	// CommitAuthor and CommitCommitter are the git identities the
	// resulting commit carries. v0.1 sets both to the same Identity.
	CommitAuthor    repo.Identity
	CommitCommitter repo.Identity

	// KeepWorkspace, when true, preserves the per-task workspace dir
	// after the executor returns. Useful for debugging; defaults to
	// false (workspace removed in defer).
	KeepWorkspace bool

	// AuthFactory builds the GitHub auth. Field is exported so tests
	// can inject a fake auth (e.g. for file:// remote tests where no
	// real token is needed). nil falls back to repo.NewAuth(""), which
	// reads $GITHUB_TOKEN or ~/.config/foreman/github-token.
	AuthFactory func() (*repo.Auth, error)

	// LoopFactory builds the Loop given the resolved OAI client and
	// tool registry. Mostly to support tests with a fake loop; nil
	// uses the real NewLoop.
	LoopFactory func(client *oai.Client, registry ToolRegistry) *Loop

	// RegistryFactory builds the tool registry for a given workspace +
	// agent. Required; the executor refuses to start without one
	// because the tools subpackage owns workspace containment and we
	// do not want the executor reimplementing it.
	RegistryFactory func(workspace string, agent *foremanv1alpha1.Agent) (ToolRegistry, error)
}

// Kind identifies this executor in Result.Kind and in logs.
func (*NativeAgentLoopExecutor) Kind() string { return "native-agent-loop" }

// ErrNoAgentRef means task.spec.agentRef is unset; the M3 executor only
// runs Agent-driven tasks. Pre-M3 tasks (kind=freeform with a Payload.
// Agent string) are not supported by this executor.
var ErrNoAgentRef = errors.New("native-agent-loop: task.spec.agentRef is required")

// ErrRegistryFactoryNotSet is a programmer error: the executor requires
// a RegistryFactory at construction time so we never run the loop with
// an empty or wrong-workspace tool set.
var ErrRegistryFactoryNotSet = errors.New("native-agent-loop: RegistryFactory is required")

// Execute is the Executor implementation. See the package-level docs on
// NativeAgentLoopExecutor for the high-level flow; this method drives
// it step by step.
func (e *NativeAgentLoopExecutor) Execute(ctx context.Context, task *foremanv1alpha1.AgenticTask) (*Result, error) {
	log := logf.FromContext(ctx).WithName("native-agent-loop").WithValues("task", task.Name, "ns", task.Namespace)
	start := time.Now()

	if e.RegistryFactory == nil {
		return nil, ErrRegistryFactoryNotSet
	}
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name == "" {
		return nil, ErrNoAgentRef
	}

	// 1. Resolve the Agent CR.
	var agent foremanv1alpha1.Agent
	agentKey := types.NamespacedName{Namespace: task.Namespace, Name: task.Spec.AgentRef.Name}
	if err := e.Client.Get(ctx, agentKey, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			// Scheduler should have caught this; if we got here the
			// Agent was deleted between scheduling and execution.
			return e.failResult(start, "AgentNotFound",
				fmt.Sprintf("Agent %q not found in namespace %q", agentKey.Name, agentKey.Namespace)), nil
		}
		return nil, fmt.Errorf("resolve agent: %w", err)
	}

	// 2. Resolve the inference base URL -- but only when the Agent has
	// an InferenceServiceRef. Deterministic Agents (M4 gate Agent: no
	// LLM, just tool dispatch) leave InferenceServiceRef empty; their
	// path runs every step from 3 onward except the loop.
	deterministic := agent.Spec.InferenceServiceRef.Name == ""
	var baseURL string
	if !deterministic {
		var err error
		baseURL, err = e.resolveInferenceBaseURL(ctx, task.Namespace, &agent)
		if err != nil {
			return e.failResult(start, "InferenceServiceUnavailable", err.Error()), nil
		}
	}

	// 3. Prep workspace + clone.
	workspaceRoot := e.WorkspaceRoot
	if workspaceRoot == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return nil, fmt.Errorf("user home dir: %w", herr)
		}
		workspaceRoot = filepath.Join(home, "foreman-workspaces")
	}
	workspace := filepath.Join(workspaceRoot, task.Namespace, task.Name)
	if err := os.MkdirAll(filepath.Dir(workspace), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir workspace parent: %w", err)
	}
	// Reset workspace if it exists from a prior partial run; safer than
	// re-using a half-cloned tree.
	if err := os.RemoveAll(workspace); err != nil {
		return nil, fmt.Errorf("reset workspace: %w", err)
	}
	if !e.KeepWorkspace {
		defer func() {
			if rmErr := os.RemoveAll(workspace); rmErr != nil {
				log.Error(rmErr, "workspace cleanup failed")
			}
		}()
	}

	auth, err := e.buildAuth()
	if err != nil {
		return e.failResult(start, "AuthUnavailable", err.Error()), nil
	}
	defer func() { _ = auth.Close() }()

	cloneURL := e.GitRemoteURL
	if cloneURL == "" {
		return e.failResult(start, "GitRemoteURLNotConfigured",
			"foreman-agent was not started with --git-remote-url; cannot clone"), nil
	}
	if err := repo.Clone(ctx, repo.CloneOptions{
		RemoteURL: cloneURL,
		Dest:      workspace,
		Auth:      auth,
	}); err != nil {
		return e.failResult(start, "CloneFailed", err.Error()), nil
	}

	// 4. Branch off main.
	branch := branchNameForTask(task)
	if err := repo.CreateAndCheckoutBranch(ctx, workspace, branch); err != nil {
		return e.failResult(start, "BranchCheckoutFailed", err.Error()), nil
	}

	// 5. Build tool registry pinned to this workspace + filtered by the
	// Agent's tool whitelist.
	registry, err := e.RegistryFactory(workspace, &agent)
	if err != nil {
		return e.failResult(start, "ToolRegistryBuildFailed", err.Error()), nil
	}

	// 5b. Deterministic Agent path: no LLM loop. Dispatch the agent's
	// first tool directly with the task payload as JSON arguments. The
	// gate Agent uses this; M5+ reviewer agents use the LLM path.
	if deterministic {
		return e.executeDeterministic(ctx, task, &agent, branch, registry, start), nil
	}

	// 6+. LLM-driven path: extracted to keep Execute below the
	// cyclomatic-complexity threshold. runLLMPath owns OAI + loop +
	// transcript + commit/push.
	return e.runLLMPath(ctx, task, &agent, baseURL, workspace, branch, registry, auth, start)
}

// runLLMPath is the model-in-the-loop continuation of Execute. Called
// only when the Agent has a non-empty InferenceServiceRef. The split
// from Execute is purely about cyclomatic complexity, not separation
// of concerns: nothing here should be reachable from the deterministic
// branch.
func (e *NativeAgentLoopExecutor) runLLMPath(
	ctx context.Context,
	task *foremanv1alpha1.AgenticTask,
	agent *foremanv1alpha1.Agent,
	baseURL, workspace, branch string,
	registry ToolRegistry,
	auth *repo.Auth,
	start time.Time,
) (*Result, error) {
	log := logf.FromContext(ctx).WithName("native-agent-loop").WithValues("task", task.Name, "ns", task.Namespace)

	// 6. Build OAI client + loop.
	oaiClient := oai.New(
		baseURL,
		durationFromSeconds(agent.Spec.RequestTimeoutSeconds, 600),
		int(agent.Spec.MaxRetries),
	)
	loopFactory := e.LoopFactory
	if loopFactory == nil {
		loopFactory = func(c *oai.Client, r ToolRegistry) *Loop { return NewLoop(c, r, nil) }
	}
	loop := loopFactory(oaiClient, registry)

	// 7. Build the user prompt from the task payload.
	userPrompt := buildUserPrompt(task)

	cfg := LoopConfig{
		Model:        agent.Spec.Model,
		SystemPrompt: agent.Spec.SystemPrompt,
		UserPrompt:   userPrompt,
		Temperature:  parseTemperature(agent.Spec.Temperature),
		MaxTurns:     int(agent.Spec.MaxTurns),
	}

	// 8. Run the loop. Always persist the transcript afterwards,
	// even on error, so the executor's terminal status carries a
	// pointer to the partial transcript.
	loopRes, loopErr := loop.Run(ctx, cfg)
	transcriptRef, twErr := WriteTranscript(ctx, e.Client, task, loopRes)
	if twErr != nil {
		log.Error(twErr, "transcript write failed; continuing")
	}

	if loopErr != nil {
		switch {
		case errors.Is(loopErr, ErrMaxTurnsExhausted):
			return e.incompleteResult(start, transcriptRef, loopRes,
				"MaxTurnsExhausted", "model did not call submit_result within max_turns"), nil
		case errors.Is(loopErr, ErrAssistantNoToolCalls):
			return e.incompleteResult(start, transcriptRef, loopRes,
				"AssistantHallucinatedFinish", "model returned text without tool_calls; loop cannot make progress"), nil
		default:
			// Anything else is a system / transport failure: bubble up
			// as an error so the watcher records ExecutorError.
			return nil, loopErr
		}
	}

	if loopRes.Terminal == nil {
		// Defensive: loop returned nil error but no terminal. Shouldn't
		// happen given the loop's invariants; report it explicitly.
		return e.incompleteResult(start, transcriptRef, loopRes,
			"LoopContractViolation", "loop returned nil error but no terminal result"), nil
	}

	verdict := foremanv1alpha1.AgenticTaskVerdict(loopRes.Terminal.Verdict)

	// 9. Non-GO verdicts: no commit, no push, just record the model's
	// stated outcome and return.
	if verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		return e.modelDecidedResult(start, transcriptRef, loopRes, verdict), nil
	}

	// 10. GO verdict: check for changes first, then commit + push. If
	// the model emitted GO but never edited a file, NO-CHANGES is the
	// honest outcome regardless of whether commit_message was set.
	hasChanges, hcErr := repo.HasChanges(ctx, workspace)
	if hcErr != nil {
		return e.commitRejectedResult(start, transcriptRef, loopRes, branch, hcErr), nil
	}
	if !hasChanges {
		return e.noChangesResult(start, transcriptRef, loopRes, branch), nil
	}

	sha, commitErr := repo.Commit(ctx, repo.CommitOptions{
		Workspace: workspace,
		Message:   loopRes.Terminal.CommitMessage,
		Author:    e.CommitAuthor,
		Committer: e.CommitCommitter,
	})
	if errors.Is(commitErr, repo.ErrNothingToCommit) {
		// Model said GO but never edited anything. Honest report: this
		// is a NO-CHANGES outcome, NOT a GO. The autofix pipeline saw
		// this often enough to deserve a distinct outcome string.
		return e.noChangesResult(start, transcriptRef, loopRes, branch), nil
	}
	if commitErr != nil {
		return e.commitRejectedResult(start, transcriptRef, loopRes, branch, commitErr), nil
	}

	if err := repo.Push(ctx, repo.PushOptions{
		Workspace: workspace,
		Branch:    branch,
		Auth:      auth,
	}); err != nil {
		return e.pushFailedResult(start, transcriptRef, loopRes, branch, sha, err), nil
	}

	return e.goResult(start, transcriptRef, loopRes, branch, sha), nil
}

// executeDeterministic is the no-LLM execution path. Used by Agents
// whose work is a single Kubernetes Job (gate Agent: run_gate_job) or
// any future deterministic workload. The flow:
//
//  1. Pick the agent's first non-terminal tool. If the Agent's tool
//     whitelist lists `submit_result`, treat it as terminal-only and
//     dispatch the first other tool. If exactly one tool is named
//     (the gate case), dispatch that one.
//  2. Build a payload-as-args JSON from the task's spec.payload.
//  3. Dispatch the tool. If the tool itself returns Terminal=true with
//     a Verdict, that verdict drives the AgenticTask status. Otherwise
//     wrap a generic GO/NO-GO based on whether the tool succeeded.
//
// No transcript ConfigMap is written -- a deterministic run has no
// model turns to preserve. Result.Extra captures the dispatched
// tool's name + output for debugging.
func (e *NativeAgentLoopExecutor) executeDeterministic(
	ctx context.Context,
	task *foremanv1alpha1.AgenticTask,
	agent *foremanv1alpha1.Agent,
	branch string,
	registry ToolRegistry,
	start time.Time,
) *Result {
	toolName := pickDeterministicTool(agent.Spec.Tools)
	if toolName == "" {
		return e.failResult(start, "NoDeterministicTool",
			"deterministic Agent has no non-terminal tool in spec.tools; expected exactly one (e.g. run_gate_job)")
	}

	// The task payload becomes the tool's arguments. For the gate
	// Agent's run_gate_job tool, this means {repo, branch, checks,
	// cloneURL} surface from spec.payload via well-known fields. The
	// cloneURL override carries the executor's --git-remote-url so the
	// gate Job clones from the fork the upstream coder pushed to,
	// rather than the upstream payload.repo where the branch does not
	// yet exist.
	args := buildDeterministicArgs(task, branch, e.GitRemoteURL)

	result, dispatchErr := registry.Dispatch(ctx, toolName, args)
	if dispatchErr != nil {
		return e.failResult(start, "ToolDispatchFailed",
			fmt.Sprintf("%s: %s", toolName, dispatchErr.Error()))
	}

	// If the tool was self-terminal (e.g. submit_result), it carried
	// its own Verdict. Otherwise fall back to a synthetic GO so the
	// task at least reaches Succeeded; the operator can inspect
	// Result.Extra.toolOutput for what happened.
	verdict := foremanv1alpha1.AgenticTaskVerdictGo
	if result.Terminal && result.Verdict != "" {
		verdict = foremanv1alpha1.AgenticTaskVerdict(result.Verdict)
	}

	summary := result.Summary
	if summary == "" {
		summary = fmt.Sprintf("deterministic %s tool returned verdict=%s", toolName, verdict)
	}

	r := NewResult(e.Kind(), verdict, summary, time.Since(start))
	r.Extra = map[string]any{
		"outcome":        "",
		"deterministic":  true,
		"dispatchedTool": toolName,
		"toolOutput":     result.Output,
		"modelExtra":     result.Extra,
		"intendedBranch": branch,
	}
	return r
}

// pickDeterministicTool finds the first non-terminal tool in the
// agent's whitelist. submit_result is always terminal (the LLM-loop
// exit tool) so we skip it here. Returns "" if no candidate found.
func pickDeterministicTool(tools []string) string {
	for _, t := range tools {
		if t == "" || t == "submit_result" {
			continue
		}
		return t
	}
	return ""
}

// buildDeterministicArgs synthesizes a JSON args blob the deterministic
// tool receives. The gate Agent's run_gate_job tool will read
// {repo, branch, cloneURL} from this; other deterministic tools can
// extend the shape as needed.
//
// cloneURL is the executor's --git-remote-url passed through verbatim.
// When set, the gate Job clones from this URL instead of constructing
// one from CloneURLBase + payload.repo. v0.1 needs this because the
// upstream coder task pushes to a fork (the foreman-agent's
// --git-remote-url) and the gate must verify that branch on the fork,
// not on the upstream payload.repo where the branch does not yet
// exist. Empty cloneURL preserves the M4 default (upstream + repo).
func buildDeterministicArgs(task *foremanv1alpha1.AgenticTask, branch, cloneURL string) json.RawMessage {
	args := map[string]any{
		"repo":     task.Spec.Payload.Repo,
		"branch":   branch,
		"cloneURL": cloneURL,
		"issue":    task.Spec.Payload.Issue,
		"prompt":   task.Spec.Payload.Prompt,
		"taskRef":  map[string]string{"namespace": task.Namespace, "name": task.Name},
	}
	out, _ := json.Marshal(args)
	return out
}

// resolveInferenceBaseURL turns Agent.spec.inferenceServiceRef into a
// base URL the OAI client can hit. Override wins for the v0.1 path
// where foreman-agent runs outside the cluster; otherwise we read
// InferenceService.status.endpoint and trim the chat-completions suffix.
func (e *NativeAgentLoopExecutor) resolveInferenceBaseURL(
	ctx context.Context,
	namespace string,
	agent *foremanv1alpha1.Agent,
) (string, error) {
	if e.InferenceBaseURLOverride != "" {
		return strings.TrimRight(e.InferenceBaseURLOverride, "/"), nil
	}
	if agent.Spec.InferenceServiceRef.Name == "" {
		return "", fmt.Errorf("agent.spec.inferenceServiceRef.name is empty")
	}
	var isvc inferencev1alpha1.InferenceService
	key := types.NamespacedName{Namespace: namespace, Name: agent.Spec.InferenceServiceRef.Name}
	if err := e.Client.Get(ctx, key, &isvc); err != nil {
		return "", fmt.Errorf("get InferenceService %s: %w", key, err)
	}
	endpoint := isvc.Status.Endpoint
	if endpoint == "" {
		return "", fmt.Errorf("InferenceService %s has empty status.endpoint", key)
	}
	// status.endpoint is the chat-completions URL; the OAI client
	// expects the /v1 base.
	endpoint = strings.TrimSuffix(endpoint, "/chat/completions")
	return strings.TrimRight(endpoint, "/"), nil
}

// buildAuth resolves credentials via the configured AuthFactory or
// repo.NewAuth's default lookup chain.
func (e *NativeAgentLoopExecutor) buildAuth() (*repo.Auth, error) {
	if e.AuthFactory != nil {
		return e.AuthFactory()
	}
	return repo.NewAuth("")
}

// --- Result builders ------------------------------------------------------

// failResult builds a verdict=INCOMPLETE Result that the watcher will
// patch as phase=Succeeded; the failure shape lives in Result.Extra.
// Used for environment errors (Agent not found, clone failed, etc.)
// where the executor never reached the model. We do not return an
// error from these paths because the failure is data-shaped: the task
// got a real, structured outcome that a downstream consumer can read.
func (e *NativeAgentLoopExecutor) failResult(start time.Time, reason, message string) *Result {
	r := NewResult(e.Kind(), foremanv1alpha1.AgenticTaskVerdictIncomplete, message, time.Since(start))
	r.Extra = map[string]any{
		"reason":  reason,
		"outcome": "EXECUTOR-PRECONDITION-FAILED",
	}
	return r
}

func (e *NativeAgentLoopExecutor) incompleteResult(
	start time.Time, tref corev1.ObjectReference, lr *LoopResult, reason, msg string,
) *Result {
	r := NewResult(e.Kind(), foremanv1alpha1.AgenticTaskVerdictIncomplete, msg, time.Since(start))
	r.Extra = map[string]any{
		"reason":        reason,
		"outcome":       "LOOP-INCOMPLETE",
		"transcriptRef": objRefAsMap(tref),
		"turnCount":     lr.Turns,
	}
	return r
}

func (e *NativeAgentLoopExecutor) modelDecidedResult(
	start time.Time, tref corev1.ObjectReference, lr *LoopResult,
	verdict foremanv1alpha1.AgenticTaskVerdict,
) *Result {
	r := NewResult(e.Kind(), verdict, lr.Terminal.Summary, time.Since(start))
	r.Extra = map[string]any{
		"outcome":       "MODEL-DECIDED",
		"transcriptRef": objRefAsMap(tref),
		"turnCount":     lr.Turns,
		"modelExtra":    lr.Terminal.Extra,
	}
	return r
}

func (e *NativeAgentLoopExecutor) noChangesResult(
	start time.Time, tref corev1.ObjectReference, lr *LoopResult, branch string,
) *Result {
	r := NewResult(e.Kind(), foremanv1alpha1.AgenticTaskVerdictNoGo,
		"model emitted GO but produced no diff", time.Since(start))
	r.Extra = map[string]any{
		"outcome":        "NO-CHANGES",
		"intendedBranch": branch,
		"transcriptRef":  objRefAsMap(tref),
		"turnCount":      lr.Turns,
		"modelSummary":   lr.Terminal.Summary,
	}
	return r
}

func (e *NativeAgentLoopExecutor) commitRejectedResult(
	start time.Time, tref corev1.ObjectReference, lr *LoopResult, branch string, cause error,
) *Result {
	r := NewResult(e.Kind(), foremanv1alpha1.AgenticTaskVerdictNoGo,
		"commit rejected", time.Since(start))
	r.Extra = map[string]any{
		"outcome":        "COMMIT-REJECTED",
		"intendedBranch": branch,
		"error":          cause.Error(),
		"transcriptRef":  objRefAsMap(tref),
		"turnCount":      lr.Turns,
	}
	return r
}

func (e *NativeAgentLoopExecutor) pushFailedResult(
	start time.Time, tref corev1.ObjectReference, lr *LoopResult,
	branch, sha string, cause error,
) *Result {
	r := NewResult(e.Kind(), foremanv1alpha1.AgenticTaskVerdictNoGo,
		"push to fork failed", time.Since(start))
	r.Extra = map[string]any{
		"outcome":        "PUSH-FAILED",
		"intendedBranch": branch,
		"commitSHA":      sha,
		"error":          cause.Error(),
		"transcriptRef":  objRefAsMap(tref),
		"turnCount":      lr.Turns,
	}
	return r
}

func (e *NativeAgentLoopExecutor) goResult(
	start time.Time, tref corev1.ObjectReference, lr *LoopResult, branch, sha string,
) *Result {
	r := NewResult(e.Kind(), foremanv1alpha1.AgenticTaskVerdictGo,
		lr.Terminal.Summary, time.Since(start))
	r.Extra = map[string]any{
		"outcome":       "",
		"branch":        branch,
		"commitSHA":     sha,
		"transcriptRef": objRefAsMap(tref),
		"turnCount":     lr.Turns,
		"modelExtra":    lr.Terminal.Extra,
	}
	return r
}

// objRefAsMap converts an empty ObjectReference into nil and a populated
// one into a small map. The map shape stays stable in the Result JSON
// regardless of how the executor evolves (we do not want to leak
// k8s.io/api/core/v1 types into Result.Extra's eventual JSON shape).
func objRefAsMap(ref corev1.ObjectReference) map[string]any {
	if ref.Name == "" {
		return nil
	}
	return map[string]any{
		"kind":       ref.Kind,
		"apiVersion": ref.APIVersion,
		"namespace":  ref.Namespace,
		"name":       ref.Name,
	}
}

// --- helpers --------------------------------------------------------------

// branchNameForTask picks a branch name for the task. Precedence:
//
//  1. Explicit task.Spec.Payload.Branch wins. This is the documented
//     hand-off for verify tasks (which gate a branch the upstream coder
//     task already produced) and the escape hatch for any task that
//     wants to pin the branch name from the caller.
//  2. issue-fix with Payload.Issue > 0 derives foreman/issue-<N>.
//  3. Everything else falls back to foreman/<task-name>.
//
// v0.1 keeps slugs minimal; v0.2 may take an explicit IssueTitle field
// on AgenticTaskPayload and slug it for the branch.
func branchNameForTask(task *foremanv1alpha1.AgenticTask) string {
	if task.Spec.Payload.Branch != "" {
		return task.Spec.Payload.Branch
	}
	if task.Spec.Kind == foremanv1alpha1.AgenticTaskKindIssueFix && task.Spec.Payload.Issue > 0 {
		return fmt.Sprintf("%s/issue-%d", repo.BranchPrefix, task.Spec.Payload.Issue)
	}
	return fmt.Sprintf("%s/%s", repo.BranchPrefix, task.Name)
}

// buildUserPrompt assembles the prompt the loop sends as the first user
// message. v0.1 is straightforward: for issue-fix, drop the issue
// number + repo + prompt body into a small template; for freeform,
// pass the prompt through unchanged.
func buildUserPrompt(task *foremanv1alpha1.AgenticTask) string {
	p := task.Spec.Payload
	var b strings.Builder
	switch task.Spec.Kind {
	case foremanv1alpha1.AgenticTaskKindIssueFix:
		fmt.Fprintf(&b, "You are working on issue #%d of repository %s.\n\n", p.Issue, p.Repo)
		if p.Prompt != "" {
			fmt.Fprintf(&b, "Issue context:\n%s\n\n", p.Prompt)
		}
		b.WriteString("The repository is checked out in the workspace at the current branch.\n")
		b.WriteString("Make the minimum change that addresses the issue, then call submit_result.\n")
		b.WriteString("Include `Fixes #")
		fmt.Fprintf(&b, "%d", p.Issue)
		b.WriteString("` in the commit_message trailer when verdict is GO.\n")
	default:
		b.WriteString(p.Prompt)
	}
	return b.String()
}

// parseTemperature turns the *string Agent.spec.temperature into a
// *float64 the OAI client wants. Nil string -> nil pointer (server
// default); bad string -> nil pointer (logged elsewhere; we do not
// fail the run over a malformed cosmetic knob).
func parseTemperature(t *string) *float64 {
	if t == nil || *t == "" {
		return nil
	}
	v, err := strconv.ParseFloat(*t, 64)
	if err != nil {
		return nil
	}
	return &v
}

// durationFromSeconds converts an int32 seconds field to a Duration,
// falling back to the supplied default if the field is unset.
func durationFromSeconds(secs int32, fallbackSecs int) time.Duration {
	if secs <= 0 {
		return time.Duration(fallbackSecs) * time.Second
	}
	return time.Duration(secs) * time.Second
}
