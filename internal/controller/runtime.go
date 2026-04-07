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

// resolveBackend returns the RuntimeBackend for the given InferenceService.
func resolveBackend(isvc *inferencev1alpha1.InferenceService) RuntimeBackend {
	switch isvc.Spec.Runtime {
	case "personaplex":
		return &PersonaPlexBackend{}
	case "generic":
		return &GenericBackend{}
	default:
		return &LlamaCppBackend{}
	}
}
