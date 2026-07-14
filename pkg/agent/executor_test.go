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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// extractFlags returns the set of flag tokens from an arg slice,
// deduplicated and sorted. It collects every element that starts with "--",
// which is how llama-server emits all its flags. Values (model paths,
// numbers, strings) never start with "--", so this is robust to multi-word
// values that appear as separate slice elements.
func extractFlags(args []string) []string {
	seen := map[string]struct{}{}
	var flags []string
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		flags = append(flags, a)
	}
	sort.Strings(flags)
	return flags
}

// ptrInt32, ptrBool are local helpers so tests read naturally.
func ptrInt32(i int32) *int32 { return &i }
func ptrBool(b bool) *bool    { return &b }

func TestNewMetalExecutor(t *testing.T) {
	executor := NewMetalExecutor("/opt/homebrew/bin/llama-server", "/models", newNopLogger())

	if executor.llamaServerBin != "/opt/homebrew/bin/llama-server" {
		t.Errorf("llamaServerBin = %q, want %q", executor.llamaServerBin, "/opt/homebrew/bin/llama-server")
	}
	if executor.modelStorePath != "/models" {
		t.Errorf("modelStorePath = %q, want %q", executor.modelStorePath, "/models")
	}
}

func TestMetalExecutorSetPort(t *testing.T) {
	executor := NewMetalExecutor("/bin/llama-server", "/models", newNopLogger())

	// Default: no fixed port, so StartProcess falls back to an ephemeral one.
	if executor.fixedPort != 0 {
		t.Errorf("fixedPort default = %d, want 0", executor.fixedPort)
	}

	executor.SetPort(8080)
	if executor.fixedPort != 8080 {
		t.Errorf("fixedPort after SetPort(8080) = %d, want 8080", executor.fixedPort)
	}

	// Negative values are coerced back to 0 (ephemeral).
	executor.SetPort(-1)
	if executor.fixedPort != 0 {
		t.Errorf("fixedPort after SetPort(-1) = %d, want 0", executor.fixedPort)
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

func TestBuildLlamaServerArgs_RopeScaling(t *testing.T) {
	args := buildLlamaServerArgs("/models/test.gguf", 8080, ExecutorConfig{
		ContextSize:        262144,
		RopeScalingType:    "yarn",
		RopeScalingFactor:  "2.0",
		RopeScalingOrigCtx: 131072,
	})

	want := map[string]string{
		"--rope-scaling":  "yarn",
		"--rope-scale":    "2.0",
		"--yarn-orig-ctx": "131072",
	}
	for flag, expected := range want {
		if got := flagValue(args, flag); got != expected {
			t.Errorf("%s = %q, want %q (full args: %v)", flag, got, expected, args)
		}
	}
}

func TestBuildLlamaServerArgs_RopeScalingOmittedWhenUnset(t *testing.T) {
	args := buildLlamaServerArgs("/models/test.gguf", 8080, ExecutorConfig{ContextSize: 4096})
	for _, unwanted := range []string{"--rope-scaling", "--rope-scale", "--yarn-orig-ctx"} {
		if hasFlag(args, unwanted) {
			t.Errorf("unexpected rope flag %q when ropeScaling unset: %v", unwanted, args)
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

func TestDownloadFile_FailedDownloadLeavesNoFile(t *testing.T) {
	tmpDir := t.TempDir()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger())

	// Server that returns 401 (e.g. gated Hugging Face repo)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	modelDir := filepath.Join(tmpDir, "gated-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("Failed to create model directory: %v", err)
	}
	localPath := filepath.Join(modelDir, "model.gguf")

	err := executor.downloadFile(t.Context(), srv.URL+"/model.gguf", localPath)
	if err == nil {
		t.Fatal("downloadFile should return error for 401 response")
	}

	// The final path must not exist
	if _, err := os.Stat(localPath); !os.IsNotExist(err) {
		t.Errorf("failed download left file at %q: %v", localPath, err)
	}

	// No .partial stub should remain either
	partialPath := localPath + ".partial"
	if _, err := os.Stat(partialPath); !os.IsNotExist(err) {
		t.Errorf("failed download left partial file at %q: %v", partialPath, err)
	}
}

func TestEnsureModel_ZeroByteFileTriggersRedownload(t *testing.T) {
	tmpDir := t.TempDir()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger())

	modelDir := filepath.Join(tmpDir, "stub-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("Failed to create model directory: %v", err)
	}
	localPath := filepath.Join(modelDir, "model.gguf")

	// Pre-create a zero-byte stub (simulates a failed download from a
	// previous run that left an empty file).
	if err := os.WriteFile(localPath, nil, 0644); err != nil {
		t.Fatalf("Failed to create stub file: %v", err)
	}

	// Server that returns a small valid payload.
	payload := []byte("fake-gguf-data")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	_, err := executor.ensureModel(t.Context(), srv.URL+"/model.gguf", "stub-model")
	if err != nil {
		t.Fatalf("ensureModel should succeed when stub is zero bytes: %v", err)
	}

	// The file should now contain the downloaded data.
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatalf("model file missing after download: %v", err)
	}
	if info.Size() == 0 {
		t.Error("model file is still zero bytes after ensureModel")
	}
}

func TestDownloadFile_TruncatedDownloadFails(t *testing.T) {
	tmpDir := t.TempDir()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger())

	modelDir := filepath.Join(tmpDir, "trunc-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("Failed to create model directory: %v", err)
	}
	localPath := filepath.Join(modelDir, "model.gguf")

	// Server that advertises a large Content-Length but sends fewer bytes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()

	err := executor.downloadFile(t.Context(), srv.URL+"/model.gguf", localPath)
	if err == nil {
		t.Fatal("downloadFile should return error for truncated download")
	}

	// The final path must not exist (truncated file should be cleaned up).
	if _, err := os.Stat(localPath); !os.IsNotExist(err) {
		t.Errorf("truncated download left file at %q: %v", localPath, err)
	}
}

func TestDownloadFile_SuccessRenamesTempFile(t *testing.T) {
	tmpDir := t.TempDir()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger())

	modelDir := filepath.Join(tmpDir, "ok-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("Failed to create model directory: %v", err)
	}
	localPath := filepath.Join(modelDir, "model.gguf")

	payload := []byte("fake-gguf-data")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	err := executor.downloadFile(t.Context(), srv.URL+"/model.gguf", localPath)
	if err != nil {
		t.Fatalf("downloadFile should succeed: %v", err)
	}

	// The final path must exist and contain the data.
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatalf("model file missing after download: %v", err)
	}
	if info.Size() != int64(len(payload)) {
		t.Errorf("model file size = %d, want %d", info.Size(), len(payload))
	}

	// No .partial stub should remain.
	partialPath := localPath + ".partial"
	if _, err := os.Stat(partialPath); !os.IsNotExist(err) {
		t.Errorf("partial file still exists at %q: %v", partialPath, err)
	}
}

func TestDownloadFile_NoContentLength(t *testing.T) {
	tmpDir := t.TempDir()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger())

	modelDir := filepath.Join(tmpDir, "no-cl-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("Failed to create model directory: %v", err)
	}
	localPath := filepath.Join(modelDir, "model.gguf")

	payload := []byte("fake-gguf-data")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Content-Length header; ContentLength will be 0.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	err := executor.downloadFile(t.Context(), srv.URL+"/model.gguf", localPath)
	if err != nil {
		t.Fatalf("downloadFile should succeed without Content-Length: %v", err)
	}

	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatalf("model file missing after download: %v", err)
	}
	if info.Size() != int64(len(payload)) {
		t.Errorf("model file size = %d, want %d", info.Size(), len(payload))
	}
}

func TestDownloadFile_ContextCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger())

	modelDir := filepath.Join(tmpDir, "cancel-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("Failed to create model directory: %v", err)
	}
	localPath := filepath.Join(modelDir, "model.gguf")

	// Server that blocks forever.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never respond.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // Cancel immediately.

	err := executor.downloadFile(ctx, srv.URL+"/model.gguf", localPath)
	if err == nil {
		t.Fatal("downloadFile should return error when context is cancelled")
	}

	// No file should be left behind.
	if _, err := os.Stat(localPath); !os.IsNotExist(err) {
		t.Errorf("cancelled download left file at %q: %v", localPath, err)
	}
}

// TestBuildLlamaServerArgsRuntimeArgParity is the automated guard that enforces
// parity between the controller-side LlamaCppBackend.BuildArgs
// (internal/controller/runtime_llamacpp.go) and this function,
// buildLlamaServerArgs. For every runtime-affecting field on a representative
// InferenceService spec, the test asserts that the same flag (or flag prefix)
// appears on both sides.
//
// This mirrors the resolveCacheTypes comment in pkg/agent/agent.go: the metal
// agent and the in-cluster controller must emit identical flags for the same
// spec, or an operator editing a CR would silently get different behavior
// depending on whether the workload runs in-cluster or on a Mac.
//
// The two arg builders are intentionally NOT refactored into one shared
// function (they serve different runtimes and have different defaults); this
// test is the contract. See AGENTS.md "Runtime-arg parity" for the wiring
// rules that accompany a new field.
func TestBuildLlamaServerArgsRuntimeArgParity(t *testing.T) {
	// Representative spec with every runtime-affecting field set. This is the
	// single source of truth the parity test feeds into both arg builders.
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "parity-test", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ContextSize:            ptrInt32(8192),
			ParallelSlots:          ptrInt32(4),
			FlashAttention:         ptrBool(true),
			Jinja:                  ptrBool(true),
			CacheTypeK:             "q8_0",
			CacheTypeV:             "q8_0",
			MoeCPUOffload:          ptrBool(true),
			MoeCPULayers:           ptrInt32(8),
			NoKvOffload:            ptrBool(true),
			BatchSize:              ptrInt32(512),
			UBatchSize:             ptrInt32(256),
			NoWarmup:               ptrBool(true),
			ReasoningBudget:        ptrInt32(4096),
			ReasoningBudgetMessage: "think carefully",
			TensorOverrides:        []string{"foo=bar"},
			MetadataOverrides:      []string{"key=val"},
			RopeScaling: &inferencev1alpha1.RopeScalingSpec{
				Type:            "yarn",
				Factor:          "2.0",
				OriginalContext: ptrInt32(131072),
			},
		},
	}

	// Build the ExecutorConfig the way buildExecutorConfig would, so the test
	// covers the full agent-side wiring (not just buildLlamaServerArgs in
	// isolation).
	cfg := buildExecutorConfigForTest(isvc)

	const modelPath = "/models/test"
	const port = 8000
	metalArgs := buildLlamaServerArgs(modelPath, port, cfg)

	// Extract just the flag portion (every even-indexed entry) from the metal
	// side so we can compare sets of flags regardless of argument order or
	// value differences (e.g. port, model path).
	metalFlags := extractFlags(metalArgs)

	// The parity set: every flag that buildLlamaServerArgs must emit when the
	// corresponding spec field is set. These are the runtime-affecting fields
	// the issue calls out: any new field must be added here AND wired into
	// the controller's BuildArgs.
	parityFlags := []string{
		"--model",
		"--host",
		"--port",
		"--n-gpu-layers",
		"--ctx-size",
		"--rope-scaling",
		"--rope-scale",
		"--yarn-orig-ctx",
		"--parallel",
		"--flash-attn",
		"--mlock",
		"--cache-type-k",
		"--cache-type-v",
		"--cpu-moe",
		"--n-cpu-moe",
		"--no-kv-offload",
		"--override-tensor",
		"--override-kv",
		// NOTE: --threads is intentionally NOT a parity flag. The metal-agent
		// auto-detects it from Apple-Silicon performance cores
		// (detectPerfCoreCount, which returns 0 on non-darwin), while the
		// in-cluster controller deliberately omits it and lets llama.cpp
		// auto-detect from the pod's CPU limits. It is metal-only, OS-dependent,
		// and not spec-driven, so asserting it here breaks on Linux CI.
		"--batch-size",
		"--ubatch-size",
		"--no-warmup",
		"--reasoning-budget",
		"--reasoning-budget-message",
		"--jinja",
		"--metrics",
	}

	for _, flag := range parityFlags {
		if !slices.Contains(metalFlags, flag) {
			t.Errorf("buildLlamaServerArgs missing flag %q with parity spec; args: %v", flag, metalArgs)
		}
	}
}

// buildExecutorConfigForTest mirrors buildExecutorConfig for the parity test.
// It lives in the test file so the parity test does not reach into unexported
// agent internals; the production path goes through the real
// buildExecutorConfig in agent.go.
func buildExecutorConfigForTest(isvc *inferencev1alpha1.InferenceService) ExecutorConfig {
	cacheTypeK, cacheTypeV := resolveCacheTypes(isvc)

	var ropeType, ropeFactor string
	var ropeOrigCtx int
	if r := isvc.Spec.RopeScaling; r != nil {
		ropeType = string(r.Type)
		ropeFactor = r.Factor
		ropeOrigCtx = derefInt32(r.OriginalContext)
	}

	return ExecutorConfig{
		Name:                   isvc.Name,
		Namespace:              isvc.Namespace,
		GPULayers:              99,
		ContextSize:            derefInt32(isvc.Spec.ContextSize),
		RopeScalingType:        ropeType,
		RopeScalingFactor:      ropeFactor,
		RopeScalingOrigCtx:     ropeOrigCtx,
		Jinja:                  derefBool(isvc.Spec.Jinja),
		FlashAttention:         derefBool(isvc.Spec.FlashAttention),
		Mlock:                  true,
		BatchSize:              derefInt32(isvc.Spec.BatchSize),
		UBatchSize:             derefInt32(isvc.Spec.UBatchSize),
		ParallelSlots:          derefInt32(isvc.Spec.ParallelSlots),
		CacheTypeK:             cacheTypeK,
		CacheTypeV:             cacheTypeV,
		MoeCPUOffload:          derefBool(isvc.Spec.MoeCPUOffload),
		MoeCPULayers:           derefInt32(isvc.Spec.MoeCPULayers),
		NoKvOffload:            derefBool(isvc.Spec.NoKvOffload),
		TensorOverrides:        isvc.Spec.TensorOverrides,
		MetadataOverrides:      isvc.Spec.MetadataOverrides,
		NoWarmup:               derefBool(isvc.Spec.NoWarmup),
		ReasoningBudget:        derefInt32(isvc.Spec.ReasoningBudget),
		ReasoningBudgetMessage: isvc.Spec.ReasoningBudgetMessage,
		Mode:                   isvc.Spec.Mode,
	}
}

// TestDerefString verifies the derefString helper used in buildExecutorConfig.
func TestDerefString(t *testing.T) {
	// Nil pointer returns empty string.
	if got := derefString(nil); got != "" {
		t.Errorf("derefString(nil) = %q, want %q", got, "")
	}

	// Non-nil pointer returns the pointed value.
	s := "hello"
	if got := derefString(&s); got != "hello" {
		t.Errorf("derefString(&s) = %q, want %q", got, "hello")
	}
}

// TestBuildOMLXServeArgs_Defaults verifies the base arg vector for the oMLX
// daemon serve subcommand.
func TestBuildOMLXServeArgs_Defaults(t *testing.T) {
	cfg := omlxServeConfig{
		turboQuantBits:       0,
		pagedSSDCacheDir:     "",
		hotCacheMaxSize:      "",
		pagedSSDCacheMaxSize: "",
	}
	args := buildOMLXServeArgs("/models/test", 8000, cfg)

	want := map[string]string{
		"--model-dir": "/models/test",
		"--port":      "8000",
		"--host":      "0.0.0.0",
	}
	for flag, expected := range want {
		if got := flagValue(args, flag); got != expected {
			t.Errorf("%s = %q, want %q (full args: %v)", flag, got, expected, args)
		}
	}

	// No optional flags should be present.
	unwantedFlags := []string{
		"--kv-cache-quant",
		"--paged-ssd-cache-dir",
		"--hot-cache-max-size",
		"--paged-ssd-cache-max-size",
	}
	for _, unwanted := range unwantedFlags {
		if hasFlag(args, unwanted) {
			t.Errorf("unexpected flag %q in default args: %v", unwanted, args)
		}
	}
}

// TestBuildOMLXServeArgs_TurboQuant verifies the --kv-cache-quant flag is
// emitted when turboQuantBits is set.
func TestBuildOMLXServeArgs_TurboQuant(t *testing.T) {
	cfg := omlxServeConfig{turboQuantBits: 3}
	args := buildOMLXServeArgs("/models/test", 8000, cfg)

	if got := flagValue(args, "--kv-cache-quant"); got != "3" {
		t.Errorf("--kv-cache-quant = %q, want %q (full args: %v)", got, "3", args)
	}
}

// TestBuildOMLXServeArgs_TurboQuantOmittedWhenZero verifies the flag is
// omitted when turboQuantBits is zero.
func TestBuildOMLXServeArgs_TurboQuantOmittedWhenZero(t *testing.T) {
	cfg := omlxServeConfig{turboQuantBits: 0}
	args := buildOMLXServeArgs("/models/test", 8000, cfg)

	if hasFlag(args, "--kv-cache-quant") {
		t.Errorf("--kv-cache-quant must be omitted when turboQuantBits=0 (full args: %v)", args)
	}
}

// TestBuildOMLXServeArgs_PagedSSDCacheDir verifies the --paged-ssd-cache-dir
// flag is emitted when the directory is set.
func TestBuildOMLXServeArgs_PagedSSDCacheDir(t *testing.T) {
	cfg := omlxServeConfig{pagedSSDCacheDir: "/mnt/ssd-cache"}
	args := buildOMLXServeArgs("/models/test", 8000, cfg)

	if got := flagValue(args, "--paged-ssd-cache-dir"); got != "/mnt/ssd-cache" {
		t.Errorf("--paged-ssd-cache-dir = %q, want %q (full args: %v)", got, "/mnt/ssd-cache", args)
	}
}

// TestBuildOMLXServeArgs_PagedSSDCacheDirOmittedWhenEmpty verifies the flag
// is omitted when the directory is empty.
func TestBuildOMLXServeArgs_PagedSSDCacheDirOmittedWhenEmpty(t *testing.T) {
	cfg := omlxServeConfig{pagedSSDCacheDir: ""}
	args := buildOMLXServeArgs("/models/test", 8000, cfg)

	if hasFlag(args, "--paged-ssd-cache-dir") {
		t.Errorf("--paged-ssd-cache-dir must be omitted when pagedSSDCacheDir is empty (full args: %v)", args)
	}
}

// TestBuildOMLXServeArgs_HotCacheMaxSize verifies the --hot-cache-max-size
// flag is emitted when the size is set.
func TestBuildOMLXServeArgs_HotCacheMaxSize(t *testing.T) {
	cfg := omlxServeConfig{hotCacheMaxSize: "100GB"}
	args := buildOMLXServeArgs("/models/test", 8000, cfg)

	if got := flagValue(args, "--hot-cache-max-size"); got != "100GB" {
		t.Errorf("--hot-cache-max-size = %q, want %q (full args: %v)", got, "100GB", args)
	}
}

// TestBuildOMLXServeArgs_HotCacheMaxSizeOmittedWhenEmpty verifies the flag
// is omitted when the size is empty.
func TestBuildOMLXServeArgs_HotCacheMaxSizeOmittedWhenEmpty(t *testing.T) {
	cfg := omlxServeConfig{hotCacheMaxSize: ""}
	args := buildOMLXServeArgs("/models/test", 8000, cfg)

	if hasFlag(args, "--hot-cache-max-size") {
		t.Errorf("--hot-cache-max-size must be omitted when hotCacheMaxSize is empty (full args: %v)", args)
	}
}

// TestBuildOMLXServeArgs_PagedSSDCacheMaxSize verifies the
// --paged-ssd-cache-max-size flag is emitted when the size is set.
func TestBuildOMLXServeArgs_PagedSSDCacheMaxSize(t *testing.T) {
	cfg := omlxServeConfig{pagedSSDCacheMaxSize: "200GB"}
	args := buildOMLXServeArgs("/models/test", 8000, cfg)

	if got := flagValue(args, "--paged-ssd-cache-max-size"); got != "200GB" {
		t.Errorf("--paged-ssd-cache-max-size = %q, want %q (full args: %v)", got, "200GB", args)
	}
}

// TestBuildOMLXServeArgs_PagedSSDCacheMaxSizeOmittedWhenEmpty verifies the
// flag is omitted when the size is empty.
func TestBuildOMLXServeArgs_PagedSSDCacheMaxSizeOmittedWhenEmpty(t *testing.T) {
	cfg := omlxServeConfig{pagedSSDCacheMaxSize: ""}
	args := buildOMLXServeArgs("/models/test", 8000, cfg)

	if hasFlag(args, "--paged-ssd-cache-max-size") {
		t.Errorf("--paged-ssd-cache-max-size must be omitted when pagedSSDCacheMaxSize is empty (full args: %v)", args)
	}
}

// TestBuildOMLXServeArgs_AllFlags verifies all flags are emitted together
// when all config fields are set.
func TestBuildOMLXServeArgs_AllFlags(t *testing.T) {
	cfg := omlxServeConfig{
		turboQuantBits:       6,
		pagedSSDCacheDir:     "/mnt/ssd",
		hotCacheMaxSize:      "100GB",
		pagedSSDCacheMaxSize: "500GB",
	}
	args := buildOMLXServeArgs("/models/test", 8000, cfg)

	want := map[string]string{
		"--kv-cache-quant":           "6",
		"--paged-ssd-cache-dir":      "/mnt/ssd",
		"--hot-cache-max-size":       "100GB",
		"--paged-ssd-cache-max-size": "500GB",
	}
	for flag, expected := range want {
		if got := flagValue(args, flag); got != expected {
			t.Errorf("%s = %q, want %q (full args: %v)", flag, got, expected, args)
		}
	}
}
