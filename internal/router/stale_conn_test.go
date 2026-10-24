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

package router

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

type connCountKey struct{}

// closesSecondRequestPerConn answers the first request on each connection and
// drops the connection when a second request arrives on it, the way an
// upstream that has just reaped an idle keep-alive connection does.
func closesSecondRequestPerConn(t *testing.T) *httptest.Server {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Context().Value(connCountKey{}).(*atomic.Int32).Add(1) >= 2 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, connCountKey{}, new(atomic.Int32))
	}
	srv.Start()
	return srv
}

func dispatchOK(t *testing.T, d *Dispatcher, b *Backend) {
	t.Helper()
	resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/v1/chat/completions", http.Header{}, []byte(`{"model":"qwen"}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// A reused keep-alive connection the upstream has just closed fails before any
// response byte. That is the connection's fault, not the pod's: the request is
// retried once on a fresh connection and the pod is not quarantined (#2195).
func TestDispatchRetriesAStaleReusedConnection(t *testing.T) {
	srv := closesSecondRequestPerConn(t)
	defer srv.Close()
	cfg, b := singleEndpointBackend(srv.URL)
	d := NewDispatcher(cfg, WithRouterLabel("r-stale"))
	d.SetEndpoints("qwen", []string{srv.URL})
	retries := prommetrics.RouterDispatchRetriesTotal.WithLabelValues("r-stale", "qwen", "stale_connection")
	before := testutil.ToFloat64(retries)

	dispatchOK(t, d, b) // opens the connection the next request reuses
	dispatchOK(t, d, b) // reuses it; the upstream drops it

	if !d.dispatchable(healthKey("qwen", srv.URL)) {
		t.Error("the pod was quarantined for a stale connection")
	}
	if got := testutil.ToFloat64(retries) - before; got != 1 {
		t.Errorf("stale-connection retries = %v, want 1", got)
	}
}

// A pod still listed in the endpoint set is quarantined only after two
// failures in a row; a success in between resets the count.
func TestEndpointQuarantineNeedsTwoConsecutiveFailures(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	cfg, b := singleEndpointBackend(srv.URL)
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})
	key := healthKey("qwen", srv.URL)
	send := func() {
		resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/x", http.Header{}, []byte(`{}`))
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
	}

	fail.Store(true)
	send()
	if !d.dispatchable(key) {
		t.Fatal("one 5xx quarantined the pod")
	}
	fail.Store(false)
	send()
	fail.Store(true)
	send()
	if !d.dispatchable(key) {
		t.Fatal("a success did not reset the failure count")
	}
	send()
	if d.dispatchable(key) {
		t.Fatal("two 5xx in a row did not quarantine the pod")
	}
}

// The proxy must close idle upstream connections before vLLM does (its
// VLLM_HTTP_TIMEOUT_KEEP_ALIVE defaults to 5 s), or it reuses connections the
// upstream is closing.
func TestIdleConnTimeoutBelowVLLMKeepAlive(t *testing.T) {
	if defaultIdleConnTimeout >= 5*time.Second {
		t.Errorf("defaultIdleConnTimeout = %s, want under vLLM's 5s keep-alive", defaultIdleConnTimeout)
	}
}

// The all_backends_failed audit line carries the underlying error, and a
// quarantine is logged at Warn.
func TestAllBackendsFailedAuditCarriesTheErrorAndQuarantineIsLogged(t *testing.T) {
	cfg := &Config{
		Backends:     []Backend{{Name: "local", Tier: "local", Address: "http://127.0.0.1:1"}},
		DefaultRoute: "local",
		Policy:       Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	mux := http.NewServeMux()
	NewProxy(cfg, slog.New(slog.NewTextHandler(&logs, nil))).Mount(mux)
	front := httptest.NewServer(mux)
	defer front.Close()

	for i := 0; i < 2; i++ {
		resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	out := logs.String()
	if !strings.Contains(out, "outcome=all_backends_failed") || !strings.Contains(out, "error=") {
		t.Errorf("audit line lacks the error:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "quarantin") {
		t.Errorf("no Warn log for the quarantine:\n%s", out)
	}
}

// With several idle connections to the pod, all reaped by the upstream, the
// retry must not take another pooled one: it needs a connection of its own.
func TestDispatchStaleRetryUsesAFreshConnection(t *testing.T) {
	// Two first requests are held until both have arrived, so the pool ends
	// up with two idle connections to the pod.
	var arrived atomic.Int32
	both := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Context().Value(connCountKey{}).(*atomic.Int32).Add(1) >= 2 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		if arrived.Add(1) == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(5 * time.Second):
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, connCountKey{}, new(atomic.Int32))
	}
	srv.Start()
	defer srv.Close()
	cfg, b := singleEndpointBackend(srv.URL)
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})

	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() { defer func() { done <- struct{}{} }(); dispatchOK(t, d, b) }()
	}
	<-done
	<-done

	dispatchOK(t, d, b) // reuses one idle connection; the retry must not take the other
}

// The stale-connection retry carries the request's prefill estimate once and
// releases it like any other dispatch.
func TestDispatchStaleRetryReleasesPrefill(t *testing.T) {
	srv := closesSecondRequestPerConn(t)
	defer srv.Close()
	cfg, b := loadAwareBackend(srv.URL)
	d := NewDispatcher(cfg, WithRouterLabel("r-stale-prefill"))
	d.SetEndpoints("qwen", []string{srv.URL})
	retries := prommetrics.RouterDispatchRetriesTotal.WithLabelValues("r-stale-prefill", "qwen", "stale_connection")
	estimated := prommetrics.RouterPrefillTokensEstimatedTotal.WithLabelValues("r-stale-prefill", "qwen", srv.URL)

	for i := 0; i < 2; i++ { // the second reuses the first's connection, which the upstream drops
		resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/v1/chat/completions", http.Header{}, promptBody())
		if err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if got := testutil.ToFloat64(retries); got != 1 {
		t.Fatalf("stale-connection retries = %v, want 1", got)
	}
	if got := prefill(d, srv.URL); got != 0 {
		t.Errorf("prefill after the retried request = %d, want 0", got)
	}
	if got := testutil.ToFloat64(estimated); got != 2000 {
		t.Errorf("estimated prefill total = %v, want 2000 (the retry is not counted twice)", got)
	}
}
