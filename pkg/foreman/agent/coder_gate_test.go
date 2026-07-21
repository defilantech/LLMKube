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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCommand describes a canned response for one invocation matched by
// the command name (gofmt, go, or the golangci-lint path).
type fakeCommand struct {
	output string
	err    error
}

// recordedCall captures one invocation of the fake runner so tests can
// assert on the environment and arguments passed to a given check.
type recordedCall struct {
	name     string
	args     []string
	extraEnv []string
}

// newFakeRunner returns a commandRunner that replies from responses keyed
// by command name, and a pointer to the slice of recorded calls.
func newFakeRunner(responses map[string]fakeCommand) (commandRunner, *[]recordedCall) {
	calls := &[]recordedCall{}
	run := func(_ context.Context, _ string, extraEnv []string, name string, args ...string) (string, error) {
		*calls = append(*calls, recordedCall{name: name, args: args, extraEnv: extraEnv})
		resp := responses[name]
		return resp.output, resp.err
	}
	return run, calls
}

func TestRunCoderGate(t *testing.T) {
	const golangciPath = "./bin/golangci-lint"
	buildErr := errors.New("exit status 1")

	tests := []struct {
		name         string
		responses    map[string]fakeCommand
		wantPass     bool
		wantContains []string
		wantAbsent   []string
	}{
		{
			name: "all checks pass",
			responses: map[string]fakeCommand{
				"gofmt":      {output: "", err: nil},
				"go":         {output: "", err: nil},
				golangciPath: {output: "", err: nil},
			},
			wantPass: true,
		},
		{
			name: "gofmt failure with non-empty output and nil err",
			responses: map[string]fakeCommand{
				"gofmt":      {output: "internal/foo/bar.go\n", err: nil},
				"go":         {output: "", err: nil},
				golangciPath: {output: "", err: nil},
			},
			wantPass:     false,
			wantContains: []string{"gofmt -l .", "internal/foo/bar.go"},
			wantAbsent:   []string{"go vet", "go build", golangciPath + " run"},
		},
		{
			name: "lint failure with non-nil err",
			responses: map[string]fakeCommand{
				"gofmt":      {output: "", err: nil},
				"go":         {output: "", err: nil},
				golangciPath: {output: "main.go:10: unused variable x", err: buildErr},
			},
			wantPass:     false,
			wantContains: []string{golangciPath + " run ./...", "unused variable x"},
		},
		{
			name: "multiple simultaneous failures all appear",
			responses: map[string]fakeCommand{
				"gofmt":      {output: "pkg/a/a.go\n", err: nil},
				"go":         {output: "vet and build both broke", err: buildErr},
				golangciPath: {output: "lint exploded", err: buildErr},
			},
			wantPass: false,
			wantContains: []string{
				"The verification gate failed",
				"gofmt -l .", "pkg/a/a.go",
				"go vet ./...",
				"go build ./...",
				golangciPath + " run ./...", "lint exploded",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, _ := newFakeRunner(tt.responses)
			pass, feedback, _ := RunCoderGate(context.Background(), "/work", golangciPath, run, "", "main", nil)

			if pass != tt.wantPass {
				t.Fatalf("pass = %v, want %v (feedback: %q)", pass, tt.wantPass, feedback)
			}
			if tt.wantPass && feedback != "" {
				t.Fatalf("expected empty feedback on pass, got %q", feedback)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(feedback, want) {
					t.Errorf("feedback missing %q\nfeedback:\n%s", want, feedback)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(feedback, absent) {
					t.Errorf("feedback unexpectedly contains %q\nfeedback:\n%s", absent, feedback)
				}
			}
		})
	}
}

// TestRunCoderGateLintEnv asserts the lint check receives GOOS=linux as an
// extra env var while the other checks receive none.
func TestRunCoderGateLintEnv(t *testing.T) {
	const golangciPath = "./bin/golangci-lint"
	run, calls := newFakeRunner(map[string]fakeCommand{
		"gofmt":      {},
		"go":         {},
		golangciPath: {},
	})

	RunCoderGate(context.Background(), "/work", golangciPath, run, "", "main", nil)

	var lintCall *recordedCall
	for i := range *calls {
		if (*calls)[i].name == golangciPath {
			lintCall = &(*calls)[i]
		}
	}
	if lintCall == nil {
		t.Fatal("golangci-lint was never invoked")
	}

	foundGOOS := false
	for _, e := range lintCall.extraEnv {
		if e == "GOOS=linux" {
			foundGOOS = true
		}
	}
	if !foundGOOS {
		t.Errorf("lint extraEnv = %v, want it to include GOOS=linux", lintCall.extraEnv)
	}

	// Non-lint checks must not carry GOOS=linux.
	for i := range *calls {
		c := (*calls)[i]
		if c.name == golangciPath {
			continue
		}
		for _, e := range c.extraEnv {
			if e == "GOOS=linux" {
				t.Errorf("check %q unexpectedly carries GOOS=linux", c.name)
			}
		}
	}
}

// TestRunCoderGateLintCacheScopedToWorkspace asserts the lint check runs
// with a GOLANGCI_LINT_CACHE scoped to a per-workspace sibling directory,
// so stale results from another coder workspace cannot pollute this run's
// lint (#759).
func TestRunCoderGateLintCacheScopedToWorkspace(t *testing.T) {
	const golangciPath = "./bin/golangci-lint"
	run, calls := newFakeRunner(map[string]fakeCommand{
		"gofmt":      {},
		"go":         {},
		golangciPath: {},
	})

	RunCoderGate(context.Background(), "/work", golangciPath, run, "", "main", nil)

	var lintCall *recordedCall
	for i := range *calls {
		if (*calls)[i].name == golangciPath {
			lintCall = &(*calls)[i]
		}
	}
	if lintCall == nil {
		t.Fatal("golangci-lint was never invoked")
	}

	want := "GOLANGCI_LINT_CACHE=/work.golangci-cache"
	found := false
	for _, e := range lintCall.extraEnv {
		if e == want {
			found = true
		}
	}
	if !found {
		t.Errorf("lint extraEnv = %v, want it to include %q (workspace-scoped lint cache)", lintCall.extraEnv, want)
	}
}

// TestChangedTestPackages_ExcludesEnvtestAndDedups verifies the changed
// package set covers non-envtest packages, dedups, and excludes
// envtest/integration packages and non-Go files (#762).
func TestChangedTestPackages_ExcludesEnvtestAndDedups(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, name string, _ ...string) (string, error) {
		if name == "git" {
			// git status -z uses NUL-terminated entries.
			return " M pkg/cli/cache_inspect.go\x00" +
				" M pkg/cli/cache_inspect_test.go\x00" +
				"?? pkg/foreman/agent/loop_session_test.go\x00" +
				" M internal/controller/model_controller.go\x00" +
				" M internal/foreman/controller/agentictask_controller.go\x00" +
				" M test/e2e/e2e_test.go\x00" +
				" M README.md\x00", nil
		}
		return "", nil
	}
	got := changedTestPackages(context.Background(), "/work", run)

	want := map[string]bool{"./pkg/cli/": true, "./pkg/foreman/agent/": true}
	if len(got) != len(want) {
		t.Fatalf("changedTestPackages = %v, want exactly %v", got, want)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected package %q in result %v (envtest/non-go should be excluded)", p, got)
		}
	}
}

// TestRunCoderGate_FailsOnChangedPackageUnitTest verifies the gate runs a
// unit-test tier on changed non-envtest packages and fails (citing go test)
// when one of those tests fails, even though the static checks pass (#762).
func TestRunCoderGate_FailsOnChangedPackageUnitTest(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		switch {
		case name == "gofmt":
			return "", nil
		case name == "git":
			return " M pkg/cli/cache_inspect_test.go\x00", nil
		case name == "go" && len(args) > 0 && args[0] == "test":
			out := "panic: runtime error: invalid memory address\n" +
				"FAIL\tgithub.com/defilantech/llmkube/pkg/cli"
			return out, errors.New("exit status 1")
		case name == "go":
			return "", nil // vet, build pass
		default:
			return "", nil // golangci-lint
		}
	}
	pass, feedback, _ := RunCoderGate(context.Background(), "/work", "./bin/golangci-lint", run, "", "main", nil)
	if pass {
		t.Fatal("gate should fail when a changed package's unit test fails")
	}
	if !strings.Contains(feedback, "go test") || !strings.Contains(feedback, "pkg/cli") {
		t.Errorf("feedback should cite the failing go test for pkg/cli; got:\n%s", feedback)
	}
}

// TestRunCoderGate_SkipsTestTierWhenNoChangedPackages verifies the gate does
// not invoke go test when git reports no changed Go packages (#762).
func TestRunCoderGate_SkipsTestTierWhenNoChangedPackages(t *testing.T) {
	sawGoTest := false
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "go" && len(args) > 0 && args[0] == "test" {
			sawGoTest = true
		}
		return "", nil // git status empty, all checks clean
	}
	if pass, _, _ := RunCoderGate(context.Background(), "/work", "./bin/golangci-lint", run, "", "main", nil); !pass {
		t.Fatal("gate should pass when all checks are clean and nothing changed")
	}
	if sawGoTest {
		t.Error("test tier should not run go test when no packages changed")
	}
}

// TestRunCoderGateTruncation verifies per-check output is capped and marked.
func TestRunCoderGateTruncation(t *testing.T) {
	const golangciPath = "./bin/golangci-lint"
	huge := strings.Repeat("x", maxCheckOutputBytes+5000)
	run, _ := newFakeRunner(map[string]fakeCommand{
		"gofmt":      {},
		"go":         {},
		golangciPath: {output: huge, err: errors.New("boom")},
	})

	_, feedback, _ := RunCoderGate(context.Background(), "/work", golangciPath, run, "", "main", nil)

	if !strings.Contains(feedback, "...(truncated)...") {
		t.Error("expected truncation marker in feedback")
	}
	// Feedback must be much smaller than the raw output plus the cap.
	if len(feedback) > maxCheckOutputBytes+1024 {
		t.Errorf("feedback length %d exceeds expected truncated bound", len(feedback))
	}
	// The captured output (all x's) must be capped at maxCheckOutputBytes.
	// A few incidental x's appear in the directive text ("Fix"), so allow a
	// small slack above the cap rather than asserting an exact count.
	if got := strings.Count(feedback, "x"); got > maxCheckOutputBytes+16 {
		t.Errorf("output not truncated: %d x's exceed cap %d", got, maxCheckOutputBytes)
	}
}

// codegenFake builds a commandRunner for the codegen-drift tier. All of the
// other gate checks (gofmt/vet/build/lint, test tier) pass. controller-gen is
// present unless noControllerGen is set. The two `git status --porcelain`
// calls (the before/after snapshots around make) return porcelainBefore then
// porcelainAfter; make returns makeErr.
// gateLintPath is the golangci-lint path the codegen gate tests pass to
// RunCoderGate and that codegenFake matches as a passing lint invocation.
const gateLintPath = "./bin/golangci-lint"

func codegenFake(
	porcelainBefore, porcelainAfter string,
	makeErr error,
	noControllerGen bool,
) commandRunner {
	porcelainCalls := 0
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		switch {
		case name == "gofmt", name == "go", name == gateLintPath:
			return "", nil
		case name == "test" && len(args) > 0 && args[0] == "-f":
			if noControllerGen {
				return "", errors.New("exit status 1")
			}
			return "", nil // controller-gen exists
		case name == "make":
			if makeErr != nil {
				return "Error: controller-gen: exit status 1\n", makeErr
			}
			return "", nil
		case name == "git" && len(args) >= 2 && args[0] == "status" && args[1] == "-z":
			return "", nil // changedTestPackages: no changed packages
		case name == "git" && len(args) >= 2 && args[0] == "status" && args[1] == "--porcelain":
			porcelainCalls++
			if porcelainCalls == 1 {
				return porcelainBefore, nil
			}
			return porcelainAfter, nil
		default:
			return "", nil
		}
	}
}

// TestRunCoderGate_Codegen_AutoResolvesGeneratedDrift verifies the gate passes
// (deterministic resolution, #851) when regeneration only dirties generated
// artifacts: the executor's `git add -A` commits them, so the coder is not
// asked to re-run a mechanical step.
func TestRunCoderGate_Codegen_AutoResolvesGeneratedDrift(t *testing.T) {
	const golangciPath = "./bin/golangci-lint"
	after := " M config/crd/bases/foreman.llmkube.dev_agentictasks.yaml\n" +
		" M charts/foreman/templates/crds/agentictasks.yaml\n" +
		" M api/foreman/v1alpha1/zz_generated.deepcopy.go\n"
	run := codegenFake("", after, nil, false)
	pass, feedback, _ := RunCoderGate(context.Background(), "/work", golangciPath, run, "", "main", nil)
	if !pass {
		t.Fatalf("gate should auto-resolve generated-only drift; feedback:\n%s", feedback)
	}
	if feedback != "" {
		t.Errorf("expected empty feedback on auto-resolve, got %q", feedback)
	}
}

// TestRunCoderGate_Codegen_IgnoresCoderEdits is the #851/#813 case: the coder
// changed an API type (and other source) but did not run the full codegen
// sync. The coder's own edits are present before make; regeneration adds the
// missing foreman chart CRD + deepcopy. Only make-induced drift is evaluated,
// it is all generated, so the gate passes instead of failing on the
// mechanical step.
func TestRunCoderGate_Codegen_IgnoresCoderEdits(t *testing.T) {
	const golangciPath = "./bin/golangci-lint"
	before := " M api/foreman/v1alpha1/agentictask_types.go\n" +
		" M pkg/foreman/agent/executor_native.go\n"
	after := before +
		" M charts/foreman/templates/crds/agentictasks.yaml\n" +
		" M api/foreman/v1alpha1/zz_generated.deepcopy.go\n"
	run := codegenFake(before, after, nil, false)
	pass, feedback, _ := RunCoderGate(context.Background(), "/work", golangciPath, run, "", "main", nil)
	if !pass {
		t.Fatalf("gate should ignore the coder's own edits and resolve generated drift; feedback:\n%s", feedback)
	}
	if feedback != "" {
		t.Errorf("expected empty feedback, got %q", feedback)
	}
}

// TestRunCoderGate_Codegen_FailsWhenRegenTouchesNonGenerated verifies the gate
// still fails when regeneration changes a file that is NOT a generated
// artifact (an anomaly: a hand-edited generated file or a generator rewriting
// source), and surfaces that file.
func TestRunCoderGate_Codegen_FailsWhenRegenTouchesNonGenerated(t *testing.T) {
	const golangciPath = "./bin/golangci-lint"
	after := " M charts/foreman/templates/crds/agentictasks.yaml\n" +
		" M internal/controller/inferenceservice_controller.go\n"
	run := codegenFake("", after, nil, false)
	pass, feedback, _ := RunCoderGate(context.Background(), "/work", golangciPath, run, "", "main", nil)
	if pass {
		t.Fatal("gate should fail when regeneration touches a non-generated file")
	}
	if !strings.Contains(feedback, "codegen drift") {
		t.Errorf("feedback should cite codegen drift; got:\n%s", feedback)
	}
	if !strings.Contains(feedback, "internal/controller/inferenceservice_controller.go") {
		t.Errorf("feedback should list the non-generated file; got:\n%s", feedback)
	}
	if strings.Contains(feedback, "charts/foreman/templates/crds/agentictasks.yaml") {
		t.Errorf("feedback should not list generated artifacts; got:\n%s", feedback)
	}
}

// TestRunCoderGate_Codegen_PassesWhenClean verifies the gate passes when
// regeneration produces no new drift.
func TestRunCoderGate_Codegen_PassesWhenClean(t *testing.T) {
	const golangciPath = "./bin/golangci-lint"
	run := codegenFake("", "", nil, false)
	pass, feedback, _ := RunCoderGate(context.Background(), "/work", golangciPath, run, "", "main", nil)
	if !pass {
		t.Fatalf("gate should pass when codegen is clean; feedback:\n%s", feedback)
	}
	if feedback != "" {
		t.Errorf("expected empty feedback on pass, got %q", feedback)
	}
}

// TestRunCoderGate_Codegen_SkippedWhenNoControllerGen verifies the codegen
// tier is skipped gracefully when controller-gen is not available.
func TestRunCoderGate_Codegen_SkippedWhenNoControllerGen(t *testing.T) {
	const golangciPath = "./bin/golangci-lint"
	run := codegenFake("", "", nil, true)
	pass, feedback, _ := RunCoderGate(context.Background(), "/work", golangciPath, run, "", "main", nil)
	if !pass {
		t.Fatalf("gate should pass when controller-gen is unavailable; feedback:\n%s", feedback)
	}
	if feedback != "" {
		t.Errorf("expected empty feedback on pass, got %q", feedback)
	}
}

// TestRunCoderGate_Codegen_FailsWhenMakeFails verifies the gate fails when the
// regeneration make target itself errors.
func TestRunCoderGate_Codegen_FailsWhenMakeFails(t *testing.T) {
	const golangciPath = "./bin/golangci-lint"
	run := codegenFake("", "", errors.New("exit status 2"), false)
	pass, feedback, _ := RunCoderGate(context.Background(), "/work", golangciPath, run, "", "main", nil)
	if pass {
		t.Fatal("gate should fail when the regeneration make target fails")
	}
	if !strings.Contains(feedback, "codegen drift") {
		t.Errorf("feedback should cite codegen drift; got:\n%s", feedback)
	}
}

func TestIsGeneratedArtifact(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"config/crd/bases/foreman.llmkube.dev_agentictasks.yaml", true},
		{"config/rbac/role.yaml", true},
		{"config/webhook/manifests.yaml", true},
		{"charts/llmkube/templates/crds/inferenceservices.yaml", true},
		{"charts/foreman/templates/crds/agentictasks.yaml", true},
		{"api/foreman/v1alpha1/zz_generated.deepcopy.go", true},
		{"api/v1alpha1/inferenceservice_types.go", false},
		{"pkg/foreman/agent/executor_native.go", false},
		{"config/rbac/kustomization.yaml", false},
		{"charts/foreman/templates/operator-rbac.yaml", false},
		{"internal/controller/model_controller.go", false},
	}
	for _, tc := range cases {
		if got := isGeneratedArtifact(tc.path); got != tc.want {
			t.Errorf("isGeneratedArtifact(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestDirtyPathSet(t *testing.T) {
	const porcelain = " M api/foreman/v1alpha1/agentictask_types.go\n" +
		"?? charts/foreman/templates/crds/agentictasks.yaml\n" +
		"R  old/path.go -> pkg/foreman/agent/renamed.go\n" +
		"\n"
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "git" && len(args) >= 2 && args[0] == "status" && args[1] == "--porcelain" {
			return porcelain, nil
		}
		return "", nil
	}
	got := dirtyPathSet(context.Background(), "/work", run)
	want := []string{
		"api/foreman/v1alpha1/agentictask_types.go",
		"charts/foreman/templates/crds/agentictasks.yaml",
		"pkg/foreman/agent/renamed.go",
	}
	if len(got) != len(want) {
		t.Fatalf("dirtyPathSet returned %d paths, want %d: %v", len(got), len(want), got)
	}
	for _, p := range want {
		if !got[p] {
			t.Errorf("dirtyPathSet missing %q; got %v", p, got)
		}
	}
}

// TestDirtyPathSet_GitErrorYieldsEmpty verifies a git error degrades to an
// empty set so the codegen tier does not fail spuriously.
func TestDirtyPathSet_GitErrorYieldsEmpty(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, _ string, _ ...string) (string, error) {
		return "", errors.New("not a git repo")
	}
	if got := dirtyPathSet(context.Background(), "/work", run); len(got) != 0 {
		t.Errorf("expected empty set on git error, got %v", got)
	}
}

// TestChangedEnvtestPackages returns only the envtest-matching changed
// packages, while the existing exclude tier still returns only the
// non-envtest ones (same git-status input).
func TestChangedEnvtestPackages(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, name string, _ ...string) (string, error) {
		if name == "git" {
			return " M pkg/cli/cache_inspect.go\x00" +
				" M internal/controller/model_controller.go\x00" +
				" M internal/foreman/controller/agentictask_controller.go\x00" +
				" M test/e2e/e2e_test.go\x00" +
				" M README.md\x00", nil
		}
		return "", nil
	}

	nonEnvtest := changedTestPackages(context.Background(), "/work", run)
	envtest := changedEnvtestPackages(context.Background(), "/work", run)

	// Non-envtest packages should not include any envtest paths.
	wantNonEnvtest := map[string]bool{"./pkg/cli/": true}
	if len(nonEnvtest) != len(wantNonEnvtest) {
		t.Fatalf("changedTestPackages = %v, want exactly %v", nonEnvtest, wantNonEnvtest)
	}
	for _, p := range nonEnvtest {
		if !wantNonEnvtest[p] {
			t.Errorf("unexpected non-envtest package %q", p)
		}
	}

	// Envtest packages should include only envtest paths.
	wantEnvtest := map[string]bool{
		"./internal/controller/":         true,
		"./internal/foreman/controller/": true,
		"./test/e2e/":                    true,
	}
	if len(envtest) != len(wantEnvtest) {
		t.Fatalf("changedEnvtestPackages = %v, want exactly %v", envtest, wantEnvtest)
	}
	for _, p := range envtest {
		if !wantEnvtest[p] {
			t.Errorf("unexpected envtest package %q", p)
		}
	}

	// The two sets must be disjoint.
	for _, p := range nonEnvtest {
		for _, q := range envtest {
			if p == q {
				t.Errorf("overlap between non-envtest and envtest: %q", p)
			}
		}
	}
}

// TestIsEnvtestPackage verifies the helper classifies package paths
// against envtestPackagePrefixes.
func TestIsEnvtestPackage(t *testing.T) {
	tests := []struct {
		pkg  string
		want bool
	}{
		{"./internal/controller/", true},
		{"./internal/controller/foo/", true},
		{"./internal/foreman/controller/", true},
		{"./test/", true},
		{"./test/e2e/", true},
		{"./pkg/cli/", false},
		{"./pkg/foreman/agent/", false},
		{"./cmd/", false},
	}
	for _, tt := range tests {
		got := isEnvtestPackage(tt.pkg)
		if got != tt.want {
			t.Errorf("isEnvtestPackage(%q) = %v, want %v", tt.pkg, got, tt.want)
		}
	}
}

// withScopeRelevant overrides the scopeRelevantFiles seam with a canned set
// for the duration of the test.
func withScopeRelevant(t *testing.T, paths []string) {
	t.Helper()
	orig := scopeRelevantFiles
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	scopeRelevantFiles = func(_, _ string) ([]string, map[string]bool) { return paths, set }
	t.Cleanup(func() { scopeRelevantFiles = orig })
}

// diffRunner returns a commandRunner that mocks `git add -A` as a no-op and
// returns the given name-only diff output for `git diff --name-only --cached`.
// All other commands succeed silently. This is the seam used by
// changedWorkingTreeGoFiles (the scope-overlap check's working-tree diff).
func diffRunner(diffOutput string) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "git" && len(args) >= 1 && args[0] == "add" && len(args) >= 2 && args[1] == "-A" {
			return "", nil
		}
		if name == "git" && len(args) >= 2 && args[0] == "diff" && args[1] == "--name-only" && args[2] == "--cached" {
			return diffOutput, nil
		}
		return "", nil
	}
}

// TestInScopeCount pins the percentile floor (#1180): a file is in-scope
// unless it falls in the bottom quartile of positively-scored files, but the
// legacy scopeRelevantTopK stays a floor so the change is strictly more
// permissive than the old top-50-only rule (it can only remove false
// positives). ceil(0.75*n) wins only once it exceeds scopeRelevantTopK.
func TestInScopeCount(t *testing.T) {
	tests := []struct {
		n    int
		want int
	}{
		{0, 0},
		{1, 1},
		{10, 10},   // below the floor: all in scope, never drift for tiny repos
		{50, 50},   // exactly the floor
		{60, 50},   // ceil(0.75*60)=45 < 50 floor
		{68, 51},   // ceil(0.75*68)=51 > 50 floor
		{100, 75},  // ceil(0.75*100)=75
		{245, 184}, // the #1116 repo: ceil(0.75*245)=184
	}
	for _, tc := range tests {
		if got := inScopeCount(tc.n); got != tc.want {
			t.Errorf("inScopeCount(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

// TestScopeRelevantView_BottomQuartileIsDrift pins #1180 end to end at the
// pure core: a relevant-but-lower-ranked file (rank 91 of 245, the measured
// #1116 progress.go position) must be in scope, while a bottom-quartile file
// must not be. The display slice stays capped at scopeRelevantTopK.
func TestScopeRelevantView_BottomQuartileIsDrift(t *testing.T) {
	const n = 245
	ranked := make([]string, n)
	for i := range ranked {
		ranked[i] = fmt.Sprintf("f%03d.go", i) // index i == rank i+1
	}
	paths, set := scopeRelevantView(ranked)

	if !set["f090.go"] { // rank 91: top 37%, clearly relevant
		t.Error("rank-91/245 file must be in scope (top 37%), not flagged as drift")
	}
	if set["f239.go"] { // rank 240: bottom quartile
		t.Error("rank-240/245 file (bottom quartile) must be out of scope")
	}
	if len(paths) != scopeRelevantTopK {
		t.Errorf("display list should be capped at %d, got %d", scopeRelevantTopK, len(paths))
	}
}

func TestCheckScopeOverlap_DriftFlagged(t *testing.T) {
	withScopeRelevant(t, []string{"pkg/agent/endpoint.go", "pkg/agent/health.go"})
	run := diffRunner("pkg/cli/cache.go\n")
	drift, fb := checkScopeOverlap(context.Background(), "/work", run, "metal-agent endpoint health")
	if !drift {
		t.Fatal("expected drift when the changed Go file is outside the relevant set")
	}
	if !strings.Contains(fb, "pkg/cli/cache.go") || !strings.Contains(fb, "pkg/agent/endpoint.go") {
		t.Errorf("feedback should name changed + relevant files; got:\n%s", fb)
	}
}

func TestCheckScopeOverlap_InScopePasses(t *testing.T) {
	withScopeRelevant(t, []string{"pkg/agent/endpoint.go"})
	// Touches a relevant file plus an unrelated one: any overlap is in scope.
	run := diffRunner("pkg/agent/endpoint.go\npkg/cli/cache.go\n")
	if drift, _ := checkScopeOverlap(context.Background(), "/work", run, "x"); drift {
		t.Error("expected no drift when a changed file is in the relevant set")
	}
}

func TestCheckScopeOverlap_SkipsNonGoOnlyChanges(t *testing.T) {
	withScopeRelevant(t, []string{"pkg/agent/endpoint.go"})
	run := diffRunner("charts/foreman/x.yaml\ndocs/readme.md\n")
	if drift, _ := checkScopeOverlap(context.Background(), "/work", run, "x"); drift {
		t.Error("a yaml/docs-only change must not be flagged by the Go-aware guard")
	}
}

func TestCheckScopeOverlap_SkipsTestOnlyChanges(t *testing.T) {
	withScopeRelevant(t, []string{"pkg/agent/endpoint.go"})
	run := diffRunner("pkg/cli/cache_test.go\n")
	if drift, _ := checkScopeOverlap(context.Background(), "/work", run, "x"); drift {
		t.Error("a test-only change must not be judged")
	}
}

func TestCheckScopeOverlap_SkipsWhenNoGoSignal(t *testing.T) {
	withScopeRelevant(t, nil) // issue produces no positively-scored Go files
	run := diffRunner("pkg/cli/cache.go\n")
	if drift, _ := checkScopeOverlap(context.Background(), "/work", run, "x"); drift {
		t.Error("no Go signal -> observe-only, no flag")
	}
}

func TestCheckScopeOverlap_SkipsWhenIssueTextEmpty(t *testing.T) {
	withScopeRelevant(t, []string{"pkg/agent/endpoint.go"})
	run := diffRunner("pkg/cli/cache.go\n")
	if drift, _ := checkScopeOverlap(context.Background(), "/work", run, ""); drift {
		t.Error("empty issueText must disable the scope check")
	}
}

func TestCheckScopeOverlap_CatchesGoFileInNewDirectory(t *testing.T) {
	// Regression for #907. The prior `git status --porcelain` path already
	// listed untracked files in *tracked* directories, but collapsed a
	// brand-new untracked directory to a single "newdir/" entry, which fails
	// the .go suffix filter -- so an out-of-scope Go file created in a new
	// directory slipped past scope-overlap. Staging (`git add -A`) then
	// `git diff --name-only --cached HEAD` lists each new file individually,
	// closing that gap. This test guards the working-tree-diff command seam:
	// the mocked diff reports a Go file in a new directory, which must drift.
	withScopeRelevant(t, []string{"pkg/agent/endpoint.go"})
	run := diffRunner("newpkg/thing.go\n")
	drift, _ := checkScopeOverlap(context.Background(), "/work", run, "metal-agent endpoint health")
	if !drift {
		t.Error("expected drift when an out-of-scope Go file in a new directory is changed")
	}
}

// gateRunnerAllPassExcept builds a runner where gofmt/vet/build/lint pass,
// codegen is skipped (no controller-gen), no changed test packages, and
// `git add -A` + `git diff --name-only --cached` report the given porcelain
// (for the scope tier). The porcelain string is a newline-separated list of
// workspace-relative paths, matching `git diff --name-only` output.
func gateRunnerScope(golangciPath, porcelain string) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		switch {
		case name == "gofmt", name == "go", name == golangciPath:
			return "", nil
		case name == "test" && len(args) > 0 && args[0] == "-f":
			return "", errors.New("no controller-gen") // skip codegen tier
		case name == "git" && len(args) >= 2 && args[0] == "status" && args[1] == "-z":
			return "", nil // no changed test packages
		case name == "git" && len(args) >= 1 && args[0] == "add" && len(args) >= 2 && args[1] == "-A":
			return "", nil // stage everything (no-op in tests)
		case name == "git" && len(args) >= 2 && args[0] == "diff" && args[1] == "--name-only" && args[2] == "--cached":
			return porcelain, nil
		default:
			return "", nil
		}
	}
}

func TestRunCoderGate_ScopeDriftFailsTheGate(t *testing.T) {
	withScopeRelevant(t, []string{"pkg/agent/endpoint.go"})
	const golangciPath = "./bin/golangci-lint"
	run := gateRunnerScope(golangciPath, " M pkg/cli/cache.go\n")
	pass, fb, _ := RunCoderGate(context.Background(), "/work", golangciPath, run, "metal-agent endpoint", "main", nil)
	if pass {
		t.Fatal("gate should fail when the coder drifts to an unrelated subsystem")
	}
	if !strings.Contains(fb, "scope overlap") {
		t.Errorf("feedback should cite scope overlap; got:\n%s", fb)
	}
}

func TestRunCoderGate_ScopeDisabledWhenIssueTextEmpty(t *testing.T) {
	withScopeRelevant(t, []string{"pkg/agent/endpoint.go"})
	const golangciPath = "./bin/golangci-lint"
	run := gateRunnerScope(golangciPath, " M pkg/cli/cache.go\n")
	pass, fb, _ := RunCoderGate(context.Background(), "/work", golangciPath, run, "", "main", nil)
	if !pass {
		t.Fatalf("empty issueText should disable the scope check; gate should pass. feedback:\n%s", fb)
	}
}

func TestReleaseConfigChanged(t *testing.T) {
	tests := []struct {
		name  string
		dirty map[string]bool
		want  bool
	}{
		{
			name:  "no release config changed",
			dirty: map[string]bool{"pkg/cli/cache.go": true},
			want:  false,
		},
		{
			name:  ".goreleaser.yaml changed",
			dirty: map[string]bool{".goreleaser.yaml": true},
			want:  true,
		},
		{
			name:  "Dockerfile.goreleaser changed",
			dirty: map[string]bool{"Dockerfile.goreleaser": true},
			want:  true,
		},
		{
			name:  "Dockerfile.foreman-agent.goreleaser changed",
			dirty: map[string]bool{"Dockerfile.foreman-agent.goreleaser": true},
			want:  true,
		},
		{
			name:  "Dockerfile.router-proxy.goreleaser changed",
			dirty: map[string]bool{"Dockerfile.router-proxy.goreleaser": true},
			want:  true,
		},
		{
			name:  "Dockerfile.goreleaser and .goreleaser.yaml both changed",
			dirty: map[string]bool{".goreleaser.yaml": true, "Dockerfile.goreleaser": true},
			want:  true,
		},
		{
			name:  "Dockerfile.goreleaser not matched by plain Dockerfile",
			dirty: map[string]bool{"Dockerfile": true},
			want:  false,
		},
		{
			name:  "Dockerfile.goreleaser not matched by Dockerfile with other suffix",
			dirty: map[string]bool{"Dockerfile.manager": true},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := releaseConfigChanged(tt.dirty); got != tt.want {
				t.Errorf("releaseConfigChanged(%v) = %v, want %v", tt.dirty, got, tt.want)
			}
		})
	}
}

// goreleaserFake builds a commandRunner for the goreleaser-config tier.
// All other gate checks pass. The goreleaserAvailable flag controls whether
// `which goreleaser` succeeds. goreleaserCheckErr controls the result of
// `goreleaser check`. goreleaserPorcelain controls the dirty set for the
// goreleaser check (the codegen tier is skipped by returning an error from
// `test -f`).
func goreleaserFake(
	goreleaserAvailable bool,
	goreleaserCheckErr error,
	goreleaserCheckOutput string,
	goreleaserPorcelain string,
) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		switch {
		case name == "gofmt", name == "go", name == gateLintPath:
			return "", nil
		case name == "test" && len(args) > 0 && args[0] == "-f":
			return "", errors.New("no controller-gen") // skip codegen tier
		case name == "git" && len(args) >= 2 && args[0] == "status" && args[1] == "-z":
			return "", nil // no changed test packages
		case name == "git" && len(args) >= 2 && args[0] == "status" && args[1] == "--porcelain":
			return goreleaserPorcelain, nil
		case name == "which" && len(args) > 0 && args[0] == "goreleaser":
			if goreleaserAvailable {
				return "/usr/local/bin/goreleaser", nil
			}
			return "", errors.New("exit status 1")
		case name == "goreleaser" && len(args) > 0 && args[0] == "check":
			return goreleaserCheckOutput, goreleaserCheckErr
		default:
			return "", nil
		}
	}
}

func TestCheckGoreleaserConfig_SkippedWhenNoReleaseConfigChanged(t *testing.T) {
	run := goreleaserFake(true, nil, "", "")
	failed, out := checkGoreleaserConfig(context.Background(), "/work", run)
	if failed {
		t.Fatal("should not fail when no release config files changed")
	}
	if out != "" {
		t.Errorf("expected empty output, got %q", out)
	}
}

func TestCheckGoreleaserConfig_SkippedWhenGoreleaserNotAvailable(t *testing.T) {
	run := goreleaserFake(false, nil, "", " M .goreleaser.yaml\n")
	failed, out := checkGoreleaserConfig(context.Background(), "/work", run)
	if failed {
		t.Fatal("should not fail when goreleaser is not available")
	}
	if out != "" {
		t.Errorf("expected empty output, got %q", out)
	}
}

func TestCheckGoreleaserConfig_PassesWhenCheckSucceeds(t *testing.T) {
	run := goreleaserFake(true, nil, "", " M .goreleaser.yaml\n")
	failed, out := checkGoreleaserConfig(context.Background(), "/work", run)
	if failed {
		t.Fatalf("should not fail when goreleaser check passes; output: %q", out)
	}
}

func TestCheckGoreleaserConfig_FailsWhenCheckFails(t *testing.T) {
	checkErr := errors.New("exit status 1")
	checkOutput := "error: invalid key 'dockers_v2'\n"
	run := goreleaserFake(true, checkErr, checkOutput, " M .goreleaser.yaml\n")
	failed, out := checkGoreleaserConfig(context.Background(), "/work", run)
	if !failed {
		t.Fatal("should fail when goreleaser check fails")
	}
	if !strings.Contains(out, "goreleaser check failed") {
		t.Errorf("output should mention 'goreleaser check failed'; got: %q", out)
	}
	if !strings.Contains(out, "invalid key") {
		t.Errorf("output should include goreleaser error; got: %q", out)
	}
}

func TestRunCoderGate_GoreleaserCheckFailsGate(t *testing.T) {
	checkErr := errors.New("exit status 1")
	checkOutput := "error: invalid key 'dockers_v2'\n"
	run := goreleaserFake(true, checkErr, checkOutput, " M .goreleaser.yaml\n")
	pass, fb, _ := RunCoderGate(context.Background(), "/work", gateLintPath, run, "", "main", nil)
	if pass {
		t.Fatal("gate should fail when goreleaser check fails")
	}
	if !strings.Contains(fb, "goreleaser check") {
		t.Errorf("feedback should cite 'goreleaser check'; got:\n%s", fb)
	}
}

func TestRunCoderGate_GoreleaserCheckPassesGate(t *testing.T) {
	run := goreleaserFake(true, nil, "", " M .goreleaser.yaml\n")
	pass, fb, _ := RunCoderGate(context.Background(), "/work", gateLintPath, run, "", "main", nil)
	if !pass {
		t.Fatalf("gate should pass when goreleaser check passes; feedback:\n%s", fb)
	}
	if fb != "" {
		t.Errorf("expected empty feedback, got %q", fb)
	}
}

func TestRunCoderGate_GoreleaserCheckSkippedWhenNoReleaseConfigChanged(t *testing.T) {
	run := goreleaserFake(true, nil, "", "")
	pass, fb, _ := RunCoderGate(context.Background(), "/work", gateLintPath, run, "", "main", nil)
	if !pass {
		t.Fatalf("gate should pass when no release config changed; feedback:\n%s", fb)
	}
}

// TestRunCoderGate_FailsOnUngroundedReference verifies that check #11 hard-fails
// the gate when an added doc references an LLMKube API group that does not exist
// in the repo's CRDs. All other gate checks must pass with this fake runner.
func TestRunCoderGate_FailsOnUngroundedReference(t *testing.T) {
	ws := t.TempDir()
	crd := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n" +
		"spec:\n  group: inference.llmkube.dev\n  names:\n    kind: InferenceService\n" +
		"  versions:\n    - name: v1alpha1\n      schema:\n        openAPIV3Schema:\n" +
		"          properties:\n            spec:\n              properties:\n" +
		"                modelRef:\n                  type: string\n"
	if err := os.MkdirAll(filepath.Join(ws, "config/crd/bases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "config/crd/bases/is.yaml"), []byte(crd), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
		// Check #11 diffs the staged working tree: git diff --cached --unified=0
		// --src-prefix=a/ --dst-prefix=b/ HEAD -- *.md *.yaml *.yml
		if name == "git" && len(args) >= 2 && args[0] == "diff" && args[1] == "--cached" {
			return "+++ b/docs/x.md\n@@ -0,0 +1,1 @@\n+apiVersion: llmkube.io/v1alpha1\n", nil
		}
		// All other commands pass silently (gofmt empty, go/lint success,
		// git add/status no-ops).
		return "", nil
	}
	pass, feedback, _ := RunCoderGate(context.Background(), ws, "./bin/golangci-lint", run, "", "main", nil)
	if pass {
		t.Fatalf("expected gate to fail on ungrounded reference; feedback=%q", feedback)
	}
	if !strings.Contains(feedback, "reference grounding") {
		t.Errorf("feedback should name the check: %q", feedback)
	}
}

// TestCheckReferenceGrounding_IgnoresExporterMetricTokens is the contamination
// regression test. Before the fix, LoadGroundTruth seeded ExporterMetricPrefixes
// unconditionally; the block-tier checkReferenceGrounding did not filter by
// severity, so "minor" exporter-metric findings would cause the gate to false-block
// a coder whose doc contained a legitimate snake_case token (n_ctx, executor_native,
// node_selector, Q4_K_M, ...).
//
// After the fix: LoadGroundTruth leaves ExporterMetricPrefixes nil, so
// checkExporterMetricTokens is inert in the block tier. This test proves that
// adding a doc line with several legitimate snake_case tokens does NOT cause
// checkReferenceGrounding to fail.
func TestCheckReferenceGrounding_IgnoresExporterMetricTokens(t *testing.T) {
	ws := t.TempDir()

	// Write a minimal CRD so the ground-truth load succeeds.
	crd := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n" +
		"spec:\n  group: inference.llmkube.dev\n  names:\n    kind: InferenceService\n" +
		"  versions: []\n"
	if err := os.MkdirAll(filepath.Join(ws, "config/crd/bases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "config/crd/bases/is.yaml"), []byte(crd), 0o644); err != nil {
		t.Fatal(err)
	}

	// The fake git diff adds a doc line containing several legitimate
	// snake_case tokens that are NOT LLMKube-owned API groups or metrics.
	// These should pass the block-tier check without any findings.
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "git" && len(args) >= 2 && args[0] == "diff" && args[1] == "--cached" {
			return "+++ b/docs/inference.md\n@@ -0,0 +1,3 @@\n" +
				"+Set n_ctx=4096 and executor_native handles Q4_K_M quants via node_selector.\n" +
				"+The llama_model_load path resolves llmkube_inferenceservice_phase for monitoring.\n" +
				"+Use node_affinity or gpu_layers to tune scheduling.\n", nil
		}
		return "", nil
	}

	// checkReferenceGrounding must not fail on these lines. The only LLMKube
	// metric on line 2 (llmkube_inferenceservice_phase) is registered in the
	// real ground truth only when metricsDir is provided; here we call the
	// function directly with an empty workspace so metrics scanning is a no-op
	// and the llmkube_* check is also inert (empty gt.Metrics). The key
	// assertion is that snake_case non-llmkube tokens do NOT block.
	failed, output := checkReferenceGrounding(context.Background(), ws, run)
	if failed {
		t.Fatalf("block-tier reference grounding must not fail on snake_case tokens "+
			"(n_ctx, executor_native, Q4_K_M, node_selector, llama_model_load); "+
			"got failed=true output=%q", output)
	}
}

// TestBuildFeedback_GocycloAdvisory pins the steer appended when golangci-lint
// reports gocyclo. The raw dump alone burned all three gate attempts on the
// #982 run: the model re-submitted the same over-complex function each time.
func TestBuildFeedback_GocycloAdvisory(t *testing.T) {
	out := buildFeedback([]checkFailure{{
		name: "golangci-lint run ./...",
		output: "pkg/foreman/agent/executor_native.go:407:1: cyclomatic complexity 31 " +
			"of func `(*NativeAgentLoopExecutor).runLLMPath` is high (> 30) (gocyclo)",
	}})
	want := "Do not add more branches to `(*NativeAgentLoopExecutor).runLLMPath`."
	if !strings.Contains(out, want) {
		t.Fatalf("feedback missing gocyclo steer %q; got:\n%s", want, out)
	}
	if !strings.Contains(out, "helper function") {
		t.Fatalf("steer should tell the model to extract a helper; got:\n%s", out)
	}
}

func TestBuildFeedback_NoAdvisoryForUnknownFailure(t *testing.T) {
	out := buildFeedback([]checkFailure{{
		name:   "test presence",
		output: "Package pkg/x/ changed functions (Foo) have no test referencing them by name.",
	}})
	if strings.Contains(out, "Advice:") {
		t.Fatalf("non-structural failure must get no advisory; got:\n%s", out)
	}
}

func TestLintAdvisories_DuplAndFunlen(t *testing.T) {
	steers := lintAdvisories("a.go:1: 20-40 lines are duplicate of `b.go:5-25` (dupl)\n" +
		"c.go:9: Function 'bigOne' is too long (75 > 60) (funlen)\n")
	if len(steers) != 2 {
		t.Fatalf("want 2 steers (dupl + funlen), got %d: %v", len(steers), steers)
	}
	if !strings.Contains(steers[0], "duplicated block") || !strings.Contains(steers[1], "too long") {
		t.Fatalf("unexpected steer content: %v", steers)
	}
}

func TestLintAdvisories_DedupesAndCaps(t *testing.T) {
	line := "x.go:1:1: cyclomatic complexity 31 of func `Alpha` is high (> 30) (gocyclo)\n"
	// Same func twice -> one steer; four distinct funcs -> capped at 3.
	steers := lintAdvisories(line + line)
	if len(steers) != 1 {
		t.Fatalf("duplicate gocyclo func must dedupe to 1 steer, got %d", len(steers))
	}
	many := "x.go:1:1: cyclomatic complexity 31 of func `A` is high (> 30) (gocyclo)\n" +
		"x.go:2:1: cyclomatic complexity 31 of func `B` is high (> 30) (gocyclo)\n" +
		"x.go:3:1: cyclomatic complexity 31 of func `C` is high (> 30) (gocyclo)\n" +
		"x.go:4:1: cyclomatic complexity 31 of func `D` is high (> 30) (gocyclo)\n"
	steers = lintAdvisories(many)
	if len(steers) != maxLintAdvisories {
		t.Fatalf("advisories must cap at %d, got %d", maxLintAdvisories, len(steers))
	}
}
