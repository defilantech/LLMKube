package grounding

import (
	"regexp"
	"strings"
)

// llmkubeGroup matches an LLMKube-owned API group, optionally with a /version.
var llmkubeGroup = regexp.MustCompile(`\b([a-z0-9.-]*llmkube\.(?:dev|io))(?:/v[0-9a-z]+)?\b`)

var (
	metricTokenRe = regexp.MustCompile(`\bllmkube_[a-z0-9_]+\b`)
	cliTokenRe    = regexp.MustCompile("llmkube\\s+([a-z][a-z0-9-]*)")
)

// checkMetricAndCLITokens appends findings for llmkube_* metric tokens and
// `llmkube <subcmd>` references in al that do not resolve. Runs on every added
// line regardless of YAML block context. Each set is consulted only when
// populated (empty => that scanner was disabled => skip, fail-open).
func checkMetricAndCLITokens(al AddedLine, gt *GroundTruth, out *[]Finding) {
	if len(gt.Metrics) > 0 {
		for _, m := range metricTokenRe.FindAllString(al.Text, -1) {
			if !gt.Metrics[m] {
				*out = append(*out, Finding{Severity: "blocker", Area: "doc-consistency", File: al.File, Line: al.Line, Message: "unknown metric " + m})
			}
		}
	}
	if len(gt.CLICmds) > 0 {
		for _, m := range cliTokenRe.FindAllStringSubmatch(al.Text, -1) {
			if !gt.CLICmds[m[1]] {
				*out = append(*out, Finding{Severity: "blocker", Area: "doc-consistency", File: al.File, Line: al.Line, Message: "unknown llmkube subcommand " + m[1]})
			}
		}
	}
}

var (
	apiVersionLine = regexp.MustCompile(`^\s*apiVersion:\s*(\S+)`)
	kindLine       = regexp.MustCompile(`^\s*kind:\s*(\S+)`)
	// fieldLine captures leading indent (group 1) and the key (group 2).
	fieldLine = regexp.MustCompile(`^(\s*)([a-zA-Z][a-zA-Z0-9]*):`)
)

// structuralKeys are YAML keys that are never spec fields.
var structuralKeys = map[string]bool{
	"apiVersion": true, "kind": true, "metadata": true, "spec": true, "status": true,
}

// DetectUngroundedReferences flags LLMKube-owned references in added lines that
// do not resolve in gt. Within an LLMKube object block (opened by an added
// `apiVersion:` whose group is llmkube-owned) it validates: the API group, the
// `kind:`, and the direct child fields of an added `spec:` line. Field
// validation is scoped to the spec subtree by indentation: keys under
// metadata/status, keys at or above spec's indent, and keys nested deeper than
// spec's direct children are never flagged. Because the diff is --unified=0, a
// field is only checked when its `spec:` parent is itself among the added
// lines. External (non-llmkube) blocks are never judged.
func DetectUngroundedReferences(added []AddedLine, gt *GroundTruth) []Finding {
	var findings []Finding
	llmkubeBlock := false
	curKind := ""
	specIndent := -1      // indent of the active spec: line; -1 = not in spec subtree
	specChildIndent := -1 // indent of spec's direct children; -1 = unknown

	for _, al := range added {
		checkMetricAndCLITokens(al, gt, &findings)
		if m := apiVersionLine.FindStringSubmatch(al.Text); m != nil {
			llmkubeBlock = false
			curKind = ""
			specIndent, specChildIndent = -1, -1
			if g := llmkubeGroup.FindStringSubmatch(m[1]); g != nil {
				llmkubeBlock = true
				group := strings.SplitN(g[0], "/", 2)[0]
				if !gt.Groups[group] {
					findings = append(findings, Finding{
						Severity: "blocker", Area: "doc-consistency", File: al.File, Line: al.Line,
						Message: "unknown LLMKube API group " + group,
					})
				}
			}
			continue
		}
		if !llmkubeBlock {
			continue
		}
		if m := kindLine.FindStringSubmatch(al.Text); m != nil {
			curKind = m[1]
			if !gt.Kinds[curKind] {
				findings = append(findings, Finding{
					Severity: "blocker", Area: "doc-consistency", File: al.File, Line: al.Line,
					Message: "unknown LLMKube kind " + curKind,
				})
			}
			continue
		}
		m := fieldLine.FindStringSubmatch(al.Text)
		if m == nil {
			continue
		}
		indent := len(m[1])
		key := m[2]
		if key == "spec" {
			specIndent = indent
			specChildIndent = -1
			continue
		}
		if structuralKeys[key] {
			// metadata/status open a non-spec subtree; one at spec's indent or
			// above ends the spec subtree.
			if specIndent >= 0 && indent <= specIndent {
				specIndent, specChildIndent = -1, -1
			}
			continue
		}
		if specIndent < 0 {
			continue // not inside a spec subtree (e.g. a key under metadata)
		}
		if indent <= specIndent {
			specIndent, specChildIndent = -1, -1 // dedented out of spec
			continue
		}
		if specChildIndent == -1 {
			specChildIndent = indent
		}
		if indent != specChildIndent {
			continue // nested deeper than a direct spec child
		}
		knownFields := gt.SpecFields[curKind]
		if curKind == "" || knownFields == nil {
			if len(gt.SpecFields) == 0 {
				continue
			}
			union := map[string]bool{}
			for _, fields := range gt.SpecFields {
				for f := range fields {
					union[f] = true
				}
			}
			knownFields = union
		}
		if !knownFields[key] {
			label := curKind
			if label == "" {
				label = "unknown kind"
			}
			findings = append(findings, Finding{
				Severity: "blocker", Area: "doc-consistency", File: al.File, Line: al.Line,
				Message: "unknown spec field " + key + " on " + label,
			})
		}
	}
	return findings
}
