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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const fuseStampHit = "(stamp hit, skipped re-hashing)"

// fuseStampModel is a single pinned file, pinned to the bytes "pinned bytes".
func fuseStampModel(digest string) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "weights", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:     "https://models.example.com",
			Format:     "gguf",
			Files:      []string{"model.gguf"},
			FileSHA256: map[string]inferencev1alpha1.SHA256Digest{"model.gguf": inferencev1alpha1.SHA256Digest(digest)},
		},
		Status: inferencev1alpha1.ModelStatus{CacheKey: "cache"},
	}
}

// fuseStampDownloader is the generated model-downloader for model: the
// serving pod's, or the prefetch Job's when prefetch is set.
func fuseStampDownloader(t *testing.T, model *inferencev1alpha1.Model, prefetch bool) *corev1.Container {
	t.Helper()
	if prefetch {
		r := &ModelReconciler{InitContainerImage: defaultPrefetchImage}
		job, err := r.buildPrefetchJob(t.Context(), model, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return downloaderOf(t, job.Spec.Template.Spec.InitContainers)
	}
	storage := buildModelStorageConfig(model, nil, resolveEffectiveModelCache(nil, model), "default", true, ModelCacheModeShared, "", defaultPrefetchImage, 0, ModelCachePrepAlways, DefaultInitContainerUser, nil, "")
	return downloaderOf(t, storage.initContainers)
}

// setMtime sets path's modification time to the unix second given.
func setMtime(t *testing.T, path string, unix int64) {
	t.Helper()
	ts := time.Unix(unix, 0)
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// fileMtime is path's modification time in unix seconds.
func fileMtime(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.ModTime().Unix()
}

// stageStamp lays out a cached file and its stamp as one mount reports them:
// the file's bytes and mtime, the stamp's recorded "<digest> <size> <mtime>",
// and the stamp file's own mtime. It returns the stamp.
func stageStamp(t *testing.T, path string, body []byte, digest string, size, dataMtime, recorded, stampMtime int64) string {
	t.Helper()
	stamp := digest + " " + strconv.FormatInt(size, 10) + " " + strconv.FormatInt(recorded, 10)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".sha256", []byte(stamp), 0o644); err != nil {
		t.Fatal(err)
	}
	setMtime(t, path, dataMtime)
	setMtime(t, path+".sha256", stampMtime)
	return stamp
}

// statShimEnv puts a stat that answers -f as fs would (fakeStatScript) first
// on PATH, for a script run outside runDownloaderOnFS.
func statShimEnv(t *testing.T, fs string) []string {
	t.Helper()
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "stat"), []byte(fakeStatScript(fs)), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "REAL_STAT=" + realStat}
}

// TestFUSEStampOrdering pins when a stamp vouches for a cached file (#2393).
// Everywhere, the stamp's exact digest, size and mtime are a hit. On a FUSE
// cache, where the mtime one mount reports for a renamed file is not the one
// another reports (a blobfuse2 rename copies the blob, and the writing node's
// file cache keeps reporting its local copy's time), a stamp whose digest and
// size match is also a hit when the file's mtime lies between the mtime the
// stamp recorded and the time the stamp itself was written: the file last
// changed before it was stamped. A hit never rewrites the stamp, so nothing
// vouches for bytes no one hashed. Anything else re-hashes, and a file whose
// bytes no longer match the pin fails closed.
func TestFUSEStampOrdering(t *testing.T) {
	requireInitShellEnvironment(t)
	pinned := []byte("pinned bytes")
	swapped := []byte("pinned bytez") // same size, other bytes
	digest := strings.ToLower(sha256Hex(pinned))
	size := int64(len(pinned))
	base := time.Now().Add(-time.Hour).Unix()

	for _, tc := range []struct {
		name     string
		fs       string
		prefetch bool
		body     []byte
		size     int64 // the size the stamp records
		data     int64 // the file's mtime on this mount
		recorded int64 // the mtime the stamp records
		stamped  int64 // the stamp file's own mtime on this mount
		envFrom  []string
		wantHit  bool
	}{
		{name: "ext4 exact triple", fs: "ext4", body: pinned, size: size, data: base, recorded: base, stamped: base + 1, wantHit: true},
		{name: "fuse exact triple on the writing node", fs: "fuse", body: pinned, size: size, data: base - 300, recorded: base - 300, stamped: base, wantHit: true},
		{name: "fuse rename landed a later mtime (#2393)", fs: "fuse", body: pinned, size: size, data: base, recorded: base - 300, stamped: base + 1, wantHit: true},
		{name: "fuse rename landed a later mtime, prefetch Job", fs: "fuse", prefetch: true, body: pinned, size: size, data: base, recorded: base - 300, stamped: base + 1, wantHit: true},
		{name: "fuse data and stamp in the same second", fs: "fuse", body: pinned, size: size, data: base, recorded: base - 300, stamped: base, wantHit: true},
		{name: "busybox fuse", fs: "busybox-fuse", body: pinned, size: size, data: base, recorded: base - 300, stamped: base + 1, wantHit: true},
		{name: "fuse file replaced after it was stamped", fs: "fuse", body: swapped, size: size, data: base + 60, recorded: base - 300, stamped: base},
		{name: "fuse file older than the stamp records", fs: "fuse", body: pinned, size: size, data: base - 600, recorded: base - 300, stamped: base},
		{name: "fuse size differs", fs: "fuse", body: pinned, size: size + 1, data: base, recorded: base - 300, stamped: base + 1},
		{name: "ext4 moved mtime", fs: "ext4", body: pinned, size: size, data: base, recorded: base - 300, stamped: base + 1},
		{name: "filesystem unknown", fs: "unavailable", body: pinned, size: size, data: base, recorded: base - 300, stamped: base + 1},
		{name: "EnvFrom cannot claim FUSE", fs: "ext4", body: pinned, size: size, data: base, recorded: base - 300, stamped: base + 1,
			envFrom: []string{"cache_fs=65735546", "_llmkube_stamp_fs=65735546"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "model.gguf")
			stamp := stageStamp(t, path, tc.body, digest, tc.size, tc.data, tc.recorded, tc.stamped)
			out, err := runDownloaderOnFS(t, fuseStampDownloader(t, fuseStampModel(digest), tc.prefetch), dir, tc.fs, tc.envFrom...)
			hit := strings.Contains(out, fuseStampHit)
			if hit != tc.wantHit {
				t.Fatalf("stamp hit = %v, want %v:\n%s", hit, tc.wantHit, out)
			}
			got := readOrFail(t, path+".sha256")
			switch {
			case tc.wantHit:
				if err != nil {
					t.Fatalf("downloader failed on a stamp hit: %v\n%s", err, out)
				}
				if got != stamp || fileMtime(t, path+".sha256") != tc.stamped {
					t.Errorf("a stamp hit rewrote the stamp: %q, was %q", got, stamp)
				}
			case string(tc.body) == string(pinned):
				if err != nil {
					t.Fatalf("re-hash of the pinned bytes failed: %v\n%s", err, out)
				}
				if want := stampTriple(t, path, digest); got != want {
					t.Errorf("stamp after the re-hash = %q, want %q", got, want)
				}
			default:
				if err == nil || !strings.Contains(out, "mismatch") {
					t.Fatalf("bytes that no longer match the pin were served: err=%v\n%s", err, out)
				}
				if got != stamp {
					t.Errorf("a failed check rewrote the stamp: %q, was %q", got, stamp)
				}
			}
		})
	}
}

// TestFUSEStampMarkerPath: the single-file downloader's rejection-marker guard
// checks the cached copy before the cache prologue runs. It must classify the
// filesystem the same way as the main path, from nothing the pod's environment
// can set.
func TestFUSEStampMarkerPath(t *testing.T) {
	requireInitShellEnvironment(t)
	pinned := []byte("pinned bytes")
	digest := strings.ToLower(sha256Hex(pinned))
	base := time.Now().Add(-time.Hour).Unix()
	script := buildModelInitCommand(false, false, true, false, true, RefreshPolicyIfNotPresent)

	for _, tc := range []struct {
		name    string
		fs      string
		envFrom []string
		wantHit bool
	}{
		{name: "fuse keeps the pinned copy without re-hashing", fs: "fuse", wantHit: true},
		{name: "EnvFrom cannot claim FUSE", fs: "ext4", envFrom: []string{"cache_fs=65735546", "_llmkube_stamp_fs=65735546"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "model.gguf")
			stageStamp(t, path, pinned, digest, int64(len(pinned)), base, base-300, base+1)
			if err := os.WriteFile(path+"."+digest+".sha256-rejected", []byte(digest), 0o644); err != nil {
				t.Fatal(err)
			}
			env := append(append([]string{"LLMKUBE_PREFETCH_STAGER=false"}, tc.envFrom...), statShimEnv(t, tc.fs)...)
			out, err := runVerifyScriptEnv(t, script, "http://127.0.0.1:1/model.gguf", path, digest, env...)
			if err != nil || !strings.Contains(out, "kept the pinned cached copy") {
				t.Fatalf("the marker guard did not keep the pinned copy: %v\n%s", err, out)
			}
			if hit := strings.Contains(out, fuseStampHit); hit != tc.wantHit {
				t.Fatalf("stamp hit = %v, want %v:\n%s", hit, tc.wantHit, out)
			}
		})
	}
}
