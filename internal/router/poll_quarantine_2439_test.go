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
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

// pollEndpoint is a vLLM-shaped pod that can drop every connection, the way
// a pod Istio ambient never enrolled resets them, while staying in the
// endpoint set.
type pollEndpoint struct {
	srv       *httptest.Server
	down      atomic.Bool
	inference atomic.Int64
}

func newPollEndpoint(t *testing.T, metrics string) *pollEndpoint {
	t.Helper()
	pe := &pollEndpoint{}
	pe.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			pe.inference.Add(1)
		}
		if pe.down.Load() {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		if r.URL.Path == "/metrics" {
			if metrics == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, metrics)
			return
		}
		_, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	t.Cleanup(pe.srv.Close)
	return pe
}

const idleMetrics = "vllm:num_requests_running 0\nvllm:num_requests_waiting 0\n"

func busyMetrics(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("testdata/vllm-metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(body) // running 7, waiting 2
}

// pollRig is one LoadAware backend behind a dispatcher with a stepped clock
// and a captured Warn log, and the scraper that polls it.
type pollRig struct {
	d       *Dispatcher
	b       *Backend
	scraper *LoadScraper
	logs    *lockedLogBuffer
	now     atomic.Int64
}

func newPollRig(t *testing.T, router string, headers []string, eps ...string) *pollRig {
	t.Helper()
	rig := &pollRig{logs: &lockedLogBuffer{}}
	rig.now.Store(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC).UnixNano())
	cfg := scrapedConfig()
	cfg.Backends[0].Balancing.SessionHeaders = headers
	rig.d = NewDispatcher(cfg, WithRouterLabel(router), WithQuarantineDuration(time.Second),
		WithNowFunc(func() time.Time { return time.Unix(0, rig.now.Load()).UTC() }))
	rig.d.logger = slog.New(slog.NewTextHandler(rig.logs, nil))
	rig.d.SetEndpoints("qwen", eps)
	rig.b = &rig.d.cfg.Backends[0]
	rig.scraper = NewLoadScraper(cfg, rig.d, router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return rig
}

func (r *pollRig) scrape(n int) {
	for i := 0; i < n; i++ {
		r.scraper.scrapeOnce(context.Background())
	}
}

func (r *pollRig) advance(d time.Duration) { r.now.Add(int64(d)) }

func (r *pollRig) send(t *testing.T, h http.Header) {
	t.Helper()
	resp, err := r.d.Dispatch(t.Context(), r.b, http.MethodPost, "/v1/chat/completions", h, []byte(`{"model":"m"}`))
	if err != nil {
		t.Errorf("dispatch: %v", err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func (r *pollRig) logCount(msg string) int {
	return strings.Count(r.logs.String(), msg)
}

// An endpoint whose load polls cannot connect leaves placement without a
// client request as the probe, stays out after the request quarantine window
// would have expired, and returns on the next poll that connects.
func TestPollFailuresQuarantineEndpoint(t *testing.T) {
	const router = "poll-quarantine"
	busy := newPollEndpoint(t, busyMetrics(t))
	flaky := newPollEndpoint(t, idleMetrics)
	rig := newPollRig(t, router, []string{"x-session-affinity"}, busy.srv.URL, flaky.srv.URL)
	session := http.Header{"X-Session-Affinity": []string{"agent-1"}}

	rig.scrape(1)
	rig.send(t, session)
	if got := flaky.inference.Load(); got != 1 {
		t.Fatalf("the session went to the busy pod; flaky inference = %d, want 1", got)
	}

	flaky.down.Store(true)
	flaky.inference.Store(0)
	rig.scrape(1)
	// The sample from before still reads idle, and the busy pod carries 9.
	for i := 0; i < 5; i++ {
		rig.send(t, nil)
	}
	if got := flaky.inference.Load(); got != 0 {
		t.Errorf("an endpoint whose last poll failed to connect got %d of 5 requests, want 0", got)
	}

	rig.scrape(pollFailuresToQuarantine - 1)
	rig.advance(10 * time.Second) // past the 1s request quarantine, inside the session idle timeout
	rig.scrape(1)
	migrated := testutil.ToFloat64(prommetrics.RouterPlacementsTotal.WithLabelValues(router, "qwen", "migrated", "quarantined"))
	rig.send(t, session)
	if got := flaky.inference.Load(); got != 0 {
		t.Errorf("a poll-quarantined endpoint got %d session requests after the request quarantine window, want 0", got)
	}
	if got := testutil.ToFloat64(prommetrics.RouterPlacementsTotal.WithLabelValues(router, "qwen", "migrated", "quarantined")) - migrated; got != 1 {
		t.Errorf("placements{migrated,quarantined} grew by %v, want 1", got)
	}
	rig.scrape(3)
	if got := rig.logCount("load polls failing"); got != 1 {
		t.Errorf("poll failure Warn lines = %d after %d failed polls, want 1\n%s", got, pollFailuresToQuarantine+4, rig.logs.String())
	}

	flaky.down.Store(false)
	rig.scrape(2)
	rig.send(t, nil)
	if got := flaky.inference.Load(); got != 1 {
		t.Errorf("after a poll connected, the idle endpoint got %d requests, want 1", got)
	}
	if got := rig.logCount("router upstream reachable again"); got != 1 {
		t.Errorf("recovery Warn lines = %d, want 1\n%s", got, rig.logs.String())
	}
}

// A metrics path that answers with an HTTP error is not a connection failure:
// the endpoint may serve inference fine and keeps its place.
func TestPollHTTPErrorKeepsEndpoint(t *testing.T) {
	busy := newPollEndpoint(t, busyMetrics(t))
	noMetrics := newPollEndpoint(t, "")
	rig := newPollRig(t, "poll-http-error", nil, busy.srv.URL, noMetrics.srv.URL)

	rig.scrape(pollFailuresToQuarantine + 2)
	rig.send(t, nil)
	if got := noMetrics.inference.Load(); got != 1 {
		t.Errorf("an idle endpoint whose metrics path answers 404 got %d requests, want 1", got)
	}
	if got := rig.logCount("load polls failing"); got != 0 {
		t.Errorf("poll failure Warn lines = %d, want 0", got)
	}
}

// A scrape cut short by the router shutting down says nothing about the
// endpoint.
func TestPollShutdownDoesNotCount(t *testing.T) {
	pe := newPollEndpoint(t, idleMetrics)
	rig := newPollRig(t, "poll-shutdown", nil, pe.srv.URL)
	pe.down.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < pollFailuresToQuarantine; i++ {
		rig.scraper.scrapeOnce(ctx)
	}
	if !rig.d.IsHealthy("qwen") {
		t.Error("cancelled polls quarantined the endpoint")
	}
}

// A request the endpoint served clears a poll quarantine too, so the two
// sources of health never leave an endpoint quarantined that one of them saw
// answer.
func TestRequestSuccessClearsPollQuarantine(t *testing.T) {
	pe := newPollEndpoint(t, idleMetrics)
	rig := newPollRig(t, "poll-request-recovery", nil, pe.srv.URL)
	pe.down.Store(true)
	rig.scrape(pollFailuresToQuarantine)
	if rig.d.IsHealthy("qwen") {
		t.Fatal("the backend is healthy with its only endpoint failing every poll")
	}

	pe.down.Store(false)
	// With every endpoint out, placement falls back to the round-robin probe.
	rig.send(t, nil)
	if !rig.d.IsHealthy("qwen") {
		t.Error("a served request left the endpoint poll-quarantined")
	}
	if got := rig.logCount("via=request"); got != 1 {
		t.Errorf("recovery Warn lines via=request = %d, want 1\n%s", got, rig.logs.String())
	}
}

// The backend health gauges follow the poll with no client request: a backend
// whose only endpoint is poll-quarantined reads 0, and 1 once a poll connects.
func TestPollTransitionsDriveBackendHealthGauges(t *testing.T) {
	const router = "poll-health-gauge"
	resetRouterSeries(router)
	pe := newPollEndpoint(t, idleMetrics)
	cfg := scrapedConfig()
	p := NewProxy(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), WithRouterName(router))
	p.Dispatcher().SetEndpoints("qwen", []string{pe.srv.URL})
	scraper := NewLoadScraper(cfg, p.Dispatcher(), router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	health := func() float64 {
		return testutil.ToFloat64(prommetrics.RouterBackendHealth.WithLabelValues(router, "qwen"))
	}
	active := func() float64 {
		return testutil.ToFloat64(prommetrics.RouterActiveBackends.WithLabelValues(router, "local"))
	}
	// The state a served request leaves behind.
	prommetrics.RouterBackendHealth.WithLabelValues(router, "qwen").Set(1)
	prommetrics.RouterActiveBackends.WithLabelValues(router, "local").Set(1)

	pe.down.Store(true)
	for i := 0; i < pollFailuresToQuarantine; i++ {
		scraper.scrapeOnce(context.Background())
	}
	if got := health(); got != 0 {
		t.Errorf("backend health with its only endpoint poll-quarantined = %v, want 0", got)
	}
	if got := active(); got != 0 {
		t.Errorf("active local backends with the only one poll-quarantined = %v, want 0", got)
	}

	pe.down.Store(false)
	scraper.scrapeOnce(context.Background())
	if got := health(); got != 1 {
		t.Errorf("backend health after a poll connected = %v, want 1", got)
	}
	if got := active(); got != 1 {
		t.Errorf("active local backends after a poll connected = %v, want 1", got)
	}
	if got := pe.inference.Load(); got != 0 {
		t.Errorf("inference requests = %d, want 0 (the gauges must move on polls alone)", got)
	}
}
