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
	"sync"
	"time"

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

// pollOnce runs a single List() pass and dispatches any task assigned to
// this node that is in phase=Scheduled.
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

	for i := range list.Items {
		t := &list.Items[i]
		if t.Status.AssignedNode != w.NodeName {
			continue
		}
		if t.Status.Phase != foremanv1alpha1.AgenticTaskPhaseScheduled {
			continue
		}
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
	patch := client.MergeFrom(fresh.DeepCopy())
	now := metav1.Now()
	fresh.Status.FinishedAt = &now

	if execErr != nil {
		fresh.Status.Phase = foremanv1alpha1.AgenticTaskPhaseFailed
		fresh.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictIncomplete
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
	return w.Client.Status().Patch(ctx, &fresh, patch)
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
