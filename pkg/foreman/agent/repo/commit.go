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
	"strconv"
	"strings"
)

// Identity is the git author/committer name+email pair. Both fields
// must be non-empty for the DCO sign-off line (`Signed-off-by: Name
// <email>`) to be valid.
type Identity struct {
	Name  string
	Email string
}

// CommitOptions configures Commit.
type CommitOptions struct {
	// Workspace is the cloned repo root.
	Workspace string

	// Message is the full, model-authored commit message: subject,
	// optional body, and any "Fixes #N" trailer. The helper never
	// modifies it; the DCO sign-off is added by `git commit -s` based
	// on the Committer identity.
	Message string

	// Author and Committer drive the sign-off. v0.1 sets both to the
	// same Identity (the foreman bot user); v0.2 may distinguish.
	Author    Identity
	Committer Identity
}

// ErrNothingToCommit is returned when the workspace has no staged or
// unstaged changes. Phase E maps this to a verdict of NO-GO with
// outcome=NO-CHANGES so the loop's transcript is preserved but no
// empty branch lands on the fork.
var ErrNothingToCommit = errors.New("repo: nothing to commit")

// HasChanges returns true when `git status --porcelain` reports any
// untracked, modified, or staged files. Callers use this to short-
// circuit the commit path on a NO-CHANGES outcome before paying the
// cost of stage + validate + commit.
func HasChanges(ctx context.Context, workspace string) (bool, error) {
	if workspace == "" {
		return false, fmt.Errorf("HasChanges: workspace is required")
	}
	out, err := runGit(ctx, workspace, baseEnv(), "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// CommitsAheadOfBase returns the number of commits reachable from HEAD
// but not from base. Uses `git rev-list --count base..HEAD`. Returns 0
// with no error when base == HEAD.
func CommitsAheadOfBase(ctx context.Context, workspace string, base string) (int, error) {
	if workspace == "" {
		return 0, fmt.Errorf("CommitsAheadOfBase: workspace is required")
	}
	if base == "" {
		return 0, fmt.Errorf("CommitsAheadOfBase: base ref is required")
	}
	out, err := runGit(ctx, workspace, baseEnv(), "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("CommitsAheadOfBase: invalid rev-list output %q: %w", out, err)
	}
	return count, nil
}

// BaseBranchSHA returns the resolved commit SHA that the task branch
// should be considered "cut from". It re-fetches BaseBranch from
// UpstreamURL into FETCH_HEAD (mirroring CreateBranchFromUpstream) and
// returns the SHA git resolved there. This is the base to use for
// commit-ahead counting and soft-reset recovery — NOT the local
// "<baseBranch>" ref, which lags the upstream tip on a stale fork
// (the original #813 failure mode that AddBranchFromUpstream was
// introduced to fix). Callers who cloned from a remote with no
// upstream fork (a self-hosted-only repo) can pass an empty UpstreamURL
// to resolve against the local ref; otherwise an empty UpstreamURL
// returns an error so the caller cannot silently fall back.
func BaseBranchSHA(ctx context.Context, workspace, upstreamURL, baseBranch string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("BaseBranchSHA: workspace is required")
	}
	if baseBranch == "" {
		return "", fmt.Errorf("BaseBranchSHA: baseBranch is required")
	}
	if upstreamURL == "" {
		// Refuse to fall back to a possibly-stale local ref — see #813.
		return "", fmt.Errorf("BaseBranchSHA: upstreamURL is required (refusing to resolve against a local ref)")
	}
	if !gitRefSafe(baseBranch) {
		return "", fmt.Errorf("BaseBranchSHA: invalid base branch %q", baseBranch)
	}
	if _, err := runGit(ctx, workspace, baseEnv(), "fetch", upstreamURL, baseBranch); err != nil {
		return "", fmt.Errorf("BaseBranchSHA: fetch %s %s: %w", upstreamURL, baseBranch, err)
	}
	out, err := runGit(ctx, workspace, baseEnv(), "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", fmt.Errorf("BaseBranchSHA: rev-parse FETCH_HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// SoftResetToBase moves commits from HEAD back into the working tree
// without touching the index, relative to the branch point. Used when a
// model self-committed its work and the executor wants to re-apply it with
// DCO sign-off and executor-owned author identity. base should be a commit
// SHA (resolved via BaseBranchSHA), not a ref name, so a stale local branch
// cannot drag upstream commits into the recovered commit.
//
// base is the CURRENT upstream tip, which may be AHEAD of the point the task
// branch was actually cut from when upstream advanced mid-run. The reset
// anchors at the TRUE branch point — merge-base(base, HEAD) — not at base, so
// only the model's own commits are re-staged; the intervening upstream delta
// between the branch point and the current tip is never squashed into the
// recovered commit (#1002).
func SoftResetToBase(ctx context.Context, workspace string, base string) error {
	if workspace == "" {
		return fmt.Errorf("SoftResetToBase: workspace is required")
	}
	if base == "" {
		return fmt.Errorf("SoftResetToBase: base is required")
	}
	out, err := runGit(ctx, workspace, baseEnv(), "merge-base", base, "HEAD")
	if err != nil {
		return fmt.Errorf("SoftResetToBase: merge-base %s HEAD: %w", base, err)
	}
	branchPoint := strings.TrimSpace(out)
	if branchPoint == "" {
		return fmt.Errorf("SoftResetToBase: empty merge-base for %s..HEAD", base)
	}
	count, err := CommitsAheadOfBase(ctx, workspace, branchPoint)
	if err != nil {
		return fmt.Errorf("SoftResetToBase: %w", err)
	}
	if count == 0 {
		return ErrNothingToCommit
	}
	if _, err := runGit(ctx, workspace, baseEnv(), "reset", "--soft", branchPoint); err != nil {
		return fmt.Errorf("SoftResetToBase: reset: %w", err)
	}
	return nil
}

// Commit stages every change in the workspace (git add -A) and creates
// a single DCO-signed commit. Returns the new commit SHA.
//
// Sign-off uses git's built-in -s, which appends "Signed-off-by: <name>
// <email>" where the identity is the Committer. The Author identity is
// also set explicitly so the commit's author block reflects the bot
// (rather than whoever ran the foreman-agent process).
func Commit(ctx context.Context, opts CommitOptions) (string, error) {
	if opts.Workspace == "" {
		return "", fmt.Errorf("Commit: Workspace is required")
	}
	if strings.TrimSpace(opts.Message) == "" {
		return "", fmt.Errorf("Commit: Message is required")
	}
	if opts.Author.Name == "" || opts.Author.Email == "" ||
		opts.Committer.Name == "" || opts.Committer.Email == "" {
		return "", fmt.Errorf("Commit: Author and Committer identities are required")
	}

	env := append(baseEnv(),
		"GIT_AUTHOR_NAME="+opts.Author.Name,
		"GIT_AUTHOR_EMAIL="+opts.Author.Email,
		"GIT_COMMITTER_NAME="+opts.Committer.Name,
		"GIT_COMMITTER_EMAIL="+opts.Committer.Email,
	)

	if _, err := runGit(ctx, opts.Workspace, env, "add", "-A"); err != nil {
		return "", fmt.Errorf("Commit: stage: %w", err)
	}
	// Detect "nothing to commit" before invoking git commit so we can
	// return a typed error rather than parsing git's exit code (which
	// is 1 for both this case and real failures).
	statusOut, err := runGit(ctx, opts.Workspace, env, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("Commit: status: %w", err)
	}
	if strings.TrimSpace(statusOut) == "" {
		return "", ErrNothingToCommit
	}

	if _, err := runGit(ctx, opts.Workspace, env, "commit", "-s", "-m", opts.Message); err != nil {
		return "", fmt.Errorf("Commit: commit: %w", err)
	}
	sha, err := runGit(ctx, opts.Workspace, env, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("Commit: rev-parse: %w", err)
	}
	return strings.TrimSpace(sha), nil
}
