package controller

import (
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	acceleratorIntel       = "intel"
	intelGPUResourceEnvVar = "LLMKUBE_INTEL_GPU_RESOURCE"
)

var (
	nvidiaGPUResourceName      = corev1.ResourceName("nvidia.com/gpu")
	intelGPUResourceNameI915   = corev1.ResourceName("gpu.intel.com/i915")
	intelGPUResourceNameXE     = corev1.ResourceName("gpu.intel.com/xe")
	defaultInsufficientGPUHint = "Insufficient "
)

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
