/*
Copyright 2026.

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

// Unit tests for the Job-mode envelope preservation (#1077):
// coderJobResultToResult must carry the in-pod executor's promoted
// outcome (and paired fields) to the top level of the supervisor's
// Result instead of re-synthesizing a generic MODEL-NO-GO and
// discarding the model's extra.

import (
	"testing"
	"time"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func TestCoderJobResultToResult_PreservesPromotedOutcome(t *testing.T) {
	start := time.Now()

	t.Run("ALREADY-RESOLVED envelope survives the Job hop", func(t *testing.T) {
		cjr := CoderJobResult{
			Verdict: string(foremanv1alpha1.AgenticTaskVerdictNoGo),
			Summary: "already fixed by abc1234",
			Branch:  "foreman/wl/issue-970",
			JobName: "coder-job-1",
			ResultExtra: map[string]any{
				"outcome":    "ALREADY-RESOLVED",
				"resolvedBy": "abc1234",
				"turnCount":  7,
				"modelExtra": map[string]any{"outcome": "ALREADY-RESOLVED"},
			},
		}
		r := coderJobResultToResult("issue-fix", start, cjr)
		if r.Extra["outcome"] != "ALREADY-RESOLVED" {
			t.Errorf("outcome: want ALREADY-RESOLVED got %v", r.Extra["outcome"])
		}
		if r.Extra["resolvedBy"] != "abc1234" {
			t.Errorf("resolvedBy lost in the Job hop: %v", r.Extra["resolvedBy"])
		}
		if r.Extra["turnCount"] != 7 {
			t.Errorf("turnCount lost: %v", r.Extra["turnCount"])
		}
		// Supervisor stamps still present and authoritative.
		if r.Extra["executionMode"] != "Job" || r.Extra["jobName"] != "coder-job-1" {
			t.Errorf("supervisor fields missing: %v", r.Extra)
		}
		if r.Extra["intendedBranch"] != "foreman/wl/issue-970" {
			t.Errorf("intendedBranch missing: %v", r.Extra)
		}
	})

	t.Run("NEEDS-VERIFICATION envelope survives with unverified", func(t *testing.T) {
		cjr := CoderJobResult{
			Verdict: string(foremanv1alpha1.AgenticTaskVerdictNoGo),
			ResultExtra: map[string]any{
				"outcome":    "NEEDS-VERIFICATION",
				"unverified": []any{"claim A"},
			},
		}
		r := coderJobResultToResult("issue-fix", start, cjr)
		if r.Extra["outcome"] != "NEEDS-VERIFICATION" {
			t.Errorf("outcome: want NEEDS-VERIFICATION got %v", r.Extra["outcome"])
		}
		if r.Extra["unverified"] == nil {
			t.Errorf("unverified lost in the Job hop")
		}
	})

	t.Run("no envelope falls back to the legacy generic outcome", func(t *testing.T) {
		cjr := CoderJobResult{Verdict: string(foremanv1alpha1.AgenticTaskVerdictNoGo)}
		r := coderJobResultToResult("issue-fix", start, cjr)
		if r.Extra["outcome"] != "MODEL-NO-GO" {
			t.Errorf("fallback outcome: want MODEL-NO-GO got %v", r.Extra["outcome"])
		}
	})

	t.Run("envelope with empty outcome also falls back", func(t *testing.T) {
		cjr := CoderJobResult{
			Verdict:     string(foremanv1alpha1.AgenticTaskVerdictNoGo),
			ResultExtra: map[string]any{"outcome": "", "turnCount": 3},
		}
		r := coderJobResultToResult("issue-fix", start, cjr)
		if r.Extra["outcome"] != "MODEL-NO-GO" {
			t.Errorf("fallback outcome: want MODEL-NO-GO got %v", r.Extra["outcome"])
		}
		if r.Extra["turnCount"] != 3 {
			t.Errorf("envelope fields should still carry: %v", r.Extra)
		}
	})

	t.Run("GO keeps envelope fields and stamps supervisor keys over collisions", func(t *testing.T) {
		cjr := CoderJobResult{
			Verdict:   string(foremanv1alpha1.AgenticTaskVerdictGo),
			Branch:    "foreman/wl/issue-1",
			CommitSHA: "def5678",
			JobName:   "coder-job-2",
			ResultExtra: map[string]any{
				"outcome":   "",
				"turnCount": 12,
				"branch":    "stale-in-pod-branch",
			},
		}
		r := coderJobResultToResult("issue-fix", start, cjr)
		if r.Extra["branch"] != "foreman/wl/issue-1" {
			t.Errorf("supervisor branch must win over envelope: %v", r.Extra["branch"])
		}
		if r.Extra["turnCount"] != 12 {
			t.Errorf("turnCount lost on GO: %v", r.Extra)
		}
		if r.Extra["commitSHA"] != "def5678" {
			t.Errorf("commitSHA missing: %v", r.Extra)
		}
	})
}
