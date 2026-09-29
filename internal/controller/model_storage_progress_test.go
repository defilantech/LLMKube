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

	// run assembles the helper plus a stub transfer and returns combined output.
	run := func(t *testing.T, total, transfer string) string {
		t.Helper()
		dest := filepath.Join(t.TempDir(), "model.partial")
		script := downloadProgressFn + transfer +
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

	t.Run("percent when total is known", func(t *testing.T) {
		out := run(t, "4194304", writer)
		if !strings.Contains(out, "Downloaded 2 MiB of 4 MiB (50%)") {
			t.Errorf("missing progress line; want substring %q in:\n%s", "Downloaded 2 MiB of 4 MiB (50%)", out)
		}
		if strings.ContainsRune(out, '\r') {
			t.Errorf("output carries a carriage return, which kubectl collapses into the prior line:\n%q", out)
		}
	})

	t.Run("bytes-only when total is unknown", func(t *testing.T) {
		out := run(t, "", writer)
		if !strings.Contains(out, "Downloaded 2 MiB") {
			t.Errorf(`missing "Downloaded 2 MiB" in:`+"\n%s", out)
		}
		if strings.Contains(out, "%") {
			t.Errorf("output contains a percent but total was empty:\n%s", out)
		}
	})

	t.Run("missing dest prints 0 MiB and swallows the stat error", func(t *testing.T) {
		out := run(t, "", `fake_transfer() { sleep 2; }`)
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
		out := run(t, "", `fake_transfer() { sleep 1; return 3; }`)
		if !strings.Contains(out, "rc=3") {
			t.Errorf("wait did not carry the transfer's exit status; want rc=3 in:\n%s", out)
		}
	})
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
			return buildModelInitCommand(false, false, true, false, RefreshPolicyIfNotPresent)
		}},
		{"OnChange", func() string { return remoteRevalidateScript(false) }},
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
		{"single-file S3 cached", buildModelInitCommand(false, true, true, false, RefreshPolicyIfNotPresent),
			`download_with_progress "$MODEL_PATH.tmp" "" curl --aws-sigv4`},
		{"single-file S3 uncached", buildModelInitCommand(false, true, false, false, RefreshPolicyIfNotPresent),
			`download_with_progress "$MODEL_PATH.tmp" "" curl --aws-sigv4`},
		{"multi-file HTTP IfNotPresent", buildMultiFileInitCommand(true, false, false, RefreshPolicyIfNotPresent),
			`download_with_progress "$MODEL_PARTIAL" "$remote_size" curl -f -L`},
		{"multi-file HTTP OnChange", buildMultiFileInitCommand(true, false, false, RefreshPolicyOnChange),
			`download_with_progress "$MODEL_PARTIAL" "$remote_size"`},
		{"multi-file S3 IfNotPresent", buildMultiFileInitCommand(true, true, false, RefreshPolicyIfNotPresent),
			`download_with_progress "$dest.tmp" "" curl --aws-sigv4`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.cmd, tc.want) {
				t.Errorf("transfer is not wrapped in download_with_progress; missing %q in:\n%s", tc.want, tc.cmd)
			}
		})
	}
}
