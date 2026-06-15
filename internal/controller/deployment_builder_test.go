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
			name: "explicit ResourceName overrides vendor mapping",
			gpu: &inferencev1alpha1.GPUSpec{
				Vendor:       "amd",
				ResourceName: "squat.ai/dri-render",
			},
			expected: corev1.ResourceName("squat.ai/dri-render"),
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
