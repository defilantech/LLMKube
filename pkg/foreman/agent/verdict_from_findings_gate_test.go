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
	"testing"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// vffExtra builds a findings extra map with one finding of the given severity,
// file, and line (reusing the shape reviewer.ParseFindings expects).
// nolint:unparam // file is parameterized for clarity at call sites even though existing tests all use "a.go"
func vffExtra(severity, file string, line int) map[string]any {
	return map[string]any{
		"findings": []any{
			map[string]any{"severity": severity, "area": "scope", "message": "m", "file": file, "line": line},
		},
	}
}

func vffChanged(fix map[string]map[int]bool) func(string) map[int]bool {
	return func(f string) map[int]bool { return fix[f] }
}

func TestVerdictFromFindings_GroundedBlockerPromotes(t *testing.T) {
	extra := vffExtra("blocker", "a.go", 10)
	got := enforceReviewerVerdictFromFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictGo,
		vffChanged(map[string]map[int]bool{"a.go": {10: true}}))
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("GO with a grounded blocker must promote to NO-GO, got %s", got)
	}
	if extra["verdictPromotedFromFindings"] != true {
		t.Fatal("expected verdictPromotedFromFindings=true")
	}
	if extra["promotingFindings"] == nil {
		t.Fatal("expected promotingFindings archived")
	}
}

func TestVerdictFromFindings_GroundedMajorPromotes(t *testing.T) {
	extra := vffExtra("major", "a.go", 10)
	got := enforceReviewerVerdictFromFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictGo,
		vffChanged(map[string]map[int]bool{"a.go": {10: true}}))
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("GO with a grounded major must promote to NO-GO, got %s", got)
	}
}

func TestVerdictFromFindings_UngroundedBlockerStaysGo(t *testing.T) {
	// Cites a changed file but an unchanged line -> ungrounded -> no promotion.
	extra := vffExtra("blocker", "a.go", 999)
	got := enforceReviewerVerdictFromFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictGo,
		vffChanged(map[string]map[int]bool{"a.go": {10: true}}))
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("GO with only an ungrounded blocker must stay GO, got %s", got)
	}
	if _, promoted := extra["verdictPromotedFromFindings"]; promoted {
		t.Fatal("ungrounded blocker must not set promotion keys")
	}
}

func TestVerdictFromFindings_MinorStaysGo(t *testing.T) {
	extra := vffExtra("minor", "a.go", 10) // minor on a changed line is not blocking
	got := enforceReviewerVerdictFromFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictGo,
		vffChanged(map[string]map[int]bool{"a.go": {10: true}}))
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("GO with only a minor finding must stay GO, got %s", got)
	}
}

func TestVerdictFromFindings_NoGoUntouched(t *testing.T) {
	extra := vffExtra("blocker", "a.go", 10) // grounded, but verdict is already NO-GO
	got := enforceReviewerVerdictFromFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictNoGo,
		vffChanged(map[string]map[int]bool{"a.go": {10: true}}))
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("rail must only act on GO; NO-GO must pass through, got %s", got)
	}
	if _, promoted := extra["verdictPromotedFromFindings"]; promoted {
		t.Fatal("NO-GO path must not set promotion keys")
	}
}

func TestVerdictFromFindings_ToggleOff(t *testing.T) {
	t.Setenv("FOREMAN_VERDICT_FROM_FINDINGS", "0")
	extra := vffExtra("blocker", "a.go", 10)
	got := enforceReviewerVerdictFromFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictGo,
		vffChanged(map[string]map[int]bool{"a.go": {10: true}}))
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("toggle off must leave GO untouched, got %s", got)
	}
}

func TestVerdictFromFindings_GitUnavailableDegradesOpen(t *testing.T) {
	extra := vffExtra("blocker", "a.go", 10)
	// changedLines == nil signals git unavailable -> degrade-open (no promotion).
	got := enforceReviewerVerdictFromFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictGo, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("nil changedLines must leave verdict untouched, got %s", got)
	}
}

// TestGroundingRailsComposition runs the demote rail then the promote rail in
// executor order and asserts the net invariant: after both, verdict == NO-GO
// iff at least one grounded blocking finding is present, regardless of the
// model's stated verdict. This guards the two-rail composition against future
// reordering (the per-rail tests cover each rail in isolation).
func TestGroundingRailsComposition(t *testing.T) {
	gogo := foremanv1alpha1.AgenticTaskVerdictGo
	nogo := foremanv1alpha1.AgenticTaskVerdictNoGo
	changed := vffChanged(map[string]map[int]bool{"a.go": {10: true}})
	blk := func() map[string]any { return vffExtra("blocker", "a.go", 10) }  // grounded blocker
	ublk := func() map[string]any { return vffExtra("blocker", "a.go", 99) } // ungrounded blocker (line unchanged)
	non := func() map[string]any { return vffExtra("minor", "a.go", 10) }    // not blocking (severity)

	cases := []struct {
		name  string
		model foremanv1alpha1.AgenticTaskVerdict
		extra map[string]any
		want  foremanv1alpha1.AgenticTaskVerdict
	}{
		{"GO+blocker->NoGo", gogo, blk(), nogo},
		{"NoGo+blocker->NoGo", nogo, blk(), nogo},
		// Ungrounded BLOCKER (the #526-style false NO-GO the demote rail exists
		// for): the blocker cites an unchanged line, so it is not grounded.
		{"GO+ungrounded-blocker->GO", gogo, ublk(), gogo},
		{"NoGo+ungrounded-blocker->GO", nogo, ublk(), gogo},
		{"GO+none->GO", gogo, non(), gogo},
		{"NoGo+none->GO", nogo, non(), gogo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.model
			v = enforceReviewerGroundedFindings(logr.Discard(), tc.extra, v, changed)
			v = enforceReviewerVerdictFromFindings(logr.Discard(), tc.extra, v, changed)
			if v != tc.want {
				t.Fatalf("net verdict = %s, want %s", v, tc.want)
			}
		})
	}
}
