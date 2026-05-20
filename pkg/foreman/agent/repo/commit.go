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
