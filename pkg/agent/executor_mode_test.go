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

package agent

import "testing"

func TestBuildLlamaServerArgs_EmbeddingMode(t *testing.T) {
	args := buildLlamaServerArgs("/m.gguf", 8080, ExecutorConfig{ContextSize: 4096, Mode: "embedding"})
	if !hasFlag(args, "--embedding") {
		t.Errorf("--embedding must be set for embedding mode (full args: %v)", args)
	}
	if got := flagValue(args, "--pooling"); got != "last" {
		t.Errorf("--pooling = %q, want %q (full args: %v)", got, "last", args)
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
