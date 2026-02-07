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
	"time"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{"seconds", 30 * time.Second, "30s"},
		{"minutes", 5 * time.Minute, "5m"},
		{"hours", 3 * time.Hour, "3h"},
		{"days", 48 * time.Hour, "2d"},
		{"just under a minute", 59 * time.Second, "59s"},
		{"just under an hour", 59 * time.Minute, "59m"},
		{"just under a day", 23 * time.Hour, "23h"},
		{"zero", 0, "0s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatDuration(tt.duration)
			if result != tt.expected {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.duration, result, tt.expected)
			}
		})
	}
}

func TestNewQueueCommand(t *testing.T) {
	cmd := NewQueueCommand()

	if cmd.Use != "queue" {
		t.Errorf("Use = %q, want %q", cmd.Use, "queue")
	}

	if f := cmd.Flags().Lookup("all-namespaces"); f == nil {
		t.Error("Missing --all-namespaces flag")
	} else if f.Shorthand != "A" {
		t.Errorf("all-namespaces shorthand = %q, want %q", f.Shorthand, "A")
	}

	if f := cmd.Flags().Lookup("namespace"); f == nil {
		t.Error("Missing --namespace flag")
	} else if f.DefValue != testDefaultNamespace {
		t.Errorf("namespace default = %q, want %q", f.DefValue, testDefaultNamespace)
	}
}
