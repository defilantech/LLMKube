package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// vllmLog is a package-level logger used for construction-time warnings from
// BuildArgs. The reconciler surfaces the same issues as a status condition via
// ValidateVLLMConfig; the log line is for operator debugging.
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
func (b *VLLMBackend) DefaultImage() string     { return "vllm/vllm-openai:latest" }
func (b *VLLMBackend) DefaultPort() int32       { return 8000 }
func (b *VLLMBackend) NeedsModelInit() bool     { return true }
func (b *VLLMBackend) DefaultHPAMetric() string { return "vllm:num_requests_running" }

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
		// KV cache dtype: emit unless unset or explicitly "auto" (vLLM's default).
		// Custom value (e.g. TurboQuant turbo2 from vLLM v0.20+) wins over the
		// enum-validated standard field, mirroring llama.cpp's resolveCacheType.
		if resolved := resolveKVCacheDtype(cfg.KVCacheCustomDtype, cfg.KVCacheDtype); resolved != "" && resolved != "auto" {
			args = append(args, "--kv-cache-dtype", resolved)
		}
		// Prefix caching: only emit when user explicitly opted in (true).
		// vLLM's own default handles the nil/false case.
		if cfg.EnablePrefixCaching != nil && *cfg.EnablePrefixCaching {
			args = append(args, "--enable-prefix-caching")
		}
		// Chunked prefill: same policy as prefix caching — opt-in only.
		if cfg.EnableChunkedPrefill != nil && *cfg.EnableChunkedPrefill {
			args = append(args, "--enable-chunked-prefill")
		}
		if cfg.MaxNumBatchedTokens != nil {
			args = append(args, "--max-num-batched-tokens", fmt.Sprintf("%d", *cfg.MaxNumBatchedTokens))
		}
		if cfg.AttentionBackend != "" {
			args = append(args, "--attention-backend", cfg.AttentionBackend)
		}
		// Speculative decoding: require both Enabled=true and a non-empty
		// draft Model ref. Silent-skip with a log line when misconfigured;
		// the reconciler also sets a status condition via ValidateVLLMConfig.
		if cfg.Speculative != nil && cfg.Speculative.Enabled != nil && *cfg.Speculative.Enabled {
			if cfg.Speculative.Model == "" {
				vllmLog.Error(nil,
					"speculative decoding enabled but spec.vllmConfig.speculative.model is empty; skipping speculative flags",
					"inferenceService", isvc.Name,
					"namespace", isvc.Namespace,
				)
			} else {
				args = append(args, "--speculative-model", cfg.Speculative.Model)
				if cfg.Speculative.NumSpeculativeTokens != nil {
					args = append(args, "--num-speculative-tokens",
						fmt.Sprintf("%d", *cfg.Speculative.NumSpeculativeTokens))
				}
			}
		}
		if cfg.EnableExpertParallel != nil && *cfg.EnableExpertParallel {
			args = append(args, "--enable-expert-parallel")
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

// resolveKVCacheDtype returns the custom vLLM KV cache type when set,
// otherwise the enum-validated standard value (dereferenced; nil → ""). Lets
// users opt into vLLM image-specific cache formats (TurboQuant turbo2 from
// v0.20+, future variants) without expanding the CRD enum on every release,
// while keeping the standard field discoverable for the common case. Mirrors
// resolveCacheType on the llama.cpp side.
func resolveKVCacheDtype(custom string, standard *string) string {
	if custom != "" {
		return custom
	}
	if standard == nil {
		return ""
	}
	return *standard
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
