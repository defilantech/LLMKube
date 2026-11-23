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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHailoLibrary serves HEAD for each path in sizes with that
// Content-Length, and 404 for anything else.
func fakeHailoLibrary(t *testing.T, sizes map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size, ok := sizes[r.URL.Path]
		if r.Method != http.MethodHead || !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", size)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// writeHailoManifests lays out manifests/<name>/<tag>/manifest.json with the
// given hef_h10h values and returns the manifests directory.
func writeHailoManifests(t *testing.T, digests map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for tag, digest := range digests {
		name, version, _ := strings.Cut(tag, ":")
		dir := filepath.Join(root, name, version)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"hef_h10h": "` + digest + `", "template_params": {"eos_token": "<|endoftext|>"}}`
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const testHailoDigest = "5310176848638505fbc28add04ba60c97abe345cdb0ec7e3b8ffaa4b0a8c65dd"

func TestHailoManifestsDir(t *testing.T) {
	home := t.TempDir()
	sys1, sys2 := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(sys2, "hailo-ollama"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := hailoManifestsDir(home, "", sys1+":"+sys2)
	if want := filepath.Join(sys2, "hailo-ollama", "models", "manifests"); err != nil || got != want {
		t.Errorf("only the second data dir has hailo-ollama: %q, %v; want %q", got, err, want)
	}

	if err := os.MkdirAll(filepath.Join(home, ".local", "share", "hailo-ollama"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, _ = hailoManifestsDir(home, "", sys1+":"+sys2)
	if want := filepath.Join(home, ".local", "share", "hailo-ollama", "models", "manifests"); got != want {
		t.Errorf("the data home wins: %q, want %q", got, want)
	}

	if _, err := hailoManifestsDir(t.TempDir(), "", sys1); err == nil {
		t.Error("no hailo-ollama directory anywhere: want an error")
	}
}

func TestHailoHEFDigest(t *testing.T) {
	dir := writeHailoManifests(t, map[string]string{
		"qwen2.5-instruct:1.5b": testHailoDigest,
		"bad:1":                 "../../etc/passwd",
	})
	if got, err := hailoHEFDigest(dir, "qwen2.5-instruct:1.5b"); err != nil || got != testHailoDigest {
		t.Errorf("qwen2.5-instruct:1.5b = %q, %v", got, err)
	}
	for _, tag := range []string{"bad:1", "missing:1", "qwen2.5-instruct"} {
		if _, err := hailoHEFDigest(dir, tag); err == nil {
			t.Errorf("%s: want an error", tag)
		}
	}
}

func TestHailoHEFDigestNamesWhereItLooked(t *testing.T) {
	dir := writeHailoManifests(t, map[string]string{"qwen2.5-instruct:1.5b": testHailoDigest})
	_, err := hailoHEFDigest(dir, "llama3.2:3b")
	want := filepath.Join(dir, "llama3.2", "3b", "manifest.json")
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "--hailo-manifests-dir") {
		t.Errorf("tag with no manifest: err = %v, want one naming %s and --hailo-manifests-dir", err, want)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	_, err = hailoHEFDigest(missing, "qwen2.5-instruct:1.5b")
	if err == nil || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "--hailo-manifests-dir") {
		t.Errorf("no manifests directory: err = %v, want one naming %s and --hailo-manifests-dir", err, missing)
	}
	_, err = hailoManifestsDir(t.TempDir(), "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "--hailo-manifests-dir") {
		t.Errorf("no hailo-ollama data directory: err = %v, want one naming --hailo-manifests-dir", err)
	}
}

func TestHailoHEFDigestStaysUnderTheManifestsDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "manifests")
	// A manifest at each path the tags below would reach without the check.
	body := []byte(`{"hef_h10h": "` + testHailoDigest + `"}`)
	for _, p := range []string{
		filepath.Join(dir, "manifest.json"),
		filepath.Join(dir, "qwen", "manifest.json"),
		filepath.Join(root, "x", "manifest.json"),
		filepath.Join(dir, "a", "b", "1", "manifest.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tag := range []string{"qwen:..", "..:x", "qwen:.", "a/b:1"} {
		if got, err := hailoHEFDigest(dir, tag); err == nil {
			t.Errorf("%s = %q; want it refused as outside %s/<name>/<tag>", tag, got, dir)
		}
	}
}

func TestHailoBlobURL(t *testing.T) {
	for _, base := range []string{"https://dev-public.hailo.ai", "https://dev-public.hailo.ai/"} {
		if got, want := hailoBlobURL(base, testHailoDigest),
			"https://dev-public.hailo.ai/blob/sha256_"+testHailoDigest; got != want {
			t.Errorf("hailoBlobURL(%q) = %q, want %q", base, got, want)
		}
	}
}
