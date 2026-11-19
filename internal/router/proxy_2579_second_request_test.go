/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestIssue2579RetriedDispatchFailureStillQuarantines pins the boundary of the
// retry's quarantine clear: it covers the requests that join the hold, not a
// member that is still gone once the hold returns. The re-dispatch quarantines
// the backend again, and the next request skips it as it does today.
func TestIssue2579RetriedDispatchFailureStillQuarantines(t *testing.T) {
	rb := newReviveBackend(t)
	if err := rb.start(); err != nil {
		t.Fatalf("start member: %v", err)
	}
	fake := newFakeMemberController()
	fake.setPhase("coder", modelReadyPhase)
	fake.setPhase("judge", modelReadyPhase)
	act := NewActivator(context.Background(), fake, "r", slog.Default())
	act.resyncInterval = time.Hour

	rel, err := act.Acquire(context.Background(), issue2579Pool("coder"))
	if err != nil {
		t.Fatalf("seed resident coder: %v", err)
	}
	rel()

	// The member's pod is gone and stays gone: the next Acquire reconciles to
	// the fake's Ready phase, so the hold returns but the dispatch is refused.
	rb.drain()
	fake.setPhase("coder", "")

	judge := newFakeBackend(t)
	cfg := issue2579Config(false, rb.url(), judge.URL(), 5*time.Second)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	proxy := NewProxy(cfg, slog.Default(), WithActivator(act))
	mux := http.NewServeMux()
	proxy.Mount(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", issue2579Body(t))
	req.Header.Set("Content-Type", "application/json")
	first := httptest.NewRecorder()
	mux.ServeHTTP(first, req)
	if first.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 after a retried dispatch to the gone member", first.Code)
	}
	if proxy.disp.IsHealthy("coder") {
		t.Fatal("coder is healthy after the retried dispatch failed; want it quarantined again")
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", issue2579Body(t))
	req.Header.Set("Content-Type", "application/json")
	second := httptest.NewRecorder()
	mux.ServeHTTP(second, req)
	if got := rb.calls.Load(); got != 0 {
		t.Errorf("member handled %d requests, want 0 (it is gone, and quarantined)", got)
	}
	if !strings.Contains(second.Body.String(), "marked unhealthy") {
		t.Errorf("502 body = %q, want the next request skipped as unhealthy, as it is today", second.Body.String())
	}
}

// TestIssue2579SecondRequestDuringHold is the follow-up reproduction: a second
// request for the drained member, sent while the first one is held, must be
// held for that same member and served by it. The first request's refused dial
// quarantines the backend; unless the retry's hold clears that quarantine, the
// second request skips the member and is answered with a 502 or the fallback.
func TestIssue2579SecondRequestDuringHold(t *testing.T) {
	for _, tc := range []struct {
		name           string
		fallback       bool
		endpointSource bool
	}{
		{"no fallback", false, false},
		{"fallback configured", true, false},
		{"endpoint resolution", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rb, rev, act := issue2579Drained(t)
			judge := newFakeBackend(t)

			cfg := issue2579Config(tc.fallback, rb.url(), judge.URL(), 10*time.Minute)
			if tc.endpointSource {
				// The drained member's ready-endpoint set is empty, so a
				// dispatch falls back to Address and the refusal quarantines
				// the backend-level key.
				cfg.Backends[0].EndpointSource = &EndpointSource{Namespace: "lab", Service: "coder", Port: 8080}
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("config: %v", err)
			}
			proxy := NewProxy(cfg, slog.Default(), WithActivator(act))
			if tc.endpointSource {
				proxy.Dispatcher().SetEndpoints("coder", nil)
			}
			mux := http.NewServeMux()
			proxy.Mount(mux)

			send := func() chan int {
				done := make(chan int, 1)
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", issue2579Body(t))
				req.Header.Set("Content-Type", "application/json")
				go func() {
					rec := httptest.NewRecorder()
					mux.ServeHTTP(rec, req)
					done <- rec.Code
				}()
				return done
			}

			first := send()
			select {
			case <-rev.started:
			case code := <-first:
				t.Fatalf("first request answered %d before the drained member began loading", code)
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for the router to activate the drained member")
			}

			// The first request is now held in the retry's re-acquire. Send the
			// second one inside that hold; it must be waiting on the pool too.
			second := send()
			held := false
			for deadline := time.Now().Add(3 * time.Second); !held && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				select {
				case code := <-second:
					t.Fatalf("second request answered %d (judge calls = %d) while the first was held for the drained member; want it held too",
						code, judge.calls.Load())
				default:
				}
				act.mu.Lock()
				pr := act.pools["lab/heavy-slot"]
				held = pr.waiting["coder"]+pr.waiting["judge"] == 2
				act.mu.Unlock()
			}
			if !held {
				t.Fatal("timed out waiting for the second request to register on the member's hold")
			}

			close(rev.gate)
			for _, req := range []struct {
				which string
				done  chan int
			}{{"first", first}, {"second", second}} {
				select {
				case code := <-req.done:
					if code != http.StatusOK {
						t.Errorf("%s request = %d, want 200 from the re-activated member", req.which, code)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("%s request did not complete after the member became resident", req.which)
				}
			}
			if got := rb.calls.Load(); got != 2 {
				t.Errorf("member handled %d requests, want 2", got)
			}
			if got := judge.calls.Load(); got != 0 {
				t.Errorf("judge calls = %d, want 0 (both requests belong to the drained member)", got)
			}
			if got := rev.activations.Load(); got != 1 {
				t.Errorf("coder activated %d times, want 1 (the second request joins the first one's swap)", got)
			}
		})
	}
}
