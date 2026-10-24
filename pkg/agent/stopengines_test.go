package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func stopEnginesCfg(dir string) StopEnginesConfig {
	return StopEnginesConfig{StateDir: dir, Logger: zap.NewNop().Sugar()}
}

// startSleeper starts a `sleep` in its own process group, reaps it in the
// background, and kills its group on cleanup so a failing test leaves nothing.
// A sleeper already reaped is not signalled: it was alone in its group, so the
// group is gone and its ID may have been reused.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
			return
		case <-time.After(100 * time.Millisecond): // a test that saw ESRCH: let the reaper finish
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	})
	return cmd
}

func TestRunStopEngines_StopsGroupAndRemovesRecord(t *testing.T) {
	dir := t.TempDir()
	cmd := startSleeper(t)
	in := newProcessInspector()
	store := newEngineRecordStore(filepath.Join(dir, "engines"), "v", in, nil)
	exes, _ := in.Executables(cmd.Process.Pid)
	exe := exes[0]
	if err := store.WriteProvisional("default", "s", runtimeLlamaServer,
		SpawnedProcess{PID: cmd.Process.Pid, PGID: cmd.Process.Pid, Executable: exe}); err != nil {
		t.Fatal(err)
	}
	if err := RunStopEngines(context.Background(), stopEnginesCfg(dir)); err != nil {
		t.Fatalf("RunStopEngines: %v", err)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("engine still alive: %v", err)
	}
	if recs, _ := store.List(); len(recs) != 0 {
		t.Fatalf("records left: %+v", recs)
	}
}

func TestRunStopEngines_IdentityMismatchIsNotSignalled(t *testing.T) {
	dir := t.TempDir()
	inspect := fakeInspector{tokens: map[int]string{4242: "rec"}}
	store := newEngineRecordStore(filepath.Join(dir, "engines"), "v", inspect, nil)
	sp := SpawnedProcess{PID: 4242, PGID: 4242}
	if err := store.WriteProvisional("default", "s", runtimeLlamaServer, sp); err != nil {
		t.Fatal(err)
	}
	var sent []syscall.Signal
	orig := killFn
	killFn = func(pid int, sig syscall.Signal) error { sent = append(sent, sig); return nil }
	t.Cleanup(func() { killFn = orig })
	// The real inspector will not find token "rec" for PID 4242 (or a live 4242 at all).
	_ = RunStopEngines(context.Background(), stopEnginesCfg(dir))
	for _, s := range sent {
		if s != 0 {
			t.Fatalf("signal %v sent to a PID whose identity did not match", s)
		}
	}
}

func TestRunStopEngines_PGIDNotPIDIsLeftAloneAndReported(t *testing.T) {
	dir := t.TempDir()
	cmd := startSleeper(t)
	in := newProcessInspector()
	store := newEngineRecordStore(filepath.Join(dir, "engines"), "v", in, nil)
	exes, _ := in.Executables(cmd.Process.Pid)
	if err := store.WriteProvisional("default", "s", runtimeLlamaServer,
		SpawnedProcess{PID: cmd.Process.Pid, PGID: cmd.Process.Pid + 1, Executable: exes[0]}); err != nil {
		t.Fatal(err)
	}
	var sent []syscall.Signal
	orig := killFn
	killFn = func(pid int, sig syscall.Signal) error { sent = append(sent, sig); return orig(pid, 0) }
	t.Cleanup(func() { killFn = orig })
	if err := RunStopEngines(context.Background(), stopEnginesCfg(dir)); err == nil {
		t.Fatal("expected an error for a record whose PGID is not its PID")
	}
	for _, s := range sent {
		if s != 0 {
			t.Fatalf("signal %v sent for a record with PGID != PID", s)
		}
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("engine should still be alive: %v", err)
	}
	if recs, _ := store.List(); len(recs) != 1 {
		t.Fatalf("record must be kept, got %+v", recs)
	}
}

// The start token proves the process is the one the agent forked, so an
// executable that no longer matches the record (a Homebrew relink, a wrapper
// that exec'd the engine) does not keep it from being stopped.
func TestRunStopEngines_ExecutableMismatchIsStopped(t *testing.T) {
	dir := t.TempDir()
	cmd := startSleeper(t)
	in := newProcessInspector()
	store := newEngineRecordStore(filepath.Join(dir, "engines"), "v", in, nil)
	if err := store.WriteProvisional("default", "s", runtimeLlamaServer, SpawnedProcess{
		PID: cmd.Process.Pid, PGID: cmd.Process.Pid, Executable: "/not/the/engine", LaunchPath: "/not/the/link",
	}); err != nil {
		t.Fatal(err)
	}
	if err := RunStopEngines(context.Background(), stopEnginesCfg(dir)); err != nil {
		t.Fatalf("RunStopEngines: %v", err)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("engine still alive: %v", err)
	}
	if recs, _ := store.List(); len(recs) != 0 {
		t.Fatalf("records left: %+v", recs)
	}
}

func TestRunStopEngines_DeadPIDRecordIsRemovedWithoutError(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	store := newEngineRecordStore(filepath.Join(dir, "engines"), "v", fakeInspector{tokens: map[int]string{pid: "t"}}, nil)
	if err := store.WriteProvisional("default", "s", runtimeLlamaServer,
		SpawnedProcess{PID: pid, PGID: pid, Executable: "/bin/true"}); err != nil {
		t.Fatal(err)
	}
	orig := killFn
	killFn = func(p int, sig syscall.Signal) error {
		if sig != 0 {
			t.Errorf("signal %v sent to a dead PID", sig)
		}
		return syscall.ESRCH
	}
	t.Cleanup(func() { killFn = orig })
	if err := RunStopEngines(context.Background(), stopEnginesCfg(dir)); err != nil {
		t.Fatalf("RunStopEngines: %v", err)
	}
	if recs, _ := store.List(); len(recs) != 0 {
		t.Fatalf("records left: %+v", recs)
	}
}

func TestRunStopEngines_OMLXRecordIsRemovedWithoutSignal(t *testing.T) {
	dir := t.TempDir()
	inspect := fakeInspector{tokens: map[int]string{4242: "t"}}
	store := newEngineRecordStore(filepath.Join(dir, "engines"), "v", inspect, nil)
	if err := store.WriteProvisional("default", "s", runtimeOMLX, SpawnedProcess{PID: 4242, PGID: 4242}); err != nil {
		t.Fatal(err)
	}
	orig := killFn
	killFn = func(int, syscall.Signal) error { t.Error("killFn called for an oMLX record"); return nil }
	t.Cleanup(func() { killFn = orig })
	if err := RunStopEngines(context.Background(), stopEnginesCfg(dir)); err != nil {
		t.Fatalf("RunStopEngines: %v", err)
	}
	if recs, _ := store.List(); len(recs) != 0 {
		t.Fatalf("records left: %+v", recs)
	}
}

func TestRunStopEngines_OllamaRecordUnloadsModel(t *testing.T) {
	dir := t.TempDir()
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		body.Store(string(buf[:n]))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(portStr)
	store := newEngineRecordStore(filepath.Join(dir, "engines"), "v", newProcessInspector(), nil)
	if err := store.WriteProvisional("default", "s", runtimeOllama, SpawnedProcess{Port: p}); err != nil {
		t.Fatal(err)
	}
	recs, _ := store.List()
	if len(recs) != 1 {
		t.Fatalf("setup: %+v", recs)
	}
	rec := recs[0]
	rec.ModelID = "llama3.2:3b"
	if err := store.write(rec); err != nil {
		t.Fatal(err)
	}
	cfg := stopEnginesCfg(dir)
	cfg.OllamaPort = p
	if err := RunStopEngines(context.Background(), cfg); err != nil {
		t.Fatalf("RunStopEngines: %v", err)
	}
	got, _ := body.Load().(string)
	if !strings.Contains(got, `"llama3.2:3b"`) || !strings.Contains(got, `"keep_alive":0`) {
		t.Fatalf("unexpected unload body %q", got)
	}
	if recs, _ := store.List(); len(recs) != 0 {
		t.Fatalf("records left: %+v", recs)
	}
}

func TestRunStopEngines_ReusedPIDIsStoppedNotReported(t *testing.T) {
	dir := t.TempDir()
	cmd := startSleeper(t)
	pid := cmd.Process.Pid
	// The record's token and executable both belong to a dead engine.
	inspect := fakeInspector{tokens: map[int]string{pid: "dead-engine"}}
	store := newEngineRecordStore(filepath.Join(dir, "engines"), "v", inspect, nil)
	sp := SpawnedProcess{PID: pid, PGID: pid, Executable: "/not/the/engine"}
	if err := store.WriteProvisional("default", "s", runtimeLlamaServer, sp); err != nil {
		t.Fatal(err)
	}
	var sent []syscall.Signal
	orig := killFn
	killFn = func(p int, sig syscall.Signal) error { sent = append(sent, sig); return orig(p, 0) }
	t.Cleanup(func() { killFn = orig })
	if err := RunStopEngines(context.Background(), stopEnginesCfg(dir)); err != nil {
		t.Fatalf("RunStopEngines: %v", err)
	}
	for _, s := range sent {
		if s != 0 {
			t.Fatalf("signal %v sent to a PID another program holds", s)
		}
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the unrelated process must be untouched: %v", err)
	}
	if recs, _ := store.List(); len(recs) != 0 {
		t.Fatalf("record must be removed: %+v", recs)
	}
}

// ollamaRecord writes an Ollama service's record (no PID) for modelID.
func ollamaRecord(t *testing.T, dir, modelID string) *engineRecordStore {
	t.Helper()
	store := newEngineRecordStore(filepath.Join(dir, "engines"), "v", newProcessInspector(), nil)
	if err := store.WriteProvisional("default", "s", runtimeOllama, SpawnedProcess{}); err != nil {
		t.Fatal(err)
	}
	recs, _ := store.List()
	if len(recs) != 1 {
		t.Fatalf("setup: %+v", recs)
	}
	rec := recs[0]
	rec.ModelID = modelID
	if err := store.write(rec); err != nil {
		t.Fatal(err)
	}
	return store
}

// With no logger configured, the Ollama executor still gets one: its success
// log line must not dereference a nil logger.
func TestRunStopEngines_NilLoggerReachesOllamaExecutor(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(portStr)
	store := ollamaRecord(t, dir, "llama3.2:3b")
	if err := RunStopEngines(context.Background(), StopEnginesConfig{StateDir: dir, OllamaPort: p}); err != nil {
		t.Fatalf("RunStopEngines: %v", err)
	}
	if recs, _ := store.List(); len(recs) != 0 {
		t.Fatalf("records left: %+v", recs)
	}
}

// A daemon that refuses the connection is not running, so no model is loaded:
// the record goes and nothing is reported.
func TestRunStopEngines_OllamaNotRunningRemovesRecord(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // nothing listens on p now
	store := ollamaRecord(t, dir, "llama3.2:3b")
	cfg := stopEnginesCfg(dir)
	cfg.OllamaPort = p
	if err := RunStopEngines(context.Background(), cfg); err != nil {
		t.Fatalf("RunStopEngines: %v", err)
	}
	if recs, _ := store.List(); len(recs) != 0 {
		t.Fatalf("records left: %+v", recs)
	}
}

// An Ollama daemon that answers with an error may still hold the model: the
// record stays and the failure is reported.
func TestRunStopEngines_OllamaErrorKeepsRecord(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(portStr)
	store := ollamaRecord(t, dir, "llama3.2:3b")
	cfg := stopEnginesCfg(dir)
	cfg.OllamaPort = p
	if err := RunStopEngines(context.Background(), cfg); err == nil {
		t.Fatal("expected an error when Ollama fails the unload")
	}
	if recs, _ := store.List(); len(recs) != 1 {
		t.Fatalf("record must be kept, got %+v", recs)
	}
}

// An API server that never answers delays each engine's stop by at most
// endpointWithdrawTimeout, not forever.
func TestRunStopEngines_EndpointWithdrawalIsBounded(t *testing.T) {
	orig := endpointWithdrawTimeout
	endpointWithdrawTimeout = 50 * time.Millisecond
	t.Cleanup(func() { endpointWithdrawTimeout = orig })
	dir := t.TempDir()
	store := newEngineRecordStore(filepath.Join(dir, "engines"), "v",
		fakeInspector{tokens: map[int]string{4242: "t"}}, nil)
	// An oMLX record: stopping it sends no signal, so only the withdrawal waits.
	if err := store.WriteProvisional("default", "s", runtimeOMLX, SpawnedProcess{PID: 4242, PGID: 4242}); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	hanging := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(
			ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption,
		) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	// Cancelled at cleanup, so a withdrawal that ignores the timeout does not
	// leave its goroutine blocked after the test fails.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := stopEnginesCfg(dir)
	cfg.K8sClient = hanging
	done := make(chan error, 1)
	go func() { done <- RunStopEngines(ctx, cfg) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunStopEngines: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunStopEngines still waiting on an API server that never answers")
	}
	if recs, _ := store.List(); len(recs) != 0 {
		t.Fatalf("records left: %+v", recs)
	}
}
