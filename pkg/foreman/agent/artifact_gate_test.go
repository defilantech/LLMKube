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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMdRunner returns a commandRunner for artifact_gate tests.
// It handles one command:
//   - git status -z : returns a NUL-delimited listing of changed paths,
//     using the git porcelain status format ("<XY> <path>").
//
// The changedPaths map is keyed by workspace-relative path; the value is the
// two-character XY status code (e.g. " M", "??", "A "). Content is always
// read from disk (os.ReadFile) by the production code, so tests must write
// files under dir before calling checkEmbeddedArtifacts.
func fakeMdRunner(changedPaths map[string]string) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" {
			return "", nil
		}
		// git status -z
		if len(args) == 2 && args[0] == "status" && args[1] == "-z" {
			var entries []string
			for path, xy := range changedPaths {
				entries = append(entries, xy+" "+path)
			}
			return strings.Join(entries, "\x00"), nil
		}
		return "", nil
	}
}

// writeFile creates a file at dir/relPath with the given content,
// creating intermediate directories as needed.
func writeFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestCheckEmbeddedArtifacts_BrokenYAMLFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "docs/site/guides/x.md", "intro text\n\n```yaml\nfoo: {bar\n```\n\nmore text\n")

	run := fakeMdRunner(map[string]string{
		"docs/site/guides/x.md": " M",
	})
	failed, out := checkEmbeddedArtifacts(context.Background(), dir, run)
	if !failed || !strings.Contains(out, "docs/site/guides/x.md") {
		t.Fatalf("want YAML-parse failure naming the file, got failed=%v out=%q", failed, out)
	}
}

func TestCheckEmbeddedArtifacts_ValidYAMLPasses(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "docs/x.md", "```yaml\nfoo: bar\nlist:\n  - a\n  - b\n```\n")

	run := fakeMdRunner(map[string]string{
		"docs/x.md": " M",
	})
	failed, _ := checkEmbeddedArtifacts(context.Background(), dir, run)
	if failed {
		t.Fatalf("valid YAML should pass")
	}
}

func TestCheckEmbeddedArtifacts_NoMarkdownChangesPasses(t *testing.T) {
	dir := t.TempDir()
	run := fakeMdRunner(nil)
	failed, _ := checkEmbeddedArtifacts(context.Background(), dir, run)
	if failed {
		t.Fatal("no changed markdown -> pass")
	}
}

// TestCheckEmbeddedArtifacts_NewUntrackedDocChecked proves that a newly created
// (untracked) .md file with a broken YAML block is caught. git status -z reports
// untracked files with status "??"; the old git-diff-based lister would miss them.
func TestCheckEmbeddedArtifacts_NewUntrackedDocChecked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "docs/new-guide.md", "# New guide\n\n```yaml\nbroken: {unclosed\n```\n")

	run := fakeMdRunner(map[string]string{
		"docs/new-guide.md": "??",
	})
	failed, out := checkEmbeddedArtifacts(context.Background(), dir, run)
	if !failed || !strings.Contains(out, "docs/new-guide.md") {
		t.Fatalf("want YAML-parse failure for new untracked doc, got failed=%v out=%q", failed, out)
	}
}

// TestCheckEmbeddedArtifacts_YamldocFenceNotMatched proves that a ```yamldoc
// fence is NOT treated as a yaml fence. Even though the body contains content
// that would fail YAML parsing, the check must PASS because the info-string
// first token is "yamldoc", not "yaml" or "yml".
func TestCheckEmbeddedArtifacts_YamldocFenceNotMatched(t *testing.T) {
	dir := t.TempDir()
	// {unclosed is invalid YAML; if the fence were matched it would FAIL.
	writeFile(t, dir, "docs/example.md", "Some text.\n\n```yamldoc\n{unclosed\n```\n\nEnd.\n")

	run := fakeMdRunner(map[string]string{
		"docs/example.md": " M",
	})
	failed, out := checkEmbeddedArtifacts(context.Background(), dir, run)
	if failed {
		t.Fatalf("yamldoc fence should NOT be treated as yaml, but got failure: %s", out)
	}
}
