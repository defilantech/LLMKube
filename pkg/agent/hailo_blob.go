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

package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultHailoLibraryURL is the host hailo-ollama pulls HEFs from.
const DefaultHailoLibraryURL = "https://dev-public.hailo.ai"

// hailoDigestPattern is the form of a manifest's hef_h10h, the SHA-256 that
// names a HEF on the library host.
var hailoDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// hailoManifestsDir finds hailo-ollama's manifest directory as hailo-ollama
// does: the first of the data home and the data dirs that has a hailo-ollama
// directory, then models/manifests under it.
func hailoManifestsDir(home, xdgDataHome, xdgDataDirs string) (string, error) {
	dataHome := xdgDataHome
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	if xdgDataDirs == "" {
		xdgDataDirs = "/usr/share:/usr/local/share"
	}
	options := []string{dataHome}
	for _, d := range strings.Split(xdgDataDirs, ":") {
		if d != "" {
			options = append(options, d)
		}
	}
	for _, opt := range options {
		dir := filepath.Join(opt, "hailo-ollama")
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return filepath.Join(dir, "models", "manifests"), nil
		}
	}
	return "", fmt.Errorf("no hailo-ollama directory under %s; %s",
		strings.Join(options, ", "), hailoManifestsHint)
}

// hailoManifestsHint is appended to a manifest lookup that failed, since the
// default directory is the one hailo-ollama uses when it runs as the agent's
// user with the agent's XDG data directories.
const hailoManifestsHint = "if hailo-ollama runs as another user or from another data directory, " +
	"set --hailo-manifests-dir to its models/manifests directory"

// hailoHEFDigest reads the digest hailo-ollama maps tag ("name:tag") to, from
// <dir>/<name>/<tag>/manifest.json. The manifest must resolve to exactly that
// path under dir.
func hailoHEFDigest(dir, tag string) (string, error) {
	name, version, ok := strings.Cut(tag, ":")
	if !ok {
		return "", fmt.Errorf("hailo model %q has no :tag; hailo-ollama names every model name:tag", tag)
	}
	path := filepath.Join(dir, name, version, "manifest.json")
	rel, err := filepath.Rel(dir, path)
	parts := strings.Split(rel, string(filepath.Separator))
	if err != nil || len(parts) != 3 || parts[0] == ".." {
		return "", fmt.Errorf("hailo model %q does not name a manifest under %s", tag, dir)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("hailo-ollama manifests directory %s is missing; %s", dir, hailoManifestsHint)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("no hailo-ollama manifest for %s at %s (%w); %s", tag, path, err, hailoManifestsHint)
	}
	var m struct {
		HEF string `json:"hef_h10h"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("manifest for %s at %s: %w", tag, path, err)
	}
	if !hailoDigestPattern.MatchString(m.HEF) {
		return "", fmt.Errorf("manifest for %s at %s has no hef_h10h digest (got %q)", tag, path, m.HEF)
	}
	return m.HEF, nil
}

// hailoBlobURL is the URL of the HEF hailo-ollama would pull for digest from
// the library at baseURL.
func hailoBlobURL(baseURL, digest string) string {
	return strings.TrimRight(baseURL, "/") + "/blob/sha256_" + digest
}
