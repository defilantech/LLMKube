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

package relay

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// testLogger returns a logger that discards output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newIngress starts an httptest TLS server standing in for the metal-agent's
// ingress and returns it along with the base64 SPKI pin of its certificate.
func newIngress(t *testing.T, h http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv, PinFromCert(srv.Certificate())
}

// writeToken writes v (trimmed on read) to a token file in a fresh temp dir
// and returns its path.
func writeToken(t *testing.T, v string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRelay_ForwardsWithTokenAndTarget(t *testing.T) {
	var (
		gotToken, gotTarget, gotAuth, gotPath, gotQuery string
	)
	srv, pin := newIngress(t, func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get(apiv1.HeaderRelayToken)
		gotTarget = r.Header.Get(apiv1.HeaderRelayTarget)
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	tokenFile := writeToken(t, "tok123")
	r, err := New(Config{
		Target:    "default/qwen",
		Upstream:  srv.URL,
		SPKIPin:   pin,
		TokenFile: tokenFile,
	}, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?x=1", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set(apiv1.HeaderRelayTarget, "other/evil")
	req.Header.Set(apiv1.HeaderRelayToken, "forged")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "ok" {
		t.Errorf("client body = %q, want %q", got, "ok")
	}
	if gotToken != "tok123" {
		t.Errorf("ingress saw token %q, want %q", gotToken, "tok123")
	}
	if gotTarget != "default/qwen" {
		t.Errorf("ingress saw target %q, want %q", gotTarget, "default/qwen")
	}
	if gotAuth != "Bearer client-key" {
		t.Errorf("ingress saw Authorization %q, want %q", gotAuth, "Bearer client-key")
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("ingress saw path %q, want %q", gotPath, "/v1/chat/completions")
	}
	if gotQuery != "x=1" {
		t.Errorf("ingress saw query %q, want %q", gotQuery, "x=1")
	}
}

func TestRelay_SetsXForwardedFromClientConnection(t *testing.T) {
	var gotFor, gotHost, gotProto string
	var gotForwarded []string
	srv, pin := newIngress(t, func(w http.ResponseWriter, r *http.Request) {
		gotFor = r.Header.Get("X-Forwarded-For")
		gotHost = r.Header.Get("X-Forwarded-Host")
		gotProto = r.Header.Get("X-Forwarded-Proto")
		gotForwarded = r.Header.Values("Forwarded")
		w.WriteHeader(http.StatusOK)
	})
	r, err := New(Config{
		Target:    "default/qwen",
		Upstream:  srv.URL,
		SPKIPin:   pin,
		TokenFile: writeToken(t, "tok123"),
	}, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://llm.example/v1/chat/completions", nil)
	req.RemoteAddr = "203.0.113.7:41234"
	// A gateway in front of the Service already recorded the original client.
	req.Header.Set("X-Forwarded-For", "198.51.100.9")
	req.Header.Set("Forwarded", "for=192.0.2.1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if want := "198.51.100.9, 203.0.113.7"; gotFor != want {
		t.Errorf("ingress saw X-Forwarded-For %q, want %q (inbound chain plus the relay's peer)", gotFor, want)
	}
	if gotHost != "llm.example" {
		t.Errorf("ingress saw X-Forwarded-Host %q, want the client's Host %q", gotHost, "llm.example")
	}
	if gotProto != "http" {
		t.Errorf("ingress saw X-Forwarded-Proto %q, want %q", gotProto, "http")
	}
	if len(gotForwarded) != 0 {
		t.Errorf("ingress saw Forwarded %q, want it dropped", gotForwarded)
	}
}

func TestRelay_PinMismatchIs502(t *testing.T) {
	var called atomic.Bool
	srv, _ := newIngress(t, func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	})

	zeroPin := base64.StdEncoding.EncodeToString(make([]byte, 32))
	r, err := New(Config{
		Target:    "default/qwen",
		Upstream:  srv.URL,
		SPKIPin:   zeroPin,
		TokenFile: writeToken(t, "tok123"),
	}, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if called.Load() {
		t.Error("ingress handler was called despite pin mismatch")
	}
}

func TestRelay_TokenFileReload(t *testing.T) {
	var lastToken string
	srv, pin := newIngress(t, func(w http.ResponseWriter, r *http.Request) {
		lastToken = r.Header.Get(apiv1.HeaderRelayToken)
		w.WriteHeader(http.StatusOK)
	})

	tokenFile := writeToken(t, "tok-a")
	r, err := New(Config{
		Target:    "default/qwen",
		Upstream:  srv.URL,
		SPKIPin:   pin,
		TokenFile: tokenFile,
	}, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)
	if lastToken != "tok-a" {
		t.Fatalf("first request token = %q, want %q", lastToken, "tok-a")
	}

	if err := os.WriteFile(tokenFile, []byte("tok-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(tokenFile, future, future); err != nil {
		t.Fatal(err)
	}

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if lastToken != "tok-b" {
		t.Errorf("second request token = %q, want %q", lastToken, "tok-b")
	}
}

func TestRelay_MissingTokenFileIs503(t *testing.T) {
	var called atomic.Bool
	srv, pin := newIngress(t, func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	})

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	r, err := New(Config{
		Target:    "default/qwen",
		Upstream:  srv.URL,
		SPKIPin:   pin,
		TokenFile: missing,
	}, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if called.Load() {
		t.Error("ingress handler was called despite missing token file")
	}

	if err := r.Ready(context.Background()); err == nil {
		t.Error("Ready() = nil, want error for missing token file")
	}
}

// TestRelay_StreamsSSE proves the relay flushes bytes to the client as they
// arrive rather than buffering until the upstream response completes. It
// serves the relay behind a real net/http server (httptest.NewServer) and
// reads over a real connection with a real http.Get, rather than a fake
// http.ResponseWriter: a real *http.Server response writer buffers writes
// internally (net/http's bufio.Writer) and only pushes partial data to the
// socket when explicitly flushed. That is exactly the path statusWriter.Flush
// exists for, and exactly what an in-process ResponseRecorder/pipe pair
// cannot exercise, since it has no such buffering to defeat.
func TestRelay_StreamsSSE(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }

	upstream, pin := newIngress(t, func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			// t.Fatal must not run outside the test goroutine.
			t.Error("ResponseWriter does not support flushing")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: 1\n\n"))
		flusher.Flush()
		<-release
		_, _ = w.Write([]byte("data: 2\n\n"))
		flusher.Flush()
	})

	r, err := New(Config{
		Target:    "default/qwen",
		Upstream:  upstream.URL,
		SPKIPin:   pin,
		TokenFile: writeToken(t, "tok123"),
	}, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	front := httptest.NewServer(r)
	// Registered before closeRelease so it runs LAST (t.Cleanup is LIFO):
	// front.Close (and the upstream test server's own Close, registered
	// inside newIngress even earlier) block until their in-flight request
	// finishes, which requires the upstream handler above to be unblocked
	// first. Without this, a failure below (t.Fatal, which unwinds via
	// runtime.Goexit) would leave the upstream handler parked on <-release
	// forever and deadlock teardown.
	t.Cleanup(front.Close)
	t.Cleanup(closeRelease)

	// A bounded client, not the default http.Get: with a broken flush, a
	// real *http.Server buffers the status line and headers together with
	// the body and never puts either on the wire until the handler returns
	// or a buffer fills, so the regression this test targets hangs the GET
	// itself waiting on headers, not just the later body read. Without this
	// timeout that failure mode would hang until go test's own -timeout
	// kills the whole binary instead of failing this test.
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(front.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	lineCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		lineCh <- line
	}()

	select {
	case line := <-lineCh:
		if line != "data: 1\n" {
			t.Fatalf("first line = %q, want %q", line, "data: 1\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first SSE event over a real connection; the relay is not flushing")
	}

	closeRelease()
}

func TestRelay_Ready(t *testing.T) {
	srv, pin := newIngress(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiv1.IngressReadyPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get(apiv1.HeaderRelayToken) != "tok123" || r.Header.Get(apiv1.HeaderRelayTarget) != "default/qwen" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	r, err := New(Config{
		Target:    "default/qwen",
		Upstream:  srv.URL,
		SPKIPin:   pin,
		TokenFile: writeToken(t, "tok123"),
	}, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := r.Ready(context.Background()); err != nil {
		t.Errorf("Ready() = %v, want nil", err)
	}

	srv2, pin2 := newIngress(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	r2, err := New(Config{
		Target:    "default/qwen",
		Upstream:  srv2.URL,
		SPKIPin:   pin2,
		TokenFile: writeToken(t, "tok123"),
	}, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r2.Ready(context.Background()); err == nil {
		t.Error("Ready() = nil, want error for 503 response")
	}
}

func TestNew_Validates(t *testing.T) {
	validPin := base64.StdEncoding.EncodeToString(make([]byte, 32))
	cases := map[string]Config{
		"bad target": {
			Target:    "qwen",
			Upstream:  "https://x:8443",
			SPKIPin:   validPin,
			TokenFile: "/tmp/token",
		},
		"non-https upstream": {
			Target:    "default/qwen",
			Upstream:  "http://x",
			SPKIPin:   validPin,
			TokenFile: "/tmp/token",
		},
		"invalid pin": {
			Target:    "default/qwen",
			Upstream:  "https://x:8443",
			SPKIPin:   "abc",
			TokenFile: "/tmp/token",
		},
		"empty token file": {
			Target:    "default/qwen",
			Upstream:  "https://x:8443",
			SPKIPin:   validPin,
			TokenFile: "",
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg, testLogger()); err == nil {
				t.Errorf("New(%+v) = nil error, want error", cfg)
			}
		})
	}
}
