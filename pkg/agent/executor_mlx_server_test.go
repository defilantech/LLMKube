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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const mlxTestModelStore = "/models"

func TestNewMLXServerExecutor(t *testing.T) {
	executor := NewMLXServerExecutor("/opt/homebrew/bin/mlx-server", mlxTestModelStore, 8080, newNopLogger())

	if executor.bin != "/opt/homebrew/bin/mlx-server" {
		t.Errorf("bin = %q, want %q", executor.bin, "/opt/homebrew/bin/mlx-server")
	}
	if executor.modelStorePath != mlxTestModelStore {
		t.Errorf("modelStorePath = %q, want %q", executor.modelStorePath, mlxTestModelStore)
	}
	if executor.port != 8080 {
		t.Errorf("port = %d, want 8080", executor.port)
	}
	if executor.startupTimeout != DefaultMLXServerStartupTimeout {
		t.Errorf("default startupTimeout = %v, want %v",
			executor.startupTimeout, DefaultMLXServerStartupTimeout)
	}
}

func TestMLXServerSetStartupTimeout(t *testing.T) {
	executor := NewMLXServerExecutor("/bin/mlx-server", mlxTestModelStore, 8080, newNopLogger())

	executor.SetStartupTimeout(200 * time.Second)
	if executor.startupTimeout != 200*time.Second {
		t.Errorf("after Set(200s) = %v, want 200s", executor.startupTimeout)
	}

	// Non-positive values coerce back to default.
	for _, d := range []time.Duration{0, -5 * time.Second} {
		executor.SetStartupTimeout(d)
		if executor.startupTimeout != DefaultMLXServerStartupTimeout {
			t.Errorf("after Set(%v) = %v, want default %v",
				d, executor.startupTimeout, DefaultMLXServerStartupTimeout)
		}
	}
}

func TestBuildMLXServerArgs_Defaults(t *testing.T) {
	args := buildMLXServerArgs("/models/Qwen3.6-35B-A3B-8bit", 8080, ExecutorConfig{})

	want := map[string]string{
		"--model": "/models/Qwen3.6-35B-A3B-8bit",
		"--host":  "127.0.0.1",
		"--port":  "8080",
	}
	for flag, expected := range want {
		if got := flagValue(args, flag); got != expected {
			t.Errorf("%s = %q, want %q (full args: %v)", flag, got, expected, args)
		}
	}

	// Defaults must not inject slot concurrency.
	if hasFlag(args, "--max-slots") {
		t.Errorf("--max-slots must be omitted by default (full args: %v)", args)
	}
}

func TestBuildMLXServerArgs_ParallelSlots(t *testing.T) {
	args := buildMLXServerArgs("/m", 8080, ExecutorConfig{ParallelSlots: 4})
	if got := flagValue(args, "--max-slots"); got != "4" {
		t.Errorf("--max-slots = %q, want %q (full args: %v)", got, "4", args)
	}

	// 0 and 1 omit the flag.
	for _, n := range []int{0, 1} {
		a := buildMLXServerArgs("/m", 8080, ExecutorConfig{ParallelSlots: n})
		if hasFlag(a, "--max-slots") {
			t.Errorf("--max-slots must be omitted for ParallelSlots=%d (full args: %v)", n, a)
		}
	}
}

func TestBuildMLXServerArgs_ExtraArgsAppendedLast(t *testing.T) {
	args := buildMLXServerArgs("/m", 8080, ExecutorConfig{
		ExtraArgs: []string{"--tool-call-format", "xml_function", "--reasoning", "prefilled"},
	})
	if len(args) < 4 {
		t.Fatalf("args too short: %v", args)
	}
	tail := args[len(args)-4:]
	want := []string{"--tool-call-format", "xml_function", "--reasoning", "prefilled"}
	for i, w := range want {
		if tail[i] != w {
			t.Errorf("tail[%d] = %q, want %q (full args: %v)", i, tail[i], w, args)
		}
	}
}

func TestMLXServerProcessLogPath(t *testing.T) {
	executor := NewMLXServerExecutor("/bin/mlx-server", "/var/lib/llmkube", 8080, newNopLogger())

	got := executor.processLogPath("default", "qwen36-opencode")
	want := filepath.Join("/var/lib/llmkube", "mlx-server-default-qwen36-opencode.log")
	if got != want {
		t.Errorf("processLogPath = %q, want %q", got, want)
	}

	if other := executor.processLogPath("prod", "qwen36-opencode"); other == got {
		t.Errorf("processLogPath collided across namespaces: %q == %q", other, got)
	}
}

func TestMLXServerStopProcess_InvalidPID(t *testing.T) {
	executor := NewMLXServerExecutor("/bin/mlx-server", mlxTestModelStore, 8080, newNopLogger())

	if err := executor.StopProcess(-99999); err == nil {
		t.Error("StopProcess with invalid PID should return error")
	}
}

// fakeMLXServer writes an executable shell script standing in for mlx-server.
func fakeMLXServer(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mlx-server")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write fake mlx-server: %v", err)
	}
	return path
}

// mlxServerFixture returns an executor over a model store holding a model
// directory, the config that resolves to it, and the per-process log path.
// port is the fixed port the executor binds and the health wait probes.
func mlxServerFixture(t *testing.T, bin string, port int) (*MLXServerExecutor, ExecutorConfig, string) {
	t.Helper()
	store := t.TempDir()
	if err := os.MkdirAll(filepath.Join(store, "m"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "m", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewMLXServerExecutor(bin, store, port, newNopLogger())
	cfg := ExecutorConfig{
		Name:        "isvc",
		Namespace:   "ns",
		ModelSource: "m",
		ModelName:   "m",
	}
	return e, cfg, filepath.Join(store, "mlx-server-ns-isvc.log")
}

// TestMLXServerStartProcess_ChildExitFailsFast is the regression for an
// mlx-server that dies on startup (a rejected flag or unsupported model, say).
// The executor used to poll /health for the whole startup timeout and report
// only "timeout waiting for health check", blocking every other
// InferenceService on the node behind it. It must now return as soon as the
// child exits, carrying the exit status and the child's own stderr.
func TestMLXServerStartProcess_ChildExitFailsFast(t *testing.T) {
	bin := fakeMLXServer(t, "echo 'error: unsupported model family: llama4' >&2\nexit 1\n")
	e, cfg, logPath := mlxServerFixture(t, bin, 0)
	e.SetStartupTimeout(60 * time.Second)

	start := time.Now()
	proc, err := e.StartProcess(context.Background(), cfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("StartProcess succeeded with a child that exited; process: %+v", proc)
	}
	if elapsed > 10*time.Second {
		t.Errorf("StartProcess took %s against a 60s timeout; it waited on a dead child", elapsed)
	}
	for _, want := range []string{"unsupported model family: llama4", "exit status 1", logPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "timeout") {
		t.Errorf("error still reports a timeout: %v", err)
	}

	info, statErr := os.Stat(logPath)
	if statErr != nil {
		t.Fatalf("per-process log not written: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log file mode = %o, want 600", perm)
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "unsupported model family: llama4") {
		t.Errorf("log file does not hold the child's stderr: %q", data)
	}
	if len(e.children) != 0 {
		t.Errorf("exited child left tracked: %v", e.children)
	}
}

// TestMLXServerStartProcess_HealthyChildThenStop covers the path the exit
// watcher must not break: a child that stays up becomes healthy, and
// StopProcess then stops it cleanly through the reaper (no second wait on the
// same PID, which would lose the status race).
func TestMLXServerStartProcess_HealthyChildThenStop(t *testing.T) {
	bin := fakeMLXServer(t, "exec sleep 60\n")
	port := healthyPort(t)
	e, cfg, logPath := mlxServerFixture(t, bin, port)
	e.SetStartupTimeout(10 * time.Second)

	proc, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess with a healthy child: %v", err)
	}
	if !proc.Healthy || proc.Port != port {
		t.Errorf("process = healthy %v port %d, want healthy on %d", proc.Healthy, proc.Port, port)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("per-process log not created for a healthy child: %v", err)
	}

	exit := e.trackedChild(proc.PID)
	if exit == nil {
		t.Fatal("started child is not tracked")
	}

	start := time.Now()
	if err := e.StopProcess(proc.PID); err != nil {
		t.Fatalf("StopProcess on a live tracked child: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("StopProcess took %s; SIGTERM should end sleep at once", elapsed)
	}
	// The reaper, not a second waiter, must have collected the exit status.
	select {
	case <-exit.done:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper did not observe the child's exit")
	}
	if got := exit.status(); got != "signal: terminated" {
		t.Errorf("reaped status = %q, want %q (a second waiter raced the reaper)", got, "signal: terminated")
	}
	if e.trackedChild(proc.PID) != nil {
		t.Error("StopProcess left the child tracked")
	}
}

// TestMLXServerStopProcess_ChildAlreadyExited covers a child that died after it
// became healthy. The reaper has already collected it, so its PID may belong to
// an unrelated process by now: StopProcess must succeed without signalling.
func TestMLXServerStopProcess_ChildAlreadyExited(t *testing.T) {
	bin := fakeMLXServer(t, "exec sleep 60\n")
	port := healthyPort(t)
	e, cfg, _ := mlxServerFixture(t, bin, port)
	e.SetStartupTimeout(10 * time.Second)

	proc, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess with a healthy child: %v", err)
	}
	exit := e.trackedChild(proc.PID)
	if exit == nil {
		t.Fatal("started child is not tracked")
	}
	if err := syscall.Kill(proc.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exit.done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not observe the child's exit")
	}

	if err := e.StopProcess(proc.PID); err != nil {
		t.Errorf("StopProcess on an already-exited child: %v", err)
	}
	if e.trackedChild(proc.PID) != nil {
		t.Error("StopProcess left the exited child tracked")
	}
}

// TestMLXServerStartProcess_HungChildStillTimesOut keeps the timeout path
// intact for a child that stays up but never answers /health.
func TestMLXServerStartProcess_HungChildStillTimesOut(t *testing.T) {
	bin := fakeMLXServer(t, "echo 'loading model' >&2\nexec sleep 60\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	port := mustExtractPort(t, srv.URL)

	e, cfg, _ := mlxServerFixture(t, bin, port)
	e.SetStartupTimeout(1500 * time.Millisecond)

	_, err := e.StartProcess(context.Background(), cfg)
	if err == nil {
		t.Fatal("StartProcess succeeded with a child that never became healthy")
	}
	for _, want := range []string{"failed health check", "loading model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	e.childMu.Lock()
	left := len(e.children)
	e.childMu.Unlock()
	if left != 0 {
		t.Errorf("timed-out child left tracked (%d children)", left)
	}
}

// TestMLXServerStartProcess_LogTruncatedOnStart: the per-process log holds
// only the current run, so a stale failure from a previous spawn cannot be
// mistaken for this one.
func TestMLXServerStartProcess_LogTruncatedOnStart(t *testing.T) {
	bin := fakeMLXServer(t, "echo 'fresh run' >&2\nexit 2\n")
	e, cfg, logPath := mlxServerFixture(t, bin, 0)
	if err := os.WriteFile(logPath, []byte("stale failure from last run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartProcess(context.Background(), cfg); err == nil {
		t.Fatal("StartProcess succeeded with a child that exited")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "stale failure") || !strings.Contains(string(data), "fresh run") {
		t.Errorf("log not truncated on start: %q", data)
	}
}

func TestMLXServerStartProcess_OnSpawnedErrorKillsChild(t *testing.T) {
	bin := fakeMLXServer(t, "exec sleep 60\n")
	e, cfg, _ := mlxServerFixture(t, bin, healthyPort(t))
	e.SetStartupTimeout(10 * time.Second)
	requireOnSpawnedErrorKillsChild(t, cfg, func(c ExecutorConfig) (*ManagedProcess, error) {
		return e.StartProcess(context.Background(), c)
	})
}

func TestMLXServerStartProcess_EngineHasItsOwnProcessGroup(t *testing.T) {
	bin := fakeMLXServer(t, "exec sleep 60\n")
	e, cfg, _ := mlxServerFixture(t, bin, healthyPort(t))
	e.SetStartupTimeout(10 * time.Second)
	var sp SpawnedProcess
	cfg.OnSpawned = func(s SpawnedProcess) error { sp = s; return nil }
	proc, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	t.Cleanup(func() { _ = e.StopProcess(proc.PID) })
	requireOwnProcessGroup(t, proc.PID, sp)
}
