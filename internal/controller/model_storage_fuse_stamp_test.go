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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

// fuseStampDownloader is the generated downloader for model: the serving
// pod's, or the prefetch Job's when prefetch is set.
func fuseStampDownloader(t *testing.T, model *inferencev1alpha1.Model, prefetch bool) corev1.Container {
	t.Helper()
	var containers []corev1.Container
	if prefetch {
		r := &ModelReconciler{InitContainerImage: defaultPrefetchImage}
		job, err := r.buildPrefetchJob(t.Context(), model, "", "")
		if err != nil {
			t.Fatal(err)
		}
		containers = job.Spec.Template.Spec.InitContainers
	} else {
		storage := buildModelStorageConfig(model, nil, resolveEffectiveModelCache(nil, model), "default", true, ModelCacheModeShared, "", defaultPrefetchImage, 0, ModelCachePrepAlways, DefaultInitContainerUser, nil, "")
		containers = storage.initContainers
	}
	for i := range containers {
		if containers[i].Name == "model-downloader" {
			return containers[i]
		}
	}
	t.Fatal("missing downloader")
	return corev1.Container{}
}

// fuseStatPath puts a stat answering -f as fs would first on PATH, beside a
// broken sha256sum: a case whose stamp must be a hit fails if anything reads
// the file's bytes. The shim defers to the real stat directly, because the
// command reads sizes and mtimes in the same call.
func fuseStatPath(t *testing.T, fs string) string {
	t.Helper()
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	// "unknown" answers -f from the host, so the probe directory's own
	// filesystem is the one classified; every other name is the shim's answer
	// for any path.
	shimFS := fs
	if fs == "unknown" {
		shimFS = "unavailable"
	}
	shim := strings.ReplaceAll(fakeStatScript(shimFS), `"$REAL_STAT"`, fmt.Sprintf("%q", filepath.Join(bin, "real-stat")))
	if err := os.WriteFile(filepath.Join(bin, "stat"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "real-stat"), []byte("#!/bin/sh\nexec "+realStat+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := "#!/bin/sh\necho 'the stamp was not a hit: the file was re-hashed' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "sha256sum"), []byte(broken), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// runStampDownloader runs a downloader's command with its own env, with dir
// as the cache directory.
func runStampDownloader(t *testing.T, c corev1.Container, dir string, prefetch bool) (string, error) {
	t.Helper()
	return runDownloaderEnv(t, c, prefetch, "CACHE_DIR="+dir)
}

// runDownloaderEnv runs a downloader's command with its own env plus extra.
func runDownloaderEnv(t *testing.T, c corev1.Container, prefetch bool, extra ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", c.Command[2])
	cmd.Env = os.Environ()
	for _, env := range c.Env {
		cmd.Env = append(cmd.Env, env.Name+"="+env.Value)
	}
	if prefetch {
		cmd.Env = append(cmd.Env, "LLMKUBE_PREFETCH_STAGER=true")
	}
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestModelInitSHA256FUSEStampTolerance pins where a stamp's mtime still
// gates: every non-FUSE cache keeps the full triple, and a FUSE cache (where
// a publish rename lands a Last-Modified the writing mount does not report)
// accepts a digest+size match and refreshes the stamp (#2393).
func TestModelInitSHA256FUSEStampTolerance(t *testing.T) {
	requireInitShellEnvironment(t)
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.ToLower(sha256Hex([]byte("pinned bytes")))
	model := fuseStampModel(digest)

	cases := []struct {
		fs       string // fakeStatScript's name for the filesystem
		prefetch bool
		state    string
		wantHit  bool
	}{
		{"fuse", false, "matching", true},
		{"fuse", false, "stale-mtime", true},
		{"fuse", false, "in-place-rewrite", true},
		{"fuse", false, "size-changed", false},
		{"fuse", true, "matching", true},
		{"fuse", true, "stale-mtime", true},
		{"fuse", true, "in-place-rewrite", true},
		{"fuse", true, "size-changed", false},
		{"ext4", false, "matching", true},
		{"ext4", false, "stale-mtime", false},
		{"ext4", false, "in-place-rewrite", false},
		{"ext4", false, "size-changed", false},
		{"unknown", false, "stale-mtime", false},
		{"unavailable", false, "stale-mtime", false},
	}
	for _, tc := range cases {
		role := "serving"
		if tc.prefetch {
			role = "prefetch"
		}
		t.Run(tc.fs+"/"+role+"/"+tc.state, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "model.gguf")
			if err := os.WriteFile(path, []byte("pinned bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
			// The stamp the publishing mount wrote, right after its rename.
			if err := os.WriteFile(path+".sha256", []byte(stampTriple(t, path, digest)), 0o644); err != nil {
				t.Fatal(err)
			}
			stampInode := fileInode(t, path+".sha256")
			switch tc.state {
			case "stale-mtime":
				shiftMtime(t, path, 300)
			case "in-place-rewrite":
				// A new inode, so the mtime moves as it does for a writer
				// that replaces the file rather than writing through it.
				// Equal length: the stamp's size field must not tell them apart.
				if err := os.WriteFile(path+".rewrite", []byte("pinned bytez"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".rewrite", path); err != nil {
					t.Fatal(err)
				}
				shiftMtime(t, path, 300)
			case "size-changed":
				if err := os.WriteFile(path, []byte("pinned bytes, longer"), 0o644); err != nil {
					t.Fatal(err)
				}
				shiftMtime(t, path, 300)
			}
			wantStamp := stampTriple(t, path, digest)
			var out string
			var err error
			if tc.wantHit {
				// A stat that defers to the real one, so the command's own
				// CACHE_DIR walk answers the magic the case names.
				bin := t.TempDir()
				if err := os.WriteFile(filepath.Join(bin, "stat"), []byte(fakeStatScript(tc.fs)), 0o755); err != nil {
					t.Fatal(err)
				}
				out, err = runDownloaderEnv(t, fuseStampDownloader(t, model, tc.prefetch), tc.prefetch,
					"CACHE_DIR="+dir, "PATH="+bin+":"+os.Getenv("PATH"), "REAL_STAT="+realStat)
			} else {
				out, err = runStampDownloader(t, fuseStampDownloader(t, model, tc.prefetch), dir, tc.prefetch)
			}
			if got := strings.Contains(out, "(stamp hit, skipped re-hashing)"); got != tc.wantHit {
				t.Fatalf("stamp hit = %v, want %v: %v\n%s", got, tc.wantHit, err, out)
			}
			if !tc.wantHit {
				return
			}
			// A refresh renames a new file over the stamp, so its inode
			// changes even within the same second.
			if tc.state == "matching" && fileInode(t, path+".sha256") != stampInode {
				t.Errorf("stamp mtime moved on a matching triple: inode %d -> %d", stampInode, fileInode(t, path+".sha256"))
			}
			if got := readOrFail(t, path+".sha256"); got != wantStamp {
				t.Errorf("stamp after hit = %q, want %q", got, wantStamp)
			}
		})
	}
}

// TestFUSERecheckStamp drives llmkube_accept_stamp alone: which stamps it
// accepts for a given $cache_fs, and the stamp it leaves behind.
func TestFUSERecheckStamp(t *testing.T) {
	requireInitShellEnvironment(t)
	digest := strings.ToLower(sha256Hex([]byte("pinned bytes")))
	cases := []struct {
		name        string
		fs          string
		stamp       string // "-" is the matching triple, "" writes no stamp
		sizeDelta   int    // bytes appended to the file after the stamp is written
		mtimeDelta  int    // seconds added to the file after the stamp is written
		readOnly    bool
		wantAccept  bool
		wantRefresh bool
		wantWarning bool
	}{
		{name: "matching-triple", fs: "fuse", stamp: "-", wantAccept: true},
		{name: "matching-triple-writes-nothing", fs: "fuse", stamp: "-", wantAccept: true},
		{name: "fuse-stale-mtime", fs: "fuse", stamp: "-", mtimeDelta: 300, wantAccept: true, wantRefresh: true},
		{name: "fuse-different-size", fs: "fuse", stamp: "-", sizeDelta: 3, wantAccept: false},
		{name: "ext4-stale-mtime", fs: "ext4", stamp: "-", mtimeDelta: 300, wantAccept: false},
		{name: "unknown-stale-mtime", fs: "unknown", stamp: "-", mtimeDelta: 300, wantAccept: false},
		{name: "empty-stamp-fuse", fs: "fuse", wantAccept: false},
		{name: "empty-stamp-ext4", fs: "ext4", wantAccept: false},
		{name: "bare-digest-fuse", fs: "fuse", stamp: digest, wantAccept: false},
		{name: "bare-digest-ext4", fs: "ext4", stamp: digest, wantAccept: false},
		{name: "garbage-fuse", fs: "fuse", stamp: "not a stamp", wantAccept: false},
		{name: "fuse-stale-mtime-readonly-stamp", fs: "fuse", stamp: "-", mtimeDelta: 300, readOnly: true, wantAccept: true, wantWarning: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "model.gguf")
			if err := os.WriteFile(path, []byte("pinned bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
			stamp := tc.stamp
			if stamp == "-" {
				stamp = stampTriple(t, path, digest)
			}
			if tc.stamp != "" {
				if err := os.WriteFile(path+".sha256", []byte(stamp), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.sizeDelta > 0 {
				if err := os.WriteFile(path, []byte("pinned bytes"+strings.Repeat("x", tc.sizeDelta)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mtimeDelta > 0 {
				shiftMtime(t, path, tc.mtimeDelta)
			}
			stampMtime, stampInode := int64(0), uint64(0)
			if tc.stamp != "" {
				stampMtime, stampInode = fileModUnix(t, path+".sha256"), fileInode(t, path+".sha256")
			}
			// A stamp is refreshed to the mtime this mount sees, so after a
			// refresh it describes the file as it is now.
			wantStamp := stamp
			if tc.wantRefresh {
				wantStamp = stampTriple(t, path, digest)
			}
			if tc.readOnly {
				if err := os.Chmod(dir, 0o555); err != nil { // #nosec G302 -- the test needs a directory the uid cannot write
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) // #nosec G302 -- restores the temp dir
			}

			// The gate set, then one check of model.gguf: the scene a restart
			// of the downloader sets up, with its caller removed.
			script := sha256VerifyFns + `llmkube_check_sha256 model.gguf`
			scriptPath := filepath.Join(t.TempDir(), "check.sh")
			if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", scriptPath)
			cmd.Args = []string{"sh", scriptPath, "model.gguf"}
			cmd.Dir = dir
			// cacheStagingPrologue sets cache_fs from CACHE_DIR's magic; this
			// test drives the check without the prologue, so it passes the
			// classification directly. "unknown" is an image whose stat has no
			// -f, which the prologue also records as unknown.
			fsValue := fuseSuperMagic
			if tc.fs == "ext4" {
				fsValue = "ef53"
			}
			if tc.fs == "unknown" {
				fsValue = "unknown"
			}
			cmd.Env = append(os.Environ(),
				"MODEL_SHA256="+digest,
				"CACHE_DIR="+dir,
				"cache_fs="+fsValue,
				"MODEL=model",
				"PATH="+fuseStatPath(t, tc.fs)+":"+os.Getenv("PATH"),
			)
			out, err := cmd.CombinedOutput()
			if accepted := err == nil; accepted != tc.wantAccept {
				t.Fatalf("accept = %v, want %v: %v\n%s", accepted, tc.wantAccept, err, out)
			}
			if !tc.wantAccept {
				return
			}
			got := readOrFail(t, path+".sha256")
			writeable := true
			if tc.readOnly {
				writeable = false
			}
			// A refresh renames a new file over the stamp, so its inode
			// changes even within the same second.
			refreshed := fileModUnix(t, path+".sha256") != stampMtime || fileInode(t, path+".sha256") != stampInode || got != stamp
			if refreshed != (tc.wantRefresh && writeable) {
				t.Errorf("stamp refreshed = %v, want %v (stamp %q)", refreshed, tc.wantRefresh && writeable, got)
			}
			if warned := strings.Contains(string(out), "WARNING: could not refresh the stamp"); warned != tc.wantWarning {
				t.Errorf("refresh warning = %v, want %v: %s", warned, tc.wantWarning, out)
			}
			if !tc.wantRefresh && got != wantStamp {
				t.Errorf("stamp = %q, want %q", got, wantStamp)
			}
		})
	}
}

func fileInode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return st.Ino
}

func fileModUnix(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.ModTime().Unix()
}
