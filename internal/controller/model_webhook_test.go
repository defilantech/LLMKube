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
	"context"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestNormalizeAccelerator(t *testing.T) {
	gpu := func(enabled bool, runtime string) *inferencev1alpha1.GPUSpec {
		return &inferencev1alpha1.GPUSpec{Enabled: enabled, Runtime: runtime}
	}
	cases := []struct {
		name     string
		hardware *inferencev1alpha1.HardwareSpec
		want     string // expected accelerator after normalization
	}{
		{"vulkan runtime + cpu default -> vulkan (#1074)",
			&inferencev1alpha1.HardwareSpec{Accelerator: "cpu", GPU: gpu(true, "vulkan")}, "vulkan"},
		{"vulkan runtime + empty accelerator -> vulkan",
			&inferencev1alpha1.HardwareSpec{Accelerator: "", GPU: gpu(true, "vulkan")}, "vulkan"},
		{"rocm runtime + cpu default -> rocm",
			&inferencev1alpha1.HardwareSpec{Accelerator: "cpu", GPU: gpu(true, "rocm")}, "rocm"},
		{"explicit non-cpu accelerator is respected",
			&inferencev1alpha1.HardwareSpec{Accelerator: "cuda", GPU: gpu(true, "vulkan")}, "cuda"},
		{"gpu disabled -> accelerator untouched",
			&inferencev1alpha1.HardwareSpec{Accelerator: "cpu", GPU: gpu(false, "vulkan")}, "cpu"},
		{"no gpu block -> cpu stays cpu",
			&inferencev1alpha1.HardwareSpec{Accelerator: "cpu"}, "cpu"},
		{"empty runtime is not normalized (back-compat)",
			&inferencev1alpha1.HardwareSpec{Accelerator: "cpu", GPU: gpu(true, "")}, "cpu"},
		{"nil hardware is a no-op",
			nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &inferencev1alpha1.Model{}
			m.Spec.Hardware = tc.hardware
			normalizeAccelerator(m)
			got := ""
			if m.Spec.Hardware != nil {
				got = m.Spec.Hardware.Accelerator
			}
			if got != tc.want {
				t.Errorf("accelerator = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestModelDefaulter_Default(t *testing.T) {
	d := &ModelDefaulter{}
	m := &inferencev1alpha1.Model{}
	m.Spec.Hardware = &inferencev1alpha1.HardwareSpec{
		Accelerator: "cpu",
		GPU:         &inferencev1alpha1.GPUSpec{Enabled: true, Runtime: "vulkan"},
	}
	if err := d.Default(context.Background(), m); err != nil {
		t.Fatalf("Default returned error: %v", err)
	}
	if m.Spec.Hardware.Accelerator != "vulkan" {
		t.Errorf("Default did not normalize accelerator: got %q", m.Spec.Hardware.Accelerator)
	}
}
