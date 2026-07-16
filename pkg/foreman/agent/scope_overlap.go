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
	"fmt"
	"path"
	"strings"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// pathRefExtensions are the file extensions a token must carry to count
// as a concrete path reference in an issue body. The set is deliberately
// small: under-extracting is safe (no refs means the scope check stays
// observe-only) while over-extracting risks false drift flags, so
// identifiers, commands, and API groups must not slip through.
//
// This is the language-agnostic base: docs, config, and Go's own source
// extension (LLMKube's home turf). A repo's PRIMARY source language is
// declared per-task via GateProfile.SourceExtensions and unioned in at
// extraction time (see extractIssuePathRefs), so a Godot repo's `.gd`
// files or a Rust repo's `.rs` files are recognized as issue refs
// exactly as the diff-side hasSourceFile guard already recognizes them.
// Without that union the extractor was blind to every non-Go language,
// so the scope-overlap vouch (#744) never fired for the polyglot fleet.
var pathRefExtensions = map[string]bool{
	"go": true, "md": true, "yaml": true, "yml": true, "sh": true,
	"json": true, "mod": true, "sum": true, "tmpl": true, "proto": true,
	"toml": true, "mk": true,
}

// extensionSet normalizes a GateProfile SourceExtensions list (".gd",
// ".TS") into the dotless-lowercase keys isPathRef compares against.
// Returns nil for an empty list so callers can cheaply skip the union.
func extensionSet(exts []string) map[string]bool {
	if len(exts) == 0 {
		return nil
	}
	m := make(map[string]bool, len(exts))
	for _, e := range exts {
		if e = strings.ToLower(strings.TrimPrefix(e, ".")); e != "" {
			m[e] = true
		}
	}
	return m
}

// extractIssuePathRefs pulls concrete file references out of an issue
// body: tokens that are either a path (slash-separated segments ending
// in a known source extension, like `config/rbac/role.yaml`) or a bare
// filename (`AGENTS.md`). Commands (`make manifests`), RBAC groups
// (`core/endpoints`), marker annotations (`kubebuilder:rbac`), and API
// type paths (`discovery.k8s.io/v1.EndpointSlice`) do not qualify.
// Escaped backticks (as returned by the GitHub API) are normalized
// away before tokenizing. Results are deduplicated in first-occurrence
// order.
//
// sourceExtensions is the task's declared GateProfile source language
// (e.g. [".gd"] for Godot); those extensions are unioned with the
// language-agnostic pathRefExtensions so the extractor sees the repo's
// primary source files, not only Go and docs.
func extractIssuePathRefs(body string, sourceExtensions []string) []string {
	if body == "" {
		return nil
	}
	extraExts := extensionSet(sourceExtensions)
	normalized := strings.ReplaceAll(body, "\\`", "`")
	normalized = strings.NewReplacer("`", " ", "(", " ", ")", " ", "[", " ", "]", " ",
		"{", " ", "}", " ", "\"", " ", "'", " ", ",", " ", ";", " ").Replace(normalized)

	var refs []string
	seen := map[string]bool{}
	for _, tok := range strings.Fields(normalized) {
		tok = strings.Trim(tok, ".:!?")
		if tok == "" || seen[tok] {
			continue
		}
		if isPathRef(tok, extraExts) {
			seen[tok] = true
			refs = append(refs, tok)
		}
	}
	return refs
}

// docRefExtensions are the extensions treated as documentation for the
// scope-overlap vouch (enforceReviewerIssueAsk). A review whose ONLY
// in-scope evidence is a touched doc file is too weak to override a failed
// issueAsk verification: the substantive named deliverable (source/config)
// was left untouched. The set is intentionally narrow — prose docs only.
var docRefExtensions = map[string]bool{
	"md": true, "markdown": true, "rst": true, "txt": true, "adoc": true,
}

// scopeMatchHasNonDoc reports whether any matched scope ref is something
// other than a documentation file. The scope vouch requires this so that
// matching only a doc the issue happens to mention — while the primary
// source/config file the issue is actually about goes untouched — does not
// rescue an otherwise unverifiable GO.
func scopeMatchHasNonDoc(matched []string) bool {
	for _, m := range matched {
		ext := strings.ToLower(strings.TrimPrefix(path.Ext(m), "."))
		if !docRefExtensions[ext] {
			return true
		}
	}
	return false
}

// hasSourceFile reports whether any path in paths ends with one of the
// extensions in exts. If exts is empty, it defaults to [".go"] so
// existing behavior is preserved.
func hasSourceFile(paths []string, exts []string) bool {
	if len(exts) == 0 {
		exts = []string{".go"}
	}
	for _, p := range paths {
		for _, ext := range exts {
			if strings.HasSuffix(p, ext) {
				return true
			}
		}
	}
	return false
}

// isPathRef reports whether a single cleaned token is a concrete file
// reference per the rules documented on extractIssuePathRefs. extraExts
// carries the task's declared source extensions (dotless-lowercase, via
// extensionSet) so a repo's primary language is recognized alongside the
// language-agnostic pathRefExtensions base.
func isPathRef(tok string, extraExts map[string]bool) bool {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(tok), "."))
	if !pathRefExtensions[ext] && !extraExts[ext] {
		return false
	}
	for _, seg := range strings.Split(tok, "/") {
		if seg == "" || strings.IndexFunc(seg, func(r rune) bool {
			return !(r == '.' || r == '_' || r == '-' ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
		}) != -1 {
			return false
		}
	}
	return true
}

// enforceReviewerScopeOverlap is the computable half of scope-drift
// detection (#647). The issue body names concrete files; the ground-truth
// diff says which files actually changed. When the issue names at least
// one file and the diff touches none of them, that is the #379 drift
// signature, and it is detectable without any model judgment, which
// matters because review judgment is demonstrably stochastic: the same
// devstral that caught #379's drift in May approved it in June.
//
// Policy mirrors enforceReviewerIssueAsk:
//   - refs exist, none matched, verdict GO: demote to NO-GO so the
//     workload controller's escalation emission routes the branch to a
//     bigger reviewer.
//   - refs exist, none matched, other verdict: annotate only.
//   - no refs in the issue, or no diff available: observe-only, no
//     annotations (absence of signal is not evidence of drift).
//
// Matching is generous on purpose (exact path or basename equality):
// a false drift flag costs one escalation review, while a missed match
// would demote a legitimate branch.
func enforceReviewerScopeOverlap(
	log logr.Logger,
	extra map[string]any,
	issueBody string,
	diffFiles []string,
	verdict foremanv1alpha1.AgenticTaskVerdict,
	sourceExtensions []string,
) foremanv1alpha1.AgenticTaskVerdict {
	if extra == nil || issueBody == "" || len(diffFiles) == 0 {
		return verdict
	}
	refs := extractIssuePathRefs(issueBody, sourceExtensions)
	if len(refs) == 0 {
		return verdict
	}

	diffBases := make(map[string]bool, len(diffFiles))
	diffPaths := make(map[string]bool, len(diffFiles))
	for _, f := range diffFiles {
		diffPaths[f] = true
		diffBases[path.Base(f)] = true
	}
	matched := []string{}
	for _, r := range refs {
		if diffPaths[r] || diffBases[path.Base(r)] {
			matched = append(matched, r)
		}
	}

	drift := len(matched) == 0
	extra["scopeRefs"] = refs
	extra["scopeMatched"] = matched
	extra["scopeDriftDetected"] = drift
	if !drift {
		return verdict
	}

	// A diff with zero indexable source files is not scope drift — it is a
	// legitimate docs- or YAML-only change (#800). Skip the scope check
	// so the run proceeds.
	if !hasSourceFile(diffFiles, sourceExtensions) {
		return verdict
	}

	if verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		log.Info("reviewer scope: diff touches none of the files the issue names; verdict already non-GO, annotating only",
			"verdict", verdict, "scopeRefs", refs)
		return verdict
	}
	extra["verdictDemoted"] = true
	extra["verdictClaimed"] = string(verdict)
	extra["demotionReason"] = fmt.Sprintf(
		"scope drift: the issue names %d file(s) (%s) and the diff touches none of them",
		len(refs), strings.Join(refs, ", "))
	log.Info("reviewer scope: drift detected on GO verdict; demoting to NO-GO",
		"scopeRefs", refs, "diffFiles", diffFiles)
	return foremanv1alpha1.AgenticTaskVerdictNoGo
}
