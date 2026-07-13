package controller

import (
	"context"
	"fmt"
	"net/http"

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

func (b *GenericBackend) NeedsModelInit() bool     { return false }
func (b *GenericBackend) DefaultHPAMetric() string { return "" }

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

// IdleProbe returns a probe closure that reads the
// AnnotationIdleEndpoint annotation from the InferenceService. If absent,
// returns (false, errIdleUnsupported). If present, GETs the annotated path and
// returns true on 2xx (idle), false on non-2xx (busy).
func (b *GenericBackend) IdleProbe(isvc *inferencev1alpha1.InferenceService, client *http.Client) func(ctx context.Context, baseURL string) (bool, error) {
	path, ok := isvc.Annotations[inferencev1alpha1.AnnotationIdleEndpoint]
	if !ok {
		return func(ctx context.Context, baseURL string) (bool, error) {
			return false, errIdleUnsupported
		}
	}

	return func(ctx context.Context, baseURL string) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
		if err != nil {
			return false, fmt.Errorf("failed to create request: %w", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Errorf("idle endpoint probe failed: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return true, nil
		}
		return false, nil
	}
}
