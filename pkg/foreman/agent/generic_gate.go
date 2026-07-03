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
	"os/exec"
	"strings"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// usesGenericGate reports whether a task's GateProfile should run the
// language-agnostic generic gate (RunGenericGate) instead of the
// specialized Go gate (RunCoderGate). A nil profile, an empty language, or
// an explicit "go" language all use the Go path, so existing Go tasks are
// unaffected and the Go gate stays byte-identical.
func usesGenericGate(profile *foremanv1alpha1.GateProfile) bool {
	return profile != nil &&
		profile.Language != "" &&
		profile.Language != foremanv1alpha1.GateLanguageGo
}

// genericGateDeferredCheck names the advisory emitted when a self-gate
// command is deferred to the clean-room verify Job because its runtime is
// missing from the coder image.
const genericGateDeferredCheck = "self-gate-deferred"

// missingRuntimeExitCode is the POSIX shell exit status for "command not
// found": the interpreter the gate command needs is absent from the image,
// not that the gate ran and genuinely failed.
const missingRuntimeExitCode = 127

// exitCoder matches errors that expose a process exit code (notably
// *exec.ExitError). Declared as an interface so table-driven tests can
// supply a fake without shelling out.
type exitCoder interface{ ExitCode() int }

// isMissingRuntime reports whether a gate command failed because the
// runtime/binary it needs is absent from the pod image (exit 127 from the
// shell, an exec ENOENT, or the shell's "command not found" / ": not found"
// message) rather than because the gate ran and genuinely failed. err is the
// commandRunner error and output the command's combined stdout+stderr. The
// output match is deliberately narrow — the exact shell phrasings — so a
// test failure whose output merely mentions "not found" is not misread as a
// missing runtime.
func isMissingRuntime(err error, output string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	var ec exitCoder
	if errors.As(err, &ec) && ec.ExitCode() == missingRuntimeExitCode {
		return true
	}
	combined := strings.ToLower(output + "\n" + err.Error())
	return strings.Contains(combined, "command not found") ||
		strings.Contains(combined, ": not found")
}

// RunGenericGate runs a language-agnostic fast gate by executing the
// resolved profile's commands in the workspace via `sh -c`. Each non-empty
// command (format, lint, build, test, codegen) is one check, and a non-zero
// exit is a failure. Like RunCoderGate, every check runs regardless of
// earlier failures so the feedback reports everything wrong at once.
//
// A command that cannot run because its runtime/binary is missing from the
// coder image (see isMissingRuntime) does NOT fail the gate: the published
// coder image is Go-only, so a node/python gate command would otherwise hit
// "command not found" and spuriously downgrade a correct GO (#929). Such a
// check is skipped with a deferral advisory — the clean-room verify Job,
// which runs in gateProfile.image, remains the authoritative gate for it.
// A command that ran and genuinely failed still fails as before.
//
// Non-Go GateProfiles use this path. The Go profile keeps the specialized
// RunCoderGate, which carries the Go-specific semantics (gofmt's
// output-not-error signal, golangci-lint's env and binary path, the
// changed-package test tier, and the controller-gen codegen-drift check)
// that do not generalize to other toolchains.
func RunGenericGate(
	ctx context.Context,
	workspace string,
	gate foremanv1alpha1.ResolvedGate,
	run commandRunner,
) (pass bool, feedback string, advisories []advisory) {
	checks := []struct {
		name string
		cmd  string
	}{
		{"format", gate.Format},
		{"lint", gate.Lint},
		{"build", gate.Build},
		{"test", gate.Test},
		{"codegen", gate.CodegenCheck},
	}

	var failures []checkFailure
	for _, c := range checks {
		cmd := strings.TrimSpace(c.cmd)
		if cmd == "" {
			continue
		}
		out, err := run(ctx, workspace, nil, "sh", "-c", cmd)
		if err == nil {
			continue
		}
		if isMissingRuntime(err, out) {
			advisories = append(advisories, advisory{
				Check: genericGateDeferredCheck,
				Detail: fmt.Sprintf(
					"self-gate deferred: runtime missing in the coder image for %s command %q; "+
						"the clean-room verify Job (gateProfile.image) is the authoritative gate for this check",
					c.name, cmd),
			})
			continue
		}
		failures = append(failures, checkFailure{name: c.name + ": " + cmd, output: out})
	}

	if len(failures) == 0 {
		return true, "", advisories
	}
	return false, buildFeedback(failures), advisories
}
