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

// TestVLLMBuildArgs is the single table-driven test that covers every new
// agentic-coding flag and the "not emitted when unset/false" counterpart.
// Each row asserts a set of must-contain and must-not-contain flags on the
// generated arg list.
func TestVLLMBuildArgs(t *testing.T) {
	backend := &VLLMBackend{}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}
	modelWithGpu := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Hardware: &inferencev1alpha1.HardwareSpec{
				GPU: &inferencev1alpha1.GPUSpec{
					Enabled: true,
					Count:   1,
				},
			},
		},
		ObjectMeta: metav1.ObjectMeta{Name: "test-model-gpu", Namespace: "default"},
	}
	const modelPath = "/models/test"
	const port = int32(8000)

	cases := []struct {
		// contains is a slice of flag/value pairs ("" value means "flag must be
		// present as a bare toggle").
		contains []FlagCheck
		// notContains is just a list of flags that must NOT appear anywhere in args.
		notContains []string
		model       *inferencev1alpha1.Model
		name        string
		spec        *inferencev1alpha1.InferenceServiceSpec
	}{
		{
			model: model,
			name:  "nil config emits only base flags (model as positional)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: nil,
			},
			// vLLM v0.20+ deprecated --model in favor of a positional argument.
			// The bare model path appears as args[0]; --model itself must NOT appear.
			contains: []FlagCheck{{"--host", "::"}, {"--port", "8000"}},
			notContains: []string{
				"--attention-backend",
				"--cpu-offload-gb",
				"--enable-chunked-prefill",
				"--enable-expert-parallel",
				"--enable-prefix-caching",
				"--gpu-memory-utilization",
				"--kv-cache-dtype",
				"--max-num-batched-tokens",
				"--model",
				"--speculative-model",
			},
		},
		{
			model: model,
			name:  "empty config emits only base flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{},
			},
			notContains: []string{
				"--cpu-offload-gb",
				"--enable-expert-parallel",
				"--enable-chunked-prefill",
				"--enable-prefix-caching",
				"--gpu-memory-utilization",
				"--kv-cache-dtype",
				"--max-num-batched-tokens",
				"--speculative-model",
			},
		},
		{
			model: modelWithGpu,
			name:  "cpuOffloadGB set emits flag if gpu is enabled",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model-gpu",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{CPUOffloadGB: ptrInt32(4)},
			},
			contains: []FlagCheck{{"--cpu-offload-gb", "4"}},
		},
		{
			model: model,
			name:  "cpuOffloadGB set does not emit flag if gpu is not enabled",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{CPUOffloadGB: ptrInt32(4)},
			},
			notContains: []string{"--cpu-offload-gb"},
		},
		{
			model: model,
			name:  "cpuOffloadGB nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{},
			},
			notContains: []string{"--cpu-offload-gb"},
		},
		{
			model: modelWithGpu,
			name:  "gpuMemoryUtilization set emits flag if gpu is enabled",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model-gpu",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{GPUMemoryUtilization: ptrFloat64(0.80)},
			},
			contains: []FlagCheck{{"--gpu-memory-utilization", "0.8"}},
		},
		{
			model: model,
			name:  "gpuMemoryUtilization does not emit flag if gpu is not enabled",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{GPUMemoryUtilization: ptrFloat64(0.80)},
			},
			notContains: []string{"--gpu-memory-utilization"},
		},
		{
			model: model,
			name:  "gpuMemoryUtilization nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{},
			},
			notContains: []string{"--gpu-memory-utilization"},
		},
		{
			model: model,
			name:  "kvCacheDtype=auto does not emit flag (vLLM default)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{KVCacheDtype: ptrString("auto")},
			},
			notContains: []string{"--kv-cache-dtype"},
		},
		{
			model: model,
			name:  "kvCacheDtype=fp8_e5m2 emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{KVCacheDtype: ptrString("fp8_e5m2")},
			},
			contains: []FlagCheck{{"--kv-cache-dtype", "fp8_e5m2"}},
		},
		{
			model: model,
			name:  "kvCacheDtype=fp8_e4m3 emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{KVCacheDtype: ptrString("fp8_e4m3")},
			},
			contains: []FlagCheck{{"--kv-cache-dtype", "fp8_e4m3"}},
		},
		{
			model: model,
			name:  "kvCacheCustomDtype=turbo2 emits flag (vLLM v0.20+ TurboQuant 2-bit)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{KVCacheCustomDtype: "turbo2"},
			},
			contains: []FlagCheck{{"--kv-cache-dtype", "turbo2"}},
		},
		{
			model: model,
			name:  "kvCacheCustomDtype wins over standard kvCacheDtype when both set",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "vllm",
				ModelRef: "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					KVCacheDtype:       ptrString("fp8_e4m3"),
					KVCacheCustomDtype: "turbo2",
				},
			},
			contains:    []FlagCheck{{"--kv-cache-dtype", "turbo2"}},
			notContains: []string{"fp8_e4m3"},
		},
		{
			model: model,
			name:  "kvCacheCustomDtype empty falls back to standard kvCacheDtype",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "vllm",
				ModelRef: "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					KVCacheDtype:       ptrString("fp8_e5m2"),
					KVCacheCustomDtype: "",
				},
			},
			contains: []FlagCheck{{"--kv-cache-dtype", "fp8_e5m2"}},
		},
		{
			model: model,
			name:  "enablePrefixCaching=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{EnablePrefixCaching: ptrBool(true)},
			},
			contains: []FlagCheck{{"--enable-prefix-caching", ""}},
		},
		{
			model: model,
			name:  "enablePrefixCaching=false does not emit flag (lets vLLM default)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{EnablePrefixCaching: ptrBool(false)},
			},
			notContains: []string{"--enable-prefix-caching"},
		},
		{
			model: model,
			name:  "enableChunkedPrefill=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{EnableChunkedPrefill: ptrBool(true)},
			},
			contains: []FlagCheck{{"--enable-chunked-prefill", ""}},
		},
		{
			model: model,
			name:  "enableChunkedPrefill=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{EnableChunkedPrefill: ptrBool(false)},
			},
			notContains: []string{"--enable-chunked-prefill"},
		},
		{
			model: model,
			name:  "maxNumBatchedTokens set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{MaxNumBatchedTokens: ptrInt32(8192)},
			},
			contains: []FlagCheck{{"--max-num-batched-tokens", "8192"}},
		},
		{
			model: model,
			name:  "maxNumBatchedTokens nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{},
			},
			notContains: []string{"--max-num-batched-tokens"},
		},
		{
			model: model,
			name:  "parallelSlots set emits flag (without extraArgs precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "vllm",
				ModelRef:      "test-model",
				ParallelSlots: ptrInt32(1),
				VLLMConfig:    &inferencev1alpha1.VLLMConfig{},
			},
			contains: []FlagCheck{{"--max-num-seqs", "1"}},
		},
		{
			model: model,
			name:  "parallelSlots set emits flag (with extraArgs precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "vllm",
				ModelRef:      "test-model",
				ExtraArgs:     []string{"--max-num-seqs", "8"},
				ParallelSlots: ptrInt32(4),
			},
			// NOTE: Since extraArgs are always last in position but still have priority
			// and containsArgs helper function always validate first occurrence, having
			// --max-num-seqs 8 case true mean that no duplicate due to parallelSlots was
			// found along the way.
			contains: []FlagCheck{{"--max-num-seqs", "8"}},
		},
		{
			model: model,
			name:  "parallelSlots set emits flag (with extraArgs inline precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "vllm",
				ModelRef:      "test-model",
				ExtraArgs:     []string{"--max-num-seqs=8"},
				ParallelSlots: ptrInt32(4),
			},
			// NOTE: Since extraArgs are always last in position but still have priority
			// and containsArgs helper function always validate first occurrence, having
			// --max-num-seqs 8 case true mean that no duplicate due to parallelSlots was
			// found along the way.
			contains: []FlagCheck{{"--max-num-seqs=8", ""}},
		},
		{
			model: model,
			name:  "parallelSlots nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{},
			},
			notContains: []string{"--max-num-seqs"},
		},
		{
			model: model,
			name:  "attentionBackend=FLASHINFER emits flag (uppercase)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{AttentionBackend: "FLASHINFER"},
			},
			contains: []FlagCheck{{"--attention-backend", "FLASHINFER"}},
		},
		{
			model: model,
			name:  "attentionBackend=flashinfer emits flag (lowercase compat)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{AttentionBackend: "flashinfer"},
			},
			contains: []FlagCheck{{"--attention-backend", "flashinfer"}},
		},
		{
			model: model,
			name:  "speculative enabled+model emits both flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "vllm",
				ModelRef: "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					Speculative: &inferencev1alpha1.SpeculativeConfig{
						Enabled:              ptrBool(true),
						Model:                "Qwen/Qwen3.6-4B",
						NumSpeculativeTokens: ptrInt32(4),
					},
				},
			},
			contains: []FlagCheck{
				{"--speculative-model", "Qwen/Qwen3.6-4B"},
				{"--num-speculative-tokens", "4"},
			},
		},
		{
			model: model,
			name:  "speculative enabled without model skips both flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "vllm",
				ModelRef: "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					Speculative: &inferencev1alpha1.SpeculativeConfig{
						Enabled:              ptrBool(true),
						NumSpeculativeTokens: ptrInt32(4),
					},
				},
			},
			notContains: []string{"--speculative-model", "--num-speculative-tokens"},
		},
		{
			model: model,
			name:  "speculative disabled does not emit flags even with model set",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "vllm",
				ModelRef: "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					Speculative: &inferencev1alpha1.SpeculativeConfig{
						Enabled: ptrBool(false),
						Model:   "Qwen/Qwen3.6-4B",
					},
				},
			},
			notContains: []string{"--speculative-model", "--num-speculative-tokens"},
		},
		{
			model: model,
			name:  "enableExpertParallel=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{EnableExpertParallel: ptrBool(true)},
			},
			contains: []FlagCheck{{"--enable-expert-parallel", ""}},
		},
		{
			model: model,
			name:  "enableExpertParallel=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{EnableExpertParallel: ptrBool(false)},
			},
			notContains: []string{"--enable-expert-parallel"},
		},
		{
			model: model,
			name:  "full agentic config emits all flags together",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "vllm",
				ModelRef:      "test-model",
				ParallelSlots: ptrInt32(1),
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					AttentionBackend:     "FLASHINFER",
					Dtype:                "bfloat16",
					EnablePrefixCaching:  ptrBool(true),
					EnableChunkedPrefill: ptrBool(true),
					KVCacheDtype:         ptrString("fp8_e5m2"),
					MaxModelLen:          ptrInt32(131072),
					MaxNumBatchedTokens:  ptrInt32(8192),
					Quantization:         "fp8",
					TensorParallelSize:   ptrInt32(2),
				},
			},
			contains: []FlagCheck{
				{"--attention-backend", "FLASHINFER"},
				{"--dtype", "bfloat16"},
				{"--enable-prefix-caching", ""},
				{"--enable-chunked-prefill", ""},
				{"--kv-cache-dtype", "fp8_e5m2"},
				{"--max-model-len", "131072"},
				{"--max-num-batched-tokens", "8192"},
				{"--max-num-seqs", "1"},
				{"--quantization", "fp8"},
				{"--tensor-parallel-size", "2"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "isvc-" + strings.ReplaceAll(tc.name, " ", "-"), Namespace: "default"},
				Spec:       *tc.spec,
			}
			args := backend.BuildArgs(isvc, tc.model, modelPath, port)
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

// TestVLLMDisableServiceLinks asserts the VLLMBackend opts out of the legacy
// K8s service-link env-var injection. vLLM v0.20+ flags every K8s-injected
// VLLM_<svcname>_* env var as unknown; disabling service links suppresses the
// noise without affecting DNS service discovery.
func TestVLLMDisableServiceLinks(t *testing.T) {
	backend := &VLLMBackend{}
	if !backend.DisableServiceLinks() {
		t.Error("VLLMBackend.DisableServiceLinks() = false, want true (vLLM v0.20+ env validator noise)")
	}
}

// TestResolveEnableServiceLinks covers the wiring between ServiceLinksOptOut
// and the *bool the deployment builder writes to Pod.Spec.EnableServiceLinks.
// nil = K8s default (links on); explicit *false = links off.
func TestResolveEnableServiceLinks(t *testing.T) {
	cases := []struct {
		name       string
		backend    RuntimeBackend
		wantNil    bool
		wantValue  bool
		hasOptOut  bool
		expectFlag string
	}{
		{
			name:    "VLLMBackend opts out -> *false",
			backend: &VLLMBackend{},
			wantNil: false, wantValue: false,
		},
		{
			name:    "LlamaCppBackend does not implement opt-out -> nil (default)",
			backend: &LlamaCppBackend{},
			wantNil: true,
		},
		{
			name:    "GenericBackend does not implement opt-out -> nil (default)",
			backend: &GenericBackend{},
			wantNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveEnableServiceLinks(tc.backend)
			if tc.wantNil {
				if got != nil {
					t.Errorf("got %v, want nil (default service links)", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil, want non-nil (explicit opt-out)")
			}
			if *got != tc.wantValue {
				t.Errorf("got *%v, want *%v", *got, tc.wantValue)
			}
		})
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
