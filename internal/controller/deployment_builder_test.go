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

package controller

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestGPUResourceNameForSpec(t *testing.T) {
	cases := []struct {
		name     string
		gpu      *inferencev1alpha1.GPUSpec
		expected corev1.ResourceName
	}{
		{
			name:     "nil GPU spec defaults to nvidia",
			gpu:      nil,
			expected: nvidiaGPUResourceName,
		},
		{
			name:     "empty GPU spec defaults to nvidia",
			gpu:      &inferencev1alpha1.GPUSpec{},
			expected: nvidiaGPUResourceName,
		},
		{
			name:     "vendor nvidia maps to nvidia.com/gpu",
			gpu:      &inferencev1alpha1.GPUSpec{Vendor: "nvidia"},
			expected: nvidiaGPUResourceName,
		},
		{
			name:     "vendor amd maps to amd.com/gpu",
			gpu:      &inferencev1alpha1.GPUSpec{Vendor: "amd"},
			expected: amdGPUResourceName,
		},
		{
			name:     "vendor intel maps to gpu.intel.com/i915",
			gpu:      &inferencev1alpha1.GPUSpec{Vendor: "intel"},
			expected: intelGPUResourceNameI915,
		},
		{
			name:     "vendor is case-insensitive",
			gpu:      &inferencev1alpha1.GPUSpec{Vendor: "AMD"},
			expected: amdGPUResourceName,
		},
		{
			name:     "unknown vendor falls back to nvidia",
			gpu:      &inferencev1alpha1.GPUSpec{Vendor: "mystery"},
			expected: nvidiaGPUResourceName,
		},
		{
			name:     "vendor amd with runtime rocm resolves the shared dri-render resource",
			gpu:      &inferencev1alpha1.GPUSpec{Vendor: "amd", Runtime: "rocm"},
			expected: vulkanDRIResourceName,
		},
		{
			name:     "rocm runtime is case-insensitive",
			gpu:      &inferencev1alpha1.GPUSpec{Vendor: "AMD", Runtime: " ROCm "},
			expected: vulkanDRIResourceName,
		},
		{
			name:     "vendor amd with empty runtime maps to amd.com/gpu",
			gpu:      &inferencev1alpha1.GPUSpec{Vendor: "amd", Runtime: ""},
			expected: amdGPUResourceName,
		},
		{
			name:     "vendor amd with runtime vulkan maps to devic.es/dri-render",
			gpu:      &inferencev1alpha1.GPUSpec{Vendor: "amd", Runtime: "vulkan"},
			expected: vulkanDRIResourceName,
		},
		{
			name:     "vulkan runtime is case-insensitive",
			gpu:      &inferencev1alpha1.GPUSpec{Vendor: "amd", Runtime: "Vulkan"},
			expected: vulkanDRIResourceName,
		},
		{
			name:     "vulkan runtime only applies to amd vendor",
			gpu:      &inferencev1alpha1.GPUSpec{Vendor: "nvidia", Runtime: "vulkan"},
			expected: nvidiaGPUResourceName,
		},
		{
			name: "explicit ResourceName overrides vendor mapping",
			gpu: &inferencev1alpha1.GPUSpec{
				Vendor:       "amd",
				ResourceName: "squat.ai/dri-render",
			},
			expected: corev1.ResourceName("squat.ai/dri-render"),
		},
		{
			name: "explicit ResourceName wins over the vulkan default",
			gpu: &inferencev1alpha1.GPUSpec{
				Vendor:       "amd",
				Runtime:      "vulkan",
				ResourceName: "amd.com/gpu",
			},
			expected: amdGPUResourceName,
		},
		{
			name: "explicit ResourceName wins over the rocm default",
			gpu: &inferencev1alpha1.GPUSpec{
				Vendor:       "amd",
				Runtime:      "rocm",
				ResourceName: "amd.com/gpu",
			},
			expected: amdGPUResourceName,
		},
		{
			name: "explicit ResourceName wins even when vendor is unset",
			gpu: &inferencev1alpha1.GPUSpec{
				ResourceName: "nvidia.com/gpu.shared",
			},
			expected: corev1.ResourceName("nvidia.com/gpu.shared"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &inferencev1alpha1.Model{
				Spec: inferencev1alpha1.ModelSpec{
					Hardware: &inferencev1alpha1.HardwareSpec{GPU: tc.gpu},
				},
			}

			if model.Spec.Hardware.GPU == nil {
				// Use a Hardware with no GPU at all for the nil cases so the
				// helper exercises the outer "GPU == nil" branch.
				model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{}
			}

			got := gpuResourceNameForSpec(model)
			if got != tc.expected {
				t.Fatalf("gpuResourceNameForSpec() = %q, want %q", got, tc.expected)
			}
		})
	}
}

func amdModel(runtime string) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Hardware: &inferencev1alpha1.HardwareSpec{
				GPU: &inferencev1alpha1.GPUSpec{
					Vendor:  "amd",
					Runtime: runtime,
				},
			},
		},
	}
}

func TestResolveRuntimeImage(t *testing.T) {
	stockLlamaCpp := (&LlamaCppBackend{}).DefaultImage()

	cases := []struct {
		name     string
		backend  RuntimeBackend
		model    *inferencev1alpha1.Model
		expected string
	}{
		{
			name:     "llamacpp amd vulkan selects the pinned vulkan image",
			backend:  &LlamaCppBackend{},
			model:    amdModel("vulkan"),
			expected: llamaCppVulkanImage,
		},
		{
			name:     "llamacpp amd vulkan is case-insensitive",
			backend:  &LlamaCppBackend{},
			model:    amdModel("Vulkan"),
			expected: llamaCppVulkanImage,
		},
		{
			name:     "llamacpp amd rocm resolves the pinned ROCm image",
			backend:  &LlamaCppBackend{},
			model:    amdModel("rocm"),
			expected: llamaCppROCmImage,
		},
		{
			name:     "llamacpp amd rocm is case-insensitive",
			backend:  &LlamaCppBackend{},
			model:    amdModel("ROCm"),
			expected: llamaCppROCmImage,
		},
		{
			name:     "llamacpp amd empty runtime keeps the stock image",
			backend:  &LlamaCppBackend{},
			model:    amdModel(""),
			expected: stockLlamaCpp,
		},
		{
			name:    "llamacpp nvidia with GPU not enabled keeps the stock image (vulkan runtime string)",
			backend: &LlamaCppBackend{},
			model: &inferencev1alpha1.Model{
				Spec: inferencev1alpha1.ModelSpec{
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{Vendor: "nvidia", Runtime: "vulkan"},
					},
				},
			},
			expected: stockLlamaCpp,
		},
		{
			name:    "llamacpp nvidia with GPU not enabled keeps the stock image (rocm runtime string)",
			backend: &LlamaCppBackend{},
			model: &inferencev1alpha1.Model{
				Spec: inferencev1alpha1.ModelSpec{
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{Vendor: "nvidia", Runtime: "rocm"},
					},
				},
			},
			expected: stockLlamaCpp,
		},
		{
			name:     "non-llamacpp backend ignores vulkan and uses its default image",
			backend:  &VLLMBackend{},
			model:    amdModel("vulkan"),
			expected: (&VLLMBackend{}).DefaultImage(),
		},
		{
			name:     "nil model uses backend default",
			backend:  &LlamaCppBackend{},
			model:    &inferencev1alpha1.Model{},
			expected: stockLlamaCpp,
		},
		{
			name:     "sglang no GPU vendor falls back to CUDA image",
			backend:  &SGLangBackend{},
			model:    &inferencev1alpha1.Model{},
			expected: sglangCUDAImage,
		},
		{
			name:    "sglang NVIDIA vendor picks CUDA image",
			backend: &SGLangBackend{},
			model: &inferencev1alpha1.Model{
				Spec: inferencev1alpha1.ModelSpec{
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{Vendor: "nvidia"},
					},
				},
			},
			expected: sglangCUDAImage,
		},
		{
			name:    "sglang AMD vendor picks ROCm image",
			backend: &SGLangBackend{},
			model: &inferencev1alpha1.Model{
				Spec: inferencev1alpha1.ModelSpec{
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{Vendor: "amd"},
					},
				},
			},
			expected: sglangROCmImage,
		},
		{
			name:    "sglang AMD vendor uppercase maps to ROCm image",
			backend: &SGLangBackend{},
			model: &inferencev1alpha1.Model{
				Spec: inferencev1alpha1.ModelSpec{
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{Vendor: "AMD"},
					},
				},
			},
			expected: sglangROCmImage,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRuntimeImage(tc.backend, tc.model, nil); got != tc.expected {
				t.Fatalf("resolveRuntimeImage() = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestShouldProtectFromDisruption(t *testing.T) {
	pTrue := func() *bool { b := true; return &b }
	pFalse := func() *bool { b := false; return &b }

	cases := []struct {
		name     string
		isvc     *inferencev1alpha1.InferenceService
		expected bool
	}{
		{
			name: "default: not Ready → protect",
			isvc: &inferencev1alpha1.InferenceService{
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: "Creating"},
			},
			expected: true,
		},
		{
			name: "default: Ready → no protect",
			isvc: &inferencev1alpha1.InferenceService{
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: PhaseReady},
			},
			expected: false,
		},
		{
			name: "default: Failed → protect",
			isvc: &inferencev1alpha1.InferenceService{
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: PhaseFailed},
			},
			expected: true,
		},
		{
			name: "ProtectStartup false → never protect",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Disruption: &inferencev1alpha1.DisruptionSpec{
						ProtectStartup: pFalse(),
					},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: "Creating"},
			},
			expected: false,
		},
		{
			name: "ProtectAlways true → always protect even when Ready",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Disruption: &inferencev1alpha1.DisruptionSpec{
						ProtectAlways: pTrue(),
					},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: PhaseReady},
			},
			expected: true,
		},
		{
			name: "ProtectAlways true → always protect even when not Ready",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Disruption: &inferencev1alpha1.DisruptionSpec{
						ProtectAlways: pTrue(),
					},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: "Creating"},
			},
			expected: true,
		},
		{
			name: "ProtectAlways false + ProtectStartup true + not Ready → protect",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Disruption: &inferencev1alpha1.DisruptionSpec{
						ProtectAlways:  pFalse(),
						ProtectStartup: pTrue(),
					},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: "Creating"},
			},
			expected: true,
		},
		{
			name: "ProtectAlways false + ProtectStartup true + Ready → no protect",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Disruption: &inferencev1alpha1.DisruptionSpec{
						ProtectAlways:  pFalse(),
						ProtectStartup: pTrue(),
					},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: PhaseReady},
			},
			expected: false,
		},
		{
			name: "ProtectAlways true overrides ProtectStartup false",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Disruption: &inferencev1alpha1.DisruptionSpec{
						ProtectAlways:  pTrue(),
						ProtectStartup: pFalse(),
					},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: PhaseReady},
			},
			expected: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldProtectFromDisruption(tc.isvc)
			if got != tc.expected {
				t.Fatalf("shouldProtectFromDisruption() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestBuildPodAnnotations(t *testing.T) {
	pTrue := func() *bool { b := true; return &b }
	pFalse := func() *bool { b := false; return &b }

	cases := []struct {
		name       string
		isvc       *inferencev1alpha1.InferenceService
		emitScrape bool
		port       int32
		expected   map[string]string
	}{
		{
			name: "not Ready, no user annotations → add disruption annotation",
			isvc: &inferencev1alpha1.InferenceService{
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: "Creating"},
			},
			expected: map[string]string{"karpenter.sh/do-not-disrupt": "true"},
		},
		{
			name: "Ready, no user annotations → no disruption annotation",
			isvc: &inferencev1alpha1.InferenceService{
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: PhaseReady},
			},
			expected: nil,
		},
		{
			name: "not Ready, user has other annotations → merge with disruption",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					PodAnnotations: map[string]string{"foo": "bar"},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: "Creating"},
			},
			expected: map[string]string{
				"foo":                         "bar",
				"karpenter.sh/do-not-disrupt": "true",
			},
		},
		{
			name: "user set karpenter annotation → user value wins",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					PodAnnotations: map[string]string{"karpenter.sh/do-not-disrupt": "false"},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: "Creating"},
			},
			expected: map[string]string{"karpenter.sh/do-not-disrupt": "false"},
		},
		{
			name: "ProtectStartup false → no disruption annotation even when not Ready",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Disruption: &inferencev1alpha1.DisruptionSpec{
						ProtectStartup: pFalse(),
					},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: "Creating"},
			},
			expected: nil,
		},
		{
			name: "ProtectAlways true → disruption annotation even when Ready",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Disruption: &inferencev1alpha1.DisruptionSpec{
						ProtectAlways: pTrue(),
					},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: PhaseReady},
			},
			expected: map[string]string{"karpenter.sh/do-not-disrupt": "true"},
		},
		{
			name: "emitScrape off → no prometheus.io annotations",
			isvc: &inferencev1alpha1.InferenceService{
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: PhaseReady},
			},
			emitScrape: false,
			expected:   nil,
		},
		{
			name: "emitScrape on → emits the resolved port it is handed",
			isvc: &inferencev1alpha1.InferenceService{
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: PhaseReady},
			},
			emitScrape: true,
			port:       8000,
			expected: map[string]string{
				"prometheus.io/scrape": "true",
				"prometheus.io/path":   "/metrics",
				"prometheus.io/port":   "8000",
			},
		},
		{
			// Regression guard: the port is emitted verbatim, never a hardcoded
			// 8080. Resolution itself is covered end-to-end in the
			// constructDeploymentForCache tests (per-runtime default + spec.containerPort).
			name: "emitScrape on, non-8080 port → emitted verbatim (not hardcoded)",
			isvc: &inferencev1alpha1.InferenceService{
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: PhaseReady},
			},
			emitScrape: true,
			port:       30000,
			expected: map[string]string{
				"prometheus.io/scrape": "true",
				"prometheus.io/path":   "/metrics",
				"prometheus.io/port":   "30000",
			},
		},
		{
			name: "emitScrape on, user set prometheus.io/port → user value wins",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					PodAnnotations: map[string]string{"prometheus.io/port": "9000"},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{Phase: PhaseReady},
			},
			emitScrape: true,
			port:       8000,
			expected: map[string]string{
				"prometheus.io/scrape": "true",
				"prometheus.io/path":   "/metrics",
				"prometheus.io/port":   "9000",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildPodAnnotations(tc.isvc, tc.emitScrape, tc.port)
			if len(got) != len(tc.expected) {
				t.Fatalf("buildPodAnnotations() = %v, want %v", got, tc.expected)
			}
			for k, v := range tc.expected {
				if got[k] != v {
					t.Fatalf("buildPodAnnotations()[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestGPUTolerationKeyForSpec(t *testing.T) {
	cases := []struct {
		name     string
		gpu      *inferencev1alpha1.GPUSpec
		expected string
	}{
		{
			name:     "nil GPU defaults to nvidia resource name",
			gpu:      nil,
			expected: string(nvidiaGPUResourceName),
		},
		{
			name:     "vendor amd toleration key falls back to amd.com/gpu",
			gpu:      &inferencev1alpha1.GPUSpec{Vendor: "amd"},
			expected: string(amdGPUResourceName),
		},
		{
			name:     "vendor intel toleration key falls back to gpu.intel.com/i915",
			gpu:      &inferencev1alpha1.GPUSpec{Vendor: "intel"},
			expected: string(intelGPUResourceNameI915),
		},
		{
			name: "explicit TolerationKey wins over ResourceName",
			gpu: &inferencev1alpha1.GPUSpec{
				Vendor:        "amd",
				ResourceName:  "amd.com/gpu",
				TolerationKey: "nvidia.com/gpu",
			},
			expected: "nvidia.com/gpu",
		},
		{
			name: "TolerationKey falls back to ResourceName when unset",
			gpu: &inferencev1alpha1.GPUSpec{
				Vendor:       "amd",
				ResourceName: "squat.ai/dri-render",
			},
			expected: "squat.ai/dri-render",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &inferencev1alpha1.Model{
				Spec: inferencev1alpha1.ModelSpec{
					Hardware: &inferencev1alpha1.HardwareSpec{GPU: tc.gpu},
				},
			}

			if model.Spec.Hardware.GPU == nil {
				model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{}
			}

			got := gpuTolerationKeyForSpec(model)
			if got != tc.expected {
				t.Fatalf("gpuTolerationKeyForSpec() = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestDirectoryOrientedRuntime(t *testing.T) {
	cases := map[string]bool{
		RuntimeVLLM: true, RuntimeSGLANG: true,
		"": false, "tgi": false, "generic": false, "personaplex": false,
	}
	for rt, want := range cases {
		if got := directoryOrientedRuntime(rt); got != want {
			t.Errorf("directoryOrientedRuntime(%q) = %v, want %v", rt, got, want)
		}
	}
}

func TestServedModelPath(t *testing.T) {
	sf := func(format string) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{Format: format}}
	}
	isvcRT := func(rt string) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: rt}}
	}
	dirSC := modelStorageConfig{modelPath: "/models/k/model.safetensors", stagedDir: "/models/k"}
	fileSC := modelStorageConfig{modelPath: "/models/k/model.gguf"} // single-file: stagedDir empty

	cases := []struct {
		name     string
		isvc     *inferencev1alpha1.InferenceService
		model    *inferencev1alpha1.Model
		sc       modelStorageConfig
		expected string
	}{
		{"sglang + safetensors + multi-file -> directory", isvcRT(RuntimeSGLANG), sf("safetensors"), dirSC, "/models/k"},
		{"vllm + safetensors + multi-file -> directory", isvcRT(RuntimeVLLM), sf("safetensors"), dirSC, "/models/k"},
		{"vllm + pytorch + multi-file -> directory", isvcRT(RuntimeVLLM), sf("pytorch"), dirSC, "/models/k"},
		{"sglang + gguf + multi-file -> primary file", isvcRT(RuntimeSGLANG), sf("gguf"), dirSC, "/models/k/model.safetensors"},
		{"sglang + unset format + multi-file -> primary file", isvcRT(RuntimeSGLANG), sf(""), dirSC, "/models/k/model.safetensors"},
		{"llamacpp + safetensors + multi-file -> primary file", isvcRT(""), sf("safetensors"), dirSC, "/models/k/model.safetensors"},
		{"sglang + safetensors + single-file -> primary file", isvcRT(RuntimeSGLANG), sf("safetensors"), fileSC, "/models/k/model.gguf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := servedModelPath(tc.isvc, tc.model, tc.sc); got != tc.expected {
				t.Errorf("servedModelPath = %q, want %q", got, tc.expected)
			}
		})
	}
}

// TestResolveRuntimeImageNVIDIAAndOverrides covers the #1197 additions: the
// NVIDIA-GPU llamacpp divert to the CUDA image (the :server default is
// CPU-only) and the fleet-level --runtime-images override that wins over
// every built-in divergence.
func TestResolveRuntimeImageNVIDIAAndOverrides(t *testing.T) {
	nvidiaGPUModel := func(vendor string) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					GPU: &inferencev1alpha1.GPUSpec{Enabled: true, Vendor: vendor},
				},
			},
		}
	}

	t.Run("llamacpp with an enabled NVIDIA GPU diverts to the CUDA image", func(t *testing.T) {
		if got := resolveRuntimeImage(&LlamaCppBackend{}, nvidiaGPUModel("nvidia"), nil); got != llamaCppCUDAImage {
			t.Fatalf("got %q, want %q", got, llamaCppCUDAImage)
		}
	})

	t.Run("llamacpp with an enabled vendorless GPU defaults to NVIDIA and diverts", func(t *testing.T) {
		if got := resolveRuntimeImage(&LlamaCppBackend{}, nvidiaGPUModel(""), nil); got != llamaCppCUDAImage {
			t.Fatalf("got %q, want %q", got, llamaCppCUDAImage)
		}
	})

	t.Run("llamacpp with no GPU section keeps the CPU image", func(t *testing.T) {
		got := resolveRuntimeImage(&LlamaCppBackend{}, &inferencev1alpha1.Model{}, nil)
		if got != (&LlamaCppBackend{}).DefaultImage() {
			t.Fatalf("got %q, want the CPU default", got)
		}
	})

	t.Run("override wins over the built-in AMD divert", func(t *testing.T) {
		overrides := map[string]string{"llamacpp": "mirror.local/llamacpp:airgap"}
		if got := resolveRuntimeImage(&LlamaCppBackend{}, amdModel("vulkan"), overrides); got != "mirror.local/llamacpp:airgap" {
			t.Fatalf("got %q, want the override", got)
		}
	})

	t.Run("override wins over a backend default", func(t *testing.T) {
		overrides := map[string]string{"vllm": "mirror.local/vllm:airgap"}
		if got := resolveRuntimeImage(&VLLMBackend{}, &inferencev1alpha1.Model{}, overrides); got != "mirror.local/vllm:airgap" {
			t.Fatalf("got %q, want the override", got)
		}
	})

	t.Run("an override for a different backend does not leak", func(t *testing.T) {
		overrides := map[string]string{"vllm": "mirror.local/vllm:airgap"}
		got := resolveRuntimeImage(&TGIBackend{}, &inferencev1alpha1.Model{}, overrides)
		if got != (&TGIBackend{}).DefaultImage() {
			t.Fatalf("got %q, want the TGI default", got)
		}
	})
}

func TestParseRuntimeImageOverrides(t *testing.T) {
	t.Run("empty means none", func(t *testing.T) {
		got, err := ParseRuntimeImageOverrides("  ")
		if err != nil || got != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
		}
	})
	t.Run("parses multiple entries with spaces and case-folds keys", func(t *testing.T) {
		got, err := ParseRuntimeImageOverrides(" VLLM=a/b:1 , tgi=c/d:2 ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["vllm"] != "a/b:1" || got["tgi"] != "c/d:2" {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("rejects unknown runtimes and malformed pairs", func(t *testing.T) {
		if _, err := ParseRuntimeImageOverrides("triton=x"); err == nil {
			t.Fatal("expected error for unknown runtime")
		}
		if _, err := ParseRuntimeImageOverrides("vllm="); err == nil {
			t.Fatal("expected error for empty image")
		}
		if _, err := ParseRuntimeImageOverrides("not-a-pair"); err == nil {
			t.Fatal("expected error for missing separator")
		}
	})
}

func TestBuildDeployment_MountsDraftModelAndEmitsMd(t *testing.T) {
	n := func(i int32) *int32 { return &i }
	// Status.CacheKey must be set on both models: buildCachedStorageConfig
	// (internal/controller/model_storage.go) only mounts the "model-cache"
	// volume when effectiveModelCacheKey(model) is non-empty, which resolves
	// from Status.CacheKey (or multi-file staging, neither of which applies
	// here). Every sibling perService-cache test in this package sets it the
	// same way; the brief's literal snippet omitted it.
	target := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Format: "gguf", Source: "s3://bucket/target.gguf"},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: "target-cache-key"},
	}
	draft := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "dspark", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Format: "gguf", Source: "s3://bucket/dspark.gguf"},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: "dspark-cache-key"},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "target",
			Runtime:  "llamacpp",
			SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
				Type: "draft-dspark", DraftModelRef: "dspark", NDraftMax: n(3),
			},
		},
	}

	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	dep := r.constructDeploymentForCache(isvc, target, resolveEffectiveModelCache(isvc, target), draft, 1, "", "")

	pod := dep.Spec.Template.Spec
	var cacheVolumes int
	for _, v := range pod.Volumes {
		if v.Name == "model-cache" {
			cacheVolumes++
		}
	}
	if cacheVolumes != 1 {
		t.Errorf("model-cache volumes = %d, want exactly 1", cacheVolumes)
	}
	// Exactly three, not "at least two": the shared cache needs ONE
	// model-cache-prep (an idempotent chown of the one mount), plus one
	// downloader per model. "At least two" was satisfied by the four
	// duplicate-named containers the apiserver rejected outright.
	if len(pod.InitContainers) != 3 {
		t.Errorf("initContainers = %v, want [model-cache-prep model-downloader draft-model-downloader]",
			containerNames(pod.InitContainers))
	}
	joined := strings.Join(pod.Containers[0].Args, " ")
	if !strings.Contains(joined, "-md ") {
		t.Errorf("argv missing -md; got %s", joined)
	}
	if !strings.Contains(joined, "--spec-type draft-dspark") {
		t.Errorf("argv missing --spec-type draft-dspark; got %s", joined)
	}
}

func containerNames(containers []corev1.Container) []string {
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		names = append(names, c.Name)
	}
	return names
}

// argValue returns the value following flag in an argv, or "" when absent.
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// pathIsUnder reports whether p lies at or below dir.
func pathIsUnder(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

func cachedGGUFModel(name, cacheKey string) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Format: "gguf", Source: "s3://bucket/" + name + ".gguf"},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: cacheKey},
	}
}

// uncachedGGUFModel has no Status.CacheKey, so effectiveModelCacheKey is empty
// and its storage falls to buildEmptyDirStorageConfig, so the "model-storage"
// emptyDir, also mounted at /models.
func uncachedGGUFModel(name string) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Format: "gguf", Source: "s3://bucket/" + name + ".gguf"},
	}
}

func pvcGGUFModel(name, claim, file string) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Format: "gguf", Source: "pvc://" + claim + "/" + file},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: name + "-cache-key"},
	}
}

// TestConstructDeployment_DraftPodIsWellFormed asserts the pod-validity
// invariants at the level the apiserver judges them, on the built Deployment,
// rather than at mergeStorageConfigs, where a hand-written fixture can invent
// resources no storage builder ever emits (a "draft-model-downloader" init
// container, say) and pass while production is rejected on every apply.
//
// Every source combination is covered because each one collides differently:
// cached/cached shares a volume, cached/uncached collides on mountPath only,
// pvc/pvc collides on volume name with DIFFERENT claims, and pvc/cached
// collides on neither but still on init container names.
func TestConstructDeployment_DraftPodIsWellFormed(t *testing.T) {
	cases := []struct {
		name          string
		target, draft *inferencev1alpha1.Model
		wantVolumes   int
		wantInitNames []string
		wantModelPath string
		wantDraftPath string
	}{
		{
			// The design's central case: one cache PVC, mounted once, the two
			// models separated by their effectiveModelCacheKey subdirectories.
			name:          "cached target, cached draft",
			target:        cachedGGUFModel("target", "target-key"),
			draft:         cachedGGUFModel("dspark", "dspark-key"),
			wantVolumes:   1,
			wantInitNames: []string{"model-cache-prep", "model-downloader", "draft-model-downloader"},
			wantModelPath: "/models/target-key/target.gguf",
			wantDraftPath: "/models/dspark-key/dspark.gguf",
		},
		{
			// Differently-named volumes, both wanting /models.
			name:          "cached target, uncached draft",
			target:        cachedGGUFModel("target", "target-key"),
			draft:         uncachedGGUFModel("dspark"),
			wantVolumes:   2,
			wantInitNames: []string{"model-cache-prep", "model-downloader", "draft-model-downloader"},
			wantModelPath: "/models/target-key/target.gguf",
			wantDraftPath: "/draft/models/default-dspark.gguf",
		},
		{
			// Same volume name, same mount path, DIFFERENT claims, and the
			// same basename inside each: name-only dedup dropped the draft's
			// claim and pointed -md at a file in the target's (#1528 I2).
			name:          "pvc target, pvc draft",
			target:        pvcGGUFModel("target", "target-claim", "model.gguf"),
			draft:         pvcGGUFModel("dspark", "draft-claim", "model.gguf"),
			wantVolumes:   2,
			wantInitNames: nil,
			wantModelPath: "/model-source/model.gguf",
			wantDraftPath: "/draft/model-source/model.gguf",
		},
		{
			// No volume or path collision at all; only the init container
			// names collide with nothing, and must still be unambiguous.
			name:          "pvc target, cached draft",
			target:        pvcGGUFModel("target", "target-claim", "target.gguf"),
			draft:         cachedGGUFModel("dspark", "dspark-key"),
			wantVolumes:   2,
			wantInitNames: []string{"draft-model-cache-prep", "draft-model-downloader"},
			wantModelPath: "/model-source/target.gguf",
			wantDraftPath: "/models/dspark-key/dspark.gguf",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: tc.target.Name,
					Runtime:  "llamacpp",
					SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
						Type: "draft-dspark", DraftModelRef: tc.draft.Name,
					},
				},
			}
			r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
			pod := r.constructDeploymentForCache(isvc, tc.target, resolveEffectiveModelCache(isvc, tc.target), tc.draft, 1, "", "").Spec.Template.Spec

			// Container names are unique across initContainers AND containers:
			// Kubernetes rejects the whole Deployment otherwise.
			seenContainer := map[string]bool{}
			for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
				if seenContainer[c.Name] {
					t.Errorf("duplicate container name %q (init=%v)", c.Name, containerNames(pod.InitContainers))
				}
				seenContainer[c.Name] = true
			}
			if got := containerNames(pod.InitContainers); !equalStrings(got, tc.wantInitNames) {
				t.Errorf("initContainers = %v, want %v", got, tc.wantInitNames)
			}

			// Volume names are unique, and every mount resolves to one.
			volumes := map[string]bool{}
			for _, v := range pod.Volumes {
				if volumes[v.Name] {
					t.Errorf("duplicate volume name %q", v.Name)
				}
				volumes[v.Name] = true
			}
			if len(pod.Volumes) != tc.wantVolumes {
				t.Errorf("volumes = %d %v, want %d", len(pod.Volumes), volumeNames(pod.Volumes), tc.wantVolumes)
			}

			// Mount paths are unique within each container.
			var mountPaths []string
			for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
				seenPath := map[string]bool{}
				for _, m := range c.VolumeMounts {
					if seenPath[m.MountPath] {
						t.Errorf("container %q mounts %q twice", c.Name, m.MountPath)
					}
					seenPath[m.MountPath] = true
					if !volumes[m.Name] {
						t.Errorf("container %q mounts volume %q, which the pod does not declare", c.Name, m.Name)
					}
				}
			}
			for _, m := range pod.Containers[0].VolumeMounts {
				mountPaths = append(mountPaths, m.MountPath)
			}

			// Both weights must be reachable in the SERVING container: a path
			// that no mount covers is a file the runtime cannot open, and a
			// draft path that silently resolves inside the target's volume is
			// worse than a rejected pod.
			args := pod.Containers[0].Args
			gotModel, gotDraft := argValue(args, "--model"), argValue(args, "-md")
			if gotModel != tc.wantModelPath {
				t.Errorf("-m = %q, want %q", gotModel, tc.wantModelPath)
			}
			if gotDraft != tc.wantDraftPath {
				t.Errorf("-md = %q, want %q", gotDraft, tc.wantDraftPath)
			}
			if gotDraft == gotModel {
				t.Errorf("-md == -m (%q): the draft resolves to the target's own weights", gotDraft)
			}
			for _, p := range []struct{ flag, path string }{{"--model", gotModel}, {"-md", gotDraft}} {
				covered := false
				for _, mp := range mountPaths {
					if pathIsUnder(p.path, mp) {
						covered = true
						break
					}
				}
				if !covered {
					t.Errorf("%s %q lies under no mountPath of the serving container %v", p.flag, p.path, mountPaths)
				}
			}
		})
	}
}

// TestConstructDeployment_DraftStorageHonoursTheInitGate covers I3: the draft
// block used to sit outside the NeedsModelInit/skipModelInit gate, so a
// llamacpp-router service (which documents that it does NOT auto-mount /models)
// or one with skipModelInit had a cache volume and two init containers injected
// behind its back.
func TestConstructDeployment_DraftStorageHonoursTheInitGate(t *testing.T) {
	target := cachedGGUFModel("target", "target-key")
	draft := cachedGGUFModel("dspark", "dspark-key")

	newISvc := func(runtime string, skip *bool) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:      "target",
				Runtime:       runtime,
				SkipModelInit: skip,
				SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
					Type: "draft-dspark", DraftModelRef: "dspark",
				},
			},
		}
	}
	skip := true

	for _, tc := range []struct {
		name string
		isvc *inferencev1alpha1.InferenceService
	}{
		{"llamacpp-router does not auto-mount /models", newISvc("llamacpp-router", nil)},
		{"skipModelInit opts out entirely", newISvc("llamacpp", &skip)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
			pod := r.constructDeploymentForCache(tc.isvc, target, resolveEffectiveModelCache(tc.isvc, target), draft, 1, "", "").Spec.Template.Spec

			if len(pod.InitContainers) != 0 {
				t.Errorf("initContainers = %v, want none", containerNames(pod.InitContainers))
			}
			if len(pod.Volumes) != 0 {
				t.Errorf("volumes = %v, want none", volumeNames(pod.Volumes))
			}
			if len(pod.Containers[0].VolumeMounts) != 0 {
				t.Errorf("volumeMounts = %d, want none", len(pod.Containers[0].VolumeMounts))
			}
			if md := argValue(pod.Containers[0].Args, "-md"); md != "" {
				t.Errorf("-md = %q, want it omitted: no draft storage was mounted", md)
			}
		})
	}
}

func volumeNames(volumes []corev1.Volume) []string {
	names := make([]string, 0, len(volumes))
	for _, v := range volumes {
		names = append(names, v.Name)
	}
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// archAwareBackend is a test RuntimeBackend that declares a single supported
// architecture, exercising the architecture-aware placement path.
type archAwareBackend struct{}

func (b *archAwareBackend) ContainerName() string { return "arch-aware" }
func (b *archAwareBackend) DefaultImage() string  { return "example/arch-aware:amd64-only" }
func (b *archAwareBackend) DefaultPort() int32    { return 8080 }
func (b *archAwareBackend) BuildArgs(*inferencev1alpha1.InferenceService, *inferencev1alpha1.Model, string, string, int32) []string {
	return nil
}
func (b *archAwareBackend) BuildProbes(int32) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
	return nil, nil, nil
}
func (b *archAwareBackend) NeedsModelInit() bool             { return false }
func (b *archAwareBackend) SupportedArchitectures() []string { return []string{"amd64"} }

// archAffinityTerms returns the kubernetes.io/arch In values in the pod's
// required node affinity, or nil when none is present.
func archAffinityTerms(pod corev1.PodSpec) []string {
	if pod.Affinity == nil || pod.Affinity.NodeAffinity == nil ||
		pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return nil
	}
	for _, term := range pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == corev1.LabelArchStable && expr.Operator == corev1.NodeSelectorOpIn {
				return expr.Values
			}
		}
	}
	return nil
}

func TestConstructDeployment_ArchAffinity(t *testing.T) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Format: "gguf", Source: "s3://bucket/m.gguf"},
	}

	// A backend that declares a supported architecture must get a
	// kubernetes.io/arch nodeAffinity when the operator chose the image.
	t.Run("operator image gets arch affinity", func(t *testing.T) {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: "llamacpp"},
		}
		r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
		// Force the arch-aware backend by stubbing resolveBackend.
		orig := resolveBackend
		resolveBackend = func(*inferencev1alpha1.InferenceService) RuntimeBackend { return &archAwareBackend{} }
		defer func() { resolveBackend = orig }()

		pod := r.constructDeploymentForCache(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template.Spec
		got := archAffinityTerms(pod)
		if !equalStrings(got, []string{"amd64"}) {
			t.Errorf("arch affinity values = %v, want [amd64]", got)
		}
	})

	// A user-supplied spec.image must bypass the constraint entirely.
	t.Run("user image bypasses arch affinity", func(t *testing.T) {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: "llamacpp",
				Image:   "custom/whatever:latest",
			},
		}
		r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
		orig := resolveBackend
		resolveBackend = func(*inferencev1alpha1.InferenceService) RuntimeBackend { return &archAwareBackend{} }
		defer func() { resolveBackend = orig }()

		pod := r.constructDeploymentForCache(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template.Spec
		if got := archAffinityTerms(pod); got != nil {
			t.Errorf("arch affinity values = %v, want none for user-supplied image", got)
		}
	})

	// A backend with no declared architectures must not add a constraint.
	t.Run("no declared arch means no constraint", func(t *testing.T) {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: "llamacpp"},
		}
		r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
		orig := resolveBackend
		resolveBackend = func(*inferencev1alpha1.InferenceService) RuntimeBackend { return &LlamaCppBackend{} }
		defer func() { resolveBackend = orig }()

		pod := r.constructDeploymentForCache(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template.Spec
		if got := archAffinityTerms(pod); got != nil {
			t.Errorf("arch affinity values = %v, want none for multi-arch backend", got)
		}
	})

	// nodeSelectorTerms are ORed, so the arch requirement must be ANDed into
	// each of the user's existing terms as a matchExpression. Appending a term
	// instead would make the user's own pin one of two alternatives and let the
	// pod schedule anywhere of the right architecture (#1583).
	t.Run("arch requirement is ANDed into the user's own affinity", func(t *testing.T) {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: "llamacpp",
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{{
								MatchExpressions: []corev1.NodeSelectorRequirement{{
									Key:      "nodepool",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"gpu-dedicated"},
								}},
							}},
						},
					},
				},
			},
		}
		r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
		orig := resolveBackend
		resolveBackend = func(*inferencev1alpha1.InferenceService) RuntimeBackend { return &archAwareBackend{} }
		defer func() { resolveBackend = orig }()

		pod := r.constructDeploymentForCache(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template.Spec
		terms := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		if len(terms) != 1 {
			t.Fatalf("nodeSelectorTerms = %d, want 1; extra terms are ORed and make the "+
				"user's nodepool pin optional: %+v", len(terms), terms)
		}
		var gotPool, gotArch bool
		for _, expr := range terms[0].MatchExpressions {
			switch expr.Key {
			case "nodepool":
				gotPool = true
			case corev1.LabelArchStable:
				gotArch = true
				if !equalStrings(expr.Values, []string{"amd64"}) {
					t.Errorf("arch values = %v, want [amd64]", expr.Values)
				}
			}
		}
		if !gotPool || !gotArch {
			t.Errorf("term must AND both requirements; nodepool=%v arch=%v, expressions=%+v",
				gotPool, gotArch, terms[0].MatchExpressions)
		}
	})

	// With no user affinity there is nothing to AND into, so the constraint
	// still has to materialise as its own term.
	t.Run("arch requirement stands alone when the user set no affinity", func(t *testing.T) {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: "llamacpp"},
		}
		r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
		orig := resolveBackend
		resolveBackend = func(*inferencev1alpha1.InferenceService) RuntimeBackend { return &archAwareBackend{} }
		defer func() { resolveBackend = orig }()

		pod := r.constructDeploymentForCache(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template.Spec
		terms := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 ||
			terms[0].MatchExpressions[0].Key != corev1.LabelArchStable {
			t.Fatalf("want a single term carrying only the arch expression, got %+v", terms)
		}
	})

	// A user who pins placement with two alternative pools must keep both
	// alternatives, each independently narrowed to the supported architecture.
	t.Run("every user term is narrowed, not just the first", func(t *testing.T) {
		userTerm := func(pool string) corev1.NodeSelectorTerm {
			return corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: "nodepool", Operator: corev1.NodeSelectorOpIn, Values: []string{pool},
				}},
			}
		}
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: "llamacpp",
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{userTerm("pool-a"), userTerm("pool-b")},
						},
					},
				},
			},
		}
		r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
		orig := resolveBackend
		resolveBackend = func(*inferencev1alpha1.InferenceService) RuntimeBackend { return &archAwareBackend{} }
		defer func() { resolveBackend = orig }()

		pod := r.constructDeploymentForCache(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template.Spec
		terms := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		if len(terms) != 2 {
			t.Fatalf("nodeSelectorTerms = %d, want the user's 2 alternatives preserved: %+v", len(terms), terms)
		}
		for i, term := range terms {
			var hasArch bool
			for _, expr := range term.MatchExpressions {
				if expr.Key == corev1.LabelArchStable {
					hasArch = true
				}
			}
			if !hasArch {
				t.Errorf("term[%d] has no arch requirement, so it is an unconstrained "+
					"alternative: %+v", i, term.MatchExpressions)
			}
		}
	})

	// The pod spec's Affinity is assigned straight from isvc.Spec.Affinity, so
	// ANDing the requirement in place would write into the cached
	// InferenceService and stack another copy on every reconcile.
	t.Run("does not mutate the InferenceService across repeated builds", func(t *testing.T) {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: "llamacpp",
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{{
								MatchExpressions: []corev1.NodeSelectorRequirement{{
									Key:      "nodepool",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"gpu-dedicated"},
								}},
							}},
						},
					},
				},
			},
		}
		want := isvc.Spec.Affinity.DeepCopy()
		r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
		orig := resolveBackend
		resolveBackend = func(*inferencev1alpha1.InferenceService) RuntimeBackend { return &archAwareBackend{} }
		defer func() { resolveBackend = orig }()

		for i := range 3 {
			pod := r.constructDeploymentForCache(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template.Spec
			terms := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
			if len(terms) != 1 || len(terms[0].MatchExpressions) != 2 {
				t.Fatalf("build %d: want 1 term of 2 expressions, got %+v", i+1, terms)
			}
		}
		if !reflect.DeepEqual(isvc.Spec.Affinity, want) {
			t.Errorf("constructDeploymentForCache mutated isvc.Spec.Affinity:\n got %+v\nwant %+v",
				isvc.Spec.Affinity, want)
		}
	})
}

// TestInferPodSecurityContext_VulkanDRIRenderGID pins the #1560 fix at the
// function level: the DRI render GID is injected into supplementalGroups only
// on the Vulkan path, and fsGroup behaviour is unchanged in both cases. This
// is a pure-function unit test so it runs without envtest (unlike the Ginkgo
// suite, which needs etcd).
func TestInferPodSecurityContext_VulkanDRIRenderGID(t *testing.T) {
	const (
		defaultFSGroup = int64(102)
		renderGID      = int64(991)
	)
	emptyISVC := &inferencev1alpha1.InferenceService{}

	t.Run("vulkan adds the render GID to supplementalGroups", func(t *testing.T) {
		psc := inferPodSecurityContext(emptyISVC, defaultFSGroup, renderGID, true)
		if psc == nil {
			t.Fatal("inferPodSecurityContext returned nil")
		}
		if psc.SupplementalGroups == nil {
			t.Fatal("vulkan: SupplementalGroups is nil; want it to contain the render GID")
		}
		found := false
		for _, g := range psc.SupplementalGroups {
			if g == renderGID {
				found = true
			}
		}
		if !found {
			t.Errorf("vulkan: SupplementalGroups = %v, want it to contain %d", psc.SupplementalGroups, renderGID)
		}
	})

	t.Run("vulkan keeps fsGroup unchanged", func(t *testing.T) {
		psc := inferPodSecurityContext(emptyISVC, defaultFSGroup, renderGID, true)
		if psc.FSGroup == nil || *psc.FSGroup != defaultFSGroup {
			t.Errorf("vulkan: FSGroup = %v, want %d", psc.FSGroup, defaultFSGroup)
		}
	})

	t.Run("cuda does not add the render GID", func(t *testing.T) {
		psc := inferPodSecurityContext(emptyISVC, defaultFSGroup, renderGID, false)
		if psc.SupplementalGroups != nil {
			t.Errorf("cuda: SupplementalGroups = %v, want empty/nil", psc.SupplementalGroups)
		}
	})

	t.Run("cuda keeps fsGroup unchanged", func(t *testing.T) {
		psc := inferPodSecurityContext(emptyISVC, defaultFSGroup, renderGID, false)
		if psc.FSGroup == nil || *psc.FSGroup != defaultFSGroup {
			t.Errorf("cuda: FSGroup = %v, want %d", psc.FSGroup, defaultFSGroup)
		}
	})

	t.Run("vulkan with render GID disabled (0) adds nothing", func(t *testing.T) {
		psc := inferPodSecurityContext(emptyISVC, defaultFSGroup, 0, true)
		if psc.SupplementalGroups != nil {
			t.Errorf("disabled: SupplementalGroups = %v, want empty/nil", psc.SupplementalGroups)
		}
	})

	t.Run("user-supplied podSecurityContext is returned verbatim (vulkan)", func(t *testing.T) {
		user := &corev1.PodSecurityContext{SupplementalGroups: []int64{777}}
		got := inferPodSecurityContext(&inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{PodSecurityContext: user},
		}, defaultFSGroup, renderGID, true)
		if got != user {
			t.Errorf("user-supplied PodSecurityContext not returned verbatim: got %v, want %v", got, user)
		}
		// The operator's render GID must not be merged into a user override.
		for _, g := range got.SupplementalGroups {
			if g == renderGID {
				t.Errorf("user override must not gain the operator render GID %d", renderGID)
			}
		}
	})
}

// TestInferPodSecurityContext_FSGroupChangePolicy pins #2058: whenever the
// operator sets fsGroup it also sets fsGroupChangePolicy OnRootMismatch, so
// kubelet skips the recursive ownership walk of the model cache once the
// volume root already matches. Without an operator fsGroup the policy stays
// unset, and a user-supplied context is never touched.
func TestInferPodSecurityContext_FSGroupChangePolicy(t *testing.T) {
	const renderGID = int64(991)
	emptyISVC := &inferencev1alpha1.InferenceService{}

	for _, vulkan := range []bool{false, true} {
		t.Run(fmt.Sprintf("default fsGroup sets OnRootMismatch (vulkan=%v)", vulkan), func(t *testing.T) {
			psc := inferPodSecurityContext(emptyISVC, 102, renderGID, vulkan)
			if psc.FSGroup == nil || *psc.FSGroup != 102 {
				t.Fatalf("FSGroup = %v, want 102", psc.FSGroup)
			}
			if psc.FSGroupChangePolicy == nil || *psc.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
				t.Errorf("FSGroupChangePolicy = %v, want %q", psc.FSGroupChangePolicy, corev1.FSGroupChangeOnRootMismatch)
			}
		})
	}

	// --default-fsgroup <= 0 (OpenShift): no fsGroup, so no policy. An SCC may
	// still inject an fsGroup, and with the prep chowning the root to <uid>:0 the
	// root would never match it anyway, so the policy is left to the platform.
	for _, disabled := range []int64{0, -1} {
		t.Run(fmt.Sprintf("disabled default fsGroup (%d) leaves the policy unset", disabled), func(t *testing.T) {
			psc := inferPodSecurityContext(emptyISVC, disabled, renderGID, false)
			if psc.FSGroup != nil {
				t.Errorf("FSGroup = %d, want nil", *psc.FSGroup)
			}
			if psc.FSGroupChangePolicy != nil {
				t.Errorf("FSGroupChangePolicy = %q, want nil", *psc.FSGroupChangePolicy)
			}
		})
	}

	t.Run("user-supplied podSecurityContext is not given a policy", func(t *testing.T) {
		fsGroup := int64(2000)
		user := &corev1.PodSecurityContext{FSGroup: &fsGroup}
		got := inferPodSecurityContext(&inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{PodSecurityContext: user},
		}, 102, renderGID, false)
		if got != user {
			t.Fatalf("user-supplied PodSecurityContext not returned verbatim: got %v, want %v", got, user)
		}
		if got.FSGroupChangePolicy != nil {
			t.Errorf("FSGroupChangePolicy = %q on a user context, want nil (the user owns it)", *got.FSGroupChangePolicy)
		}
	})
}

// TestWithPrefetchGroup: a pod that reads a prefetched cache runs in the group
// the prefetch Job writes it as (#2450), without changing the service's spec.
func TestWithPrefetchGroup(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	cases := []struct {
		name       string
		psc        *corev1.PodSecurityContext
		defaultFS  int64
		prefetched bool
		want       []int64 // supplementalGroups of the result
		verbatim   bool    // the input pointer comes back unchanged
	}{
		{name: "not a prefetched cache", psc: &corev1.PodSecurityContext{FSGroup: i64(2000)}, defaultFS: 102, verbatim: true},
		{name: "own fsGroup", psc: &corev1.PodSecurityContext{RunAsUser: i64(2000), FSGroup: i64(2000)}, defaultFS: 102, prefetched: true, want: []int64{102}},
		{name: "own supplemental groups are kept", psc: &corev1.PodSecurityContext{FSGroup: i64(2000), SupplementalGroups: []int64{5}}, defaultFS: 102, prefetched: true, want: []int64{5, 102}},
		{name: "fsGroup is already the Job's group", psc: &corev1.PodSecurityContext{FSGroup: i64(102)}, defaultFS: 102, prefetched: true, verbatim: true},
		{name: "runAsGroup is already the Job's group", psc: &corev1.PodSecurityContext{RunAsGroup: i64(102)}, defaultFS: 102, prefetched: true, verbatim: true},
		{name: "already a supplemental group", psc: &corev1.PodSecurityContext{FSGroup: i64(2000), SupplementalGroups: []int64{102}}, defaultFS: 102, prefetched: true, verbatim: true},
		{name: "default fsGroup disabled", psc: &corev1.PodSecurityContext{FSGroup: i64(2000)}, defaultFS: 0, prefetched: true, verbatim: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := c.psc.DeepCopy()
			got := withPrefetchGroup(c.psc, c.defaultFS, c.prefetched)
			if !reflect.DeepEqual(c.psc, before) {
				t.Fatalf("the input was modified: %+v, was %+v", c.psc, before)
			}
			if c.verbatim {
				if got != c.psc {
					t.Fatalf("got a new context %+v, want the input back", got)
				}
				return
			}
			if got == c.psc {
				t.Fatal("got the input back, want a copy with the Job's group")
			}
			if !reflect.DeepEqual(got.SupplementalGroups, c.want) {
				t.Errorf("supplementalGroups = %v, want %v", got.SupplementalGroups, c.want)
			}
			if !reflect.DeepEqual(got.FSGroup, c.psc.FSGroup) || !reflect.DeepEqual(got.RunAsUser, c.psc.RunAsUser) {
				t.Errorf("fsGroup or runAsUser changed: %+v", got)
			}
		})
	}

	t.Run("the operator's own context already carries the Job's group", func(t *testing.T) {
		psc := inferPodSecurityContext(&inferencev1alpha1.InferenceService{}, 102, 0, false)
		if got := withPrefetchGroup(psc, 102, true); got != psc {
			t.Errorf("got %+v, want the operator's context unchanged", got)
		}
	})
}

// TestBuildContainerResources_MemoryLimit sets a memory LIMIT alongside the
// request. Historically the builder set only Requests[memory], so a serving
// container was unbounded: a model whose resident set exceeded available RAM
// took the node down (kubelet stopped posting status, node went NotReady)
// instead of being OOM-killed. See #1724.
func TestBuildContainerResources_MemoryLimit(t *testing.T) {
	build := func(isvc *inferencev1alpha1.InferenceService) corev1.ResourceRequirements {
		return buildContainerResources(isvc, &inferencev1alpha1.Model{}, 0, "")
	}

	t.Run("memory sets equal request and limit", func(t *testing.T) {
		res := build(&inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Resources: &inferencev1alpha1.InferenceResourceRequirements{Memory: "8Gi"},
			},
		})
		want := resource.MustParse("8Gi")
		if got := res.Requests[corev1.ResourceMemory]; !got.Equal(want) {
			t.Errorf("Requests[memory] = %s, want %s", got.String(), want.String())
		}
		if got := res.Limits[corev1.ResourceMemory]; !got.Equal(want) {
			t.Errorf("Limits[memory] = %s, want %s", got.String(), want.String())
		}
	})

	t.Run("hostMemory wins and drives both request and limit", func(t *testing.T) {
		res := build(&inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Resources: &inferencev1alpha1.InferenceResourceRequirements{Memory: "8Gi", HostMemory: "64Gi"},
			},
		})
		want := resource.MustParse("64Gi")
		if got := res.Requests[corev1.ResourceMemory]; !got.Equal(want) {
			t.Errorf("Requests[memory] = %s, want %s", got.String(), want.String())
		}
		if got := res.Limits[corev1.ResourceMemory]; !got.Equal(want) {
			t.Errorf("Limits[memory] = %s, want %s", got.String(), want.String())
		}
	})

	t.Run("memoryLimit raises the ceiling above the request", func(t *testing.T) {
		// The point of #1763: reserve steady state, allow a burst. Without
		// this the workload has to RESERVE its peak to be allowed to reach it.
		res := build(&inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					Memory: "8Gi", MemoryLimit: "64Gi",
				},
			},
		})
		wantReq, wantLim := resource.MustParse("8Gi"), resource.MustParse("64Gi")
		if got := res.Requests[corev1.ResourceMemory]; !got.Equal(wantReq) {
			t.Errorf("Requests[memory] = %s, want %s", got.String(), wantReq.String())
		}
		if got := res.Limits[corev1.ResourceMemory]; !got.Equal(wantLim) {
			t.Errorf("Limits[memory] = %s, want %s", got.String(), wantLim.String())
		}
	})

	t.Run("memoryLimit applies over hostMemory too", func(t *testing.T) {
		res := build(&inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					Memory: "8Gi", HostMemory: "48Gi", MemoryLimit: "64Gi",
				},
			},
		})
		wantReq, wantLim := resource.MustParse("48Gi"), resource.MustParse("64Gi")
		if got := res.Requests[corev1.ResourceMemory]; !got.Equal(wantReq) {
			t.Errorf("Requests[memory] = %s, want %s", got.String(), wantReq.String())
		}
		if got := res.Limits[corev1.ResourceMemory]; !got.Equal(wantLim) {
			t.Errorf("Limits[memory] = %s, want %s", got.String(), wantLim.String())
		}
	})

	t.Run("memoryLimit below the request is ignored", func(t *testing.T) {
		// The kubelet refuses to admit a pod whose limit is under its request,
		// so honouring it would turn a typo into an unschedulable workload.
		res := build(&inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					Memory: "8Gi", MemoryLimit: "4Gi",
				},
			},
		})
		want := resource.MustParse("8Gi")
		if got := res.Limits[corev1.ResourceMemory]; !got.Equal(want) {
			t.Errorf("Limits[memory] = %s, want %s (limit below request ignored)", got.String(), want.String())
		}
	})

	t.Run("malformed memoryLimit falls back to the request", func(t *testing.T) {
		res := build(&inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					Memory: "8Gi", MemoryLimit: "not-a-quantity",
				},
			},
		})
		want := resource.MustParse("8Gi")
		if got := res.Limits[corev1.ResourceMemory]; !got.Equal(want) {
			t.Errorf("Limits[memory] = %s, want %s", got.String(), want.String())
		}
	})

	t.Run("memoryLimit alone sets nothing", func(t *testing.T) {
		// No request means no memory keys at all; a bare ceiling would make the
		// pod BestEffort with a limit, which is not what anyone asked for.
		res := build(&inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Resources: &inferencev1alpha1.InferenceResourceRequirements{MemoryLimit: "64Gi"},
			},
		})
		if v, ok := res.Limits[corev1.ResourceMemory]; ok {
			t.Errorf("Limits has memory %s, want none", v.String())
		}
	})

	t.Run("neither memory nor hostMemory sets no memory key", func(t *testing.T) {
		res := build(&inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Resources: &inferencev1alpha1.InferenceResourceRequirements{CPU: "1"},
			},
		})
		if v, ok := res.Requests[corev1.ResourceMemory]; ok {
			t.Errorf("Requests has memory %s, want none", v.String())
		}
		if v, ok := res.Limits[corev1.ResourceMemory]; ok {
			t.Errorf("Limits has memory %s, want none", v.String())
		}
	})
}

// lookupEnv returns the value of the env var name in env, and whether it is set.
func lookupEnv(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// TestConstructDeployment_GenericStageModel covers #1961: spec.stageModel opts a runtime
// that does not stage by default (generic) into Model-managed staging, and tells the
// container where the weights landed so its args can say $(LLMKUBE_MODEL_PATH).
func TestConstructDeployment_GenericStageModel(t *testing.T) {
	on, off := true, false
	multi := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "flashnext-q4", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Format: "gguf",
			Source: "s3://models/unsloth/Qwen3.8-Flash-Next-GGUF",
			Files: []string{
				"UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00001-of-00002.gguf",
				"UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00002-of-00002.gguf",
				"MTP/mtp-Qwen3.8-Flash-Next-shared-Q8_0.gguf",
			},
		},
		Status: inferencev1alpha1.ModelStatus{CacheKey: "fnq4"},
	}
	newISvc := func(runtime string, stage, skip *bool) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:      "flashnext-q4",
				Runtime:       runtime,
				Image:         "example.invalid/gufo@sha256:" + strings.Repeat("a", 64),
				StageModel:    stage,
				SkipModelInit: skip,
				Args:          []string{"serve", "--model", "$(LLMKUBE_MODEL_PATH)", "--mtp-model", "$(LLMKUBE_MODEL_DIR)/MTP/x.gguf"},
			},
		}
	}
	build := func(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) corev1.PodSpec {
		r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
		return r.constructDeploymentForCache(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template.Spec
	}

	t.Run("generic with stageModel stages the Model and exports its paths", func(t *testing.T) {
		pod := build(newISvc("generic", &on, nil), multi)
		if got := containerNames(pod.InitContainers); len(got) != 2 || got[0] != "model-cache-prep" || got[1] != "model-downloader" {
			t.Fatalf("initContainers = %v, want [model-cache-prep model-downloader]", got)
		}
		if got := volumeNames(pod.Volumes); len(got) != 1 || got[0] != "model-cache" {
			t.Errorf("volumes = %v, want [model-cache]", got)
		}
		c := pod.Containers[0]
		var mounted bool
		for _, m := range c.VolumeMounts {
			if m.Name == "model-cache" && m.MountPath == "/models" && m.ReadOnly {
				mounted = true
			}
		}
		if !mounted {
			t.Errorf("container mounts %v, want model-cache read-only at /models", c.VolumeMounts)
		}
		if v, _ := lookupEnv(c.Env, "LLMKUBE_MODEL_PATH"); v != "/models/fnq4/UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00001-of-00002.gguf" {
			t.Errorf("LLMKUBE_MODEL_PATH = %q, want the primary file under the staged directory", v)
		}
		if v, _ := lookupEnv(c.Env, "LLMKUBE_MODEL_DIR"); v != "/models/fnq4" {
			t.Errorf("LLMKUBE_MODEL_DIR = %q, want /models/fnq4", v)
		}
		if got := strings.Join(c.Args, " "); !strings.Contains(got, "$(LLMKUBE_MODEL_PATH)") {
			t.Errorf("args = %q, want the user's args verbatim (Kubernetes expands $(VAR))", got)
		}
	})

	t.Run("single-file Model exports the file's directory", func(t *testing.T) {
		pod := build(newISvc("generic", &on, nil), cachedGGUFModel("one", "onekey"))
		c := pod.Containers[0]
		p, _ := lookupEnv(c.Env, "LLMKUBE_MODEL_PATH")
		d, _ := lookupEnv(c.Env, "LLMKUBE_MODEL_DIR")
		if p == "" || d == "" || !pathIsUnder(p, d) || p == d {
			t.Errorf("LLMKUBE_MODEL_PATH = %q, LLMKUBE_MODEL_DIR = %q, want a file inside its directory", p, d)
		}
	})

	for _, tc := range []struct {
		name string
		isvc *inferencev1alpha1.InferenceService
	}{
		{"generic without stageModel stays unstaged", newISvc("generic", nil, nil)},
		{"generic with stageModel false stays unstaged", newISvc("generic", &off, nil)},
		{"skipModelInit still opts out", newISvc("generic", &on, &on)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := build(tc.isvc, multi)
			if len(pod.InitContainers) != 0 || len(pod.Volumes) != 0 || len(pod.Containers[0].VolumeMounts) != 0 {
				t.Errorf("initContainers %v, volumes %v, mounts %d; want none",
					containerNames(pod.InitContainers), volumeNames(pod.Volumes), len(pod.Containers[0].VolumeMounts))
			}
			if _, ok := lookupEnv(pod.Containers[0].Env, "LLMKUBE_MODEL_PATH"); ok {
				t.Error("LLMKUBE_MODEL_PATH set on a service that staged nothing")
			}
		})
	}

	t.Run("a runtime that already stages ignores stageModel", func(t *testing.T) {
		with := build(newISvc("llamacpp", &on, nil), cachedGGUFModel("m", "k"))
		without := build(newISvc("llamacpp", nil, nil), cachedGGUFModel("m", "k"))
		if a, b := strings.Join(containerNames(with.InitContainers), ","), strings.Join(containerNames(without.InitContainers), ","); a != b {
			t.Errorf("initContainers with stageModel %s, without %s; want identical", a, b)
		}
		if _, ok := lookupEnv(with.Containers[0].Env, "LLMKUBE_MODEL_PATH"); ok {
			t.Error("LLMKUBE_MODEL_PATH set on llamacpp, which builds its own --model")
		}
	})
}

// TestConstructDeployment_VLLMStallLivenessAndOverrides pins the probe
// pipeline order for #2020: the vLLM stall customizer runs after BuildProbes,
// and spec.probeOverrides.liveness is applied after the customizer, so a
// user override always wins.
func TestConstructDeployment_VLLMStallLivenessAndOverrides(t *testing.T) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Format: "gguf", Source: "s3://bucket/m.gguf"},
	}
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	enabled := true

	newISvc := func() *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "target",
				Runtime:  "vllm",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					StallDetection: &inferencev1alpha1.VLLMStallDetection{Enabled: &enabled},
				},
			},
		}
	}
	vllmContainer := func(dep *appsv1.Deployment) corev1.Container {
		for _, c := range dep.Spec.Template.Spec.Containers {
			if c.Name == "vllm" {
				return c
			}
		}
		t.Fatal("vllm container not found in deployment")
		return corev1.Container{}
	}

	t.Run("stall detection renders the exec liveness probe", func(t *testing.T) {
		dep := r.constructDeploymentForCache(newISvc(), model, resolveEffectiveModelCache(newISvc(), model), nil, 1, "", "")
		container := vllmContainer(dep)
		if container.LivenessProbe == nil || container.LivenessProbe.Exec == nil {
			t.Fatalf("liveness probe = %+v, want an exec probe", container.LivenessProbe)
		}
		if container.LivenessProbe.HTTPGet != nil {
			t.Errorf("liveness probe kept an HTTPGet handler alongside the exec probe")
		}
		if got := container.LivenessProbe.Exec.Command[0]; got != "python3" {
			t.Errorf("exec command interpreter = %q, want python3", got)
		}
	})

	t.Run("probeOverrides.liveness still wins", func(t *testing.T) {
		isvc := newISvc()
		isvc.Spec.ProbeOverrides = &inferencev1alpha1.ProbeOverrides{
			Liveness: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/custom-health"},
				},
			},
		}
		dep := r.constructDeploymentForCache(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "")
		container := vllmContainer(dep)
		if container.LivenessProbe.Exec != nil {
			t.Fatal("user probeOverrides.liveness was overridden by the stall exec probe")
		}
		if container.LivenessProbe.HTTPGet == nil || container.LivenessProbe.HTTPGet.Path != "/custom-health" {
			t.Fatalf("liveness probe = %+v, want the user's /custom-health override", container.LivenessProbe)
		}
	})
}
