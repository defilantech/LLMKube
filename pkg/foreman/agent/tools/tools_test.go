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
	"os"
	"path/filepath"
	"runtime"
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

	t.Run("absolute rejected", func(t *testing.T) {
		_, err := resolveInside(ws, filepath.Join(ws, "ok.txt"))
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
	_, err := tool.Execute(context.Background(), body)
	if err == nil || !strings.Contains(err.Error(), "chars or fewer") {
		t.Errorf("expected summary-too-long error, got %v", err)
	}
}
