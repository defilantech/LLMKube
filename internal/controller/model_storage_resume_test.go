/*
Copyright 2025.

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
	"sync/atomic"
	"testing"
)

// Resume support (#1765) has to be proven by driving the generated shell, not by
// string matching, for the same reason TestRemoteRevalidateScript_Behavioral
// exists (#1326): a ContainSubstring assertion passes on a curl-flag claim that
// turns out to be dead. These tests run the resume code paths (the
// IfNotPresent branch of buildModelInitCommand, remoteRevalidateScript, and both
// HTTP branches of buildMultiFileInitCommand) under sh against an origin that serves byte ranges, and assert the bytes that
// actually land on disk.
//
// The origin is a stub for the range behaviour an S3 endpoint or a CDN has:
//   - it honours "Range: bytes=N-" with a 206 and a Content-Range, so curl -C -
//     genuinely resumes;
//   - it advertises a version in an ETag (or, in the no-ETag variant, only in
//     Content-Length), so the partial key is derived from a real validator;
//   - a GET with no Range counts as a from-zero transfer. The resume assertions
//     check that the origin was asked for a range rather than the full body,
//     because Go's http.Server writes a small ranged response with an implicit
//     200 status even though we ask it to WriteHeader(206).

const (
	contentALen = 80000
	contentBLen = 140000
)

// rangeOrigin is a versioned, range-serving stub. Version "A" and "B" differ in
// content and size so a spliced file (an A prefix followed by a B suffix) matches
// no valid version's size. The sizes are comfortably over a flush boundary so the
// ranged responses are large enough for net/http to actually emit the 206 status.
type rangeOrigin struct {
	srv           *httptest.Server
	version       atomic.Value // string: "A" or "B"
	sendETag      bool
	ignoreRange   bool         // answer Range with 200 + full body, advertise no Accept-Ranges
	fullFromZero  atomic.Int32 // GETs with no Range header
	rangeRequests atomic.Int32 // GETs with a Range header
}

func newRangeOrigin(t *testing.T, sendETag bool) *rangeOrigin {
	t.Helper()
	o := &rangeOrigin{sendETag: sendETag}
	o.version.Store("A")
	o.srv = httptest.NewServer(http.HandlerFunc(o.handle))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *rangeOrigin) content() []byte {
	if o.version.Load().(string) == "B" {
		return []byte(strings.Repeat("B", contentBLen))
	}
	return []byte(strings.Repeat("A", contentALen))
}

func (o *rangeOrigin) validator() string {
	return `"v` + o.version.Load().(string) + `"`
}

func (o *rangeOrigin) handle(w http.ResponseWriter, r *http.Request) {
	body := o.content()
	full := len(body)
	if o.sendETag {
		w.Header().Set("ETag", o.validator())
	}
	if !o.ignoreRange {
		w.Header().Set("Accept-Ranges", "bytes")
	}

	if rng := r.Header.Get("Range"); rng != "" && !o.ignoreRange {
		start := parseRangeStart(rng)
		if start < 0 || start >= full {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", full))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		o.rangeRequests.Add(1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, full-1, full))
		w.Header().Set("Content-Length", strconv.Itoa(full-start))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[start:])
		return
	}

	// A HEAD or a plain GET is treated as a from-zero transfer. An origin that
	// ignores Range (o.ignoreRange) lands here for a Range request too, with a
	// 200 and the whole body and no Accept-Ranges, which is what JFrog's
	// huggingfaceml endpoint does.
	if r.Method != http.MethodHead {
		o.fullFromZero.Add(1)
	}
	w.Header().Set("Content-Length", strconv.Itoa(full))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

// partialKey mirrors validatorDeriveAndSweep's shell key: first 12 hex of
// sha256($remote_validator). Both the resumePrologue and remoteRevalidateScript
// probes emit that validator with the delimiter-free -w format 'CL...ET...', so
// the test seeds the same exact string. The format is deliberately free of any
// "|" that could appear inside a quoted ETag.
func partialKey(validator string) string {
	sum := sha256.Sum256([]byte(validator))
	return hex.EncodeToString(sum[:])[:12]
}

// shellValidator builds the exact $remote_validator the probes assemble from a
// given ETag and Content-Length: "CL<len>ET<etag>". Pass an empty etag for an
// origin that sends none (the validator then carries only the length).
func shellValidator(contentLength, etag string) string {
	return "CL" + contentLength + "ET" + etag
}

func parseRangeStart(rng string) int {
	// "bytes=N-M" or "bytes=N-"
	if !strings.HasPrefix(rng, "bytes=") {
		return -1
	}
	spec := strings.TrimPrefix(rng, "bytes=")
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return -1
	}
	n, err := strconv.Atoi(spec[:dash])
	if err != nil {
		return -1
	}
	return n
}

// runInitScript runs a generated init script under sh, exactly as the init
// container does, and fails the test on a non-zero exit.
func runInitScript(t *testing.T, script, modelSource, modelPath, cacheDir string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(),
		"MODEL_SOURCE="+modelSource,
		"MODEL_PATH="+modelPath,
		"CACHE_DIR="+cacheDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("init script failed: %v\n%s", err, out)
	}
}

func TestModelDownloadResume_Behavioral(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available; skipping behavioral resume test")
	}
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("abc"), 0o644); err != nil {
		t.Fatalf("probe file: %v", err)
	}
	if out, err := exec.Command("stat", "-c", "%s", probe).Output(); err != nil || strings.TrimSpace(string(out)) != "3" {
		t.Skip("host stat lacks the -c size format (script targets the busybox/Linux init image)")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum not available; the partial key derivation cannot run")
	}

	t.Run("IfNotPresent", func(t *testing.T) {
		testResumeVariant(t, resumeVariant{
			name:   "IfNotPresent",
			script: func() string { return buildModelInitCommand(false, false, true, false, RefreshPolicyIfNotPresent) },
		})
	})
	t.Run("OnChange", func(t *testing.T) {
		testResumeVariant(t, resumeVariant{
			name:   "OnChange",
			script: func() string { return remoteRevalidateScript(false) },
		})
	})
	// The multi-file loop reuses the same resume helpers per file. With
	// MODEL_FILES naming one file, dest is $CACHE_DIR/model.gguf, which is the
	// modelPath every variant seeds and asserts on.
	for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
		t.Run("MultiFile"+policy, func(t *testing.T) {
			t.Setenv("MODEL_FILES", "model.gguf")
			testResumeVariant(t, resumeVariant{
				name:   "MultiFile" + policy,
				script: func() string { return buildMultiFileInitCommand(true, false, false, policy) },
			})
		})
	}
	t.Run("MultiFileSweepsDebrisAfterLoop", testMultiFileSweepsDebrisAfterLoop)
}

// testMultiFileSweepsDebrisAfterLoop pins the #1435 guarantee for the
// multi-file loop, which no longer sweeps before it starts: once every listed
// file is published, a partial belonging to a file no longer in the manifest,
// in a subdirectory the per-file sweep never visits, is removed.
func testMultiFileSweepsDebrisAfterLoop(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	orphan := filepath.Join(dir, "old", "dropped.gguf.0123456789ab.tmp")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(orphan, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed orphan partial: %v", err)
	}
	t.Setenv("MODEL_FILES", "model.gguf")

	runInitScript(t, buildMultiFileInitCommand(true, false, false, RefreshPolicyIfNotPresent), o.srv.URL, "", dir)

	if got, err := os.ReadFile(filepath.Join(dir, "model.gguf")); err != nil || string(got) != string(o.content()) {
		t.Fatalf("model.gguf not published intact (err %v)", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphaned partial survived a completed loop (#1435)")
	}
}

type resumeVariant struct {
	name   string
	script func() string
}

// testResumeVariant drives one generated resume script under sh against a
// range-serving origin and asserts the bytes that land on disk for every state
// the partial can be in.
func testResumeVariant(t *testing.T, v resumeVariant) {
	t.Helper()

	// keyFor derives the partial name the script would compute for a given
	// (size, etag) pair, so the test seeds the exact file the shell will look for.
	keyFor := func(etag string) string {
		return partialKey(shellValidator(strconv.Itoa(contentALen), etag))
	}
	// Splice regression (load-bearing): a partial left by a different
	// content version must be discarded, and the published file must
	// contain no bytes from the stale partial. This is the exact failure
	// mode a source-keyed partial (PR #1766) had: curl -C - sends no
	// If-Range, so without validator keying the new content would append
	// onto the stale bytes and even match the new Content-Length.
	t.Run("stale partial from different content is discarded, no splice", func(t *testing.T) {
		o := newRangeOrigin(t, true)
		o.version.Store("B")
		dir := t.TempDir()
		modelPath := filepath.Join(dir, "model.gguf")

		// Seed a partial keyed on version A's validator, filled with the
		// first 40000 bytes of version-A content (a genuine "in progress"
		// partial). Upstream is now version B. A splice would append B onto
		// these A bytes.
		keyA := keyFor(`"vA"`)
		partialA := fmt.Sprintf("%s.%s.tmp", modelPath, keyA)
		if err := os.WriteFile(partialA, []byte(strings.Repeat("A", 40000)), 0o644); err != nil {
			t.Fatalf("seed stale partial: %v", err)
		}

		runInitScript(t, v.script(), o.srv.URL+"/model.gguf", modelPath, dir)

		got, err := os.ReadFile(modelPath)
		if err != nil {
			t.Fatalf("published file missing: %v", err)
		}
		want := o.content() // version B
		if len(got) != len(want) {
			t.Fatalf("published size = %d, want %d (spliced/partial file?)", len(got), len(want))
		}
		if strings.ContainsRune(string(got), 'A') {
			t.Errorf("published file contains bytes from the stale version-A partial (splice)")
		}
		if string(got) != string(want) {
			t.Errorf("published bytes != version B")
		}
		if _, err := os.Stat(partialA); !os.IsNotExist(err) {
			t.Errorf("the stale version-A partial survived the sweep")
		}
	})

	t.Run("matching partial resumes without re-fetching held bytes", func(t *testing.T) {
		o := newRangeOrigin(t, true)
		dir := t.TempDir()
		modelPath := filepath.Join(dir, "model.gguf")

		etag := `"vA"`
		key := keyFor(etag)
		partial := fmt.Sprintf("%s.%s.tmp", modelPath, key)
		// Seed a partial with the first 4000 bytes of the current content.
		if err := os.WriteFile(partial, []byte(strings.Repeat("A", 4000)), 0o644); err != nil {
			t.Fatalf("seed resumable partial: %v", err)
		}

		o.rangeRequests.Store(0)
		o.fullFromZero.Store(0)
		runInitScript(t, v.script(), o.srv.URL+"/model.gguf", modelPath, dir)

		if o.fullFromZero.Load() != 0 {
			t.Errorf("expected a resumed (ranged) transfer, but the origin served %d from-zero GETs", o.fullFromZero.Load())
		}
		if o.rangeRequests.Load() == 0 {
			t.Errorf("expected the transfer to resume with a Range request; it did not")
		}
		got, err := os.ReadFile(modelPath)
		if err != nil {
			t.Fatalf("published file missing: %v", err)
		}
		if string(got) != string(o.content()) {
			t.Errorf("resumed file bytes are wrong (len %d, want %d)", len(got), len(o.content()))
		}
	})

	t.Run("already-complete partial publishes without a body transfer", func(t *testing.T) {
		o := newRangeOrigin(t, true)
		dir := t.TempDir()
		modelPath := filepath.Join(dir, "model.gguf")

		etag := `"vA"`
		key := keyFor(etag)
		partial := fmt.Sprintf("%s.%s.tmp", modelPath, key)
		if err := os.WriteFile(partial, []byte(strings.Repeat("A", contentALen)), 0o644); err != nil {
			t.Fatalf("seed complete partial: %v", err)
		}

		o.rangeRequests.Store(0)
		o.fullFromZero.Store(0)
		runInitScript(t, v.script(), o.srv.URL+"/model.gguf", modelPath, dir)

		got, err := os.ReadFile(modelPath)
		if err != nil {
			t.Fatalf("publish failed: %v", err)
		}
		if string(got) != strings.Repeat("A", contentALen) {
			t.Errorf("complete partial did not publish intact")
		}
		if o.fullFromZero.Load() != 0 || o.rangeRequests.Load() != 0 {
			t.Errorf("a complete partial should publish without any body GET; full=%d range=%d",
				o.fullFromZero.Load(), o.rangeRequests.Load())
		}
	})

	t.Run("no partial downloads normally", func(t *testing.T) {
		o := newRangeOrigin(t, true)
		dir := t.TempDir()
		modelPath := filepath.Join(dir, "model.gguf")

		runInitScript(t, v.script(), o.srv.URL+"/model.gguf", modelPath, dir)

		got, err := os.ReadFile(modelPath)
		if err != nil {
			t.Fatalf("published file missing: %v", err)
		}
		if string(got) != string(o.content()) {
			t.Errorf("fresh download bytes are wrong")
		}
	})

	// The escape hatch for an upstream that ignores Range (JFrog's
	// huggingfaceml endpoint answers every range request with the whole file and
	// advertises no Accept-Ranges). A seeded partial cannot be resumed there, so
	// it must be dropped and the transfer restarted from zero: `curl -C -` against
	// such a server exits 33, which would fail the init container on every restart
	// and leave the pod CrashLooping until someone deleted the partial by hand.
	// This is the case the mirror auth change routes traffic into.
	t.Run("origin that ignores Range drops the partial and restarts", func(t *testing.T) {
		assertIgnoringOriginRestarts(t, v)
	})

	t.Run("origin with no ETag keys on content-length alone", func(t *testing.T) {
		o := newRangeOrigin(t, false)
		dir := t.TempDir()
		modelPath := filepath.Join(dir, "model.gguf")

		key := keyFor("")
		partial := fmt.Sprintf("%s.%s.tmp", modelPath, key)
		if err := os.WriteFile(partial, []byte(strings.Repeat("A", 3000)), 0o644); err != nil {
			t.Fatalf("seed partial: %v", err)
		}

		o.rangeRequests.Store(0)
		o.fullFromZero.Store(0)
		runInitScript(t, v.script(), o.srv.URL+"/model.gguf", modelPath, dir)

		got, err := os.ReadFile(modelPath)
		if err != nil {
			t.Fatalf("published file missing: %v", err)
		}
		if string(got) != string(o.content()) {
			t.Errorf("no-ETag resume produced wrong bytes")
		}
		if o.rangeRequests.Load() == 0 {
			t.Errorf("expected the content-length-keyed partial to resume with a Range request")
		}
	})

	t.Run("probe failure still downloads a fresh complete file", func(t *testing.T) {
		o := newRangeOrigin(t, true)
		dir := t.TempDir()
		modelPath := filepath.Join(dir, "model.gguf")

		// A leftover .tmp from an unrelated key must be swept and the
		// download proceed to completion even though nothing resumes.
		stale := fmt.Sprintf("%s.deadbeefcafe.tmp", modelPath)
		if err := os.WriteFile(stale, []byte("garbage"), 0o644); err != nil {
			t.Fatalf("seed stale tmp: %v", err)
		}

		runInitScript(t, v.script(), o.srv.URL+"/model.gguf", modelPath, dir)

		got, err := os.ReadFile(modelPath)
		if err != nil {
			t.Fatalf("published file missing: %v", err)
		}
		if string(got) != string(o.content()) {
			t.Errorf("download produced wrong bytes after a leftover partial was swept")
		}
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("unrelated .tmp debris survived the sweep (#1435)")
		}
	})
}

// assertIgnoringOriginRestarts drives one generated script against an origin
// that ignores Range and asserts the bytes that land on disk. Split out of
// testResumeVariant to keep that function's branch count in bounds.
func assertIgnoringOriginRestarts(t *testing.T, v resumeVariant) {
	t.Helper()
	o := newRangeOrigin(t, true)
	o.ignoreRange = true
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")

	key := partialKey(shellValidator(strconv.Itoa(contentALen), `"vA"`))
	partial := fmt.Sprintf("%s.%s.tmp", modelPath, key)
	if err := os.WriteFile(partial, []byte(strings.Repeat("A", 4000)), 0o644); err != nil {
		t.Fatalf("seed partial: %v", err)
	}

	o.rangeRequests.Store(0)
	o.fullFromZero.Store(0)
	runInitScript(t, v.script(), o.srv.URL+"/model.gguf", modelPath, dir)

	got, err := os.ReadFile(modelPath)
	if err != nil {
		t.Fatalf("published file missing: %v", err)
	}
	if string(got) != string(o.content()) {
		t.Errorf("from-zero download bytes are wrong (len %d, want %d)", len(got), len(o.content()))
	}
	if o.fullFromZero.Load() == 0 {
		t.Errorf("expected a from-zero transfer after the partial was dropped")
	}
	if o.rangeRequests.Load() != 0 {
		t.Errorf("the partial was resumed against an origin that cannot serve ranges")
	}
	// A successful transfer mv's the partial onto MODEL_PATH, so nothing may be
	// left behind under the seeded name.
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Errorf("the seeded partial survived the transfer")
	}
}

// TestWarmCacheIfNotPresentMakesNoNetworkRequest pins #1765 requirement 4: the
// IfNotPresent validator probe belongs inside the model-absent branch, so a warm
// cache starts the pod with no request at all. An unconditional probe would add
// a HEAD (and, with no --max-time on the curl, a possible stall on an air-gapped
// node) to a start that previously made none.
func TestWarmCacheIfNotPresentMakesNoNetworkRequest(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", `"vA"`)
		w.Header().Set("Content-Length", "5")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(modelPath, []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed warm cache: %v", err)
	}

	cmd := exec.Command("sh", "-c", buildModelInitCommand(false, false, true, false, RefreshPolicyIfNotPresent))
	cmd.Env = append(os.Environ(),
		"MODEL_SOURCE="+srv.URL+"/model.gguf",
		"MODEL_PATH="+modelPath,
		"CACHE_DIR="+dir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("init script failed: %v\n%s", err, out)
	}
	if requests.Load() != 0 {
		t.Errorf("warm cache issued %d network request(s) before the cache check\n%s", requests.Load(), out)
	}
}
