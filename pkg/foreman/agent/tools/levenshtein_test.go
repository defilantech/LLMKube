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

package tools

import "testing"

func TestBoundedLevenshtein(t *testing.T) {
	cases := []struct {
		name    string
		a, b    string
		maxDist int
		want    int
	}{
		{"both empty", "", "", 5, 0},
		{"identical", "abc", "abc", 0, 0},
		{"classic kitten/sitting", "kitten", "sitting", 10, 3},
		{"single deletion", "userCount", "usrCount", 4, 1},
		{"over cap returns cap+1", "abc", "xyz", 1, 2},
		{"length screen over cap", "abcdefgh", "a", 3, 4},
		{"exact at cap", "kitten", "sitting", 3, 3},
		{"unequal at cap 0", "abc", "abd", 0, 1},
		{"multibyte runes", "héllo", "hello", 2, 1},
		{"empty vs nonempty", "abc", "", 5, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := boundedLevenshtein(tc.a, tc.b, tc.maxDist); got != tc.want {
				t.Errorf("boundedLevenshtein(%q, %q, %d) = %d, want %d",
					tc.a, tc.b, tc.maxDist, got, tc.want)
			}
			// Distance is symmetric; the cap contract must hold both ways.
			if got := boundedLevenshtein(tc.b, tc.a, tc.maxDist); got != tc.want {
				t.Errorf("boundedLevenshtein(%q, %q, %d) = %d, want %d (symmetry)",
					tc.b, tc.a, tc.maxDist, got, tc.want)
			}
		})
	}
}
