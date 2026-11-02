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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// setBudgetGaugeInterval changes the minimum spacing between a proxy's token
// budget gauge republishes. Call it before the proxy serves a request.
func setBudgetGaugeInterval(p *Proxy, d time.Duration) {
	p.budgetGauge.mu.Lock()
	defer p.budgetGauge.mu.Unlock()
	p.budgetGauge.interval = d
}

// waitBudgetGauge polls the token budget gauge for a budget until ok accepts
// its values by scope, and fails the test if that takes longer than a few
// seconds. The gauge is republished off the request path, so a test reads it
// after the republish a request scheduled, not right after the request.
func waitBudgetGauge(t testing.TB, budget string, ok func(map[string]float64) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := budgetGaugeScopes(t, budget)
		if ok(got) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("token budget gauge for %q never reached the expected values; last %v", budget, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBudgetGaugeRepublishRateLimited is the #2338 gate: a burst of budgeted
// requests inside one interval runs no full gauge republish on the request
// path, and at most one off it, while still scheduling the trailing republish
// that publishes the burst's charges.
func TestBudgetGaugeRepublishRateLimited(t *testing.T) {
	const budget = "team-cap-2338-burst"
	dropBudgetGauge(t, budget)
	proxy, mux, back := budgetTestProxy(t, []Budget{
		{Name: budget, Scope: BudgetScopeTeam, HeaderKey: DefaultTeamHeaderKey, Window: time.Hour, MaxTokens: 1 << 30},
	})
	setBudgetGaugeInterval(proxy, time.Hour)

	// The first request finds no republish in the last interval, so its
	// republish runs at once.
	resp := budgetPost(t, mux, map[string]string{DefaultTeamHeaderKey: "lead"})
	_ = resp.Body.Close()
	waitFor(t, "the first republish", func() bool { return proxy.budgetGaugeRepublishes.Load() == 1 })
	waitBudgetGauge(t, budget, func(g map[string]float64) bool {
		_, ok := g[teamScopeKey(DefaultTeamHeaderKey, "lead")]
		return ok
	})

	const burst = 50
	for i := 0; i < burst; i++ {
		resp := budgetPost(t, mux, map[string]string{DefaultTeamHeaderKey: fmt.Sprintf("burst-%02d", i)})
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("burst request %d status = %d, want 200", i, resp.StatusCode)
		}
	}
	if got := back.calls.Load(); got != burst+1 {
		t.Fatalf("backend calls = %d, want %d (every request served and charged)", got, burst+1)
	}
	if got := proxy.budgetGaugeRepublishes.Load(); got != 1 {
		t.Fatalf("full gauge republishes = %d after a burst of %d requests inside one interval, want 1", got, burst)
	}
	proxy.budgetGauge.mu.Lock()
	pending := proxy.budgetGauge.pending
	proxy.budgetGauge.mu.Unlock()
	if !pending {
		t.Fatal("no trailing republish is scheduled for the burst's charges")
	}

	// Close cancels the scheduled republish and returns without waiting out
	// the interval, so no timer or goroutine outlives the proxy.
	closed := make(chan struct{})
	go func() { proxy.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return with a republish scheduled")
	}
	if got := proxy.budgetGaugeRepublishes.Load(); got != 1 {
		t.Fatalf("full gauge republishes = %d after Close, want 1", got)
	}
}

// TestBudgetGaugeTrailingRepublish charges inside an interval that already
// had a republish and sends nothing more: the gauge must still show the
// charge once the interval ends.
func TestBudgetGaugeTrailingRepublish(t *testing.T) {
	const budget = "team-cap-2338-trailing"
	const interval = 200 * time.Millisecond
	dropBudgetGauge(t, budget)
	proxy, mux, _ := budgetTestProxy(t, []Budget{
		{Name: budget, Scope: BudgetScopeTeam, HeaderKey: DefaultTeamHeaderKey, Window: time.Hour, MaxTokens: 1000},
	})
	setBudgetGaugeInterval(proxy, interval)
	alpha := teamScopeKey(DefaultTeamHeaderKey, "alpha")

	resp := budgetPost(t, mux, map[string]string{DefaultTeamHeaderKey: "alpha"})
	_ = resp.Body.Close()
	waitBudgetGauge(t, budget, func(g map[string]float64) bool { return g[alpha] == 0.1 })

	charged := time.Now()
	resp = budgetPost(t, mux, map[string]string{DefaultTeamHeaderKey: "alpha"})
	_ = resp.Body.Close()
	waitBudgetGauge(t, budget, func(g map[string]float64) bool { return g[alpha] == 0.2 })
	t.Logf("second charge published after %v (interval %v)", time.Since(charged), interval)
	if got := proxy.budgetGaugeRepublishes.Load(); got != 2 {
		t.Fatalf("full gauge republishes = %d, want 2 (one per request, each in its own interval)", got)
	}
}

// TestGaugePublisherNeverLeavesARequestUnpublished hammers request from many
// goroutines while runs race them: once the requests stop, the last run must
// have started after the last request, and runs must start at least an
// interval apart.
func TestGaugePublisherNeverLeavesARequestUnpublished(t *testing.T) {
	const interval = 2 * time.Millisecond
	var version, published, runs atomic.Int64
	g := newGaugePublisher(interval, func() {
		runs.Add(1)
		published.Store(version.Load())
	})
	t.Cleanup(g.close)

	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				version.Add(1)
				g.request()
			}
		}()
	}
	wg.Wait()
	final := version.Load()
	waitFor(t, "a run that saw the last request", func() bool { return published.Load() == final })
	elapsed := time.Since(start)
	if got, limit := runs.Load(), int64(elapsed/interval)+1; got > limit {
		t.Fatalf("%d runs in %v, want at most %d at one per %v", got, elapsed, limit, interval)
	}
}

// TestGaugePublisherCloseWaitsForRun pins that close waits for a run in
// flight, so nothing publishes after it returns, and that a request after
// close schedules nothing.
func TestGaugePublisherCloseWaitsForRun(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int64
	g := newGaugePublisher(time.Hour, func() {
		if runs.Add(1) == 1 {
			close(entered)
			<-release
		}
	})
	g.request()
	<-entered

	closed := make(chan struct{})
	go func() { g.close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("close returned while a run was publishing")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not return after the run finished")
	}

	g.request()
	g.close()
	if got := runs.Load(); got != 1 {
		t.Fatalf("runs = %d, want 1 (a request after close must not publish)", got)
	}
}

// BenchmarkBudgetedRequestTenThousandTeamKeys measures one budgeted request
// through the handler while the store holds maxTeamKeys live team keys, the
// bound #2337 sets. Every request is served, charged to an existing team key
// and the router scope, and then publishes (or schedules) the token budget
// gauge.
func BenchmarkBudgetedRequestTenThousandTeamKeys(b *testing.B) {
	const budget = "team-cap-2338-bench"
	usageBody := `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":6,"completion_tokens":4}}`
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, usageBody)
	}))
	b.Cleanup(back.Close)
	cfg := &Config{
		Backends:     []Backend{{Name: "local-qwen", Tier: "local", Address: back.URL}},
		Rules:        []Rule{{Name: "all", Route: RuleRoute{Backends: []string{"local-qwen"}}}},
		DefaultRoute: "local-qwen",
		Policy: Policy{
			Classification: ClassificationPolicy{Mode: "header-only"},
			Budgets: []Budget{
				{Name: "router-cap-2338-bench", Scope: BudgetScopeRouter, Window: time.Hour, MaxTokens: 1 << 50},
				{Name: budget, Scope: BudgetScopeTeam, HeaderKey: DefaultTeamHeaderKey, Window: time.Hour, MaxTokens: 1 << 40},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		b.Fatalf("bench config: %v", err)
	}
	// Cleanups run last-registered first: Close stops the republish before
	// the gauge series are dropped.
	dropBudgetGauge(b, budget)
	dropBudgetGauge(b, "router-cap-2338-bench")
	proxy := NewProxy(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(proxy.Close)
	mux := http.NewServeMux()
	proxy.Mount(mux)

	teams := make([]string, maxTeamKeys)
	for i := range teams {
		teams[i] = fmt.Sprintf("t%05d", i)
		proxy.budgets.Charge([]string{teamScopeKey(DefaultTeamHeaderKey, teams[i])}, 1, 0)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"any"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(DefaultTeamHeaderKey, teams[i%len(teams)])
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d, want 200", rec.Code)
		}
	}
}
