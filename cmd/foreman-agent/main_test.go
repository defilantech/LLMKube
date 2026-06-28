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

package main

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestClampInt32(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int32
	}{
		{"negative_clamps_to_zero", -1, 0},
		{"min_int_clamps_to_zero", math.MinInt32, 0},
		{"zero_stays_zero", 0, 0},
		{"small_positive_passes_through", 47, 47},
		{"context_size_passes_through", 131072, 131072},
		{"exactly_max_int32_passes_through", math.MaxInt32, math.MaxInt32},
		{"over_max_int32_saturates", int(math.MaxInt32) + 1, math.MaxInt32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampInt32(tc.in)
			if got != tc.want {
				t.Errorf("clampInt32(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already_clean", "m5-max", "m5-max"},
		{"uppercase_lowercased", "M5-MAX", "m5-max"},
		{"dots_become_hyphens", "m5.max.local", "m5-max-local"},
		{"underscores_become_hyphens", "m5_max", "m5-max"},
		{"runs_of_invalid_collapse", "m5...max", "m5-max"},
		{"leading_hyphen_trimmed", "-m5-max", "m5-max"},
		{"trailing_hyphen_trimmed", "m5-max-", "m5-max"},
		{"both_ends_trimmed", "---m5-max---", "m5-max"},
		{"empty_falls_back_to_fleetnode", "", "fleetnode"},
		{"only_invalid_falls_back", "...", "fleetnode"},
		{"too_long_truncated_to_63", strings.Repeat("a", 100), strings.Repeat("a", 63)},
		{"hostname_with_macos_local_suffix", "Christophers-MacBook-Pro.local", "christophers-macbook-pro-local"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeName(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > 63 {
				t.Errorf("sanitizeName returned %d chars; DNS-1123 cap is 63", len(got))
			}
		})
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty_returns_nil", "", nil},
		{"single_value", "worker", []string{"worker"}},
		{"two_values", "worker,verifier", []string{"worker", "verifier"}},
		{"three_values_with_whitespace", "worker, verifier , gate", []string{"worker", "verifier", "gate"}},
		{"empty_entries_skipped", "worker,,verifier,", []string{"worker", "verifier"}},
		{"only_separators_returns_nil", ",,,", nil},
		{"whitespace_only_entries_skipped", "worker,   ,verifier", []string{"worker", "verifier"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitCSV(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitCSV(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseKeyValueCSV(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]string
	}{
		{"empty_returns_nil", "", nil},
		{"single_pair", "coder-pool=amd", map[string]string{"coder-pool": "amd"}},
		{"two_pairs", "coder-pool=amd,zone=lab", map[string]string{"coder-pool": "amd", "zone": "lab"}},
		{"whitespace_trimmed", " coder-pool = amd , zone = lab ", map[string]string{"coder-pool": "amd", "zone": "lab"}},
		{"trailing_comma_tolerated", "coder-pool=amd,", map[string]string{"coder-pool": "amd"}},
		{"malformed_segment_skipped", "coder-pool=amd,garbage", map[string]string{"coder-pool": "amd"}},
		{"empty_key_skipped", "=amd,zone=lab", map[string]string{"zone": "lab"}},
		{"empty_value_allowed", "coder-pool=", map[string]string{"coder-pool": ""}},
		{"only_separators_returns_nil", ",,,", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseKeyValueCSV(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseKeyValueCSV(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
