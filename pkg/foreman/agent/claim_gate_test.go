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
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/grounding"
)

// unsourcedBenchmarkPatch is a working-tree diff (the shape grounding.
// AddedLines parses out of `git diff --cached`) that adds one benchmark row
// to a docs README: an empirical claim (~45 tok/s) with no accompanying
// evidence.
const unsourcedBenchmarkPatch = `diff --git a/examples/README.md b/examples/README.md
+++ b/examples/README.md
@@ -0,0 +1 @@
+| Mixtral 8x7B | ~45 tok/s |
`

// fakeEvidenceBaseSHA stands in for evidenceBaseSHA: the literal upstream
// base tip commit the executor resolves via repo.BaseBranchSHA before the
// gate's fix-retry loop starts (see runLLMPath in executor_native.go), and
// threads down to checkClaimEvidence unchanged. It is deliberately NOT a
// ref name (never "main", never "origin/main"): the round-2 redesign's
// whole point is that neither checkClaimEvidence nor resolveClaimGateAnchor
// ever reads a ref out of the workspace, only this literal commit.
const fakeEvidenceBaseSHA = "fake0upstreamtipsha0"

// fakeForkPointSHA stands in for the commit `git merge-base HEAD
// <evidenceBaseSHA>` resolves to: the coder's task branch fork point. It is
// deliberately a different fake value than fakeEvidenceBaseSHA so a test
// asserting the diff/show calls used the merge-base RESULT cannot pass by
// accident against evidenceBaseSHA itself (see the fork-lag test below).
const fakeForkPointSHA = "abc123fakeforkpoint"

// fakeAddedLinesRunner is a hermetic commandRunner shaped after what
// checkClaimEvidence actually invokes at run time: `git merge-base HEAD
// <evidenceBaseSHA>` to resolve the fork-point anchor (answered with
// fakeForkPointSHA regardless of the second argument, standing in for
// whatever `git merge-base HEAD <evidenceBaseSHA>` resolves to for these
// tests), a `git add -A -- <one pathspec>` per-pathspec staging call
// (ignored; returns cleanly so AddedLines's best-effort staging never
// errors), a `git diff --cached ...` call against that anchor (answered
// with patch), and any `git show <anchor>:<path>` call (answered with
// showContent, or "" if the case does not need one).
func fakeAddedLinesRunner(patch, showContent string) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" || len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "merge-base":
			return fakeForkPointSHA, nil
		case "diff":
			return patch, nil
		case "show":
			return showContent, nil
		default: // "add" (per-pathspec staging), and anything else.
			return "", nil
		}
	}
}

func TestCheckClaimEvidence(t *testing.T) {
	// No evidence declared: the check must fail and name the claim.
	run := fakeAddedLinesRunner(unsourcedBenchmarkPatch, "")
	fn := checkClaimEvidence(nil, fakeEvidenceBaseSHA)
	failed, output := fn(context.Background(), t.TempDir(), run)
	if !failed {
		t.Fatal("expected claim-evidence to fail with an unsourced benchmark row")
	}
	if !strings.Contains(output, "45") || !strings.Contains(output, "tok/s") {
		t.Errorf("output should name the claim, got: %s", output)
	}
	if !strings.HasPrefix(output, "[claim-evidence]") {
		t.Errorf("output must start with the [claim-evidence] marker, got: %s", output)
	}
}

func TestCheckClaimEvidence_SourcedClaimPasses(t *testing.T) {
	// The cited source, read at the fork-point anchor, carries the same
	// number, unit, and subject as the claim: satisfied, so the check
	// passes.
	showContent := "one\ntwo\nMixtral 8x7B benchmark: ~45 tok/s decode\nfour\nfive\n"
	run := fakeAddedLinesRunner(unsourcedBenchmarkPatch, showContent)
	evidence := []grounding.Evidence{{Claim: "~45 tok/s Mixtral", Source: "docs/foo.md:3"}}
	fn := checkClaimEvidence(evidence, fakeEvidenceBaseSHA)
	failed, output := fn(context.Background(), t.TempDir(), run)
	if failed {
		t.Fatalf("expected a correctly sourced claim to pass, got failed with output: %s", output)
	}
	if output != "" {
		t.Errorf("expected empty output on pass, got: %s", output)
	}
}

func TestCheckClaimEvidence_NoDocsChanges(t *testing.T) {
	// An empty diff (no added docs lines) yields no claims: pass, no output.
	run := fakeAddedLinesRunner("", "")
	fn := checkClaimEvidence(nil, fakeEvidenceBaseSHA)
	failed, output := fn(context.Background(), t.TempDir(), run)
	if failed {
		t.Fatalf("expected no findings with no added docs lines, got failed with output: %s", output)
	}
	if output != "" {
		t.Errorf("expected empty output, got: %s", output)
	}
}

func TestCheckClaimEvidence_AddedLinesErrorFailsOpen(t *testing.T) {
	// grounding.AddedLines returning an error (e.g. workspace is not a git
	// repo) must never block: with no external backstop for claim-evidence in
	// slice 1, fail-open here is narrowly for the infrastructure-error case
	// where we cannot tell whether a claim exists at all (merge-base
	// resolves fine; the diff itself errors).
	stubErr := errors.New("fatal: not a git repository")
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		switch {
		case name == "git" && len(args) > 0 && args[0] == "merge-base":
			return fakeForkPointSHA, nil
		case name == "git" && len(args) > 0 && args[0] == "diff":
			return "", stubErr
		default:
			return "", nil
		}
	}
	fn := checkClaimEvidence(nil, fakeEvidenceBaseSHA)
	failed, output := fn(context.Background(), t.TempDir(), run)
	if failed {
		t.Fatalf("expected fail-open on a git diff error, got failed with output: %s", output)
	}
	if output != "" {
		t.Errorf("expected empty output on fail-open, got: %s", output)
	}
}

// TestCheckClaimEvidence_BypassRegression_SelfCommittedEvidenceRejected is
// the regression for finding 1 (critical): a coder writes bench/notes.txt
// (outside the docs pathspec, so it is never claim-scanned itself), commits
// it via bash mid-loop (recoverSelfCommitsOrNoChange in executor_native.go
// anticipates exactly this self-commit behavior), then cites
// bench/notes.txt:1 as evidence for a fabricated docs claim.
//
// Before this fix, evidence was read at literal "HEAD": since the coder's
// self-commit IS part of HEAD, `git show HEAD:bench/notes.txt` would
// succeed and the fabricated claim would be wrongly proven. After this fix,
// evidence is read at the fork-point anchor (git merge-base HEAD
// evidenceBaseSHA, where evidenceBaseSHA is the literal upstream base tip
// the executor resolved before the coder's branch existed), which predates
// every commit the coder made on its own branch: `git show
// <forkpoint>:bench/notes.txt` must fail (the file did not exist there), so
// the claim must NOT be proven.
func TestCheckClaimEvidence_BypassRegression_SelfCommittedEvidenceRejected(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" || len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "merge-base":
			return fakeForkPointSHA, nil
		case "diff":
			return unsourcedBenchmarkPatch, nil
		case "show":
			target := ""
			if len(args) > 1 {
				target = args[1]
			}
			if target == "HEAD:bench/notes.txt" {
				// A HEAD-based read (the pre-fix behavior) would see the
				// coder's own self-commit and wrongly prove the claim.
				return "Mixtral 8x7B benchmark: ~45 tok/s decode\n", nil
			}
			// Any other read (i.e. at the fork-point anchor) fails: the
			// file did not exist there, before the coder's self-commit.
			return "", fmt.Errorf("fatal: path 'bench/notes.txt' does not exist in %q", target)
		default:
			return "", nil
		}
	}
	evidence := []grounding.Evidence{{Claim: "~45 tok/s Mixtral", Source: "bench/notes.txt:1"}}
	fn := checkClaimEvidence(evidence, fakeEvidenceBaseSHA)
	failed, output := fn(context.Background(), t.TempDir(), run)
	if !failed {
		t.Fatal("bypass not closed: a claim backed only by a self-committed file must not be proven")
	}
	if !strings.HasPrefix(output, "[claim-evidence]") {
		t.Errorf("output must start with the [claim-evidence] marker, got: %s", output)
	}
}

// TestCheckClaimEvidence_AnchorUnresolvedFailsClosedWhenClaimsExist covers
// the fail-closed half of finding 1's fail-mode change, ported to the
// round-2 literal-SHA design (findings A/C/D): `git merge-base HEAD
// <evidenceBaseSHA>` itself fails in the workspace (e.g. evidenceBaseSHA
// was never fetched into this clone), but a claim is still detected (via
// the HEAD-fallback diff used only for detection). Since the evidence base
// could not be resolved, the coder's claim cannot be verified either way,
// so the check must fail CLOSED rather than silently pass it through.
func TestCheckClaimEvidence_AnchorUnresolvedFailsClosedWhenClaimsExist(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" || len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "merge-base":
			return "", errors.New("fatal: no merge base found")
		case "diff":
			return unsourcedBenchmarkPatch, nil
		default:
			return "", nil
		}
	}
	fn := checkClaimEvidence(nil, fakeEvidenceBaseSHA)
	failed, output := fn(context.Background(), t.TempDir(), run)
	if !failed {
		t.Fatal("expected fail-closed when claims exist but the evidence anchor cannot be resolved")
	}
	if !strings.HasPrefix(output, "[claim-evidence]") {
		t.Errorf("output must start with the [claim-evidence] marker, got: %s", output)
	}
}

// TestCheckClaimEvidence_AnchorUnresolvedNoClaimsFailsOpen is the
// counterpart of the fail-closed case above: when git merge-base fails but
// the HEAD-fallback detection finds no claims at all, there is nothing to
// verify, so the check must still fail open (no block).
func TestCheckClaimEvidence_AnchorUnresolvedNoClaimsFailsOpen(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" || len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "merge-base":
			return "", errors.New("fatal: no merge base found")
		default:
			return "", nil // empty diff: no added docs lines
		}
	}
	fn := checkClaimEvidence(nil, fakeEvidenceBaseSHA)
	failed, output := fn(context.Background(), t.TempDir(), run)
	if failed {
		t.Fatalf("expected fail-open when no claims are detected, got failed with output: %s", output)
	}
	if output != "" {
		t.Errorf("expected empty output, got: %s", output)
	}
}

// TestCheckClaimEvidence_EmptyEvidenceBaseSHA_FailsClosedWhenClaimsExist
// covers round-2 finding D: the executor could not resolve evidenceBaseSHA
// at all (e.g. baseBranch does not exist upstream, an unsynced release
// branch), so it is passed down as "". This must degrade exactly like a
// git merge-base failure: claims found via the HEAD-fallback scan fail
// CLOSED. Critically, resolveClaimGateAnchor must reach this outcome
// WITHOUT depending on any ref existing in the workspace (finding D's
// requirement); the fake runner below never answers a "merge-base" or any
// other git call, so a passing test here also proves no such lookup is
// attempted for an empty evidenceBaseSHA.
func TestCheckClaimEvidence_EmptyEvidenceBaseSHA_FailsClosedWhenClaimsExist(t *testing.T) {
	var gitCalls []string
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" || len(args) == 0 {
			return "", nil
		}
		gitCalls = append(gitCalls, args[0])
		if args[0] == "diff" {
			return unsourcedBenchmarkPatch, nil
		}
		return "", nil
	}
	fn := checkClaimEvidence(nil, "")
	failed, output := fn(context.Background(), t.TempDir(), run)
	if !failed {
		t.Fatal("expected fail-closed when claims exist but evidenceBaseSHA is empty (unresolved)")
	}
	if !strings.HasPrefix(output, "[claim-evidence]") {
		t.Errorf("output must start with the [claim-evidence] marker, got: %s", output)
	}
	for _, c := range gitCalls {
		if c == "merge-base" {
			t.Errorf("an empty evidenceBaseSHA must short-circuit before any git merge-base call; got calls: %v", gitCalls)
		}
	}
}

// TestCheckClaimEvidence_EmptyEvidenceBaseSHA_NoClaimsPasses is the
// counterpart: an empty evidenceBaseSHA with no claims detected in the
// degraded HEAD-fallback scan must still pass (round-2 finding D: an
// unresolved evidence base is not, by itself, a reason to block a change
// that never touched docs).
func TestCheckClaimEvidence_EmptyEvidenceBaseSHA_NoClaimsPasses(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		return "", nil // no docs changes at all
	}
	fn := checkClaimEvidence(nil, "")
	failed, output := fn(context.Background(), t.TempDir(), run)
	if failed {
		t.Fatalf("expected fail-open when evidenceBaseSHA is empty and no claims are detected, "+
			"got failed with output: %s", output)
	}
	if output != "" {
		t.Errorf("expected empty output, got: %s", output)
	}
}

// TestResolveClaimGateAnchor pins resolveClaimGateAnchor's contract
// directly: it never reads a ref out of the workspace, only ever resolves
// `git merge-base HEAD <evidenceBaseSHA>` against the literal SHA it is
// given, and treats an empty evidenceBaseSHA, a merge-base command error,
// and an empty merge-base result as three distinct, all-erroring cases.
func TestResolveClaimGateAnchor(t *testing.T) {
	t.Run("empty evidenceBaseSHA short-circuits without invoking git", func(t *testing.T) {
		called := false
		run := func(_ context.Context, _ string, _ []string, _ string, _ ...string) (string, error) {
			called = true
			return "", nil
		}
		if _, err := resolveClaimGateAnchor(context.Background(), t.TempDir(), run, ""); err == nil {
			t.Error("expected an error for an empty evidenceBaseSHA")
		}
		if called {
			t.Error("resolveClaimGateAnchor must not invoke git when evidenceBaseSHA is empty")
		}
	})

	t.Run("whitespace-only evidenceBaseSHA is treated as empty", func(t *testing.T) {
		run := func(_ context.Context, _ string, _ []string, _ string, _ ...string) (string, error) {
			t.Fatal("git must not be invoked for a whitespace-only evidenceBaseSHA")
			return "", nil
		}
		if _, err := resolveClaimGateAnchor(context.Background(), t.TempDir(), run, "   "); err == nil {
			t.Error("expected an error for a whitespace-only evidenceBaseSHA")
		}
	})

	t.Run("merge-base command error is wrapped", func(t *testing.T) {
		stubErr := errors.New("boom")
		run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
			if name == "git" && len(args) > 0 && args[0] == "merge-base" {
				return "", stubErr
			}
			return "", nil
		}
		_, err := resolveClaimGateAnchor(context.Background(), t.TempDir(), run, fakeEvidenceBaseSHA)
		if err == nil || !errors.Is(err, stubErr) {
			t.Errorf("expected the underlying merge-base error to be wrapped, got: %v", err)
		}
	})

	t.Run("empty merge-base output is an error", func(t *testing.T) {
		run := func(_ context.Context, _ string, _ []string, _ string, _ ...string) (string, error) {
			return "  \n", nil // whitespace-only: no common ancestor found
		}
		if _, err := resolveClaimGateAnchor(context.Background(), t.TempDir(), run, fakeEvidenceBaseSHA); err == nil {
			t.Error("expected an error when git merge-base yields no output")
		}
	})

	t.Run("success resolves to the trimmed merge-base output, called with the literal SHA", func(t *testing.T) {
		var gotArgs []string
		run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
			if name == "git" {
				gotArgs = args
			}
			return "  " + fakeForkPointSHA + "\n", nil
		}
		sha, err := resolveClaimGateAnchor(context.Background(), t.TempDir(), run, fakeEvidenceBaseSHA)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sha != fakeForkPointSHA {
			t.Errorf("resolved anchor = %q, want %q", sha, fakeForkPointSHA)
		}
		want := []string{"merge-base", "HEAD", fakeEvidenceBaseSHA}
		if len(gotArgs) != len(want) {
			t.Fatalf("git args = %v, want %v", gotArgs, want)
		}
		for i := range want {
			if gotArgs[i] != want[i] {
				t.Errorf("git args = %v, want %v", gotArgs, want)
				break
			}
		}
	})
}

// TestCheckClaimEvidence_ForkLag_UsesMergeBaseResultNotUpstreamTipOrOriginRef
// is the regression pin for round-2 finding A (fork-lag false positive):
// the anchor used to read evidence and scope the added-lines diff must be
// the RESULT of `git merge-base HEAD <evidenceBaseSHA>`, never
// evidenceBaseSHA itself (the current upstream tip, which in a fork-lag
// scenario can sit AHEAD of the branch's true fork point) and never any
// "origin/*" ref (an in-workspace remote-tracking ref, which is exactly
// what let the fork-lag bug happen: a lagging fork's origin/<baseBranch>
// resolves to a stale ancestor, sweeping the whole upstream delta into the
// diff as unproven "claims"). This test uses a fake merge-base RESULT
// distinct from evidenceBaseSHA and asserts both the AddedLines diff and
// the MatchClaims evidence read use that result.
func TestCheckClaimEvidence_ForkLag_UsesMergeBaseResultNotUpstreamTipOrOriginRef(t *testing.T) {
	var mergeBaseArgs, diffArgs, showArgs []string
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" || len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "merge-base":
			mergeBaseArgs = args
			return fakeForkPointSHA, nil
		case "diff":
			diffArgs = args
			return unsourcedBenchmarkPatch, nil
		case "show":
			showArgs = args
			return "", fmt.Errorf("not found")
		default:
			return "", nil
		}
	}
	evidence := []grounding.Evidence{{Claim: "~45 tok/s Mixtral", Source: "docs/foo.md:3"}}
	fn := checkClaimEvidence(evidence, fakeEvidenceBaseSHA)
	failed, output := fn(context.Background(), t.TempDir(), run)
	if !failed {
		t.Fatalf("expected the unsourced claim to fail, got pass with output: %s", output)
	}

	wantMergeBase := []string{"merge-base", "HEAD", fakeEvidenceBaseSHA}
	if len(mergeBaseArgs) != len(wantMergeBase) {
		t.Fatalf("merge-base args = %v, want %v", mergeBaseArgs, wantMergeBase)
	}
	for i := range wantMergeBase {
		if mergeBaseArgs[i] != wantMergeBase[i] {
			t.Errorf("merge-base args = %v, want %v", mergeBaseArgs, wantMergeBase)
			break
		}
	}

	if !containsArg(diffArgs, fakeForkPointSHA) {
		t.Errorf("AddedLines diff must run against the merge-base RESULT %q, got args %v",
			fakeForkPointSHA, diffArgs)
	}
	if containsArg(diffArgs, fakeEvidenceBaseSHA) {
		t.Errorf("AddedLines diff must NOT run against evidenceBaseSHA directly, got args %v", diffArgs)
	}
	if len(showArgs) < 2 || !strings.HasPrefix(showArgs[1], fakeForkPointSHA+":") {
		t.Errorf("MatchClaims evidence read = %v, want a git show %s:<path> call", showArgs, fakeForkPointSHA)
	}
	for _, a := range append(append([]string{}, diffArgs...), showArgs...) {
		if strings.Contains(a, "origin/") {
			t.Errorf("must never reference an origin/* ref; diff args %v, show args %v", diffArgs, showArgs)
		}
	}
}

// containsArg reports whether any element of args equals or contains want.
func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want || strings.Contains(a, want) {
			return true
		}
	}
	return false
}

// TestFormatClaimFindings_NeverExceedsMaxCheckOutputBytes is the regression
// for finding 3: hundreds of findings with long messages must not push
// formatClaimFindings's own output past maxCheckOutputBytes (INCLUDING the
// "... and N more" suffix), so downstream truncateOutput's tail-keeping
// truncation never fires on it and the "[claim-evidence]" marker at the
// front survives buildFeedback intact (the marker was observed lost at
// ~156 findings before this fix).
func TestFormatClaimFindings_NeverExceedsMaxCheckOutputBytes(t *testing.T) {
	findings := make([]grounding.Finding, 500)
	for i := range findings {
		findings[i] = grounding.Finding{
			File: fmt.Sprintf("docs/bench-%d.md", i),
			Line: i + 1,
			Message: fmt.Sprintf(
				"empirical claim %q (%d tok/s) has no verifiable source: cite file:line, delete "+
					"the claim, or mark it illustrative and unmeasured", strings.Repeat("x", 200), i),
		}
	}

	out := formatClaimFindings(findings)
	if !strings.HasPrefix(out, "[claim-evidence]") {
		t.Fatalf("formatClaimFindings must start with the marker, got: %.200s", out)
	}
	if len(out) > maxCheckOutputBytes {
		t.Fatalf("formatClaimFindings output is %d bytes, want <= maxCheckOutputBytes (%d)",
			len(out), maxCheckOutputBytes)
	}

	// Feed it through buildFeedback, the actual downstream path a Go-repo
	// claim-evidence failure travels (RunCoderGate -> checkFailure ->
	// buildFeedback -> truncateOutput per check). The marker must survive
	// because truncateOutput never fires: formatClaimFindings already
	// bounded its own output below maxCheckOutputBytes.
	fb := buildFeedback([]checkFailure{{name: "claim-evidence", output: out}})
	if !strings.Contains(fb, "[claim-evidence]") {
		t.Fatalf("marker lost after buildFeedback; feedback:\n%.500s", fb)
	}
	if strings.Contains(fb, "...(truncated)...") {
		t.Fatalf("truncateOutput fired on formatClaimFindings output; it should never need to:\n%.500s", fb)
	}
}

// TestAppendClaimEvidenceFailure_ComposesWithEmptyFeedback covers finding 4:
// a passing generic gate (empty feedback) plus a claim-evidence failure
// synthesizes the directive header exactly once and carries the
// "[claim-evidence]" marker.
func TestAppendClaimEvidenceFailure_ComposesWithEmptyFeedback(t *testing.T) {
	claimOut := "[claim-evidence] unsourced claim\n  - docs/x.md:1 some message\n"
	got := appendClaimEvidenceFailure("", claimOut)

	if n := strings.Count(got, "The verification gate failed"); n != 1 {
		t.Errorf("want the gate-failed header exactly once, got %d in:\n%s", n, got)
	}
	if !strings.Contains(got, "## claim-evidence") {
		t.Errorf("want a '## claim-evidence' section header, got:\n%s", got)
	}
	if !strings.Contains(got, "[claim-evidence]") {
		t.Errorf("want the [claim-evidence] marker preserved, got:\n%s", got)
	}
}

// TestAppendClaimEvidenceFailure_AppendsToExistingFailureNoDuplicateHeader
// covers finding 4's second composition case: an ALREADY-failing gate
// (non-empty feedback, its own directive header already present) gets the
// claim-evidence section appended without a second directive header.
func TestAppendClaimEvidenceFailure_AppendsToExistingFailureNoDuplicateHeader(t *testing.T) {
	existing := "The verification gate failed. Fix the issues below and resubmit.\n" +
		"\n## lint: ruff check .\nboom\n"
	claimOut := "[claim-evidence] unsourced claim\n  - docs/x.md:1 some message\n"
	got := appendClaimEvidenceFailure(existing, claimOut)

	if n := strings.Count(got, "The verification gate failed"); n != 1 {
		t.Errorf("want the gate-failed header exactly once (not duplicated), got %d in:\n%s", n, got)
	}
	if !strings.Contains(got, "## lint: ruff check .") {
		t.Errorf("want the pre-existing lint failure preserved, got:\n%s", got)
	}
	if !strings.Contains(got, "## claim-evidence") {
		t.Errorf("want a '## claim-evidence' section header appended, got:\n%s", got)
	}
	if !strings.Contains(got, "[claim-evidence]") {
		t.Errorf("want the [claim-evidence] marker preserved, got:\n%s", got)
	}
}
