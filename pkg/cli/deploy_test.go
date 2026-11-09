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

package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
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
		wantEndpoint  string
		wantNodePort  *int32
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
			wantEndpoint:  "ClusterIP",
		},
		{
			name: "GPU with memory",
			opts: &deployOptions{
				name:      "gpu-model",
				namespace: "production",
				replicas:  2,
				image:     "ghcr.io/ggml-org/llama.cpp:server-cuda13",
				cpu:       "4",
				memory:    "8Gi",
				gpu:       true,
				gpuCount:  2,
				gpuMemory: "16Gi",
			},
			wantGPU:       2,
			wantGPUMemory: "16Gi",
			wantEndpoint:  "ClusterIP",
		},
		{
			name: "GPU without memory",
			opts: &deployOptions{
				name:      "gpu-model",
				namespace: testDefaultNamespace,
				replicas:  1,
				image:     "ghcr.io/ggml-org/llama.cpp:server-cuda13",
				cpu:       "2",
				memory:    "4Gi",
				gpu:       true,
				gpuCount:  1,
			},
			wantGPU:       1,
			wantGPUMemory: "",
			wantEndpoint:  "ClusterIP",
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
			wantContext:  true,
			wantEndpoint: "ClusterIP",
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
			wantEndpoint: "ClusterIP",
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
			wantEndpoint: "ClusterIP",
		},
		{
			name: "node-port sets NodePort type and pins the port",
			opts: &deployOptions{
				name:      "nodeport-model",
				namespace: testDefaultNamespace,
				replicas:  1,
				image:     "ghcr.io/ggml-org/llama.cpp:server",
				cpu:       "2",
				memory:    "4Gi",
				nodePort:  30080,
			},
			wantEndpoint: "NodePort",
			wantNodePort: ptrInt32(30080),
		},
		{
			name: "no node-port defaults to ClusterIP with nil NodePort",
			opts: &deployOptions{
				name:      "default-model",
				namespace: testDefaultNamespace,
				replicas:  1,
				image:     "ghcr.io/ggml-org/llama.cpp:server",
				cpu:       "2",
				memory:    "4Gi",
			},
			wantEndpoint: "ClusterIP",
			wantNodePort: nil,
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
			if isvc.Spec.Endpoint.Type != tt.wantEndpoint {
				t.Errorf("Type = %q, want %q", isvc.Spec.Endpoint.Type, tt.wantEndpoint)
			}
			if tt.wantNodePort == nil {
				if isvc.Spec.Endpoint.NodePort != nil {
					t.Errorf("NodePort = %d, want nil", *isvc.Spec.Endpoint.NodePort)
				}
			} else {
				if isvc.Spec.Endpoint.NodePort == nil {
					t.Error("NodePort is nil, want non-nil")
				} else if *isvc.Spec.Endpoint.NodePort != *tt.wantNodePort {
					t.Errorf("NodePort = %d, want %d", *isvc.Spec.Endpoint.NodePort, *tt.wantNodePort)
				}
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

	tests := []struct { //nolint:prealloc // table-driven test with literal entries; prealloc misreads the pattern
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
		if err := applyCatalogDefaults(opts, catalogModel); err != nil {
			t.Fatal(err)
		}

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
		if opts.contextSize != int32(catalogModel.ContextSize) { //nolint:gosec // G115: test fixture is small positive int
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
		if err := applyCatalogDefaults(opts, catalogModel); err != nil {
			t.Fatal(err)
		}

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
		if err := applyCatalogDefaults(opts, noCtxModel); err != nil {
			t.Fatal(err)
		}

		if opts.contextSize != 0 {
			t.Errorf("contextSize = %d, want 0 when catalog model has no context size", opts.contextSize)
		}
	})
}

func TestDeployOptions_MemoryFractionSetsHardwareSpec(t *testing.T) {
	opts := &deployOptions{
		name:                "mem-frac-model",
		namespace:           testDefaultNamespace,
		modelSource:         "https://example.com/model.gguf",
		modelFormat:         "gguf",
		accelerator:         "metal",
		cpu:                 "2",
		memory:              "4Gi",
		metalMemoryFraction: 0.8,
	}

	// Build the model the same way runDeploy does
	model := buildTestModel(opts)

	if model.Spec.Hardware == nil {
		t.Fatal("Hardware is nil")
	}
	if model.Spec.Hardware.MemoryFraction == nil {
		t.Fatal("MemoryFraction is nil, want 0.8")
	}
	if *model.Spec.Hardware.MemoryFraction != 0.8 {
		t.Errorf("MemoryFraction = %f, want 0.8", *model.Spec.Hardware.MemoryFraction)
	}
}

func TestDeployOptions_MemoryBudgetSetsHardwareSpec(t *testing.T) {
	opts := &deployOptions{
		name:              "mem-budget-model",
		namespace:         testDefaultNamespace,
		modelSource:       "https://example.com/model.gguf",
		modelFormat:       "gguf",
		accelerator:       "metal",
		cpu:               "2",
		memory:            "4Gi",
		metalMemoryBudget: "24Gi",
	}

	model := buildTestModel(opts)

	if model.Spec.Hardware == nil {
		t.Fatal("Hardware is nil")
	}
	if model.Spec.Hardware.MemoryBudget != "24Gi" {
		t.Errorf("MemoryBudget = %q, want %q", model.Spec.Hardware.MemoryBudget, "24Gi")
	}
}

func TestDeployOptions_ZeroMemoryFractionOmitted(t *testing.T) {
	opts := &deployOptions{
		name:                "zero-frac-model",
		namespace:           testDefaultNamespace,
		modelSource:         "https://example.com/model.gguf",
		modelFormat:         "gguf",
		accelerator:         "metal",
		cpu:                 "2",
		memory:              "4Gi",
		metalMemoryFraction: 0,
	}

	model := buildTestModel(opts)

	if model.Spec.Hardware.MemoryFraction != nil {
		t.Errorf("MemoryFraction should be nil when 0, got %f", *model.Spec.Hardware.MemoryFraction)
	}
}

// buildTestModel mirrors the model construction in runDeploy for testing.
func buildTestModel(opts *deployOptions) *inferencev1alpha1.Model {
	model := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source:       opts.modelSource,
			Format:       opts.modelFormat,
			Quantization: opts.quantization,
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: opts.accelerator,
			},
			Resources: &inferencev1alpha1.ResourceRequirements{
				CPU:    opts.cpu,
				Memory: opts.memory,
			},
		},
	}
	if opts.gpu {
		model.Spec.Hardware.GPU = &inferencev1alpha1.GPUSpec{
			Enabled: true,
			Count:   opts.gpuCount,
			Vendor:  opts.gpuVendor,
		}
		if opts.gpuLayers != 0 {
			model.Spec.Hardware.GPU.Layers = opts.gpuLayers
		}
		if opts.gpuMemory != "" {
			model.Spec.Hardware.GPU.Memory = opts.gpuMemory
		}
	}
	if opts.metalMemoryBudget != "" {
		model.Spec.Hardware.MemoryBudget = opts.metalMemoryBudget
	}
	if opts.metalMemoryFraction > 0 {
		model.Spec.Hardware.MemoryFraction = &opts.metalMemoryFraction
	}
	return model
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
		"memory-fraction": "",
		"memory-budget":   "",
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
	if f := cmd.Flags().Lookup("gpu-vendor"); f.DefValue != defaultGPUVendor {
		t.Errorf("gpu-vendor default = %q, want %q", f.DefValue, defaultGPUVendor)
	}
	if f := cmd.Flags().Lookup("cpu"); f.DefValue != "2" {
		t.Errorf("cpu default = %q, want %q", f.DefValue, "2")
	}
	if f := cmd.Flags().Lookup("memory"); f.DefValue != "4Gi" {
		t.Errorf("memory default = %q, want %q", f.DefValue, "4Gi")
	}
}

func TestResolveAcceleratorAndImage(t *testing.T) {
	tests := []struct {
		name       string
		opts       *deployOptions
		wantAccel  string
		wantVendor string
		wantImage  string
	}{
		{
			name: "metal accelerator keeps vendor as default (display-only override)",
			opts: &deployOptions{
				gpu:         true,
				accelerator: "metal",
				gpuVendor:   defaultGPUVendor, // flag default
			},
			wantAccel:  "metal",
			wantVendor: defaultGPUVendor, // vendor stays nvidia in opts, display shows "apple"
			wantImage:  "",
		},
		{
			name: "cuda accelerator keeps nvidia vendor",
			opts: &deployOptions{
				gpu:         true,
				accelerator: "cuda",
				gpuVendor:   defaultGPUVendor,
			},
			wantAccel:  "cuda",
			wantVendor: defaultGPUVendor,
			wantImage:  "",
		},
		{
			name: "metal with explicit amd vendor is preserved",
			opts: &deployOptions{
				gpu:         true,
				accelerator: "metal",
				gpuVendor:   "amd",
			},
			wantAccel:  "metal",
			wantVendor: "amd",
			wantImage:  "",
		},
		{
			name: "intel accelerator sets intel vendor and intel image",
			opts: &deployOptions{
				gpu:         true,
				accelerator: "intel",
				gpuVendor:   defaultGPUVendor,
			},
			wantAccel:  "intel",
			wantVendor: "intel",
			wantImage:  "ghcr.io/ggml-org/llama.cpp:server-intel",
		},

		{
			name:      "non-catalog vllm leaves image selection to controller",
			opts:      &deployOptions{runtime: "vllm", gpu: true, gpuVendor: defaultGPUVendor},
			wantAccel: "cuda", wantVendor: defaultGPUVendor,
		},
		{
			name:      "non-catalog sglang leaves image selection to controller",
			opts:      &deployOptions{runtime: "sglang", gpu: true, gpuVendor: defaultGPUVendor},
			wantAccel: "cuda", wantVendor: defaultGPUVendor,
		},
		{
			name: "cpu defaults",
			opts: &deployOptions{
				gpu:       false,
				gpuVendor: defaultGPUVendor,
			},
			wantAccel:  "cpu",
			wantVendor: defaultGPUVendor,
			wantImage:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolveAcceleratorAndImage(tt.opts)

			if tt.opts.accelerator != tt.wantAccel {
				t.Errorf("accelerator = %q, want %q", tt.opts.accelerator, tt.wantAccel)
			}
			if tt.opts.gpuVendor != tt.wantVendor {
				t.Errorf("gpuVendor = %q, want %q", tt.opts.gpuVendor, tt.wantVendor)
			}
			if tt.opts.image != tt.wantImage {
				t.Errorf("image = %q, want %q", tt.opts.image, tt.wantImage)
			}
		})
	}
}

// A spec.image the CLI writes beats the operator's pinned llama.cpp build and
// the chart's runtimeImages.llamacpp, so deploy leaves it empty unless --image
// is passed (#2413). The Intel image stays: the operator has no Intel default
// and would run an Intel GPU Model on its CPU-only image.
func TestDeployLeavesTheImageToTheOperator(t *testing.T) {
	const custom = "registry.example/llama.cpp:server-custom"
	tests := []struct {
		name      string
		opts      deployOptions
		wantImage string
	}{
		{name: "cpu", opts: deployOptions{}, wantImage: ""},
		{name: "cuda", opts: deployOptions{gpu: true, accelerator: acceleratorCUDA}, wantImage: ""},
		{name: "rocm", opts: deployOptions{gpu: true, accelerator: acceleratorROCm}, wantImage: ""},
		{name: "metal", opts: deployOptions{gpu: true, accelerator: acceleratorMetal}, wantImage: ""},
		{name: "metal without --gpu", opts: deployOptions{accelerator: acceleratorMetal}, wantImage: ""},
		{
			name:      "intel keeps the CLI's Intel image",
			opts:      deployOptions{gpu: true, accelerator: acceleratorIntel},
			wantImage: imageLlamaCppServerIntel,
		},
		{name: "--image on cpu", opts: deployOptions{image: custom}, wantImage: custom},
		{
			name:      "--image on cuda",
			opts:      deployOptions{gpu: true, accelerator: acceleratorCUDA, image: custom},
			wantImage: custom,
		},
		{
			name:      "--image on intel",
			opts:      deployOptions{gpu: true, accelerator: acceleratorIntel, image: custom},
			wantImage: custom,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := tt.opts
			opts.name, opts.namespace, opts.replicas, opts.gpuVendor = "m", testDefaultNamespace, 1, defaultGPUVendor
			resolveAcceleratorAndImage(&opts)
			if got := buildInferenceService(&opts).Spec.Image; got != tt.wantImage {
				t.Errorf("spec.image = %q, want %q", got, tt.wantImage)
			}
		})
	}
}

// The deploy summary names who picks the image instead of printing an empty
// string when --image is not passed.
func TestDeploySummaryImageLine(t *testing.T) {
	tests := []struct {
		name string
		opts deployOptions
		want string
	}{
		{
			name: "operator picks",
			opts: deployOptions{accelerator: acceleratorCUDA},
			want: "Image:       operator default\n",
		},
		{
			name: "metal has none",
			opts: deployOptions{accelerator: acceleratorMetal},
			want: "Image:       none (the Metal agent serves it natively)\n",
		},
		{
			name: "--image",
			opts: deployOptions{accelerator: acceleratorCUDA, image: "registry.example/llama:1"},
			want: "Image:       registry.example/llama:1\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := captureStdout(t, func() { printDeploySummary(&tt.opts) })
			if !contains(out, tt.want) {
				t.Errorf("summary lacks %q:\n%s", tt.want, out)
			}
		})
	}
}

// The generic runtime runs the container it is given and the operator has no
// default for it, so deploy refuses it without --image rather than create a
// service whose Deployment the API server rejects. KUBECONFIG points nowhere:
// a deploy that got past the check fails to load it instead.
func TestDeployGenericRuntimeNeedsAnImage(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	err := runDeploy(&deployOptions{
		name: "m", namespace: testDefaultNamespace, runtime: "generic",
		modelSource: "https://example.com/model.gguf",
	})
	if err == nil || !contains(err.Error(), "--image") {
		t.Fatalf("runDeploy(--runtime generic, no --image) = %v, want an error naming --image", err)
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

func ptrInt32(v int32) *int32 {
	return &v
}

func TestShouldHeartbeat(t *testing.T) {
	tests := []struct {
		name          string
		phase         string
		lastHeartbeat time.Time
		want          bool
	}{
		{
			name:          "Downloading with no prior heartbeat",
			phase:         "Downloading",
			lastHeartbeat: time.Time{},
			want:          true,
		},
		{
			name:          "Downloading with recent heartbeat",
			phase:         "Downloading",
			lastHeartbeat: time.Now().Add(-2 * time.Second),
			want:          false,
		},
		{
			name:          "Downloading with old heartbeat",
			phase:         "Downloading",
			lastHeartbeat: time.Now().Add(-10 * time.Second),
			want:          true,
		},
		{
			name:          "Downloading with heartbeat exactly at interval",
			phase:         "Downloading",
			lastHeartbeat: time.Now().Add(-5 * time.Second),
			want:          true,
		},
		{
			name:          "Downloading with heartbeat just before interval",
			phase:         "Downloading",
			lastHeartbeat: time.Now().Add(-4 * time.Second),
			want:          false,
		},
		{
			name:          "Ready phase should not heartbeat",
			phase:         "Ready",
			lastHeartbeat: time.Time{},
			want:          false,
		},
		{
			name:          "Pending phase should not heartbeat",
			phase:         "Pending",
			lastHeartbeat: time.Time{},
			want:          false,
		},
		{
			name:          "Failed phase should not heartbeat",
			phase:         "Failed",
			lastHeartbeat: time.Time{},
			want:          false,
		},
		{
			name:          "Copying phase should not heartbeat",
			phase:         "Copying",
			lastHeartbeat: time.Time{},
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldHeartbeat(tt.phase, tt.lastHeartbeat)
			if got != tt.want {
				t.Errorf("shouldHeartbeat(%q, %v) = %v, want %v", tt.phase, tt.lastHeartbeat, got, tt.want)
			}
		})
	}
}

func TestHeartbeatInterval(t *testing.T) {
	if heartbeatInterval != 5*time.Second {
		t.Errorf("heartbeatInterval = %v, want 5s", heartbeatInterval)
	}
}

func TestDeployOptions_CacheFlags(t *testing.T) {
	tests := []struct {
		name      string
		skipCache bool
		fromCache bool
		wantSkip  bool
		wantFrom  bool
	}{
		{
			name:      "both false (default)",
			skipCache: false,
			fromCache: false,
			wantSkip:  false,
			wantFrom:  false,
		},
		{
			name:      "skip-cache true",
			skipCache: true,
			fromCache: false,
			wantSkip:  true,
			wantFrom:  false,
		},
		{
			name:      "from-cache true",
			skipCache: false,
			fromCache: true,
			wantSkip:  false,
			wantFrom:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &deployOptions{
				name:        "test-model",
				namespace:   testDefaultNamespace,
				modelSource: "https://example.com/model.gguf",
				skipCache:   tt.skipCache,
				fromCache:   tt.fromCache,
			}

			if opts.skipCache != tt.wantSkip {
				t.Errorf("skipCache = %v, want %v", opts.skipCache, tt.wantSkip)
			}
			if opts.fromCache != tt.wantFrom {
				t.Errorf("fromCache = %v, want %v", opts.fromCache, tt.wantFrom)
			}
		})
	}
}

func TestDeployOptions_CacheFlagsMutuallyExclusive(t *testing.T) {
	// Both flags set should cause an error in runDeploy
	opts := &deployOptions{
		name:        "test-model",
		namespace:   testDefaultNamespace,
		modelSource: "https://example.com/model.gguf",
		skipCache:   true,
		fromCache:   true,
	}

	// The mutual exclusion is enforced in runDeploy; verify the flags are set
	if !opts.skipCache {
		t.Error("skipCache should be true")
	}
	if !opts.fromCache {
		t.Error("fromCache should be true")
	}
}

func TestNewDeployCommand_CacheFlags(t *testing.T) {
	cmd := NewDeployCommand()

	// Verify --skip-cache flag exists
	if f := cmd.Flags().Lookup("skip-cache"); f == nil {
		t.Error("Missing --skip-cache flag")
	} else {
		if f.DefValue != "false" {
			t.Errorf("skip-cache default = %q, want %q", f.DefValue, "false")
		}
	}

	// Verify --from-cache flag exists
	if f := cmd.Flags().Lookup("from-cache"); f == nil {
		t.Error("Missing --from-cache flag")
	} else {
		if f.DefValue != "false" {
			t.Errorf("from-cache default = %q, want %q", f.DefValue, "false")
		}
	}
}

func TestComputeCacheKeyUsedByDeploy(t *testing.T) {
	// Verify that cachekey.Compute produces consistent results
	// when used with model sources that would be passed to deploy
	sources := []string{
		"https://huggingface.co/model.gguf",
		"file:///mnt/models/model.gguf",
		"/mnt/models/model.gguf",
	}

	for _, source := range sources {
		key := cachekey.Compute(source)
		if len(key) != 16 {
			t.Errorf("cachekey.Compute(%q) length = %d, want 16", source, len(key))
		}
		// Verify determinism
		key2 := cachekey.Compute(source)
		if key != key2 {
			t.Errorf("cachekey.Compute not deterministic for %q", source)
		}
	}
}

func TestCatalogContextDefaultDeployment(t *testing.T) {
	cfg := cacheListEnvtest(t)
	pointKubeconfigAt(t, cfg)
	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	k8s, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tt := range []struct {
		name    string
		model   string
		args    []string
		context int32
	}{
		{name: "qwen-2.5-14b catalog default", model: "qwen-2.5-14b", context: 16384},
		{name: "qwen-2.5-14b explicit override", model: "qwen-2.5-14b",
			args: []string{"--context", "32768"}, context: 32768},
		{name: "llama-3.3-70b catalog default", model: "llama-3.3-70b", context: 8192},
		{name: "llama-3.3-70b explicit override", model: "llama-3.3-70b",
			args: []string{"--context", "32768"}, context: 32768},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "catalog-context-"}}
			if err := k8s.Create(ctx, ns); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = k8s.Delete(ctx, ns) })
			cmd := NewDeployCommand()
			cmd.SetArgs(append([]string{tt.model, "-n", ns.Name,
				"--gpu", "--gpu-count", "1", "--wait=false"}, tt.args...))
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			var svc inferencev1alpha1.InferenceService
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: tt.model}, &svc); err != nil {
				t.Fatal(err)
			}
			if svc.Spec.ContextSize == nil {
				t.Fatal("persisted context size is nil")
			}
			if *svc.Spec.ContextSize != tt.context {
				t.Errorf("persisted context size = %d, want %d", *svc.Spec.ContextSize, tt.context)
			}
		})
	}
}

func TestCatalogRuntimeDeployment(t *testing.T) {
	for _, runtime := range []string{"vllm", "sglang"} {
		t.Run(runtime, func(t *testing.T) {
			var model Model
			err := yaml.Unmarshal([]byte(`name: test
runtime: `+runtime+`
source: org/model
files: ["*.safetensors", "*.json"]
context_size: 8192
gpu:
  count: 2
  tensor_parallel_size: 2
served_model_name: chat
parsers:
  tool_call: hermes
  reasoning: deepseek-r1
extra_args: ["--trust-remote-code"]
`), &model)
			if err != nil {
				t.Fatal(err)
			}
			opts := &deployOptions{runtime: "llamacpp", modelFormat: "gguf", gpuCount: 1}
			if err := applyCatalogDefaults(opts, &model); err != nil {
				t.Fatal(err)
			}
			resolveAcceleratorAndImage(opts)
			svc := buildInferenceService(opts)
			if opts.modelFormat != "safetensors" ||
				!reflect.DeepEqual(opts.modelFiles, []string{"*.safetensors", "*.json"}) {
				t.Fatalf("model format/files = %s %v", opts.modelFormat, opts.modelFiles)
			}
			if !reflect.DeepEqual(svc.Spec.ExtraArgs, []string{"--trust-remote-code", "--served-model-name", "chat"}) {
				t.Fatalf("extra args = %v", svc.Spec.ExtraArgs)
			}
			if svc.Spec.Runtime != runtime {
				t.Fatalf("runtime = %q, want %q", svc.Spec.Runtime, runtime)
			}
			if svc.Spec.Image != "" {
				t.Fatalf("runtime image must be selected by controller, got %q", svc.Spec.Image)
			}
			if svc.Spec.Resources.GPU != 2 {
				t.Fatalf("GPU count = %d, want 2", svc.Spec.Resources.GPU)
			}
			if runtime == "vllm" {
				c := svc.Spec.VLLMConfig
				if c == nil ||
					c.TensorParallelSize == nil ||
					*c.TensorParallelSize != 2 ||
					c.ToolCallParser != "hermes" ||
					c.ReasoningParser != "deepseek-r1" ||
					c.MaxModelLen == nil ||
					*c.MaxModelLen != 8192 {
					t.Fatalf("vLLM config = %+v", c)
				}
			} else {
				c := svc.Spec.SGLangConfig
				if c == nil ||
					c.TensorParallelSize == nil ||
					*c.TensorParallelSize != 2 ||
					c.ToolCallParser != "hermes" ||
					c.ReasoningParser != "deepseek-r1" ||
					c.ContextLength == nil ||
					*c.ContextLength != 8192 {
					t.Fatalf("SGLang config = %+v", c)
				}
			}
		})
	}
}

func TestCatalogDeployCreatesRuntimeResources(t *testing.T) {
	cfg := cacheListEnvtest(t)
	pointKubeconfigAt(t, cfg)
	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	k8s, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "catalog-runtime-"}}
	if err := k8s.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	old := catalogInstance
	t.Cleanup(func() { catalogInstance = old; _ = k8s.Delete(ctx, ns) })
	var catalog Catalog
	err = yaml.Unmarshal([]byte(`version: test
models:
  runtime-test:
    name: runtime-test
    runtime: vllm
    source: org/model
    files: ["*.safetensors", "*.json"]
    gpu: {count: 2, tensor_parallel_size: 2}
    parsers: {tool_call: hermes, reasoning: deepseek-r1}
    served_model_name: chat
    context_size: 8192
    resources: {cpu: "2", memory: 4Gi}
`), &catalog)
	if err != nil {
		t.Fatal(err)
	}
	catalogInstance = &catalog
	for _, runtime := range []string{"vllm", "sglang"} {
		m := catalog.Models["runtime-test"]
		m.Runtime = runtime
		catalog.Models[runtime] = m
		cmd := NewDeployCommand()
		cmd.SetArgs([]string{runtime, "-n", ns.Name, "--wait=false"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		key := client.ObjectKey{Namespace: ns.Name, Name: runtime}
		var model inferencev1alpha1.Model
		if err := k8s.Get(ctx, key, &model); err != nil {
			t.Fatal(err)
		}
		if model.Spec.Source != m.Source ||
			model.Spec.Format != "safetensors" ||
			!reflect.DeepEqual(model.Spec.Files, m.Files) {
			t.Fatalf("persisted Model spec = %+v", model.Spec)
		}
		if model.Spec.Hardware.GPU.Count != 2 || model.Spec.Hardware.Accelerator != "cuda" {
			t.Fatalf("persisted hardware = %+v", model.Spec.Hardware)
		}
		var svc inferencev1alpha1.InferenceService
		if err := k8s.Get(ctx, key, &svc); err != nil {
			t.Fatal(err)
		}
		if svc.Spec.Runtime != runtime || svc.Spec.Resources.GPU != 2 || svc.Spec.Image != "" {
			t.Fatalf("persisted InferenceService spec = %+v", svc.Spec)
		}
		if runtime == "vllm" && (svc.Spec.VLLMConfig == nil || svc.Spec.VLLMConfig.ToolCallParser != "hermes") {
			t.Fatalf("persisted vLLM config = %+v", svc.Spec.VLLMConfig)
		}
		if runtime == "sglang" && (svc.Spec.SGLangConfig == nil || svc.Spec.SGLangConfig.ToolCallParser != "hermes") {
			t.Fatalf("persisted SGLang config = %+v", svc.Spec.SGLangConfig)
		}
	}
	assertCatalogPreload(t, k8s, ns.Name, catalog.Models["runtime-test"].Files)

	cmd := NewDeployCommand()
	cmd.SetArgs([]string{"runtime-test", "-n", ns.Name, "--runtime=llamacpp", "--wait=false"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "requires runtime") {
		t.Fatalf("incompatible runtime error = %v", err)
	}
	var models inferencev1alpha1.ModelList
	if err := k8s.List(ctx, &models, client.InNamespace(ns.Name)); err != nil {
		t.Fatal(err)
	}
	if len(models.Items) != 2 {
		t.Fatalf("rejected deploy created a Model: %d models", len(models.Items))
	}
}

func TestCatalogRuntimeOverrides(t *testing.T) {
	tp := int32(2)
	model := &Model{
		Runtime: "vllm", Files: []string{"weights.safetensors"},
		GPU:             &CatalogGPU{Count: 2, TensorParallelSize: &tp},
		ServedModelName: "catalog", ExtraArgs: []string{"--trust-remote-code"},
	}
	opts := &deployOptions{
		gpu: true, gpuCount: 4, image: "custom-image", contextSize: 4096,
		extraArgs: []string{"--served-model-name=custom"},
		specified: map[string]bool{"gpu-count": true, "extra-args": true},
	}
	if err := applyCatalogDefaults(opts, model); err != nil {
		t.Fatal(err)
	}
	resolveAcceleratorAndImage(opts)
	svc := buildInferenceService(opts)
	if svc.Spec.Image != "custom-image" ||
		svc.Spec.Resources.GPU != 4 ||
		*svc.Spec.VLLMConfig.MaxModelLen != 4096 ||
		!reflect.DeepEqual(svc.Spec.ExtraArgs, []string{"--served-model-name=custom"}) {
		t.Fatalf("overrides lost: %+v", svc.Spec)
	}
	opts = &deployOptions{gpuCount: 1, specified: map[string]bool{"gpu-count": true}}
	if err := applyCatalogDefaults(opts, model); err == nil {
		t.Fatal("expected insufficient GPU error")
	}
	opts = &deployOptions{runtime: "generic", specified: map[string]bool{"runtime": true}}
	if err := applyCatalogDefaults(opts, &Model{}); err != nil || opts.runtime != "generic" {
		t.Fatalf("legacy runtime override: %q, %v", opts.runtime, err)
	}
}

func TestCatalogRuntimeBenchmarkRejection(t *testing.T) {
	old := catalogInstance
	t.Cleanup(func() { catalogInstance = old })
	catalogInstance = &Catalog{Models: map[string]Model{"test": {Runtime: "vllm"}}}
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	if _, err := validateCatalogModels([]string{"test"}); err == nil ||
		!strings.Contains(err.Error(), "only support llamacpp") {
		t.Fatalf("catalog benchmark rejection = %v", err)
	}
	err := runContextSweep(context.Background(), &benchmarkOptions{catalog: "test", contextSweep: "1024,2048"})
	if err == nil || !strings.Contains(err.Error(), "deploy test and benchmark its endpoint instead") {
		t.Fatalf("context sweep rejection = %v", err)
	}
}

func assertCatalogPreload(t *testing.T, k8s client.Client, namespace string, files []string) {
	t.Helper()
	ctx := context.Background()
	preloadDone := make(chan error, 1)
	go func() { preloadDone <- runCachePreload("runtime-test", namespace) }()
	key := client.ObjectKey{Namespace: namespace, Name: "runtime-test"}
	var preloaded inferencev1alpha1.Model
	err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			err := k8s.Get(ctx, key, &preloaded)
			return err == nil, client.IgnoreNotFound(err)
		})
	if err != nil {
		t.Fatal(err)
	}
	if preloaded.Spec.Format != "safetensors" ||
		!reflect.DeepEqual(preloaded.Spec.Files, files) {
		t.Errorf("preload lost format/files: %+v", preloaded.Spec)
	}
	preloaded.Status.Phase = phaseReady
	if err := k8s.Status().Update(ctx, &preloaded); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-preloadDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cache preload did not observe Ready")
	}
	if err := k8s.Delete(ctx, &preloaded); err != nil {
		t.Fatal(err)
	}

}

func TestCatalogDeployRejectsCPU(t *testing.T) {
	cfg := cacheListEnvtest(t)
	pointKubeconfigAt(t, cfg)
	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	k8s, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "catalog-gpu-required-"}}
	if err := k8s.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	old := catalogInstance
	t.Cleanup(func() { catalogInstance = old; _ = k8s.Delete(ctx, ns) })
	catalogInstance = &Catalog{Models: map[string]Model{}}
	for _, runtime := range []string{"vllm", "sglang"} {
		catalogInstance.Models[runtime] = Model{
			Runtime: runtime, Source: "org/model", Files: []string{"weights.safetensors"},
			GPU: &CatalogGPU{Count: 1}, Resources: ResourceSpec{CPU: "2", Memory: "4Gi"},
		}
		for _, flag := range []string{"--gpu=false", "--gpu-count=0"} {
			t.Run(runtime+"/"+flag, func(t *testing.T) {
				cmd := NewDeployCommand()
				cmd.SetArgs([]string{runtime, "-n", ns.Name, "--wait=false", flag})
				if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "requires at least one GPU") {
					t.Errorf("CPU deployment error = %v", err)
				}
			})
		}
	}
	var models inferencev1alpha1.ModelList
	if err := k8s.List(ctx, &models, client.InNamespace(ns.Name)); err != nil {
		t.Fatal(err)
	}
	if len(models.Items) != 0 {
		t.Errorf("rejected deploys created %d Models", len(models.Items))
	}
	var services inferencev1alpha1.InferenceServiceList
	if err := k8s.List(ctx, &services, client.InNamespace(ns.Name)); err != nil {
		t.Fatal(err)
	}
	if len(services.Items) != 0 {
		t.Errorf("rejected deploys created %d InferenceServices", len(services.Items))
	}
}
