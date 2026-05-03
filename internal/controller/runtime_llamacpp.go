package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// llamaCppLog is a package-level logger used for construction-time warnings from
// BuildArgs.
var llamaCppLog = logf.Log.WithName("runtime.llamacpp")

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
		"--host", "0.0.0.0",
		"--port", fmt.Sprintf("%d", port),
	}

	gpuCount := resolveGPUCount(isvc, model)

	if gpuCount > 0 {
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
	args, err = appendParallelSlotsArgs(args, isvc.Spec.ParallelSlots, isvc.Spec.ExtraArgs)
	if err != nil {
		llamaCppLog.Info(
			err.Error(),
			"inferenceService", isvc.Name,
			"namespace", isvc.Namespace,
		)
	}
	args = appendFlashAttentionArgs(args, isvc.Spec.FlashAttention, gpuCount)
	args = appendJinjaArgs(args, isvc.Spec.Jinja)
	args = appendCacheTypeArgs(args, resolveCacheType(isvc.Spec.CacheTypeCustomK, isvc.Spec.CacheTypeK), resolveCacheType(isvc.Spec.CacheTypeCustomV, isvc.Spec.CacheTypeV))
	args = appendMoeCPUOffloadArgs(args, isvc.Spec.MoeCPUOffload)
	args = appendMoeCPULayersArgs(args, isvc.Spec.MoeCPULayers)
	args = appendNoKvOffloadArgs(args, isvc.Spec.NoKvOffload)
	args = appendTensorOverrideArgs(args, isvc.Spec.TensorOverrides)
	args = appendBatchSizeArgs(args, isvc.Spec.BatchSize)
	args = appendUBatchSizeArgs(args, isvc.Spec.UBatchSize)
	args = appendNoWarmupArgs(args, isvc.Spec.NoWarmup)
	args = appendReasoningBudgetArgs(args, isvc.Spec.ReasoningBudget, isvc.Spec.ReasoningBudgetMessage)
	args = appendMetadataOverrideArgs(args, isvc.Spec.MetadataOverrides)
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

// resolveGPUCount determines the GPU count from Model spec or InferenceService spec.
func resolveGPUCount(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) int32 {
	if model.Spec.Hardware != nil && model.Spec.Hardware.GPU != nil && model.Spec.Hardware.GPU.Count > 0 {
		return model.Spec.Hardware.GPU.Count
	}
	if isvc.Spec.Resources != nil && isvc.Spec.Resources.GPU > 0 {
		return isvc.Spec.Resources.GPU
	}
	return 0
}
