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

func TestNewDeleteCommand(t *testing.T) {
	cmd := NewDeleteCommand()

	if cmd.Use != "delete [NAME]" {
		t.Errorf("Use = %q, want %q", cmd.Use, "delete [NAME]")
	}

	expectedAliases := []string{"del", "rm"}
	if len(cmd.Aliases) != len(expectedAliases) {
		t.Errorf("Aliases count = %d, want %d", len(cmd.Aliases), len(expectedAliases))
	}
	for i, alias := range expectedAliases {
		if i < len(cmd.Aliases) && cmd.Aliases[i] != alias {
			t.Errorf("Aliases[%d] = %q, want %q", i, cmd.Aliases[i], alias)
		}
	}

	if f := cmd.Flags().Lookup("namespace"); f == nil {
		t.Error("Missing --namespace flag")
	} else {
		if f.Shorthand != "n" {
			t.Errorf("namespace shorthand = %q, want %q", f.Shorthand, "n")
		}
		if f.DefValue != testDefaultNamespace {
			t.Errorf("namespace default = %q, want %q", f.DefValue, testDefaultNamespace)
		}
	}
}

func TestDeleteCommandRequiresArg(t *testing.T) {
	cmd := NewDeleteCommand()
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("Expected error when no argument provided")
	}
}
