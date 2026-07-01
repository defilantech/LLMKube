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
	"strings"
	"testing"
)

// fakeRunnerFor builds a commandRunner keyed by the space-joined argv string.
// The git status -z key must encode the changed files as " M <path>" entries
// separated by NUL bytes, matching changedNonTestGoFiles' expected output.
// Any command whose joined argv is not in the map returns ("", nil).
func fakeRunnerFor(responses map[string]string) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
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

func TestCheckImportGraph_NewCLIToControllerEdgeFails(t *testing.T) {
	run := fakeRunnerFor(map[string]string{
		"git status -z":                  changedFilesStatus("pkg/cli/cache.go"),
		"git show :pkg/cli/cache.go":     "package cli\n\nimport \"github.com/defilantech/llmkube/internal/controller\"\n\nvar _ = controller.ModelCacheKey\n",
		"git show HEAD:pkg/cli/cache.go": "package cli\n\nfunc computeCacheKey(s string) string { return s }\n",
	})
	failed, out := checkImportGraph(context.Background(), "/ws", run)
	if !failed || !strings.Contains(out, "pkg/cli") || !strings.Contains(out, "internal/controller") {
		t.Fatalf("want failure for pkg/cli->internal/controller, got failed=%v out=%q", failed, out)
	}
}

func TestCheckImportGraph_PreexistingEdgeDoesNotFail(t *testing.T) {
	src := "package cli\n\nimport \"github.com/defilantech/llmkube/internal/controller\"\n"
	run := fakeRunnerFor(map[string]string{
		"git status -z":                  changedFilesStatus("pkg/cli/cache.go"),
		"git show :pkg/cli/cache.go":     src,
		"git show HEAD:pkg/cli/cache.go": src, // edge already present on HEAD
	})
	failed, _ := checkImportGraph(context.Background(), "/ws", run)
	if failed {
		t.Fatal("a pre-existing edge is not a new violation")
	}
}

func TestCheckImportGraph_AllowedEdgeDoesNotFail(t *testing.T) {
	// pkg importing another pkg (not internal) -> allowed.
	run := fakeRunnerFor(map[string]string{
		"git status -z":                  changedFilesStatus("pkg/cli/cache.go"),
		"git show :pkg/cli/cache.go":     "package cli\n\nimport \"github.com/defilantech/llmkube/pkg/foreman/agent\"\n",
		"git show HEAD:pkg/cli/cache.go": "package cli\n",
	})
	failed, _ := checkImportGraph(context.Background(), "/ws", run)
	if failed {
		t.Fatal("pkg->pkg import is allowed")
	}
}
