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

package agent

import (
	"encoding/json"
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func TestUpstreamURLForRepo(t *testing.T) {
	cases := []struct {
		repo string
		want string
	}{
		{"defilantech/LLMKube", "https://github.com/defilantech/LLMKube.git"},
		{"  defilantech/LLMKube  ", "https://github.com/defilantech/LLMKube.git"},
		{"", ""},
		{"   ", ""},
		// malformed / unsafe slugs derive no URL (caller falls back to fork HEAD).
		{"defilantech", ""},               // no slash
		{"defilantech/LLMKube/extra", ""}, // extra path segment
		{"../../etc/passwd", ""},          // path traversal
		{"defilan tech/LLMKube", ""},      // whitespace
	}
	for _, tc := range cases {
		if got := upstreamURLForRepo(tc.repo); got != tc.want {
			t.Errorf("upstreamURLForRepo(%q) = %q, want %q", tc.repo, got, tc.want)
		}
	}
}

func TestBaseBranchOrDefault(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "main"},
		{"   ", "main"},
		{"main", "main"},
		{"release-1.2", "release-1.2"},
		{" develop ", "develop"},
	}
	for _, tc := range cases {
		if got := baseBranchOrDefault(tc.in); got != tc.want {
			t.Errorf("baseBranchOrDefault(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBuildDeterministicArgs_ThreadsUpstreamURL verifies every deterministic
// gate run carries the canonical upstream repo URL, so the gate's bite check
// fetches baseBranch from the true upstream and reverts to the real
// merge-base instead of the fork's possibly-stale ref tip (#1259). A task
// with no repo slug (freeform tasks) must yield an absent/empty upstreamURL
// so the shell script's origin-fallback path stays intact.
func TestBuildDeterministicArgs_ThreadsUpstreamURL(t *testing.T) {
	cases := []struct {
		name string
		repo string
		want string
	}{
		{"repo set", "defilantech/LLMKube", "https://github.com/defilantech/LLMKube.git"},
		{"repo empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &foremanv1alpha1.AgenticTask{
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
					Payload: foremanv1alpha1.AgenticTaskPayload{
						Repo:  tc.repo,
						Issue: 1259,
					},
				},
			}
			raw := buildDeterministicArgs(task, "foreman/issue-1259", "https://github.com/Defilan/LLMKube.git")
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal args: %v", err)
			}
			got, _ := m["upstreamURL"].(string)
			if got != tc.want {
				t.Errorf("args[upstreamURL] = %q, want %q", got, tc.want)
			}
			if tc.want != "" && got != upstreamURLForRepo(tc.repo) {
				t.Errorf("args[upstreamURL] = %q, want it to equal upstreamURLForRepo(%q) = %q",
					got, tc.repo, upstreamURLForRepo(tc.repo))
			}
		})
	}
}

// TestBuildDeterministicArgs_ThreadsBaseBranch verifies the verify-gate args
// carry the task's base branch (defaulting to main), so the bite check diffs
// against the same base the coder branched from (#813).
func TestBuildDeterministicArgs_ThreadsBaseBranch(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		{"explicit base", "release-2", "release-2"},
		{"default base", "", "main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &foremanv1alpha1.AgenticTask{
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
					Payload: foremanv1alpha1.AgenticTaskPayload{
						Repo:       "defilantech/LLMKube",
						Issue:      813,
						BaseBranch: tc.base,
					},
				},
			}
			raw := buildDeterministicArgs(task, "foreman/issue-813", "https://github.com/Defilan/LLMKube.git")
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal args: %v", err)
			}
			if got, _ := m["baseBranch"].(string); got != tc.want {
				t.Errorf("args[baseBranch] = %q, want %q", got, tc.want)
			}
		})
	}
}
