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

// boundedLevenshtein returns the Levenshtein edit distance between a and b,
// giving up early once the distance provably exceeds maxDist. When the true
// distance is greater than maxDist it returns maxDist+1; callers only need
// "over budget", not the exact value. The two-row DP abandons a row as soon
// as its minimum exceeds maxDist, so clearly-mismatched inputs are rejected
// in O(len x maxDist) instead of O(len^2). maxDist must be >= 0; a negative
// cap still reports over-budget for unequal inputs but the returned value is
// meaningless as a distance.
func boundedLevenshtein(a, b string, maxDist int) int {
	if a == b {
		return 0
	}
	ra, rb := []rune(a), []rune(b)
	// The distance is at least the rune-length difference.
	diff := len(ra) - len(rb)
	if diff < 0 {
		diff = -diff
	}
	if diff > maxDist {
		return maxDist + 1
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := 0; j <= len(rb); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		rowMin := curr[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			m := prev[j] + 1               // deletion
			if v := curr[j-1] + 1; v < m { // insertion
				m = v
			}
			if v := prev[j-1] + cost; v < m { // substitution
				m = v
			}
			curr[j] = m
			if m < rowMin {
				rowMin = m
			}
		}
		if rowMin > maxDist {
			return maxDist + 1
		}
		prev, curr = curr, prev
	}
	if prev[len(rb)] > maxDist {
		return maxDist + 1
	}
	return prev[len(rb)]
}
