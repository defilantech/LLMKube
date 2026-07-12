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
	"fmt"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent/grounding"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// claimGateDetectFallbackBase is the ref used ONLY to detect whether a
// change adds any empirical claims when the evidence anchor (see
// resolveClaimGateAnchor) could not be resolved: either evidenceBaseSHA
// itself was empty (the executor could not resolve the upstream base tip;
// see runLLMPath in executor_native.go) or `git merge-base HEAD
// <evidenceBaseSHA>` failed in this workspace. It is never used to read
// evidence sources: MatchClaims only ever runs against a resolved anchor
// (checkClaimEvidence fails closed instead when claims are detected under
// this fallback). "HEAD" is the coder's uncommitted working-tree changes on
// the current branch, matching what checkReferenceGrounding (coder_gate.go)
// and checkGroundingBreadth (grounding_breadth_gate.go) already pass to
// grounding.AddedLines in this same pre-commit gate context.
const claimGateDetectFallbackBase = "HEAD"

// claimDocsPathspec scopes grounding.AddedLines to the doc surfaces
// DetectEmpiricalClaims inspects: markdown files anywhere in the tree, and
// everything under docs/ or examples/. grounding.AddedLines stages each of
// these pathspecs with its own `git add -A` call (see diff.go), so a repo
// missing docs/ or examples/ entirely (a normal shape, not an error) still
// gets its *.md files scanned (#1075 round-2 finding B: a single combined
// `git add -A -- '*.md' docs examples` call used to fail atomically and
// silently stage nothing at all whenever one pathspec matched no files).
var claimDocsPathspec = []string{"*.md", "docs", "examples"}

// resolveClaimGateAnchor resolves the commit that evidence provenance is
// anchored to: the point where the coder's task branch (HEAD in this
// workspace) diverges from evidenceBaseSHA, the LITERAL upstream base tip
// commit the executor resolved once, before this task's gate retries
// started (see runLLMPath in executor_native.go, which reuses the same
// repo.BaseBranchSHA helper reviewerDiffBase and
// recoverSelfCommitsOrNoChange already call; the #995 precedent). The
// executor holds that SHA in memory for the rest of the run; this function
// never re-derives it from a ref inside the workspace.
//
// git merge-base HEAD evidenceBaseSHA is always an ancestor of
// evidenceBaseSHA. That is a property of merge-base itself, not of any ref
// staying put, so a commit the coder makes on its own branch during this
// run can never become the anchor: every commit HEAD gains happens AFTER
// evidenceBaseSHA was captured, and merge-base(HEAD, evidenceBaseSHA)
// cannot walk past it. This holds for a fresh run (the anchor is the
// branch point) and for a revise cycle (setupTaskBranch's rebase replays
// the coder's own prior-cycle commits onto the current base; those
// replayed commits post-date evidenceBaseSHA's ancestry too).
//
// This resolves against a literal SHA supplied by the caller, never a ref
// name, and in particular never re-derives origin/<baseBranch> inside the
// workspace the way an earlier version of this function did. Two round-2
// review findings drove that change:
//
//   - Fork-lag false positive: in a fork-based deployment, --git-remote-url
//     points origin at a fork that can lag the upstream project by an
//     arbitrary amount, while setupTaskBranch always cuts the task branch
//     from the CURRENT upstream tip (repo.CreateBranchFromUpstream, #813).
//     Deriving the anchor from `git merge-base HEAD origin/<baseBranch>`
//     inside the workspace landed on the STALE fork tip, so AddedLines swept
//     the entire upstream lag delta into the diff and flagged other
//     people's docs numbers as the coder's unproven claims.
//   - Fail-closed on a missing ref: an unsynced release branch with no
//     origin/<baseBranch> ref at all made both `git merge-base` and the old
//     `git rev-parse origin/<baseBranch>` fallback fail, tripping the
//     fail-closed branch below even when the coder's docs were untouched.
//
// Resolving evidenceBaseSHA once in the executor, from a real fetch against
// the upstream project (not the fork), and passing the literal commit down
// removes both failure modes: there is no in-workspace ref lookup left to
// go stale or go missing. It also corrects a claim an earlier version of
// this comment made: moving refs/remotes/origin/<baseBranch> does NOT
// require a push or credentials: `git update-ref` moves it locally, no
// network access needed. That claim is moot for the current design (there
// is no origin/* ref read here anymore), but the record needed fixing
// regardless (#1075 round-2 finding C).
//
// An error is returned when evidenceBaseSHA is empty (the executor could
// not resolve it) or when git merge-base itself fails or yields no output.
// The caller treats that as "the evidence base could not be verified," not
// as "no claims exist" (see checkClaimEvidence's degraded-scan fallback).
func resolveClaimGateAnchor(
	ctx context.Context, workspace string, run commandRunner, evidenceBaseSHA string,
) (string, error) {
	evidenceBaseSHA = strings.TrimSpace(evidenceBaseSHA)
	if evidenceBaseSHA == "" {
		return "", fmt.Errorf(
			"claim-evidence: evidence base SHA is unresolved (the executor could not resolve the " +
				"upstream base tip); cannot verify claim provenance")
	}

	out, err := run(ctx, workspace, nil, "git", "merge-base", "HEAD", evidenceBaseSHA)
	if err != nil {
		return "", fmt.Errorf("claim-evidence: git merge-base HEAD %s failed: %w", evidenceBaseSHA, err)
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return "", fmt.Errorf("claim-evidence: git merge-base HEAD %s returned no common ancestor", evidenceBaseSHA)
	}
	return sha, nil
}

// checkClaimEvidence returns a gateCheck-compatible fn enforcing the
// claim-evidence contract (proposal 1075, slice 1): every empirical claim
// (a number carrying a measurement unit, e.g. "~87 tok/s") added since the
// evidence anchor (see resolveClaimGateAnchor) in this change's docs must
// be backed by a declared evidence entry whose cited file:line, read at
// that same anchor, contains the same number, unit, and subject. evidence
// is the coder's declared ledger from submit_result.extra (see
// grounding.ParseEvidence), captured once per gate run by the caller.
// evidenceBaseSHA is the literal upstream base tip commit the executor
// resolved before this task's gate retries began (see runLLMPath in
// executor_native.go); resolveClaimGateAnchor derives the actual anchor
// from it via git merge-base. It may be empty when the executor could not
// resolve it, which this check treats as a degraded scan (see below), not
// as an automatic pass or an automatic block.
//
// Zero findings returns (false, ""). Otherwise (true, output): output
// always starts with the "[claim-evidence]" marker (even when the finding
// list is capped by maxCheckOutputBytes) and lists each finding's
// File:Line and Message, so the coder loop can fix the issue and resubmit:
// cite the real source, delete the claim, or mark it illustrative and
// unmeasured.
//
// Fail-open, narrowly: this check only passes without blocking when NO
// claims can be read at all (an infrastructure failure: the workspace is
// not a git repo, grounding.AddedLines otherwise errors, etc.), because in
// that case we cannot tell whether an unproven claim exists. There is no
// external backstop for claim-evidence in slice 1 to catch what this check
// misses: the clean-room gate Job re-runs only the resolved profile's
// format/lint/build/test/codegen commands (RunGenericGate, or the Go
// preset's equivalent) and has no notion of claim-evidence. This
// in-workspace check is the sole enforcement point.
//
// Degraded scan: when the anchor cannot be resolved (evidenceBaseSHA is
// empty, or resolveClaimGateAnchor's git merge-base call itself fails),
// claim detection falls back to a HEAD-only scan of the coder's
// uncommitted working-tree changes (claimGateDetectFallbackBase). That
// scan is used SOLELY to decide fail-open vs. fail-closed below; it is
// never treated as proof that a claim IS backed by evidence, because a
// self-committed "evidence" file would be part of that same HEAD (see the
// bypass regression test). A claim found under the degraded scan fails
// CLOSED (formatClaimAnchorUnresolved): the coder's honest exits are the
// same as an unproven claim (cite it once the anchor issue is fixed,
// delete it, or mark it illustrative). No claim found under the degraded
// scan passes, but logs an advisory note that the scan was degraded so the
// posture is inspectable rather than silently indistinguishable from an
// ordinary clean pass.
func checkClaimEvidence(
	evidence []grounding.Evidence, evidenceBaseSHA string,
) func(ctx context.Context, workspace string, run commandRunner) (bool, string) {
	return func(ctx context.Context, workspace string, run commandRunner) (bool, string) {
		anchor, anchorErr := resolveClaimGateAnchor(ctx, workspace, run, evidenceBaseSHA)

		// Detect claims against the resolved anchor when available (so the
		// scanned diff is everything since the anchor, catching a claim
		// the coder committed mid-loop, which a literal "HEAD" diff would
		// miss entirely). When the anchor could not be resolved, detection
		// falls back to "HEAD" purely to decide fail-open vs. fail-closed
		// below; a claim proven only against that fallback is never treated
		// as backed by evidence.
		detectBase := claimGateDetectFallbackBase
		if anchorErr == nil {
			detectBase = anchor
		}

		added, err := grounding.AddedLines(
			ctx, workspace, grounding.CommandRunner(run), detectBase, claimDocsPathspec,
		)
		if err != nil {
			// AddedLines itself failed: we cannot tell whether claims exist.
			// Fail open (infrastructure error, not a claims decision).
			return false, ""
		}
		claims := grounding.DetectEmpiricalClaims(added)
		if len(claims) == 0 {
			if anchorErr != nil {
				// Degraded scan found nothing to worry about: pass, but log
				// so the degraded posture stays inspectable instead of
				// reading identically to an ordinary clean pass.
				logf.FromContext(ctx).Info(
					"claim-evidence: evidence base unresolved; degraded scan found no claims",
					"err", anchorErr.Error())
			}
			return false, ""
		}

		if anchorErr != nil {
			// Claims exist but the evidence base could not be resolved: fail
			// CLOSED rather than let an unresolvable anchor silently pass an
			// unverifiable claim.
			return true, formatClaimAnchorUnresolved(anchorErr)
		}

		findings := grounding.MatchClaims(
			ctx, workspace, grounding.CommandRunner(run), anchor, claims, evidence,
		)
		if len(findings) == 0 {
			return false, ""
		}
		return true, formatClaimFindings(findings)
	}
}

// formatClaimAnchorUnresolved renders the fail-closed message for claims
// detected when resolveClaimGateAnchor could not resolve an evidence base.
// Always starts with the "[claim-evidence]" marker so the caller can key on
// it identically to formatClaimFindings's output.
func formatClaimAnchorUnresolved(anchorErr error) string {
	return "[claim-evidence] This change adds docs lines asserting a number with a measurement " +
		"unit, but the evidence base could not be resolved, so no citation can be verified " +
		"(" + anchorErr.Error() + "). Delete the claim(s), or mark them illustrative and " +
		"unmeasured, and resubmit.\n"
}

// appendClaimEvidenceFailure merges a failing claim-evidence check into an
// existing (possibly empty) gate feedback string. RunGenericGate has no
// notion of claim-evidence (it only knows the resolved profile's format/
// lint/build/test/codegen commands), so makeCoderGateVerifier runs the
// check separately for non-Go repos and folds the result in here, following
// the same "## <check>\n<output>" per-check block buildFeedback uses for
// every RunCoderGate failure, so a non-Go repo's claim-evidence failure
// reads identically to a Go repo's.
func appendClaimEvidenceFailure(feedback, output string) string {
	if strings.TrimSpace(feedback) == "" {
		feedback = "The verification gate failed. Fix the issues below and resubmit.\n"
	}
	feedback += "\n## claim-evidence\n" + truncateOutput(output) + "\n"
	return feedback
}

// claimFindingsReserveBytes reserves headroom below maxCheckOutputBytes for
// the "... and N more" suffix line formatClaimFindings appends after it
// stops adding findings. Without this reserve, the bound check below
// compared the running length against maxCheckOutputBytes and only THEN
// appended the suffix, so the suffix itself could push the total past
// maxCheckOutputBytes; downstream tail-keeping truncation (truncateOutput,
// used by both buildFeedback and appendClaimEvidenceFailure) would then
// fire on this check's own output and strip the leading "[claim-evidence]"
// marker it exists to protect (observed at ~156 findings). 512 bytes is far
// more than any realistic "... and N more" line needs.
const claimFindingsReserveBytes = 512

// formatClaimFindings renders claim-evidence findings as gate feedback. The
// "[claim-evidence]" header is always written first and is never dropped by
// bounding: unlike truncateOutput's tail-keeping truncation (which would cut
// the leading marker off a long list), this stops ADDING findings once the
// bound is reached and says how many more were omitted, so the marker
// authored by this check is always present for the caller to key on.
//
// The output is guaranteed to never exceed maxCheckOutputBytes, INCLUDING
// the "... and N more" suffix: every finding line is bound-checked against
// maxCheckOutputBytes-claimFindingsReserveBytes before it is written, so the
// suffix (written only after breaking out of the loop, and always well
// under claimFindingsReserveBytes) can never carry the total past
// maxCheckOutputBytes. This is what makes it safe for the caller to skip
// truncateOutput's tail-keeping truncation on this check's output entirely:
// it never fires because the bound is never exceeded in the first place.
func formatClaimFindings(findings []grounding.Finding) string {
	bound := maxCheckOutputBytes - claimFindingsReserveBytes
	var b strings.Builder
	b.WriteString("[claim-evidence] These added docs lines assert a number with no verifiable " +
		"source. For each, cite the real source (file:line whose text at the base commit backs " +
		"the claim), delete the claim, or mark it illustrative and unmeasured:\n")
	for i, f := range findings {
		line := fmt.Sprintf("  - %s:%d %s\n", f.File, f.Line, f.Message)
		if b.Len()+len(line) > bound {
			fmt.Fprintf(&b, "  ... and %d more\n", len(findings)-i)
			return b.String()
		}
		b.WriteString(line)
	}
	return b.String()
}
