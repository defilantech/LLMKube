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

	// ReplaceOnReject retries a rejected push with --force-with-lease
	// pinned to the remote SHA observed via ls-remote — a compare-and-
	// swap that replaces exactly the ref we saw, and still fails if a
	// concurrent writer moves it in between. Callers set this ONLY for
	// branches the agent owns outright (the foreman/<workload>/...
	// namespace), where the stale ref is a previous run of the same
	// Workload: without it, every re-run after a delete-and-recreate
	// retry dies NO-GO / PUSH-FAILED on non-fast-forward (#934).
	ReplaceOnReject bool
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
	if !isPushRejection(err.Error()) {
		return err
	}
	if opts.ReplaceOnReject {
		if replaceErr := replaceRemoteBranch(ctx, opts.Workspace, env, remote, opts.Branch); replaceErr == nil {
			return nil
		} else if isPushRejection(replaceErr.Error()) {
			// The lease failed: someone moved the ref between our
			// ls-remote and the push. Surface the original rejection
			// semantics so the caller's PUSH-FAILED handling applies.
			return fmt.Errorf("%w: force-with-lease retry also rejected: %w", ErrPushRejected, replaceErr)
		} else {
			return replaceErr
		}
	}
	return fmt.Errorf("%w: %w", ErrPushRejected, err)
}

// replaceRemoteBranch re-pushes Branch with --force-with-lease pinned to
// the remote SHA read via ls-remote: replace exactly the ref we observed,
// fail if it moves concurrently. Used only for agent-owned branch
// namespaces (see PushOptions.ReplaceOnReject).
func replaceRemoteBranch(ctx context.Context, workspace string, env []string, remote, branch string) error {
	ref := "refs/heads/" + branch
	out, err := runGit(ctx, workspace, env, "ls-remote", remote, ref)
	if err != nil {
		return fmt.Errorf("ls-remote before force-with-lease: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		// The branch vanished between the rejected push and now (e.g. a
		// human deleted it). A plain push should succeed; keep the lease
		// against an empty expectation to stay race-safe.
		fields = []string{""}
	}
	lease := fmt.Sprintf("--force-with-lease=%s:%s", ref, fields[0])
	_, err = runGit(ctx, workspace, env, "push", lease, "--set-upstream", remote, branch)
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
