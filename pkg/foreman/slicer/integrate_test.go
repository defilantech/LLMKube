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

package slicer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs a git subcommand in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// readAt reads dir/path, failing the test on error.
func readAt(t *testing.T, dir, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeAt writes content to dir/path, creating parent dirs.
func writeAt(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// initRepo makes a temp git repo on branch main with one base commit.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "Test")
	git(t, dir, "config", "commit.gpgsign", "false")
	writeAt(t, dir, "base.txt", "base\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "base")
	return dir
}

// sliceBranch creates branch name off main, writes files, commits, returns to main.
func sliceBranch(t *testing.T, dir, name string, files map[string]string) {
	t.Helper()
	git(t, dir, "checkout", "-q", "-B", name, "main")
	for p, c := range files {
		writeAt(t, dir, p, c)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", name)
	git(t, dir, "checkout", "-q", "main")
}

func TestIntegrate_CleanDisjointUnion(t *testing.T) {
	dir := initRepo(t)
	// two slices touching distinct files, plus one modifying the base file.
	sliceBranch(t, dir, "slice-a", map[string]string{"a/new.txt": "AAA\n"})
	sliceBranch(t, dir, "slice-b", map[string]string{"base.txt": "base\nfrom-b\n"})

	res, err := Integrate(context.Background(), IntegrateOptions{
		RepoDir: dir, Base: "main", Branch: "integ", Slices: []string{"slice-a", "slice-b"},
	})
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if res.Branch != "integ" {
		t.Fatalf("branch = %q, want integ", res.Branch)
	}
	if res.Owners["a/new.txt"] != "slice-a" || res.Owners["base.txt"] != "slice-b" {
		t.Fatalf("owners = %v", res.Owners)
	}
	// the integration branch holds BOTH slices' contributions.
	git(t, dir, "checkout", "-q", "integ")
	if got := readAt(t, dir, "a/new.txt"); got != "AAA\n" {
		t.Fatalf("a/new.txt = %q", got)
	}
	if got := readAt(t, dir, "base.txt"); !strings.Contains(got, "from-b") {
		t.Fatalf("base.txt = %q, want the slice-b change", got)
	}
	if !strings.Contains(res.UnionDiff, "a/new.txt") || !strings.Contains(res.UnionDiff, "from-b") {
		t.Fatalf("union diff missing a slice's change:\n%s", res.UnionDiff)
	}
}

func TestIntegrate_OverlapRejected(t *testing.T) {
	dir := initRepo(t)
	// both slices change the SAME file -> not disjoint.
	sliceBranch(t, dir, "slice-a", map[string]string{"shared.txt": "from-a\n"})
	sliceBranch(t, dir, "slice-b", map[string]string{"shared.txt": "from-b\n"})

	_, err := Integrate(context.Background(), IntegrateOptions{
		RepoDir: dir, Base: "main", Branch: "integ", Slices: []string{"slice-a", "slice-b"},
	})
	var overlap *OverlapError
	if !errors.As(err, &overlap) {
		t.Fatalf("want *OverlapError, got %v", err)
	}
	if overlap.File != "shared.txt" {
		t.Fatalf("overlap file = %q, want shared.txt", overlap.File)
	}
	// the union branch must not have been left behind on a rejected integration.
	if out := git(t, dir, "branch", "--list", "integ"); strings.TrimSpace(out) != "" {
		t.Fatalf("integ branch should not exist after overlap, got %q", out)
	}
}

func TestIntegrate_EmptyUnion(t *testing.T) {
	dir := initRepo(t)
	// pointing a "slice" at main itself yields no changes.
	_, err := Integrate(context.Background(), IntegrateOptions{
		RepoDir: dir, Base: "main", Branch: "integ", Slices: []string{"main"},
	})
	var empty *EmptyUnionError
	if !errors.As(err, &empty) {
		t.Fatalf("want *EmptyUnionError, got %v", err)
	}
}

func TestIntegrate_MissingOptions(t *testing.T) {
	_, err := Integrate(context.Background(), IntegrateOptions{Base: "main", Branch: "integ", Slices: []string{"s"}})
	if err == nil {
		t.Fatal("want error for missing RepoDir")
	}
}

// TestIntegrate_NoAmbientGitIdentity reproduces the in-cluster foreman-agent
// pod, where git has no user.name/user.email in any config scope. The slice
// branches were committed by the coder (with an identity), but the pod that
// runs Integrate has none, so the union commit must supply its own or it dies
// with "Author identity unknown" (exit 128). The package's own fixture masks
// this because initRepo sets a LOCAL identity; here we neutralize global and
// system config and strip the local identity before the union commit.
func TestIntegrate_NoAmbientGitIdentity(t *testing.T) {
	// Point git's global/system config at nonexistent files so no ambient
	// identity from the dev's ~/.gitconfig can leak in and hide the bug.
	// t.Setenv makes these process-wide for the test and restores after.
	empty := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(empty, "no-global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(empty, "no-system"))

	dir := initRepo(t)
	sliceBranch(t, dir, "slice-a", map[string]string{"a/new.txt": "AAA\n"})
	// Drop the local identity: now the repo has NO identity from any scope,
	// exactly like the integrate pod's fresh clone.
	git(t, dir, "config", "--unset", "user.email")
	git(t, dir, "config", "--unset", "user.name")

	res, err := Integrate(context.Background(), IntegrateOptions{
		RepoDir: dir, Base: "main", Branch: "integ", Slices: []string{"slice-a"},
	})
	if err != nil {
		t.Fatalf("integrate without ambient git identity: %v", err)
	}
	// The union commit landed, authored by the foreman bot identity.
	got := strings.TrimSpace(git(t, dir, "log", "-1", "--format=%an <%ae>", "integ"))
	if !strings.Contains(got, "foreman@llmkube.dev") {
		t.Fatalf("union commit author = %q, want the foreman bot identity", got)
	}
	if res.Branch != "integ" {
		t.Fatalf("branch = %q, want integ", res.Branch)
	}
}
