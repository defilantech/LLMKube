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
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

func TestVLLMSwiftWaitForHealthy_OK(t *testing.T) {
	// Mock health server that responds 200 immediately.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	port := mustExtractPort(t, srv.URL)
	executor := NewVLLMSwiftExecutor("/bin/vllm-swift", "/models", newNopLogger())

	if err := executor.waitForHealthy(port, 5*time.Second); err != nil {
		t.Errorf("waitForHealthy returned error against healthy server: %v", err)
	}
}

func TestVLLMSwiftWaitForHealthy_Timeout(t *testing.T) {
	// Server that returns 503 forever — waitForHealthy should give up after
	// the deadline rather than hanging or false-positive-ing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	port := mustExtractPort(t, srv.URL)
	executor := NewVLLMSwiftExecutor("/bin/vllm-swift", "/models", newNopLogger())

	start := time.Now()
	err := executor.waitForHealthy(port, 1500*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("waitForHealthy must return error when server is never healthy")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error must mention timeout, got: %v", err)
	}
	if elapsed < 1400*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("waitForHealthy elapsed = %v, want roughly 1.5s", elapsed)
	}
}

func TestVLLMSwiftStopProcess_HappyPath(t *testing.T) {
	// Spawn a `sleep` child the executor doesn't manage, then ask
	// StopProcess to send it SIGTERM. The default SIGTERM handler exits the
	// child cleanly, which Wait() observes. This validates the SIGTERM path
	// without needing a real vllm-swift child.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not spawn sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	executor := NewVLLMSwiftExecutor("/bin/vllm-swift", "/models", newNopLogger())
	if err := executor.StopProcess(cmd.Process.Pid); err != nil {
		t.Errorf("StopProcess returned error on graceful SIGTERM exit: %v", err)
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
