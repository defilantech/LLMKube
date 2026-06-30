package grounding

import (
	"regexp"
	"strings"
)

// llmkubeGroup matches an LLMKube-owned API group, optionally with a /version.
var llmkubeGroup = regexp.MustCompile(`\b([a-z0-9.-]*llmkube\.(?:dev|io))(?:/v[0-9a-z]+)?\b`)

var apiVersionLine = regexp.MustCompile(`^\s*apiVersion:\s*(\S+)`)

var (
	metricTokenRe = regexp.MustCompile(`\bllmkube_[a-z0-9_]+\b`)
	cliTokenRe    = regexp.MustCompile(`llmkube\s+([a-z][a-z0-9-]*)`)
)

// DetectUngroundedReferences flags two high-confidence, low-false-positive
// classes of LLMKube-owned reference in added doc/yaml lines:
//   - an `apiVersion:` naming an llmkube.dev / llmkube.io API GROUP that does
//     not exist in the repo, and
//   - an `llmkube_*` METRIC token that is not registered.
//
// CRD-kind and spec-field grounding are intentionally NOT done here. On the real
// committed doc corpus they false-positive: a `kind:` that is a field value (an
// AgenticTask's `spec.kind: issue-fix`) rather than a CRD kind, spec fields
// nested inside a `spec.<list>[]`, and historical release notes describing old
// schemas. A hard-fail gate must not block legitimate doc edits, so kind/field
// grounding is deferred to the tool-using reviewer (Plan 2), which can parse
// YAML and grep for real definitions. The group and metric checks produced zero
// false positives across the entire committed doc corpus.
func DetectUngroundedReferences(added []AddedLine, gt *GroundTruth) []Finding {
	var findings []Finding
	for _, al := range added {
		checkMetricAndCLITokens(al, gt, &findings)
		m := apiVersionLine.FindStringSubmatch(al.Text)
		if m == nil {
			continue
		}
		g := llmkubeGroup.FindStringSubmatch(m[1])
		if g == nil {
			continue // external (non-llmkube) group: never judged
		}
		group := strings.SplitN(g[0], "/", 2)[0]
		if !gt.Groups[group] {
			findings = append(findings, Finding{
				Severity: "blocker", Area: "doc-consistency", File: al.File, Line: al.Line,
				Message: "unknown LLMKube API group " + group,
			})
		}
	}
	return findings
}

// checkMetricAndCLITokens appends findings for llmkube_* metric tokens and
// `llmkube <subcmd>` references in al that do not resolve. Runs on every added
// line. Each set is consulted only when populated (empty => that scanner was
// disabled => skip, fail-open).
func checkMetricAndCLITokens(al AddedLine, gt *GroundTruth, out *[]Finding) {
	if len(gt.Metrics) > 0 {
		for _, m := range metricTokenRe.FindAllString(al.Text, -1) {
			if !gt.Metrics[m] {
				*out = append(*out, Finding{
					Severity: "blocker", Area: "doc-consistency",
					File: al.File, Line: al.Line, Message: "unknown metric " + m,
				})
			}
		}
	}
	// WARNING: cliTokenRe also matches ordinary prose ("the llmkube operator",
	// "each llmkube deployment"). CLI validation is therefore DISABLED in
	// production (the gate passes cmdDir="" so gt.CLICmds is empty and this block
	// is inert). Before enabling it, anchor the match to a code context
	// (backticks, or a following flag/arg).
	if len(gt.CLICmds) > 0 {
		for _, m := range cliTokenRe.FindAllStringSubmatch(al.Text, -1) {
			if !gt.CLICmds[m[1]] {
				*out = append(*out, Finding{
					Severity: "blocker", Area: "doc-consistency",
					File: al.File, Line: al.Line, Message: "unknown llmkube subcommand " + m[1],
				})
			}
		}
	}
}
