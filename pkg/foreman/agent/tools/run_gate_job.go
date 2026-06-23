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

package tools

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/template"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
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
var DefaultGateChecks = []string{
	"fmt", "vet", "lint", "test",
	"manifests", "chart-crds", "foreman-chart-crds",
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
	// disables the volume mount. Defaults to "foreman-gate-cache".
	PVCName string

	// Image is the container image the Job runs. Defaults to
	// "golang:1.26". Override for offline mirrors or pinned shas.
	Image string

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
	// check diffs the coder branch against it and reverts production to it
	// to verify new tests bite. Defaults to "main" when empty.
	BaseBranch string   `json:"baseBranch,omitempty"`
	CloneURL   string   `json:"cloneURL,omitempty"`
	Checks     []string `json:"checks,omitempty"`
	BiteCheck  bool     `json:"biteCheck,omitempty"`
	TaskRef    struct {
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
  "checks":    {"type": "array", "items": {"type": "string"},
    "description": "ordered list of make targets to run; defaults to the foreman gate suite"},
  "biteCheck": {"type": "boolean",
    "description": "when true, run the bite check after standard checks"}
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
	if len(a.Checks) == 0 {
		a.Checks = DefaultGateChecks
	}
	if a.BaseBranch == "" {
		a.BaseBranch = "main"
	}

	cfg := applyConfigDefaults(t.Cfg)

	taskName := a.TaskRef.Name
	if taskName == "" {
		taskName = "task"
	}
	jobName := cfg.NameFn(taskName)

	// Render the Job template.
	rendered, err := renderGateJob(rendererInput{
		Name:                    jobName,
		Namespace:               cfg.Namespace,
		Image:                   cfg.Image,
		Repo:                    a.Repo,
		Branch:                  a.Branch,
		BaseBranch:              a.BaseBranch,
		Checks:                  a.Checks,
		BiteCheck:               a.BiteCheck,
		PVCName:                 cfg.PVCName,
		ActiveDeadlineSeconds:   cfg.ActiveDeadlineSeconds,
		TTLSecondsAfterFinished: cfg.TTLSecondsAfterFinished,
		CPURequest:              cfg.CPURequest,
		CPULimit:                cfg.CPULimit,
		MemRequest:              cfg.MemRequest,
		MemLimit:                cfg.MemLimit,
		CloneURLBase:            cfg.CloneURLBase,
		CloneURL:                a.CloneURL,
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
	verdict, summary, pollErr := t.pollForTerminal(ctx, cfg, jobName)

	// Always try the log tail, even on poll error, so the operator
	// has *something* to look at.
	logTail := ""
	if cfg.LogTailFn != nil {
		logTail = cfg.LogTailFn(ctx, cfg.Namespace, jobName)
		if len(logTail) > MaxLogTailBytes {
			logTail = logTail[len(logTail)-MaxLogTailBytes:]
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
// PollTimeout elapses. Returns (verdict, summary, pollError string).
// pollError is the empty string on a clean Succeeded/Failed.
func (t *RunGateJobTool) pollForTerminal(
	ctx context.Context, cfg RunGateJobToolConfig, jobName string,
) (string, string, string) {
	deadline := time.Now().Add(cfg.PollTimeout)
	key := types.NamespacedName{Namespace: cfg.Namespace, Name: jobName}

	for {
		var job batchv1.Job
		if err := t.Client.Get(ctx, key, &job); err != nil {
			if apierrors.IsNotFound(err) {
				// Job vanished between Create and Get -- TTL fired or
				// someone deleted it. Treat as GATE-ERROR.
				return VerdictGateError, "Job disappeared before reaching a terminal phase",
					"job not found during poll: " + err.Error()
			}
			return VerdictGateError, "apiserver poll failed", err.Error()
		}

		switch {
		case job.Status.Succeeded >= 1:
			return VerdictGatePass, "all gate checks passed", ""
		case job.Status.Failed >= 1:
			return VerdictGateFail, "one or more gate checks failed", ""
		}

		if time.Now().After(deadline) {
			return VerdictGateError,
				fmt.Sprintf("Job did not reach a terminal phase within %s", cfg.PollTimeout),
				"poll timeout"
		}

		select {
		case <-ctx.Done():
			return VerdictGateError, "context cancelled while polling Job",
				ctx.Err().Error()
		case <-time.After(cfg.PollInterval):
		}
	}
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
	Name                    string
	Namespace               string
	Image                   string
	Repo                    string
	Branch                  string
	BaseBranch              string
	Checks                  []string
	BiteCheck               bool
	PVCName                 string
	ActiveDeadlineSeconds   int32
	TTLSecondsAfterFinished int32
	CPURequest              string
	CPULimit                string
	MemRequest              string
	MemLimit                string
	CloneURLBase            string
	CloneURL                string
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
	tmpl, err := template.New("gate-job").Parse(gateJobTemplate)
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
// RunGateJobToolConfig stays trivially constructable in tests; tests
// only override the fields they care about.
func applyConfigDefaults(c RunGateJobToolConfig) RunGateJobToolConfig {
	if c.Namespace == "" {
		c.Namespace = "foreman-system"
	}
	if c.PVCName == "" {
		c.PVCName = "foreman-gate-cache"
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
			// foreman-gate-<task>-<unix-ms>; the timestamp suffix is
			// enough collision avoidance for the 1-job-at-a-time-per-
			// foreman-agent invariant. Trim to k8s name limits.
			name := fmt.Sprintf("foreman-gate-%s-%d", sanitizeName(taskName), time.Now().UnixMilli())
			if len(name) > 63 {
				name = name[:63]
			}
			return name
		}
	}
	return c
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
