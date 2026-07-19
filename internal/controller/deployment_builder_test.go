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

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

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
			name:    "llamacpp nvidia keeps the stock image even with vulkan runtime",
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
			name:    "llamacpp nvidia keeps the stock image even with rocm runtime",
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
			if got := resolveRuntimeImage(tc.backend, tc.model); got != tc.expected {
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
		name     string
		isvc     *inferencev1alpha1.InferenceService
		expected map[string]string
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildPodAnnotations(tc.isvc)
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
