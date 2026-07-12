package grounding

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Evidence is one entry from the coder's declared evidence ledger.
type Evidence struct{ Claim, Source string }

// evidenceWindow is how many lines either side of a cited line count as
// the citation (tolerates small drift between citing and reading).
const evidenceWindow = 2

// ParseEvidence extracts the evidence ledger from a submit_result extra
// map. Malformed entries are dropped; enforcement happens in MatchClaims
// where a claim without usable evidence fails.
func ParseEvidence(extra map[string]any) []Evidence {
	raw, _ := extra["evidence"].([]any)
	var out []Evidence
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		c, _ := m["claim"].(string)
		s, _ := m["source"].(string)
		if c != "" {
			out = append(out, Evidence{Claim: c, Source: s})
		}
	}
	return out
}

// MatchClaims validates every detected claim against the declared evidence.
// Sources are read at the base commit (git show base:path): evidence must
// pre-date the change under review, so a file the coder added or edited in
// this same change cannot self-certify its claims.
func MatchClaims(
	ctx context.Context, workspace string, run CommandRunner, base string, claims []Claim, evidence []Evidence,
) []Finding {
	var out []Finding
	for _, c := range claims {
		if !claimSatisfied(ctx, workspace, run, base, c, evidence) {
			out = append(out, Finding{
				Severity: SeverityBlocker,
				Area:     "claim-evidence",
				File:     c.File,
				Line:     c.Line,
				Message: fmt.Sprintf(
					"empirical claim %q (%s %s) has no verifiable source: cite file:line whose "+
						"text (as it existed at the base commit) contains the same number, unit, "+
						"and subject; a file added or edited in this same change cannot "+
						"self-certify its own claims; or delete the claim; or mark it "+
						"illustrative and unmeasured",
					strings.TrimSpace(c.Text), c.Number, c.Unit),
			})
		}
	}
	return out
}

func claimSatisfied(
	ctx context.Context, workspace string, run CommandRunner, base string, c Claim, evidence []Evidence,
) bool {
	for _, ev := range evidence {
		if sourceProves(ctx, workspace, run, base, ev.Source, c) {
			return true
		}
	}
	return false
}

func sourceProves(ctx context.Context, workspace string, run CommandRunner, base, source string, c Claim) bool {
	i := strings.LastIndex(source, ":")
	if i <= 0 {
		return false // NONE, empty, URL, or bare path: not verifiable in slice 1
	}
	path, lineStr := source[:i], source[i+1:]
	lineNo, err := strconv.Atoi(lineStr)
	if err != nil || strings.HasPrefix(path, "http") {
		return false
	}
	if lineNo < 1 {
		return false
	}
	// Belt and suspenders ahead of git-show: reject traversal or absolute
	// paths outright rather than let git resolve them relative to base.
	if strings.Contains(path, "..") || strings.HasPrefix(path, "/") {
		return false
	}
	// Read the cited blob at the base commit, not the working tree: a coder
	// cannot write or edit a file in this same change and then cite it as
	// its own evidence. Any git error (path absent at base, bad ref) means
	// the source does not prove the claim.
	data, err := run(ctx, workspace, nil, "git", "show", base+":"+path)
	if err != nil {
		return false
	}
	lines := strings.Split(data, "\n")
	lo, hi := lineNo-1-evidenceWindow, lineNo-1+evidenceWindow
	if lo < 0 {
		lo = 0
	}
	if lo >= len(lines) {
		return false
	}
	if hi >= len(lines) {
		hi = len(lines) - 1
	}
	window := strings.Join(lines[lo:hi+1], "\n")
	if !windowHasNumberUnit(window, c) {
		return false
	}
	if len(c.Subjects) == 0 {
		return true
	}
	lowerWindow := strings.ToLower(window)
	for _, s := range c.Subjects {
		if strings.Contains(lowerWindow, strings.ToLower(s)) {
			return true
		}
	}
	return false
}

// windowHasNumberUnit checks the cited window contains the claim's exact
// number adjacent to its unit (any separator formatting). Normalization
// strips thousands separators only, never the decimal point: stripping
// the decimal point would make 9.9 and 99 collide, which must match
// claims.go's Number field exactly for equality comparison to be sound.
func windowHasNumberUnit(window string, c Claim) bool {
	for _, m := range claimNumberRE.FindAllStringSubmatch(window, -1) {
		num := strings.ReplaceAll(m[1], ",", "")
		if num == c.Number && normalizeUnit(m[2]) == c.Unit {
			return true
		}
	}
	return false
}
