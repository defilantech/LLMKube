/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package controller — sglang runtime backend. Mirrors the vLLM backend
// (runtime_vllm.go + runtime_vllm_args.go) but emits SGLang's CLI flags.
// SGLang is GPU-only; CPU-only deployments will fail to start the container.
// The `sglangLog` package logger carries construction-time warnings from
// BuildArgs that aren't fatal to reconciliation.

package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var sglangLog = logf.Log.WithName("runtime.sglang")

// RuntimeSGLANG is the InferenceService.Spec.Runtime value that selects the
// SGLang backend.
const RuntimeSGLANG = "sglang"

// ConditionSGLangSpecValid is the metav1.Condition type set by the reconciler
// when the SGLangConfig portion of the spec is structurally invalid but not
// fatal to reconciliation (e.g., speculative enabled without an algorithm).
const ConditionSGLangSpecValid = "SGLangSpecValid"

// sglangCUDAImage is the pinned default CUDA image. Tag verified at PR time
// against Docker Hub; bump in a follow-up if a newer SGLang release is required.
const sglangCUDAImage = "lmsysorg/sglang:v0.5.15-cu129"

// sglangROCmImage is the AMD ROCm variant. Selected by resolveRuntimeImage
// when model.Spec.Hardware.GPU.Vendor == "amd".
const sglangROCmImage = "lmsysorg/sglang:v0.5.15-rocm720-mi30x"

// SGLangBackend generates container configuration for the SGLang inference
// server (https://github.com/sgl-project/sglang).
type SGLangBackend struct{}

func (b *SGLangBackend) ContainerName() string    { return "sglang" }
func (b *SGLangBackend) DefaultImage() string     { return sglangCUDAImage }
func (b *SGLangBackend) DefaultPort() int32       { return 30000 }
func (b *SGLangBackend) NeedsModelInit() bool     { return true }
func (b *SGLangBackend) DefaultHPAMetric() string { return "sglang:num_requests_running" }

// BuildArgs generates the SGLang server CLI arguments. Order:
//  1. --model-path, --host, --port (always)
//  2. --served-model-name (auto-derived from ModelRef / model name)
//  3. typed SGLangConfig flags in declaration order
//  4. --tp auto-derived from gpuCount when unset
//  5. --is-embedding when spec.mode == "embedding" (skip if user set it)
//  6. spec.extraArgs last (user wins on collision)
//
// GPU-only flags (memFractionStatic) log a warning when set on a CPU model.
func (b *SGLangBackend) BuildArgs(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelPath string, port int32) []string {
	source := modelPath
	if source == "" {
		source = normalizeHFSource(model.Spec.Source)
	}
	args := []string{
		"--model-path", source,
		// Bind the dual-stack wildcard so pods are reachable on IPv6-only
		// clusters (#972). SGLang keeps the last occurrence of a repeated
		// flag, so users can override via extraArgs ("--host", "0.0.0.0").
		"--host", "::",
		"--port", fmt.Sprintf("%d", port),
		// SGLang requires --enable-metrics to expose /metrics (unlike vLLM which
		// always exposes it, and llama.cpp which always passes --metrics). Without
		// this flag, PodMonitor scrapes fail and HPA cannot read the custom metric.
		"--enable-metrics",
	}

	// --served-model-name: skip if user already set it in extraArgs.
	servedName := isvc.Spec.ModelRef
	if servedName == "" && model != nil {
		servedName = model.Name
	}
	if servedName != "" && !hasMatchingExtraArg(isvc.Spec.ExtraArgs, "served-model-name") {
		args = append(args, "--served-model-name", servedName)
	}

	cfg := isvc.Spec.SGLangConfig
	gpuCount := resolveGPUCount(isvc, model)
	if cfg != nil {
		args = sglangAppendTensorParallelSize(args, cfg.TensorParallelSize)
		args = sglangAppendExpertParallelSize(args, cfg.ExpertParallelSize)
		args = sglangAppendDataParallelSize(args, cfg.DataParallelSize)
		args = sglangAppendContextLength(args, cfg.ContextLength)
		if cfg.MemFractionStatic != nil && gpuCount == 0 {
			// TODO: This guard uses resolveGPUCount (device-plugin only). DRA-only
			// models (resourceClaims > 0, gpuCount == 0) will spuriously skip
			// --mem-fraction-static here. Switch to hasGPUPresent(isvc, model) when
			// the shared vLLM guard is also updated. See #1060.
			sglangLog.Info(
				"spec.sglangConfig.memFractionStatic is defined with no GPU hardware; skipping --mem-fraction-static",
				"inferenceService", isvc.Name,
				"namespace", isvc.Namespace,
			)
		} else {
			args = sglangAppendMemFractionStatic(args, cfg.MemFractionStatic)
		}
		args = sglangAppendChunkedPrefillSize(args, cfg.ChunkedPrefillSize)
		args = sglangAppendMaxRunningRequests(args, cfg.MaxRunningRequests)
		args = sglangAppendQuantization(args, cfg.Quantization)
		args = sglangAppendKVCacheDtype(args, cfg.KVCacheDtype, cfg.KVCacheCustomDtype)
		args = sglangAppendAttentionBackend(args, cfg.AttentionBackend)
		args = sglangAppendEnablePrefixCaching(args, cfg.EnablePrefixCaching)
		args = sglangAppendToolCallParser(args, cfg.ToolCallParser)
		args = sglangAppendReasoningParser(args, cfg.ReasoningParser)
		args = sglangAppendChatTemplate(args, cfg.ChatTemplate)
		args = sglangAppendSpeculative(args, cfg.Speculative)
		args = sglangAppendLoraModules(args, cfg.LoraModules)
		args = sglangAppendMaxLoraRank(args, cfg.MaxLoraRank)
		args = sglangAppendLoraTargetModules(args, cfg.LoraTargetModules)
	}

	// Auto-derive --tp when user didn't set it.
	if gpuCount > 1 && (cfg == nil || cfg.TensorParallelSize == nil) {
		args = append(args, "--tp", fmt.Sprintf("%d", gpuCount))
	}

	// Mode handling: --is-embedding (skip if user already set it).
	if resolveServingMode(isvc) == servingModeEmbedding && !hasMatchingExtraArg(isvc.Spec.ExtraArgs, "is-embedding") {
		args = append(args, "--is-embedding")
	}

	// ExtraArgs last (user wins).
	if len(isvc.Spec.ExtraArgs) > 0 {
		args = append(args, isvc.Spec.ExtraArgs...)
	}

	return args
}

// ValidateSGLangConfig checks the SGLangConfig for structurally invalid
// combinations that are non-fatal to reconciliation but should be surfaced
// as a status condition. Returns (reason, message) when invalid; empty
// strings when fine.
func ValidateSGLangConfig(isvc *inferencev1alpha1.InferenceService) (reason, message string) {
	if isvc == nil || isvc.Spec.SGLangConfig == nil {
		return "", ""
	}
	cfg := isvc.Spec.SGLangConfig
	if cfg.Speculative != nil && cfg.Speculative.Enabled != nil && *cfg.Speculative.Enabled {
		if cfg.Speculative.Algorithm == "" || cfg.Speculative.DraftModelPath == "" {
			return "SpeculativeMissingConfig",
				"spec.sglangConfig.speculative.enabled is true but algorithm/draftModelPath is empty; speculative decoding flags will be skipped"
		}
	}
	return "", ""
}

// BuildCommand returns the entrypoint for the SGLang container. SGLang
// launches via a Python module rather than a bare binary, mirroring
// PersonaPlexBackend.
func (b *SGLangBackend) BuildCommand() []string {
	return []string{"python3", "-m", "sglang.launch_server"}
}

// BuildProbes returns startup, liveness, and readiness probes. SGLang
// exposes /health (cheap liveness) and /health_generate (runs a token,
// accurate readiness but slow on cold start). Startup tolerates 180
// failures (~30 minutes at 10s period) to cover model load + warmup.
func (b *SGLangBackend) BuildProbes(port int32) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
	startup := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health_generate",
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
				Path: "/health_generate",
				Port: intstr.FromInt32(port),
			},
		},
		PeriodSeconds:    10,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}
	return startup, liveness, readiness
}

// BuildEnv returns HF_TOKEN from SGLangConfig.HFTokenSecretRef when set.
// SGLang reads HF_TOKEN from the environment to authenticate gated-model
// downloads from HuggingFace Hub.
func (b *SGLangBackend) BuildEnv(isvc *inferencev1alpha1.InferenceService) []corev1.EnvVar {
	cfg := isvc.Spec.SGLangConfig
	if cfg != nil && cfg.HFTokenSecretRef != nil {
		return []corev1.EnvVar{{
			Name:      "HF_TOKEN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: cfg.HFTokenSecretRef},
		}}
	}
	return nil
}
