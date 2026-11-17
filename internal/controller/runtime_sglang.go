/*
Copyright 2026.

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
	"context"
	"fmt"
	"io"
	"net/http"

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

// sglangInvalidAttentionBackend is a value the attentionBackend enum admits
// that SGLang rejects: it is not among SGLang's --attention-backend choices in
// any release checked (v0.4.6, v0.5.0, v0.5.12, v0.5.15, v0.5.22).
const sglangInvalidAttentionBackend = "flash_attn"

// sglangRemovedAttentionBackends are enum values SGLang accepted through
// v0.5.19 and removed from its --attention-backend choices in v0.5.20, so the
// default image rejects them while an older pinned image still runs them
// (#2213). The value maps to a replacement worth naming in the condition.
var sglangRemovedAttentionBackends = map[string]string{
	"cutlass_mla":           "trtllm_mla, cutedsl_mla or flashmla on Blackwell",
	"dual_chunk_flash_attn": "leave it unset to let SGLang choose",
}

// sglangRelease is the release both default images pin. The tool-call parser
// message names it from here so the release claim cannot drift from the
// constant, and the parser fixture is named after it, so a bump that forgets to
// regenerate the fixture fails to open its oracle instead of shipping a stale
// claim (#2219).
const sglangRelease = "v0.5.22"

// sglangUnsupportedToolCallParsers are toolCallParser values no release checked
// registers, so --tool-call-parser exits at argument parsing with an
// invalid-choice error. They stay in the enum so objects written against
// earlier LLMKube releases remain valid, and SGLangSpecValid reports them. The
// value is the advice the message gives. The captures behind the check are
// testdata/sglang-tool-call-parsers-v0.5.15.post1.txt and
// testdata/sglang-tool-call-parsers-v0.5.22.txt; the values are also absent from
// v0.4.9, v0.5.5.post1, v0.5.12, v0.5.19 and v0.5.20, checked by hand.
var sglangUnsupportedToolCallParsers = map[string]string{
	"qwen3":       "Use qwen25 for Qwen3 models, or qwen3_coder for Qwen3-Coder and for Qwen3.5 or newer",
	"functionary": "Leave it unset, or name the tool-call parser SGLang registers for your model family",
}

// sglangCUDAImage pins the newest stable SGLang CUDA image (verified
// 2026-10-24 via github.com/sgl-project/sglang/releases and Docker Hub,
// published for amd64 and arm64). It is a CUDA 13 build, so it needs the
// R580 driver branch or newer, the same floor as the vLLM default. SGLang
// stopped publishing -cu129 images at v0.5.20, and R570 reached end of life
// in February 2026; nodes still on R570 can pin
// lmsysorg/sglang:v0.5.19-cu129 via runtimeImages.sglang or spec.image
// (#2213). Blackwell is a first-class SGLang target (SM100 CuteDSL kernels
// since v0.5.14, Blackwell MLA backend since v0.5.12).
const sglangCUDAImage = "lmsysorg/sglang:" + sglangRelease + "-cu130"

// sglangROCmImage is the AMD ROCm variant. Selected by resolveRuntimeImage
// when model.Spec.Hardware.GPU.Vendor == "amd".
const sglangROCmImage = "lmsysorg/sglang:" + sglangRelease + "-rocm720-mi30x"

// SGLangBackend generates container configuration for the SGLang inference
// server (https://github.com/sgl-project/sglang).
type SGLangBackend struct{}

func (b *SGLangBackend) ContainerName() string { return "sglang" }
func (b *SGLangBackend) DefaultImage() string  { return sglangCUDAImage }
func (b *SGLangBackend) DefaultPort() int32    { return 30000 }

// SupportedArchitectures reports the architectures the operator-chosen SGLang
// image supports. The default CUDA/ROCm images are amd64-only, but the operator
// cannot verify a single arch list across every variant it may pick, so it
// declines to guess (#1479).
func (b *SGLangBackend) SupportedArchitectures() []string { return nil }
func (b *SGLangBackend) NeedsModelInit() bool             { return true }
func (b *SGLangBackend) DefaultHPAMetric() string         { return "sglang:num_running_reqs" }

// NeedsSharedMemory reports whether SGLang runs several worker processes on the
// node: BuildArgs defaults --tp to the GPU count, and an explicit tensor,
// expert or data-parallel size above 1 does the same (#2099).
func (b *SGLangBackend) NeedsSharedMemory(isvc *inferencev1alpha1.InferenceService, gpuCount int32) bool {
	if gpuCount > 1 {
		return true
	}
	cfg := isvc.Spec.SGLangConfig
	return cfg != nil && anyAboveOne(cfg.TensorParallelSize, cfg.ExpertParallelSize, cfg.DataParallelSize)
}

// RuntimeCacheDirs returns SGLang's compile and autotune caches (#2117).
// SGLANG_CACHE_DIR is the parent SGLang routes every third-party cache into
// (Triton, TorchInductor, TileLang, FlashInfer, CUDA) unless that cache's own
// variable is set, so one variable covers them all.
//
// Mirrors VLLMBackend.RuntimeCacheDirs; the lists are the contract, not shared
// code.
func (b *SGLangBackend) RuntimeCacheDirs() []RuntimeCacheDir {
	return []RuntimeCacheDir{
		{Env: "SGLANG_CACHE_DIR", Value: "", Dir: "", Default: "/root/.cache/sglang"},
	}
}

// BuildArgs generates the SGLang server CLI arguments. Order:
//  1. --model-path, --host, --port (always)
//  2. --served-model-name (auto-derived from ModelRef / model name)
//  3. typed SGLangConfig flags in declaration order
//  4. --tp auto-derived from gpuCount when unset
//  5. --is-embedding when spec.mode == "embedding" (skip if user set it)
//  6. spec.extraArgs last (user wins on collision)
//
// GPU-only flags (memFractionStatic) log a warning when set on a CPU model.
func (b *SGLangBackend) BuildArgs(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelPath string, _ string, port int32) []string {
	source := modelPath
	if source == "" {
		// Serve path: hand SGLang the bare "org/name[@rev]" repo id, not a
		// resolve URL (which it rejects). See hfServeArg.
		source = hfServeArg(model.Spec.Source)
	}
	args := []string{
		"--model-path", source,
		"--port", fmt.Sprintf("%d", port),
		// SGLang requires --enable-metrics to expose /metrics (unlike vLLM which
		// always exposes it, and llama.cpp which always passes --metrics). Without
		// this flag, PodMonitor scrapes fail and HPA cannot read the custom metric.
		"--enable-metrics",
	}

	// BindAddress: SGLang defaults to "" (every interface, both address
	// families), not the "::" the other runtimes use (#972/#973). SGLang's
	// server binds "::" IPv6-only, because asyncio sets IPV6_V6ONLY on an IPv6
	// listener, so an IPv4 pod IP has no listener and every kubelet probe is
	// refused (#2027). With an empty host asyncio binds 0.0.0.0 and :: on
	// separate sockets. Skip if user already set --host in extraArgs
	// (extraArgs wins).
	if !hasMatchingExtraArg(isvc.Spec.ExtraArgs, "host") {
		bindAddr := ""
		if isvc.Spec.BindAddress != "" {
			bindAddr = isvc.Spec.BindAddress
		}
		args = append(args, "--host", bindAddr)
	}

	// --served-model-name: skip if user already set it in extraArgs.
	if servedName := servedModelName(isvc, model); servedName != "" &&
		!hasMatchingExtraArg(isvc.Spec.ExtraArgs, "served-model-name") {
		args = append(args, "--served-model-name", servedName)
	}

	cfg := isvc.Spec.SGLangConfig
	gpuCount := resolveGPUCount(isvc, model)
	if cfg != nil {
		args = sglangAppendTensorParallelSize(args, cfg.TensorParallelSize)
		args = sglangAppendExpertParallelSize(args, cfg.ExpertParallelSize)
		args = sglangAppendDataParallelSize(args, cfg.DataParallelSize)
		args = sglangAppendContextLength(args, cfg.ContextLength)
		// hasGPUPresent, not gpuCount: a DRA-only model (resourceClaims, no
		// count) has a GPU and needs the flag (#2059).
		if cfg.MemFractionStatic != nil && !hasGPUPresent(isvc, model) {
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
		args = sglangAppendEnablePrefixCaching(args, cfg.EnablePrefixCaching, isvc.Spec.ExtraArgs)
		args = sglangAppendToolCallParser(args, cfg.ToolCallParser)
		args = sglangAppendReasoningParser(args, cfg.ReasoningParser)
		args = sglangAppendChatTemplate(args, cfg.ChatTemplate)
		args = sglangAppendSpeculative(args, cfg.Speculative)
		args = sglangAppendLoraModulesUnified(args, cfg.LoraAdapters, cfg.LoraModules)
		args = sglangAppendMaxLoraRank(args, cfg.MaxLoraRank)
		args = sglangAppendLoraTargetModules(args, cfg.LoraTargetModules)
		args = sglangAppendLogLevel(args, cfg.LogLevel)
		args = sglangAppendTrustRemoteCode(args, cfg.TrustRemoteCode)
		args = sglangAppendSkipTokenizerInit(args, cfg.SkipTokenizerInit)
	}

	// Auto-derive --tp when user didn't set it.
	if gpuCount > 1 && (cfg == nil || cfg.TensorParallelSize == nil) {
		args = append(args, "--tp", fmt.Sprintf("%d", gpuCount))
	}

	// Mode handling: --is-embedding (skip if user already set it).
	if resolveServingMode(isvc) == servingModeEmbedding && !hasMatchingExtraArg(isvc.Spec.ExtraArgs, "is-embedding") {
		args = append(args, "--is-embedding")
	}

	// The launcher writes the Secret-backed env var to a private config file;
	// expanding it in --api-key would expose it in /proc/<pid>/cmdline.
	if endpointAPIKeyRef(isvc) != nil {
		args = append(args, "--config", sglangAPIKeyConfigPath)
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
	// First: the only case here where the pod cannot start at all. The CRD
	// enum still admits flash_attn so objects stored before the enum was
	// corrected stay valid, but no SGLang release has had that choice.
	if cfg.AttentionBackend == sglangInvalidAttentionBackend {
		return "AttentionBackendUnsupported",
			"spec.sglangConfig.attentionBackend flash_attn is not an SGLang attention backend; SGLang exits at startup with an invalid-choice error. Use fa3 on Hopper, trtllm_mha or flashinfer on Blackwell, or leave it unset to let SGLang choose"
	}
	// Informational like the rest of this condition: the flag still renders,
	// because a service pinned to an image at v0.5.19 or older runs it fine.
	if instead, removed := sglangRemovedAttentionBackends[cfg.AttentionBackend]; removed {
		return "AttentionBackendRemoved",
			fmt.Sprintf("spec.sglangConfig.attentionBackend %s was removed in SGLang v0.5.20; the default image exits at startup with an invalid-choice error. It still runs on an image at v0.5.19 or older, such as lmsysorg/sglang:v0.5.19-cu129 set in spec.image; otherwise use %s",
				cfg.AttentionBackend, instead)
	}
	// The ordering invariant: a value that keeps the pod from starting outranks
	// one that only skips flags, so this sits after the attention-backend
	// checks and before the speculative ones.
	if advice, unsupported := sglangUnsupportedToolCallParsers[cfg.ToolCallParser]; unsupported {
		return "ToolCallParserUnsupported",
			fmt.Sprintf("spec.sglangConfig.toolCallParser %s is not a parser SGLang %s registers, so SGLang exits at startup with an invalid-choice error. %s",
				cfg.ToolCallParser, sglangRelease, advice)
	}
	if cfg.Speculative != nil && cfg.Speculative.Enabled != nil && *cfg.Speculative.Enabled {
		if cfg.Speculative.Algorithm == "" || cfg.Speculative.DraftModelPath == "" {
			return "SpeculativeMissingConfig",
				"spec.sglangConfig.speculative.enabled is true but algorithm/draftModelPath is empty; speculative decoding flags will be skipped"
		}
	}
	if cfg.Speculative != nil && (cfg.Speculative.Enabled == nil || !*cfg.Speculative.Enabled) {
		if cfg.Speculative.AcceptThresholdSingle != nil || cfg.Speculative.AcceptThresholdAcc != nil {
			return "SpeculativeAcceptThresholdUnused",
				"spec.sglangConfig.speculative.acceptThreshold* requires speculative.enabled=true; thresholds will be ignored"
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

// IdleProbe returns a probe closure that checks SGLang /metrics for
// `sglang:num_running_reqs` gauge sum. Idle when sum == 0. Absent metric
// returns (false, nil) — fail-closed, treats unknown as busy.
func (b *SGLangBackend) IdleProbe(_ *inferencev1alpha1.InferenceService, client *http.Client) func(ctx context.Context, baseURL string) (bool, error) {
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

		sum, found := parsePrometheusGaugeSum(string(body), "sglang:num_running_reqs")
		if !found {
			return false, nil
		}
		return sum == 0, nil
	}
}

// BuildEnv returns HF_TOKEN from SGLangConfig.HFTokenSecretRef when set, plus
// the endpoint API key under sglangAPIKeyEnv (SGLang reads HF_TOKEN from the
// environment to authenticate gated-model downloads from HuggingFace Hub; the
// API key has no SGLang-native env var, so the launcher writes it to a config
// file before replacing itself with SGLang).
func (b *SGLangBackend) BuildEnv(isvc *inferencev1alpha1.InferenceService) []corev1.EnvVar {
	var env []corev1.EnvVar
	cfg := isvc.Spec.SGLangConfig
	if cfg != nil && cfg.HFTokenSecretRef != nil {
		env = append(env, corev1.EnvVar{
			Name:      "HF_TOKEN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: cfg.HFTokenSecretRef},
		})
	}
	if ref := endpointAPIKeyRef(isvc); ref != nil {
		env = append(env, apiKeyEnvVar(sglangAPIKeyEnv, ref))
	}
	return env
}
