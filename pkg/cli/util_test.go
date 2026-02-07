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

func TestFormatAge(t *testing.T) {
	tests := []struct {
		name     string
		age      time.Duration
		expected string
	}{
		{"seconds", 30 * time.Second, "30s"},
		{"minutes", 5 * time.Minute, "5m"},
		{"hours", 3 * time.Hour, "3h"},
		{"days", 48 * time.Hour, "2d"},
		{"just under a minute", 59 * time.Second, "59s"},
		{"just under an hour", 59 * time.Minute, "59m"},
		{"just under a day", 23 * time.Hour, "23h"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timestamp := time.Now().Add(-tt.age)
			result := formatAge(timestamp)
			if result != tt.expected {
				t.Errorf("formatAge(now - %v) = %q, want %q", tt.age, result, tt.expected)
			}
		})
	}
}
