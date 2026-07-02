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
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repomap"
	"github.com/defilantech/llmkube/pkg/foreman/agent/reviewer"
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

	// UpstreamURLForRepo derives the upstream project's git URL from a
	// payload.repo "owner/name" slug; the coder branch is cut from that
	// upstream's base ref so a stale fork default branch does not produce a
	// stale-base branch (#813). Nil uses the default GitHub derivation
	// (https://github.com/<repo>.git); tests inject a local path. Returning ""
	// makes the executor fall back to branching from the cloned fork HEAD.
	UpstreamURLForRepo func(repoSlug string) string

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

	// IssueFetcher pulls the GitHub issue title + body so buildUserPrompt
	// can include them. Best-effort: a nil fetcher or a failed fetch
	// runs the loop with the empty-body behavior (pre-#571 default),
	// preserving backward compatibility. Tests inject a fake; production
	// wires githubissue.NewClient() in cmd/foreman-agent.
	IssueFetcher githubissue.Fetcher

	// CoderJobSubmitter, when non-nil, routes Job-mode Agents
	// (spec.execution.mode == Job) to an ephemeral per-task Kubernetes Job
	// instead of running the loop in this process (#620). It is the seam
	// to pkg/foreman/agent/tools.RunCoderJob; the agent package cannot
	// import tools directly (tools imports agent), so cmd/foreman-agent
	// wires a closure over RunCoderJob.Run here.
	//
	// RECURSION GUARD: the coder Job itself runs `foreman-agent run-task`,
	// which calls Execute with the SAME Agent (still mode==Job). RunTask
	// builds its executor WITHOUT a CoderJobSubmitter, so useCoderJobPath
	// returns false inside the Job and the loop runs in-process there. The
	// Job IS the execution; only the watcher's executor (the one this
	// field is set on) ever submits a Job. See executor_coderjob.go.
	CoderJobSubmitter CoderJobSubmitter

	// EnvtestJobRunner, when non-nil, verifies envtest-backed packages in a
	// clean-room Job (`make test`) on the pushed branch after a coder GO
	// (#859). cmd/foreman-agent wires a closure over tools.RunGateJobTool
	// here. Nil skips the post-push envtest gate (e.g. the in-process
	// run-task path, where the clean-room gate Job is the backstop).
	EnvtestJobRunner EnvtestJobRunner
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
			return e.failResult(start, foremanv1alpha1.FailureAgentNotFound,
				fmt.Sprintf("Agent %q not found in namespace %q", agentKey.Name, agentKey.Namespace)), nil
		}
		return nil, fmt.Errorf("resolve agent: %w", err)
	}

	// 1b. Job-mode dispatch (#620). When the Agent selects spec.execution.
	// mode == Job AND a CoderJobSubmitter is wired, the loop + workspace +
	// toolchain run in an ephemeral per-task Kubernetes Job instead of in
	// this process. None of the in-process steps below (endpoint resolve,
	// clone, registry, loop) happen here -- the Job does all of that by
	// running `foreman-agent run-task`, which re-enters Execute IN-PROCESS
	// (RunTask wires no submitter, so useCoderJobPath is false there). The
	// InProcess path (Execution nil or mode==InProcess, or no submitter
	// wired) is left byte-for-byte unchanged below.
	if e.useCoderJobPath(&agent) {
		return e.executeCoderJob(ctx, task, &agent, start), nil
	}

	// 2. Resolve the model-serving endpoint. Three branches:
	//
	//   - Deterministic Agent (gate role, M4): no LLM at all. Empty
	//     InferenceServiceRef AND provider unset / "local". The
	//     executor runs the agent's first non-terminal tool directly
	//     and skips the model loop entirely.
	//   - Cloud-proxy Agent (v0.2): provider="cloud-proxy",
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
			return e.failResult(start, foremanv1alpha1.FailureInferenceServiceUnavailable, err.Error()), nil
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
	if err := removeAllResilient(workspace); err != nil {
		return nil, fmt.Errorf("reset workspace: %w", err)
	}
	if !e.KeepWorkspace {
		defer func() {
			if rmErr := removeAllResilient(workspace); rmErr != nil {
				log.Error(rmErr, "workspace cleanup failed")
			}
		}()
	}

	auth, err := e.buildAuth()
	if err != nil {
		return e.failResult(start, foremanv1alpha1.FailureAuthUnavailable, err.Error()), nil
	}
	defer func() { _ = auth.Close() }()

	// resolveUpstream derives a repo's git URL from its payload.repo
	// "owner/name" slug. It picks the clone target when no static fork
	// remote is configured (#915) and cuts the task branch from the
	// upstream base below (#813). The test override lets envtest inject a
	// local remote.
	resolveUpstream := upstreamURLForRepo
	if e.UpstreamURLForRepo != nil {
		resolveUpstream = e.UpstreamURLForRepo
	}

	// Clone target: the statically configured fork remote when set,
	// otherwise the task's own repo (#915). Deriving the clone+push target
	// from payload.repo lets a single agent serve many repos instead of
	// being pinned to one --git-remote-url; the branch still pushes to this
	// clone's origin, which in that case is the task's repo itself.
	cloneURL := e.GitRemoteURL
	if cloneURL == "" {
		cloneURL = resolveUpstream(task.Spec.Payload.Repo)
	}
	if cloneURL == "" {
		return e.failResult(start, foremanv1alpha1.FailureGitRemoteNotConfigured,
			"no --git-remote-url configured and task payload.repo is empty or invalid; cannot clone"), nil
	}
	if err := repo.Clone(ctx, repo.CloneOptions{
		RemoteURL: cloneURL,
		Dest:      workspace,
		Auth:      auth,
	}); err != nil {
		return e.failResult(start, foremanv1alpha1.FailureCloneFailed, err.Error()), nil
	}

	// 4. Branch off the CURRENT upstream base (#813). The clone above is the
	// fork (origin = push target); cutting the task branch from the fork's
	// default branch produces a stale-base branch whenever the fork lags
	// upstream. When the task carries an upstream repo slug (payload.repo),
	// fetch its base ref and branch from that instead. Origin stays the fork,
	// so the branch still pushes there for the PR. Freeform tasks without a
	// repo slug fall back to branching from the cloned fork HEAD. When the
	// clone target was itself derived from payload.repo (#915), origin and
	// upstream resolve to the same URL — branching off the base is then a
	// no-op-equivalent that still yields a current-base branch.
	branch := branchNameForTask(task)
	baseBranch := baseBranchOrDefault(task.Spec.Payload.BaseBranch)
	if upstreamURL := resolveUpstream(task.Spec.Payload.Repo); upstreamURL != "" {
		if err := repo.CreateBranchFromUpstream(ctx, repo.UpstreamBranchOptions{
			Workspace:   workspace,
			Branch:      branch,
			UpstreamURL: upstreamURL,
			BaseBranch:  baseBranch,
			Auth:        auth,
		}); err != nil {
			// Fail loud rather than silently branching from the stale fork
			// base; bucket with CloneFailed for the retry policy.
			return e.failResult(start, foremanv1alpha1.FailureCloneFailed, err.Error()), nil
		}
	} else if err := repo.CreateAndCheckoutBranch(ctx, workspace, branch); err != nil {
		// Branch checkout is part of workspace prep; bucket with
		// CloneFailed so downstream retry policy treats them the same.
		return e.failResult(start, foremanv1alpha1.FailureCloneFailed, err.Error()), nil
	}

	// 5. Build tool registry pinned to this workspace + filtered by the
	// Agent's tool whitelist.
	registry, err := e.RegistryFactory(workspace, &agent)
	if err != nil {
		// Registry build failure is an operator config issue (bad
		// whitelist name, duplicate tool); not a runtime model
		// failure. Bucket as infrastructure so it surfaces distinctly
		// from in-loop tool errors.
		return e.failResult(start, foremanv1alpha1.FailureInfrastructureError, err.Error()), nil
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
		// Per-request header timeout (#532): how long one turn waits for
		// the first token before retrying. The loop-wide budget is
		// applied separately via LoopConfig.LoopBudget below.
		durationFromSeconds(agent.Spec.RequestTurnTimeoutSeconds, 120),
		int(agent.Spec.MaxRetries),
		oaiOpts...,
	)
	loopFactory := e.LoopFactory
	if loopFactory == nil {
		loopFactory = func(c *oai.Client, r ToolRegistry) *Loop { return NewLoop(c, r, nil) }
	}
	loop := loopFactory(oaiClient, registry)

	// 7. Build the user prompt from the task payload. The composition,
	// outermost-first:
	//
	//   1. Workspace orientation block (always; #567)
	//   2. Repo-map summary (coder Agents only; #560)
	//   3. The role-specific task prompt from buildUserPrompt
	//
	// The orientation block gives the model a stable anchor for
	// "where is my workspace" so it does not fall back to `find /`
	// and pick up stale paths from previous batches. The cd-guard in
	// BashTool enforces the boundary; this block tells the model the
	// contract exists. The repo-map prefix (when present) sits between
	// the two so the model reads orientation -> repo map -> task in a
	// natural order.
	//
	// Before assembling, fetch the GitHub issue body when the task is
	// an issue-fix with an empty payload prompt (#571). The M6 stub
	// planner synthesizes AgenticTasks from issue numbers and leaves
	// the prompt empty; without this fetch the model is asked to fix
	// "#510" with no knowledge of what #510 is about. Best-effort: a
	// failed fetch logs and the loop runs with the pre-#571 behavior.
	fetchIssueBodyIfNeeded(ctx, e.IssueFetcher, task, auth, log)
	userPrompt := buildUserPrompt(task)
	// issueText ranks files for both the repo-map prefix (coder Agents) and the
	// scope-overlap guard in the coder gate verifier (#782).
	issueText := repoMapQuery(task)
	if agent.Spec.Role == foremanv1alpha1.AgentRoleCoder {
		summary, mapErr := repomap.Build(ctx, workspace, issueText, repomap.Options{})
		switch {
		case mapErr != nil:
			log.Info("repomap build failed; continuing without summary", "err", mapErr.Error())
		case summary != "":
			userPrompt = summary + "\n" + userPrompt
		}
	}
	userPrompt = workspaceOrientationBlock(workspace) + "\n" + userPrompt

	// Resolve an optional ModelProfile and layer it onto the loop config
	// below. Cluster-scoped; a dangling ref degrades gracefully (run without
	// the profile) rather than failing the task.
	var modelProfile *foremanv1alpha1.ModelProfile
	if ref := agent.Spec.ModelProfileRef; ref != "" {
		var mp foremanv1alpha1.ModelProfile
		switch err := e.Client.Get(ctx, types.NamespacedName{Name: ref}, &mp); {
		case err == nil:
			modelProfile = &mp
		case apierrors.IsNotFound(err):
			log.Info("modelProfileRef not found; running without profile", "profile", ref)
		default:
			return nil, fmt.Errorf("resolve model profile %q: %w", ref, err)
		}
	}

	cfg := LoopConfig{
		Model:                  endpoint.modelName,
		SystemPrompt:           agent.Spec.SystemPrompt,
		UserPrompt:             userPrompt,
		Temperature:            parseTemperature(agent.Spec.Temperature),
		MaxTurns:               int(agent.Spec.MaxTurns),
		ContextWindowTokens:    int(agent.Spec.ContextWindowTokens),
		ObservationWindowTurns: int(agent.Spec.ObservationWindowTurns),
		ContextStrategy:        agent.Spec.ContextStrategy,
		Progress:               progressConfigFromAgent(agent),
		// Loop-wide wall-clock budget (#532). Repurposed from the old
		// per-request meaning of RequestTimeoutSeconds; the per-request
		// header timeout now lives on the OAI client above.
		LoopBudget: durationFromSeconds(agent.Spec.RequestTimeoutSeconds, 3600),
	}

	// Coder gate feedback loop (#749): coders verify their work through a
	// deterministic in-workspace gate (fmt / vet / build / lint), not by
	// hand-running tests (which on a Mac workspace cannot run the envtest
	// suite and spiral the loop). Wire the verifier so a GO that fails the
	// fast checks is sent back for a fix instead of landing dirty.
	//
	// gateAdvisories accumulates non-blocking findings from the gate that
	// survive to the GO result's Extra map so the reviewer can act on them.
	gateAdvisories := &[]advisory{}
	if agent.Spec.Role == foremanv1alpha1.AgentRoleCoder {
		cfg.MaxVerifyRetries = coderGateMaxRetries
		cfg.VerifyTerminal = makeCoderGateVerifier(workspace, issueText, log, task.Spec.GateProfile, gateAdvisories)
	}

	// Layer the model profile onto the resolved config (addendum + stuck-loop
	// overrides + forcing-phase read restriction). No-op when modelProfile is nil.
	applyModelProfile(&cfg, agent, modelProfile)

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
				foremanv1alpha1.FailureMaxTurnsExhausted,
				"model did not call submit_result within max_turns"), nil
		case errors.Is(loopErr, ErrAssistantNoToolCalls):
			return e.incompleteResult(start, transcriptRef, loopRes,
				foremanv1alpha1.FailureModelMisunderstood,
				"model returned text without tool_calls; loop cannot make progress"), nil
		case errors.Is(loopErr, ErrAssistantReasoningOnly):
			// A thinking model that exhausted its reasoning-only budget
			// (#650/#651) is a model behavior outcome, not an
			// infrastructure failure: record INCOMPLETE and persist the
			// transcript (the reasoning trace is the evidence an
			// operator needs) instead of bubbling an ExecutorError that
			// drops both.
			return e.incompleteResult(start, transcriptRef, loopRes,
				foremanv1alpha1.FailureModelMisunderstood,
				"model exhausted its reasoning-only budget without emitting a tool call"), nil
		case errors.Is(loopErr, context.Canceled), errors.Is(loopErr, context.DeadlineExceeded):
			return e.incompleteResult(start, transcriptRef, loopRes,
				foremanv1alpha1.FailureTimeout,
				loopErr.Error()), nil
		default:
			// Anything else is a system / transport failure: bubble up
			// as an error so the watcher records ExecutorError. The
			// watcher's execErr path tags this as InfrastructureError
			// via the FailureReason mapping in patchTerminal.
			return nil, loopErr
		}
	}

	if loopRes.Terminal == nil {
		// Defensive: loop returned nil error but no terminal. Shouldn't
		// happen given the loop's invariants; report it explicitly.
		return e.incompleteResult(start, transcriptRef, loopRes,
			foremanv1alpha1.FailureInfrastructureError,
			"loop returned nil error but no terminal result"), nil
	}

	verdict, normalizedReason := normalizeModelVerdict(loopRes.Terminal.Verdict)

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
		// For reviewer terminals, validate the structured findings
		// payload and log a one-line summary so operators can see what
		// the reviewer flagged without opening the transcript ConfigMap.
		// Malformed findings are dropped with a warning; the verdict
		// (GO=APPROVE / NO-GO=REQUEST-CHANGES) is the authoritative
		// signal regardless of findings validity (see pkg/foreman/agent/
		// reviewer/findings.go for the schema).
		if agent.Spec.Role == foremanv1alpha1.AgentRoleReviewer &&
			loopRes.Terminal != nil {
			// Ground-truth filesTouched against the actual diff before
			// findings are logged. Devstral, in particular, hallucinates
			// this field on multi-file diffs (#582) even when its tool
			// calls returned correct data; the server-side rewrite makes
			// the model's claim a debugging artifact and the diff the
			// authoritative answer.
			reconcileReviewerFilesTouched(ctx, log, workspace, loopRes.Terminal.Extra)
			// Ground-truth issueAsk against the fetch_issue tool result
			// the model already had in its context. Devstral on the same
			// post-#584 batch confabulated issueAsk on every multi-file
			// diff -- the prompt-tightening to require verbatim quoting
			// did not change the model behavior, and the confabulated
			// ask then drove a *false NO-GO* on #526. Mirror of the
			// #582/filesTouched fix: harness owns the authoritative
			// field, model's claim is archived under issueAskClaimed.
			reconcileReviewerIssueAsk(log, loopRes.Transcript, loopRes.Terminal.Extra)
			// Computable scope-overlap check (#647): when the issue names
			// concrete files and the diff touches none of them, demote a
			// GO deterministically. Runs before issueAsk enforcement so
			// the scope signal can rescue an honest paraphrase (#744).
			var scopeDriftDetected bool
			var scopeMatched []string
			if scopeDiff, scopeErr := repo.DiffNameOnly(ctx, workspace, "main"); scopeErr == nil {
				verdict = enforceReviewerScopeOverlap(log, loopRes.Terminal.Extra,
					extractFetchIssueBody(loopRes.Transcript), scopeDiff, verdict,
					task.Spec.GateProfile.Resolve().SourceExtensions)
				scopeDriftDetected, _ = loopRes.Terminal.Extra["scopeDriftDetected"].(bool)
				scopeMatched, _ = loopRes.Terminal.Extra["scopeMatched"].([]string)
			} else {
				log.Info("reviewer scope: ground-truth diff unavailable; skipping scope check",
					"err", scopeErr.Error())
			}
			// Enforce the verification result (#644): an unverifiable
			// issueAsk demotes a GO to NO-GO so it routes to escalation
			// instead of approving a branch on fabricated understanding.
			// When scope-overlap vouches for the diff, keep GO even if
			// the model paraphrased the issue ask (#744).
			verdict = enforceReviewerIssueAsk(log, loopRes.Terminal.Extra, verdict,
				scopeDriftDetected, scopeMatched)
			logReviewerFindings(log, loopRes.Terminal.Extra)
		}
		r := e.modelDecidedResult(start, transcriptRef, loopRes, verdict)
		// Attach the normalized failure reason from the model-to-CRD
		// mapping (e.g. ERROR→INCOMPLETE + ModelReportedError for #649). Only
		// set when the normalizer produced a reason AND the result does
		// not already carry one. The guard is defensive: nothing between
		// modelDecidedResult() and here sets r.FailureReason today, but
		// future reason-setting paths (e.g. additional enforcement passes)
		// should not be silently clobbered.
		if normalizedReason != "" && r.FailureReason == "" {
			r.FailureReason = normalizedReason
		}
		return r, nil
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

	// Whether the change touches an envtest-backed package, captured before
	// the commit clears the working-tree status. Used for the post-push gate.
	envtestTouched := len(changedEnvtestPackages(ctx, workspace, execCommandRunner)) > 0

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
		// The task's branch lives in the agent-owned foreman/* namespace;
		// a stale remote ref is a previous run of this same Workload, so
		// replace it (compare-and-swap via force-with-lease) instead of
		// failing every re-run with non-fast-forward (#934).
		ReplaceOnReject: strings.HasPrefix(branch, "foreman/"),
	}); err != nil {
		return e.pushFailedResult(start, transcriptRef, loopRes, branch, sha, err), nil
	}

	// Post-push envtest gate (#859): verify envtest-backed packages in a
	// clean-room Job on the now-pushed branch. A real failure downgrades the
	// GO to INCOMPLETE; a could-not-run leaves the GO standing.
	if failed, feedback := evaluatePostPushEnvtest(
		ctx, envtestTouched, e.EnvtestJobRunner,
		task.Namespace, task.Name,
		task.Spec.Payload.Repo, branch, e.GitRemoteURL,
	); failed {
		r := e.envtestGateFailedResult(start, transcriptRef, loopRes, branch, sha, feedback)
		attachGateAdvisories(r.Extra, gateAdvisories)
		return r, nil
	}

	r := e.goResult(start, transcriptRef, loopRes, branch, sha)
	attachGateAdvisories(r.Extra, gateAdvisories)
	return r, nil
}

// coderGateMaxRetries bounds the coder gate fix attempts (#749): a GO whose
// fast checks fail gets this many feedback-and-retry cycles before the loop
// downgrades it to INCOMPLETE.
const coderGateMaxRetries = 3

// makeCoderGateVerifier returns the TerminalVerifier that runs the fast
// in-workspace gate on a coder's GO terminal. Non-GO terminals pass through
// untouched. golangci-lint is bootstrapped into the workspace bin on first
// use; a bootstrap or run error is reported as could-not-verify so the
// terminal stands and the clean-room gate Job remains the authoritative
// backstop. issueText is passed to the gate's scope-overlap check (#782).
// acc accumulates non-blocking advisory findings for the reviewer; it may
// be nil (advisory collection disabled).
func makeCoderGateVerifier(
	workspace, issueText string, log logr.Logger, profile *foremanv1alpha1.GateProfile, acc *[]advisory,
) TerminalVerifier {
	return func(ctx context.Context, terminal *ToolResult, _ []oai.Message) (bool, string, error) {
		if terminal == nil {
			return true, "", nil
		}
		if v, _ := normalizeModelVerdict(terminal.Verdict); v != foremanv1alpha1.AgenticTaskVerdictGo {
			return true, "", nil
		}
		// Non-Go GateProfiles run the language-agnostic generic gate from the
		// resolved commands. The Go path below is left byte-identical: a nil,
		// empty-language, or explicit-"go" profile takes it unchanged.
		if usesGenericGate(profile) {
			pass, feedback := RunGenericGate(ctx, workspace, profile.Resolve(), execCommandRunner)
			if !pass {
				log.Info("coder gate (generic): fast checks failed; returning feedback to the loop for a fix",
					"language", string(profile.Language))
			}
			return pass, feedback, nil
		}
		lintPath := filepath.Join(workspace, "bin", "golangci-lint")
		if _, statErr := os.Stat(lintPath); statErr != nil {
			if _, err := execCommandRunner(ctx, workspace, nil, "make", "golangci-lint"); err != nil {
				log.Info("coder gate: could not bootstrap golangci-lint; terminal stands, gate Job is the backstop",
					"err", err.Error())
				return true, "", err
			}
		}
		pass, feedback, advisories := RunCoderGate(ctx, workspace, lintPath, execCommandRunner, issueText)
		if acc != nil {
			*acc = append(*acc, advisories...)
		}
		if !pass {
			log.Info("coder gate: fast checks failed; returning feedback to the loop for a fix")
		}
		return pass, feedback, nil
	}
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
		return e.failResult(start, foremanv1alpha1.FailureInfrastructureError,
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
		return e.failResult(start, foremanv1alpha1.FailureToolFailed,
			fmt.Sprintf("%s: %s", toolName, dispatchErr.Error()))
	}

	// If the tool was self-terminal (e.g. submit_result), it carried
	// its own Verdict. Otherwise fall back to a synthetic GO so the
	// task at least reaches Succeeded; the operator can inspect
	// Result.Extra.toolOutput for what happened.
	verdict := foremanv1alpha1.AgenticTaskVerdictGo
	var detNormalizedReason foremanv1alpha1.AgenticTaskFailureReason
	if result.Terminal && result.Verdict != "" {
		verdict, detNormalizedReason = normalizeModelVerdict(result.Verdict)
	}

	summary := result.Summary
	if summary == "" {
		summary = fmt.Sprintf("deterministic %s tool returned verdict=%s", toolName, verdict)
	}

	r := NewResult(e.Kind(), verdict, summary, time.Since(start))
	// v0.3 #559: surface a structured FailureReason for the gate
	// verdicts. GATE-PASS / GO leave FailureReason empty (success);
	// GATE-FAIL maps to GateFailed (diff didn't meet quality bar;
	// retry is a code-side fix, not a gate-side issue); GATE-ERROR
	// maps to GateError (gate infrastructure problem, retryable).
	switch verdict {
	case foremanv1alpha1.AgenticTaskVerdictGateFail:
		r.FailureReason = foremanv1alpha1.FailureGateFailed
	case foremanv1alpha1.AgenticTaskVerdictGateError:
		r.FailureReason = foremanv1alpha1.FailureGateError
	}
	// Apply the model-to-CRD normalization reason (#649) only when the
	// switch above did not already set a more-specific gate reason.
	if detNormalizedReason != "" && r.FailureReason == "" {
		r.FailureReason = detNormalizedReason
	}
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
//
// It delegates to the exported FirstDeterministicTool so the executor's
// real selection behavior IS that pure helper; the admission webhook's
// private copy is asserted equivalent against it in the webhook tests.
func pickDeterministicTool(tools []string) string {
	return FirstDeterministicTool(tools)
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
	// Resolve once. Resolve() is nil-safe: a nil GateProfile yields the go
	// preset (image golang:1.26, the make-target checks), so a Go task stays
	// byte-identical to before this field existed.
	resolved := task.Spec.GateProfile.Resolve()
	args := map[string]any{
		"repo":     task.Spec.Payload.Repo,
		"branch":   branch,
		"cloneURL": cloneURL,
		"issue":    task.Spec.Payload.Issue,
		"prompt":   task.Spec.Payload.Prompt,
		"taskRef":  map[string]string{"namespace": task.Namespace, "name": task.Name},
		// Bite check is on by default for the verify gate: every final
		// verification confirms the coder's new/changed tests fail against
		// pre-change production, rejecting self-confirming tests (#787/#799).
		// The gate Job skips it safely when no test or no production files
		// changed, so default-on costs nothing when there is nothing to check.
		"biteCheck": true,
		// baseBranch is the ref the bite check diffs the coder branch against
		// and reverts production to. The clone is shallow + single-branch, so
		// the bite check fetches this ref explicitly. Defaults to main, and
		// stays consistent with the base the coder branched from (#813).
		"baseBranch": baseBranchOrDefault(task.Spec.Payload.BaseBranch),
		// image is the container image the gate Job runs.
		"image": resolved.Image,
	}

	// Non-Go GateProfiles switch the verify gate off the Go path (make
	// targets + bite check) and onto the resolved commands, run in order.
	// The bite check is Go-specific and intentionally not run on the generic
	// path in this slice. A nil or "go" profile leaves args without
	// "generic"/"commands", so the Go gate is byte-identical.
	if usesGenericGate(task.Spec.GateProfile) {
		var cmds []string
		for _, c := range []string{resolved.Format, resolved.Lint, resolved.Build, resolved.Test, resolved.CodegenCheck} {
			if strings.TrimSpace(c) != "" {
				cmds = append(cmds, c)
			}
		}
		args["generic"] = true
		args["commands"] = cmds
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
//
// It delegates to the exported IsDeterministicAgent so the executor's
// real branch selection IS that pure helper; the admission webhook's
// private copy is asserted equivalent against it in the webhook tests.
func isDeterministicAgent(agent *foremanv1alpha1.Agent) bool {
	return IsDeterministicAgent(agent.Spec)
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
// InferenceService's EndpointSlice. Slices are listed by the well-known
// kubernetes.io/service-name label (== sanitizeDNSName(isvcName); dots
// become hyphens; see internal/controller/inferenceservice_controller.go)
// because the metal-agent and the EndpointSliceMirroring controller may
// each produce one.
func (e *NativeAgentLoopExecutor) rewriteHostFromEndpoints(
	ctx context.Context, namespace, isvcName, baseURL string,
) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse status.endpoint %q: %w", baseURL, err)
	}
	svcName := strings.ReplaceAll(isvcName, ".", "-")
	var slices discoveryv1.EndpointSliceList
	if err := e.Client.List(ctx, &slices,
		client.InNamespace(namespace),
		client.MatchingLabels{"kubernetes.io/service-name": svcName},
	); err != nil {
		return "", fmt.Errorf("list EndpointSlices for service %s/%s host-override resolution: %w", namespace, svcName, err)
	}
	port, err := firstReadyPort(slices)
	if err != nil {
		return "", fmt.Errorf("EndpointSlices for service %s/%s: %w", namespace, svcName, err)
	}
	u.Host = fmt.Sprintf("%s:%d", e.InferenceBaseURLHostOverride, port)
	return strings.TrimRight(u.String(), "/"), nil
}

// firstReadyPort returns the first port advertised by a slice that has at
// least one ready endpoint. metal-agent registers one address + one port per
// InferenceService today; a future multi-replica metal path would need a
// smarter selector, but for the v0.2 same-host case "the only port" is the
// right port. An endpoint with Conditions.Ready unset is treated as ready,
// matching the EndpointSlice convention.
func firstReadyPort(slices discoveryv1.EndpointSliceList) (int32, error) {
	for i := range slices.Items {
		slice := &slices.Items[i]
		hasReady := false
		for j := range slice.Endpoints {
			if cond := slice.Endpoints[j].Conditions.Ready; cond == nil || *cond {
				hasReady = true
				break
			}
		}
		if !hasReady {
			continue
		}
		for _, p := range slice.Ports {
			if p.Port != nil && *p.Port > 0 {
				return *p.Port, nil
			}
		}
	}
	// metal-agent has not registered llama-server yet, or just
	// respawned and is between unregister + register.
	return 0, errors.New("no ready endpoint with a port on the EndpointSlices")
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
// patch as phase=Succeeded; the failure shape lives in Result.Extra
// and (v0.3 #559) in the structured Result.FailureReason. Used for
// environment errors (Agent not found, clone failed, etc.) where the
// executor never reached the model. We do not return an error from
// these paths because the failure is data-shaped: the task got a
// real, structured outcome that a downstream consumer can read.
func (e *NativeAgentLoopExecutor) failResult(
	start time.Time, reason foremanv1alpha1.AgenticTaskFailureReason, message string,
) *Result {
	r := NewResult(e.Kind(), foremanv1alpha1.AgenticTaskVerdictIncomplete, message, time.Since(start))
	r.FailureReason = reason
	r.Extra = map[string]any{
		"reason":  string(reason), // mirror for back-compat; v0.3 #559
		"outcome": "EXECUTOR-PRECONDITION-FAILED",
	}
	return r
}

func (e *NativeAgentLoopExecutor) incompleteResult(
	start time.Time, tref corev1.ObjectReference, lr *LoopResult,
	reason foremanv1alpha1.AgenticTaskFailureReason, msg string,
) *Result {
	r := NewResult(e.Kind(), foremanv1alpha1.AgenticTaskVerdictIncomplete, msg, time.Since(start))
	r.FailureReason = reason
	r.Extra = map[string]any{
		"reason":        string(reason),
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

// envtestGateFailedResult downgrades a pushed GO to INCOMPLETE when the
// post-push envtest gate Job (`make test`) failed on the pushed branch
// (#859). The commit is already pushed (sha); a re-run or a human fixes
// the failing envtest packages. feedback is the Job's log tail.
func (e *NativeAgentLoopExecutor) envtestGateFailedResult(
	start time.Time, tref corev1.ObjectReference, lr *LoopResult, branch, sha, feedback string,
) *Result {
	r := NewResult(e.Kind(), foremanv1alpha1.AgenticTaskVerdictIncomplete,
		"post-push envtest gate failed", time.Since(start))
	r.Extra = map[string]any{
		"outcome":       "ENVTEST-GATE-FAILED",
		"branch":        branch,
		"commitSHA":     sha,
		"feedback":      feedback,
		"transcriptRef": objRefAsMap(tref),
		"turnCount":     lr.Turns,
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
		"commitMessage": lr.Terminal.CommitMessage,
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

// attachGateAdvisories adds the collected coder-gate advisories to a GO
// result's Extra map under "gateAdvisories" so the reviewer and audit record
// see the gate's non-blocking findings. No key is added when there are none.
func attachGateAdvisories(extra map[string]any, acc *[]advisory) {
	if acc == nil || len(*acc) == 0 {
		return
	}
	extra["gateAdvisories"] = *acc
}

// renderGateAdvisories formats a slice of coder-gate advisories as a
// reviewer-prompt section. Returns an empty string when the slice is empty
// so callers can append unconditionally without adding noise. Called from
// buildUserPrompt when rendering advisories into a reviewer task's prompt.
func renderGateAdvisories(advisories []foremanv1alpha1.GateAdvisory) string {
	if len(advisories) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Gate advisories to verify (mechanical suspicions, confirm or dismiss each):\n")
	for _, a := range advisories {
		fmt.Fprintf(&b, "- [%s] %s\n", a.Check, a.Detail)
	}
	return b.String()
}

// --- helpers --------------------------------------------------------------

// branchNameForTask picks a branch name for the task. Precedence:
//
//  1. Explicit task.Spec.Payload.Branch wins. This is the documented
//     hand-off for verify tasks (which gate a branch the upstream coder
//     task already produced), the M6 reconciler's prepopulated
//     foreman/<workload>/issue-N name, and the escape hatch for any
//     caller that wants to pin the branch name.
//  2. issue-fix with Payload.Issue > 0 plus a Workload owner-ref
//     derives foreman/<workload>/issue-N. The workload prefix is what
//     makes the branch unique across reruns on the same issue (#573).
//  3. issue-fix with Payload.Issue > 0 and no workload owner falls
//     back to foreman/issue-N. Kept for hand-applied AgenticTasks
//     that target a one-off issue with no parent Workload.
//
// baseBranchOrDefault resolves the task's base ref, defaulting to "main" when
// payload.baseBranch is unset.
func baseBranchOrDefault(baseBranch string) string {
	if b := strings.TrimSpace(baseBranch); b != "" {
		return b
	}
	return "main"
}

// upstreamURLForRepo derives the upstream project's HTTPS git URL from a
// payload.repo "owner/name" slug (the GitHub convention LLMKube uses). It
// returns "" for an empty or malformed slug so callers fall back to the cloned
// fork's HEAD (e.g. freeform tasks that carry no repo slug). The slug is
// validated against a strict "owner/name" allowlist so it cannot inject path
// traversal, spaces, or extra path segments into the derived URL.
func upstreamURLForRepo(repoSlug string) string {
	repoSlug = strings.TrimSpace(repoSlug)
	if !repoSlugPattern.MatchString(repoSlug) {
		return ""
	}
	return "https://github.com/" + repoSlug + ".git"
}

// repoSlugPattern matches a single "owner/name" GitHub slug. Each segment is
// limited to git/GitHub-safe characters and exactly one slash is allowed, so
// "..", multiple path segments, and whitespace are rejected.
var repoSlugPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// 4. Everything else falls back to foreman/<task-name>.
func branchNameForTask(task *foremanv1alpha1.AgenticTask) string {
	if task.Spec.Payload.Branch != "" {
		return task.Spec.Payload.Branch
	}
	if task.Spec.Kind == foremanv1alpha1.AgenticTaskKindIssueFix && task.Spec.Payload.Issue > 0 {
		if owner := workloadOwnerName(task); owner != "" {
			return fmt.Sprintf("%s/%s/issue-%d", repo.BranchPrefix, owner, task.Spec.Payload.Issue)
		}
		return fmt.Sprintf("%s/issue-%d", repo.BranchPrefix, task.Spec.Payload.Issue)
	}
	return fmt.Sprintf("%s/%s", repo.BranchPrefix, task.Name)
}

// workloadOwnerName returns the .metadata.name of the Workload that
// owns this AgenticTask (set by WorkloadReconciler via owner-ref), or
// "" if no Workload owner exists. Used by branchNameForTask to
// disambiguate per-rerun branches.
//
// The check is intentionally name-based, not UID-based: the reader
// is human-friendly (foreman/v03-validation-batch-rerun-5/issue-510
// is greppable; a UUID suffix is not), and the name is unique within
// a namespace which is the only scope the executor cares about.
func workloadOwnerName(task *foremanv1alpha1.AgenticTask) string {
	for _, ref := range task.OwnerReferences {
		if ref.Kind == "Workload" && ref.APIVersion == foremanv1alpha1.GroupVersion.String() {
			return ref.Name
		}
	}
	return ""
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
	case foremanv1alpha1.AgenticTaskKindReview:
		// Build a non-empty user message for reviewer tasks. The
		// reviewer.md system prompt directs the model to read the
		// repo / issue / branch from the task payload; surfacing
		// them explicitly here gives Step 1 (navigate to the branch)
		// something concrete to operate on without a kubectl roundtrip.
		// gateVerdict and coderSummary are discovered by the reviewer
		// via tools (gh CLI / git log) per the system prompt.
		//
		// The harness-side reason this case exists: stricter OAI
		// upstreams (Devstral 24B / Mistral / DeepSeek) reject
		// HTTP 400 with "All non-assistant messages must contain
		// 'content'" when the user message has an empty Content
		// field. Qwen and other llama.cpp-served models tolerate
		// it, but parity across the local reviewer fleet matters.
		// Empirical: rerun-7 review-510-1 (devstral) failed turn 1
		// with that exact 400 before this case existed.
		fmt.Fprintf(&b, "You are reviewing the branch the coder produced for issue #%d of %s.\n\n",
			p.Issue, p.Repo)
		fmt.Fprintf(&b, "- repo: %s\n", p.Repo)
		fmt.Fprintf(&b, "- issue: %d\n", p.Issue)
		fmt.Fprintf(&b, "- branch: %s\n", p.Branch)
		b.WriteString("\nFollow Step 1 of your system prompt to navigate to ")
		b.WriteString("the branch under review before forming any judgment, ")
		b.WriteString("then apply the Step 2 review checklist, then call ")
		b.WriteString("submit_result with your verdict and findings.\n")
		// Append gate advisories when the reconciler has wired them in
		// from the upstream coder task's result. The reviewer is asked to
		// confirm or dismiss each mechanical suspicion so they are not
		// silently ignored.
		if block := renderGateAdvisories(p.GateAdvisories); block != "" {
			b.WriteString("\n")
			b.WriteString(block)
		}
	default:
		// Freeform / other kinds: pass payload prompt through unchanged.
		// We guarantee a non-empty content field on the wire even when
		// p.Prompt is empty: oai.Message.MarshalJSON emits `"content":""`
		// for non-assistant roles (#556), but some upstreams still
		// reject empty strings. Cases that legitimately want an empty
		// user prompt should send a placeholder via Payload.Prompt.
		b.WriteString(p.Prompt)
	}
	return b.String()
}

// logReviewerFindings parses the reviewer's submit_result.extra
// findings payload and emits a single info log line summarizing the
// findings count by severity. Malformed findings produce warning log
// lines but do not change task state. The reviewer's verdict
// (NO-GO = REQUEST-CHANGES) remains the cascade-affecting signal;
// findings are decoration that helps operators debug.
//
// See pkg/foreman/agent/reviewer/findings.go for the schema and
// config/foreman/system-prompts/reviewer.md for the contract the
// reviewer agent is asked to honor.
// reconcileReviewerFilesTouched overwrites
// `submit_result.extra.filesTouched` with the ground-truth file list
// from `git diff --name-only main...HEAD` in the reviewer's workspace.
// Preserves the model's original claim under `filesTouchedClaimed` so
// the discrepancy stays inspectable in the AgenticTask result.
//
// Why this exists (#582): devstral on multi-file diffs reliably emits
// a confabulated filesTouched even when its earlier read_file / bash
// tool calls accessed the correct files. The "trust the model's
// terminal payload" assumption is wrong for reviewers above a small
// complexity threshold. The fix is structural: the executor knows
// what changed (the workspace has the diff), so it should be the
// authority on filesTouched, not the model.
//
// Failure modes are non-fatal: a git error or an absent main ref
// just logs a warning and leaves the model's claim untouched. The
// reviewer's verdict + findings still surface; only the filesTouched
// field is downgraded to "model-reported" instead of "ground-truth."
//
// Note about base ref selection: we use the local `main` branch.
// `repo.Clone` defaults to cloning from origin with main checked out;
// when the model runs Step 1 (`git fetch <branch> + git checkout`),
// the local main branch stays at clone-time HEAD, which is the right
// base for the three-dot diff (compare HEAD against the merge-base
// with main).
func reconcileReviewerFilesTouched(
	ctx context.Context, log logr.Logger,
	workspace string, extra map[string]any,
) {
	if extra == nil || workspace == "" {
		return
	}
	groundTruth, err := repo.DiffNameOnly(ctx, workspace, "main")
	if err != nil {
		log.Info("reviewer filesTouched: ground-truth diff failed; preserving model claim",
			"err", err.Error())
		return
	}
	prev := extra["filesTouched"]
	if !fileListsEqual(prev, groundTruth) {
		// Preserve what the model said so the confabulation case is
		// inspectable (kubectl get agentictask -o yaml). filesTouched
		// becomes the source of truth; filesTouchedClaimed is the
		// archaeology field.
		extra["filesTouchedClaimed"] = prev
		log.Info("reviewer filesTouched: overwriting model claim with diff ground truth",
			"groundTruth", groundTruth,
			"modelClaim", prev,
		)
	}
	extra["filesTouched"] = groundTruth
}

// fileListsEqual returns true when prev (which is `any` because it
// came through map[string]any from a json.Unmarshal of the model's
// submit_result.extra) names the same set of files as groundTruth.
// Used to skip the noisy "overwriting" log line when the model
// already got it right; the executor still rewrites the field so the
// stored payload is a canonical []string.
func fileListsEqual(prev any, groundTruth []string) bool {
	got, ok := prev.([]any)
	if !ok {
		if s, sok := prev.([]string); sok {
			if len(s) != len(groundTruth) {
				return false
			}
			seen := make(map[string]bool, len(s))
			for _, v := range s {
				seen[v] = true
			}
			for _, g := range groundTruth {
				if !seen[g] {
					return false
				}
			}
			return true
		}
		return false
	}
	if len(got) != len(groundTruth) {
		return false
	}
	seen := make(map[string]bool, len(got))
	for _, v := range got {
		s, ok := v.(string)
		if !ok {
			return false
		}
		seen[s] = true
	}
	for _, g := range groundTruth {
		if !seen[g] {
			return false
		}
	}
	return true
}

// reconcileReviewerIssueAsk grounds the reviewer's
// `submit_result.extra.issueAsk` field against the real issue body
// that came back from the reviewer's `fetch_issue` tool call. The
// claim has to be a literal substring of that body; if it is not,
// the harness archives the model's claim under `issueAskClaimed`
// and rewrites `issueAsk` with the first useful prose paragraph of
// the body (skipping markdown headers). Pairs with #582's filesTouched
// fix: same shape, different field.
//
// Why this is structural rather than a prompt-tightening fix: the
// post-#584 rereview showed devstral on the Mac Studio confabulating
// `issueAsk` on every multi-file diff *even though* the prompt was
// explicitly tightened to require verbatim quoting and to set
// verdict=ERROR on failure to quote. The model's claim was a confident
// hallucination ("Add a cluster-wide default LiteLLM URL ..." for
// #449, "reconcile orphaned endpoints" for #526) that then drove a
// false NO-GO on #526 because the diff did not address the
// hallucinated ask. Prompt tightening did not move the model below
// its confabulation ceiling; the harness has to own the field.
//
// Failure modes are non-fatal: a missing fetch_issue tool result,
// malformed tool content JSON, or a missing body field all log a
// warning and leave the model's claim untouched. The reviewer's
// verdict + findings still surface; only the issueAsk field is
// downgraded to "model-reported" instead of "ground-truth."
func reconcileReviewerIssueAsk(log logr.Logger, msgs []oai.Message, extra map[string]any) {
	if extra == nil {
		return
	}
	body := extractFetchIssueBody(msgs)
	if body == "" {
		log.Info("reviewer issueAsk: no fetch_issue body in transcript; preserving model claim")
		return
	}
	claim, _ := extra["issueAsk"].(string)
	claim = strings.TrimSpace(claim)
	if claim == "" {
		// Model omitted the field. Fill it from the body so downstream
		// has *some* scope anchor; mark unverified for archaeology.
		extra["issueAsk"] = firstBodyParagraph(body, 200)
		extra["issueAskVerified"] = false
		return
	}
	if strings.Contains(body, claim) {
		// Honest claim: model quoted from the body verbatim. Leave alone
		// and mark verified so downstream knows the field is trustworthy.
		extra["issueAskVerified"] = true
		return
	}
	// Confabulation: claim is not a substring of the body the model
	// itself fetched. Archive it and rewrite issueAsk with a real
	// excerpt from the body.
	extra["issueAskClaimed"] = claim
	replaced := firstBodyParagraph(body, 200)
	extra["issueAsk"] = replaced
	extra["issueAskVerified"] = false
	log.Info("reviewer issueAsk: model claim not a substring of fetch_issue body; rewriting from body",
		"modelClaim", claim,
		"rewrittenTo", replaced,
	)
}

// enforceReviewerIssueAsk converts a failed issueAsk verification from
// an observation into a routing decision (#644). reconcileReviewerIssueAsk
// records whether the model's stated understanding of the issue is a
// verbatim quote of the body it fetched; until now a `false` there was
// archaeology while the verdict stood. The 2026-06-10 Mellum2 battery
// showed why that is not enough: 5/5 runs failed verification, including
// a GO on a known scope-drift branch and a NO-GO justified by a fully
// hallucinated ask. Both confidently wrong verdicts stood.
//
// Policy:
//   - verified false + GO + scope vouches (scopeDriftDetected==false,
//     scopeMatched non-empty): keep GO. The deterministic scope-overlap
//     rail confirms the diff is in-scope even though the model paraphrased
//     the issue ask instead of quoting it verbatim (#744).
//   - verified false + GO + no scope vouch (drift detected or no refs):
//     demote to NO-GO. A reviewer that cannot prove it read the issue
//     must not approve a branch. Because the workload controller emits
//     escalation reviewers on base NO-GO, demotion routes the branch to
//     a bigger model instead of green-lighting it.
//   - verified false + any other verdict: keep the verdict but mark it,
//     so the escalation reviewer and operators know the base review's
//     reasoning is untrusted.
//   - verified absent (no fetch_issue body in the transcript, a
//     harness-side gap rather than model dishonesty): observe-only,
//     unchanged.
//
// The original verdict is archived under verdictClaimed and the
// rewritten one flagged with verdictDemoted + demotionReason, mirroring
// the issueAskClaimed convention.
func enforceReviewerIssueAsk(
	log logr.Logger,
	extra map[string]any,
	verdict foremanv1alpha1.AgenticTaskVerdict,
	scopeDriftDetected bool,
	scopeMatched []string,
) foremanv1alpha1.AgenticTaskVerdict {
	if extra == nil {
		return verdict
	}
	verified, present := extra["issueAskVerified"].(bool)
	if !present || verified {
		return verdict
	}

	extra["verdictDemoted"] = true
	extra["verdictClaimed"] = string(verdict)

	if verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		extra["demotionReason"] = "issueAsk could not be verified as a verbatim quote of the " +
			"fetched issue body; review verdict is untrusted"
		log.Info("reviewer integrity: unverified issueAsk on non-GO verdict; keeping verdict but marking untrusted",
			"verdict", verdict)
		return verdict
	}

	// Scope-overlap vouch: when the issue names concrete files and the
	// diff touches at least one of them, the deterministic scope rail
	// confirms the reviewer was in-scope even though it paraphrased the
	// issue ask. Keep GO and annotate the outcome (#744).
	scopeVouches := !scopeDriftDetected && len(scopeMatched) > 0
	if scopeVouches {
		extra["issueAskVerified"] = false
		extra["scopeVouched"] = true
		extra["demotionReason"] = "issueAsk could not be verified as a verbatim quote of the " +
			"fetched issue body; scope-overlap confirms in-scope review"
		log.Info("reviewer integrity: unverified issueAsk on GO verdict; scope-overlap vouches, keeping GO",
			"scopeMatched", scopeMatched)
		return verdict
	}

	extra["demotionReason"] = "issueAsk could not be verified as a verbatim quote of the " +
		"fetched issue body; review verdict is untrusted"
	log.Info("reviewer integrity: unverified issueAsk on GO verdict; demoting to NO-GO",
		"verdictClaimed", verdict)
	return foremanv1alpha1.AgenticTaskVerdictNoGo
}

// extractFetchIssueBody finds the most recent fetch_issue tool result
// in the transcript and pulls the "body" field out of its JSON content.
// Returns "" if no fetch_issue call landed in the transcript or if
// the result content was malformed; either case is non-fatal in the
// reconciler.
//
// Multiple fetch_issue calls are unusual but legal (the model might
// retry on transient failure); we take the last successful one.
func extractFetchIssueBody(msgs []oai.Message) string {
	// First pass: collect ids of every fetch_issue tool_call the model
	// emitted. Second pass: find their matching tool-role results.
	fetchIDs := make(map[string]bool, 2)
	for _, m := range msgs {
		if m.Role != oai.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "fetch_issue" {
				fetchIDs[tc.ID] = true
			}
		}
	}
	if len(fetchIDs) == 0 {
		return ""
	}
	var lastBody string
	for _, m := range msgs {
		if m.Role != oai.RoleTool {
			continue
		}
		if !fetchIDs[m.ToolCallID] {
			continue
		}
		var parsed struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal([]byte(m.Content), &parsed); err != nil {
			continue
		}
		if parsed.Body != "" {
			lastBody = parsed.Body
		}
	}
	return lastBody
}

// firstBodyParagraph returns up to maxChars of the first useful
// paragraph in an issue body. Skips leading markdown headers
// ("## ...", "# ...") and blank lines so the rewritten issueAsk
// points at actual prose rather than a section title.
func firstBodyParagraph(body string, maxChars int) string {
	lines := strings.Split(body, "\n")
	var paragraph strings.Builder
	for _, l := range lines {
		stripped := strings.TrimSpace(l)
		if stripped == "" {
			if paragraph.Len() > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(stripped, "#") {
			// Markdown heading: skip; the prose starts after it.
			if paragraph.Len() > 0 {
				break
			}
			continue
		}
		if paragraph.Len() > 0 {
			paragraph.WriteByte(' ')
		}
		paragraph.WriteString(stripped)
		if paragraph.Len() >= maxChars {
			break
		}
	}
	out := paragraph.String()
	if len(out) > maxChars {
		out = out[:maxChars]
	}
	return out
}

func logReviewerFindings(log logr.Logger, extra map[string]any) {
	findings, warnings := reviewer.ParseFindings(extra)
	for _, w := range warnings {
		log.Info("reviewer findings: malformed entry dropped", "warning", w)
	}
	if len(findings) == 0 {
		return
	}
	counts := reviewer.CountBySeverity(findings)
	log.Info("reviewer findings",
		"total", len(findings),
		"blocker", counts[reviewer.SeverityBlocker],
		"major", counts[reviewer.SeverityMajor],
		"minor", counts[reviewer.SeverityMinor],
		"hasBlockers", reviewer.HasBlockers(findings),
	)
}

// fetchIssueBodyIfNeeded populates task.Spec.Payload.Prompt from the
// GitHub issue body when the task is an issue-fix with an empty
// prompt. The M6 stub planner does not pull issue bodies at synthesis
// time; this lazy fetch makes a coder Agent's first turn actually
// useful instead of being told "fix #510" with no context (#571).
//
// Best-effort: no fetcher, no auth token, malformed repo, or HTTP
// failure all yield a log line and leave the payload prompt empty,
// preserving the pre-#571 behavior. The model can still grep the repo
// for clues; the goal here is to give it a much better starting
// point when GitHub is reachable.
//
// Truncation + title formatting happen inside the fetcher; the body
// we paste includes the title prefix so the model sees what it is
// being asked to do before reading the longer body.
func fetchIssueBodyIfNeeded(
	ctx context.Context,
	fetcher githubissue.Fetcher,
	task *foremanv1alpha1.AgenticTask,
	auth *repo.Auth,
	log logr.Logger,
) {
	if fetcher == nil {
		return
	}
	if task.Spec.Kind != foremanv1alpha1.AgenticTaskKindIssueFix {
		return
	}
	if task.Spec.Payload.Prompt != "" {
		return
	}
	if task.Spec.Payload.Issue <= 0 {
		return
	}
	owner, repoName, err := githubissue.ParseRepo(task.Spec.Payload.Repo)
	if err != nil {
		log.Info("issue fetch skipped: bad repo string", "repo", task.Spec.Payload.Repo, "err", err.Error())
		return
	}
	token := ""
	if auth != nil {
		token = auth.Token
	}
	iss, err := fetcher.Fetch(ctx, owner, repoName, int(task.Spec.Payload.Issue), token)
	if err != nil {
		// Distinguish the common cases so the log line is actionable.
		var herr *githubissue.HTTPError
		switch {
		case errors.As(err, &herr) && herr.IsNotFound():
			log.Info("issue fetch: not found; continuing with empty body",
				"issue", task.Spec.Payload.Issue, "repo", task.Spec.Payload.Repo)
		case errors.As(err, &herr) && herr.IsUnauthorized():
			log.Info("issue fetch: unauthorized; check GITHUB_TOKEN",
				"issue", task.Spec.Payload.Issue, "repo", task.Spec.Payload.Repo)
		default:
			log.Info("issue fetch failed; continuing with empty body",
				"err", err.Error(),
				"issue", task.Spec.Payload.Issue, "repo", task.Spec.Payload.Repo)
		}
		return
	}
	// Compose title + state + labels + body. The model needs the title
	// (often the entire ask, especially for small docs/CI issues), the
	// state (closed -> probably already fixed -> NO-GO candidate), and
	// the labels (helps with triage). Body is the longest part and
	// goes last so the structured fields stay near the top.
	var b strings.Builder
	fmt.Fprintf(&b, "# Issue #%d: %s\n\n", iss.Number, iss.Title)
	fmt.Fprintf(&b, "State: %s\n", iss.State)
	if len(iss.Labels) > 0 {
		fmt.Fprintf(&b, "Labels: %s\n", strings.Join(iss.Labels, ", "))
	}
	b.WriteString("\n")
	b.WriteString(iss.Body)
	task.Spec.Payload.Prompt = b.String()
	log.Info("issue body fetched",
		"issue", iss.Number, "state", iss.State, "bodyLen", len(iss.Body))
}

// progressConfigFromAgent maps the Agent CR's stuckLoopDetection field
// onto a ProgressConfig the loop can use. The contract:
//
//   - Nil pointer  -> DefaultProgressConfig (debut-quality defaults; #544)
//   - Non-nil with zero fields -> all-zero ProgressConfig (detector disabled)
//   - Non-nil with set fields -> per-field override
//
// The non-nil-but-zero case lets a review-only Agent CR opt out
// explicitly with `stuckLoopDetection: {}`; the nil case (the default
// shape when no key is set) gets the conservative production defaults.
func progressConfigFromAgent(agent *foremanv1alpha1.Agent) ProgressConfig {
	var cfg ProgressConfig
	if agent == nil || agent.Spec.StuckLoopDetection == nil {
		cfg = DefaultProgressConfig
	} else {
		s := agent.Spec.StuckLoopDetection
		cfg = ProgressConfig{
			RepeatedToolThreshold: int(s.RepeatedToolThreshold),
			EditFreeTurnsLimit:    int(s.EditFreeTurnsLimit),
			ContextSoftCap:        int(s.ContextSoftCap),
			ContextHardCap:        int(s.ContextHardCap),
		}
	}
	// Role-aware override: reviewers are read-only by design (tool
	// whitelist excludes write_file / str_replace; their entire job is
	// to investigate and call submit_result). The edit-free streak
	// signal in the stuck-loop detector would therefore fire on every
	// well-behaved reviewer run that takes more than EditFreeTurnsLimit
	// turns to investigate, regardless of how productive the trajectory
	// is. Disabling the signal for reviewers is the right semantics:
	// the other signals (RepeatedToolCall, ContextSoftCap, ContextHardCap)
	// still apply and still catch genuinely-stuck reviewer trajectories.
	// Empirical motivation: the rerun-7 batch (2026-05-27) had the
	// qwen reviewer correctly investigate a diff for 16 turns and get
	// force-terminated by EditFreeStreak even though it was making
	// progress; the devstral reviewer wedged on a separate OAI issue.
	if agent != nil && agent.Spec.Role == foremanv1alpha1.AgentRoleReviewer {
		cfg.EditFreeTurnsLimit = 0
	}
	return cfg
}

// workspaceOrientationBlock renders the anchor block prepended to
// every coder/reviewer Agent's user prompt. The block tells the model
// where its workspace lives and that the value is also available as
// $WORKSPACE_ROOT in any bash call.
//
// Fact-only language by design. Prohibitions ("do not cd outside the
// workspace") get ignored by a confused model and look like a
// band-aid in a debut release; the cd-guard in BashTool does the
// enforcement, this block just provides the contract. See #567.
func workspaceOrientationBlock(workspace string) string {
	if workspace == "" {
		return ""
	}
	return "## Workspace\n" +
		"Your repository is at `" + workspace + "`.\n" +
		"The same path is exported to bash calls as `$WORKSPACE_ROOT`.\n" +
		"All relative paths you pass to read_file, write_file, grep, and " +
		"str_replace resolve against this root. Bash commands start with " +
		"cwd set to this root.\n"
}

// repoMapQuery returns the text the repo-map scorer uses to rank files.
// For issue-fix tasks we concatenate the issue number + the (often
// rich) body the planner wrote into payload.Prompt; that combination
// gives the scorer both the path hints ("see tools/bash.go") that
// often appear in issue bodies and the bag-of-words signal from the
// rest of the prose. Freeform tasks pass the prompt through unchanged.
func repoMapQuery(task *foremanv1alpha1.AgenticTask) string {
	p := task.Spec.Payload
	if task.Spec.Kind == foremanv1alpha1.AgenticTaskKindIssueFix {
		var b strings.Builder
		if p.Issue > 0 {
			fmt.Fprintf(&b, "issue #%d ", p.Issue)
		}
		if p.Repo != "" {
			b.WriteString(p.Repo)
			b.WriteString(" ")
		}
		b.WriteString(p.Prompt)
		return strings.TrimSpace(b.String())
	}
	return p.Prompt
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

// normalizeModelVerdict maps the model-facing verdict vocabulary onto the
// AgenticTaskVerdict enum. The submit_result tool contract allows "ERROR"
// (model-reported inability to complete the task: a reviewer's
// could-not-review, a coder's unrecoverable-error), which the CRD
// intentionally does not store as a verdict: it becomes INCOMPLETE with
// FailureModelReportedError so callers can distinguish model-reported
// inability from harness-detected failures (issue #649; per #644).
//
// For all other verdicts the raw string is cast directly to the typed enum.
// GATE-* and INCOMPLETE also arrive via run_gate_job and loop-synthesized
// envelopes, not only submit_result. An unknown value here is a harness bug
// rather than a model quirk; the watcher backstop handles any remaining
// out-of-enum strings as a follow-up (issue #649).
func normalizeModelVerdict(raw string) (foremanv1alpha1.AgenticTaskVerdict, foremanv1alpha1.AgenticTaskFailureReason) {
	if raw == "ERROR" {
		return foremanv1alpha1.AgenticTaskVerdictIncomplete, foremanv1alpha1.FailureModelReportedError
	}
	return foremanv1alpha1.AgenticTaskVerdict(raw), ""
}

// removeAllResilient removes path like os.RemoveAll, but tolerates
// read-only files and directories left behind by prior runs (e.g.
// envtest-fetched binaries, issue #654): on a permission error it walks
// the tree restoring owner write+execute on directories and write on
// files, then retries the removal once.
//
// WalkDir visits each directory node before descending into it, so
// chmodding the dir in its own callback unblocks the subsequent descent
// into its children. This means a single walk is sufficient to repair
// arbitrarily deep read-only trees.
func removeAllResilient(path string) error {
	if err := os.RemoveAll(path); err == nil || os.IsNotExist(err) {
		return nil
	}
	// Best-effort permission repair; WalkDir visits what it can reach.
	// Because WalkDir calls the function for a directory before reading
	// its entries, chmodding the dir here allows the walk to descend and
	// process its children in the same pass.
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable entries are handled by the retry below
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o755) //nolint:gosec // intentional: restore owner rwx so RemoveAll can descend and unlink
		} else if d.Type().IsRegular() {
			_ = os.Chmod(p, 0o644) //nolint:gosec // intentional: restore owner write so RemoveAll can unlink
		}
		// Symlinks and other special entries get no chmod: os.Chmod follows
		// links, which would rewrite the permissions of a target OUTSIDE the
		// workspace (e.g. a model-created symlink to a host file). RemoveAll
		// unlinks the link itself once its parent dir is writable.
		return nil
	})
	return os.RemoveAll(path)
}
