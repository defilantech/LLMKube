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

// TestRemoteRevalidateScript_Behavioral drives the actual OnChange revalidation
// shell (remoteRevalidateScript(false)) against a stub HTTP server. It is the
// regression guard for #1309: the remote-size probe must read Content-Length
// from the HEAD response. A HEAD has no body, so the earlier
// -w '%{size_download}' always reported 0, the size comparison never matched,
// and a warm, unchanged cache was re-downloaded on every restart.
//
// Unlike the ContainSubstring assertions elsewhere in this package (which pass
// on a script whose skip branch can never fire), this test counts actual GET
// requests, so it FAILS if the probe returns 0 and the script downloads a
// size-matching cache. It skips when curl is unavailable (the init image ships
// curlimages/curl 8.x, and CI runners have curl).
func TestRemoteRevalidateScript_Behavioral(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available; skipping behavioral revalidation test")
	}
	// The init-container script uses `stat -c %s` (GNU/busybox). On hosts whose
	// stat lacks -c (macOS/BSD) the local-size read fails and this would false-
	// fail, so skip there: this stays a Linux/CI behavioral guard (the runtime
	// is always the Alpine curlimages/curl image, verified separately).
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("abc"), 0o644); err != nil {
		t.Fatalf("probe file: %v", err)
	}
	if out, err := exec.Command("stat", "-c", "%s", probe).Output(); err != nil || strings.TrimSpace(string(out)) != "3" {
		t.Skip("host stat lacks the -c size format (script targets the busybox/Linux init image)")
	}

	body := []byte(strings.Repeat("x", 4096))
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Advertise Content-Length on every response, including HEAD, exactly
		// as an origin or CDN does. The revalidation probe relies on it.
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		atomic.AddInt32(&gets, 1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	run := func(t *testing.T, cacheContents []byte) (string, int32) {
		t.Helper()
		atomic.StoreInt32(&gets, 0)
		dir := t.TempDir()
		modelPath := filepath.Join(dir, "model.gguf")
		if cacheContents != nil {
			if err := os.WriteFile(modelPath, cacheContents, 0o644); err != nil {
				t.Fatalf("seed cache: %v", err)
			}
		}
		cmd := exec.Command("sh", "-c", remoteRevalidateScript(false))
		cmd.Env = append(os.Environ(),
			"MODEL_SOURCE="+srv.URL+"/model.gguf",
			"MODEL_PATH="+modelPath,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("revalidation script failed: %v\n%s", err, out)
		}
		return string(out), atomic.LoadInt32(&gets)
	}

	t.Run("warm cache with matching size skips the download", func(t *testing.T) {
		out, downloads := run(t, body) // cached size == remote Content-Length
		if downloads != 0 {
			t.Errorf("expected 0 downloads for a size-matching warm cache, got %d "+
				"(the size probe likely returned 0 instead of Content-Length)\n%s", downloads, out)
		}
		if !strings.Contains(out, "skipped download") {
			t.Errorf("expected 'skipped download' in output, got:\n%s", out)
		}
	})

	t.Run("size mismatch re-downloads", func(t *testing.T) {
		out, downloads := run(t, []byte("stale")) // cached size != remote size
		if downloads != 1 {
			t.Errorf("expected exactly 1 download for a size-mismatched cache, got %d\n%s", downloads, out)
		}
		if !strings.Contains(out, "downloaded") {
			t.Errorf("expected 'downloaded' in output, got:\n%s", out)
		}
	})

	t.Run("no cache downloads", func(t *testing.T) {
		out, downloads := run(t, nil)
		if downloads != 1 {
			t.Errorf("expected exactly 1 download when nothing is cached, got %d\n%s", downloads, out)
		}
	})
}
