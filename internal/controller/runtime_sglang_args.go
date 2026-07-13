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
	"encoding/json"
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
// a log line when misconfigured; ValidateSGLangConfig surfaces a status condition.
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
	if cfg.AcceptThresholdSingle != nil {
		args = append(args, "--speculative-accept-threshold-single",
			strconv.FormatFloat(*cfg.AcceptThresholdSingle, 'f', -1, 64))
	}
	if cfg.AcceptThresholdAcc != nil {
		args = append(args, "--speculative-accept-threshold-acc",
			strconv.FormatFloat(*cfg.AcceptThresholdAcc, 'f', -1, 64))
	}
	return args
}

// sglangParseLoraPair parses one legacy LoraModules []string entry into a
// (name, path) pair. Two forms are accepted for back-compat with operators
// running pre-#1060 CRs:
//
//   - `name=path` shorthand, e.g. `loraA=/loras/a`. This was the
//     historical user-friendly form on top of vLLM's --lora-modules.
//   - JSON object: `{"name":"loraA","path":"/loras/a"}` (the form
//     sglang_v0.5.15's --lora-paths natively accepts via LoRAPathAction).
//
// Strings that fail BOTH forms (e.g. `"not-json"`) are silently dropped:
// the prior controller passed them verbatim and SGLang would have
// crashed at startup. The drop is logged once per reconcile via the
// condition message below so the operator knows their config is being
// silently filtered. Empty name (regardless of source form) is also
// dropped — SGLang's `LoRAPathAction` validator rejects empty names.
//
// Returns ok=false when the entry is invalid; ok=true with name==""
// when the entry parsed but the resulting pair should be skipped
// (empty name). All callers should `continue` on both ok==false and
// on `name==""`.
func sglangParseLoraPair(s string) (name, path string, ok bool) {
	if s == "" {
		return "", "", false
	}
	// name=path shorthand.
	if eq := strings.IndexByte(s, '='); eq > 0 && !strings.ContainsAny(s[:eq], "{[ \"") {
		name = s[:eq]
		path = s[eq+1:]
		if name == "" {
			return "", "", true
		}
		return name, path, true
	}
	// JSON object form.
	var parsed struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(s), &parsed); err != nil || parsed.Name == "" {
		if parsed.Name == "" && err == nil {
			return "", "", true
		}
		return "", "", false
	}
	return parsed.Name, parsed.Path, true
}

// sglangBuildLoraModulePairs merges typed LoraAdapters with the legacy
// LoraModules []string. The typed list wins on name collision. Returns
// the merged name=path pairs in a STABLE order: typed adapters first in
// declared order, then any legacy entries that didn't collide, also in
// encountered order. Order matters — the prior implementation built the
// final slice by ranging over a Go map, which is randomized, and caused
// spurious Deployment rollouts on every reconcile (#1060 review).
func sglangBuildLoraModulePairs(adapters []inferencev1alpha1.SGLangLoRAAdapter, legacy []string) []string {
	if len(adapters) == 0 && len(legacy) == 0 {
		return nil
	}
	// typedSeen lets the legacy loop skip names already locked in.
	typedSeen := make(map[string]struct{}, len(adapters))
	type pair struct{ name, path string }
	// ordered pairs preserve input order.
	ordered := make([]pair, 0, len(adapters)+len(legacy))
	for _, a := range adapters {
		if a.Name == "" {
			continue
		}
		typedSeen[a.Name] = struct{}{}
		ordered = append(ordered, pair{a.Name, a.Path})
	}
	for _, raw := range legacy {
		name, path, ok := sglangParseLoraPair(raw)
		if !ok || name == "" {
			continue
		}
		if _, taken := typedSeen[name]; taken {
			continue // typed entry wins on collision
		}
		ordered = append(ordered, pair{name, path})
	}
	pairs := make([]string, 0, len(ordered))
	for _, p := range ordered {
		pairs = append(pairs, p.name+"="+p.path)
	}
	return pairs
}

func sglangAppendLoraModulesUnified(args []string, adapters []inferencev1alpha1.SGLangLoRAAdapter, legacy []string) []string {
	pairs := sglangBuildLoraModulePairs(adapters, legacy)
	if len(pairs) == 0 {
		return args
	}
	// SGLang v0.5.15 calls this flag `--lora-paths`, not vLLM's
	// `--lora-modules`. Naming drift was the load-bearing bug caught
	// in #1060 review; see server_args.py:lora_paths.
	return append(args, "--lora-paths", strings.Join(pairs, ","))
}

func sglangAppendModel(args []string, model string) []string {
	if model != "" {
		return append(args, "--model", model)
	}
	return args
}

func sglangAppendLogLevel(args []string, level string) []string {
	if level != "" {
		return append(args, "--log-level", level)
	}
	return args
}

// sglangAppendTrustRemoteCode: emit only when user opted in (true).
func sglangAppendTrustRemoteCode(args []string, enabled *bool) []string {
	if enabled != nil && *enabled {
		return append(args, "--trust-remote-code")
	}
	return args
}

// sglangAppendSkipTokenizerInit: emit only when user opted in (true).
func sglangAppendSkipTokenizerInit(args []string, enabled *bool) []string {
	if enabled != nil && *enabled {
		return append(args, "--skip-tokenizer-init")
	}
	return args
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
