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

package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestComputeLength(t *testing.T) {
	got := Compute("https://huggingface.co/model.gguf")
	if len(got) != 16 {
		t.Errorf("Compute length = %d, want 16", len(got))
	}
}

func TestComputeHexChars(t *testing.T) {
	got := Compute("https://huggingface.co/model.gguf")
	for i, c := range got {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("Compute char at %d = %q, want hex", i, string(c))
		}
	}
}

func TestComputeDeterministic(t *testing.T) {
	a := Compute("https://huggingface.co/model.gguf")
	b := Compute("https://huggingface.co/model.gguf")
	if a != b {
		t.Errorf("Compute not deterministic: %q != %q", a, b)
	}
}

func TestComputeMatchesSHA256Prefix(t *testing.T) {
	source := "https://huggingface.co/TheBloke/Llama-2-7B-GGUF/resolve/main/llama-2-7b.Q4_K_M.gguf"
	want := sha256.Sum256([]byte(source))
	wantStr := hex.EncodeToString(want[:])[:16]
	got := Compute(source)
	if got != wantStr {
		t.Errorf("Compute = %q, want %q", got, wantStr)
	}
}

func TestComputeEmptySource(t *testing.T) {
	got := Compute("")
	if len(got) != 16 {
		t.Errorf("Compute empty length = %d, want 16", len(got))
	}
	// Empty string still produces a valid 16-char hex digest.
	want := sha256.Sum256([]byte(""))
	wantStr := hex.EncodeToString(want[:])[:16]
	if got != wantStr {
		t.Errorf("Compute empty = %q, want %q", got, wantStr)
	}
}

func TestEffectiveKeyNilModel(t *testing.T) {
	if got := EffectiveKey(nil); got != "" {
		t.Errorf("EffectiveKey(nil) = %q, want empty", got)
	}
}

func TestEffectiveKeyStatusCacheKeyWins(t *testing.T) {
	model := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://huggingface.co/example/model.gguf",
		},
		Status: inferencev1alpha1.ModelStatus{
			CacheKey: "status-wins",
		},
	}
	got := EffectiveKey(model)
	if got != "status-wins" {
		t.Errorf("EffectiveKey with Status.CacheKey set = %q, want status-wins", got)
	}
}

func TestEffectiveKeyMultiFileNonMetal(t *testing.T) {
	model := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://example/model",
			Files:  []string{"model.gguf", "mmproj.gguf"},
		},
	}
	want := Compute("hf://example/model")
	got := EffectiveKey(model)
	if got != want {
		t.Errorf("EffectiveKey multi-file non-metal = %q, want %q", got, want)
	}
}

func TestEffectiveKeyMetalMultiFile(t *testing.T) {
	model := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://example/model",
			Files:  []string{"model.gguf"},
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: "metal",
			},
		},
	}
	got := EffectiveKey(model)
	if got != "" {
		t.Errorf("EffectiveKey metal multi-file = %q, want empty", got)
	}
}

func TestEffectiveKeySingleFile(t *testing.T) {
	model := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://huggingface.co/example/model.gguf",
		},
	}
	got := EffectiveKey(model)
	if got != "" {
		t.Errorf("EffectiveKey single-file = %q, want empty", got)
	}
}
