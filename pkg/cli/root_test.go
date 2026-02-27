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

func TestNewRootCommand(t *testing.T) {
	cmd := NewRootCommand()

	if cmd.Use != "llmkube" {
		t.Errorf("Use = %q, want %q", cmd.Use, "llmkube")
	}

	if !cmd.SilenceUsage {
		t.Error("SilenceUsage should be true")
	}

	// Verify all expected subcommands are registered
	expectedSubcommands := map[string]bool{
		"deploy":    false,
		"list":      false,
		"delete":    false,
		"status":    false,
		"queue":     false,
		"version":   false,
		"catalog":   false,
		"benchmark": false,
		"cache":     false,
		"inspect":   false,
		"license":   false,
	}

	for _, sub := range cmd.Commands() {
		if _, expected := expectedSubcommands[sub.Name()]; expected {
			expectedSubcommands[sub.Name()] = true
		}
	}

	for name, found := range expectedSubcommands {
		if !found {
			t.Errorf("Missing subcommand %q", name)
		}
	}
}
