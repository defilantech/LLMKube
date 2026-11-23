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
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// A release archive and a release image redistribute every third-party module
// the binary links, and Apache 2.0 section 4(d) requires a readable copy of
// their NOTICE files. These tests pin the three places that make that true:
// the generator the release runs, the archive file lists, and the image
// Dockerfiles against the paths goreleaser stages into their context (#2564).

// licensesConfig is the slice of .goreleaser.yaml these tests read. It is
// declared here, not shared, so this file parses the same on any tree.
type licensesConfig struct {
	Before struct {
		Hooks []string `json:"hooks"`
	} `json:"before"`
	Builds []struct {
		ID   string   `json:"id"`
		Main string   `json:"main"`
		GOOS []string `json:"goos"`
	} `json:"builds"`
	Archives []struct {
		ID    string   `json:"id"`
		Files []string `json:"files"`
	} `json:"archives"`
	DockersV2 []struct {
		ID         string   `json:"id"`
		Dockerfile string   `json:"dockerfile"`
		ExtraFiles []string `json:"extra_files"`
	} `json:"dockers_v2"`
}

func loadLicensesConfig(t *testing.T) licensesConfig {
	t.Helper()
	var cfg licensesConfig
	raw, err := os.ReadFile(filepath.Join(repoRoot, ".goreleaser.yaml"))
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse .goreleaser.yaml: %v", err)
	}
	return cfg
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// makefileVar returns the value of a Makefile `NAME ?= ...` assignment.
func makefileVar(t *testing.T, makefile, name string) (string, bool) {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` \?= (.*)$`).FindStringSubmatch(makefile)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// TestArchivesShipThirdPartyLicenses pins the generator and every archive's
// file list: a release must carry every linked module's license and NOTICE
// next to its own LICENSE.
func TestArchivesShipThirdPartyLicenses(t *testing.T) {
	cfg := loadLicensesConfig(t)
	if !slices.Contains(cfg.Before.Hooks, "make third-party-licenses") {
		t.Errorf(".goreleaser.yaml before.hooks does not run make third-party-licenses: %v", cfg.Before.Hooks)
	}

	makefile := readRepoFile(t, "Makefile")
	if !regexp.MustCompile(`(?m)^third-party-licenses:`).MatchString(makefile) {
		t.Errorf("Makefile has no third-party-licenses target")
	}

	ids := map[string]bool{}
	for _, a := range cfg.Archives {
		ids[a.ID] = true
		if !slices.Contains(a.Files, "third_party_licenses/**") {
			t.Errorf("archive %s does not ship third_party_licenses/** (files: %v)", a.ID, a.Files)
		}
	}
	for _, id := range []string{"llmkube-cli", "llmkube-metal-agent", "llmkube-metal-agent-linux", "llmkube-foreman-agent"} {
		if !ids[id] {
			t.Errorf("archive %s not in .goreleaser.yaml", id)
		}
	}
}

// TestGoreleaserImagesShipLicenses pins both halves of each image's contract:
// every path dockers_v2 stages into the context is COPYed by the Dockerfile,
// and every non-artifact COPY source is staged there under /licenses/.
func TestGoreleaserImagesShipLicenses(t *testing.T) {
	cfg := loadLicensesConfig(t)
	if len(cfg.DockersV2) == 0 {
		t.Fatal(".goreleaser.yaml has no dockers_v2 entries")
	}
	for _, d := range cfg.DockersV2 {
		t.Run(d.ID, func(t *testing.T) {
			copies := dockerfileCopies(t, d.Dockerfile)
			for _, src := range []string{"LICENSE", "third_party_licenses"} {
				if _, ok := copies[src]; !ok {
					t.Errorf("%s does not COPY %s", d.Dockerfile, src)
				}
			}
			for src, dst := range copies {
				if !strings.HasPrefix(dst, "/licenses/") {
					t.Errorf("%s COPYs %s to %s; want it under /licenses/", d.Dockerfile, src, dst)
				}
			}

			staged := map[string]bool{}
			for _, f := range d.ExtraFiles {
				staged[f] = true
			}
			for _, src := range []string{"LICENSE", "third_party_licenses"} {
				if !staged[src] {
					t.Errorf("%s does not stage %s in extra_files", d.Dockerfile, src)
				}
			}
			for src := range copies {
				if !staged[src] {
					t.Errorf("%s COPYs %s, which dockers_v2 does not stage in extra_files", d.Dockerfile, src)
				}
			}
			for src := range staged {
				if _, ok := copies[src]; !ok {
					t.Errorf("%s stages %s in extra_files but does not COPY it", d.Dockerfile, src)
				}
			}
		})
	}
}

// TestThirdPartyLicensesCoverEveryShippedBuild pins the generator's package and
// GOOS lists against the builds and platforms goreleaser ships: a build whose
// modules no pass visits would ship a dependency's notice nowhere.
func TestThirdPartyLicensesCoverEveryShippedBuild(t *testing.T) {
	cfg := loadLicensesConfig(t)
	if len(cfg.Builds) == 0 {
		t.Fatal(".goreleaser.yaml has no builds")
	}
	makefile := readRepoFile(t, "Makefile")

	pkgsValue, ok := makefileVar(t, makefile, "THIRD_PARTY_LICENSES_PKGS")
	if !ok {
		t.Fatalf("Makefile has no THIRD_PARTY_LICENSES_PKGS assignment")
	}
	goosValue, ok := makefileVar(t, makefile, "THIRD_PARTY_LICENSES_GOOS")
	if !ok {
		t.Fatalf("Makefile has no THIRD_PARTY_LICENSES_GOOS assignment")
	}
	pkgs := strings.Fields(pkgsValue)
	for i, p := range pkgs {
		pkgs[i] = strings.TrimPrefix(p, "./")
	}
	gooses := strings.Fields(goosValue)

	for _, b := range cfg.Builds {
		main := b.Main
		if strings.HasSuffix(main, ".go") {
			main = path.Dir(main)
		}
		main = strings.TrimPrefix(main, "./")
		if !slices.Contains(pkgs, main) {
			t.Errorf("build %q (%s) is not in THIRD_PARTY_LICENSES_PKGS", b.ID, b.Main)
		}
		for _, goos := range b.GOOS {
			if !slices.Contains(gooses, goos) {
				t.Errorf("build %q ships %s but THIRD_PARTY_LICENSES_GOOS does not generate a tree for it", b.ID, goos)
			}
		}
	}
}

// dockerfileCopies returns the single-source COPY instructions of dockerfile,
// keyed by source with the destination as the value. The $TARGETPLATFORM
// artifact is goreleaser's per-platform binary, not a license file.
func dockerfileCopies(t *testing.T, dockerfile string) map[string]string {
	t.Helper()
	src := readRepoFile(t, dockerfile)
	copies := map[string]string{}
	seen := false
	for _, line := range strings.Split(src, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "COPY") {
			continue
		}
		seen = true
		if len(fields) != 3 {
			t.Fatalf("%s: cannot parse COPY instruction %q; want one source and one destination", dockerfile, line)
		}
		if strings.HasPrefix(fields[1], "$TARGETPLATFORM/") {
			continue
		}
		copies[fields[1]] = fields[2]
	}
	if !seen {
		t.Fatalf("%s has no COPY instruction; a test that parses nothing tests nothing", dockerfile)
	}
	return copies
}
