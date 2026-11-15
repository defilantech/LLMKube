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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// #2393: on a blobfuse2 cache a publish rename lands a new Last-Modified that
// every other mount reports as the mtime, while the writing mount keeps
// reporting the pre-rename one from its attribute cache. The stamp the
// prefetch Job writes therefore misses on every serving pod, and each one
// re-hashes every shard. These subtests drive the Job's downloader and then a
// serving pod's downloader over the same cache directory, with the mtime moved
// between them as the other mount sees it.
func TestPrefetchStampFUSEMiss(t *testing.T) {
	requireInitShellEnvironment(t)
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Fatal(err)
	}
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
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "stat"), []byte(fakeStatScript("fuse")), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, containers []corev1.Container) string {
		t.Helper()
		var downloader *corev1.Container
		for i := range containers {
			if containers[i].Name == "model-downloader" {
				downloader = &containers[i]
			}
		}
		if downloader == nil {
			t.Fatal("missing downloader")
		}
		cmd := exec.Command("sh", "-c", downloader.Command[2])
		cmd.Env = os.Environ()
		for _, env := range downloader.Env {
			cmd.Env = append(cmd.Env, env.Name+"="+env.Value)
		}
		cmd.Env = append(cmd.Env, "CACHE_DIR="+dir, "PATH="+bin+":"+os.Getenv("PATH"), "REAL_STAT="+realStat)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("downloader failed: %v\n%s", err, out)
		}
		return string(out)
	}

	t.Run("prefetch-publishes-stamps", func(t *testing.T) {
		r := &ModelReconciler{InitContainerImage: defaultPrefetchImage}
		job, err := r.buildPrefetchJob(context.Background(), model, "", "")
		if err != nil {
			t.Fatal(err)
		}
		run(t, job.Spec.Template.Spec.InitContainers)
		for _, f := range files {
			path := filepath.Join(dir, f)
			if got := readOrFail(t, path); got != string(origin.content()) {
				t.Fatalf("%s was not staged", f)
			}
			mustStamp(t, path, digest)
			// The post-rename Last-Modified every other mount reports.
			shiftMtime(t, path, 300)
		}
	})

	t.Run("serving-uses-them", func(t *testing.T) {
		storage := buildModelStorageConfig(model, nil, resolveEffectiveModelCache(nil, model), "default", true, ModelCacheModeShared, "", defaultPrefetchImage, 0, ModelCachePrepAlways, DefaultInitContainerUser, nil, "")
		out := run(t, storage.initContainers)
		hits := strings.Count(out, "(stamp hit, skipped re-hashing)")
		if hits != len(files) {
			t.Fatalf("stamp hits = %d, want %d:\n%s", hits, len(files), out)
		}
		for _, f := range files {
			path := filepath.Join(dir, f)
			if got, want := readOrFail(t, path+".sha256"), stampTriple(t, path, digest); got != want {
				t.Errorf("stamp for %s = %q, want %q", f, got, want)
			}
		}
	})
}

// shiftMtime moves a file's modification time forward by seconds, the way a
// rename that copies the blob lands a new Last-Modified.
func shiftMtime(t *testing.T, path string, seconds int) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	cmd := exec.Command("touch", "-d", "@"+strconv.FormatInt(fi.ModTime().Unix()+int64(seconds), 10), path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("touch %s: %v\n%s", path, err, out)
	}
	if now, _ := os.Stat(path); now.ModTime().Unix() == fi.ModTime().Unix() {
		t.Fatalf("touch did not move the mtime of %s (%s)", path, fmt.Sprint(fi.ModTime()))
	}
}
