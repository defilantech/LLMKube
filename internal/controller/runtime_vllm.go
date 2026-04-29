package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// vllmLog is a package-level logger used for construction-time warnings from
// BuildArgs.
var vllmLog = logf.Log.WithName("runtime.vllm")

// Condition types set by the InferenceService reconciler when a vLLM-specific
// portion of the spec is structurally invalid but not fatal to reconciliation.
const (
	// ConditionVLLMSpecValid is True when the VLLMConfig is internally
	// consistent. It is False when, for example, speculative decoding is
	// enabled without a draft model reference.
	ConditionVLLMSpecValid = "VLLMSpecValid"

	// RuntimeVLLM is the InferenceService.Spec.Runtime value that selects the
	// vLLM backend. Kept as a named constant so callers can cross-check the
	// runtime without duplicating the string literal.
	RuntimeVLLM = "vllm"
)

type VLLMBackend struct{}

func (b *VLLMBackend) ContainerName() string    { return "vllm" }
func (b *VLLMBackend) DefaultImage() string     { return "vllm/vllm-openai:v0.20.0" }
func (b *VLLMBackend) DefaultPort() int32       { return 8000 }
func (b *VLLMBackend) NeedsModelInit() bool     { return true }
func (b *VLLMBackend) DefaultHPAMetric() string { return "vllm:num_requests_running" }

// DisableServiceLinks returns true so the operator emits Pods with
// `enableServiceLinks: false`. vLLM v0.20+ logs a warning for every K8s
// service-link env var that matches the `VLLM_*` prefix; in a namespace with
// multiple vLLM Services that's per-pod per-other-service noise. DNS-based
// service discovery is unaffected — the env vars were a legacy mechanism.
func (b *VLLMBackend) DisableServiceLinks() bool { return true }

// BuildArgs generates the vLLM server CLI arguments. Arguments are emitted in a
// deterministic order so snapshot-style tests and diff reviews stay stable:
//
//  1. --model, --host, --port (always)
//  2. Typed VLLMConfig flags, top-to-bottom as declared on the struct
//  3. Tensor parallel auto-derived from GPU count (when user did not set it)
//  4. ExtraArgs (user escape hatch, always last so user flags win)
//
// Flags follow a strict "only-emit-on-explicit-opt-in" rule: boolean toggles
// whose zero value matches vLLM's own default are only emitted when the user
// set them to true. This keeps generated pod specs minimal and lets us track
// vLLM upstream default changes without needing an operator release.
func (b *VLLMBackend) BuildArgs(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelPath string, port int32) []string {
	source := modelPath
	if source == "" {
		source = model.Spec.Source
	}
	// vLLM v0.20 deprecated --model in favor of a positional argument; --model
	// will be removed in a future minor. The positional form is supported by
	// every vLLM release we run against, so this works on both v0.19 (silent
	// accept) and v0.20+ (no deprecation warning).
	args := []string{
		source,
		"--host", "0.0.0.0",
		"--port", fmt.Sprintf("%d", port),
	}

	var err error

	cfg := isvc.Spec.VLLMConfig
	if cfg != nil {
		args = appendTensorParallelSize(args, cfg.TensorParallelSize)
		args = appendMaxModelLen(args, cfg.MaxModelLen)
		args = appendQuantization(args, cfg.Quantization)
		args = appendDtype(args, cfg.Dtype)
		args = appendKVCacheDtype(args, cfg.KVCacheDtype, cfg.KVCacheCustomDtype)
		args = appendEnablePrefixCaching(args, cfg.EnablePrefixCaching)
		args = appendEnableChunkedPrefill(args, cfg.EnableChunkedPrefill)
		args = appendMaxNumBatchedTokens(args, cfg.MaxNumBatchedTokens)
		args = appendAttentionBackend(args, cfg.AttentionBackend)
		args, err = appendSpeculativeModel(args, cfg.Speculative)
		if err != nil {
			vllmLog.Info(
				err.Error(),
				"inferenceService", isvc.Name,
				"namespace", isvc.Namespace,
			)
		}
		args = appendEnableExpertParallel(args, cfg.EnableExpertParallel)
	}

	gpuCount := resolveGPUCount(isvc, model)
	if gpuCount > 1 && (cfg == nil || cfg.TensorParallelSize == nil) {
		args = appendTensorParallelSize(args, &gpuCount)
	}

	args, err = appendMaxNumSeqsArgs(args, isvc.Spec.ParallelSlots, isvc.Spec.ExtraArgs)
	if err != nil {
		vllmLog.Info(
			err.Error(),
			"inferenceService", isvc.Name,
			"namespace", isvc.Namespace,
		)
	}

	if len(isvc.Spec.ExtraArgs) > 0 {
		args = append(args, isvc.Spec.ExtraArgs...)
	}

	return args
}

// ValidateVLLMConfig checks the VLLMConfig for structurally invalid
// combinations that are non-fatal to reconciliation but should be surfaced as
// a status condition. Returns (reason, message) when invalid; empty strings
// when the config is fine. The caller is expected to translate these into a
// metav1.Condition on the InferenceService status.
//
// Today it only checks speculative decoding; add cases here as the CRD grows.
func ValidateVLLMConfig(isvc *inferencev1alpha1.InferenceService) (reason, message string) {
	if isvc == nil || isvc.Spec.VLLMConfig == nil {
		return "", ""
	}
	cfg := isvc.Spec.VLLMConfig
	if cfg.Speculative != nil && cfg.Speculative.Enabled != nil && *cfg.Speculative.Enabled {
		if cfg.Speculative.Model == "" {
			return "SpeculativeMissingModel",
				"spec.vllmConfig.speculative.enabled is true but spec.vllmConfig.speculative.model is empty; speculative decoding flags will be skipped"
		}
	}
	return "", ""
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
