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
	"reflect"
	"strings"
	"testing"
)

func TestParseNameOnly(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace-only", "  \n\n  \n", nil},
		{"single-line", "pkg/agent/registry.go", []string{"pkg/agent/registry.go"}},
		{
			"multi-line",
			"pkg/agent/registry.go\npkg/agent/registry_test.go\n",
			[]string{"pkg/agent/registry.go", "pkg/agent/registry_test.go"},
		},
		{
			"trailing-blanks-and-trims",
			"  pkg/a.go  \n\npkg/b.go\n\n\n",
			[]string{"pkg/a.go", "pkg/b.go"},
		},
		{
			"paths-with-spaces-preserved",
			"docs/site/concepts/model router.md\n.goreleaser.yaml",
			[]string{"docs/site/concepts/model router.md", ".goreleaser.yaml"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNameOnly(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestDiffNameOnly_RejectsEmptyArgs(t *testing.T) {
	ctx := context.Background()
	if _, err := DiffNameOnly(ctx, "", "main"); err == nil {
		t.Error("DiffNameOnly: empty workspace should error")
	}
	if _, err := DiffNameOnly(ctx, "/tmp", ""); err == nil {
		t.Error("DiffNameOnly: empty base should error")
	}
}

// TestDiffNameOnly_RoundTrip exercises the full happy path against a
// real bare git workspace: init a repo, commit two files on main,
// branch off, modify both + add a third on the branch, and assert
// DiffNameOnly returns exactly the three changed paths in any order.
// Skipped if `git` is not on PATH.
func TestDiffNameOnly_RoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ws := t.TempDir()
	ctx := context.Background()

	run := func(args ...string) {
		t.Helper()
		if _, err := runGit(ctx, ws, baseEnv(), args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	mustWrite := func(rel, content string) {
		t.Helper()
		full := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdirall %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	// init repo + initial commit on main
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	mustWrite("a.go", "package a\n")
	mustWrite("b.go", "package b\n")
	run("add", ".")
	run("commit", "-m", "initial")

	// branch off, change a.go, add c.go (leave b.go untouched)
	run("checkout", "-b", "feature")
	mustWrite("a.go", "package a\n// edit\n")
	mustWrite("c.go", "package c\n")
	run("add", ".")
	run("commit", "-m", "feature work")

	got, err := DiffNameOnly(ctx, ws, "main")
	if err != nil {
		t.Fatalf("DiffNameOnly: %v", err)
	}
	want := map[string]bool{"a.go": true, "c.go": true}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected path %q in diff (b.go should have been excluded)", p)
		}
	}

	// switching back to main and asking for the same diff returns empty:
	// HEAD == main means there are no commits ahead.
	run("checkout", "main")
	got, err = DiffNameOnly(ctx, ws, "main")
	if err != nil {
		t.Fatalf("DiffNameOnly main vs HEAD: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("HEAD == base should yield empty diff; got %v", got)
	}
}

func TestCommitsAheadOfBase(t *testing.T) {
	tmp := t.TempDir()
	env := []string{
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
	}

	// Create initial repo state (main branch, one commit).
	runGitOrFatal(t, tmp, env, "init", "-b", "main")
	writeFileTemp(t, tmp, "initial.txt", "hello\n")
	runGitOrFatal(t, tmp, env, "add", "-A")
	runGitOrFatal(t, tmp, env, "commit", "-m", "initial")

	// Cut a branch and add one commit ahead of base.
	runGitOrFatal(t, tmp, env, "checkout", "-b", "feature")
	writeFileTemp(t, tmp, "new.txt", "world\n")
	runGitOrFatal(t, tmp, env, "add", "-A")
	runGitOrFatal(t, tmp, env, "commit", "-m", "second")

	// Test: one commit ahead of main.
	count, err := CommitsAheadOfBase(context.Background(), tmp, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 commit ahead, got %d", count)
	}

	// Test: zero commits when base == HEAD.
	count, err = CommitsAheadOfBase(context.Background(), tmp, "feature")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 commits ahead (base==HEAD), got %d", count)
	}

	// Test: workspace required guard.
	_, err = CommitsAheadOfBase(context.Background(), "", "main")
	if err == nil || !strings.Contains(err.Error(), "workspace is required") {
		t.Errorf("expected 'workspace is required' error, got: %v", err)
	}

	// Test: base ref required guard.
	_, err = CommitsAheadOfBase(context.Background(), tmp, "")
	if err == nil || !strings.Contains(err.Error(), "base ref is required") {
		t.Errorf("expected 'base ref is required' error, got: %v", err)
	}
}

func TestSoftResetToBase(t *testing.T) {
	tmp := t.TempDir()
	env := []string{
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
	}

	runGitOrFatal(t, tmp, env, "init", "-b", "main")
	writeFileTemp(t, tmp, "initial.txt", "hello\n")
	runGitOrFatal(t, tmp, env, "add", "-A")
	runGitOrFatal(t, tmp, env, "commit", "-m", "initial")

	// Cut a branch and add one commit ahead.
	runGitOrFatal(t, tmp, env, "checkout", "-b", "feature")
	writeFileTemp(t, tmp, "new.txt", "world\n")
	runGitOrFatal(t, tmp, env, "add", "-A")
	runGitOrFatal(t, tmp, env, "commit", "-m", "second")

	// Verify commits ahead.
	count, _ := CommitsAheadOfBase(context.Background(), tmp, "main")
	if count != 1 {
		t.Fatalf("expected 1 commit ahead before reset, got %d", count)
	}

	// Soft reset: moves HEAD back to main, changes go into working tree.
	err := SoftResetToBase(context.Background(), tmp, "main")
	if err != nil {
		t.Fatalf("SoftResetToBase error: %v", err)
	}

	// After reset: HEAD is at main (0 commits ahead), but HasChanges is true.
	count, _ = CommitsAheadOfBase(context.Background(), tmp, "main")
	if count != 0 {
		t.Errorf("expected 0 commits ahead after reset, got %d", count)
	}

	hasChanges, _ := HasChanges(context.Background(), tmp)
	if !hasChanges {
		t.Fatal("after soft reset, HasChanges should be true (model's edits recovered)")
	}

	// Test: ErrNothingToCommit when base == HEAD.
	err = SoftResetToBase(context.Background(), tmp, "feature")
	if !errors.Is(err, ErrNothingToCommit) {
		t.Errorf("expected ErrNothingToCommit when base==HEAD, got: %v", err)
	}

	// Test: workspace required guard.
	err = SoftResetToBase(context.Background(), "", "main")
	if err == nil || !strings.Contains(err.Error(), "workspace is required") {
		t.Errorf("expected 'workspace is required' error, got: %v", err)
	}

	// Test: base ref required guard.
	err = SoftResetToBase(context.Background(), tmp, "")
	if err == nil || !strings.Contains(err.Error(), "base ref is required") {
		t.Errorf("expected 'base ref is required' error, got: %v", err)
	}
}

func writeFileTemp(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writeFileTemp: %v", err)
	}
}

func runGitOrFatal(t *testing.T, workspace string, env []string, args ...string) {
	t.Helper()
	out, err := runGit(context.Background(), workspace, env, args...)
	if err != nil {
		t.Fatalf("runGit %v: %v (output: %s)", strings.Join(args, " "), err, out)
	}
}
