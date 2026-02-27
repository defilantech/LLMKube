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
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/defilantech/llmkube/pkg/license"
)

func TestNewLicenseCommand(t *testing.T) {
	cmd := NewLicenseCommand()

	if cmd.Use != "license" {
		t.Errorf("Use = %q, want %q", cmd.Use, "license")
	}

	subcommands := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subcommands[sub.Name()] = true
	}

	if !subcommands["check"] {
		t.Error("Missing 'check' subcommand")
	}
	if !subcommands["list"] {
		t.Error("Missing 'list' subcommand")
	}
}

func TestRunLicenseCheck(t *testing.T) {
	tests := []struct {
		name     string
		modelID  string
		wantErr  bool
		wantStrs []string
		dontWant []string
	}{
		{
			name:     "apache model",
			modelID:  "mistral-7b",
			wantStrs: []string{"Apache License 2.0", "Commercial Use:", "Yes", "No special restrictions"},
		},
		{
			name:     "llama model with restrictions",
			modelID:  "llama-3.1-8b",
			wantStrs: []string{"Llama 3.1 Community", "700M monthly active users"},
		},
		{
			name:     "MIT model",
			modelID:  "phi-4-mini",
			wantStrs: []string{"MIT License", "Commercial Use:", "Yes"},
		},
		{
			name:    "nonexistent model",
			modelID: "nonexistent-model-xyz",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := os.Stdout
			r, w, _ := os.Pipe()
			os.Stdout = w

			err := runLicenseCheck(tt.modelID)

			_ = w.Close()
			os.Stdout = old

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var buf bytes.Buffer
			_, _ = buf.ReadFrom(r)
			output := buf.String()

			for _, s := range tt.wantStrs {
				if !strings.Contains(output, s) {
					t.Errorf("output missing %q\nGot: %s", s, output)
				}
			}
		})
	}
}

func TestRunLicenseList(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runLicenseList()

	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "ID") {
		t.Error("output should contain header")
	}
	if !strings.Contains(output, "apache-2.0") {
		t.Error("output should contain apache-2.0")
	}
	if !strings.Contains(output, "mit") {
		t.Error("output should contain mit")
	}
	if !strings.Contains(output, "gemma") {
		t.Error("output should contain gemma")
	}
}

func TestAllModelsHaveKnownLicense(t *testing.T) {
	catalog, err := LoadCatalog()
	if err != nil {
		t.Fatalf("Failed to load catalog: %v", err)
	}

	for modelID, model := range catalog.Models {
		t.Run(modelID, func(t *testing.T) {
			if model.License == "" {
				t.Errorf("Model '%s' has no license", modelID)
				return
			}
			if license.Get(model.License) == nil {
				t.Errorf("Model '%s' has unknown license: %s", modelID, model.License)
			}
		})
	}
}
