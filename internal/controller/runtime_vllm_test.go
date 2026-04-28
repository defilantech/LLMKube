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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// containsArg reports whether args contains the given flag. When value is
// non-empty, it also requires the immediately following entry to equal value
// (i.e. `--flag value` as separate slice elements, which is how BuildArgs
// emits everything).
func containsArg(args []string, flag, value string) bool {
	for i, a := range args {
		if a != flag {
			continue
		}
		if value == "" {
			return true
		}
		if i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// ptrString, ptrBool, ptrInt32 are local helpers so tests read naturally.
func ptrString(s string) *string { return &s }
func ptrBool(b bool) *bool       { return &b }
func ptrInt32(i int32) *int32    { return &i }

// TestVLLMBuildArgs is the single table-driven test that covers every new
// agentic-coding flag and the "not emitted when unset/false" counterpart.
// Each row asserts a set of must-contain and must-not-contain flags on the
// generated arg list.
func TestVLLMBuildArgs(t *testing.T) {
	backend := &VLLMBackend{}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}
	const modelPath = "/models/test"
	const port = int32(8000)

	// contains is a slice of flag/value pairs ("" value means "flag must be
	// present as a bare toggle"). notContains is just a list of flags that
	// must NOT appear anywhere in args.
	type flagCheck struct {
		flag  string
		value string
	}

	cases := []struct {
		name        string
		cfg         *inferencev1alpha1.VLLMConfig
		contains    []flagCheck
		notContains []string
	}{
		{
			name:        "nil config emits only base flags",
			cfg:         nil,
			contains:    []flagCheck{{"--model", modelPath}, {"--host", "0.0.0.0"}, {"--port", "8000"}},
			notContains: []string{"--kv-cache-dtype", "--enable-prefix-caching", "--enable-chunked-prefill", "--max-num-batched-tokens", "--attention-backend", "--speculative-model", "--enable-expert-parallel"},
		},
		{
			name:        "empty config emits only base flags",
			cfg:         &inferencev1alpha1.VLLMConfig{},
			notContains: []string{"--kv-cache-dtype", "--enable-prefix-caching", "--enable-chunked-prefill", "--max-num-batched-tokens", "--speculative-model", "--enable-expert-parallel"},
		},
		{
			name:        "kvCacheDtype=auto does not emit flag (vLLM default)",
			cfg:         &inferencev1alpha1.VLLMConfig{KVCacheDtype: ptrString("auto")},
			notContains: []string{"--kv-cache-dtype"},
		},
		{
			name:     "kvCacheDtype=fp8_e5m2 emits flag",
			cfg:      &inferencev1alpha1.VLLMConfig{KVCacheDtype: ptrString("fp8_e5m2")},
			contains: []flagCheck{{"--kv-cache-dtype", "fp8_e5m2"}},
		},
		{
			name:     "kvCacheDtype=fp8_e4m3 emits flag",
			cfg:      &inferencev1alpha1.VLLMConfig{KVCacheDtype: ptrString("fp8_e4m3")},
			contains: []flagCheck{{"--kv-cache-dtype", "fp8_e4m3"}},
		},
		{
			name:     "kvCacheCustomDtype=turbo2 emits flag (vLLM v0.20+ TurboQuant 2-bit)",
			cfg:      &inferencev1alpha1.VLLMConfig{KVCacheCustomDtype: "turbo2"},
			contains: []flagCheck{{"--kv-cache-dtype", "turbo2"}},
		},
		{
			name: "kvCacheCustomDtype wins over standard kvCacheDtype when both set",
			cfg: &inferencev1alpha1.VLLMConfig{
				KVCacheDtype:       ptrString("fp8_e4m3"),
				KVCacheCustomDtype: "turbo2",
			},
			contains:    []flagCheck{{"--kv-cache-dtype", "turbo2"}},
			notContains: []string{"fp8_e4m3"},
		},
		{
			name: "kvCacheCustomDtype empty falls back to standard kvCacheDtype",
			cfg: &inferencev1alpha1.VLLMConfig{
				KVCacheDtype:       ptrString("fp8_e5m2"),
				KVCacheCustomDtype: "",
			},
			contains: []flagCheck{{"--kv-cache-dtype", "fp8_e5m2"}},
		},
		{
			name:     "enablePrefixCaching=true emits flag",
			cfg:      &inferencev1alpha1.VLLMConfig{EnablePrefixCaching: ptrBool(true)},
			contains: []flagCheck{{"--enable-prefix-caching", ""}},
		},
		{
			name:        "enablePrefixCaching=false does not emit flag (lets vLLM default)",
			cfg:         &inferencev1alpha1.VLLMConfig{EnablePrefixCaching: ptrBool(false)},
			notContains: []string{"--enable-prefix-caching"},
		},
		{
			name:     "enableChunkedPrefill=true emits flag",
			cfg:      &inferencev1alpha1.VLLMConfig{EnableChunkedPrefill: ptrBool(true)},
			contains: []flagCheck{{"--enable-chunked-prefill", ""}},
		},
		{
			name:        "enableChunkedPrefill=false does not emit flag",
			cfg:         &inferencev1alpha1.VLLMConfig{EnableChunkedPrefill: ptrBool(false)},
			notContains: []string{"--enable-chunked-prefill"},
		},
		{
			name:     "maxNumBatchedTokens set emits flag",
			cfg:      &inferencev1alpha1.VLLMConfig{MaxNumBatchedTokens: ptrInt32(8192)},
			contains: []flagCheck{{"--max-num-batched-tokens", "8192"}},
		},
		{
			name:        "maxNumBatchedTokens nil does not emit flag",
			cfg:         &inferencev1alpha1.VLLMConfig{},
			notContains: []string{"--max-num-batched-tokens"},
		},
		{
			name:     "attentionBackend=FLASHINFER emits flag (uppercase)",
			cfg:      &inferencev1alpha1.VLLMConfig{AttentionBackend: "FLASHINFER"},
			contains: []flagCheck{{"--attention-backend", "FLASHINFER"}},
		},
		{
			name:     "attentionBackend=flashinfer emits flag (lowercase compat)",
			cfg:      &inferencev1alpha1.VLLMConfig{AttentionBackend: "flashinfer"},
			contains: []flagCheck{{"--attention-backend", "flashinfer"}},
		},
		{
			name: "speculative enabled+model emits both flags",
			cfg: &inferencev1alpha1.VLLMConfig{
				Speculative: &inferencev1alpha1.SpeculativeConfig{
					Enabled:              ptrBool(true),
					Model:                "Qwen/Qwen3.6-4B",
					NumSpeculativeTokens: ptrInt32(4),
				},
			},
			contains: []flagCheck{
				{"--speculative-model", "Qwen/Qwen3.6-4B"},
				{"--num-speculative-tokens", "4"},
			},
		},
		{
			name: "speculative enabled without model skips both flags",
			cfg: &inferencev1alpha1.VLLMConfig{
				Speculative: &inferencev1alpha1.SpeculativeConfig{
					Enabled:              ptrBool(true),
					NumSpeculativeTokens: ptrInt32(4),
				},
			},
			notContains: []string{"--speculative-model", "--num-speculative-tokens"},
		},
		{
			name: "speculative disabled does not emit flags even with model set",
			cfg: &inferencev1alpha1.VLLMConfig{
				Speculative: &inferencev1alpha1.SpeculativeConfig{
					Enabled: ptrBool(false),
					Model:   "Qwen/Qwen3.6-4B",
				},
			},
			notContains: []string{"--speculative-model", "--num-speculative-tokens"},
		},
		{
			name:     "enableExpertParallel=true emits flag",
			cfg:      &inferencev1alpha1.VLLMConfig{EnableExpertParallel: ptrBool(true)},
			contains: []flagCheck{{"--enable-expert-parallel", ""}},
		},
		{
			name:        "enableExpertParallel=false does not emit flag",
			cfg:         &inferencev1alpha1.VLLMConfig{EnableExpertParallel: ptrBool(false)},
			notContains: []string{"--enable-expert-parallel"},
		},
		{
			name: "full agentic config emits all flags together",
			cfg: &inferencev1alpha1.VLLMConfig{
				TensorParallelSize:   ptrInt32(2),
				MaxModelLen:          ptrInt32(131072),
				Quantization:         "fp8",
				Dtype:                "bfloat16",
				KVCacheDtype:         ptrString("fp8_e5m2"),
				EnablePrefixCaching:  ptrBool(true),
				EnableChunkedPrefill: ptrBool(true),
				MaxNumBatchedTokens:  ptrInt32(8192),
				AttentionBackend:     "FLASHINFER",
			},
			contains: []flagCheck{
				{"--tensor-parallel-size", "2"},
				{"--max-model-len", "131072"},
				{"--quantization", "fp8"},
				{"--dtype", "bfloat16"},
				{"--kv-cache-dtype", "fp8_e5m2"},
				{"--enable-prefix-caching", ""},
				{"--enable-chunked-prefill", ""},
				{"--max-num-batched-tokens", "8192"},
				{"--attention-backend", "FLASHINFER"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "isvc-" + strings.ReplaceAll(tc.name, " ", "-"), Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime:    "vllm",
					ModelRef:   "test-model",
					VLLMConfig: tc.cfg,
				},
			}
			args := backend.BuildArgs(isvc, model, modelPath, port)
			for _, fc := range tc.contains {
				if !containsArg(args, fc.flag, fc.value) {
					t.Errorf("expected %q %q in args, got: %v", fc.flag, fc.value, args)
				}
			}
			for _, f := range tc.notContains {
				if containsArg(args, f, "") {
					t.Errorf("expected %q NOT in args, got: %v", f, args)
				}
			}
		})
	}
}

// TestVLLMBuildArgsDeterministic verifies BuildArgs emits flags in the same
// order across calls — important so Deployment .spec diffs stay quiet and
// snapshot tests do not flake.
func TestVLLMBuildArgsDeterministic(t *testing.T) {
	backend := &VLLMBackend{}
	model := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime: "vllm",
			VLLMConfig: &inferencev1alpha1.VLLMConfig{
				TensorParallelSize:   ptrInt32(2),
				KVCacheDtype:         ptrString("fp8_e5m2"),
				EnablePrefixCaching:  ptrBool(true),
				EnableChunkedPrefill: ptrBool(true),
				MaxNumBatchedTokens:  ptrInt32(8192),
				AttentionBackend:     "FLASHINFER",
			},
		},
	}

	first := backend.BuildArgs(isvc, model, "/models/x", 8000)
	for i := 0; i < 10; i++ {
		got := backend.BuildArgs(isvc, model, "/models/x", 8000)
		if len(got) != len(first) {
			t.Fatalf("iteration %d: length differs: got %d want %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("iteration %d pos %d: %q != %q", i, j, got[j], first[j])
			}
		}
	}
}

// TestResolveKVCacheDtype covers the precedence rules for the custom-vs-standard
// KV cache type field. Direct unit tests on the resolver are easier to debug
// than going through BuildArgs and arg-list scanning.
func TestResolveKVCacheDtype(t *testing.T) {
	cases := []struct {
		name     string
		custom   string
		standard *string
		want     string
	}{
		{name: "both unset returns empty", custom: "", standard: nil, want: ""},
		{name: "standard nil and custom empty returns empty", custom: "", standard: nil, want: ""},
		{name: "standard set, custom empty returns standard", custom: "", standard: ptrString("fp8_e4m3"), want: "fp8_e4m3"},
		{name: "standard auto, custom empty returns auto", custom: "", standard: ptrString("auto"), want: "auto"},
		{name: "custom set, standard nil returns custom", custom: "turbo2", standard: nil, want: "turbo2"},
		{name: "custom set, standard set returns custom (custom wins)", custom: "turbo2", standard: ptrString("fp8_e5m2"), want: "turbo2"},
		{name: "custom set, standard auto returns custom", custom: "turbo2", standard: ptrString("auto"), want: "turbo2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveKVCacheDtype(tc.custom, tc.standard)
			if got != tc.want {
				t.Errorf("resolveKVCacheDtype(%q, %v) = %q, want %q", tc.custom, tc.standard, got, tc.want)
			}
		})
	}
}

// TestValidateVLLMConfig exercises the spec validator that feeds the
// VLLMSpecValid status condition.
func TestValidateVLLMConfig(t *testing.T) {
	cases := []struct {
		name       string
		isvc       *inferencev1alpha1.InferenceService
		wantReason string
	}{
		{
			name:       "nil isvc is valid",
			isvc:       nil,
			wantReason: "",
		},
		{
			name: "nil vllm config is valid",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "vllm"},
			},
			wantReason: "",
		},
		{
			name: "speculative disabled is valid",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "vllm",
					VLLMConfig: &inferencev1alpha1.VLLMConfig{
						Speculative: &inferencev1alpha1.SpeculativeConfig{Enabled: ptrBool(false)},
					},
				},
			},
			wantReason: "",
		},
		{
			name: "speculative enabled with model is valid",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "vllm",
					VLLMConfig: &inferencev1alpha1.VLLMConfig{
						Speculative: &inferencev1alpha1.SpeculativeConfig{
							Enabled: ptrBool(true),
							Model:   "draft-model",
						},
					},
				},
			},
			wantReason: "",
		},
		{
			name: "speculative enabled without model reports SpeculativeMissingModel",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "vllm",
					VLLMConfig: &inferencev1alpha1.VLLMConfig{
						Speculative: &inferencev1alpha1.SpeculativeConfig{Enabled: ptrBool(true)},
					},
				},
			},
			wantReason: "SpeculativeMissingModel",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, message := ValidateVLLMConfig(tc.isvc)
			if reason != tc.wantReason {
				t.Errorf("reason: got %q want %q (message=%q)", reason, tc.wantReason, message)
			}
			if reason != "" && message == "" {
				t.Errorf("expected non-empty message when reason is set, got empty")
			}
		})
	}
}
