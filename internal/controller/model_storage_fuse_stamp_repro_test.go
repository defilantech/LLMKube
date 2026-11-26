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

package controller

import (
	"path/filepath"
	"strings"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// #2393: on a blobfuse2 cache a publish rename copies the blob, so every other
// node reports the copy's Last-Modified as the file's mtime, while the node
// that wrote it keeps reporting its local copy's time from the file cache. The
// stamp the prefetch Job writes therefore missed on every serving pod on
// another node, and each one re-hashed every shard. These subtests drive the
// Job's downloader and then a serving pod's downloader over the same cache
// directory. Between them the mtimes move the way another node reports them:
// the file to its copy's Last-Modified, and the stamp, written after the
// rename, to a later one.
func TestPrefetchStampFUSEMiss(t *testing.T) {
	requireInitShellEnvironment(t)
	origin := newRangeOrigin(t, true)
	files := []string{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"}
	digest := strings.ToLower(sha256Hex(origin.content()))
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "weights", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:     origin.srv.URL,
			Format:     "safetensors",
			Files:      files,
			FileSHA256: map[string]inferencev1alpha1.SHA256Digest{files[0]: inferencev1alpha1.SHA256Digest(digest), files[1]: inferencev1alpha1.SHA256Digest(digest)},
		},
		Status: inferencev1alpha1.ModelStatus{CacheKey: "cache"},
	}
	dir := t.TempDir()
	stamps := map[string]string{}

	t.Run("prefetch-publishes-stamps", func(t *testing.T) {
		out, err := runDownloaderOnFS(t, fuseStampDownloader(t, model, true), dir, "fuse")
		if err != nil {
			t.Fatalf("prefetch downloader failed: %v\n%s", err, out)
		}
		for _, f := range files {
			path := filepath.Join(dir, f)
			if got := readOrFail(t, path); got != string(origin.content()) {
				t.Fatalf("%s was not staged", f)
			}
			mustStamp(t, path, digest)
			stamps[f] = readOrFail(t, path+".sha256")
			// The copy's Last-Modified, then the stamp's, as another node
			// reports them.
			copied := fileMtime(t, path) + 300
			setMtime(t, path, copied)
			setMtime(t, path+".sha256", copied+1)
		}
	})

	t.Run("serving-uses-them", func(t *testing.T) {
		out, err := runDownloaderOnFS(t, fuseStampDownloader(t, model, false), dir, "fuse")
		if err != nil {
			t.Fatalf("serving downloader failed: %v\n%s", err, out)
		}
		if hits := strings.Count(out, fuseStampHit); hits != len(files) {
			t.Fatalf("stamp hits = %d, want %d:\n%s", hits, len(files), out)
		}
		for _, f := range files {
			if got := readOrFail(t, filepath.Join(dir, f)+".sha256"); got != stamps[f] {
				t.Errorf("a stamp hit rewrote the stamp for %s: %q, was %q", f, got, stamps[f])
			}
		}
	})
}
