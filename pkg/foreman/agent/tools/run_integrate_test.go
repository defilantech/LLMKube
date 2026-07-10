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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// integrateFixture builds a bare "origin" repo with a base commit on main and
// the given slice branches, and clones it into a workspace with a git identity.
// Returns (originPath, workspacePath).
func integrateFixture(t *testing.T, slices map[string]map[string]string) (string, string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", origin)

	// seed: base commit on main.
	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", origin, seed)
	git(t, seed, "config", "user.email", "t@e.com")
	git(t, seed, "config", "user.name", "T")
	git(t, seed, "config", "commit.gpgsign", "false")
	writeFile(t, seed, "base.txt", "base\n")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "base")
	git(t, seed, "push", "-q", "origin", "main")

	// each slice branch off main.
	for name, files := range slices {
		git(t, seed, "checkout", "-q", "-B", name, "main")
		for p, c := range files {
			writeFile(t, seed, p, c)
		}
		git(t, seed, "add", "-A")
		git(t, seed, "commit", "-qm", name)
		git(t, seed, "push", "-q", "origin", name)
		git(t, seed, "checkout", "-q", "main")
	}

	// the executor's clone (origin = the bare repo).
	ws := filepath.Join(root, "ws")
	git(t, root, "clone", "-q", origin, ws)
	git(t, ws, "config", "user.email", "t@e.com")
	git(t, ws, "config", "user.name", "T")
	git(t, ws, "config", "commit.gpgsign", "false")
	return origin, ws
}

func TestRunIntegrate_CleanUnionPushed(t *testing.T) {
	origin, ws := integrateFixture(t, map[string]map[string]string{
		"foreman/s/a": {"a/new.txt": "AAA\n"},
		"foreman/s/b": {"b/new.txt": "BBB\n"},
	})
	tool := &RunIntegrateTool{Workspace: ws}
	args, _ := json.Marshal(map[string]any{
		"branch":      "foreman/s/integ",
		"baseBranch":  "main",
		"upstreamURL": origin, // base fetched from here; slices from origin remote
		"slices": []map[string]any{
			{"name": "a", "branch": "foreman/s/a"},
			{"name": "b", "branch": "foreman/s/b"},
		},
	})
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != VerdictGatePass {
		t.Fatalf("verdict = %q (%s), want GATE-PASS", res.Verdict, res.Summary)
	}
	// the integration branch was pushed to origin and carries both slices.
	git(t, ws, "fetch", "-q", "origin", "foreman/s/integ")
	names := git(t, ws, "ls-tree", "-r", "--name-only", "FETCH_HEAD")
	if !strings.Contains(names, "a/new.txt") || !strings.Contains(names, "b/new.txt") {
		t.Fatalf("integration branch missing a slice's file:\n%s", names)
	}
}

func TestRunIntegrate_OverlapIsGateFail(t *testing.T) {
	origin, ws := integrateFixture(t, map[string]map[string]string{
		"foreman/s/a": {"shared.txt": "from-a\n"},
		"foreman/s/b": {"shared.txt": "from-b\n"},
	})
	tool := &RunIntegrateTool{Workspace: ws}
	args, _ := json.Marshal(map[string]any{
		"branch":      "foreman/s/integ",
		"baseBranch":  "main",
		"upstreamURL": origin,
		"slices": []map[string]any{
			{"name": "a", "branch": "foreman/s/a"},
			{"name": "b", "branch": "foreman/s/b"},
		},
	})
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != VerdictGateFail {
		t.Fatalf("verdict = %q (%s), want GATE-FAIL for overlapping slices", res.Verdict, res.Summary)
	}
}

func TestRunIntegrate_BadArgsIsGateError(t *testing.T) {
	tool := &RunIntegrateTool{Workspace: t.TempDir()}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"branch":"x"}`)) // no slices
	if res.Verdict != VerdictGateError {
		t.Fatalf("verdict = %q, want GATE-ERROR for missing slices", res.Verdict)
	}
}

// A branch/ref that begins with '-' (or an option-looking URL) must be rejected
// before it reaches git argv, closing the flag-smuggling vector.
func TestRunIntegrate_ArgvInjectionRejected(t *testing.T) {
	tool := &RunIntegrateTool{Workspace: t.TempDir()}
	cases := []string{
		`{"branch":"--upload-pack=touch /tmp/pwn","slices":[{"name":"a","branch":"foreman/s/a"}]}`,
		`{"branch":"integ","slices":[{"name":"a","branch":"--output=/etc/x"}]}`,
		`{"branch":"integ","baseBranch":"-x","slices":[{"name":"a","branch":"foreman/s/a"}]}`,
		`{"branch":"integ","upstreamURL":"--upload-pack=x","slices":[{"name":"a","branch":"foreman/s/a"}]}`,
	}
	for _, c := range cases {
		res, _ := tool.Execute(context.Background(), json.RawMessage(c))
		if res.Verdict != VerdictGateError {
			t.Fatalf("verdict = %q for %s, want GATE-ERROR", res.Verdict, c)
		}
	}
}
