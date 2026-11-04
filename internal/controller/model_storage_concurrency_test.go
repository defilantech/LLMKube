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

// gatedOrigin serves fixed per-file bodies, and serves each one only once the
// test releases that file: a transfer that must not start is proven by the next
// one arriving only after its blocker finished, instead of by a sleep long
// enough for the guess to be true. A GET counts as in flight from the moment it
// arrives, before the gate, which is what the byte cap and the pool size are
// supposed to limit. HEAD answers with the size and an ETag so the resume probe
// derives a real key, and files are served by name under any directory: a
// repository, an S3 bucket, or the root.
//
// A name the fixture did not list answers 404 at once, so a case that reaches
// for a name it never gated fails on the transfer instead of waiting.
type gatedOrigin struct {
	srv      *httptest.Server
	files    map[string][]byte
	mu       sync.Mutex
	gates    map[string]chan struct{}
	inFlight int
	maxSeen  int
	requests map[string]int
	gets     map[string]int
	heads    map[string]int
	// headPaths counts HEADs by full path, getPaths counts GETs by full path;
	// both name the repository.
	headPaths map[string]int
	getPaths  map[string]int
	// noLength leaves Content-Length off HEAD responses; fakeLength replaces it.
	noLength   bool
	fakeLength string
	// stallFirstHead names files whose first HEAD never answers.
	stallFirstHead map[string]bool
}

// newGatedOrigin returns an origin that holds every transfer until the test
// calls release for its file or releaseAll.
func newGatedOrigin(t *testing.T, files map[string][]byte) *gatedOrigin {
	t.Helper()
	o := &gatedOrigin{
		files:          files,
		gates:          map[string]chan struct{}{},
		requests:       map[string]int{},
		gets:           map[string]int{},
		heads:          map[string]int{},
		headPaths:      map[string]int{},
		getPaths:       map[string]int{},
		stallFirstHead: map[string]bool{},
	}
	for name := range files {
		o.gates[name] = make(chan struct{})
	}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				// A code path that stops bounding its own probe leaves this
				// handler waiting for a client that never gives up, and the
				// origin then outlives the test: srv.Close blocks on this
				// connection and the package dies of a timeout dump. Give up on
				// the stall by itself so the case fails an assertion instead.
				select {
				case <-r.Context().Done():
				case <-time.After(20 * time.Second):
				}
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
		o.gets[name]++
		o.getPaths[r.URL.Path]++
		o.inFlight++
		if o.inFlight > o.maxSeen {
			o.maxSeen = o.inFlight
		}
		gate := o.gates[name]
		o.mu.Unlock()
		defer func() {
			o.mu.Lock()
			o.inFlight--
			o.mu.Unlock()
		}()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if _, err := w.Write(body); err != nil {
			return
		}
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *gatedOrigin) maxInFlight() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.maxSeen
}

// inFlightNow is the number of transfers held right now.
func (o *gatedOrigin) inFlightNow() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.inFlight
}

func (o *gatedOrigin) requested(name string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.requests[name]
}

func (o *gatedOrigin) probedPath(p string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.headPaths[p]
}

func (o *gatedOrigin) fetchedPath(p string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.getPaths[p]
}

func (o *gatedOrigin) probed(name string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.heads[name]
}

// release lets one file's transfers through, and every later transfer of it.
func (o *gatedOrigin) release(name string) {
	o.mu.Lock()
	if gate, ok := o.gates[name]; ok {
		close(gate)
		delete(o.gates, name)
	}
	o.mu.Unlock()
}

// await calls cond until it holds or the deadline passes, polling in
// milliseconds: the wait ends at the event, not at a guessed duration.
func (o *gatedOrigin) await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10s for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitInFlight waits until the origin sees n transfers in flight.
func (o *gatedOrigin) awaitInFlight(t *testing.T, n int) {
	t.Helper()
	if !o.eventually(t, func() bool { return o.maxInFlight() >= n }) {
		t.Fatalf("waited 10s for %d transfers in flight (have %d)", n, o.maxInFlight())
	}
}

// awaitGet waits until a transfer of name has arrived.
func (o *gatedOrigin) awaitGet(t *testing.T, name string) {
	t.Helper()
	o.await(t, "a transfer of "+name, func() bool { return o.requested(name) > 0 })
}

// eventually reports whether cond held within the deadline, without failing.
func (o *gatedOrigin) eventually(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

func concurrencyFixture(names ...string) map[string][]byte {
	files := map[string][]byte{}
	for i, n := range names {
		files[n] = []byte(strings.Repeat(string(rune('a'+i)), 64*1024))
	}
	return files
}

// multiFileRun is one generated init command running in the background. The
// command runs in the background because the test drives the origin's gates
// while it runs, so every gate it could still be waiting on has to open when
// the test ends or fails, or a regression would hang the package instead of
// failing an assertion.
type multiFileRun struct {
	done   chan struct{}
	out    strings.Builder
	mu     sync.Mutex
	err    error
	origin *gatedOrigin
}

// startMultiFile starts script against o under sh and returns once it is
// running. Anything the test leaves held is released when the test ends.
func startMultiFile(t *testing.T, script, source, dir, files string, o *gatedOrigin, extra ...string) *multiFileRun {
	t.Helper()
	// A pool that never dispatches its next file would hang the test.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	r := &multiFileRun{done: make(chan struct{}), origin: o}
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
	cmd.Stdout = &lockedWriter{mu: &r.mu, w: &r.out}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start init script: %v", err)
	}
	go func() {
		r.err = cmd.Wait()
		close(r.done)
	}()
	t.Cleanup(func() {
		o.releaseAll()
		<-r.done
	})
	return r
}

// releaseAll opens every fixture file's gate, so no transfer and no held
// worker is left waiting when the test ends.
func (o *gatedOrigin) releaseAll() {
	o.mu.Lock()
	names := make([]string, 0, len(o.files))
	for name := range o.files {
		names = append(names, name)
	}
	o.mu.Unlock()
	for _, name := range names {
		o.release(name)
	}
}

// lockedWriter serializes the command's stdout and stderr into one builder, and
// lets the test read what has been logged so far while the command runs.
type lockedWriter struct {
	mu *sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// output returns what the command has logged so far.
func (r *multiFileRun) output() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out.String()
}

// awaitOutput waits until the command's log contains want.
func (r *multiFileRun) awaitOutput(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(r.output(), want) {
		select {
		case <-r.done:
			t.Fatalf("the command ended before it logged %q:\n%s", want, r.output())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited 10s for the log to carry %q:\n%s", want, r.output())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// wait returns the command's whole output and its exit error.
func (r *multiFileRun) wait(t *testing.T) (string, error) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(90 * time.Second):
		t.Fatalf("the init script did not finish in 90s:\n%s", r.output())
	}
	return r.output(), r.err
}

// runMultiFile runs script against o with every gate open, for a case that
// stages normally and asserts afterwards.
func runMultiFile(t *testing.T, script string, o *gatedOrigin, dir, files string, extra ...string) (string, error) {
	t.Helper()
	o.releaseAll()
	return startMultiFile(t, script, o.srv.URL, dir, files, o, extra...).wait(t)
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

// start starts script against o, with the S3 env pointing s3_curl at it.
func (v poolVariant) start(t *testing.T, script string, o *gatedOrigin, dir, files string, extra ...string) *multiFileRun {
	t.Helper()
	source := o.srv.URL
	if v.s3 {
		source = "s3://bucket"
		extra = append(append(s3TestEnv(o.srv.URL), "S3_PREFIX="), extra...)
	}
	return startMultiFile(t, script, source, dir, files, o, extra...)
}

// run starts script against o with every gate open, and returns its output and
// exit status: for a case that stages normally and asserts afterwards.
func (v poolVariant) run(t *testing.T, script string, o *gatedOrigin, dir, files string, extra ...string) (string, error) {
	t.Helper()
	o.releaseAll()
	return v.start(t, script, o, dir, files, extra...).wait(t)
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
			o := newGatedOrigin(t, files)
			o.stallFirstHead = map[string]bool{"b.bin": true}
			dir := t.TempDir()

			// The dispatcher's probe gives up after MODEL_SIZE_PROBE_TIMEOUT;
			// the shipped default is 30s, shortened here so the case tests the
			// give-up branch instead of outlasting it.
			start := time.Now()
			out, err := v.run(t, v.script(true), o, dir, "a.bin\nb.bin",
				"MODEL_DOWNLOAD_CONCURRENCY=2", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=1048576", "MODEL_SIZE_PROBE_TIMEOUT=2")
			took := time.Since(start)
			if err != nil {
				t.Fatalf("init script failed: %v\n%s", err, out)
			}
			if took > 10*time.Second {
				t.Errorf("a stalled probe took %s to give up at MODEL_SIZE_PROBE_TIMEOUT=2, want under 10s", took.Round(time.Millisecond))
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
				o := newGatedOrigin(t, files)
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
			o := newGatedOrigin(t, files)
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

			// Both files count their full 64 KiB, so under a 100000-byte cap
			// only one can be in flight: hold whichever started until the
			// other has arrived, and the cap is what kept them apart.
			r := v.start(t, v.script(true), o, dir, "a.bin\nb.bin",
				"MODEL_DOWNLOAD_CONCURRENCY=2", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=100000")
			o.await(t, "a transfer", func() bool { return o.maxInFlight() >= 1 })
			first := "a.bin"
			if o.requested("b.bin") > 0 && o.requested("a.bin") == 0 {
				first = "b.bin"
			}
			o.release(first)
			o.awaitGet(t, map[string]string{"a.bin": "b.bin", "b.bin": "a.bin"}[first])
			o.releaseAll()
			out, err := r.wait(t)
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
	o := newGatedOrigin(t, files)
	dir := t.TempDir()

	r := v.start(t, v.script(true), o, dir, strings.Join(names, "\n"), "MODEL_DOWNLOAD_CONCURRENCY=3")
	// Three dispatch at once; the fourth waits for a free worker.
	o.awaitInFlight(t, 3)
	o.awaitGet(t, "a.bin")
	o.awaitGet(t, "b.bin")
	o.awaitGet(t, "c.bin")
	if n := o.requested("d.bin"); n != 0 {
		t.Errorf("d.bin started while three files were held in flight (%d GETs)", n)
	}
	o.release("a.bin")
	if !o.eventually(t, func() bool { return o.requested("d.bin") > 0 }) {
		t.Fatalf("d.bin did not start after a.bin finished, though a worker freed\n%s", r.output())
	}
	o.releaseAll()
	out, err := r.wait(t)
	if err != nil {
		t.Fatalf("init script failed: %v\n%s", err, out)
	}
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
	o := newGatedOrigin(t, files)
	dir := t.TempDir()

	r := v.start(t, v.script(false), o, dir, strings.Join(names[:3], "\n"), "MODEL_DOWNLOAD_CONCURRENCY=3")
	o.awaitGet(t, "a.bin")
	if n := o.requested("b.bin") + o.requested("c.bin"); n != 0 {
		t.Errorf("the serial loop started %d later file(s) while a.bin was held", n)
	}
	o.releaseAll()
	out, err := r.wait(t)
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
	o := newGatedOrigin(t, files)
	dir := t.TempDir()

	// Both transfers are held, so the pool has two in flight while its
	// heartbeat ticks. Each line names its own file and the count.
	r := v.start(t, v.script(true), o, dir, strings.Join(names[:2], "\n"), "MODEL_DOWNLOAD_CONCURRENCY=2", "PROGRESS_INTERVAL=1")
	o.awaitInFlight(t, 2)
	o.awaitGet(t, "a.bin")
	o.awaitGet(t, "b.bin")
	r.awaitOutput(t, "MiB of a.bin (")
	r.awaitOutput(t, "MiB of b.bin (")
	r.awaitOutput(t, "(concurrency: 2)")
	o.releaseAll()
	out, err := r.wait(t)
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
	o := newGatedOrigin(t, files)
	dir := t.TempDir()

	// b.bin finishes first; the count a.bin's lines carry must then fall.
	// S3 reports no total, so its lines carry no percentage either.
	r := v.start(t, v.script(true), o, dir, "a.bin\nb.bin", "MODEL_DOWNLOAD_CONCURRENCY=2", "PROGRESS_INTERVAL=1")
	o.awaitInFlight(t, 2)
	r.awaitOutput(t, "(concurrency: 2)")
	o.release("b.bin")
	o.await(t, "b.bin to finish and leave a.bin alone in flight", func() bool {
		return o.requested("b.bin") > 0 && o.inFlightNow() == 1
	})
	r.awaitOutput(t, "(concurrency: 1)")
	o.releaseAll()
	out, err := r.wait(t)
	if err != nil {
		t.Fatalf("init script failed: %v\n%s", err, out)
	}
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
	o := newGatedOrigin(t, files)
	dir := t.TempDir()
	kept := filepath.Join(dir, "old", "dropped.bin.0123456789ab.tmp")
	if err := os.MkdirAll(filepath.Dir(kept), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(kept, []byte("partial"), 0o644); err != nil {
		t.Fatalf("seed partial: %v", err)
	}

	// missing.bin and a.bin dispatch together; a.bin is held until
	// missing.bin's failure has been logged, so the failure happens with a
	// worker still in flight and nothing after a.bin may be fetched.
	r := v.start(t, v.script(true), o, dir, "missing.bin\na.bin\nc.bin\nd.bin",
		"MODEL_DOWNLOAD_CONCURRENCY=2", "PROGRESS_INTERVAL=1")
	// The worker's own curl stderr is held until the worker ends, but the
	// dispatch-side failure line is printed as it happens; each refresh
	// policy words it its own way.
	failure := "ERROR: failed to download missing.bin"
	if v.policy == RefreshPolicyOnChange {
		failure = "ERROR: model artifact missing.bin could not be validated or downloaded"
	}
	r.awaitOutput(t, failure)
	o.awaitGet(t, "a.bin")
	o.releaseAll()
	out, err := r.wait(t)
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
	// a failure with a worker still in flight leaves no room open: the
	// files after a.bin were never dispatched.
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
	o := newGatedOrigin(t, files)
	dir := t.TempDir()

	// Two files fit 150000 bytes, three do not.
	r := v.start(t, script, o, dir, strings.Join(names, "\n"),
		"MODEL_DOWNLOAD_CONCURRENCY=3", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=150000", "PROGRESS_INTERVAL=1")
	o.awaitInFlight(t, 2)
	o.awaitGet(t, "a.bin")
	o.awaitGet(t, "b.bin")
	// Let the dispatcher settle on c.bin: either a third file is held back
	// (the cap works) or one of c and d started (it does not).
	if !o.eventually(t, func() bool {
		return strings.Contains(r.output(), "Waiting to start c.bin: ") || o.requested("c.bin") > 0 || o.requested("d.bin") > 0
	}) {
		o.releaseAll()
		t.Fatalf("the dispatcher neither held c.bin back nor started it:\n%s", r.output())
	}
	if n := o.requested("c.bin") + o.requested("d.bin"); n != 0 {
		o.releaseAll()
		t.Fatalf("started %d file(s) while two 64 KiB files were held under a 150000-byte cap", n)
	}
	o.release("a.bin")
	o.awaitGet(t, "c.bin")
	o.releaseAll()
	out, err := r.wait(t)
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

// The dispatcher's wait polls in tenths, so a file that leaves the pool frees
// its slot for the next one without a whole second of latency. The fixture
// admits one file at a time: every file but the last is held at the origin
// until the test releases it, and with the cap set to one file's size each
// release is the moment a slot opens. Measured on head, a handoff costs about
// 210ms; with a whole-second poll it costs 1.01s, which is what the bound
// separates.
func TestMultiFileConcurrentPoolHandsOffWithoutAWholeSecondDelay(t *testing.T) {
	requireInitShellEnvironment(t)

	names := make([]string, 12)
	for i := range names {
		names[i] = fmt.Sprintf("f%02d.bin", i)
	}
	files := concurrencyFixture(names...)
	o := newGatedOrigin(t, files)
	dir := t.TempDir()
	v := poolVariant{"http-" + RefreshPolicyIfNotPresent, false, RefreshPolicyIfNotPresent}

	r := v.start(t, v.script(true), o, dir, strings.Join(names, "\n"),
		"MODEL_DOWNLOAD_CONCURRENCY=12", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=65536")
	o.awaitGet(t, names[0])
	for i := 1; i < len(names); i++ {
		start := time.Now()
		o.release(names[i-1])
		o.awaitGet(t, names[i])
		if took := time.Since(start); took > 750*time.Millisecond {
			t.Errorf("handoff %d of %d took %s after the file in flight left its slot, want under 750ms",
				i, len(names)-1, took.Round(time.Millisecond))
		}
	}
	o.releaseAll()
	if _, err := r.wait(t); err != nil {
		t.Fatalf("init script failed: %v\n%s", err, r.output())
	}
}

// stageBySet starts script and asserts the pool holds one file at a time: it
// waits until some file is in flight, checks no second one ever arrives while
// that file is held, releases it, and repeats until every listed file has
// finished. For a case where the pool dispatches every file up front and only
// the cap keeps them apart.
func stageBySet(t *testing.T, v poolVariant, script string, o *gatedOrigin, dir string, names []string, extra ...string) (string, error) {
	t.Helper()
	r := v.start(t, script, o, dir, strings.Join(names, "\n"), extra...)
	deadline := time.Now().Add(20 * time.Second)
	for _, name := range names {
		o.awaitGet(t, name)
		// This file is the one in flight; a second one arriving now is the
		// cap failing.
		if got := o.maxInFlight(); got != 1 {
			t.Errorf("max concurrent transfers = %d, want 1", got)
		}
		o.release(name)
		// A worker kept alive by a transfer that already arrived (or by a
		// regression that never lets one leave) must fail the case, not hang
		// the package: the cleanup opens the rest when the test returns.
		if !o.eventually(t, func() bool { return o.inFlightNow() == 0 }) {
			t.Fatalf("%s did not leave after its gate opened; %d transfer(s) still held", name, o.inFlightNow())
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pool did not stage %s within 20s", name)
		}
	}
	o.releaseAll()
	return r.wait(t)
}

func testCapStagesOversizedFileAlone(t *testing.T, v poolVariant) {
	names := poolFileNames
	script := v.script(true)
	files := concurrencyFixture(names[:3]...)
	o := newGatedOrigin(t, files)
	dir := t.TempDir()

	out, err := stageBySet(t, v, script, o, dir, names[:3],
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
	o := newGatedOrigin(t, files)
	o.fakeLength = "99999999999999999999"
	dir := t.TempDir()

	// An HTTP worker then rejects the body, whose size does not match
	// the one advertised; what matters here is that the pool counted
	// each file as the cap and held b.bin back. a.bin stays held at the
	// origin until the dispatcher has logged that it holds b.bin back, so
	// the check does not depend on how long a.bin's worker takes.
	r := v.start(t, script, o, dir, strings.Join(names[:3], "\n"),
		"MODEL_DOWNLOAD_CONCURRENCY=3", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=1048576")
	r.awaitOutput(t, "Waiting to start b.bin: ")
	o.releaseAll()
	out, _ := r.wait(t)
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
	o := newGatedOrigin(t, files)
	o.noLength = true
	dir := t.TempDir()

	// All three would fit 1 MiB if their sizes were known.
	out, err := stageBySet(t, v, script, o, dir, names[:3],
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
			o := newGatedOrigin(t, files)
			dir := t.TempDir()
			manifest := strings.Join([]string{
				"a.bin\t" + o.srv.URL + "/repoA/a.bin\tplain",
				"b.bin\t" + o.srv.URL + "/repoB/b.bin\tplain",
				"c.bin\ts3://bucket/c.bin\ts3",
				"d.bin\t" + o.srv.URL + "/repoB/d.bin\tplain",
			}, "\n") + "\n"
			env := append(s3TestEnv(o.srv.URL), "MODEL_DOWNLOADS="+manifest,
				"MODEL_DOWNLOAD_CONCURRENCY=3", "MODEL_DOWNLOAD_MAX_INFLIGHT_BYTES=150000")

			// Two 64 KiB files fit 150000 bytes, three do not: hold the first
			// two, and the third arrives only once one of them finished.
			r := startMultiFile(t, multiFileInitCommand(true, false, true, false, true, policy, true),
				o.srv.URL+"/elsewhere", dir, strings.Join(poolFileNames, "\n"), o, env...)
			o.awaitInFlight(t, 2)
			o.awaitGet(t, "a.bin")
			o.awaitGet(t, "b.bin")
			o.release("a.bin")
			o.awaitGet(t, "c.bin")
			o.releaseAll()
			out, err := r.wait(t)
			if err != nil {
				t.Fatalf("init script failed: %v\n%s", err, out)
			}
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
			// The transfers went where the manifest says, not to
			// $MODEL_SOURCE: the origin serves a name under any path, so a
			// fetch from the wrong repository would publish the right bytes.
			for _, p := range []string{"/repoA/a.bin", "/repoB/b.bin", "/bucket/c.bin", "/repoB/d.bin"} {
				if o.fetchedPath(p) == 0 {
					t.Errorf("never fetched %s, the URL its manifest line names\n%s", p, out)
				}
			}
			for _, name := range poolFileNames {
				if n := o.fetchedPath("/elsewhere/" + name); n != 0 {
					t.Errorf("fetched $MODEL_SOURCE/%s %d time(s), which its manifest line does not name\n%s", name, n, out)
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
	o := newGatedOrigin(t, files)
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
	out, _ := runMultiFile(t, script, o, dir, "a.bin\na*b.bin", "MODEL_DOWNLOAD_CONCURRENCY=2")
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
	o := newGatedOrigin(t, files)
	dir := t.TempDir()
	dest := filepath.Join(dir, "a.bin")
	script := buildMultiFileInitCommand(true, false, false, true, true, RefreshPolicyIfNotPresent)
	env := []string{"MODEL_FILE_SHA256=" + digest + " a.bin", "MODEL_DIGEST_FIELD=spec.fileSha256", "MODEL_DOWNLOAD_CONCURRENCY=2"}
	o.releaseAll()

	out, err := runMultiFile(t, script, o, dir, "a.bin", env...)
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

	out, err = runMultiFile(t, script, o, dir, "a.bin", env...)
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

// The dispatcher's size probe must give up after 30 seconds unless a run
// overrides it. A shorter default would time out a legitimately slow HEAD on a
// large model host and count that file as the whole cap; the give-up is now
// env-tunable, so nothing else would notice a changed default.
func TestSizeProbeDefaultsToThirtySeconds(t *testing.T) {
	for _, isS3 := range []bool{false, true} {
		for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
			for _, mixed := range []bool{false, true} {
				if mixed && isS3 {
					continue
				}
				cmd := multiFileInitCommand(true, isS3, false, false, true, policy, mixed)
				var probes int
				for _, seg := range strings.Split(cmd, "--connect-timeout") {
					if strings.HasPrefix(seg, " 10 --max-time ") {
						probes++
					}
				}
				if probes != 1 {
					t.Errorf("s3=%v policy=%s mixed=%v: found %d connect-timeout probes, want 1", isS3, policy, mixed, probes)
					continue
				}
				if want := "--max-time ${MODEL_SIZE_PROBE_TIMEOUT:-30}"; !strings.Contains(cmd, want) {
					t.Errorf("s3=%v policy=%s mixed=%v: the size probe does not carry %q, so its budget default moved", isS3, policy, mixed, want)
				}
			}
		}
	}
}

// spec.downloadConcurrency reaches the downloader only above 1: at 1 or unset
// the init container keeps the serial command and env, so the field adds no
// template change of its own. The downloader's own change still moves every
// staging template, as the cache upgrade note records.
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
