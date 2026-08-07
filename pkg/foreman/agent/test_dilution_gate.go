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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// fileHunks holds the content (leading +/-/space stripped) of one file's added,
// removed, and context lines from a `git diff` output.
//
// Context is populated only when the caller requested a non-zero --unified;
// under --unified=0 git emits no context lines and the slice stays nil. It
// exists because a Go command-construction call is routinely spread over
// several lines, so the changed line alone cannot show that it belongs to one
// (see hasCommandStringChange).
type fileHunks struct {
	Added   []string
	Removed []string
	Context []string
}

// parseUnifiedDiff parses `git diff --unified=0 --src-prefix=a/ --dst-prefix=b/`
// output into per-file added and removed content lines. Added lines are keyed
// by the new-file path (+++ b/PATH); removed lines are keyed by the same path,
// or by the old path (--- a/PATH) when the new side is /dev/null (a deletion).
// Diff headers (---, +++) are never counted as content.
func parseUnifiedDiff(out string) map[string]*fileHunks {
	byFile := map[string]*fileHunks{}
	ensure := func(f string) *fileHunks {
		if byFile[f] == nil {
			byFile[f] = &fileHunks{}
		}
		return byFile[f]
	}
	var cur, aPath string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "--- a/"):
			aPath = strings.TrimPrefix(line, "--- a/")
		case strings.HasPrefix(line, "--- "):
			aPath = "" // /dev/null (added file) etc.
		case strings.HasPrefix(line, "+++ b/"):
			cur = strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "+++ "):
			cur = aPath // deletion: attribute removed lines to the old path
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") && cur != "":
			ensure(cur).Added = append(ensure(cur).Added, line[1:])
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			key := cur
			if key == "" {
				key = aPath
			}
			if key != "" {
				ensure(key).Removed = append(ensure(key).Removed, line[1:])
			}
		case strings.HasPrefix(line, " ") && cur != "":
			// Context line. Only present when the caller passed a non-zero
			// --unified; harmless (and empty) for --unified=0 callers.
			ensure(cur).Context = append(ensure(cur).Context, line[1:])
		}
	}
	return byFile
}

// assertionTokens are substrings that mark a line as an assertion. Kept
// deliberately small and shape-based: Gomega (Expect/Ω), testify
// (assert./require.), the ContainSubstring matcher, the stdlib t.Error/t.Fatal
// failures, and the got/want comparison idiom.
var assertionTokens = []string{
	"Expect(", "Ω(", "assert.", "require.", "ContainSubstring(",
	"t.Error", "t.Fatal", "!= want", "want !=", "got !=", "!= got",
}

// isAssertionLine reports whether a diff content line looks like a test
// assertion. It is intentionally lenient: false positives only add reviewer
// context, never fail a gate.
func isAssertionLine(s string) bool {
	t := strings.TrimSpace(s)
	for _, tok := range assertionTokens {
		if strings.Contains(t, tok) {
			return true
		}
	}
	return false
}

// assertionErosion counts assertion-shaped removed and added lines in one
// file's hunks and returns the trimmed text of the removed assertions (for the
// reviewer message). Net erosion is removed > added; the caller applies that.
func assertionErosion(fh *fileHunks) (removed, added int, snippets []string) {
	for _, l := range fh.Removed {
		if isAssertionLine(l) {
			removed++
			snippets = append(snippets, strings.TrimSpace(l))
		}
	}
	for _, l := range fh.Added {
		if isAssertionLine(l) {
			added++
		}
	}
	return removed, added, snippets
}

// firstN returns the first n elements of s, or all of s when shorter. Used to
// cap how many removed-assertion snippets appear in the advisory.
func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

var (
	// urlHostRe captures the host of an http(s) URL string literal.
	urlHostRe = regexp.MustCompile(`https?://([^/\s"'` + "`" + `]+)`)
	// testdataPathRe captures a testdata/ file path literal.
	testdataPathRe = regexp.MustCompile(`[\w./-]*testdata/[\w./-]+`)
	// assertionValueRe captures the matcher name and expected value in
	// Gomega-style assertions like Equal(N) or ContainSubstring(N).
)

// fixtureTokens extracts fixture-input identifiers from a set of diff lines:
// URL hosts (prefixed "host:") and testdata/ paths (prefixed "path:"). The
// prefix keeps the two kinds from colliding in the set-difference below.
func fixtureTokens(lines []string) map[string]bool {
	set := map[string]bool{}
	for _, l := range lines {
		for _, m := range urlHostRe.FindAllStringSubmatch(l, -1) {
			set["host:"+m[1]] = true
		}
		for _, m := range testdataPathRe.FindAllString(l, -1) {
			set["path:"+m] = true
		}
	}
	return set
}

// fixtureLiteralChurn flags a fixture input that was changed in place: a host
// or testdata path present only on the removed side while a different one
// appears only on the added side. This is relocation (the #1322 signature),
// distinct from a pure fixture addition (added only) or deletion (removed
// only), both of which return nil. Deterministic: tokens are sorted.
func fixtureLiteralChurn(fh *fileHunks) []string {
	rem := fixtureTokens(fh.Removed)
	add := fixtureTokens(fh.Added)
	var gone, appeared []string
	for tkn := range rem {
		if !add[tkn] {
			gone = append(gone, tkn)
		}
	}
	for tkn := range add {
		if !rem[tkn] {
			appeared = append(appeared, tkn)
		}
	}
	if len(gone) == 0 || len(appeared) == 0 {
		return nil
	}
	sort.Strings(gone)
	sort.Strings(appeared)
	return []string{fmt.Sprintf("fixture input changed (removed %v, added %v)", gone, appeared)}
}

// assertionValueChurn flags an assertion whose expected value was rewritten
// in place: a matcher+value pair (e.g. Equal(N)) present only on the removed
// side while a different one appears only on the added side. This is the
// #1347 shape: the coder changed the expected value to match buggy behavior.
// Pure additions or deletions of assertions are not churn (those are caught
// by assertionErosion). Deterministic: tokens are sorted.
func assertionValueChurn(fh *fileHunks) []string {
	rem := assertionValueTokens(fh.Removed)
	add := assertionValueTokens(fh.Added)
	var gone, appeared []string
	for tkn := range rem {
		if !add[tkn] {
			gone = append(gone, tkn)
		}
	}
	for tkn := range add {
		if !rem[tkn] {
			appeared = append(appeared, tkn)
		}
	}
	if len(gone) == 0 || len(appeared) == 0 {
		return nil
	}
	sort.Strings(gone)
	sort.Strings(appeared)
	return []string{fmt.Sprintf("assertion value changed (removed %v, added %v)", gone, appeared)}
}

// assertionValueTokens extracts matcher+value pairs from assertion lines:
// e.g. "Equal(42)" or "ContainSubstring(boom)". The key is the full
// matcher+value string so that Equal(42) vs Equal(0) is a churn signal.
func assertionValueTokens(lines []string) map[string]bool {
	set := map[string]bool{}
	for _, l := range lines {
		for _, tok := range extractAssertionTokens(l) {
			set[tok] = true
		}
	}
	return set
}

// assertionMatchers are the matcher names whose expected value is compared.
var assertionMatchers = []string{"Equal", "ContainSubstring"}

// extractAssertionTokens pulls "Equal(...)" / "ContainSubstring(...)" spans out
// of a line using BALANCED paren matching, not a `[^)]+` run. A regex stops at
// the first ')', so Equal(f(x), 42) truncates to "Equal(f(x)" and a rewrite of
// the trailing 42 yields an identical token on both sides -- a silent miss, the
// wrong failure direction for a dilution signal.
func extractAssertionTokens(line string) []string {
	var out []string
	for _, name := range assertionMatchers {
		for off := 0; off < len(line); {
			i := strings.Index(line[off:], name+"(")
			if i < 0 {
				break
			}
			start := off + i
			// Reject a match inside a longer identifier (e.g. NotEqual().
			if start > 0 && isIdentByte(line[start-1]) {
				off = start + 1
				continue
			}
			end := matchParen(line, start+len(name))
			if end < 0 {
				off = start + 1
				continue
			}
			out = append(out, normalizeAssertionToken(line[start:end+1]))
			off = end + 1
		}
	}
	return out
}

// matchParen returns the index of the ')' closing the '(' at open, or -1.
// Parens inside string literals do not count toward the depth.
func matchParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '"', '`', '\'':
			if j := skipString(s, i); j > i {
				i = j
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// skipString returns the index of the closing quote for the literal opening at
// i, or i if unterminated.
func skipString(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\\' && q != '`' {
			j++
			continue
		}
		if s[j] == q {
			return j
		}
	}
	return i
}

// normalizeAssertionToken drops whitespace that sits OUTSIDE string literals,
// so a gofmt-driven respacing such as Equal( 42 ) does not read as churn
// against Equal(42). Whitespace inside a literal is preserved, because
// Equal("a b") and Equal("a  b") are genuinely different expectations.
func normalizeAssertionToken(tok string) string {
	var b strings.Builder
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if c == '"' || c == '`' || c == '\'' {
			end := skipString(tok, i)
			b.WriteString(tok[i : end+1])
			i = end
			continue
		}
		if c == ' ' || c == '\t' {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// nameStatusEntry is one file-level change from `git diff --name-status`.
type nameStatusEntry struct {
	Code    string // "M", "A", "D", "R100", "C75", ...
	Path    string // destination path (rename) or the changed path
	OldPath string // source path, only set for renames/copies
}

// parseNameStatus parses tab-separated `git diff --name-status` output. Rename
// and copy rows carry two paths (old, new); all others carry one.
func parseNameStatus(out string) []nameStatusEntry {
	var entries []nameStatusEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		code := f[0]
		if (strings.HasPrefix(code, "R") || strings.HasPrefix(code, "C")) && len(f) >= 3 {
			entries = append(entries, nameStatusEntry{Code: code, OldPath: f[1], Path: f[2]})
			continue
		}
		entries = append(entries, nameStatusEntry{Code: code, Path: f[len(f)-1]})
	}
	return entries
}

// changedProdPackages returns the set of package directories whose non-test Go
// source changed. A package that changed only its tests is not included: the
// linkage requires a production change to gate the test-dilution signal.
func changedProdPackages(entries []nameStatusEntry) map[string]bool {
	pkgs := map[string]bool{}
	for _, e := range entries {
		p := e.Path
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			continue
		}
		pkgs[filepath.Dir(p)] = true
	}
	return pkgs
}

// testdataOwner returns the package directory that owns a testdata path (the
// segment before "/testdata/"), or ("", false) when the path is not under a
// testdata directory. A top-level "testdata/..." is owned by ".".
func testdataOwner(path string) (string, bool) {
	if i := strings.Index(path, "/testdata/"); i >= 0 {
		return path[:i], true
	}
	if strings.HasPrefix(path, "testdata/") {
		return ".", true
	}
	return "", false
}

// fixtureFileChanges reports testdata fixtures that were deleted or renamed
// under a package whose production code changed. Deleting or moving a fixture
// is how coverage of a path can be dropped without touching an assertion.
func fixtureFileChanges(entries []nameStatusEntry, prodPkgs map[string]bool) []string {
	var out []string
	for _, e := range entries {
		// Resolve the fixture's owning package from its PRE-change location:
		// OldPath for a rename/copy (the package the fixture came from, which
		// could have lost coverage), Path for a plain delete (no OldPath).
		// Destination-first would miss a fixture moved OUT of a changed package
		// into an untouched one, the exact dilution this catches (#1332).
		lookup := e.Path
		if e.OldPath != "" {
			lookup = e.OldPath
		}
		owner, ok := testdataOwner(lookup)
		if !ok || !prodPkgs[owner] {
			continue
		}
		switch {
		case strings.HasPrefix(e.Code, "D"):
			out = append(out, "deleted fixture "+e.Path)
		case strings.HasPrefix(e.Code, "R"):
			out = append(out, "relocated fixture "+e.OldPath+" -> "+e.Path)
		}
	}
	return out
}

// checkTestDilution is a tierAdvisory gate check (#1332). It surfaces to the
// reviewer when a submission that changes production code also weakens the
// tests covering that code: net-removed assertions, a relocated fixture input,
// or a deleted/renamed testdata fixture, all scoped to a package whose
// production code changed. It never fails the gate and never feeds the coder.
//
// Fail-open: any git error, or no production change in the submission, returns
// (false, "") so a bad diff signal or a docs-only change stays silent.
func checkTestDilution(ctx context.Context, workspace string, run commandRunner) (bool, string) {
	// Stage the working tree so a pre-commit diff includes new/untracked files.
	// Idempotent with the executor's later `git add -A`; the -A exit status is
	// not actionable here, so a stage error simply fails the check open below.
	if _, err := run(ctx, workspace, nil, "git", "add", "-A"); err != nil {
		return false, ""
	}
	nsOut, err := run(ctx, workspace, nil, "git", "diff", "--name-status", "--cached", "HEAD")
	if err != nil {
		return false, ""
	}
	entries := parseNameStatus(nsOut)
	prodPkgs := changedProdPackages(entries)
	if len(prodPkgs) == 0 {
		return false, "" // no production change: not a green-gate-earning dilution
	}

	diffOut, err := run(ctx, workspace, nil, "git", "diff", "--cached", "--unified=0",
		"--src-prefix=a/", "--dst-prefix=b/", "HEAD", "--", "*_test.go")
	if err != nil {
		return false, ""
	}
	byFile := parseUnifiedDiff(diffOut)

	var findings []string
	for file, fh := range byFile {
		if !prodPkgs[filepath.Dir(file)] {
			continue // package linkage: only judge tests of changed-prod packages
		}
		if removed, added, snippets := assertionErosion(fh); removed > added {
			findings = append(findings, fmt.Sprintf(
				"%s net-removed %d assertion(s): %s",
				file, removed-added, strings.Join(firstN(snippets, 3), "; ")))
		}
		for _, c := range fixtureLiteralChurn(fh) {
			findings = append(findings, file+" "+c)
		}
		for _, c := range assertionValueChurn(fh) {
			findings = append(findings, file+" "+c)
		}
	}
	findings = append(findings, fixtureFileChanges(entries, prodPkgs)...)

	if len(findings) == 0 {
		return false, ""
	}
	sort.Strings(findings) // deterministic order across map iteration
	detail := "production code changed and its tests weakened their own coverage " +
		"(confirm the changed behavior is still covered, not dodged): " +
		strings.Join(findings, "; ")
	return true, truncateOutput(detail)
}
