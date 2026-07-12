package grounding

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAddedLines_UsesWorkingTreeAndSurfacesUntracked(t *testing.T) {
	var sawStage bool
	var diffArgs []string
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "git" && len(args) > 0 && args[0] == "add" {
			sawStage = true // expect: add -A -- <pathspec> (stage so new files show)
			return "", nil
		}
		if name == "git" && len(args) > 0 && args[0] == "diff" {
			diffArgs = args
			// Expect a staged working-tree diff vs HEAD: `diff --cached
			// --unified=0 HEAD -- ...`. A `<base>...HEAD` committed diff would
			// miss the coder's uncommitted, still-staged changes.
			if contains(args, "--cached") && contains(args, "HEAD") && !contains(args, "HEAD...HEAD") {
				return "+++ b/docs/new.md\n@@ -0,0 +1,1 @@\n+apiVersion: llmkube.io/v1alpha1\n", nil
			}
			return "", nil
		}
		return "", nil
	}
	added, err := AddedLines(context.Background(), "/ws", run, "HEAD", []string{"*.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !sawStage {
		t.Error("AddedLines must stage (`git add -A`) so untracked new files appear in the pre-commit diff")
	}
	if !contains(diffArgs, "--cached") {
		t.Errorf("AddedLines must use a staged diff (`git diff --cached <base>`); got %v", diffArgs)
	}
	if len(added) != 1 || added[0].Text != "apiVersion: llmkube.io/v1alpha1" {
		t.Errorf("expected the working-tree added line, got %v", added)
	}
}

// TestAddedLines_PerPathspecStagingSurvivesUnmatchedPathspec is the
// regression pin for #1075 round-2 finding B: `git add -A -- <p1> <p2>
// <p3>` is ATOMIC across pathspecs, so a repo with only a root README.md
// (no docs/, no examples/, an entirely normal shape) made the combined
// call exit 128 and stage NOTHING, silently defeating the check. This fake
// runner mirrors real git's per-call behavior exactly as observed live:
// `git add -A -- docs` and `git add -A -- examples` fail (no such path),
// while `git add -A -- *.md` succeeds and marks README.md staged; the
// "diff" response reflects that staged state, exactly as `git diff
// --cached` would. If AddedLines regressed to the single combined-pathspec
// call, every "add" invocation would carry all three pathspecs at once
// (never matching the len(args)==4 case below), so every call would hit
// the error branch, nothing would ever be marked staged, and the diff
// response (and so `added`) would come back empty.
func TestAddedLines_PerPathspecStagingSurvivesUnmatchedPathspec(t *testing.T) {
	staged := false
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" || len(args) == 0 {
			return "", nil
		}
		switch {
		case args[0] == "add" && len(args) == 4 && args[3] == "*.md":
			staged = true
			return "", nil
		case args[0] == "add":
			// A pathspec-per-call design must never send this shape; a
			// combined-pathspec call (or any other single pathspec) lands
			// here and fails, exactly as `git add -A -- docs` and
			// `git add -A -- examples` do against a repo with neither
			// directory.
			return "", errors.New("fatal: pathspec did not match any files")
		case args[0] == "diff":
			if !staged {
				return "", nil // nothing staged: real `git diff --cached` is empty
			}
			return "diff --git a/README.md b/README.md\n" +
				"--- a/README.md\n+++ b/README.md\n" +
				"@@ -1,0 +2 @@\n+| Mixtral 8x7B | ~45 tok/s |\n", nil
		default:
			return "", nil
		}
	}
	added, err := AddedLines(context.Background(), "/ws", run, "HEAD", []string{"*.md", "docs", "examples"})
	if err != nil {
		t.Fatalf("AddedLines: %v", err)
	}
	if len(added) != 1 || !strings.Contains(added[0].Text, "45 tok/s") {
		t.Fatalf("expected the root README.md benchmark line to be detected despite missing docs/ "+
			"and examples/ directories, got: %+v", added)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestAddedLines_AttributesLineNumbers(t *testing.T) {
	diff := "diff --git a/docs/x.md b/docs/x.md\n" +
		"--- a/docs/x.md\n+++ b/docs/x.md\n" +
		"@@ -0,0 +1,2 @@\n+first line\n+second line\n" +
		"@@ -10,0 +20,1 @@\n+twentieth\n"
	run := func(_ context.Context, _ string, _ []string, _ string, _ ...string) (string, error) {
		return diff, nil
	}
	got, err := AddedLines(context.Background(), "/ws", run, "main", []string{"*.md"})
	if err != nil {
		t.Fatal(err)
	}
	want := []AddedLine{
		{File: "docs/x.md", Line: 1, Text: "first line"},
		{File: "docs/x.md", Line: 2, Text: "second line"},
		{File: "docs/x.md", Line: 20, Text: "twentieth"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}
