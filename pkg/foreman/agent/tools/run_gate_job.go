/*
Copyright 2026.

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

package tools

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"

	"github.com/defilantech/llmkube/pkg/foreman/agent/codehost"
)

// gateJobTemplate is the YAML template the tool renders for each call.
// Lives in gate_job_template.yaml; embedded here so the binary ships
// without an external dependency on a configmap or downloaded asset.
//
//go:embed gate_job_template.yaml
var gateJobTemplate string

// Verdict strings the gate tool produces. They overlap with but are
// distinct from the M3 GO/NO-GO/ERROR set; the executor's deterministic
// branch passes them through unmodified into the AgenticTask verdict
// so downstream consumers can pivot specifically on a gate outcome.
const (
	VerdictGatePass  = "GATE-PASS"
	VerdictGateFail  = "GATE-FAIL"
	VerdictGateError = "GATE-ERROR"
)

// MaxLogTailBytes is the cap on the log-tail surface area. The autofix
// runbook found 32 KiB enough to capture the last failed `make test`
// invocation in nearly every real failure.
const MaxLogTailBytes = 32 * 1024

// DefaultGateChecks is the make-target list every gate run executes
// when the caller does not override it. Mirrors what the autofix gate
// pipeline ran across hundreds of coder-to-verifier runs.
//
// A GATE-PASS is read by operators, and by the verdict and escalation
// machinery, as "this branch is expected to pass CI". This list is what
// backs that reading, and it must remain a superset of the make-invoked
// subset of CI, plus the golangci configs a workflow names explicitly
// with --config=. TestDefaultGateChecksCoverCI pins exactly that against
// .github/workflows, and gateExemptCIChecks records the deliberate
// omissions from it with their reasons (#1637).
//
// The claim stops there. A CI check added as a direct `run:` step is NOT
// covered by that test: security.yml runs `govulncheck ./...` on every
// pull request and the test does not see it, and most of helm-chart.yml
// drives helm and ct the same way. Widening the gate to such a check is
// a manual decision; nothing here will prompt for it.
//
// The gate's `test` is single-pass by #1693's decided approach: CI runs
// the envtest suites a second time under a second seed (test-envtest,
// exempted in gateExemptCIChecks), while the gate pays for one ordering
// only. A GATE-PASS therefore does not claim the second-seed ordering
// was exercised.
var DefaultGateChecks = []string{
	"fmt", "vet", "lint", "lint-deadcode", "test",
	"generate", "manifests", "chart-crds", "foreman-chart-crds", "federation-chart-crds",
	"check-reviewer-prompts", "check-agents-md", "check-docs-reference", "check-go-version", "test-scripts", ChartCheck,
	MetalAgentRBACCheck, B200HarnessCheck,
}

// ChartCheck is the make target that lints and unit-tests the Helm charts.
// Named because the Job has to know whether to install helm before running
// the checks: the gate image is a plain golang image with no helm in it.
const ChartCheck = "test-chart"

// MetalAgentRBACCheck is the make target that compares the chart's metal-agent
// identity with deployment/macos/metal-agent-rbac.yaml (#2152). It renders the
// chart, so like ChartCheck it needs helm on PATH.
const MetalAgentRBACCheck = "check-metal-agent-rbac"

// B200HarnessCheck is the make target that runs the B200 validation harness
// self-test (#1376). Named because the Job has to know whether to install jq
// before running the checks: the gate image is a plain golang image, and the
// self-test normalizes benchmark JSON with jq (#1833).
const B200HarnessCheck = "test-b200-harness"

// Pinned so a helm or plugin release cannot silently change gate behavior.
// HelmUnittestVersion matches .github/workflows/helm-chart.yml so the gate
// and CI validate charts with the same tool.
const (
	HelmVersion         = "v3.13.0"
	HelmUnittestVersion = "1.1.1"
	// JQVersion is the jq release the gate installs for the B200 harness
	// self-test (#1833). The tag and the asset name both carry the version,
	// so they are composed in the template rather than pinned separately.
	JQVersion = "1.7.1"
)

// helmVersionFor returns the pinned helm version when the checks need it, and
// "" otherwise so the template skips the install entirely on Go-only runs.
func helmVersionFor(checks []string) string {
	if needsHelm(checks) {
		return HelmVersion
	}
	return ""
}

// needsHelm reports whether any requested check requires helm on PATH.
func needsHelm(checks []string) bool {
	for _, c := range checks {
		if c == ChartCheck || c == MetalAgentRBACCheck {
			return true
		}
	}
	return false
}

// jqVersionFor returns the pinned jq version when the checks need it, and ""
// otherwise so the template skips the install entirely on runs that do not
// touch the B200 harness.
func jqVersionFor(checks []string) string {
	if needsJQ(checks) {
		return JQVersion
	}
	return ""
}

// needsJQ reports whether any requested check requires jq on PATH.
func needsJQ(checks []string) bool {
	for _, c := range checks {
		if c == B200HarnessCheck {
			return true
		}
	}
	return false
}

// RunGateJobToolConfig is the static configuration the foreman-agent
// hands to the tool at construction time. Per-call args (repo, branch,
// checks) come through Execute's args JSON.
type RunGateJobToolConfig struct {
	// Namespace is where the gate Job is submitted. The ServiceAccount
	// the foreman-agent runs under must have create/get/watch/delete on
	// Jobs in this namespace. Defaults to "foreman-system" if empty.
	Namespace string

	// PVCName is the persistent volume claim mounted at /cache for
	// GOMODCACHE / GOCACHE / XDG_DATA_HOME reuse across runs. Empty
	// disables the volume mount entirely: the Job renders no cache
	// volume and no /cache volumeMount, so the gate runs cold rather
	// than mounting a claim nobody created. There is deliberately NO
	// default -- a hardcoded name is what #1538 was about.
	// The foreman-agent threads the submitting agent's per-agent claim
	// (foreman.gateCache.pvcName) here via --gate-cache-pvc so named
	// agent pools mount the PVC the chart actually creates (#1538).
	PVCName string

	// Image is the container image the Job runs. Defaults to
	// "golang:1.26". Override for offline mirrors or pinned shas.
	Image string

	// CodeHost is the provider-neutral seam (#1158). When set, the gate
	// Job's clone URL is derived from it rather than from CloneURLBase, so
	// a Forgejo or GitLab fleet clones from its own host. Nil keeps the
	// GitHub default below. This is the gate-Job half of #1298: the seam
	// was injectable everywhere except here, so a third-party CodeHost
	// still produced a github.com clone inside the Job.
	CodeHost codehost.CodeHost

	// CloneURLBase is prepended to {repo}.git when the Job clones the
	// fork. Defaults to "https://github.com". Override for GHE or
	// self-hosted mirrors.
	CloneURLBase string

	// ActiveDeadlineSeconds bounds wall-clock per run. Default 1800
	// (30 min) matches the autofix pipeline's tolerance.
	ActiveDeadlineSeconds int32

	// TTLSecondsAfterFinished bounds how long the Job + its Pod linger
	// after completion for log retrieval. Default 86400 (24 h).
	TTLSecondsAfterFinished int32

	// Resource sizing. Defaults match the autofix gate template
	// (2/4 CPU, 4Gi/8Gi memory). Tune at install time per node class.
	CPURequest string
	CPULimit   string
	MemRequest string
	MemLimit   string

	// PollInterval is how often Execute polls Job.Status while waiting
	// for a terminal phase. Default 5s in production; tests inject
	// milliseconds.
	PollInterval time.Duration

	// PollTimeout caps Execute's wall-clock wait for a terminal
	// Job.Status. Defaults to twice ActiveDeadlineSeconds so the Job's
	// own deadline always fires first; we treat hitting this as a
	// GATE-ERROR (apiserver lag, not a gate failure).
	PollTimeout time.Duration

	// LogTailFn fetches the last MaxLogTailBytes of the Pod log. The
	// controller-runtime fake client does not support pod-log
	// subresource reads, so this is its own seam: production wires a
	// real kubernetes.Interface here, tests stub a static string.
	// May be nil; an empty logTail then surfaces in Result.Extra.
	LogTailFn func(ctx context.Context, namespace, jobName string) string

	// NameFn lets tests pin Job names so polling can resolve them
	// without listing. Production wires a uuid-suffixed naming helper.
	// Default produces "foreman-gate-<task-name>-<unix-ms>".
	NameFn func(taskName string) string
}

// RunGateJobTool implements the deterministic M4 gate Agent's only
// tool. It submits a Kubernetes Job that clones a branch of the fork,
// runs `make <checks>`, and exits non-zero on any failure. The tool
// polls for terminal Job status, fetches the Pod log tail, and emits
// a Terminal=true ToolResult mapping the Job outcome onto a verdict.
type RunGateJobTool struct {
	// Client is the controller-runtime client the tool uses to Create
	// + Get + Delete the Job. Required.
	Client client.Client

	// Cfg is the static configuration. Defaults fill in via
	// applyConfigDefaults at Execute time.
	Cfg RunGateJobToolConfig
}

// runGateJobArgs is the OAI-side argument shape the tool advertises.
// `taskRef` is auto-populated by executor_native.go's
// buildDeterministicArgs and lets the tool stamp owner-ref-style
// labels on the Job for observability.
//
// `cloneURL` is optional. When non-empty, the gate Job clones it
// verbatim instead of constructing a URL from CloneURLBase + Repo. It
// exists because v0.1 coders push to a fork (the foreman-agent's
// --git-remote-url) while payload.repo names the upstream the fix is
// for; the gate must verify the branch on the fork where it actually
// lives. Empty preserves the historical CloneURLBase + Repo behavior.
type runGateJobArgs struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	// BaseBranch is the branch the coder branch was cut from. The bite
	// check diffs against and reverts production to the MERGE-BASE of HEAD
	// and this ref (fetched from upstreamURL when set, not the ref tip) to
	// verify new tests bite. Defaults to "main" when empty.
	BaseBranch string `json:"baseBranch,omitempty"`
	CloneURL   string `json:"cloneURL,omitempty"`
	// UpstreamURL is the canonical repo clone URL. When set, the bite check
	// fetches BaseBranch from it and diffs/reverts against the merge-base of
	// HEAD and that ref, so a stale fork base ref can never manufacture
	// failures (#1259). Empty falls back to fetching BaseBranch from origin
	// (the fork), the pre-#1259 behavior.
	UpstreamURL string   `json:"upstreamURL,omitempty"`
	Image       string   `json:"image,omitempty"`
	Checks      []string `json:"checks,omitempty"`
	BiteCheck   bool     `json:"biteCheck,omitempty"`
	// HunkCheck runs the per-hunk mutation coverage pass for envtest packages
	// (a per-hunk variant of the bite check: revert one production hunk at a
	// time and require some test to fail). Reported as an advisory, never a
	// block. Defaults to matching BiteCheck so it runs whenever the bite
	// check does; the gate Job skips it safely when no envtest files changed.
	HunkCheck bool `json:"hunkCheck,omitempty"`
	// Generic switches the Job from the Go path (make-target Checks plus the
	// bite check) to running Commands directly. Set for non-Go GateProfiles.
	Generic bool `json:"generic,omitempty"`
	// Commands are the resolved shell commands the generic path runs in
	// order; each is a check and a non-zero exit fails the gate. Ignored
	// unless Generic is true.
	Commands []string `json:"commands,omitempty"`
	TaskRef  struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"taskRef"`
}

// Name returns the tool name advertised to the model. The gate Agent's
// spec.tools whitelist references this exact string.
func (RunGateJobTool) Name() string { return "run_gate_job" }

// Schema returns the OAI schema advertisement. The deterministic
// executor path never actually shows this to a model, but other code
// paths (e.g. a future LLM-driven Agent that wraps the gate as one of
// many tools) would, so we still produce a faithful schema.
func (RunGateJobTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name: "run_gate_job",
		Description: "Submit a Kubernetes Job that clones the given branch of the repo and " +
			"runs the verification checks (fmt/vet/lint/test/codegen by default). Returns " +
			"verdict GATE-PASS on success, GATE-FAIL on any check failing, or GATE-ERROR on " +
			"a Job-submit or apiserver-poll error.",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "repo":      {"type": "string", "description": "owner/name slug of the repo (e.g. defilantech/LLMKube)"},
  "branch":    {"type": "string", "description": "branch on the fork to verify, e.g. foreman/issue-503"},
  "baseBranch": {"type": "string", "description": "base branch the bite check diffs against (default main)"},
  "upstreamURL": {"type": "string",
    "description": "canonical repo URL the bite check fetches baseBranch from (falls back to origin when empty)"},
  "checks":    {"type": "array", "items": {"type": "string"},
    "description": "ordered list of make targets to run; defaults to the foreman gate suite"},
  "biteCheck": {"type": "boolean",
    "description": "when true, run the bite check after standard checks"},
  "hunkCheck": {"type": "boolean",
    "description": "when true, run the per-hunk mutation coverage check for envtest packages after standard checks"}
},
"required": ["repo", "branch"]
}`),
	}
}

// Execute is the deterministic-Agent entrypoint. It renders the Job
// template, submits the Job, polls for terminal status, fetches the
// log tail, and returns Terminal=true with the mapped verdict.
func (t *RunGateJobTool) Execute(ctx context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	if t.Client == nil {
		return nil, errors.New("run_gate_job: Client is required")
	}
	var a runGateJobArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("run_gate_job: bad args: %w", err)
	}
	if a.Repo == "" {
		return nil, errors.New("run_gate_job: repo is required")
	}
	if a.Branch == "" {
		return nil, errors.New("run_gate_job: branch is required")
	}
	if a.UpstreamURL != "" && !upstreamURLSafe(a.UpstreamURL) {
		return nil, fmt.Errorf("run_gate_job: unsafe upstreamURL %q", a.UpstreamURL)
	}
	if !a.Generic && len(a.Checks) == 0 {
		a.Checks = DefaultGateChecks
	}
	if a.BaseBranch == "" {
		a.BaseBranch = "main"
	}

	cfg := applyConfigDefaults(t.Cfg)

	// Use the per-call image if provided; otherwise fall back to the
	// config default (golang:1.26).
	image := a.Image
	if image == "" {
		image = cfg.Image
	}

	taskName := a.TaskRef.Name
	if taskName == "" {
		taskName = "task"
	}
	jobName := cfg.NameFn(taskName)

	// Render the Job template.
	rendered, err := renderGateJob(rendererInput{
		Name:                    jobName,
		Namespace:               cfg.Namespace,
		Image:                   image,
		Repo:                    a.Repo,
		Branch:                  a.Branch,
		BaseBranch:              a.BaseBranch,
		Checks:                  a.Checks,
		HelmVersion:             helmVersionFor(a.Checks),
		HelmUnittestVersion:     HelmUnittestVersion,
		JQVersion:               jqVersionFor(a.Checks),
		BiteCheck:               a.BiteCheck,
		HunkCheck:               a.HunkCheck,
		Generic:                 a.Generic,
		Commands:                a.Commands,
		PVCName:                 cfg.PVCName,
		ActiveDeadlineSeconds:   cfg.ActiveDeadlineSeconds,
		TTLSecondsAfterFinished: cfg.TTLSecondsAfterFinished,
		CPURequest:              cfg.CPURequest,
		CPULimit:                cfg.CPULimit,
		MemRequest:              cfg.MemRequest,
		MemLimit:                cfg.MemLimit,
		CloneURLBase:            cfg.CloneURLBase,
		CloneURL:                resolveGateCloneURL(cfg, a.Repo, a.CloneURL),
		UpstreamURL:             a.UpstreamURL,
		TaskNamespace:           a.TaskRef.Namespace,
		TaskName:                a.TaskRef.Name,
	})
	if err != nil {
		return t.errorResult(jobName, "render: "+err.Error()), nil
	}

	// Submit. We do not own the Job (no controller-runtime owner-ref
	// from a tool-call), but the TTL + the labels keep cleanup
	// predictable.
	if err := t.Client.Create(ctx, rendered); err != nil {
		return t.errorResult(jobName, "create job: "+err.Error()), nil
	}

	// Poll. Job.Status.Succeeded == 1 means GATE-PASS; .Failed >= 1
	// means GATE-FAIL; neither set after PollTimeout means
	// GATE-ERROR (apiserver lag or stuck Job).
	verdict, summary, pollErr, deadlineHit := t.pollForTerminal(ctx, cfg, jobName)

	// Always try the log tail, even on poll error, so the operator
	// has *something* to look at.
	logTail := ""
	if cfg.LogTailFn != nil {
		logTail = cfg.LogTailFn(ctx, cfg.Namespace, jobName)
		if len(logTail) > MaxLogTailBytes {
			logTail = logTail[len(logTail)-MaxLogTailBytes:]
		}
	}

	// A DeadlineExceeded kill is a gate infrastructure problem, not a
	// failing test. Name the phase the Job was in when it died so the
	// operator can see whether it survived the standard checks and
	// stalled in a long pass (e.g. per-hunk mutation coverage) (#1748).
	if deadlineHit {
		if phase := lastGatePhase(logTail); phase != "" {
			summary += " during " + phase
		}
	}

	out := &agent.ToolResult{
		Terminal: true,
		Verdict:  verdict,
		Summary:  summary,
		Output: map[string]any{
			"jobName":   jobName,
			"namespace": cfg.Namespace,
			"branch":    a.Branch,
			"repo":      a.Repo,
		},
		Extra: map[string]any{
			"jobName":   jobName,
			"namespace": cfg.Namespace,
			"logTail":   logTail,
		},
	}
	if pollErr != "" {
		out.Extra["pollError"] = pollErr
	}
	return out, nil
}

// pollForTerminal blocks until Job.Status reports a terminal phase or
// PollTimeout elapses. Returns (verdict, summary, pollError string,
// deadlineHit bool).
// pollError is the empty string on a clean Succeeded/Failed;
// deadlineHit reports whether the terminal state was a DeadlineExceeded
// kill (so Execute can annotate the summary with the active phase).
func (t *RunGateJobTool) pollForTerminal(
	ctx context.Context, cfg RunGateJobToolConfig, jobName string,
) (string, string, string, bool) {
	deadline := time.Now().Add(cfg.PollTimeout)
	key := types.NamespacedName{Namespace: cfg.Namespace, Name: jobName}

	for {
		var job batchv1.Job
		if err := t.Client.Get(ctx, key, &job); err != nil {
			if apierrors.IsNotFound(err) {
				// Job vanished between Create and Get -- TTL fired or
				// someone deleted it. Treat as GATE-ERROR.
				return VerdictGateError, "Job disappeared before reaching a terminal phase",
					"job not found during poll: " + err.Error(), false
			}
			return VerdictGateError, "apiserver poll failed", err.Error(), false
		}

		switch {
		case job.Status.Succeeded >= 1:
			return VerdictGatePass, "all gate checks passed", "", false
		case job.Status.Failed >= 1:
			if gateJobDeadlineExceeded(&job) {
				// A DeadlineExceeded kill is a gate infrastructure
				// problem (the Job ran out of wall-clock), not a failing
				// test. It maps to GATE-ERROR so the executor retries /
				// escalates rather than marking the branch bad (#1748).
				return VerdictGateError,
					fmt.Sprintf("gate Job exceeded its %ds deadline", cfg.ActiveDeadlineSeconds), "", true
			}
			return VerdictGateFail, "one or more gate checks failed", "", false
		}

		if time.Now().After(deadline) {
			return VerdictGateError,
				fmt.Sprintf("Job did not reach a terminal phase within %s", cfg.PollTimeout),
				"poll timeout", false
		}

		select {
		case <-ctx.Done():
			return VerdictGateError, "context cancelled while polling Job",
				ctx.Err().Error(), false
		case <-time.After(cfg.PollInterval):
		}
	}
}

// gateJobDeadlineExceeded reports whether a terminated Job's failure
// condition is the fixed ActiveDeadlineSeconds kill, rather than a
// check that actually failed. The Kubernetes controller sets a
// Failed condition with Reason DeadlineExceeded when a Job outlives
// its activeDeadlineSeconds; every other failure reason means a gate
// check ran to completion and failed (#1748).
func gateJobDeadlineExceeded(job *batchv1.Job) bool {
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed &&
			cond.Status == corev1.ConditionTrue &&
			cond.Reason == batchv1.JobReasonDeadlineExceeded {
			return true
		}
	}
	return false
}

// gatePhaseHeaderPattern matches the "=== <phase> ===" banner the gate
// script prints once per phase (clone, standard checks, bite check,
// per-hunk mutation coverage). We keep the whole header text so the
// operator sees the base ref carried inside it (e.g. "per-hunk mutation
// coverage (base=abc123)").
var gatePhaseHeaderPattern = regexp.MustCompile(`(?m)^=== (.+) ===$`)

// lastGatePhase returns the LAST phase header in a log tail, or "" when
// none matches. A DeadlineExceeded kill lands mid-phase, so the last
// complete header tells the operator which pass the Job was in (#1748).
func lastGatePhase(logTail string) string {
	matches := gatePhaseHeaderPattern.FindAllStringSubmatch(logTail, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

// errorResult is the Terminal=true result returned when something
// fails *before* the gate ran (template render error, Job create
// error). The verdict is GATE-ERROR so the executor surfaces an
// honest "we never got to test this branch" outcome.
func (t *RunGateJobTool) errorResult(jobName, msg string) *agent.ToolResult {
	return &agent.ToolResult{
		Terminal: true,
		Verdict:  VerdictGateError,
		Summary:  "gate did not run: " + msg,
		Output: map[string]any{
			"jobName": jobName,
			"error":   msg,
		},
		Extra: map[string]any{
			"jobName": jobName,
			"reason":  msg,
			"logTail": "",
		},
	}
}

// --- internals ------------------------------------------------------------

// upstreamURLPattern is the allowlist for runGateJobArgs.UpstreamURL: an
// https:// or git@ prefix followed by URL-safe characters only. Anything
// else (leading `-`, exotic transports like ext::, embedded whitespace)
// is rejected before the value can reach the gate Job's git argv, defense
// in depth on top of the template's `--` end-of-options guard (#1259).
var upstreamURLPattern = regexp.MustCompile(`^(https://|git@)[A-Za-z0-9._~:/@-]+$`)

// upstreamURLSafe reports whether s is safe to render into the gate Job as
// UPSTREAM_URL. Callers gate on non-empty first; empty means "no upstream,
// use the origin fallback" and never reaches git.
func upstreamURLSafe(s string) bool {
	return !strings.HasPrefix(s, "-") && upstreamURLPattern.MatchString(s)
}

// rendererInput is the struct text/template binds against. Keeping it
// here (rather than reusing the public Config + args structs) keeps the
// template stable across signature changes upstream.
//
// CloneURL, when non-empty, replaces the default `CloneURLBase/Repo.git`
// clone target. The template renders one or the other; Repo is still
// used in the human-readable log line either way ("=== clone <repo>
// @ <branch> ===") so an operator scanning Pod logs sees what was
// being verified, regardless of where the branch physically lives.
type rendererInput struct {
	Name       string
	Namespace  string
	Image      string
	Repo       string
	Branch     string
	BaseBranch string
	Checks     []string
	// HelmVersion is non-empty only when a chart check was requested; the
	// template installs helm and the unittest plugin when it is set.
	HelmVersion         string
	HelmUnittestVersion string
	// JQVersion is non-empty only when a check that needs jq was requested;
	// the template installs a pinned jq when it is set.
	JQVersion               string
	BiteCheck               bool
	HunkCheck               bool
	Generic                 bool
	Commands                []string
	PVCName                 string
	ActiveDeadlineSeconds   int32
	TTLSecondsAfterFinished int32
	CPURequest              string
	CPULimit                string
	MemRequest              string
	MemLimit                string
	CloneURLBase            string
	CloneURL                string
	UpstreamURL             string
	TaskNamespace           string
	TaskName                string
}

func renderGateJob(in rendererInput) (*batchv1.Job, error) {
	if in.TaskNamespace == "" {
		in.TaskNamespace = "default"
	}
	if in.TaskName == "" {
		in.TaskName = "unknown"
	}
	if in.BaseBranch == "" {
		in.BaseBranch = "main"
	}
	tmpl, err := template.New("gate-job").Funcs(template.FuncMap{
		// A gate command may legitimately be multi-line shell. The template
		// interpolates it inside a YAML block scalar, so a raw newline ends
		// the block and the Job fails to unmarshal (#1293). indentShell
		// re-indents continuation lines to the block's own indentation;
		// firstLine keeps the echo label on one line.
		"indentShell": indentShell,
		"firstLine":   firstLine,
	}).Parse(gateJobTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, in); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}
	var job batchv1.Job
	if err := yaml.Unmarshal(buf.Bytes(), &job); err != nil {
		return nil, fmt.Errorf("unmarshal job: %w", err)
	}
	return &job, nil
}

// applyConfigDefaults fills in every empty field with the documented
// default. Kept separate from the struct definition so the zero-value
// resolveGateCloneURL picks the URL the gate Job clones from.
//
// Precedence: an explicit cloneURL argument wins (it is how a fork-style push
// target reaches the Job), then the injected CodeHost seam, then the template's
// CloneURLBase + repo fallback, which is GitHub.
//
// The seam returns a whole URL rather than a base because provider URL shapes
// differ: a nested Forgejo or GitLab path is not "base/owner/name.git", so
// composing one from a base would be wrong for exactly the providers this seam
// exists to support. Returning "" leaves the template on its CloneURLBase path
// so a malformed slug degrades to today's behavior rather than an empty clone.
func resolveGateCloneURL(cfg RunGateJobToolConfig, repoSlug, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if cfg.CodeHost == nil {
		return ""
	}
	return cfg.CodeHost.ResolveCloneURL(repoSlug)
}

// RunGateJobToolConfig stays trivially constructable in tests; tests
// only override the fields they care about.
func applyConfigDefaults(c RunGateJobToolConfig) RunGateJobToolConfig {
	if c.Namespace == "" {
		c.Namespace = "foreman-system"
	}
	if c.Image == "" {
		c.Image = "golang:1.26"
	}
	if c.CloneURLBase == "" {
		c.CloneURLBase = "https://github.com"
	}
	if c.ActiveDeadlineSeconds == 0 {
		c.ActiveDeadlineSeconds = 1800
	}
	if c.TTLSecondsAfterFinished == 0 {
		c.TTLSecondsAfterFinished = 86400
	}
	c.CPURequest, c.CPULimit, c.MemRequest, c.MemLimit = defaultJobResources(
		c.CPURequest, c.CPULimit, c.MemRequest, c.MemLimit)
	if c.PollInterval == 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.PollTimeout == 0 {
		c.PollTimeout = 2 * time.Duration(c.ActiveDeadlineSeconds) * time.Second
	}
	if c.NameFn == nil {
		c.NameFn = func(taskName string) string {
			return gateJobName(taskName, time.Now().UnixMilli())
		}
	}
	return c
}

// gateJobName builds the gate Job name "foreman-gate-<task>-<unix-ms>",
// trimmed to the 63-char k8s object-name limit by truncating the TASK
// portion, never the trailing <unix-ms> disambiguator. The #768 retry loop
// submits a second gate Job for the same task while the prior attempt's Job
// still exists; the old code trimmed the whole name from the right, cutting
// off the timestamp for long task names, so the retry's Create collided with
// the prior Job (AlreadyExists -> GATE-ERROR -> could-not-verify -> GO). Keeping
// the suffix guarantees each submission gets a distinct name.
func gateJobName(taskName string, unixMilli int64) string {
	const prefix = "foreman-gate-"
	suffix := fmt.Sprintf("-%d", unixMilli)
	budget := 63 - len(prefix) - len(suffix)
	if budget < 0 {
		budget = 0
	}
	task := sanitizeName(taskName)
	if len(task) > budget {
		task = task[:budget]
	}
	return prefix + task + suffix
}

// defaultJobResources fills in the gate-matching container resource
// defaults (2/4 CPU, 4Gi/8Gi memory) for any field left empty. Shared by
// the gate and coder submitters so their defaulting stays identical and
// in one place.
func defaultJobResources(cpuReq, cpuLim, memReq, memLim string) (string, string, string, string) {
	if cpuReq == "" {
		cpuReq = "2"
	}
	if cpuLim == "" {
		cpuLim = "4"
	}
	if memReq == "" {
		memReq = "4Gi"
	}
	if memLim == "" {
		memLim = "8Gi"
	}
	return cpuReq, cpuLim, memReq, memLim
}

// sanitizeName turns an arbitrary taskName into a DNS-1123-friendly
// fragment safe for use as a Job name component.
func sanitizeName(in string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(in) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "task"
	}
	return out
}

// gateBlockIndent is the indentation of the command lines inside the Job
// template's args block scalar. Continuation lines of a multi-line command
// must match it or the YAML block ends early.
const gateBlockIndent = "              "

// indentShell re-indents every line after the first to gateBlockIndent, so a
// multi-line gate command stays inside the YAML block scalar it is rendered
// into.
func indentShell(cmd string) string {
	lines := strings.Split(cmd, "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = gateBlockIndent + lines[i]
	}
	return strings.Join(lines, "\n")
}

// firstLine returns the first line of cmd, with an ellipsis when there are
// more. Used for the "=== <cmd> ===" banner, which must stay on one line.
func firstLine(cmd string) string {
	if i := strings.IndexByte(cmd, '\n'); i >= 0 {
		return strings.TrimSpace(cmd[:i]) + " ..."
	}
	return cmd
}
