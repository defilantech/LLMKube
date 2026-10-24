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
	running, waiting, kv, err := parseEngineGauges(f, vllmProfile)
	if err != nil {
		t.Fatal(err)
	}
	if running != 7 || waiting != 2 || kv != 0.42 {
		t.Errorf("parsed running=%v waiting=%v kv=%v, want 7, 2 (not 4: _by_reason is a different metric), 0.42", running, waiting, kv)
	}
	if _, _, _, err := parseEngineGauges(strings.NewReader("vllm:kv_cache_usage_perc 0.1\n"), vllmProfile); err == nil {
		t.Error("a body without running and waiting must be an error")
	}
	two := "vllm:num_requests_running{engine=\"0\"} 1\nvllm:num_requests_running{engine=\"1\"} 2\nvllm:num_requests_waiting 0\n"
	if r, _, _, _ := parseEngineGauges(strings.NewReader(two), vllmProfile); r != 3 {
		t.Errorf("label sets must sum: running = %v, want 3", r)
	}

	// Non-finite values must never reach the picker: the line is skipped as if absent.
	nan := "vllm:num_requests_running{engine=\"0\"} NaN\nvllm:num_requests_running{engine=\"1\"} 4\nvllm:num_requests_waiting 1\n"
	if r, w, _, err := parseEngineGauges(strings.NewReader(nan), vllmProfile); err != nil || r != 4 || w != 1 {
		t.Errorf("a NaN running line must be skipped: running=%v waiting=%v err=%v, want 4, 1, nil", r, w, err)
	}
	onlyNaN := "vllm:num_requests_running NaN\nvllm:num_requests_waiting 1\n"
	if _, _, _, err := parseEngineGauges(strings.NewReader(onlyNaN), vllmProfile); err == nil {
		t.Error("a body whose only running line is NaN must be an error (running missing)")
	}
	inf := "vllm:num_requests_running +Inf\nvllm:num_requests_running 2\nvllm:num_requests_waiting -Inf\nvllm:num_requests_waiting 1\nvllm:kv_cache_usage_perc +Inf\n"
	if r, w, kv, err := parseEngineGauges(strings.NewReader(inf), vllmProfile); err != nil || r != 2 || w != 1 || kv != 0 {
		t.Errorf("infinite lines must be skipped: running=%v waiting=%v kv=%v err=%v, want 2, 1, 0, nil", r, w, kv, err)
	}
	// A negative value is clamped to 0 before summing.
	neg := "vllm:num_requests_running -3\nvllm:num_requests_running 2\nvllm:num_requests_waiting -1\n"
	if r, w, _, err := parseEngineGauges(strings.NewReader(neg), vllmProfile); err != nil || r != 2 || w != 0 {
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
	if got := testutil.ToFloat64(prommetrics.RouterEndpointMetricsFresh.WithLabelValues("r-scrape", "qwen", good.URL)); got != 1 {
		t.Errorf("fresh gauge for the good endpoint = %v, want 1", got)
	}
	if got := testutil.ToFloat64(prommetrics.RouterEndpointMetricsFresh.WithLabelValues("r-scrape", "qwen", slow.URL)); got != 0 {
		t.Errorf("fresh gauge for the slow endpoint = %v, want 0", got)
	}
	if got := testutil.ToFloat64(prommetrics.RouterEndpointLoad.WithLabelValues("r-scrape", "qwen", good.URL)); got != 9 {
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
	if got := testutil.ToFloat64(prommetrics.RouterEndpointMetricsFresh.WithLabelValues("r-stale", "qwen", srv.URL)); got != 0 {
		t.Errorf("fresh gauge after the sample aged out = %v, want 0", got)
	}
	if got := testutil.ToFloat64(prommetrics.RouterEndpointLoad.WithLabelValues("r-stale", "qwen", srv.URL)); got != 3 {
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
		if vec.DeleteLabelValues("r-depart", "qwen", gone.URL) {
			t.Errorf("%s gauge series of the departed endpoint survived", name)
		}
		if !vec.DeleteLabelValues("r-depart", "qwen", keep.URL) {
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
	if prommetrics.RouterEndpointLoad.DeleteLabelValues("r-leave", "qwen", leaving.URL) {
		t.Error("a departed endpoint has a load gauge series")
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
	if got := testutil.ToFloat64(prommetrics.RouterEndpointPrefillTokens.WithLabelValues("r-prefill", "qwen", gone.URL)); got != 5000 {
		t.Fatalf("prefill gauge = %v, want 5000", got)
	}

	d.SetEndpoints("qwen", []string{keep.URL})
	s.scrapeOnce(context.Background())
	if prommetrics.RouterEndpointPrefillTokens.DeleteLabelValues("r-prefill", "qwen", gone.URL) {
		t.Error("prefill gauge series of the departed endpoint survived")
	}
	if prommetrics.RouterPrefillTokensEstimatedTotal.DeleteLabelValues("r-prefill", "qwen", gone.URL) {
		t.Error("estimated-tokens counter series of the departed endpoint survived")
	}
	if !prommetrics.RouterEndpointPrefillTokens.DeleteLabelValues("r-prefill", "qwen", keep.URL) {
		t.Error("prefill gauge series of the kept endpoint is missing")
	}
}
