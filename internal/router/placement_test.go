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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/defilantech/llmkube/internal/router/picker"
)

func balancedDispatcher(headers []string, n int, opts ...DispatcherOption) (*Dispatcher, *Backend, []string) {
	cfg := &Config{Backends: []Backend{{
		Name: "qwen", Tier: "local", Address: "http://qwen.ns.svc.cluster.local:8000",
		EndpointSource: &EndpointSource{Namespace: "ns", Service: "qwen", Port: 8000},
		Balancing:      &Balancing{Mode: BalancingLoadAware, SessionHeaders: headers},
		Metrics:        &vllmProfile,
	}}}
	d := NewDispatcher(cfg, append([]DispatcherOption{WithQuarantineDuration(time.Hour)}, opts...)...)
	eps := make([]string, n)
	for i := range eps {
		eps[i] = fmt.Sprintf("http://10.0.1.%d:8000", i+1)
	}
	d.SetEndpoints("qwen", eps)
	return d, &d.cfg.Backends[0], eps
}

func TestSessionKeyHeaderOrder(t *testing.T) {
	names := []string{"x-session-affinity", "x-openwebui-chat-id", "x-opencode-session"}
	h := http.Header{}
	h.Set("X-OpenWebUI-Chat-Id", "chat-1")
	h.Set("X-Opencode-Session", "oc-1")
	if got := sessionKey(h, names); got != "chat-1" {
		t.Errorf("key = %q, want the second header (first is absent)", got)
	}
	h.Set("X-Session-Affinity", "")
	if got := sessionKey(h, names); got != "chat-1" {
		t.Errorf("an empty first header must fall through; key = %q", got)
	}
	h.Set("X-Session-Affinity", "s-9")
	if got := sessionKey(h, names); got != "s-9" {
		t.Errorf("key = %q, want the first header", got)
	}
	if got := sessionKey(h, nil); got != "" {
		t.Errorf("no configured headers: key = %q, want empty", got)
	}
}

func TestPlaceLoadAwarePicksLeastLoaded(t *testing.T) {
	d, b, eps := balancedDispatcher(nil, 3)
	now := d.nowFn()
	d.RecordSample("qwen", eps[0], picker.Sample{Running: 9, At: now})
	d.RecordSample("qwen", eps[1], picker.Sample{Running: 1, At: now})
	d.RecordSample("qwen", eps[2], picker.Sample{Running: 5, At: now})
	ep, res, _, _ := d.place(b, http.Header{}, 0)
	if ep != eps[1] || res != picker.LeastLoaded {
		t.Errorf("place = %s/%s, want %s least_loaded", ep, res, eps[1])
	}
}

func TestPlaceSessionStaysWhileNothingQueues(t *testing.T) {
	d, b, eps := balancedDispatcher([]string{"x-session-affinity"}, 3)
	now := d.nowFn()
	d.RecordSample("qwen", eps[0], picker.Sample{Running: 2, At: now})
	d.RecordSample("qwen", eps[1], picker.Sample{Running: 3, At: now})
	d.RecordSample("qwen", eps[2], picker.Sample{Running: 1, At: now})
	h := http.Header{}
	h.Set("X-Session-Affinity", "agent-1")
	first, res, _, _ := d.place(b, h, 0)
	if first == "" || res != picker.LeastLoaded {
		t.Fatalf("first place = %q/%s, want a pod, least_loaded", first, res)
	}
	for i := 0; i < 10; i++ {
		ep, res, reason, _ := d.place(b, h, 0)
		if ep != first || res != picker.Sticky || reason != picker.ReasonNone {
			t.Fatalf("request %d: place = %s/%s/%q, want %s sticky", i, ep, res, reason, first)
		}
	}
}

func TestPlaceSessionMigratesOffQuarantinedPodAndStays(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	clock := WithNowFunc(func() time.Time { return now })
	d, b, _ := balancedDispatcher([]string{"x-session-affinity"}, 4, clock, WithQuarantineDuration(time.Minute))
	h := http.Header{}
	h.Set("X-Session-Affinity", "agent-2")
	home, _, _, _ := d.place(b, h, 0)
	quarantineEndpoint(d, "qwen", home)
	ep, res, reason, _ := d.place(b, h, 0)
	if ep == home || ep == "" || res != picker.Migrated || reason != picker.ReasonNotReady {
		t.Fatalf("quarantined pod: place = %s/%s/%q, want another pod, migrated, not_ready", ep, res, reason)
	}
	// The quarantine expires inside the idle timeout; the session stays on
	// its new pod rather than returning to the old one.
	now = now.Add(2 * time.Minute)
	for i := 0; i < 3; i++ {
		if ep2, res2, _, _ := d.place(b, h, 0); ep2 != ep || res2 != picker.Sticky {
			t.Fatalf("after the quarantine expired: place = %s/%s, want %s sticky", ep2, res2, ep)
		}
	}
}

func TestPlaceRoundRobinIgnoresSessionHeaders(t *testing.T) {
	cfg := &Config{Backends: []Backend{{
		Name: "qwen", Tier: "local", Address: "http://qwen.ns.svc.cluster.local:8000",
		EndpointSource: &EndpointSource{Namespace: "ns", Service: "qwen", Port: 8000},
		Balancing:      &Balancing{Mode: BalancingRoundRobin, SessionHeaders: []string{"x-session-affinity"}},
	}}}
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{"http://10.0.2.1:8000", "http://10.0.2.2:8000", "http://10.0.2.3:8000"})
	h := http.Header{}
	h.Set("X-Session-Affinity", "agent-1")
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		ep, res, reason, _ := d.place(&d.cfg.Backends[0], h, 0)
		if res != "" || reason != picker.ReasonNone {
			t.Errorf("round robin reported %q/%q, want empty", res, reason)
		}
		seen[ep] = true
	}
	if len(seen) != 3 {
		t.Errorf("same-header requests landed on %v, want a rotation across all three pods", seen)
	}
	if _, ok := d.sessions.Load("qwen"); ok {
		t.Error("a round-robin backend created a session table")
	}
}

func TestPlaceEmptySessionHeaderIsNoSession(t *testing.T) {
	d, b, _ := balancedDispatcher([]string{"x-session-affinity"}, 3)
	h := http.Header{}
	h.Set("X-Session-Affinity", "   ")
	ep, res, _, _ := d.place(b, h, 0)
	if ep == "" || res != picker.LeastLoaded {
		t.Fatalf("blank header: place = %q/%s, want least_loaded", ep, res)
	}
	if v, ok := d.sessions.Load("qwen"); ok && v.(*picker.Sessions).Len() != 0 {
		t.Errorf("a blank header bound %d sessions, want none", v.(*picker.Sessions).Len())
	}
}

func TestSweeperRacesPlacementSafely(t *testing.T) {
	var clockMu sync.Mutex
	start := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	now := start
	clock := WithNowFunc(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	})
	d, b, _ := balancedDispatcher([]string{"x-session-affinity"}, 4, clock)

	stop := make(chan struct{})
	var sweeper sync.WaitGroup
	sweeper.Add(1)
	go func() {
		defer sweeper.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// The fake clock advances in bounded steps and stops well inside
			// the 30m idle timeout, so every key placed during the test is
			// still live; the sweeper keeps sweeping at the capped time.
			clockMu.Lock()
			if now.Sub(start) < 20*time.Minute {
				now = now.Add(time.Second)
			}
			at := now
			clockMu.Unlock()
			d.SweepSessions(at)
		}
	}()

	const keys, workers, rounds = 200, 32, 20
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				for k := w; k < keys; k += workers {
					h := http.Header{}
					h.Set("X-Session-Affinity", fmt.Sprintf("agent-%d", k))
					if ep, _, _, _ := d.place(b, h, 0); ep == "" {
						t.Errorf("key %d placed on no pod", k)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	sweeper.Wait()

	// Every key placed within the idle timeout, so each is still bound to one pod.
	v, _ := d.sessions.Load("qwen")
	tbl := v.(*picker.Sessions)
	if got := tbl.Len(); got != keys {
		t.Fatalf("table holds %d bindings, want %d", got, keys)
	}
	clockMu.Lock()
	at := now
	clockMu.Unlock()
	for k := 0; k < keys; k++ {
		h := http.Header{}
		h.Set("X-Session-Affinity", fmt.Sprintf("agent-%d", k))
		ep1, res1, _, _ := d.place(b, h, 0)
		ep2, _, _, _ := d.place(b, h, 0)
		if res1 != picker.Sticky || ep1 != ep2 {
			t.Fatalf("key %d at %s: place = %s/%s then %s, want one pod, sticky", k, at, ep1, res1, ep2)
		}
	}
}

func TestSessionBindingsGauge(t *testing.T) {
	d, b, _ := balancedDispatcher([]string{"x-session-affinity"}, 3, WithRouterLabel("gauge-router"))
	for i := 0; i < 3; i++ {
		h := http.Header{}
		h.Set("X-Session-Affinity", fmt.Sprintf("agent-%d", i))
		d.place(b, h, 0)
	}
	d.SweepSessions(d.nowFn())
	if got := testutil.ToFloat64(prommetrics.RouterSessionBindings.WithLabelValues("gauge-router", "qwen")); got != 3 {
		t.Errorf("llmkube_router_session_bindings = %v, want 3", got)
	}
}

func TestPlacementReasonLabel(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	clock := WithNowFunc(func() time.Time { return now })
	d, b, _ := balancedDispatcher([]string{"x-session-affinity"}, 4, clock, WithRouterLabel("reason-router"))
	counter := func(result, reason string) float64 {
		return testutil.ToFloat64(prommetrics.RouterPlacementsTotal.WithLabelValues("reason-router", "qwen", result, reason))
	}
	h := http.Header{}
	h.Set("X-Session-Affinity", "agent-1")
	home, res, reason, _ := d.place(b, h, 0)
	d.countPlacement("qwen", res, reason)
	stickyBefore := counter("sticky", "")
	ep, res, reason, _ := d.place(b, h, 0)
	d.countPlacement("qwen", res, reason)
	if ep != home || counter("sticky", "") != stickyBefore+1 {
		t.Errorf("sticky placement: counter {sticky,\"\"} = %v, want %v", counter("sticky", ""), stickyBefore+1)
	}
	migratedBefore := counter("migrated", "not_ready")
	quarantineEndpoint(d, "qwen", home)
	_, res, reason, _ = d.place(b, h, 0)
	d.countPlacement("qwen", res, reason)
	if counter("migrated", "not_ready") != migratedBefore+1 {
		t.Errorf("migration: counter {migrated,not_ready} = %v, want %v", counter("migrated", "not_ready"), migratedBefore+1)
	}
}

// TestSessionValueNeverExposed: the session header value is hashed into a
// table key and never stored, logged or used as a metric label.
func TestSessionValueNeverExposed(t *testing.T) {
	const secret = "secret-session-123"
	upstream := newFakeBackend(t)
	cfg := &Config{
		Backends: []Backend{{
			Name: "qwen", Tier: "local", Address: upstream.URL(),
			Endpoints: []string{upstream.URL()},
			Balancing: &Balancing{Mode: BalancingLoadAware, SessionHeaders: []string{"x-session-affinity"}},
			Metrics:   &vllmProfile,
		}},
		DefaultRoute: "qwen",
		Policy:       Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	proxy := NewProxy(cfg, logger, WithRouterName("secret-router"))
	mux := http.NewServeMux()
	proxy.Mount(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Affinity", secret)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request = %d, want 200", rec.Code)
	}
	if upstream.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstream.calls.Load())
	}

	if strings.Contains(logs.String(), secret) {
		t.Errorf("log output contains the session value:\n%s", logs.String())
	}
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if strings.Contains(lp.GetValue(), secret) {
					t.Errorf("metric %s label %s=%q contains the session value", mf.GetName(), lp.GetName(), lp.GetValue())
				}
			}
		}
	}
}

func TestPlaceRoundRobinUnchanged(t *testing.T) {
	cfg := &Config{Backends: []Backend{{
		Name: "qwen", Tier: "local", Address: "http://qwen.ns.svc.cluster.local:8000",
		EndpointSource: &EndpointSource{Namespace: "ns", Service: "qwen", Port: 8000},
	}}}
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{"http://10.0.2.1:8000", "http://10.0.2.2:8000"})
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		ep, res, _, _ := d.place(&d.cfg.Backends[0], http.Header{"X-Session-Affinity": {"s"}}, 0)
		if res != "" {
			t.Errorf("an unbalanced backend reported result %q", res)
		}
		seen[ep] = true
	}
	if len(seen) != 2 {
		t.Errorf("round-robin picks = %v, want both endpoints", seen)
	}
}

// quarantineEndpoint fails an endpoint enough times in a row to quarantine it.
func quarantineEndpoint(d *Dispatcher, backend, ep string) {
	for i := 0; i < endpointFailuresToQuarantine; i++ {
		d.markUnhealthy(backend, ep)
	}
}

func TestPlaceHeaderlessAvoidsPodMidColdPrefill(t *testing.T) {
	d, b, eps := balancedDispatcher(nil, 2)
	now := d.nowFn()
	d.RecordSample("qwen", eps[0], picker.Sample{Running: 5, At: now})
	d.RecordSample("qwen", eps[1], picker.Sample{Running: 5, At: now})
	d.loadFor("qwen", eps[0]).prefill.Store(140000)
	ep, res, _, tok := d.place(b, http.Header{}, 4000)
	if ep != eps[1] || res != picker.LeastLoaded || tok != picker.EstimateTokens(4000) {
		t.Errorf("place = %s/%s/%d, want %s least_loaded %d", ep, res, tok, eps[1], picker.EstimateTokens(4000))
	}
}
