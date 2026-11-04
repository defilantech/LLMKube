//go:build darwin

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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// startNonChild starts `sleep 60` detached through a shell that exits at
// once, so the sleep is reparented and is not this process's child (as an
// engine adopted from a previous agent process is not the agent's). It is
// killed when the test ends.
func startNonChild(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("/bin/sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $!").Output()
	if err != nil {
		t.Fatalf("start detached sleep: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse detached PID %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("setup: PID %d is a child of the test process (wait4: %v)", pid, err)
	}
	return pid
}

// closedWithin reports whether done closes within d.
func closedWithin(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestWatchProcessExit_NonChild checks the kqueue watch on a process the
// test process cannot wait on: it does not fire while the process runs and
// fires at once when it exits.
func TestWatchProcessExit_NonChild(t *testing.T) {
	pid := startNonChild(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exit, err := watchProcessExit(ctx, pid)
	if err != nil {
		t.Fatalf("watchProcessExit(%d): %v", pid, err)
	}
	if closedWithin(exit.done, 200*time.Millisecond) {
		t.Fatal("the watch fired for a process that is still running")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the detached sleep: %v", err)
	}
	if !closedWithin(exit.done, 2*time.Second) {
		t.Fatal("the watch did not fire within 2s of the process exiting")
	}
	if got := exit.status(); !strings.Contains(got, "exit status unknown") {
		t.Errorf("status = %q, want it to say the exit status is unknown", got)
	}
}

// TestWatchProcessExit_GoneAndCancelled checks that a PID with no process is
// reported as gone (ESRCH), and that a watch whose context ended does not
// fire later.
func TestWatchProcessExit_GoneAndCancelled(t *testing.T) {
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := watchProcessExit(context.Background(), cmd.Process.Pid); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("watchProcessExit of a reaped PID: err = %v, want ESRCH", err)
	}

	pid := startNonChild(t)
	ctx, cancel := context.WithCancel(context.Background())
	exit, err := watchProcessExit(ctx, pid)
	if err != nil {
		t.Fatalf("watchProcessExit(%d): %v", pid, err)
	}
	cancel()
	time.Sleep(processExitPoll + 200*time.Millisecond) // the watch sees the cancel
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if closedWithin(exit.done, 500*time.Millisecond) {
		t.Error("a watch whose context ended still fired")
	}
}

// TestEngineExit_AdoptedEngine is #2405 for an engine adopted from a previous
// agent process, which has no reaper in this one: its exit is watched through
// kqueue, so the endpoint is withdrawn and the restart queued without waiting
// for the health probe threshold.
func TestEngineExit_AdoptedEngine(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a fake engine binary")
	}
	bin := filepath.Join(t.TempDir(), "llama-server")
	buildFakeEngine(t, bin)
	stateDir := t.TempDir()
	port := healthyPort(t)
	proc := startRecordedEngine(t, bin, stateDir, port)

	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "real", Namespace: "default"}}
	scheme := newTestScheme()
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isvc).Build()
	rec := record.NewFakeRecorder(16)
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient: c, Namespace: "default", EventRecorder: rec,
		MemoryProvider: &mockMemoryProvider{totalBytes: 128 << 30}, StateDir: stateDir,
	})
	a.registry = NewServiceRegistry(c, "10.0.0.1", newNopLogger(), "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !a.adoptEngines(ctx)["default/real"] {
		t.Fatal("setup: the engine was not adopted")
	}
	a.registerEndpoint(ctx, "default/real", a.processes["default/real"])
	a.watchAdoptedExits(ctx)

	if err := syscall.Kill(proc.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the engine: %v", err)
	}
	if !eventually(func() bool { return restartPending(a, "default/real") }) {
		t.Fatalf("no restart queued %s after the adopted engine exited", exitWaitTimeout)
	}
	if ready, _ := endpointReady(t, c, "real"); ready {
		t.Error("the adopted engine's endpoint still advertises Ready after it exited")
	}
	var exited []string
	eventually(func() bool {
		exited = append(exited, eventsWithReason(drainEvents(rec), wantEngineExitedReason)...)
		return len(exited) > 0
	})
	if len(exited) != 1 || !strings.Contains(exited[0], "exit status unknown") {
		t.Errorf("%s events = %v, want one saying the exit status is unknown", wantEngineExitedReason, exited)
	}
}
