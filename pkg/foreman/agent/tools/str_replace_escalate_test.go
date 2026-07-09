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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func strReplaceArgsJSON(t *testing.T, path, oldS, newS string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"path": path, "old_string": oldS, "new_string": newS})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

// TestStrReplace_EscalatesAfterRepeatedFailure: the first non-matching edit
// returns the normal anchor hint; the second consecutive failure on the same
// file escalates to a STOP directive with the full file inlined and a
// write_file order (#1025). This is the deterministic escape a model that
// keeps re-hallucinating old_string needs, so it stops looping into
// RepeatedToolCall.
func TestStrReplace_EscalatesAfterRepeatedFailure(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "f.go"), []byte("package x\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &StrReplaceTool{Workspace: ws}
	bad := strReplaceArgsJSON(t, "f.go", "ZZZ_NO_SUCH_LINE_ANYWHERE_ZZZ", "func B() {}")

	_, err1 := tool.Execute(context.Background(), bad)
	if err1 == nil {
		t.Fatal("first failing edit should error")
	}
	if strings.Contains(err1.Error(), "STOP calling str_replace") {
		t.Fatalf("first failure should be a normal hint, not escalated: %v", err1)
	}

	_, err2 := tool.Execute(context.Background(), bad)
	if err2 == nil {
		t.Fatal("second failing edit should error")
	}
	if !strings.Contains(err2.Error(), "STOP calling str_replace") {
		t.Errorf("second failure should escalate with a STOP directive; got: %v", err2)
	}
	if !strings.Contains(err2.Error(), "write_file") {
		t.Errorf("escalated hint should order write_file; got: %v", err2)
	}
	if !strings.Contains(err2.Error(), "func A()") {
		t.Errorf("escalated hint should inline the small file's content; got: %v", err2)
	}
}

// TestStrReplace_SuccessResetsFailureCounter: a successful edit clears the
// per-file failure count, so a later failure starts over at the normal hint
// rather than immediately escalating.
func TestStrReplace_SuccessResetsFailureCounter(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "f.go"), []byte("package x\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &StrReplaceTool{Workspace: ws}
	bad := strReplaceArgsJSON(t, "f.go", "ZZZ_NO_SUCH_LINE_ANYWHERE_ZZZ", "x")
	good := strReplaceArgsJSON(t, "f.go", "func A() {}", "func A() { _ = 1 }")

	if _, err := tool.Execute(context.Background(), bad); err == nil {
		t.Fatal("first edit should fail")
	}
	if _, err := tool.Execute(context.Background(), good); err != nil {
		t.Fatalf("good edit should succeed and reset the counter: %v", err)
	}
	// Counter reset: the next failure is a "first" failure again, not escalated.
	_, err := tool.Execute(context.Background(), bad)
	if err == nil {
		t.Fatal("edit should fail")
	}
	if strings.Contains(err.Error(), "STOP calling str_replace") {
		t.Errorf("a success should reset the counter; got escalated: %v", err)
	}
}

func TestEscalatedEditHint_LargeFileNotInlined(t *testing.T) {
	big := strings.Repeat("line\n", strReplaceEscalateMaxLines+50)
	h := escalatedEditHint("big.txt", big, 2)
	if strings.Contains(h, numberLines(big)) {
		t.Error("a large file must not be dumped inline")
	}
	if !strings.Contains(h, "read_file") || !strings.Contains(h, "write_file") {
		t.Errorf("large-file escalation should steer to read_file/write_file; got head: %.120s", h)
	}
}

func TestNumberLines(t *testing.T) {
	if got := numberLines("a\nb"); got != "1\ta\n2\tb\n" {
		t.Errorf("numberLines = %q", got)
	}
}
