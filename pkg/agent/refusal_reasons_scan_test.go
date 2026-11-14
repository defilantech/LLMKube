/*
Copyright 2026.

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

// This file replaces a hand-maintained table of "every refusal reason the
// agent uses" (which itself existed to guard against the drift behind a
// refusal the controller did not recognize, which then looped:
// ServiceNameTooLong was added to the agent in 0.10.0 with no matching entry
// in internal/controller/scheduling.go's agentRefusalReasons) with a
// structural scan: it parses this package's own non-test source with
// go/parser/go/ast (no type checking, no golang.org/x/tools/go/packages, no
// subprocess; pure syntax, so it stays fast) and finds every call to refuseStart and every direct
// assignment to Status.SchedulingStatus, then asserts each one's reason
// argument resolves to a known EventReason constant whose value is in
// inferencev1alpha1.MetalAgentRefusalReasons. A hand-maintained *table* of
// sites can go stale the moment someone adds a new refusal without updating
// it; a scan of the actual call sites cannot.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// knownReasonConstants maps every exported EventReason* identifier this
// package defines to its resolved string value. It exists purely to let the
// syntax-only scan below resolve an *ast.Ident without go/types: each entry
// here is a plain reference to the real constant, so Go itself already
// resolved the `= inferencev1alpha1.ReasonFoo` chain at compile time, and
// this map is exact by construction. A new EventReason* constant that is not
// added here makes the scan report its call site as "unresolvable" the next
// time this test runs, which is the intended fail-closed behavior: this map
// is a resolution table for identifiers, not a second copy of "which
// reasons are valid" (that question is answered once, by
// inferencev1alpha1.MetalAgentRefusalReasons).
var knownReasonConstants = map[string]string{
	"EventReasonEndpointNameConflict":   EventReasonEndpointNameConflict,
	"EventReasonModelSourceNotAllowed":  EventReasonModelSourceNotAllowed,
	"EventReasonExtraArgsRejected":      EventReasonExtraArgsRejected,
	"EventReasonServiceNameTooLong":     EventReasonServiceNameTooLong,
	"EventReasonModelDigestMismatch":    EventReasonModelDigestMismatch,
	"EventReasonModelDownloadTooLarge":  EventReasonModelDownloadTooLarge,
	"EventReasonSourceSecretNotAllowed": EventReasonSourceSecretNotAllowed,
	"EventReasonRuntimeNotAvailable":    EventReasonRuntimeNotAvailable,
	"EventReasonMemoryCheckFailed":      EventReasonMemoryCheckFailed,
	"EventReasonInsufficientMemory":     EventReasonInsufficientMemory,
	"EventReasonStartFailed":            EventReasonStartFailed,
}

// emptyStringLit is the source text go/ast.BasicLit.Value carries for the
// literal `""`, used to recognize (and ignore) a status-clearing assignment
// like `isvc.Status.SchedulingStatus = ""`: clearing is not a refusal.
const emptyStringLit = `""`

func TestMetalAgentRefusalReasons_CoversEveryAgentRefusalReason(t *testing.T) {
	known := make(map[string]bool, len(inferencev1alpha1.MetalAgentRefusalReasons))
	for _, r := range inferencev1alpha1.MetalAgentRefusalReasons {
		known[r] = true
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("glob found zero .go files; the scan is not running against the right directory")
	}

	fset := token.NewFileSet()
	sites := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		sites += scanFileForRefusalReasons(t, fset, file, known)
	}
	// If a future refactor renames refuseStart or restructures the
	// SchedulingStatus assignments this scan looks for, it would silently
	// stop finding anything and this test would pass vacuously. Pin the
	// current site count as a floor so that kind of breakage is loud.
	const minExpectedSites = 19
	if sites < minExpectedSites {
		t.Fatalf("scan found %d refusal reason site(s), want at least %d; either a refusal reason "+
			"was removed or the scan itself no longer recognizes the current call-site shape", sites, minExpectedSites)
	}
}

// scanFileForRefusalReasons walks file's top-level function declarations for
// refuseStart calls and Status.SchedulingStatus assignments, resolves each
// reason expression, and asserts it against known. Returns the number of
// sites it checked.
func scanFileForRefusalReasons(t *testing.T, fset *token.FileSet, file *ast.File, known map[string]bool) int {
	t.Helper()
	sites := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		// refuseStart's own body assigns its `reason` PARAMETER to
		// Status.SchedulingStatus: that is generic plumbing every call site
		// already resolves a constant for before reaching it, not a second
		// refusal site to check. refuseStart is never called from within
		// itself, so this only needs to guard the assignment scan below, not
		// the call scan.
		isRefuseStartDef := fn.Name.Name == "refuseStart"

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "refuseStart" {
					return true
				}
				if len(node.Args) < 3 {
					t.Errorf("%s: refuseStart call has %d arg(s), want at least 3 (ctx, isvc, reason, ...)",
						fset.Position(node.Pos()), len(node.Args))
					return true
				}
				sites++
				checkReasonArg(t, fset, node.Args[2], known)
			case *ast.AssignStmt:
				if isRefuseStartDef {
					return true
				}
				for i, lhs := range node.Lhs {
					if !isSchedulingStatusSelector(lhs) || i >= len(node.Rhs) {
						continue
					}
					if isEmptyStringLit(node.Rhs[i]) {
						continue // clearing the status is not a refusal
					}
					sites++
					checkReasonArg(t, fset, node.Rhs[i], known)
				}
			}
			return true
		})
	}
	return sites
}

// isSchedulingStatusSelector reports whether expr is a "<x>.Status.SchedulingStatus"
// selector chain.
func isSchedulingStatusSelector(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "SchedulingStatus" {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	return ok && inner.Sel.Name == "Status"
}

// isEmptyStringLit reports whether expr is the literal `""`.
func isEmptyStringLit(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && lit.Value == emptyStringLit
}

// checkReasonArg resolves expr (a refusal reason) via knownReasonConstants
// and asserts the resolved value is in known. A raw string literal, or any
// expression this scan does not recognize as a named constant, fails the
// test explicitly rather than being silently skipped.
func checkReasonArg(t *testing.T, fset *token.FileSet, expr ast.Expr, known map[string]bool) {
	t.Helper()
	pos := fset.Position(expr.Pos())

	ident, ok := expr.(*ast.Ident)
	if !ok {
		if lit, isLit := expr.(*ast.BasicLit); isLit {
			t.Errorf("%s: refusal reason is a raw string literal (%s), not a named EventReason constant",
				pos, lit.Value)
			return
		}
		t.Errorf("%s: refusal reason is an unresolvable expression (%T); use a named EventReason "+
			"constant so this scan can verify it", pos, expr)
		return
	}

	value, ok := knownReasonConstants[ident.Name]
	if !ok {
		t.Errorf("%s: refusal reason %s is not in knownReasonConstants; add it there (mapped to the "+
			"real constant) and confirm its value is in inferencev1alpha1.MetalAgentRefusalReasons",
			pos, ident.Name)
		return
	}
	if !known[value] {
		t.Errorf("%s: refusal reason %s (%q) is not in inferencev1alpha1.MetalAgentRefusalReasons; "+
			"the controller's determinePhase would overwrite this refusal with WaitingForMetalAgent "+
			"on every reconcile", pos, ident.Name, value)
	}
}
