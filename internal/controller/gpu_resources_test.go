package controller

import (
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestResolveGPUResourceName(t *testing.T) {
	t.Run("defaults to nvidia when model hardware is missing", func(t *testing.T) {
		if got := resolveGPUResourceName(&inferencev1alpha1.Model{}); got != nvidiaGPUResourceName {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, nvidiaGPUResourceName)
		}
	})

	t.Run("uses nvidia for non-intel vendor", func(t *testing.T) {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					GPU: &inferencev1alpha1.GPUSpec{Vendor: "nvidia"},
				},
			},
		}

		if got := resolveGPUResourceName(model); got != nvidiaGPUResourceName {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, nvidiaGPUResourceName)
		}
	})

	t.Run("uses intel i915 resource by default", func(t *testing.T) {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					GPU: &inferencev1alpha1.GPUSpec{Vendor: "intel"},
				},
			},
		}

		if got := resolveGPUResourceName(model); got != intelGPUResourceNameI915 {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, intelGPUResourceNameI915)
		}
	})

	t.Run("uses intel i915 resource when accelerator is intel and gpu vendor is unset", func(t *testing.T) {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "intel",
				},
			},
		}

		if got := resolveGPUResourceName(model); got != intelGPUResourceNameI915 {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, intelGPUResourceNameI915)
		}
	})

	t.Run("uses configured intel resource override", func(t *testing.T) {
		t.Setenv(intelGPUResourceEnvVar, "gpu.intel.com/xe")

		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					GPU: &inferencev1alpha1.GPUSpec{Vendor: "intel"},
				},
			},
		}

		if got := resolveGPUResourceName(model); got != intelGPUResourceNameXE {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, intelGPUResourceNameXE)
		}
	})

	t.Run("uses vulkan resource when accelerator is vulkan", func(t *testing.T) {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "vulkan",
				},
			},
		}

		if got := resolveGPUResourceName(model); got != vulkanDRIResourceName {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, vulkanDRIResourceName)
		}
	})

	t.Run("uses vulkan resource when accelerator is vulkan with amd gpu vendor", func(t *testing.T) {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "vulkan",
					GPU:         &inferencev1alpha1.GPUSpec{Vendor: "amd"},
				},
			},
		}

		if got := resolveGPUResourceName(model); got != vulkanDRIResourceName {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, vulkanDRIResourceName)
		}
	})

	t.Run("uses the shared dri-render resource when accelerator is rocm", func(t *testing.T) {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "rocm",
				},
			},
		}

		if got := resolveGPUResourceName(model); got != vulkanDRIResourceName {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, vulkanDRIResourceName)
		}
	})
}

func TestDetectInsufficientGPUResource(t *testing.T) {
	t.Run("detects nvidia insufficient resource", func(t *testing.T) {
		message := "0/1 nodes are available: 1 Insufficient nvidia.com/gpu."

		got, ok := detectInsufficientGPUResource(message)
		if !ok {
			t.Fatalf("detectInsufficientGPUResource() ok = false, want true")
		}
		if got != nvidiaGPUResourceName {
			t.Fatalf("detectInsufficientGPUResource() = %q, want %q", got, nvidiaGPUResourceName)
		}
	})

	t.Run("detects intel i915 insufficient resource", func(t *testing.T) {
		message := "0/1 nodes are available: 1 Insufficient gpu.intel.com/i915."

		got, ok := detectInsufficientGPUResource(message)
		if !ok {
			t.Fatalf("detectInsufficientGPUResource() ok = false, want true")
		}
		if got != intelGPUResourceNameI915 {
			t.Fatalf("detectInsufficientGPUResource() = %q, want %q", got, intelGPUResourceNameI915)
		}
	})

	t.Run("detects intel custom resource from env override", func(t *testing.T) {
		t.Setenv(intelGPUResourceEnvVar, "gpu.intel.com/custom")
		message := "0/1 nodes are available: 1 Insufficient gpu.intel.com/custom."

		got, ok := detectInsufficientGPUResource(message)
		if !ok {
			t.Fatalf("detectInsufficientGPUResource() ok = false, want true")
		}
		if got != "gpu.intel.com/custom" {
			t.Fatalf("detectInsufficientGPUResource() = %q, want %q", got, "gpu.intel.com/custom")
		}
	})

	t.Run("ignores non-GPU insufficient resources", func(t *testing.T) {
		message := "0/1 nodes are available: 1 Insufficient cpu."

		if _, ok := detectInsufficientGPUResource(message); ok {
			t.Fatalf("detectInsufficientGPUResource() ok = true, want false")
		}
	})

	t.Run("detects vulkan insufficient resource", func(t *testing.T) {
		message := "0/1 nodes are available: 1 Insufficient devic.es/dri-render."

		got, ok := detectInsufficientGPUResource(message)
		if !ok {
			t.Fatalf("detectInsufficientGPUResource() ok = false, want true")
		}
		if got != vulkanDRIResourceName {
			t.Fatalf("detectInsufficientGPUResource() = %q, want %q", got, vulkanDRIResourceName)
		}
	})
}
