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
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// migProfilePattern validates NVIDIA MIG profile names ("1g.24gb", "3g.90gb").
// A profile that fails this pattern would silently produce an extended
// resource no device plugin advertises, leaving the pod Pending forever, so
// the resolver rejects it up front.
var migProfilePattern = regexp.MustCompile(`^[0-9]+g\.[0-9]+gb$`)

// migResourcePrefix is the extended-resource prefix the NVIDIA device plugin
// uses to advertise MIG partitions (mig.strategy=mixed), e.g.
// nvidia.com/mig-1g.24gb.
const migResourcePrefix = "nvidia.com/mig-"

// gpuSharingResolution is the concrete scheduling mechanism a gpuSharing mode
// resolves to: which extended resource the pod requests, which taint key the
// auto GPU toleration matches, and any node-pool selector the pod must carry.
type gpuSharingResolution struct {
	resourceName  corev1.ResourceName
	tolerationKey string
	// nodeSelector is the shared-pool selector, nil for exclusive and
	// partitioned. Merged under the user's own nodeSelector (user wins on
	// key conflict).
	nodeSelector map[string]string
}

// gpuSharingMode returns the effective sharing mode for an InferenceService:
// the declared mode, or exclusive when gpuSharing is unset (every manifest
// written before this field existed).
func gpuSharingMode(isvc *inferencev1alpha1.InferenceService) string {
	if isvc.Spec.Resources == nil || isvc.Spec.Resources.GPUSharing == nil {
		return inferencev1alpha1.GPUSharingModeExclusive
	}
	if isvc.Spec.Resources.GPUSharing.Mode == "" {
		return inferencev1alpha1.GPUSharingModeExclusive
	}
	return isvc.Spec.Resources.GPUSharing.Mode
}

// resolveGPUSharing maps the InferenceService's gpuSharing spec plus the
// Model's GPU vendor to the concrete scheduling mechanism. sharedPoolSelector
// is the operator-level node selector for the shared pool (from
// --gpu-sharing-shared-pool-selector, ultimately chart values); shared mode
// is rejected when it is empty, because shared workloads collide with
// exclusive ones on the same extended resource name and MUST be steered to a
// label-separated pool. Fail-closed beats silently co-locating.
//
// The exclusive path reproduces today's behavior exactly (resource name and
// toleration key from the Model spec), so a nil/exclusive gpuSharing changes
// nothing for existing manifests.
//
// Validation performed here is reconcile-time; promoting it to admission
// time is a planned follow-up (#1196 story 5). CEL on the CRD already
// enforces the structural rules (profile iff partitioned, memoryLimitGiB
// only for shared).
func resolveGPUSharing(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, sharedPoolSelector map[string]string) (gpuSharingResolution, error) {
	exclusive := gpuSharingResolution{
		resourceName:  gpuResourceNameForSpec(model),
		tolerationKey: gpuTolerationKeyForSpec(model),
	}

	mode := gpuSharingMode(isvc)
	if mode == inferencev1alpha1.GPUSharingModeExclusive {
		return exclusive, nil
	}

	// Both non-exclusive modes are single-device by definition: a partition
	// cannot span devices and a shared slot is one seat on one device.
	if count := resolveGPUCount(isvc, model); count != 1 {
		return gpuSharingResolution{}, fmt.Errorf(
			"gpuSharing mode %q requires exactly 1 GPU, got %d (tensor parallelism needs mode exclusive)", mode, count)
	}

	// DRA claims allocate devices out-of-band; combining them with a sharing
	// mode would leave two owners for the same placement decision.
	if len(modelResourceClaims(model)) > 0 {
		return gpuSharingResolution{}, fmt.Errorf(
			"gpuSharing mode %q cannot be combined with hardware.gpu.resourceClaims (DRA claims own device placement)", mode)
	}

	switch mode {
	case inferencev1alpha1.GPUSharingModePartitioned:
		return resolvePartitioned(isvc, model)
	case inferencev1alpha1.GPUSharingModeShared:
		return resolveShared(model, exclusive, sharedPoolSelector)
	default:
		// CRD enum makes this unreachable; keep it an error, not a fallback,
		// so a future enum addition cannot silently schedule exclusive.
		return gpuSharingResolution{}, fmt.Errorf("unknown gpuSharing mode %q", mode)
	}
}

// resolvePartitioned maps mode partitioned to the vendor's partition
// resource. NVIDIA MIG is the only supported implementation today; AMD APUs
// (Strix) cannot partition, and MI300 CPX/SPX mapping is a follow-up.
func resolvePartitioned(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) (gpuSharingResolution, error) {
	if model != nil && model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil {
		if vendor := strings.ToLower(strings.TrimSpace(model.Spec.Hardware.GPU.Vendor)); vendor != "" && vendor != "nvidia" {
			return gpuSharingResolution{}, fmt.Errorf(
				"gpuSharing mode partitioned is not supported for GPU vendor %q (NVIDIA MIG only today)", vendor)
		}
		if override := strings.TrimSpace(model.Spec.Hardware.GPU.ResourceName); override != "" {
			return gpuSharingResolution{}, fmt.Errorf(
				"gpuSharing mode partitioned conflicts with hardware.gpu.resourceName %q: the partition profile already determines the resource name; remove one", override)
		}
	}

	profile := strings.TrimSpace(isvc.Spec.Resources.GPUSharing.Profile)
	if !migProfilePattern.MatchString(profile) {
		return gpuSharingResolution{}, fmt.Errorf(
			"gpuSharing.profile %q is not a valid MIG profile (expected e.g. \"1g.24gb\")", profile)
	}

	resourceName := corev1.ResourceName(migResourcePrefix + profile)
	tolerationKey := string(resourceName)
	// The explicit TolerationKey override keeps working for partitioned
	// pods, mirroring the exclusive path.
	if model != nil && model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil {
		if override := strings.TrimSpace(model.Spec.Hardware.GPU.TolerationKey); override != "" {
			tolerationKey = override
		}
	}

	return gpuSharingResolution{resourceName: resourceName, tolerationKey: tolerationKey}, nil
}

// resolveShared keeps the vendor's ordinary device resource (a shared seat is
// still one nvidia.com/gpu on a time-sliced node, or the Strix iGPU dri
// resource) but steers the pod onto the label-separated shared pool.
func resolveShared(model *inferencev1alpha1.Model, exclusive gpuSharingResolution, sharedPoolSelector map[string]string) (gpuSharingResolution, error) {
	// AMD APU (Vulkan/ROCm iGPU) co-location shares the single device
	// resource by design, so it needs no pool separation: every workload on
	// the node already lands on the same iGPU. Only vendors whose shared and
	// exclusive pods would collide on the same resource name need the pool.
	if model != nil && model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil {
		if strings.EqualFold(strings.TrimSpace(model.Spec.Hardware.GPU.Vendor), "amd") {
			return exclusive, nil
		}
	}

	if len(sharedPoolSelector) == 0 {
		return gpuSharingResolution{}, fmt.Errorf(
			"gpuSharing mode shared requires a configured shared pool: set gpuSharing.pools.shared.nodeSelector in the chart (operator flag --gpu-sharing-shared-pool-selector), or use mode exclusive")
	}

	resolved := exclusive
	resolved.nodeSelector = sharedPoolSelector
	return resolved, nil
}

// ParseGPUSharingSharedPoolSelector parses the
// --gpu-sharing-shared-pool-selector flag value ("key=value[,key=value...]")
// into a node selector map. An empty value means no shared pool is
// configured. Exported for cmd/main.go.
func ParseGPUSharingSharedPoolSelector(flagValue string) (map[string]string, error) {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue == "" {
		return nil, nil
	}
	selector := map[string]string{}
	for _, pair := range strings.Split(flagValue, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(pair), "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !found || key == "" || value == "" {
			return nil, fmt.Errorf("invalid --gpu-sharing-shared-pool-selector entry %q (expected key=value)", pair)
		}
		selector[key] = value
	}
	return selector, nil
}
