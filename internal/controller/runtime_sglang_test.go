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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// sglangFlagsNeverInBase is the list of SGLang-specific flags that must NOT
// appear in BuildArgs output when SGLangConfig is nil or empty. Shared across
// TestSGLangBuildArgs_NilConfig and TestSGLangBuildArgs_EmptyConfig.
var sglangFlagsNeverInBase = []string{
	"--tp", "--ep", "--dp", "--context-length", "--mem-fraction-static",
	"--chunked-prefill-size", "--max-running-requests", "--quantization",
	"--kv-cache-dtype", "--attention-backend", "--enable-prefix-caching",
	"--tool-call-parser", "--reasoning-parser", "--chat-template",
	"--speculative-algorithm", "--speculative-accept-threshold-single",
	"--speculative-accept-threshold-acc",
	"--lora-modules", "--max-lora-rank", "--lora-target-modules",
	"--model", "--reasoning-content", "--return-logprob", "--log-level",
	"--trust-remote-code", "--skip-tokenizer-init",
	"--is-embedding",
}

// TestSGLangBackendDefaults locks in the trivial-method contracts that every
// runtime backend exposes (image, port, container name, model-init flag,
// HPA metric). Mirrors the structure of VLLMBackend tests.
func TestSGLangBackendDefaults(t *testing.T) {
	b := &SGLangBackend{}
	if got := b.ContainerName(); got != "sglang" {
		t.Errorf("ContainerName() = %q, want %q", got, "sglang")
	}
	if got := b.DefaultImage(); got != sglangCUDAImage {
		t.Errorf("DefaultImage() = %q, want %q", got, sglangCUDAImage)
	}
	if got := b.DefaultPort(); got != 30000 {
		t.Errorf("DefaultPort() = %d, want 30000", got)
	}
	if !b.NeedsModelInit() {
		t.Error("NeedsModelInit() = false, want true")
	}
	if got := b.DefaultHPAMetric(); got != "sglang:num_running_reqs" {
		t.Errorf("DefaultHPAMetric() = %q, want %q", got, "sglang:num_running_reqs")
	}
}

// TestSGLangBuildArgs_NilConfig asserts the base arg emission when no
// SGLangConfig is provided.
func TestSGLangBuildArgs_NilConfig(t *testing.T) {
	backend := &SGLangBackend{}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "isvc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:  "sglang",
			ModelRef: "test-model",
		},
	}
	args := backend.BuildArgs(isvc, model, "/models/test", 30000)

	mustContain := []FlagCheck{
		{"--model-path", "/models/test"},
		{"--host", "::"},
		{"--port", "30000"},
		{"--enable-metrics", ""},
	}
	for _, fc := range mustContain {
		if !containsArg(args, fc.flag, fc.value) {
			t.Errorf("expected %q %q in args, got: %v", fc.flag, fc.value, args)
		}
	}
	for _, f := range sglangFlagsNeverInBase {
		if containsArg(args, f, "") {
			t.Errorf("expected %q NOT in args, got: %v", f, args)
		}
	}
}

// TestSGLangBuildArgs_EmptyConfig asserts the same base flags when an empty
// (non-nil) SGLangConfig is provided.
func TestSGLangBuildArgs_EmptyConfig(t *testing.T) {
	backend := &SGLangBackend{}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "isvc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:      "sglang",
			ModelRef:     "test-model",
			SGLangConfig: &inferencev1alpha1.SGLangConfig{},
		},
	}
	args := backend.BuildArgs(isvc, model, "/models/test", 30000)

	for _, fc := range []FlagCheck{{"--model-path", "/models/test"}, {"--host", "::"}, {"--port", "30000"}, {"--enable-metrics", ""}} {
		if !containsArg(args, fc.flag, fc.value) {
			t.Errorf("expected %q %q in args, got: %v", fc.flag, fc.value, args)
		}
	}
	for _, f := range sglangFlagsNeverInBase {
		if containsArg(args, f, "") {
			t.Errorf("expected %q NOT in args, got: %v", f, args)
		}
	}
}

// sglangGPUModel returns a Model with one NVIDIA GPU enabled (helper for the
// memFractionStatic GPU-only check).
func sglangGPUModel() *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Hardware: &inferencev1alpha1.HardwareSpec{
				GPU: &inferencev1alpha1.GPUSpec{Enabled: true, Count: 1},
			},
		},
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-model", Namespace: "default"},
	}
}

// sglangMultiGPUModel returns a Model with two NVIDIA GPUs for tp-auto tests.
func sglangMultiGPUModel() *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Hardware: &inferencev1alpha1.HardwareSpec{
				GPU: &inferencev1alpha1.GPUSpec{Enabled: true, Count: 2},
			},
		},
		ObjectMeta: metav1.ObjectMeta{Name: "multi-gpu-model", Namespace: "default"},
	}
}

func TestSGLangBuildArgs(t *testing.T) {
	backend := &SGLangBackend{}
	const modelPath = "/models/test"
	const port = int32(30000)

	cases := []struct {
		contains    []FlagCheck
		notContains []string
		model       *inferencev1alpha1.Model
		name        string
		spec        *inferencev1alpha1.InferenceServiceSpec
	}{
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"}},
			name:  "served-model-name defaults to ModelRef",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "sglang",
				ModelRef: "test-model",
			},
			contains: []FlagCheck{{"--served-model-name", "test-model"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "fallback-name"}},
			name:  "served-model-name falls back to model.Name when ModelRef empty",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
			},
			contains: []FlagCheck{{"--served-model-name", "fallback-name"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "served-model-name not emitted when extraArgs already has it",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:   "sglang",
				ModelRef:  "should-not-appear",
				ExtraArgs: []string{"--served-model-name", "custom"},
			},
			contains:    []FlagCheck{{"--served-model-name", "custom"}},
			notContains: []string{"should-not-appear"},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "mode=embedding emits --is-embedding",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				Mode:    "embedding",
			},
			contains: []FlagCheck{{"--is-embedding", ""}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "endpoint-path /embeddings infers embedding mode, emits --is-embedding",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				Endpoint: &inferencev1alpha1.EndpointSpec{
					Path: "/v1/embeddings",
				},
			},
			contains: []FlagCheck{{"--is-embedding", ""}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "mode=embedding not double-emitted when extraArgs already has it",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:   "sglang",
				Mode:      "embedding",
				ExtraArgs: []string{"--is-embedding=false"},
			},
			contains: []FlagCheck{{"--is-embedding=false", ""}},
		},
		{
			model:    sglangMultiGPUModel(),
			name:     "gpuCount>1 auto-derives --tp",
			spec:     &inferencev1alpha1.InferenceServiceSpec{Runtime: "sglang"},
			contains: []FlagCheck{{"--tp", "2"}},
		},
		{
			model: sglangMultiGPUModel(),
			name:  "explicit tensorParallelSize overrides gpuCount auto-derive",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{TensorParallelSize: ptrInt32(4)},
			},
			contains: []FlagCheck{{"--tp", "4"}},
		},
		{
			model:       &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:        "gpuCount==1 does not auto-emit --tp",
			spec:        &inferencev1alpha1.InferenceServiceSpec{Runtime: "sglang"},
			notContains: []string{"--tp"},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "expertParallelSize emits --ep",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{ExpertParallelSize: ptrInt32(2)},
			},
			contains: []FlagCheck{{"--ep", "2"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "dataParallelSize emits --dp",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{DataParallelSize: ptrInt32(4)},
			},
			contains: []FlagCheck{{"--dp", "4"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "contextLength emits --context-length",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{ContextLength: ptrInt32(131072)},
			},
			contains: []FlagCheck{{"--context-length", "131072"}},
		},
		{
			model: sglangGPUModel(),
			name:  "memFractionStatic emits --mem-fraction-static on GPU model",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{MemFractionStatic: ptrFloat64(0.85)},
			},
			contains: []FlagCheck{{"--mem-fraction-static", "0.85"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "memFractionStatic on CPU model logs warning, no flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{MemFractionStatic: ptrFloat64(0.85)},
			},
			notContains: []string{"--mem-fraction-static"},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "chunkedPrefillSize emits --chunked-prefill-size",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{ChunkedPrefillSize: ptrInt32(8192)},
			},
			contains: []FlagCheck{{"--chunked-prefill-size", "8192"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "maxRunningRequests emits --max-running-requests",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{MaxRunningRequests: ptrInt32(64)},
			},
			contains: []FlagCheck{{"--max-running-requests", "64"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "quantization emits --quantization",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{Quantization: "fp8"},
			},
			contains: []FlagCheck{{"--quantization", "fp8"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "kvCacheDtype=auto does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{KVCacheDtype: ptrString("auto")},
			},
			notContains: []string{"--kv-cache-dtype"},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "kvCacheDtype=fp8_e5m2 emits --kv-cache-dtype",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{KVCacheDtype: ptrString("fp8_e5m2")},
			},
			contains: []FlagCheck{{"--kv-cache-dtype", "fp8_e5m2"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "kvCacheCustomDtype wins over kvCacheDtype",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					KVCacheDtype:       ptrString("fp8_e4m3"),
					KVCacheCustomDtype: "turbo2",
				},
			},
			contains:    []FlagCheck{{"--kv-cache-dtype", "turbo2"}},
			notContains: []string{"fp8_e4m3"},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "attentionBackend=flashinfer emits --attention-backend",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{AttentionBackend: "flashinfer"},
			},
			contains: []FlagCheck{{"--attention-backend", "flashinfer"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "enablePrefixCaching=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{EnablePrefixCaching: ptrBool(true)},
			},
			contains: []FlagCheck{{"--enable-prefix-caching", ""}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "enablePrefixCaching=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{EnablePrefixCaching: ptrBool(false)},
			},
			notContains: []string{"--enable-prefix-caching"},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "toolCallParser=llama3 emits --tool-call-parser",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{ToolCallParser: "llama3"},
			},
			contains: []FlagCheck{{"--tool-call-parser", "llama3"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "reasoningParser=qwen3 emits --reasoning-parser",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{ReasoningParser: "qwen3"},
			},
			contains: []FlagCheck{{"--reasoning-parser", "qwen3"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "chatTemplate emits --chat-template",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{ChatTemplate: "/path/to/template.jinja"},
			},
			contains: []FlagCheck{{"--chat-template", "/path/to/template.jinja"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "speculative enabled without algorithm skips all speculative flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					Speculative: &inferencev1alpha1.SGLangSpeculativeConfig{
						Enabled:        ptrBool(true),
						Algorithm:      "",
						DraftModelPath: "/models/draft",
					},
				},
			},
			notContains: []string{"--speculative-algorithm", "--speculative-draft-model-path", "--speculative-num-steps", "--speculative-eagle-topk", "--speculative-num-draft-tokens"},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "speculative enabled+configured emits all flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					Speculative: &inferencev1alpha1.SGLangSpeculativeConfig{
						Enabled:        ptrBool(true),
						Algorithm:      "EAGLE3",
						DraftModelPath: "/models/draft",
						NumSteps:       ptrInt32(3),
						EagleTopK:      ptrInt32(8),
						NumDraftTokens: ptrInt32(5),
					},
				},
			},
			contains: []FlagCheck{
				{"--speculative-algorithm", "EAGLE3"},
				{"--speculative-draft-model-path", "/models/draft"},
				{"--speculative-num-steps", "3"},
				{"--speculative-eagle-topk", "8"},
				{"--speculative-num-draft-tokens", "5"},
			},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "loraModules emits --lora-modules",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					LoraModules: []string{`{"name":"loraA","path":"/loras/a"}`},
				},
			},
			contains: []FlagCheck{{"--lora-modules", "loraA=/loras/a"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "maxLoraRank emits --max-lora-rank",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{MaxLoraRank: ptrInt32(64)},
			},
			contains: []FlagCheck{{"--max-lora-rank", "64"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "loraTargetModules emits --lora-target-modules",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					LoraTargetModules: []string{"q_proj", "k_proj"},
				},
			},
			contains: []FlagCheck{{"--lora-target-modules", "q_proj,k_proj"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "model override emits --model",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					Model: "openai/gpt-oss-120b",
				},
			},
			contains: []FlagCheck{{"--model", "openai/gpt-oss-120b"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "reasoning-content enabled emits --reasoning-content enabled",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					ReasoningContent: ptrString("enabled"),
				},
			},
			contains: []FlagCheck{{"--reasoning-content", "enabled"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "return-logprob emits --return-logprob",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					ReturnLogprob: ptrBool(true),
				},
			},
			contains: []FlagCheck{{"--return-logprob", ""}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "log-level emits --log-level",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					LogLevel: "warning",
				},
			},
			contains: []FlagCheck{{"--log-level", "warning"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "trust-remote-code true emits --trust-remote-code",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					TrustRemoteCode: ptrBool(true),
				},
			},
			contains: []FlagCheck{{"--trust-remote-code", ""}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "skip-tokenizer-init true emits --skip-tokenizer-init",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					SkipTokenizerInit: ptrBool(true),
				},
			},
			contains: []FlagCheck{{"--skip-tokenizer-init", ""}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "speculative accept thresholds emit --speculative-accept-threshold-*",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					Speculative: &inferencev1alpha1.SGLangSpeculativeConfig{
						Enabled:               ptrBool(true),
						Algorithm:             "EAGLE",
						DraftModelPath:        "/models/draft",
						AcceptThresholdSingle: ptrFloat64(0.9),
						AcceptThresholdAcc:    ptrFloat64(0.8),
					},
				},
			},
			contains: []FlagCheck{
				{"--speculative-algorithm", "EAGLE"},
				{"--speculative-draft-model-path", "/models/draft"},
				{"--speculative-accept-threshold-single", "0.9"},
				{"--speculative-accept-threshold-acc", "0.8"},
			},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "typed loraAdapters emits --lora-modules name=path pairs",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					LoraAdapters: []inferencev1alpha1.SGLangLoRAAdapter{
						{Name: "loraA", Path: "/loras/a"},
						{Name: "loraB", Path: "/loras/b"},
					},
				},
			},
			contains: []FlagCheck{{"--lora-modules", "loraA=/loras/a,loraB=/loras/b"}},
		},
		{
			model: &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			name:  "typed and legacy LoRA merge with typed winning on name collision",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					LoraAdapters: []inferencev1alpha1.SGLangLoRAAdapter{
						{Name: "loraA", Path: "/loras/typed-a"},
					},
					LoraModules: []string{
						`{"name":"loraA","path":"/loras/legacy-a"}`,
						`{"name":"loraB","path":"/loras/legacy-b"}`,
					},
				},
			},
			contains: []FlagCheck{
				{"--lora-modules", "loraA=/loras/typed-a,loraB=/loras/legacy-b"},
			},
			notContains: []string{"/loras/legacy-a"},
		},
		{
			model: sglangGPUModel(),
			name:  "full agentic config emits all flags together",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime: "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{
					TensorParallelSize:  ptrInt32(2),
					ContextLength:       ptrInt32(131072),
					MemFractionStatic:   ptrFloat64(0.85),
					ChunkedPrefillSize:  ptrInt32(8192),
					MaxRunningRequests:  ptrInt32(64),
					Quantization:        "fp8",
					KVCacheDtype:        ptrString("fp8_e5m2"),
					AttentionBackend:    "flashinfer",
					EnablePrefixCaching: ptrBool(true),
					ToolCallParser:      "qwen3",
					ReasoningParser:     "qwen3",
				},
			},
			contains: []FlagCheck{
				{"--tp", "2"},
				{"--context-length", "131072"},
				{"--mem-fraction-static", "0.85"},
				{"--chunked-prefill-size", "8192"},
				{"--max-running-requests", "64"},
				{"--quantization", "fp8"},
				{"--kv-cache-dtype", "fp8_e5m2"},
				{"--attention-backend", "flashinfer"},
				{"--enable-prefix-caching", ""},
				{"--tool-call-parser", "qwen3"},
				{"--reasoning-parser", "qwen3"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "isvc", Namespace: "default"},
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

// TestSGLangBuildArgsDeterministic verifies BuildArgs emits flags in the
// same order across calls so Deployment .spec diffs stay quiet.
func TestSGLangBuildArgsDeterministic(t *testing.T) {
	backend := &SGLangBackend{}
	model := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime: "sglang",
			SGLangConfig: &inferencev1alpha1.SGLangConfig{
				TensorParallelSize:  ptrInt32(2),
				KVCacheDtype:        ptrString("fp8_e5m2"),
				EnablePrefixCaching: ptrBool(true),
				ContextLength:       ptrInt32(131072),
			},
		},
	}
	first := backend.BuildArgs(isvc, model, "/models/x", 30000)
	for i := 0; i < 10; i++ {
		got := backend.BuildArgs(isvc, model, "/models/x", 30000)
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

func TestSGLangBuildCommand(t *testing.T) {
	b := &SGLangBackend{}
	want := []string{"python3", "-m", "sglang.launch_server"}
	got := b.BuildCommand()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildCommand() = %v, want %v", got, want)
	}
}

func TestSGLangProbes(t *testing.T) {
	b := &SGLangBackend{}
	startup, liveness, readiness := b.BuildProbes(30000)

	if startup == nil || startup.HTTPGet == nil || startup.HTTPGet.Path != "/health_generate" {
		t.Errorf("startup probe should hit /health_generate, got %+v", startup)
	}
	if startup.FailureThreshold != 180 {
		t.Errorf("startup FailureThreshold = %d, want 180 (cold-start tolerance)", startup.FailureThreshold)
	}

	if liveness == nil || liveness.HTTPGet == nil || liveness.HTTPGet.Path != "/health" {
		t.Errorf("liveness probe should hit /health, got %+v", liveness)
	}
	if liveness.FailureThreshold != 3 {
		t.Errorf("liveness FailureThreshold = %d, want 3", liveness.FailureThreshold)
	}

	if readiness == nil || readiness.HTTPGet == nil || readiness.HTTPGet.Path != "/health_generate" {
		t.Errorf("readiness probe should hit /health_generate, got %+v", readiness)
	}
	if readiness.FailureThreshold != 3 {
		t.Errorf("readiness FailureThreshold = %d, want 3", readiness.FailureThreshold)
	}
}

func TestSGLangBuildEnv(t *testing.T) {
	b := &SGLangBackend{}

	// nil when no HFTokenSecretRef.
	if got := b.BuildEnv(&inferencev1alpha1.InferenceService{
		Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "sglang"},
	}); got != nil {
		t.Errorf("BuildEnv() with no HFTokenSecretRef = %v, want nil", got)
	}

	// HF_TOKEN env when SecretRef is set.
	isvc := &inferencev1alpha1.InferenceService{
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime: "sglang",
			SGLangConfig: &inferencev1alpha1.SGLangConfig{
				HFTokenSecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "hf-secret"},
					Key:                  "HF_TOKEN",
				},
			},
		},
	}
	got := b.BuildEnv(isvc)
	if len(got) != 1 {
		t.Fatalf("BuildEnv() = %v, want one env var", got)
	}
	if got[0].Name != "HF_TOKEN" {
		t.Errorf("env[0].Name = %q, want %q", got[0].Name, "HF_TOKEN")
	}
	if got[0].ValueFrom == nil || got[0].ValueFrom.SecretKeyRef == nil {
		t.Errorf("env[0].ValueFrom = nil, want SecretKeyRef")
	}
}

func TestValidateSGLangConfig(t *testing.T) {
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
			name: "nil sglang config is valid",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "sglang"},
			},
			wantReason: "",
		},
		{
			name: "speculative disabled is valid",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "sglang",
					SGLangConfig: &inferencev1alpha1.SGLangConfig{
						Speculative: &inferencev1alpha1.SGLangSpeculativeConfig{Enabled: ptrBool(false)},
					},
				},
			},
			wantReason: "",
		},
		{
			name: "speculative enabled+configured is valid",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "sglang",
					SGLangConfig: &inferencev1alpha1.SGLangConfig{
						Speculative: &inferencev1alpha1.SGLangSpeculativeConfig{
							Enabled:        ptrBool(true),
							Algorithm:      "EAGLE3",
							DraftModelPath: "/models/draft",
						},
					},
				},
			},
			wantReason: "",
		},
		{
			name: "speculative enabled without algorithm reports SpeculativeMissingConfig",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "sglang",
					SGLangConfig: &inferencev1alpha1.SGLangConfig{
						Speculative: &inferencev1alpha1.SGLangSpeculativeConfig{
							Enabled:        ptrBool(true),
							DraftModelPath: "/models/draft",
						},
					},
				},
			},
			wantReason: "SpeculativeMissingConfig",
		},
		{
			name: "speculative enabled without draft-model-path reports SpeculativeMissingConfig",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "sglang",
					SGLangConfig: &inferencev1alpha1.SGLangConfig{
						Speculative: &inferencev1alpha1.SGLangSpeculativeConfig{
							Enabled:   ptrBool(true),
							Algorithm: "EAGLE3",
						},
					},
				},
			},
			wantReason: "SpeculativeMissingConfig",
		},
		{
			name: "accept threshold without speculative enabled is invalid",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "sglang",
					SGLangConfig: &inferencev1alpha1.SGLangConfig{
						Speculative: &inferencev1alpha1.SGLangSpeculativeConfig{
							Enabled:               ptrBool(false),
							Algorithm:             "EAGLE",
							DraftModelPath:        "/models/draft",
							AcceptThresholdSingle: ptrFloat64(0.9),
						},
					},
				},
			},
			wantReason: "SpeculativeAcceptThresholdUnused",
		},
		{
			name: "accept threshold with speculative enabled+configured is valid",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "sglang",
					SGLangConfig: &inferencev1alpha1.SGLangConfig{
						Speculative: &inferencev1alpha1.SGLangSpeculativeConfig{
							Enabled:               ptrBool(true),
							Algorithm:             "EAGLE",
							DraftModelPath:        "/models/draft",
							AcceptThresholdSingle: ptrFloat64(0.9),
							AcceptThresholdAcc:    ptrFloat64(0.8),
						},
					},
				},
			},
			wantReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, message := ValidateSGLangConfig(tc.isvc)
			if reason != tc.wantReason {
				t.Errorf("reason: got %q want %q (message=%q)", reason, tc.wantReason, message)
			}
			if reason != "" && message == "" {
				t.Errorf("expected non-empty message when reason is set, got empty")
			}
		})
	}
}

func TestReconcileSGLangSpecCondition(t *testing.T) {
	cases := []struct {
		name           string
		isvc           *inferencev1alpha1.InferenceService
		existingConds  []metav1.Condition
		wantTypeExists bool
		wantStatus     metav1.ConditionStatus
		wantReason     string
	}{
		{
			name: "non-sglang runtime removes condition",
			isvc: &inferencev1alpha1.InferenceService{
				Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: "vllm"},
				ObjectMeta: metav1.ObjectMeta{Name: "test", Generation: 1},
				Status: inferencev1alpha1.InferenceServiceStatus{
					Conditions: []metav1.Condition{{
						Type:   ConditionSGLangSpecValid,
						Status: metav1.ConditionFalse,
					}},
				},
			},
			wantTypeExists: false,
		},
		{
			name: "valid config with no prior condition does nothing",
			isvc: &inferencev1alpha1.InferenceService{
				Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: "sglang"},
				ObjectMeta: metav1.ObjectMeta{Name: "test", Generation: 1},
			},
			wantTypeExists: false,
		},
		{
			name: "valid config clears prior False condition",
			isvc: &inferencev1alpha1.InferenceService{
				Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: "sglang"},
				ObjectMeta: metav1.ObjectMeta{Name: "test", Generation: 1},
				Status: inferencev1alpha1.InferenceServiceStatus{
					Conditions: []metav1.Condition{{
						Type:   ConditionSGLangSpecValid,
						Status: metav1.ConditionFalse,
					}},
				},
			},
			existingConds: []metav1.Condition{{
				Type:   ConditionSGLangSpecValid,
				Status: metav1.ConditionFalse,
			}},
			wantTypeExists: true,
			wantStatus:     metav1.ConditionTrue,
			wantReason:     "ConfigValid",
		},
		{
			name: "invalid speculative config sets False condition",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "sglang",
					SGLangConfig: &inferencev1alpha1.SGLangConfig{
						Speculative: &inferencev1alpha1.SGLangSpeculativeConfig{
							Enabled: ptrBool(true),
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{Name: "test", Generation: 2},
			},
			wantTypeExists: true,
			wantStatus:     metav1.ConditionFalse,
			wantReason:     "SpeculativeMissingConfig",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &InferenceServiceReconciler{}
			r.reconcileSGLangSpecCondition(tc.isvc)

			cond := findCondition(tc.isvc.Status.Conditions, ConditionSGLangSpecValid)
			if tc.wantTypeExists {
				if cond == nil {
					t.Fatalf("expected condition %q to exist", ConditionSGLangSpecValid)
				}
				if cond.Status != tc.wantStatus {
					t.Errorf("status: got %q want %q", cond.Status, tc.wantStatus)
				}
				if cond.Reason != tc.wantReason {
					t.Errorf("reason: got %q want %q", cond.Reason, tc.wantReason)
				}
			} else {
				if cond != nil {
					t.Errorf("expected condition %q to not exist, got status=%q reason=%q", ConditionSGLangSpecValid, cond.Status, cond.Reason)
				}
			}
		})
	}
}

func findCondition(conds []metav1.Condition, ctype string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == ctype {
			return &conds[i]
		}
	}
	return nil
}
