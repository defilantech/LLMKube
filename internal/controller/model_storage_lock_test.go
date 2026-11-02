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
	"bytes"
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
)

func skipWithoutInitScriptTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"curl", "flock", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available; the init script cannot run on this host", tool)
		}
	}
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("abc"), 0o644); err != nil {
		t.Fatalf("probe file: %v", err)
	}
	if out, err := exec.Command("stat", "-c", "%s", probe).Output(); err != nil || strings.TrimSpace(string(out)) != "3" {
		t.Skip("host stat lacks the -c size format (script targets the busybox/Linux init image)")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestModelDownloadLock_Behavioral reproduces the pod churn in #2110: the old
// pod's shell is killed while its curl keeps writing, and the replacement pod
// starts on the same cache directory. The replacement must wait for that curl
// to exit instead of resuming the same partial alongside it.
func TestModelDownloadLock_Behavioral(t *testing.T) {
	skipWithoutInitScriptTools(t)
	for _, tc := range []struct {
		name   string
		script string
		files  string
	}{
		{"single-file", buildModelInitCommand(false, false, true, false, false, RefreshPolicyIfNotPresent), ""},
		{"single-file OnChange", buildModelInitCommand(false, false, true, false, false, RefreshPolicyOnChange), ""},
		{"multi-file", buildMultiFileInitCommand(true, false, false, false, false, RefreshPolicyIfNotPresent), "model.gguf"},
		{"multi-file OnChange", buildMultiFileInitCommand(true, false, false, false, false, RefreshPolicyOnChange), "model.gguf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newRangeOrigin(t, true)
			release := make(chan struct{})
			var releaseOnce sync.Once
			// The first body GET stalls halfway until release, standing in for
			// a large download still in flight on the terminating pod.
			var stalled sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Range") != "" {
					o.handle(w, r)
					return
				}
				first := false
				stalled.Do(func() { first = true })
				if !first {
					o.handle(w, r)
					return
				}
				o.fullFromZero.Add(1)
				body := o.content()
				w.Header().Set("ETag", o.validator())
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				_, _ = w.Write(body[:len(body)/2])
				w.(http.Flusher).Flush()
				<-release
				_, _ = w.Write(body[len(body)/2:])
			}))
			t.Cleanup(srv.Close)
			// Registered after srv.Close so it runs first: Close waits on the
			// stalled handler.
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

			dir := t.TempDir()
			modelPath := filepath.Join(dir, "model.gguf")
			source := srv.URL + "/model.gguf"
			if tc.files != "" {
				source = srv.URL
			}
			// Output goes to files, not pipes: cmd.Wait would otherwise block
			// until the surviving curl closes the pipe it inherited.
			start := func(name string) (*exec.Cmd, func() string) {
				out, err := os.Create(filepath.Join(t.TempDir(), name))
				if err != nil {
					t.Fatalf("output file: %v", err)
				}
				t.Cleanup(func() { _ = out.Close() })
				cmd := exec.Command("sh", "-c", tc.script)
				cmd.Env = append(os.Environ(),
					"MODEL_SOURCE="+source, "MODEL_PATH="+modelPath, "CACHE_DIR="+dir,
					"MODEL_FILES="+tc.files, "PROGRESS_INTERVAL=1")
				cmd.Stdout, cmd.Stderr = out, out
				if err := cmd.Start(); err != nil {
					t.Fatalf("start init script: %v", err)
				}
				return cmd, func() string { b, _ := os.ReadFile(out.Name()); return string(b) }
			}

			old, _ := start("old.log")
			partials := func() []string {
				m, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
				return m
			}
			waitFor(t, "the old pod's partial", func() bool {
				p := partials()
				if len(p) != 1 {
					return false
				}
				fi, err := os.Stat(p[0])
				return err == nil && fi.Size() > 0
			})
			// SIGKILL reaches only the shell; its backgrounded curl survives,
			// as a container's does through the termination grace period.
			_ = old.Process.Kill()
			_ = old.Wait()

			replacement, newOut := start("new.log")
			waitFor(t, "the replacement to report the held lock", func() bool {
				return strings.Contains(newOut(), "Waiting for the download lock held by another pod")
			})
			time.Sleep(300 * time.Millisecond)
			if _, err := os.Stat(modelPath); err == nil {
				t.Fatalf("the replacement published while the old curl still held the lock\n%s", newOut())
			}
			if n := o.rangeRequests.Load(); n != 0 {
				t.Fatalf("the replacement resumed the partial while the old curl was still writing it (%d range GETs)", n)
			}

			releaseOnce.Do(func() { close(release) })
			if err := replacement.Wait(); err != nil {
				t.Fatalf("replacement init script failed: %v\n%s", err, newOut())
			}
			got, err := os.ReadFile(modelPath)
			if err != nil {
				t.Fatalf("published file missing: %v\n%s", err, newOut())
			}
			if !bytes.Equal(got, o.content()) {
				t.Errorf("published %d bytes, want the origin's %d intact", len(got), len(o.content()))
			}
			if _, err := os.Stat(filepath.Join(dir, ".llmkube-download.lock")); err != nil {
				t.Errorf("the lockfile must persist across runs: %v", err)
			}
		})
	}
}

// A partial that is longer than the origin's Content-Length (what two appending
// writers leave behind) must be discarded, never published.
func TestModelDownloadSizeGuard_Behavioral(t *testing.T) {
	skipWithoutInitScriptTools(t)
	for _, tc := range []struct {
		name   string
		script string
		files  string
	}{
		{"single-file", buildModelInitCommand(false, false, true, false, false, RefreshPolicyIfNotPresent), ""},
		{"single-file uncached", buildModelInitCommand(false, false, false, false, false, RefreshPolicyIfNotPresent), ""},
		{"multi-file", buildMultiFileInitCommand(true, false, false, false, false, RefreshPolicyIfNotPresent), "model.gguf"},
		{"multi-file OnChange", buildMultiFileInitCommand(true, false, false, false, false, RefreshPolicyOnChange), "model.gguf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newRangeOrigin(t, true)
			dir := t.TempDir()
			modelPath := filepath.Join(dir, "model.gguf")
			partial := fmt.Sprintf("%s.%s.tmp", modelPath, partialKey(shellValidator(strconv.Itoa(contentALen), `"vA"`)))
			if err := os.WriteFile(partial, bytes.Repeat([]byte("A"), contentALen+1000), 0o644); err != nil {
				t.Fatalf("seed oversized partial: %v", err)
			}
			source := o.srv.URL + "/model.gguf"
			if tc.files != "" {
				source = o.srv.URL
			}
			cmd := exec.Command("sh", "-c", tc.script)
			cmd.Env = append(os.Environ(),
				"MODEL_SOURCE="+source, "MODEL_PATH="+modelPath, "CACHE_DIR="+dir, "MODEL_FILES="+tc.files)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected the oversized partial to fail the init script\n%s", out)
			}
			if !strings.Contains(string(out), "but the origin advertised") {
				t.Errorf("missing size-mismatch error\n%s", out)
			}
			if _, err := os.Stat(modelPath); !os.IsNotExist(err) {
				t.Errorf("an oversized partial was published")
			}
			if _, err := os.Stat(partial); !os.IsNotExist(err) {
				t.Errorf("the oversized partial was kept for the next attempt to resume")
			}
		})
	}
}

// A warm cache this uid cannot write, as the air-gapped import in
// docs/MODEL-CACHE.md can leave behind, must still start: the lock is skipped
// with a warning instead of failing to create the lockfile.
func TestModelDownloadLock_ReadOnlyWarmCache(t *testing.T) {
	skipWithoutInitScriptTools(t)
	if os.Geteuid() == 0 {
		t.Skip("root writes through directory permissions")
	}
	for _, tc := range []struct {
		name   string
		script string
		files  string
	}{
		{"single-file", buildModelInitCommand(false, false, true, false, false, RefreshPolicyIfNotPresent), ""},
		{"multi-file", buildMultiFileInitCommand(true, false, false, false, false, RefreshPolicyIfNotPresent), "model.gguf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			modelPath := filepath.Join(dir, "model.gguf")
			if err := os.WriteFile(modelPath, []byte("cached"), 0o444); err != nil {
				t.Fatalf("seed cached model: %v", err)
			}
			if err := os.Chmod(dir, 0o555); err != nil { //nolint:gosec // a directory needs x to be traversed
				t.Fatalf("chmod cache dir: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) //nolint:gosec // restored so TempDir cleanup can delete it
			cmd := exec.Command("sh", "-c", tc.script)
			cmd.Env = append(os.Environ(),
				"MODEL_SOURCE=http://127.0.0.1:1/model.gguf", "MODEL_PATH="+modelPath, "CACHE_DIR="+dir,
				"MODEL_FILES="+tc.files)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("a read-only warm cache failed the init script: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "skipping the download lock") {
				t.Errorf("missing the skipped-lock warning\n%s", out)
			}
		})
	}
}
