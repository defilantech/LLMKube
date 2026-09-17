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

package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/tools/catalog"
)

// TestBuildAll_ThreadsGateActiveDeadlineSeconds is the regression test for
// #1748: a configured GateActiveDeadlineSeconds on ToolDeps must reach the
// RunGateJobTool's own config so the gate Job's activeDeadlineSeconds is
// settable from the verifier Agent. Zero keeps the 1800 s default via
// applyConfigDefaults (covered by TestApplyConfigDefaults_FillsEveryField).
func TestBuildAll_ThreadsGateActiveDeadlineSeconds(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps ToolDeps
		want int32
	}{
		{"configured deadline reaches the tool", ToolDeps{GateActiveDeadlineSeconds: 7200}, 7200},
		{"zero stays zero before defaults", ToolDeps{GateActiveDeadlineSeconds: 0}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := BuildAll(tc.deps)
			var got *RunGateJobTool
			for _, tool := range tools {
				if gt, ok := tool.(*RunGateJobTool); ok {
					got = gt
					break
				}
			}
			if got == nil {
				t.Fatal("BuildAll did not construct a *RunGateJobTool")
			}
			if got.Cfg.ActiveDeadlineSeconds != tc.want {
				t.Errorf("RunGateJobTool.Cfg.ActiveDeadlineSeconds: want %d got %d",
					tc.want, got.Cfg.ActiveDeadlineSeconds)
			}
		})
	}
}

// The webhook allow-list must match the tools that actually exist.
//
// A tool registered at runtime but missing from catalog is unusable in both
// directions: naming it in spec.tools is rejected by admission, and omitting it
// means the runtime Filter() never surfaces it to the model. v0.9.16 shipped
// three tools in exactly that state (#1482).
//
// The previous guard could not catch it. It compared catalog against a
// hand-written canonicalToolSet() in the test, while the real registration
// lived in a third copy inside cmd/foreman-agent/main.go. Both mirrors stayed
// stale together and the test kept passing.
//
// This derives one side from BuildAll, the same function main.go calls, so the
// comparison is against reality rather than against a copy of it.
func TestBuildAllMatchesCatalog(t *testing.T) {
	built := BuildAll(ToolDeps{Workspace: t.TempDir()})

	got := make([]string, 0, len(built))
	for _, tool := range built {
		got = append(got, tool.Name())
	}
	sort.Strings(got)

	want := catalog.CanonicalToolNames()
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("BuildAll exposes %d tools, catalog allows %d\n  built:   %v\n  catalog: %v",
			len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("tool set differs at index %d: built %q, catalog %q\n"+
				"  built:   %v\n  catalog: %v\n"+
				"a tool present in one and not the other is unusable: admission "+
				"rejects an unknown name, and an unregistered name never reaches "+
				"the model", i, got[i], want[i], got, want)
		}
	}
}

// Duplicate names would let one tool silently shadow another in the registry,
// and the length check above would still pass if a name were repeated while
// another went missing.
func TestBuildAllNamesAreUnique(t *testing.T) {
	seen := map[string]int{}
	for _, tool := range BuildAll(ToolDeps{Workspace: t.TempDir()}) {
		seen[tool.Name()]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("tool name %q is registered %d times", name, n)
		}
	}
}

// Tool types deliberately NOT wired into BuildAll, with why.
//
// BuildAll is the model-facing registry: everything it constructs reaches a
// model's tool whitelist through admission (catalog). RunScanJobTool is
// dispatched by the executor through the agent.ScanJobRunner seam instead —
// a scan is a deterministic CI reproduce step, and a model that could call it
// directly would be able to spend the scan budget itself. Wiring it into
// BuildAll would put "run_scan_job" in the same surface admission vetts,
// inviting exactly that.
//
// The exemption is a reviewed declaration, not a loophole: a new tool type
// still fails this guard until it is either wired into BuildAll or listed
// here with a reason.
var executorOnlyToolTypes = map[string]string{
	"RunScanJobTool": "executor-dispatched scan gate (#1798); never model-facing",
}

// Every tool type in the package must appear in BuildAll.
//
// This is the remaining hole the two tests above cannot close: a new tool type
// that nobody wires into BuildAll is consistent with catalog (both omit it) and
// therefore invisible. Counting Tool implementations in the package and
// comparing against what BuildAll returns makes that omission fail here rather
// than in production.
//
// Deliberately compares counts rather than names: the goal is to notice that a
// type exists and is unwired, which is exactly the case where its name is not
// yet anywhere to compare against.
func TestBuildAllWiresEveryToolType(t *testing.T) {
	built := len(BuildAll(ToolDeps{Workspace: t.TempDir()}))
	var declared []string
	for _, name := range toolTypeNames(t) {
		if _, exempt := executorOnlyToolTypes[name]; !exempt {
			declared = append(declared, name)
		}
	}

	if built != len(declared) {
		t.Errorf("the package declares %d model-facing Tool implementations but BuildAll wires %d.\n"+
			"A tool type that is never constructed cannot be reached by any Agent; "+
			"add it to BuildAll and to catalog.canonicalToolNames, or list it in "+
			"executorOnlyToolTypes with a reason if it is dispatched without a model.\n"+
			"declared: %v", len(declared), built, declared)
	}
}

// toolTypeNames returns every type in this package that implements Name()
// string, parsed from the package source rather than matched with a regex so
// the guard cannot be fooled by formatting.
func toolTypeNames(t *testing.T) []string {
	t.Helper()

	fset := token.NewFileSet()

	// ParseFile per entry rather than ParseDir: ParseDir is deprecated, and
	// the alternative (go/tools/go/packages) is far more machinery than a
	// guard test needs. This package has no build-tagged files, which is the
	// only thing ParseDir would have handled differently.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var files []*ast.File
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		files = append(files, f)
	}

	var names []string
	for _, file := range files {
		{
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != "Name" || fn.Recv == nil || len(fn.Recv.List) == 0 {
					continue
				}
				if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
					continue
				}
				if id, ok := fn.Type.Results.List[0].Type.(*ast.Ident); !ok || id.Name != "string" {
					continue
				}
				switch rt := fn.Recv.List[0].Type.(type) {
				case *ast.StarExpr:
					if id, ok := rt.X.(*ast.Ident); ok {
						names = append(names, id.Name)
					}
				case *ast.Ident:
					names = append(names, rt.Name)
				}
			}
		}
	}
	sort.Strings(names)
	return names
}
