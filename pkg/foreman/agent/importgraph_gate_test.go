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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// importGraphFakeRunner builds a commandRunner for import-graph tests. It is
// keyed by the space-joined argv string and returns the mapped value. Any
// unrecognised command returns ("", nil) (fail-open behaviour).
func importGraphFakeRunner(responses map[string]string, errKeys map[string]bool) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if errKeys[key] {
			return "", errors.New("simulated git error")
		}
		out, ok := responses[key]
		if !ok {
			return "", nil
		}
		return out, nil
	}
}

// changedFilesStatus returns a git status -z output string for the given
// workspace-relative file paths, formatted as changedNonTestGoFiles expects.
func changedFilesStatus(paths ...string) string {
	var entries []string
	for _, p := range paths {
		entries = append(entries, " M "+p)
	}
	return strings.Join(entries, "\x00")
}

// writeGoFile creates intermediate directories and writes Go source content under dir.
func writeGoFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", full, err)
	}
}

// TestCheckImportGraph_NewCLIToControllerEdgeFails verifies that adding a new
// import from pkg/cli to internal/controller is detected and reported.
func TestCheckImportGraph_NewCLIToControllerEdgeFails(t *testing.T) {
	dir := t.TempDir()

	// Disk (working tree): the coder added the internal/controller import.
	diskSrc := "package cli\n\nimport \"github.com/defilantech/llmkube/internal/controller\"\n\nvar _ = controller.ModelCacheKey\n"
	writeGoFile(t, dir, "pkg/cli/cache.go", diskSrc)

	// HEAD: no such import existed before.
	headSrc := "package cli\n\nfunc computeCacheKey(s string) string { return s }\n"

	run := importGraphFakeRunner(map[string]string{
		"git status -z":                  changedFilesStatus("pkg/cli/cache.go"),
		"git show HEAD:pkg/cli/cache.go": headSrc,
	}, nil)

	failed, out := checkImportGraph(context.Background(), dir, run)
	if !failed || !strings.Contains(out, "pkg/cli") || !strings.Contains(out, "internal/controller") {
		t.Fatalf("want failure for pkg/cli->internal/controller, got failed=%v out=%q", failed, out)
	}
}

// TestCheckImportGraph_PreexistingEdgeDoesNotFail verifies that an import edge
// already present on HEAD is not reported as a new violation.
func TestCheckImportGraph_PreexistingEdgeDoesNotFail(t *testing.T) {
	dir := t.TempDir()

	src := "package cli\n\nimport \"github.com/defilantech/llmkube/internal/controller\"\n"
	writeGoFile(t, dir, "pkg/cli/cache.go", src)

	run := importGraphFakeRunner(map[string]string{
		"git status -z":                  changedFilesStatus("pkg/cli/cache.go"),
		"git show HEAD:pkg/cli/cache.go": src, // edge already present on HEAD
	}, nil)

	failed, _ := checkImportGraph(context.Background(), dir, run)
	if failed {
		t.Fatal("a pre-existing edge must not be reported as a new violation")
	}
}

// TestCheckImportGraph_AllowedEdgeDoesNotFail verifies that a pkg/ file newly
// importing another pkg/ (not internal/) package is allowed.
func TestCheckImportGraph_AllowedEdgeDoesNotFail(t *testing.T) {
	dir := t.TempDir()

	diskSrc := "package cli\n\nimport \"github.com/defilantech/llmkube/pkg/foreman/agent\"\n"
	writeGoFile(t, dir, "pkg/cli/cache.go", diskSrc)

	run := importGraphFakeRunner(map[string]string{
		"git status -z":                  changedFilesStatus("pkg/cli/cache.go"),
		"git show HEAD:pkg/cli/cache.go": "package cli\n",
	}, nil)

	failed, _ := checkImportGraph(context.Background(), dir, run)
	if failed {
		t.Fatal("pkg->pkg import must be allowed")
	}
}

// TestCheckImportGraph_NewFileWithForbiddenEdgeFails verifies that a brand-new
// file (no HEAD version) under pkg/cli/ importing internal/controller fails.
// This proves that new files — where all imports are by definition new — are
// correctly handled: "git show HEAD:<path>" returning an error yields an empty
// HEAD import set, so every current import is treated as new.
func TestCheckImportGraph_NewFileWithForbiddenEdgeFails(t *testing.T) {
	dir := t.TempDir()

	diskSrc := "package cli\n\nimport \"github.com/defilantech/llmkube/internal/controller\"\n\nfunc newHelper() {}\n"
	writeGoFile(t, dir, "pkg/cli/newfile.go", diskSrc)

	// git status -z reports the new file (untracked / added); changedNonTestGoFiles
	// accepts any non-empty XY prefix, so "??" works.
	statusOut := "?? pkg/cli/newfile.go"

	run := importGraphFakeRunner(map[string]string{
		"git status -z": statusOut,
		// No "git show HEAD:pkg/cli/newfile.go" entry — the errKeys map will
		// return an error, signalling "file does not exist on HEAD".
	}, map[string]bool{
		"git show HEAD:pkg/cli/newfile.go": true,
	})

	failed, out := checkImportGraph(context.Background(), dir, run)
	if !failed || !strings.Contains(out, "pkg/cli") || !strings.Contains(out, "internal/controller") {
		t.Fatalf("want failure for new file pkg/cli->internal/controller, got failed=%v out=%q", failed, out)
	}
}
