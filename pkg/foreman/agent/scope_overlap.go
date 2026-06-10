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
var pathRefExtensions = map[string]bool{
	"go": true, "md": true, "yaml": true, "yml": true, "sh": true,
	"json": true, "mod": true, "sum": true, "tmpl": true, "proto": true,
	"toml": true, "mk": true,
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
func extractIssuePathRefs(body string) []string {
	if body == "" {
		return nil
	}
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
		if isPathRef(tok) {
			seen[tok] = true
			refs = append(refs, tok)
		}
	}
	return refs
}

// isPathRef reports whether a single cleaned token is a concrete file
// reference per the rules documented on extractIssuePathRefs.
func isPathRef(tok string) bool {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(tok), "."))
	if !pathRefExtensions[ext] {
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
) foremanv1alpha1.AgenticTaskVerdict {
	if extra == nil || issueBody == "" || len(diffFiles) == 0 {
		return verdict
	}
	refs := extractIssuePathRefs(issueBody)
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
