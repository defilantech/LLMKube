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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/reviewer"
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

// TestReviewerGroundedChangedLines_UsesCommittedBranchDiff is the integration
// guard the synthetic-closure unit tests above miss: it drives the real git
// wiring in a reviewer-shaped workspace, where the coder's work is already
// committed and the working tree is clean. `git diff HEAD` is empty there, so
// the rail must diff the branch against its merge-base with main (main...HEAD).
// Otherwise every blocking finding is classified ungrounded and every NO-GO is
// demoted to GO unconditionally.
func TestReviewerGroundedChangedLines_UsesCommittedBranchDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ws := t.TempDir()
	foo := filepath.Join(ws, "foo.go")
	gitIn(t, "", "init", "-b", "main", ws)
	if err := os.WriteFile(foo, []byte("package p\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-m", "base")

	// The coder's work: a committed change on a branch, then checked out so the
	// working tree is clean, exactly what the reviewer sees after checkout.
	gitIn(t, ws, "checkout", "-b", "fix/x")
	if err := os.WriteFile(foo, []byte("package p\n\nfunc A() {}\n\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-m", "add B")

	// Precondition: the work is committed, so `git diff HEAD` (the old base) is
	// empty. This is the exact condition that made the rail a no-op.
	if out := gitIn(t, ws, "diff", "--name-only", "HEAD"); out != "" {
		t.Fatalf("precondition: working tree must be clean, got %q", out)
	}

	// The caller passes the ground-truth diff file list (non-empty here).
	changed := reviewerGroundedChangedLines(context.Background(), logr.Discard(), ws, "main", []string{"foo.go"}, nil)
	if changed == nil {
		t.Fatal("closure must be non-nil when the ground-truth diff is available")
	}
	got := changed("foo.go")
	if len(got) == 0 {
		t.Fatal("no changed lines for a committed branch: the rail is a no-op " +
			"(git diff HEAD is empty when work is committed); must diff main...HEAD")
	}
	// `func B() {}` is added at new-file line 5.
	if !got[5] {
		t.Errorf("expected added line 5 (func B) in the grounded set, got %v", got)
	}
}

// TestReviewerGroundedChangedLines_EmptyBranchDiffDegradesClosed guards the
// asymmetric-degrade fail-open: a successful-but-empty branch diff (nil error,
// zero changed files) means the reviewer never established the coder's changes
// against the base (e.g. it skipped the Step 1 fetch+checkout). The rail must
// step aside (nil closure) so a NO-GO is not demoted to GO and its PR opened.
// This must match the git-error degrade, not the happy path.
func TestReviewerGroundedChangedLines_EmptyBranchDiffDegradesClosed(t *testing.T) {
	ctx, log := context.Background(), logr.Discard()
	// Empty diff, no error: must degrade closed (nil closure).
	if got := reviewerGroundedChangedLines(ctx, log, t.TempDir(), "main", nil, nil); got != nil {
		t.Fatal("empty branch diff must yield a nil closure (degrade closed), got non-nil")
	}
	// Non-empty diff: a real closure is returned.
	if got := reviewerGroundedChangedLines(ctx, log, t.TempDir(), "main", []string{"a.go"}, nil); got == nil {
		t.Fatal("non-empty branch diff must yield a real closure")
	}
}

func TestGroundedBlockingFindings_Partition(t *testing.T) {
	findings := []reviewer.Finding{
		// grounded
		{Severity: reviewer.SeverityBlocker, Area: "scope", Message: "m", File: "a.go", Line: 10},
		// ungrounded: line not changed
		{Severity: reviewer.SeverityMajor, Area: "scope", Message: "m", File: "b.go", Line: 99},
		// ungrounded: no line
		{Severity: reviewer.SeverityMajor, Area: "scope", Message: "m", File: "c.go", Line: 0},
		// excluded: minor
		{Severity: reviewer.SeverityMinor, Area: "style", Message: "m", File: "a.go", Line: 10},
	}
	changed := func(f string) map[int]bool {
		return map[string]map[int]bool{"a.go": {10: true}, "b.go": {5: true}}[f]
	}
	grounded, ungrounded := groundedBlockingFindings(findings, changed)
	if len(grounded) != 1 || grounded[0].File != "a.go" {
		t.Fatalf("grounded = %+v, want [a.go:10]", grounded)
	}
	if len(ungrounded) != 2 {
		t.Fatalf("ungrounded = %d, want 2 (b.go unchanged line + c.go no line)", len(ungrounded))
	}
}
