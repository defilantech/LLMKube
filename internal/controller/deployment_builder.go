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
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Deployment construction. Turns an InferenceService + Model pair into the
// concrete Deployment that the controller applies to the cluster. The
// per-runtime backend (resolved via resolveBackend) contributes the container
// image, probes, arg/env list, and command. This file also owns the pod- and
// container-level security contexts that only the inference pod needs; the
// init-container security context stays in the controller file with its
// storage-builder callers.

// resolveEnableServiceLinks returns the value to set on PodSpec.EnableServiceLinks
// for a given backend. Backends that implement ServiceLinksOptOut and return
// true get an explicit `false`; everyone else gets nil (Kubernetes default,
// which is true). This keeps the legacy service-link env-var injection on for
// llama.cpp / generic / personaplex / tgi where it is harmless, and disables
// it for vLLM where the v0.20+ env-var validator turns it into log noise.
// resolveRuntimeImage returns the container image for the runtime, making the
// otherwise vendor-blind backend.DefaultImage() vendor- and runtime-aware where
// it matters. Today the only divergence is the llama.cpp backend with an
// AMD/Vulkan Model: it uses LLMKube's pinned Vulkan image instead of the stock
// upstream image. Every other backend/vendor/runtime combination falls through
// to backend.DefaultImage(). An explicit InferenceService.spec.image still wins
// over whatever this returns (handled by the caller).
func resolveRuntimeImage(backend RuntimeBackend, model *inferencev1alpha1.Model) string {
	if _, ok := backend.(*LlamaCppBackend); ok && isVulkanAMDModel(model) {
		return llamaCppVulkanImage
	}
	return backend.DefaultImage()
}

// isVulkanAMDModel reports whether the Model requests the AMD vendor with the
// Vulkan GPU runtime.
func isVulkanAMDModel(model *inferencev1alpha1.Model) bool {
	if model == nil || model.Spec.Hardware == nil || model.Spec.Hardware.GPU == nil {
		return false
	}
	gpu := model.Spec.Hardware.GPU
	return strings.EqualFold(strings.TrimSpace(gpu.Vendor), "amd") && isVulkanRuntime(gpu.Runtime)
}

func resolveEnableServiceLinks(backend RuntimeBackend) *bool {
	if d, ok := backend.(ServiceLinksOptOut); ok && d.DisableServiceLinks() {
		f := false
		return &f
	}
	return nil
}

// inferPodSecurityContext returns the user-supplied PodSecurityContext when
// present, otherwise a default that works with the standard non-root init
// container image (curlimages/curl, uid=101 gid=102).
//
// defaultFSGroup is the operator-configured default fsGroup (--default-fsgroup
// flag, default 102 to match curl_group). Kubernetes recursively chowns the
// volume to this GID and adds it to all containers' supplementary groups,
// which makes the volume writable for the curl init container and readable
// for the inference container regardless of its primary UID.
//
// defaultFSGroup <= 0 disables the default. This is the recommended setting on
// OpenShift, where the restricted-v2 SCC injects an appropriate fsGroup from
// the namespace's allocated range and rejects pods with explicit values
// outside that range.
//
// Operators using a custom init container image (--init-container-image) with
// a different UID/GID should override Spec.PodSecurityContext or set
// --default-fsgroup to match the new image's group.
func inferPodSecurityContext(isvc *inferencev1alpha1.InferenceService, defaultFSGroup int64) *corev1.PodSecurityContext {
	if isvc.Spec.PodSecurityContext != nil {
		return isvc.Spec.PodSecurityContext
	}
	psc := &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
	if defaultFSGroup > 0 {
		fsGroup := defaultFSGroup
		psc.FSGroup = &fsGroup
	}
	return psc
}

func inferContainerSecurityContext(isvc *inferencev1alpha1.InferenceService) *corev1.SecurityContext {
	if isvc.Spec.SecurityContext != nil {
		return isvc.Spec.SecurityContext
	}
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// deploymentSelectorLabels returns the immutable subset of operator-managed
// labels used for Deployment.Spec.Selector.MatchLabels. Kubernetes treats
// the selector as immutable after creation, so any label that can change
// over the InferenceService's lifetime (notably model.Name when the user
// edits spec.modelRef) must NOT appear here. The model label still ships
// on the Pod template + Deployment metadata for kubectl filtering; only
// the selector is restricted.
//
// See #301 for the silent-update bug this avoids: putting modelRef into
// the selector made the field effectively immutable, with no error
// surfaced to the user, just an apiserver "field is immutable" error
// looping in the controller logs.
func deploymentSelectorLabels(isvc *inferencev1alpha1.InferenceService) map[string]string {
	return map[string]string{
		"app":                           isvc.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}
}

// mergePodLabels combines operator-managed labels with the user's
// spec.podLabels for use on the Pod template metadata. Operator-managed keys
// always win on collision so the Deployment selector (which uses the
// operator-only set, not this merged result) keeps matching the Pods it owns.
// Returns a fresh map; callers may safely mutate either input afterwards.
func mergePodLabels(operator, user map[string]string) map[string]string {
	merged := make(map[string]string, len(operator)+len(user))
	for k, v := range user {
		merged[k] = v
	}
	for k, v := range operator {
		merged[k] = v // operator wins on collision
	}
	return merged
}

// copyMap returns a fresh shallow copy of m, or nil when m is empty. Used to
// pass spec.podAnnotations through to PodTemplateSpec.ObjectMeta.Annotations
// without sharing storage with the user's spec.
func copyMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// shouldProtectFromDisruption returns true when the operator should set the
// karpenter.sh/do-not-disrupt annotation on the pod template. It is true
// when ProtectStartup is enabled (default) and the InferenceService is not yet
// Ready, or when ProtectAlways is true. If the user has already set the
// annotation via podAnnotations, this returns false to avoid overwriting the
// user's value (the user's value always wins).
func shouldProtectFromDisruption(isvc *inferencev1alpha1.InferenceService) bool {
	if isvc.Spec.Disruption == nil {
		// Default: protect startup
		return isvc.Status.Phase != PhaseReady
	}
	d := isvc.Spec.Disruption

	// ProtectAlways: always set the annotation regardless of phase
	if d.ProtectAlways != nil && *d.ProtectAlways {
		return true
	}

	// ProtectStartup: set the annotation only while the service is not Ready
	protectStartup := true // default
	if d.ProtectStartup != nil {
		protectStartup = *d.ProtectStartup
	}
	if !protectStartup {
		return false
	}
	return isvc.Status.Phase != PhaseReady
}

// buildPodAnnotations merges the user's podAnnotations with the operator's
// disruption-protection annotation. User-provided values always win on
// collision.
func buildPodAnnotations(isvc *inferencev1alpha1.InferenceService) map[string]string {
	annotations := copyMap(isvc.Spec.PodAnnotations)
	if shouldProtectFromDisruption(isvc) {
		if annotations == nil {
			annotations = make(map[string]string)
		}
		// Only set the annotation if the user hasn't already set it
		if _, ok := annotations["karpenter.sh/do-not-disrupt"]; !ok {
			annotations["karpenter.sh/do-not-disrupt"] = "true"
		}
	}
	return annotations
}

func (r *InferenceServiceReconciler) constructDeployment(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	replicas int32,
) *appsv1.Deployment {
	backend := resolveBackend(isvc)

	labels := map[string]string{
		"app":                           isvc.Name,
		"inference.llmkube.dev/model":   model.Name,
		"inference.llmkube.dev/service": isvc.Name,
		"inference.llmkube.dev/runtime": runtimeNameLabel(isvc),
	}

	image := resolveRuntimeImage(backend, model)
	if isvc.Spec.Image != "" {
		image = isvc.Spec.Image
	}

	port := backend.DefaultPort()
	if isvc.Spec.ContainerPort != nil {
		port = *isvc.Spec.ContainerPort
	} else if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
		port = isvc.Spec.Endpoint.Port
	}

	skipInit := isvc.Spec.SkipModelInit != nil && *isvc.Spec.SkipModelInit

	var storageConfig modelStorageConfig
	var modelPath string
	if backend.NeedsModelInit() && !skipInit {
		useCache := model.Status.CacheKey != "" && r.ModelCachePath != ""
		storageConfig = buildModelStorageConfig(model, isvc, isvc.Namespace, useCache, r.ModelCacheMode, r.CACertConfigMap, r.InitContainerImage)
		modelPath = storageConfig.modelPath
	}

	args := backend.BuildArgs(isvc, model, modelPath, port)

	startupProbe, livenessProbe, readinessProbe := backend.BuildProbes(port)
	if isvc.Spec.ProbeOverrides != nil {
		if isvc.Spec.ProbeOverrides.Startup != nil {
			startupProbe = isvc.Spec.ProbeOverrides.Startup
		}
		if isvc.Spec.ProbeOverrides.Liveness != nil {
			livenessProbe = isvc.Spec.ProbeOverrides.Liveness
		}
		if isvc.Spec.ProbeOverrides.Readiness != nil {
			readinessProbe = isvc.Spec.ProbeOverrides.Readiness
		}
	}

	container := corev1.Container{
		Name:            backend.ContainerName(),
		Image:           image,
		SecurityContext: inferContainerSecurityContext(isvc),
		Ports: []corev1.ContainerPort{
			{
				Name:          "http",
				ContainerPort: port,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts:   storageConfig.volumeMounts,
		StartupProbe:   startupProbe,
		LivenessProbe:  livenessProbe,
		ReadinessProbe: readinessProbe,
	}

	// Set command/args based on runtime
	if len(isvc.Spec.Command) > 0 {
		container.Command = isvc.Spec.Command
	} else if cb, ok := backend.(CommandBuilder); ok {
		container.Command = cb.BuildCommand()
	}
	if args != nil {
		container.Args = args
	}

	// Add runtime-generated env vars, then user-specified env vars (user wins on conflict)
	if eb, ok := backend.(EnvBuilder); ok {
		container.Env = append(container.Env, eb.BuildEnv(isvc)...)
	}
	if len(isvc.Spec.Env) > 0 {
		container.Env = append(container.Env, isvc.Spec.Env...)
	}

	gpuCount := resolveGPUCount(isvc, model)
	gpuResourceName := gpuResourceNameForSpec(model)

	container.Resources = buildContainerResources(isvc, model, gpuCount, gpuResourceName)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      isvc.Name,
			Namespace: isvc.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				// Selector uses the immutable subset only; the model label
				// is allowed to change when the user edits spec.modelRef
				// and must not be matched on. See #301.
				MatchLabels: deploymentSelectorLabels(isvc),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      mergePodLabels(labels, isvc.Spec.PodLabels),
					Annotations: buildPodAnnotations(isvc),
				},
				Spec: corev1.PodSpec{
					SecurityContext:    inferPodSecurityContext(isvc, r.DefaultFSGroup),
					InitContainers:     storageConfig.initContainers,
					Containers:         []corev1.Container{container},
					Volumes:            storageConfig.volumes,
					PriorityClassName:  r.resolvePriorityClassName(isvc),
					RuntimeClassName:   isvc.Spec.RuntimeClassName,
					ImagePullSecrets:   isvc.Spec.ImagePullSecrets,
					EnableServiceLinks: resolveEnableServiceLinks(backend),
					ResourceClaims:     modelResourceClaims(model),
				},
			},
		},
	}

	if gpuCount > 0 {
		// Use Recreate strategy for GPU workloads to prevent deadlock:
		// RollingUpdate requires the new pod to be Ready before terminating the old,
		// but the new pod cannot schedule if the old pod holds the only available GPU(s).
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RecreateDeploymentStrategyType,
		}

		tolerations := []corev1.Toleration{
			{
				Key:      gpuTolerationKeyForSpec(model),
				Operator: corev1.TolerationOpEqual,
				Value:    "present",
				Effect:   corev1.TaintEffectNoSchedule,
			},
		}

		if len(isvc.Spec.Tolerations) > 0 {
			tolerations = append(tolerations, isvc.Spec.Tolerations...)
		}

		deployment.Spec.Template.Spec.Tolerations = tolerations

		if len(isvc.Spec.NodeSelector) > 0 {
			deployment.Spec.Template.Spec.NodeSelector = isvc.Spec.NodeSelector
		}
	}

	// DRA: apply nodeSelector and tolerations (no auto GPU taint for DRA)
	if len(modelResourceClaims(model)) > 0 {
		applyDRAPodScheduling(deployment, isvc)
	}

	return deployment
}

// applyDRAPodScheduling configures pod-level scheduling for a DRA workload.
// The DRA claim itself drives placement, but an explicit nodeSelector and any
// user tolerations are still honored. Recreate strategy is used to avoid the
// same scheduling deadlock as device-plugin GPU pods (a new pod can't get the
// claim while the old one holds it).
func applyDRAPodScheduling(deployment *appsv1.Deployment, isvc *inferencev1alpha1.InferenceService) {
	deployment.Spec.Strategy = appsv1.DeploymentStrategy{
		Type: appsv1.RecreateDeploymentStrategyType,
	}
	if len(isvc.Spec.NodeSelector) > 0 {
		deployment.Spec.Template.Spec.NodeSelector = isvc.Spec.NodeSelector
	}
	if len(isvc.Spec.Tolerations) > 0 {
		deployment.Spec.Template.Spec.Tolerations = isvc.Spec.Tolerations
	}
}

// modelResourceClaims safely extracts DRA resource claims from the model spec.
// Returns nil if the model has no GPU hardware or no resource claims configured.
func modelResourceClaims(model *inferencev1alpha1.Model) []corev1.PodResourceClaim {
	if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil {
		return model.Spec.Hardware.GPU.ResourceClaims
	}
	return nil
}

// buildContainerResources assembles the ResourceRequirements for the inference
// container, handling the device-plugin GPU limit path, DRA resource claims,
// and user-supplied CPU / memory requests.
func buildContainerResources(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, gpuCount int32, gpuResourceName corev1.ResourceName) corev1.ResourceRequirements {
	var res corev1.ResourceRequirements

	if gpuCount > 0 {
		res.Limits = corev1.ResourceList{
			gpuResourceName: resource.MustParse(fmt.Sprintf("%d", gpuCount)),
		}
	}

	if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil && len(model.Spec.Hardware.GPU.ResourceClaims) > 0 {
		for _, claim := range model.Spec.Hardware.GPU.ResourceClaims {
			res.Claims = append(res.Claims, corev1.ResourceClaim{Name: claim.Name})
		}
	}

	if isvc.Spec.Resources != nil {
		if res.Limits == nil {
			res.Limits = corev1.ResourceList{}
		}
		if res.Requests == nil {
			res.Requests = corev1.ResourceList{}
		}
		if isvc.Spec.Resources.CPU != "" {
			res.Requests[corev1.ResourceCPU] = resource.MustParse(isvc.Spec.Resources.CPU)
		}
		if isvc.Spec.Resources.HostMemory != "" {
			res.Requests[corev1.ResourceMemory] = resource.MustParse(isvc.Spec.Resources.HostMemory)
		} else if isvc.Spec.Resources.Memory != "" {
			res.Requests[corev1.ResourceMemory] = resource.MustParse(isvc.Spec.Resources.Memory)
		}
	}

	return res
}
