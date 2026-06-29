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
//   - Layer 1 (checkTestPresence): a changed package that adds net-new
//     functions in hand-written, non-test Go but has no changed _test.go fails.
//     Pure diff inspection, so it covers envtest/controller packages too.
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
// lines in `git diff` for the given file. Heuristic and intentionally simple:
// it keys the presence check on NET-NEW functions so behavior-preserving edits
// to existing funcs do not require a new test.
func addedFuncNames(ctx context.Context, workspace, file string, run commandRunner) []string {
	out, err := run(ctx, workspace, nil, "git", "diff", "--", file)
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

// checkTestPresence fails when a changed package introduces net-new functions
// in hand-written, non-test Go but has no changed _test.go in that package.
// Pure inspection -- no test execution -- so it covers envtest/controller
// packages the fast unit-test tier cannot run. Returns (failed, feedback).
func checkTestPresence(ctx context.Context, workspace string, run commandRunner) (bool, string) {
	changed := changedNonTestGoFiles(ctx, workspace, run)
	if len(changed) == 0 {
		return false, ""
	}
	testedPkgs := changedTestFilePackages(ctx, workspace, run)

	newFuncs := map[string][]string{} // pkgDir -> new func names
	for _, f := range changed {
		names := addedFuncNames(ctx, workspace, f, run)
		if len(names) == 0 {
			continue
		}
		dir := filepath.Dir(f)
		newFuncs[dir] = append(newFuncs[dir], names...)
	}

	var dirs []string
	for d := range newFuncs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	var b strings.Builder
	var failed bool
	for _, dir := range dirs {
		if testedPkgs[dir] {
			continue
		}
		failed = true
		fmt.Fprintf(&b, "Package %s/ adds new functions (%s) but has no changed _test.go. "+
			"Add a test that exercises the new behavior and fails without it.\n",
			dir, strings.Join(dedupSorted(newFuncs[dir]), ", "))
	}
	if !failed {
		return false, ""
	}
	return true, b.String()
}

// hunkHeaderRe captures the new-file start line and length from a unified-diff
// hunk header: "@@ -a,b +c,d @@".
var hunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// changedNewLines parses `git diff -U0 -- <file>` and returns the set of
// new-file line numbers touched by added lines.
func changedNewLines(ctx context.Context, workspace, file string, run commandRunner) map[int]bool {
	out, err := run(ctx, workspace, nil, "git", "diff", "-U0", "--", file)
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
func neuterAndTestPackage(ctx context.Context, workspace, pkgDir string, files []string, run commandRunner) ([]string, error) {
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
