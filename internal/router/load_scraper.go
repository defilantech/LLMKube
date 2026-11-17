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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/defilantech/llmkube/internal/router/picker"
)

const (
	scrapeInterval = time.Second
	scrapeTimeout  = 500 * time.Millisecond

	// pollFailuresToQuarantine is how many load polls in a row must fail to
	// connect before an endpoint leaves placement: as many as fit in
	// picker.StaleAfter, so an endpoint is quarantined when its last sample
	// would read stale anyway, and a pod behind a path that drops a few polls
	// in a row is not.
	pollFailuresToQuarantine = int32(picker.StaleAfter / scrapeInterval)
)

type scrapeTarget struct {
	backend string
	profile MetricsProfile
	limit   int32
}

// LoadScraper reads each pod's engine metrics for every LoadAware backend
// with a metrics profile and stores them in the dispatcher.
type LoadScraper struct {
	disp       *Dispatcher
	client     *http.Client
	targets    []scrapeTarget
	routerName string
	logger     *slog.Logger

	// written is the series keys (backend, endpoint, pod, node) the last
	// scrapeOnce wrote. The scraper owns both gauge series: it alone writes
	// and deletes them, from scrapeOnce, so a departed endpoint's series
	// cannot be brought back by a late scrape. scrapeOnce must not run
	// concurrently with itself.
	written map[gaugeKey]struct{}
}

// gaugeKey carries the pod and node too, so a URL that moves to another
// Pod drops the series labelled with the old one.
type gaugeKey struct{ backend, endpoint, pod, node string }

// NewLoadScraper indexes the backends that need scraping. It starts nothing.
func NewLoadScraper(cfg *Config, disp *Dispatcher, routerName string, logger *slog.Logger) *LoadScraper {
	s := &LoadScraper{
		disp: disp, routerName: routerName, logger: logger,
		client:  &http.Client{Timeout: scrapeTimeout},
		written: map[gaugeKey]struct{}{},
	}
	for _, b := range cfg.Backends {
		if b.Balancing == nil || b.Balancing.Mode != BalancingLoadAware || b.Metrics == nil {
			continue
		}
		s.targets = append(s.targets, scrapeTarget{backend: b.Name, profile: *b.Metrics, limit: b.ConcurrencyLimit})
	}
	return s
}

// HasTargets reports whether any backend needs scraping.
func (s *LoadScraper) HasTargets() bool { return len(s.targets) > 0 }

// Start scrapes every scrapeInterval until ctx ends.
func (s *LoadScraper) Start(ctx context.Context) {
	t := time.NewTicker(scrapeInterval)
	defer t.Stop()
	for {
		s.scrapeOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// errPollUnreachable marks a scrape that got no HTTP response: the endpoint
// refused, reset or timed out the connection.
var errPollUnreachable = errors.New("endpoint unreachable")

// scrapeOnce scrapes every current endpoint of every target once, in parallel,
// then refreshes the load and freshness gauges. A failed scrape stores nothing,
// so the endpoint's last sample ages out and placement falls back to in-flight
// after picker.StaleAfter. A scrape that got no HTTP response also counts
// toward the endpoint's poll quarantine; any HTTP response clears it, since a
// runtime without the metrics path may still serve inference.
func (s *LoadScraper) scrapeOnce(ctx context.Context) {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		observed = map[gaugeKey]struct{}{}
	)
	for _, tgt := range s.targets {
		for _, ep := range s.disp.EndpointsOf(tgt.backend) {
			wg.Add(1)
			go func(tgt scrapeTarget, ep string) {
				defer wg.Done()
				running, waiting, kv, err := s.scrape(ctx, ep, tgt.profile)
				if ctx.Err() != nil {
					return // shutting down; the failure says nothing about the endpoint
				}
				if err != nil {
					s.logger.Debug("engine metrics scrape failed", "backend", tgt.backend, "endpoint", ep, "error", err)
					if s.stillLive(tgt.backend, ep) {
						k := s.countScrapeFailure(tgt.backend, ep)
						mu.Lock()
						observed[k] = struct{}{}
						mu.Unlock()
					}
					if errors.Is(err, errPollUnreachable) {
						s.disp.recordPollFailure(tgt.backend, ep, err)
					} else {
						s.disp.recordPollAnswered(tgt.backend, ep)
					}
					return
				}
				if !s.stillLive(tgt.backend, ep) {
					return // the pod left while this scrape was in flight
				}
				s.disp.recordPollAnswered(tgt.backend, ep)
				s.disp.RecordSample(tgt.backend, ep, picker.Sample{Running: running, Waiting: waiting, KVUsage: kv, At: s.disp.nowFn()})
				k := s.observeSample(tgt, ep, running, waiting, kv)
				mu.Lock()
				observed[k] = struct{}{}
				mu.Unlock()
			}(tgt, ep)
		}
	}
	wg.Wait()
	now := s.disp.nowFn()
	current := map[gaugeKey]struct{}{}
	for _, tgt := range s.targets {
		for _, ep := range s.disp.EndpointsOf(tgt.backend) {
			l := s.disp.loadFor(tgt.backend, ep)
			c := picker.Candidate{Endpoint: ep, InFlight: l.inflight.Load(), Sample: l.sample.Load()}
			t := s.disp.targetOf(tgt.backend, ep)
			prommetrics.RouterEndpointLoad.WithLabelValues(s.routerName, tgt.backend, ep, t.Pod, t.Node).Set(float64(picker.Load(c, now)))
			fresh := 0.0
			if c.Sample != nil && now.Sub(c.Sample.At) <= picker.StaleAfter {
				fresh = 1
			}
			prommetrics.RouterEndpointMetricsFresh.WithLabelValues(s.routerName, tgt.backend, ep, t.Pod, t.Node).Set(fresh)
			prommetrics.RouterEndpointPrefillTokens.WithLabelValues(s.routerName, tgt.backend, ep, t.Pod, t.Node).Set(float64(l.prefill.Load()))
			current[gaugeKey{tgt.backend, ep, t.Pod, t.Node}] = struct{}{}
		}
	}
	// A histogram observed for a key the gauges did not write (the pod left, or
	// its pod or node changed, after its scrape) would otherwise never be
	// deleted.
	for k := range observed {
		if _, ok := current[k]; !ok {
			s.deleteSeries(k)
		}
	}
	for k := range s.written {
		if _, ok := current[k]; !ok {
			s.deleteSeries(k)
		}
	}
	s.written = current
}

// deleteSeries drops every per-endpoint series the scraper writes for k.
func (s *LoadScraper) deleteSeries(k gaugeKey) {
	lvs := []string{s.routerName, k.backend, k.endpoint, k.pod, k.node}
	prommetrics.RouterEndpointLoad.DeleteLabelValues(lvs...)
	prommetrics.RouterEndpointMetricsFresh.DeleteLabelValues(lvs...)
	prommetrics.RouterEndpointPrefillTokens.DeleteLabelValues(lvs...)
	prommetrics.RouterPrefillTokensEstimatedTotal.DeleteLabelValues(lvs...)
	prommetrics.RouterEndpointKVUsage.DeleteLabelValues(lvs...)
	prommetrics.RouterEndpointSlotUtilization.DeleteLabelValues(lvs...)
	prommetrics.RouterEndpointRunningRequests.DeleteLabelValues(lvs...)
	prommetrics.RouterEndpointWaitingRequests.DeleteLabelValues(lvs...)
	prommetrics.RouterEndpointLoadScrapeFailuresTotal.DeleteLabelValues(lvs...)
}

// countScrapeFailure records a live pod's failed scrape and returns its key.
func (s *LoadScraper) countScrapeFailure(backend, ep string) gaugeKey {
	t := s.disp.targetOf(backend, ep)
	prommetrics.RouterEndpointLoadScrapeFailuresTotal.WithLabelValues(s.routerName, backend, ep, t.Pod, t.Node).Inc()
	return gaugeKey{backend, ep, t.Pod, t.Node}
}

// observeSample records one stored sample in the pressure histograms. KV is
// skipped for a profile without a KV gauge, whose parsed value is a
// placeholder 0, and slot utilization for a backend without a known limit.
// It returns the series key it wrote.
func (s *LoadScraper) observeSample(tgt scrapeTarget, ep string, running, waiting, kv float64) gaugeKey {
	t := s.disp.targetOf(tgt.backend, ep)
	lvs := []string{s.routerName, tgt.backend, ep, t.Pod, t.Node}
	prommetrics.RouterEndpointRunningRequests.WithLabelValues(lvs...).Observe(running)
	prommetrics.RouterEndpointWaitingRequests.WithLabelValues(lvs...).Observe(waiting)
	if tgt.profile.KVUsage != "" {
		prommetrics.RouterEndpointKVUsage.WithLabelValues(lvs...).Observe(kv)
	}
	if tgt.limit > 0 {
		prommetrics.RouterEndpointSlotUtilization.WithLabelValues(lvs...).Observe(running / float64(tgt.limit))
	}
	return gaugeKey{tgt.backend, ep, t.Pod, t.Node}
}

// stillLive reports whether endpoint is in backend's current endpoint set.
func (s *LoadScraper) stillLive(backend, endpoint string) bool {
	for _, ep := range s.disp.EndpointsOf(backend) {
		if ep == endpoint {
			return true
		}
	}
	return false
}

func (s *LoadScraper) scrape(ctx context.Context, endpoint string, p MetricsProfile) (float64, float64, float64, error) {
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+p.Path, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%w: %w", errPollUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	return parseEngineGauges(io.LimitReader(resp.Body, 4<<20), p)
}

// parseEngineGauges sums the samples of the profile's three gauges in a
// Prometheus text exposition. A metric matches only by its exact name, so a
// sibling such as vllm:num_requests_waiting_by_reason is not counted. Running
// and waiting are required; KV use is optional and takes the maximum across
// label sets. A sample whose value is NaN or +/-Inf is skipped as if the line
// were absent, so a non-finite gauge never reaches the picker (converting a
// non-finite float to an integer is architecture-dependent in Go), and a
// negative value is clamped to 0 before summing.
func parseEngineGauges(r io.Reader, p MetricsProfile) (running, waiting, kv float64, err error) {
	var haveRunning, haveWaiting bool
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		nameEnd := strings.IndexAny(line, "{ ")
		if nameEnd < 0 {
			continue
		}
		name := line[:nameEnd]
		rest := line[nameEnd:]
		if i := strings.LastIndexByte(rest, '}'); i >= 0 && rest[0] == '{' {
			rest = rest[i+1:]
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, perr := strconv.ParseFloat(fields[0], 64)
		if perr != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		if v < 0 {
			v = 0
		}
		switch name {
		case p.Running:
			running += v
			haveRunning = true
		case p.Waiting:
			waiting += v
			haveWaiting = true
		case p.KVUsage:
			if p.KVUsage != "" && v > kv {
				kv = v
			}
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, 0, err
	}
	if !haveRunning || !haveWaiting {
		return 0, 0, 0, fmt.Errorf("metrics lack %s or %s", p.Running, p.Waiting)
	}
	return running, waiting, kv, nil
}
