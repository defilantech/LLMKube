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
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNewVLLMSwiftExecutor(t *testing.T) {
	executor := NewVLLMSwiftExecutor("/opt/homebrew/bin/vllm-swift", "/models", newNopLogger())

	if executor.bin != "/opt/homebrew/bin/vllm-swift" {
		t.Errorf("bin = %q, want %q", executor.bin, "/opt/homebrew/bin/vllm-swift")
	}
	if executor.modelStorePath != "/models" {
		t.Errorf("modelStorePath = %q, want %q", executor.modelStorePath, "/models")
	}
	if executor.startupTimeout != DefaultVLLMSwiftStartupTimeout {
		t.Errorf("default startupTimeout = %v, want %v",
			executor.startupTimeout, DefaultVLLMSwiftStartupTimeout)
	}
}

func TestVLLMSwiftSetStartupTimeout(t *testing.T) {
	executor := NewVLLMSwiftExecutor("/bin/vllm-swift", "/models", newNopLogger())

	executor.SetStartupTimeout(180 * time.Second)
	if executor.startupTimeout != 180*time.Second {
		t.Errorf("after Set(180s) = %v, want 180s", executor.startupTimeout)
	}

	// Non-positive values coerce back to default.
	executor.SetStartupTimeout(0)
	if executor.startupTimeout != DefaultVLLMSwiftStartupTimeout {
		t.Errorf("after Set(0) = %v, want default %v",
			executor.startupTimeout, DefaultVLLMSwiftStartupTimeout)
	}
	executor.SetStartupTimeout(-5 * time.Second)
	if executor.startupTimeout != DefaultVLLMSwiftStartupTimeout {
		t.Errorf("after Set(-5s) = %v, want default %v",
			executor.startupTimeout, DefaultVLLMSwiftStartupTimeout)
	}
}

func TestVLLMSwiftStopProcess_InvalidPID(t *testing.T) {
	executor := NewVLLMSwiftExecutor("/bin/vllm-swift", "/models", newNopLogger())

	err := executor.StopProcess(-99999)
	if err == nil {
		t.Error("StopProcess with invalid PID should return error")
	}
}

func TestTurboQuantConfig(t *testing.T) {
	tests := []struct {
		cacheType  string
		wantScheme string
		wantBits   int
	}{
		{"turbo4v2", "turbo4v2", 4},
		{"turbo4", "turbo4", 4},
		{"turbo3", "turbo3", 3},
		{"turbo2", "turbo2", 2},
		{"", "", 0},
		{"f16", "", 0},
		{"q8_0", "", 0},
		{"iq4_nl", "", 0},
		{"TURBO4V2", "", 0}, // case-sensitive, upstream uses lowercase
	}

	for _, tc := range tests {
		t.Run(tc.cacheType, func(t *testing.T) {
			scheme, bits := turboQuantConfig(tc.cacheType)
			if scheme != tc.wantScheme {
				t.Errorf("scheme = %q, want %q", scheme, tc.wantScheme)
			}
			if bits != tc.wantBits {
				t.Errorf("bits = %d, want %d", bits, tc.wantBits)
			}
		})
	}
}

func TestBuildVLLMSwiftArgs_Defaults(t *testing.T) {
	args := buildVLLMSwiftArgs("/models/Qwen3-4B-4bit", 8080, ExecutorConfig{
		ContextSize: 32768,
	})

	// Positional model path comes after "serve" subcommand.
	if len(args) < 2 || args[0] != "serve" {
		t.Fatalf("first arg must be \"serve\" subcommand, got: %v", args)
	}
	if args[1] != "/models/Qwen3-4B-4bit" {
		t.Errorf("model path = %q, want %q (full args: %v)",
			args[1], "/models/Qwen3-4B-4bit", args)
	}

	want := map[string]string{
		"--port":          "8080",
		"--max-model-len": "32768",
	}
	for flag, expected := range want {
		if got := flagValue(args, flag); got != expected {
			t.Errorf("%s = %q, want %q (full args: %v)", flag, got, expected, args)
		}
	}

	// Defaults must NOT inject TurboQuant or seq concurrency.
	for _, unwanted := range []string{
		"--additional-config", "--max-num-seqs", "--gpu-memory-utilization",
	} {
		if hasFlag(args, unwanted) {
			t.Errorf("unexpected flag %q in default args: %v", unwanted, args)
		}
	}
}

func TestBuildVLLMSwiftArgs_ParallelSlots(t *testing.T) {
	args := buildVLLMSwiftArgs("/m", 8080, ExecutorConfig{
		ContextSize:   4096,
		ParallelSlots: 8,
	})
	if got := flagValue(args, "--max-num-seqs"); got != "8" {
		t.Errorf("--max-num-seqs = %q, want %q (full args: %v)", got, "8", args)
	}
}

func TestBuildVLLMSwiftArgs_ParallelSlotsOneOrZeroOmits(t *testing.T) {
	for _, n := range []int{0, 1} {
		args := buildVLLMSwiftArgs("/m", 8080, ExecutorConfig{
			ContextSize:   4096,
			ParallelSlots: n,
		})
		if hasFlag(args, "--max-num-seqs") {
			t.Errorf("--max-num-seqs must be omitted for ParallelSlots=%d (full args: %v)",
				n, args)
		}
	}
}

func TestBuildVLLMSwiftArgs_TurboQuant(t *testing.T) {
	tests := []struct {
		name       string
		cacheTypeK string
		wantScheme string
		wantBits   int
	}{
		{"turbo4v2_recommended", "turbo4v2", "turbo4v2", 4},
		{"turbo3_max_compression", "turbo3", "turbo3", 3},
		{"turbo4_legacy", "turbo4", "turbo4", 4},
		{"turbo2_aggressive", "turbo2", "turbo2", 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := buildVLLMSwiftArgs("/m", 8080, ExecutorConfig{
				ContextSize: 131072,
				CacheTypeK:  tc.cacheTypeK,
			})
			raw := flagValue(args, "--additional-config")
			if raw == "" {
				t.Fatalf("--additional-config missing for %s (full args: %v)", tc.cacheTypeK, args)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("--additional-config value is not valid JSON: %q (err %v)", raw, err)
			}
			if got["kv_scheme"] != tc.wantScheme {
				t.Errorf("kv_scheme = %v, want %q", got["kv_scheme"], tc.wantScheme)
			}
			// JSON numbers decode to float64.
			if int(got["kv_bits"].(float64)) != tc.wantBits {
				t.Errorf("kv_bits = %v, want %d", got["kv_bits"], tc.wantBits)
			}
		})
	}
}

func TestBuildVLLMSwiftArgs_TurboQuantOmittedForNonTurboCacheTypes(t *testing.T) {
	for _, ct := range []string{"", "f16", "q8_0", "iq4_nl"} {
		args := buildVLLMSwiftArgs("/m", 8080, ExecutorConfig{
			ContextSize: 4096,
			CacheTypeK:  ct,
		})
		if hasFlag(args, "--additional-config") {
			t.Errorf("--additional-config must be omitted for non-turbo CacheTypeK=%q (full args: %v)",
				ct, args)
		}
	}
}

func TestBuildVLLMSwiftArgs_ExtraArgsAppendedLast(t *testing.T) {
	args := buildVLLMSwiftArgs("/m", 8080, ExecutorConfig{
		ContextSize: 4096,
		ExtraArgs:   []string{"--enable-reasoning", "--reasoning-parser", "deepseek_r1"},
	})
	if len(args) < 3 {
		t.Fatalf("args too short: %v", args)
	}
	tail := args[len(args)-3:]
	want := []string{"--enable-reasoning", "--reasoning-parser", "deepseek_r1"}
	for i, w := range want {
		if tail[i] != w {
			t.Errorf("tail[%d] = %q, want %q (full args: %v)", i, tail[i], w, args)
		}
	}
}

func TestBuildVLLMSwiftArgs_TurboQuantPlusParallelSlots(t *testing.T) {
	// Real-world coding-model invocation: TurboQuant for long context PLUS
	// parallel slots for concurrent agent requests. Both must be present.
	args := buildVLLMSwiftArgs("/models/qwen", 8080, ExecutorConfig{
		ContextSize:   131072,
		ParallelSlots: 4,
		CacheTypeK:    "turbo4v2",
	})
	if got := flagValue(args, "--max-num-seqs"); got != "4" {
		t.Errorf("--max-num-seqs = %q, want %q", got, "4")
	}
	if !hasFlag(args, "--additional-config") {
		t.Errorf("--additional-config missing in combined invocation: %v", args)
	}
}

func TestVLLMSwiftProcessLogPath(t *testing.T) {
	executor := NewVLLMSwiftExecutor("/bin/vllm-swift", "/var/lib/llmkube", newNopLogger())

	got := executor.processLogPath("default", "qwen-coder")
	want := filepath.Join("/var/lib/llmkube", "vllm-swift-default-qwen-coder.log")
	if got != want {
		t.Errorf("processLogPath = %q, want %q", got, want)
	}

	// Different namespace + name yields a distinct path.
	other := executor.processLogPath("prod", "qwen-coder")
	if other == got {
		t.Errorf("processLogPath collided across namespaces: %q == %q", other, got)
	}
}

func TestVLLMSwiftResolveModelPath(t *testing.T) {
	tmp := t.TempDir()

	// Build a real on-disk layout that exercises the symlink path:
	//   <tmp>/models/Qwen3-4B-4bit/        (the actual model dir)
	//   <tmp>/models/mlx-community/Qwen3-4B-4bit -> ../Qwen3-4B-4bit
	modelStore := filepath.Join(tmp, "models")
	realDir := filepath.Join(modelStore, "Qwen3-4B-4bit")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	hfDir := filepath.Join(modelStore, "mlx-community")
	if err := os.MkdirAll(hfDir, 0o755); err != nil {
		t.Fatalf("mkdir hf: %v", err)
	}
	symlink := filepath.Join(hfDir, "Qwen3-4B-4bit")
	if err := os.Symlink(realDir, symlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	executor := NewVLLMSwiftExecutor("/bin/vllm-swift", modelStore, newNopLogger())

	tests := []struct {
		name   string
		config ExecutorConfig
		want   string
	}{
		{
			name:   "absolute path passes through and resolves symlinks",
			config: ExecutorConfig{ModelSource: symlink},
			want:   realDir,
		},
		{
			name:   "relative HF-shorthand resolves under modelStorePath then through symlink",
			config: ExecutorConfig{ModelSource: "mlx-community/Qwen3-4B-4bit"},
			want:   realDir,
		},
		{
			name:   "absolute non-symlinked path stays as-is",
			config: ExecutorConfig{ModelSource: realDir},
			want:   realDir,
		},
		{
			name:   "empty source falls back to <modelStore>/<name>",
			config: ExecutorConfig{ModelName: "Qwen3-4B-4bit"},
			want:   realDir,
		},
		{
			name:   "non-existent path returns the candidate unchanged",
			config: ExecutorConfig{ModelSource: "/nope/does-not-exist"},
			want:   "/nope/does-not-exist",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// On macOS /tmp resolves to /private/tmp; resolve the want side
			// the same way so the comparison is symlink-agnostic.
			wantResolved, err := filepath.EvalSymlinks(tc.want)
			if err != nil {
				wantResolved = tc.want
			}
			got := executor.resolveModelPath(tc.config)
			if got != wantResolved && got != tc.want {
				t.Errorf("resolveModelPath = %q, want %q (or %q before EvalSymlinks)",
					got, wantResolved, tc.want)
			}
		})
	}
}

func TestVLLMSwiftAllocatePort(t *testing.T) {
	executor := NewVLLMSwiftExecutor("/bin/vllm-swift", "/models", newNopLogger())

	port, err := executor.allocatePort()
	if err != nil {
		t.Fatalf("allocatePort: %v", err)
	}
	if port < 1 || port > 65535 {
		t.Errorf("allocatePort returned port %d outside valid range", port)
	}

	// The returned port must be immediately bindable (we want a fresh
	// non-collided port back, not one stuck in TIME_WAIT or similar).
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d not bindable after allocate: %v", port, err)
	}
	_ = ln.Close()
}

func TestVLLMSwiftStopProcess_HappyPath(t *testing.T) {
	// Spawn a `sleep` child the executor doesn't manage, then ask
	// StopProcess to send it SIGTERM. This exercises the untracked-PID
	// fallback in stopChild (a process adopted after an agent restart),
	// which signals and polls the PID. The default SIGTERM handler exits the
	// child cleanly.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not spawn sleep: %v", err)
	}
	// A reaper outside the tracker, as launchd is for an adopted engine: an
	// unreaped zombie would still answer kill(pid, 0) and look alive.
	go func() { _ = cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()

	executor := NewVLLMSwiftExecutor("/bin/vllm-swift", "/models", newNopLogger())
	if err := executor.StopProcess(cmd.Process.Pid); err != nil {
		t.Errorf("StopProcess returned error on graceful SIGTERM exit: %v", err)
	}
}

// fakeVLLMSwift writes an executable shell script standing in for
// vllm-swift and returns its path.
func fakeVLLMSwift(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vllm-swift")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write fake vllm-swift: %v", err)
	}
	return path
}

// vllmSwiftFixture returns an executor over a model store holding a model
// directory, the config that resolves to it, and the per-process log path.
func vllmSwiftFixture(t *testing.T, bin string) (*VLLMSwiftExecutor, ExecutorConfig, string) {
	t.Helper()
	store := t.TempDir()
	if err := os.MkdirAll(filepath.Join(store, "qwen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "qwen", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewVLLMSwiftExecutor(bin, store, newNopLogger())
	cfg := ExecutorConfig{
		Name:        "isvc",
		Namespace:   "ns",
		ModelSource: "qwen",
		ModelName:   "qwen-model",
		ContextSize: 32768,
	}
	return e, cfg, filepath.Join(store, "vllm-swift-ns-isvc.log")
}

// TestVLLMSwiftStartProcess_ChildExitFailsFast is the regression for a
// vllm-swift that dies on startup (a bad flag, an unsupported model). The
// executor used to poll /health for the whole startup timeout and report only
// a health timeout, blocking every other InferenceService on the node behind
// it. It must now return as soon as the child exits, carrying the exit status
// and the child's own stderr.
func TestVLLMSwiftStartProcess_ChildExitFailsFast(t *testing.T) {
	bin := fakeVLLMSwift(t, "echo 'error: unsupported model type: qwen3' >&2\nexit 1\n")
	e, cfg, logPath := vllmSwiftFixture(t, bin)
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
	for _, want := range []string{"unsupported model type: qwen3", "exit status 1", logPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "failed health check") {
		t.Errorf("error still reports a health failure: %v", err)
	}

	info, statErr := os.Stat(logPath)
	if statErr != nil {
		t.Fatalf("per-process log not written: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log file mode = %o, want 600", perm)
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "unsupported model type: qwen3") {
		t.Errorf("log file does not hold the child's stderr: %q", data)
	}
	if e.trackedChild(0) != nil || len(e.children) != 0 {
		t.Errorf("exited child left tracked: %v", e.children)
	}
}

// TestVLLMSwiftStartProcess_HealthyChildThenStop covers the path the exit
// watcher must not break: a child that stays up becomes healthy, and
// StopProcess then stops it cleanly through the reaper. The reaper collects
// the exit status, so StopProcess must not also wait on the PID (a second
// wait loses the race and sees "no child processes" instead of the signal).
func TestVLLMSwiftStartProcess_HealthyChildThenStop(t *testing.T) {
	bin := fakeVLLMSwift(t, "exec sleep 60\n")
	e, cfg, logPath := vllmSwiftFixture(t, bin)
	port := healthyPort(t)
	e.allocatePort = func() (int, error) { return port, nil }
	e.SetStartupTimeout(10 * time.Second)

	proc, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess with a healthy child: %v", err)
	}
	if !proc.Healthy || proc.Port != port {
		t.Errorf("process = %+v, want healthy on port %d", proc, port)
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

// TestVLLMSwiftStopProcess_ChildAlreadyExited: a child that died after it
// became healthy has been reaped, so its PID may be reused. StopProcess must
// succeed without signalling.
func TestVLLMSwiftStopProcess_ChildAlreadyExited(t *testing.T) {
	bin := fakeVLLMSwift(t, "exec sleep 60\n")
	e, cfg, _ := vllmSwiftFixture(t, bin)
	port := healthyPort(t)
	e.allocatePort = func() (int, error) { return port, nil }
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

// TestVLLMSwiftStartProcess_HungChildStillTimesOut keeps the timeout path
// intact for a child that stays up but never answers /health.
func TestVLLMSwiftStartProcess_HungChildStillTimesOut(t *testing.T) {
	bin := fakeVLLMSwift(t, "echo 'loading model' >&2\nexec sleep 60\n")
	e, cfg, _ := vllmSwiftFixture(t, bin)
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
}

// mustExtractPort pulls the numeric port out of an httptest.Server URL.
func mustExtractPort(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse server URL %q: %v", raw, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port from %q: %v", u.Port(), err)
	}
	return port
}

func TestVLLMSwiftStartProcess_OnSpawnedErrorKillsChild(t *testing.T) {
	bin := fakeVLLMSwift(t, "exec sleep 60\n")
	e, cfg, _ := vllmSwiftFixture(t, bin)
	port := healthyPort(t)
	e.allocatePort = func() (int, error) { return port, nil }
	e.SetStartupTimeout(10 * time.Second)
	requireOnSpawnedErrorKillsChild(t, cfg, func(c ExecutorConfig) (*ManagedProcess, error) {
		return e.StartProcess(context.Background(), c)
	})
}

func TestVLLMSwiftStartProcess_EngineHasItsOwnProcessGroup(t *testing.T) {
	bin := fakeVLLMSwift(t, "exec sleep 60\n")
	e, cfg, _ := vllmSwiftFixture(t, bin)
	port := healthyPort(t)
	e.allocatePort = func() (int, error) { return port, nil }
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
