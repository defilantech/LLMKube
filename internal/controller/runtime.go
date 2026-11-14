package controller

import (
	"context"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/apiutil"
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

	// BuildArgs renders the runtime's argv. draftPath is the in-container path
	// to speculative-decoding draft weights, or "" when the service configures
	// none; consumed by LlamaCppBackend and VLLMBackend.
	BuildArgs(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelPath string, draftPath string, port int32) []string

	// BuildProbes returns startup, liveness, and readiness probes.
	BuildProbes(port int32) (startup, liveness, readiness *corev1.Probe)

	// NeedsModelInit returns true if this runtime needs an init container
	// to download the model file.
	NeedsModelInit() bool

	// SupportedArchitectures returns the CPU architectures the backend's
	// operator-chosen image supports, as kubernetes.io/arch label values
	// (e.g. "amd64"). An empty/nil result means the image is genuinely
	// multi-arch (or unknown), so no architecture constraint is applied.
	// A user-supplied spec.image always bypasses this constraint, since the
	// operator cannot know what a custom image supports (#1479).
	SupportedArchitectures() []string
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

// MultiNodeArgsBuilder is optionally implemented by backends that can run one
// process per node as a single serving group (spec.multiNode). rank is the
// member's index in spec.multiNode.members; rank 0 serves the endpoint. The
// returned argv is appended to BuildArgs' output and the env is prepended to
// the container's env (user env still wins on conflict).
type MultiNodeArgsBuilder interface {
	BuildMultiNodeArgs(isvc *inferencev1alpha1.InferenceService, rank int32) []string
	BuildMultiNodeEnv(isvc *inferencev1alpha1.InferenceService, rank int32) []corev1.EnvVar
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

// IdleDetector is optionally implemented by backends that can probe replica
// idleness for drain-before-roll. The returned probe function is called with
// the base URL of a single replica (scheme + host:port) and returns true when
// the replica has no in-flight requests. The isvc parameter carries annotations
// such as AnnotationIdleEndpoint for generic runtimes; the client is reused
// across probes. Backends that do not implement this interface cause the
// reconciler to skip idle detection and immediately proceed with the rollout.
type IdleDetector interface {
	IdleProbe(isvc *inferencev1alpha1.InferenceService, client *http.Client) func(ctx context.Context, baseURL string) (bool, error)
}

// ProbeCustomizer is optionally implemented by backends that derive probe
// content from the service spec rather than the runtime alone. It runs after
// BuildProbes and before spec.probeOverrides is applied, so a user override
// still wins over anything the backend customizes.
type ProbeCustomizer interface {
	CustomizeProbes(isvc *inferencev1alpha1.InferenceService, port int32, startup, liveness, readiness *corev1.Probe)
}

// SharedMemoryRequirer is optionally implemented by backends that run several
// worker processes on one node and exchange data through /dev/shm (tensor,
// pipeline or data parallelism, NCCL's intra-node transport). When it returns
// true, the deployment builder mounts a memory-backed emptyDir at /dev/shm,
// because the container runtime's default is 64 MiB (#2099). gpuCount is the
// resolved device-plugin GPU count, 0 for DRA-allocated GPUs.
type SharedMemoryRequirer interface {
	NeedsSharedMemory(isvc *inferencev1alpha1.InferenceService, gpuCount int32) bool
}

// anyAboveOne reports whether any of the given parallelism sizes is set above 1.
func anyAboveOne(sizes ...*int32) bool {
	for _, s := range sizes {
		if s != nil && *s > 1 {
			return true
		}
	}
	return false
}

// resolveBackend returns the RuntimeBackend for the given InferenceService.
// Declared as a var so tests can stub it.
var resolveBackend = func(isvc *inferencev1alpha1.InferenceService) RuntimeBackend {
	switch isvc.Spec.Runtime {
	case "personaplex":
		return &PersonaPlexBackend{}
	case RuntimeVLLM:
		return &VLLMBackend{}
	case RuntimeLlamaCppRouter:
		return &LlamaCppRouterBackend{}
	case "tgi":
		return &TGIBackend{}
	case RuntimeSGLANG:
		return &SGLangBackend{}
	case "generic":
		return &GenericBackend{}
	default:
		return &LlamaCppBackend{}
	}
}

// directoryOrientedRuntime reports whether a runtime loads a model from a
// directory (the full Hugging Face model tree) rather than a single weights
// file. Used to decide whether to hand a multi-file model its staged directory
// instead of the primary file (#1157).
func directoryOrientedRuntime(runtime string) bool {
	switch runtime {
	case RuntimeVLLM, RuntimeSGLANG:
		return true
	default:
		return false
	}
}

// validateRuntimePlacement rejects a metal-agent-only runtime (mlx-server,
// omlx, vllm-swift, ollama, tensorfold) on a Model the metal-agent will not
// pick up. Those runtimes have no in-cluster backend: resolveBackend would fall
// through to llama.cpp and serve something other than what the CR asked for. Placement is
// decided by the referenced Model's accelerator (the same test the agent's
// watcher applies), which CEL on the InferenceService cannot see, so this runs
// in the validating webhook and again at reconcile. A nil Model (not created
// yet at admission) is undecidable and passes; the reconciler re-checks with
// the real Model.
func validateRuntimePlacement(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) error {
	runtime := isvc.Spec.Runtime
	if model != nil && inferencev1alpha1.IsHailoSource(model.Spec.Source) && runtime != "" &&
		runtime != inferencev1alpha1.RuntimeOllama {
		return fmt.Errorf("model %q has a hailo:// source, which only the ollama runtime (hailo-ollama) serves; "+
			"spec.runtime is %q", model.Name, runtime)
	}
	if !inferencev1alpha1.IsMetalOnlyRuntime(runtime) || model == nil || isMetalModel(model) {
		return nil
	}
	accel := "accelerator unset"
	if model.Spec.Hardware != nil && model.Spec.Hardware.Accelerator != "" {
		accel = fmt.Sprintf("accelerator %q", model.Spec.Hardware.Accelerator)
	}
	return fmt.Errorf(
		"spec.runtime %q is served only by the metal-agent on Apple Silicon, but Model %q has "+
			"hardware.%s; set the Model's hardware.accelerator to metal, or choose an in-cluster "+
			"runtime (llamacpp, llamacpp-router, vllm, sglang, tgi, personaplex, generic)",
		runtime, model.Name, accel)
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
	return apiutil.GPUCount(isvc, model)
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
