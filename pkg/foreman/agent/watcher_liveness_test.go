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
	"errors"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// #1136: the per-run liveness watchdog must cancel the executor's context when
// the inflight AgenticTask is deleted out from under the agent (e.g. a
// `kubectl delete workload` GC-ing its child task), so the loop stops
// generating instead of running on to LoopBudget. These tests exercise the
// watchdog in isolation and its wiring through launchExecutor.

const testLivenessInterval = 20 * time.Millisecond

// TestWatchTaskLiveness_CancelsOnDelete: a definitive NotFound (the task was
// deleted) must cancel the run.
func TestWatchTaskLiveness_CancelsOnDelete(t *testing.T) {
	task := runningTask("live-del", "m5max-coder")
	c := newRecoveryClient(t, task)
	w := &AgenticTaskWatcher{
		Client:               c,
		NodeName:             "m5max-coder",
		Namespace:            "default",
		TaskLivenessInterval: testLivenessInterval,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.watchTaskLiveness(ctx, cancel, task)

	if err := c.Delete(context.Background(), task); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	select {
	case <-ctx.Done():
		// watchdog cancelled the run: correct.
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not cancel the run after task deletion")
	}
}

// TestWatchTaskLiveness_CancelsOnDeletionTimestamp: a finalizer-mediated delete
// leaves the object present with DeletionTimestamp set; that terminating state
// must also cancel the run (the task is on its way out).
func TestWatchTaskLiveness_CancelsOnDeletionTimestamp(t *testing.T) {
	task := runningTask("live-term", "m5max-coder")
	task.Finalizers = []string{"llmkube.dev/test-hold"}
	c := newRecoveryClient(t, task)
	w := &AgenticTaskWatcher{
		Client:               c,
		NodeName:             "m5max-coder",
		Namespace:            "default",
		TaskLivenessInterval: testLivenessInterval,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.watchTaskLiveness(ctx, cancel, task)

	// Finalizer present, so Delete sets DeletionTimestamp rather than removing.
	if err := c.Delete(context.Background(), task); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	select {
	case <-ctx.Done():
		// watchdog cancelled on the terminating task: correct.
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not cancel the run for a task with DeletionTimestamp")
	}
}

// TestWatchTaskLiveness_FailsOpenOnTransientErrors: persistent NON-NotFound
// errors (a partition, apiserver hiccup) must NOT be mistaken for a deletion.
// The watchdog gives up after a bounded number of misses WITHOUT cancelling,
// so the run continues under its LoopBudget as before.
func TestWatchTaskLiveness_FailsOpenOnTransientErrors(t *testing.T) {
	task := runningTask("live-transient", "m5max-coder")
	c := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(task).
		WithStatusSubresource(&foremanv1alpha1.AgenticTask{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return errors.New("apiserver unavailable")
			},
		}).
		Build()
	w := &AgenticTaskWatcher{
		Client:               c,
		NodeName:             "m5max-coder",
		Namespace:            "default",
		TaskLivenessInterval: testLivenessInterval,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { w.watchTaskLiveness(ctx, cancel, task); close(done) }()

	select {
	case <-done:
		// watchdog gave up after the transient-error cap: correct.
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not give up on persistent transient errors")
	}
	if ctx.Err() != nil {
		t.Fatal("watchdog cancelled the run on transient errors; it must fail OPEN")
	}
}

// TestLaunchExecutor_AbortsInflightRunOnDelete: end-to-end wiring — a blocking
// executor is launched, its task is deleted, and the run must unwind (inflight
// cleared) because the watchdog cancelled the executor context.
func TestLaunchExecutor_AbortsInflightRunOnDelete(t *testing.T) {
	task := runningTask("live-wire", "m5max-coder")
	c := newRecoveryClient(t, task)
	w := &AgenticTaskWatcher{
		Client:               c,
		NodeName:             "m5max-coder",
		Namespace:            "default",
		TaskLivenessInterval: testLivenessInterval,
		Executor:             &StubExecutor{SleepDuration: time.Hour}, // blocks until ctx cancelled
	}

	w.launchExecutor(context.Background(), task)

	// Confirm the run is actually in flight before deleting.
	if !waitInflight(w, true, time.Second) {
		t.Fatal("executor never went inflight")
	}
	if err := c.Delete(context.Background(), task); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	if !waitInflight(w, false, 3*time.Second) {
		t.Fatal("inflight run did not unwind after task deletion")
	}
}

// waitInflight polls w.inflight (under its mutex) until it matches want or the
// timeout elapses.
func waitInflight(w *AgenticTaskWatcher, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		w.inflightMu.Lock()
		got := w.inflight != nil
		w.inflightMu.Unlock()
		if got == want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
