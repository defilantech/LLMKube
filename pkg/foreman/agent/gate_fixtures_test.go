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

// gate_fixtures_test.go: integration-level regression tests proving each gate
// check fires on its target defect and does NOT fire on a known-good
// counterpart. These tests shell real git/grep binaries (via execCommandRunner)
// and materialize real temp git repos rather than using mocked runners, so they
// exercise the true git status -z / git show HEAD: / disk-read paths.
//
// Coverage:
//   - rbac-use (#904)        -- controller file missing +kubebuilder:rbac marker
//   - import-graph (#921)    -- pkg/ file newly importing internal/controller
//   - embedded-artifact (#478) -- markdown with broken YAML fenced block
//   - grounding-breadth (#478) -- markdown citing a hallucinated exporter metric
//   - caller-impact (#907)   -- modified shared function has external callers
//   - issue-example (#809)   -- issue body containing a fenced example block
//   - test-presence (#904)   -- new controller function with no referencing test

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureFile describes one file in a materializeFixture call.
type fixtureFile struct {
	// relPath is the workspace-relative path (forward-slash separators).
	relPath string
	// base is the content committed to HEAD. Empty string means the file
	// does not exist at HEAD (new/added file); when base is empty the file
	// is not written before the initial commit.
	base string
	// current is the working-tree content written AFTER the initial commit
	// (uncommitted). Empty string means the file is deleted / not present
	// in the working tree; when current is empty the file is removed if it
	// existed in the base.
	current string
}

// materializeFixture sets up a real git repository in a temp directory:
//  1. git init + set dummy user config so commits work.
//  2. Write base content for each file with a non-empty base field.
//  3. git add -A && git commit (HEAD = base state).
//  4. Write/overwrite the current content for each file (DO NOT commit).
//
// Returns the workspace directory and a commandRunner backed by
// execCommandRunner pointing at that directory.
func materializeFixture(t *testing.T, files []fixtureFile) (workspace string, run commandRunner) {
	t.Helper()

	// Verify git is available; skip the test if not.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH; skipping integration fixture test")
	}

	dir := t.TempDir()

	// Helper: run git in dir, fail the test on error.
	gitCmd := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	// Helper: write a file, creating parent dirs as needed.
	writeF := func(relPath, content string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", full, err)
		}
	}

	// Step 1: git init with dummy user config.
	gitCmd("init")
	gitCmd("config", "user.email", "gate-fixtures@test.llmkube")
	gitCmd("config", "user.name", "Gate Fixtures Test")

	// Step 2: write base files and commit them.
	hasBase := false
	for _, f := range files {
		if f.base != "" {
			writeF(f.relPath, f.base)
			hasBase = true
		}
	}
	if hasBase {
		gitCmd("add", "-A")
		gitCmd("commit", "-m", "base")
	} else {
		// Git requires at least one commit so HEAD is valid and git status -z
		// works correctly. Commit an empty placeholder.
		writeF(".gitkeep", "")
		gitCmd("add", "-A")
		gitCmd("commit", "-m", "init")
		// Remove the placeholder so it does not interfere with the checks.
		_ = os.Remove(filepath.Join(dir, ".gitkeep"))
	}

	// Step 3: write (or remove) working-tree files -- DO NOT commit.
	for _, f := range files {
		if f.current != "" {
			writeF(f.relPath, f.current)
		} else if f.base != "" {
			// current is empty and base existed: delete from working tree.
			_ = os.Remove(filepath.Join(dir, filepath.FromSlash(f.relPath)))
		}
		// If both base and current are empty: file never existed; nothing to do.
	}

	// Run `git add -N .` (intent-to-add) so that newly written files in new
	// subdirectories appear in `git status -z` as individual file paths rather
	// than as directory-level untracked entries (e.g. "?? internal/" instead of
	// "?? internal/controller/x.go"). Intent-to-add stages the paths without
	// capturing their content, so the working tree content remains the source of
	// truth for all checks. Errors are intentionally ignored: if git add -N
	// fails (e.g. a file was removed), git status -z will still report it.
	{
		cmd := exec.Command("git", "add", "-N", ".")
		cmd.Dir = dir
		_ = cmd.Run()
	}

	// Return execCommandRunner bound to this workspace.
	run = func(ctx context.Context, _ string, extraEnv []string, name string, args ...string) (string, error) {
		// The production runner accepts a dir argument; for fixture tests we
		// always run in the workspace dir, so we ignore the passed dir and
		// use the fixture dir directly.
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), extraEnv...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	return dir, run
}

// ---------------------------------------------------------------------------
// TestGateFixtures_CatchKnownDefects: each case must FIRE (failed == true).
// ---------------------------------------------------------------------------

func TestGateFixtures_CatchKnownDefects(t *testing.T) {
	t.Run("rbac-use/#904: controller uses r.Create without marker", func(t *testing.T) {
		// Base: empty (new file scenario).
		// Current: a controller file that calls r.Create(&batchv1.Job{}) but has
		// NO +kubebuilder:rbac marker covering batch/jobs/create.
		controllerSrc := `package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
)

func (r *R) Reconcile(ctx context.Context) error {
	return r.Create(ctx, &batchv1.Job{})
}
`
		ws, run := materializeFixture(t, []fixtureFile{
			{relPath: "internal/controller/x.go", base: "", current: controllerSrc},
		})

		failed, out := checkRBACUse(context.Background(), ws, run)
		if !failed {
			t.Fatalf("rbac-use: expected failure for missing batch/jobs/create marker, got passed; out=%q", out)
		}
		if !strings.Contains(out, "batch") || !strings.Contains(out, "jobs") || !strings.Contains(out, "create") {
			t.Errorf("rbac-use: output should mention batch/jobs/create; got: %s", out)
		}
	})

	t.Run("import-graph/#921: pkg/ file newly imports internal/controller", func(t *testing.T) {
		// Base: pkg/cli/cache.go with no internal import.
		// Current: same file now importing internal/controller.
		baseSrc := `package cli

func computeCacheKey(s string) string { return s }
`
		currentSrc := `package cli

import "github.com/defilantech/llmkube/internal/controller"

var _ = controller.ModelCacheKey

func computeCacheKey(s string) string { return s }
`
		ws, run := materializeFixture(t, []fixtureFile{
			{relPath: "pkg/cli/cache.go", base: baseSrc, current: currentSrc},
		})

		failed, out := checkImportGraph(context.Background(), ws, run)
		if !failed {
			t.Fatalf("import-graph: expected failure for pkg/->internal/ edge, got passed; out=%q", out)
		}
		if !strings.Contains(out, "pkg/cli") || !strings.Contains(out, "internal/controller") {
			t.Errorf("import-graph: output should mention pkg/cli and internal/controller; got: %s", out)
		}
	})

	t.Run("embedded-artifact/#478: markdown with broken YAML block", func(t *testing.T) {
		// Current: docs/x.md with a fenced yaml block containing a broken brace.
		mdContent := "# Guide\n\nSome text.\n\n```yaml\nfoo: {bar\n```\n\nMore text.\n"
		ws, run := materializeFixture(t, []fixtureFile{
			{relPath: "docs/x.md", base: "", current: mdContent},
		})

		failed, out := checkEmbeddedArtifacts(context.Background(), ws, run)
		if !failed {
			t.Fatalf("embedded-artifact: expected failure for broken YAML block, got passed; out=%q", out)
		}
		if !strings.Contains(out, "docs/x.md") {
			t.Errorf("embedded-artifact: output should name the file; got: %s", out)
		}
	})

	t.Run("grounding-breadth/#478: markdown citing hallucinated exporter metric", func(t *testing.T) {
		// Current: docs/observability.md that cites dcgm_gpu_utilization.
		// checkGroundingBreadth -> grounding.AddedLines runs:
		//   git add -A -- *.md *.yaml *.yml   (best-effort staging)
		//   git diff --cached HEAD -- *.md *.yaml *.yml
		// When *.yaml / *.yml match no files, `git add -A` exits 128 and the
		// staging step silently aborts, leaving the intent-to-add entry with empty
		// content, so the cached diff is empty. Include a minimal YAML stub so
		// every pathspec matches at least one file and staging succeeds.
		mdContent := "# Observability\n\nScrape dcgm_gpu_utilization from the DCGM exporter for GPU utilization.\n"
		yamlStub := "# placeholder\nkind: stub\n"
		ws, run := materializeFixture(t, []fixtureFile{
			{relPath: "docs/observability.md", base: "", current: mdContent},
			// AddedLines calls `git add -A -- *.md *.yaml *.yml`: all three
			// pathspecs must match at least one file or git exits 128 and aborts
			// the staging step, leaving the cached diff empty.
			{relPath: "docs/stub.yaml", base: "", current: yamlStub},
			{relPath: "docs/stub.yml", base: "", current: yamlStub},
		})

		failed, out := checkGroundingBreadth(context.Background(), ws, run)
		if !failed {
			t.Fatalf("grounding-breadth: expected failure for dcgm_gpu_utilization, got passed; out=%q", out)
		}
		if !strings.Contains(out, "dcgm_gpu_utilization") {
			t.Errorf("grounding-breadth: output should mention dcgm_gpu_utilization; got: %s", out)
		}
	})

	t.Run("caller-impact/#907: modified shared function has external callers", func(t *testing.T) {
		// Base: pkg/x/a.go defines Shared() with original body.
		//       pkg/y/b.go calls Shared().
		// Current: pkg/x/a.go modifies Shared() body -- git diff -U0 will show
		//          a hunk inside Shared. The grep over the working tree finds the
		//          caller in pkg/y/b.go (a different file = external caller).
		baseShared := `package x

// Shared does the original thing.
func Shared() string {
	return "original"
}
`
		currentShared := `package x

// Shared does the updated thing.
func Shared() string {
	return "updated"
}
`
		callerSrc := `package y

import "github.com/defilantech/llmkube/pkg/x"

func Use() string {
	return x.Shared()
}
`
		ws, run := materializeFixture(t, []fixtureFile{
			{relPath: "pkg/x/a.go", base: baseShared, current: currentShared},
			{relPath: "pkg/y/b.go", base: callerSrc, current: callerSrc},
		})

		failed, out := checkCallerImpact(context.Background(), ws, run)
		if !failed {
			t.Fatalf("caller-impact: expected advisory for external caller of Shared, got passed; out=%q", out)
		}
		if !strings.Contains(out, "Shared") {
			t.Errorf("caller-impact: output should mention Shared; got: %s", out)
		}
		if !strings.Contains(out, "b.go") {
			t.Errorf("caller-impact: output should mention b.go (the external caller file); got: %s", out)
		}
	})

	t.Run("issue-example/#809: issue body contains fenced example block", func(t *testing.T) {
		// checkIssueExample is purely text-based: no workspace / git needed.
		issueBody := "## Repro\n```\ndo the thing\n```\n"
		fn := checkIssueExample(issueBody)
		failed, out := fn(context.Background(), "/unused", noopRunner)
		if !failed {
			t.Fatalf("issue-example: expected advisory for fenced block near 'Repro', got passed; out=%q", out)
		}
		if !strings.Contains(out, "do the thing") {
			t.Errorf("issue-example: output should include the example; got: %s", out)
		}
	})

	t.Run("test-presence/#904: new controller function with no referencing test", func(t *testing.T) {
		// Current: internal/controller/x.go adds func NewThing() with no _test.go
		// referencing it. checkTestPresence uses git diff to find added func names.
		controllerSrc := `package controller

func NewThing() string {
	return "new"
}
`
		ws, run := materializeFixture(t, []fixtureFile{
			{relPath: "internal/controller/x.go", base: "", current: controllerSrc},
		})

		failed, out := checkTestPresence(context.Background(), ws, run)
		if !failed {
			t.Fatalf("test-presence: expected failure for new func NewThing with no test, got passed; out=%q", out)
		}
		if !strings.Contains(out, "NewThing") {
			t.Errorf("test-presence: output should mention NewThing; got: %s", out)
		}
	})
}

// ---------------------------------------------------------------------------
// TestGateFixtures_NoFalsePositives: known-good counterparts must NOT fire.
// ---------------------------------------------------------------------------

func TestGateFixtures_NoFalsePositives(t *testing.T) {
	t.Run("rbac-use: correct marker present -> no fire", func(t *testing.T) {
		// Same controller call but WITH the correct +kubebuilder:rbac marker.
		controllerSrc := `package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
)

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create

func (r *R) Reconcile(ctx context.Context) error {
	return r.Create(ctx, &batchv1.Job{})
}
`
		ws, run := materializeFixture(t, []fixtureFile{
			{relPath: "internal/controller/x.go", base: "", current: controllerSrc},
		})

		failed, out := checkRBACUse(context.Background(), ws, run)
		if failed {
			t.Errorf("rbac-use: correct marker present should not fire; got: %s", out)
		}
	})

	t.Run("import-graph: pkg/ adding allowed pkg/ import -> no fire", func(t *testing.T) {
		// pkg/cli/cache.go newly imports another pkg/ package (not internal/).
		baseSrc := "package cli\n\nfunc computeCacheKey(s string) string { return s }\n"
		currentSrc := `package cli

import "github.com/defilantech/llmkube/pkg/foreman/agent/repo"

var _ = repo.DiffNameOnly

func computeCacheKey(s string) string { return s }
`
		ws, run := materializeFixture(t, []fixtureFile{
			{relPath: "pkg/cli/cache.go", base: baseSrc, current: currentSrc},
		})

		failed, out := checkImportGraph(context.Background(), ws, run)
		if failed {
			t.Errorf("import-graph: pkg->pkg import should be allowed; got: %s", out)
		}
	})

	t.Run("embedded-artifact: valid YAML block -> no fire", func(t *testing.T) {
		// docs/x.md with a syntactically valid yaml block.
		mdContent := "# Guide\n\n```yaml\nfoo: bar\nlist:\n  - a\n  - b\n```\n"
		ws, run := materializeFixture(t, []fixtureFile{
			{relPath: "docs/x.md", base: "", current: mdContent},
		})

		failed, out := checkEmbeddedArtifacts(context.Background(), ws, run)
		if failed {
			t.Errorf("embedded-artifact: valid YAML should not fire; got: %s", out)
		}
	})

	t.Run("issue-example: no fenced block in issue -> no fire", func(t *testing.T) {
		fn := checkIssueExample("This is a bug. No code fences here. Please fix it.")
		failed, _ := fn(context.Background(), "/unused", noopRunner)
		if failed {
			t.Error("issue-example: issue with no example should not fire")
		}
	})

	t.Run("test-presence: new function covered by a changed test -> no fire", func(t *testing.T) {
		// pkg/x/a.go adds NewHelper(); pkg/x/a_test.go references NewHelper.
		// checkTestPresence: uses pkg/ dir (not internal/controller/) so git diff
		// should show a new func line.
		newFunc := `package x

func NewHelper() string {
	return "help"
}
`
		testFile := `package x

import "testing"

func TestNewHelper(t *testing.T) {
	if got := NewHelper(); got != "help" {
		t.Errorf("got %q", got)
	}
}
`
		ws, run := materializeFixture(t, []fixtureFile{
			{relPath: "pkg/x/a.go", base: "", current: newFunc},
			{relPath: "pkg/x/a_test.go", base: "", current: testFile},
		})

		failed, out := checkTestPresence(context.Background(), ws, run)
		if failed {
			t.Errorf("test-presence: function covered by changed test should not fire; got: %s", out)
		}
	})

	t.Run("caller-impact: body-modified function with no external callers -> no fire", func(t *testing.T) {
		// Base: pkg/x/a.go defines Solo() which is only used within the same file.
		// Current: body of Solo() changes ("a" -> "b"). modifiedFuncNames will pick
		// up Solo; externalCallers will grep the workspace and find only the
		// definition line in pkg/x/a.go (no cross-file caller) -> no advisory.
		baseSrc := `package x

// Solo does the original thing.
func Solo() string {
	return "a"
}
`
		currentSrc := `package x

// Solo does the updated thing.
func Solo() string {
	return "b"
}
`
		ws, run := materializeFixture(t, []fixtureFile{
			{relPath: "pkg/x/a.go", base: baseSrc, current: currentSrc},
		})

		failed, out := checkCallerImpact(context.Background(), ws, run)
		if failed {
			t.Errorf("caller-impact: no external callers of Solo should not fire; got: %s", out)
		}
	})

	t.Run("grounding-breadth: doc token grounded by exporter prefix -> no fire", func(t *testing.T) {
		// Current: docs/x.md cites node_memory_working_set_bytes. The token starts
		// with "node_" which is in ExporterMetricPrefixes, so checkGroundingBreadth
		// must NOT flag it. Include yaml/yml stubs so `git add -A -- *.md *.yaml
		// *.yml` in AddedLines finds at least one file per pathspec and staging does
		// not abort with exit 128 (leaving the cached diff empty).
		mdContent := "# Metrics\n\nWatch node_memory_working_set_bytes to track container RSS on each node.\n"
		yamlStub := "# placeholder\nkind: stub\n"
		ws, run := materializeFixture(t, []fixtureFile{
			{relPath: "docs/x.md", base: "", current: mdContent},
			{relPath: "docs/stub.yaml", base: "", current: yamlStub},
			{relPath: "docs/stub.yml", base: "", current: yamlStub},
		})

		failed, out := checkGroundingBreadth(context.Background(), ws, run)
		if failed {
			t.Errorf(
				"grounding-breadth: node_memory_working_set_bytes is grounded by node_ prefix; "+
					"should not fire; got: %s", out)
		}
	})
}
