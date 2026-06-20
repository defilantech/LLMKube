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
			pass, feedback := RunCoderGate(context.Background(), "/work", golangciPath, run)

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

	RunCoderGate(context.Background(), "/work", golangciPath, run)

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

// TestChangedTestPackages_ExcludesEnvtestAndDedups verifies the changed
// package set covers non-envtest packages, dedups, and excludes
// envtest/integration packages and non-Go files (#762).
func TestChangedTestPackages_ExcludesEnvtestAndDedups(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, name string, _ ...string) (string, error) {
		if name == "git" {
			return " M pkg/cli/cache_inspect.go\n" +
				" M pkg/cli/cache_inspect_test.go\n" +
				"?? pkg/foreman/agent/loop_session_test.go\n" +
				" M internal/controller/model_controller.go\n" +
				" M internal/foreman/controller/agentictask_controller.go\n" +
				" M test/e2e/e2e_test.go\n" +
				" M README.md\n", nil
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
			return " M pkg/cli/cache_inspect_test.go\n", nil
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
	pass, feedback := RunCoderGate(context.Background(), "/work", "./bin/golangci-lint", run)
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
	if pass, _ := RunCoderGate(context.Background(), "/work", "./bin/golangci-lint", run); !pass {
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

	_, feedback := RunCoderGate(context.Background(), "/work", golangciPath, run)

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
