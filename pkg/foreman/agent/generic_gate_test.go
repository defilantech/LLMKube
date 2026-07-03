/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func TestUsesGenericGate(t *testing.T) {
	cases := []struct {
		name    string
		profile *foremanv1alpha1.GateProfile
		want    bool
	}{
		{"nil profile -> go path", nil, false},
		{"empty language -> go path", &foremanv1alpha1.GateProfile{}, false},
		{"explicit go -> go path", &foremanv1alpha1.GateProfile{Language: foremanv1alpha1.GateLanguageGo}, false},
		{"python -> generic", &foremanv1alpha1.GateProfile{Language: foremanv1alpha1.GateLanguagePython}, true},
		{"rust -> generic", &foremanv1alpha1.GateProfile{Language: foremanv1alpha1.GateLanguageRust}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usesGenericGate(tc.profile); got != tc.want {
				t.Errorf("usesGenericGate = %v, want %v", got, tc.want)
			}
		})
	}
}

type gateCall struct {
	name string
	args []string
}

func TestRunGenericGateAllPassRunsEachViaShC(t *testing.T) {
	var calls []gateCall
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		calls = append(calls, gateCall{name, args})
		return "", nil
	}
	gate := foremanv1alpha1.ResolvedGate{
		Format: "ruff format --check .",
		Lint:   "ruff check .",
		Build:  "python -m compileall .",
		Test:   "pytest -q",
	}
	pass, fb, advisories := RunGenericGate(context.Background(), "/ws", gate, run)
	if !pass || fb != "" {
		t.Fatalf("want pass with empty feedback; got pass=%v feedback=%q", pass, fb)
	}
	if len(advisories) != 0 {
		t.Fatalf("want no advisories on a clean pass, got %+v", advisories)
	}
	if len(calls) != 4 {
		t.Fatalf("want 4 commands run (empty codegen skipped), got %d: %+v", len(calls), calls)
	}
	for _, c := range calls {
		if c.name != "sh" || len(c.args) != 2 || c.args[0] != "-c" {
			t.Errorf("command not run via `sh -c`: %+v", c)
		}
	}
}

func TestRunGenericGateSkipsEmptyAndCollectsFailures(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		if strings.Contains(cmd, "ruff check") || strings.Contains(cmd, "pytest") {
			return "boom", errors.New("exit status 1")
		}
		return "", nil
	}
	gate := foremanv1alpha1.ResolvedGate{
		// Format, Build, Codegen empty -> skipped.
		Lint: "ruff check .",
		Test: "pytest -q",
	}
	pass, fb, advisories := RunGenericGate(context.Background(), "/ws", gate, run)
	if pass {
		t.Fatal("want fail when lint and test fail")
	}
	if !strings.Contains(fb, "ruff check") || !strings.Contains(fb, "pytest") {
		t.Errorf("feedback should name both failing checks; got %q", fb)
	}
	if len(advisories) != 0 {
		t.Fatalf("real command failures must not produce deferral advisories, got %+v", advisories)
	}
}

// fakeExitErr is an error exposing an exit code, standing in for
// *exec.ExitError in table-driven tests without shelling out.
type fakeExitErr struct{ code int }

func (e fakeExitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e fakeExitErr) ExitCode() int { return e.code }

func TestIsMissingRuntime(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		output string
		want   bool
	}{
		{"nil error never defers", nil, "sh: npm: not found", false},
		{"exit 127 defers", fakeExitErr{127}, "", true},
		{"exec.ErrNotFound defers", exec.ErrNotFound, "", true},
		{"wrapped exec.ErrNotFound defers", fmt.Errorf("run: %w", exec.ErrNotFound), "", true},
		{"bash-style command not found defers", errors.New("exit status 127"),
			"sh: npm: command not found\n", true},
		{"dash-style not found defers", fakeExitErr{2}, "sh: 1: python3: not found\n", true},
		{"plain test failure does not defer", fakeExitErr{1}, "FAIL tests/test_api.py\n", false},
		{"pytest fixture not found does not defer", fakeExitErr{1},
			"E fixture 'db' not found\n", false},
		{"http 404 output does not defer", fakeExitErr{1}, "npm ERR! 404 Not Found\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMissingRuntime(tc.err, tc.output); got != tc.want {
				t.Errorf("isMissingRuntime(%v, %q) = %v, want %v", tc.err, tc.output, got, tc.want)
			}
		})
	}
}

func TestRunGenericGateMissingRuntimeDefersNotFails(t *testing.T) {
	// npm is absent from the (Go-only) coder image: the shell exits 127.
	run := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "npm") {
			return "sh: npm: not found\n", fakeExitErr{127}
		}
		return "", nil
	}
	gate := foremanv1alpha1.ResolvedGate{
		Lint: "eslint .",
		Test: "npm ci && npm test",
	}
	pass, fb, advisories := RunGenericGate(context.Background(), "/ws", gate, run)
	if !pass || fb != "" {
		t.Fatalf("missing runtime must not fail the self-gate; got pass=%v feedback=%q", pass, fb)
	}
	if len(advisories) != 1 {
		t.Fatalf("want 1 deferral advisory, got %+v", advisories)
	}
	if advisories[0].Check != genericGateDeferredCheck {
		t.Errorf("advisory check = %q, want %q", advisories[0].Check, genericGateDeferredCheck)
	}
	if !strings.Contains(advisories[0].Detail, "self-gate deferred: runtime missing") ||
		!strings.Contains(advisories[0].Detail, "npm ci && npm test") {
		t.Errorf("advisory should say the runtime is missing and name the command; got %q", advisories[0].Detail)
	}
}

func TestRunGenericGateMixedDeferralAndRealFailure(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		switch {
		case strings.Contains(cmd, "npm"):
			return "sh: npm: command not found\n", fakeExitErr{127}
		case strings.Contains(cmd, "ruff"):
			return "would reformat app.py\n", fakeExitErr{1}
		}
		return "", nil
	}
	gate := foremanv1alpha1.ResolvedGate{
		Format: "ruff format --check .",
		Test:   "npm test",
	}
	pass, fb, advisories := RunGenericGate(context.Background(), "/ws", gate, run)
	if pass {
		t.Fatal("a real failure must still fail the gate even when another check deferred")
	}
	if !strings.Contains(fb, "ruff format") {
		t.Errorf("feedback should name the real failure; got %q", fb)
	}
	if strings.Contains(fb, "npm test") {
		t.Errorf("feedback must not report the deferred check as a failure; got %q", fb)
	}
	if len(advisories) != 1 || !strings.Contains(advisories[0].Detail, "npm test") {
		t.Errorf("want one deferral advisory naming the npm command, got %+v", advisories)
	}
}

// TestRunGenericGateRealExec exercises the production execCommandRunner:
// a genuinely absent binary defers, a command that runs and fails still
// fails, and a passing command passes.
func TestRunGenericGateRealExec(t *testing.T) {
	ws := t.TempDir()

	t.Run("missing binary defers", func(t *testing.T) {
		gate := foremanv1alpha1.ResolvedGate{Test: "llmkube-no-such-binary-929 --version"}
		pass, fb, advisories := RunGenericGate(context.Background(), ws, gate, execCommandRunner)
		if !pass || fb != "" {
			t.Fatalf("want deferral (pass, no feedback); got pass=%v feedback=%q", pass, fb)
		}
		if len(advisories) != 1 || !strings.Contains(advisories[0].Detail, "runtime missing") {
			t.Fatalf("want a runtime-missing advisory, got %+v", advisories)
		}
	})

	t.Run("real failure still fails", func(t *testing.T) {
		gate := foremanv1alpha1.ResolvedGate{Test: "echo tests failed; exit 1"}
		pass, fb, advisories := RunGenericGate(context.Background(), ws, gate, execCommandRunner)
		if pass {
			t.Fatal("a command that runs and exits non-zero must fail the gate")
		}
		if !strings.Contains(fb, "tests failed") {
			t.Errorf("feedback should carry the command output; got %q", fb)
		}
		if len(advisories) != 0 {
			t.Errorf("want no advisories for a real failure, got %+v", advisories)
		}
	})

	t.Run("passing command passes", func(t *testing.T) {
		gate := foremanv1alpha1.ResolvedGate{Test: "true"}
		pass, fb, advisories := RunGenericGate(context.Background(), ws, gate, execCommandRunner)
		if !pass || fb != "" || len(advisories) != 0 {
			t.Fatalf("want clean pass; got pass=%v feedback=%q advisories=%+v", pass, fb, advisories)
		}
	})
}
