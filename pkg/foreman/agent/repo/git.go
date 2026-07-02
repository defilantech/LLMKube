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

package repo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runGit executes "git args..." with the given workdir and extra env.
// The base env is intentionally minimal (PATH only) to keep external
// state out of git operations; callers append HOME, GIT_*, and the
// Auth env via extraEnv.
//
// Returns trimmed stdout on success, or an error whose message includes
// the args and stderr (capped at 4 KiB) so failures stay debuggable.
func runGit(ctx context.Context, workdir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: args composed from typed callers
	if workdir != "" {
		cmd.Dir = workdir
	}
	// Start from PATH; everything else is opt-in via extraEnv. This
	// avoids leaking ANTHROPIC_API_KEY / AWS creds / etc. into the git
	// subprocess environment.
	cmd.Env = append([]string{"PATH=" + envOr("PATH", "/usr/local/bin:/usr/bin:/bin")}, extraEnv...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errOut := stderr.String()
		if len(errOut) > 4096 {
			errOut = errOut[:4096] + "...[truncated]"
		}
		return "", &gitError{
			args:   strings.Join(args, " "),
			err:    err,
			stderr: strings.TrimSpace(errOut),
		}
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// envOr returns os.Getenv(key) if non-empty, else fallback.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// gitError preserves the structured pieces of a failed git invocation.
// The message keeps the old "git <args>: <err>: <stderr>" shape for
// logs, while callers that need to classify the failure (e.g. push
// rejection sniffing) can inspect stderr alone — matching on the full
// message would also match the ARGS, and branch names embed
// caller-controlled Workload names.
type gitError struct {
	args   string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", e.args, e.err, e.stderr)
}

func (e *gitError) Unwrap() error { return e.err }
