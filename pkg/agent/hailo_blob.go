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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	return "", fmt.Errorf("no hailo-ollama data directory in %s; set --hailo-manifests-dir",
		strings.Join(options, ", "))
}

// hailoHEFDigest reads the digest hailo-ollama maps tag ("name:tag") to, from
// <dir>/<name>/<tag>/manifest.json.
func hailoHEFDigest(dir, tag string) (string, error) {
	name, version, ok := strings.Cut(tag, ":")
	if !ok {
		return "", fmt.Errorf("hailo model %q has no :tag; hailo-ollama names every model name:tag", tag)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name, version, "manifest.json"))
	if err != nil {
		return "", fmt.Errorf("hailo-ollama's manifests in %s have no %s: %w", dir, tag, err)
	}
	var m struct {
		HEF string `json:"hef_h10h"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("manifest for %s in %s: %w", tag, dir, err)
	}
	if !hailoDigestPattern.MatchString(m.HEF) {
		return "", fmt.Errorf("manifest for %s in %s has no hef_h10h digest (got %q)", tag, dir, m.HEF)
	}
	return m.HEF, nil
}

// hailoBlobSize HEADs the HEF hailo-ollama would pull for digest and returns
// its Content-Length.
func hailoBlobSize(ctx context.Context, client *http.Client, baseURL, digest string) (uint64, error) {
	url := strings.TrimRight(baseURL, "/") + "/blob/sha256_" + digest
	size, err := remoteModelSize(ctx, client, url)
	if err != nil {
		return 0, fmt.Errorf("size %s: %w", url, err)
	}
	return size, nil
}
