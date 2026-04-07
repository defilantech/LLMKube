package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// GenericBackend deploys user-provided containers with custom command, args, env, and probes.
// It does not generate any runtime-specific configuration.
type GenericBackend struct{}

func (b *GenericBackend) ContainerName() string {
	return "inference-server"
}

func (b *GenericBackend) DefaultImage() string {
	return ""
}

func (b *GenericBackend) DefaultPort() int32 {
	return 8080
}

func (b *GenericBackend) NeedsModelInit() bool {
	return false
}

func (b *GenericBackend) BuildArgs(isvc *inferencev1alpha1.InferenceService, _ *inferencev1alpha1.Model, _ string, _ int32) []string {
	return isvc.Spec.Args
}

func (b *GenericBackend) BuildProbes(port int32) (startup, liveness, readiness *corev1.Probe) {
	// Default to TCP socket probes for generic containers
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
