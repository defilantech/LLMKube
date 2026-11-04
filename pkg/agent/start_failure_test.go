/*
Copyright 2026.

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
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These tests cover #2404: a start that fails for a reason that is neither a
// refusal nor a transient API error (an engine that exits while loading, a
// download that hits a full disk, a startup timeout) used to log and do
// nothing else. The InferenceService kept the controller's "Waiting for the
// host metal-agent to fetch the model" and the start was never retried.

// The wire values a user and the controller see for a failed start.
const (
	wantStartFailedReason = "StartFailed"
	waitingForAgentStatus = "WaitingForMetalAgent"
)

// engineCrashedOnLoad is the error MetalExecutor.StartProcess returns for an
// engine that exits while loading its model: the exit status and the tail of
// the engine log, as withLogTail attaches it.
func engineCrashedOnLoad(logPath string) error {
	return withLogTail(fmt.Errorf("llama-server %w (exit status 1)", errChildExited), logPath)
}

// startFailureFixture is a refusal fixture over InferenceService "svc" whose
// Model has a known size (so memory admission completes), with a fake clock,
// a watcher (so a refusal-style re-check would be visible) and an executor
// whose start fails with an engine crash carrying a log tail.
func startFailureFixture(t *testing.T) (*MetalAgent, *recordingExecutor, *record.FakeRecorder, *fakeClock) {
	t.Helper()
	model := refusalModel()
	model.Status.Size = "1.0 GiB"
	return startFailureFixtureFor(t, model)
}

// startFailureFixtureFor is startFailureFixture over model.
func startFailureFixtureFor(
	t *testing.T, model *inferencev1alpha1.Model,
) (*MetalAgent, *recordingExecutor, *record.FakeRecorder, *fakeClock) {
	t.Helper()
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, refusalISVC("svc"), model)
	clk := &fakeClock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	a.now = clk.now
	a.watcher = NewInferenceServiceWatcher(a.config.K8sClient, "default", newNopLogger())
	a.watcher.now = clk.now

	logPath := writeEngineLog(t, a.config.ModelStorePath,
		"llama_model_loader: loaded meta data with 41 key-value pairs\n"+
			"llama_model_load: error loading model: unknown model architecture: 'qwen9'\n")
	ex.startErr = engineCrashedOnLoad(logPath)
	return a, ex, rec, clk
}

// writeEngineLog writes content as the engine log of default/svc in store and
// returns its path.
func writeEngineLog(t *testing.T, store, content string) string {
	t.Helper()
	path := store + "/llama-server-default-svc.log"
	f, err := createNoFollow(path)
	if err != nil {
		t.Fatalf("write engine log: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write engine log: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close engine log: %v", err)
	}
	return path
}

// eventsWithReason returns the recorded events whose reason is reason.
func eventsWithReason(events []string, reason string) []string {
	var out []string
	for _, e := range events {
		if strings.Contains(e, " "+reason+" ") {
			out = append(out, e)
		}
	}
	return out
}

// runRestartsAt moves the fake clock to at and runs the restarts due then.
func runRestartsAt(a *MetalAgent, clk *fakeClock, at time.Time) {
	clk.t = at
	a.runDueRestarts(context.Background())
}

// TestEnsureProcess_FailedStartIsReportedAndRetried is the #2404 bug: a start
// whose engine exits while loading must leave a Warning Event with the error
// and the engine log tail, a status that says the start failed (so the
// controller stops saying the agent is fetching the model), and a retry on the
// health-restart backoff (10s, then 20s, ...) that needs no InferenceService
// change. Once a retry brings an engine up, the status is cleared.
func TestEnsureProcess_FailedStartIsReportedAndRetried(t *testing.T) {
	a, ex, rec, clk := startFailureFixture(t)
	t0 := clk.t
	ctx := context.Background()

	if err := a.ensureProcess(ctx, freshISVC(t, a)); err == nil {
		t.Fatal("ensureProcess succeeded; the test needs the start to fail")
	}

	events := drainEvents(rec)
	failed := eventsWithReason(events, wantStartFailedReason)
	if len(failed) != 1 {
		t.Errorf("events = %v, want one Warning %s", events, wantStartFailedReason)
	} else {
		for _, want := range []string{"Warning", "exit status 1", "unknown model architecture: 'qwen9'"} {
			if !strings.Contains(failed[0], want) {
				t.Errorf("%s event %q does not carry %q", wantStartFailedReason, failed[0], want)
			}
		}
	}
	reason, message := isvcStatus(t, a)
	if reason != wantStartFailedReason || !strings.Contains(message, "exit status 1") {
		t.Errorf("status = %q / %q, want %s naming the exit status", reason, message, wantStartFailedReason)
	}
	if !restartPending(a, "default/svc") {
		t.Errorf("no restart pending after the failed start: it is never retried")
	}
	// A failed start is retried on the restart backoff only, not re-checked
	// every few seconds like a refusal: each retry may download or load the
	// whole model again.
	a.watcher.requeueMu.Lock()
	_, requeued := a.watcher.requeued["default/svc"]
	a.watcher.requeueMu.Unlock()
	if requeued {
		t.Errorf("the failed start was queued for a refusal re-check, which ignores the restart backoff")
	}

	steps := []struct {
		at         time.Duration
		wantStarts int
	}{
		{9 * time.Second, 1},  // inside the first 10s backoff
		{10 * time.Second, 2}, // first retry
		{29 * time.Second, 2}, // inside the doubled 20s backoff
		{30 * time.Second, 3}, // second retry
	}
	for _, s := range steps {
		runRestartsAt(a, clk, t0.Add(s.at))
		if ex.starts != s.wantStarts {
			t.Fatalf("at +%s: starts = %d, want %d", s.at, ex.starts, s.wantStarts)
		}
	}
	if reason, _ := isvcStatus(t, a); reason != wantStartFailedReason {
		t.Errorf("status after a failed retry = %q, want %s", reason, wantStartFailedReason)
	}

	ex.startErr = nil
	runRestartsAt(a, clk, t0.Add(70*time.Second))
	if ex.starts != 4 {
		t.Fatalf("starts = %d after the third retry, want 4", ex.starts)
	}
	a.mu.RLock()
	running := a.processes["default/svc"] != nil
	a.mu.RUnlock()
	if !running {
		t.Fatal("no engine tracked after the retry succeeded")
	}
	if reason, message := isvcStatus(t, a); reason != "" || message != "" {
		t.Errorf("status after the engine came up = %q / %q, want it cleared", reason, message)
	}
	runRestartsAt(a, clk, t0.Add(time.Hour))
	if ex.starts != 4 {
		t.Errorf("starts = %d an hour after the engine came up, want 4 (no further retries)", ex.starts)
	}
}

// TestEnsureProcess_FailedStartStatusClearedWithoutAdmission covers the path
// where memory admission does not clear the status on success: an unsized
// Model in memory-check-mode=warn proceeds without completing admission, so
// nothing but the start itself can clear StartFailed once an engine runs.
func TestEnsureProcess_FailedStartStatusClearedWithoutAdmission(t *testing.T) {
	a, ex, _, clk := startFailureFixtureFor(t, refusalModel()) // no size: admission cannot complete
	if !a.memoryCheckWarnOnly {
		t.Fatal("setup: the fixture must run memory-check-mode=warn")
	}

	_ = a.ensureProcess(context.Background(), freshISVC(t, a))
	if reason, _ := isvcStatus(t, a); reason != wantStartFailedReason {
		t.Fatalf("status = %q after the failed start, want %s", reason, wantStartFailedReason)
	}
	ex.startErr = nil
	runRestartsAt(a, clk, clk.t.Add(10*time.Second))
	if ex.starts != 2 {
		t.Fatalf("starts = %d, want 2 (the retry)", ex.starts)
	}
	if reason, message := isvcStatus(t, a); reason != "" || message != "" {
		t.Errorf("status after the engine came up = %q / %q, want it cleared", reason, message)
	}
}

// TestEnsureProcess_FailedStartRetryEnds checks that a failed start's retry
// ends the way a health restart's does: a delete or a scale to zero cancels
// it, and a spec change starts the new spec at once instead of waiting out
// the backoff.
func TestEnsureProcess_FailedStartRetryEnds(t *testing.T) {
	cases := []struct {
		name string
		// act delivers the change and returns the starts it expects right
		// after it and an hour later.
		act func(t *testing.T, a *MetalAgent, ex *recordingExecutor) (now, later int)
	}{
		{"deleted", func(t *testing.T, a *MetalAgent, ex *recordingExecutor) (int, int) {
			ev := InferenceServiceEvent{Type: EventTypeDeleted, InferenceService: freshISVC(t, a)}
			if err := a.handleEvent(context.Background(), ev); err != nil {
				t.Fatalf("DELETED: %v", err)
			}
			return 1, 1
		}},
		{"scaled to zero", func(t *testing.T, a *MetalAgent, ex *recordingExecutor) (int, int) {
			isvc := freshISVC(t, a)
			isvc.Spec.Replicas = ptr.To[int32](0)
			if err := a.config.K8sClient.Update(context.Background(), isvc); err != nil {
				t.Fatal(err)
			}
			ev := InferenceServiceEvent{Type: EventTypeUpdated, InferenceService: freshISVC(t, a)}
			if err := a.handleEvent(context.Background(), ev); err != nil {
				t.Fatalf("UPDATED: %v", err)
			}
			return 1, 1
		}},
		{"spec changed", func(t *testing.T, a *MetalAgent, ex *recordingExecutor) (int, int) {
			ex.startErr = nil
			isvc := freshISVC(t, a)
			isvc.Spec.ContextSize = ptr.To[int32](8192)
			if err := a.config.K8sClient.Update(context.Background(), isvc); err != nil {
				t.Fatal(err)
			}
			ev := InferenceServiceEvent{Type: EventTypeUpdated, InferenceService: freshISVC(t, a)}
			if err := a.handleEvent(context.Background(), ev); err != nil {
				t.Fatalf("UPDATED: %v", err)
			}
			// The new spec starts on the next loop turn, without the backoff.
			a.runDueRestarts(context.Background())
			return 2, 2
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, ex, _, clk := startFailureFixture(t)
			_ = a.ensureProcess(context.Background(), freshISVC(t, a))
			if ex.starts != 1 {
				t.Fatalf("setup: starts = %d, want 1", ex.starts)
			}
			now, later := tc.act(t, a, ex)
			if ex.starts != now {
				t.Errorf("starts = %d right after the change, want %d", ex.starts, now)
			}
			runRestartsAt(a, clk, clk.t.Add(time.Hour))
			if ex.starts != later {
				t.Errorf("starts = %d an hour after the change, want %d", ex.starts, later)
			}
		})
	}
}

// TestHealthRestart_FailedRestartSaysStartFailed checks that a health restart
// whose start fails also stops the status from claiming the agent is fetching
// the model: memory admission clears the status before every start, so
// without a write after the failure the controller's WaitingForMetalAgent
// would come back.
func TestHealthRestart_FailedRestartSaysStartFailed(t *testing.T) {
	f := newRestartFixture(t, "restart-status")
	f.exec.startErr = engineCrashedOnLoad(writeEngineLog(t, t.TempDir(), "fatal: out of memory\n"))
	f.probe(DefaultHealthFailureThreshold)
	f.agent.runDueRestarts(context.Background())
	if got := f.exec.starts(); got != 1 {
		t.Fatalf("starts = %d after the restart, want 1", got)
	}
	cur := &inferencev1alpha1.InferenceService{}
	key := types.NamespacedName{Namespace: "default", Name: f.isvc.Name}
	if err := f.client.Get(context.Background(), key, cur); err != nil {
		t.Fatal(err)
	}
	if cur.Status.SchedulingStatus != wantStartFailedReason {
		t.Errorf("status after a failed restart = %q, want %s", cur.Status.SchedulingStatus, wantStartFailedReason)
	}
	// One Event per failed attempt: the restart's RestartFailed, carrying
	// the engine log tail, and no StartFailed next to it.
	events := drainEvents(f.rec)
	restartFailed := eventsWithReason(events, EventReasonRestartFailed)
	if len(restartFailed) != 1 || !strings.Contains(restartFailed[0], "fatal: out of memory") {
		t.Errorf("%s events = %v, want one carrying the engine log tail", EventReasonRestartFailed, restartFailed)
	}
	if got := eventsWithReason(events, wantStartFailedReason); len(got) != 0 {
		t.Errorf("a failed restart also emitted %v", got)
	}
}

// TestWithEventLogTail_Bounded checks that an Event carries at most
// eventLogTailBytes of the engine log, from a line boundary, ending with the
// last line, where the engine prints the fatal cause.
func TestWithEventLogTail_Bounded(t *testing.T) {
	var b strings.Builder
	for i := range 200 {
		fmt.Fprintf(&b, "load line %d %s\n", i, strings.Repeat("x", 40))
	}
	b.WriteString("fatal: out of memory")
	msg := withEventLogTail("Start failed: boom", "/store/x.log", b.String())

	head := "Start failed: boom. Last lines of /store/x.log:\n...\n"
	if !strings.HasPrefix(msg, head) {
		t.Fatalf("message starts %q, want %q", msg[:min(len(msg), len(head))], head)
	}
	tail := strings.TrimPrefix(msg, head)
	if len(tail) > eventLogTailBytes || !strings.HasPrefix(tail, "load line ") ||
		!strings.HasSuffix(tail, "fatal: out of memory") {
		t.Errorf("bounded tail (%d bytes) = %q, want at most %d bytes from a line start to the last line",
			len(tail), tail, eventLogTailBytes)
	}
	if got := withEventLogTail("Start failed: boom", "", ""); got != "Start failed: boom" {
		t.Errorf("message with no tail = %q, want it unchanged", got)
	}
}

// TestEnsureProcess_FailedStartDuringShutdownIsNotRetried checks that a start
// cut short by the agent stopping is neither reported nor retried: the next
// agent process starts the service.
func TestEnsureProcess_FailedStartDuringShutdownIsNotRetried(t *testing.T) {
	a, ex, rec, _ := startFailureFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	ex.onStart = cancel
	ex.startErr = errors.New("failed to ensure model: context canceled")

	_ = a.ensureProcess(ctx, freshISVC(t, a))
	if got := eventsWithReason(drainEvents(rec), wantStartFailedReason); len(got) != 0 {
		t.Errorf("events = %v, want none for a start cut short by shutdown", got)
	}
	if restartPending(a, "default/svc") {
		t.Errorf("a start cut short by shutdown was queued for a retry")
	}
}
