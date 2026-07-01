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
	"strings"
	"testing"
)

// callerImpactRunner builds a fake commandRunner keyed on argv patterns for
// checkCallerImpact tests. It answers three call types:
//  1. git status -z  -> returns the changedFiles NUL-separated list
//  2. git diff -U0 -- <file>  -> returns diffOutput for that file
//  3. grep -rn --include=*.go <name> .  -> returns grepOutput for that name
func callerImpactRunner(changedFiles, diffOutput string, grepOutputs map[string]string) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		switch name {
		case "git":
			if len(args) >= 2 && args[0] == "status" && args[1] == "-z" {
				return changedFiles, nil
			}
			if len(args) >= 3 && args[0] == "diff" && args[1] == "-U0" {
				// args[2] is "--", args[3] is the file
				return diffOutput, nil
			}
		case "grep":
			// grep -rn --include=*.go <name> .
			// args: ["-rn", "--include=*.go", <name>, "."]
			if len(args) >= 3 {
				funcName := args[2]
				if out, ok := grepOutputs[funcName]; ok {
					return out, nil
				}
			}
		}
		return "", nil
	}
}

// TestCheckCallerImpact_ListsOtherCallers verifies that checkCallerImpact
// returns an advisory listing external callers when a changed shared function
// is called from files other than the one being changed.
func TestCheckCallerImpact_ListsOtherCallers(t *testing.T) {
	// changed files: one non-test Go file
	changedFiles := " M pkg/foreman/agent/repo/diff.go"

	// git diff -U0 output showing a hunk inside DiffNameOnly
	diffOutput := "@@ -50,3 +50,4 @@ func DiffNameOnly(base string) string {\n+\tgitAdd()\n"

	// grep results: definition site + two call sites in a different file
	grepOutputs := map[string]string{
		"DiffNameOnly": "./pkg/foreman/agent/repo/diff.go:50:func DiffNameOnly(base string) string {\n" +
			"./pkg/foreman/agent/executor_native.go:559:\tDiffNameOnly(\"HEAD\")\n" +
			"./pkg/foreman/agent/executor_native.go:1386:\tDiffNameOnly(\"main\")\n",
	}

	run := callerImpactRunner(changedFiles, diffOutput, grepOutputs)

	failed, out := checkCallerImpact(context.Background(), "/ws", run)
	if !failed {
		t.Fatalf("want advisory (failed=true) listing other callers, got failed=false out=%q", out)
	}
	if !strings.Contains(out, "executor_native.go:1386") {
		t.Fatalf("want advisory listing executor_native.go:1386, got out=%q", out)
	}
	if !strings.Contains(out, "executor_native.go:559") {
		t.Fatalf("want advisory listing executor_native.go:559, got out=%q", out)
	}
}

// TestCheckCallerImpact_UnexportedSingleUseNoAdvisory verifies that an
// unexported function whose grep output contains only its definition line
// (no external callers) does NOT produce an advisory.
func TestCheckCallerImpact_UnexportedSingleUseNoAdvisory(t *testing.T) {
	// changed files: one non-test Go file
	changedFiles := " M pkg/foreman/agent/helper.go"

	// git diff -U0 output showing a hunk inside the unexported helper function
	diffOutput := "@@ -10,3 +10,4 @@ func helper(x int) int {\n+\treturn x + 1\n"

	// grep returns only the definition line (no external callers)
	grepOutputs := map[string]string{
		"helper": "./pkg/foreman/agent/helper.go:10:func helper(x int) int {\n",
	}

	run := callerImpactRunner(changedFiles, diffOutput, grepOutputs)

	failed, _ := checkCallerImpact(context.Background(), "/ws", run)
	if failed {
		t.Fatal("a function with no external callers should not produce an advisory")
	}
}

// TestCheckCallerImpact_ExportedWithNoExternalCallers verifies that an
// exported function with no external callers also produces no advisory
// (only the definition site found by grep).
func TestCheckCallerImpact_ExportedWithNoExternalCallers(t *testing.T) {
	changedFiles := " M pkg/foreman/agent/foo.go"
	diffOutput := "@@ -5,3 +5,4 @@ func MyExportedFunc() {\n+\tlog.Println(\"updated\")\n"

	grepOutputs := map[string]string{
		"MyExportedFunc": "./pkg/foreman/agent/foo.go:5:func MyExportedFunc() {\n",
	}

	run := callerImpactRunner(changedFiles, diffOutput, grepOutputs)

	failed, _ := checkCallerImpact(context.Background(), "/ws", run)
	if failed {
		t.Fatal("an exported function with only the definition site should produce no advisory")
	}
}

// TestCheckCallerImpact_FailOpenOnNoChanges verifies that checkCallerImpact
// returns (false, "") when there are no changed non-test Go files.
func TestCheckCallerImpact_FailOpenOnNoChanges(t *testing.T) {
	run := callerImpactRunner("", "", nil)

	failed, out := checkCallerImpact(context.Background(), "/ws", run)
	if failed {
		t.Errorf("no changed files: want failed=false, got failed=true out=%q", out)
	}
	if out != "" {
		t.Errorf("no changed files: want empty output, got %q", out)
	}
}

// TestCheckCallerImpact_SameFileRecursiveCallNoAdvisory verifies that a
// function whose only grep hits are the definition line and a same-file
// recursive call does NOT produce an advisory. Before the fix, the recursive
// call site was treated as an external caller because it was not the
// definition line; after the fix the entire definition file is excluded.
func TestCheckCallerImpact_SameFileRecursiveCallNoAdvisory(t *testing.T) {
	changedFiles := " M pkg/x/a.go"
	// hunk inside recurse body
	diffOutput := "@@ -10,3 +10,4 @@ func recurse(n int) int {\n+\tif n == 0 { return 0 }\n"

	// grep returns: the definition line + a same-file recursive call site
	grepOutputs := map[string]string{
		"recurse": "./pkg/x/a.go:10:func recurse(n int) int {\n" +
			"./pkg/x/a.go:20:\treturn recurse(n-1)\n",
	}

	run := callerImpactRunner(changedFiles, diffOutput, grepOutputs)

	failed, out := checkCallerImpact(context.Background(), "/ws", run)
	if failed {
		t.Fatalf("same-file recursive call must not produce an advisory; got failed=true out=%q", out)
	}
}

// TestCheckCallerImpact_AddedFuncWithExternalCaller verifies the addedFuncNames
// path: when a new function is introduced ("+func" in the diff) and a
// cross-file grep hit is found, an advisory is emitted.
func TestCheckCallerImpact_AddedFuncWithExternalCaller(t *testing.T) {
	changedFiles := " M pkg/x/b.go"
	// diff shows an added function signature (no leading -U0 hunk header needed
	// because addedFuncNames matches lines starting with "+func")
	diffOutput := "+func NewThing() {\n"

	// grep returns: the definition line in b.go + a caller in another file
	grepOutputs := map[string]string{
		"NewThing": "./pkg/x/b.go:5:func NewThing() {\n" +
			"./pkg/y/consumer.go:42:\tNewThing()\n",
	}

	// addedFuncNames uses "git diff -- <file>" (without -U0); the fake runner
	// above answers args[0]=="diff" && args[1]=="-U0" for modifiedFuncNames.
	// We need a separate branch for the plain "git diff -- <file>" call used by
	// addedFuncNames.  Extend the runner to handle both.
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		switch name {
		case "git":
			if len(args) >= 2 && args[0] == "status" && args[1] == "-z" {
				return changedFiles, nil
			}
			if len(args) >= 2 && args[0] == "diff" {
				// Both -U0 (modifiedFuncNames) and plain (addedFuncNames) paths
				// return the same diff; addedFuncNames only cares about "+func" lines.
				return diffOutput, nil
			}
		case "grep":
			if len(args) >= 3 {
				if out, ok := grepOutputs[args[2]]; ok {
					return out, nil
				}
			}
		}
		return "", nil
	}

	failed, out := checkCallerImpact(context.Background(), "/ws", run)
	if !failed {
		t.Fatalf("want advisory for added func with external caller, got failed=false")
	}
	if !strings.Contains(out, "consumer.go:42") {
		t.Fatalf("want advisory listing consumer.go:42, got out=%q", out)
	}
}

// TestCheckCallerImpact_TruncatesLargeCallerLists verifies that when a
// function has many callers the output is capped and a "(+N more)" note
// is appended.
func TestCheckCallerImpact_TruncatesLargeCallerLists(t *testing.T) {
	changedFiles := " M pkg/x/shared.go"
	diffOutput := "@@ -1,3 +1,4 @@ func SharedFunc() {\n+\tfoo()\n"

	// Build 15 grep lines from different files (> the 10-site cap).
	var lines []string
	lines = append(lines, "./pkg/x/shared.go:1:func SharedFunc() {\n")
	for i := 0; i < 15; i++ {
		lines = append(lines, "./pkg/x/caller"+string(rune('A'+i))+".go:10:\tSharedFunc()\n")
	}
	grepOutputs := map[string]string{
		"SharedFunc": strings.Join(lines, ""),
	}

	run := callerImpactRunner(changedFiles, diffOutput, grepOutputs)

	failed, out := checkCallerImpact(context.Background(), "/ws", run)
	if !failed {
		t.Fatal("want advisory for a heavily-called shared function, got failed=false")
	}
	if !strings.Contains(out, "more") {
		t.Errorf("want truncation note in output, got %q", out)
	}
}
