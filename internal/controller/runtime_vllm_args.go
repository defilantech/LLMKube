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

// Argument builders for the vllm runtime. Each helper takes the current
// args slice plus the relevant CRD field and returns the appended slice (or
// the unchanged slice when the field is unset or not applicable). Kept as
// free functions so they are trivially testable in isolation and can be
// composed in any order from the deployment builder.

func appendAttentionBackend(args []string, attentionBackend string) []string {
	if attentionBackend != "" {
		return append(args, "--attention-backend", attentionBackend)
	}
	return args
}

func appendDtype(args []string, dtype string) []string {
	if dtype != "" {
		return append(args, "--dtype", dtype)
	}
	return args
}

// appendEnableChunkedPrefill only emit when user explicitly opted in (true).
// vLLM's own default handles the nil/false case.
func appendEnableChunkedPrefill(args []string, enableChunkedPrefill *bool) []string {
	if enableChunkedPrefill != nil && *enableChunkedPrefill {
		return append(args, "--enable-chunked-prefill")
	}
	return args
}

func appendEnableExpertParallel(args []string, enableExpertParallel *bool) []string {
	if enableExpertParallel != nil && *enableExpertParallel {
		return append(args, "--enable-expert-parallel")
	}
	return args
}

// appendEnablePrefixCaching only emit when user explicitly opted in (true).
// vLLM's own default handles the nil/false case.
func appendEnablePrefixCaching(args []string, enablePrefixCaching *bool) []string {
	if enablePrefixCaching != nil && *enablePrefixCaching {
		return append(args, "--enable-prefix-caching")
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

// appendKVCacheDtype emit flag unless unset or explicitly "auto" (vLLM's default).
func appendKVCacheDtype(args []string, kvCacheDtype *string, kvCacheCustomDtype string) []string {
	if resolved := resolveKVCacheDtype(kvCacheCustomDtype, kvCacheDtype); resolved != "" && resolved != "auto" {
		args = append(args, "--kv-cache-dtype", resolved)
	}
	return args
}

func appendQuantization(args []string, quantization string) []string {
	if quantization != "" {
		return append(args, "--quantization", quantization)
	}
	return args
}

func appendMaxModelLen(args []string, maxModelLen *int32) []string {
	if maxModelLen != nil {
		return append(args, "--max-model-len", fmt.Sprintf("%d", *maxModelLen))
	}
	return args
}

func appendMaxNumBatchedTokens(args []string, maxNumBatchedTokens *int32) []string {
	if maxNumBatchedTokens != nil {
		return append(args, "--max-num-batched-tokens", fmt.Sprintf("%d", *maxNumBatchedTokens))
	}
	return args
}

func appendMaxNumSeqsArgs(args []string, parallelSlots *int32, extraArgs []string) ([]string, error) {
	// NOTE(#339): extra args has precedence.
	if parallelSlots != nil && *parallelSlots >= 1 {
		if hasMatchingExtraArg(extraArgs, "max-num-seqs") {
			return args, errors.New("spec.parallelSlots is enabled but `--max-num-seqs` is already defined in spec.ExtraArgs, skipping")
		}
		return append(args, "--max-num-seqs", fmt.Sprintf("%d", *parallelSlots)), nil
	}
	return args, nil
}

// appendSpeculativeModel require both Enabled=true and a non-empty
// draft Model ref. Silent-skip with a log line when misconfigured;
// the reconciler also sets a status condition via ValidateVLLMConfig.
func appendSpeculativeModel(args []string, speculativeCfg *inferencev1alpha1.SpeculativeConfig) ([]string, error) {
	if speculativeCfg == nil || speculativeCfg.Enabled == nil || !*speculativeCfg.Enabled {
		return args, nil
	}
	if speculativeCfg.Model == "" {
		return args, errors.New("speculative decoding enabled but spec.vllmConfig.speculative.model is empty; skipping speculative flags")
	}
	args = append(args, "--speculative-model", speculativeCfg.Model)
	if speculativeCfg.NumSpeculativeTokens != nil {
		args = append(args, "--num-speculative-tokens",
			fmt.Sprintf("%d", *speculativeCfg.NumSpeculativeTokens))
	}
	return args, nil
}

func appendTensorParallelSize(args []string, tensorParallelSize *int32) []string {
	if tensorParallelSize != nil && *tensorParallelSize > 1 {
		return append(args, "--tensor-parallel-size", fmt.Sprintf("%d", *tensorParallelSize))
	}
	return args
}
