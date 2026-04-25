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

func TestStopProcess_InvalidPID(t *testing.T) {
	executor := NewMetalExecutor("/bin/llama-server", "/models", newNopLogger())

	// PID 0 is the calling process's group — Signal will fail
	err := executor.StopProcess(-99999)
	if err == nil {
		t.Error("StopProcess with invalid PID should return error")
	}
}
