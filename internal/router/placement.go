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
	"strconv"
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

// prefixFor returns the backend's prefix index, creating it on first use.
func (d *Dispatcher) prefixFor(backend string, bal *Balancing) *picker.PrefixIndex {
	if v, ok := d.prefixes.Load(backend); ok {
		return v.(*picker.PrefixIndex)
	}
	n := bal.PrefixMaxBlocks
	if n <= 0 {
		n = defaultPrefixMaxBlocks
	}
	v, _ := d.prefixes.LoadOrStore(backend, picker.NewPrefixIndex(n))
	return v.(*picker.PrefixIndex)
}

// prefix is one request's view of a backend's prefix index.
type prefix struct {
	index *picker.PrefixIndex
	chain []uint64
}

// matchPrefix sets each candidate's Cached from the backend's prefix index and
// returns the request's block chain with the index, or a zero prefix when the
// backend has no prefix affinity. A pod that left the endpoint set is
// forgotten first, so a new pod at its address claims nothing.
func (d *Dispatcher) matchPrefix(backend *Backend, eps []string, cands []picker.Candidate, body []byte) prefix {
	bal := backend.Balancing
	if bal.PrefixBlockBytes <= 0 {
		return prefix{}
	}
	idx := d.prefixFor(backend.Name, bal)
	idx.Retain(eps)
	chain := picker.BlockHashes(promptStream(body), bal.PrefixBlockBytes)
	limit := picker.EstimateTokens(int64(len(body)))
	for i := range cands {
		held := int64(idx.Matched(cands[i].Endpoint, chain)) * int64(bal.PrefixBlockBytes) / picker.BytesPerToken
		cands[i].Cached = min(held, limit)
	}
	return prefix{index: idx, chain: chain}
}

// homeTieStart returns the position, in the candidates PickLeastLoaded will
// scan, of the ring home of the prompt's first block, so equally loaded pods
// resolve to the same pod on every proxy replica. ok is false when the prompt
// has no full block, some pod already matched it, or the home is not eligible.
func homeTieStart(ring *picker.Ring, p prefix, cands []picker.Candidate, now time.Time) (int, bool) {
	if len(p.chain) == 0 {
		return 0, false
	}
	for _, c := range cands {
		if c.Cached > 0 {
			return 0, false
		}
	}
	home := ring.Home(strconv.FormatUint(p.chain[0], 16))
	for i, c := range picker.Eligible(cands, now) {
		if c.Endpoint == home {
			return i, true
		}
	}
	return 0, false
}

// place chooses the endpoint for a request. Backends that are not LoadAware
// round-robin exactly as before, ignore session headers and report an empty
// result. The int64 is the request's estimated uncached prompt tokens, 0 when
// the backend is not LoadAware.
func (d *Dispatcher) place(backend *Backend, headers http.Header, body []byte) (string, picker.Result, picker.Reason, int64) {
	bodyBytes := int64(len(body))
	eps := d.liveEndpoints(backend)
	bal := backend.Balancing
	if len(eps) == 0 || bal == nil || bal.Mode != BalancingLoadAware {
		return d.roundRobin(backend, eps), "", picker.ReasonNone, 0
	}
	cands := make([]picker.Candidate, 0, len(eps))
	var quarantined []string
	for _, ep := range eps {
		key := healthKey(backend.Name, ep)
		if !d.dispatchable(key) {
			// A departing endpoint can still be in eps after its health entry
			// was deleted. Missing health is not evidence of quarantine.
			if v, ok := d.health.Load(key); ok && d.quarantined(v.(*backendHealth)) {
				quarantined = append(quarantined, ep)
			}
			continue
		}
		l := d.loadFor(backend.Name, ep)
		cands = append(cands, picker.Candidate{Endpoint: ep, InFlight: l.inflight.Load(), Sample: l.sample.Load(),
			Prefill: l.prefill.Load(), Unreachable: d.pollFailing(key)})
	}
	if len(cands) == 0 {
		return d.roundRobin(backend, eps), picker.Fallback, picker.ReasonNone, 0
	}
	now := d.nowFn()
	// The tie start shares the round-robin cursor so equal loads rotate; the
	// pickers accept any int, so a wrapped conversion is fine.
	tie := int(d.endpointCursor(backend.Name).Add(1) - 1) //nolint:gosec // wraps harmlessly, the pickers take any int
	p := d.matchPrefix(backend, eps, cands, body)
	if start, ok := homeTieStart(d.ringFor(backend.Name, eps), p, cands, now); ok {
		tie = start
	}
	var (
		ep     string
		res    picker.Result
		reason picker.Reason
		tokens int64
	)
	if key := sessionKey(headers, bal.SessionHeaders); key != "" {
		ep, res, reason, tokens = d.sessionsFor(backend.Name, bal).Place(
			picker.SessionHash(key), cands, now, tie, d.ringFor(backend.Name, eps).Home(key), bodyBytes, quarantined)
	} else {
		ep, res = picker.PickLeastLoaded(picker.Eligible(cands, now), now, tie)
		reason, tokens = picker.ReasonNone, picker.EstimateTokens(bodyBytes)
	}
	if p.index != nil && ep != "" {
		// The index measures what the chosen pod holds, which beats a
		// session's growth estimate when the prompt changed at the front.
		var held int64
		for _, c := range cands {
			if c.Endpoint == ep {
				held = c.Cached
				break
			}
		}
		tokens = picker.EstimateTokens(bodyBytes) - held
		p.index.Record(ep, p.chain)
		prommetrics.RouterPrefixPromptTokensTotal.WithLabelValues(d.routerName, backend.Name).Add(float64(picker.EstimateTokens(bodyBytes)))
		prommetrics.RouterPrefixCachedTokensTotal.WithLabelValues(d.routerName, backend.Name).Add(float64(held))
	}
	return ep, res, reason, tokens
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
