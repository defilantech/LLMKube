package controller

import (
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	acceleratorIntel           = "intel"
	acceleratorVulkan          = "vulkan"
	intelGPUResourceEnvVar     = "LLMKUBE_INTEL_GPU_RESOURCE"
	defaultInsufficientGPUHint = "Insufficient "
)

// gpuRuntimeVulkan selects the Vulkan llama.cpp compute backend (LLMKube's own
// image + the generic-device-plugin /dev/dri resource).
const gpuRuntimeVulkan = "vulkan"

// gpuRuntimeROCm selects the ROCm/HIP llama.cpp compute backend, backed by
// LLMKube's hardware-validated ROCm image (llamaCppROCmImage, see
// runtime_llamacpp.go and #701). It shares the AMD GPU device-plugin resource
// (vulkanDRIResourceName, devic.es/dri-render) with the Vulkan runtime: on a
// single-GPU AMD node the two runtimes contend for the same physical device,
// and squat/generic-device-plugin cannot expose one device under two resource
// names without health-watch collisions. ROCm differs from Vulkan only by the
// container image. The resourceName override remains the escape hatch for
// AMD's official amd.com/gpu plugin.
const gpuRuntimeROCm = "rocm"

// acceleratorROCm is declared in model_controller.go (Model.Spec.Hardware.
// Accelerator enum value "rocm"); reused here rather than redeclared.

var (
	nvidiaGPUResourceName    = corev1.ResourceName("nvidia.com/gpu")
	amdGPUResourceName       = corev1.ResourceName("amd.com/gpu")
	intelGPUResourceNameI915 = corev1.ResourceName("gpu.intel.com/i915")
	intelGPUResourceNameXE   = corev1.ResourceName("gpu.intel.com/xe")
	// vulkanDRIResourceName is the generic-device-plugin resource that
	// advertises /dev/dri render nodes. Requesting it makes the plugin inject
	// the render device(s) into the container, so no hostPath device mount is
	// required. Used as the default GPU resource for the AMD/Vulkan path
	// (both the scheduling path via gpuResourceNameForSpec and the
	// readiness path via resolveGPUResourceName).
	vulkanDRIResourceName = corev1.ResourceName("devic.es/dri-render")
)

// gpuResourceNameForSpec resolves the extended resource the pod requests for
// GPU scheduling. Resolution order:
//
//  1. Model.Spec.Hardware.GPU.ResourceName, when set, wins over everything
//     else. This is the escape hatch for non-default device plugins (e.g. a
//     custom name like squat.ai/dri-render is just an illustrative override).
//  2. amd + runtime=vulkan or runtime=rocm -> devic.es/dri-render (the shared
//     generic-device-plugin resource; see gpuRuntimeROCm for why ROCm reuses
//     the Vulkan resource instead of amd.com/gpu).
//  3. Model.Spec.Hardware.GPU.Vendor maps to the device-plugin default for
//     that vendor (nvidia -> nvidia.com/gpu, amd -> amd.com/gpu,
//     intel -> gpu.intel.com/i915).
//  4. Unset / unknown -> nvidia.com/gpu (backwards-compatible default).
//
// Used by the deployment builder; the accelerator-aware variant
// resolveGPUResourceName is used by the Model reconciler's readiness check
// and intentionally stays separate so the two code paths can evolve
// independently.
// isVulkanRuntime reports whether the GPU runtime selector requests the Vulkan
// compute backend. Comparison is case-insensitive and trims surrounding space.
func isVulkanRuntime(runtime string) bool {
	return strings.EqualFold(strings.TrimSpace(runtime), gpuRuntimeVulkan)
}

// isROCmRuntime reports whether the GPU runtime selector requests the ROCm
// compute backend. Case-insensitive, trims surrounding space.
func isROCmRuntime(runtime string) bool {
	return strings.EqualFold(strings.TrimSpace(runtime), gpuRuntimeROCm)
}

func gpuResourceNameForSpec(model *inferencev1alpha1.Model) corev1.ResourceName {
	if model != nil && model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil {
		if override := strings.TrimSpace(model.Spec.Hardware.GPU.ResourceName); override != "" {
			return corev1.ResourceName(override)
		}

		switch strings.ToLower(strings.TrimSpace(model.Spec.Hardware.GPU.Vendor)) {
		case "amd":
			// Both the Vulkan and ROCm runtimes schedule against the shared
			// generic-device-plugin /dev/dri resource (vulkanDRIResourceName),
			// not the amd.com/gpu resource; see gpuRuntimeROCm for why.
			if isVulkanRuntime(model.Spec.Hardware.GPU.Runtime) ||
				isROCmRuntime(model.Spec.Hardware.GPU.Runtime) {
				return vulkanDRIResourceName
			}
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

		if strings.EqualFold(model.Spec.Hardware.Accelerator, acceleratorVulkan) {
			return vulkanDRIResourceName
		}

		if strings.EqualFold(model.Spec.Hardware.Accelerator, acceleratorROCm) {
			return vulkanDRIResourceName
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
		vulkanDRIResourceName,
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
