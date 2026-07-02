package grounding

import (
	"context"
	"regexp"
	"strings"
)

// annotationKeyRe matches an LLMKube annotation/label key string literal:
// a double-quoted token of the form <ns>/<name> where <ns> ends in
// llmkube.dev or llmkube.io. CLI flags ("--foo") and bare identifiers do not
// match (no namespace slash, no llmkube domain), which is the #277 guard.
var annotationKeyRe = regexp.MustCompile(`"([a-z0-9.-]*llmkube\.(?:dev|io)/[a-z0-9._-]+)"`)

// DetectInertSymbols flags newly-added LLMKube annotation/label keys (in
// non-test .go files) that have no reader: the key literal occurs only at its
// adding site and nowhere else in the repo. It greps the workspace for each
// key via run. Posture is escalate-not-fail; the caller (Plan 2) demotes the
// verdict and hands these to the reviewer. Returns nil on any diff error
// (fail-open).
func DetectInertSymbols(ctx context.Context, workspace string, run CommandRunner, base string) []Finding {
	added, err := AddedLines(ctx, workspace, run, base, []string{"*.go"})
	if err != nil {
		return nil
	}
	seen := map[string]AddedLine{}
	var order []string
	for _, al := range added {
		if strings.HasSuffix(al.File, "_test.go") {
			continue
		}
		for _, m := range annotationKeyRe.FindAllStringSubmatch(al.Text, -1) {
			if _, ok := seen[m[1]]; !ok {
				seen[m[1]] = al
				order = append(order, m[1])
			}
		}
	}
	var findings []Finding
	for _, key := range order {
		al := seen[key]
		out, err := run(ctx, workspace, nil, "grep", "-rFn", "--include=*.go", key, ".")
		if err != nil {
			// grep exits 1 when there are no matches; that would mean not even
			// the writer is present -> treat as no reader and skip (defensive).
			continue
		}
		if countNonEmptyLines(out) <= 1 {
			findings = append(findings, Finding{
				Severity: SeverityMajor, Area: "wired-up",
				File: al.File, Line: al.Line,
				Message: "annotation key " + key +
					" is written but never read (inert);" +
					" confirm a consumer exists or the change is a no-op",
			})
		}
	}
	return findings
}

func countNonEmptyLines(s string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}
