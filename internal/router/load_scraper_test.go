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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/defilantech/llmkube/internal/router/picker"
)

var vllmProfile = MetricsProfile{Path: "/metrics", Running: "vllm:num_requests_running", Waiting: "vllm:num_requests_waiting", KVUsage: "vllm:kv_cache_usage_perc"}

func TestParseEngineGaugesExactNames(t *testing.T) {
	f, err := os.Open("testdata/vllm-metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	running, waiting, kv, _, err := parseEngineGauges(f, vllmProfile)
	if err != nil {
		t.Fatal(err)
	}
	if running != 7 || waiting != 2 || kv != 0.42 {
		t.Errorf("parsed running=%v waiting=%v kv=%v, want 7, 2 (not 4: _by_reason is a different metric), 0.42", running, waiting, kv)
	}
	if _, _, _, _, err := parseEngineGauges(strings.NewReader("vllm:kv_cache_usage_perc 0.1\n"), vllmProfile); err == nil {
		t.Error("a body without running and waiting must be an error")
	}
	two := "vllm:num_requests_running{engine=\"0\"} 1\nvllm:num_requests_running{engine=\"1\"} 2\nvllm:num_requests_waiting 0\n"
	if r, _, _, _, _ := parseEngineGauges(strings.NewReader(two), vllmProfile); r != 3 {
		t.Errorf("label sets must sum: running = %v, want 3", r)
	}

	// Non-finite values must never reach the picker: the line is skipped as if absent.
	nan := "vllm:num_requests_running{engine=\"0\"} NaN\nvllm:num_requests_running{engine=\"1\"} 4\nvllm:num_requests_waiting 1\n"
	if r, w, _, _, err := parseEngineGauges(strings.NewReader(nan), vllmProfile); err != nil || r != 4 || w != 1 {
		t.Errorf("a NaN running line must be skipped: running=%v waiting=%v err=%v, want 4, 1, nil", r, w, err)
	}
	onlyNaN := "vllm:num_requests_running NaN\nvllm:num_requests_waiting 1\n"
	if _, _, _, _, err := parseEngineGauges(strings.NewReader(onlyNaN), vllmProfile); err == nil {
		t.Error("a body whose only running line is NaN must be an error (running missing)")
	}
	inf := "vllm:num_requests_running +Inf\nvllm:num_requests_running 2\nvllm:num_requests_waiting -Inf\nvllm:num_requests_waiting 1\nvllm:kv_cache_usage_perc +Inf\n"
	if r, w, kv, _, err := parseEngineGauges(strings.NewReader(inf), vllmProfile); err != nil || r != 2 || w != 1 || kv != 0 {
		t.Errorf("infinite lines must be skipped: running=%v waiting=%v kv=%v err=%v, want 2, 1, 0, nil", r, w, kv, err)
	}
	// A negative value is clamped to 0 before summing.
	neg := "vllm:num_requests_running -3\nvllm:num_requests_running 2\nvllm:num_requests_waiting -1\n"
	if r, w, _, _, err := parseEngineGauges(strings.NewReader(neg), vllmProfile); err != nil || r != 2 || w != 0 {
		t.Errorf("negative values must clamp to 0: running=%v waiting=%v err=%v, want 2, 0, nil", r, w, err)
	}
}

func scrapedConfig() *Config {
	return &Config{Backends: []Backend{{
		Name: "qwen", Tier: "local", Address: "http://qwen.ns.svc.cluster.local:8000",
		EndpointSource: &EndpointSource{Namespace: "ns", Service: "qwen", Port: 8000},
		Balancing:      &Balancing{Mode: BalancingLoadAware},
		Metrics:        &vllmProfile,
	}}}
}

func TestLoadScraperRecordsSamplesAndFreshness(t *testing.T) {
	body, _ := os.ReadFile("testdata/vllm-metrics.txt")
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer good.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // past the 500 ms scrape timeout
	}))
	defer slow.Close()

	cfg := scrapedConfig()
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{good.URL, slow.URL})
	s := NewLoadScraper(cfg, d, "r-scrape", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !s.HasTargets() {
		t.Fatal("HasTargets = false for a LoadAware backend with a profile")
	}
	s.scrapeOnce(context.Background())

	if smp := d.loadFor("qwen", good.URL).sample.Load(); smp == nil || smp.Running != 7 || smp.Waiting != 2 {
		t.Fatalf("good endpoint sample = %+v, want running 7 waiting 2", smp)
	}
	if smp := d.loadFor("qwen", slow.URL).sample.Load(); smp != nil {
		t.Errorf("a timed-out scrape stored a sample: %+v", smp)
	}
	if got := testutil.ToFloat64(prommetrics.RouterEndpointMetricsFresh.WithLabelValues("r-scrape", "qwen", good.URL, "", "")); got != 1 {
		t.Errorf("fresh gauge for the good endpoint = %v, want 1", got)
	}
	if got := testutil.ToFloat64(prommetrics.RouterEndpointMetricsFresh.WithLabelValues("r-scrape", "qwen", slow.URL, "", "")); got != 0 {
		t.Errorf("fresh gauge for the slow endpoint = %v, want 0", got)
	}
	if got := testutil.ToFloat64(prommetrics.RouterEndpointLoad.WithLabelValues("r-scrape", "qwen", good.URL, "", "")); got != 9 {
		t.Errorf("load gauge for the good endpoint = %v, want 9", got)
	}
}

// TestLoadScraperFailedScrapeAgesOut: once a pod's scrapes start failing, its
// last sample is not refreshed, and past picker.StaleAfter the fresh gauge
// drops to 0 and the load gauge falls back to this replica's in-flight count.
func TestLoadScraperFailedScrapeAgesOut(t *testing.T) {
	body, _ := os.ReadFile("testdata/vllm-metrics.txt")
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			http.Error(w, "engine down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(t0.UnixNano())
	cfg := scrapedConfig()
	d := NewDispatcher(cfg, WithNowFunc(func() time.Time { return time.Unix(0, clock.Load()).UTC() }))
	d.SetEndpoints("qwen", []string{srv.URL})
	s := NewLoadScraper(cfg, d, "r-stale", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.scrapeOnce(context.Background())
	first := d.loadFor("qwen", srv.URL).sample.Load()
	if first == nil || !first.At.Equal(t0) {
		t.Fatalf("first sample = %+v, want one stamped %v", first, t0)
	}

	failing.Store(true)
	d.loadFor("qwen", srv.URL).inflight.Store(3)
	clock.Store(t0.Add(picker.StaleAfter + time.Second).UnixNano())
	s.scrapeOnce(context.Background())

	if got := d.loadFor("qwen", srv.URL).sample.Load(); got != first {
		t.Errorf("a failed scrape replaced the sample: %+v, want the first sample kept", got)
	}
	if got := testutil.ToFloat64(prommetrics.RouterEndpointMetricsFresh.WithLabelValues("r-stale", "qwen", srv.URL, "", "")); got != 0 {
		t.Errorf("fresh gauge after the sample aged out = %v, want 0", got)
	}
	if got := testutil.ToFloat64(prommetrics.RouterEndpointLoad.WithLabelValues("r-stale", "qwen", srv.URL, "", "")); got != 3 {
		t.Errorf("load gauge after the sample aged out = %v, want the in-flight count 3", got)
	}
}

func TestLoadScraperIgnoresUnbalancedBackends(t *testing.T) {
	cfg := scrapedConfig()
	cfg.Backends[0].Balancing = &Balancing{Mode: BalancingRoundRobin}
	if NewLoadScraper(cfg, NewDispatcher(cfg), "r", slog.New(slog.NewTextHandler(io.Discard, nil))).HasTargets() {
		t.Error("a RoundRobin backend must not be scraped")
	}
	cfg.Backends[0].Balancing = &Balancing{Mode: BalancingLoadAware}
	cfg.Backends[0].Metrics = nil
	if NewLoadScraper(cfg, NewDispatcher(cfg), "r", slog.New(slog.NewTextHandler(io.Discard, nil))).HasTargets() {
		t.Error("a backend without a metrics profile must not be scraped")
	}
}

func TestLoadScraperDropsDepartedEndpointSeries(t *testing.T) {
	body, _ := os.ReadFile("testdata/vllm-metrics.txt")
	serve := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	}
	keep, gone := serve(), serve()
	defer keep.Close()
	defer gone.Close()

	cfg := scrapedConfig()
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{keep.URL, gone.URL})
	s := NewLoadScraper(cfg, d, "r-depart", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.scrapeOnce(context.Background())
	d.SetEndpoints("qwen", []string{keep.URL})
	s.scrapeOnce(context.Background())

	for name, vec := range map[string]*prometheus.GaugeVec{"load": prommetrics.RouterEndpointLoad, "fresh": prommetrics.RouterEndpointMetricsFresh} {
		if vec.DeleteLabelValues("r-depart", "qwen", gone.URL, "", "") {
			t.Errorf("%s gauge series of the departed endpoint survived", name)
		}
		if !vec.DeleteLabelValues("r-depart", "qwen", keep.URL, "", "") {
			t.Errorf("%s gauge series of the kept endpoint is missing", name)
		}
	}
}

func TestLoadScraperSkipsEndpointThatLeftMidScrape(t *testing.T) {
	body, _ := os.ReadFile("testdata/vllm-metrics.txt")
	started, release := make(chan struct{}), make(chan struct{})
	leaving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = w.Write(body)
	}))
	defer leaving.Close()

	cfg := scrapedConfig()
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{leaving.URL})
	s := NewLoadScraper(cfg, d, "r-leave", slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() {
		s.scrapeOnce(context.Background())
		close(done)
	}()
	<-started
	d.SetEndpoints("qwen", nil) // the pod leaves while its scrape is in flight
	close(release)
	<-done

	if _, ok := d.loads.Load(healthKey("qwen", leaving.URL)); ok {
		t.Error("a scrape that finished after the pod left re-created its load record")
	}
	if prommetrics.RouterEndpointLoad.DeleteLabelValues("r-leave", "qwen", leaving.URL, "", "") {
		t.Error("a departed endpoint has a load gauge series")
	}
	for name, vec := range pressureHistograms {
		if vec.DeleteLabelValues("r-leave", "qwen", leaving.URL, "", "") {
			t.Errorf("a departed endpoint has a %s histogram series", name)
		}
	}
}

func TestScraperWritesAndDeletesPrefillGauge(t *testing.T) {
	body, _ := os.ReadFile("testdata/vllm-metrics.txt")
	serve := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	}
	keep, gone := serve(), serve()
	defer keep.Close()
	defer gone.Close()

	cfg := scrapedConfig()
	d := NewDispatcher(cfg, WithRouterLabel("r-prefill"))
	d.SetEndpoints("qwen", []string{keep.URL, gone.URL})
	release := d.trackPrefill("qwen", gone.URL, 5000)
	defer release()
	s := NewLoadScraper(cfg, d, "r-prefill", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.scrapeOnce(context.Background())
	if got := testutil.ToFloat64(prommetrics.RouterEndpointPrefillTokens.WithLabelValues("r-prefill", "qwen", gone.URL, "", "")); got != 5000 {
		t.Fatalf("prefill gauge = %v, want 5000", got)
	}

	d.SetEndpoints("qwen", []string{keep.URL})
	s.scrapeOnce(context.Background())
	if prommetrics.RouterEndpointPrefillTokens.DeleteLabelValues("r-prefill", "qwen", gone.URL, "", "") {
		t.Error("prefill gauge series of the departed endpoint survived")
	}
	if prommetrics.RouterPrefillTokensEstimatedTotal.DeleteLabelValues("r-prefill", "qwen", gone.URL, "", "") {
		t.Error("estimated-tokens counter series of the departed endpoint survived")
	}
	if !prommetrics.RouterEndpointPrefillTokens.DeleteLabelValues("r-prefill", "qwen", keep.URL, "", "") {
		t.Error("prefill gauge series of the kept endpoint is missing")
	}
}

func TestLoadScraperLabelsSeriesWithPodAndNode(t *testing.T) {
	body, _ := os.ReadFile("testdata/vllm-metrics.txt")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()

	cfg := scrapedConfig()
	cfg.Backends[0].ConcurrencyLimit = 8
	d := NewDispatcher(cfg)
	d.SetReadyEndpoints("qwen", []ReadyEndpoint{{URL: srv.URL, EndpointTarget: EndpointTarget{Pod: "qwen-a", Node: "gpu-1"}}})
	s := NewLoadScraper(cfg, d, "r-target", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.scrapeOnce(context.Background())

	for name, vec := range map[string]*prometheus.GaugeVec{
		"load": prommetrics.RouterEndpointLoad, "fresh": prommetrics.RouterEndpointMetricsFresh, "prefill": prommetrics.RouterEndpointPrefillTokens,
	} {
		if !vec.DeleteLabelValues("r-target", "qwen", srv.URL, "qwen-a", "gpu-1") {
			t.Errorf("%s series is not labelled with the endpoint's pod and node", name)
		}
	}
	for name, vec := range pressureHistograms {
		if !vec.DeleteLabelValues("r-target", "qwen", srv.URL, "qwen-a", "gpu-1") {
			t.Errorf("%s histogram is not labelled with the endpoint's pod and node", name)
		}
	}
}

// A pod IP reused by another Pod (or the same Pod rescheduled elsewhere) must
// not keep series labelled with the old one: they would double the endpoint.
func TestLoadScraperDropsSeriesOfAReplacedTarget(t *testing.T) {
	body, _ := os.ReadFile("testdata/vllm-metrics.txt")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()

	cfg := scrapedConfig()
	cfg.Backends[0].ConcurrencyLimit = 8
	d := NewDispatcher(cfg, WithRouterLabel("r-reuse"))
	d.SetReadyEndpoints("qwen", []ReadyEndpoint{{URL: srv.URL, EndpointTarget: EndpointTarget{Pod: "qwen-old", Node: "gpu-1"}}})
	s := NewLoadScraper(cfg, d, "r-reuse", slog.New(slog.NewTextHandler(io.Discard, nil)))
	release := d.trackPrefill("qwen", srv.URL, 100)
	release()
	s.scrapeOnce(context.Background())

	d.SetReadyEndpoints("qwen", []ReadyEndpoint{{URL: srv.URL, EndpointTarget: EndpointTarget{Pod: "qwen-new", Node: "gpu-2"}}})
	s.scrapeOnce(context.Background())

	for name, vec := range map[string]*prometheus.GaugeVec{"load": prommetrics.RouterEndpointLoad, "fresh": prommetrics.RouterEndpointMetricsFresh} {
		if vec.DeleteLabelValues("r-reuse", "qwen", srv.URL, "qwen-old", "gpu-1") {
			t.Errorf("%s series of the replaced pod survived", name)
		}
		if !vec.DeleteLabelValues("r-reuse", "qwen", srv.URL, "qwen-new", "gpu-2") {
			t.Errorf("%s series of the new pod is missing", name)
		}
	}
	for name, vec := range pressureHistograms {
		if vec.DeleteLabelValues("r-reuse", "qwen", srv.URL, "qwen-old", "gpu-1") {
			t.Errorf("%s histogram of the replaced pod survived", name)
		}
		if !vec.DeleteLabelValues("r-reuse", "qwen", srv.URL, "qwen-new", "gpu-2") {
			t.Errorf("%s histogram of the new pod is missing", name)
		}
	}
	if prommetrics.RouterPrefillTokensEstimatedTotal.DeleteLabelValues("r-reuse", "qwen", srv.URL, "qwen-old", "gpu-1") {
		t.Error("prefill counter of the replaced pod survived")
	}
}

var pressureHistograms = map[string]*prometheus.HistogramVec{
	"kv":          prommetrics.RouterEndpointKVUsage,
	"utilization": prommetrics.RouterEndpointSlotUtilization,
	"running":     prommetrics.RouterEndpointRunningRequests,
	"waiting":     prommetrics.RouterEndpointWaitingRequests,
}

// resetPressureSeries drops one router label's pressure histograms, so a test
// can assert absolute counts under -count above 1.
func resetPressureSeries(router string) {
	for _, vec := range pressureHistograms {
		vec.DeletePartialMatch(prometheus.Labels{"router": router})
	}
}

// histogramOf reads one series' histogram without asserting it existed.
func histogramOf(t *testing.T, vec *prometheus.HistogramVec, lvs ...string) *dto.Histogram {
	t.Helper()
	m := &dto.Metric{}
	if err := vec.WithLabelValues(lvs...).(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("read histogram: %v", err)
	}
	return m.GetHistogram()
}

// cumulative returns the count of observations at or below le.
func cumulative(h *dto.Histogram, le float64) uint64 {
	for _, b := range h.GetBucket() {
		if b.GetUpperBound() == le {
			return b.GetCumulativeCount()
		}
	}
	return 0
}

// TestLoadScraperRecordsPressureHistograms: each stored sample is one
// observation, in the bucket its value belongs to. The fixture reads running 7,
// waiting 2, KV 0.42; with a limit of 8, utilization is 0.875.
func TestLoadScraperRecordsPressureHistograms(t *testing.T) {
	const router = "r-pressure"
	resetPressureSeries(router)
	body, _ := os.ReadFile("testdata/vllm-metrics.txt")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()

	cfg := scrapedConfig()
	cfg.Backends[0].ConcurrencyLimit = 8
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})
	s := NewLoadScraper(cfg, d, router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.scrapeOnce(context.Background())
	s.scrapeOnce(context.Background())

	for _, tc := range []struct {
		name       string
		below, at  float64 // the value is above below and at or under at
		wantSample uint64
	}{
		{"kv", 0.25, 0.5, 2},
		{"utilization", 0.85, 0.9, 2},
		{"running", 4, 8, 2},
		{"waiting", 1, 2, 2},
	} {
		h := histogramOf(t, pressureHistograms[tc.name], router, "qwen", srv.URL, "", "")
		if h.GetSampleCount() != tc.wantSample {
			t.Errorf("%s: sample count = %d, want %d (one per scrape)", tc.name, h.GetSampleCount(), tc.wantSample)
		}
		if got := cumulative(h, tc.below); got != 0 {
			t.Errorf("%s: %d observations at or under %v, want 0", tc.name, got, tc.below)
		}
		if got := cumulative(h, tc.at); got != tc.wantSample {
			t.Errorf("%s: %d observations at or under %v, want %d", tc.name, got, tc.at, tc.wantSample)
		}
	}
}

// TestLoadScraperPressureHistogramsSkipUnknowns: no utilization series without
// a concurrency limit, no KV series for a profile without a KV gauge or an
// engine that doesn't expose the profile's one, and nothing at all for a pod
// whose scrape failed.
func TestLoadScraperWaitingHistogramResolvesOverload(t *testing.T) {
	const router = "r-pressure-overload"
	resetPressureSeries(router)
	body := "vllm:num_requests_running 128\nvllm:num_requests_waiting 300\nvllm:kv_cache_usage_perc 0.99\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()

	cfg := scrapedConfig()
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})
	s := NewLoadScraper(cfg, d, router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.scrapeOnce(context.Background())

	h := histogramOf(t, prommetrics.RouterEndpointWaitingRequests, router, "qwen", srv.URL, "", "")
	if got := cumulative(h, 256); got != 0 {
		t.Errorf("%d waiting observations at or under 256, want 0", got)
	}
	if got := cumulative(h, 512); got != 1 {
		t.Errorf("%d waiting observations at or under 512, want 1 (300 waiting must land in a finite bucket)", got)
	}
}

func TestLoadScraperPressureHistogramsSkipUnknowns(t *testing.T) {
	const router = "r-pressure-skip"
	resetPressureSeries(router)
	body, _ := os.ReadFile("testdata/vllm-metrics.txt")
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer good.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "engine down", http.StatusInternalServerError)
	}))
	defer down.Close()

	cfg := scrapedConfig()
	noKV := vllmProfile
	noKV.KVUsage = ""
	cfg.Backends[0].Metrics = &noKV
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{good.URL, down.URL})
	s := NewLoadScraper(cfg, d, router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.scrapeOnce(context.Background())

	if !prommetrics.RouterEndpointRunningRequests.DeleteLabelValues(router, "qwen", good.URL, "", "") {
		t.Fatal("running histogram missing for the scraped pod")
	}
	if prommetrics.RouterEndpointSlotUtilization.DeleteLabelValues(router, "qwen", good.URL, "", "") {
		t.Error("utilization recorded for a backend without a concurrency limit")
	}
	if prommetrics.RouterEndpointKVUsage.DeleteLabelValues(router, "qwen", good.URL, "", "") {
		t.Error("KV usage recorded for a profile without a KV gauge")
	}

	// The profile names a KV gauge the engine's exposition lacks.
	noGauge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("vllm:num_requests_running 3\nvllm:num_requests_waiting 0\n"))
	}))
	defer noGauge.Close()
	cfg = scrapedConfig()
	d = NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{noGauge.URL})
	NewLoadScraper(cfg, d, router, slog.New(slog.NewTextHandler(io.Discard, nil))).scrapeOnce(context.Background())
	if !prommetrics.RouterEndpointRunningRequests.DeleteLabelValues(router, "qwen", noGauge.URL, "", "") {
		t.Fatal("running histogram missing for the pod without a KV gauge")
	}
	if prommetrics.RouterEndpointKVUsage.DeleteLabelValues(router, "qwen", noGauge.URL, "", "") {
		t.Error("KV usage recorded for an exposition without the profile's KV gauge")
	}
	for name, vec := range pressureHistograms {
		if vec.DeleteLabelValues(router, "qwen", down.URL, "", "") {
			t.Errorf("%s recorded for a pod whose scrape failed", name)
		}
	}
}

func TestLoadScraperDropsDepartedEndpointHistograms(t *testing.T) {
	const router = "r-pressure-depart"
	resetPressureSeries(router)
	body, _ := os.ReadFile("testdata/vllm-metrics.txt")
	serve := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	}
	keep, gone := serve(), serve()
	defer keep.Close()
	defer gone.Close()

	cfg := scrapedConfig()
	cfg.Backends[0].ConcurrencyLimit = 8
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{keep.URL, gone.URL})
	s := NewLoadScraper(cfg, d, router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.scrapeOnce(context.Background())
	d.SetEndpoints("qwen", []string{keep.URL})
	s.scrapeOnce(context.Background())

	for name, vec := range pressureHistograms {
		if vec.DeleteLabelValues(router, "qwen", gone.URL, "", "") {
			t.Errorf("%s histogram of the departed endpoint survived", name)
		}
		if !vec.DeleteLabelValues(router, "qwen", keep.URL, "", "") {
			t.Errorf("%s histogram of the kept endpoint is missing", name)
		}
	}
}

// TestLoadScraperRecordsRealVLLMNightlyBytes feeds the scraper a vLLM nightly
// /metrics capture (trimmed to the parsed families and the
// num_requests_waiting_by_reason sibling, lines verbatim) from a GLM pod under
// load, served with --max-num-seqs 104.
func TestLoadScraperRecordsRealVLLMNightlyBytes(t *testing.T) {
	const router = "r-pressure-real"
	resetPressureSeries(router)
	body, err := os.ReadFile("testdata/vllm-nightly-glm-loaded.txt")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()

	cfg := scrapedConfig()
	cfg.Backends[0].ConcurrencyLimit = 104
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})
	s := NewLoadScraper(cfg, d, router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.scrapeOnce(context.Background())

	for _, tc := range []struct {
		name string
		want float64
	}{
		{"running", 3},
		{"waiting", 0},
		{"kv", 0.02488184598276344},
		{"utilization", 3.0 / 104},
	} {
		h := histogramOf(t, pressureHistograms[tc.name], router, "qwen", srv.URL, "", "")
		if h.GetSampleCount() != 1 || h.GetSampleSum() != tc.want {
			t.Errorf("%s: %d observations summing to %v, want 1 of %v", tc.name, h.GetSampleCount(), h.GetSampleSum(), tc.want)
		}
	}
}

func scrapeFailures(t *testing.T, router, ep, pod, node string) float64 {
	t.Helper()
	return testutil.ToFloat64(prommetrics.RouterEndpointLoadScrapeFailuresTotal.WithLabelValues(router, "qwen", ep, pod, node))
}

// TestLoadScraperCountsFailedScrapes: each failure mode that leaves a live pod
// without a sample counts once per scrape, under the pod's labels; a good
// scrape counts nothing.
func TestLoadScraperCountsFailedScrapes(t *testing.T) {
	const router = "r-scrape-fail"
	prommetrics.RouterEndpointLoadScrapeFailuresTotal.DeletePartialMatch(prometheus.Labels{"router": router})
	body, _ := os.ReadFile("testdata/vllm-metrics.txt")
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer good.Close()
	status := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "engine down", http.StatusServiceUnavailable)
	}))
	defer status.Close()
	parse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("# no engine gauges here\nother_metric 1\n"))
	}))
	defer parse.Close()
	unblock := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(unblock)

	cfg := scrapedConfig()
	d := NewDispatcher(cfg)
	d.SetReadyEndpoints("qwen", []ReadyEndpoint{
		{URL: good.URL, EndpointTarget: EndpointTarget{Pod: "good", Node: "n1"}},
		{URL: status.URL, EndpointTarget: EndpointTarget{Pod: "status", Node: "n1"}},
		{URL: parse.URL, EndpointTarget: EndpointTarget{Pod: "parse", Node: "n2"}},
		{URL: slow.URL, EndpointTarget: EndpointTarget{Pod: "slow", Node: "n2"}},
	})
	s := NewLoadScraper(cfg, d, router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.scrapeOnce(context.Background())
	s.scrapeOnce(context.Background())

	for _, tc := range []struct{ ep, pod, node string }{
		{status.URL, "status", "n1"}, {parse.URL, "parse", "n2"}, {slow.URL, "slow", "n2"},
	} {
		if got := scrapeFailures(t, router, tc.ep, tc.pod, tc.node); got != 2 {
			t.Errorf("%s: failures = %v, want 2 (one per scrape)", tc.pod, got)
		}
	}
	if prommetrics.RouterEndpointLoadScrapeFailuresTotal.DeleteLabelValues(router, "qwen", good.URL, "good", "n1") {
		t.Error("a good scrape counted as a failure")
	}
}

// A scrape cut short by shutdown says nothing about the pod and must not count.
func TestLoadScraperDoesNotCountShutdownAsFailure(t *testing.T) {
	const router = "r-scrape-shutdown"
	prommetrics.RouterEndpointLoadScrapeFailuresTotal.DeletePartialMatch(prometheus.Labels{"router": router})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "engine down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := scrapedConfig()
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})
	s := NewLoadScraper(cfg, d, router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.scrapeOnce(ctx)

	if prommetrics.RouterEndpointLoadScrapeFailuresTotal.DeleteLabelValues(router, "qwen", srv.URL, "", "") {
		t.Error("a scrape cancelled by shutdown counted as a failure")
	}
}

func TestLoadScraperDropsDepartedEndpointFailureCounter(t *testing.T) {
	const router = "r-scrape-fail-depart"
	prommetrics.RouterEndpointLoadScrapeFailuresTotal.DeletePartialMatch(prometheus.Labels{"router": router})
	down := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "engine down", http.StatusServiceUnavailable)
		}))
	}
	keep, gone := down(), down()
	defer keep.Close()
	defer gone.Close()

	cfg := scrapedConfig()
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{keep.URL, gone.URL})
	s := NewLoadScraper(cfg, d, router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.scrapeOnce(context.Background())
	d.SetEndpoints("qwen", []string{keep.URL})
	s.scrapeOnce(context.Background())

	if prommetrics.RouterEndpointLoadScrapeFailuresTotal.DeleteLabelValues(router, "qwen", gone.URL, "", "") {
		t.Error("failure counter of the departed endpoint survived")
	}
	if !prommetrics.RouterEndpointLoadScrapeFailuresTotal.DeleteLabelValues(router, "qwen", keep.URL, "", "") {
		t.Error("failure counter of the kept endpoint is missing")
	}
}

// A pod that left while its failing scrape was in flight is gone, not
// unsampled: counting it would leave a series nothing deletes.
func TestLoadScraperDoesNotCountFailureOfDepartedEndpoint(t *testing.T) {
	const router = "r-scrape-fail-leave"
	prommetrics.RouterEndpointLoadScrapeFailuresTotal.DeletePartialMatch(prometheus.Labels{"router": router})
	started, release := make(chan struct{}), make(chan struct{})
	leaving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		http.Error(w, "engine down", http.StatusServiceUnavailable)
	}))
	defer leaving.Close()

	cfg := scrapedConfig()
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{leaving.URL})
	s := NewLoadScraper(cfg, d, router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() {
		s.scrapeOnce(context.Background())
		close(done)
	}()
	<-started
	d.SetEndpoints("qwen", nil)
	close(release)
	<-done

	if prommetrics.RouterEndpointLoadScrapeFailuresTotal.DeleteLabelValues(router, "qwen", leaving.URL, "", "") {
		t.Error("a pod that left mid-scrape has a failure counter series")
	}
}
