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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitOrSkip skips the test if git is not on PATH.
func gitOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
}

// initBareOrigin creates a bare repo we can clone from + push to.
// Returns the bare repo path.
func initBareOrigin(t *testing.T, dir string) string {
	t.Helper()
	bare := filepath.Join(dir, "origin.git")
	cmd := exec.Command("git", "init", "--bare", bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	return bare
}

// seedOrigin makes the bare repo non-empty by cloning it, adding an
// initial commit on main, and pushing it back. Required because git
// clone of a truly-empty bare repo prints a "warning: You appear to
// have cloned an empty repository" line and leaves HEAD unborn.
func seedOrigin(t *testing.T, bare string) {
	t.Helper()
	tmp := t.TempDir()
	work := filepath.Join(tmp, "seed")
	mustGit(t, "", "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "README.md"),
		[]byte("# seed\n"), 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	mustGit(t, work, "-c", "user.email=seed@x", "-c", "user.name=seed",
		"add", "README.md")
	mustGit(t, work, "-c", "user.email=seed@x", "-c", "user.name=seed",
		"commit", "-m", "seed")
	// Ensure the branch name is "main" regardless of host's init.defaultBranch.
	out, _ := exec.Command("git", "-C", work, "branch", "--show-current").Output()
	cur := strings.TrimSpace(string(out))
	if cur != "main" {
		mustGit(t, work, "branch", "-M", cur, "main")
	}
	mustGit(t, work, "push", "origin", "main")
}

// mustGit runs "git args..." in dir and fails the test on error.
// (Always git; tests that needed a different binary would deserve their
// own helper.)
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// --- branch / slug --------------------------------------------------------

func TestBranchNameForIssue_Slugify(t *testing.T) {
	cases := []struct {
		issue int
		title string
		want  string
	}{
		{1, "Fix the typo in README", "foreman/issue-1-fix-the-typo-in-readme"},
		{42, "Foreman M3: native loop", "foreman/issue-42-foreman-m3-native-loop"},
		{7, "  trim  -  whitespace - and dashes  ", "foreman/issue-7-trim-whitespace-and-dashes"},
		{99, "", "foreman/issue-99"},
		{5, "a_very_long_title_that_definitely_will_exceed_thirty_two_chars_for_sure",
			"foreman/issue-5-a-very-long-title-that-definitel"}, // truncated at 32 chars
	}
	for _, c := range cases {
		t.Run(c.title, func(t *testing.T) {
			got := BranchNameForIssue(c.issue, c.title)
			if got != c.want {
				t.Errorf("BranchNameForIssue(%d,%q):\n  got  %q\n  want %q", c.issue, c.title, got, c.want)
			}
		})
	}
}

func TestSlugify_NoTrailingDashOnTruncate(t *testing.T) {
	in := "a-bc-de-fg-hi-jk-lm-no-pq-rs-tu-vw"
	out := slugify(in, 10)
	if strings.HasSuffix(out, "-") {
		t.Errorf("slug has trailing dash: %q", out)
	}
}

// --- auth -----------------------------------------------------------------

func TestNewAuth_ExplicitTokenAndCloseRemovesFile(t *testing.T) {
	a, err := NewAuth("ghp_test_token")
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	if a.Token != "ghp_test_token" {
		t.Errorf("token: got %q", a.Token)
	}
	if _, err := os.Stat(a.askpassPath); err != nil {
		t.Errorf("askpass file not present: %v", err)
	}
	env := a.Env()
	var sawAskpass, sawTokenEnv, sawNoPrompt bool
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "GIT_ASKPASS="):
			sawAskpass = true
		case strings.HasPrefix(kv, "FOREMAN_GIT_TOKEN="):
			sawTokenEnv = true
		case strings.HasPrefix(kv, "GIT_TERMINAL_PROMPT="):
			sawNoPrompt = true
		}
	}
	if !sawAskpass || !sawTokenEnv || !sawNoPrompt {
		t.Errorf("Env() missing entries: askpass=%v token=%v noprompt=%v", sawAskpass, sawTokenEnv, sawNoPrompt)
	}
	if err := a.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := os.Stat(a.askpassPath); !os.IsNotExist(err) {
		// askpassPath was zeroed inside Close, so Stat("") errors with
		// "no such file or directory" too. Either way, expect NotExist.
		t.Logf("post-close stat: %v (expected NotExist)", err)
	}
	// Double-close must be a no-op.
	if err := a.Close(); err != nil {
		t.Errorf("double-Close: %v", err)
	}
}

func TestTokenFromEnvOrFile_EnvWins(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "from-env")
	got, err := TokenFromEnvOrFile()
	if err != nil {
		t.Fatalf("TokenFromEnvOrFile: %v", err)
	}
	if got != "from-env" {
		t.Errorf("got %q want %q", got, "from-env")
	}
}

func TestTokenFromEnvOrFile_MissingReturnsErrNoToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	// Point HOME at an empty dir so the fallback path resolves to a
	// nonexistent ~/.config/foreman/github-token.
	t.Setenv("HOME", t.TempDir())
	_, err := TokenFromEnvOrFile()
	if !errors.Is(err, ErrNoToken) {
		t.Errorf("expected ErrNoToken, got %v", err)
	}
}

// --- clone / branch / commit / push round-trip ----------------------------

// repoIdent returns a deterministic Identity for tests.
func repoIdent() Identity {
	return Identity{Name: "Foreman Bot", Email: "bot@foreman.test"}
}

func TestRepoRoundTrip_CloneBranchCommitPush(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareOrigin(t, root)
	seedOrigin(t, bare)

	ctx := context.Background()
	dest := filepath.Join(root, "work")

	if err := Clone(ctx, CloneOptions{
		RemoteURL: bare,
		Dest:      dest,
	}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("seed missing in clone: %v", err)
	}

	branch := BranchNameForIssue(1, "tiny demo")
	if err := CreateAndCheckoutBranch(ctx, dest, branch); err != nil {
		t.Fatalf("CreateAndCheckoutBranch: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dest, "added.txt"),
		[]byte("foreman was here\n"), 0o644); err != nil {
		t.Fatalf("write change: %v", err)
	}

	sha, err := Commit(ctx, CommitOptions{
		Workspace: dest,
		Message:   "fix: add greeting\n\nFixes #1",
		Author:    repoIdent(),
		Committer: repoIdent(),
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(sha) < 7 {
		t.Errorf("sha looks wrong: %q", sha)
	}

	// Local push (no token needed for file:// bare repo): pass nil Auth.
	if err := Push(ctx, PushOptions{
		Workspace: dest,
		Remote:    "origin",
		Branch:    branch,
	}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Verify the branch exists in the bare repo.
	cmd := exec.Command("git", "-C", bare, "branch", "--list", branch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("post-push branch --list: %v: %s", err, out)
	}
	if !strings.Contains(string(out), branch) {
		t.Errorf("branch %q not found in bare repo; got: %s", branch, out)
	}

	// Verify the commit carries the DCO sign-off trailer.
	cmd = exec.Command("git", "-C", bare, "log", "-1", "--format=%B", "refs/heads/"+branch)
	logOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("post-push log: %v: %s", err, logOut)
	}
	want := "Signed-off-by: Foreman Bot <bot@foreman.test>"
	if !strings.Contains(string(logOut), want) {
		t.Errorf("missing DCO trailer; commit message was:\n%s", logOut)
	}
}

func TestCommit_NothingToCommit(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareOrigin(t, root)
	seedOrigin(t, bare)
	dest := filepath.Join(root, "work")
	if err := Clone(context.Background(), CloneOptions{RemoteURL: bare, Dest: dest}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	_, err := Commit(context.Background(), CommitOptions{
		Workspace: dest,
		Message:   "no changes",
		Author:    repoIdent(),
		Committer: repoIdent(),
	})
	if !errors.Is(err, ErrNothingToCommit) {
		t.Errorf("expected ErrNothingToCommit, got %v", err)
	}
}

func TestClone_RejectsNonemptyDest(t *testing.T) {
	gitOrSkip(t)
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "anything"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := Clone(context.Background(), CloneOptions{
		RemoteURL: "https://example.com/x.git",
		Dest:      dest,
	})
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("expected non-empty dest error, got %v", err)
	}
}

func TestSetRemote_IdempotentAndAdds(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareOrigin(t, root)
	seedOrigin(t, bare)
	dest := filepath.Join(root, "work")
	if err := Clone(context.Background(), CloneOptions{RemoteURL: bare, Dest: dest}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := SetRemote(context.Background(), dest, "fork", "https://example.com/x.git"); err != nil {
		t.Fatalf("SetRemote add: %v", err)
	}
	// Calling again must update, not duplicate or error.
	if err := SetRemote(context.Background(), dest, "fork", "https://example.com/y.git"); err != nil {
		t.Fatalf("SetRemote update: %v", err)
	}
	cmd := exec.Command("git", "-C", dest, "remote", "get-url", "fork")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("remote get-url: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "y.git") {
		t.Errorf("remote url not updated; got: %s", out)
	}
}
