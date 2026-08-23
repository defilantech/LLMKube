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
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// #1559: a Job-mode task's work runs in an ephemeral Job pod, so it must not
// hold the agent's single in-process slot for the Job's whole lifetime. These
// tests pin the resulting slot accounting: in-process runs still serialize,
// Job-mode supervisions run concurrently up to MaxSupervisedTasks, and both
// slots are released on the error path.

// blockingExecutor blocks in Execute until release is closed, so a test can
// hold a slot for as long as it needs one. It deliberately does NOT implement
// SupervisingExecutor: it stands in for the stub executor, and for any node
// whose Executor cannot supervise at all.
type blockingExecutor struct {
	release  chan struct{}
	execErr  error
	stopOnce sync.Once

	// agentsMu guards agents.
	agentsMu sync.Mutex
	// agents records the Agent value handed to each Execute call, so a test
	// can assert the dispatch loop passed on the Agent it resolved instead
	// of leaving the executor to read its own.
	agents []*foremanv1alpha1.Agent

	// wg joins the Execute calls the watcher launched. Without it the test
	// body can return while an Execute goroutine is still running, and it
	// then logs (and patches) against a finished test.
	wg sync.WaitGroup
	// started takes one send per Execute entry, so waitStarted can prove
	// every launched goroutine has reached Execute (and so has already done
	// its wg.Add) before the cleanup Waits on the group.
	started chan struct{}
}

func newBlockingExecutor(t *testing.T) *blockingExecutor {
	t.Helper()
	e := &blockingExecutor{
		release: make(chan struct{}),
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

// stop unblocks every in-flight Execute. Idempotent: the cleanup calls it even
// when a test already did.
func (e *blockingExecutor) stop() {
	e.stopOnce.Do(func() { close(e.release) })
}

// waitStarted blocks until n Execute calls have entered.
func (e *blockingExecutor) waitStarted(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		select {
		case <-e.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d Execute call(s) started; want %d", i, n)
		}
	}
}

func (*blockingExecutor) Kind() string { return "test-mode" }

func (e *blockingExecutor) Execute(
	ctx context.Context, _ *foremanv1alpha1.AgenticTask, agent *foremanv1alpha1.Agent,
) (*Result, error) {
	e.wg.Add(1)
	defer e.wg.Done()
	e.agentsMu.Lock()
	e.agents = append(e.agents, agent)
	e.agentsMu.Unlock()
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

// executedAgents returns the Agent handed to each Execute call so far, in
// call order.
func (e *blockingExecutor) executedAgents() []*foremanv1alpha1.Agent {
	e.agentsMu.Lock()
	defer e.agentsMu.Unlock()
	return append([]*foremanv1alpha1.Agent(nil), e.agents...)
}

// modeExecutor is a blockingExecutor that also answers the watcher's
// execution-mode question, so slot accounting can be driven without a real
// coder Job. It answers PER AGENT, keyed by name, because that is what the
// real predicate does: mode is a property of the Agent, so a node's fleet can
// be mixed. An Agent not in the map (and a nil Agent) runs in-process.
type modeExecutor struct {
	*blockingExecutor
	jobMode map[string]bool
}

func newModeExecutor(t *testing.T, jobMode map[string]bool) *modeExecutor {
	t.Helper()
	return &modeExecutor{blockingExecutor: newBlockingExecutor(t), jobMode: jobMode}
}

func (e *modeExecutor) SupervisesAgent(agent *foremanv1alpha1.Agent) bool {
	if agent == nil {
		return false
	}
	return e.jobMode[agent.Name]
}

// agentCR builds an Agent whose spec.execution.mode matches what the
// modeExecutor double will answer for it, so the fixture and the double do
// not tell two different stories.
func agentCR(name string, jobMode bool) *foremanv1alpha1.Agent {
	a := &foremanv1alpha1.Agent{}
	a.Name = name
	a.Namespace = "default"
	if jobMode {
		a.Spec.Execution = &foremanv1alpha1.ExecutionSpec{Mode: foremanv1alpha1.ExecutionModeJob}
	}
	return a
}

// taskForAgent is scheduledTask plus the agentRef the dispatch loop resolves
// before it can decide which slot the task wants.
func taskForAgent(name, agentName string, age time.Duration) *foremanv1alpha1.AgenticTask {
	task := scheduledTask(name, "node-1", foremanv1alpha1.AgenticTaskKindIssueFix, age)
	task.Spec.AgentRef = &corev1.LocalObjectReference{Name: agentName}
	return task
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
			first := taskForAgent("first", "coder", time.Hour)
			second := taskForAgent("second", "coder", time.Minute)
			exec := newModeExecutor(t, map[string]bool{"coder": tc.supervise})
			w := &AgenticTaskWatcher{
				Client:   newRecoveryClient(t, first, second, agentCR("coder", tc.supervise)),
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
	objs := []client.Object{agentCR("coder", true)}
	for i, name := range names {
		objs = append(objs, taskForAgent(name, "coder", time.Duration(len(names)-i)*time.Hour))
	}
	exec := newModeExecutor(t, map[string]bool{"coder": true})
	w := &AgenticTaskWatcher{
		Client:             newRecoveryClient(t, objs...),
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

// TestPollOnce_SkipsListWhenNoSlotCanTakeWork: an Executor that cannot
// supervise has no supervision budget to spend, so a busy in-process slot must
// short-circuit the poll BEFORE the List. The agent's client is uncached, so a
// guard that never fires means a full namespace List every tick for the whole
// run, which then skips every candidate (#1635 review).
func TestPollOnce_SkipsListWhenNoSlotCanTakeWork(t *testing.T) {
	var lists atomic.Int32
	held := scheduledTask("held", "node-1", foremanv1alpha1.AgenticTaskKindIssueFix, time.Hour)
	queued := scheduledTask("queued", "node-1", foremanv1alpha1.AgenticTaskKindIssueFix, time.Minute)
	c := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(held, queued).
		WithStatusSubresource(&foremanv1alpha1.AgenticTask{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(
				ctx context.Context, cl client.WithWatch,
				list client.ObjectList, opts ...client.ListOption,
			) error {
				lists.Add(1)
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()
	exec := newBlockingExecutor(t)
	w := &AgenticTaskWatcher{
		Client:               c,
		NodeName:             "node-1",
		TaskLivenessInterval: time.Hour, // no watchdog interference
		Executor:             exec,
	}

	if err := w.pollOnce(context.Background(), "default"); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	exec.waitStarted(t, 1)
	if got := getTask(t, w.Client, "held").Status.Phase; got != foremanv1alpha1.AgenticTaskPhaseRunning {
		t.Fatalf("first poll did not claim the in-process slot: phase %s", got)
	}

	before := lists.Load()
	for range 3 {
		if err := w.pollOnce(context.Background(), "default"); err != nil {
			t.Fatalf("pollOnce: %v", err)
		}
	}
	if got := lists.Load() - before; got != 0 {
		t.Fatalf("pollOnce issued %d List(s) with the only slot busy; want 0", got)
	}
}

// TestPollOnce_ResolvesAgentOncePerPass: resolving a candidate's execution
// mode costs an uncached Agent GET, and a pass has to resolve the candidates
// AHEAD of the one it claims too, since it cannot know which slot they want
// without asking. Candidates queued on one node nearly always share a single
// Agent, so a pass must read it once, not once per candidate (#1635 review).
func TestPollOnce_ResolvesAgentOncePerPass(t *testing.T) {
	names := []string{"job-1", "job-2", "job-3", "job-4"}
	objs := []client.Object{agentCR("coder", true)}
	for i, name := range names {
		objs = append(objs, taskForAgent(name, "coder", time.Duration(len(names)-i)*time.Hour))
	}
	var agentGets atomic.Int32
	c := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&foremanv1alpha1.AgenticTask{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				ctx context.Context, cl client.WithWatch,
				key client.ObjectKey, obj client.Object, opts ...client.GetOption,
			) error {
				if _, ok := obj.(*foremanv1alpha1.Agent); ok {
					agentGets.Add(1)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	exec := newModeExecutor(t, map[string]bool{"coder": true})
	w := &AgenticTaskWatcher{
		Client:               c,
		NodeName:             "node-1",
		TaskLivenessInterval: time.Hour, // no watchdog interference
		Executor:             exec,
		MaxSupervisedTasks:   1,
	}

	// Spend the whole budget, so the next pass walks every remaining
	// candidate instead of claiming the first one and returning.
	if err := w.pollOnce(context.Background(), "default"); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	exec.waitStarted(t, 1)

	agentGets.Store(0)
	if err := w.pollOnce(context.Background(), "default"); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if got := agentGets.Load(); got != 1 {
		t.Fatalf("Agent read %d time(s) for %d candidates sharing one Agent; want 1",
			got, len(names)-1)
	}
}

// TestLaunchExecutor_ReleasesSlotOnError: an executor that returns an error
// must release whichever slot it took, or the node wedges after one failure.
// The release is asserted through its observable consequence — a follow-up
// poll can claim another task — rather than by reading the counters.
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
			next := taskForAgent("next", "coder", time.Minute)
			exec := newModeExecutor(t, map[string]bool{"coder": tc.supervise})
			exec.execErr = errors.New("executor exploded")
			w := &AgenticTaskWatcher{
				Client:               newRecoveryClient(t, task, next, agentCR("coder", tc.supervise)),
				NodeName:             "node-1",
				TaskLivenessInterval: time.Hour, // no watchdog interference
				Executor:             exec,
				// One supervision slot, so a leaked one blocks the
				// follow-up claim instead of hiding under the default of 4.
				MaxSupervisedTasks: 1,
			}

			w.launchExecutor(context.Background(), task, agentCR("coder", tc.supervise), tc.supervise)

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
