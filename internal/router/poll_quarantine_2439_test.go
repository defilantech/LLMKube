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
	"github.com/defilantech/llmkube/internal/router/picker"
)

// pollEndpoint is a vLLM-shaped pod that can drop every connection, the way
// a pod Istio ambient never enrolled resets them, while staying in the
// endpoint set.
type pollEndpoint struct {
	srv       *httptest.Server
	down      atomic.Bool
	inference atomic.Int64
	// held, when set before the first request, makes an inference request
	// signal started, wait for release, then answer 200 even while down.
	held             bool
	started, release chan struct{}
}

func newPollEndpoint(t *testing.T, metrics string) *pollEndpoint {
	t.Helper()
	pe := &pollEndpoint{started: make(chan struct{}, 1), release: make(chan struct{})}
	pe.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			pe.inference.Add(1)
			if pe.held {
				pe.started <- struct{}{}
				<-pe.release
				_, _ = io.ReadAll(r.Body)
				_, _ = io.WriteString(w, `{"choices":[]}`)
				return
			}
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

	rig.scrape(int(pollFailuresToQuarantine) - 1)
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
		t.Errorf("poll failure Warn lines = %d after %d failed polls, want 1\n%s", got, int(pollFailuresToQuarantine)+4, rig.logs.String())
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

// A pod is not poll-quarantined before its last sample would read stale: a
// path that drops a few polls in a row must not migrate its sessions.
func TestPollQuarantineWaitsForStaleAfter(t *testing.T) {
	pe := newPollEndpoint(t, idleMetrics)
	rig := newPollRig(t, "poll-stale-after", nil, pe.srv.URL)
	pe.down.Store(true)
	rig.scrape(int(picker.StaleAfter/scrapeInterval) - 1)
	if !rig.d.IsHealthy("qwen") {
		t.Errorf("quarantined after %d failed polls, inside picker.StaleAfter (%s)", int(picker.StaleAfter/scrapeInterval)-1, picker.StaleAfter)
	}
	rig.scrape(1)
	if rig.d.IsHealthy("qwen") {
		t.Errorf("not quarantined after %d failed polls", int(picker.StaleAfter/scrapeInterval))
	}
}

// A poll that fails after its pod left the endpoint set records nothing: the
// pod's health entry is not re-created, so an address reused by a new pod
// starts clean.
func TestPollOfDepartedEndpointIsIgnored(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unreachable bool
	}{
		{name: "connection_failure", unreachable: true},
		{name: "http_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started, release := make(chan struct{}, 1), make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				started <- struct{}{}
				<-release
				if tc.unreachable {
					if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
						_ = conn.Close()
					}
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			rig := newPollRig(t, "poll-departed-"+tc.name, nil, srv.URL)
			done := make(chan struct{})
			go func() {
				defer close(done)
				rig.scrape(1)
			}()
			<-started
			rig.d.SetEndpoints("qwen", nil) // the pod leaves while its poll is in flight
			close(release)
			<-done

			if _, ok := rig.d.health.Load(healthKey("qwen", srv.URL)); ok {
				t.Error("a poll that finished after the pod left re-created its health entry")
			}
		})
	}
}

// A metrics path that answers with an HTTP error is not a connection failure:
// the endpoint may serve inference fine and keeps its place.
func TestPollHTTPErrorKeepsEndpoint(t *testing.T) {
	busy := newPollEndpoint(t, busyMetrics(t))
	noMetrics := newPollEndpoint(t, "")
	rig := newPollRig(t, "poll-http-error", nil, busy.srv.URL, noMetrics.srv.URL)

	rig.scrape(int(pollFailuresToQuarantine) + 2)
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
	for i := int32(0); i < pollFailuresToQuarantine; i++ {
		rig.scraper.scrapeOnce(ctx)
	}
	if !rig.d.IsHealthy("qwen") {
		t.Error("cancelled polls quarantined the endpoint")
	}
}

// A request already in flight when its endpoint is poll-quarantined, and that
// the endpoint then serves, clears the poll quarantine: the endpoint answered.
// No new request can reach a poll-quarantined endpoint (see
// TestPollQuarantinedBackendSkippedByProxy).
func TestRequestSuccessClearsPollQuarantine(t *testing.T) {
	pe := newPollEndpoint(t, idleMetrics)
	pe.held = true
	rig := newPollRig(t, "poll-request-recovery", nil, pe.srv.URL)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rig.send(t, nil)
	}()
	<-pe.started

	pe.down.Store(true)
	rig.scrape(int(pollFailuresToQuarantine))
	quarantined := !rig.d.IsHealthy("qwen")
	close(pe.release)
	<-done
	if !quarantined {
		t.Fatal("the backend was healthy with its only endpoint failing every poll")
	}
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
	for i := int32(0); i < pollFailuresToQuarantine; i++ {
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

// Recovery from a poll quarantine is poll-only: once every endpoint of a
// backend is poll-quarantined, a client request through the proxy is answered
// 502 without reaching the pod, even after the pod could serve it again.
func TestPollQuarantinedBackendSkippedByProxy(t *testing.T) {
	const router = "poll-proxy-skip"
	resetRouterSeries(router)
	pe := newPollEndpoint(t, idleMetrics)
	cfg := scrapedConfig()
	cfg.DefaultRoute = "qwen"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg: %v", err)
	}
	logs := &lockedLogBuffer{}
	p := NewProxy(cfg, slog.New(slog.NewTextHandler(logs, nil)), WithRouterName(router))
	mux := http.NewServeMux()
	p.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p.Dispatcher().SetEndpoints("qwen", []string{pe.srv.URL})
	scraper := NewLoadScraper(cfg, p.Dispatcher(), router, slog.New(slog.NewTextHandler(io.Discard, nil)))

	pe.down.Store(true)
	for i := int32(0); i < pollFailuresToQuarantine; i++ {
		scraper.scrapeOnce(context.Background())
	}
	// The pod could serve now, but no poll has seen it yet.
	pe.down.Store(false)

	if code := postModel(t, srv.URL, nil); code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", code)
	}
	if got := pe.inference.Load(); got != 0 {
		t.Errorf("inference requests reaching the poll-quarantined pod = %d, want 0", got)
	}
	if got := testutil.ToFloat64(prommetrics.RouterRequestsTotal.WithLabelValues(router, "", "qwen", "", "all_backends_failed")); got != 1 {
		t.Errorf("requests_total{backend=qwen,outcome=all_backends_failed} = %v, want 1", got)
	}
	if !strings.Contains(logs.String(), "outcome=all_backends_failed") {
		t.Errorf("no all_backends_failed audit line:\n%s", logs.String())
	}
}
