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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestSanitizeModelFilename(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"already safe", "Gemma-4-E4B-It", "Gemma-4-E4B-It"},
		{"spaces", "Llama 3.1 Instruct", "Llama-3.1-Instruct"},
		{"slash", "Llama 3.1/Instruct", "Llama-3.1-Instruct"},
		{"colon", "Qwen3:30B", "Qwen3-30B"},
		{"underscore preserved", "model_v2", "model_v2"},
		{"dot preserved", "model.v2.gguf", "model.v2.gguf"},
		{"unicode replaced", "café", "caf"},
		{"all separators", "///", ""},
		{"all dots", "...", ""},
		{"path traversal", "..", ""},
		{"leading dot", ".hidden", "hidden"},
		{"trailing dash", "name-", "name"},
		{"runs of separators", "a   b", "a-b"},
		{"runs of dashes from mixed", "a / b", "a-b"},
		{"only dashes", "----", ""},
		{"long mixed", "Q4_K_M / Llama-3.1 (8B)", "Q4_K_M-Llama-3.1-8B"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeModelFilename(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeModelFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalModelBasename(t *testing.T) {
	cases := []struct {
		name      string
		modelName string
		gguf      *inferencev1alpha1.GGUFMetadata
		want      string
	}{
		{
			name:      "uses GGUF metadata name when available",
			modelName: "gemma",
			gguf:      &inferencev1alpha1.GGUFMetadata{ModelName: "Gemma-4-E4B-It"},
			want:      "Gemma-4-E4B-It.gguf",
		},
		{
			name:      "falls back to Model.Name when GGUF status is nil",
			modelName: "gemma-4-e4b",
			gguf:      nil,
			want:      "gemma-4-e4b.gguf",
		},
		{
			name:      "falls back to Model.Name when GGUF name is empty",
			modelName: "gemma-4-e4b",
			gguf:      &inferencev1alpha1.GGUFMetadata{ModelName: ""},
			want:      "gemma-4-e4b.gguf",
		},
		{
			name:      "sanitizes GGUF name with unsafe characters",
			modelName: "fallback",
			gguf:      &inferencev1alpha1.GGUFMetadata{ModelName: "Llama 3.1/Instruct"},
			want:      "Llama-3.1-Instruct.gguf",
		},
		{
			name:      "falls back to Model.Name when GGUF name sanitizes to empty",
			modelName: "fallback",
			gguf:      &inferencev1alpha1.GGUFMetadata{ModelName: "///"},
			want:      "fallback.gguf",
		},
		{
			name:      "falls back to literal model when both empty",
			modelName: "",
			gguf:      &inferencev1alpha1.GGUFMetadata{ModelName: ""},
			want:      "model.gguf",
		},
		{
			name:      "rejects path traversal in GGUF name",
			modelName: "safe",
			gguf:      &inferencev1alpha1.GGUFMetadata{ModelName: ".."},
			want:      "safe.gguf",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: tc.modelName},
			}
			model.Status.GGUF = tc.gguf
			got := canonicalModelBasename(model)
			if got != tc.want {
				t.Errorf("canonicalModelBasename(modelName=%q, gguf=%+v) = %q, want %q",
					tc.modelName, tc.gguf, got, tc.want)
			}
		})
	}
}

func TestCanonicalModelBasenameNoGGUFStatus(t *testing.T) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model"},
	}
	if got, want := canonicalModelBasename(model), "my-model.gguf"; got != want {
		t.Errorf("canonicalModelBasename = %q, want %q", got, want)
	}
}
