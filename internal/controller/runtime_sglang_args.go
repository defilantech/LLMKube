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
	"fmt"
	"strconv"
	"strings"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Argument builders for the sglang runtime. Each helper takes the current
// args slice plus the relevant CRD field and returns the appended slice (or
// the unchanged slice when the field is unset or not applicable). Prefixed
// with sglangAppend to avoid name collisions with vLLM helpers in the same
// package. Mirrors runtime_vllm_args.go.

func sglangAppendTensorParallelSize(args []string, size *int32) []string {
	if size != nil && *size >= 1 {
		return append(args, "--tp", fmt.Sprintf("%d", *size))
	}
	return args
}

func sglangAppendExpertParallelSize(args []string, size *int32) []string {
	if size != nil && *size >= 1 {
		return append(args, "--ep", fmt.Sprintf("%d", *size))
	}
	return args
}

func sglangAppendDataParallelSize(args []string, size *int32) []string {
	if size != nil && *size >= 1 {
		return append(args, "--dp", fmt.Sprintf("%d", *size))
	}
	return args
}

func sglangAppendContextLength(args []string, ctx *int32) []string {
	if ctx != nil {
		return append(args, "--context-length", fmt.Sprintf("%d", *ctx))
	}
	return args
}

// sglangAppendMemFractionStatic: GPU-only. Caller logs a warning when set on a
// CPU model; this helper just emits the flag.
func sglangAppendMemFractionStatic(args []string, frac *float64) []string {
	if frac != nil {
		return append(args, "--mem-fraction-static", strconv.FormatFloat(*frac, 'f', -1, 64))
	}
	return args
}

func sglangAppendChunkedPrefillSize(args []string, size *int32) []string {
	if size != nil {
		return append(args, "--chunked-prefill-size", fmt.Sprintf("%d", *size))
	}
	return args
}

func sglangAppendMaxRunningRequests(args []string, max *int32) []string {
	if max != nil {
		return append(args, "--max-running-requests", fmt.Sprintf("%d", *max))
	}
	return args
}

func sglangAppendQuantization(args []string, q string) []string {
	if q != "" {
		return append(args, "--quantization", q)
	}
	return args
}

// sglangResolveKVCacheDtype: custom wins; "" / nil / "auto" → empty (don't emit).
func sglangResolveKVCacheDtype(custom string, standard *string) string {
	if custom != "" {
		return custom
	}
	if standard == nil {
		return ""
	}
	return *standard
}

func sglangAppendKVCacheDtype(args []string, kvCacheDtype *string, custom string) []string {
	if resolved := sglangResolveKVCacheDtype(custom, kvCacheDtype); resolved != "" && resolved != "auto" {
		return append(args, "--kv-cache-dtype", resolved)
	}
	return args
}

func sglangAppendAttentionBackend(args []string, backend string) []string {
	if backend != "" {
		return append(args, "--attention-backend", backend)
	}
	return args
}

// sglangAppendEnablePrefixCaching: only emit when user explicitly opted in (true).
// SGLang's own default handles nil/false.
func sglangAppendEnablePrefixCaching(args []string, enabled *bool) []string {
	if enabled != nil && *enabled {
		return append(args, "--enable-prefix-caching")
	}
	return args
}

func sglangAppendToolCallParser(args []string, parser string) []string {
	if parser != "" {
		return append(args, "--tool-call-parser", parser)
	}
	return args
}

func sglangAppendReasoningParser(args []string, parser string) []string {
	if parser != "" {
		return append(args, "--reasoning-parser", parser)
	}
	return args
}

func sglangAppendChatTemplate(args []string, tmpl string) []string {
	if tmpl != "" {
		return append(args, "--chat-template", tmpl)
	}
	return args
}

// sglangAppendSpeculative: enabled+algorithm+draft-model required. Silent-skip with
// a log line when misconfigured; ValidateSGLangConfig (Task 7) also surfaces
// a status condition.
func sglangAppendSpeculative(args []string, cfg *inferencev1alpha1.SGLangSpeculativeConfig) []string {
	if cfg == nil || cfg.Enabled == nil || !*cfg.Enabled {
		return args
	}
	if cfg.Algorithm == "" || cfg.DraftModelPath == "" {
		sglangLog.Info(
			"speculative decoding enabled but algorithm/draft-model-path empty; skipping speculative flags",
		)
		return args
	}
	args = append(args, "--speculative-algorithm", cfg.Algorithm)
	args = append(args, "--speculative-draft-model-path", cfg.DraftModelPath)
	if cfg.NumSteps != nil {
		args = append(args, "--speculative-num-steps", fmt.Sprintf("%d", *cfg.NumSteps))
	}
	if cfg.EagleTopK != nil {
		args = append(args, "--speculative-eagle-topk", fmt.Sprintf("%d", *cfg.EagleTopK))
	}
	if cfg.NumDraftTokens != nil {
		args = append(args, "--speculative-num-draft-tokens", fmt.Sprintf("%d", *cfg.NumDraftTokens))
	}
	return args
}

func sglangAppendLoraModules(args []string, modules []string) []string {
	if len(modules) == 0 {
		return args
	}
	// SGLang's --lora-modules accepts a comma-separated list of
	// <name>=<path> or JSON entries. Join the CRD slice into a single string.
	return append(args, "--lora-modules", strings.Join(modules, ","))
}

func sglangAppendMaxLoraRank(args []string, rank *int32) []string {
	if rank != nil {
		return append(args, "--max-lora-rank", fmt.Sprintf("%d", *rank))
	}
	return args
}

// SGLang's --lora-target-modules accepts a comma-separated list of module
// names. Join the CRD slice into a single string with commas.
func sglangAppendLoraTargetModules(args []string, modules []string) []string {
	if len(modules) == 0 {
		return args
	}
	return append(args, "--lora-target-modules", strings.Join(modules, ","))
}
