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

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name     string
		v1       string
		v2       string
		expected int
	}{
		{"equal versions", "1.0.0", "1.0.0", 0},
		{"v1 less than v2 patch", "1.0.0", "1.0.1", -1},
		{"v1 greater than v2 patch", "1.0.1", "1.0.0", 1},
		{"v1 less than v2 minor", "1.0.0", "1.1.0", -1},
		{"v1 greater than v2 minor", "1.1.0", "1.0.0", 1},
		{"v1 less than v2 major", "1.0.0", "2.0.0", -1},
		{"v1 greater than v2 major", "2.0.0", "1.0.0", 1},
		{"double digit patch comparison", "0.4.9", "0.4.10", -1},
		{"double digit patch comparison reverse", "0.4.10", "0.4.9", 1},
		{"triple digit patch", "1.0.99", "1.0.100", -1},
		{"with v prefix", "v1.0.0", "v1.0.1", -1},
		{"mixed v prefix", "v1.0.0", "1.0.1", -1},
		{"double digit minor", "1.9.0", "1.10.0", -1},
		{"equal with v prefix", "v0.4.10", "0.4.10", 0},
		{"real world case", "0.4.10", "0.4.9", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := compareVersions(tt.v1, tt.v2)
			if result != tt.expected {
				t.Errorf("compareVersions(%q, %q) = %d, expected %d", tt.v1, tt.v2, result, tt.expected)
			}
		})
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		expected []int
	}{
		{"simple version", "1.2.3", []int{1, 2, 3}},
		{"with v prefix", "v1.2.3", []int{1, 2, 3}},
		{"double digits", "1.10.100", []int{1, 10, 100}},
		{"two parts", "1.2", []int{1, 2}},
		{"single part", "1", []int{1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseVersion(tt.version)
			if len(result) != len(tt.expected) {
				t.Errorf("parseVersion(%q) returned %d parts, expected %d", tt.version, len(result), len(tt.expected))
				return
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("parseVersion(%q)[%d] = %d, expected %d", tt.version, i, v, tt.expected[i])
				}
			}
		})
	}
}
