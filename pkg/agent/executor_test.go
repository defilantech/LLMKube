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
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewMetalExecutor(t *testing.T) {
	executor := NewMetalExecutor("/opt/homebrew/bin/llama-server", "/models", newNopLogger())

	if executor.llamaServerBin != "/opt/homebrew/bin/llama-server" {
		t.Errorf("llamaServerBin = %q, want %q", executor.llamaServerBin, "/opt/homebrew/bin/llama-server")
	}
	if executor.modelStorePath != "/models" {
		t.Errorf("modelStorePath = %q, want %q", executor.modelStorePath, "/models")
	}
}

func TestAllocatePort(t *testing.T) {
	executor := NewMetalExecutor("/bin/llama-server", "/models", newNopLogger())

	port, err := executor.allocatePort()
	if err != nil {
		t.Fatalf("allocatePort returned error: %v", err)
	}
	if port < 1 || port > 65535 {
		t.Errorf("allocatePort returned port %d outside valid range 1-65535", port)
	}
}

func TestAllocatePort_Listenable(t *testing.T) {
	executor := NewMetalExecutor("/bin/llama-server", "/models", newNopLogger())

	port, err := executor.allocatePort()
	if err != nil {
		t.Fatalf("allocatePort returned error: %v", err)
	}

	// The returned port must be immediately re-bindable by the caller.
	// If the kernel left it in TIME_WAIT or similar we'd fail here.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("allocated port %d was not bindable: %v", port, err)
	}
	_ = ln.Close()
}

func TestEnsureModel_AlreadyExists(t *testing.T) {
	tmpDir := t.TempDir()
	modelDir := filepath.Join(tmpDir, "test-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("Failed to create model directory: %v", err)
	}

	modelFile := filepath.Join(modelDir, "model.gguf")
	if err := os.WriteFile(modelFile, []byte("fake-gguf-data"), 0644); err != nil {
		t.Fatalf("Failed to create model file: %v", err)
	}

	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger())

	// source URL basename must match the file we created
	path, err := executor.ensureModel(
		t.Context(),
		"https://huggingface.co/org/repo/resolve/main/model.gguf",
		"test-model",
	)
	if err != nil {
		t.Fatalf("ensureModel returned error: %v", err)
	}
	if path != modelFile {
		t.Errorf("ensureModel path = %q, want %q", path, modelFile)
	}
}

func TestEnsureModel_DownloadFails(t *testing.T) {
	tmpDir := t.TempDir()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger())

	// Use an invalid URL that will fail to download
	_, err := executor.ensureModel(
		t.Context(),
		"http://localhost:1/nonexistent-model.gguf",
		"bad-model",
	)
	if err == nil {
		t.Error("ensureModel with invalid URL should return error")
	}
}

func TestBuildLlamaServerArgs_Defaults(t *testing.T) {
	args := buildLlamaServerArgs("/models/test.gguf", 8080, ExecutorConfig{
		ContextSize: 32768,
	})

	want := map[string]string{
		"--model":        "/models/test.gguf",
		"--host":         "0.0.0.0",
		"--port":         "8080",
		"--n-gpu-layers": "99",
		"--ctx-size":     "32768",
		"--batch-size":   "2048",
	}
	for flag, expected := range want {
		if got := flagValue(args, flag); got != expected {
			t.Errorf("%s = %q, want %q (full args: %v)", flag, got, expected, args)
		}
	}

	if hasFlag(args, "--metrics") != true {
		t.Error("--metrics flag must always be present")
	}

	// FlashAttention/Mlock/Jinja default to false at the buildArgs boundary;
	// the agent layer is what defaults them to true.
	for _, unwanted := range []string{"--flash-attn", "--mlock", "--jinja", "--ubatch-size"} {
		if hasFlag(args, unwanted) {
			t.Errorf("unexpected flag %q in default args: %v", unwanted, args)
		}
	}
}

func TestBuildLlamaServerArgs_AppleSiliconOptimized(t *testing.T) {
	args := buildLlamaServerArgs("/models/test.gguf", 9000, ExecutorConfig{
		ContextSize:    65536,
		FlashAttention: true,
		Mlock:          true,
		Threads:        12,
		BatchSize:      4096,
		UBatchSize:     512,
		Jinja:          true,
	})

	if got := flagValue(args, "--flash-attn"); got != "on" {
		t.Errorf("--flash-attn = %q, want %q", got, "on")
	}
	if !hasFlag(args, "--mlock") {
		t.Error("--mlock missing")
	}
	if got := flagValue(args, "--threads"); got != "12" {
		t.Errorf("--threads = %q, want %q", got, "12")
	}
	if got := flagValue(args, "--batch-size"); got != "4096" {
		t.Errorf("--batch-size = %q, want %q", got, "4096")
	}
	if got := flagValue(args, "--ubatch-size"); got != "512" {
		t.Errorf("--ubatch-size = %q, want %q", got, "512")
	}
	if !hasFlag(args, "--jinja") {
		t.Error("--jinja missing")
	}
}

func TestBuildLlamaServerArgs_GPULayersOverride(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{
		ContextSize: 4096,
		GPULayers:   42,
	})
	if got := flagValue(args, "--n-gpu-layers"); got != "42" {
		t.Errorf("--n-gpu-layers = %q, want %q", got, "42")
	}
}

func TestBuildLlamaServerArgs_CacheTypes(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{
		ContextSize: 4096,
		CacheTypeK:  "turbo3",
		CacheTypeV:  "turbo4",
	})
	if got := flagValue(args, "--cache-type-k"); got != "turbo3" {
		t.Errorf("--cache-type-k = %q, want %q (full args: %v)", got, "turbo3", args)
	}
	if got := flagValue(args, "--cache-type-v"); got != "turbo4" {
		t.Errorf("--cache-type-v = %q, want %q (full args: %v)", got, "turbo4", args)
	}
}

func TestBuildLlamaServerArgs_CacheTypesEmptyOmits(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{ContextSize: 4096})
	if hasFlag(args, "--cache-type-k") {
		t.Errorf("--cache-type-k must be omitted when CacheTypeK is empty (full args: %v)", args)
	}
	if hasFlag(args, "--cache-type-v") {
		t.Errorf("--cache-type-v must be omitted when CacheTypeV is empty (full args: %v)", args)
	}
}

func TestBuildLlamaServerArgs_ParallelSlots(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{
		ContextSize:   4096,
		ParallelSlots: 4,
	})
	if got := flagValue(args, "--parallel"); got != "4" {
		t.Errorf("--parallel = %q, want %q (full args: %v)", got, "4", args)
	}
}

func TestBuildLlamaServerArgs_ParallelSlotsOneOrZeroOmits(t *testing.T) {
	for _, n := range []int{0, 1} {
		args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{
			ContextSize:   4096,
			ParallelSlots: n,
		})
		if hasFlag(args, "--parallel") {
			t.Errorf("--parallel must be omitted for ParallelSlots=%d (full args: %v)", n, args)
		}
	}
}

func TestBuildLlamaServerArgs_MoeOffloadFlags(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{
		ContextSize:   4096,
		MoeCPUOffload: true,
		MoeCPULayers:  6,
		NoKvOffload:   true,
	})
	if !hasFlag(args, "--cpu-moe") {
		t.Errorf("--cpu-moe missing when MoeCPUOffload=true (full args: %v)", args)
	}
	if got := flagValue(args, "--n-cpu-moe"); got != "6" {
		t.Errorf("--n-cpu-moe = %q, want %q", got, "6")
	}
	if !hasFlag(args, "--no-kv-offload") {
		t.Errorf("--no-kv-offload missing when NoKvOffload=true")
	}
}

func TestBuildLlamaServerArgs_TensorAndMetadataOverrides(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{
		ContextSize:       4096,
		TensorOverrides:   []string{"exps=CPU", "attn=GPU"},
		MetadataOverrides: []string{"general.architecture=qwen3", "qwen3.context_length=u32:262144"},
	})
	tensorCount := 0
	kvCount := 0
	for i, a := range args {
		if a == "--override-tensor" && i+1 < len(args) {
			tensorCount++
		}
		if a == "--override-kv" && i+1 < len(args) {
			kvCount++
		}
	}
	if tensorCount != 2 {
		t.Errorf("--override-tensor count = %d, want 2 (full args: %v)", tensorCount, args)
	}
	if kvCount != 2 {
		t.Errorf("--override-kv count = %d, want 2 (full args: %v)", kvCount, args)
	}
}

func TestBuildLlamaServerArgs_NoWarmup(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{
		ContextSize: 4096,
		NoWarmup:    true,
	})
	if !hasFlag(args, "--no-warmup") {
		t.Errorf("--no-warmup missing when NoWarmup=true (full args: %v)", args)
	}
}

func TestBuildLlamaServerArgs_ReasoningBudget(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{
		ContextSize:            4096,
		ReasoningBudget:        2048,
		ReasoningBudgetMessage: "wrap it up",
	})
	if got := flagValue(args, "--reasoning-budget"); got != "2048" {
		t.Errorf("--reasoning-budget = %q, want %q", got, "2048")
	}
	if got := flagValue(args, "--reasoning-budget-message"); got != "wrap it up" {
		t.Errorf("--reasoning-budget-message = %q, want %q", got, "wrap it up")
	}
}

func TestBuildLlamaServerArgs_ReasoningBudgetMessageRequiresBudget(t *testing.T) {
	// Message without a budget is meaningless and must not produce a stray flag.
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{
		ContextSize:            4096,
		ReasoningBudgetMessage: "wrap it up",
	})
	if hasFlag(args, "--reasoning-budget") {
		t.Errorf("--reasoning-budget must be omitted when ReasoningBudget=0")
	}
	if hasFlag(args, "--reasoning-budget-message") {
		t.Errorf("--reasoning-budget-message must be omitted when ReasoningBudget=0 (full args: %v)", args)
	}
}

func TestBuildLlamaServerArgs_ExtraArgsAppendedLast(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{
		ContextSize: 4096,
		ExtraArgs:   []string{"--rope-scaling", "yarn", "--rope-scale", "4"},
	})
	// All four ExtraArgs tokens must appear in order at the very end.
	if len(args) < 4 {
		t.Fatalf("args too short: %v", args)
	}
	tail := args[len(args)-4:]
	want := []string{"--rope-scaling", "yarn", "--rope-scale", "4"}
	for i, w := range want {
		if tail[i] != w {
			t.Errorf("tail[%d] = %q, want %q (full args: %v)", i, tail[i], w, args)
		}
	}
}

// hasFlag reports whether a bare flag (no value following) is present.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

// flagValue returns the argument immediately following the named flag, or ""
// if the flag is absent or appears as the last element.
func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestSetStartupTimeout(t *testing.T) {
	executor := NewMetalExecutor("/bin/llama-server", "/models", newNopLogger())

	if executor.startupTimeout != DefaultLlamaServerStartupTimeout {
		t.Errorf("default startupTimeout = %v, want %v",
			executor.startupTimeout, DefaultLlamaServerStartupTimeout)
	}

	executor.SetStartupTimeout(45 * time.Second)
	if executor.startupTimeout != 45*time.Second {
		t.Errorf("after Set(45s) = %v, want 45s", executor.startupTimeout)
	}

	// Non-positive should coerce back to default.
	executor.SetStartupTimeout(0)
	if executor.startupTimeout != DefaultLlamaServerStartupTimeout {
		t.Errorf("after Set(0) = %v, want default %v",
			executor.startupTimeout, DefaultLlamaServerStartupTimeout)
	}
	executor.SetStartupTimeout(-5 * time.Second)
	if executor.startupTimeout != DefaultLlamaServerStartupTimeout {
		t.Errorf("after Set(-5s) = %v, want default %v",
			executor.startupTimeout, DefaultLlamaServerStartupTimeout)
	}
}

func TestOMLXSetStartupTimeout(t *testing.T) {
	executor := NewOMLXExecutor("/bin/omlx", "/models", 8000, newNopLogger())

	if executor.startupTimeout != DefaultOMLXStartupTimeout {
		t.Errorf("default startupTimeout = %v, want %v",
			executor.startupTimeout, DefaultOMLXStartupTimeout)
	}

	executor.SetStartupTimeout(180 * time.Second)
	if executor.startupTimeout != 180*time.Second {
		t.Errorf("after Set(180s) = %v, want 180s", executor.startupTimeout)
	}

	executor.SetStartupTimeout(0)
	if executor.startupTimeout != DefaultOMLXStartupTimeout {
		t.Errorf("after Set(0) = %v, want default %v",
			executor.startupTimeout, DefaultOMLXStartupTimeout)
	}
}

func TestStopProcess_InvalidPID(t *testing.T) {
	executor := NewMetalExecutor("/bin/llama-server", "/models", newNopLogger())

	// PID 0 is the calling process's group — Signal will fail
	err := executor.StopProcess(-99999)
	if err == nil {
		t.Error("StopProcess with invalid PID should return error")
	}
}
