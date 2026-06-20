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
