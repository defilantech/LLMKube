package controller

import (
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	acceleratorIntel           = "intel"
	intelGPUResourceEnvVar     = "LLMKUBE_INTEL_GPU_RESOURCE"
	defaultInsufficientGPUHint = "Insufficient "
)

var (
	nvidiaGPUResourceName    = corev1.ResourceName("nvidia.com/gpu")
	amdGPUResourceName       = corev1.ResourceName("amd.com/gpu")
	intelGPUResourceNameI915 = corev1.ResourceName("gpu.intel.com/i915")
	intelGPUResourceNameXE   = corev1.ResourceName("gpu.intel.com/xe")
)

// gpuResourceNameForSpec resolves the extended resource the pod requests for
// GPU scheduling. Resolution order:
//
//  1. Model.Spec.Hardware.GPU.ResourceName, when set, wins over everything
//     else. This is the escape hatch for non-default device plugins
//     (e.g. squat/generic-device-plugin advertising squat.ai/dri-render).
//  2. Model.Spec.Hardware.GPU.Vendor maps to the device-plugin default for
//     that vendor (nvidia -> nvidia.com/gpu, amd -> amd.com/gpu,
//     intel -> gpu.intel.com/i915).
//  3. Unset / unknown -> nvidia.com/gpu (backwards-compatible default).
//
// Used by the deployment builder; the accelerator-aware variant
// resolveGPUResourceName is used by the Model reconciler's readiness check
// and intentionally stays separate so the two code paths can evolve
// independently.
func gpuResourceNameForSpec(model *inferencev1alpha1.Model) corev1.ResourceName {
	if model != nil && model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil {
		if override := strings.TrimSpace(model.Spec.Hardware.GPU.ResourceName); override != "" {
			return corev1.ResourceName(override)
		}

		switch strings.ToLower(strings.TrimSpace(model.Spec.Hardware.GPU.Vendor)) {
		case "amd":
			return amdGPUResourceName
		case "intel":
			return intelGPUResourceNameI915
		}
	}

	return nvidiaGPUResourceName
}

// gpuTolerationKeyForSpec returns the taint key the GPU toleration should
// match. Defaults to the resource name (so the operator tolerates the taint
// the device plugin typically applies to advertise its nodes), with an
// explicit override via Model.Spec.Hardware.GPU.TolerationKey for setups
// where the two differ.
func gpuTolerationKeyForSpec(model *inferencev1alpha1.Model) string {
	if model != nil && model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil {
		if override := strings.TrimSpace(model.Spec.Hardware.GPU.TolerationKey); override != "" {
			return override
		}
	}

	return string(gpuResourceNameForSpec(model))
}

func resolveGPUResourceName(model *inferencev1alpha1.Model) corev1.ResourceName {
	if model != nil && model.Spec.Hardware != nil {
		if strings.EqualFold(model.Spec.Hardware.Accelerator, acceleratorIntel) {
			return resolveIntelGPUResourceName()
		}

		if model.Spec.Hardware.GPU != nil && strings.EqualFold(model.Spec.Hardware.GPU.Vendor, acceleratorIntel) {
			return resolveIntelGPUResourceName()
		}
	}

	return nvidiaGPUResourceName
}

func resolveIntelGPUResourceName() corev1.ResourceName {
	configured := strings.TrimSpace(os.Getenv(intelGPUResourceEnvVar))
	if configured == "" {
		return intelGPUResourceNameI915
	}

	return corev1.ResourceName(configured)
}

func detectInsufficientGPUResource(message string) (corev1.ResourceName, bool) {
	candidates := []corev1.ResourceName{
		nvidiaGPUResourceName,
		intelGPUResourceNameI915,
		intelGPUResourceNameXE,
	}

	configuredIntelResource := resolveIntelGPUResourceName()
	if configuredIntelResource != intelGPUResourceNameI915 && configuredIntelResource != intelGPUResourceNameXE {
		candidates = append(candidates, configuredIntelResource)
	}

	for _, resourceName := range candidates {
		if strings.Contains(message, defaultInsufficientGPUHint+string(resourceName)) {
			return resourceName, true
		}
	}

	return "", false
}
