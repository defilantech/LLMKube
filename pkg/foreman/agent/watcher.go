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
	"sort"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// DefaultWatcherInterval is the poll cadence between AgenticTask list
// passes. 5s mirrors the metal-agent's InferenceService watcher; bigger
// is too slow for interactive demos, smaller produces apiserver chatter
// for no benefit at v0.1 task volumes.
const DefaultWatcherInterval = 5 * time.Second

// DefaultMaxConsecutiveWatcherFailures is the threshold past which the
// watcher returns ErrWatcherStalled. Matches the metal-agent's pattern.
const DefaultMaxConsecutiveWatcherFailures = 3

// ErrWatcherStalled is returned from Run when consecutive List() failures
// exceed the configured threshold; the supervisor (launchd / systemd /
// the harness) is expected to recycle the process to rebuild the client.
var ErrWatcherStalled = errors.New("foreman agentictask watcher stalled: consecutive list failures exceeded threshold")

// AgenticTaskWatcher is the node-side dispatch loop: it polls the
// cluster for AgenticTasks assigned to this FleetNode, claims any in
// phase=Scheduled, hands them to the configured Executor, and patches
// the final phase/verdict/result when the executor returns.
//
// v0.1 runs one task at a time per node (single Executor.Execute in
// flight, controlled by a mutex). v0.2 may introduce a worker pool.
type AgenticTaskWatcher struct {
	// Client is the Kubernetes client. Required.
	Client client.Client

	// NodeName is this host's FleetNode.metadata.name (and the value the
	// scheduler writes into AgenticTask.status.assignedNode for tasks it
	// routes here). Required.
	NodeName string

	// Namespace bounds the List() call. v0.1 watches one namespace.
	// Defaults to "default" when empty.
	Namespace string

	// Interval is the poll cadence. Zero defaults to DefaultWatcherInterval.
	Interval time.Duration

	// MaxConsecutiveFailures is the stall threshold. Zero defaults to
	// DefaultMaxConsecutiveWatcherFailures.
	MaxConsecutiveFailures int

	// Executor runs claimed tasks. Required.
	Executor Executor

	// inflightMu guards inflight. Exactly one task at a time in v0.1.
	inflightMu sync.Mutex
	inflight   *foremanv1alpha1.AgenticTask
}

// Run blocks, polling every Interval until ctx is cancelled. Returns
// ErrWatcherStalled if List() fails MaxConsecutiveFailures times in a
// row; the supervisor should recycle the process.
func (w *AgenticTaskWatcher) Run(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("agentictask-watcher").WithValues("node", w.NodeName)

	interval := w.Interval
	if interval <= 0 {
		interval = DefaultWatcherInterval
	}
	maxFails := w.MaxConsecutiveFailures
	if maxFails <= 0 {
		maxFails = DefaultMaxConsecutiveWatcherFailures
	}
	ns := w.Namespace
	if ns == "" {
		ns = "default"
	}

	log.Info("starting", "interval", interval.String(), "namespace", ns, "executor", w.Executor.Kind())

	// Recover any tasks orphaned in phase=Running by a previous agent
	// process (crash, OOM, launchctl bounce) before we start polling, so
	// the scheduler can re-dispatch them. Best-effort: a failure here is
	// logged but must not prevent the poll loop from starting.
	if err := w.recoverOrphanedTasks(ctx, ns); err != nil {
		log.Error(err, "orphaned task recovery failed; continuing to poll loop")
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutiveFails := 0
	for {
		select {
		case <-ctx.Done():
			log.Info("stopping")
			return nil
		case <-ticker.C:
			if err := w.pollOnce(ctx, ns); err != nil {
				consecutiveFails++
				log.Error(err, "poll failed", "consecutiveFails", consecutiveFails, "max", maxFails)
				if consecutiveFails >= maxFails {
					return fmt.Errorf("%w: %d consecutive failures", ErrWatcherStalled, consecutiveFails)
				}
				continue
			}
			consecutiveFails = 0
		}
	}
}

// recoverOrphanedTasks runs once at startup, before the poll loop. It
// resets AgenticTasks left in phase=Running on this node by a previous
// (now-dead) agent process. The poll loop only dispatches phase=Scheduled,
// so without this a crashed or bounced agent would orphan its in-flight
// task forever (it stays Running with a stale claimedAt and is never
// re-examined). Reset-to-Pending is the simplest correct recovery so the
// scheduler re-dispatches the work; resume-from-transcript is a future
// refinement. Fixes defilantech/LLMKube#542.
func (w *AgenticTaskWatcher) recoverOrphanedTasks(ctx context.Context, namespace string) error {
	log := logf.FromContext(ctx).WithName("agentictask-watcher").WithValues("node", w.NodeName)

	var list foremanv1alpha1.AgenticTaskList
	if err := w.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list AgenticTasks for recovery: %w", err)
	}

	recovered := 0
	for i := range list.Items {
		t := &list.Items[i]
		if t.Status.AssignedNode != w.NodeName || t.Status.Phase != foremanv1alpha1.AgenticTaskPhaseRunning {
			continue
		}
		if err := w.resetOrphanedTask(ctx, t); err != nil {
			// Best-effort: log and continue. A transient patch failure
			// should not block the poll loop from starting; the next
			// agent restart will retry.
			log.Error(err, "failed to recover orphaned task", "task", t.Name)
			continue
		}
		recovered++
		log.Info("reset orphaned task to Pending for re-dispatch", "task", t.Name)
	}
	if recovered > 0 {
		log.Info("orphaned task recovery complete", "recovered", recovered)
	}
	return nil
}

// resetOrphanedTask flips an orphaned Running task back to Pending and
// clears the dead PID's claim, so the scheduler re-dispatches it.
func (w *AgenticTaskWatcher) resetOrphanedTask(ctx context.Context, t *foremanv1alpha1.AgenticTask) error {
	patch := client.MergeFrom(t.DeepCopy())
	now := metav1.Now()
	t.Status.Phase = foremanv1alpha1.AgenticTaskPhasePending
	t.Status.AssignedNode = ""
	t.Status.ClaimedAt = nil
	t.Status.StartedAt = nil
	setCondition(&t.Status.Conditions, metav1.Condition{
		Type:               "Running",
		Status:             metav1.ConditionFalse,
		Reason:             "AgentRestartRecovery",
		Message:            fmt.Sprintf("reset to Pending: %s restarted while this task was Running", w.NodeName),
		LastTransitionTime: now,
	})
	return w.Client.Status().Patch(ctx, t, patch)
}

// kindClaimPriority orders candidate tasks depth-first by pipeline
// position: finish in-flight Workloads (review, then verify) before
// starting new coder work. Claiming in List order instead maximizes
// WIP — with a deep issue-fix backlog on a one-task-per-node agent,
// verify/review tasks starve and no Workload ever completes (#936).
func kindClaimPriority(kind foremanv1alpha1.AgenticTaskKind) int {
	switch kind {
	case foremanv1alpha1.AgenticTaskKindReview:
		return 0
	case foremanv1alpha1.AgenticTaskKindVerify:
		return 1
	case foremanv1alpha1.AgenticTaskKindIssueFix:
		return 2
	default:
		return 3
	}
}

// sortTasksDepthFirst orders claim candidates by kindClaimPriority,
// breaking ties oldest-first so no task starves within its kind.
func sortTasksDepthFirst(tasks []*foremanv1alpha1.AgenticTask) {
	sort.SliceStable(tasks, func(i, j int) bool {
		pi, pj := kindClaimPriority(tasks[i].Spec.Kind), kindClaimPriority(tasks[j].Spec.Kind)
		if pi != pj {
			return pi < pj
		}
		return tasks[i].CreationTimestamp.Before(&tasks[j].CreationTimestamp)
	})
}

// pollOnce runs a single List() pass and dispatches any task assigned to
// this node that is in phase=Scheduled, preferring downstream tasks
// (review, verify) over new issue-fix work — see sortTasksDepthFirst.
func (w *AgenticTaskWatcher) pollOnce(ctx context.Context, namespace string) error {
	// If a task is already in flight, skip until it completes; v0.1 is
	// one-task-per-node.
	w.inflightMu.Lock()
	busy := w.inflight != nil
	w.inflightMu.Unlock()
	if busy {
		return nil
	}

	var list foremanv1alpha1.AgenticTaskList
	if err := w.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list AgenticTasks: %w", err)
	}

	candidates := make([]*foremanv1alpha1.AgenticTask, 0, len(list.Items))
	for i := range list.Items {
		t := &list.Items[i]
		if t.Status.AssignedNode != w.NodeName {
			continue
		}
		if t.Status.Phase != foremanv1alpha1.AgenticTaskPhaseScheduled {
			continue
		}
		candidates = append(candidates, t)
	}
	sortTasksDepthFirst(candidates)

	for _, t := range candidates {
		if err := w.claim(ctx, t); err != nil {
			// Patch race or transient apiserver error; let the next
			// poll retry. Do not count toward the stall threshold
			// because List() itself succeeded.
			logf.FromContext(ctx).WithName("agentictask-watcher").Error(err, "claim failed", "task", t.Name)
			continue
		}
		// Took it. Launch the executor and return to the polling loop;
		// the next poll will see inflight!=nil and skip.
		w.launchExecutor(ctx, t)
		return nil
	}
	return nil
}

// claim flips Scheduled -> Running using a status merge patch. Optimistic
// concurrency: if the patch fails because someone else moved the task,
// the next poll will see the new phase and skip cleanly.
func (w *AgenticTaskWatcher) claim(ctx context.Context, t *foremanv1alpha1.AgenticTask) error {
	patch := client.MergeFrom(t.DeepCopy())
	now := metav1.Now()
	t.Status.Phase = foremanv1alpha1.AgenticTaskPhaseRunning
	t.Status.ClaimedAt = &now
	t.Status.StartedAt = &now
	setCondition(&t.Status.Conditions, metav1.Condition{
		Type:               "Running",
		Status:             metav1.ConditionTrue,
		Reason:             "Claimed",
		Message:            fmt.Sprintf("claimed by %s", w.NodeName),
		LastTransitionTime: now,
	})
	return w.Client.Status().Patch(ctx, t, patch)
}

// launchExecutor runs Execute in a goroutine, patches the terminal
// status when it returns, and clears the inflight slot.
func (w *AgenticTaskWatcher) launchExecutor(ctx context.Context, t *foremanv1alpha1.AgenticTask) {
	w.inflightMu.Lock()
	w.inflight = t
	w.inflightMu.Unlock()

	log := logf.FromContext(ctx).WithName("agentictask-watcher").WithValues("task", t.Name, "kind", t.Spec.Kind)
	log.Info("dispatching to executor")

	go func() {
		defer func() {
			w.inflightMu.Lock()
			w.inflight = nil
			w.inflightMu.Unlock()
		}()

		// Use a fresh context so the executor's lifetime is decoupled from
		// the poll tick. Cancellation still propagates from the parent.
		execCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		res, execErr := w.Executor.Execute(execCtx, t)
		if patchErr := w.patchTerminal(ctx, t, res, execErr); patchErr != nil {
			log.Error(patchErr, "patching terminal status failed")
		}
	}()
}

// patchTerminal re-fetches the task and writes its final phase, verdict,
// result envelope, and Completed condition.
func (w *AgenticTaskWatcher) patchTerminal(
	ctx context.Context,
	t *foremanv1alpha1.AgenticTask,
	res *Result,
	execErr error,
) error {
	var fresh foremanv1alpha1.AgenticTask
	key := types.NamespacedName{Namespace: t.Namespace, Name: t.Name}
	if err := w.Client.Get(ctx, key, &fresh); err != nil {
		return fmt.Errorf("refetch %s: %w", key, err)
	}

	// Ownership guard (#668): if the controller expired this claim (#669) or
	// another node re-claimed the task while we were partitioned, this agent
	// no longer owns the task and must not write a terminal status over the
	// new owner's state.
	if fresh.Status.AssignedNode != w.NodeName ||
		fresh.Status.ClaimedAt == nil ||
		t.Status.ClaimedAt == nil ||
		!fresh.Status.ClaimedAt.Equal(t.Status.ClaimedAt) {
		logf.FromContext(ctx).WithName("agentictask-watcher").WithValues("task", t.Name).
			Info("terminal patch skipped: task no longer owned by this agent",
				"assignedNode", fresh.Status.AssignedNode,
				"thisNode", w.NodeName)
		return nil
	}

	patch := client.MergeFrom(fresh.DeepCopy())
	now := metav1.Now()
	fresh.Status.FinishedAt = &now

	if execErr != nil {
		fresh.Status.Phase = foremanv1alpha1.AgenticTaskPhaseFailed
		fresh.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictIncomplete
		// v0.3 #559: the bubble-up path lands here. Map to
		// InfrastructureError (the watcher has no visibility into
		// what KIND of err this is, only that the executor wanted
		// the supervisor to see it).
		fresh.Status.FailureReason = foremanv1alpha1.FailureInfrastructureError
		setCondition(&fresh.Status.Conditions, metav1.Condition{
			Type:               "Completed",
			Status:             metav1.ConditionFalse,
			Reason:             "ExecutorError",
			Message:            execErr.Error(),
			LastTransitionTime: now,
		})
		return w.Client.Status().Patch(ctx, &fresh, patch)
	}

	if res == nil {
		// Defensive: nil error + nil result is a contract violation;
		// surface it explicitly rather than silently succeeding.
		fresh.Status.Phase = foremanv1alpha1.AgenticTaskPhaseFailed
		fresh.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictIncomplete
		fresh.Status.FailureReason = foremanv1alpha1.FailureInfrastructureError
		setCondition(&fresh.Status.Conditions, metav1.Condition{
			Type:               "Completed",
			Status:             metav1.ConditionFalse,
			Reason:             "ExecutorContractViolation",
			Message:            "executor returned nil error and nil Result",
			LastTransitionTime: now,
		})
		return w.Client.Status().Patch(ctx, &fresh, patch)
	}

	fresh.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
	fresh.Status.Verdict = res.Verdict
	// v0.3 #559: lift the structured FailureReason from the Result
	// envelope onto the status. Empty on a successful task (Verdict
	// in {GO, GATE-PASS}); set on Phase=Succeeded + non-success
	// Verdict (NO-GO, GATE-FAIL, INCOMPLETE).
	fresh.Status.FailureReason = res.FailureReason
	// #624: lift the produced branch + head commit out of the Result
	// envelope onto status so downstream consumers (verify/gate auto-spawn)
	// can key off them. Both executors stash these in Result.Extra: the
	// in-process path (goResult) and the Job-mode path (coderJobResultToResult)
	// set "branch"/"commitSHA" on a GO and "intendedBranch" on a non-GO, so we
	// coalesce them the same way runTaskResultFromResult does. Before this the
	// fields stayed empty even though the branch was pushed.
	if res.Extra != nil {
		fresh.Status.Branch = firstStringField(res.Extra, "branch", "intendedBranch")
		fresh.Status.CommitSHA = stringField(res.Extra, "commitSHA")
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	fresh.Status.Result = &runtime.RawExtension{Raw: raw}
	setCondition(&fresh.Status.Conditions, metav1.Condition{
		Type:               "Completed",
		Status:             metav1.ConditionTrue,
		Reason:             "ExecutorSucceeded",
		Message:            res.Summary,
		LastTransitionTime: now,
	})
	patchErr := w.Client.Status().Patch(ctx, &fresh, patch)
	if patchErr == nil {
		return nil
	}
	if !apierrors.IsInvalid(patchErr) {
		return patchErr
	}

	// The apiserver rejected the patch, most likely because res.Verdict or
	// res.FailureReason carries a string value not (yet) in the CRD enum.
	// This can happen when a future contract version emits a verdict string
	// that the currently-installed CRD does not know about. Without a
	// fallback the task would stay in Running forever, wedging the node.
	//
	// Defensive recovery: re-fetch, overwrite with minimal known-valid
	// status, and patch again. The raw Result JSON is preserved in
	// status.result because that field is x-kubernetes-preserve-unknown-fields
	// and is therefore not subject to enum validation.
	log := logf.FromContext(ctx).WithName("agentictask-watcher").WithValues("task", t.Name)
	log.Error(patchErr, "terminal patch rejected by apiserver; falling back to InfrastructureError",
		"rejectedVerdict", res.Verdict,
		"rejectedFailureReason", res.FailureReason,
	)

	var fallback foremanv1alpha1.AgenticTask
	if getErr := w.Client.Get(ctx, key, &fallback); getErr != nil {
		return fmt.Errorf("fallback refetch %s: %w", key, getErr)
	}
	fallbackPatch := client.MergeFrom(fallback.DeepCopy())
	fallbackNow := metav1.Now()
	fallback.Status.Phase = foremanv1alpha1.AgenticTaskPhaseFailed
	fallback.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictIncomplete
	fallback.Status.FailureReason = foremanv1alpha1.FailureInfrastructureError
	fallback.Status.FinishedAt = &fallbackNow
	// Preserve the raw result envelope: status.result is
	// x-kubernetes-preserve-unknown-fields so it survives the patch even
	// if Verdict inside the JSON is not in the current enum.
	fallback.Status.Result = &runtime.RawExtension{Raw: raw}

	// Truncate the validation error message so the condition stays within
	// apiserver limits (conditions.message is limited to 32768 chars).
	patchErrMsg := patchErr.Error()
	const maxErrLen = 512
	if len(patchErrMsg) > maxErrLen {
		patchErrMsg = patchErrMsg[:maxErrLen] + "... (truncated)"
	}
	setCondition(&fallback.Status.Conditions, metav1.Condition{
		Type:               "Completed",
		Status:             metav1.ConditionFalse,
		Reason:             "TerminalPatchRejected",
		Message:            fmt.Sprintf("original verdict %q rejected by apiserver: %s", res.Verdict, patchErrMsg),
		LastTransitionTime: fallbackNow,
	})
	return w.Client.Status().Patch(ctx, &fallback, fallbackPatch)
}

// setCondition upserts a condition by type. Unexported because both the
// watcher and the scheduler each have their own copy for now; if we add
// a third writer we move this to a shared internal package.
func setCondition(conds *[]metav1.Condition, c metav1.Condition) {
	for i, existing := range *conds {
		if existing.Type == c.Type {
			(*conds)[i] = c
			return
		}
	}
	*conds = append(*conds, c)
}
