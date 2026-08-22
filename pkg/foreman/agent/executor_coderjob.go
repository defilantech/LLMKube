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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	// OwnerReference is the AgenticTask the Job belongs to. The submitter
	// stamps it onto the Job's ownerReferences so Kubernetes garbage
	// collection reclaims the Job (and its pods) when the AgenticTask is
	// deleted, and so the re-dispatch path can identify + reap the
	// previous Job for the same task.
	OwnerReference *metav1.OwnerReference

	// OnJobCreated, when non-nil, is invoked once the submitter has
	// successfully created the Job (before it blocks polling for terminal
	// status). The executor uses it to stamp status.jobName on the
	// AgenticTask while the task is RUNNING, so an operator or a downstream
	// reaper can tell which Job belongs to a live task (#1535). It receives
	// the created Job's name. A nil value (tests, the in-pod run-task path)
	// simply skips the status write.
	OnJobCreated func(ctx context.Context, jobName string)

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
	// Namespace is the namespace the Job was submitted to.
	Namespace string
	// ResultExtra is the in-pod executor Result's full Extra map (already
	// outcome-promoted by the native executor); see the field of the same
	// name on tools.CoderJobResult (#1077).
	ResultExtra map[string]any
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

// SupervisesExternally implements SupervisingExecutor: a Job-mode task's
// loop, workspace and toolchain live in the coder Job's pod, so Execute here
// only submits, polls Job.Status and tails logs. It resolves the Agent and
// reuses useCoderJobPath -- the same predicate Execute dispatches on -- so
// the watcher's slot accounting cannot drift from the path actually taken.
// A missing agentRef or a failed lookup answers false, the conservative
// direction: the task keeps the in-process slot it would have held before,
// and Execute fails it on the same error a moment later.
func (e *NativeAgentLoopExecutor) SupervisesExternally(
	ctx context.Context,
	task *foremanv1alpha1.AgenticTask,
) bool {
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name == "" {
		return false
	}
	var agent foremanv1alpha1.Agent
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Spec.AgentRef.Name}
	if err := e.Client.Get(ctx, key, &agent); err != nil {
		return false
	}
	return e.useCoderJobPath(&agent)
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
		// Controller=true ties the Job to the AgenticTask so Kubernetes GC
		// cascades the Job (and its pods) when the task is deleted (#1535).
		//
		// BlockOwnerDeletion is deliberately NOT set: it would require the
		// agent to hold `update` on the AgenticTask's `finalizers`
		// subresource, which the agent Role does not grant (only the operator
		// does). The codebase already settles this in the other direction in
		// internal/controller/ownerref.go, which clears BlockOwnerDeletion for
		// the same reason: cascading GC still reclaims the child when the
		// owner is deleted, and the "block" semantics only matter for
		// finalizer-based cleanup workflows LLMKube does not use. Omitting it
		// keeps the create working under the agent's current RBAC.
		OwnerReference: &metav1.OwnerReference{
			APIVersion: foremanv1alpha1.GroupVersion.String(),
			Kind:       "AgenticTask",
			Name:       task.Name,
			UID:        task.UID,
			Controller: boolPtr(true),
		},
		// Submit blocks until the Job reaches a terminal phase, so the
		// executor cannot learn the Job name from the return value while the
		// task is still RUNNING. The submitter fires this callback the moment
		// the Job exists, letting us stamp status.jobName for the running
		// window (#1535). Best-effort: a patch failure is logged, not
		// fatal. The terminal patch re-lifts the name, so a missed write here
		// still lands on completion.
		OnJobCreated: func(ctx context.Context, jobName string) {
			e.stampJobName(ctx, task, jobName)
		},
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
// jobExtra seeds a Result Extra map from the in-pod envelope's Extra when
// the Job carried one, so fields the controller and rollups read (the
// promoted top-level outcome, resolvedBy, unverified, modelExtra,
// transcriptRef, turnCount) survive the Job hop instead of being
// re-synthesized flat (#1077). Job-supervisor fields are stamped on top and
// win over any same-named envelope key.
func jobExtra(cjr CoderJobResult, supervisor map[string]any) map[string]any {
	extra := make(map[string]any, len(cjr.ResultExtra)+len(supervisor))
	for k, v := range cjr.ResultExtra {
		extra[k] = v
	}
	for k, v := range supervisor {
		extra[k] = v
	}
	return extra
}

// stampJobName writes status.jobName on the AgenticTask the moment its coder
// Job is created, so the field is populated for the RUNNING window, not just
// at completion (#1535). It refetches the task first (the watcher's claim
// patch may have bumped the resourceVersion since Execute began) and applies
// a status merge patch that touches only jobName, so it cannot clobber
// phase/verdict/conditions. Best-effort: a refetch that finds no task (it was
// deleted) or a patch failure is logged and swallowed; the terminal patch
// re-lifts the name, so a miss here still lands on completion.
func (e *NativeAgentLoopExecutor) stampJobName(ctx context.Context, task *foremanv1alpha1.AgenticTask, jobName string) {
	if e.Client == nil || jobName == "" {
		return
	}
	log := logf.FromContext(ctx).WithName("native-agent-loop").WithValues("task", task.Name, "ns", task.Namespace)

	var fresh foremanv1alpha1.AgenticTask
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	if err := e.Client.Get(ctx, key, &fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return // task gone; nothing to stamp.
		}
		log.Error(err, "stamping status.jobName: refetch failed")
		return
	}
	if fresh.Status.JobName == jobName {
		return // already set (e.g. a duplicate callback); no-op.
	}
	patch := client.MergeFrom(fresh.DeepCopy())
	fresh.Status.JobName = jobName
	if err := e.Client.Status().Patch(ctx, &fresh, patch); err != nil {
		// Not fatal: the terminal patch re-lifts jobName from Result.Extra.
		log.Error(err, "stamping status.jobName: patch failed")
	}
}

// boolPtr returns a pointer to b, for the pointer-to-bool fields on
// metav1.OwnerReference.
func boolPtr(b bool) *bool {
	return &b
}

func coderJobResultToResult(kind string, start time.Time, cjr CoderJobResult) *Result {
	switch cjr.Verdict {
	case string(foremanv1alpha1.AgenticTaskVerdictGo):
		r := NewResult(kind, foremanv1alpha1.AgenticTaskVerdictGo, cjr.Summary, time.Since(start))
		r.Extra = jobExtra(cjr, map[string]any{
			"branch":        cjr.Branch,
			"commitSHA":     cjr.CommitSHA,
			"commitMessage": cjr.CommitMessage,
			"executionMode": "Job",
			"jobName":       cjr.JobName,
			"namespace":     cjr.Namespace,
			"logTail":       cjr.LogTail,
		})
		if _, ok := r.Extra["outcome"]; !ok {
			r.Extra["outcome"] = ""
		}
		return r
	case string(foremanv1alpha1.AgenticTaskVerdictNoGo):
		r := NewResult(kind, foremanv1alpha1.AgenticTaskVerdictNoGo, cjr.Summary, time.Since(start))
		r.Extra = jobExtra(cjr, map[string]any{
			"intendedBranch": cjr.Branch,
			"executionMode":  "Job",
			"jobName":        cjr.JobName,
			"namespace":      cjr.Namespace,
			"logTail":        cjr.LogTail,
		})
		// Preserve the envelope's promoted outcome (ALREADY-RESOLVED /
		// NEEDS-VERIFICATION / MODEL-DECIDED, plus paired resolvedBy or
		// unverified fields already in the map); only a Job with no
		// envelope outcome falls back to the legacy generic tag.
		if outcome, _ := r.Extra["outcome"].(string); outcome == "" {
			r.Extra["outcome"] = "MODEL-NO-GO"
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
			"namespace":      cjr.Namespace,
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
			"namespace":     cjr.Namespace,
			"reason":        cjr.FailureReason,
			"logTail":       cjr.LogTail,
		}
		return r
	}
}
