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
	"strconv"
	"strings"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/changepolicy"
)

// needsVerificationOutcome mirrors
// internal/foreman/controller/workload_coder_escalation.go's
// needsVerificationOutcome constant (#1033: a load-bearing external fact
// could not be grounded from the workspace, so escalating to a bigger
// model cannot help). Declared independently here rather than imported:
// pkg/foreman/agent must not depend on internal/foreman/controller (the
// dependency would run the wrong direction -- the controller already
// imports nothing from here, and this package is also used by the
// in-cluster coder Job binary, which has no business linking the
// reconciler). The two packages agree on the literal string by
// convention; if either changes, the other must change to match.
const needsVerificationOutcome = "NEEDS-VERIFICATION"

// alreadyResolvedOutcome mirrors the controller's alreadyResolvedOutcome
// constant in that same file (#970: the coder concluded the work is
// already on the branch/base; a terminal non-failure NO-GO that must not
// escalate). Same no-import rationale as needsVerificationOutcome above.
const alreadyResolvedOutcome = "ALREADY-RESOLVED"

// defaultSelfGO is the work-class policy (proposal 1075, section 3.1):
// the footprint classes a coder's own GO verdict may stand for without
// further human sign-off. ci-policy and release-policy are deliberately
// absent: the fast in-workspace gate (fmt/vet/build/lint, #749) can tell
// a GitHub Actions workflow or a release config still parses and the
// repo still builds, but it cannot tell whether the workflow logic or
// release behavior is actually correct -- exactly the failure mode
// proposal 1075 exists to catch. docs stays in the list because Task 4's
// claim-evidence gate (checkClaimEvidence, claim_gate.go) already
// enforces that every measured-sounding number added to docs lines cites
// a real, checked source; a docs change that passed that gate is not
// "unverified" the way an uninspected ci-policy/release-policy change
// is.
//
// Declared as a direct reference to foremanv1alpha1.DefaultSelfGO rather
// than its own literal (which is what this var held before #1075 Task 6):
// api/foreman/v1alpha1 owns the canonical list because
// VerdictPolicy.Resolve must return it without importing this package
// (this package already imports api/foreman/v1alpha1 for every CRD type,
// so the reverse import would cycle). Task 6's executor wiring
// (applyWorkClassPolicyForTask below) reaches this only through
// task.Spec.VerdictPolicy.Resolve(), which falls back to the same value
// when a Workload never set spec.verdictPolicy; this var remains for the
// tests in this package that exercise applyVerdictPolicy directly.
var defaultSelfGO = foremanv1alpha1.DefaultSelfGO

// unverifiedClaimReason and unverifiedClaimHowTo are the fixed
// whyItMatters/howToVerify strings attached to every claim-evidence
// gate-retry-exhaustion entry (see parseUnverifiedClaims in loop.go):
// the fact differs per finding, but why it needs a human and how to
// resolve it are the same for all of them.
const (
	unverifiedClaimReason = "the change presents this as a validated/measured fact"
	unverifiedClaimHowTo  = "measure on the target hardware or cite an existing in-repo source"
)

// workClassInList reports whether class's string form appears in list.
// list is small (a handful of policy classes at most) so a linear scan
// is simplest; called at most a few times per GO verdict.
func workClassInList(class workClass, list []string) bool {
	for _, c := range list {
		if c == string(class) {
			return true
		}
	}
	return false
}

// unverifiedPolicyEntry builds the single Extra["unverified"] entry
// applyVerdictPolicy attaches when a GO is downgraded for footprint
// policy reasons: the downgrade itself is the "claim" that needs a
// human (the class as a whole is unauditable by the fast gate), not an
// individual line in the diff -- contrast with checkClaimEvidence's
// per-claim findings (Task 4), where each unproven number is its own
// entry.
func unverifiedPolicyEntry(class workClass) map[string]string {
	return map[string]string{
		"fact": fmt.Sprintf(
			"this GO's diff footprint classifies as %q, which is outside the self-GO policy", class),
		"whyItMatters": fmt.Sprintf(
			"a %q change is certified here by the fast checks alone, and that class requires "+
				"human sign-off before it can be trusted", class),
		"howToVerify": "a human reviewer signs off on this class of change before it lands",
	}
}

// applyVerdictPolicy is the work-class downgrade path (proposal 1075,
// slice 1, Task 5a): a coder's self-certified GO is only as trustworthy
// as the checks that verified it, and the fast in-workspace gate cannot
// tell a correct CI workflow or release-config edit from a broken one.
// changed is the diff footprint (see diffFootprint), classified by
// classifyFootprint (Task 1) into a single dominant workClass (or
// "mixed" when no class reaches footprintDominance). selfGO is the
// operator-controlled allowlist of classes a GO may stand for
// unattended; the executor's call site (applyWorkClassPolicyForTask in
// executor_native.go) passes task.Spec.VerdictPolicy.Resolve() (#1075
// Task 6), which falls back to defaultSelfGO when the task carries no
// explicit policy.
//
// Non-GO results pass through completely untouched (res is returned as
// given, Extra untouched): the policy only ever demotes a GO, never
// promotes or otherwise annotates a NO-GO/INCOMPLETE/GATE-* verdict.
//
// On a GO, Extra["actualWorkClass"] is always recorded. When the coder
// declared its own class (res.Extra["workClass"], set by the caller from
// the model's submit_result extra before this call), it is preserved at
// Extra["declaredWorkClass"]. A class outside selfGO downgrades to NO-GO
// with Extra["outcome"] = needsVerificationOutcome and a single
// Extra["unverified"] entry explaining why; a declared/actual mismatch
// where BOTH classes are self-GO-able does not downgrade but records
// Extra["workClassMismatch"] = true so the discrepancy stays visible.
func applyVerdictPolicy(
	res *Result, changed map[string]int, selfGO []string, policy changepolicy.ChangePolicy,
) *Result {
	if res == nil || res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		return res
	}
	if res.Extra == nil {
		res.Extra = map[string]any{}
	}

	class := workClass(policy.Classify(changed))
	res.Extra["actualWorkClass"] = string(class)

	declared, hasDeclared := res.Extra["workClass"].(string)
	hasDeclared = hasDeclared && declared != ""
	if hasDeclared {
		res.Extra["declaredWorkClass"] = declared
	}

	if !workClassInList(class, selfGO) {
		res.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
		res.Extra["outcome"] = needsVerificationOutcome
		res.Extra["unverified"] = []map[string]string{unverifiedPolicyEntry(class)}
		return res
	}

	if hasDeclared && declared != string(class) && workClassInList(workClass(declared), selfGO) {
		res.Extra["workClassMismatch"] = true
	}
	return res
}

// diffFootprint reads the added+deleted line counts per file changed
// since the evidence anchor (see resolveClaimGateAnchor in
// claim_gate.go), for classifyFootprint to bucket into a workClass.
// evidenceBaseSHA is the SAME executor-resolved upstream base tip Task 4
// threads to the claim-evidence check (see resolveEvidenceBaseSHA /
// runLLMPath in executor_native.go); this function resolves the anchor
// through the identical resolveClaimGateAnchor helper rather than a
// second in-workspace ref derivation, so a footprint downgrade and a
// claim-evidence finding are always computed against the identical
// commit (the #1075 round-2 fork-lag / missing-ref lessons documented on
// resolveClaimGateAnchor apply here unchanged).
//
// Returns an error when the anchor cannot be resolved (evidenceBaseSHA
// empty, or `git merge-base` fails) or the numstat diff itself errors.
// The caller (runLLMPath in executor_native.go) fails open on any error:
// skip the policy entirely, log why, and record
// Extra["workClassUnknown"]=true rather than block a verdict on a
// footprint read failure.
func diffFootprint(
	ctx context.Context, workspace string, run commandRunner, evidenceBaseSHA string,
) (map[string]int, error) {
	anchor, err := resolveClaimGateAnchor(ctx, workspace, run, evidenceBaseSHA)
	if err != nil {
		return nil, err
	}
	out, err := run(ctx, workspace, nil, "git", "diff", "--numstat", anchor)
	if err != nil {
		return nil, fmt.Errorf("diffFootprint: git diff --numstat %s failed: %w", anchor, err)
	}
	return parseNumstat(out), nil
}

// parseNumstat parses `git diff --numstat` output into a per-file
// added+deleted line count, the shape classifyFootprint consumes. Each
// line is "<added>\t<deleted>\t<path>"; a binary file reports "-" for
// both counts (no line-based class signal) and is skipped. Malformed
// lines are skipped rather than erroring the whole read: a single
// unparsable line should not sink the policy check. Rename lines carry
// an "old => new" path (see normalizeNumstatRename); the counts are
// recorded under the post-rename path so classifyFile sees a real
// repository path, not git's rename notation.
func parseNumstat(out string) map[string]int {
	changed := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		added, addErr := strconv.Atoi(fields[0])
		deleted, delErr := strconv.Atoi(fields[1])
		if addErr != nil || delErr != nil {
			continue
		}
		changed[normalizeNumstatRename(fields[2])] += added + deleted
	}
	return changed
}

// normalizeNumstatRename resolves git's rename notation in a numstat
// path field to the post-rename path. git prints two forms:
//
//   - braced, when the old and new paths share a prefix/suffix:
//     "pkg/{old => new}/file.go" (either side of " => " may be empty
//     when a whole path segment was added or removed, e.g.
//     "{cmd => }/main.go");
//   - plain, when nothing is shared: "old/path.go => new/path.go".
//
// Non-rename paths pass through unchanged.
func normalizeNumstatRename(p string) string {
	// Braced form: substitute each "{old => new}" group with its new side.
	for {
		open := strings.Index(p, "{")
		if open < 0 {
			break
		}
		end := strings.Index(p[open:], "}")
		if end < 0 {
			break
		}
		end += open
		inner := p[open+1 : end]
		arrow := strings.Index(inner, " => ")
		if arrow < 0 {
			break
		}
		p = p[:open] + inner[arrow+4:] + p[end+1:]
	}
	// An emptied braced segment leaves a doubled or leading separator
	// ("pkg//file.go", "/file.go"); collapse it.
	p = strings.ReplaceAll(p, "//", "/")
	p = strings.TrimPrefix(p, "/")
	// Plain form (no shared prefix/suffix, so no braces were printed).
	if arrow := strings.Index(p, " => "); arrow >= 0 {
		p = p[arrow+4:]
	}
	return p
}
