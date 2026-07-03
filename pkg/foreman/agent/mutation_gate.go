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

// Package-local mutation gate: two deterministic coder-gate checks that catch
// changes which pass the syntactic checks (gofmt/vet/build/lint) but are not
// actually constrained by a test (the #856 class).
//
//   - Layer 1 (checkTestPresence): for each changed package, every new or
//     body-modified function in hand-written, non-test Go must be referenced
//     by name in a changed _test.go in that package; unreferenced functions
//     fail. Pure diff/text inspection, so it covers envtest/controller
//     packages too.
//   - Layer 2 (checkMutationSurvival): for non-envtest changed packages that DO
//     have a changed test, blank the changed function bodies on an in-memory
//     backup, re-run the package tests, and flag any package whose tests still
//     pass (the tests do not bite). Files are always restored.
//
// Both are disabled by FOREMAN_MUTATION_GATE=0. Neuter-survival for controller
// packages runs in the post-push envtest gate Job (v1.1), not here.
package agent

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// generatedMarker matches the canonical "// Code generated ... DO NOT EDIT."
// line that conventional Go generators emit.
var generatedMarker = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// isGeneratedGoFile reports whether a workspace-relative .go path is generated
// and therefore exempt from the mutation gate's presence and neuter checks.
// Name heuristics catch the common cases without I/O; the DO NOT EDIT marker is
// checked against the file head for the rest. A read error is treated as
// not-generated (the checks then apply, which is the safe default).
func isGeneratedGoFile(workspace, path string) bool {
	base := filepath.Base(path)
	if strings.HasPrefix(base, "zz_generated.") || strings.HasSuffix(base, ".pb.go") {
		return true
	}
	data, err := os.ReadFile(filepath.Join(workspace, path))
	if err != nil {
		return false
	}
	head := data
	if len(head) > 2048 {
		head = head[:2048]
	}
	return generatedMarker.Match(head)
}

// changedNonTestGoFiles returns workspace-relative paths of changed .go files
// that are neither tests nor generated -- i.e. the hand-written logic files the
// presence and neuter checks judge. Mirrors changedPackages' git porcelain
// parsing. A git error yields nil (checks skip rather than fail spuriously).
func changedNonTestGoFiles(ctx context.Context, workspace string, run commandRunner) []string {
	out, err := run(ctx, workspace, nil, "git", "status", "-z")
	if err != nil {
		return nil
	}
	var files []string
	for _, entry := range strings.Split(out, "\x00") {
		fields := strings.Fields(strings.TrimSpace(entry))
		if len(fields) == 0 {
			continue
		}
		path := fields[len(fields)-1] // rename "-> new" leaves the new path last
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		if isGeneratedGoFile(workspace, path) {
			continue
		}
		files = append(files, path)
	}
	return files
}

// mutationGateDisabled reports whether the mutation gate (presence + neuter) is
// turned off via FOREMAN_MUTATION_GATE=0. Default (unset) is enabled.
func mutationGateDisabled() bool {
	return os.Getenv("FOREMAN_MUTATION_GATE") == "0"
}

// addedFuncRe matches an added unified-diff line introducing a Go function or
// method declaration, e.g. "+func (r *R) Foo(" or "+func Bar(".
var addedFuncRe = regexp.MustCompile(`^\+\s*func\b[^/]*\(`)

// addedFuncNames returns the names of functions/methods introduced by added
// lines in `git diff HEAD` for the given file. Heuristic and intentionally
// simple: it keys the presence check on NET-NEW functions so behavior-preserving
// edits to existing funcs do not require a new test.
//
// The diff is taken against HEAD (not the plain working-tree diff) so it is
// independent of whether an earlier gate staged the tree. checkScopeOverlap
// runs `git add -A` before this check (#906/#962); a plain `git diff` would then
// see an empty (working-tree == index) diff and report zero new funcs, silently
// blinding checkTestPresence/checkMutationSurvival on the GO path (#907). Since
// the coder's work is uncommitted, HEAD is the pre-work base, so `git diff HEAD`
// is exactly "the coder's changes" whether or not they are staged.
func addedFuncNames(ctx context.Context, workspace, file string, run commandRunner) []string {
	out, err := run(ctx, workspace, nil, "git", "diff", "HEAD", "--", file)
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if !addedFuncRe.MatchString(line) {
			continue
		}
		names = append(names, funcNameFromDecl(strings.TrimPrefix(line, "+")))
	}
	return names
}

// funcNameFromDecl extracts the identifier from a "func ..." declaration line,
// handling both "func Name(" and "func (recv T) Name(".
func funcNameFromDecl(decl string) string {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(decl), "func"))
	if strings.HasPrefix(s, "(") { // method: skip the receiver
		if i := strings.Index(s, ")"); i >= 0 {
			s = strings.TrimSpace(s[i+1:])
		}
	}
	if i := strings.IndexAny(s, "([ "); i >= 0 {
		s = s[:i]
	}
	return s
}

// changedTestFilePackages returns the set of package dirs that have a changed
// _test.go file.
func changedTestFilePackages(ctx context.Context, workspace string, run commandRunner) map[string]bool {
	out, err := run(ctx, workspace, nil, "git", "status", "-z")
	if err != nil {
		return map[string]bool{}
	}
	set := map[string]bool{}
	for _, entry := range strings.Split(out, "\x00") {
		fields := strings.Fields(strings.TrimSpace(entry))
		if len(fields) == 0 {
			continue
		}
		path := fields[len(fields)-1]
		if strings.HasSuffix(path, "_test.go") {
			set[filepath.Dir(path)] = true
		}
	}
	return set
}

// changedTestFilesInDir returns workspace-relative paths of changed _test.go
// files in the given package directory.
func changedTestFilesInDir(ctx context.Context, workspace, dir string, run commandRunner) []string {
	out, err := run(ctx, workspace, nil, "git", "status", "-z")
	if err != nil {
		return nil
	}
	var files []string
	for _, entry := range strings.Split(out, "\x00") {
		fields := strings.Fields(strings.TrimSpace(entry))
		if len(fields) == 0 {
			continue
		}
		path := fields[len(fields)-1]
		if strings.HasSuffix(path, "_test.go") && filepath.Dir(path) == dir {
			files = append(files, path)
		}
	}
	return files
}

// hunkFuncRe matches the trailing function context in a unified-diff hunk
// header: "@@ ... @@ func Name(" or "@@ ... @@ func (recv T) Name(".
var hunkFuncRe = regexp.MustCompile(`@@[^@]*@@\s+func\b.*`)

// modifiedFuncNames returns the names of functions whose bodies were touched
// (but not necessarily introduced) by the diff of the given file. It parses
// `git diff -U0 HEAD -- <file>` and extracts the enclosing function name from the
// hunk header trailing context that Go emits when the hunk falls inside a
// function. Functions already captured by addedFuncNames (net-new +func lines)
// are included here too; the caller deduplicates via a union set.
//
// The diff is taken against HEAD (not the plain working-tree diff) so it is
// independent of whether an earlier gate staged the tree: checkScopeOverlap runs
// `git add -A` before this check (#906/#962), which would make a plain
// `git diff` empty and blind checkTestPresence/checkMutationSurvival (#907).
// The coder's work is uncommitted, so HEAD is the pre-work base.
func modifiedFuncNames(ctx context.Context, workspace, file string, run commandRunner) []string {
	out, err := run(ctx, workspace, nil, "git", "diff", "-U0", "HEAD", "--", file)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "@@") {
			continue
		}
		if !hunkFuncRe.MatchString(line) {
			continue
		}
		// Extract the func declaration that follows the final "@@".
		idx := strings.LastIndex(line, "@@")
		if idx < 0 {
			continue
		}
		decl := strings.TrimSpace(line[idx+2:])
		if !strings.HasPrefix(decl, "func") {
			continue
		}
		name := funcNameFromDecl(decl)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// testFileReferencesFunc reports whether the _test.go file at the given
// workspace-relative path contains the identifier funcName as a bare word.
// The check is a simple substring scan for the identifier surrounded by
// non-identifier runes (or at a line boundary) so it avoids false positives
// from names that are prefixes of longer identifiers.
func testFileReferencesFunc(workspace, path, funcName string) bool {
	if funcName == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(workspace, path))
	if err != nil {
		return false
	}
	// Use a word-boundary regex: the func name must not be immediately preceded
	// or followed by a letter, digit or underscore.
	re := regexp.MustCompile(`(?m)(^|[^a-zA-Z0-9_])` + regexp.QuoteMeta(funcName) + `([^a-zA-Z0-9_]|$)`)
	return re.Match(data)
}

func dedupSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// checkTestPresence fails when a changed package has new or body-modified
// functions in hand-written, non-test Go that are not referenced by name in
// any changed _test.go in that package. Pure inspection -- no test execution
// -- so it covers envtest/controller packages the fast unit-test tier cannot
// run. Returns (failed, feedback).
func checkTestPresence(ctx context.Context, workspace string, run commandRunner) (bool, string) {
	changed := changedNonTestGoFiles(ctx, workspace, run)
	if len(changed) == 0 {
		return false, ""
	}

	// changedFuncs maps pkgDir -> deduplicated set of function names that are
	// either net-new (+func line) or body-modified (hunk inside an existing
	// func, captured via the hunk-header trailing context).
	changedFuncs := map[string]map[string]bool{}
	for _, f := range changed {
		dir := filepath.Dir(f)
		added := addedFuncNames(ctx, workspace, f, run)
		modified := modifiedFuncNames(ctx, workspace, f, run)
		all := append(added, modified...) //nolint:gocritic // intentional extend
		for _, name := range all {
			if name == "" {
				continue
			}
			if changedFuncs[dir] == nil {
				changedFuncs[dir] = map[string]bool{}
			}
			changedFuncs[dir][name] = true
		}
	}

	if len(changedFuncs) == 0 {
		return false, ""
	}

	var dirs []string
	for d := range changedFuncs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	var b strings.Builder
	var failed bool
	for _, dir := range dirs {
		funcSet := changedFuncs[dir]
		testFiles := changedTestFilesInDir(ctx, workspace, dir, run)

		var unreferenced []string
		for name := range funcSet {
			referenced := false
			for _, tf := range testFiles {
				if testFileReferencesFunc(workspace, tf, name) {
					referenced = true
					break
				}
			}
			if !referenced {
				unreferenced = append(unreferenced, name)
			}
		}
		if len(unreferenced) == 0 {
			continue
		}
		failed = true
		sort.Strings(unreferenced)
		fmt.Fprintf(&b, "Package %s/ changed functions (%s) have no test referencing them by name. "+
			"Add a test that exercises the new behavior and fails without it.\n",
			dir, strings.Join(unreferenced, ", "))
	}
	if !failed {
		return false, ""
	}
	return true, b.String()
}

// hunkHeaderRe captures the new-file start line and length from a unified-diff
// hunk header: "@@ -a,b +c,d @@".
var hunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// changedNewLines parses `git diff -U0 HEAD -- <file>` and returns the set of
// new-file line numbers touched by added lines.
//
// The diff is taken against HEAD (not the plain working-tree diff) so it is
// independent of whether an earlier gate staged the tree: checkScopeOverlap runs
// `git add -A` before checkMutationSurvival (#906/#962), which would make a plain
// `git diff` empty and leave nothing to neuter, auto-passing the survival check
// (#907). The coder's work is uncommitted, so HEAD is the pre-work base.
func changedNewLines(ctx context.Context, workspace, file string, run commandRunner) map[int]bool {
	out, err := run(ctx, workspace, nil, "git", "diff", "-U0", "HEAD", "--", file)
	if err != nil {
		return nil
	}
	lines := map[int]bool{}
	cur := 0
	for _, ln := range strings.Split(out, "\n") {
		if m := hunkHeaderRe.FindStringSubmatch(ln); m != nil {
			cur, _ = strconv.Atoi(m[1])
			continue
		}
		if strings.HasPrefix(ln, "+") && !strings.HasPrefix(ln, "+++") {
			lines[cur] = true
			cur++
		}
	}
	return lines
}

// neuterFuncsInSource parses Go source and replaces the body of every function
// whose line span intersects changedLines with `panic("mutation-gate: ...")`.
// Returns the rewritten source and the names of the funcs it neutered. A
// declaration with no body (interface methods, externals) is skipped.
//
// Panic semantics: a test that EXECUTES a neutered function panics and fails
// (the mutant is "killed"); a test that never executes it still passes (the
// mutant "survives"). So the survival signal is precisely "the changed function
// is not exercised by any test" -- a coverage gap on the new logic. The
// stronger "executed but result not asserted" case (return-value mutation) is a
// v1.1 refinement.
func neuterFuncsInSource(filename string, src []byte, changedLines map[int]bool) ([]byte, []string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}
	var neutered []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Body.Lbrace).Line
		end := fset.Position(fn.Body.Rbrace).Line
		if !rangeIntersects(changedLines, start, end) {
			continue
		}
		fn.Body.List = []ast.Stmt{
			&ast.ExprStmt{X: &ast.CallExpr{
				Fun:  ast.NewIdent("panic"),
				Args: []ast.Expr{&ast.BasicLit{Kind: token.STRING, Value: `"mutation-gate: neutered"`}},
			}},
		}
		neutered = append(neutered, fn.Name.Name)
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), neutered, nil
}

// rangeIntersects reports whether any line in [start,end] is in the set.
func rangeIntersects(lines map[int]bool, start, end int) bool {
	for l := start; l <= end; l++ {
		if lines[l] {
			return true
		}
	}
	return false
}

// maxNeuterPackages bounds how many packages the neuter check will mutate in
// one gate run, so a sprawling change cannot blow the loop's time budget.
const maxNeuterPackages = 5

// checkMutationSurvival neuters the changed functions in each non-envtest
// changed package that has a changed _test.go, re-runs that package's tests on
// an in-memory-backed-up copy, and flags any package whose tests still PASS
// (the change is unconstrained). Files are always restored. Returns
// (failed, feedback).
//
// Safety/conservatism: if the neutered package fails to compile or its tests
// fail, that is treated as "tests bite" (no survivor) -- the check never fails
// the coder on its own mutation noise. Layer 1 already covers packages with no
// test at all. Controller/envtest packages are skipped here; their
// neuter-survival runs in the post-push gate Job (v1.1).
func checkMutationSurvival(ctx context.Context, workspace string, run commandRunner) (bool, string) {
	if mutationGateDisabled() {
		return false, ""
	}
	changed := changedNonTestGoFiles(ctx, workspace, run)
	if len(changed) == 0 {
		return false, ""
	}
	testedPkgs := changedTestFilePackages(ctx, workspace, run)

	byPkg := map[string][]string{}
	for _, f := range changed {
		dir := filepath.Dir(f)
		if !testedPkgs[dir] {
			continue // Layer 1 owns "no test"
		}
		if isEnvtestPackage("./" + dir + "/") {
			continue // v1.1 handles these in the gate Job
		}
		byPkg[dir] = append(byPkg[dir], f)
	}

	var dirs []string
	for d := range byPkg {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	if len(dirs) > maxNeuterPackages {
		dirs = dirs[:maxNeuterPackages]
	}

	var b strings.Builder
	var failed bool
	for _, dir := range dirs {
		survivors, err := neuterAndTestPackage(ctx, workspace, dir, byPkg[dir], run)
		if err != nil || len(survivors) == 0 {
			continue
		}
		failed = true
		fmt.Fprintf(&b, "Package %s/: its tests still PASS when the bodies of %s are removed, "+
			"so they do not actually test the new logic. Add or strengthen a test that fails "+
			"without the real implementation.\n", dir, strings.Join(dedupSorted(survivors), ", "))
	}
	if !failed {
		return false, ""
	}
	return true, b.String()
}

// neuterAndTestPackage backs up and neuters the changed funcs in the given
// files, runs the package's tests, and restores the files (always). It returns
// the neutered func names IF the tests passed under neuter (survivors), else
// nil. A read/write error returns that error so the caller skips the package.
func neuterAndTestPackage(
	ctx context.Context, workspace, pkgDir string, files []string, run commandRunner,
) ([]string, error) {
	type backup struct {
		path string
		data []byte
	}
	var backups []backup
	var allNeutered []string

	restore := func() {
		for _, bk := range backups {
			_ = os.WriteFile(filepath.Join(workspace, bk.path), bk.data, 0o644)
		}
	}
	defer restore()

	for _, f := range files {
		full := filepath.Join(workspace, f)
		orig, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		backups = append(backups, backup{path: f, data: orig})
		lines := changedNewLines(ctx, workspace, f, run)
		if len(lines) == 0 {
			continue
		}
		out, neutered, err := neuterFuncsInSource(f, orig, lines)
		if err != nil || len(neutered) == 0 {
			continue
		}
		if err := os.WriteFile(full, out, 0o644); err != nil {
			return nil, err
		}
		allNeutered = append(allNeutered, neutered...)
	}
	if len(allNeutered) == 0 {
		return nil, nil
	}

	// Run the package's tests under neuter. A nil error means they PASSED with
	// the logic removed -> survivors. A non-nil error (build break or failing
	// test) means the tests bit -> no survivor.
	_, err := run(ctx, workspace, nil, "go", "test", "-count=1", "-timeout=180s", "./"+pkgDir+"/")
	if err != nil {
		return nil, nil
	}
	return allNeutered, nil
}
