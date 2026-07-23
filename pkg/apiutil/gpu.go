// Package apiutil exposes stable helpers over the LLMKube API types for
// external components (e.g. the llmkube-kueue integration) that need the
// same GPU resource mapping the operator applies, without importing
// operator internals.
package apiutil

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	runtimeVulkan = "vulkan"
	runtimeROCm   = "rocm"
)

var (
	nvidiaGPUResourceName    = corev1.ResourceName("nvidia.com/gpu")
	amdGPUResourceName       = corev1.ResourceName("amd.com/gpu")
	intelGPUResourceNameI915 = corev1.ResourceName("gpu.intel.com/i915")
	vulkanDRIResourceName    = corev1.ResourceName("devic.es/dri-render")
)

func isRuntime(runtime, want string) bool {
	return strings.EqualFold(strings.TrimSpace(runtime), want)
}

// GPUResourceName resolves the extended resource name an InferenceService
// pod requests for GPU scheduling, given its referenced Model. Resolution
// order matches the operator's deployment builder:
//
//  1. Model.Spec.Hardware.GPU.ResourceName override wins.
//  2. amd vendor with the vulkan or rocm runtime -> devic.es/dri-render.
//  3. Vendor default: nvidia -> nvidia.com/gpu, amd -> amd.com/gpu,
//     intel -> gpu.intel.com/i915.
//  4. Nil/unset/unknown -> nvidia.com/gpu.
func GPUResourceName(model *inferencev1alpha1.Model) corev1.ResourceName {
	if model != nil && model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil {
		if override := strings.TrimSpace(model.Spec.Hardware.GPU.ResourceName); override != "" {
			return corev1.ResourceName(override)
		}
		switch strings.ToLower(strings.TrimSpace(model.Spec.Hardware.GPU.Vendor)) {
		case "amd":
			if isRuntime(model.Spec.Hardware.GPU.Runtime, runtimeVulkan) ||
				isRuntime(model.Spec.Hardware.GPU.Runtime, runtimeROCm) {
				return vulkanDRIResourceName
			}
			return amdGPUResourceName
		case "intel":
			return intelGPUResourceNameI915
		}
	}
	return nvidiaGPUResourceName
}

// GPUCount determines the desired GPU count per replica: the Model's
// hardware count wins, else the InferenceService resources.gpu, else 0.
// Both arguments are nil-safe.
func GPUCount(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) int32 {
	if model != nil && model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil && model.Spec.Hardware.GPU.Count > 0 {
		return model.Spec.Hardware.GPU.Count
	}
	if isvc != nil && isvc.Spec.Resources != nil && isvc.Spec.Resources.GPU > 0 {
		return isvc.Spec.Resources.GPU
	}
	return 0
}
