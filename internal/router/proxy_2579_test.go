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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// reviveBackend is a ModelPool member endpoint at a fixed loopback address
// whose listener the test can close (the controller draining the member) and
// re-open (the router re-activating it). The address survives the drain so a
// dispatch that lands while the member is gone is refused rather than sent
// somewhere else.
type reviveBackend struct {
	addr  string
	calls atomic.Int64

	mu  sync.Mutex
	ln  net.Listener
	srv *http.Server
	// served closes when the Serve goroutine returns, so drain can wait for
	// the listener to be closed instead of returning while Serve may still be
	// accepting.
	served chan struct{}
}

func newReviveBackend(t *testing.T) *reviveBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve member address: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved member address: %v", err)
	}
	return &reviveBackend{addr: addr}
}

// start binds the reserved address and serves one canned completion per
// request. It fails loudly if something else took the port in between, so the
// test cannot pass by silently sending the request nowhere.
func (r *reviveBackend) start() error {
	ln, err := net.Listen("tcp", r.addr)
	if err != nil {
		return fmt.Errorf("bind member address %s: %w", r.addr, err)
	}
	served := make(chan struct{})
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	})}
	r.mu.Lock()
	r.ln, r.srv, r.served = ln, srv, served
	r.mu.Unlock()
	go func() {
		defer close(served)
		_ = srv.Serve(ln)
	}()
	return nil
}

// drain closes the listener, so a dispatch to the member is refused the way it
// is while the controller's drain tears the pod down. It waits for the Serve
// goroutine to return: http.Server.Close can return before Serve has closed the
// listener, and a request landing in that window is accepted and then reset
// instead of refused.
func (r *reviveBackend) drain() {
	r.mu.Lock()
	ln, srv, served := r.ln, r.srv, r.served
	r.ln, r.srv, r.served = nil, nil, nil
	r.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
	if ln != nil {
		_ = ln.Close()
	}
	if served != nil {
		<-served
	}
}

func (r *reviveBackend) url() string { return "http://" + r.addr }

// revivingMemberController is a MemberController whose Activate of one member
// models a real swap: the test holds the activation on a gate, and only when it
// opens does the member's listener come back and the member report Ready.
type revivingMemberController struct {
	*fakeMemberController
	member  string
	backend *reviveBackend
	started chan struct{}
	gate    chan struct{}
	once    sync.Once

	activations atomic.Int64
}

func (r *revivingMemberController) Activate(ctx context.Context, namespace, isvc string) error {
	if isvc == r.member {
		r.activations.Add(1)
		r.once.Do(func() { close(r.started) })
		select {
		case <-r.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := r.backend.start(); err != nil {
			return err
		}
	}
	return r.fakeMemberController.Activate(ctx, namespace, isvc)
}

func issue2579Pool(member string) *BackendPool {
	return &BackendPool{
		Name:      "heavy-slot",
		Namespace: "lab",
		Member:    member,
		Members:   []string{"coder", "judge"},
	}
}

// issue2579Config routes to the drained member alone, or to a rule that lists
// the drained member and its pool sibling so a fall-through is visible.
func issue2579Config(fallback bool, coderURL, judgeURL string, budget time.Duration) *Config {
	pool := func(member string) *BackendPool {
		p := issue2579Pool(member)
		p.SwapBudget = budget
		return p
	}
	cfg := &Config{
		Backends: []Backend{
			{Name: "coder", Tier: "local", Address: coderURL, InferenceService: "coder", Pool: pool("coder")},
			{Name: "judge", Tier: "local", Address: judgeURL, InferenceService: "judge", Pool: pool("judge")},
		},
		DefaultRoute: "coder",
		Policy:       Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}
	if fallback {
		cfg.Rules = []Rule{{
			Name:  "prefer-coder",
			Match: RuleMatch{Models: []string{"work"}},
			Route: RuleRoute{Backends: []string{"coder", "judge"}},
		}}
	}
	return cfg
}

func issue2579Body(t *testing.T) *strings.Reader {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":    "work",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return strings.NewReader(string(body))
}

// issue2579Drained starts a member that answers, seeds the activator's belief
// that it is resident with a resync interval long enough that the request below
// is inside the window by construction, then drains it the way the controller
// does when it reclaims the slot for the pool's default (judge).
func issue2579Drained(t *testing.T) (*reviveBackend, *revivingMemberController, *Activator) {
	t.Helper()
	rb := newReviveBackend(t)
	if err := rb.start(); err != nil {
		t.Fatalf("start member: %v", err)
	}
	fake := newFakeMemberController()
	fake.setPhase("coder", modelReadyPhase)
	fake.setPhase("judge", modelReadyPhase)
	rev := &revivingMemberController{
		fakeMemberController: fake,
		member:               "coder",
		backend:              rb,
		started:              make(chan struct{}),
		gate:                 make(chan struct{}),
	}
	act := NewActivator(context.Background(), rev, "r", slog.Default())
	act.resyncInterval = time.Hour

	rel, err := act.Acquire(context.Background(), issue2579Pool("coder"))
	if err != nil {
		t.Fatalf("seed resident coder: %v", err)
	}
	rel()

	rb.drain()
	fake.setPhase("coder", "")
	return rb, rev, act
}

// TestIssue2579DrainedResidentHeldThenServed is the reproduction: a request for
// a member the controller drained inside the resident-resync window must be held
// while the router re-activates it, then served by that member, instead of
// answered with a 502 or sent to the fallback.
func TestIssue2579DrainedResidentHeldThenServed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fallback bool
	}{
		{"no fallback", false},
		{"fallback configured", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rb, rev, act := issue2579Drained(t)
			judge := newFakeBackend(t)

			cfg := issue2579Config(tc.fallback, rb.url(), judge.URL(), 10*time.Minute)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("config: %v", err)
			}
			proxy := NewProxy(cfg, slog.Default(), WithActivator(act))
			mux := http.NewServeMux()
			proxy.Mount(mux)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", issue2579Body(t))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			done := make(chan int, 1)
			go func() {
				mux.ServeHTTP(rec, req)
				done <- rec.Code
			}()

			select {
			case <-rev.started:
			case code := <-done:
				t.Fatalf("request answered with %d (judge calls = %d) while the drained member was not resident",
					code, judge.calls.Load())
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for the router to activate the drained member")
			}

			// The activation is held: the request must still be unanswered.
			select {
			case code := <-done:
				t.Fatalf("request answered with %d before the drained member came back", code)
			case <-time.After(100 * time.Millisecond):
			}
			close(rev.gate)

			var code int
			select {
			case code = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not complete after the member became resident")
			}
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200 from the re-activated member", code)
			}
			if got := rb.calls.Load(); got != 1 {
				t.Errorf("member handled %d requests, want 1", got)
			}
			if got := judge.calls.Load(); got != 0 {
				t.Errorf("judge calls = %d, want 0 (the request belongs to the drained member)", got)
			}
			if got := rev.activations.Load(); got != 1 {
				t.Errorf("coder activated %d times, want 1", got)
			}
		})
	}
}

// TestIssue2579DrainedResidentHoldBoundedBySwapBudget pins the bound: a member
// that never comes back ends the hold at swapBudget with 503 + Retry-After, not
// a 502 and not an unbounded wait.
func TestIssue2579DrainedResidentHoldBoundedBySwapBudget(t *testing.T) {
	rb, rev, act := issue2579Drained(t)
	judge := newFakeBackend(t)

	cfg := issue2579Config(false, rb.url(), judge.URL(), 150*time.Millisecond)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	proxy := NewProxy(cfg, slog.Default(), WithActivator(act))
	mux := http.NewServeMux()
	proxy.Mount(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", issue2579Body(t))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-rev.started:
	case <-done:
		t.Fatalf("status = %d, want 503", rec.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the router to activate the drained member")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not complete within the swap budget")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header on the expired pool hold")
	}
	if got := rb.calls.Load(); got != 0 {
		t.Errorf("member handled %d requests before it became resident, want 0", got)
	}
	if got := judge.calls.Load(); got != 0 {
		t.Errorf("judge calls = %d, want 0 (the request must be held for coder, not sent to the fallback)", got)
	}
	if got := rev.activations.Load(); got != 1 {
		t.Errorf("coder activated %d times, want 1", got)
	}
}

// TestIssue2579ParityWrittenRequestNotRetried pins that a request whose bytes
// reached the upstream is never dispatched a second time: the member reads the
// whole request and then drops the connection, and the request falls through to
// the next backend exactly as it does today.
func TestIssue2579ParityWrittenRequestNotRetried(t *testing.T) {
	var coderCalls atomic.Int64
	coder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		coderCalls.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("coder: response writer is not a hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("coder: hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer coder.Close()
	judge := newFakeBackend(t)

	fake := newFakeMemberController()
	fake.setPhase("coder", modelReadyPhase)
	fake.setPhase("judge", modelReadyPhase)
	act := NewActivator(context.Background(), fake, "r", slog.Default())
	rel, err := act.Acquire(context.Background(), issue2579Pool("coder"))
	if err != nil {
		t.Fatalf("seed resident coder: %v", err)
	}
	rel()
	fake.setPhase("coder", "")

	cfg := issue2579Config(true, coder.URL, judge.URL(), 5*time.Second)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	proxy := NewProxy(cfg, slog.Default(), WithActivator(act))
	mux := http.NewServeMux()
	proxy.Mount(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", issue2579Body(t))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the fallback", rec.Code)
	}
	if got := coderCalls.Load(); got != 1 {
		t.Errorf("member handled %d requests, want 1", got)
	}
	if got := judge.calls.Load(); got != 1 {
		t.Errorf("judge calls = %d, want 1", got)
	}
	if got := fake.activateCount("coder"); got != 0 {
		t.Errorf("coder activated %d times, want 0", got)
	}
}

// refusedURL reserves a loopback address and releases it, so a dispatch to it
// is refused at the connection the way a downed pod's is.
func refusedURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release address: %v", err)
	}
	return "http://" + addr
}

// TestIssue2579ParityUnretriedErrorMessageUnchanged pins that a backend the
// retry cannot serve keeps the 502 body it answered before the retry existed:
// the same string the audit line carries.
func TestIssue2579ParityUnretriedErrorMessageUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		pool bool
	}{
		{"non-pooled backend", false},
		{"pool without an activator", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := Backend{Name: "coder", Tier: "local", Address: refusedURL(t)}
			if tc.pool {
				b.InferenceService = "coder"
				b.Pool = issue2579Pool("coder")
			}
			cfg := &Config{
				Backends:     []Backend{b},
				DefaultRoute: "coder",
				Policy:       Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("config: %v", err)
			}
			proxy := NewProxy(cfg, slog.Default())
			mux := http.NewServeMux()
			proxy.Mount(mux)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", issue2579Body(t))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", rec.Code)
			}
			body := rec.Body.String()
			if strings.Contains(body, "request not sent") {
				t.Fatalf("502 body = %q, want the pre-retry wording without the retry's sentinel", body)
			}
			if !strings.Contains(body, "all backends failed: upstream request: ") {
				t.Fatalf("502 body = %q, want the cause unchanged", body)
			}
		})
	}
}

// TestIssue2579ParityIfIdleNeverHolds pins that an IfIdle rule keeps its
// documented contract: a request for a drained member falls through to the warm
// member and nothing is held or activated.
func TestIssue2579ParityIfIdleNeverHolds(t *testing.T) {
	rb, rev, act := issue2579Drained(t)
	judge := newFakeBackend(t)

	cfg := issue2579Config(true, rb.url(), judge.URL(), 5*time.Second)
	cfg.Rules[0].Route.PoolActivation = PoolActivationIfIdle
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	proxy := NewProxy(cfg, slog.Default(), WithActivator(act))
	mux := http.NewServeMux()
	proxy.Mount(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", issue2579Body(t))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	start := time.Now()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the warm member", rec.Code)
	}
	if got := rev.activations.Load(); got != 0 {
		t.Errorf("coder activated %d times, want 0", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("request took %v; IfIdle must not hold behind a drained member", elapsed)
	}
	if got := judge.calls.Load(); got != 1 {
		t.Errorf("judge calls = %d, want 1", got)
	}
	if got := rb.calls.Load(); got != 0 {
		t.Errorf("drained member handled %d requests, want 0", got)
	}
}
