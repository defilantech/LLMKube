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

// Package changepolicy provides a provider-neutral work-class classification
// and human-review gate for AgenticTask verdicts. It mirrors the GitHub-specific
// logic in workclass.go and verdict_policy.go, extracted behind an interface
// so future providers (Forgejo, Linear, etc.) can supply their own policies.
package changepolicy

import "path/filepath"

// WorkClass buckets a changed file into the honest-verdict policy classes
// (proposal 1075, section 3.1).
type WorkClass string

const (
	workClassCIPolicy      WorkClass = "ci-policy"
	workClassReleasePolicy WorkClass = "release-policy"
	workClassPackaging     WorkClass = "packaging"
	workClassDocs          WorkClass = "docs"
	workClassConfig        WorkClass = "config"
	workClassCodeFix       WorkClass = "code-fix"
	workClassMixed         WorkClass = "mixed"
)

// footprintDominance is the changed-line share a class must reach for the
// diff to take that class; below it the footprint is mixed.
const footprintDominance = 0.70

type classRule struct {
	globs []string
	class WorkClass
}

// classRules is evaluated in order; first matching glob wins. Globs match
// with path.Match against the full slash path and against the base name,
// plus a prefix form for directory trees (dir/**).
var classRules = []classRule{
	{[]string{".github/workflows/**", ".github/actions/**"}, workClassCIPolicy},
	{[]string{".goreleaser*", "release-please*"}, workClassReleasePolicy},
	{[]string{"Formula/**", "Dockerfile*", "charts/**", "*.spec",
		"hack/publish-*"}, workClassPackaging},
	{[]string{"*.md", "docs/**", "examples/**"}, workClassDocs},
	{[]string{"*.yaml", "*.yml", "*.toml", "*.json"}, workClassConfig},
}

func matchGlob(glob, path string) bool {
	if ok, _ := filepath.Match(glob, path); ok {
		return true
	}
	if ok, _ := filepath.Match(glob, filepath.Base(path)); ok {
		return true
	}
	// dir/** prefix form: filepath.Match does not cross separators.
	if len(glob) > 3 && glob[len(glob)-3:] == "/**" {
		prefix := glob[:len(glob)-2]
		return len(path) > len(prefix) && path[:len(prefix)] == prefix
	}
	return false
}

func classifyFile(path string) WorkClass {
	for _, r := range classRules {
		for _, g := range r.globs {
			if matchGlob(g, path) {
				return r.class
			}
		}
	}
	return workClassCodeFix
}

func classifyFootprint(changed map[string]int) WorkClass {
	if len(changed) == 0 {
		return workClassCodeFix
	}
	total, byClass := 0, map[WorkClass]int{}
	for f, n := range changed {
		total += n
		byClass[classifyFile(f)] += n
	}
	// Zero-line diffs (rename-only, permission-only) must not nondeterministically
	// classify as a self-GO-able class; mixed is the safe fallback.
	if total == 0 {
		return workClassMixed
	}
	for class, n := range byClass {
		if float64(n) >= footprintDominance*float64(total) {
			return class
		}
	}
	return workClassMixed
}

// ChangePolicy classifies changed paths into work classes and gates
// human review when a change falls outside the self-GO allowlist.
type ChangePolicy interface {
	// Classify returns the dominant work class for a set of changed paths
	// with their added+deleted line counts.
	Classify(changed map[string]int) WorkClass
	// RequiresHumanReview returns true when any changed path classifies
	// to a class NOT in the provided selfGO list.
	RequiresHumanReview(changedPaths []string, selfGO []string) bool
}

// NewDefaultPolicy returns the default ChangePolicy, which mirrors the
// GitHub-specific classification in workclass.go and the human-review
// gate in verdict_policy.go.
func NewDefaultPolicy() ChangePolicy {
	return defaultPolicy{}
}

// defaultPolicy is the default implementation that mirrors the
// workclass.go/verdict_policy.go logic.
type defaultPolicy struct{}

// Classify implements ChangePolicy.Classify.
func (defaultPolicy) Classify(changed map[string]int) WorkClass {
	return classifyFootprint(changed)
}

// RequiresHumanReview implements ChangePolicy.RequiresHumanReview.
func (defaultPolicy) RequiresHumanReview(changedPaths []string, selfGO []string) bool {
	// Build the footprint map from the path list (each path counts as 1
	// line for the purpose of this gate; the caller may pass a richer
	// map via Classify when line counts are available).
	changed := map[string]int{}
	for _, p := range changedPaths {
		changed[p] = 1
	}
	class := classifyFootprint(changed)
	return !workClassInList(class, selfGO)
}

// workClassInList reports whether class's string form appears in list.
// list is small (a handful of policy classes at most) so a linear scan
// is simplest; called at most a few times per GO verdict.
func workClassInList(class WorkClass, list []string) bool {
	for _, c := range list {
		if c == string(class) {
			return true
		}
	}
	return false
}
