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
	"os"
	"path/filepath"
	"testing"
)

func TestNewMetalExecutor(t *testing.T) {
	executor := NewMetalExecutor("/opt/homebrew/bin/llama-server", "/models")

	if executor.llamaServerBin != "/opt/homebrew/bin/llama-server" {
		t.Errorf("llamaServerBin = %q, want %q", executor.llamaServerBin, "/opt/homebrew/bin/llama-server")
	}
	if executor.modelStorePath != "/models" {
		t.Errorf("modelStorePath = %q, want %q", executor.modelStorePath, "/models")
	}
	if executor.nextPort != 8080 {
		t.Errorf("nextPort = %d, want %d", executor.nextPort, 8080)
	}
}

func TestAllocatePort(t *testing.T) {
	executor := NewMetalExecutor("/bin/llama-server", "/models")

	ports := make([]int, 5)
	for i := range ports {
		ports[i] = executor.allocatePort()
	}

	expected := []int{8080, 8081, 8082, 8083, 8084}
	for i, want := range expected {
		if ports[i] != want {
			t.Errorf("allocatePort() call %d = %d, want %d", i+1, ports[i], want)
		}
	}
}

func TestAllocatePort_Sequential(t *testing.T) {
	executor := NewMetalExecutor("/bin/llama-server", "/models")

	first := executor.allocatePort()
	second := executor.allocatePort()

	if second != first+1 {
		t.Errorf("second port (%d) should be first port (%d) + 1", second, first)
	}
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

	executor := NewMetalExecutor("/bin/llama-server", tmpDir)

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
	executor := NewMetalExecutor("/bin/llama-server", tmpDir)

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

func TestStopProcess_InvalidPID(t *testing.T) {
	executor := NewMetalExecutor("/bin/llama-server", "/models")

	// PID 0 is the calling process's group — Signal will fail
	err := executor.StopProcess(-99999)
	if err == nil {
		t.Error("StopProcess with invalid PID should return error")
	}
}
