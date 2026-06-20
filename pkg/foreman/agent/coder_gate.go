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
	"os/exec"
	"path/filepath"
	"strings"
)

// envtestPackagePrefixes are workspace-relative package path prefixes whose
// tests require KUBEBUILDER_ASSETS (envtest) or a live cluster, which the
// coder workspace does not have. The fast gate's unit-test tier skips them
// (running them hangs); CI runs them separately.
var envtestPackagePrefixes = []string{
	"internal/controller/",
	"internal/foreman/controller/",
	"test/",
}

// maxCheckOutputBytes bounds the captured output included in the gate
// feedback for each failing check, so a noisy compiler or linter cannot
// produce an unbounded user message. Output longer than this is truncated.
const maxCheckOutputBytes = 8 * 1024

// commandRunner runs one command in dir with extra env vars (KEY=VALUE)
// appended to the process environment, returning combined stdout+stderr
// and the exec error. Injectable so tests do not shell out.
type commandRunner func(
	ctx context.Context,
	dir string,
	extraEnv []string,
	name string,
	args ...string,
) (output string, err error)

// execCommandRunner is the production commandRunner backed by os/exec. It
// appends extraEnv to the inherited process environment and captures
// combined stdout+stderr. Wired into the coder agent loop via
// makeCoderGateVerifier as the runner passed to RunCoderGate.
var execCommandRunner commandRunner = func(
	ctx context.Context,
	dir string,
	extraEnv []string,
	name string,
	args ...string,
) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// checkFailure records a single failing verification check and the output
// that explains the failure.
type checkFailure struct {
	name   string
	output string
}

// RunCoderGate runs the fast in-workspace verification tier against a
// coder's workspace and reports whether every check passed. On failure,
// feedback is a directive the agent loop injects as a user message:
// it names the failing check(s) and includes their output so the model
// can fix the issue and resubmit. golangciPath is the path to the
// golangci-lint binary (e.g. "./bin/golangci-lint"). run is the command
// runner (production callers pass execCommandRunner).
//
// The gate runs four deterministic checks in order: gofmt, go vet,
// go build, and golangci-lint. Heavy envtest or integration tests are
// intentionally out of scope; they run in a separate clean-room
// Kubernetes Job. All four checks run regardless of earlier failures so
// the feedback reports everything wrong at once.
func RunCoderGate(ctx context.Context, workspace, golangciPath string, run commandRunner) (pass bool, feedback string) {
	var failures []checkFailure

	// 1. gofmt -l . lists misformatted files on stdout and exits 0 even
	// when files are listed, so the failure signal is non-empty output,
	// not the exec error.
	if out, _ := run(ctx, workspace, nil, "gofmt", "-l", "."); strings.TrimSpace(out) != "" {
		failures = append(failures, checkFailure{name: "gofmt -l .", output: out})
	}

	// 2. go vet ./... fails with a non-nil error.
	if out, err := run(ctx, workspace, nil, "go", "vet", "./..."); err != nil {
		failures = append(failures, checkFailure{name: "go vet ./...", output: out})
	}

	// 3. go build ./... fails with a non-nil error.
	if out, err := run(ctx, workspace, nil, "go", "build", "./..."); err != nil {
		failures = append(failures, checkFailure{name: "go build ./...", output: out})
	}

	// 4. golangci-lint run ./... fails with a non-nil error. GOOS=linux is
	// required: on macOS, plain lint silently skips //go:build !darwin
	// files and would not match CI.
	if out, err := run(ctx, workspace, []string{"GOOS=linux"}, golangciPath, "run", "./..."); err != nil {
		failures = append(failures, checkFailure{name: golangciPath + " run ./...", output: out})
	}

	// 5. Fast unit-test tier: go test on the non-envtest packages the coder
	// changed. The static checks above cannot catch a failing or panicking
	// unit test, so a broken test would otherwise reach a GO and only fail
	// in CI (#762). Envtest/integration packages are excluded (they need
	// KUBEBUILDER_ASSETS / a cluster the workspace lacks; CI runs them).
	if pkgs := changedTestPackages(ctx, workspace, run); len(pkgs) > 0 {
		args := append([]string{"test", "-count=1", "-timeout=180s"}, pkgs...)
		if out, err := run(ctx, workspace, nil, "go", args...); err != nil {
			failures = append(failures, checkFailure{name: "go test " + strings.Join(pkgs, " "), output: out})
		}
	}

	if len(failures) == 0 {
		return true, ""
	}

	return false, buildFeedback(failures)
}

// changedTestPackages returns the workspace-relative Go package directories
// (as "./<dir>/" patterns) that have uncommitted changes per
// `git status --porcelain` and are not envtest-backed. It dedups packages
// and ignores non-Go files and root-level (package main) changes. A git
// error yields no packages (the tier is skipped rather than failing the
// gate spuriously).
func changedTestPackages(ctx context.Context, workspace string, run commandRunner) []string {
	out, err := run(ctx, workspace, nil, "git", "status", "--porcelain")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var pkgs []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		// porcelain is "XY <path>" (renames end with "-> <new>"); the
		// final field is the current path.
		path := fields[len(fields)-1]
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		dir := filepath.Dir(path)
		if dir == "." {
			continue // root package main; not part of the unit-test tier
		}
		dirKey := dir + "/"
		excluded := false
		for _, pfx := range envtestPackagePrefixes {
			if strings.HasPrefix(dirKey, pfx) {
				excluded = true
				break
			}
		}
		if excluded || seen[dirKey] {
			continue
		}
		seen[dirKey] = true
		pkgs = append(pkgs, "./"+dirKey)
	}
	return pkgs
}

// buildFeedback renders the directive and a per-check section for every
// failing check, truncating each check's output to maxCheckOutputBytes.
func buildFeedback(failures []checkFailure) string {
	var b strings.Builder
	b.WriteString("The verification gate failed. Fix the issues below and resubmit.\n")
	for _, f := range failures {
		b.WriteString("\n## ")
		b.WriteString(f.name)
		b.WriteString("\n")
		b.WriteString(truncateOutput(f.output))
		b.WriteString("\n")
	}
	return b.String()
}

// truncateOutput caps output at maxCheckOutputBytes, keeping the tail
// (most recent output) and prefixing a marker when truncation occurs.
func truncateOutput(output string) string {
	if len(output) <= maxCheckOutputBytes {
		return output
	}
	return "...(truncated)...\n" + output[len(output)-maxCheckOutputBytes:]
}
