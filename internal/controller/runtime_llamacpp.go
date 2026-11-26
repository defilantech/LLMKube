package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// llamaCppLog is a package-level logger used for construction-time warnings from
// BuildArgs.
var llamaCppLog = logf.Log.WithName("runtime.llamacpp")

// llamaCppVulkanImage is LLMKube's hardware-validated Vulkan llama.cpp server
// image, built and promoted by defilantech/llmkube-runtimes. Pinned by digest
// for reproducibility; this digest is the :stable / :b9663-llmkube1 image. Bump
// it via PR when the promoter publishes a new :stable digest.
const llamaCppVulkanImage = "ghcr.io/defilantech/llmkube-llama-vulkan@sha256:cbab8af682ecac9b5c865d85219d808dc356814326c70346e05b8b20b333e295"

// llamaCppROCmImage is LLMKube's hardware-validated ROCm/HIP llama.cpp server
// image for AMD nodes (gfx1151-targeted, rocWMMA FlashAttention, hipBLASLt).
// Digest-pinned like the Vulkan image; bumped via reviewed PR after the
// promoter smokes the candidate on real hardware. See #701.
const llamaCppROCmImage = "ghcr.io/defilantech/llmkube-llama-rocm@sha256:8da16041a18b4f03f0be4de5e064be97ba8937149091d6693ee09711da362849"

// llamaCppBuild is the upstream llama.cpp build every default llama.cpp image
// comes from: the CPU default, the NVIDIA image and router mode. One build
// means a service accepts the same flags whether or not its Model declares a
// GPU, and an immutable tag means two rollouts of the same service run the
// same server (#2214). Upstream does not publish images for every build, so
// a bump picks a build that has both server-<build> and server-cuda-<build>
// on GHCR. b11854 has both (checked 2026-10-24), and it served chat on a GB10
// on CPU and CUDA.
const llamaCppBuild = "b11854"

// llamaCppCPUImage is the CPU-only default: correct for CPU serving and for
// Models with no GPU section.
const llamaCppCPUImage = "ghcr.io/ggml-org/llama.cpp:server-" + llamaCppBuild

// llamaCppCUDAImage is the upstream CUDA llama.cpp server image the operator
// substitutes for the CPU-only default when the Model declares an NVIDIA GPU
// (see resolveRuntimeImage). Same build as llamaCppCPUImage, CUDA 12.8.1 base
// (llama.cpp's .devops/cuda.Dockerfile at b11854). CAVEAT for
// Blackwell: upstream's prebuilt CUDA images ship no native sm_100 codegen
// (ggml's CMAKE_CUDA_ARCHITECTURES covers 50..90-virtual plus consumer
// 120a/121a only), so a B200 runs via PTX JIT from the 90-virtual target:
// functional, with first-run JIT cost and no sm_100-tuned kernels. Fleets
// serious about llama.cpp-on-B200 should build with CUDA_DOCKER_ARCH=100a-real
// and point runtimeImages.llamacpp (or spec.image) at it.
const llamaCppCUDAImage = "ghcr.io/ggml-org/llama.cpp:server-cuda-" + llamaCppBuild

// LlamaCppBackend generates container configuration for the llama.cpp inference server.
type LlamaCppBackend struct{}

func (b *LlamaCppBackend) ContainerName() string {
	return "llama-server"
}

// DefaultImage is the CPU-only image. GPU Models never see it: AMD diverts to
// the hardware-validated Vulkan/ROCm digests and NVIDIA diverts to
// llamaCppCUDAImage (resolveRuntimeImage).
func (b *LlamaCppBackend) DefaultImage() string {
	return llamaCppCPUImage
}

func (b *LlamaCppBackend) DefaultPort() int32 {
	return 8080
}

// SupportedArchitectures reports the architectures the operator-chosen llama.cpp
// image supports. The default CPU-only :server tag is published for both amd64
// and arm64, so no constraint is applied (nil). The GPU variants selected by
// resolveRuntimeImage are amd64-only, but the operator cannot verify a single
// arch list across every variant it may pick, so it declines to guess (#1479).
func (b *LlamaCppBackend) SupportedArchitectures() []string { return nil }

func (b *LlamaCppBackend) NeedsModelInit() bool     { return true }
func (b *LlamaCppBackend) DefaultHPAMetric() string { return "llamacpp:requests_processing" }

func (b *LlamaCppBackend) BuildArgs(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelPath string, draftPath string, port int32) []string {
	args := []string{
		"--model", modelPath,
		"--port", fmt.Sprintf("%d", port),
	}

	// BindAddress: default "::" (dual-stack wildcard, #972/#973). Skip if
	// user already set --host in extraArgs (extraArgs wins).
	if !hasMatchingExtraArg(isvc.Spec.ExtraArgs, "host") {
		bindAddr := "::"
		if isvc.Spec.BindAddress != "" {
			bindAddr = isvc.Spec.BindAddress
		}
		args = append(args, "--host", bindAddr)
	}

	// --alias: report a clean model ID from /v1/models instead of the on-disk
	// path, matching the SGLang backend's --served-model-name (#1894). Without
	// it llama.cpp reports the namespaced file path, which Open WebUI and other
	// OpenAI clients show verbatim, and which changes when a service moves
	// namespace. Skip if the user already set --alias in extraArgs.
	if servedName := servedModelName(isvc, model); servedName != "" && !hasMatchingExtraArg(isvc.Spec.ExtraArgs, "alias") {
		args = append(args, "--alias", servedName)
	}

	gpuCount := resolveGPUCount(isvc, model)

	if hasGPUPresent(isvc, model) {
		layers := int32(99)
		if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil && model.Spec.Hardware.GPU.Layers > 0 {
			layers = model.Spec.Hardware.GPU.Layers
		} else if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil && model.Spec.Hardware.GPU.Layers == -1 {
			layers = 99
		}
		args = append(args, "--n-gpu-layers", fmt.Sprintf("%d", layers))

		if gpuCount > 1 {
			var sharding *inferencev1alpha1.GPUShardingSpec
			if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil {
				sharding = model.Spec.Hardware.GPU.Sharding
			}
			splitMode := resolveSplitMode(sharding)
			args = append(args, "--split-mode", splitMode)

			// --tensor-split ratios only apply to layer/row modes, not none.
			if splitMode != splitModeNone {
				tensorSplit := calculateTensorSplit(gpuCount, sharding)
				args = append(args, "--tensor-split", tensorSplit)
			}
		}
	}

	var err error

	args = appendContextSizeArgs(args, isvc.Spec.ContextSize)
	args, err = appendRopeScalingArgs(args, isvc.Spec.RopeScaling, isvc.Spec.ExtraArgs)
	if err != nil {
		llamaCppLog.Info(
			err.Error(),
			"inferenceService", isvc.Name,
			"namespace", isvc.Namespace,
		)
	}
	args, err = appendParallelSlotsArgs(args, isvc.Spec.ParallelSlots, isvc.Spec.ExtraArgs)
	if err != nil {
		llamaCppLog.Info(
			err.Error(),
			"inferenceService", isvc.Name,
			"namespace", isvc.Namespace,
		)
	}
	args = appendFlashAttentionArgs(args, isvc.Spec.FlashAttention, hasGPUPresent(isvc, model))
	args = appendJinjaArgs(args, isvc.Spec.Jinja)
	args = appendCacheTypeArgs(args, resolveCacheType(isvc.Spec.CacheTypeCustomK, isvc.Spec.CacheTypeK), resolveCacheType(isvc.Spec.CacheTypeCustomV, isvc.Spec.CacheTypeV))
	args = appendMoeCPUOffloadArgs(args, isvc.Spec.MoeCPUOffload)
	args = appendMoeCPULayersArgs(args, isvc.Spec.MoeCPULayers)
	args = appendNoKvOffloadArgs(args, isvc.Spec.NoKvOffload)
	args = appendTensorOverrideArgs(args, isvc.Spec.TensorOverrides)
	args = appendBatchSizeArgs(args, isvc.Spec.BatchSize)
	args = appendUBatchSizeArgs(args, isvc.Spec.UBatchSize)
	args = appendNoWarmupArgs(args, isvc.Spec.NoWarmup)
	draftPath, mmprojPath := resolveStagedCompanions(isvc, model, modelPath, draftPath)

	args = appendSpeculativeDecodingArgs(args, isvc.Spec.SpeculativeDecoding, draftPath)
	args = appendReasoningBudgetArgs(args, isvc.Spec.ReasoningBudget, isvc.Spec.ReasoningBudgetMessage)
	args = appendMmprojArgs(args, mmprojPath, isvc.Spec.ExtraArgs)
	args = appendMetadataOverrideArgs(args, isvc.Spec.MetadataOverrides)
	args = appendModeArgs(args, isvc.Spec.Mode, isvc.Spec.ExtraArgs, embeddingPoolingDefault(model, isvc.Spec.MetadataOverrides))
	if len(isvc.Spec.ExtraArgs) > 0 {
		args = append(args, isvc.Spec.ExtraArgs...)
	}

	// Enable Prometheus metrics endpoint on llama.cpp, unless the user already
	// asked for it in ExtraArgs. Appending unconditionally emitted
	// "--metrics --metrics" (#1384); llama.cpp tolerates the repeat, but the
	// operator should not be delegating its own flag decisions to the
	// runtime's last-flag-wins semantics.
	if !hasMatchingExtraArg(isvc.Spec.ExtraArgs, "metrics") {
		args = append(args, "--metrics")
	}

	return args
}

// resolveStagedCompanions resolves the absolute in-container paths of the two
// companion files the operator stages alongside the primary model: the
// multimodal projector (Model.mmproj) and a file-based draft
// (spec.speculativeDecoding.draftModel, #1495). Both resolve against the
// model's cache base dir — the directory holding the primary file — so the
// manifest never names a content-hash path that is unknowable before the
// first reconcile.
//
// It returns the draft path to hand to appendSpeculativeDecodingArgs and the
// mmproj path to hand to appendMmprojArgs. The draft path is left as passed
// when a Model-CR draft (draftModelRef) already supplied one; a file-based
// draft only fills the gap when the base dir is resolvable. Each path is ""
// when its source is unset or the file set is invalid, in which case the
// corresponding append* helper emits nothing.
func resolveStagedCompanions(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelPath, draftPath string) (draftPathOut, mmprojPath string) {
	draftPathOut = draftPath
	if model == nil || modelPath == "" {
		return
	}

	baseDir := ""
	plan, err := modelStagingPlan(model)
	if err != nil {
		llamaCppLog.Info("Cannot resolve the staged file set; companion files are not passed", "model", model.Name, "error", err.Error())
	}
	if err == nil && plan != nil && plan.Primary != "" {
		baseDir = path.Dir(modelPath)
		if suffix := "/" + plan.Primary; strings.HasSuffix(modelPath, suffix) {
			baseDir = modelPath[:len(modelPath)-len(suffix)]
		}
	}
	if baseDir == "" {
		return
	}

	if model.Spec.Mmproj != "" {
		mmprojPath = stagedCachePath(baseDir, model.Spec.Mmproj)
	}
	if draftPathOut == "" {
		if sd := isvc.Spec.SpeculativeDecoding; sd != nil && sd.DraftModel != "" {
			draftPathOut = stagedCachePath(baseDir, sd.DraftModel)
		}
	}
	return
}

func (b *LlamaCppBackend) BuildProbes(port int32) (startup, liveness, readiness *corev1.Probe) {
	startup = &corev1.Probe{
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

	liveness = &corev1.Probe{
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

	readiness = &corev1.Probe{
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

// llamaCPUSlot represents a single slot from the llama.cpp /slots endpoint.
type llamaCPUSlot struct {
	ID           int  `json:"id"`
	IsProcessing bool `json:"is_processing"`
}

// IdleProbe returns a probe closure that checks llama.cpp /slots endpoint for
// idle status. All slots must report is_processing == false for the probe to
// return true. Lifted from the original checkServerIdle in drain_before_rollout.go.
func (b *LlamaCppBackend) IdleProbe(_ *inferencev1alpha1.InferenceService, client *http.Client) func(ctx context.Context, baseURL string) (bool, error) {
	return llamaCppSlotsIdleProbe(client)
}

// llamaCppSlotsIdleProbe returns an idle-probe closure that queries the llama.cpp
// /slots endpoint and reports true only when every slot has is_processing == false.
// Shared by the single-model (LlamaCppBackend) and router (LlamaCppRouterBackend)
// llama.cpp runtimes so the /slots logic lives in exactly one place.
func llamaCppSlotsIdleProbe(client *http.Client) func(ctx context.Context, baseURL string) (bool, error) {
	return func(ctx context.Context, baseURL string) (bool, error) {
		url := fmt.Sprintf("%s/slots", baseURL)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, fmt.Errorf("failed to create request: %w", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Errorf("failed to query /slots: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("/slots returned status %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, fmt.Errorf("failed to read /slots response: %w", err)
		}

		var slots []llamaCPUSlot
		if err := json.Unmarshal(body, &slots); err != nil {
			return false, fmt.Errorf("failed to parse /slots response: %w", err)
		}

		for _, slot := range slots {
			if slot.IsProcessing {
				return false, nil
			}
		}

		return true, nil
	}
}
