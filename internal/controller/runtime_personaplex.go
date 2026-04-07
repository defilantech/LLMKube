package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// PersonaPlexBackend generates container configuration for the NVIDIA PersonaPlex
// (Moshi) speech-to-speech inference server.
type PersonaPlexBackend struct{}

func (b *PersonaPlexBackend) ContainerName() string {
	return "personaplex"
}

func (b *PersonaPlexBackend) DefaultImage() string {
	return ""
}

func (b *PersonaPlexBackend) DefaultPort() int32 {
	return 8998
}

func (b *PersonaPlexBackend) NeedsModelInit() bool {
	return false
}

func (b *PersonaPlexBackend) BuildCommand() []string {
	return []string{"/app/moshi/.venv/bin/python", "-m", "moshi.server"}
}

func (b *PersonaPlexBackend) BuildArgs(isvc *inferencev1alpha1.InferenceService, _ *inferencev1alpha1.Model, _ string, _ int32) []string {
	args := []string{"--ssl", "/app/ssl"}

	cfg := isvc.Spec.PersonaPlexConfig
	if cfg != nil {
		if cfg.Quantize4Bit != nil && *cfg.Quantize4Bit {
			args = append(args, "--quantize-4bit")
		}
		if cfg.CPUOffload != nil && *cfg.CPUOffload {
			args = append(args, "--cpu-offload")
		}
	}

	return args
}

func (b *PersonaPlexBackend) BuildProbes(port int32) (startup, liveness, readiness *corev1.Probe) {
	// PersonaPlex uses WebSocket on its main port — TCP socket probes are appropriate
	startup = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt32(port),
			},
		},
		PeriodSeconds:    10,
		TimeoutSeconds:   5,
		FailureThreshold: 180,
	}

	liveness = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt32(port),
			},
		},
		PeriodSeconds:    15,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}

	readiness = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt32(port),
			},
		},
		PeriodSeconds:    10,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}

	return startup, liveness, readiness
}

// BuildEnv returns environment variables for the PersonaPlex container,
// including HF_TOKEN from a Secret reference if configured.
func (b *PersonaPlexBackend) BuildEnv(isvc *inferencev1alpha1.InferenceService) []corev1.EnvVar {
	var env []corev1.EnvVar

	cfg := isvc.Spec.PersonaPlexConfig
	if cfg != nil && cfg.HFTokenSecretRef != nil {
		env = append(env, corev1.EnvVar{
			Name: "HF_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: cfg.HFTokenSecretRef,
			},
		})
	}

	// Disable torch.compile for faster startup
	env = append(env, corev1.EnvVar{
		Name:  "NO_TORCH_COMPILE",
		Value: "1",
	})

	return env
}
