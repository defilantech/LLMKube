package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

type TGIBackend struct{}

func (b *TGIBackend) ContainerName() string { return "tgi" }
func (b *TGIBackend) DefaultImage() string {
	return "ghcr.io/huggingface/text-generation-inference:latest"
}
func (b *TGIBackend) DefaultPort() int32       { return 80 }
func (b *TGIBackend) NeedsModelInit() bool     { return false }
func (b *TGIBackend) DefaultHPAMetric() string { return "tgi:queue_size" }

func (b *TGIBackend) BuildArgs(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelPath string, port int32) []string {
	source := modelPath
	if source == "" {
		source = normalizeHFSource(model.Spec.Source)
	}

	args := []string{
		"--model-id", source,
		// Bind the dual-stack wildcard so pods are reachable on IPv6-only
		// clusters (#972). With the default net.ipv6.bindv6only=0, :: also
		// accepts IPv4, so IPv4-only and dual-stack clusters keep working.
		"--hostname", "::",
		"--port", fmt.Sprintf("%d", port),
	}

	cfg := isvc.Spec.TGIConfig
	if cfg != nil {
		if cfg.Quantize != "" {
			args = append(args, "--quantize", cfg.Quantize)
		}
		if cfg.MaxInputLength != nil {
			args = append(args, "--max-input-length", fmt.Sprintf("%d", *cfg.MaxInputLength))
		}
		if cfg.MaxTotalTokens != nil {
			args = append(args, "--max-total-tokens", fmt.Sprintf("%d", *cfg.MaxTotalTokens))
		}
		if cfg.Dtype != "" {
			args = append(args, "--dtype", cfg.Dtype)
		}
	}

	gpuCount := resolveGPUCount(isvc, model)
	if gpuCount > 1 {
		args = append(args, "--num-shard", fmt.Sprintf("%d", gpuCount))
	}

	return args
}

func (b *TGIBackend) BuildProbes(port int32) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
	startup := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt32(port),
			},
		},
		PeriodSeconds:    10,
		TimeoutSeconds:   5,
		FailureThreshold: 180,
	}
	liveness := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt32(port),
			},
		},
		PeriodSeconds:    15,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}
	readiness := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt32(port),
			},
		},
		PeriodSeconds:    10,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}
	return startup, liveness, readiness
}

// IdleProbe returns a probe closure that checks TGI /metrics for
// `tgi_batch_current_size` gauge. Idle when value == 0. Absent metric returns
// (false, nil) — fail-closed, treats unknown as busy.
func (b *TGIBackend) IdleProbe(_ *inferencev1alpha1.InferenceService, client *http.Client) func(ctx context.Context, baseURL string) (bool, error) {
	return func(ctx context.Context, baseURL string) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/metrics", nil)
		if err != nil {
			return false, fmt.Errorf("failed to create request: %w", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Errorf("failed to query /metrics: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("/metrics returned status %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, fmt.Errorf("failed to read /metrics response: %w", err)
		}

		sum, found := parsePrometheusGaugeSum(string(body), "tgi_batch_current_size")
		if !found {
			return false, nil
		}
		return sum == 0, nil
	}
}

func (b *TGIBackend) BuildEnv(isvc *inferencev1alpha1.InferenceService) []corev1.EnvVar {
	cfg := isvc.Spec.TGIConfig
	if cfg != nil && cfg.HFTokenSecretRef != nil {
		return []corev1.EnvVar{{
			Name:      "HUGGING_FACE_HUB_TOKEN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: cfg.HFTokenSecretRef},
		}}
	}
	return nil
}
