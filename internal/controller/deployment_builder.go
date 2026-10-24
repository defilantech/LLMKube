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
	"path"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/apiutil"
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
// it matters. Resolution order:
//
//  1. A fleet-level override for the backend (--runtime-images, chart value
//     runtimeImages.{llamacpp,vllm,sglang,tgi}) wins over every built-in
//     choice below: it exists for air-gapped/mirrored fleets, which must be
//     able to redirect the registry unconditionally.
//  2. Built-in vendor/runtime divergences:
//     - LlamaCppBackend with AMD + Vulkan Model: LLMKube's pinned Vulkan image
//     - LlamaCppBackend with AMD + ROCm Model: LLMKube's pinned ROCm image
//     - LlamaCppBackend with an NVIDIA-GPU Model: upstream CUDA image, because
//     the :server default is CPU-only and an NVIDIA GPU Model on it would
//     silently serve on CPU (#1197)
//     - SGLangBackend with AMD (ROCm) Model: SGLang ROCm image
//  3. backend.DefaultImage().
//
// An explicit InferenceService.spec.image still wins over whatever this
// returns (handled by the caller).
func resolveRuntimeImage(backend RuntimeBackend, model *inferencev1alpha1.Model, overrides map[string]string) string {
	if img := overrides[runtimeImageOverrideKey(backend)]; img != "" {
		return img
	}
	if _, ok := backend.(*LlamaCppBackend); ok && isVulkanAMDModel(model) {
		return llamaCppVulkanImage
	}
	if _, ok := backend.(*LlamaCppBackend); ok && isROCmAMDModel(model) {
		return llamaCppROCmImage
	}
	if _, ok := backend.(*LlamaCppBackend); ok && isNVIDIAGPUModel(model) {
		return llamaCppCUDAImage
	}
	if _, ok := backend.(*SGLangBackend); ok && isAMDROCmModel(model) {
		return sglangROCmImage
	}
	return backend.DefaultImage()
}

// runtimeImageOverrideKey maps a backend to its --runtime-images key. Only
// backends whose defaults the operator owns are overridable; every other
// backend (generic requires spec.image by contract) returns "".
func runtimeImageOverrideKey(backend RuntimeBackend) string {
	switch backend.(type) {
	case *LlamaCppBackend:
		return "llamacpp"
	case *VLLMBackend:
		return "vllm"
	case *SGLangBackend:
		return "sglang"
	case *TGIBackend:
		return "tgi"
	default:
		return ""
	}
}

// isNVIDIAGPUModel reports whether the Model declares a GPU that resolves to
// the NVIDIA vendor, explicitly or by the unset-vendor default (mirroring
// gpuResourceNameForSpec: unset vendor means NVIDIA).
func isNVIDIAGPUModel(model *inferencev1alpha1.Model) bool {
	if model == nil || model.Spec.Hardware == nil || model.Spec.Hardware.GPU == nil {
		return false
	}
	gpu := model.Spec.Hardware.GPU
	if !gpu.Enabled && gpu.Count <= 0 {
		return false
	}
	vendor := strings.ToLower(strings.TrimSpace(gpu.Vendor))
	return vendor == "" || vendor == "nvidia"
}

// isAMDROCmModel reports whether the Model requests the AMD vendor. ROCm vs
// Vulkan is not distinguished here — SGLang ships ROCm images, not Vulkan.
func isAMDROCmModel(model *inferencev1alpha1.Model) bool {
	if model == nil || model.Spec.Hardware == nil || model.Spec.Hardware.GPU == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(model.Spec.Hardware.GPU.Vendor), "amd")
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

// isROCmAMDModel reports whether the Model requests the AMD vendor with the
// ROCm/HIP GPU runtime (the per-model opt-in tier from #701). Not to be
// confused with isAMDROCmModel above, which is the SGLang backend's
// vendor-only check (SGLang ships only ROCm images for AMD, so it does not
// need to distinguish runtime=vulkan from runtime=rocm the way llama.cpp does).
func isROCmAMDModel(model *inferencev1alpha1.Model) bool {
	if model == nil || model.Spec.Hardware == nil || model.Spec.Hardware.GPU == nil {
		return false
	}
	gpu := model.Spec.Hardware.GPU
	return strings.EqualFold(strings.TrimSpace(gpu.Vendor), "amd") && isROCmRuntime(gpu.Runtime)
}

func resolveEnableServiceLinks(backend RuntimeBackend) *bool {
	if d, ok := backend.(ServiceLinksOptOut); ok && d.DisableServiceLinks() {
		f := false
		return &f
	}
	return nil
}

const (
	devShmPath       = "/dev/shm"
	devShmVolumeName = "llmkube-shm"
)

// devShmDefaultLimit is the /dev/shm cap with no memory limit to scale from, the
// value the two-Spark DeepSeek lab mounts by hand.
var devShmDefaultLimit = resource.MustParse("16Gi")

// devShmVolume returns the memory-backed emptyDir to mount at /dev/shm, or nil
// when the pod should keep the container runtime's default. Multi-process
// runtimes (SharedMemoryRequirer) exchange tensors and NCCL buffers through
// /dev/shm, and the 64 MiB default fails their engine start (#2099).
//
// It returns nil when:
//   - the backend does not need it for this service;
//   - spec.multiNode is set: member pods run with hostIPC, which gives them the
//     host's /dev/shm, and are built from this template, so an emptyDir would
//     shadow it;
//   - the user already mounts /dev/shm through extraVolumeMounts (their volume,
//     size and medium win, which is also how to opt out or resize), or already
//     has a volume with the operator's name.
//
// The size is a cap, not a reservation: tmpfs pages are allocated as written and
// charged to the pod's memory, so with a memory limit the cap is half of it (at
// most 16Gi) and the engine keeps the other half.
func devShmVolume(isvc *inferencev1alpha1.InferenceService, backend RuntimeBackend, gpuCount int32, memLimit *resource.Quantity) *corev1.Volume {
	r, ok := backend.(SharedMemoryRequirer)
	if !ok || !r.NeedsSharedMemory(isvc, gpuCount) || isvc.Spec.MultiNode != nil {
		return nil
	}
	for _, m := range isvc.Spec.ExtraVolumeMounts {
		if path.Clean(m.MountPath) == devShmPath {
			return nil
		}
	}
	for _, v := range isvc.Spec.ExtraVolumes {
		if v.Name == devShmVolumeName {
			return nil
		}
	}
	limit := devShmDefaultLimit.DeepCopy()
	if memLimit != nil && !memLimit.IsZero() {
		if half := resource.NewQuantity(memLimit.Value()/2, resource.BinarySI); half.Cmp(limit) < 0 {
			limit = *half
		}
	}
	return &corev1.Volume{
		Name: devShmVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &limit},
		},
	}
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
// Whenever the default fsGroup is set, fsGroupChangePolicy is set to
// OnRootMismatch (#2058). With the Kubernetes default (Always) kubelet walks the
// whole volume and changes the group and mode of every entry on every pod
// start, which on a large shared model cache, especially on a network
// filesystem, is paid on every restart and grows with the cache. OnRootMismatch
// skips the walk when the volume root already has the fsGroup GID, group rwx
// and the setgid bit; the model-cache-prep step (chown 0:<fsGroup>, chmod
// g+rwX) keeps the root in that state. On storage that applies no fsGroup at
// all (CSI fsGroupPolicy None) the field has no effect.
//
// defaultFSGroup <= 0 disables the default. This is the recommended setting on
// OpenShift, where the restricted-v2 SCC injects an appropriate fsGroup from
// the namespace's allocated range and rejects pods with explicit values
// outside that range.
//
// Vulkan render GID. fsGroup is a cache-volume-ownership control and does
// nothing for device access: a Vulkan serving container must also carry the
// node's render GID in supplementalGroups to open /dev/dri/renderD128, or
// llama.cpp fails open and serves from CPU at roughly half speed with no
// status condition (#1560). When the resolved runtime is Vulkan (vulkan) and
// driRenderGID > 0, that GID is appended to supplementalGroups. The GID is
// node-local and not knowable at admission, so it comes from the operator's
// --dri-render-gid flag, which defaults to 0 (disabled): `render` is allocated
// dynamically and differs per host, so there is no portable default to ship
// (#1572). This is applied only on the Vulkan
// path; the CUDA and Metal paths are untouched. A user-supplied
// Spec.PodSecurityContext is returned as-is, so setting it takes full
// ownership of supplementalGroups (the per-service workaround for a node
// whose render GID differs from the operator default).
//
// Operators using a custom init container image (--init-container-image) with
// a different UID/GID should override Spec.PodSecurityContext or set
// --default-fsgroup to match the new image's group.
func inferPodSecurityContext(isvc *inferencev1alpha1.InferenceService, defaultFSGroup, driRenderGID int64, vulkan bool) *corev1.PodSecurityContext {
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
		psc.FSGroupChangePolicy = fsGroupChangeOnRootMismatch()
	}
	if vulkan && driRenderGID > 0 {
		psc.SupplementalGroups = append(psc.SupplementalGroups, driRenderGID)
	}
	return psc
}

// fsGroupChangeOnRootMismatch returns the fsGroupChangePolicy the operator sets
// next to every fsGroup it renders (inference pods and the prefetch Job).
func fsGroupChangeOnRootMismatch() *corev1.PodFSGroupChangePolicy {
	policy := corev1.FSGroupChangeOnRootMismatch
	return &policy
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
// disruption-protection annotation and, when emitScrape is set, Prometheus
// annotation-discovery hints. User-provided values always win on collision.
func buildPodAnnotations(isvc *inferencev1alpha1.InferenceService, emitScrape bool, port int32) map[string]string {
	annotations := copyMap(isvc.Spec.PodAnnotations)
	set := func(k, v string) {
		if annotations == nil {
			annotations = make(map[string]string)
		}
		// The user's value always wins.
		if _, ok := annotations[k]; !ok {
			annotations[k] = v
		}
	}
	if shouldProtectFromDisruption(isvc) {
		set("karpenter.sh/do-not-disrupt", "true")
	}
	if emitScrape {
		// port is the fully-resolved container port (spec.containerPort ->
		// spec.endpoint.port -> backend.DefaultPort(), resolved once in
		// constructDeployment), so annotation scrapers hit the real app port
		// for every runtime and honor spec.containerPort — regardless of the
		// container-port name ("http", which metrics-named scrape regexes miss).
		set("prometheus.io/scrape", "true")
		set("prometheus.io/path", "/metrics")
		set("prometheus.io/port", fmt.Sprintf("%d", port))
	}
	return annotations
}

// servedModelPath picks the path handed to the runtime. For a multi-file model
// (stagedDir set) on a directory-oriented runtime (vLLM/SGLang) whose format is
// not GGUF, that is the staged directory (the full Hugging Face model tree);
// otherwise it is the primary file, preserving GGUF and llama.cpp behavior.
// (#1157)
func servedModelPath(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, sc modelStorageConfig) string {
	if sc.stagedDir != "" &&
		directoryOrientedRuntime(isvc.Spec.Runtime) &&
		model.Spec.Format != "" && model.Spec.Format != "gguf" {
		return sc.stagedDir
	}
	return sc.modelPath
}

// servingStorage builds the model storage the serving pod mounts and the paths handed to
// the runtime. It stages when the runtime stages by default, or when spec.stageModel opts
// a runtime that does not into the same staging (#1961); skipModelInit always opts out.
// optIn reports the second case, whose container learns the paths from LLMKUBE_MODEL_*.
func (r *InferenceServiceReconciler) servingStorage(
	isvc *inferencev1alpha1.InferenceService,
	model, draftModel *inferencev1alpha1.Model,
	backend RuntimeBackend,
	hfEndpoint, draftHFEndpoint string,
) (storageConfig modelStorageConfig, modelPath, draftPath string, optIn bool) {
	if !operatorStagesModel(isvc, backend) {
		return modelStorageConfig{}, "", "", false
	}
	optIn = !backend.NeedsModelInit()
	// Same predicate as the provisioning side (modelNeedsCachePVC), so a
	// service that declined the cache mounts an emptyDir instead of a claim
	// nobody created (#1451).
	useCache := modelWantsCacheVolume(model, isvc, r.ModelCachePath)
	if g := r.prefetchGate(isvc, model); g.applies && g.satisfied {
		// The weights are already on the claim (#2136): mount it read-only
		// and verify the names the Job recorded, rather than run the
		// downloader on the serving node.
		var staged []string
		if g.entry != nil {
			staged = g.entry.Files
		}
		storageConfig = buildPrefetchedStorageConfig(model, effectiveCacheService(isvc, model), r.ModelCacheMode, r.InitContainerImage, staged)
	} else {
		storageConfig = buildModelStorageConfig(model, isvc, isvc.Namespace, useCache, r.ModelCacheMode, r.CACertConfigMap, r.InitContainerImage, r.DefaultFSGroup, r.ModelCachePrep, r.AllowedHostPathRoots, hfEndpoint)
	}
	modelPath = servedModelPath(isvc, model, storageConfig)

	// The draft's weights ride in the same pod, under the SAME gate as the
	// target's. A runtime that does not auto-mount /models (the llamacpp
	// router documents exactly that) or a service that set skipModelInit
	// must not have a cache volume and two init containers injected behind
	// its back just because a draft model is referenced.
	if draftModel != nil {
		draftUseCache := modelWantsCacheVolume(draftModel, isvc, r.ModelCachePath)
		draftStorage := buildModelStorageConfig(draftModel, isvc, isvc.Namespace, draftUseCache,
			r.ModelCacheMode, r.CACertConfigMap, r.InitContainerImage, r.DefaultFSGroup, r.ModelCachePrep, r.AllowedHostPathRoots, draftHFEndpoint)
		// The path comes from the merge's rewritten draft config, not from
		// draftStorage: the merge may have remounted the draft's volume to
		// clear a collision with the target's, and -md must follow it.
		var placedDraft modelStorageConfig
		storageConfig, placedDraft = mergeStorageConfigs(storageConfig, draftStorage)
		draftPath = servedModelPath(isvc, draftModel, placedDraft)
	}
	return storageConfig, modelPath, draftPath, optIn
}

// stagedModelEnv tells a runtime that builds no model arguments itself (spec.stageModel
// on generic) where staging put the weights: LLMKUBE_MODEL_PATH is the primary file and
// LLMKUBE_MODEL_DIR the staged directory of a multi-file Model, or the primary file's
// directory. Kubernetes expands $(VAR) in args from the container's env, so the user's
// args can name these without any templating here.
func stagedModelEnv(modelPath string, sc modelStorageConfig) []corev1.EnvVar {
	dir := sc.stagedDir
	if dir == "" {
		dir = path.Dir(modelPath)
	}
	return []corev1.EnvVar{
		{Name: "LLMKUBE_MODEL_PATH", Value: modelPath},
		{Name: "LLMKUBE_MODEL_DIR", Value: dir},
	}
}

// setCommandAndArgs resolves the container's entrypoint and argv. A custom
// entrypoint owns its args, but only where the runtime would otherwise have
// injected its own CLI: a bring-your-own launcher (e.g. a tuned vLLM image
// whose entrypoint dispatches serving modes) must not be handed
// `vllm serve <model> --host …`, so honor spec.args verbatim — then still
// append spec.extraArgs, the documented escape hatch — and let a custom-image
// server run under a native runtime purely to inherit its idle probe and
// metrics scraping without adopting its CLI.
//
// Runtimes that build no command (llamacpp, generic) serve via the image
// ENTRYPOINT plus generated args, so for them spec.command overrides the
// entrypoint only and the generated args still apply. Keying the verbatim path
// on spec.command alone started llama-server with no `--model` at all (#1842).
func setCommandAndArgs(
	container *corev1.Container,
	isvc *inferencev1alpha1.InferenceService,
	backend RuntimeBackend,
	args []string,
) {
	cb, injectsCLI := backend.(CommandBuilder)
	if len(isvc.Spec.Command) > 0 && injectsCLI {
		container.Command = isvc.Spec.Command
		container.Args = append(append([]string{}, isvc.Spec.Args...), isvc.Spec.ExtraArgs...)
		return
	}
	if len(isvc.Spec.Command) > 0 {
		container.Command = isvc.Spec.Command
	} else if injectsCLI {
		container.Command = cb.BuildCommand()
	}
	if args != nil {
		container.Args = args
	}
}

func (r *InferenceServiceReconciler) constructDeployment(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	draftModel *inferencev1alpha1.Model,
	replicas int32,
	hfEndpoint string,
	draftHFEndpoint string,
) *appsv1.Deployment {
	backend := resolveBackend(isvc)

	labels := map[string]string{
		"app":                           isvc.Name,
		"inference.llmkube.dev/model":   model.Name,
		"inference.llmkube.dev/service": isvc.Name,
		"inference.llmkube.dev/runtime": runtimeNameLabel(isvc),
	}

	image := resolveRuntimeImage(backend, model, r.RuntimeImageOverrides)
	if isvc.Spec.Image != "" {
		image = isvc.Spec.Image
	}

	port := backend.DefaultPort()
	if isvc.Spec.ContainerPort != nil {
		port = *isvc.Spec.ContainerPort
	} else if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
		port = isvc.Spec.Endpoint.Port
	}

	storageConfig, modelPath, draftPath, optInStaging := r.servingStorage(isvc, model, draftModel, backend, hfEndpoint, draftHFEndpoint)
	args := backend.BuildArgs(isvc, model, modelPath, draftPath, port)

	startupProbe, livenessProbe, readinessProbe := backend.BuildProbes(port)
	// Spec-derived probe customization runs before probeOverrides so a user
	// override always wins over what the backend customizes (e.g. the vLLM
	// stall-detecting exec liveness probe, #2020).
	if pc, ok := backend.(ProbeCustomizer); ok {
		pc.CustomizeProbes(isvc, port, startupProbe, livenessProbe, readinessProbe)
	}
	// spec.startupTimeout only widens the budget of the probe the runtime
	// built; handler and period stay the runtime's, and probeOverrides.startup
	// still replaces the probe wholesale below.
	applyStartupTimeout(isvc.Spec.StartupTimeout, startupProbe)
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
		Name:  backend.ContainerName(),
		Image: image,
		// spec.imagePullPolicy applies to the runtime container only. The init
		// containers built by servingStorage run operator-chosen images and
		// keep their own policy.
		ImagePullPolicy: isvc.Spec.ImagePullPolicy,
		SecurityContext: inferContainerSecurityContext(isvc),
		// Engines print the fatal error (CUDA init, model load) to the log
		// rather than /dev/termination-log, so fall back to the log tail on
		// error to make terminations self-describing; the driver-compat
		// diagnosis reads its signatures from this message.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
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
	container.VolumeMounts = append(container.VolumeMounts, isvc.Spec.ExtraVolumeMounts...)

	setCommandAndArgs(&container, isvc, backend, args)

	// Staged model paths for opt-in staging, then runtime-generated env vars, then
	// user-specified env vars (user wins on conflict).
	if optInStaging {
		container.Env = append(container.Env, stagedModelEnv(modelPath, storageConfig)...)
	}
	if eb, ok := backend.(EnvBuilder); ok {
		container.Env = append(container.Env, eb.BuildEnv(isvc)...)
	}
	// Mirror env, then the Hub-offline env, then spec.env: the user's entry for
	// any of these names comes last and wins.
	mirrorEnv, _ := runtimeHFMirrorEnv(backend, isvc, model, hfEndpoint)
	container.Env = append(container.Env, mirrorEnv...)
	container.Env = append(container.Env, hubOfflineEnv(isvc)...)
	if len(isvc.Spec.Env) > 0 {
		container.Env = append(container.Env, isvc.Spec.Env...)
	}

	gpuCount := resolveGPUCount(isvc, model)
	// Resolve the gpuSharing tier to its scheduling mechanism (resource name,
	// toleration key, shared-pool selector). reconcileDeployment already
	// rejected invalid specs before calling this builder, so the error path
	// here falls back to the exclusive defaults the resolution starts from.
	sharing, err := resolveGPUSharing(isvc, model, r.GPUSharingSharedPool)
	if err != nil {
		sharing = gpuSharingResolution{
			resourceName:  gpuResourceNameForSpec(model),
			tolerationKey: gpuTolerationKeyForSpec(model),
		}
	}
	gpuResourceName := sharing.resourceName

	// The Vulkan path requests the generic-device-plugin /dev/dri resource
	// (devic.es/dri-render). The container still needs the host render GID in
	// supplementalGroups to open /dev/dri/renderD128, so the security context
	// injects it only when this service schedules that resource. CUDA and Metal
	// request other resources (or none) and are left untouched (#1560).
	vulkan := gpuResourceName == vulkanDRIResourceName

	container.Resources = buildContainerResources(isvc, model, gpuCount, gpuResourceName)

	// Sized from the memory limit just resolved, so it must follow
	// buildContainerResources.
	shmVolume := devShmVolume(isvc, backend, gpuCount, container.Resources.Limits.Memory())
	if shmVolume != nil {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: devShmVolumeName, MountPath: devShmPath})
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      isvc.Name,
			Namespace: isvc.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:             &replicas,
			RevisionHistoryLimit: revisionHistoryLimitOrDefault(isvc.Spec.RevisionHistoryLimit),
			Selector: &metav1.LabelSelector{
				// Selector uses the immutable subset only; the model label
				// is allowed to change when the user edits spec.modelRef
				// and must not be matched on. See #301.
				MatchLabels: deploymentSelectorLabels(isvc),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      mergePodLabels(labels, isvc.Spec.PodLabels),
					Annotations: buildPodAnnotations(isvc, r.EmitScrapeAnnotations, port),
				},
				Spec: corev1.PodSpec{
					SecurityContext:    inferPodSecurityContext(isvc, r.DefaultFSGroup, r.DRIRenderGID, vulkan),
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
	deployment.Spec.Template.Spec.Volumes = append(deployment.Spec.Template.Spec.Volumes, isvc.Spec.ExtraVolumes...)
	configureSGLangAPIKey(deployment, isvc)
	if shmVolume != nil {
		deployment.Spec.Template.Spec.Volumes = append(deployment.Spec.Template.Spec.Volumes, *shmVolume)
	}

	// GPU-only scheduling: the device taint toleration and the shared-pool
	// selector. Both are meaningless without a GPU request, so they stay gated.
	var gpuTolerations []corev1.Toleration
	gpuNodeSelector := map[string]string{}

	if gpuCount > 0 {
		gpuTolerations = []corev1.Toleration{
			{
				// Keyed off the sharing-resolved name so a partitioned pod
				// tolerates the MIG resource's taint, not the whole-GPU one.
				Key:      sharing.tolerationKey,
				Operator: corev1.TolerationOpEqual,
				Value:    "present",
				Effect:   corev1.TaintEffectNoSchedule,
			},
		}

		// The pool label steers shared pods away from exclusive nodes that
		// advertise the same extended resource.
		for k, v := range sharing.nodeSelector {
			gpuNodeSelector[k] = v
		}
	}

	// The user's own nodeSelector and tolerations are general pod-scheduling
	// passthrough with no GPU semantics, so they apply to EVERY service.
	//
	// They used to live inside the gpuCount > 0 branch above, which meant a CPU
	// or Vulkan-served model silently got neither: the field was accepted,
	// stored, visible in `kubectl get isvc -o yaml`, and dropped (#1503). The
	// pod then scheduled anywhere, and the scheduler's failure message named
	// nodes the user had never asked for. TopologySpreadConstraints and Affinity
	// just below were already unconditional for exactly this reason.
	//
	// GPU-specific entries go first so the user's win on key conflict, matching
	// the previous behaviour for GPU workloads.
	tolerations := append([]corev1.Toleration{}, gpuTolerations...)
	tolerations = append(tolerations, isvc.Spec.Tolerations...)
	if len(tolerations) > 0 {
		deployment.Spec.Template.Spec.Tolerations = tolerations
	}

	if len(gpuNodeSelector) > 0 || len(isvc.Spec.NodeSelector) > 0 {
		nodeSelector := make(map[string]string, len(gpuNodeSelector)+len(isvc.Spec.NodeSelector))
		for k, v := range gpuNodeSelector {
			nodeSelector[k] = v
		}
		for k, v := range isvc.Spec.NodeSelector {
			nodeSelector[k] = v
		}
		deployment.Spec.Template.Spec.NodeSelector = nodeSelector
	}

	// DRA pods get their scheduling fields from the unconditional merge above
	// (GPU toleration when count > 0, shared-pool selector, user entries last):
	// the claim drives placement, not the pod spec, but a GPU-tainted node
	// still needs the toleration (#2061).
	//
	// Applied once, after the merge, so the DRA answer wins over the GPU
	// device-plugin one; resolveRolloutStrategy is the single authority for it.
	if strategy, _ := resolveRolloutStrategy(isvc, model); strategy.Type != "" {
		deployment.Spec.Strategy = strategy
	}

	// TopologySpreadConstraints and Affinity are general pod-scheduling
	// passthrough (not GPU-specific), so apply them regardless of the GPU/DRA
	// path above — e.g. soft-spreading model servers one-per-node.
	if len(isvc.Spec.TopologySpreadConstraints) > 0 {
		deployment.Spec.Template.Spec.TopologySpreadConstraints = isvc.Spec.TopologySpreadConstraints
	}
	if isvc.Spec.Affinity != nil {
		deployment.Spec.Template.Spec.Affinity = isvc.Spec.Affinity
	}

	applyArchAffinity(deployment, backend, isvc)

	return deployment
}

// applyArchAffinity applies architecture-aware placement (#1479). When the
// operator chose the image (no user-supplied spec.image) and the backend
// declares a non-empty set of supported architectures, it constrains the pod to
// nodes of those architectures via a kubernetes.io/arch nodeAffinity. A
// user-supplied image bypasses this entirely, since the operator cannot know
// what a custom image supports and must not guess.
func applyArchAffinity(deployment *appsv1.Deployment, backend RuntimeBackend, isvc *inferencev1alpha1.InferenceService) {
	if isvc.Spec.Image != "" {
		return
	}
	if archs := backend.SupportedArchitectures(); len(archs) > 0 {
		applyArchNodeAffinity(deployment, archs)
	}
}

// applyArchNodeAffinity restricts scheduling to nodes whose architecture is in
// archs, ANDing that requirement into whatever nodeAffinity is already present.
//
// nodeSelectorTerms are ORed and the matchExpressions within a single term are
// ANDed, so the architecture requirement has to go *into* each existing term.
// Appending a term of its own would widen placement instead of narrowing it:
// the user's spec.affinity would become one of two alternatives and the pod
// could schedule on any node of the right architecture, ignoring their pin
// (#1583). preferredDuringScheduling is deliberately untouched, since it
// expresses a preference rather than a constraint.
func applyArchNodeAffinity(deployment *appsv1.Deployment, archs []string) {
	values := make([]string, 0, len(archs))
	for _, a := range archs {
		if a != "" {
			values = append(values, a)
		}
	}
	if len(values) == 0 {
		return
	}
	req := corev1.NodeSelectorRequirement{
		Key:      corev1.LabelArchStable,
		Operator: corev1.NodeSelectorOpIn,
		Values:   values,
	}

	// Deep-copy before mutating: the pod spec's Affinity is assigned straight
	// from isvc.Spec.Affinity, so editing terms in place would write into the
	// cached InferenceService and re-append this requirement on every reconcile.
	affinity := deployment.Spec.Template.Spec.Affinity.DeepCopy()
	if affinity == nil {
		affinity = &corev1.Affinity{}
	}
	if affinity.NodeAffinity == nil {
		affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	if affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}

	selector := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(selector.NodeSelectorTerms) == 0 {
		// No terms means every node qualifies, so the requirement stands alone
		// as the only term. This still narrows rather than widens.
		selector.NodeSelectorTerms = []corev1.NodeSelectorTerm{
			{MatchExpressions: []corev1.NodeSelectorRequirement{req}},
		}
	} else {
		for i := range selector.NodeSelectorTerms {
			selector.NodeSelectorTerms[i].MatchExpressions = append(
				selector.NodeSelectorTerms[i].MatchExpressions, req)
		}
	}
	deployment.Spec.Template.Spec.Affinity = affinity
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

	// With DRA claims the claims allocate the GPUs, so no device-plugin limit:
	// on a DRA-only cluster nodes advertise no nvidia.com/gpu capacity and the
	// limit alone made the pod unschedulable (#2059). gpuCount still sizes the
	// runtime (tensor parallelism, memory flags) on that path. External
	// consumers (llmkube-kueue) make the same call through apiutil.UsesDRAClaims.
	if gpuCount > 0 && !apiutil.UsesDRAClaims(model) {
		res.Limits = corev1.ResourceList{
			gpuResourceName: resource.MustParse(fmt.Sprintf("%d", gpuCount)),
		}
	}

	for _, claim := range modelResourceClaims(model) {
		res.Claims = append(res.Claims, corev1.ResourceClaim{Name: claim.Name})
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
		// Request AND limit, driven by whichever of hostMemory/memory wins.
		// The limit is what keeps an overrun charged to this pod: without one an
		// unbounded container crosses MemoryPressure and stops the kubelet posting
		// status (the node goes NotReady) instead of being OOM-killed. ParseQuantity,
		// not MustParse, for the same reason as ephemeralStorage below: a panic here
		// takes the controller down for every workload rather than failing the one
		// object at fault.
		var memoryQ *resource.Quantity
		if isvc.Spec.Resources.HostMemory != "" {
			if q, err := resource.ParseQuantity(isvc.Spec.Resources.HostMemory); err == nil {
				memoryQ = &q
			}
		} else if isvc.Spec.Resources.Memory != "" {
			if q, err := resource.ParseQuantity(isvc.Spec.Resources.Memory); err == nil {
				memoryQ = &q
			}
		}
		if memoryQ != nil {
			res.Requests[corev1.ResourceMemory] = *memoryQ
			// memoryLimit, when given, decouples the ceiling from the
			// reservation so a workload can burst above what it permanently
			// reserves (#1763). Unset keeps limit == request, so the #1724
			// protection above still applies by default.
			limitQ := memoryQ
			if isvc.Spec.Resources.MemoryLimit != "" {
				if q, err := resource.ParseQuantity(isvc.Spec.Resources.MemoryLimit); err == nil {
					// A limit below the request is not a narrower ceiling, it is
					// a pod the kubelet refuses to admit. Ignoring it leaves the
					// workload running with the previous behaviour rather than
					// failing to schedule on a typo.
					if q.Cmp(*memoryQ) >= 0 {
						limitQ = &q
					}
				}
			}
			res.Limits[corev1.ResourceMemory] = *limitQ
		}
		// Request AND limit. The request is what the scheduler reserves, so it
		// stops placing further pods on a node this download is about to fill.
		// The limit is what keeps an overrun charged to this pod: without one
		// the node crosses its DiskPressure threshold instead and eviction
		// proceeds by QoS class, which can remove unrelated workloads first.
		// ParseQuantity, not MustParse: a CRD pattern rejects malformed values at
		// admission, but MustParse panics, and a panic here takes the controller
		// down for every workload rather than failing the one object at fault.
		// Not worth that blast radius to save an error check.
		if isvc.Spec.Resources.EphemeralStorage != "" {
			if q, err := resource.ParseQuantity(isvc.Spec.Resources.EphemeralStorage); err == nil {
				res.Requests[corev1.ResourceEphemeralStorage] = q
				res.Limits[corev1.ResourceEphemeralStorage] = q
			}
		}
	}

	return res
}

// ParseRuntimeImageOverrides parses the --runtime-images flag value
// ("runtime=image[,runtime=image...]", keys llamacpp|vllm|sglang|tgi) into
// the override map consumed by resolveRuntimeImage. Empty input means no
// overrides. Exported for cmd/main.go.
func ParseRuntimeImageOverrides(flagValue string) (map[string]string, error) {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue == "" {
		return nil, nil
	}
	valid := map[string]bool{"llamacpp": true, "vllm": true, "sglang": true, "tgi": true}
	overrides := map[string]string{}
	for _, pair := range strings.Split(flagValue, ",") {
		key, img, found := strings.Cut(strings.TrimSpace(pair), "=")
		key = strings.ToLower(strings.TrimSpace(key))
		img = strings.TrimSpace(img)
		if !found || key == "" || img == "" {
			return nil, fmt.Errorf("invalid --runtime-images entry %q (expected runtime=image)", pair)
		}
		if !valid[key] {
			return nil, fmt.Errorf("invalid --runtime-images runtime %q (valid: llamacpp, vllm, sglang, tgi)", key)
		}
		overrides[key] = img
	}
	return overrides, nil
}

// defaultRevisionHistoryLimit is how many old ReplicaSets LLMKube-managed
// Deployments keep when the user does not set one. The operator reconciles
// these Deployments, so `kubectl rollout undo` is reverted on the next
// reconcile and the Kubernetes default of 10 only accumulates dead ReplicaSets.
const defaultRevisionHistoryLimit int32 = 3

// revisionHistoryLimitOrDefault returns a copy of the user-set limit (0 is a
// valid user choice and is kept), or the LLMKube default when unset.
func revisionHistoryLimitOrDefault(limit *int32) *int32 {
	v := defaultRevisionHistoryLimit
	if limit != nil {
		v = *limit
	}
	return &v
}
