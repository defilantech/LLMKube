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

// fakeRipwire writes an executable that ignores ripwire semantics and prints
// its argv, one element per line, so a test can inspect exactly what the tool
// handed the binary.
func fakeRipwire(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ripwire")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\n"), 0o755); err != nil {
		t.Fatalf("write fake ripwire: %v", err)
	}
	t.Setenv("FOREMAN_RIPWIRE_BIN", p)
}

func runRipwire(t *testing.T, verb, query string) (*string, error) {
	t.Helper()
	tool := &RipwireTool{Workspace: "/workspace"}
	args, _ := json.Marshal(map[string]string{"verb": verb, "query": query})
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		return nil, err
	}
	m, _ := res.Output.(map[string]any)
	s, _ := m["result"].(string)
	return &s, nil
}

// TestRipwireTool_VerbBuildsFixedArgv pins the argument contract: workspace,
// the verb's fixed flag prefix with the query appended, then --json. The model
// supplies no flag of its own.
func TestRipwireTool_VerbBuildsFixedArgv(t *testing.T) {
	fakeRipwire(t)
	got, err := runRipwire(t, "impact", "Reconcile")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(*got), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 argv elements, got %d: %q", len(lines), *got)
	}
	if lines[0] != "/workspace" || lines[1] != "--impact=Reconcile" || lines[2] != "--json" {
		t.Errorf("argv mismatch: %q", lines)
	}
}

// TestRipwireTool_UnknownVerbRejected is the security gate: a verb outside the
// fixed set never reaches the binary.
func TestRipwireTool_UnknownVerbRejected(t *testing.T) {
	fakeRipwire(t)
	if _, err := runRipwire(t, "apply", "x"); err == nil {
		t.Fatal("unknown verb must be rejected, got nil error")
	}
}

// TestRipwireTool_QueryCannotInjectFlag drives the injection case: a query
// that looks like a flag stays inside the single --for= element, so it cannot
// become a second flag ripwire would act on.
func TestRipwireTool_QueryCannotInjectFlag(t *testing.T) {
	fakeRipwire(t)
	got, err := runRipwire(t, "for", "fix the bug --apply")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(*got), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 argv elements, got %d: %q", len(lines), *got)
	}
	if lines[1] != "--for=fix the bug --apply" {
		t.Errorf("query must stay inside the --for= element, got %q", lines[1])
	}
	for _, l := range lines {
		if l == "--apply" {
			t.Errorf("model query became a standalone flag: %q", lines)
		}
	}
}

// TestRipwireTool_OutputCapped bounds a large graph so it cannot dominate the
// transcript.
func TestRipwireTool_OutputCapped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ripwire")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nhead -c 100000 /dev/zero | tr '\\0' 'a'\n"), 0o755); err != nil {
		t.Fatalf("write fake ripwire: %v", err)
	}
	t.Setenv("FOREMAN_RIPWIRE_BIN", p)

	tool := &RipwireTool{Workspace: "/workspace", MaxBytes: 1024}
	args, _ := json.Marshal(map[string]string{"verb": "for", "query": "x"})
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	m, _ := res.Output.(map[string]any)
	if m["truncated"] != true {
		t.Errorf("expected truncated=true for a 100KB result under a 1KB cap")
	}
	got, _ := m["result"].(string)
	// Cap plus the truncation marker and a little slack.
	if len(got) > 1024+64 {
		t.Errorf("capped result too long: %d bytes", len(got))
	}
}
