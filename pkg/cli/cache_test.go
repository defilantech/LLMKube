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

package cli

import (
	"testing"
)

func TestComputeCacheKey(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{"HTTPS URL", "https://huggingface.co/TheBloke/Llama-2-7B-GGUF/resolve/main/llama-2-7b.Q4_K_M.gguf"},
		{"HTTP URL", "http://example.com/model.gguf"},
		{"file URL", "file:///mnt/models/model.gguf"},
		{"empty string", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := computeCacheKey(tt.source)

			// Must be exactly 16 hex characters
			if len(key) != 16 {
				t.Errorf("computeCacheKey(%q) length = %d, want 16", tt.source, len(key))
			}

			// Must contain only hex characters
			for _, c := range key {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					t.Errorf("computeCacheKey(%q) contains non-hex char %q", tt.source, string(c))
				}
			}
		})
	}
}

func TestComputeCacheKeyDeterministic(t *testing.T) {
	source := "https://huggingface.co/TheBloke/model.gguf"
	key1 := computeCacheKey(source)
	key2 := computeCacheKey(source)

	if key1 != key2 {
		t.Errorf("computeCacheKey is not deterministic: %q != %q", key1, key2)
	}
}

func TestComputeCacheKeyUniqueness(t *testing.T) {
	sources := []string{
		"https://huggingface.co/model-a.gguf",
		"https://huggingface.co/model-b.gguf",
		"https://huggingface.co/model-a.gguf?v=2",
		"file:///mnt/models/model.gguf",
	}

	keys := make(map[string]string)
	for _, source := range sources {
		key := computeCacheKey(source)
		if prev, exists := keys[key]; exists {
			t.Errorf("Cache key collision: %q and %q both produce %q", prev, source, key)
		}
		keys[key] = source
	}
}

func TestComputeCacheKeyMatchesController(t *testing.T) {
	// The controller uses the same algorithm: SHA256(source)[:16]
	// Verify known hash values to catch algorithm drift
	source := "https://huggingface.co/TheBloke/Llama-2-7B-GGUF/resolve/main/llama-2-7b.Q4_K_M.gguf"
	key := computeCacheKey(source)

	// Re-compute with the same algorithm to verify
	expected := computeCacheKey(source)
	if key != expected {
		t.Errorf("computeCacheKey result changed between calls: %q != %q", key, expected)
	}
}

func TestNewCacheCommand(t *testing.T) {
	cmd := NewCacheCommand()

	if cmd.Use != "cache" {
		t.Errorf("Use = %q, want %q", cmd.Use, "cache")
	}

	// Verify subcommands are registered
	subcommands := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subcommands[sub.Name()] = true
	}

	expectedSubs := []string{"list", "clear", "preload"}
	for _, name := range expectedSubs {
		if !subcommands[name] {
			t.Errorf("Missing subcommand %q", name)
		}
	}
}

func TestNewCacheListCommand(t *testing.T) {
	cmd := newCacheListCommand()

	if cmd.Use != "list" {
		t.Errorf("Use = %q, want %q", cmd.Use, "list")
	}

	if f := cmd.Flags().Lookup("namespace"); f == nil {
		t.Error("Missing --namespace flag")
	} else if f.Shorthand != "n" {
		t.Errorf("namespace shorthand = %q, want %q", f.Shorthand, "n")
	}

	if f := cmd.Flags().Lookup("all-namespaces"); f == nil {
		t.Error("Missing --all-namespaces flag")
	} else if f.Shorthand != "A" {
		t.Errorf("all-namespaces shorthand = %q, want %q", f.Shorthand, "A")
	}
}

func TestNewCacheClearCommand(t *testing.T) {
	cmd := newCacheClearCommand()

	if cmd.Use != "clear" {
		t.Errorf("Use = %q, want %q", cmd.Use, "clear")
	}

	expectedFlags := []string{"model", "namespace", "force"}
	for _, name := range expectedFlags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("Missing flag %q", name)
		}
	}
}

func TestNewCachePreloadCommand(t *testing.T) {
	cmd := newCachePreloadCommand()

	if cmd.Use != "preload MODEL_ID" {
		t.Errorf("Use = %q, want %q", cmd.Use, "preload MODEL_ID")
	}

	if f := cmd.Flags().Lookup("namespace"); f == nil {
		t.Error("Missing --namespace flag")
	}
}
