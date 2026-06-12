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
	"time"

	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// CoderJobSubmitter is the seam between the executor (this package) and
// the coder-Job submitter (pkg/foreman/agent/tools.RunCoderJob). The
// dependency direction is the reason it lives here: the tools package
// imports the agent package (for ToolResult, RunTaskResult, the sentinel
// constants), so the agent package cannot import tools without a cycle.
// cmd/foreman-agent wires a closure over tools.RunCoderJob.Run into the
// executor's CoderJobSubmitter field.
//
// Submit renders + submits a per-task Job that runs `foreman-agent
// run-task`, polls it to completion, and returns the parsed verdict +
// branch + commit + log tail. It must never block forever: the submitter
// owns the poll timeout.
type CoderJobSubmitter interface {
	Submit(ctx context.Context, req CoderJobRequest) (CoderJobResult, error)
}

// CoderJobRequest is everything the submitter needs to render + run the
// coder Job for one task. The executor fills it from the AgenticTask +
// the Agent's ExecutionSpec.
type CoderJobRequest struct {
	// TaskName / TaskNamespace identify the AgenticTask the Job runs.
	TaskName      string
	TaskNamespace string

	// Image is the per-task container image from
	// Agent.spec.execution.image. Empty lets the submitter default it.
	Image string

	// ServiceAccountName runs the Job pod under a least-privilege SA.
	ServiceAccountName string

	// ActiveDeadlineSeconds bounds the Job wall-clock. nil lets the
	// submitter default it.
	ActiveDeadlineSeconds *int64

	// Resources overrides the Job container resource requests + limits
	// from Agent.spec.execution.resources. nil lets the submitter apply
	// its gate-matching defaults.
	Resources *corev1.ResourceRequirements
}

// CoderJobResult is the parsed outcome the submitter returns. It mirrors
// the flat fields of tools.CoderJobResult; the executor folds it into a
// *Result. Verdict is a string form of foremanv1alpha1.AgenticTaskVerdict
// (GO / NO-GO / INCOMPLETE) or the synthetic "ERROR" for a Job-level
// failure that never reached a verdict.
type CoderJobResult struct {
	Verdict       string
	Summary       string
	Branch        string
	CommitSHA     string
	CommitMessage string
	FailureReason string
	LogTail       string
	JobName       string
}

// useCoderJobPath reports whether Execute should dispatch this task to a
// coder Job instead of running the loop in-process.
//
// Both conditions are required, and together they form the recursion
// guard:
//
//  1. The Agent selects Job mode (spec.execution.mode == Job).
//  2. A CoderJobSubmitter is wired on the executor.
//
// The in-process run-task body (RunTask, the thing the Job itself runs)
// constructs its NativeAgentLoopExecutor WITHOUT a CoderJobSubmitter, so
// even though it executes the SAME Agent (still mode==Job), condition (2)
// is false and it runs the loop in-process. Only the watcher's executor
// -- the one cmd/foreman-agent wires a submitter into -- ever takes the
// Job path. That is what keeps the Job from submitting another Job.
func (e *NativeAgentLoopExecutor) useCoderJobPath(agent *foremanv1alpha1.Agent) bool {
	if e.CoderJobSubmitter == nil {
		return false
	}
	if agent.Spec.Execution == nil {
		return false
	}
	return agent.Spec.Execution.Mode == foremanv1alpha1.ExecutionModeJob
}

// executeCoderJob submits the per-task coder Job via the wired
// CoderJobSubmitter, waits for it to finish, and folds the parsed result
// into a *Result. It is the Job-mode counterpart to runLLMPath /
// executeDeterministic: no workspace prep, no clone, no loop happens in
// THIS process -- all of that runs inside the Job, which calls RunTask.
func (e *NativeAgentLoopExecutor) executeCoderJob(
	ctx context.Context,
	task *foremanv1alpha1.AgenticTask,
	agent *foremanv1alpha1.Agent,
	start time.Time,
) *Result {
	log := logf.FromContext(ctx).WithName("native-agent-loop").WithValues(
		"task", task.Name, "ns", task.Namespace, "mode", "Job",
	)

	req := CoderJobRequest{
		TaskName:      task.Name,
		TaskNamespace: task.Namespace,
	}
	if agent.Spec.Execution != nil {
		req.Image = agent.Spec.Execution.Image
		req.ServiceAccountName = agent.Spec.Execution.ServiceAccountName
		req.ActiveDeadlineSeconds = agent.Spec.Execution.ActiveDeadlineSeconds
		req.Resources = agent.Spec.Execution.Resources
	}

	cjr, err := e.CoderJobSubmitter.Submit(ctx, req)
	if err != nil {
		// A Go error from the submitter is caller-misuse (bad config),
		// not a data-shaped outcome; surface it as an infrastructure
		// failure so the watcher flags it distinctly from a model NO-GO.
		log.Error(err, "coder Job submit failed")
		return e.failResult(start, foremanv1alpha1.FailureInfrastructureError,
			"coder Job submit: "+err.Error())
	}

	return coderJobResultToResult(e.Kind(), start, cjr)
}

// coderJobResultToResult maps a CoderJobResult onto the executor's *Result
// envelope. The verdict mapping is direct (GO->GO, NO-GO->NO-GO,
// INCOMPLETE->INCOMPLETE); a Job-level ERROR becomes a NO-GO-shaped
// failure result carrying an infrastructure FailureReason and the log
// tail, so downstream retry policy treats it like any other run failure
// rather than a successful model decision.
func coderJobResultToResult(kind string, start time.Time, cjr CoderJobResult) *Result {
	switch cjr.Verdict {
	case string(foremanv1alpha1.AgenticTaskVerdictGo):
		r := NewResult(kind, foremanv1alpha1.AgenticTaskVerdictGo, cjr.Summary, time.Since(start))
		r.Extra = map[string]any{
			"outcome":       "",
			"branch":        cjr.Branch,
			"commitSHA":     cjr.CommitSHA,
			"commitMessage": cjr.CommitMessage,
			"executionMode": "Job",
			"jobName":       cjr.JobName,
			"logTail":       cjr.LogTail,
		}
		return r
	case string(foremanv1alpha1.AgenticTaskVerdictNoGo):
		r := NewResult(kind, foremanv1alpha1.AgenticTaskVerdictNoGo, cjr.Summary, time.Since(start))
		r.Extra = map[string]any{
			"outcome":        "MODEL-NO-GO",
			"intendedBranch": cjr.Branch,
			"executionMode":  "Job",
			"jobName":        cjr.JobName,
			"logTail":        cjr.LogTail,
		}
		return r
	case string(foremanv1alpha1.AgenticTaskVerdictIncomplete):
		r := NewResult(kind, foremanv1alpha1.AgenticTaskVerdictIncomplete, cjr.Summary, time.Since(start))
		// Prefer the reason the in-pod run-task already computed (e.g.
		// FailureModelReportedError when the model called submit_result with
		// verdict=ERROR). Fall back to FailureMaxTurnsExhausted only when no
		// structured reason was embedded in the FOREMAN-RESULT envelope.
		if cjr.FailureReason != "" {
			r.FailureReason = foremanv1alpha1.AgenticTaskFailureReason(cjr.FailureReason)
		} else {
			r.FailureReason = foremanv1alpha1.FailureMaxTurnsExhausted
		}
		r.Extra = map[string]any{
			"outcome":        "INCOMPLETE",
			"intendedBranch": cjr.Branch,
			"executionMode":  "Job",
			"jobName":        cjr.JobName,
			"logTail":        cjr.LogTail,
		}
		return r
	default:
		// "ERROR" or any unrecognized verdict: the Job failed before
		// reaching a model decision (image-pull, OOM, deadline, poll lag,
		// missing result line). Surface as an infrastructure failure.
		summary := cjr.Summary
		if summary == "" {
			summary = "coder Job failed before producing a verdict"
		}
		r := NewResult(kind, foremanv1alpha1.AgenticTaskVerdictNoGo, summary, time.Since(start))
		r.FailureReason = foremanv1alpha1.FailureInfrastructureError
		r.Extra = map[string]any{
			"outcome":       "JOB-ERROR",
			"executionMode": "Job",
			"jobName":       cjr.JobName,
			"reason":        cjr.FailureReason,
			"logTail":       cjr.LogTail,
		}
		return r
	}
}
