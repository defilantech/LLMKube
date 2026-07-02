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
	"os"
	"path/filepath"
	"testing"
)

// pushCollisionFixture builds the #934 scenario: a bare origin whose
// foreman/<wl>/issue-N branch was pushed by a previous run, and a fresh
// clone (the retry) with a DIFFERENT commit on the same branch name.
// Returns the retry workspace.
func pushCollisionFixture(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	bare := initBareOrigin(t, dir)
	seedOrigin(t, bare)

	// Previous run: clone, branch, commit "old", push.
	oldWork := filepath.Join(dir, "old-run")
	mustGit(t, "", "clone", bare, oldWork)
	mustGit(t, oldWork, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(oldWork, "old.txt"), []byte("old attempt\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, oldWork, "-c", "user.email=a@x", "-c", "user.name=a", "add", ".")
	mustGit(t, oldWork, "-c", "user.email=a@x", "-c", "user.name=a", "commit", "-m", "old attempt")
	mustGit(t, oldWork, "push", "--set-upstream", "origin", branch)

	// Retry run: fresh clone of main, same branch name, different commit.
	retryWork := filepath.Join(dir, "retry-run")
	mustGit(t, "", "clone", bare, retryWork)
	mustGit(t, retryWork, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(retryWork, "new.txt"), []byte("retry attempt\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, retryWork, "-c", "user.email=a@x", "-c", "user.name=a", "add", ".")
	mustGit(t, retryWork, "-c", "user.email=a@x", "-c", "user.name=a", "commit", "-m", "retry attempt")
	return retryWork
}

// TestPush_StaleOwnedBranch_RejectedWithoutReplaceOnReject pins the
// pre-#934 behavior: a retry pushing its own branch name over a previous
// run's ref is a non-fast-forward rejection.
func TestPush_StaleOwnedBranch_RejectedWithoutReplaceOnReject(t *testing.T) {
	branch := "foreman/wl-x-7/issue-7"
	work := pushCollisionFixture(t, branch)

	err := Push(context.Background(), PushOptions{Workspace: work, Branch: branch})
	if !errors.Is(err, ErrPushRejected) {
		t.Fatalf("want ErrPushRejected, got %v", err)
	}
}

// TestPush_StaleOwnedBranch_ReplacedWithReplaceOnReject is the #934 fix:
// with ReplaceOnReject the retry replaces its predecessor's ref via
// force-with-lease and succeeds.
func TestPush_StaleOwnedBranch_ReplacedWithReplaceOnReject(t *testing.T) {
	branch := "foreman/wl-x-7/issue-7"
	work := pushCollisionFixture(t, branch)

	if err := Push(context.Background(), PushOptions{
		Workspace:       work,
		Branch:          branch,
		ReplaceOnReject: true,
	}); err != nil {
		t.Fatalf("Push with ReplaceOnReject: %v", err)
	}

	// The remote ref must now be the retry's commit (log contains
	// "retry attempt", not "old attempt").
	verify := filepath.Join(t.TempDir(), "verify")
	// Workspace origin is the bare; read its ref log via a fresh clone.
	out := mustGitOut(t, work, "ls-remote", "origin", "refs/heads/"+branch)
	local := mustGitOut(t, work, "rev-parse", "HEAD")
	if len(out) < 40 || out[:40] != local[:40] {
		t.Fatalf("remote ref not replaced: remote=%q local=%q (verify dir %s)", out, local, verify)
	}
}

// TestPush_CleanPush_NeverForces proves ReplaceOnReject is inert on a
// clean push (no rejection, no lease dance).
func TestPush_CleanPush_NeverForces(t *testing.T) {
	dir := t.TempDir()
	bare := initBareOrigin(t, dir)
	seedOrigin(t, bare)
	work := filepath.Join(dir, "work")
	mustGit(t, "", "clone", bare, work)
	mustGit(t, work, "checkout", "-b", "foreman/wl-y-1/issue-1")
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, work, "-c", "user.email=a@x", "-c", "user.name=a", "add", ".")
	mustGit(t, work, "-c", "user.email=a@x", "-c", "user.name=a", "commit", "-m", "x")

	if err := Push(context.Background(), PushOptions{
		Workspace:       work,
		Branch:          "foreman/wl-y-1/issue-1",
		ReplaceOnReject: true,
	}); err != nil {
		t.Fatalf("clean push: %v", err)
	}
}

// mustGitOut runs git and returns trimmed stdout, failing the test on error.
func mustGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(context.Background(), dir, baseEnv(), args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}
