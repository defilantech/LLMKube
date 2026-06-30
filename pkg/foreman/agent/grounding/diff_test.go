package grounding

import (
	"context"
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
