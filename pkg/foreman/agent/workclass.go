package agent

import "path/filepath"

// workClass buckets a changed file into the honest-verdict policy classes
// (proposal 1075, section 3.1). Ordering in classifyFile is first match
// wins, so ci-policy beats the generic yaml bucket.
type workClass string

const (
	workClassCIPolicy      workClass = "ci-policy"
	workClassReleasePolicy workClass = "release-policy"
	workClassPackaging     workClass = "packaging"
	workClassDocs          workClass = "docs"
	workClassConfig        workClass = "config"
	workClassCodeFix       workClass = "code-fix"
	workClassMixed         workClass = "mixed"
)

// footprintDominance is the changed-line share a class must reach for the
// diff to take that class; below it the footprint is mixed.
const footprintDominance = 0.70

type classRule struct {
	globs []string
	class workClass
}

// classRules is evaluated in order; first matching glob wins. Globs match
// with path.Match against the full slash path and against the base name,
// plus a prefix form for directory trees ("dir/**").
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

func classifyFile(path string) workClass {
	for _, r := range classRules {
		for _, g := range r.globs {
			if matchGlob(g, path) {
				return r.class
			}
		}
	}
	return workClassCodeFix
}

func classifyFootprint(changed map[string]int) workClass {
	if len(changed) == 0 {
		return workClassCodeFix
	}
	total, byClass := 0, map[workClass]int{}
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
