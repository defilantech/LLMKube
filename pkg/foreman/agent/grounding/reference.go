package grounding

import (
	"regexp"
	"strings"
)

// exporterMetricTokenRe matches tokens that look like Prometheus/exporter metric
// names: an identifier that starts with a letter, contains only alphanumerics and
// underscores, and has at least one underscore (which distinguishes metric names
// from plain words). This is the token class scanned by the advisory
// grounding-breadth check.
var exporterMetricTokenRe = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9]*(?:_[A-Za-z0-9]+)+)\b`)

// commonMetricFirstSegments is a set of very common first-underscore-segment
// values that appear legitimately in docs as YAML keys, English compound words,
// or spec field names. When the first segment of an underscore token is in this
// set, the token is treated as an ordinary prose identifier (not a metric name)
// and is NOT flagged by the advisory check, even if it would otherwise match.
// Aim: keep false-positive rate near zero for LLMKube's own doc corpus.
var commonMetricFirstSegments = map[string]bool{
	// Common Go/K8s/YAML field-name prefixes
	"api": true, "app": true, "auth": true,
	"cache": true, "chart": true, "cluster": true, "config": true, "controller": true,
	"debug": true, "default": true,
	"enable": true, "env": true, "error": true,
	"fleet": true, "foreman": true,
	"gate": true, "gpu": true,
	"helm": true, "http": true,
	"image": true,
	"key":   true, "kube": true,
	"label": true, "limit": true, "log": true,
	"max": true, "metric": true, "min": true, "model": true,
	"name": true, "namespace": true,
	"pod": true, "port": true,
	"release": true, "replica": true, "resource": true,
	"service": true, "spec": true, "status": true,
	"target": true, "tier": true, "timeout": true, "type": true,
	"url":     true,
	"version": true,
	// llmkube_ metrics are checked by the existing blocker check; skip them here
	"llmkube": true,
}

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
// When gt.ExporterMetricPrefixes is non-empty it also runs the advisory
// exporter-metric check (minor severity): tokens that look like a Prometheus
// metric name but do not start with any known exporter prefix and are not in
// ChartResourceNames or the common-segment exclusion list.
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
		checkExporterMetricTokens(al, gt, &findings)
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

// checkExporterMetricTokens is the advisory breadth check. It flags tokens that
// look like a Prometheus/exporter metric name (letter-led, alphanumeric+underscore,
// at least one underscore) but are not grounded in any known set:
//   - not a known llmkube_* metric (those are handled by checkMetricAndCLITokens)
//   - not starting with a known ExporterMetricPrefixes entry
//   - not in ChartResourceNames
//   - not starting with a commonMetricFirstSegments entry (false-positive guard)
//
// Only runs when gt.ExporterMetricPrefixes is non-empty. Findings are "minor"
// severity to signal advisory (non-blocking) status.
func checkExporterMetricTokens(al AddedLine, gt *GroundTruth, out *[]Finding) {
	if len(gt.ExporterMetricPrefixes) == 0 {
		return
	}
	for _, m := range exporterMetricTokenRe.FindAllString(al.Text, -1) {
		// skip llmkube_* tokens: handled by the blocker check above.
		if strings.HasPrefix(m, "llmkube_") {
			continue
		}
		// skip if the token is a known chart resource name.
		if gt.ChartResourceNames[m] {
			continue
		}
		// skip if the first underscore-segment is a very common word that
		// appears legitimately in docs (e.g. "model_ref", "gpu_layers").
		firstSeg := m
		if idx := strings.Index(m, "_"); idx >= 0 {
			firstSeg = m[:idx]
		}
		if commonMetricFirstSegments[strings.ToLower(firstSeg)] {
			continue
		}
		// skip if the token starts with a known exporter metric prefix.
		groundedByPrefix := false
		for _, pfx := range gt.ExporterMetricPrefixes {
			if strings.HasPrefix(m, pfx) {
				groundedByPrefix = true
				break
			}
		}
		if groundedByPrefix {
			continue
		}
		*out = append(*out, Finding{
			Severity: "minor", Area: "doc-consistency",
			File: al.File, Line: al.Line,
			Message: "possible hallucinated metric or resource name: " + m +
				" (not found in known exporter prefixes or chart resources; verify)",
		})
	}
}
