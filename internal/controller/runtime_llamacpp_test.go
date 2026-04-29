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

// TestLlamaCppBuildArgs is the single table-driven test that covers every new
// agentic-coding flag and the "not emitted when unset/false" counterpart.
// Each row asserts a set of must-contain and must-not-contain flags on the
// generated arg list.
func TestLlamaCppBuildArgs(t *testing.T) {
	backend := &LlamaCppBackend{}
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
		spec        *inferencev1alpha1.InferenceServiceSpec
		contains    []flagCheck
		notContains []string
	}{
		{
			name: "empty config emits only base flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--ctx-size", "--parallel", "--flash-attn", "--jinja", "--cache-type-k", "--cpu-moe", "--n-cpu-moe", "--no-kv-offload", "--override-tensor", "--override-kv", "--batch-size", "--ubatch-size", "--no-warmup", "--reasoning-budget", "--reasoning-budget-message"},
		},
		{
			name: "contextSize set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:     "llama",
				ModelRef:    "test-model",
				ContextSize: ptrInt32(8192),
			},
			contains: []flagCheck{{"--ctx-size", "8192"}},
		},
		{
			name: "contextSize nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--ctx-size"},
		},
		{
			name: "parallelSlots set emits flag (without extraArgs precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "llama",
				ModelRef:      "test-model",
				ParallelSlots: ptrInt32(8192),
			},
			contains: []flagCheck{{"--parallel", "8192"}},
		},
		{
			name: "parallelSlots set emits flag (with extraArgs precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "llama",
				ModelRef:      "test-model",
				ExtraArgs:     []string{"--parallel", "8"},
				ParallelSlots: ptrInt32(4),
			},
			// NOTE: Since extraArgs are always last in position but still have priority
			// and containsArgs helper function always validate first occurrence, having
			// --parallel 8 case true mean that no duplicate due to parallelSlots was
			// found along the way.
			contains: []flagCheck{{"--parallel", "8"}},
		},
		{
			name: "parallelSlots set emits flag (with extraArgs inline precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "llama",
				ModelRef:      "test-model",
				ExtraArgs:     []string{"--parallel=8"},
				ParallelSlots: ptrInt32(4),
			},
			// NOTE: Since extraArgs are always last in position but still have priority
			// and containsArgs helper function always validate first occurrence, having
			// --parallel 8 case true mean that no duplicate due to parallelSlots was
			// found along the way.
			contains: []flagCheck{{"--parallel=8", ""}},
		},
		{
			name: "parallelSlots nil does not emit flag (without extraArgs precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--parallel"},
		},
		{
			name: "batchSize set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:   "llama",
				ModelRef:  "test-model",
				BatchSize: ptrInt32(8192),
			},
			contains: []flagCheck{{"--batch-size", "8192"}},
		},
		{
			name: "batchSize nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--batch-size"},
		},
		{
			name: "uBatchSize set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "llama",
				ModelRef:   "test-model",
				UBatchSize: ptrInt32(8192),
			},
			contains: []flagCheck{{"--ubatch-size", "8192"}},
		},
		{
			name: "uBatchSize nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--ubatch-size"},
		},
		{
			name: "moeCPULayers set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "llama",
				ModelRef:     "test-model",
				MoeCPULayers: ptrInt32(8192),
			},
			contains: []flagCheck{{"--n-cpu-moe", "8192"}},
		},
		{
			name: "moeCPULayers nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--n-cpu-moe"},
		},
		{
			name: "flashAttention=true does not emit flag without GPU",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:        "llama",
				ModelRef:       "test-model",
				FlashAttention: ptrBool(true),
			},
			notContains: []string{"--flash-attn"},
		},
		{
			name: "flashAttention=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:        "llama",
				ModelRef:       "test-model",
				FlashAttention: ptrBool(false),
			},
			notContains: []string{"--flash-attn"},
		},
		{
			name: "jinja=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				Jinja:    ptrBool(true),
			},
			contains: []flagCheck{{"--jinja", ""}},
		},
		{
			name: "jinja=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				Jinja:    ptrBool(false),
			},
			notContains: []string{"--jinja"},
		},
		{
			name: "moeCPUOffload=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "llama",
				ModelRef:      "test-model",
				MoeCPUOffload: ptrBool(true),
			},
			contains: []flagCheck{{"--cpu-moe", ""}},
		},
		{
			name: "moeCPUOffload=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "llama",
				ModelRef:      "test-model",
				MoeCPUOffload: ptrBool(false),
			},
			notContains: []string{"--cpu-moe"},
		},
		{
			name: "noKVOffload=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:     "llama",
				ModelRef:    "test-model",
				NoKvOffload: ptrBool(true),
			},
			contains: []flagCheck{{"--no-kv-offload", ""}},
		},
		{
			name: "noKVOffload=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:     "llama",
				ModelRef:    "test-model",
				NoKvOffload: ptrBool(false),
			},
			notContains: []string{"--no-kv-offload"},
		},
		{
			name: "noWarmup=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				NoWarmup: ptrBool(true),
			},
			contains: []flagCheck{{"--no-warmup", ""}},
		},
		{
			name: "noWarmup=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				NoWarmup: ptrBool(false),
			},
			notContains: []string{"--no-warmup"},
		},
		{
			name: "reasoningBudget set emits flag (without message)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:         "llama",
				ModelRef:        "test-model",
				ReasoningBudget: ptrInt32(8192),
			},
			contains: []flagCheck{{"--reasoning-budget", "8192"}},
		},
		{
			name: "reasoningBudget set emits flag (with message)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:                "llama",
				ModelRef:               "test-model",
				ReasoningBudget:        ptrInt32(8192),
				ReasoningBudgetMessage: "message",
			},
			contains: []flagCheck{
				{"--reasoning-budget", "8192"},
				{"--reasoning-budget-message", "message"},
			},
		},
		{
			name: "cacheTypeK set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "llama",
				ModelRef:   "test-model",
				CacheTypeK: "key",
			},
			contains: []flagCheck{{"--cache-type-k", "key"}},
		},
		{
			name: "cacheTypeK nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--cache-type-k"},
		},
		{
			name: "cacheTypeV set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "llama",
				ModelRef:   "test-model",
				CacheTypeV: "value",
			},
			contains: []flagCheck{{"--cache-type-v", "value"}},
		},
		{
			name: "cacheTypeV nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--cache-type-v"},
		},
		{
			name: "tensorOverride set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:         "llama",
				ModelRef:        "test-model",
				TensorOverrides: []string{"value1", "value2"},
			},
			contains: []flagCheck{
				{"--override-tensor", "value1"},
				{"--override-tensor", "value2"},
			},
		},
		{
			name: "tensorOverrides nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--override-tensor"},
		},
		{
			name: "metadataOverride set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:           "llama",
				ModelRef:          "test-model",
				MetadataOverrides: []string{"value1", "value2"},
			},
			contains: []flagCheck{
				{"--override-kv", "value1"},
				{"--override-kv", "value2"},
			},
		},
		{
			name: "metadataOverride nil does not emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--override-kv"},
		},
		{
			name: "full agentic config emits all flags together",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:                "llama",
				ModelRef:               "test-model",
				BatchSize:              ptrInt32(2),
				CacheTypeK:             "key",
				CacheTypeV:             "value",
				ContextSize:            ptrInt32(2),
				FlashAttention:         ptrBool(true),
				Jinja:                  ptrBool(true),
				MetadataOverrides:      []string{"value1", "value2"},
				MoeCPUOffload:          ptrBool(true),
				MoeCPULayers:           ptrInt32(2),
				NoKvOffload:            ptrBool(true),
				NoWarmup:               ptrBool(true),
				ParallelSlots:          ptrInt32(2),
				ReasoningBudget:        ptrInt32(2),
				ReasoningBudgetMessage: "message",
				TensorOverrides:        []string{"value1", "value2"},
				UBatchSize:             ptrInt32(2),
			},
			contains: []flagCheck{
				{"--batch-size", "2"},
				{"--cache-type-k", "key"},
				{"--cache-type-v", "value"},
				{"--ctx-size", "2"},
				{"--jinja", ""},
				{"--override-kv", "value1"},
				{"--override-kv", "value2"},
				{"--cpu-moe", ""},
				{"--n-cpu-moe", "2"},
				{"--n-cpu-moe", "2"},
				{"--no-kv-offload", ""},
				{"--no-warmup", ""},
				{"--parallel", "2"},
				{"--reasoning-budget", "2"},
				{"--reasoning-budget-message", "message"},
				{"--override-tensor", "value1"},
				{"--override-tensor", "value2"},
				{"--ubatch-size", "2"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "isvc-" + strings.ReplaceAll(tc.name, " ", "-"), Namespace: "default"},
				Spec:       *tc.spec,
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
