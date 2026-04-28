package controller

import (
	"fmt"

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
		source = model.Spec.Source
	}

	args := []string{
		"--model-id", source,
		"--hostname", "0.0.0.0",
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
