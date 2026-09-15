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
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
// idle keep-alive connections to upstreams in its pool. The 10s value
// is short enough that local llama-server / vLLM connections expire
// before the upstream's own idle reaper closes them server-side
// (which is what causes the "silent 30s stall on a healthy backend"
// failure mode), but long enough to amortize the TCP handshake across
// burst traffic in a typical agentic workload. Cloud-tier backends
// bypass the pool entirely by setting `Connection: close` per request
// in Dispatch; see the cloud-tier branch there for why.
const defaultIdleConnTimeout = 10 * time.Second

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
}

// Dispatcher knows how to forward an inbound request to a chosen backend.
// One Dispatcher instance is shared across all requests; it owns the
// per-backend http.Client pool and health bookkeeping.
type Dispatcher struct {
	cfg    *Config
	client *http.Client
	health sync.Map // healthKey -> *backendHealth

	// endpoints holds the per-pod addresses for backends that balance across
	// them, keyed by backend name, so IsHealthy can decide from the endpoint
	// set without the caller passing the Backend.
	endpoints sync.Map // backendName -> []string

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

	// nowFn is overridable in tests.
	nowFn func() time.Time
}

// DispatcherOption customizes a Dispatcher at construction time.
type DispatcherOption func(*Dispatcher)

// WithQuarantineDuration overrides defaultQuarantineDuration. Useful
// in tests that want sub-second windows so they don't have to sleep
// for fifteen seconds to verify recovery.
func WithQuarantineDuration(d time.Duration) DispatcherOption {
	return func(disp *Dispatcher) { disp.quarantineDuration = d }
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
		nowFn:                 time.Now,
	}
	for _, opt := range opts {
		opt(d)
	}
	d.client = &http.Client{
		// Streaming requests can be long-lived. The per-request
		// context is the real deadline; this caps idle and connect
		// phases to prevent leaking goroutines on dead backends.
		Timeout: 0,
		Transport: &http.Transport{
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       defaultIdleConnTimeout,
			ResponseHeaderTimeout: d.responseHeaderTimeout,
		},
	}
	for _, b := range cfg.Backends {
		h := &backendHealth{}
		h.healthy.Store(true)
		d.health.Store(b.Name, h)
		if len(b.Endpoints) > 0 {
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

// pickEndpoint chooses the pod to dispatch to for a backend that balances
// across its endpoints. Consecutive calls rotate so a keep-alive client spreads
// across pods rather than pinning to one through conntrack. Quarantined
// endpoints are skipped; when every endpoint is quarantined the rotation
// returns one anyway so the half-open probe still happens.
func (d *Dispatcher) pickEndpoint(backend *Backend) string {
	n := len(backend.Endpoints)
	if n == 0 {
		return ""
	}
	if n == 1 {
		return backend.Endpoints[0]
	}
	start := int(d.endpointCursor(backend.Name).Add(1)-1) % n //nolint:gosec // modulo bounds the value below n, an int
	for i := 0; i < n; i++ {
		ep := backend.Endpoints[(start+i)%n]
		if d.dispatchable(healthKey(backend.Name, ep)) {
			return ep
		}
	}
	return backend.Endpoints[start]
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
	h.healthy.Store(false)
	h.quarantineUntil.Store(d.nowFn().Add(d.quarantineDuration).UnixNano())
}

// markHealthy clears quarantine on one endpoint (or the whole backend when
// endpoint is empty).
func (d *Dispatcher) markHealthy(name, endpoint string) {
	h := d.healthFor(healthKey(name, endpoint))
	h.healthy.Store(true)
	h.quarantineUntil.Store(0)
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
	endpoint := d.pickEndpoint(backend)
	url := joinURL(dispatchTarget(backend, endpoint), path)

	// Send the model name this backend actually serves, not the
	// client-facing alias the router matched on.
	outboundBody := applyModelOverride(requestBody, backend.outboundModel())

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(outboundBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	d.copyForwardedHeaders(headers, req.Header)
	if err := d.applyCredentials(backend, req); err != nil {
		return nil, err
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
	// IdleConnTimeout (defaultIdleConnTimeout, 10s) is short enough
	// that we don't see the same stale-conn failure mode.
	if strings.EqualFold(backend.Tier, "cloud") {
		req.Header.Set("Connection", "close")
		req.Close = true
	}

	resp, err := d.client.Do(req)
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
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	if resp.StatusCode >= 500 {
		// Don't flip on 4xx: those are client errors, not backend
		// problems. 5xx however means the backend itself misbehaved.
		d.markUnhealthy(backend.Name, endpoint)
	} else {
		d.markHealthy(backend.Name, endpoint)
	}
	return resp, nil
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
// backends serve whatever their InferenceService is named, which is also the
// alias the controller registers upstream, so a request that reached this
// backend under a different alias (a rule fall-through, an IfIdle skip past a
// busy pool member) must be rewritten to it: llama.cpp ignores the field, but
// vLLM / SGLang / TGI answer an unknown name with 404 "The model X does not
// exist". A backend with neither set (a hand-written config, a backend whose
// InferenceService did not resolve) keeps the historical pass-through.
func (b *Backend) outboundModel() string {
	if b.Model != "" {
		return b.Model
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
