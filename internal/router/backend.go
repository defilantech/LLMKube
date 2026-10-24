/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

// defaultQuarantineDuration is how long a backend stays in the
// quarantined (skip) state before the dispatcher's IsHealthy check
// returns true again as a half-open probe. Picked to be long enough
// that genuinely-down backends don't get hammered every request but
// short enough that transient blips (one Metal context switch, a
// kubelet-eviction-then-respawn cycle) self-heal quickly without ops
// intervention.
const defaultQuarantineDuration = 15 * time.Second

// defaultIdleConnTimeout caps how long the shared transport keeps
// idle keep-alive connections to upstreams in its pool. It must be
// shorter than the upstreams' own idle reapers, so the proxy always
// closes an idle connection first: vLLM closes idle keep-alive
// connections after 5s (VLLM_HTTP_TIMEOUT_KEEP_ALIVE), and reusing one
// it is closing fails the request with an EOF (#2195). 4s still
// amortizes the TCP handshake across burst traffic in an agentic
// workload. Cloud-tier backends bypass the pool entirely by setting
// `Connection: close` per request in Dispatch; see the cloud-tier
// branch there for why.
const defaultIdleConnTimeout = 4 * time.Second

// endpointFailuresToQuarantine is how many failures in a row quarantine a
// pod that is still in the endpoint set. One failure alone is too often a
// connection blip, and quarantining a pod rebinds every session on it.
const endpointFailuresToQuarantine = 2

// defaultResponseHeaderTimeout caps how long the dispatcher waits for
// the upstream to begin sending response headers. For non-streaming
// chat completions, llama.cpp / vLLM / cloud providers emit headers
// only after the full token stream completes, so this is effectively
// a max-generation-time cap. 120s covers (a) 70B-class local models
// doing ~1500-token completions on consumer Metal, (b) Anthropic on
// long reasoning, (c) Bedrock cold-start. Streaming dispatches send
// first SSE chunk within ~1s, so the cap is invisible to them.
// Tunable via --response-header-timeout (see WithResponseHeaderTimeout).
const defaultResponseHeaderTimeout = 120 * time.Second

// defaultStreamIdleTimeout caps how long a response body may go without a
// single byte from the upstream once headers have arrived. A healthy stream
// emits bytes continuously; the gap that matters is the prefill before the
// first content token, which can take minutes for a very long prompt on slow
// hardware, so the default is generous. A body that stays silent this long
// is treated as a wedged backend: the request is aborted and the endpoint is
// quarantined. Tunable via --stream-idle-timeout (see WithStreamIdleTimeout).
const defaultStreamIdleTimeout = 300 * time.Second

// backendHealth is the dispatcher's per-backend state. It implements
// the half-open part of a circuit breaker: when MarkUnhealthy fires,
// the backend is skipped until quarantineUntil; the first request
// after the window expires is allowed through as a probe, and the
// probe's outcome either re-marks healthy (on 2xx) or extends the
// quarantine (on 5xx / network error).
//
// Stored in Dispatcher.health by backend name. Concurrent reads from
// the dispatch loop happen on every request, so the fields are
// atomic and the struct never moves once stored.
type backendHealth struct {
	healthy         atomic.Bool
	quarantineUntil atomic.Int64 // unix nano; 0 when healthy
	// failures counts failures since the last success, for endpoint
	// entries; see endpointFailuresToQuarantine.
	failures atomic.Int32
}

// Dispatcher knows how to forward an inbound request to a chosen backend.
// One Dispatcher instance is shared across all requests; it owns the
// per-backend http.Client pool and health bookkeeping.
type Dispatcher struct {
	cfg    *Config
	client *http.Client
	// freshClient shares client's settings with keep-alives off, so each
	// request it sends dials a new connection. Only the stale-connection
	// retry uses it: setting Request.Close does not stop the transport
	// handing out another pooled connection.
	freshClient *http.Client
	health      sync.Map // healthKey -> *backendHealth

	// endpoints holds the per-pod addresses for backends that balance across
	// them, keyed by backend name, so IsHealthy can decide from the endpoint
	// set without the caller passing the Backend.
	endpoints sync.Map // backendName -> []string
	loads     sync.Map // healthKey -> *endpointLoad

	// prefillMu guards prefillLedger, the prefill counts not yet released,
	// so the janitor can release one whose body never produced a byte.
	prefillMu     sync.Mutex
	prefillLedger map[uint64]prefillEntry
	prefillSeq    uint64

	// rings caches the consistent-hash ring per balanced backend, rebuilt when
	// the endpoint set changes. It holds endpoint addresses only, never a
	// session value.
	rings sync.Map // backendName -> *ringCache

	// sessions holds the session-to-pod binding table per LoadAware backend,
	// created on first use. The proxy's config is fixed for its life, so a
	// table is never dropped at runtime. Keys are hashes of session values.
	sessions sync.Map // backendName -> *picker.Sessions

	// cursors is the round-robin position per endpoint backend. Consecutive
	// requests advance it so a keep-alive client spreads across pods.
	cursors sync.Map // backendName -> *atomic.Uint64

	// quarantineDuration controls how long a backend stays in the
	// "skip" state after MarkUnhealthy. Defaults to
	// defaultQuarantineDuration; tests can shrink it via the
	// QuarantineDuration option.
	quarantineDuration time.Duration

	// responseHeaderTimeout is the dispatcher's view of the proxy
	// default. The actual cap on a given request is the *minimum*
	// of this and any per-rule / per-backend timeout resolved at
	// dispatch time. Defaults to defaultResponseHeaderTimeout.
	responseHeaderTimeout time.Duration

	// streamIdleTimeout is the longest a response body may stay silent
	// after headers before the dispatcher aborts it and quarantines the
	// endpoint. 0 disables the guard. Defaults to defaultStreamIdleTimeout.
	streamIdleTimeout time.Duration

	// routerName labels the per-endpoint load gauges. Empty unless set with
	// WithRouterLabel.
	routerName string

	// nowFn is overridable in tests.
	nowFn func() time.Time

	// logger records quarantines. NewProxy sets it to the proxy's logger.
	logger *slog.Logger
}

// DispatcherOption customizes a Dispatcher at construction time.
type DispatcherOption func(*Dispatcher)

// WithQuarantineDuration overrides defaultQuarantineDuration. Useful
// in tests that want sub-second windows so they don't have to sleep
// for fifteen seconds to verify recovery.
func WithQuarantineDuration(d time.Duration) DispatcherOption {
	return func(disp *Dispatcher) { disp.quarantineDuration = d }
}

// WithRouterLabel sets the router label value on the dispatcher's per-endpoint
// metrics.
func WithRouterLabel(name string) DispatcherOption {
	return func(disp *Dispatcher) { disp.routerName = name }
}

// WithNowFunc overrides the dispatcher's time source. Tests use it to
// step time forward without a real clock.
func WithNowFunc(fn func() time.Time) DispatcherOption {
	return func(disp *Dispatcher) { disp.nowFn = fn }
}

// WithResponseHeaderTimeout sets the upper bound on how long the
// dispatcher waits for the upstream to begin sending response headers.
// Wired from cmd/router-proxy's --response-header-timeout flag, which
// in turn is set by the controller from
// ModelRouter.spec.proxy.responseHeaderTimeout. Per-rule / per-backend
// timeouts (see resolveDispatchTimeout in proxy.go) tighten this on a
// per-request basis but cannot extend it beyond this cap.
func WithResponseHeaderTimeout(d time.Duration) DispatcherOption {
	return func(disp *Dispatcher) { disp.responseHeaderTimeout = d }
}

// WithStreamIdleTimeout sets how long a response body may go without a byte
// from the upstream, after headers, before the dispatcher aborts the request
// and quarantines that endpoint. 0 disables the guard. Wired from
// cmd/router-proxy's --stream-idle-timeout flag, which the operator sets from
// ModelRouter.spec.proxy.streamIdleTimeout.
func WithStreamIdleTimeout(d time.Duration) DispatcherOption {
	return func(disp *Dispatcher) { disp.streamIdleTimeout = d }
}

// ResponseHeaderTimeout returns the dispatcher's proxy-default
// timeout. Exposed for the proxy handler to use when resolving the
// effective per-request timeout (rule.timeout || backend.timeout ||
// dispatcher.ResponseHeaderTimeout()).
func (d *Dispatcher) ResponseHeaderTimeout() time.Duration {
	return d.responseHeaderTimeout
}

// NewDispatcher returns a Dispatcher bound to the given Config. All
// backends start in a healthy state. The proxy quarantines a backend
// on 5xx / network error for quarantineDuration, after which the
// next request is allowed through as a half-open probe.
func NewDispatcher(cfg *Config, opts ...DispatcherOption) *Dispatcher {
	d := &Dispatcher{
		cfg:                   cfg,
		quarantineDuration:    defaultQuarantineDuration,
		responseHeaderTimeout: defaultResponseHeaderTimeout,
		streamIdleTimeout:     defaultStreamIdleTimeout,
		nowFn:                 time.Now,
		logger:                slog.Default(),
	}
	for _, opt := range opts {
		opt(d)
	}
	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       defaultIdleConnTimeout,
		ResponseHeaderTimeout: d.responseHeaderTimeout,
	}
	d.client = &http.Client{
		// Streaming requests can be long-lived. The per-request
		// context is the real deadline; this caps idle and connect
		// phases to prevent leaking goroutines on dead backends.
		Timeout:   0,
		Transport: transport,
	}
	fresh := transport.Clone()
	fresh.DisableKeepAlives = true
	d.freshClient = &http.Client{Timeout: 0, Transport: fresh}
	for _, b := range cfg.Backends {
		h := &backendHealth{}
		h.healthy.Store(true)
		d.health.Store(b.Name, h)
		if len(b.Endpoints) > 0 && b.EndpointSource == nil {
			d.endpoints.Store(b.Name, b.Endpoints)
			for _, ep := range b.Endpoints {
				eh := &backendHealth{}
				eh.healthy.Store(true)
				d.health.Store(healthKey(b.Name, ep), eh)
			}
		}
	}
	return d
}

// healthKey is the dispatcher's health-map key for a backend. An endpoint
// backend keys per endpoint so one bad pod is quarantined on its own; a backend
// with no endpoints keys on its name, preserving the per-backend quarantine.
func healthKey(backendName, endpoint string) string {
	if endpoint == "" {
		return backendName
	}
	return backendName + "|" + endpoint
}

// healthFor returns the health state for key, creating a healthy entry on first
// use so an endpoint is quarantinable before it has ever been dispatched to.
func (d *Dispatcher) healthFor(key string) *backendHealth {
	if v, ok := d.health.Load(key); ok {
		return v.(*backendHealth)
	}
	h := &backendHealth{}
	h.healthy.Store(true)
	actual, _ := d.health.LoadOrStore(key, h)
	return actual.(*backendHealth)
}

// endpointCursor returns the round-robin position for an endpoint backend.
func (d *Dispatcher) endpointCursor(name string) *atomic.Uint64 {
	if v, ok := d.cursors.Load(name); ok {
		return v.(*atomic.Uint64)
	}
	c := &atomic.Uint64{}
	actual, _ := d.cursors.LoadOrStore(name, c)
	return actual.(*atomic.Uint64)
}

// dispatchable reports whether the single health entry key may be tried now:
// healthy outright, or quarantined with the window expired (half-open probe).
// An unknown key is not dispatchable, so an unknown backend name stays
// unhealthy.
func (d *Dispatcher) dispatchable(key string) bool {
	v, ok := d.health.Load(key)
	if !ok {
		return false
	}
	h := v.(*backendHealth)
	if h.healthy.Load() {
		return true
	}
	until := h.quarantineUntil.Load()
	return until > 0 && d.nowFn().UnixNano() >= until
}

// roundRobin chooses the pod to dispatch to among eps, the backend's live
// endpoint set. Consecutive calls rotate so a keep-alive client spreads
// across pods rather than pinning to one through conntrack. Quarantined
// endpoints are skipped; when every endpoint is quarantined the rotation
// returns one anyway so the half-open probe still happens.
func (d *Dispatcher) roundRobin(backend *Backend, eps []string) string {
	n := len(eps)
	if n == 0 {
		return ""
	}
	if n == 1 {
		return eps[0]
	}
	start := int(d.endpointCursor(backend.Name).Add(1)-1) % n //nolint:gosec // modulo bounds the value below n, an int
	for i := 0; i < n; i++ {
		ep := eps[(start+i)%n]
		if d.dispatchable(healthKey(backend.Name, ep)) {
			return ep
		}
	}
	return eps[start]
}

// SetEndpoints replaces the live endpoint set of a backend whose pods the
// proxy tracks (an EndpointSource backend). The set is stored sorted so every
// replica sees the same order. New endpoints start healthy; health entries of
// endpoints that left are dropped, so a pod that later reuses an IP does not
// inherit an old quarantine. An empty set makes dispatch fall back to the
// backend's Address.
func (d *Dispatcher) SetEndpoints(name string, endpoints []string) {
	next := append([]string(nil), endpoints...)
	sort.Strings(next)
	var prev []string
	if v, ok := d.endpoints.Load(name); ok {
		prev = v.([]string)
	}
	keep := make(map[string]bool, len(next))
	for _, ep := range next {
		keep[ep] = true
		d.healthFor(healthKey(name, ep))
	}
	for _, ep := range prev {
		if !keep[ep] {
			d.health.Delete(healthKey(name, ep))
			d.loads.Delete(healthKey(name, ep))
			// Backends without a metrics profile have no scraper to drop
			// this per-pod series, so departure drops it here.
			prommetrics.RouterPrefillTokensEstimatedTotal.DeleteLabelValues(d.routerName, name, ep)
		}
	}
	d.endpoints.Store(name, next)
}

// liveEndpoints returns the endpoint set dispatch uses for backend: the set
// SetEndpoints last stored (seeded from Backend.Endpoints at construction),
// else the configured list. An EndpointSource backend never uses the
// configured list: until its first sync it dispatches to Address.
func (d *Dispatcher) liveEndpoints(backend *Backend) []string {
	if v, ok := d.endpoints.Load(backend.Name); ok {
		return v.([]string)
	}
	if backend.EndpointSource != nil {
		return nil
	}
	return backend.Endpoints
}

// dispatchTarget resolves the base URL a dispatch to backend should use: the
// chosen pod endpoint when the backend balances, else the Service DNS Address.
func dispatchTarget(backend *Backend, endpoint string) string {
	if endpoint != "" {
		return endpoint
	}
	return backend.Address
}

// IsHealthy reports whether the dispatcher will *consider* the backend
// for the next dispatch. The check returns true in two cases:
//
//  1. The backend is currently in the healthy state.
//  2. The backend is quarantined but the quarantine window has
//     expired (half-open): the next dispatch probes the backend and
//     either re-marks healthy (on success) or extends quarantine
//     (on failure).
//
// An endpoint backend is considered healthy while any one of its pods is
// dispatchable, so a single quarantined pod does not skip the backend.
//
// Unknown backend names report unhealthy.
func (d *Dispatcher) IsHealthy(name string) bool {
	if eps, ok := d.endpoints.Load(name); ok {
		list := eps.([]string)
		if len(list) > 0 {
			for _, ep := range list {
				if d.dispatchable(healthKey(name, ep)) {
					return true
				}
			}
			return false
		}
	}
	return d.dispatchable(name)
}

// MarkHealthy flips the backend to healthy and clears any pending
// quarantine. Called from Dispatch on a 2xx response.
func (d *Dispatcher) MarkHealthy(name string) {
	if v, ok := d.health.Load(name); ok {
		h := v.(*backendHealth)
		h.healthy.Store(true)
		h.quarantineUntil.Store(0)
	}
}

// MarkUnhealthy quarantines the backend for quarantineDuration. The
// caller (Dispatch) calls this on 5xx / network errors. Subsequent
// IsHealthy checks return false until the window expires, at which
// point the backend becomes eligible for a half-open probe.
func (d *Dispatcher) MarkUnhealthy(name string) {
	if v, ok := d.health.Load(name); ok {
		h := v.(*backendHealth)
		h.healthy.Store(false)
		h.quarantineUntil.Store(d.nowFn().Add(d.quarantineDuration).UnixNano())
	}
}

// markUnhealthy quarantines one endpoint of a backend, or the whole backend
// when endpoint is empty. Dispatch calls this because it knows the pod it
// actually talked to, so one bad pod does not skip the backend's siblings.
func (d *Dispatcher) markUnhealthy(name, endpoint string) {
	h := d.healthFor(healthKey(name, endpoint))
	if endpoint != "" && h.failures.Add(1) < endpointFailuresToQuarantine {
		return
	}
	d.quarantine(name, endpoint, h)
}

// quarantine skips an endpoint (or the whole backend when endpoint is empty)
// for quarantineDuration, without waiting for repeated failures. The stream
// idle guard uses it directly: a pod that accepted a request and then went
// silent for the whole idle timeout is wedged, not blipping.
func (d *Dispatcher) quarantine(name, endpoint string, h *backendHealth) {
	h.healthy.Store(false)
	h.quarantineUntil.Store(d.nowFn().Add(d.quarantineDuration).UnixNano())
	d.logger.Warn("router quarantined upstream", "backend", name, "endpoint", endpoint,
		"for", d.quarantineDuration.String())
}

// markHealthy clears quarantine on one endpoint (or the whole backend when
// endpoint is empty).
func (d *Dispatcher) markHealthy(name, endpoint string) {
	h := d.healthFor(healthKey(name, endpoint))
	h.healthy.Store(true)
	h.quarantineUntil.Store(0)
	h.failures.Store(0)
}

// Dispatch forwards the request to the named backend and returns the
// upstream response. Caller is responsible for streaming the body to
// the inbound client and closing it. On error the response is nil.
//
// requestBody is the shared inbound body, reused across fallback attempts.
// Dispatch rewrites the "model" field of an outbound copy (see
// applyModelOverride) without mutating it.
func (d *Dispatcher) Dispatch(
	ctx context.Context,
	backend *Backend,
	method, path string,
	headers http.Header,
	requestBody []byte,
) (*http.Response, error) {
	if backend == nil {
		return nil, fmt.Errorf("dispatch: backend is nil")
	}
	// count_tokens tokenizes the conversation without a prefill: it is placed
	// without the session header, adds no prefill tokens, leaves the
	// session's binding and size record to the real turns, and is not counted
	// as a placement.
	tokenCount := isTokenCountPath(path)
	placeHeaders, placeBytes := headers, int64(len(requestBody))
	if tokenCount {
		placeHeaders, placeBytes = nil, 0
	}
	endpoint, placed, reason, tokens := d.place(backend, placeHeaders, placeBytes)
	if !tokenCount {
		d.countPlacement(backend.Name, placed, reason)
	}
	release := d.track(backend.Name, endpoint)
	releasePrefill := d.trackPrefill(backend.Name, endpoint, tokens)
	url := joinURL(dispatchTarget(backend, endpoint), path)

	// Send the model name this backend actually serves, not the
	// client-facing alias the router matched on.
	outboundBody := applyModelOverride(requestBody, backend.outboundModel())

	// The request runs under its own cancellable context so the stream idle
	// guard can abort a body that went silent without touching the caller's
	// context.
	reqCancel, req, reused, err := d.newUpstreamRequest(ctx, backend, method, url, headers, outboundBody)
	if err != nil {
		release()
		releasePrefill()
		return nil, err
	}

	resp, err := d.client.Do(req) //nolint:gosec // G704: same destination as the request above, never derived from request headers
	if err != nil && reused.Load() && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		// A reused keep-alive connection failed before any response byte:
		// the upstream closed it while it sat idle. That is the
		// connection's fault, not the pod's, so retry once on a fresh
		// connection to the same pod (the session's cache stays warm)
		// and do not count it against the pod (#2195).
		prommetrics.RouterDispatchRetriesTotal.WithLabelValues(d.routerName, backend.Name, "stale_connection").Inc()
		reqCancel()
		var rerr error
		reqCancel, req, _, rerr = d.newUpstreamRequest(ctx, backend, method, url, headers, outboundBody)
		if rerr != nil {
			release()
			releasePrefill()
			return nil, rerr
		}
		// A client with keep-alives off: the pool may hold more connections
		// the upstream has already closed.
		resp, err = d.freshClient.Do(req) //nolint:gosec // G704: same destination as above
	}
	if err != nil {
		// A per-attempt context deadline (caller's
		// context.WithTimeout, eg the rule.timeout path from #461)
		// is a rule-level policy decision, not a backend-health
		// signal. The upstream may still be perfectly healthy and
		// capable of serving other rules with longer budgets. Same
		// for context.Canceled, which fires when an inbound client
		// disconnects mid-dispatch. Reserve quarantine for genuine
		// connectivity / 5xx failures so a tight rule's timeout
		// doesn't poison a lenient sibling rule targeting the same
		// backend. Closes #462.
		if !errors.Is(err, context.DeadlineExceeded) &&
			!errors.Is(err, context.Canceled) {
			d.markUnhealthy(backend.Name, endpoint)
		}
		reqCancel()
		release()
		releasePrefill()
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	if resp.StatusCode >= 500 {
		// Don't flip on 4xx: those are client errors, not backend
		// problems. 5xx however means the backend itself misbehaved.
		d.markUnhealthy(backend.Name, endpoint)
	} else {
		d.markHealthy(backend.Name, endpoint)
	}
	if d.streamIdleTimeout > 0 {
		name := backend.Name
		resp.Body = newIdleGuardBody(resp.Body, d.streamIdleTimeout, func() {
			// The upstream accepted the request and sent headers, then
			// stopped producing bytes: a wedged backend, not a client
			// disconnect or a rule deadline, so quarantine the endpoint.
			d.quarantine(name, endpoint, d.healthFor(healthKey(name, endpoint)))
			reqCancel()
		}, reqCancel)
	} else {
		resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: reqCancel}
	}
	resp.Body = &firstByteBody{ReadCloser: resp.Body, release: releasePrefill}
	resp.Body = &releaseOnCloseBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

// ErrStreamIdle reports a response body aborted by the stream idle guard.
var ErrStreamIdle = errors.New("upstream stream idle timeout")

// idleGuardBody aborts a response body that produces no bytes for timeout.
// The timer runs only while a Read is blocked on the upstream. Between Reads
// the proxy is writing to the client, and a slow client applying backpressure
// is not a wedged backend, so that time does not count. Close disarms it.
type idleGuardBody struct {
	rc      io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
	fired   atomic.Bool
	closed  atomic.Bool
	onIdle  func()
	onClose func()
}

func newIdleGuardBody(rc io.ReadCloser, timeout time.Duration, onIdle, onClose func()) *idleGuardBody {
	g := &idleGuardBody{rc: rc, timeout: timeout, onIdle: onIdle, onClose: onClose}
	// Disarmed until the first Read: the proxy writes the response headers to
	// the client before it starts reading the body.
	g.timer = time.AfterFunc(timeout, g.fire)
	g.timer.Stop()
	return g
}

func (g *idleGuardBody) fire() {
	if g.closed.Load() {
		return
	}
	if g.fired.CompareAndSwap(false, true) {
		g.onIdle()
	}
}

func (g *idleGuardBody) Read(p []byte) (int, error) {
	if !g.fired.Load() {
		g.timer.Reset(g.timeout)
	}
	n, err := g.rc.Read(p)
	g.timer.Stop()
	if err != nil && err != io.EOF && g.fired.Load() {
		return n, fmt.Errorf("%w after %s: %v", ErrStreamIdle, g.timeout, err)
	}
	return n, err
}

func (g *idleGuardBody) Close() error {
	g.closed.Store(true)
	g.timer.Stop()
	err := g.rc.Close()
	if g.onClose != nil {
		g.onClose()
	}
	return err
}

// cancelOnCloseBody releases the request context once the caller closes the
// body, when the idle guard is disabled.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnCloseBody) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// copyForwardedHeaders copies inbound headers to the outbound request,
// dropping hop-by-hop headers per RFC 7230 section 6.1 plus a small
// set of headers we explicitly do not want to forward upstream
// (authorization is replaced by the backend's own credentials; content-
// length is recomputed by the http client).
func (d *Dispatcher) copyForwardedHeaders(in, out http.Header) {
	for k, vals := range in {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vals {
			out.Add(k, v)
		}
	}
}

// applyCredentials injects the backend's credentials into the outbound
// request. Local backends typically have no credentials; cloud backends
// reference an env var by name in CredentialsEnv. The provider value
// controls header shape (Authorization: Bearer for OpenAI / LiteLLM,
// x-api-key for Anthropic, etc.).
func (d *Dispatcher) applyCredentials(b *Backend, req *http.Request) error {
	if b.CredentialsEnv == "" {
		return nil
	}
	val := os.Getenv(b.CredentialsEnv)
	if val == "" {
		return fmt.Errorf("backend %s: credentials env %s is unset", b.Name, b.CredentialsEnv)
	}
	switch strings.ToLower(b.Provider) {
	case "anthropic":
		req.Header.Set("x-api-key", val)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "openai", "litellm", "":
		req.Header.Set("Authorization", "Bearer "+val)
	default:
		req.Header.Set("Authorization", "Bearer "+val)
	}
	return nil
}

// outboundModel returns the model identifier this backend expects to see in
// the request body. External backends declare it explicitly as Model. Local
// backends normally serve whatever their InferenceService is named, which is
// also the alias the controller registers upstream, so a request that reached
// this backend under a different alias (a rule fall-through, an IfIdle skip
// past a busy pool member) must be rewritten to it: llama.cpp ignores the
// field, but vLLM / SGLang / TGI answer an unknown name with 404 "The model X
// does not exist". ServedModel overrides the InferenceService name for a
// runtime that serves a different id, such as oMLX, whose engine serves the
// model-store directory basename. A backend with none of the three set (a
// hand-written config, a backend whose InferenceService did not resolve)
// keeps the historical pass-through.
func (b *Backend) outboundModel() string {
	if b.Model != "" {
		return b.Model
	}
	if b.ServedModel != "" {
		return b.ServedModel
	}
	return b.InferenceService
}

// applyModelOverride returns a copy of body with the OpenAI "model" field
// set to model, so the upstream receives an identifier it recognizes and a
// fallback chain degrades across models instead of re-sending the client
// alias. An empty model, a body whose model field already equals model, and
// non-JSON-object bodies are returned unchanged; the equality case keeps the
// common request byte-identical instead of re-marshalling it.
func applyModelOverride(body []byte, model string) []byte {
	if model == "" {
		return body
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	enc, err := json.Marshal(model)
	if err != nil {
		return body
	}
	if cur, ok := obj["model"]; ok && bytes.Equal(cur, enc) {
		return body
	}
	obj["model"] = enc
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return rewritten
}

func joinURL(base, p string) string {
	if base == "" {
		return p
	}
	if p == "" {
		return base
	}
	if strings.HasSuffix(base, "/") && strings.HasPrefix(p, "/") {
		return base + p[1:]
	}
	if !strings.HasSuffix(base, "/") && !strings.HasPrefix(p, "/") {
		return base + "/" + p
	}
	return base + p
}

// Hop-by-hop headers per RFC 7230 section 6.1. We also drop authorization
// because the backend supplies its own credentials.
var hopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"authorization":       true,
	"content-length":      true,
	"host":                true,
}

// PipeBody copies the upstream response body to the client writer,
// flushing after every read so SSE chunks are delivered immediately.
// http.ResponseWriter must implement http.Flusher for streaming to work;
// the standard library's net/http server does, but tests using
// httptest.ResponseRecorder do not, so callers handle that fallback.
func PipeBody(dst io.Writer, src io.Reader, flush func()) (int64, error) {
	buf := make([]byte, 8*1024) // 8 KiB matches net/http's internal default
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			written, werr := dst.Write(buf[:n])
			total += int64(written)
			if werr != nil {
				return total, werr
			}
			if flush != nil {
				flush()
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// sentOnceBody is a request body that releases its bytes once they have been
// read to the end or the body is closed, so a long streamed response does not
// keep the request it answered in memory.
type sentOnceBody struct {
	mu  sync.Mutex
	buf []byte
}

func newSentOnceBody(b []byte) io.ReadCloser { return &sentOnceBody{buf: b} }

func (s *sentOnceBody) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		s.buf = nil
		return 0, io.EOF
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	if len(s.buf) == 0 {
		s.buf = nil
	}
	return n, nil
}

func (s *sentOnceBody) Close() error {
	s.mu.Lock()
	s.buf = nil
	s.mu.Unlock()
	return nil
}

// newUpstreamRequest builds one outbound request under its own cancellable
// context, so the stream idle guard can abort a body that went silent without
// touching the caller's context. reused reports, once the request has been
// sent, whether it went out on a pooled keep-alive connection.
func (d *Dispatcher) newUpstreamRequest(
	ctx context.Context,
	backend *Backend,
	method, url string,
	headers http.Header,
	outboundBody []byte,
) (context.CancelFunc, *http.Request, *atomic.Bool, error) {
	reused := &atomic.Bool{}
	reqCtx, reqCancel := context.WithCancel(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) },
	}))
	req, err := http.NewRequestWithContext(reqCtx, method, url, newSentOnceBody(outboundBody)) //nolint:gosec // G704: url is the configured backend address or a pod from the controller-managed endpoint set; the session header is only hashed onto the ring
	if err != nil {
		reqCancel()
		return nil, nil, nil, fmt.Errorf("build request: %w", err)
	}
	// The transport keeps the request until the response body is done, so a
	// bytes.Reader body (and the GetBody closure NewRequest builds for one)
	// would hold the whole prompt for the length of the stream (#2194).
	// sentOnceBody drops it once written; the length is set explicitly so
	// the request is not sent chunked.
	req.ContentLength = int64(len(outboundBody))
	req.GetBody = nil

	d.copyForwardedHeaders(headers, req.Header)
	if err := d.applyCredentials(backend, req); err != nil {
		reqCancel()
		return nil, nil, nil, err
	}

	// Cloud-tier backends commonly sit behind load balancers (AWS NLB,
	// Cloudflare, Anthropic / OpenAI global LB) that aggressively
	// recycle idle TCP connections without telling the client. Setting
	// Connection: close on every outbound request opts that backend
	// out of the shared keep-alive pool entirely: Go's http.Transport
	// honors the header and closes the conn after this exchange
	// regardless of pool capacity. The per-request handshake cost is
	// negligible compared to typical cloud upstream latency, and we
	// trade it for never seeing a "stale conn -> 30s silent stall"
	// fingerprint. Local-tier backends keep the pool; their
	// IdleConnTimeout (defaultIdleConnTimeout) is shorter than their
	// upstreams' idle reapers, and a stale reused connection is retried
	// once on a fresh one.
	if strings.EqualFold(backend.Tier, "cloud") {
		req.Header.Set("Connection", "close")
		req.Close = true
	}

	return reqCancel, req, reused, nil
}
