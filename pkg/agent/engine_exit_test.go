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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These tests cover #2405: when an engine process exits, the agent used to do
// nothing until the next health probe (every 30s) withdrew its endpoint, and
// restarted it only after three failed probes. The exit status was never
// logged and the restart truncated the crashed run's log.

// wantEngineExitedReason is the Warning Event reason for an engine that exits.
const wantEngineExitedReason = "EngineExited"

// exitWaitTimeout bounds how long a test waits for the agent to act on an
// exit. The reaper sees the exit at once, so a working handler is far inside.
const exitWaitTimeout = 3 * time.Second

// fakeServingEngine is a llama-server stand-in that answers --help (the
// load-mode probe), logs a line naming its PID and then sleeps, so it runs
// until it is killed. healthyPort answers its /health.
const fakeServingEngine = `case "$1" in --help) echo "usage: --mlock"; exit 0;; esac
echo "engine run $$ loaded the model"
exec sleep 60
`

// engineExitFixture is an agent with the real llama-server executor running
// a fake engine for InferenceService "crash", with a fake client and recorder.
type engineExitFixture struct {
	agent   *MetalAgent
	client  client.Client
	rec     *record.FakeRecorder
	key     string
	logPath string
}

func newEngineExitFixture(t *testing.T) *engineExitFixture {
	t.Helper()
	const name = "crash"
	model := ggufModel(t, name)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: name},
	}
	scheme := newTestScheme()
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{}).
		WithObjects(model, isvc).Build()
	rec := record.NewFakeRecorder(64)
	store := t.TempDir()
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient:         c,
		Namespace:         "default",
		ModelStorePath:    store,
		MemoryProvider:    &mockMemoryProvider{totalBytes: 128 << 30, availableBytes: 120 << 30},
		AllowedModelRoots: []string{filepath.Dir(model.Spec.Source)},
		EventRecorder:     rec,
	})
	a.registry = NewServiceRegistry(c, "10.0.0.1", newNopLogger(), "")
	e := NewMetalExecutor(fakeLlamaServer(t, fakeServingEngine), store, newNopLogger())
	e.SetPort(healthyPort(t))
	e.SetStartupTimeout(10 * time.Second)
	a.executors[runtimeLlamaServer] = e
	a.executors[runtimeLlamaCPP] = e

	f := &engineExitFixture{
		agent: a, client: c, rec: rec, key: "default/" + name,
		logPath: filepath.Join(store, "llama-server-default-"+name+".log"),
	}
	t.Cleanup(func() {
		// Stop whatever engine the test left running, through the agent.
		_ = a.deleteProcess(context.Background(), f.key)
	})
	return f
}

// start runs one start of the service and returns the tracked engine.
func (f *engineExitFixture) start(t *testing.T, ctx context.Context) *ManagedProcess {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{}
	if err := f.client.Get(ctx, keyOfDefault("crash"), isvc); err != nil {
		t.Fatal(err)
	}
	if err := f.agent.ensureProcess(ctx, isvc); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	p := f.tracked()
	if p == nil {
		t.Fatal("no engine tracked after the start")
	}
	// healthyPort, not the engine, answers /health, so the start can return
	// before the script has logged its line.
	waitFor(t, "the engine to log its start", func() bool {
		data, _ := os.ReadFile(f.logPath)
		return strings.Contains(string(data), fmt.Sprintf("engine run %d", p.PID))
	})
	return p
}

func (f *engineExitFixture) tracked() *ManagedProcess {
	f.agent.mu.RLock()
	defer f.agent.mu.RUnlock()
	return f.agent.processes[f.key]
}

// ready reports whether the service's EndpointSlice advertises Ready.
func (f *engineExitFixture) ready(t *testing.T) bool {
	t.Helper()
	ready, _ := endpointReady(t, f.client, "crash")
	return ready
}

// keyOfDefault returns the namespaced name of name in "default".
func keyOfDefault(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: "default", Name: name}
}

// waitFor polls cond until it holds or exitWaitTimeout passes, then fails
// the test naming what it waited for.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if !eventually(cond) {
		t.Fatalf("timed out after %s waiting for %s", exitWaitTimeout, what)
	}
}

// eventually polls cond until it holds or exitWaitTimeout passes.
func eventually(cond func() bool) bool {
	deadline := time.Now().Add(exitWaitTimeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEngineExit_WithdrawsAndRestartsAtOnce is the #2405 bug: an engine that
// exits must lose its endpoint at once (not at the next 30s probe), be
// restarted without waiting for the probe threshold, have its exit status and
// the log's path (not its content) reported in a Warning Event, and keep the crashed run's log as
// <engine>.log.1 when the restart opens a new one.
func TestEngineExit_WithdrawsAndRestartsAtOnce(t *testing.T) {
	f := newEngineExitFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := f.start(t, ctx)
	if !f.ready(t) {
		t.Fatal("setup: the endpoint is not Ready after the start")
	}
	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the fake engine: %v", err)
	}

	withdrawn := eventually(func() bool { return !f.ready(t) })
	queued := eventually(func() bool { return restartPending(f.agent, f.key) })
	var exited []string
	eventually(func() bool {
		exited = append(exited, eventsWithReason(drainEvents(f.rec), wantEngineExitedReason)...)
		return len(exited) > 0
	})
	if !withdrawn {
		t.Errorf("the endpoint still advertises Ready %s after the engine exited", exitWaitTimeout)
	}
	if !queued {
		t.Errorf("no restart queued %s after the engine exited", exitWaitTimeout)
	}
	if len(exited) != 1 {
		t.Errorf("%s events = %v, want one", wantEngineExitedReason, exited)
	} else {
		for _, want := range []string{"Warning", "signal: killed", f.logPath, f.logPath + ".1"} {
			if !strings.Contains(exited[0], want) {
				t.Errorf("%s event %q does not carry %q", wantEngineExitedReason, exited[0], want)
			}
		}
		if strings.Contains(exited[0], "engine run") {
			t.Errorf("%s event %q carries engine log content", wantEngineExitedReason, exited[0])
		}
	}
	if !withdrawn || !queued {
		t.FailNow()
	}

	// The event loop runs the queued restart.
	f.agent.runDueRestarts(ctx)
	second := f.tracked()
	if second == nil || second.PID == first.PID {
		t.Fatalf("tracked engine after the restart = %+v, want a new one", second)
	}
	waitFor(t, "the new engine to log its start", func() bool {
		data, _ := os.ReadFile(f.logPath)
		return strings.Contains(string(data), fmt.Sprintf("engine run %d", second.PID))
	})
	if !f.ready(t) {
		t.Errorf("the endpoint is not Ready after the restart")
	}
	prev, err := os.ReadFile(f.logPath + ".1")
	if err != nil {
		t.Fatalf("the crashed run's log was not kept as %s.1: %v", filepath.Base(f.logPath), err)
	}
	if !strings.Contains(string(prev), fmt.Sprintf("engine run %d", first.PID)) {
		t.Errorf("%s.1 = %q, want the crashed run's output", filepath.Base(f.logPath), prev)
	}
	cur, _ := os.ReadFile(f.logPath)
	if strings.Contains(string(cur), fmt.Sprintf("engine run %d", first.PID)) {
		t.Errorf("the new run's log still holds the crashed run's output: %q", cur)
	}
}

// TestEngineExit_StopByTheAgentIsNotACrash checks that an engine the agent
// stops itself (a delete, a scale to zero, a spec change) is not reported as
// exited or restarted.
func TestEngineExit_StopByTheAgentIsNotACrash(t *testing.T) {
	f := newEngineExitFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f.start(t, ctx)
	if err := f.agent.deleteProcess(ctx, f.key); err != nil {
		t.Fatalf("deleteProcess: %v", err)
	}
	// Give a handler time to (wrongly) act on the exit the stop caused.
	time.Sleep(300 * time.Millisecond)
	if got := eventsWithReason(drainEvents(f.rec), wantEngineExitedReason); len(got) != 0 {
		t.Errorf("events = %v, want no %s for an engine the agent stopped", got, wantEngineExitedReason)
	}
	if restartPending(f.agent, f.key) {
		t.Errorf("a restart was queued for an engine the agent stopped")
	}
}

// TestOpenEngineLog_KeepsOnePreviousGeneration checks the rotation at the
// file level: the previous log moves to <log>.1 (replacing an older .1), the
// new log starts empty with mode 0600, and a symlink planted at <log>.1 is
// replaced, never written through.
func TestOpenEngineLog_KeepsOnePreviousGeneration(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "llama-server-ns-svc.log")
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("not the agent's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 3; run++ {
		f, err := openEngineLog("llama-server", logPath)
		if err != nil {
			t.Fatalf("run %d: openEngineLog: %v", run, err)
		}
		if info, err := f.Stat(); err != nil || info.Size() != 0 {
			t.Fatalf("run %d: new log is not empty (%v, %v)", run, info, err)
		}
		if _, err := fmt.Fprintf(f, "run %d\n", run); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	for path, want := range map[string]string{logPath: "run 3\n", logPath + ".1": "run 2\n", victim: "not the agent's\n"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %q", filepath.Base(path), got, err, want)
		}
	}
	for _, path := range []string{logPath, logPath + ".1"} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %s, want a regular file with mode 0600", filepath.Base(path), info.Mode())
		}
	}
}

// TestOpenEngineLog_SymlinkedLogStillRefused checks that rotation does not
// move a symlink planted at the log path out of the way: the start still
// fails naming it, and nothing is written through it.
func TestOpenEngineLog_SymlinkedLogStillRefused(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "llama-server-ns-svc.log")
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("not the agent's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, logPath); err != nil {
		t.Fatal(err)
	}
	if f, err := openEngineLog("llama-server", logPath); err == nil {
		_ = f.Close()
		t.Fatal("openEngineLog opened a log with a symlink at the log path; it must refuse it")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error = %v, want it to name the symlink", err)
	}
	if info, err := os.Lstat(logPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink at the log path was moved or replaced (%v, %v)", info, err)
	}
	if _, err := os.Lstat(logPath + ".1"); !os.IsNotExist(err) {
		t.Errorf("a symlinked log was rotated to .1: %v", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "not the agent's\n" {
		t.Errorf("the symlink's target was written: %q", got)
	}
}

// TestSpawningExecutors_HandOverReaperAndLog checks that every executor that
// spawns one engine per service hands the agent the engine's reaper and log
// path, which is what lets the agent act on an exit at once.
func TestSpawningExecutors_HandOverReaperAndLog(t *testing.T) {
	type spawner interface {
		ProcessExecutor
		trackedChild(pid int) *childExit
	}
	cases := []struct {
		name  string
		build func(t *testing.T, port int) (spawner, ExecutorConfig, string)
	}{
		{"llama-server", func(t *testing.T, port int) (spawner, ExecutorConfig, string) {
			e, cfg, logPath := childExitFixture(t, fakeLlamaServer(t, "exec sleep 60\n"))
			e.SetPort(port)
			e.SetStartupTimeout(10 * time.Second)
			return e, cfg, logPath
		}},
		{"mlx-server", func(t *testing.T, port int) (spawner, ExecutorConfig, string) {
			e, cfg, logPath := mlxServerFixture(t, fakeMLXServer(t, "exec sleep 60\n"), port)
			e.SetStartupTimeout(10 * time.Second)
			return e, cfg, logPath
		}},
		{"vllm-swift", func(t *testing.T, port int) (spawner, ExecutorConfig, string) {
			e, cfg, logPath := vllmSwiftFixture(t, fakeVLLMSwift(t, "exec sleep 60\n"))
			e.allocatePort = func() (int, error) { return port, nil }
			e.SetStartupTimeout(10 * time.Second)
			return e, cfg, logPath
		}},
		{"tensorfold", func(t *testing.T, port int) (spawner, ExecutorConfig, string) {
			e, cfg, logPath := tensorFoldFixture(t, fakeTensorFold(t, "exec sleep 60\n"))
			e.allocatePort = func() (int, error) { return port, nil }
			e.SetStartupTimeout(10 * time.Second)
			return e, cfg, logPath
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, cfg, logPath := tc.build(t, healthyPort(t))
			proc, err := e.StartProcess(context.Background(), cfg)
			if err != nil {
				t.Fatalf("StartProcess: %v", err)
			}
			t.Cleanup(func() { _ = e.StopProcess(proc.PID) })
			if proc.exit == nil || proc.exit != e.trackedChild(proc.PID) {
				t.Errorf("process exit = %p, want the executor's reaper %p", proc.exit, e.trackedChild(proc.PID))
			}
			if proc.logPath != logPath {
				t.Errorf("process logPath = %q, want %q", proc.logPath, logPath)
			}
		})
	}
}

// TestEngineExit_WaitsForAStartInFlight checks an exit that arrives while a
// reconcile holds the service's start guard: the endpoint is withdrawn at
// once, and the restart follows as soon as the guard is released instead of
// being dropped until the probe threshold.
func TestEngineExit_WaitsForAStartInFlight(t *testing.T) {
	f := newRestartFixture(t, "exit-guarded")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proc := f.tracked()
	proc.exit = &childExit{done: make(chan struct{})}
	proc.logPath = writeEngineLog(t, t.TempDir(), "ggml_metal_init: error: out of device memory\n")
	f.agent.registerEndpoint(ctx, f.key, proc)
	f.agent.mu.Lock()
	f.agent.starting[f.key] = true
	f.agent.mu.Unlock()

	f.agent.watchEngineExit(ctx, f.key, proc)
	close(proc.exit.done)

	if !eventually(func() bool { ready, _ := endpointReady(t, f.client, f.isvc.Name); return !ready }) {
		t.Error("the endpoint was not withdrawn while the start guard was held")
	}
	time.Sleep(2 * restartGuardRetry)
	if f.tracked() != proc || restartPending(f.agent, f.key) {
		t.Fatal("the engine was restarted while another start held its key")
	}
	// The start holding the key registers the endpoint Ready as it finishes,
	// after the withdrawal, as reconcileProcess does after StartProcess.
	if err := f.agent.registry.RegisterEndpoint(ctx, f.isvc, proc.Port); err != nil {
		t.Fatalf("register the endpoint: %v", err)
	}
	if ready, _ := endpointReady(t, f.client, f.isvc.Name); !ready {
		t.Fatal("setup: the start's registration did not make the endpoint Ready")
	}

	f.agent.mu.Lock()
	delete(f.agent.starting, f.key)
	f.agent.mu.Unlock()
	if !eventually(func() bool { return f.tracked() == nil && restartPending(f.agent, f.key) }) {
		t.Fatalf("no restart queued %s after the start guard was released", exitWaitTimeout)
	}
	if ready, _ := endpointReady(t, f.client, f.isvc.Name); ready {
		t.Error("the dead engine's endpoint was left Ready by the start that held its key")
	}
	var exited []string
	eventually(func() bool {
		exited = append(exited, eventsWithReason(drainEvents(f.rec), wantEngineExitedReason)...)
		return len(exited) > 0
	})
	if len(exited) != 1 || !strings.Contains(exited[0], "exit status 0") ||
		!strings.Contains(exited[0], proc.logPath) || strings.Contains(exited[0], "out of device memory") {
		t.Errorf("%s events = %v, want one with the exit status and the log's path, not its content",
			wantEngineExitedReason, exited)
	}
}
