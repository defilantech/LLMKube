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
