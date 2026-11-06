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
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Download progress (#1895) is only useful if it actually reaches kubectl
// logs, and the failure it fixes is invisible in a unit test that just checks
// the script contains a flag. These tests drive the generated shell under sh,
// the way the init container runs it: the helper's own branches against a stub
// transfer, and the real per-source scripts against a stub origin that stalls
// mid-stream so a heartbeat line is guaranteed to print.

// progressTestReady skips when the host cannot run the init container's shell
// idiom (the runtime is always the Alpine curlimages/curl image, whose busybox
// stat has -c; a macOS/BSD dev host does not, so this stays a Linux/CI guard,
// matching the resume and revalidate behavioral tests).
func progressTestReady(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available; skipping behavioral progress test")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not available; the cached init scripts lock the cache dir with it")
	}
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("abc"), 0o644); err != nil {
		t.Fatalf("probe file: %v", err)
	}
	if out, err := exec.Command("stat", "-c", "%s", probe).Output(); err != nil || strings.TrimSpace(string(out)) != "3" {
		t.Skip("host stat lacks the -c size format (script targets the busybox/Linux init image)")
	}
}

// TestDownloadWithProgress drives download_with_progress itself. The stub
// transfer writes a known 2 MiB and then outlives one interval, so each
// assertion can pin the exact MiB and percent the heartbeat prints rather than
// merely that some line appeared.
func TestDownloadWithProgress(t *testing.T) {
	progressTestReady(t)

	// labeled is what a pool's workers run, with the two names the pool defines
	// stubbed. Only it polls in tenths; the serial helper's text is what shipped.
	labeled := `rel=m.bin; llmkube_pool_label() { :; }; ` + downloadProgressLabeledFn

	// run assembles a helper plus a stub transfer and returns combined output.
	run := func(t *testing.T, helper, total, transfer string) string {
		t.Helper()
		dest := filepath.Join(t.TempDir(), "model.partial")
		script := helper + transfer +
			` && download_with_progress "$DEST" "$TOTAL" fake_transfer "$DEST"; echo "rc=$?"`
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(),
			"DEST="+dest,
			"TOTAL="+total,
			"PROGRESS_INTERVAL=1",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helper script failed: %v\n%s", err, out)
		}
		return string(out)
	}

	// 2048 * 1024 = 2097152 bytes = exactly 2 MiB.
	const writer = `fake_transfer() { dd if=/dev/zero of="$1" bs=1024 count=2048 2>/dev/null; sleep 2; }`

	// One line per whole interval: a 2s transfer at PROGRESS_INTERVAL=1 prints at
	// t=1 and at t=2, never more. The serial helper races between one and two,
	// because it tests the transfer's death before printing and a stub that ends
	// on the second loses that comparison; the tenths poll prints the interval a
	// transfer ends in, so two is deterministic there.
	for _, v := range []struct {
		name, helper, percent string
		minLines              int
	}{
		{"serial", downloadProgressFn, "Downloaded 2 MiB of 4 MiB (50%)", 1},
		{"labeled", labeled, "Downloaded 2 MiB of 4 MiB of m.bin (50%)", 2},
	} {
		t.Run(v.name, func(t *testing.T) {
			t.Run("percent when total is known", func(t *testing.T) {
				out := run(t, v.helper, "4194304", writer)
				if !strings.Contains(out, v.percent) {
					t.Errorf("missing progress line; want substring %q in:\n%s", v.percent, out)
				}
				if n := strings.Count(out, "Downloaded "); n < v.minLines || n > 2 {
					t.Errorf("printed %d progress lines for a 2s transfer at PROGRESS_INTERVAL=1, want %d to 2:\n%s", n, v.minLines, out)
				}
				if strings.ContainsRune(out, '\r') {
					t.Errorf("output carries a carriage return, which kubectl collapses into the prior line:\n%q", out)
				}
			})

			t.Run("bytes-only when total is unknown", func(t *testing.T) {
				out := run(t, v.helper, "", writer)
				if !strings.Contains(out, "Downloaded 2 MiB") {
					t.Errorf(`missing "Downloaded 2 MiB" in:`+"\n%s", out)
				}
				if strings.Contains(out, "%") {
					t.Errorf("output contains a percent but total was empty:\n%s", out)
				}
			})

			t.Run("missing dest prints 0 MiB and swallows the stat error", func(t *testing.T) {
				out := run(t, v.helper, "", `fake_transfer() { sleep 2; }`)
				if !strings.Contains(out, "Downloaded 0 MiB") {
					t.Errorf(`missing "Downloaded 0 MiB" (the stat fallback):`+"\n%s", out)
				}
				// Empty shell arithmetic is 0, so the value alone does not pin the
				// fallback: `stat -c %s` on a missing partial prints an error to stderr
				// unless the helper redirects it. That redirection is the assertion.
				if strings.Contains(out, "stat:") {
					t.Errorf("the stat fallback leaked an error for a missing partial:\n%s", out)
				}
			})

			t.Run("transfer exit status is preserved", func(t *testing.T) {
				out := run(t, v.helper, "", `fake_transfer() { sleep 1; return 3; }`)
				if !strings.Contains(out, "rc=3") {
					t.Errorf("wait did not carry the transfer's exit status; want rc=3 in:\n%s", out)
				}
			})
		})
	}

	// A transfer that finishes in milliseconds costs milliseconds, not the
	// poll's period: a pool's worker must notice the exit on its next poll (#2386).
	t.Run("returns when the transfer finished in milliseconds", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "model.partial")
		script := labeled + `fake_transfer() { sleep 0.05; } && ` +
			`download_with_progress "$DEST" "" fake_transfer "$DEST"`
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "DEST="+dest, "PROGRESS_INTERVAL=1")
		start := time.Now()
		out, err := cmd.CombinedOutput()
		took := time.Since(start)
		if err != nil {
			t.Fatalf("helper script failed: %v\n%s", err, out)
		}
		if took > 500*time.Millisecond {
			t.Errorf("download_with_progress took %s for a 50ms transfer, want under 500ms", took.Round(time.Millisecond))
		}
	})

	// At the shipped interval one line lands per whole 10 seconds: an 11s
	// transfer prints the 10s tick once, not once per poll in the first
	// second and not ten times at the tick.
	t.Run("one line per tick at the default interval", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "model.partial")
		script := labeled + `fake_transfer() { dd if=/dev/zero of="$1" bs=1024 count=2048 2>/dev/null; sleep 11; } && ` +
			`download_with_progress "$DEST" "" fake_transfer "$DEST"`
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "DEST="+dest)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helper script failed: %v\n%s", err, out)
		}
		if n := strings.Count(string(out), "Downloaded "); n != 1 {
			t.Errorf("printed %d progress lines for an 11s transfer at the default interval, want exactly 1:\n%s", n, out)
		}
	})

	// A transfer that spans several intervals prints one line per interval
	// crossed. The guard counts tenths, so each 10th poll is a boundary; the
	// count comes up short only when per-poll overhead makes counted seconds
	// lag the wall clock, and requiring exactly six across six intervals
	// catches a guard keyed on a stride wider than one poll.
	t.Run("one line per interval across a long transfer", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "model.partial")
		script := labeled + `fake_transfer() { dd if=/dev/zero of="$1" bs=1024 count=2048 2>/dev/null; sleep 6.5; } && ` +
			`download_with_progress "$DEST" "" fake_transfer "$DEST"`
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "DEST="+dest, "PROGRESS_INTERVAL=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helper script failed: %v\n%s", err, out)
		}
		if n := strings.Count(string(out), "Downloaded "); n != 6 {
			t.Errorf("printed %d progress lines for a 6.5s transfer at PROGRESS_INTERVAL=1, want exactly 6:\n%s", n, out)
		}
	})

	// An init image whose sleep has no fractional argument must keep the old
	// one-second rhythm, not spin: the fallback counts the whole second, so a
	// transfer that outlives one interval prints one line per interval.
	t.Run("fractional sleep unavailable keeps the interval", func(t *testing.T) {
		bin := t.TempDir()
		shim := "#!/bin/sh\ncase \"$1\" in *.*) echo 'sleep: invalid number' >&2; exit 1;; esac\nexec \"$REAL_SLEEP\" \"$@\"\n"
		if err := os.WriteFile(filepath.Join(bin, "sleep"), []byte(shim), 0o755); err != nil {
			t.Fatalf("write sleep shim: %v", err)
		}
		realSleep, err := exec.LookPath("sleep")
		if err != nil {
			t.Fatal(err)
		}
		// The shim rejects only the sub-second form the helper asks for;
		// the stub transfer keeps a whole-second sleep of its own.
		dest := filepath.Join(t.TempDir(), "model.partial")
		script := labeled +
			`fake_transfer() { dd if=/dev/zero of="$1" bs=1024 count=2048 2>/dev/null; "$REAL_SLEEP" 2; } && ` +
			`download_with_progress "$DEST" "" fake_transfer "$DEST"`
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(),
			"DEST="+dest,
			"PROGRESS_INTERVAL=1",
			"PATH="+bin+":"+os.Getenv("PATH"),
			"REAL_SLEEP="+realSleep,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helper script failed: %v\n%s", err, out)
		}
		// A 2 s transfer at a 1 s interval is two lines. A hot spin that
		// counted each failed poll would print hundreds.
		if n := strings.Count(string(out), "Downloaded "); n > 3 {
			t.Errorf("printed %d progress lines in 2s at PROGRESS_INTERVAL=1, want at most 3:\n%s", n, out)
		}
		if !strings.Contains(string(out), "Downloaded 2 MiB") {
			t.Errorf("no progress line at all with a one-second fallback:\n%s", out)
		}
	})
}

// The serial command is in the template hash of every Model that stages, so the
// tenths poll stays out of it: a golden can be regenerated without anyone noticing
// that the release now restarts those Models' serving pods.
func TestOnlyConcurrentStagingPollsInTenths(t *testing.T) {
	for _, useCache := range []bool{false, true} {
		for shape, serial := range map[string]string{
			"multi-file":  buildMultiFileInitCommand(useCache, false, true, false, false, "IfNotPresent"),
			"single-file": buildModelInitCommand(false, false, useCache, true, false, "IfNotPresent"),
		} {
			if strings.Contains(serial, "sleep 0.1") {
				t.Errorf("serial %s command (useCache=%v) polls in tenths, which restarts every staging Model's pods on upgrade:\n%s", shape, useCache, serial)
			}
			if !strings.Contains(serial, downloadProgressFn) {
				t.Errorf("serial %s command (useCache=%v) does not carry the serial helper:\n%s", shape, useCache, serial)
			}
		}
		// The pool's own polls are in tenths too, so look for the helper itself.
		concurrent := buildMultiFileInitCommand(useCache, false, true, false, true, "IfNotPresent")
		if !strings.Contains(concurrent, downloadProgressLabeledFn) {
			t.Errorf("concurrent command (useCache=%v) does not carry the labeled helper:\n%s", useCache, concurrent)
		}
	}
	if !strings.Contains(downloadProgressLabeledFn, "sleep 0.1") {
		t.Errorf("labeled download_with_progress lost the tenths poll (#2386):\n%s", downloadProgressLabeledFn)
	}
}

// slowOrigin serves a fixed body but holds the connection open after the first
// half until the test releases it, so a heartbeat line prints deterministically
// instead of racing a sleep. A release that never comes (the falsification
// case: no progress line) is bounded by the test's own watchdog.
type slowOrigin struct {
	srv     *httptest.Server
	release chan struct{}
	once    sync.Once
	body    []byte
}

func newSlowOrigin(t *testing.T, size int) *slowOrigin {
	t.Helper()
	o := &slowOrigin{
		release: make(chan struct{}),
		body:    bytes.Repeat([]byte("x"), size),
	}
	o.srv = httptest.NewServer(http.HandlerFunc(o.handle))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *slowOrigin) releaseOnce() { o.once.Do(func() { close(o.release) }) }

func (o *slowOrigin) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("ETag", `"vA"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(o.body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	half := len(o.body) / 2
	_, _ = w.Write(o.body[:half])
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	<-o.release
	_, _ = w.Write(o.body[half:])
}

// progressLine matches the percent form the heartbeat prints when it knows the
// total. N and the percent must be non-zero: a line that reports no bytes, or
// no percent, is the wrong branch, not a passing heartbeat.
var progressLine = regexp.MustCompile(`Downloaded ([1-9][0-9]*) MiB of ([1-9][0-9]*) MiB \(([1-9][0-9]*)%\)`)

// TestModelDownloadProgress_Behavioral runs the real single-file scripts (the
// IfNotPresent resume transfer and the OnChange revalidation transfer) against
// a stalled origin and asserts a newline progress line reaches the log, with no
// carriage return. It FAILS if the transfer is not wrapped: no line appears and
// the watchdog reports the timeout.
func TestModelDownloadProgress_Behavioral(t *testing.T) {
	progressTestReady(t)

	const size = 5 << 20 // 5 MiB
	variants := []struct {
		name   string
		script func() string
	}{
		{"IfNotPresent", func() string {
			return buildModelInitCommand(false, false, true, false, false, RefreshPolicyIfNotPresent)
		}},
		{"OnChange", func() string { return remoteRevalidateScript(false, false, false) }},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			o := newSlowOrigin(t, size)
			dir := t.TempDir()
			modelPath := filepath.Join(dir, "model.gguf")

			cmd := exec.Command("sh", "-c", v.script())
			cmd.Env = append(os.Environ(),
				"MODEL_SOURCE="+o.srv.URL+"/model.gguf",
				"MODEL_PATH="+modelPath,
				"CACHE_DIR="+dir,
				"PROGRESS_INTERVAL=1",
			)
			pr, pw, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			cmd.Stdout = pw
			cmd.Stderr = pw
			if err := cmd.Start(); err != nil {
				_ = pw.Close()
				_ = pr.Close()
				t.Fatalf("start: %v", err)
			}
			_ = pw.Close()

			var (
				mu    sync.Mutex
				lines []string
			)
			seen := make(chan struct{})
			var seenOnce sync.Once

			go func() {
				sc := bufio.NewScanner(pr)
				sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
				for sc.Scan() {
					line := sc.Text()
					mu.Lock()
					lines = append(lines, line)
					mu.Unlock()
					if strings.Contains(line, "Downloaded ") {
						seenOnce.Do(func() { close(seen) })
					}
				}
			}()

			// Release the stalled origin as soon as a heartbeat line is seen, and
			// unconditionally after 30s so the test terminates even when no line
			// ever appears.
			go func() {
				select {
				case <-seen:
				case <-time.After(30 * time.Second):
				}
				o.releaseOnce()
			}()

			if err := cmd.Wait(); err != nil {
				mu.Lock()
				failOut := strings.Join(lines, "\n")
				mu.Unlock()
				t.Fatalf("init script failed: %v\n%s", err, failOut)
			}
			_ = pr.Close()

			mu.Lock()
			out := strings.Join(lines, "\n")
			mu.Unlock()

			if !progressLine.MatchString(out) {
				t.Fatalf("no newline progress line within the timeout; init container log:\n%s", out)
			}
			if strings.ContainsRune(out, '\r') {
				t.Errorf("init container output carries a carriage return:\n%q", out)
			}
			got, err := os.ReadFile(modelPath)
			if err != nil || len(got) != size {
				t.Errorf("published model wrong: len=%d err=%v want %d", len(got), err, size)
			}
		})
	}
}

// TestDownloadProgressWiredIntoEveryCurlPath covers the paths the behavioral
// tests do not drive end to end (S3 with sigv4, and the multi-file loop). A
// per-branch substring is the cheap guard that the wrapper is actually invoked
// there; the helper's own logic is proven behaviorally above.
func TestDownloadProgressWiredIntoEveryCurlPath(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"single-file S3 cached", buildModelInitCommand(false, true, true, false, false, RefreshPolicyIfNotPresent),
			`download_with_progress "$MODEL_PATH.tmp" "" s3_curl`},
		{"single-file S3 uncached", buildModelInitCommand(false, true, false, false, false, RefreshPolicyIfNotPresent),
			`download_with_progress "$MODEL_PATH.tmp" "" s3_curl`},
		{"multi-file HTTP IfNotPresent", buildMultiFileInitCommand(true, false, false, false, false, RefreshPolicyIfNotPresent),
			`download_with_progress "$MODEL_PARTIAL" "$remote_size" curl -f -L`},
		{"multi-file HTTP OnChange", buildMultiFileInitCommand(true, false, false, false, false, RefreshPolicyOnChange),
			`download_with_progress "$MODEL_PARTIAL" "$remote_size"`},
		{"multi-file S3 IfNotPresent", buildMultiFileInitCommand(true, true, false, false, false, RefreshPolicyIfNotPresent),
			`download_with_progress "$dest.tmp" "" s3_curl`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.cmd, tc.want) {
				t.Errorf("transfer is not wrapped in download_with_progress; missing %q in:\n%s", tc.want, tc.cmd)
			}
		})
	}
}
