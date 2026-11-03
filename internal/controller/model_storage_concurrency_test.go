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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
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
	heads    map[string]int
	// headPaths counts HEADs by full path, which names the repository.
	headPaths map[string]int
	// noLength leaves Content-Length off HEAD responses; fakeLength replaces it.
	noLength   bool
	fakeLength string
	// durations overrides the transfer time per file.
	durations map[string]time.Duration
	// stallFirstHead names files whose first HEAD never answers.
	stallFirstHead map[string]bool
}

func newPacedOrigin(t *testing.T, files map[string][]byte, perFile time.Duration) *pacedOrigin {
	t.Helper()
	o := &pacedOrigin{files: files, requests: map[string]int{}, heads: map[string]int{}, headPaths: map[string]int{}}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Files are served by name under any directory: a repository, an S3
		// bucket, or the root.
		name := path.Base(r.URL.Path)
		body, ok := o.files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", fmt.Sprintf("%q", name))
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			o.mu.Lock()
			o.heads[name]++
			o.headPaths[r.URL.Path]++
			stall := o.stallFirstHead[name] && o.heads[name] == 1
			o.mu.Unlock()
			if stall {
				<-r.Context().Done()
				return
			}
			switch {
			case o.fakeLength != "":
				w.Header().Set("Content-Length", o.fakeLength)
			case !o.noLength:
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			}
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
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
		pace := perFile
		if d, ok := o.durations[name]; ok {
			pace = d
		}
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
			time.Sleep(pace / chunks)
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

func (o *pacedOrigin) probedPath(p string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.headPaths[p]
}

func (o *pacedOrigin) probed(name string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.heads[name]
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
	// A pool that never dispatches its next file would hang the test.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	// The pool runs in a pipeline subshell that outlives a killed sh and holds
	// the output pipe; without a delay the test waits on it forever.
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(),
		"MODEL_SOURCE="+source,
		"CACHE_DIR="+dir,
		"MODEL_FILES="+files,
	)
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// poolVariant is one of buildMultiFileInitCommand's four loop variants. Each
// builds its own per-file body and size probe, so the pool's tests run over
// all four. S3 points s3_curl at the origin through s3TestEnv, whose stub
// ignores the signature.
type poolVariant struct {
	name   string
	s3     bool
	policy string
}

// poolFileNames are the fixture files the pool's tests stage, 64 KiB each.
var poolFileNames = []string{"a.bin", "b.bin", "c.bin", "d.bin"}

var poolVariants = []poolVariant{
	{"http-" + RefreshPolicyIfNotPresent, false, RefreshPolicyIfNotPresent},
	{"http-" + RefreshPolicyOnChange, false, RefreshPolicyOnChange},
	{"s3-" + RefreshPolicyIfNotPresent, true, RefreshPolicyIfNotPresent},
	{"s3-" + RefreshPolicyOnChange, true, RefreshPolicyOnChange},
}

func (v poolVariant) script(concurrent bool) string {
	return buildMultiFileInitCommand(true, v.s3, false, false, concurrent, v.policy)
}

func (v poolVariant) run(t *testing.T, script string, o *pacedOrigin, dir, files string, extra ...string) (string, error) {
	t.Helper()
	source := o.srv.URL
	if v.s3 {
		source = "s3://bucket"
		extra = append(append(s3TestEnv(o.srv.URL), "S3_PREFIX="), extra...)
	}
	return runMultiFile(t, script, source, dir, files, extra...)
}

// TestMultiFileConcurrentStaging_Behavioral drives the worker pool (#2364) under
// sh against an origin that counts overlapping transfers, for every loop variant.
func TestMultiFileConcurrentStaging_Behavioral(t *testing.T) {
	requireInitShellEnvironment(t)

	for _, v := range poolVariants {
		t.Run(v.name, func(t *testing.T) {
			t.Run("stages up to the concurrency at once and publishes every file", func(t *testing.T) { testPoolStagesUpToConcurrency(t, v) })

			t.Run("the serial command stages one file at a time", func(t *testing.T) { testPoolSerialCommandStagesOneAtATime(t, v) })

			t.Run("progress lines name their file and the files in flight", func(t *testing.T) { testPoolProgressNamesFileAndCount(t, v) })

			// Once every file is dispatched the pool only joins, and the count
			// must still fall as workers finish.
			t.Run("the count in flight falls as workers finish", func(t *testing.T) { testPoolCountFallsAsWorkersFinish(t, v) })

			t.Run("a failed file fails the init, stops dispatch and keeps partials", func(t *testing.T) { testPoolFailureStopsDispatch(t, v) })
		})
	}
}

// spec.downloadMaxInFlightSize caps the bytes the pool's workers hold, since a
// FUSE cache such as blobfuse2's keeps each open file on the node's disk. Each
// fixture file is 64 KiB.
func TestMultiFileByteCap_Behavioral(t *testing.T) {
	requireInitShellEnvironment(t)

	for _, v := range poolVariants {
		t.Run(v.name, func(t *testing.T) {
			t.Run("holds back a file that would pass the cap", func(t *testing.T) { testCapHoldsBackFile(t, v) })

			t.Run("stages a file larger than the cap alone", func(t *testing.T) { testCapStagesOversizedFileAlone(t, v) })

			t.Run("counts a size too large to compare as the whole cap", func(t *testing.T) { testCapClampsHugeSize(t, v) })

			t.Run("counts an unreported size as the whole cap", func(t *testing.T) { testCapCountsUnknownSize(t, v) })
		})
	}
}

// The dispatcher starts nothing while it probes a size, so an origin that
// never answers must not hold every later file: the probe gives up, and the
// file counts as the whole cap.
func TestMultiFileByteCapProbeTimeout_Behavioral(t *testing.T) {
	requireInitShellEnvironment(t)

	for _, v := range poolVariants {
		if v.policy != RefreshPolicyIfNotPresent {
			continue
		}
		t.Run("gives up on a size probe that never answers/"+v.name, func(t *testing.T) {
			files := concurrencyFixture("a.bin", "b.bin")
			o := newPacedOrigin(t, files, 200*time.Millisecond)
			o.stallFirstHead = map[string]bool{"b.bin": true}
			dir := t.TempDir()

			out, err := v.run(t, v.script(true), o, dir, "a.bin\nb.bin",
				"MODEL_DOWNLOAD_CONCURRENCY=2", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=1048576")
			if err != nil {
				t.Fatalf("init script failed: %v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "b.bin")); err != nil {
				t.Errorf("b.bin not published after its size probe stalled: %v\n%s", err, out)
			}
		})
	}
}

// An IfNotPresent warm start under a cap makes no request for a cached file,
// whether its size record vouches for it or not.
func TestMultiFileByteCapPresentFile_Behavioral(t *testing.T) {
	requireInitShellEnvironment(t)

	for _, v := range poolVariants {
		if v.policy != RefreshPolicyIfNotPresent {
			continue
		}
		for _, record := range []bool{true, false} {
			t.Run(fmt.Sprintf("sizes a present file without a request/%s/record=%v", v.name, record), func(t *testing.T) {
				files := concurrencyFixture("a.bin", "b.bin")
				o := newPacedOrigin(t, files, 200*time.Millisecond)
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "a.bin"), files["a.bin"], 0o644); err != nil {
					t.Fatalf("seed a.bin: %v", err)
				}
				if record {
					if err := os.WriteFile(filepath.Join(dir, "a.bin.llmkube-size"), []byte(strconv.Itoa(len(files["a.bin"]))), 0o644); err != nil {
						t.Fatalf("seed size record: %v", err)
					}
				}

				out, err := v.run(t, v.script(true), o, dir, "a.bin\nb.bin",
					"MODEL_DOWNLOAD_CONCURRENCY=2", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=1048576")
				if err != nil {
					t.Fatalf("init script failed: %v\n%s", err, out)
				}
				if n := o.probed("a.bin") + o.requested("a.bin"); n != 0 {
					t.Errorf("made %d request(s) for the cached a.bin\n%s", n, out)
				}
				if o.probed("b.bin") == 0 {
					t.Errorf("never probed the missing b.bin's size\n%s", out)
				}
			})
		}
	}
}

// A present file the worker replaces is replaced at the origin's size, so
// its size on disk must not be what the cap counts. Each file is 64 KiB and
// the present a.bin is half that: counted by disk, a.bin and b.bin fit
// 100000 bytes together.
func TestMultiFileByteCapReplacedFile_Behavioral(t *testing.T) {
	requireInitShellEnvironment(t)

	for _, v := range poolVariants {
		t.Run("counts a replaced file at its full size/"+v.name, func(t *testing.T) {
			files := concurrencyFixture("a.bin", "b.bin")
			o := newPacedOrigin(t, files, 1500*time.Millisecond)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "a.bin"), files["a.bin"][:32*1024], 0o644); err != nil {
				t.Fatalf("seed a.bin: %v", err)
			}
			// IfNotPresent replaces it because its size record disagrees. OnChange's
			// record agrees with the copy on disk; the origin's size does not.
			record := len(files["a.bin"])
			if v.policy == RefreshPolicyOnChange {
				record = 32 * 1024
			}
			if err := os.WriteFile(filepath.Join(dir, "a.bin.llmkube-size"), []byte(strconv.Itoa(record)), 0o644); err != nil {
				t.Fatalf("seed size record: %v", err)
			}

			out, err := v.run(t, v.script(true), o, dir, "a.bin\nb.bin",
				"MODEL_DOWNLOAD_CONCURRENCY=2", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=100000")
			if err != nil {
				t.Fatalf("init script failed: %v\n%s", err, out)
			}
			if got := o.maxInFlight(); got != 1 {
				t.Errorf("max concurrent transfers = %d, want 1\n%s", got, out)
			}
			if got, err := os.ReadFile(filepath.Join(dir, "a.bin")); err != nil || string(got) != string(files["a.bin"]) {
				t.Errorf("a.bin was not replaced (err %v)\n%s", err, out)
			}
		})
	}
}

func testPoolStagesUpToConcurrency(t *testing.T, v poolVariant) {
	names := poolFileNames
	files := concurrencyFixture(names...)
	o := newPacedOrigin(t, files, 1500*time.Millisecond)
	dir := t.TempDir()

	out, err := v.run(t, v.script(true), o, dir, strings.Join(names, "\n"), "MODEL_DOWNLOAD_CONCURRENCY=3")
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
}

func testPoolSerialCommandStagesOneAtATime(t *testing.T, v poolVariant) {
	names := poolFileNames
	files := concurrencyFixture(names[:3]...)
	o := newPacedOrigin(t, files, 600*time.Millisecond)
	dir := t.TempDir()

	out, err := v.run(t, v.script(false), o, dir, strings.Join(names[:3], "\n"), "MODEL_DOWNLOAD_CONCURRENCY=3")
	if err != nil {
		t.Fatalf("init script failed: %v\n%s", err, out)
	}
	if got := o.maxInFlight(); got != 1 {
		t.Errorf("max concurrent transfers = %d, want 1", got)
	}
}

func testPoolProgressNamesFileAndCount(t *testing.T, v poolVariant) {
	names := poolFileNames
	files := concurrencyFixture(names[:2]...)
	o := newPacedOrigin(t, files, 2500*time.Millisecond)
	dir := t.TempDir()

	out, err := v.run(t, v.script(true), o, dir, strings.Join(names[:2], "\n"), "MODEL_DOWNLOAD_CONCURRENCY=2", "PROGRESS_INTERVAL=1")
	if err != nil {
		t.Fatalf("init script failed: %v\n%s", err, out)
	}
	for _, name := range names[:2] {
		if !strings.Contains(out, "MiB of "+name+" (") {
			t.Errorf("no progress line naming %s\n%s", name, out)
		}
	}
	if !strings.Contains(out, "(concurrency: 2)") {
		t.Errorf("no progress line carries the two files in flight\n%s", out)
	}
	if !strings.Contains(out, "Staging up to 2 files at once\n") {
		t.Errorf("no line naming the pool's size\n%s", out)
	}
}

func testPoolCountFallsAsWorkersFinish(t *testing.T, v poolVariant) {
	files := concurrencyFixture("a.bin", "b.bin")
	o := newPacedOrigin(t, files, 3500*time.Millisecond)
	o.durations = map[string]time.Duration{"b.bin": 300 * time.Millisecond}
	dir := t.TempDir()

	out, err := v.run(t, v.script(true), o, dir, "a.bin\nb.bin", "MODEL_DOWNLOAD_CONCURRENCY=2", "PROGRESS_INTERVAL=1")
	if err != nil {
		t.Fatalf("init script failed: %v\n%s", err, out)
	}
	// S3 reports no total, so its lines carry no percentage.
	fell := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "MiB of a.bin") && strings.HasSuffix(line, "(concurrency: 1)") {
			fell = true
		}
	}
	if !fell {
		t.Errorf("a.bin's progress never reported one file in flight after b.bin finished\n%s", out)
	}
}

func testPoolFailureStopsDispatch(t *testing.T, v poolVariant) {
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

	out, err := v.run(t, v.script(true), o, dir, "missing.bin\na.bin\nc.bin\nd.bin",
		"MODEL_DOWNLOAD_CONCURRENCY=2", "PROGRESS_INTERVAL=1")
	if err == nil {
		t.Fatalf("expected the init to fail on missing.bin\n%s", out)
	}
	// a.bin keeps logging after missing.bin fails; the failure must
	// still be the last thing logged, where the termination message's
	// tail and the transfer diagnosis read it.
	if strings.LastIndex(out, "returned error: 404") < strings.LastIndex(out, "a.bin") {
		t.Errorf("missing.bin's 404 is not logged after a.bin's last line\n%s", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	tail := strings.Join(lines[max(0, len(lines)-3):], "\n")
	if f, ok := matchModelTransferFailure(tail); !ok || f.reason != ReasonModelSourceNotFound {
		t.Errorf("the log's last lines do not classify as %s: %q", ReasonModelSourceNotFound, tail)
	}
	// missing.bin and a.bin dispatch together; missing.bin fails before a
	// worker frees, so nothing after a.bin is fetched.
	if n := o.requested("c.bin") + o.requested("d.bin"); n != 0 {
		t.Errorf("dispatched %d file(s) after a worker failed\n%s", n, out)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "a.bin")); err != nil || string(got) != string(files["a.bin"]) {
		t.Errorf("a.bin, in flight when missing.bin failed, did not finish (err %v)\n%s", err, out)
	}
	// S3 has no resume: its loop sweeps every partial before it starts.
	if _, err := os.Stat(kept); err != nil && !v.s3 {
		t.Errorf("a failed loop swept a partial it should keep: %v", err)
	}
}

func testCapHoldsBackFile(t *testing.T, v poolVariant) {
	names := poolFileNames
	script := v.script(true)
	files := concurrencyFixture(names...)
	o := newPacedOrigin(t, files, 1500*time.Millisecond)
	dir := t.TempDir()

	// Two files fit 150000 bytes, three do not.
	out, err := v.run(t, script, o, dir, strings.Join(names, "\n"),
		"MODEL_DOWNLOAD_CONCURRENCY=3", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=150000", "PROGRESS_INTERVAL=1")
	if err != nil {
		t.Fatalf("init script failed: %v\n%s", err, out)
	}
	if got := o.maxInFlight(); got != 2 {
		t.Errorf("max concurrent transfers = %d, want 2\n%s", got, out)
	}
	if !strings.Contains(out, "Waiting to start c.bin: ") || !strings.Contains(out, "MiB allowed (concurrency: 2)") {
		t.Errorf("no wait line for c.bin with the two files in flight\n%s", out)
	}
	// The progress lines report the pool the cap allows, not the
	// three downloadConcurrency asks for.
	if !strings.Contains(out, "(concurrency: 2)") || strings.Contains(out, "(concurrency: 3)") {
		t.Errorf("progress lines do not report the capped concurrency of 2\n%s", out)
	}
	for name, want := range files {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != string(want) {
			t.Errorf("%s not published intact (err %v)", name, err)
		}
	}
}

func testCapStagesOversizedFileAlone(t *testing.T, v poolVariant) {
	names := poolFileNames
	script := v.script(true)
	files := concurrencyFixture(names[:3]...)
	o := newPacedOrigin(t, files, 600*time.Millisecond)
	dir := t.TempDir()

	out, err := v.run(t, script, o, dir, strings.Join(names[:3], "\n"),
		"MODEL_DOWNLOAD_CONCURRENCY=3", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=32768")
	if err != nil {
		t.Fatalf("init script failed: %v\n%s", err, out)
	}
	if got := o.maxInFlight(); got != 1 {
		t.Errorf("max concurrent transfers = %d, want 1", got)
	}
	for name := range files {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s not published: %v", name, err)
		}
	}
}

func testCapClampsHugeSize(t *testing.T, v poolVariant) {
	names := poolFileNames
	script := v.script(true)
	files := concurrencyFixture(names[:3]...)
	o := newPacedOrigin(t, files, 600*time.Millisecond)
	o.fakeLength = "99999999999999999999"
	dir := t.TempDir()

	// An HTTP worker then rejects the body, whose size does not match
	// the one advertised; what matters here is that the pool counted
	// each file as the cap and held b.bin back.
	out, _ := v.run(t, script, o, dir, strings.Join(names[:3], "\n"),
		"MODEL_DOWNLOAD_CONCURRENCY=3", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=1048576")
	if want := "Waiting to start b.bin: 1 MiB in flight, 1 MiB needed, 1 MiB allowed (concurrency: 1)"; !strings.Contains(out, want) {
		t.Errorf("log lacks %q\n%s", want, out)
	}
	if got := o.maxInFlight(); got != 1 {
		t.Errorf("max concurrent transfers = %d, want 1\n%s", got, out)
	}
}

func testCapCountsUnknownSize(t *testing.T, v poolVariant) {
	names := poolFileNames
	script := v.script(true)
	files := concurrencyFixture(names[:3]...)
	o := newPacedOrigin(t, files, 600*time.Millisecond)
	o.noLength = true
	dir := t.TempDir()

	// All three would fit 1 MiB if their sizes were known.
	out, err := v.run(t, script, o, dir, strings.Join(names[:3], "\n"),
		"MODEL_DOWNLOAD_CONCURRENCY=3", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=1048576")
	if err != nil {
		t.Fatalf("init script failed: %v\n%s", err, out)
	}
	if got := o.maxInFlight(); got != 1 {
		t.Errorf("max concurrent transfers = %d, want 1\n%s", got, out)
	}
}

// A set whose spec.files are URLs in several repositories (#2316) reads
// $MODEL_DOWNLOADS, one destination, URL and auth per line. The pool must
// dispatch from it, and size each file at its own URL with its own auth: a
// probe of $MODEL_SOURCE/<file> would size a file the worker never fetches.
func TestMultiFileConcurrentMixedSources_Behavioral(t *testing.T) {
	requireInitShellEnvironment(t)

	for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
		t.Run(policy, func(t *testing.T) {
			files := concurrencyFixture(poolFileNames...)
			o := newPacedOrigin(t, files, 1500*time.Millisecond)
			dir := t.TempDir()
			manifest := strings.Join([]string{
				"a.bin\t" + o.srv.URL + "/repoA/a.bin\tplain",
				"b.bin\t" + o.srv.URL + "/repoB/b.bin\tplain",
				"c.bin\ts3://bucket/c.bin\ts3",
				"d.bin\t" + o.srv.URL + "/repoB/d.bin\tplain",
			}, "\n") + "\n"
			env := append(s3TestEnv(o.srv.URL), "MODEL_DOWNLOADS="+manifest,
				"MODEL_DOWNLOAD_CONCURRENCY=3", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=150000")

			out, err := runMultiFile(t, multiFileInitCommand(true, false, true, false, true, policy, true),
				o.srv.URL+"/elsewhere", dir, strings.Join(poolFileNames, "\n"), env...)
			if err != nil {
				t.Fatalf("init script failed: %v\n%s", err, out)
			}
			// Two 64 KiB files fit 150000 bytes, three do not.
			if got := o.maxInFlight(); got != 2 {
				t.Errorf("max concurrent transfers = %d, want 2\n%s", got, out)
			}
			for name, want := range files {
				if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != string(want) {
					t.Errorf("%s not published intact (err %v)\n%s", name, err, out)
				}
			}
			for _, p := range []string{"/repoA/a.bin", "/repoB/b.bin", "/bucket/c.bin", "/repoB/d.bin"} {
				if o.probedPath(p) == 0 {
					t.Errorf("never probed %s, where its file is fetched\n%s", p, out)
				}
			}
			for _, name := range poolFileNames {
				if n := o.probedPath("/elsewhere/" + name); n != 0 {
					t.Errorf("probed $MODEL_SOURCE/%s %d time(s), which no worker fetches\n%s", name, n, out)
				}
			}
		})
	}
}

// The pool's workers share the destination directory. Each one sweeps only its
// own stale partials before its transfer: the directory-wide sweep the serial
// loop runs would delete a neighbour's partial in the middle of its transfer.
func TestMultiFileConcurrentSweepSparesOtherFiles(t *testing.T) {
	requireInitShellEnvironment(t)

	// a*b.bin's name is a find pattern that matches aXb.bin; escaped, it does not.
	files := concurrencyFixture("a.bin", "a*b.bin")
	o := newPacedOrigin(t, files, 200*time.Millisecond)
	dir := t.TempDir()
	stale := filepath.Join(dir, "a.bin.0123456789ab.tmp")
	starStale := filepath.Join(dir, "a*b.bin.0123456789ab.tmp")
	neighbour := filepath.Join(dir, "b.bin.0123456789ab.tmp")
	longer := filepath.Join(dir, "a.bin.extra.0123456789ab.tmp")
	lookalike := filepath.Join(dir, "aXb.bin.0123456789ab.tmp")
	for _, p := range []string{stale, starStale, neighbour, longer, lookalike} {
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	// The post-loop sweep would take every *.tmp on success, so read the
	// directory from inside the run: a.bin's worker has swept by the time
	// its transfer starts.
	script := strings.Replace(buildMultiFileInitCommand(true, false, false, false, true, RefreshPolicyIfNotPresent),
		`llmkube_pool_join; }`, `llmkube_pool_join; } && ls "$CACHE_DIR" > "$CACHE_DIR/.listing" && false`, 1)
	out, _ := runMultiFile(t, script, o.srv.URL, dir, "a.bin\na*b.bin", "MODEL_DOWNLOAD_CONCURRENCY=2")
	listing, err := os.ReadFile(filepath.Join(dir, ".listing"))
	if err != nil {
		t.Fatalf("no listing written: %v\n%s", err, out)
	}
	have := map[string]bool{}
	for _, f := range strings.Fields(string(listing)) {
		have[f] = true
	}
	for _, p := range []string{stale, starStale} {
		if have[filepath.Base(p)] {
			t.Errorf("%s survived its own worker's sweep\n%s", filepath.Base(p), listing)
		}
	}
	for _, p := range []string{neighbour, longer, lookalike} {
		if !have[filepath.Base(p)] {
			t.Errorf("a worker swept %s, which is not its partial\n%s", filepath.Base(p), listing)
		}
	}
	for name := range files {
		if !have[name] {
			t.Errorf("%s was not published\n%s", name, listing)
		}
	}
}

// In the concurrent command a successful SHA256 gate logs the file, the
// expected digest, then the computed digest with its size and time, and the
// publish; a cache hit on the stamp names the file it skipped. None of it may
// read as a mismatch to the transfer diagnosis.
func TestMultiFileSHA256VerifyLogs(t *testing.T) {
	requireInitShellEnvironment(t)

	files := concurrencyFixture("a.bin")
	sum := sha256.Sum256(files["a.bin"])
	digest := hex.EncodeToString(sum[:])
	o := newPacedOrigin(t, files, 100*time.Millisecond)
	dir := t.TempDir()
	dest := filepath.Join(dir, "a.bin")
	script := buildMultiFileInitCommand(true, false, false, true, true, RefreshPolicyIfNotPresent)
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
}

// Only the concurrent command logs the SHA256 gates or runs the pool. The
// verify helpers are part of every pinned Model's init command, so either in
// another command would change the pod template of Models that never set
// spec.downloadConcurrency, and roll their pods on an operator upgrade (#1965).
func TestPoolAndVerifyLogsStayInTheConcurrentCommand(t *testing.T) {
	tokens := []string{"llmkube_verify_start", "llmkube_pool_", "of $rel"}
	bs := []bool{true, false}
	cmds := make([]string, 0, 2*2*2*2*2*4+8)
	for _, useCache := range bs {
		for _, isS3 := range bs {
			for _, isHFAuth := range bs {
				for _, withSHA256 := range bs {
					for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
						cmds = append(cmds, buildMultiFileInitCommand(useCache, isS3, isHFAuth, withSHA256, false, policy),
							multiFileInitCommand(useCache, isS3, isHFAuth, withSHA256, false, policy, true))
						for _, isLocal := range bs {
							cmds = append(cmds, buildModelInitCommand(isLocal, isS3, useCache, isHFAuth, withSHA256, policy))
						}
					}
				}
				cmds = append(cmds, remoteRevalidateScript(isHFAuth, isS3, useCache))
			}
		}
	}
	for i, cmd := range cmds {
		for _, tok := range tokens {
			if strings.Contains(cmd, tok) {
				t.Errorf("command %d outside the concurrent pool contains %q", i, tok)
			}
		}
	}
	if pool := buildMultiFileInitCommand(true, false, false, true, true, RefreshPolicyIfNotPresent); !strings.Contains(pool, "llmkube_verify_start") {
		t.Error("the concurrent command does not log its SHA256 gates")
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
	if _, ok := poolEnv["MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES"]; ok {
		t.Error("no downloadMaxInFlightSize set MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES")
	}

	capped := newModel(4)
	capped.Spec.DownloadMaxInFlightSize = "90Gi"
	cappedCmd, cappedEnv := downloader(t, capped)
	if got := cappedEnv["MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES"]; got != "96636764160" {
		t.Errorf("MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES = %q, want 96636764160 (90Gi)", got)
	}
	if cappedCmd != poolCmd {
		t.Error("downloadMaxInFlightSize changed the downloader command; it travels in env only")
	}

	// A set of URLs in several repositories (#2316) goes through the same pool,
	// which then reads $MODEL_DOWNLOADS.
	mixed := newModel(4)
	mixed.Spec.Source = ""
	mixed.Spec.Files = []string{"https://example.com/a/model.gguf", "https://example.org/b/mmproj.gguf"}
	mixedCmd, mixedEnv := downloader(t, mixed)
	if !strings.Contains(mixedCmd, `printf '%s\n' "$MODEL_DOWNLOADS" | { `) || !strings.Contains(mixedCmd, "llmkube_pool_dispatch") {
		t.Error("a mixed set with downloadConcurrency 4 did not dispatch the pool from $MODEL_DOWNLOADS")
	}
	if mixedEnv["MODEL_DOWNLOADS"] == "" || mixedEnv["MODEL_DOWNLOAD_CONCURRENCY"] != "4" {
		t.Errorf("mixed set env lacks MODEL_DOWNLOADS or MODEL_DOWNLOAD_CONCURRENCY=4: %v", mixedEnv)
	}

	serialCapped := newModel(1)
	serialCapped.Spec.DownloadMaxInFlightSize = "90Gi"
	if _, env := downloader(t, serialCapped); len(env) != len(oneEnv) {
		t.Errorf("a cap on a serial Model changed its env: %v", env)
	}
}
