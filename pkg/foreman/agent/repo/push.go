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
	"context"
	"errors"
	"fmt"
	"strings"
)

// PushOptions configures Push.
type PushOptions struct {
	// Workspace is the cloned repo root.
	Workspace string

	// Remote is the git remote to push to. If empty, defaults to
	// "origin". The remote must already exist; SetRemote can add it
	// if the clone came from upstream but the push targets a fork.
	Remote string

	// Branch is the local branch to push.
	Branch string

	// Auth provides the GIT_ASKPASS scaffolding. Required for
	// authenticated pushes (which is every realistic case).
	Auth *Auth
}

// ErrPushRejected wraps a non-fast-forward / pre-receive rejection
// from the remote. Phase E maps this to outcome=PUSH-FAILED in the
// Result.Extra envelope so M4's gate scheduler can distinguish it
// from auth or network failures.
var ErrPushRejected = errors.New("repo: push rejected by remote")

// Push pushes Branch to Remote (with --set-upstream so future pushes
// from the same workspace work without args). Returns nil on a clean
// push, ErrPushRejected on a remote rejection, or a wrapped error for
// anything else (auth failure, network, etc.).
func Push(ctx context.Context, opts PushOptions) error {
	if opts.Workspace == "" {
		return fmt.Errorf("Push: Workspace is required")
	}
	if opts.Branch == "" {
		return fmt.Errorf("Push: Branch is required")
	}
	remote := opts.Remote
	if remote == "" {
		remote = "origin"
	}

	env := baseEnv()
	if opts.Auth != nil {
		env = append(env, opts.Auth.Env()...)
	}

	_, err := runGit(ctx, opts.Workspace, env,
		"push", "--set-upstream", remote, opts.Branch)
	if err == nil {
		return nil
	}
	// Heuristic: git push prints "[remote rejected]" or "[rejected]"
	// in stderr on a non-fast-forward / pre-receive rejection. We
	// captured stderr inside the wrapped error message in runGit.
	if isPushRejection(err.Error()) {
		return fmt.Errorf("%w: %w", ErrPushRejected, err)
	}
	return err
}

// SetRemote adds (or updates) a git remote. Used by callers that clone
// from upstream and push to a fork: clone with one remote name, then
// AddRemote("fork", forkURL), then Push(Remote="fork").
func SetRemote(ctx context.Context, workspace, name, url string) error {
	if workspace == "" || name == "" || url == "" {
		return fmt.Errorf("SetRemote: workspace, name, and url are required")
	}
	// Idempotency: try set-url first; if the remote does not exist,
	// add it. This avoids races where the same workspace is reused.
	if _, err := runGit(ctx, workspace, baseEnv(), "remote", "set-url", name, url); err == nil {
		return nil
	}
	_, err := runGit(ctx, workspace, baseEnv(), "remote", "add", name, url)
	return err
}

// isPushRejection sniffs git's stderr for the canonical rejection
// markers. Brittle in principle (string-matching on git output) but
// stable across modern git versions for this specific case.
func isPushRejection(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "[rejected]") ||
		strings.Contains(lower, "remote rejected") ||
		strings.Contains(lower, "non-fast-forward")
}
