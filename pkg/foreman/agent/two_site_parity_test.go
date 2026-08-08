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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parityRunner fakes the git calls checkTwoSiteParity makes:
// `git status -z`, `git diff -U0 HEAD -- <file>`, and `git diff HEAD -- <file>`.
func parityRunner(statusOut, diffU0Out, diffOut string) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" {
			return "", nil
		}
		switch {
		case len(args) >= 1 && args[0] == "status":
			return statusOut, nil
		case len(args) >= 3 && args[0] == "diff" && args[1] == "-U0" && args[2] == "HEAD":
			return diffU0Out, nil
		case len(args) >= 2 && args[0] == "diff" && args[1] == "HEAD":
			return diffOut, nil
		default:
			return "", nil
		}
	}
}

// TestCheckTwoSiteParity_FiresOnOneSiteFix is the #1418 / #1406 shape:
// appendModeArgs gains "--cache-ram" in internal/controller/runtime_llamacpp_args.go
// while the same-named function in pkg/agent/executor.go does not.
func TestCheckTwoSiteParity_FiresOnOneSiteFix(t *testing.T) {
	ws := t.TempDir()

	// Create the changed file: internal/controller/runtime_llamacpp_args.go
	ctrlDir := filepath.Join(ws, "internal", "controller")
	_ = os.MkdirAll(ctrlDir, 0o755)
	ctrlSrc := `package controller

func appendModeArgs(args []string, mode string, extraArgs []string) []string {
	switch mode {
	case "rerank":
		args = append(args, "--reranking")
		args = append(args, "--cache-ram", "0")
	}
	return args
}
`
	_ = os.WriteFile(filepath.Join(ctrlDir, "runtime_llamacpp_args.go"), []byte(ctrlSrc), 0o644)

	// Create the sibling file: pkg/agent/executor.go (unchanged, no "--cache-ram")
	agentDir := filepath.Join(ws, "pkg", "agent")
	_ = os.MkdirAll(agentDir, 0o755)
	agentSrc := `package agent

func appendModeArgs(args []string, mode string, extraArgs []string) []string {
	switch mode {
	case "rerank":
		args = append(args, "--reranking")
	}
	return args
}
`
	_ = os.WriteFile(filepath.Join(agentDir, "executor.go"), []byte(agentSrc), 0o644)

	statusOut := " M internal/controller/runtime_llamacpp_args.go\x00"
	// The diff adds "--cache-ram" as a string literal and modifies appendModeArgs
	diffU0Out := `@@ -1,1 +1,2 @@ func appendModeArgs(args []string, mode string, extraArgs []string) []string {
+args = append(args, "--cache-ram", "0")
`
	diffOut := diffU0Out

	run := parityRunner(statusOut, diffU0Out, diffOut)
	failed, out := checkTwoSiteParity(context.Background(), ws, run)
	if !failed {
		t.Fatal("expected two-site parity advisory for the #1406 shape")
	}
	if !strings.Contains(out, "appendModeArgs") {
		t.Errorf("detail should name the function; got %q", out)
	}
	if !strings.Contains(out, "--cache-ram") {
		t.Errorf("detail should name the added literal; got %q", out)
	}
	if !strings.Contains(out, "pkg/agent/executor.go") {
		t.Errorf("detail should name the sibling site; got %q", out)
	}
}

// TestCheckTwoSiteParity_SilentWhenBothSitesUpdated verifies that when both
// siblings are updated in the same diff, no advisory is emitted.
func TestCheckTwoSiteParity_SilentWhenBothSitesUpdated(t *testing.T) {
	ws := t.TempDir()

	ctrlDir := filepath.Join(ws, "internal", "controller")
	_ = os.MkdirAll(ctrlDir, 0o755)
	ctrlSrc := `package controller

func appendModeArgs(args []string, mode string, extraArgs []string) []string {
	switch mode {
	case "rerank":
		args = append(args, "--reranking")
		args = append(args, "--cache-ram", "0")
	}
	return args
}
`
	_ = os.WriteFile(filepath.Join(ctrlDir, "runtime_llamacpp_args.go"), []byte(ctrlSrc), 0o644)

	agentDir := filepath.Join(ws, "pkg", "agent")
	_ = os.MkdirAll(agentDir, 0o755)
	agentSrc := `package agent

func appendModeArgs(args []string, mode string, extraArgs []string) []string {
	switch mode {
	case "rerank":
		args = append(args, "--reranking")
		args = append(args, "--cache-ram", "0")
	}
	return args
}
`
	_ = os.WriteFile(filepath.Join(agentDir, "executor.go"), []byte(agentSrc), 0o644)

	statusOut := " M internal/controller/runtime_llamacpp_args.go\x00"
	diffU0Out := `@@ -1,1 +1,2 @@ func appendModeArgs(args []string, mode string, extraArgs []string) []string {
+args = append(args, "--cache-ram", "0")
`
	diffOut := diffU0Out

	run := parityRunner(statusOut, diffU0Out, diffOut)
	failed, out := checkTwoSiteParity(context.Background(), ws, run)
	if failed {
		t.Fatalf("both siblings updated: must be silent, got failed=%v out=%q", failed, out)
	}
}

// TestCheckTwoSiteParity_SilentWhenNoSibling verifies that a changed function
// with no same-named sibling anywhere produces no advisory.
func TestCheckTwoSiteParity_SilentWhenNoSibling(t *testing.T) {
	ws := t.TempDir()

	ctrlDir := filepath.Join(ws, "internal", "controller")
	_ = os.MkdirAll(ctrlDir, 0o755)
	ctrlSrc := `package controller

func uniqueFuncName(args []string) []string {
	args = append(args, "--new-flag")
	return args
}
`
	_ = os.WriteFile(filepath.Join(ctrlDir, "x.go"), []byte(ctrlSrc), 0o644)

	statusOut := " M internal/controller/x.go\x00"
	diffU0Out := `@@ -1,1 +1,2 @@ func uniqueFuncName(args []string) []string {
+args = append(args, "--new-flag")
`
	diffOut := diffU0Out

	run := parityRunner(statusOut, diffU0Out, diffOut)
	failed, out := checkTwoSiteParity(context.Background(), ws, run)
	if failed {
		t.Fatalf("no sibling: must be silent, got failed=%v out=%q", failed, out)
	}
}

// TestCheckTwoSiteParity_SilentWhenSiblingAlreadyHasLiteral verifies that a
// sibling that already contains the literal produces no advisory.
func TestCheckTwoSiteParity_SilentWhenSiblingAlreadyHasLiteral(t *testing.T) {
	ws := t.TempDir()

	ctrlDir := filepath.Join(ws, "internal", "controller")
	_ = os.MkdirAll(ctrlDir, 0o755)
	ctrlSrc := `package controller

func appendModeArgs(args []string, mode string, extraArgs []string) []string {
	switch mode {
	case "rerank":
		args = append(args, "--reranking")
		args = append(args, "--cache-ram", "0")
	}
	return args
}
`
	_ = os.WriteFile(filepath.Join(ctrlDir, "runtime_llamacpp_args.go"), []byte(ctrlSrc), 0o644)

	agentDir := filepath.Join(ws, "pkg", "agent")
	_ = os.MkdirAll(agentDir, 0o755)
	agentSrc := `package agent

func appendModeArgs(args []string, mode string, extraArgs []string) []string {
	switch mode {
	case "rerank":
		args = append(args, "--reranking")
		args = append(args, "--cache-ram", "0")
	}
	return args
}
`
	_ = os.WriteFile(filepath.Join(agentDir, "executor.go"), []byte(agentSrc), 0o644)

	statusOut := " M internal/controller/runtime_llamacpp_args.go\x00"
	diffU0Out := `@@ -1,1 +1,2 @@ func appendModeArgs(args []string, mode string, extraArgs []string) []string {
+args = append(args, "--cache-ram", "0")
`
	diffOut := diffU0Out

	run := parityRunner(statusOut, diffU0Out, diffOut)
	failed, out := checkTwoSiteParity(context.Background(), ws, run)
	if failed {
		t.Fatalf("sibling already has literal: must be silent, got failed=%v out=%q", failed, out)
	}
}

// TestCheckTwoSiteParity_SilentWhenNoAddedLiterals verifies that a body
// modification that adds no new string literals produces no advisory.
func TestCheckTwoSiteParity_SilentWhenNoAddedLiterals(t *testing.T) {
	ws := t.TempDir()

	ctrlDir := filepath.Join(ws, "internal", "controller")
	_ = os.MkdirAll(ctrlDir, 0o755)
	ctrlSrc := `package controller

func appendModeArgs(args []string, mode string, extraArgs []string) []string {
	switch mode {
	case "rerank":
		args = append(args, "--reranking")
	}
	return args
}
`
	_ = os.WriteFile(filepath.Join(ctrlDir, "runtime_llamacpp_args.go"), []byte(ctrlSrc), 0o644)

	agentDir := filepath.Join(ws, "pkg", "agent")
	_ = os.MkdirAll(agentDir, 0o755)
	agentSrc := `package agent

func appendModeArgs(args []string, mode string, extraArgs []string) []string {
	switch mode {
	case "rerank":
		args = append(args, "--reranking")
	}
	return args
}
`
	_ = os.WriteFile(filepath.Join(agentDir, "executor.go"), []byte(agentSrc), 0o644)

	statusOut := " M internal/controller/runtime_llamacpp_args.go\x00"
	// The diff modifies the function but adds no new string literal
	diffU0Out := `@@ -1,1 +1,1 @@ func appendModeArgs(args []string, mode string, extraArgs []string) []string {
+args = append(args, "--reranking")
`
	diffOut := diffU0Out

	run := parityRunner(statusOut, diffU0Out, diffOut)
	failed, out := checkTwoSiteParity(context.Background(), ws, run)
	if failed {
		t.Fatalf("no added literals: must be silent, got failed=%v out=%q", failed, out)
	}
}

// TestCheckTwoSiteParity_RegisteredAsAdvisory verifies the check is registered
// at the advisory tier.
func TestCheckTwoSiteParity_RegisteredAsAdvisory(t *testing.T) {
	var found bool
	var tier gateTier
	for _, c := range gateCheckRegistry("", "", nil) {
		if c.name == "two-site-parity" {
			found = true
			tier = c.tier
		}
	}
	if !found {
		t.Fatal(`gateCheckRegistry is missing the "two-site-parity" check`)
	}
	if tier != tierAdvisory {
		t.Errorf("two-site-parity tier = %v, want tierAdvisory", tier)
	}
}

// TestCheckTwoSiteParity_SurfacesAsAdvisoryNotBlocking verifies the check
// never blocks the gate.
func TestCheckTwoSiteParity_SurfacesAsAdvisoryNotBlocking(t *testing.T) {
	ws := t.TempDir()

	ctrlDir := filepath.Join(ws, "internal", "controller")
	_ = os.MkdirAll(ctrlDir, 0o755)
	ctrlSrc := `package controller

func appendModeArgs(args []string, mode string, extraArgs []string) []string {
	switch mode {
	case "rerank":
		args = append(args, "--reranking")
		args = append(args, "--cache-ram", "0")
	}
	return args
}
`
	_ = os.WriteFile(filepath.Join(ctrlDir, "runtime_llamacpp_args.go"), []byte(ctrlSrc), 0o644)

	agentDir := filepath.Join(ws, "pkg", "agent")
	_ = os.MkdirAll(agentDir, 0o755)
	agentSrc := `package agent

func appendModeArgs(args []string, mode string, extraArgs []string) []string {
	switch mode {
	case "rerank":
		args = append(args, "--reranking")
	}
	return args
}
`
	_ = os.WriteFile(filepath.Join(agentDir, "executor.go"), []byte(agentSrc), 0o644)

	statusOut := " M internal/controller/runtime_llamacpp_args.go\x00"
	diffU0Out := `@@ -1,1 +1,2 @@ func appendModeArgs(args []string, mode string, extraArgs []string) []string {
+args = append(args, "--cache-ram", "0")
`
	diffOut := diffU0Out

	run := parityRunner(statusOut, diffU0Out, diffOut)
	blocking, advisories := runGateChecks(context.Background(), ws, run,
		[]gateCheck{{name: "two-site-parity", tier: tierAdvisory, fn: checkTwoSiteParity}})
	if len(blocking) != 0 {
		t.Errorf("two-site-parity must never block; got %d blocking", len(blocking))
	}
	if len(advisories) != 1 || advisories[0].Check != "two-site-parity" {
		t.Fatalf("expected one two-site-parity advisory; got %+v", advisories)
	}
}

// TestExtractStringLiterals verifies the string literal extraction helper.
func TestExtractStringLiterals(t *testing.T) {
	cases := []struct {
		line string
		want []string
	}{
		{
			line: `args = append(args, "--cache-ram", "0")`,
			want: []string{"--cache-ram", "0"},
		},
		{
			line: `x := "hello"`,
			want: []string{"hello"},
		},
		{
			line: `x := "escaped \" quote"`,
			want: []string{"escaped \" quote"},
		},
		{
			line: `// no literals here`,
			want: nil,
		},
	}
	for _, tc := range cases {
		got := extractStringLiterals(tc.line)
		if len(got) != len(tc.want) {
			t.Errorf("extractStringLiterals(%q) = %v, want %v", tc.line, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("extractStringLiterals(%q)[%d] = %q, want %q", tc.line, i, got[i], tc.want[i])
			}
		}
	}
}

// TestHasFuncDecl verifies the function declaration detection helper.
func TestHasFuncDecl(t *testing.T) {
	cases := []struct {
		src      string
		funcName string
		want     bool
	}{
		{
			src:      "func appendModeArgs(args []string) {}",
			funcName: "appendModeArgs",
			want:     true,
		},
		{
			src:      "func (e *Executor) appendModeArgs(args []string) {}",
			funcName: "appendModeArgs",
			want:     true,
		},
		{
			src:      "func otherFunc(args []string) {}",
			funcName: "appendModeArgs",
			want:     false,
		},
	}
	for _, tc := range cases {
		got := hasFuncDecl(tc.src, tc.funcName)
		if got != tc.want {
			t.Errorf("hasFuncDecl(%q, %q) = %v, want %v", tc.src, tc.funcName, got, tc.want)
		}
	}
}

// TestFuncBodyContainsLiteral verifies the AST-based literal check.
func TestFuncBodyContainsLiteral(t *testing.T) {
	src := `package x

func appendModeArgs(args []string) []string {
	args = append(args, "--cache-ram", "0")
	return args
}

func otherFunc(args []string) []string {
	args = append(args, "--other")
	return args
}
`
	cases := []struct {
		funcName string
		literal  string
		want     bool
	}{
		{"appendModeArgs", "--cache-ram", true},
		{"appendModeArgs", "0", true},
		{"appendModeArgs", "--other", false},
		{"otherFunc", "--other", true},
		{"otherFunc", "--cache-ram", false},
	}
	for _, tc := range cases {
		got := funcBodyContainsLiteral(src, tc.funcName, tc.literal)
		if got != tc.want {
			t.Errorf("funcBodyContainsLiteral(%q, %q) = %v, want %v",
				tc.funcName, tc.literal, got, tc.want)
		}
	}
}
