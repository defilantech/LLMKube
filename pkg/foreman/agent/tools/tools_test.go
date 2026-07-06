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

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// makeWorkspace returns a tmp dir auto-cleaned at test end.
func makeWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	abs, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("evalsymlinks(%s): %v", dir, err)
	}
	return abs
}

// --- registry -------------------------------------------------------------

func TestRegistry_New_RejectsDuplicates(t *testing.T) {
	ws := makeWorkspace(t)
	a := &ReadFileTool{Workspace: ws}
	b := &ReadFileTool{Workspace: ws}
	if _, err := New(a, b); err == nil {
		t.Fatal("expected duplicate-name error, got nil")
	}
}

func TestRegistry_Filter_RejectsUnknown(t *testing.T) {
	ws := makeWorkspace(t)
	r, err := New(&ReadFileTool{Workspace: ws})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.Filter([]string{"read_file", "noooope"}); err == nil {
		t.Fatal("expected unknown-tool error, got nil")
	}
}

func TestRegistry_Schemas_SortedAndDispatchUnknown(t *testing.T) {
	ws := makeWorkspace(t)
	r, err := New(&WriteFileTool{Workspace: ws}, &ReadFileTool{Workspace: ws})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	schemas := r.Schemas()
	if got := schemas[0].Function.Name; got != "read_file" {
		t.Errorf("schemas should be sorted; got [0]=%q", got)
	}
	if got := schemas[1].Function.Name; got != "write_file" {
		t.Errorf("schemas should be sorted; got [1]=%q", got)
	}

	if _, err := r.Dispatch(context.Background(), "noooope", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected unknown-tool dispatch error")
	}
}

// TestRegistry_Dispatch_FilteredToolReturnsWhitelistSentinel pins
// the v0.3 #561 fix: when an Agent's whitelist excludes a tool that
// exists in the broader registry, Dispatch returns the structured
// ErrToolNotInWhitelist sentinel rather than the generic "unknown
// tool" error. This distinction lets the future failure taxonomy
// (#559) route the two failure modes to ConstraintViolated vs
// ModelMisunderstood.
func TestRegistry_Dispatch_FilteredToolReturnsWhitelistSentinel(t *testing.T) {
	ws := makeWorkspace(t)
	r, err := New(&ReadFileTool{Workspace: ws}, &WriteFileTool{Workspace: ws}, &GrepTool{Workspace: ws})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Reviewer-style whitelist: read_file + grep only. write_file is
	// known to the system but excluded.
	reviewer, err := r.Filter([]string{"read_file", "grep"})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}

	// Whitelisted tool dispatches successfully (sanity).
	if _, err := reviewer.Dispatch(context.Background(), "read_file",
		json.RawMessage(`{"path":"nope-but-the-arg-error-is-fine.txt"}`)); err == nil {
		// We don't care about success here; we care that we got past
		// the registry layer into the tool itself (which will fail on
		// the missing file but that's a tool-level error, not a
		// dispatch error). Either way, no whitelist error.
		_ = err
	}

	// write_file is filtered out: expect ErrToolNotInWhitelist.
	_, err = reviewer.Dispatch(context.Background(), "write_file", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected ErrToolNotInWhitelist, got nil")
	}
	if !errors.Is(err, ErrToolNotInWhitelist) {
		t.Errorf("err: want errors.Is(ErrToolNotInWhitelist), got %v", err)
	}
	// The error should also name the offending tool for diagnosability.
	if !strings.Contains(err.Error(), "write_file") {
		t.Errorf("err message should name the tool, got: %v", err)
	}

	// A truly unknown tool name returns a DIFFERENT error (not the
	// whitelist sentinel). Confirms the two paths are distinguishable.
	_, err = reviewer.Dispatch(context.Background(), "completely_made_up", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected unknown-tool error, got nil")
	}
	if errors.Is(err, ErrToolNotInWhitelist) {
		t.Errorf("unknown-tool error should NOT be ErrToolNotInWhitelist, got: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("err message should say 'unknown tool', got: %v", err)
	}
}

// TestRegistry_Dispatch_UnfilteredRegistryNoFalsePositives pins
// back-compat: a Registry built with New() (no Filter step) never
// emits ErrToolNotInWhitelist. The sentinel only fires for filtered
// registries; un-Filter'd ones treat every unknown name as
// "unknown tool" exactly like before.
func TestRegistry_Dispatch_UnfilteredRegistryNoFalsePositives(t *testing.T) {
	ws := makeWorkspace(t)
	r, err := New(&ReadFileTool{Workspace: ws})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Dispatch(context.Background(), "write_file", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for non-registered tool")
	}
	if errors.Is(err, ErrToolNotInWhitelist) {
		t.Errorf("un-Filter'd Registry should not emit ErrToolNotInWhitelist, got: %v", err)
	}
}

// --- workspace ------------------------------------------------------------

func TestResolveInside_Cases(t *testing.T) {
	ws := makeWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "ok.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	t.Run("relative ok", func(t *testing.T) {
		got, err := resolveInside(ws, "ok.txt")
		if err != nil {
			t.Fatalf("resolveInside: %v", err)
		}
		if want := filepath.Join(ws, "ok.txt"); got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("absolute inside workspace accepted", func(t *testing.T) {
		abs := filepath.Join(ws, "ok.txt")
		got, err := resolveInside(ws, abs)
		if err != nil {
			t.Fatalf("resolveInside absolute inside: %v", err)
		}
		if got != abs {
			t.Errorf("absolute inside: got %q want %q", got, abs)
		}
	})

	t.Run("absolute outside workspace rejected", func(t *testing.T) {
		_, err := resolveInside(ws, "/etc/passwd")
		if !errors.Is(err, ErrPathEscapesWorkspace) {
			t.Errorf("expected ErrPathEscapesWorkspace, got %v", err)
		}
	})

	t.Run("parent traversal rejected", func(t *testing.T) {
		_, err := resolveInside(ws, "../etc/passwd")
		if !errors.Is(err, ErrPathEscapesWorkspace) {
			t.Errorf("expected ErrPathEscapesWorkspace, got %v", err)
		}
	})

	t.Run("symlink escape rejected", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink semantics differ on windows")
		}
		outside := t.TempDir()
		// Create symlink inside ws pointing outside.
		link := filepath.Join(ws, "outside-link")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		_, err := resolveInside(ws, "outside-link")
		if !errors.Is(err, ErrPathEscapesWorkspace) {
			t.Errorf("expected ErrPathEscapesWorkspace via symlink, got %v", err)
		}
	})

	// Prefix-collision: a sibling directory whose name has the workspace path
	// as a string prefix (ws + "-evil") must NOT be treated as inside. This
	// pins the guarantee that containment uses filepath.Rel (a real path
	// relationship), not a naive strings.HasPrefix on the cleaned path.
	t.Run("absolute prefix-collision sibling rejected", func(t *testing.T) {
		evil := ws + "-evil"
		if err := os.MkdirAll(evil, 0o755); err != nil {
			t.Fatalf("mkdir sibling: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(evil) })
		secret := filepath.Join(evil, "secret.txt")
		if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed sibling secret: %v", err)
		}
		if _, err := resolveInside(ws, secret); !errors.Is(err, ErrPathEscapesWorkspace) {
			t.Errorf("prefix-collision sibling must be rejected, got %v", err)
		}
	})

	// Absolute + symlink escape: the new absolute-path branch must still be
	// caught by the terminal symlink-resolution check. An in-workspace symlink
	// to an outside dir, referenced by its ABSOLUTE path, must be rejected.
	t.Run("absolute path through escaping symlink rejected", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink semantics differ on windows")
		}
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed outside secret: %v", err)
		}
		link := filepath.Join(ws, "abs-escape-link")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		abs := filepath.Join(ws, "abs-escape-link", "secret.txt")
		if _, err := resolveInside(ws, abs); !errors.Is(err, ErrPathEscapesWorkspace) {
			t.Errorf("absolute path through escaping symlink must be rejected, got %v", err)
		}
	})

	t.Run("nonexistent path ok for write_file", func(t *testing.T) {
		_, err := resolveInside(ws, "subdir/new.txt")
		if err != nil {
			t.Errorf("nonexistent path should resolve: %v", err)
		}
	})
}

// --- read_file ------------------------------------------------------------

func TestReadFile_OK_AndTruncate(t *testing.T) {
	ws := makeWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hi there"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rf := &ReadFileTool{Workspace: ws, MaxBytes: 100}
	res, err := rf.Execute(context.Background(), json.RawMessage(`{"path":"hello.txt"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if out["content"] != "hi there" {
		t.Errorf("content: got %q", out["content"])
	}
	if out["truncated"].(bool) {
		t.Errorf("did not expect truncation")
	}

	// Now force truncation.
	rf.MaxBytes = 3
	res, err = rf.Execute(context.Background(), json.RawMessage(`{"path":"hello.txt"}`))
	if err != nil {
		t.Fatalf("Execute (truncate): %v", err)
	}
	out = res.Output.(map[string]any)
	if out["content"] != "hi " {
		t.Errorf("truncated content: got %q want %q", out["content"], "hi ")
	}
	if !out["truncated"].(bool) {
		t.Errorf("expected truncated=true")
	}
}

func TestReadFile_PathRequired(t *testing.T) {
	rf := &ReadFileTool{Workspace: makeWorkspace(t)}
	if _, err := rf.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for empty path")
	}
}

// TestReadFile_WholeReadReportsLineCounts pins the default-mode contract:
// no offset / no limit returns raw bytes (no added trailing newline) plus
// truthful line bookkeeping fields. The line fields exist on every
// result -- including default-mode -- so the model has the data needed
// to decide whether to follow up with a ranged read.
func TestReadFile_WholeReadReportsLineCounts(t *testing.T) {
	ws := makeWorkspace(t)
	content := "alpha\nbeta\ngamma\n"
	if err := os.WriteFile(filepath.Join(ws, "three.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rf := &ReadFileTool{Workspace: ws, MaxBytes: 100}
	res, err := rf.Execute(context.Background(), json.RawMessage(`{"path":"three.txt"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if out["content"] != content {
		t.Errorf("content: got %q want %q", out["content"], content)
	}
	if out["bytes"].(int) != len(content) {
		t.Errorf("bytes: got %v want %d", out["bytes"], len(content))
	}
	if out["total_lines"].(int) != 3 {
		t.Errorf("total_lines: got %v want 3", out["total_lines"])
	}
	if out["start_line"].(int) != 1 || out["end_line"].(int) != 3 {
		t.Errorf("line range: got [%v..%v] want [1..3]", out["start_line"], out["end_line"])
	}
}

// TestReadFile_DefaultModeTracksTotalLinesOnTruncate covers the
// truncation case: when the default-mode read hits MaxBytes mid-file,
// total_lines must still reflect the whole file so the caller can
// follow up with a ranged read targeting the remaining lines.
func TestReadFile_DefaultModeTracksTotalLinesOnTruncate(t *testing.T) {
	ws := makeWorkspace(t)
	// 20 lines of "line-NN\n"; each line ~8 bytes so 20 lines = 160 bytes.
	content := ""
	for i := 1; i <= 20; i++ {
		content += fmt.Sprintf("line-%02d\n", i)
	}
	if err := os.WriteFile(filepath.Join(ws, "long.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Cap below the file size so we definitely truncate.
	rf := &ReadFileTool{Workspace: ws, MaxBytes: 40}
	res, err := rf.Execute(context.Background(), json.RawMessage(`{"path":"long.txt"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if !out["truncated"].(bool) {
		t.Errorf("expected truncated=true")
	}
	if out["total_lines"].(int) != 20 {
		t.Errorf("total_lines after truncate: got %v want 20", out["total_lines"])
	}
}

// TestReadFile_RangedRead exercises the offset+limit path. The model
// reads a window of a long file without dragging the whole thing into
// the OAI message history (the bug that motivated this tool change --
// a single 65 KB read of CHANGELOG.md polluted every subsequent turn).
func TestReadFile_RangedRead(t *testing.T) {
	ws := makeWorkspace(t)
	// 50 lines so we can pick a clear window in the middle.
	content := ""
	for i := 1; i <= 50; i++ {
		content += fmt.Sprintf("L%02d\n", i)
	}
	if err := os.WriteFile(filepath.Join(ws, "fifty.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rf := &ReadFileTool{Workspace: ws, MaxBytes: 16 * 1024}
	res, err := rf.Execute(context.Background(),
		json.RawMessage(`{"path":"fifty.txt","offset":11,"limit":5}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	want := "L11\nL12\nL13\nL14\nL15\n"
	if out["content"] != want {
		t.Errorf("ranged content: got %q want %q", out["content"], want)
	}
	if out["start_line"].(int) != 11 || out["end_line"].(int) != 15 {
		t.Errorf("ranged line span: got [%v..%v] want [11..15]", out["start_line"], out["end_line"])
	}
	if out["total_lines"].(int) != 50 {
		t.Errorf("total_lines: got %v want 50", out["total_lines"])
	}
	if out["truncated"].(bool) {
		t.Errorf("did not expect truncated=true on a within-cap range")
	}
}

// TestReadFile_RangedRead_PastEOF documents the contract for an
// offset past the end of the file: empty content, truthful total
// lines, end_line < start_line.
func TestReadFile_RangedRead_PastEOF(t *testing.T) {
	ws := makeWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "short.txt"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rf := &ReadFileTool{Workspace: ws}
	res, err := rf.Execute(context.Background(),
		json.RawMessage(`{"path":"short.txt","offset":100,"limit":10}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if out["content"] != "" {
		t.Errorf("content past EOF: got %q want empty", out["content"])
	}
	if out["total_lines"].(int) != 3 {
		t.Errorf("total_lines: got %v want 3", out["total_lines"])
	}
}

// TestReadFile_RangedRead_HitsMaxBytes ensures that even in ranged
// mode, MaxBytes is the hard ceiling -- the model cannot ask for a
// huge limit and bypass the cap.
func TestReadFile_RangedRead_HitsMaxBytes(t *testing.T) {
	ws := makeWorkspace(t)
	content := ""
	for i := 1; i <= 200; i++ {
		content += fmt.Sprintf("line-%03d-padding-text\n", i)
	}
	if err := os.WriteFile(filepath.Join(ws, "big.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rf := &ReadFileTool{Workspace: ws, MaxBytes: 100}
	res, err := rf.Execute(context.Background(),
		json.RawMessage(`{"path":"big.txt","offset":1,"limit":200}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if !out["truncated"].(bool) {
		t.Errorf("expected truncated=true at MaxBytes ceiling")
	}
	if out["bytes"].(int) > 100 {
		t.Errorf("bytes exceeded cap: got %v", out["bytes"])
	}
}

// TestReadFile_InvalidArgs covers the new arg validation: negative
// offset / negative limit / bad JSON.
func TestReadFile_InvalidArgs(t *testing.T) {
	rf := &ReadFileTool{Workspace: makeWorkspace(t)}
	cases := []string{
		`{"path":"p.txt","offset":-1}`,
		`{"path":"p.txt","limit":-5}`,
	}
	for _, c := range cases {
		if _, err := rf.Execute(context.Background(), json.RawMessage(c)); err == nil {
			t.Errorf("expected error for args %s", c)
		}
	}
}

// --- write_file -----------------------------------------------------------

func TestWriteFile_CreatesParents(t *testing.T) {
	ws := makeWorkspace(t)
	wf := &WriteFileTool{Workspace: ws}
	_, err := wf.Execute(context.Background(),
		json.RawMessage(`{"path":"a/b/c.txt","content":"hello"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(ws, "a", "b", "c.txt"))
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("readback: got %q", string(got))
	}
}

// --- str_replace ----------------------------------------------------------

func TestStrReplace_HappyPath_ExactlyOnce(t *testing.T) {
	ws := makeWorkspace(t)
	src := "foo bar baz\n"
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte(src), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tool := &StrReplaceTool{Workspace: ws}
	_, err := tool.Execute(context.Background(),
		json.RawMessage(`{"path":"f.txt","old_string":"bar","new_string":"BAR"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(ws, "f.txt"))
	if string(got) != "foo BAR baz\n" {
		t.Errorf("got %q", string(got))
	}
}

func TestStrReplace_RecoversFromWhitespaceMismatch(t *testing.T) {
	ws := makeWorkspace(t)
	// Real file is tab-indented (as Go source is).
	src := "func f() {\n\treturn 1\n}\n"
	if err := os.WriteFile(filepath.Join(ws, "f.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tool := &StrReplaceTool{Workspace: ws}
	// Model reproduces the body with SPACES instead of the tab. Exact match
	// fails, but the whitespace-normalized fallback should locate it and apply
	// the replacement against the real byte span.
	wsArgs := map[string]string{
		"path":       "f.go",
		"old_string": "func f() {\n    return 1\n}", // spaces, not the file's tab
		"new_string": "func f() {\n\treturn 2\n}",
	}
	wsBuf, _ := json.Marshal(wsArgs)
	_, err := tool.Execute(context.Background(), wsBuf)
	if err != nil {
		t.Fatalf("expected whitespace-normalized recovery, got error: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(ws, "f.go"))
	if !strings.Contains(string(got), "return 2") {
		t.Errorf("replacement not applied: %q", string(got))
	}
}

func TestStrReplace_ReturnsActualContentOnFabricatedOldString(t *testing.T) {
	ws := makeWorkspace(t)
	// Real file: a plain hyphen in the comment, a specific body.
	src := "// keep this comment - a plain hyphen here\n" +
		"func compute() string {\n\treturn hash()\n}\n"
	if err := os.WriteFile(filepath.Join(ws, "f.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tool := &StrReplaceTool{Workspace: ws}
	// Model fabricates old_string (em dash + wrong wording + wrong body) but
	// anchors on the real, verbatim func signature line. The tool must not
	// match, and must surface the ACTUAL current content so the model can retry
	// against truth instead of re-hallucinating.
	old := "// keep this comment — an EM dash and wrong words\n" +
		"func compute() string {\n\treturn somethingElse()\n}"
	args := map[string]string{"path": "f.go", "old_string": old, "new_string": "x"}
	buf, _ := json.Marshal(args)
	_, err := tool.Execute(context.Background(), buf)
	if err == nil {
		t.Fatal("expected error for fabricated old_string")
	}
	if !strings.Contains(err.Error(), "a plain hyphen here") || !strings.Contains(err.Error(), "return hash()") {
		t.Errorf("error should surface real file content near the anchor, got: %v", err)
	}
}

func TestStrReplace_CountMismatchIsError(t *testing.T) {
	ws := makeWorkspace(t)
	src := "x x x\n"
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte(src), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tool := &StrReplaceTool{Workspace: ws}
	_, err := tool.Execute(context.Background(),
		json.RawMessage(`{"path":"f.txt","old_string":"x","new_string":"y"}`))
	if err == nil || !strings.Contains(err.Error(), "found 3 times") {
		t.Errorf("expected count-mismatch error, got %v", err)
	}
}

func TestStrReplace_ExpectedReplacements(t *testing.T) {
	ws := makeWorkspace(t)
	src := "x x x\n"
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte(src), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tool := &StrReplaceTool{Workspace: ws}
	_, err := tool.Execute(context.Background(),
		json.RawMessage(`{"path":"f.txt","old_string":"x","new_string":"y","expected_replacements":3}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(ws, "f.txt"))
	if string(got) != "y y y\n" {
		t.Errorf("got %q", string(got))
	}
}

// --- grep -----------------------------------------------------------------

func TestGrep_BasicAndSkipsGitDir(t *testing.T) {
	ws := makeWorkspace(t)
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
		t.Fatalf("seed git: %v", err)
	}
	must := func(name, body string) {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	must("a.txt", "alpha\nbeta\ngamma\n")
	must("b.txt", "gamma is here\n")
	must(".git/HEAD", "ref: refs/heads/main\n")

	g := &GrepTool{Workspace: ws}
	res, err := g.Execute(context.Background(), json.RawMessage(`{"pattern":"gamma"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	matches := out["matches"].([]grepMatch)
	if len(matches) != 2 {
		t.Errorf("matches: want 2 got %d (%v)", len(matches), matches)
	}
	for _, m := range matches {
		if strings.Contains(m.File, ".git"+string(filepath.Separator)) {
			t.Errorf(".git dir should have been skipped, got match in %q", m.File)
		}
	}
}

func TestGrep_MaxCap(t *testing.T) {
	ws := makeWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "many.txt"),
		[]byte("hit\nhit\nhit\nhit\nhit\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	g := &GrepTool{Workspace: ws}
	res, err := g.Execute(context.Background(),
		json.RawMessage(`{"pattern":"hit","max":3}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	matches := out["matches"].([]grepMatch)
	if len(matches) != 3 {
		t.Errorf("expected cap of 3, got %d", len(matches))
	}
	if !out["truncated"].(bool) {
		t.Errorf("expected truncated=true")
	}
}

func TestGrep_InvalidPatternIsError(t *testing.T) {
	g := &GrepTool{Workspace: makeWorkspace(t)}
	if _, err := g.Execute(context.Background(),
		json.RawMessage(`{"pattern":"[unbalanced"}`)); err == nil {
		t.Fatal("expected regex compile error")
	}
}

// --- bash -----------------------------------------------------------------

func TestBash_HappyPathExitZero(t *testing.T) {
	ws := makeWorkspace(t)
	b := &BashTool{Workspace: ws}
	res, err := b.Execute(context.Background(),
		json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if out["exit_code"].(int) != 0 {
		t.Errorf("exit_code: got %v", out["exit_code"])
	}
	if !strings.Contains(out["stdout"].(string), "hi") {
		t.Errorf("stdout: got %q", out["stdout"])
	}
	if out["timed_out"].(bool) {
		t.Errorf("did not expect timeout")
	}
}

func TestBash_NonZeroExitNotAnError(t *testing.T) {
	b := &BashTool{Workspace: makeWorkspace(t)}
	res, err := b.Execute(context.Background(),
		json.RawMessage(`{"command":"exit 7"}`))
	if err != nil {
		t.Fatalf("non-zero should not be an error: %v", err)
	}
	out := res.Output.(map[string]any)
	if out["exit_code"].(int) != 7 {
		t.Errorf("exit_code: want 7 got %v", out["exit_code"])
	}
}

func TestBash_TimeoutFlag(t *testing.T) {
	b := &BashTool{Workspace: makeWorkspace(t), Timeout: 100 * time.Millisecond}
	res, err := b.Execute(context.Background(),
		json.RawMessage(`{"command":"sleep 5"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if !out["timed_out"].(bool) {
		t.Errorf("expected timed_out=true; output=%v", out)
	}
}

// TestBash_GrandchildPipeDoesNotDeadlock is the regression test for
// defilantech/LLMKube#539. Without WaitDelay + process-group kill on
// Cancel, a bash command that backgrounds a long-lived grandchild
// causes Cmd.Wait to block until the grandchild exits (because the
// grandchild inherited the shell's stdout/stderr pipes and Cmd.Wait
// blocks in awaitGoroutines waiting for io.Copy to see EOF).
//
// The command `(sleep 60 &) ; echo started ; sleep 0.1`:
//   - the subshell `(...)` runs `sleep 60 &` and exits immediately
//   - `sleep 60` is left running in the background, holding the
//     parent shell's pipes via inheritance
//   - the parent echoes "started", sleeps 0.1s, and exits
//   - without the fix, Cmd.Wait blocks for 60s waiting for the
//     orphaned `sleep 60` to finish
//   - with the fix, the 200ms timeout fires Cancel, which kills the
//     entire process group (including sleep), pipes close, Wait
//     returns within timeout + WaitDelay (200ms + 5s).
func TestBash_GrandchildPipeDoesNotDeadlock(t *testing.T) {
	b := &BashTool{
		Workspace: makeWorkspace(t),
		// Short Timeout so we hit the cancel-then-WaitDelay path
		// quickly. The fix bounds total wall at Timeout + WaitDelay.
		Timeout: 200 * time.Millisecond,
	}
	const cmd = `(sleep 60 &) ; echo started ; sleep 0.1`
	argsJSON := json.RawMessage(`{"command":` + strconv.Quote(cmd) + `}`)

	start := time.Now()
	res, err := b.Execute(context.Background(), argsJSON)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Without the fix this blocks for ~60s. With it, Timeout (200ms)
	// plus WaitDelay (5s) caps wall at ~5.2s plus a little OS slack.
	// 15s is a generous upper bound; anything above 30s is the bug.
	if elapsed > 15*time.Second {
		t.Errorf("BashTool blocked for %v on a backgrounded-grandchild "+
			"command; expected <15s; #539 regression. cmd=%q", elapsed, cmd)
	}
	out := res.Output.(map[string]any)
	stdout, _ := out["stdout"].(string)
	// The shell printed "started" before the 0.1s sleep at the end,
	// so we should see it captured. (The 0.1s tail makes the parent
	// shell hit the 200ms Timeout reliably even on slow CI.)
	if !strings.Contains(stdout, "started") {
		t.Errorf("expected 'started' in stdout; got: %q", stdout)
	}
	if !out["timed_out"].(bool) {
		t.Errorf("expected timed_out=true (200ms < 0.1s sleep should "+
			"miss, but in practice the +0.1s sleep is enough); out=%v", out)
	}
}

func TestBash_EnvScrubbed(t *testing.T) {
	t.Setenv("FOREMAN_TEST_SHOULD_NOT_LEAK", "secret-value")
	t.Setenv("PATH", os.Getenv("PATH")) // ensure PATH visible to subshell
	b := &BashTool{Workspace: makeWorkspace(t)}
	res, err := b.Execute(context.Background(),
		json.RawMessage(`{"command":"env"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	envDump := res.Output.(map[string]any)["stdout"].(string)
	if strings.Contains(envDump, "FOREMAN_TEST_SHOULD_NOT_LEAK") {
		t.Errorf("env leaked into sandbox: %s", envDump)
	}
	if !strings.Contains(envDump, "PATH=") {
		t.Errorf("PATH should pass through; env dump=%s", envDump)
	}
}

// --- bash sandbox: WORKSPACE_ROOT + cd guard (#567) ----------------------

// TestBash_WorkspaceRootEnvIsSet verifies that the model can read its
// workspace path from $WORKSPACE_ROOT inside a bash call. This is the
// orientation contract the system prompt's workspace block tells the
// model about; without it the model has to discover the path with
// `pwd` on every turn (or worse, `find /`).
func TestBash_WorkspaceRootEnvIsSet(t *testing.T) {
	ws := makeWorkspace(t)
	b := &BashTool{Workspace: ws}
	res, err := b.Execute(context.Background(),
		json.RawMessage(`{"command":"printf %s \"$WORKSPACE_ROOT\""}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	stdout, _ := out["stdout"].(string)
	if stdout != ws {
		t.Errorf("WORKSPACE_ROOT in bash: want %q got %q", ws, stdout)
	}
}

// TestBash_CdGuardBlocksAbsolutePath proves the cd-guard rejects an
// absolute-path cd. The post-cd command (echo) must NOT run because
// the `cd ... && ...` short-circuits on the cd's non-zero exit.
func TestBash_CdGuardBlocksAbsolutePath(t *testing.T) {
	b := &BashTool{Workspace: makeWorkspace(t)}
	res, err := b.Execute(context.Background(),
		json.RawMessage(`{"command":"cd /tmp && echo SHOULD-NOT-PRINT"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if strings.Contains(out["stdout"].(string), "SHOULD-NOT-PRINT") {
		t.Errorf("guard failed; absolute cd allowed downstream cmd to run. stdout=%q", out["stdout"])
	}
	if !strings.Contains(out["stderr"].(string), "outside workspace") {
		t.Errorf("guard error message missing from stderr. stderr=%q", out["stderr"])
	}
	if out["exit_code"].(int) == 0 {
		t.Errorf("exit_code should be non-zero on blocked cd; got 0")
	}
}

// TestBash_CdGuardAllowsRelativePath confirms relative cd still works
// for normal workspace navigation. The model needs this for routine
// tasks like `cd internal/foo && grep ...`.
func TestBash_CdGuardAllowsRelativePath(t *testing.T) {
	ws := makeWorkspace(t)
	// Make a subdir we can cd into.
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "sub", "marker"), []byte("ok\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	b := &BashTool{Workspace: ws}
	res, err := b.Execute(context.Background(),
		json.RawMessage(`{"command":"cd sub && cat marker"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if !strings.Contains(out["stdout"].(string), "ok") {
		t.Errorf("relative cd should work; stdout=%q stderr=%q", out["stdout"], out["stderr"])
	}
	if out["exit_code"].(int) != 0 {
		t.Errorf("exit_code: want 0 got %v", out["exit_code"])
	}
}

// TestBash_CdGuardAllowsParentRelative confirms `cd ..` works. The
// guard only blocks paths starting with `/`; parent-relative navigation
// stays unrestricted so the model can move freely within the workspace
// tree. The OS still rejects parent traversal that exits the workspace
// at exec time (workspace was created as a temp dir; `cd ../..` lands
// outside it but is still a non-absolute cd and so allowed by guard).
//
// This is intentional: the cd-guard is for *orientation*, not security.
// Path-bounded tools (read_file, str_replace, grep) already enforce
// containment via resolveInside; bash is the escape hatch by design.
func TestBash_CdGuardAllowsParentRelative(t *testing.T) {
	b := &BashTool{Workspace: makeWorkspace(t)}
	res, err := b.Execute(context.Background(),
		json.RawMessage(`{"command":"cd .. && pwd"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if out["exit_code"].(int) != 0 {
		t.Errorf("cd .. should succeed; got exit=%v stderr=%q",
			out["exit_code"], out["stderr"])
	}
}

// TestBash_CdGuardBlocksTildeHome confirms cd ~ and cd ~user are
// blocked. Tilde expansion produces an absolute path before the
// function sees it, so the `/*` case catches it.
func TestBash_CdGuardBlocksTildeHome(t *testing.T) {
	b := &BashTool{Workspace: makeWorkspace(t)}
	res, err := b.Execute(context.Background(),
		json.RawMessage(`{"command":"cd ~ && echo NOPE"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if strings.Contains(out["stdout"].(string), "NOPE") {
		t.Errorf("guard failed on cd ~; stdout=%q", out["stdout"])
	}
}

// --- submit_result --------------------------------------------------------

func TestSubmitResult_HappyPath(t *testing.T) {
	tool := SubmitResultTool{}
	res, err := tool.Execute(context.Background(), json.RawMessage(
		`{"verdict":"GO","summary":"fixed","commit_message":"fix: x\n\nFixes #1"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Terminal || res.Verdict != "GO" {
		t.Errorf("envelope wrong: %+v", res)
	}
	if res.CommitMessage == "" {
		t.Errorf("commit_message must round-trip; got empty")
	}
}

func TestSubmitResult_InvalidVerdict(t *testing.T) {
	tool := SubmitResultTool{}
	_, err := tool.Execute(context.Background(),
		json.RawMessage(`{"verdict":"MAYBE","summary":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "invalid verdict") {
		t.Errorf("expected invalid-verdict error, got %v", err)
	}
}

func TestSubmitResult_SummaryRequired(t *testing.T) {
	tool := SubmitResultTool{}
	_, err := tool.Execute(context.Background(),
		json.RawMessage(`{"verdict":"NO-GO","summary":""}`))
	if err == nil || !strings.Contains(err.Error(), "summary is required") {
		t.Errorf("expected summary-required error, got %v", err)
	}
}

func TestSubmitResult_SummaryTooLong(t *testing.T) {
	tool := SubmitResultTool{}
	long := strings.Repeat("x", MaxSubmitSummaryLen+1)
	body, _ := json.Marshal(map[string]any{"verdict": "GO", "summary": long})
	res, err := tool.Execute(context.Background(), body)
	if err != nil {
		t.Fatalf("Execute should succeed after truncation: %v", err)
	}
	if len(res.Summary) > MaxSubmitSummaryLen {
		t.Errorf("summary not truncated: got %d bytes", len(res.Summary))
	}
	if !strings.HasSuffix(res.Summary, "…") {
		t.Errorf("truncated summary should end with ellipsis, got %q", res.Summary)
	}
}

func TestSubmitResult_SummaryTruncationRuneSafe(t *testing.T) {
	tool := SubmitResultTool{}
	// Build a summary that is exactly MaxSubmitSummaryLen bytes, then
	// append a multi-byte rune so the byte limit falls inside it.
	base := strings.Repeat("x", MaxSubmitSummaryLen-3) // 3 bytes for "…"
	summary := base + "🔥"                              // 4-byte rune; total > MaxSubmitSummaryLen
	body, _ := json.Marshal(map[string]any{"verdict": "GO", "summary": summary})
	res, err := tool.Execute(context.Background(), body)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Summary) > MaxSubmitSummaryLen {
		t.Errorf("summary exceeds cap: got %d bytes", len(res.Summary))
	}
	// The emoji should be dropped entirely, not split.
	if strings.Contains(res.Summary, "🔥") {
		t.Errorf("multi-byte rune should be dropped, not split")
	}
}

func TestTruncateRuneSafe(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
	}{
		{
			name:   "short string unchanged",
			input:  "hello",
			maxLen: 10,
		},
		{
			name:   "exact length unchanged",
			input:  strings.Repeat("x", 10),
			maxLen: 10,
		},
		{
			name:   "truncate with ellipsis",
			input:  strings.Repeat("x", 15),
			maxLen: 10,
		},
		{
			name:   "multi-byte rune dropped not split",
			input:  strings.Repeat("x", 9) + "🔥",
			maxLen: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateRuneSafe(tt.input, tt.maxLen)
			if len(result) > tt.maxLen {
				t.Errorf("truncateRuneSafe(%q, %d) = %d bytes, want <= %d",
					tt.input, tt.maxLen, len(result), tt.maxLen)
			}
			if len(tt.input) > tt.maxLen && !strings.HasSuffix(result, "…") {
				t.Errorf("truncated result should end with ellipsis, got %q", result)
			}
		})
	}
}
