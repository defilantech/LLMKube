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

func TestMultiFileCachedSize(t *testing.T) {
	requireInitShellEnvironment(t)
	for _, kind := range []string{"http", "hf", "s3", "partially-pinned"} {
		for _, state := range []string{"truncated", "oversized", "matching", "empty", "unknown-size", "head-failed", "rate-limited", "offline", "download-failed"} {
			t.Run(kind+"/"+state, func(t *testing.T) { runMultiFileCachedSizeCase(t, kind, state) })
		}
	}
}

func runMultiFileCachedSizeCase(t *testing.T, kind, state string) {
	content := "complete model artifact"
	if state == "empty" {
		content = ""
	}
	cached := multiFileCachedSizeFixture(state, content)
	url, gets := newMultiFileCachedSizeOrigin(t, kind, state, content)
	dest := filepath.Join(t.TempDir(), "model.safetensors")
	if err := os.WriteFile(dest, []byte(cached), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"MODEL_FILES=model.safetensors"}
	if kind == "hf" {
		env = append(env, "HF_TOKEN=test-token")
	}
	if kind == "s3" {
		env = append(env, s3TestEnv(url)...)
	}
	if kind == "partially-pinned" {
		// Cache a pinned second file, leaving the shard unpinned.
		env = []string{"MODEL_FILES=model.safetensors\nconfig.json", "MODEL_FILE_SHA256=" + sha256Hex([]byte(content)) + " config.json"}
		if err := os.WriteFile(filepath.Join(filepath.Dir(dest), "config.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := buildMultiFileInitCommand(true, kind == "s3", kind == "hf", kind == "partially-pinned", RefreshPolicyIfNotPresent)
	out, err := runVerifyScriptEnv(t, script, url, dest, "", env...)
	if state == "download-failed" {
		if err == nil {
			t.Fatalf("accepted a truncated cache after download failure: %s", out)
		}
		return
	}
	if err != nil {
		t.Fatalf("init failed: %v\n%s", err, out)
	}
	if got := readOrFail(t, dest); got != content {
		t.Fatalf("cached artifact = %q, want %q", got, content)
	}
	if state == "unknown-size" || state == "rate-limited" {
		out, err = runVerifyScriptEnv(t, script, url, dest, "", env...)
		if err != nil {
			t.Fatalf("second warm start failed: %v\n%s", err, out)
		}
	}
	assertMultiFileCachedSizeResult(t, state, out, gets.Load())
}

func multiFileCachedSizeFixture(state, content string) string {
	switch state {
	case "matching", "unknown-size", "head-failed", "rate-limited", "offline":
		return content
	case "oversized":
		return content + "extra"
	case "empty":
		return ""
	default:
		return "short"
	}
}

func newMultiFileCachedSizeOrigin(t *testing.T, kind, state, content string) (string, *atomic.Int32) {
	t.Helper()
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kind == "hf" && r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("HF size probe or download omitted the source token")
		}
		if r.Method == http.MethodHead {
			switch state {
			case "rate-limited":
				w.WriteHeader(http.StatusTooManyRequests)
				return
			case "head-failed", "offline":
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if state != "unknown-size" {
				w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		gets.Add(1)
		if state == "download-failed" || state == "offline" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(srv.Close)
	if state == "offline" {
		srv.Close()
	}
	return srv.URL, &gets
}

func assertMultiFileCachedSizeResult(t *testing.T, state, out string, gotGets int32) {
	t.Helper()
	if strings.Contains(out, "test-token") {
		t.Error("source token leaked to downloader output")
	}
	wantGets := int32(1)
	if state == "matching" || state == "empty" || state == "unknown-size" || state == "head-failed" || state == "rate-limited" || state == "offline" {
		wantGets = 0
	}
	if gotGets != wantGets {
		t.Errorf("GET requests = %d, want %d", gotGets, wantGets)
	}
	if (state == "unknown-size" || state == "head-failed" || state == "rate-limited" || state == "offline") && !strings.Contains(out, "keeping cached copy without size validation") {
		t.Errorf("missing warning for an unavailable origin size: %s", out)
	}
}

func TestMultiFileIfNotPresentKeepsRecordedSizeAcrossOriginChange(t *testing.T) {
	requireInitShellEnvironment(t)
	content := "staged revision"
	var origin atomic.Value
	origin.Store(content)
	var heads, gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := origin.Load().(string)
		if r.Method == http.MethodHead {
			heads.Add(1)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			return
		}
		gets.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "model.safetensors")
	script := buildMultiFileInitCommand(true, false, false, false, RefreshPolicyIfNotPresent)
	first, err := runVerifyScriptEnv(t, script, srv.URL, dest, "", "MODEL_FILES=model.safetensors")
	if err != nil {
		t.Fatalf("initial stage failed: %v\n%s", err, first)
	}
	if got := readOrFail(t, dest); got != content {
		t.Fatalf("initial content = %q, want %q", got, content)
	}
	if got, err := os.ReadFile(dest + ".llmkube-size"); err != nil || strings.TrimSpace(string(got)) != strconv.Itoa(len(content)) {
		t.Fatalf("staged size record = %q, %v", got, err)
	}
	origin.Store("replacement from a mutable upstream")
	second, err := runVerifyScriptEnv(t, script, srv.URL, dest, "", "MODEL_FILES=model.safetensors")
	if err != nil {
		t.Fatalf("warm cached start failed: %v\n%s", err, second)
	}
	if got := readOrFail(t, dest); got != "staged revision" {
		t.Fatalf("warm cache changed to %q after the origin changed", got)
	}
	if heads.Load() != 1 || gets.Load() != 1 {
		t.Fatalf("origin requests after two starts = HEAD %d, GET %d; want HEAD 1, GET 1", heads.Load(), gets.Load())
	}
	// The local baseline must still detect a subsequently truncated file.
	origin.Store(content)
	if err := os.WriteFile(dest, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runVerifyScriptEnv(t, script, srv.URL, dest, "", "MODEL_FILES=model.safetensors")
	if err != nil {
		t.Fatalf("repair failed: %v\n%s", err, out)
	}
	if got := readOrFail(t, dest); got != content || gets.Load() != 2 {
		t.Fatalf("truncated cache was not repaired: content %q, GET requests %d", got, gets.Load())
	}
}

func TestMultiFileOnChangeRejectsMismatchedCache(t *testing.T) {
	requireInitShellEnvironment(t)
	for _, s3 := range []bool{false, true} {
		for _, pinned := range []bool{false, true} {
			t.Run(fmt.Sprintf("s3-%v/partially-pinned-%v", s3, pinned), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodHead {
						w.Header().Set("Content-Length", "100")
						return
					}
					w.WriteHeader(http.StatusServiceUnavailable)
				}))
				defer srv.Close()
				dest := filepath.Join(t.TempDir(), "model.safetensors")
				if err := os.WriteFile(dest, []byte("short"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(filepath.Dir(dest), "config.json"), []byte("config"), 0o644); err != nil {
					t.Fatal(err)
				}
				env := []string{"MODEL_FILES=model.safetensors\nconfig.json"}
				if pinned {
					env = append(env, "MODEL_FILE_SHA256="+sha256Hex([]byte("config"))+" config.json")
				}
				if s3 {
					env = append(env, s3TestEnv(srv.URL)...)
				}
				out, err := runVerifyScriptEnv(t, buildMultiFileInitCommand(true, s3, false, pinned, RefreshPolicyOnChange), srv.URL, dest, "", env...)
				if err == nil {
					t.Fatalf("accepted a truncated cache after failed refresh: %s", out)
				}
				if got := readOrFail(t, dest); got != "short" {
					t.Errorf("failed download overwrote cache with %q", got)
				}
			})
		}
	}
}

func TestMultiFilePublishedSize(t *testing.T) {
	requireInitShellEnvironment(t)
	realMV, err := exec.LookPath("mv")
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
		for _, pinned := range []bool{false, true} {
			for _, s3 := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/pinned-%v/s3-%v", policy, pinned, s3), func(t *testing.T) {
					o := newRangeOrigin(t, true)
					dest := filepath.Join(t.TempDir(), "model.safetensors")
					bin := t.TempDir()
					stub := "#!/bin/sh\n\"$REAL_MV\" \"$@\" || exit $?\nif [ \"$2\" = \"$PUBLISH_DEST\" ]; then printf short > \"$2\"; fi\n"
					if err := os.WriteFile(filepath.Join(bin, "mv"), []byte(stub), 0o755); err != nil {
						t.Fatal(err)
					}
					env := []string{"MODEL_FILES=model.safetensors", "PATH=" + bin + ":" + os.Getenv("PATH"), "REAL_MV=" + realMV, "PUBLISH_DEST=" + dest}
					if pinned {
						env = append(env, "MODEL_FILE_SHA256="+sha256Hex(o.content())+" model.safetensors")
					}
					if s3 {
						env = append(env, s3TestEnv(o.srv.URL)...)
					}
					out, err := runVerifyScriptEnv(t, buildMultiFileInitCommand(true, s3, false, pinned, policy), o.srv.URL, dest, "", env...)
					if err == nil {
						t.Fatalf("reported success after a truncated publish: %s", out)
					}
					if _, err := os.Stat(dest); !os.IsNotExist(err) {
						t.Error("truncated publication left a readable destination")
					}
					if _, err := os.Stat(dest + ".sha256"); !os.IsNotExist(err) {
						t.Error("truncated publication retained a valid checksum stamp")
					}
					if _, err := os.Stat(dest + ".llmkube-size"); !os.IsNotExist(err) {
						t.Error("truncated publication retained a staged size record")
					}
				})
			}
		}
	}
}

func TestMultiFileOnChangeOfflineCache(t *testing.T) {
	requireInitShellEnvironment(t)
	for _, kind := range []string{"http", "hf", "s3", "partially-pinned"} {
		for _, state := range []string{"offline", "503"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusServiceUnavailable)
				}))
				t.Cleanup(srv.Close)
				if state == "offline" {
					srv.Close()
				}
				dir := t.TempDir()
				for _, name := range []string{"model.safetensors", "config.json"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("cached"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				env := []string{"MODEL_FILES=model.safetensors\nconfig.json"}
				if kind == "s3" {
					env = append(env, s3TestEnv(srv.URL)...)
				}
				if kind == "partially-pinned" {
					env = append(env, "MODEL_FILE_SHA256="+sha256Hex([]byte("cached"))+" config.json")
				}
				out, err := runVerifyScriptEnv(t, buildMultiFileInitCommand(true, kind == "s3", kind == "hf", kind == "partially-pinned", RefreshPolicyOnChange), srv.URL, filepath.Join(dir, "model.safetensors"), "", env...)
				if err != nil {
					t.Fatalf("offline warm cache failed: %v\n%s", err, out)
				}
				for _, name := range []string{"model.safetensors", "config.json"} {
					if got := readOrFail(t, filepath.Join(dir, name)); got != "cached" {
						t.Errorf("offline cache changed %s: %q", name, got)
					}
				}
				if !strings.Contains(out, "kept cached copy") {
					t.Errorf("missing offline fallback diagnostic: %s", out)
				}
			})
		}
	}
}
