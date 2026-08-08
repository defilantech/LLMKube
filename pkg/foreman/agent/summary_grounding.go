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
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Summary grounding (#1411). Every other model-supplied claim in this
// pipeline is ground-truthed before it is trusted -- filesTouched against
// `git diff --name-only` (#582), issueAsk against the fetched issue body
// (#644), reviewer findings against the lines the diff changed (#1166) --
// but the summary that becomes the PR description a human reads at merge
// time was passed through verbatim. A reviewer GO on a real, good diff
// still shipped a body claiming "adds a /health endpoint" for a diff that
// contained zero occurrences of "health".
//
// The check here is deterministic and deliberately narrow, matching the
// rails it sits beside: extract the concrete, checkable things the summary
// names (backticked identifiers, file paths, route-shaped strings) and
// require each to appear somewhere in the branch diff. It is NOT a
// semantic check and makes no attempt to decide whether the summary is a
// fair description of the change.
//
// The bias throughout is against false positives. A correct summary that
// gets flagged puts noise on every PR and trains humans to ignore the
// note, which is strictly worse than the bug it guards. Concretely:
//
//   - Only backticked spans are considered. Unbackticked prose is never
//     parsed, so ordinary English cannot trip the check.
//   - Grounding uses the FULL diff (added, removed, and context lines,
//     plus the file headers), not just added lines. "renamed `foo` to
//     `bar`" therefore grounds `foo` on the removal that renamed it.
//   - A claim with no literal hit falls back to its identifier atoms and
//     passes if ANY atom appears. `logger.info()` grounds on `logger`.
//   - Claim shapes that are indistinguishable from English (a bare
//     lowercase word in backticks) are not checkable and are skipped.
//   - Any missing input -- no diff, git error, empty summary -- skips the
//     check entirely and preserves the model's prose.

// maxListedUnverifiedClaims bounds the note appended to the PR body. A
// summary that trips more than a handful of claims is better read as
// "distrust the whole summary" than as a list, and an unbounded list
// would bury the diff link under generated text.
const maxListedUnverifiedClaims = 5

var (
	// inlineCodeRe matches a backticked span: the "quoted identifiers"
	// the summary is claiming exist. Spans must close on the same line,
	// so a summary truncated mid-span yields no claim.
	inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")

	// fencedCodeRe strips fenced blocks before extraction: a code sample
	// is an illustration, not a claim about the diff.
	fencedCodeRe = regexp.MustCompile("(?s)```.*?```")

	// pathClaimRe matches a slash-separated or bare filename carrying an
	// extension: `pkg/foreman/agent/loop.go`, `README.md`.
	pathClaimRe = regexp.MustCompile(`^[\w.@~-]+(?:/[\w.@~-]+)*\.[A-Za-z][A-Za-z0-9]{0,9}$`)

	// routeClaimRe matches a route-ish string: a leading slash and no
	// file extension. `/health`, `/api/v1/models`, `/users/{id}`.
	routeClaimRe = regexp.MustCompile(`^/[A-Za-z0-9][A-Za-z0-9/_{}:.-]*$`)

	// identClaimRe matches a single (possibly dotted) identifier with an
	// optional call suffix: `submit_result`, `logger.info()`, `Ensurer`.
	identClaimRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*(?:\(\))?$`)

	// atomRe pulls identifier atoms out of a claim for the tolerant
	// fallback pass. Three characters minimum: shorter atoms match
	// everything and would ground any claim at all.
	atomRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{2,}`)
)

// claimKind classifies an extracted backtick span. Spans that do not fall
// into one of the checkable shapes are skipped rather than guessed at.
type claimKind int

const (
	claimSkip claimKind = iota
	claimPath
	claimRoute
	claimIdent
)

// classifySummaryClaim decides whether a backticked span is a concrete,
// checkable claim about the change and, if so, which shape it is.
//
// The identifier rule is the load-bearing false-positive guard: a bare
// lowercase word in backticks (`logging`, `health`, `tests`) is
// indistinguishable from emphasis and is NOT checked. An identifier only
// qualifies once it carries a code marker -- an underscore, a dot, a call
// suffix, or an uppercase letter.
func classifySummaryClaim(span string) (string, claimKind) {
	c := strings.TrimSpace(span)
	// Prose in backticks (a command line, a phrase) is not a single
	// claim; refuse to guess which of its words the diff should contain.
	if c == "" || strings.ContainsAny(c, " \t") {
		return "", claimSkip
	}
	// Trailing sentence punctuation belongs to the prose, not the claim.
	c = strings.TrimRight(c, ".,;:!?")
	if c == "" {
		return "", claimSkip
	}
	switch {
	case pathClaimRe.MatchString(c):
		return c, claimPath
	case routeClaimRe.MatchString(c) && len(c) > 2:
		return c, claimRoute
	case identClaimRe.MatchString(c) && codeShapedIdent(c):
		return c, claimIdent
	}
	return "", claimSkip
}

// codeShapedIdent reports whether an identifier carries a marker that
// distinguishes it from an ordinary English word: an underscore, a dot, a
// call suffix, or any uppercase letter.
func codeShapedIdent(c string) bool {
	if strings.ContainsAny(c, "_.") || strings.HasSuffix(c, "()") {
		return true
	}
	return strings.ToLower(c) != c
}

// extractSummaryClaims returns the checkable claims a summary makes, in
// first-occurrence order and deduplicated.
func extractSummaryClaims(summary string) []string {
	prose := fencedCodeRe.ReplaceAllString(summary, " ")
	var claims []string
	seen := map[string]bool{}
	for _, m := range inlineCodeRe.FindAllStringSubmatch(prose, -1) {
		c, kind := classifySummaryClaim(m[1])
		if kind == claimSkip || seen[c] {
			continue
		}
		seen[c] = true
		claims = append(claims, c)
	}
	return claims
}

// diffGroundTruth is what a claim is checked against: the full text of
// `git diff <base>...HEAD` plus the changed-file list, and the workspace
// root so a path claim can be grounded on the file merely existing.
type diffGroundTruth struct {
	workspace string
	text      string
	files     []string
}

// branchDiffText returns the full unified diff of the branch against base,
// context lines included. Context is deliberate: it widens what counts as
// grounded, and this check must err toward accepting the summary.
func branchDiffText(ctx context.Context, workspace, base string, run commandRunner) string {
	out, err := run(ctx, workspace, nil, "git", "diff", base+"...HEAD")
	if err != nil {
		return ""
	}
	return out
}

// claimGrounded reports whether a single claim is supported by the diff.
func claimGrounded(claim string, kind claimKind, g diffGroundTruth) bool {
	if strings.Contains(g.text, claim) {
		return true
	}
	if kind == claimPath {
		return pathClaimGrounded(claim, g)
	}
	// Route and identifier claims: any identifier atom appearing anywhere
	// in the diff grounds the claim. `logger.info()/warning()` never gets
	// here (it is not a single identifier), but `logger.info()` does, and
	// grounds on `logger` even when the diff spells the call differently.
	for _, a := range atomRe.FindAllString(claim, -1) {
		if strings.Contains(g.text, a) {
			return true
		}
		for _, f := range g.files {
			if strings.Contains(f, a) {
				return true
			}
		}
	}
	return false
}

// pathClaimGrounded grounds a file-path claim. Basename equality against
// the changed-file list covers the normal case; existence in the workspace
// covers a summary that legitimately points at a file it did not change
// ("follows the pattern in `pkg/foreman/agent/loop.go`"), which must not
// be reported as an invented path.
//
// Path claims deliberately skip the atom fallback: a claim's leading
// segments (`pkg`, `internal`, `cmd`) appear in almost any diff and would
// ground a wholly fabricated path.
func pathClaimGrounded(claim string, g diffGroundTruth) bool {
	base := path.Base(claim)
	if strings.Contains(g.text, base) {
		return true
	}
	for _, f := range g.files {
		if f == claim || path.Base(f) == base {
			return true
		}
	}
	return workspaceHasPath(g.workspace, claim)
}

// workspaceHasPath reports whether the claimed path exists in the checked
// out tree. Claims that try to escape the workspace are refused rather
// than followed.
func workspaceHasPath(workspace, claim string) bool {
	if workspace == "" || strings.Contains(claim, "..") || path.IsAbs(claim) {
		return false
	}
	_, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(claim)))
	return err == nil
}

// ungroundedSummaryClaims returns the checkable claims the summary makes
// that the diff does not support, in summary order. An empty result means
// either that every claim checked out or that there was nothing to check.
func ungroundedSummaryClaims(summary string, g diffGroundTruth) []string {
	if strings.TrimSpace(summary) == "" {
		return nil
	}
	// No ground truth means no check. Reporting "unverified" because git
	// failed would put the note on honest PRs, which is the failure mode
	// this whole file is built to avoid.
	if g.text == "" && len(g.files) == 0 {
		return nil
	}
	var out []string
	for _, claim := range extractSummaryClaims(summary) {
		_, kind := classifySummaryClaim(claim)
		if !claimGrounded(claim, kind, g) {
			out = append(out, claim)
		}
	}
	return out
}

// annotateUnverifiedClaims appends a visible note to the summary naming
// the claims the diff does not support.
//
// It appends rather than rewrites on purpose. Deleting the offending
// sentence would require deciding, without any parse of the prose, which
// sentence a claim belongs to -- and a wrong guess silently destroys an
// accurate description, which is unrecoverable for the human reading the
// PR. Appending is additive: the reviewer's words survive verbatim and
// the disagreement between the words and the diff is stated where the
// merge decision is made. The original is archived under
// `extra.summaryClaimed` either way.
func annotateUnverifiedClaims(summary string, unverified []string) string {
	if len(unverified) == 0 {
		return summary
	}
	shown := unverified
	var more int
	if len(shown) > maxListedUnverifiedClaims {
		more = len(shown) - maxListedUnverifiedClaims
		shown = shown[:maxListedUnverifiedClaims]
	}
	quoted := make([]string, 0, len(shown))
	for _, c := range shown {
		quoted = append(quoted, "`"+c+"`")
	}
	list := strings.Join(quoted, ", ")
	if more > 0 {
		list += fmt.Sprintf(" (+%d more)", more)
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(summary, "\n"))
	b.WriteString("\n\n> [!WARNING]\n")
	b.WriteString("> **Unverified claims.** The summary above is the reviewer's; foreman ")
	b.WriteString("cross-checks the concrete things it names against the branch diff. ")
	fmt.Fprintf(&b, "These do not appear in the diff: %s.\n", list)
	b.WriteString("> Treat them as unsubstantiated when deciding whether this branch ")
	b.WriteString("closes the linked issue.")
	return b.String()
}
