/*
Copyright 2026.

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

package machoplist

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// These tests enforce that the linux metal-agent release is a static,
// linux-only build of the darwin agent's main, binary and ldflags, packed in
// an archive of its own that no cask installs, and that the GoReleaser check
// workflow runs the release job's goreleaser-action pin and version.

const (
	linuxAgentBuildID    = "llmkube-metal-agent-linux"
	goreleaserCheckFlow  = ".github/workflows/goreleaser-check.yml"
	goreleaserActionUses = "goreleaser/goreleaser-action@"
)

type linuxReleaseConfig struct {
	Builds []struct {
		ID      string   `json:"id"`
		Main    string   `json:"main"`
		Binary  string   `json:"binary"`
		Ldflags []string `json:"ldflags"`
		Goos    []string `json:"goos"`
		Goarch  []string `json:"goarch"`
		Env     []string `json:"env"`
		Hooks   struct {
			Post []any `json:"post"`
		} `json:"hooks"`
	} `json:"builds"`
	Archives []struct {
		ID  string   `json:"id"`
		IDs []string `json:"ids"`
	} `json:"archives"`
	HomebrewCasks []struct {
		Name string   `json:"name"`
		IDs  []string `json:"ids"`
	} `json:"homebrew_casks"`
}

// TestLinuxMetalAgentReleaseIsStaticAndApart pins the linux build to the
// darwin one's main, binary and ldflags, keeps it static and unhooked, and
// keeps its archive out of every cask. A cask with no ids takes every
// archive, so each cask must list its ids.
func TestLinuxMetalAgentReleaseIsStaticAndApart(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg linuxReleaseConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse .goreleaser.yaml: %v", err)
	}

	builds := map[string]int{}
	for i, b := range cfg.Builds {
		builds[b.ID] = i
	}
	di, ok := builds[caskBuildID]
	if !ok {
		t.Fatalf("no build %q in .goreleaser.yaml", caskBuildID)
	}
	li, ok := builds[linuxAgentBuildID]
	if !ok {
		t.Fatalf("no build %q in .goreleaser.yaml", linuxAgentBuildID)
	}
	darwin, linux := cfg.Builds[di], cfg.Builds[li]

	if !slices.Equal(darwin.Goos, []string{"darwin"}) {
		t.Errorf("build %q goos = %q, want only darwin: the cask and the Info.plist hook take every binary it builds", caskBuildID, darwin.Goos)
	}
	if !slices.Equal(linux.Goos, []string{"linux"}) {
		t.Errorf("build %q goos = %q, want only linux", linuxAgentBuildID, linux.Goos)
	}
	for _, arch := range []string{"arm64", "amd64"} {
		if !slices.Contains(linux.Goarch, arch) {
			t.Errorf("build %q goarch = %q, want %s", linuxAgentBuildID, linux.Goarch, arch)
		}
	}
	if !slices.Contains(linux.Env, "CGO_ENABLED=0") {
		t.Errorf("build %q env = %q, want CGO_ENABLED=0: the release and the guide promise a static binary", linuxAgentBuildID, linux.Env)
	}
	got := append([]string{linux.Main, linux.Binary}, linux.Ldflags...)
	want := append([]string{darwin.Main, darwin.Binary}, darwin.Ldflags...)
	if !slices.Equal(got, want) {
		t.Errorf("build %q main, binary and ldflags = %q, want the darwin build's %q", linuxAgentBuildID, got, want)
	}
	if len(linux.Hooks.Post) != 0 {
		t.Errorf("build %q has post hooks %v, want none", linuxAgentBuildID, linux.Hooks.Post)
	}

	var linuxArchives []string
	for _, a := range cfg.Archives {
		if slices.Contains(a.IDs, linuxAgentBuildID) {
			linuxArchives = append(linuxArchives, a.ID)
			if !slices.Equal(a.IDs, []string{linuxAgentBuildID}) {
				t.Errorf("archive %q packs builds %q, want only [%q]", a.ID, a.IDs, linuxAgentBuildID)
			}
		}
	}
	if len(linuxArchives) == 0 {
		t.Errorf("no archive packs build %q", linuxAgentBuildID)
	}
	for _, c := range cfg.HomebrewCasks {
		if len(c.IDs) == 0 {
			t.Errorf("cask %q lists no ids, so it takes every archive, the linux ones %q included", c.Name, linuxArchives)
		}
		for _, id := range linuxArchives {
			if slices.Contains(c.IDs, id) {
				t.Errorf("cask %q installs the linux archive %q", c.Name, id)
			}
		}
	}
}

type workflowFile struct {
	Jobs map[string]struct {
		Steps []struct {
			Uses string            `json:"uses"`
			With map[string]string `json:"with"`
		} `json:"steps"`
	} `json:"jobs"`
}

// goreleaserPins returns the goreleaser-action reference and GoReleaser
// version of every goreleaser-action step in a workflow.
func goreleaserPins(t *testing.T, rel string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflowFile
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	var pins []string
	for _, job := range wf.Jobs {
		for _, s := range job.Steps {
			if strings.HasPrefix(s.Uses, goreleaserActionUses) {
				pins = append(pins, s.Uses+" "+s.With["version"])
			}
		}
	}
	if len(pins) == 0 {
		t.Fatalf("%s has no %s step", rel, goreleaserActionUses)
	}
	return pins
}

// TestGoreleaserCheckMatchesTheReleaseJob keeps the check workflow on the
// action and GoReleaser version the release runs.
func TestGoreleaserCheckMatchesTheReleaseJob(t *testing.T) {
	release := goreleaserPins(t, releaseWorkflow)
	if len(release) != 1 {
		t.Fatalf("%s has %d goreleaser-action steps, want one: %q", releaseWorkflow, len(release), release)
	}
	for _, pin := range goreleaserPins(t, goreleaserCheckFlow) {
		if pin != release[0] {
			t.Errorf("%s runs %q, want the release job's %q", goreleaserCheckFlow, pin, release[0])
		}
	}
}
