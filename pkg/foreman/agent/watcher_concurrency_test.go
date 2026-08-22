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
}

func newModeExecutor(t *testing.T, supervise bool) *modeExecutor {
	t.Helper()
	e := &modeExecutor{supervise: supervise, release: make(chan struct{})}
	// Unblock any still-running Execute goroutines when the test ends.
	t.Cleanup(func() { close(e.release) })
	return e
}

func (*modeExecutor) Kind() string { return "test-mode" }

func (e *modeExecutor) Execute(
	ctx context.Context, task *foremanv1alpha1.AgenticTask,
) (*Result, error) {
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

// waitSupervised polls w.supervised (under its mutex) until it equals want.
func waitSupervised(w *AgenticTaskWatcher, want int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		w.inflightMu.Lock()
		got := w.supervised
		w.inflightMu.Unlock()
		if got == want {
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
			w := &AgenticTaskWatcher{
				Client:   newRecoveryClient(t, first, second),
				NodeName: "node-1",
				Executor: newModeExecutor(t, tc.supervise),
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
	w := &AgenticTaskWatcher{
		Client:             newRecoveryClient(t, objs[0], objs[1], objs[2]),
		NodeName:           "node-1",
		Executor:           newModeExecutor(t, true),
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
}

// TestLaunchExecutor_ReleasesSlotOnError: an executor that returns an error
// must release whichever slot it took, or the node wedges after one failure.
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
			exec := newModeExecutor(t, tc.supervise)
			exec.execErr = errors.New("executor exploded")
			w := &AgenticTaskWatcher{
				Client:               newRecoveryClient(t, task),
				NodeName:             "node-1",
				TaskLivenessInterval: time.Hour, // no watchdog interference
				Executor:             exec,
			}

			w.launchExecutor(context.Background(), task, tc.supervise)

			if !waitSupervised(w, 0, 2*time.Second) {
				t.Fatal("supervision slot leaked after executor error")
			}
			if !waitInflight(w, false, 2*time.Second) {
				t.Fatal("in-process slot leaked after executor error")
			}
			if got := getTask(t, w.Client, "boom").Status.Phase; got != foremanv1alpha1.AgenticTaskPhaseFailed {
				t.Fatalf("task phase after executor error: want Failed got %s", got)
			}
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
// above; it is never invoked.
type stubCoderJobSubmitter struct{}

func (stubCoderJobSubmitter) Submit(context.Context, CoderJobRequest) (CoderJobResult, error) {
	return CoderJobResult{}, errors.New("not called")
}
