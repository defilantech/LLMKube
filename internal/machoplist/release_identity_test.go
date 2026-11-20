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
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The release identity lives in four places that must agree: the GoReleaser
// hook arguments (the identifier rcodesign signs with), the Info.plist
// templates (CFBundleIdentifier, which macOS shows and codesign binds), the
// build's binary name (CFBundleExecutable) and the launchd plists
// (AssociatedBundleIdentifiers). These tests read the checked-in files.

const repoRoot = "../.."

type releaseIdentity struct {
	identifier string
	template   string // empty: identifier only, no Info.plist
	launchd    string // launchd plist that must associate the identifier
}

var darwinReleaseBuilds = map[string]releaseIdentity{
	"llmkube-cli": {identifier: "dev.llmkube.cli"},
	"llmkube-metal-agent": {
		identifier: "dev.llmkube.metal-agent",
		template:   "deployment/macos/metal-agent.Info.plist.tmpl",
		launchd:    "deployment/macos/com.llmkube.metal-agent.plist",
	},
}

type goreleaserHook struct {
	Cmd    string   `json:"cmd"`
	Env    []string `json:"env"`
	Output bool     `json:"output"`
}

type goreleaserConfig struct {
	Before struct {
		Hooks []string `json:"hooks"`
	} `json:"before"`
	Builds []struct {
		ID     string   `json:"id"`
		Binary string   `json:"binary"`
		Goos   []string `json:"goos"`
		Hooks  struct {
			Post []goreleaserHook `json:"post"`
		} `json:"hooks"`
	} `json:"builds"`
	Archives []struct {
		ID    string   `json:"id"`
		Files []string `json:"files"`
	} `json:"archives"`
	Notarize any `json:"notarize"`
}

func loadGoreleaser(t *testing.T) goreleaserConfig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg goreleaserConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse .goreleaser.yaml: %v", err)
	}
	return cfg
}

// TestGoreleaserSignsEveryDarwinBuild pins the hook wiring: every darwin build
// runs hack/macos-sign.sh with its identifier (and template), the hook gets
// the prebuilt machoplist, and GoReleaser's own notarize step stays off (it
// re-signs with the file name as identifier and drops the plist binding).
func TestGoreleaserSignsEveryDarwinBuild(t *testing.T) {
	cfg := loadGoreleaser(t)

	if !slices.Contains(cfg.Before.Hooks, "go build -o ./bin/machoplist ./cmd/machoplist") {
		t.Fatalf("before.hooks does not build ./bin/machoplist once for the post hooks: %q", cfg.Before.Hooks)
	}
	if cfg.Notarize != nil {
		t.Fatal(".goreleaser.yaml has a notarize section; quill would re-sign the binaries and unbind the Info.plist")
	}

	seen := map[string]bool{}
	for _, b := range cfg.Builds {
		if !slices.Contains(b.Goos, "darwin") {
			continue
		}
		seen[b.ID] = true
		want, ok := darwinReleaseBuilds[b.ID]
		if !ok {
			t.Errorf("darwin build %q has no release identity; add it to darwinReleaseBuilds and give it a macos-sign hook", b.ID)
			continue
		}
		if len(b.Hooks.Post) != 1 {
			t.Errorf("build %q: %d post hooks, want exactly the macos-sign hook", b.ID, len(b.Hooks.Post))
			continue
		}
		h := b.Hooks.Post[0]
		wantCmd := "./hack/macos-sign.sh {{ .Os }} {{ .Path }} {{ .Version }} " + want.identifier
		if want.template != "" {
			wantCmd += " " + want.template
		}
		if got := strings.Join(strings.Fields(h.Cmd), " "); got != wantCmd {
			t.Errorf("build %q hook cmd = %q, want %q", b.ID, got, wantCmd)
		}
		if !slices.Contains(h.Env, "MACHOPLIST=./bin/machoplist") {
			t.Errorf("build %q hook env %q lacks MACHOPLIST=./bin/machoplist", b.ID, h.Env)
		}
		if !h.Output {
			t.Errorf("build %q hook hides its output; signing and notary logs must reach the release log", b.ID)
		}
		if want.template != "" {
			assertTemplate(t, want, b.Binary)
		}
	}
	for id := range darwinReleaseBuilds {
		if !seen[id] {
			t.Errorf("expected darwin build %q in .goreleaser.yaml", id)
		}
	}
}

// assertTemplate renders the template as the hook does and checks the keys
// macOS reads, then that it fits a real Go binary's header padding.
func assertTemplate(t *testing.T, want releaseIdentity, binary string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, want.template))
	if err != nil {
		t.Fatal(err)
	}
	rendered := bytes.ReplaceAll(raw, []byte("__VERSION__"), []byte("1.2.3"))
	dict := parsePlistDict(t, rendered)
	for key, value := range map[string]string{
		"CFBundleIdentifier":            want.identifier,
		"CFBundleExecutable":            binary,
		"CFBundleShortVersionString":    "1.2.3",
		"CFBundleVersion":               "1.2.3",
		"CFBundleInfoDictionaryVersion": "6.0",
		"LSMinimumSystemVersion":        "13.0",
	} {
		if dict[key] != value {
			t.Errorf("%s: %s = %v, want %q", want.template, key, dict[key], value)
		}
	}
	for _, key := range []string{"CFBundleName", "CFBundleDisplayName", "NSLocalNetworkUsageDescription"} {
		if s, _ := dict[key].(string); s == "" {
			t.Errorf("%s: %s is missing or empty", want.template, key)
		}
	}
	if _, ok := dict["CFBundlePackageType"]; ok {
		t.Errorf("%s: CFBundlePackageType is for bundles, not a bare executable", want.template)
	}
	// darwin/amd64 is the tight case: no linker signature, so Inject keeps
	// room for codesign's LC_CODE_SIGNATURE. Measured on the real 0.10.12
	// amd64 binaries (sizeofcmds 2288), a plist of up to 1680 bytes fits; the
	// cap leaves headroom for a Go upgrade that adds load commands, so a
	// growing template fails here rather than in a tagged release.
	if len(rendered) > maxTemplateBytes {
		t.Errorf("%s renders to %d bytes, over the %d-byte cap", want.template, len(rendered), maxTemplateBytes)
	}
	if _, err := Inject(darwinBinary(t, target{arch: "amd64", stripped: true}), rendered); err != nil {
		t.Errorf("%s does not embed into a release-shaped amd64 binary: %v", want.template, err)
	}
}

const maxTemplateBytes = 1400

// TestLaunchdPlistsAssociateBundleIdentifiers checks TN3179's advice for a
// launchd agent not installed with SMAppService: name the signed bundle
// identifier so System Settings attributes the background item to it.
func TestLaunchdPlistsAssociateBundleIdentifiers(t *testing.T) {
	for id, want := range darwinReleaseBuilds {
		if want.launchd == "" {
			continue
		}
		t.Run(id, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(repoRoot, want.launchd))
			if err != nil {
				t.Fatal(err)
			}
			dict := parsePlistDict(t, raw)
			ids, _ := dict["AssociatedBundleIdentifiers"].([]string)
			if !slices.Equal(ids, []string{want.identifier}) {
				t.Fatalf("%s: AssociatedBundleIdentifiers = %v, want [%s]", want.launchd, dict["AssociatedBundleIdentifiers"], want.identifier)
			}
		})
	}
}

// xmlComment matches comments, which are dropped before parsing: Apple's plist
// parser accepts "--" inside a comment, encoding/xml does not.
var xmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// parsePlistDict decodes the top-level dict of an XML plist into strings and
// string arrays; other value types are recorded as present with a nil value.
func parsePlistDict(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	d := xml.NewDecoder(bytes.NewReader(xmlComment.ReplaceAll(raw, nil)))
	out := map[string]any{}
	depth, key := 0, ""
	var array []string
	inArray := false
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse plist: %v", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			depth++
			if depth != 3 && !(inArray && depth == 4) {
				continue
			}
			switch el.Name.Local {
			case "key":
				var s string
				if err := d.DecodeElement(&s, &el); err != nil {
					t.Fatal(err)
				}
				depth--
				key = s
			case "string":
				var s string
				if err := d.DecodeElement(&s, &el); err != nil {
					t.Fatal(err)
				}
				depth--
				if inArray {
					array = append(array, s)
				} else {
					out[key] = s
				}
			case "array":
				inArray, array = true, []string{}
			default:
				out[key] = nil
			}
		case xml.EndElement:
			if inArray && depth == 3 && el.Name.Local == "array" {
				out[key], inArray = array, false
			}
			depth--
		}
	}
	return out
}

// TestArchivesShipDeploymentMacosWithoutTemplates checks the agent archives
// carry every file under deployment/macos except the build-only ones: the
// Info.plist templates (their __VERSION__ placeholder would confuse an archive
// reader next to the real launchd plists) and the Go source that embeds the
// manifests into the CLI. Expanding the archive globs against the tree also
// catches a new file the globs would miss.
func TestArchivesShipDeploymentMacosWithoutTemplates(t *testing.T) {
	cfg := loadGoreleaser(t)
	var want []string
	err := filepath.WalkDir(filepath.Join(repoRoot, "deployment/macos"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || buildOnlyMacosFile(p) {
			return err
		}
		rel, err := filepath.Rel(repoRoot, p)
		want = append(want, rel)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"llmkube-metal-agent"} {
		t.Run(id, func(t *testing.T) {
			var files []string
			found := false
			for _, a := range cfg.Archives {
				if a.ID == id {
					files, found = a.Files, true
				}
			}
			if !found {
				t.Fatalf("archive %q not in .goreleaser.yaml", id)
			}
			got := map[string]bool{}
			for _, glob := range files {
				if !strings.HasPrefix(glob, "deployment/macos/") {
					continue
				}
				matches, err := filepath.Glob(filepath.Join(repoRoot, glob))
				if err != nil {
					t.Fatalf("glob %q: %v", glob, err)
				}
				// GoReleaser archives a matched directory with everything in it.
				for _, m := range matches {
					err := filepath.WalkDir(m, func(p string, d os.DirEntry, err error) error {
						if err != nil || d.IsDir() {
							return err
						}
						rel, err := filepath.Rel(repoRoot, p)
						got[rel] = true
						return err
					})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			for f := range got {
				if buildOnlyMacosFile(f) {
					t.Errorf("archive %s ships the build-only file %s", id, f)
				}
			}
			for _, f := range want {
				if !got[f] {
					t.Errorf("archive %s no longer ships %s", id, f)
				}
			}
		})
	}
}

// buildOnlyMacosFile reports whether a file under deployment/macos is used only
// to build the release, so the agent archives must leave it out.
func buildOnlyMacosFile(path string) bool {
	return strings.HasSuffix(path, ".tmpl") || strings.HasSuffix(path, ".go")
}
