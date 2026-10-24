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
	"net/http"
	"strings"
	"time"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/defilantech/llmkube/internal/router/picker"
)

// sessionKey returns the first configured header with a non-empty value. The
// value is used only to hash into a table key and onto the ring; it is never
// stored, logged or used as a metric label.
func sessionKey(h http.Header, names []string) string {
	if h == nil {
		return ""
	}
	for _, n := range names {
		if v := strings.TrimSpace(h.Get(n)); v != "" {
			return v
		}
	}
	return ""
}

// ringCache is a backend's ring and the endpoint set it was built from.
type ringCache struct {
	set  string
	ring *picker.Ring
}

// ringFor returns the backend's ring for the current endpoint set, rebuilding
// it when the set changed.
func (d *Dispatcher) ringFor(backend string, eps []string) *picker.Ring {
	set := strings.Join(eps, ",")
	if v, ok := d.rings.Load(backend); ok {
		if rc := v.(*ringCache); rc.set == set {
			return rc.ring
		}
	}
	rc := &ringCache{set: set, ring: picker.NewRing(eps)}
	d.rings.Store(backend, rc)
	return rc.ring
}

// sessionsFor returns the backend's session table, creating it on first use.
func (d *Dispatcher) sessionsFor(backend string, bal *Balancing) *picker.Sessions {
	if v, ok := d.sessions.Load(backend); ok {
		return v.(*picker.Sessions)
	}
	v, _ := d.sessions.LoadOrStore(backend, picker.NewSessions(bal.SessionIdleTimeout, picker.MaxSessions))
	return v.(*picker.Sessions)
}

// SweepSessions drops expired bindings from every table and publishes each
// table's size.
func (d *Dispatcher) SweepSessions(now time.Time) {
	d.sessions.Range(func(k, v any) bool {
		tbl := v.(*picker.Sessions)
		n := tbl.Sweep(now)
		prommetrics.RouterSessionBindings.WithLabelValues(d.routerName, k.(string)).Set(float64(n))
		return true
	})
}

// RunSessionSweeper sweeps the session tables every interval until ctx is done.
func (d *Dispatcher) RunSessionSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.SweepSessions(d.nowFn())
		}
	}
}

// place chooses the endpoint for a request. Backends that are not LoadAware
// round-robin exactly as before, ignore session headers and report an empty
// result. The int64 is the request's estimated uncached prompt tokens, 0 when
// the backend is not LoadAware.
func (d *Dispatcher) place(backend *Backend, headers http.Header, bodyBytes int64) (string, picker.Result, picker.Reason, int64) {
	eps := d.liveEndpoints(backend)
	bal := backend.Balancing
	if len(eps) == 0 || bal == nil || bal.Mode != BalancingLoadAware {
		return d.roundRobin(backend, eps), "", picker.ReasonNone, 0
	}
	cands := make([]picker.Candidate, 0, len(eps))
	for _, ep := range eps {
		if !d.dispatchable(healthKey(backend.Name, ep)) {
			continue
		}
		l := d.loadFor(backend.Name, ep)
		cands = append(cands, picker.Candidate{Endpoint: ep, InFlight: l.inflight.Load(), Sample: l.sample.Load(), Prefill: l.prefill.Load()})
	}
	if len(cands) == 0 {
		return d.roundRobin(backend, eps), picker.Fallback, picker.ReasonNone, 0
	}
	now := d.nowFn()
	// The tie start shares the round-robin cursor so equal loads rotate; the
	// pickers accept any int, so a wrapped conversion is fine.
	tie := int(d.endpointCursor(backend.Name).Add(1) - 1) //nolint:gosec // wraps harmlessly, the pickers take any int
	if key := sessionKey(headers, bal.SessionHeaders); key != "" {
		return d.sessionsFor(backend.Name, bal).Place(
			picker.SessionHash(key), cands, now, tie, d.ringFor(backend.Name, eps).Home(key), bodyBytes)
	}
	ep, res := picker.PickLeastLoaded(picker.Eligible(cands, now), now, tie)
	return ep, res, picker.ReasonNone, picker.EstimateTokens(bodyBytes)
}

// isTokenCountPath reports whether path only counts a prompt's tokens.
func isTokenCountPath(path string) bool {
	return path == "/v1/messages/count_tokens"
}

// countPlacement records a balanced placement decision.
func (d *Dispatcher) countPlacement(backend string, res picker.Result, reason picker.Reason) {
	if res == "" {
		return
	}
	prommetrics.RouterPlacementsTotal.WithLabelValues(d.routerName, backend, string(res), string(reason)).Inc()
}
