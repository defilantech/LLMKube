package controller

import (
	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// RuntimeBackend generates runtime-specific container configuration
// for a Kubernetes Deployment. Each implementation handles a different
// inference server (llama.cpp, generic containers, etc.).
type RuntimeBackend interface {
	// ContainerName returns the main container name.
	ContainerName() string

	// DefaultImage returns the default container image for this runtime.
	DefaultImage() string

	// DefaultPort returns the default container port.
	DefaultPort() int32

	// BuildArgs generates the container arguments from the InferenceService,
	// Model, model file path, and container port.
	BuildArgs(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelPath string, port int32) []string

	// BuildProbes returns startup, liveness, and readiness probes.
	BuildProbes(port int32) (startup, liveness, readiness *corev1.Probe)

	// NeedsModelInit returns true if this runtime needs an init container
	// to download the model file.
	NeedsModelInit() bool
}

// resolveBackend returns the RuntimeBackend for the given InferenceService.
// CommandBuilder is optionally implemented by backends that need a custom container entrypoint.
type CommandBuilder interface {
	BuildCommand() []string
}

// EnvBuilder is optionally implemented by backends that generate runtime-specific env vars.
type EnvBuilder interface {
	BuildEnv(isvc *inferencev1alpha1.InferenceService) []corev1.EnvVar
}

// HPAMetricProvider is optionally implemented by backends that have a default autoscaling metric.
type HPAMetricProvider interface {
	DefaultHPAMetric() string
}

// ServiceLinksOptOut is optionally implemented by backends that should run with
// the legacy Kubernetes service-link env-var injection disabled. Returning true
// sets `enableServiceLinks: false` on the Pod spec, which suppresses the
// `<UPPER_SERVICE_NAME>_*` env vars Kubernetes auto-injects for every Service
// in the namespace. vLLM v0.20+ implements a strict env-var validator that
// flags any `VLLM_*` env var as unknown — meaning every other vLLM Service in
// the namespace produces a warning per env var per pod. DNS-based service
// discovery is unaffected.
type ServiceLinksOptOut interface {
	DisableServiceLinks() bool
}

// resolveBackend returns the RuntimeBackend for the given InferenceService.
func resolveBackend(isvc *inferencev1alpha1.InferenceService) RuntimeBackend {
	switch isvc.Spec.Runtime {
	case "personaplex":
		return &PersonaPlexBackend{}
	case RuntimeVLLM:
		return &VLLMBackend{}
	case "tgi":
		return &TGIBackend{}
	case "generic":
		return &GenericBackend{}
	default:
		return &LlamaCppBackend{}
	}
}

// runtimeNameLabel returns a stable, lowercase identifier for the runtime
// selected by isvc.Spec.Runtime. The returned value is used as the
// `inference.llmkube.dev/runtime` pod label so Prometheus scrapes can attach a
// `runtime` series label without parsing isvc back out of the cluster. Empty
// spec.Runtime maps to "llamacpp" because that's resolveBackend's default.
func runtimeNameLabel(isvc *inferencev1alpha1.InferenceService) string {
	if isvc == nil || isvc.Spec.Runtime == "" {
		return "llamacpp"
	}
	return isvc.Spec.Runtime
}

// resolveGPUCount determines the GPU count from Model spec or InferenceService spec.
func resolveGPUCount(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) int32 {
	if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil && model.Spec.Hardware.GPU.Count > 0 {
		return model.Spec.Hardware.GPU.Count
	}
	if isvc.Spec.Resources != nil && isvc.Spec.Resources.GPU > 0 {
		return isvc.Spec.Resources.GPU
	}
	return 0
}

// hasGPUPresent returns true when the workload has GPU access through either
// device-plugin (gpuCount > 0) or DRA (resourceClaims).
// Use this instead of gpuCount > 0 for decisions about GPU-dependent runtime args.
func hasGPUPresent(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) bool {
	if resolveGPUCount(isvc, model) > 0 {
		return true
	}
	if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil {
		return len(model.Spec.Hardware.GPU.ResourceClaims) > 0
	}
	return false
}
