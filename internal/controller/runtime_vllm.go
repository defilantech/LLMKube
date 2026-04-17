package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

type VLLMBackend struct{}

func (b *VLLMBackend) ContainerName() string    { return "vllm" }
func (b *VLLMBackend) DefaultImage() string     { return "vllm/vllm-openai:latest" }
func (b *VLLMBackend) DefaultPort() int32       { return 8000 }
func (b *VLLMBackend) NeedsModelInit() bool     { return true }
func (b *VLLMBackend) DefaultHPAMetric() string { return "vllm:num_requests_running" }

func (b *VLLMBackend) BuildArgs(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelPath string, port int32) []string {
	source := modelPath
	if source == "" {
		source = model.Spec.Source
	}
	args := []string{
		"--model", source,
		"--host", "0.0.0.0",
		"--port", fmt.Sprintf("%d", port),
	}

	cfg := isvc.Spec.VLLMConfig
	if cfg != nil {
		if cfg.TensorParallelSize != nil && *cfg.TensorParallelSize > 1 {
			args = append(args, "--tensor-parallel-size", fmt.Sprintf("%d", *cfg.TensorParallelSize))
		}
		if cfg.MaxModelLen != nil {
			args = append(args, "--max-model-len", fmt.Sprintf("%d", *cfg.MaxModelLen))
		}
		if cfg.Quantization != "" {
			args = append(args, "--quantization", cfg.Quantization)
		}
		if cfg.Dtype != "" {
			args = append(args, "--dtype", cfg.Dtype)
		}
		if cfg.EnablePrefixCaching != nil && *cfg.EnablePrefixCaching {
			args = append(args, "--enable-prefix-caching")
		}
		if cfg.AttentionBackend != "" {
			args = append(args, "--attention-backend", cfg.AttentionBackend)
		}
	}

	gpuCount := resolveGPUCount(isvc, model)
	if gpuCount > 1 && (cfg == nil || cfg.TensorParallelSize == nil) {
		args = append(args, "--tensor-parallel-size", fmt.Sprintf("%d", gpuCount))
	}

	if len(isvc.Spec.ExtraArgs) > 0 {
		args = append(args, isvc.Spec.ExtraArgs...)
	}

	return args
}

func (b *VLLMBackend) BuildProbes(port int32) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
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

func (b *VLLMBackend) BuildEnv(isvc *inferencev1alpha1.InferenceService) []corev1.EnvVar {
	cfg := isvc.Spec.VLLMConfig
	if cfg != nil && cfg.HFTokenSecretRef != nil {
		return []corev1.EnvVar{{
			Name:      "HF_TOKEN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: cfg.HFTokenSecretRef},
		}}
	}
	return nil
}
