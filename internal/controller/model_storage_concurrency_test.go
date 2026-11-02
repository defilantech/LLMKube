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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// pacedOrigin serves fixed per-file bodies slowly enough that concurrent
// transfers overlap, and records how many GETs were in flight at once. HEAD
// answers with the size and an ETag so the resume probe derives a real key.
type pacedOrigin struct {
	srv      *httptest.Server
	files    map[string][]byte
	mu       sync.Mutex
	inFlight int
	maxSeen  int
	requests map[string]int
}

func newPacedOrigin(t *testing.T, files map[string][]byte, perFile time.Duration) *pacedOrigin {
	t.Helper()
	o := &pacedOrigin{files: files, requests: map[string]int{}}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		body, ok := o.files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", fmt.Sprintf("%q", name))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodHead {
			return
		}
		o.mu.Lock()
		o.requests[name]++
		o.inFlight++
		if o.inFlight > o.maxSeen {
			o.maxSeen = o.inFlight
		}
		o.mu.Unlock()
		defer func() {
			o.mu.Lock()
			o.inFlight--
			o.mu.Unlock()
		}()
		const chunks = 10
		step := len(body) / chunks
		for i := 0; i < chunks; i++ {
			end := (i + 1) * step
			if i == chunks-1 {
				end = len(body)
			}
			if _, err := w.Write(body[i*step : end]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(perFile / chunks)
		}
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *pacedOrigin) maxInFlight() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.maxSeen
}

func (o *pacedOrigin) requested(name string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.requests[name]
}

func concurrencyFixture(names ...string) map[string][]byte {
	files := map[string][]byte{}
	for i, n := range names {
		files[n] = []byte(strings.Repeat(string(rune('a'+i)), 64*1024))
	}
	return files
}

func runMultiFile(t *testing.T, script, source, dir, files string, extra ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(),
		"MODEL_SOURCE="+source,
		"CACHE_DIR="+dir,
		"MODEL_FILES="+files,
	)
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestMultiFileConcurrentStaging_Behavioral drives the worker pool (#2364) under
// sh against an origin that counts overlapping transfers, for both HTTP refresh
// policies, since each builds its own per-file body.
func TestMultiFileConcurrentStaging_Behavioral(t *testing.T) {
	requireInitShellEnvironment(t)

	names := []string{"a.bin", "b.bin", "c.bin", "d.bin"}
	for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
		t.Run(policy, func(t *testing.T) {
			t.Run("stages up to the concurrency at once and publishes every file", func(t *testing.T) {
				files := concurrencyFixture(names...)
				o := newPacedOrigin(t, files, 1500*time.Millisecond)
				dir := t.TempDir()

				out, err := runMultiFile(t, buildMultiFileInitCommand(true, false, false, false, true, policy),
					o.srv.URL, dir, strings.Join(names, "\n"), "MODEL_DOWNLOAD_CONCURRENCY=3")
				if err != nil {
					t.Fatalf("init script failed: %v\n%s", err, out)
				}
				// Three dispatch at once; the fourth waits for a free worker.
				if got := o.maxInFlight(); got != 3 {
					t.Errorf("max concurrent transfers = %d, want 3\n%s", got, out)
				}
				for name, want := range files {
					got, err := os.ReadFile(filepath.Join(dir, name))
					if err != nil || string(got) != string(want) {
						t.Errorf("%s not published intact (err %v)\n%s", name, err, out)
					}
				}
				if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) > 0 {
					t.Errorf("partials left after a completed loop: %v", left)
				}
			})

			t.Run("the serial command stages one file at a time", func(t *testing.T) {
				files := concurrencyFixture(names[:3]...)
				o := newPacedOrigin(t, files, 600*time.Millisecond)
				dir := t.TempDir()

				out, err := runMultiFile(t, buildMultiFileInitCommand(true, false, false, false, false, policy),
					o.srv.URL, dir, strings.Join(names[:3], "\n"), "MODEL_DOWNLOAD_CONCURRENCY=3")
				if err != nil {
					t.Fatalf("init script failed: %v\n%s", err, out)
				}
				if got := o.maxInFlight(); got != 1 {
					t.Errorf("max concurrent transfers = %d, want 1", got)
				}
			})

			t.Run("progress lines name their file", func(t *testing.T) {
				files := concurrencyFixture(names[:2]...)
				o := newPacedOrigin(t, files, 2500*time.Millisecond)
				dir := t.TempDir()

				out, err := runMultiFile(t, buildMultiFileInitCommand(true, false, false, false, true, policy),
					o.srv.URL, dir, strings.Join(names[:2], "\n"), "MODEL_DOWNLOAD_CONCURRENCY=2", "PROGRESS_INTERVAL=1")
				if err != nil {
					t.Fatalf("init script failed: %v\n%s", err, out)
				}
				for _, name := range names[:2] {
					if !strings.Contains(out, name+": Downloaded ") {
						t.Errorf("no progress line naming %s\n%s", name, out)
					}
				}
			})

			t.Run("a failed file fails the init, stops dispatch and keeps partials", func(t *testing.T) {
				files := concurrencyFixture("a.bin", "c.bin", "d.bin")
				o := newPacedOrigin(t, files, 1500*time.Millisecond)
				dir := t.TempDir()
				kept := filepath.Join(dir, "old", "dropped.bin.0123456789ab.tmp")
				if err := os.MkdirAll(filepath.Dir(kept), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(kept, []byte("partial"), 0o644); err != nil {
					t.Fatalf("seed partial: %v", err)
				}

				out, err := runMultiFile(t, buildMultiFileInitCommand(true, false, false, false, true, policy),
					o.srv.URL, dir, "missing.bin\na.bin\nc.bin\nd.bin", "MODEL_DOWNLOAD_CONCURRENCY=2")
				if err == nil {
					t.Fatalf("expected the init to fail on missing.bin\n%s", out)
				}
				// missing.bin and a.bin dispatch together; missing.bin fails before a
				// worker frees, so nothing after a.bin is fetched.
				if n := o.requested("c.bin") + o.requested("d.bin"); n != 0 {
					t.Errorf("dispatched %d file(s) after a worker failed\n%s", n, out)
				}
				if got, err := os.ReadFile(filepath.Join(dir, "a.bin")); err != nil || string(got) != string(files["a.bin"]) {
					t.Errorf("a.bin, in flight when missing.bin failed, did not finish (err %v)\n%s", err, out)
				}
				if _, err := os.Stat(kept); err != nil {
					t.Errorf("a failed loop swept a partial it should keep: %v", err)
				}
			})
		})
	}
}

// The pool's workers share the destination directory. Each one sweeps only its
// own stale partials before its transfer: the directory-wide sweep the serial
// loop runs would delete a neighbour's partial in the middle of its transfer.
func TestMultiFileConcurrentSweepSparesOtherFiles(t *testing.T) {
	requireInitShellEnvironment(t)

	files := concurrencyFixture("a.bin")
	o := newPacedOrigin(t, files, 200*time.Millisecond)
	dir := t.TempDir()
	stale := filepath.Join(dir, "a.bin.0123456789ab.tmp")
	neighbour := filepath.Join(dir, "b.bin.0123456789ab.tmp")
	longer := filepath.Join(dir, "a.bin.extra.0123456789ab.tmp")
	for _, p := range []string{stale, neighbour, longer} {
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	// The post-loop sweep would take every *.tmp on success, so read the
	// directory from inside the run: a.bin's worker has swept by the time
	// its transfer starts.
	script := strings.Replace(buildMultiFileInitCommand(true, false, false, false, true, RefreshPolicyIfNotPresent),
		`llmkube_pool_join; }`, `llmkube_pool_join; } && ls "$CACHE_DIR" > "$CACHE_DIR/.listing" && false`, 1)
	out, _ := runMultiFile(t, script, o.srv.URL, dir, "a.bin", "MODEL_DOWNLOAD_CONCURRENCY=2")
	listing, err := os.ReadFile(filepath.Join(dir, ".listing"))
	if err != nil {
		t.Fatalf("no listing written: %v\n%s", err, out)
	}
	have := map[string]bool{}
	for _, f := range strings.Fields(string(listing)) {
		have[f] = true
	}
	if have[filepath.Base(stale)] {
		t.Errorf("a.bin's own stale partial survived its worker's sweep\n%s", listing)
	}
	for _, p := range []string{neighbour, longer} {
		if !have[filepath.Base(p)] {
			t.Errorf("a.bin's worker swept %s, which is not its partial\n%s", filepath.Base(p), listing)
		}
	}
}

// A successful SHA256 gate logs the file, the expected digest, then the
// computed digest with its size and time, and the publish; a cache hit on the
// stamp names the file it skipped. None of it may read as a mismatch to the
// transfer diagnosis.
func TestMultiFileSHA256VerifyLogs(t *testing.T) {
	requireInitShellEnvironment(t)

	files := concurrencyFixture("a.bin")
	sum := sha256.Sum256(files["a.bin"])
	digest := hex.EncodeToString(sum[:])
	o := newPacedOrigin(t, files, 100*time.Millisecond)
	for _, concurrent := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent=%v", concurrent), func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "a.bin")
			script := buildMultiFileInitCommand(true, false, false, true, concurrent, RefreshPolicyIfNotPresent)
			env := []string{"MODEL_FILE_SHA256=" + digest + " a.bin", "MODEL_DIGEST_FIELD=spec.fileSha256", "MODEL_DOWNLOAD_CONCURRENCY=2"}

			out, err := runMultiFile(t, script, o.srv.URL, dir, "a.bin", env...)
			if err != nil {
				t.Fatalf("init script failed: %v\n%s", err, out)
			}
			for _, want := range []string{
				"Verifying the SHA256 of " + dest + " against spec.fileSha256 (expected " + digest + ")...",
				"SHA256 OK for " + dest + ": " + digest + " (0 MiB in ",
				"Published " + dest + " (rename ",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("fresh download log lacks %q\n%s", want, out)
				}
			}
			if f, ok := matchModelTransferFailure(out); ok {
				t.Errorf("a successful verify log classifies as %s\n%s", f.reason, out)
			}

			out, err = runMultiFile(t, script, o.srv.URL, dir, "a.bin", env...)
			if err != nil {
				t.Fatalf("cached rerun failed: %v\n%s", err, out)
			}
			if want := "Model verified against spec.fileSha256 (stamp hit, skipped re-hashing): " + dest; !strings.Contains(out, want) {
				t.Errorf("cached rerun log lacks %q\n%s", want, out)
			}
		})
	}
}

// spec.downloadConcurrency reaches the downloader only above 1: at 1 or unset
// the init container keeps the serial command and env it had before the field
// existed, so a Model that does not use it keeps its pod template.
func TestDownloadConcurrencyWiring(t *testing.T) {
	newModel := func(concurrency int32) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Source:              "hf://org/repo",
				Files:               []string{"a.safetensors", "b.safetensors"},
				DownloadConcurrency: concurrency,
			},
		}
	}
	downloader := func(t *testing.T, m *inferencev1alpha1.Model) (string, map[string]string) {
		t.Helper()
		cfg := buildCachedStorageConfig(m, nil, resolveEffectiveModelCache(nil, m), "", "", "curl:8.18.0", 102, ModelCachePrepAlways, DefaultInitContainerUser, "")
		for _, c := range cfg.initContainers {
			if c.Name == "model-downloader" {
				env := map[string]string{}
				for _, e := range c.Env {
					env[e.Name] = e.Value
				}
				return c.Command[2], env
			}
		}
		t.Fatal("no model-downloader init container")
		return "", nil
	}

	serialCmd, serialEnv := downloader(t, newModel(0))
	if strings.Contains(serialCmd, "llmkube_pool_dispatch") {
		t.Error("an unset downloadConcurrency built the concurrent command")
	}
	if _, ok := serialEnv["MODEL_DOWNLOAD_CONCURRENCY"]; ok {
		t.Error("an unset downloadConcurrency set MODEL_DOWNLOAD_CONCURRENCY")
	}

	oneCmd, oneEnv := downloader(t, newModel(1))
	if oneCmd != serialCmd {
		t.Error("downloadConcurrency 1 changed the downloader command")
	}
	if _, ok := oneEnv["MODEL_DOWNLOAD_CONCURRENCY"]; ok {
		t.Error("downloadConcurrency 1 set MODEL_DOWNLOAD_CONCURRENCY")
	}

	poolCmd, poolEnv := downloader(t, newModel(4))
	if !strings.Contains(poolCmd, "llmkube_pool_dispatch") {
		t.Error("downloadConcurrency 4 did not build the concurrent command")
	}
	if got := poolEnv["MODEL_DOWNLOAD_CONCURRENCY"]; got != "4" {
		t.Errorf("MODEL_DOWNLOAD_CONCURRENCY = %q, want 4", got)
	}
}
