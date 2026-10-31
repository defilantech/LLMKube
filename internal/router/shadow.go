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
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

// ShadowHeader marks a request the proxy mirrored to a shadow backend, so the
// backend (and anything that logs its traffic) can tell a copy from a request
// a client is waiting on.
const ShadowHeader = "x-llmkube-shadow"

// maxShadowInFlight caps the mirrored copies in flight to one backend. A shadow
// target is usually a candidate being evaluated, often slower than the
// backend serving clients, so an uncapped mirror would queue without bound on
// it and pile up goroutines in the proxy. A copy that finds the cap full is
// dropped and counted; mirroring is best effort by design.
const maxShadowInFlight = 4

// Shadow outcomes, the "outcome" label of llmkube_router_shadow_requests_total.
const (
	shadowOutcomeOK      = "ok"
	shadowOutcomeError   = "error"
	shadowOutcomeDropped = "dropped"
)

// shadower runs mirrored copies off the request path. Each copy holds one of
// its backend's maxShadowInFlight slots for as long as it runs. Close cancels
// every copy still running and waits for them, so none outlives the proxy.
type shadower struct {
	ctx    context.Context
	cancel context.CancelFunc
	limit  int32

	// mu orders start against close, so wg.Add never races wg.Wait.
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup

	inflight sync.Map // backend name -> *atomic.Int32
}

func newShadower(limit int32) *shadower {
	ctx, cancel := context.WithCancel(context.Background())
	return &shadower{ctx: ctx, cancel: cancel, limit: limit}
}

func (s *shadower) slots(backend string) *atomic.Int32 {
	if v, ok := s.inflight.Load(backend); ok {
		return v.(*atomic.Int32)
	}
	v, _ := s.inflight.LoadOrStore(backend, &atomic.Int32{})
	return v.(*atomic.Int32)
}

// start runs fn in its own goroutine under the shadower's context, holding a
// slot of backend's in-flight cap. It returns false, without running fn, when
// the cap is full or the shadower is closed.
func (s *shadower) start(backend string, fn func(ctx context.Context)) bool {
	slot := s.slots(backend)
	if slot.Add(1) > s.limit {
		slot.Add(-1)
		return false
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		slot.Add(-1)
		return false
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer slot.Add(-1)
		fn(s.ctx)
	}()
	return true
}

// close cancels every running copy, refuses new ones, and waits for the
// running ones to return.
func (s *shadower) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
}

// Close stops the proxy's background work tied to requests: it cancels the
// shadow copies still in flight and waits for them to return. Call it after
// the HTTP server has shut down, so no handler starts a new copy; a copy a
// handler tries to start afterwards is dropped.
func (p *Proxy) Close() {
	p.shadows.close()
}

// shadowTargets returns the backends a shadow rule mirrors this request to:
// the rule's servable backends (capableBackends) that come after the one that
// served the client in dispatch order, are healthy, and are not ModelPool
// members. A backend before the served one was tried and failed (so it already
// had the request) or was skipped as unhealthy, and either way gets no copy.
// A pooled backend is never mirrored to, since a copy must not trigger a swap;
// Config.Validate rejects a pooled shadow target, and this check holds for a
// pooled backend that serves as a fallback primary.
func (p *Proxy) shadowTargets(dec *MatchResult, served string) []*Backend {
	order := p.dispatchOrder(dec)
	at := -1
	for i, name := range order {
		if name == served {
			at = i
			break
		}
	}
	if at < 0 {
		return nil
	}
	seen := map[string]bool{served: true}
	var out []*Backend
	for _, name := range order[at+1:] {
		if seen[name] {
			continue
		}
		seen[name] = true
		b := p.matcher.BackendByName(name)
		if b == nil || b.Pool != nil || !p.disp.IsHealthy(name) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// mirror sends a copy of a request a shadow rule served to each of the rule's
// other eligible backends, without waiting for them. headers and body are the
// request as dispatched to the serving backend; the copies use a clone of the
// headers, so the client request may be released while they run.
func (p *Proxy) mirror(f *RequestFeatures, dec *MatchResult, served *Backend, headers http.Header, body []byte, path string) {
	targets := p.shadowTargets(dec, served.Name)
	if len(targets) == 0 {
		return
	}
	hdr := headers.Clone()
	for _, b := range targets {
		if !p.shadows.start(b.Name, func(ctx context.Context) {
			p.runShadow(ctx, *f, dec, b, hdr, body, path)
		}) {
			p.observeShadow(*f, dec, b, shadowOutcomeDropped, 0, 0, nil)
		}
	}
}

// runShadow sends one mirrored copy and reads its response to the end,
// discarding it. Nothing here touches the backend's health, a budget, or a
// ModelPool: the copy is an observation, and the backend's quarantine state
// belongs to the rules that serve clients from it.
func (p *Proxy) runShadow(ctx context.Context, f RequestFeatures, dec *MatchResult, b *Backend, headers http.Header, body []byte, path string) {
	start := time.Now()
	timeout := resolveDispatchTimeout(dec, b, p.disp.ResponseHeaderTimeout())
	resp, err := p.disp.Mirror(ctx, b, http.MethodPost, path, headers, body, timeout)
	status := 0
	if err == nil {
		status = resp.StatusCode
		_, err = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if err == nil && status >= 400 {
			err = fmt.Errorf("%s returned %d", b.Name, status)
		}
	}
	outcome := shadowOutcomeOK
	if err != nil {
		outcome = shadowOutcomeError
	}
	p.observeShadow(f, dec, b, outcome, status, time.Since(start), err)
}

// observeShadow records one mirrored copy: the counter, the latency histogram
// for a copy that was sent, and a router.shadow log line shaped like
// router.dispatch.
func (p *Proxy) observeShadow(f RequestFeatures, dec *MatchResult, b *Backend, outcome string, status int, elapsed time.Duration, err error) {
	rule := ""
	if dec.Rule != nil {
		rule = dec.Rule.Name
	}
	prommetrics.RouterShadowRequestsTotal.WithLabelValues(p.routerName, rule, b.Name, outcome).Inc()
	if outcome != shadowOutcomeDropped {
		prommetrics.RouterShadowRequestDuration.WithLabelValues(p.routerName, rule, b.Name).Observe(elapsed.Seconds())
	}
	attrs := []any{
		"model", f.Model,
		"rule", rule,
		"backend", b.Name,
		"backendTier", b.Tier,
		"status", status,
		"outcome", outcome,
		"latencyMs", elapsed.Milliseconds(),
	}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	p.logger.Info("router.shadow", attrs...)
}

// Mirror sends a shadow copy of a request to backend and returns the response
// for the caller to drain and close. It shapes the request exactly as Dispatch
// does (endpoint choice, model rewrite, forwarded headers without hop-by-hop
// ones, the backend's credentials) and adds ShadowHeader, but it leaves every
// piece of shared state alone: no health or quarantine change, no placement,
// load or session accounting, no stale-connection retry. headerTimeout bounds
// the wait for response headers; the stream idle guard bounds a body that goes
// silent, without quarantining anything.
func (d *Dispatcher) Mirror(
	ctx context.Context,
	backend *Backend,
	method, path string,
	headers http.Header,
	requestBody []byte,
	headerTimeout time.Duration,
) (*http.Response, error) {
	endpoint := d.roundRobin(backend, d.liveEndpoints(backend))
	url := joinURL(dispatchTarget(backend, endpoint), path)
	outboundBody := applyModelOverride(requestBody, backend.outboundModel())
	reqCancel, req, _, err := d.newUpstreamRequest(ctx, backend, method, url, headers, outboundBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set(ShadowHeader, "true")

	var timedOut atomic.Bool
	timer := time.AfterFunc(headerTimeout, func() {
		timedOut.Store(true)
		reqCancel()
	})
	resp, err := d.client.Do(req) //nolint:gosec // G704: the configured backend address or a controller-managed pod, as in Dispatch
	if !timer.Stop() && timedOut.Load() {
		if err == nil {
			_ = resp.Body.Close()
		}
		reqCancel()
		return nil, fmt.Errorf("%w: waiting for response headers from %s", context.DeadlineExceeded, backend.Name)
	}
	if err != nil {
		reqCancel()
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	if d.streamIdleTimeout > 0 {
		resp.Body = newIdleGuardBody(resp.Body, d.streamIdleTimeout, reqCancel, reqCancel)
	} else {
		resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: reqCancel}
	}
	return resp, nil
}
