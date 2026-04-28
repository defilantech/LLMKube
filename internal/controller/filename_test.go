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
	"os"
	"path/filepath"
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
		// "/-/" forces the main loop to emit "---" because the literal "-"
		// is in the allowed set and clears the prevDash flag, which the
		// post-trim collapse loop must then reduce back to a single dash.
		{"collapses post-trim double dash", "a/-/b", "a-b"},
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

// TestFindCachedModelFilePrefersCanonical exercises the inner-loop branch in
// findCachedModelFile: when both `model.gguf` (legacy) and a non-legacy GGUF
// exist in the same directory, the non-legacy one wins regardless of sort
// order. The non-legacy filename here ("qwen.gguf") sorts AFTER "model.gguf",
// so the helper must walk past the legacy match.
func TestFindCachedModelFilePrefersCanonical(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "model.gguf")
	canonical := filepath.Join(dir, "qwen.gguf")
	if err := os.WriteFile(legacy, []byte("legacy"), 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	if err := os.WriteFile(canonical, []byte("canonical"), 0o644); err != nil {
		t.Fatalf("write canonical: %v", err)
	}

	got, info, ok := findCachedModelFile(dir)
	if !ok {
		t.Fatal("findCachedModelFile reported no match")
	}
	if got != canonical {
		t.Errorf("got %q, want %q (non-legacy preference)", got, canonical)
	}
	if info == nil || info.Size() == 0 {
		t.Errorf("expected non-nil FileInfo with size > 0, got %+v", info)
	}
}

func TestFindCachedModelFileNoMatches(t *testing.T) {
	dir := t.TempDir()
	if _, _, ok := findCachedModelFile(dir); ok {
		t.Fatal("expected no match for empty directory")
	}
}

func TestMigrateModelFilenameNoOpWhenAlreadyCanonical(t *testing.T) {
	dir := t.TempDir()
	model := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "noop"}}
	current := filepath.Join(dir, canonicalModelBasename(model))
	if err := os.WriteFile(current, []byte("data"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	r := &ModelReconciler{}
	got, err := r.migrateModelFilename(current, dir, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != current {
		t.Errorf("got %q, want %q (no-op)", got, current)
	}
	if _, err := os.Stat(current); err != nil {
		t.Errorf("file should still exist at original path: %v", err)
	}
}

// TestMigrateModelFilenameSkipsWhenCanonicalExists covers the defensive
// collision branch: if both the source and the canonical destination exist
// (shouldn't happen during a normal reconcile, but possible under crash-recovery),
// we keep the source path unchanged and never overwrite the destination.
func TestMigrateModelFilenameSkipsWhenCanonicalExists(t *testing.T) {
	dir := t.TempDir()
	model := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "collide"}}

	current := filepath.Join(dir, "model.gguf")
	canonical := filepath.Join(dir, canonicalModelBasename(model))
	if err := os.WriteFile(current, []byte("legacy"), 0o644); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	if err := os.WriteFile(canonical, []byte("preexisting"), 0o644); err != nil {
		t.Fatalf("seed canonical: %v", err)
	}

	r := &ModelReconciler{}
	got, err := r.migrateModelFilename(current, dir, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != current {
		t.Errorf("got %q, want %q (no overwrite)", got, current)
	}
	// Both files should still exist, untouched.
	if data, _ := os.ReadFile(canonical); string(data) != "preexisting" {
		t.Errorf("canonical was overwritten: got %q, want %q", data, "preexisting")
	}
	if data, _ := os.ReadFile(current); string(data) != "legacy" {
		t.Errorf("current was modified: got %q, want %q", data, "legacy")
	}
}

func TestMigrateModelFilenameRenameError(t *testing.T) {
	dir := t.TempDir()
	model := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "rename-fail"}}
	// Source does not exist on disk → os.Rename returns ENOENT.
	current := filepath.Join(dir, "model.gguf")

	r := &ModelReconciler{}
	got, err := r.migrateModelFilename(current, dir, model)
	if err == nil {
		t.Fatal("expected rename error, got nil")
	}
	if got != current {
		t.Errorf("got %q, want %q (caller falls back to current on error)", got, current)
	}
}
