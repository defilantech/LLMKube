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

package controller

import (
	"errors"
	"fmt"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Argument builders for the llama.cpp runtime. Each helper takes the current
// args slice plus the relevant CRD field and returns the appended slice (or
// the unchanged slice when the field is unset or not applicable). Kept as
// free functions so they are trivially testable in isolation and can be
// composed in any order from the deployment builder.

// needsOffloadMemoryWarning returns true when llama.cpp hybrid-offload flags
// (MoeCPUOffload, NoKvOffload) are set but no host RAM budget has been given
// via resources.memory / resources.hostMemory. In that case the controller
// emits a warning event so users are not surprised by pods getting OOM-killed
// under offloaded weights.
func needsOffloadMemoryWarning(isvc *inferencev1alpha1.InferenceService) bool {
	needsRAM := (isvc.Spec.MoeCPUOffload != nil && *isvc.Spec.MoeCPUOffload) ||
		(isvc.Spec.NoKvOffload != nil && *isvc.Spec.NoKvOffload)
	memorySet := isvc.Spec.Resources != nil && (isvc.Spec.Resources.Memory != "" || isvc.Spec.Resources.HostMemory != "")
	return needsRAM && !memorySet
}

func appendContextSizeArgs(args []string, contextSize *int32) []string {
	if contextSize != nil && *contextSize > 0 {
		return append(args, "--ctx-size", fmt.Sprintf("%d", *contextSize))
	}
	return args
}

func appendParallelSlotsArgs(args []string, parallelSlots *int32, extraArgs []string) ([]string, error) {
	// NOTE(#339): extra args has precedence.
	if parallelSlots != nil && *parallelSlots >= 1 {
		if hasMatchingExtraArg(extraArgs, "parallel") {
			return args, errors.New("spec.parallelSlots is enabled but `--parallel` is already defined in spec.ExtraArgs, skipping")
		}
		return append(args, "--parallel", fmt.Sprintf("%d", *parallelSlots)), nil
	}
	return args, nil
}

func appendFlashAttentionArgs(args []string, flashAttention *bool, gpuCount int32) []string {
	if gpuCount > 0 && flashAttention != nil && *flashAttention {
		return append(args, "--flash-attn", "on")
	}
	return args
}

func appendJinjaArgs(args []string, jinja *bool) []string {
	if jinja != nil && *jinja {
		return append(args, "--jinja")
	}
	return args
}

func appendCacheTypeArgs(args []string, cacheTypeK string, cacheTypeV string) []string {
	if cacheTypeK != "" {
		args = append(args, "--cache-type-k", cacheTypeK)
	}
	if cacheTypeV != "" {
		args = append(args, "--cache-type-v", cacheTypeV)
	}
	return args
}

// resolveCacheType returns the custom cache type when set, otherwise the
// enum-validated standard value. Lets users opt into fork-specific cache types
// (TurboQuant turbo3/turbo4, etc.) without expanding the CRD enum, while
// keeping the standard fields discoverable for the common case.
func resolveCacheType(custom, standard string) string {
	if custom != "" {
		return custom
	}
	return standard
}

func appendMoeCPUOffloadArgs(args []string, moeCPUOffload *bool) []string {
	if moeCPUOffload != nil && *moeCPUOffload {
		return append(args, "--cpu-moe")
	}
	return args
}

func appendMoeCPULayersArgs(args []string, moeCPULayers *int32) []string {
	if moeCPULayers != nil && *moeCPULayers > 0 {
		return append(args, "--n-cpu-moe", fmt.Sprintf("%d", *moeCPULayers))
	}
	return args
}

func appendNoKvOffloadArgs(args []string, noKvOffload *bool) []string {
	if noKvOffload != nil && *noKvOffload {
		return append(args, "--no-kv-offload")
	}
	return args
}

func appendTensorOverrideArgs(args []string, overrides []string) []string {
	for _, override := range overrides {
		args = append(args, "--override-tensor", override)
	}
	return args
}

func appendMetadataOverrideArgs(args []string, overrides []string) []string {
	for _, override := range overrides {
		args = append(args, "--override-kv", override)
	}
	return args
}

func appendBatchSizeArgs(args []string, batchSize *int32) []string {
	if batchSize != nil && *batchSize > 0 {
		return append(args, "--batch-size", fmt.Sprintf("%d", *batchSize))
	}
	return args
}

func appendUBatchSizeArgs(args []string, uBatchSize *int32) []string {
	if uBatchSize != nil && *uBatchSize > 0 {
		return append(args, "--ubatch-size", fmt.Sprintf("%d", *uBatchSize))
	}
	return args
}

func appendNoWarmupArgs(args []string, noWarmup *bool) []string {
	if noWarmup != nil && *noWarmup {
		return append(args, "--no-warmup")
	}
	return args
}

func appendReasoningBudgetArgs(args []string, budget *int32, message string) []string {
	if budget == nil {
		return args
	}
	args = append(args, "--reasoning-budget", fmt.Sprintf("%d", *budget))
	if message != "" {
		args = append(args, "--reasoning-budget-message", message)
	}
	return args
}
