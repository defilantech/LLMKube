/*
Copyright 2026.

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

package agent

import (
	"slices"
	"strings"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/controller"
)

func TestBuildLlamaServerArgs_EmbeddingMode(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{ContextSize: 4096, Mode: "embedding"})
	if !hasFlag(args, "--embedding") {
		t.Errorf("--embedding must be set for embedding mode (full args: %v)", args)
	}
	if hasFlag(args, "--pooling") {
		t.Errorf("embedding must leave pooling to model metadata (full args: %v)", args)
	}
	if hasFlag(args, "--reranking") {
		t.Errorf("--reranking must not be set for embedding mode (full args: %v)", args)
	}
}

func TestBuildLlamaServerArgs_RerankMode(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{ContextSize: 4096, Mode: "rerank"})
	if !hasFlag(args, "--reranking") || !hasFlag(args, "--embedding") {
		t.Errorf("rerank mode must set both --reranking and --embedding (full args: %v)", args)
	}
	if got := flagValue(args, "--pooling"); got != "rank" {
		t.Errorf("--pooling = %q, want %q (full args: %v)", got, "rank", args)
	}
}

func TestBuildLlamaServerArgs_ModeDoesNotOverrideExtraArgs(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{
		ContextSize: 4096,
		Mode:        "embedding",
		ExtraArgs:   []string{"--pooling", "cls"},
	})
	// Mode must not inject its own --pooling when the user already set one.
	if got := flagValue(args, "--pooling"); got != "cls" {
		t.Errorf("--pooling = %q, want user value %q (full args: %v)", got, "cls", args)
	}
}

func TestBuildLlamaServerArgs_ChatModeAddsNoModeFlags(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{ContextSize: 4096, Mode: "chat"})
	if hasFlag(args, "--embedding") || hasFlag(args, "--reranking") {
		t.Errorf("chat mode must add no serving-mode flags (full args: %v)", args)
	}
}

func TestBuildLlamaServerArgs_EmbeddingModeDisablesCacheRam(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{ContextSize: 4096, Mode: "embedding"})
	if got := flagValue(args, "--cache-ram"); got != "0" {
		t.Errorf("--cache-ram = %q, want %q (full args: %v)", got, "0", args)
	}
}

func TestBuildLlamaServerArgs_RerankModeDisablesCacheRam(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{ContextSize: 4096, Mode: "rerank"})
	if got := flagValue(args, "--cache-ram"); got != "0" {
		t.Errorf("--cache-ram = %q, want %q (full args: %v)", got, "0", args)
	}
}

func TestBuildLlamaServerArgs_CacheRamNotOverriddenByExtraArgs(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{
		ContextSize: 4096,
		Mode:        "embedding",
		ExtraArgs:   []string{"--cache-ram", "512"},
	})
	if got := flagValue(args, "--cache-ram"); got != "512" {
		t.Errorf("--cache-ram = %q, want user value %q (full args: %v)", got, "512", args)
	}
}

// Both runtimes must leave embedding pooling to GGUF metadata unless explicitly overridden.
func TestBuildLlamaServerArgsModeParity(t *testing.T) {
	for _, tc := range []struct {
		name, mode  string
		extra, want []string
	}{
		{"embedding", "embedding", nil, []string{"--embedding", "--cache-ram", "0"}},
		{"embedding mean", "embedding", []string{"--pooling", "mean"},
			[]string{"--embedding", "--cache-ram", "0", "--pooling", "mean"}},
		{"embedding last", "embedding", []string{"--pooling", "last"},
			[]string{"--embedding", "--cache-ram", "0", "--pooling", "last"}},
		{"embedding equals override", "embedding", []string{"--pooling=cls"},
			[]string{"--embedding", "--cache-ram", "0", "--pooling=cls"}},
		{"embedding cache override", "embedding", []string{"--cache-ram", "512"},
			[]string{"--embedding", "--cache-ram", "512"}},
		{"rerank", "rerank", nil, []string{"--reranking", "--embedding", "--pooling", "rank", "--cache-ram", "0"}},
		{"rerank override", "rerank", []string{"--pooling", "cls"},
			[]string{"--reranking", "--embedding", "--cache-ram", "0", "--pooling", "cls"}},
		{"chat", "chat", nil, nil},
		{"unset", "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Mode: tc.mode, ExtraArgs: tc.extra},
			}
			model := &inferencev1alpha1.Model{}
			cfg := buildExecutorConfig(isvc, model, executorBaseConfig{})
			backend := &controller.LlamaCppBackend{}
			for name, args := range map[string][]string{
				"controller": backend.BuildArgs(isvc, model, "/m.gguf", "", 8080),
				"agent":      buildLlamaServerArgs("/m.gguf", 8080, cfg),
			} {
				t.Run(name, func(t *testing.T) {
					var got []string
					for i := 0; i < len(args); i++ {
						flag, _, inline := strings.Cut(args[i], "=")
						switch flag {
						case "--embedding", "--reranking":
							got = append(got, args[i])
						case "--pooling", "--cache-ram":
							got = append(got, args[i])
							if !inline && i+1 < len(args) {
								i++
								got = append(got, args[i])
							}
						}
					}
					if !slices.Equal(got, tc.want) {
						t.Errorf("mode args = %v, want %v", got, tc.want)
					}
				})
			}
		})
	}
}
