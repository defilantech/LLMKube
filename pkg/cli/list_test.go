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

func TestNewListCommand(t *testing.T) {
	cmd := NewListCommand()

	if cmd.Use != "list [models|services]" {
		t.Errorf("Use = %q, want %q", cmd.Use, "list [models|services]")
	}

	expectedAliases := []string{"ls"}
	if len(cmd.Aliases) != len(expectedAliases) {
		t.Errorf("Aliases count = %d, want %d", len(cmd.Aliases), len(expectedAliases))
	}
	if len(cmd.Aliases) > 0 && cmd.Aliases[0] != "ls" {
		t.Errorf("Aliases[0] = %q, want %q", cmd.Aliases[0], "ls")
	}

	if f := cmd.Flags().Lookup("namespace"); f == nil {
		t.Error("Missing --namespace flag")
	} else if f.DefValue != testDefaultNamespace {
		t.Errorf("namespace default = %q, want %q", f.DefValue, testDefaultNamespace)
	}
}

func TestRunListResourceRouting(t *testing.T) {
	// Only test the unknown resource type path, since valid types require a K8s client
	tests := []struct {
		resource  string
		wantError bool
	}{
		{"unknown", true},
		{"pods", true},
		{"", true},
	}

	for _, tt := range tests {
		t.Run(tt.resource, func(t *testing.T) {
			opts := &listOptions{
				namespace: testDefaultNamespace,
				resource:  tt.resource,
			}
			err := runList(opts)
			if tt.wantError && err == nil {
				t.Errorf("runList(resource=%q) = nil, want error", tt.resource)
			}
		})
	}
}
