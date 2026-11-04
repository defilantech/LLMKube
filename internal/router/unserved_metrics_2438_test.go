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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

// durationCount reads the sample count of one llmkube_router_request_duration_seconds series.
func durationCount(t *testing.T, router, rule, backend string) uint64 {
	t.Helper()
	m := &dto.Metric{}
	if err := prommetrics.RouterRequestDuration.WithLabelValues(router, rule, backend).(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("read duration histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// resetRouterSeries drops the request and health series of one router label,
// so a test can assert absolute values under -count above 1.
func resetRouterSeries(router string) {
	l := prometheus.Labels{"router": router}
	prommetrics.RouterRequestsTotal.DeletePartialMatch(l)
	prommetrics.RouterRequestDuration.DeletePartialMatch(l)
	prommetrics.RouterFailClosedTotal.DeletePartialMatch(l)
	prommetrics.RouterBackendHealth.DeletePartialMatch(l)
	prommetrics.RouterActiveBackends.DeletePartialMatch(l)
}

func postModel(t *testing.T, url string, header map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// A backend whose connections are refused answers 502 all_backends_failed.
// That request must be counted against the backend it was sent to, and the
// health gauges must drop, even though the last request before it succeeded.
func TestUnservedRequestCountedAndHealthGaugesDrop(t *testing.T) {
	const router = "unserved-refused"
	resetRouterSeries(router)
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	srv, _ := newOutcomeProxy(t, &Config{
		Backends:     []Backend{{Name: "b", Tier: "local", Address: be.URL}},
		DefaultRoute: "b",
	}, WithRouterName(router))

	if code := postModel(t, srv.URL, nil); code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	if got := testutil.ToFloat64(prommetrics.RouterBackendHealth.WithLabelValues(router, "b")); got != 1 {
		t.Fatalf("backend health after a served request = %v, want 1", got)
	}
	served := durationCount(t, router, "", "b")

	be.Close()
	if code := postModel(t, srv.URL, nil); code != http.StatusBadGateway {
		t.Fatalf("status with the backend refusing connections = %d, want 502", code)
	}
	if got := requestsTotal(router, "all_backends_failed"); got != 1 {
		t.Errorf("requests_total{backend=b,outcome=all_backends_failed} = %v, want 1", got)
	}
	if got := durationCount(t, router, "", "b") - served; got != 1 {
		t.Errorf("request_duration_seconds{backend=b} gained %d samples, want 1", got)
	}
	if got := testutil.ToFloat64(prommetrics.RouterBackendHealth.WithLabelValues(router, "b")); got != 0 {
		t.Errorf("backend health after the backend was quarantined = %v, want 0", got)
	}
	if got := testutil.ToFloat64(prommetrics.RouterActiveBackends.WithLabelValues(router, "local")); got != 0 {
		t.Errorf("active local backends with the only one quarantined = %v, want 0", got)
	}
}

// Every writeDispatchError branch counts the request under its audit outcome,
// labelled with the last backend the dispatch reached.
func TestUnservedOutcomesCounted(t *testing.T) {
	for _, tc := range []struct {
		outcome    string
		err        error
		failClosed bool
		cancelled  bool
		status     int
	}{
		{outcome: "client_cancelled", err: context.Canceled, cancelled: true},
		{outcome: "pool_activation_timeout", err: ErrHoldBudgetExceeded, status: http.StatusServiceUnavailable},
		{outcome: "pool_activation_timeout", err: ErrSwapDeadlineExceeded, status: http.StatusServiceUnavailable},
		{outcome: "pool_incumbent_busy", err: ErrIncumbentBusy, status: http.StatusServiceUnavailable},
		{outcome: "fail_closed_runtime", err: errors.New("dial tcp: connection refused"), failClosed: true, status: http.StatusServiceUnavailable},
		{outcome: "timeout", err: fmt.Errorf("%w: waiting for response headers from b", context.DeadlineExceeded), status: http.StatusGatewayTimeout},
		{outcome: "all_backends_failed", err: errors.New("read: connection reset by peer"), status: http.StatusBadGateway},
	} {
		t.Run(tc.outcome+"/"+tc.err.Error(), func(t *testing.T) {
			router := "unserved-" + strings.NewReplacer(" ", "-", ":", "").Replace(t.Name())
			resetRouterSeries(router)
			cfg := &Config{Backends: []Backend{{Name: "b", Tier: "local", Address: "http://127.0.0.1:1"}}, DefaultRoute: "b"}
			p := NewProxy(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), WithRouterName(router))
			features := RequestFeatures{Classification: "pii"}
			decision := MatchResult{Rule: &Rule{Name: "r"}, Backends: []string{"b"}, FailClosed: tc.failClosed}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if tc.cancelled {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			rec := httptest.NewRecorder()

			p.writeDispatchError(rec, req, false, &features, &decision, &cfg.Backends[0], tc.err, 250*time.Millisecond)

			if tc.status != 0 && rec.Code != tc.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.status)
			}
			if got := testutil.ToFloat64(prommetrics.RouterRequestsTotal.WithLabelValues(router, "r", "b", "pii", tc.outcome)); got != 1 {
				t.Errorf("requests_total{backend=b,outcome=%s} = %v, want 1", tc.outcome, got)
			}
			if got := durationCount(t, router, "r", "b"); got != 1 {
				t.Errorf("request_duration_seconds{backend=b} samples = %d, want 1", got)
			}
		})
	}
}

// The two rejections before dispatch are counted with no backend.
func TestEarlyRejectionsCounted(t *testing.T) {
	const router = "unserved-early"
	resetRouterSeries(router)
	cfg := &Config{
		Backends: []Backend{
			{Name: "local", Tier: "local", Address: "http://127.0.0.1:1"},
			{Name: "cloud", Tier: "cloud", Address: "http://127.0.0.1:1"},
		},
		Rules: []Rule{{
			Name:       "pii-closed",
			Match:      RuleMatch{DataClassification: []string{"pii"}},
			Route:      RuleRoute{Backends: []string{"cloud"}},
			FailClosed: true,
		}},
		Policy: Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}
	// No Validate: the static fail-closed check in the proxy defends against
	// a config the controller should have rejected.
	p := NewProxy(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), WithRouterName(router))
	mux := http.NewServeMux()
	p.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if code := postModel(t, srv.URL, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("no-route status = %d, want 503", code)
	}
	if got := testutil.ToFloat64(prommetrics.RouterRequestsTotal.WithLabelValues(router, "", "", "", "no_route")); got != 1 {
		t.Errorf("requests_total{outcome=no_route} = %v, want 1", got)
	}
	if code := postModel(t, srv.URL, map[string]string{"X-LLMKube-Classification": "pii"}); code != http.StatusServiceUnavailable {
		t.Fatalf("fail-closed status = %d, want 503", code)
	}
	if got := testutil.ToFloat64(prommetrics.RouterRequestsTotal.WithLabelValues(router, "pii-closed", "", "pii", "fail_closed")); got != 1 {
		t.Errorf("requests_total{outcome=fail_closed} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(prommetrics.RouterFailClosedTotal.WithLabelValues(router, "pii-closed", "pii")); got != 1 {
		t.Errorf("fail_closed_total = %v, want 1", got)
	}
	if got := durationCount(t, router, "", "") + durationCount(t, router, "pii-closed", ""); got != 2 {
		t.Errorf("request_duration_seconds samples = %d, want 2", got)
	}
}

// When every backend of a fallback chain fails, the health gauge of each one
// reflects the dispatcher, and the request is counted against the last one.
func TestFallbackRefreshesHealthOfEveryTriedBackend(t *testing.T) {
	const router = "unserved-fallback"
	resetRouterSeries(router)
	refused := func() string {
		s := httptest.NewServer(http.NotFoundHandler())
		s.Close()
		return s.URL
	}
	srv, _ := newOutcomeProxy(t, &Config{
		Backends: []Backend{
			{Name: "a", Tier: "local", Address: refused()},
			{Name: "b", Tier: "local", Address: refused()},
		},
		Rules: []Rule{{
			Name:  "chain",
			Match: RuleMatch{DataClassification: []string{"internal"}},
			Route: RuleRoute{Backends: []string{"a", "b"}},
		}},
		DefaultRoute: "a",
		Policy:       Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}, WithRouterName(router))
	// The state a served request leaves behind.
	prommetrics.RouterBackendHealth.WithLabelValues(router, "a").Set(1)
	prommetrics.RouterBackendHealth.WithLabelValues(router, "b").Set(1)

	if code := postModel(t, srv.URL, map[string]string{"X-LLMKube-Classification": "internal"}); code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", code)
	}
	for _, name := range []string{"a", "b"} {
		if got := testutil.ToFloat64(prommetrics.RouterBackendHealth.WithLabelValues(router, name)); got != 0 {
			t.Errorf("backend health of %s = %v, want 0", name, got)
		}
	}
	if got := testutil.ToFloat64(prommetrics.RouterRequestsTotal.WithLabelValues(router, "chain", "b", "internal", "all_backends_failed")); got != 1 {
		t.Errorf("requests_total{backend=b,outcome=all_backends_failed} = %v, want 1", got)
	}

	// With both quarantined, the next request skips both without a dispatch;
	// the skip alone must still publish their state.
	prommetrics.RouterBackendHealth.WithLabelValues(router, "a").Set(1)
	prommetrics.RouterBackendHealth.WithLabelValues(router, "b").Set(1)
	if code := postModel(t, srv.URL, map[string]string{"X-LLMKube-Classification": "internal"}); code != http.StatusBadGateway {
		t.Fatalf("status with both quarantined = %d, want 502", code)
	}
	for _, name := range []string{"a", "b"} {
		if got := testutil.ToFloat64(prommetrics.RouterBackendHealth.WithLabelValues(router, name)); got != 0 {
			t.Errorf("backend health of skipped %s = %v, want 0", name, got)
		}
	}
	if got := testutil.ToFloat64(prommetrics.RouterRequestsTotal.WithLabelValues(router, "chain", "b", "internal", "all_backends_failed")); got != 2 {
		t.Errorf("requests_total{backend=b,outcome=all_backends_failed} = %v, want 2", got)
	}
}
