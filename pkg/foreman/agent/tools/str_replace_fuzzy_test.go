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

// execStrReplace marshals the args and runs the tool against ws.
func execStrReplace(t *testing.T, ws, path, oldString, newString string) error {
	t.Helper()
	tool := &StrReplaceTool{Workspace: ws}
	buf, err := json.Marshal(map[string]string{
		"path": path, "old_string": oldString, "new_string": newString,
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	_, err = tool.Execute(context.Background(), buf)
	return err
}

func seedFile(t *testing.T, ws, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
}

func readBack(t *testing.T, ws, name string) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(ws, name))
	if err != nil {
		t.Fatalf("readback %s: %v", name, err)
	}
	return string(got)
}

// The primary #907 failure mode: the model read the file, then retyped
// old_string from memory with one identifier drifted. Exact match and the
// whitespace fallback both miss; the fuzzy window must locate the unique
// near-match and apply against the file's real bytes.
func TestStrReplace_FuzzyRecoversSingleLineTokenDrift(t *testing.T) {
	ws := makeWorkspace(t)
	src := "func compute() int {\n\treturn userCount\n}\n"
	seedFile(t, ws, "f.go", src)
	// Model drifted userCount -> usrCount (distance 1).
	err := execStrReplace(t, ws, "f.go", "\treturn usrCount", "\treturn userCount + 1")
	if err != nil {
		t.Fatalf("expected fuzzy recovery, got error: %v", err)
	}
	got := readBack(t, ws, "f.go")
	if !strings.Contains(got, "return userCount + 1") {
		t.Errorf("replacement not applied: %q", got)
	}
	if strings.Contains(got, "usrCount") {
		t.Errorf("drifted token leaked into the file: %q", got)
	}
}

// Multi-line drift: a paraphrased comment above an otherwise-verbatim block.
// Every line clears the per-line threshold and the window aggregate stays
// small, so the unique window applies.
func TestStrReplace_FuzzyRecoversMultiLineCommentDrift(t *testing.T) {
	ws := makeWorkspace(t)
	src := "// recordHeartbeat stamps the node's lastSeen time.\n" +
		"func (r *Registrar) recordHeartbeat(now time.Time) {\n" +
		"\tr.node.Status.LastSeen = now\n" +
		"}\n"
	seedFile(t, ws, "f.go", src)
	old := "// recordHeartbeat stamps the node's lastSeen timestamp.\n" +
		"func (r *Registrar) recordHeartbeat(now time.Time) {\n" +
		"\tr.node.Status.LastSeen = now\n" +
		"}"
	repl := "// recordHeartbeat stamps the node's lastSeen time.\n" +
		"func (r *Registrar) recordHeartbeat(now time.Time) {\n" +
		"\tr.node.Status.LastSeen = now.UTC()\n" +
		"}"
	if err := execStrReplace(t, ws, "f.go", old, repl); err != nil {
		t.Fatalf("expected fuzzy recovery, got error: %v", err)
	}
	got := readBack(t, ws, "f.go")
	if !strings.Contains(got, "now.UTC()") {
		t.Errorf("replacement not applied: %q", got)
	}
}

// Two near-identical blocks: the fuzzy window sees two candidates and MUST
// refuse rather than guess. The file must be untouched.
func TestStrReplace_FuzzyRefusesAmbiguousWindows(t *testing.T) {
	ws := makeWorkspace(t)
	src := "func alpha() int {\n\treturn count\n}\n" +
		"func beta() int {\n\treturn count\n}\n"
	seedFile(t, ws, "f.go", src)
	err := execStrReplace(t, ws, "f.go", "\treturn cnt", "\treturn total")
	if err == nil {
		t.Fatal("expected refusal on ambiguous fuzzy match, got nil error")
	}
	if got := readBack(t, ws, "f.go"); got != src {
		t.Errorf("file must be unchanged on refusal, got %q", got)
	}
}

// A genuinely different block of the same line count must not match: the
// per-line threshold rejects it and the tool falls through to an error.
func TestStrReplace_FuzzyRefusesGenuinelyDifferentBlock(t *testing.T) {
	ws := makeWorkspace(t)
	src := "func parse(raw []byte) (Config, error) {\n" +
		"\tvar c Config\n" +
		"\treturn c, yaml.Unmarshal(raw, &c)\n" +
		"}\n"
	seedFile(t, ws, "f.go", src)
	old := "func render(w io.Writer) error {\n" +
		"\ttpl := template.Must(template.New(\"x\").Parse(body))\n" +
		"\treturn tpl.Execute(w, nil)\n" +
		"}"
	err := execStrReplace(t, ws, "f.go", old, "x")
	if err == nil {
		t.Fatal("expected error for a genuinely different block, got nil")
	}
	if got := readBack(t, ws, "f.go"); got != src {
		t.Errorf("file must be unchanged, got %q", got)
	}
}

// An all-blank window (every line empty after whitespace normalization) must
// never fuzzy-match: it would otherwise qualify everywhere blank lines
// cluster. Tested via applyFuzzyMatch directly because Execute's earlier
// whitespace fallback intercepts the unique-blank-window case (pre-existing
// #917 behavior, unchanged here); the fuzzy layer must still refuse on its
// own so the defense does not depend on call order.
func TestApplyFuzzyMatch_RefusesAllBlankWindow(t *testing.T) {
	tool := &StrReplaceTool{}
	// The middle window ("   ", "\t") normalizes to two empty lines.
	if _, _, ok := tool.applyFuzzyMatch("x\n   \n\t\ny", " \n ", "INJECTED"); ok {
		t.Fatal("all-blank window must never fuzzy-match")
	}
}

// FOREMAN_STRREPLACE_FUZZY=0 reverts to pre-#942 behavior: the drift case
// that Test...SingleLineTokenDrift recovers must now fail.
func TestStrReplace_FuzzyKillSwitch(t *testing.T) {
	t.Setenv("FOREMAN_STRREPLACE_FUZZY", "0")
	ws := makeWorkspace(t)
	src := "func compute() int {\n\treturn userCount\n}\n"
	seedFile(t, ws, "f.go", src)
	err := execStrReplace(t, ws, "f.go", "\treturn usrCount", "\treturn userCount + 1")
	if err == nil {
		t.Fatal("expected error with fuzzy disabled, got nil")
	}
	if got := readBack(t, ws, "f.go"); got != src {
		t.Errorf("file must be unchanged with fuzzy disabled, got %q", got)
	}
}

// The fuzzy result must report the recovery note so transcripts show the
// harness stepped in (mirrors the whitespace-fallback note contract).
func TestStrReplace_FuzzyReportsNote(t *testing.T) {
	ws := makeWorkspace(t)
	seedFile(t, ws, "f.go", "func compute() int {\n\treturn userCount\n}\n")
	tool := &StrReplaceTool{Workspace: ws}
	buf, _ := json.Marshal(map[string]string{
		"path": "f.go", "old_string": "\treturn usrCount", "new_string": "\treturn 0",
	})
	res, err := tool.Execute(context.Background(), buf)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out, ok := res.Output.(map[string]any)
	if !ok {
		t.Fatalf("unexpected output type %T", res.Output)
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "fuzzy") {
		t.Errorf("expected fuzzy recovery note, got %q", note)
	}
}
