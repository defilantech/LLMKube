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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
)

// coderJobTemplate is the YAML template run_coder_job renders for each
// run. Lives in coder_job_template.yaml; embedded so the binary ships
// without an external configmap dependency, exactly like the gate Job.
//
//go:embed coder_job_template.yaml
var coderJobTemplate string

// Coder-Job verdict strings. Unlike the gate's GATE-* set, the coder Job
// reproduces the RunTask verdict vocabulary (GO / NO-GO / INCOMPLETE)
// plus a synthetic ERROR for run-failures the run-task body never reached
// a verdict for (image-pull, OOM, deadline, apiserver poll lag). These
// match foremanv1alpha1.AgenticTaskVerdict string values so the executor
// can pass them straight through.
const (
	coderVerdictGo         = "GO"
	coderVerdictNoGo       = "NO-GO"
	coderVerdictIncomplete = "INCOMPLETE"
	coderVerdictError      = "ERROR"
)

// RunCoderJobConfig is the static configuration the foreman-agent hands
// to the submitter. Per-run values (task name/namespace) come through
// RunCoderJobArgs. Defaults mirror the gate Job where they overlap
// (resources, TTL, poll cadence) and the Agent's ExecutionSpec where the
// installer overrides them (image, SA, deadline, resources).
type RunCoderJobConfig struct {
	// Namespace is where the coder Job is submitted. Defaults to
	// "foreman-system".
	Namespace string

	// Image is the per-task container image (foreman-agent binary plus
	// the project toolchain). Comes from Agent.spec.execution.image.
	// Defaults to "ghcr.io/defilantech/foreman-agent:latest" so the
	// zero-value config is constructable in tests.
	Image string

	// ServiceAccountName runs the Job pod under a least-privilege SA.
	// Empty omits the field (pod runs under the namespace default SA).
	ServiceAccountName string

	// ActiveDeadlineSeconds bounds wall-clock per run. Default 3600
	// (60 min) matches ExecutionSpec's CRD default; coder runs are
	// longer than the gate's 30 min.
	ActiveDeadlineSeconds int64

	// TTLSecondsAfterFinished bounds how long the Job + its Pod linger
	// after completion for log retrieval. Default 86400 (24 h), same as
	// the gate.
	TTLSecondsAfterFinished int32

	// Resource sizing. Defaults match the gate template (2/4 CPU,
	// 4Gi/8Gi memory).
	CPURequest string
	CPULimit   string
	MemRequest string
	MemLimit   string

	// GitCredentialsSecret is the Secret name holding the GitHub token for
	// the clone + push. The template projects it as the GITHUB_TOKEN env
	// var (read by the run-task body via repo.TokenFromEnvOrFile) and also
	// mounts it at /secrets/git. Defaults to "foreman-git-credentials".
	GitCredentialsSecret string

	// GitCredentialsSecretKey is the key within GitCredentialsSecret that
	// holds the token. Defaults to "token".
	GitCredentialsSecretKey string

	// ModelAuthSecret, when non-empty, mounts a Secret at /secrets/model
	// for remote model endpoint auth. Empty omits the mount entirely.
	// NOTE: the in-cluster InferenceService is unauthenticated today, so
	// nothing reads this yet; external / cloud-proxy model auth wiring is
	// a follow-up.
	ModelAuthSecret string

	// GitRemoteURL is the git URL the in-pod run-task clones from and
	// pushes the result branch to. Passed through to the run-task
	// container as --git-remote-url. Empty omits the flag entirely: a
	// deterministic gate-only install has no remote, and coder tasks that
	// need it then fail cleanly with GitRemoteNotConfigured rather than
	// the Job receiving a bare --git-remote-url= (an empty URL). Mirrors
	// the watcher's --git-remote-url.
	GitRemoteURL string

	// CommitAuthorName is the git author + committer name run-task stamps
	// onto the produced branch. Passed through as --commit-author-name;
	// omitted when empty so run-task falls back to its own default.
	CommitAuthorName string

	// CommitAuthorEmail is the git author + committer email run-task
	// stamps onto the produced branch. Passed through as
	// --commit-author-email; omitted when empty (coder tasks that commit
	// then fail cleanly). Mirrors the watcher's --commit-author-email.
	CommitAuthorEmail string

	// Resources, when non-nil, overrides the coder container's resource
	// requests + limits (from Agent.spec.execution.resources). Any field
	// it leaves unset falls back to the gate-matching string defaults
	// (CPURequest/CPULimit/MemRequest/MemLimit).
	Resources *corev1.ResourceRequirements

	// PollInterval is how often Run polls Job.Status while waiting for a
	// terminal phase. Default 5s; tests inject milliseconds.
	PollInterval time.Duration

	// PollTimeout caps Run's wall-clock wait for a terminal Job.Status.
	// Defaults to twice ActiveDeadlineSeconds so the Job's own deadline
	// always fires first; hitting this surfaces as an ERROR verdict
	// (apiserver lag, not a model NO-GO).
	PollTimeout time.Duration

	// LogTailFn fetches the last MaxLogTailBytes of the pod log. Same
	// seam as the gate's: the controller-runtime fake client does not
	// support pod-log subresource reads, so production wires a real
	// kubernetes.Interface here, tests stub a static string. May be nil;
	// an empty LogTail then surfaces in the result.
	LogTailFn func(ctx context.Context, namespace, jobName string) string

	// NameFn lets tests pin Job names so polling can resolve them. Default
	// produces "foreman-coder-<task-name>-<unix-ms>".
	NameFn func(taskName string) string
}

// RunCoderJobArgs are the per-run inputs: which AgenticTask the Job runs.
type RunCoderJobArgs struct {
	// TaskName is the AgenticTask name passed to run-task --task.
	TaskName string

	// TaskNamespace is the AgenticTask namespace passed to
	// run-task --namespace. Defaults to "default" when empty.
	TaskNamespace string
}

// CoderJobResult is the parsed outcome of a coder Job run. It maps the
// run-task body's RunTaskResult (read from the pod log tail) plus the
// Job-level outcome onto a verdict the executor can fold into a *Result.
type CoderJobResult struct {
	// Verdict is GO / NO-GO / INCOMPLETE / ERROR.
	Verdict string

	// Summary is the one-line "what happened".
	Summary string

	// Branch is the branch the run targeted (pushed on GO).
	Branch string

	// CommitSHA is the head commit pushed on a GO verdict.
	CommitSHA string

	// CommitMessage is the model's commit message on a GO verdict.
	CommitMessage string

	// FailureReason is a short machine-ish reason set only on an ERROR
	// verdict (job-failed, poll-timeout, create-failed, render-failed).
	FailureReason string

	// LogTail is the captured pod log tail, for operator triage.
	LogTail string

	// JobName / Namespace identify the submitted Job.
	JobName   string
	Namespace string
}

// RunCoderJob submits a per-task coder Job (Agent.spec.execution.mode=Job),
// polls for terminal Job status, fetches the pod log tail, and parses the
// RunTask result + sentinel out of it. It is the sibling of
// RunGateJobTool: same render -> create -> poll -> log-tail -> map
// structure, but the Job runs `foreman-agent run-task` rather than the
// gate's `make <checks>`.
//
// RunCoderJob is NOT an LLM tool (it has no Schema / Execute on the
// agent.Tool interface): the executor calls Run directly when an Agent's
// ExecutionSpec selects Job mode. The seam that lets the agent package
// reach this code without an import cycle (tools imports agent, not the
// reverse) is agent.CoderJobSubmitter; cmd/foreman-agent wires a closure
// over Run into the executor.
type RunCoderJob struct {
	// Client is the controller-runtime client used to Create + Get the
	// Job. Required.
	Client client.Client

	// Cfg is the static configuration; defaults fill in via
	// applyCoderConfigDefaults at Run time.
	Cfg RunCoderJobConfig
}

// Submit implements agent.CoderJobSubmitter. It folds the per-request
// fields the executor supplies (task identity + the Agent's ExecutionSpec
// overrides for image / SA / deadline) onto a copy of the static config,
// runs the Job, and maps the parsed tools.CoderJobResult onto the
// agent.CoderJobResult the executor consumes. Per-request overrides win
// over the static Cfg defaults; an empty override field falls through to
// the configured default.
func (r *RunCoderJob) Submit(ctx context.Context, req agent.CoderJobRequest) (agent.CoderJobResult, error) {
	sub := &RunCoderJob{Client: r.Client, Cfg: r.Cfg}
	if req.Image != "" {
		sub.Cfg.Image = req.Image
	}
	if req.ServiceAccountName != "" {
		sub.Cfg.ServiceAccountName = req.ServiceAccountName
	}
	if req.ActiveDeadlineSeconds != nil {
		sub.Cfg.ActiveDeadlineSeconds = *req.ActiveDeadlineSeconds
	}
	if req.Resources != nil {
		sub.Cfg.Resources = req.Resources
	}

	res, err := sub.Run(ctx, RunCoderJobArgs{
		TaskName:      req.TaskName,
		TaskNamespace: req.TaskNamespace,
	})
	if err != nil {
		return agent.CoderJobResult{}, err
	}
	return agent.CoderJobResult{
		Verdict:       res.Verdict,
		Summary:       res.Summary,
		Branch:        res.Branch,
		CommitSHA:     res.CommitSHA,
		CommitMessage: res.CommitMessage,
		FailureReason: res.FailureReason,
		LogTail:       res.LogTail,
		JobName:       res.JobName,
	}, nil
}

// Run renders the coder Job, submits it, polls for terminal status,
// fetches the log tail, and returns the parsed CoderJobResult. It never
// returns a Go error for a data-shaped outcome (NO-GO, run-failure): those
// come back as a populated result with the appropriate verdict. A Go error
// is reserved for caller-misuse (nil Client, empty TaskName).
func (r *RunCoderJob) Run(ctx context.Context, args RunCoderJobArgs) (CoderJobResult, error) {
	if r.Client == nil {
		return CoderJobResult{}, errors.New("run_coder_job: Client is required")
	}
	if args.TaskName == "" {
		return CoderJobResult{}, errors.New("run_coder_job: task name is required")
	}
	if args.TaskNamespace == "" {
		args.TaskNamespace = "default"
	}

	cfg := applyCoderConfigDefaults(r.Cfg)
	jobName := cfg.NameFn(args.TaskName)

	rendered, err := renderCoderJob(coderRendererInput{
		Name:                    jobName,
		Namespace:               cfg.Namespace,
		Image:                   cfg.Image,
		TaskName:                args.TaskName,
		TaskNamespace:           args.TaskNamespace,
		ServiceAccountName:      cfg.ServiceAccountName,
		ActiveDeadlineSeconds:   cfg.ActiveDeadlineSeconds,
		TTLSecondsAfterFinished: cfg.TTLSecondsAfterFinished,
		CPURequest:              cfg.CPURequest,
		CPULimit:                cfg.CPULimit,
		MemRequest:              cfg.MemRequest,
		MemLimit:                cfg.MemLimit,
		GitCredentialsSecret:    cfg.GitCredentialsSecret,
		GitCredentialsSecretKey: cfg.GitCredentialsSecretKey,
		ModelAuthSecret:         cfg.ModelAuthSecret,
		GitRemoteURL:            cfg.GitRemoteURL,
		CommitAuthorName:        cfg.CommitAuthorName,
		CommitAuthorEmail:       cfg.CommitAuthorEmail,
		Resources:               cfg.Resources,
	})
	if err != nil {
		return r.errorResult(jobName, cfg.Namespace, "render: "+err.Error(), ""), nil
	}

	if err := r.Client.Create(ctx, rendered); err != nil {
		return r.errorResult(jobName, cfg.Namespace, "create job: "+err.Error(), ""), nil
	}

	jobVerdict, jobReason := r.pollForTerminal(ctx, cfg, jobName)

	logTail := ""
	if cfg.LogTailFn != nil {
		logTail = cfg.LogTailFn(ctx, cfg.Namespace, jobName)
		if len(logTail) > MaxLogTailBytes {
			logTail = logTail[len(logTail)-MaxLogTailBytes:]
		}
	}

	res := CoderJobResult{
		JobName:   jobName,
		Namespace: cfg.Namespace,
		LogTail:   logTail,
	}

	if jobVerdict == coderVerdictError {
		// The Job itself failed (image-pull / OOM / deadline / poll lag):
		// run-task never reached a verdict. Surface ERROR with the reason
		// and whatever log we managed to capture.
		res.Verdict = coderVerdictError
		res.FailureReason = jobReason
		res.Summary = "coder Job failed before producing a verdict: " + jobReason
		return res, nil
	}

	// Job completed (Succeeded). Parse the RunTask result out of the log.
	// run-task always exits 0 on a data-shaped outcome (GO/NO-GO/
	// INCOMPLETE) and exits non-zero only on a system error -- which the
	// jobVerdict==ERROR branch above already handled. So a Succeeded Job
	// carries a parseable RunTaskResult line.
	parsed := parseRunTaskLog(logTail)
	res.Verdict = parsed.Verdict
	res.Summary = parsed.Summary
	res.Branch = parsed.Branch
	res.CommitSHA = parsed.CommitSHA
	res.CommitMessage = parsed.CommitMessage
	// Lift the structured FailureReason out of the embedded Result so the
	// Job-mode supervisor (coderJobResultToResult) can preserve it rather
	// than overwriting with a generic reason. This matters for INCOMPLETE
	// verdicts carrying FailureModelReportedError from an in-pod model ERROR.
	if parsed.Result != nil && parsed.Result.FailureReason != "" {
		res.FailureReason = string(parsed.Result.FailureReason)
	}
	if res.Verdict == "" {
		// Succeeded Job but no recognizable result line: treat as a
		// run-failure rather than silently dropping it.
		res.Verdict = coderVerdictError
		res.FailureReason = "no FOREMAN-RESULT line in pod log"
		res.Summary = "coder Job completed but emitted no parseable result"
	}
	if res.Verdict == coderVerdictError && res.FailureReason == "" {
		res.FailureReason = "run-task reported ERROR"
	}
	return res, nil
}

// pollForTerminal blocks until Job.Status reports a terminal phase or
// PollTimeout elapses. Returns ("", "") on a clean Succeeded; returns
// (coderVerdictError, reason) on Failure / NotFound / timeout / context
// cancellation. A Succeeded Job is signalled by an empty verdict so the
// caller proceeds to parse the log.
func (r *RunCoderJob) pollForTerminal(
	ctx context.Context, cfg RunCoderJobConfig, jobName string,
) (string, string) {
	deadline := time.Now().Add(cfg.PollTimeout)
	key := types.NamespacedName{Namespace: cfg.Namespace, Name: jobName}

	for {
		var job batchv1.Job
		if err := r.Client.Get(ctx, key, &job); err != nil {
			if apierrors.IsNotFound(err) {
				return coderVerdictError, "job disappeared before reaching a terminal phase"
			}
			return coderVerdictError, "apiserver poll failed: " + err.Error()
		}

		switch {
		case job.Status.Succeeded >= 1:
			return "", ""
		case job.Status.Failed >= 1:
			return coderVerdictError, "Job failed (image-pull, OOM, or active-deadline exceeded)"
		}

		if time.Now().After(deadline) {
			return coderVerdictError,
				fmt.Sprintf("Job did not reach a terminal phase within %s", cfg.PollTimeout)
		}

		select {
		case <-ctx.Done():
			return coderVerdictError, "context cancelled while polling Job: " + ctx.Err().Error()
		case <-time.After(cfg.PollInterval):
		}
	}
}

// errorResult builds a CoderJobResult for a failure that occurred before
// (or instead of) a clean Job completion (render error, create error).
func (r *RunCoderJob) errorResult(jobName, namespace, reason, logTail string) CoderJobResult {
	return CoderJobResult{
		Verdict:       coderVerdictError,
		Summary:       "coder Job did not run: " + reason,
		FailureReason: reason,
		LogTail:       logTail,
		JobName:       jobName,
		Namespace:     namespace,
	}
}

// --- result parsing -------------------------------------------------------

// parseRunTaskLog scans a pod log tail for the RunTask result line
// (agent.RunTaskResultPrefix + JSON) and, failing that, for the sentinel
// tokens. The structured JSON line is authoritative; the sentinel scan is
// the fallback for a truncated log that lost the JSON line but kept the
// shorter sentinel.
func parseRunTaskLog(logTail string) agent.RunTaskResult {
	var out agent.RunTaskResult
	for _, line := range strings.Split(logTail, "\n") {
		if idx := strings.Index(line, agent.RunTaskResultPrefix); idx >= 0 {
			payload := line[idx+len(agent.RunTaskResultPrefix):]
			var rt agent.RunTaskResult
			if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &rt); err == nil {
				return rt
			}
		}
	}
	// No parseable JSON line: fall back to sentinel scan.
	switch {
	case strings.Contains(logTail, agent.RunTaskSentinelGo):
		out.Verdict = coderVerdictGo
	case strings.Contains(logTail, agent.RunTaskSentinelNoGo):
		out.Verdict = coderVerdictNoGo
	case strings.Contains(logTail, agent.RunTaskSentinelIncomplete):
		out.Verdict = coderVerdictIncomplete
	case strings.Contains(logTail, agent.RunTaskSentinelError):
		out.Verdict = coderVerdictError
	}
	return out
}

// --- rendering ------------------------------------------------------------

// coderRendererInput is the struct text/template binds against for the
// coder Job template. Kept separate from the public Config + args structs
// so the template stays stable across signature changes upstream.
type coderRendererInput struct {
	Name                    string
	Namespace               string
	Image                   string
	TaskName                string
	TaskNamespace           string
	ServiceAccountName      string
	ActiveDeadlineSeconds   int64
	TTLSecondsAfterFinished int32
	CPURequest              string
	CPULimit                string
	MemRequest              string
	MemLimit                string
	GitCredentialsSecret    string
	GitCredentialsSecretKey string
	ModelAuthSecret         string
	GitRemoteURL            string
	CommitAuthorName        string
	CommitAuthorEmail       string

	// Resources, when non-nil, replaces the container resources the
	// string fields above produce. Applied after the template unmarshals
	// so a partially-specified ResourceRequirements still keeps the
	// defaults for the fields it omits.
	Resources *corev1.ResourceRequirements
}

func renderCoderJob(in coderRendererInput) (*batchv1.Job, error) {
	if in.TaskNamespace == "" {
		in.TaskNamespace = "default"
	}
	if in.TaskName == "" {
		in.TaskName = "unknown"
	}
	if in.GitCredentialsSecretKey == "" {
		in.GitCredentialsSecretKey = "token"
	}
	tmpl, err := template.New("coder-job").Parse(coderJobTemplate)
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
	applyResourceOverrides(&job, in.Resources)
	return &job, nil
}

// applyResourceOverrides folds a per-Agent ResourceRequirements override
// onto the coder container the template produced. It merges field-by-field
// so a partially-specified override (e.g. only a CPU limit) keeps the
// gate-matching string defaults for every field it omits. A nil override
// leaves the template's defaults untouched.
func applyResourceOverrides(job *batchv1.Job, res *corev1.ResourceRequirements) {
	if res == nil {
		return
	}
	containers := job.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return
	}
	c := &containers[0]
	for name, q := range res.Requests {
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		c.Resources.Requests[name] = q
	}
	for name, q := range res.Limits {
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		c.Resources.Limits[name] = q
	}
}

// applyCoderConfigDefaults fills in every empty field with the documented
// default. Kept separate from the struct definition so the zero-value
// RunCoderJobConfig stays trivially constructable in tests.
func applyCoderConfigDefaults(c RunCoderJobConfig) RunCoderJobConfig {
	if c.Namespace == "" {
		c.Namespace = "foreman-system"
	}
	if c.Image == "" {
		c.Image = "ghcr.io/defilantech/foreman-agent:latest"
	}
	if c.ActiveDeadlineSeconds == 0 {
		c.ActiveDeadlineSeconds = 3600
	}
	if c.TTLSecondsAfterFinished == 0 {
		c.TTLSecondsAfterFinished = 86400
	}
	c.CPURequest, c.CPULimit, c.MemRequest, c.MemLimit = defaultJobResources(
		c.CPURequest, c.CPULimit, c.MemRequest, c.MemLimit)
	if c.GitCredentialsSecret == "" {
		c.GitCredentialsSecret = "foreman-git-credentials"
	}
	if c.GitCredentialsSecretKey == "" {
		c.GitCredentialsSecretKey = "token"
	}
	if c.PollInterval == 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.PollTimeout == 0 {
		c.PollTimeout = 2 * time.Duration(c.ActiveDeadlineSeconds) * time.Second
	}
	if c.NameFn == nil {
		c.NameFn = func(taskName string) string {
			name := fmt.Sprintf("foreman-coder-%s-%d", sanitizeName(taskName), time.Now().UnixMilli())
			if len(name) > 63 {
				name = name[:63]
			}
			return name
		}
	}
	return c
}
