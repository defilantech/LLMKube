/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"
	"time"
)

// TestDispatcherHalfOpenRecovery covers the core circuit-breaker
// invariant: MarkUnhealthy quarantines the backend for the configured
// duration, after which IsHealthy returns true again (half-open
// probe), and MarkHealthy clears the quarantine outright.
//
// Regression test for the bug where a single transient timeout
// permanently quarantined a backend and the dispatcher's IsHealthy
// check stayed false forever (the only "back to healthy" path was a
// successful Dispatch, which IsHealthy itself gated).
func TestDispatcherHalfOpenRecovery(t *testing.T) {
	cfg := &Config{Backends: []Backend{{Name: "primary", Tier: "local"}}}

	now := time.Unix(0, 0)
	disp := NewDispatcher(cfg,
		WithQuarantineDuration(100*time.Millisecond),
		WithNowFunc(func() time.Time { return now }),
	)

	if !disp.IsHealthy("primary") {
		t.Fatal("fresh backend should be healthy")
	}

	disp.MarkUnhealthy("primary")
	if disp.IsHealthy("primary") {
		t.Fatal("backend is quarantined; IsHealthy must return false until the window expires")
	}

	// Advance just shy of the window: still quarantined.
	now = now.Add(99 * time.Millisecond)
	if disp.IsHealthy("primary") {
		t.Fatal("inside the quarantine window IsHealthy must remain false")
	}

	// Advance past the window: half-open probe permitted.
	now = now.Add(2 * time.Millisecond)
	if !disp.IsHealthy("primary") {
		t.Fatal("after quarantineDuration IsHealthy must return true so the next dispatch probes")
	}

	// A successful Dispatch would call MarkHealthy. Simulate that and
	// verify quarantine is cleared.
	disp.MarkHealthy("primary")
	if !disp.IsHealthy("primary") {
		t.Fatal("MarkHealthy should clear quarantine")
	}
}

// TestDispatcherRepeatedFailureExtendsQuarantine covers the "still
// dead after probe" case. A probe that hits MarkUnhealthy again must
// push the quarantine deadline forward, not leave the backend
// immediately eligible for another probe.
func TestDispatcherRepeatedFailureExtendsQuarantine(t *testing.T) {
	cfg := &Config{Backends: []Backend{{Name: "flaky", Tier: "cloud"}}}

	now := time.Unix(0, 0)
	disp := NewDispatcher(cfg,
		WithQuarantineDuration(100*time.Millisecond),
		WithNowFunc(func() time.Time { return now }),
	)

	disp.MarkUnhealthy("flaky")
	now = now.Add(150 * time.Millisecond) // past the first window
	if !disp.IsHealthy("flaky") {
		t.Fatal("first quarantine window should have expired")
	}

	// Probe failed; MarkUnhealthy fires again.
	disp.MarkUnhealthy("flaky")
	if disp.IsHealthy("flaky") {
		t.Fatal("a fresh MarkUnhealthy must re-quarantine; backend is not immediately probeable")
	}

	now = now.Add(50 * time.Millisecond) // inside the new window
	if disp.IsHealthy("flaky") {
		t.Fatal("inside the second quarantine window IsHealthy must remain false")
	}

	now = now.Add(60 * time.Millisecond) // past the new window
	if !disp.IsHealthy("flaky") {
		t.Fatal("after the extended window IsHealthy must return true again")
	}
}

// TestDispatcherUnknownBackendIsUnhealthy preserves the pre-existing
// invariant that a name not in the config registers as unhealthy
// (defensive in the controller-restart race where a backend was
// renamed and the proxy hasn't picked up the new config yet).
func TestDispatcherUnknownBackendIsUnhealthy(t *testing.T) {
	disp := NewDispatcher(&Config{})
	if disp.IsHealthy("does-not-exist") {
		t.Error("unknown backend should be reported unhealthy")
	}
}

// TestDispatchCloudTierClosesConnection covers the cloud-tier
// branch added for #459. Cloud backends sit behind LBs that recycle
// idle conns silently; the proxy opts out of keep-alive on every
// outbound cloud request via Connection: close + req.Close=true so
// the next request always opens a fresh TCP conn rather than
// risking a stale-pool 30s stall.
func TestDispatchCloudTierClosesConnection(t *testing.T) {
	var seenConnHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenConnHeader = r.Header.Get("Connection")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := &Config{Backends: []Backend{{
		Name: "cloud-anthropic", Tier: "cloud", Address: srv.URL,
	}}}
	disp := NewDispatcher(cfg)

	resp, err := disp.Dispatch(context.Background(),
		&cfg.Backends[0], http.MethodPost, "/v1/chat/completions",
		http.Header{}, []byte(`{}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	_ = resp.Body.Close()

	// Go's http.Request.Close=true causes the transport to send
	// Connection: close to the upstream. The server sees the literal
	// header value.
	if seenConnHeader != "close" {
		t.Errorf("cloud-tier dispatch should carry Connection: close; server saw %q", seenConnHeader)
	}
}

// TestDispatchLocalTierKeepsAlive is the inverse: local-tier backends
// stay in the keep-alive pool to amortize the handshake. The proxy
// must NOT send Connection: close on local dispatches.
func TestDispatchLocalTierKeepsAlive(t *testing.T) {
	var seenConnHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenConnHeader = r.Header.Get("Connection")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := &Config{Backends: []Backend{{
		Name: "local-qwen", Tier: "local", Address: srv.URL,
	}}}
	disp := NewDispatcher(cfg)

	resp, err := disp.Dispatch(context.Background(),
		&cfg.Backends[0], http.MethodPost, "/v1/chat/completions",
		http.Header{}, []byte(`{}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	_ = resp.Body.Close()

	if seenConnHeader == "close" {
		t.Errorf("local-tier dispatch should reuse keep-alive; got Connection: close")
	}
}

// TestDispatchCloudTierOpensFreshConnPerRequest verifies the
// end-to-end pool-bypass: two back-to-back dispatches to the same
// cloud backend establish two separate TCP connections, while two
// to a local backend reuse one. This is the observable property
// that closes out the "stale conn -> 30s stall" failure mode at
// the protocol level.
func TestDispatchCloudTierOpensFreshConnPerRequest(t *testing.T) {
	cases := []struct {
		tier         string
		wantDistinct int // we expect this many *distinct* local addrs
	}{
		{"cloud", 2},
		{"local", 1},
	}
	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			cfg := &Config{Backends: []Backend{{
				Name: "b", Tier: tc.tier, Address: srv.URL,
			}}}
			disp := NewDispatcher(cfg)

			localAddrs := map[string]struct{}{}
			trace := &httptrace.ClientTrace{
				GotConn: func(info httptrace.GotConnInfo) {
					localAddrs[info.Conn.LocalAddr().String()] = struct{}{}
				},
			}
			for i := 0; i < 2; i++ {
				ctx := httptrace.WithClientTrace(context.Background(), trace)
				resp, err := disp.Dispatch(ctx,
					&cfg.Backends[0], http.MethodPost, "/x",
					http.Header{}, []byte(`{}`))
				if err != nil {
					t.Fatalf("Dispatch %d: %v", i, err)
				}
				// Drain the body before close so Go's transport
				// can hand the conn back to the pool. Without
				// this the local-tier case shows two conns even
				// though keep-alive is allowed, because the
				// transport can't reuse a half-read body.
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}

			if got := len(localAddrs); got != tc.wantDistinct {
				t.Errorf("tier=%s: distinct local addrs = %d, want %d (%v)",
					tc.tier, got, tc.wantDistinct, localAddrs)
			}
		})
	}
}

// TestDispatcherUsesShortIdleConnTimeout pins the package-level
// constant the transport uses so the "expire stale-from-upstream
// conns before they wedge a request" behavior is observable from
// outside the dispatcher.
func TestDispatcherUsesShortIdleConnTimeout(t *testing.T) {
	disp := NewDispatcher(&Config{})
	tr, ok := disp.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", disp.client.Transport)
	}
	if tr.IdleConnTimeout != defaultIdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v", tr.IdleConnTimeout, defaultIdleConnTimeout)
	}
	if defaultIdleConnTimeout > 15*time.Second {
		t.Errorf("defaultIdleConnTimeout = %v; should stay tight (<= 15s) to expire stale upstream conns",
			defaultIdleConnTimeout)
	}
}

// TestDispatcherResponseHeaderTimeoutDefault confirms the 120s
// default is wired into both the dispatcher field and the underlying
// transport. Operators reading the audit log expect the
// `timeoutMs` attribute to reflect this when no per-rule / per-backend
// override applies.
func TestDispatcherResponseHeaderTimeoutDefault(t *testing.T) {
	disp := NewDispatcher(&Config{})
	if got := disp.ResponseHeaderTimeout(); got != defaultResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout() = %v, want %v", got, defaultResponseHeaderTimeout)
	}
	if defaultResponseHeaderTimeout < 60*time.Second {
		t.Errorf("default = %v; too short for long-form LLM generation", defaultResponseHeaderTimeout)
	}
	tr, _ := disp.client.Transport.(*http.Transport)
	if tr.ResponseHeaderTimeout != defaultResponseHeaderTimeout {
		t.Errorf("transport ResponseHeaderTimeout = %v, want %v",
			tr.ResponseHeaderTimeout, defaultResponseHeaderTimeout)
	}
}

// TestDispatcherResponseHeaderTimeoutOverride covers the
// WithResponseHeaderTimeout option used by cmd/router-proxy main.go
// to thread the --response-header-timeout CLI flag through.
func TestDispatcherResponseHeaderTimeoutOverride(t *testing.T) {
	disp := NewDispatcher(&Config{}, WithResponseHeaderTimeout(7*time.Second))
	if got := disp.ResponseHeaderTimeout(); got != 7*time.Second {
		t.Errorf("ResponseHeaderTimeout() = %v, want 7s", got)
	}
	tr, _ := disp.client.Transport.(*http.Transport)
	if tr.ResponseHeaderTimeout != 7*time.Second {
		t.Errorf("transport ResponseHeaderTimeout = %v, want 7s", tr.ResponseHeaderTimeout)
	}
}

// TestDispatchContextDeadlineDoesNotQuarantine covers the #462
// regression: a per-attempt context deadline is a rule-level policy
// decision, not a backend-health signal. The dispatcher must NOT
// MarkUnhealthy on context.DeadlineExceeded so a strict rule's
// timeout doesn't poison a lenient sibling rule targeting the same
// backend.
func TestDispatchContextDeadlineDoesNotQuarantine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Sleep longer than any caller's deadline. The dispatcher
		// should give up via context.WithTimeout but leave us alive.
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := &Config{Backends: []Backend{{
		Name: "slow-but-alive", Tier: "local", Address: srv.URL,
	}}}
	disp := NewDispatcher(cfg)

	// Tight deadline forces context.DeadlineExceeded.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	resp, err := disp.Dispatch(ctx, &cfg.Backends[0],
		http.MethodPost, "/x", http.Header{}, []byte(`{}`))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("Dispatch should have returned the deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded chain", err)
	}

	// The backend must still be reported healthy: another rule with
	// a generous budget should be free to dispatch right now.
	if !disp.IsHealthy("slow-but-alive") {
		t.Error("backend marked unhealthy on deadline-exceeded; sibling rules will starve until quarantine expires")
	}
}

// TestDispatchContextCanceledDoesNotQuarantine pins the same
// invariant for the context.Canceled case (inbound client
// disconnects mid-dispatch). Same reasoning: the backend didn't
// fail, the upstream caller went away.
func TestDispatchContextCanceledDoesNotQuarantine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{Backends: []Backend{{
		Name: "cancellable", Tier: "local", Address: srv.URL,
	}}}
	disp := NewDispatcher(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel the moment we send the request.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	resp, err := disp.Dispatch(ctx, &cfg.Backends[0],
		http.MethodPost, "/x", http.Header{}, []byte(`{}`))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("Dispatch should have returned the cancel error")
	}

	if !disp.IsHealthy("cancellable") {
		t.Error("backend marked unhealthy on context.Canceled; client disconnect is not a backend signal")
	}
}

// TestDispatchConnectionFailureStillQuarantines confirms the
// dispatcher still treats genuine connection failures (closed
// server, unreachable host) as a backend-health signal and
// quarantines. Distinguishes the deadline-exception change above
// from a blanket "never mark unhealthy" regression.
func TestDispatchConnectionFailureStillQuarantines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	addr := srv.URL
	srv.Close() // shut it down so the next dial fails

	cfg := &Config{Backends: []Backend{{
		Name: "dead", Tier: "local", Address: addr,
	}}}
	disp := NewDispatcher(cfg)

	resp, err := disp.Dispatch(context.Background(), &cfg.Backends[0],
		http.MethodPost, "/x", http.Header{}, []byte(`{}`))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("Dispatch should have failed against a closed server")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("unexpected deadline error; want a real connect/dial error")
	}
	if disp.IsHealthy("dead") {
		t.Error("backend should be marked unhealthy after a connect failure")
	}
}

// TestApplyModelOverride is the unit-level contract: rewrite only when the
// backend declares a Model; pass everything else through untouched.
func TestApplyModelOverride(t *testing.T) {
	modelOf := func(t *testing.T, body []byte) string {
		t.Helper()
		var m struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("result is not JSON: %v (%s)", err, body)
		}
		return m.Model
	}

	t.Run("rewrites existing model", func(t *testing.T) {
		out := applyModelOverride([]byte(`{"model":"opus","messages":[]}`), "claude-opus-4-7")
		if got := modelOf(t, out); got != "claude-opus-4-7" {
			t.Errorf("model = %q, want claude-opus-4-7", got)
		}
	})

	t.Run("preserves sibling fields", func(t *testing.T) {
		out := applyModelOverride([]byte(`{"model":"opus","temperature":0.5,"stream":true}`), "x")
		var m struct {
			Temperature float64 `json:"temperature"`
			Stream      bool    `json:"stream"`
		}
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if m.Temperature != 0.5 || !m.Stream {
			t.Errorf("sibling fields not preserved: %s", out)
		}
	})

	t.Run("injects model when absent", func(t *testing.T) {
		out := applyModelOverride([]byte(`{"messages":[]}`), "sonnet")
		if got := modelOf(t, out); got != "sonnet" {
			t.Errorf("model = %q, want sonnet (injected)", got)
		}
	})

	t.Run("empty Model is a no-op", func(t *testing.T) {
		in := `{"model":"opus"}`
		if out := applyModelOverride([]byte(in), ""); string(out) != in {
			t.Errorf("empty Model rewrote body to %q; want unchanged", out)
		}
	})

	t.Run("non-object body is returned unchanged", func(t *testing.T) {
		for _, in := range []string{`not json`, `[1,2,3]`, `"a string"`, ``} {
			if out := applyModelOverride([]byte(in), "sonnet"); string(out) != in {
				t.Errorf("body %q rewritten to %q; want unchanged", in, out)
			}
		}
	})
}

// TestDispatchRewritesModelForExternalBackend proves the override reaches
// the wire: an external backend's Model replaces the client alias upstream.
func TestDispatchRewritesModelForExternalBackend(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&m)
		gotModel = m.Model
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := &Config{Backends: []Backend{{
		Name: "cloud-sonnet", Tier: "cloud", Address: srv.URL,
		Provider: "anthropic", Model: "claude-sonnet-4-6",
	}}}
	disp := NewDispatcher(cfg)

	resp, err := disp.Dispatch(context.Background(), &cfg.Backends[0],
		http.MethodPost, "/v1/chat/completions", http.Header{},
		[]byte(`{"model":"ha","messages":[]}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	_ = resp.Body.Close()

	if gotModel != "claude-sonnet-4-6" {
		t.Errorf("upstream received model %q, want claude-sonnet-4-6 (external.model override)", gotModel)
	}
}

// TestDispatchLeavesBodyForLocalBackend is the inverse: a local backend
// declares no Model, so the body's model passes through verbatim.
func TestDispatchLeavesBodyForLocalBackend(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&m)
		gotModel = m.Model
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := &Config{Backends: []Backend{{
		Name: "local-gemma", Tier: "local", Address: srv.URL,
	}}}
	disp := NewDispatcher(cfg)

	resp, err := disp.Dispatch(context.Background(), &cfg.Backends[0],
		http.MethodPost, "/v1/chat/completions", http.Header{},
		[]byte(`{"model":"gemma-4-e4b","messages":[]}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	_ = resp.Body.Close()

	if gotModel != "gemma-4-e4b" {
		t.Errorf("local upstream received model %q, want gemma-4-e4b (verbatim)", gotModel)
	}
}

// TestDispatchUpstream5xxStillQuarantines is the third corner of the
// triangle: 5xx response must still trigger MarkUnhealthy (was the
// only quarantine path before the half-open work; still true today).
func TestDispatchUpstream5xxStillQuarantines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &Config{Backends: []Backend{{
		Name: "broken", Tier: "local", Address: srv.URL,
	}}}
	disp := NewDispatcher(cfg)

	resp, err := disp.Dispatch(context.Background(), &cfg.Backends[0],
		http.MethodPost, "/x", http.Header{}, []byte(`{}`))
	if err != nil {
		t.Fatalf("Dispatch returned an error on 5xx: %v", err)
	}
	_ = resp.Body.Close()

	if disp.IsHealthy("broken") {
		t.Error("backend should be marked unhealthy after a 5xx response")
	}
}
