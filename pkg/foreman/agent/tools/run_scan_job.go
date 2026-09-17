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

	"github.com/defilantech/llmkube/pkg/foreman/agent/codehost"
)

// scanJobTemplate is the YAML template the scan tool renders for each
// call. Lives in scan_job_template.yaml; embedded here so the binary
// ships without an external dependency on a configmap or downloaded
// asset (the gate template's idiom).
//
//go:embed scan_job_template.yaml
var scanJobTemplate string

// Verdict strings the scan tool produces. SCAN-ERROR means the scan could
// not be run to a verdict (the ScanJobRunner contract's ran=false);
// SCAN-FAIL means the scan ran and findings remain (pass=false, ran=true);
// SCAN-PASS means it ran clean. The executor maps them onto that
// (pass, ran) shape -- they must never be conflated, the same
// infra-vs-branch distinction as #1748's GATE-ERROR rule.
const (
	VerdictScanPass  = "SCAN-PASS"
	VerdictScanFail  = "SCAN-FAIL"
	VerdictScanError = "SCAN-ERROR"
)

// TrivyVersion is pinned so a trivy release cannot silently change what a
// SCAN-PASS means, the same reason the gate pins helm (#1441's rule
// applied to the scan gate). The runner fetches it as the release asset of
// this exact tag rather than piping Aqua's install.sh (trivy is not in the
// Alpine repos; a mutable-branch install script has no business running
// inside a supply-chain gate). Bump deliberately, together with
// hack/scan-images.sh if CI pins trivy too.
const TrivyVersion = "v0.74.0"

// RunScanJobTool submits a clean-room container-image scan Job: clone the
// branch, build each release target the way GoReleaser does, assemble
// each image daemonless, and run the same pinned Trivy invocation
// hack/scan-images.sh runs. It mirrors RunGateJobTool exactly in shape
// (render, submit, poll to terminal, log tail, verdict).
//
// It is deliberately NOT registered in BuildAll or
// catalog.canonicalToolNames: the scan gate is executor-dispatched (the
// cmd/foreman-agent wiring sub constructs this tool and closes over it
// into the executor's ScanJobRunner seam), never model-facing. The
// catalog drift test stays balanced because neither side gains a name.
// Schema is still advertised faithfully -- a future Agent that wants it
// as a model tool would find an accurate contract, the same stance as
// RunGateJobTool.Schema.
type RunScanJobTool struct {
	// Client is the controller-runtime client the tool uses to Create
	// + Get the Job. Required.
	Client client.Client

	// Cfg is the static configuration. Defaults fill in via
	// applyScanConfigDefaults at Execute time.
	Cfg RunScanJobToolConfig
}

// RunScanJobToolConfig is the static configuration the scan gate caller
// hands to the tool at construction time. Per-call args (repo, branch,
// images, severity) come through Execute's args JSON.
type RunScanJobToolConfig struct {
	// Namespace is where the scan Job is submitted. Defaults to
	// "foreman-system" if empty.
	Namespace string

	// PVCName is the persistent volume claim mounted at /cache for
	// GOMODCACHE / GOCACHE / build reuse across runs. Empty disables the
	// volume mount entirely (cold builds). There is deliberately NO
	// default -- #1538's lesson carried over from the gate.
	PVCName string

	// BuilderImage runs the Go builds in the initContainer. Defaults to
	// "golang:1.26". Override for offline mirrors or pinned shas.
	BuilderImage string

	// RunnerImage assembles and scans each image (daemonless buildah +
	// pinned trivy). Defaults to "alpine:3.24".
	RunnerImage string

	// CodeHost is the provider-neutral code-host seam (#1158), the scan
	// half of the gate's #1298 rule: when set, the scan Job's clone URL
	// is derived from it rather than from CloneURLBase, so a Forgejo or
	// GitLab fleet scans its own host's branch. Nil keeps the GitHub
	// default below.
	CodeHost codehost.CodeHost

	// CloneURLBase is prepended to {repo}.git when the Job clones the
	// fork. Defaults to "https://github.com".
	CloneURLBase string

	// ActiveDeadlineSeconds bounds wall-clock per run. Default 3600
	// (60 min): daemonless buildah/vfs assembles are byte-copies of the
	// base image layers, generous by design -- a scan gate killed by a
	// tight deadline is indistinguishable from a slow one.
	ActiveDeadlineSeconds int32

	// TTLSecondsAfterFinished bounds how long the Job + its Pod linger
	// after completion for log retrieval. Default 86400 (24 h).
	TTLSecondsAfterFinished int32

	// Resource sizing. Defaults match the gate template (2/4 CPU,
	// 4Gi/8Gi memory) via defaultJobResources.
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
	// SCAN-ERROR (apiserver lag, not a scan failure).
	PollTimeout time.Duration

	// LogTailFn fetches the last MaxLogTailBytes of the Pod log. The
	// controller-runtime fake client does not support pod-log subresource
	// reads, so this is its own seam (the gate tool's pattern). May be
	// nil; an empty logTail then surfaces in Result.Extra.
	LogTailFn func(ctx context.Context, namespace, jobName string) string

	// NameFn lets tests pin Job names so polling can resolve them
	// without listing. Default produces
	// "foreman-scan-<task-name>-<unix-ms>".
	NameFn func(taskName string) string
}

// runScanJobArgs is the argument shape the tool accepts. `taskRef` is
// auto-populated by the executor's deterministic-args builder and lets
// the tool stamp owner-ref-style labels on the Job for observability.
//
// `cloneURL` is optional. When non-empty, the scan Job clones it verbatim
// instead of constructing a URL from CloneURLBase + Repo: the v0.1 coders
// push to a fork while payload.repo names the upstream, and the scan must
// assemble images from the branch where it actually lives (the
// run_gate_job clone-precedence contract, verbatim).
//
// `images` selects built-in scan targets by id; nil/empty means all of
// them (the api ScanGate.Images nil-means-all rule). An unknown id is an
// argument error, not a scan verdict.
type runScanJobArgs struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	// CloneURL overrides the derived clone target when non-empty.
	CloneURL string `json:"cloneURL,omitempty"`
	// UpstreamURL is the canonical repo clone URL (#1259's provenance
	// distinction from the fork). It is rendered into the Job as
	// UPSTREAM_URL env; the scan itself builds only the clone, which is
	// the branch under test.
	UpstreamURL string `json:"upstreamURL,omitempty"`
	// Images lists built-in scan-target ids; empty means all.
	Images []string `json:"images,omitempty"`
	// Severity is the Trivy severity floor; empty means DefaultScanSeverity.
	Severity []string `json:"severity,omitempty"`
	// IgnoreUnfixed mirrors Trivy's --ignore-unfixed; nil means
	// DefaultScanIgnoreUnfixed (true).
	IgnoreUnfixed *bool `json:"ignoreUnfixed,omitempty"`
	// BuilderImage / RunnerImage override the configured images per call.
	BuilderImage string `json:"builderImage,omitempty"`
	RunnerImage  string `json:"runnerImage,omitempty"`
	TaskRef      struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"taskRef"`
}

// Name returns the tool name. The executor's ScanJobRunner wiring
// references this tool; no model-facing Agent spec.tools whitelist names
// it (see the type comment).
func (RunScanJobTool) Name() string { return "run_scan_job" }

// Schema returns the OAI schema advertisement. The scan gate is
// executor-dispatched and never actually shown to a model, but other code
// paths (e.g. a future Agent that wraps the scan as one of many tools)
// would, so we still produce a faithful schema -- the same spirit as
// RunGateJobTool.Schema.
func (RunScanJobTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name: "run_scan_job",
		Description: "Submit a Kubernetes Job that clones the given branch of the repo, " +
			"builds each release image the way GoReleaser does (linux/amd64, daemonless " +
			"buildah), and Trivy-scans each one for CRITICAL/HIGH fixable vulnerabilities " +
			"(the release-gate scan from hack/scan-images.sh reproduced in a clean room). " +
			"Returns verdict SCAN-PASS on a clean scan, SCAN-FAIL when findings remain, or " +
			"SCAN-ERROR when the scan could not be run (Job-submit or apiserver-poll error).",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "repo":    {"type": "string", "description": "owner/name slug of the repo (e.g. defilantech/LLMKube)"},
  "branch":  {"type": "string", "description": "branch on the fork to scan, e.g. foreman/issue-503"},
  "cloneURL": {"type": "string",
    "description": "when set, clone this URL verbatim instead of the configured host + repo (fork push target)"},
  "upstreamURL": {"type": "string",
    "description": "canonical repo URL, surfaced on the Job for provenance"},
  "images":  {"type": "array", "items": {"type": "string"},
    "description": "built-in scan-target ids (controller, foreman-operator, foreman-agent, router-proxy); empty means all"},
  "severity": {"type": "array", "items": {"type": "string"},
    "description": "Trivy severity floor; empty means CRITICAL,HIGH"},
  "ignoreUnfixed": {"type": "boolean",
    "description": "apply Trivy --ignore-unfixed (default true: only fixable findings block)"},
  "builderImage": {"type": "string", "description": "image running the Go builds (default golang:1.26)"},
  "runnerImage":  {"type": "string", "description": "image running buildah + trivy (default alpine:3.24)"}
},
"required": ["repo", "branch"]
}`),
	}
}

// Execute is the scan-gate entrypoint. It renders the Job template,
// submits the Job, polls for terminal status, fetches the log tail, and
// returns Terminal=true with the mapped verdict.
func (t *RunScanJobTool) Execute(ctx context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	if t.Client == nil {
		return nil, errors.New("run_scan_job: Client is required")
	}
	var a runScanJobArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("run_scan_job: bad args: %w", err)
	}
	if a.Repo == "" {
		return nil, errors.New("run_scan_job: repo is required")
	}
	if a.Branch == "" {
		return nil, errors.New("run_scan_job: branch is required")
	}
	if a.UpstreamURL != "" && !upstreamURLSafe(a.UpstreamURL) {
		return nil, fmt.Errorf("run_scan_job: unsafe upstreamURL %q", a.UpstreamURL)
	}
	// An unknown image id is a bad declaration: an argument error, never
	// a SCAN-ERROR verdict. A SCAN-ERROR reads as "scan could not run"
	// and the executor retries/escalates; a typo would retry forever.
	targets, err := resolveScanTargets(a.Images)
	if err != nil {
		return nil, fmt.Errorf("run_scan_job: %w", err)
	}
	ignoreUnfixed := DefaultScanIgnoreUnfixed
	if a.IgnoreUnfixed != nil {
		ignoreUnfixed = *a.IgnoreUnfixed
	}

	cfg := applyScanConfigDefaults(t.Cfg)

	// Per-call images override the config defaults (the gate's image
	// override contract).
	builderImage := a.BuilderImage
	if builderImage == "" {
		builderImage = cfg.BuilderImage
	}
	runnerImage := a.RunnerImage
	if runnerImage == "" {
		runnerImage = cfg.RunnerImage
	}

	taskName := a.TaskRef.Name
	if taskName == "" {
		taskName = "task"
	}
	jobName := cfg.NameFn(taskName)

	// Render the Job template.
	rendered, err := renderScanJob(scanRendererInput{
		Name:                    jobName,
		Namespace:               cfg.Namespace,
		BuilderImage:            builderImage,
		RunnerImage:             runnerImage,
		Repo:                    a.Repo,
		Branch:                  a.Branch,
		Targets:                 targets,
		Severity:                resolveScanSeverity(a.Severity),
		IgnoreUnfixed:           ignoreUnfixed,
		TrivyVersion:            TrivyVersion,
		PVCName:                 cfg.PVCName,
		ActiveDeadlineSeconds:   cfg.ActiveDeadlineSeconds,
		TTLSecondsAfterFinished: cfg.TTLSecondsAfterFinished,
		CPURequest:              cfg.CPURequest,
		CPULimit:                cfg.CPULimit,
		MemRequest:              cfg.MemRequest,
		MemLimit:                cfg.MemLimit,
		CloneURLBase:            cfg.CloneURLBase,
		CloneURL:                resolveScanCloneURL(cfg, a.Repo, a.CloneURL),
		UpstreamURL:             a.UpstreamURL,
		TaskNamespace:           a.TaskRef.Namespace,
		TaskName:                a.TaskRef.Name,
	})
	if err != nil {
		return t.errorResult(jobName, "render: "+err.Error()), nil
	}

	// Submit. We do not own the Job (no controller-runtime owner-ref from
	// a tool-call), but the TTL + the labels keep cleanup predictable.
	if err := t.Client.Create(ctx, rendered); err != nil {
		return t.errorResult(jobName, "create job: "+err.Error()), nil
	}

	// Poll. Job.Status.Succeeded == 1 means SCAN-PASS; .Failed >= 1 with
	// a DeadlineExceeded condition means SCAN-ERROR (#1748's rule: a
	// deadline kill is infrastructure, not findings); .Failed >= 1
	// otherwise means SCAN-FAIL; neither set after PollTimeout means
	// SCAN-ERROR (apiserver lag or stuck Job).
	verdict, summary, pollErr := t.pollForTerminal(ctx, cfg, jobName)

	// Always try the log tail, even on poll error: on SCAN-FAIL the
	// finding table IS the feedback surface the retry prompt gets, and on
	// any outcome the operator has *something* to look at.
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
func (t *RunScanJobTool) pollForTerminal(
	ctx context.Context, cfg RunScanJobToolConfig, jobName string,
) (string, string, string) {
	deadline := time.Now().Add(cfg.PollTimeout)
	key := types.NamespacedName{Namespace: cfg.Namespace, Name: jobName}

	for {
		var job batchv1.Job
		if err := t.Client.Get(ctx, key, &job); err != nil {
			if apierrors.IsNotFound(err) {
				// Job vanished between Create and Get -- TTL fired or
				// someone deleted it. Treat as SCAN-ERROR.
				return VerdictScanError, "Job disappeared before reaching a terminal phase",
					"job not found during poll: " + err.Error()
			}
			return VerdictScanError, "apiserver poll failed", err.Error()
		}

		switch {
		case job.Status.Succeeded >= 1:
			return VerdictScanPass, "all image scans clean", ""
		case job.Status.Failed >= 1:
			if gateJobDeadlineExceeded(&job) {
				// A DeadlineExceeded kill is a scan infrastructure
				// problem (the Job ran out of wall-clock), not a finding.
				// It maps to SCAN-ERROR so the executor reports
				// could-not-run rather than marking the branch bad
				// (#1748, reused from the gate).
				return VerdictScanError,
					fmt.Sprintf("scan Job exceeded its %ds deadline", cfg.ActiveDeadlineSeconds), ""
			}
			return VerdictScanFail, "one or more image scans reported blocking findings", ""
		}

		if time.Now().After(deadline) {
			return VerdictScanError,
				fmt.Sprintf("Job did not reach a terminal phase within %s", cfg.PollTimeout),
				"poll timeout"
		}

		select {
		case <-ctx.Done():
			return VerdictScanError, "context cancelled while polling Job", ctx.Err().Error()
		case <-time.After(cfg.PollInterval):
		}
	}
}

// errorResult is the Terminal=true result returned when something fails
// *before* the scan ran (template render error, Job create error). The
// verdict is SCAN-ERROR so the executor surfaces an honest "we never got
// to scan this branch" outcome.
func (t *RunScanJobTool) errorResult(jobName, msg string) *agent.ToolResult {
	return &agent.ToolResult{
		Terminal: true,
		Verdict:  VerdictScanError,
		Summary:  "scan did not run: " + msg,
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

// scanRendererInput is the struct text/template binds against. Keeping it
// here (rather than reusing the public Config + args structs) keeps the
// template stable across signature changes upstream (the gate's
// rendererInput pattern).
//
// CloneURL, when non-empty, replaces the default `CloneURLBase/Repo.git`
// clone target; Repo is still named in the human-readable clone log line
// either way.
type scanRendererInput struct {
	Name                    string
	Namespace               string
	BuilderImage            string
	RunnerImage             string
	Repo                    string
	Branch                  string
	Targets                 []ScanTarget
	Severity                string
	IgnoreUnfixed           bool
	TrivyVersion            string
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

func renderScanJob(in scanRendererInput) (*batchv1.Job, error) {
	if in.TaskNamespace == "" {
		in.TaskNamespace = "default"
	}
	if in.TaskName == "" {
		in.TaskName = "unknown"
	}
	tmpl, err := template.New("scan-job").Parse(scanJobTemplate)
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

// resolveScanSeverity renders the Trivy --severity value: the list
// uppercased and comma-joined into the single string the template
// interpolates. Empty falls back to DefaultScanSeverity (CI's floor), so
// the rendered default is byte-identical to hack/scan-images.sh's
// "--severity CRITICAL,HIGH" (pinned by the parity test).
func resolveScanSeverity(severity []string) string {
	if len(severity) == 0 {
		severity = DefaultScanSeverity
	}
	parts := make([]string, 0, len(severity))
	for _, s := range severity {
		parts = append(parts, strings.ToUpper(strings.TrimSpace(s)))
	}
	return strings.Join(parts, ",")
}

// resolveScanCloneURL picks the URL the scan Job clones from.
//
// Precedence: an explicit cloneURL argument wins (it is how a fork-style
// push target reaches the Job), then the injected CodeHost seam, then the
// template's CloneURLBase + repo fallback, which is GitHub. Exactly the
// gate's resolveGateCloneURL contract (#1298): the seam returns a whole
// URL rather than a base because provider URL shapes differ (a nested
// Forgejo or GitLab path is not "base/owner/name.git"). Returning ""
// leaves the template on its CloneURLBase path so a malformed slug
// degrades to the GitHub behavior rather than an empty clone.
func resolveScanCloneURL(cfg RunScanJobToolConfig, repoSlug, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if cfg.CodeHost == nil {
		return ""
	}
	return cfg.CodeHost.ResolveCloneURL(repoSlug)
}

// applyScanConfigDefaults fills in every empty field with the documented
// default. Kept separate from the struct definition so
// RunScanJobToolConfig stays trivially constructable in tests; tests only
// override the fields they care about.
func applyScanConfigDefaults(c RunScanJobToolConfig) RunScanJobToolConfig {
	if c.Namespace == "" {
		c.Namespace = "foreman-system"
	}
	if c.BuilderImage == "" {
		c.BuilderImage = "golang:1.26"
	}
	if c.RunnerImage == "" {
		c.RunnerImage = "alpine:3.24"
	}
	// PVCName is deliberately NOT defaulted (#1538's rule from the gate):
	// empty means "no cache volume", which the renderer honours.
	if c.CloneURLBase == "" {
		c.CloneURLBase = "https://github.com"
	}
	if c.ActiveDeadlineSeconds == 0 {
		c.ActiveDeadlineSeconds = 3600
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
			return scanJobName(taskName, time.Now().UnixMilli())
		}
	}
	return c
}

// scanJobName builds the scan Job name "foreman-scan-<task>-<unix-ms>",
// trimmed to the 63-char k8s object-name limit by truncating the TASK
// portion, never the trailing <unix-ms> disambiguator. The gateJobName
// lesson applies verbatim (#768): a retry loop submits a second scan Job
// for the same task while the prior attempt's Job still exists; trimming
// from the right would cut off the timestamp and collide with the prior
// Job. Keeping the suffix guarantees each submission gets a distinct name.
func scanJobName(taskName string, unixMilli int64) string {
	const prefix = "foreman-scan-"
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
