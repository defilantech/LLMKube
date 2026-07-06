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

// blockerFinding builds a findings extra map with one finding.
func findingExtra(severity, file string, line int) map[string]any {
	return map[string]any{
		"findings": []any{
			map[string]any{
				"severity": severity,
				"area":     "scope",
				"message":  "m",
				"file":     file,
				"line":     line,
			},
		},
	}
}

// changed returns a changedLines closure for a fixed file->lines fixture.
func changed(fix map[string]map[int]bool) func(string) map[int]bool {
	return func(f string) map[int]bool { return fix[f] }
}

func TestGroundedFindings_GroundedBlockerStaysNoGo(t *testing.T) {
	extra := findingExtra("blocker", "pkg/cli/cache.go", 42)
	fix := map[string]map[int]bool{"pkg/cli/cache.go": {42: true}}
	got := enforceReviewerGroundedFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictNoGo, changed(fix))
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("grounded blocker must keep NO-GO, got %s", got)
	}
	if _, demoted := extra["groundedFindingDemotion"]; demoted {
		t.Fatal("grounded NO-GO must not be marked demoted")
	}
}

func TestGroundedFindings_FabricatedFileDemotes(t *testing.T) {
	// Cites a file the diff never changed (the docs-fabrication case).
	extra := findingExtra("blocker", "docs/MODEL-CACHE.md", 10)
	fix := map[string]map[int]bool{"pkg/cli/cache.go": {42: true}} // docs file absent
	got := enforceReviewerGroundedFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictNoGo, changed(fix))
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("fabricated-file NO-GO must demote to GO, got %s", got)
	}
	if extra["groundedFindingDemotion"] != true {
		t.Fatal("expected groundedFindingDemotion=true")
	}
	if extra["ungroundedFindings"] == nil {
		t.Fatal("expected ungroundedFindings archived")
	}
}

func TestGroundedFindings_UnchangedLineInChangedFileDemotes(t *testing.T) {
	// Cites a changed file but a line OUTSIDE any hunk (the kubectl-exec case).
	extra := findingExtra("major", "pkg/cli/delete.go", 999)
	fix := map[string]map[int]bool{"pkg/cli/delete.go": {106: true, 107: true}} // 999 not changed
	got := enforceReviewerGroundedFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictNoGo, changed(fix))
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("unchanged-line NO-GO must demote to GO, got %s", got)
	}
}

func TestGroundedFindings_NoLineDemotes(t *testing.T) {
	extra := findingExtra("blocker", "pkg/cli/cache.go", 0) // line 0 = not pinned
	fix := map[string]map[int]bool{"pkg/cli/cache.go": {42: true}}
	got := enforceReviewerGroundedFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictNoGo, changed(fix))
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("blocking finding with no line must demote to GO, got %s", got)
	}
}

func TestGroundedFindings_MinorOnlyDemotes(t *testing.T) {
	extra := findingExtra("minor", "pkg/cli/cache.go", 42) // minor is not blocking
	fix := map[string]map[int]bool{"pkg/cli/cache.go": {42: true}}
	got := enforceReviewerGroundedFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictNoGo, changed(fix))
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("NO-GO with only minor findings must demote to GO, got %s", got)
	}
}

func TestGroundedFindings_GoUntouched(t *testing.T) {
	extra := findingExtra("blocker", "docs/MODEL-CACHE.md", 10) // ungrounded, but verdict is GO
	fix := map[string]map[int]bool{}
	got := enforceReviewerGroundedFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictGo, changed(fix))
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("GO must pass through untouched, got %s", got)
	}
	if _, demoted := extra["groundedFindingDemotion"]; demoted {
		t.Fatal("GO path must not set demotion keys")
	}
}

func TestGroundedFindings_ToggleOff(t *testing.T) {
	t.Setenv("FOREMAN_GROUNDED_FINDINGS", "0")
	extra := findingExtra("blocker", "docs/MODEL-CACHE.md", 10) // ungrounded
	fix := map[string]map[int]bool{}
	got := enforceReviewerGroundedFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictNoGo, changed(fix))
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("toggle off must leave NO-GO untouched, got %s", got)
	}
}

func TestGroundedFindings_GitUnavailableDegradesOpen(t *testing.T) {
	extra := findingExtra("blocker", "docs/MODEL-CACHE.md", 10) // ungrounded
	// changedLines == nil signals git unavailable -> degrade-open (no demotion).
	got := enforceReviewerGroundedFindings(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictNoGo, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("nil changedLines must leave verdict untouched, got %s", got)
	}
}
