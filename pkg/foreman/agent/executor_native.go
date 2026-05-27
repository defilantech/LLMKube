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
	"net/url"
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

	// InferenceBaseURLOverride bypasses InferenceService resolution
	// entirely and dispatches OAI requests to this URL. Use for tests
	// and stub OAI servers; for off-cluster, same-host installs (e.g.
	// foreman-agent on the M5 Max) prefer InferenceBaseURLHostOverride
	// instead so the live port from the metal-agent's Endpoints object
	// flows through on every llama-server respawn. Must include the
	// /v1 suffix; the OAI client appends /chat/completions.
	InferenceBaseURLOverride string

	// InferenceBaseURLHostOverride, when non-empty, rewrites the host
	// of the resolved InferenceService URL to this value (e.g.
	// "127.0.0.1") and substitutes the live port from the v1 Endpoints
	// object the metal-agent maintains for the InferenceService. The
	// scheme and path are kept from InferenceService.status.endpoint.
	//
	// This is the K8s-native answer for off-cluster, same-host installs:
	// the metal-agent rewrites Endpoints on every llama-server respawn,
	// so each task dispatch re-reads the current port through the
	// controller-runtime cache. Compare to InferenceBaseURLOverride,
	// which locks the port at install time and breaks on respawn
	// (#540).
	InferenceBaseURLHostOverride string

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

	// 2. Resolve the model-serving endpoint. Three branches:
	//
	//   - Deterministic Agent (gate role, M4): no LLM at all. Empty
	//     InferenceServiceRef AND provider unset / "local". The
	//     executor runs the agent's first non-terminal tool directly
	//     and skips the model loop entirely.
	//   - Cloud-proxy Agent (v0.2 Day 4): provider="cloud-proxy",
	//     dispatch via providerConfig.BaseURL + auth header from the
	//     referenced Secret. No InferenceService lookup.
	//   - Local Agent (default): resolveInferenceBaseURL reads
	//     InferenceService.status.endpoint and optionally rewrites the
	//     host (per #540).
	deterministic := isDeterministicAgent(&agent)
	var endpoint providerEndpoint
	if !deterministic {
		var err error
		endpoint, err = e.resolveProviderEndpoint(ctx, task.Namespace, &agent)
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
	return e.runLLMPath(ctx, task, &agent, endpoint, workspace, branch, registry, auth, start)
}

// runLLMPath is the model-in-the-loop continuation of Execute. Called
// only when the Agent runs a model loop (any non-deterministic Agent,
// local or cloud-proxy). The split from Execute is purely about
// cyclomatic complexity, not separation of concerns: nothing here
// should be reachable from the deterministic branch.
func (e *NativeAgentLoopExecutor) runLLMPath(
	ctx context.Context,
	task *foremanv1alpha1.AgenticTask,
	agent *foremanv1alpha1.Agent,
	endpoint providerEndpoint,
	workspace, branch string,
	registry ToolRegistry,
	auth *repo.Auth,
	start time.Time,
) (*Result, error) {
	log := logf.FromContext(ctx).WithName("native-agent-loop").WithValues("task", task.Name, "ns", task.Namespace)

	// 6. Build OAI client + loop. The auth header is empty for local
	// providers and "Bearer <token>" for cloud-proxy Agents whose
	// providerConfig carries an APIKeySecretRef.
	oaiOpts := []oai.Option{}
	if endpoint.authHeader != "" {
		oaiOpts = append(oaiOpts, oai.WithAuthHeader(endpoint.authHeader))
	}
	oaiClient := oai.New(
		endpoint.baseURL,
		durationFromSeconds(agent.Spec.RequestTimeoutSeconds, 600),
		int(agent.Spec.MaxRetries),
		oaiOpts...,
	)
	loopFactory := e.LoopFactory
	if loopFactory == nil {
		loopFactory = func(c *oai.Client, r ToolRegistry) *Loop { return NewLoop(c, r, nil) }
	}
	loop := loopFactory(oaiClient, registry)

	// 7. Build the user prompt from the task payload.
	userPrompt := buildUserPrompt(task)

	cfg := LoopConfig{
		Model:                  endpoint.modelName,
		SystemPrompt:           agent.Spec.SystemPrompt,
		UserPrompt:             userPrompt,
		Temperature:            parseTemperature(agent.Spec.Temperature),
		MaxTurns:               int(agent.Spec.MaxTurns),
		ContextWindowTokens:    int(agent.Spec.ContextWindowTokens),
		ObservationWindowTurns: int(agent.Spec.ObservationWindowTurns),
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
	//
	// Reviewer-role Agents take this same path on GO. A reviewer's
	// "GO" means APPROVE, not "commit this diff." Reviewers are
	// read-only by design (tool whitelist excludes write_file and
	// str_replace), so HasChanges would always be false and the
	// model's structured findings in submit_result.extra would get
	// dropped by the noChangesResult fallback. Route through
	// modelDecidedResult so extra.modelExtra (the full review
	// payload: reviewOutcome, findings, issueAsk, etc.) surfaces in
	// status.result.
	if verdict != foremanv1alpha1.AgenticTaskVerdictGo ||
		agent.Spec.Role == foremanv1alpha1.AgentRoleReviewer {
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

// providerEndpoint is the resolved triple the LLM path needs to dial
// any provider: where to POST, which model to name in the request body,
// and the optional Authorization header value. The cloud-proxy branch
// populates all three from Agent.spec.providerConfig + a referenced
// Secret; the local branch leaves authHeader empty and pulls modelName
// from Agent.spec.Model.
type providerEndpoint struct {
	baseURL    string
	modelName  string
	authHeader string
}

// isDeterministicAgent reports whether the Agent runs the model-free
// branch. Deterministic = no LLM at all, only direct tool dispatch
// (the M4 gate Agent shape). A cloud-proxy Agent is NEVER
// deterministic; it always runs the LLM loop against its remote
// endpoint.
func isDeterministicAgent(agent *foremanv1alpha1.Agent) bool {
	if agent.Spec.Provider != "" && agent.Spec.Provider != foremanv1alpha1.AgentProviderLocal {
		return false
	}
	return agent.Spec.InferenceServiceRef.Name == ""
}

// resolveProviderEndpoint dispatches to the right resolver based on
// Agent.spec.Provider. Empty / "local" -> existing InferenceService
// resolution; "cloud-proxy" -> providerConfig.BaseURL + Secret lookup
// for the auth header.
func (e *NativeAgentLoopExecutor) resolveProviderEndpoint(
	ctx context.Context, namespace string, agent *foremanv1alpha1.Agent,
) (providerEndpoint, error) {
	switch agent.Spec.Provider {
	case "", foremanv1alpha1.AgentProviderLocal:
		baseURL, err := e.resolveInferenceBaseURL(ctx, namespace, agent)
		if err != nil {
			return providerEndpoint{}, err
		}
		return providerEndpoint{baseURL: baseURL, modelName: agent.Spec.Model}, nil

	case foremanv1alpha1.AgentProviderCloudProxy:
		return e.resolveCloudProxyEndpoint(ctx, namespace, agent)

	default:
		return providerEndpoint{}, fmt.Errorf("unknown agent.spec.provider %q", agent.Spec.Provider)
	}
}

// resolveCloudProxyEndpoint reads providerConfig + the optional
// APIKeySecretRef to build the endpoint triple. baseURL and model are
// required; the Secret is optional (LAN-only LiteLLM gateways behind a
// NetworkPolicy can run without auth).
func (e *NativeAgentLoopExecutor) resolveCloudProxyEndpoint(
	ctx context.Context, namespace string, agent *foremanv1alpha1.Agent,
) (providerEndpoint, error) {
	cfg := agent.Spec.ProviderConfig
	if cfg == nil {
		return providerEndpoint{}, fmt.Errorf("agent.spec.providerConfig is required for provider=cloud-proxy")
	}
	if cfg.BaseURL == "" {
		return providerEndpoint{}, fmt.Errorf("agent.spec.providerConfig.baseURL is required for provider=cloud-proxy")
	}
	if cfg.Model == "" {
		return providerEndpoint{}, fmt.Errorf("agent.spec.providerConfig.model is required for provider=cloud-proxy")
	}
	ep := providerEndpoint{
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		modelName: cfg.Model,
	}
	if cfg.APIKeySecretRef != nil {
		token, err := e.resolveAuthToken(ctx, namespace, cfg.APIKeySecretRef)
		if err != nil {
			return providerEndpoint{}, err
		}
		ep.authHeader = "Bearer " + token
	}
	return ep, nil
}

// resolveAuthToken reads the named Secret + key from the Agent's
// namespace. Empty values are rejected so a misconfigured Secret
// surfaces as a clean executor error rather than a 401 from the
// upstream proxy after the loop has already burned a turn.
func (e *NativeAgentLoopExecutor) resolveAuthToken(
	ctx context.Context, namespace string, ref *corev1.SecretKeySelector,
) (string, error) {
	var secret corev1.Secret
	key := types.NamespacedName{Name: ref.Name, Namespace: namespace}
	if err := e.Client.Get(ctx, key, &secret); err != nil {
		return "", fmt.Errorf("get Secret %s for provider auth: %w", key, err)
	}
	b, ok := secret.Data[ref.Key]
	if !ok || len(b) == 0 {
		return "", fmt.Errorf("Secret %s has no value for key %q", key, ref.Key)
	}
	return strings.TrimSpace(string(b)), nil
}

// resolveInferenceBaseURL turns Agent.spec.inferenceServiceRef into a
// base URL the OAI client can hit. Three modes, in precedence order:
//
//  1. InferenceBaseURLOverride: full URL replacement. Used by tests
//     and stub OAI servers.
//  2. InferenceBaseURLHostOverride: read InferenceService.status.endpoint
//     for scheme + path, read the v1 Endpoints object the metal-agent
//     maintains for the live port, substitute the override host. Used
//     by off-cluster, same-host installs (foreman-agent on the M5 Max
//     where cluster DNS does not resolve but the metal-agent rewrites
//     Endpoints on every llama-server respawn).
//  3. Default: trust status.endpoint as the cluster-DNS form, used by
//     in-cluster foreman-agents.
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
	endpoint = strings.TrimRight(endpoint, "/")

	if e.InferenceBaseURLHostOverride != "" {
		return e.rewriteHostFromEndpoints(ctx, namespace, isvc.Name, endpoint)
	}
	return endpoint, nil
}

// rewriteHostFromEndpoints replaces the host of baseURL with the
// configured InferenceBaseURLHostOverride and the live port from the
// InferenceService's v1 Endpoints object. The Endpoints name mirrors
// the operator's sanitizeDNSName (dots become hyphens; see
// internal/controller/inferenceservice_controller.go).
//
// EndpointSlice migration is tracked separately and producer + consumer
// move together.
//
//nolint:staticcheck // SA1019: the metal-agent registers v1 Endpoints;
func (e *NativeAgentLoopExecutor) rewriteHostFromEndpoints(
	ctx context.Context, namespace, isvcName, baseURL string,
) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse status.endpoint %q: %w", baseURL, err)
	}
	var eps corev1.Endpoints
	epsName := strings.ReplaceAll(isvcName, ".", "-")
	key := types.NamespacedName{Namespace: namespace, Name: epsName}
	if err := e.Client.Get(ctx, key, &eps); err != nil {
		return "", fmt.Errorf("get Endpoints %s for host-override resolution: %w", key, err)
	}
	port, err := firstReadyPort(eps)
	if err != nil {
		return "", fmt.Errorf("Endpoints %s: %w", key, err)
	}
	u.Host = fmt.Sprintf("%s:%d", e.InferenceBaseURLHostOverride, port)
	return strings.TrimRight(u.String(), "/"), nil
}

// firstReadyPort returns the first port advertised on a subset that
// has at least one ready address. metal-agent registers one address +
// one port per InferenceService today; a future multi-replica metal
// path would need a smarter selector, but for the v0.2 same-host case
// "the only port" is the right port.
//
//nolint:staticcheck // SA1019: see rewriteHostFromEndpoints note.
func firstReadyPort(eps corev1.Endpoints) (int32, error) {
	for _, subset := range eps.Subsets {
		if len(subset.Addresses) == 0 {
			continue
		}
		for _, p := range subset.Ports {
			if p.Port > 0 {
				return p.Port, nil
			}
		}
	}
	// metal-agent has not registered llama-server yet, or just
	// respawned and is between unregister + register.
	return 0, errors.New("no ready address with a port on the Endpoints object")
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
