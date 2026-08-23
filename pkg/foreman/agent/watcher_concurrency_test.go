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
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// #1559: a Job-mode task's work runs in an ephemeral Job pod, so it must not
// hold the agent's single in-process slot for the Job's whole lifetime. These
// tests pin the resulting slot accounting: in-process runs still serialize,
// Job-mode supervisions run concurrently up to MaxSupervisedTasks, and both
// slots are released on the error path.

// modeExecutor blocks in Execute until release is closed and reports whichever
// execution mode a test needs, so slot accounting can be driven without a real
// coder Job.
type modeExecutor struct {
	supervise bool
	release   chan struct{}
	execErr   error

	stopOnce sync.Once
	// wg joins the Execute calls the watcher launched. Without it a test
	// body can return while an Execute goroutine is still running, and it
	// then patches status and logs against a finished test.
	wg sync.WaitGroup
	// started takes one send per Execute entry, so waitStarted can prove
	// every launched goroutine has reached Execute -- and so has already
	// done its wg.Add -- before the cleanup Waits on the group.
	started chan struct{}
}

func newModeExecutor(t *testing.T, supervise bool) *modeExecutor {
	t.Helper()
	e := &modeExecutor{
		supervise: supervise,
		release:   make(chan struct{}),
		// Buffered so Execute never blocks on a test that does not read
		// every send; deeper than any task count these tests use.
		started: make(chan struct{}, 8),
	}
	// Unblock any still-running Execute goroutines when the test ends, then
	// join them.
	t.Cleanup(func() {
		e.stop()
		e.wg.Wait()
	})
	return e
}

// stop unblocks every in-flight Execute. Idempotent, so the cleanup can call
// it whether or not a test already did.
func (e *modeExecutor) stop() {
	e.stopOnce.Do(func() { close(e.release) })
}

// waitStarted blocks until n Execute calls have entered.
func (e *modeExecutor) waitStarted(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		select {
		case <-e.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d Execute call(s) started; want %d", i, n)
		}
	}
}

func (*modeExecutor) Kind() string { return "test-mode" }

func (e *modeExecutor) Execute(
	ctx context.Context, task *foremanv1alpha1.AgenticTask,
) (*Result, error) {
	e.wg.Add(1)
	defer e.wg.Done()
	e.started <- struct{}{}

	if e.execErr != nil {
		return nil, e.execErr
	}
	select {
	case <-e.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &Result{
		SchemaVersion: ResultSchemaVersion,
		Kind:          e.Kind(),
		Verdict:       foremanv1alpha1.AgenticTaskVerdictGo,
		Summary:       "test",
	}, nil
}

func (e *modeExecutor) SupervisesExternally(
	_ context.Context, _ *foremanv1alpha1.AgenticTask,
) bool {
	return e.supervise
}

// pollUntilLeavesScheduled polls until the named task is claimed, or the
// timeout expires. Retrying is deliberate: the slot a finished run held is
// released after its terminal status patch, so the first poll after a failure
// can legitimately still see it held.
func pollUntilLeavesScheduled(
	t *testing.T, w *AgenticTaskWatcher, name string, timeout time.Duration,
) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if err := w.pollOnce(context.Background(), "default"); err != nil {
			t.Fatalf("pollOnce: %v", err)
		}
		if getTask(t, w.Client, name).Status.Phase != foremanv1alpha1.AgenticTaskPhaseScheduled {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitPhase waits for the named task to reach phase.
func waitPhase(
	t *testing.T, w *AgenticTaskWatcher, name string,
	phase foremanv1alpha1.AgenticTaskPhase, timeout time.Duration,
) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if getTask(t, w.Client, name).Status.Phase == phase {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// countPhase reports how many of the named tasks are in the given phase.
func countPhase(
	t *testing.T, w *AgenticTaskWatcher, phase foremanv1alpha1.AgenticTaskPhase, names ...string,
) int {
	t.Helper()
	n := 0
	for _, name := range names {
		if getTask(t, w.Client, name).Status.Phase == phase {
			n++
		}
	}
	return n
}

// TestPollOnce_SlotAccountingByExecutionMode: with one task already running,
// a second in-process task must wait, but a second Job-mode task must be
// claimed — the agent is idle while it supervises (#1559).
func TestPollOnce_SlotAccountingByExecutionMode(t *testing.T) {
	tests := []struct {
		name        string
		supervise   bool
		wantRunning int
	}{
		{
			name:        "in-process runs serialize on the single slot",
			supervise:   false,
			wantRunning: 1,
		},
		{
			name:        "job-mode supervisions run concurrently",
			supervise:   true,
			wantRunning: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first := scheduledTask("first", "node-1", foremanv1alpha1.AgenticTaskKindIssueFix, time.Hour)
			second := scheduledTask("second", "node-1", foremanv1alpha1.AgenticTaskKindIssueFix, time.Minute)
			exec := newModeExecutor(t, tc.supervise)
			w := &AgenticTaskWatcher{
				Client:   newRecoveryClient(t, first, second),
				NodeName: "node-1",
				Executor: exec,
			}

			for range 2 {
				if err := w.pollOnce(context.Background(), "default"); err != nil {
					t.Fatalf("pollOnce: %v", err)
				}
			}

			got := countPhase(t, w, foremanv1alpha1.AgenticTaskPhaseRunning, "first", "second")
			if got != tc.wantRunning {
				t.Fatalf("Running tasks: want %d got %d", tc.wantRunning, got)
			}
			// Every claimed task has an Execute goroutine; join them all
			// before the test body returns.
			exec.waitStarted(t, tc.wantRunning)
		})
	}
}

// TestPollOnce_SupervisionBoundEnforced: Job-mode tasks do not hold the
// in-process slot, but they are not unbounded either — MaxSupervisedTasks
// caps how many coder Jobs one node has outstanding.
func TestPollOnce_SupervisionBoundEnforced(t *testing.T) {
	names := []string{"job-1", "job-2", "job-3"}
	objs := make([]*foremanv1alpha1.AgenticTask, 0, len(names))
	for i, name := range names {
		objs = append(objs, scheduledTask(
			name, "node-1", foremanv1alpha1.AgenticTaskKindIssueFix,
			time.Duration(len(names)-i)*time.Hour,
		))
	}
	exec := newModeExecutor(t, true)
	w := &AgenticTaskWatcher{
		Client:             newRecoveryClient(t, objs[0], objs[1], objs[2]),
		NodeName:           "node-1",
		Executor:           exec,
		MaxSupervisedTasks: 2,
	}

	for range len(names) {
		if err := w.pollOnce(context.Background(), "default"); err != nil {
			t.Fatalf("pollOnce: %v", err)
		}
	}

	if got := countPhase(t, w, foremanv1alpha1.AgenticTaskPhaseRunning, names...); got != 2 {
		t.Fatalf("Running tasks: want 2 (the bound) got %d", got)
	}
	if got := countPhase(t, w, foremanv1alpha1.AgenticTaskPhaseScheduled, names...); got != 1 {
		t.Fatalf("Scheduled tasks: want 1 (held back by the bound) got %d", got)
	}
	exec.waitStarted(t, 2)
}

// TestLaunchExecutor_ReleasesSlotOnError: an executor that returns an error
// must release whichever slot it took, or the node wedges after one failure.
// The release is asserted through its observable consequence -- a follow-up
// poll can claim another task -- rather than by reading the counters.
func TestLaunchExecutor_ReleasesSlotOnError(t *testing.T) {
	tests := []struct {
		name      string
		supervise bool
	}{
		{name: "in-process", supervise: false},
		{name: "job-mode", supervise: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Second-precision claimedAt: metav1.Time JSON-encodes to
			// seconds, so a sub-second value would fail patchTerminal's
			// ownership guard after the fake client's round-trip.
			claimedAt := metav1.NewTime(time.Unix(time.Now().Unix(), 0))
			task := claimedRunningTask("boom", "node-1", claimedAt)
			next := scheduledTask("next", "node-1", foremanv1alpha1.AgenticTaskKindIssueFix, time.Minute)
			exec := newModeExecutor(t, tc.supervise)
			exec.execErr = errors.New("executor exploded")
			w := &AgenticTaskWatcher{
				Client:               newRecoveryClient(t, task, next),
				NodeName:             "node-1",
				TaskLivenessInterval: time.Hour, // no watchdog interference
				Executor:             exec,
				// One supervision slot, so a leaked one blocks the follow-up
				// claim instead of hiding under the default of 4.
				MaxSupervisedTasks: 1,
			}

			w.launchExecutor(context.Background(), task, tc.supervise)

			if !waitPhase(t, w, "boom", foremanv1alpha1.AgenticTaskPhaseFailed, 2*time.Second) {
				t.Fatalf("task phase after executor error: want Failed got %s",
					getTask(t, w.Client, "boom").Status.Phase)
			}
			if !pollUntilLeavesScheduled(t, w, "next", 2*time.Second) {
				t.Fatal("slot leaked after the executor error: no further task could be claimed")
			}
			exec.waitStarted(t, 2)
		})
	}
}

// TestSupervisesExternally: the watcher's mode question must be answered by
// the same predicate Execute dispatches on, so slot accounting cannot drift
// from the path actually taken (#1559).
func TestSupervisesExternally(t *testing.T) {
	jobAgent := &foremanv1alpha1.Agent{}
	jobAgent.Name = "coder"
	jobAgent.Namespace = "default"
	jobAgent.Spec.Execution = &foremanv1alpha1.ExecutionSpec{
		Mode: foremanv1alpha1.ExecutionModeJob,
	}
	inProcAgent := &foremanv1alpha1.Agent{}
	inProcAgent.Name = "inproc"
	inProcAgent.Namespace = "default"

	tests := []struct {
		name          string
		agentRef      string
		withSubmitter bool
		want          bool
	}{
		{name: "job-mode agent with submitter", agentRef: "coder", withSubmitter: true, want: true},
		{name: "job-mode agent without submitter", agentRef: "coder", want: false},
		{name: "in-process agent", agentRef: "inproc", withSubmitter: true, want: false},
		{name: "missing agent", agentRef: "gone", withSubmitter: true, want: false},
		{name: "no agentRef", agentRef: "", withSubmitter: true, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &NativeAgentLoopExecutor{Client: newRecoveryClient(t, jobAgent, inProcAgent)}
			if tc.withSubmitter {
				e.CoderJobSubmitter = stubCoderJobSubmitter{}
			}
			task := claimedRunningTask("t", "node-1", metav1.Now())
			if tc.agentRef != "" {
				task.Spec.AgentRef = &corev1.LocalObjectReference{Name: tc.agentRef}
			}
			if got := e.SupervisesExternally(context.Background(), task); got != tc.want {
				t.Fatalf("SupervisesExternally: want %v got %v", tc.want, got)
			}
		})
	}
}

// stubCoderJobSubmitter satisfies CoderJobSubmitter for the wiring checks
// above; it is never invoked. (executor_native_test.go has an equivalent
// double, but it lives in package agent_test and cannot be reached from here.)
type stubCoderJobSubmitter struct{}

func (stubCoderJobSubmitter) Submit(context.Context, CoderJobRequest) (CoderJobResult, error) {
	return CoderJobResult{}, errors.New("not called")
}
