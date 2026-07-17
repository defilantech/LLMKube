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
const llamaCppROCmImage = "ghcr.io/defilantech/llmkube-llama-rocm@sha256:8638b6824b6992f31e2bb46e222e2863def26ae3197ebe321739825acb001675"

// LlamaCppBackend generates container configuration for the llama.cpp inference server.
type LlamaCppBackend struct{}

func (b *LlamaCppBackend) ContainerName() string {
	return "llama-server"
}

func (b *LlamaCppBackend) DefaultImage() string {
	return "ghcr.io/ggml-org/llama.cpp:server"
}

func (b *LlamaCppBackend) DefaultPort() int32 {
	return 8080
}

func (b *LlamaCppBackend) NeedsModelInit() bool     { return true }
func (b *LlamaCppBackend) DefaultHPAMetric() string { return "llamacpp:requests_processing" }

func (b *LlamaCppBackend) BuildArgs(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelPath string, port int32) []string {
	args := []string{
		"--model", modelPath,
		// Bind the dual-stack wildcard so pods are reachable on IPv6-only
		// clusters (#972). With the default net.ipv6.bindv6only=0, :: also
		// accepts IPv4, so IPv4-only and dual-stack clusters keep working.
		// On IPv6-disabled nodes, override via extraArgs ("--host", "0.0.0.0");
		// llama.cpp keeps the last occurrence of a repeated flag.
		"--host", "::",
		"--port", fmt.Sprintf("%d", port),
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
	args = appendSpeculativeDecodingArgs(args, isvc.Spec.SpeculativeDecoding)
	args = appendReasoningBudgetArgs(args, isvc.Spec.ReasoningBudget, isvc.Spec.ReasoningBudgetMessage)
	if model != nil && model.Spec.Mmproj != "" && modelPath != "" {
		if plan, err := ResolveFileSet(model.Spec.Files, model.Spec.Mmproj, nil); err == nil && plan != nil && plan.Primary != "" {
			mmprojDir := path.Dir(modelPath)
			suffix := "/" + plan.Primary
			if strings.HasSuffix(modelPath, suffix) {
				mmprojDir = modelPath[:len(modelPath)-len(suffix)]
			}
			args = appendMmprojArgs(args, stagedCachePath(mmprojDir, model.Spec.Mmproj), isvc.Spec.ExtraArgs)
		}
	}
	args = appendMetadataOverrideArgs(args, isvc.Spec.MetadataOverrides)
	args = appendModeArgs(args, isvc.Spec.Mode, isvc.Spec.ExtraArgs)
	if len(isvc.Spec.ExtraArgs) > 0 {
		args = append(args, isvc.Spec.ExtraArgs...)
	}

	// Enable Prometheus metrics endpoint on llama.cpp
	args = append(args, "--metrics")

	return args
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
