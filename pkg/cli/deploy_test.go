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

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

const testDefaultNamespace = "default"

func TestBuildInferenceService(t *testing.T) {
	tests := []struct {
		name          string
		opts          *deployOptions
		wantGPU       int32
		wantGPUMemory string
		wantContext   bool
		wantParallel  bool
	}{
		{
			name: "CPU defaults",
			opts: &deployOptions{
				name:      "test-model",
				namespace: testDefaultNamespace,
				replicas:  1,
				image:     "ghcr.io/ggml-org/llama.cpp:server",
				cpu:       "2",
				memory:    "4Gi",
			},
			wantGPU:       0,
			wantGPUMemory: "",
		},
		{
			name: "GPU with memory",
			opts: &deployOptions{
				name:      "gpu-model",
				namespace: "production",
				replicas:  2,
				image:     "ghcr.io/ggml-org/llama.cpp:server-cuda",
				cpu:       "4",
				memory:    "8Gi",
				gpu:       true,
				gpuCount:  2,
				gpuMemory: "16Gi",
			},
			wantGPU:       2,
			wantGPUMemory: "16Gi",
		},
		{
			name: "GPU without memory",
			opts: &deployOptions{
				name:      "gpu-model",
				namespace: testDefaultNamespace,
				replicas:  1,
				image:     "ghcr.io/ggml-org/llama.cpp:server-cuda",
				cpu:       "2",
				memory:    "4Gi",
				gpu:       true,
				gpuCount:  1,
			},
			wantGPU:       1,
			wantGPUMemory: "",
		},
		{
			name: "with context size",
			opts: &deployOptions{
				name:        "ctx-model",
				namespace:   testDefaultNamespace,
				replicas:    1,
				image:       "ghcr.io/ggml-org/llama.cpp:server",
				cpu:         "2",
				memory:      "4Gi",
				contextSize: 8192,
			},
			wantContext: true,
		},
		{
			name: "with parallel slots",
			opts: &deployOptions{
				name:          "parallel-model",
				namespace:     testDefaultNamespace,
				replicas:      1,
				image:         "ghcr.io/ggml-org/llama.cpp:server",
				cpu:           "2",
				memory:        "4Gi",
				parallelSlots: 4,
			},
			wantParallel: true,
		},
		{
			name: "zero context and parallel are omitted",
			opts: &deployOptions{
				name:          "minimal",
				namespace:     testDefaultNamespace,
				replicas:      1,
				image:         "ghcr.io/ggml-org/llama.cpp:server",
				cpu:           "2",
				memory:        "4Gi",
				contextSize:   0,
				parallelSlots: 0,
			},
			wantContext:  false,
			wantParallel: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isvc := buildInferenceService(tt.opts)

			if isvc.Name != tt.opts.name {
				t.Errorf("Name = %q, want %q", isvc.Name, tt.opts.name)
			}
			if isvc.Namespace != tt.opts.namespace {
				t.Errorf("Namespace = %q, want %q", isvc.Namespace, tt.opts.namespace)
			}
			if isvc.Spec.ModelRef != tt.opts.name {
				t.Errorf("ModelRef = %q, want %q", isvc.Spec.ModelRef, tt.opts.name)
			}
			if *isvc.Spec.Replicas != tt.opts.replicas {
				t.Errorf("Replicas = %d, want %d", *isvc.Spec.Replicas, tt.opts.replicas)
			}
			if isvc.Spec.Image != tt.opts.image {
				t.Errorf("Image = %q, want %q", isvc.Spec.Image, tt.opts.image)
			}
			if isvc.Spec.Resources.CPU != tt.opts.cpu {
				t.Errorf("CPU = %q, want %q", isvc.Spec.Resources.CPU, tt.opts.cpu)
			}
			if isvc.Spec.Resources.Memory != tt.opts.memory {
				t.Errorf("Memory = %q, want %q", isvc.Spec.Resources.Memory, tt.opts.memory)
			}

			// Endpoint defaults
			if isvc.Spec.Endpoint.Port != 8080 {
				t.Errorf("Port = %d, want 8080", isvc.Spec.Endpoint.Port)
			}
			if isvc.Spec.Endpoint.Path != "/v1/chat/completions" {
				t.Errorf("Path = %q, want /v1/chat/completions", isvc.Spec.Endpoint.Path)
			}
			if isvc.Spec.Endpoint.Type != "ClusterIP" {
				t.Errorf("Type = %q, want ClusterIP", isvc.Spec.Endpoint.Type)
			}

			// GPU
			if isvc.Spec.Resources.GPU != tt.wantGPU {
				t.Errorf("GPU = %d, want %d", isvc.Spec.Resources.GPU, tt.wantGPU)
			}
			if isvc.Spec.Resources.GPUMemory != tt.wantGPUMemory {
				t.Errorf("GPUMemory = %q, want %q", isvc.Spec.Resources.GPUMemory, tt.wantGPUMemory)
			}

			// ContextSize
			if tt.wantContext {
				if isvc.Spec.ContextSize == nil {
					t.Error("ContextSize is nil, want non-nil")
				} else if *isvc.Spec.ContextSize != tt.opts.contextSize {
					t.Errorf("ContextSize = %d, want %d", *isvc.Spec.ContextSize, tt.opts.contextSize)
				}
			} else {
				if isvc.Spec.ContextSize != nil {
					t.Errorf("ContextSize = %d, want nil", *isvc.Spec.ContextSize)
				}
			}

			// ParallelSlots
			if tt.wantParallel {
				if isvc.Spec.ParallelSlots == nil {
					t.Error("ParallelSlots is nil, want non-nil")
				} else if *isvc.Spec.ParallelSlots != tt.opts.parallelSlots {
					t.Errorf("ParallelSlots = %d, want %d", *isvc.Spec.ParallelSlots, tt.opts.parallelSlots)
				}
			} else {
				if isvc.Spec.ParallelSlots != nil {
					t.Errorf("ParallelSlots = %d, want nil", *isvc.Spec.ParallelSlots)
				}
			}
		})
	}
}

func TestSanitizeServiceName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple-name", "simple-name"},
		{"name.with.dots", "name-with-dots"},
		{"no-dots-here", "no-dots-here"},
		{"a.b.c.d", "a-b-c-d"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := sanitizeServiceName(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeServiceName(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsLocalSourcePath(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		expected bool
	}{
		{"file:// URL", "file:///mnt/models/model.gguf", true},
		{"absolute path", "/mnt/models/model.gguf", true},
		{"https URL", "https://huggingface.co/model.gguf", false},
		{"http URL", "http://example.com/model.gguf", false},
		{"relative path", "models/model.gguf", false},
		{"empty string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isLocalSourcePath(tt.source)
			if result != tt.expected {
				t.Errorf("isLocalSourcePath(%q) = %v, want %v", tt.source, result, tt.expected)
			}
		})
	}
}

func TestValidateLocalPath(t *testing.T) {
	// Create a temp .gguf file for valid-path tests
	tmpDir := t.TempDir()
	validFile := filepath.Join(tmpDir, "model.gguf")
	if err := os.WriteFile(validFile, []byte("fake-gguf"), 0644); err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}

	tests := []struct {
		name      string
		source    string
		wantError bool
		errSubstr string
	}{
		{"valid absolute path", validFile, false, ""},
		{"valid file:// URL", "file://" + validFile, false, ""},
		{"non-existent file", "/tmp/nonexistent-model-12345.gguf", true, "does not exist"},
		{"not .gguf extension", filepath.Join(tmpDir, "model.bin"), true, ".gguf extension"},
		{"directory instead of file", tmpDir + "/fake.gguf", true, "does not exist"},
		{"relative path", "relative/model.gguf", true, "must be absolute"},
	}

	// Create a directory to test the "is a directory" case
	dirPath := filepath.Join(tmpDir, "adir.gguf")
	if err := os.Mkdir(dirPath, 0755); err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	tests = append(tests, struct {
		name      string
		source    string
		wantError bool
		errSubstr string
	}{"path is a directory", dirPath, true, "directory"})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLocalPath(tt.source)
			if tt.wantError {
				if err == nil {
					t.Errorf("validateLocalPath(%q) = nil, want error containing %q", tt.source, tt.errSubstr)
				} else if tt.errSubstr != "" && !contains(err.Error(), tt.errSubstr) {
					t.Errorf("validateLocalPath(%q) error = %q, want substring %q", tt.source, err.Error(), tt.errSubstr)
				}
			} else {
				if err != nil {
					t.Errorf("validateLocalPath(%q) = %v, want nil", tt.source, err)
				}
			}
		})
	}
}

func TestApplyCatalogDefaults(t *testing.T) {
	catalogModel := &Model{
		Name:         "Test Model",
		Source:       "https://huggingface.co/test/model.gguf",
		Quantization: "Q4_K_M",
		GPULayers:    33,
		ContextSize:  8192,
		Resources: ResourceSpec{
			CPU:       "4",
			Memory:    "8Gi",
			GPUMemory: "6Gi",
		},
	}

	t.Run("applies all defaults when user has defaults", func(t *testing.T) {
		opts := &deployOptions{
			cpu:       "2",   // default
			memory:    "4Gi", // default
			gpuLayers: -1,    // default
		}
		applyCatalogDefaults(opts, catalogModel)

		if opts.modelSource != catalogModel.Source {
			t.Errorf("modelSource = %q, want %q", opts.modelSource, catalogModel.Source)
		}
		if opts.quantization != catalogModel.Quantization {
			t.Errorf("quantization = %q, want %q", opts.quantization, catalogModel.Quantization)
		}
		if opts.cpu != catalogModel.Resources.CPU {
			t.Errorf("cpu = %q, want %q", opts.cpu, catalogModel.Resources.CPU)
		}
		if opts.memory != catalogModel.Resources.Memory {
			t.Errorf("memory = %q, want %q", opts.memory, catalogModel.Resources.Memory)
		}
		if opts.gpuLayers != catalogModel.GPULayers {
			t.Errorf("gpuLayers = %d, want %d", opts.gpuLayers, catalogModel.GPULayers)
		}
		if opts.gpuMemory != catalogModel.Resources.GPUMemory {
			t.Errorf("gpuMemory = %q, want %q", opts.gpuMemory, catalogModel.Resources.GPUMemory)
		}
		if opts.contextSize != int32(catalogModel.ContextSize) {
			t.Errorf("contextSize = %d, want %d", opts.contextSize, catalogModel.ContextSize)
		}
	})

	t.Run("preserves user-specified values", func(t *testing.T) {
		opts := &deployOptions{
			quantization: "Q8_0",
			cpu:          "8",    // user-specified (not default "2")
			memory:       "16Gi", // user-specified (not default "4Gi")
			gpuLayers:    16,     // user-specified (not default -1)
			gpuMemory:    "24Gi",
			contextSize:  4096,
		}
		applyCatalogDefaults(opts, catalogModel)

		if opts.quantization != "Q8_0" {
			t.Errorf("quantization = %q, want Q8_0 (user-specified)", opts.quantization)
		}
		if opts.cpu != "8" {
			t.Errorf("cpu = %q, want 8 (user-specified)", opts.cpu)
		}
		if opts.memory != "16Gi" {
			t.Errorf("memory = %q, want 16Gi (user-specified)", opts.memory)
		}
		if opts.gpuLayers != 16 {
			t.Errorf("gpuLayers = %d, want 16 (user-specified)", opts.gpuLayers)
		}
		if opts.gpuMemory != "24Gi" {
			t.Errorf("gpuMemory = %q, want 24Gi (user-specified)", opts.gpuMemory)
		}
		if opts.contextSize != 4096 {
			t.Errorf("contextSize = %d, want 4096 (user-specified)", opts.contextSize)
		}
	})

	t.Run("skips context size when catalog model has zero", func(t *testing.T) {
		noCtxModel := &Model{
			Name:         "No Context",
			Source:       "https://example.com/model.gguf",
			Quantization: "Q4_K_M",
			GPULayers:    33,
			ContextSize:  0,
			Resources:    ResourceSpec{CPU: "2", Memory: "4Gi", GPUMemory: "4Gi"},
		}
		opts := &deployOptions{
			cpu:       "2",
			memory:    "4Gi",
			gpuLayers: -1,
		}
		applyCatalogDefaults(opts, noCtxModel)

		if opts.contextSize != 0 {
			t.Errorf("contextSize = %d, want 0 when catalog model has no context size", opts.contextSize)
		}
	})
}

func TestNewDeployCommand(t *testing.T) {
	cmd := NewDeployCommand()

	if cmd.Use != "deploy [MODEL_NAME]" {
		t.Errorf("Use = %q, want %q", cmd.Use, "deploy [MODEL_NAME]")
	}

	expectedFlags := map[string]string{
		"namespace":       "n",
		"source":          "s",
		"source-override": "",
		"format":          "",
		"quantization":    "q",
		"replicas":        "r",
		"gpu":             "",
		"accelerator":     "",
		"gpu-count":       "",
		"gpu-layers":      "",
		"gpu-memory":      "",
		"gpu-vendor":      "",
		"context":         "",
		"parallel":        "",
		"cpu":             "",
		"memory":          "",
		"image":           "",
		"wait":            "w",
		"timeout":         "",
	}

	for name, shorthand := range expectedFlags {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			t.Errorf("Missing flag %q", name)
			continue
		}
		if shorthand != "" && flag.Shorthand != shorthand {
			t.Errorf("Flag %q shorthand = %q, want %q", name, flag.Shorthand, shorthand)
		}
	}

	// Check defaults
	if f := cmd.Flags().Lookup("namespace"); f.DefValue != testDefaultNamespace {
		t.Errorf("namespace default = %q, want %q", f.DefValue, testDefaultNamespace)
	}
	if f := cmd.Flags().Lookup("replicas"); f.DefValue != "1" {
		t.Errorf("replicas default = %q, want %q", f.DefValue, "1")
	}
	if f := cmd.Flags().Lookup("gpu-vendor"); f.DefValue != "nvidia" {
		t.Errorf("gpu-vendor default = %q, want %q", f.DefValue, "nvidia")
	}
	if f := cmd.Flags().Lookup("cpu"); f.DefValue != "2" {
		t.Errorf("cpu default = %q, want %q", f.DefValue, "2")
	}
	if f := cmd.Flags().Lookup("memory"); f.DefValue != "4Gi" {
		t.Errorf("memory default = %q, want %q", f.DefValue, "4Gi")
	}
}

// contains is a helper since strings.Contains import isn't needed elsewhere
func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
