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

	"github.com/go-logr/logr"
)

// repomapMarker and ripwireMarker are the header lines each backend's renderer
// emits, so a test can tell which backend produced a summary.
const (
	repomapMarker = "weighted by relevance to the task"
	ripwireMarker = "Ranked by the ripwire call graph." //nolint:gosec // a header string, not a credential
)

func seedCoderWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	p := filepath.Join(root, "internal", "processor", "processor.go")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `// Package processor contains the core processing pipeline.
package processor

// Processor is the main worker.
type Processor struct{}

// Process runs one unit of work.
func (p *Processor) Process(input string) (string, error) { return input, nil }
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return root
}

// TestRenderRipwire_RealFixture renders bytes ripwire actually produced (a
// committed fixture captured from a real run), not a double built from our
// own types, so a rename of ripwire's wire keys fails here.
func TestRenderRipwire_RealFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "ripwire_sigs.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	out, err := renderRipwire(raw)
	if err != nil {
		t.Fatalf("renderRipwire: %v", err)
	}
	want := []string{
		"## Repository overview",
		"### internal/processor/processor.go",
		"- type Processor struct",
		"- func (p *Processor) Process(input string) (string, error)",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("rendered summary missing %q, got:\n%s", w, out)
		}
	}
}

// TestRepoMapSummary_DefaultIsRepomap pins the default: with no opt-in env,
// the summary is repomap's, byte-for-byte the pre-change behavior.
func TestRepoMapSummary_DefaultIsRepomap(t *testing.T) {
	ws := seedCoderWorkspace(t)
	got := repoMapSummary(context.Background(), ws, "processor suffix bug", logr.Discard())
	if !strings.Contains(got, repomapMarker) {
		t.Errorf("default backend should be repomap; got:\n%s", got)
	}
	if strings.Contains(got, ripwireMarker) {
		t.Errorf("default backend must not be ripwire; got:\n%s", got)
	}
}

// TestRepoMapSummary_UsesRipwireWhenEnabled drives the opt-in path with the
// real fixture: a fake ripwire on the configured path emits the committed
// bytes, and the summary must be ripwire's rendering.
func TestRepoMapSummary_UsesRipwireWhenEnabled(t *testing.T) {
	ws := seedCoderWorkspace(t)
	fake := filepath.Join(t.TempDir(), "ripwire")
	script := "#!/bin/sh\ncat " + filepath.Join("testdata", "ripwire_sigs.json") + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ripwire: %v", err)
	}
	t.Setenv(ripwireBackendEnv, "ripwire")
	t.Setenv(ripwireBinEnv, fake)

	got := repoMapSummary(context.Background(), ws, "processor suffix bug", logr.Discard())
	if !strings.Contains(got, ripwireMarker) {
		t.Errorf("enabled backend should be ripwire; got:\n%s", got)
	}
	if !strings.Contains(got, "### internal/processor/processor.go") {
		t.Errorf("ripwire summary lost the file header; got:\n%s", got)
	}
}

// TestRepoMapSummary_FallsBackWhenRipwireAbsent is the fail-open contract: a
// coder that cannot reach ripwire still gets a repo-map, never a bare prompt.
func TestRepoMapSummary_FallsBackWhenRipwireAbsent(t *testing.T) {
	ws := seedCoderWorkspace(t)
	t.Setenv(ripwireBackendEnv, "ripwire")
	t.Setenv(ripwireBinEnv, filepath.Join(t.TempDir(), "does-not-exist"))

	got := repoMapSummary(context.Background(), ws, "processor suffix bug", logr.Discard())
	if !strings.Contains(got, repomapMarker) {
		t.Errorf("absent ripwire must fall through to repomap; got:\n%s", got)
	}
}
