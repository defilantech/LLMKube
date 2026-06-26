/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	otelattribute "go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

// Proxy is the router-proxy HTTP application. Construct via NewProxy and
// register handlers with Mount; the proxy is safe for concurrent use.
type Proxy struct {
	cfg        *Config
	matcher    *Matcher
	disp       *Dispatcher
	logger     *slog.Logger
	routerName string
}

// ProxyOption customizes a Proxy at construction time. The proxy
// owns the Dispatcher, so options that affect dispatching (eg
// quarantine duration) are forwarded through.
type ProxyOption func(*Proxy)

// WithDispatcherOptions threads DispatcherOption values down to the
// proxy's owned Dispatcher. Used to set --quarantine-duration from
// the CLI without leaking Dispatcher construction up to callers.
func WithDispatcherOptions(opts ...DispatcherOption) ProxyOption {
	return func(p *Proxy) {
		// Rebuild the dispatcher with the requested options. NewProxy
		// runs WithDispatcherOptions *after* NewDispatcher already
		// constructed a default-options dispatcher; rebuilding here
		// keeps the option API uniform without making callers care
		// about construction order.
		p.disp = NewDispatcher(p.cfg, opts...)
	}
}

// WithRouterName sets the router name used in metric labels and OTel
// span attributes. Defaults to "default" when omitted.
func WithRouterName(name string) ProxyOption {
	return func(p *Proxy) { p.routerName = name }
}

// NewProxy constructs a Proxy from a loaded Config.
func NewProxy(cfg *Config, logger *slog.Logger, opts ...ProxyOption) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Proxy{
		cfg:        cfg,
		matcher:    NewMatcher(cfg),
		disp:       NewDispatcher(cfg),
		logger:     logger,
		routerName: "default",
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Mount wires up the OpenAI-compatible endpoints plus /health on the
// given mux. Callers attach the mux to an http.Server.
func (p *Proxy) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", p.handleChatCompletions)
	mux.HandleFunc("GET /v1/models", p.handleModels)
	mux.HandleFunc("GET /health", p.handleHealth)
	mux.HandleFunc("GET /healthz", p.handleHealth)
}

// handleHealth always returns 200. The Kubernetes liveness probe uses
// this; the proxy is "alive" as long as its goroutine runs. Readiness
// gating on backend health lands with #432 / #428.
func (p *Proxy) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleModels returns an OpenAI-compatible /v1/models payload listing
// every backend in the config. Cloud backends report their upstream
// Model field; local backends report their backend name. When a backend
// has a DisplayName, that is published as the model id instead of Name.
func (p *Proxy) handleModels(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	now := time.Now().Unix()
	models := make([]model, 0, len(p.cfg.Backends))
	for _, b := range p.cfg.Backends {
		id := b.Name
		if b.DisplayName != "" {
			id = b.DisplayName
		} else if b.Model != "" {
			id = b.Model
		}
		owned := "llmkube"
		if b.Provider != "" {
			owned = b.Provider
		}
		models = append(models, model{ID: id, Object: "model", Created: now, OwnedBy: owned})
	}
	body, _ := json.Marshal(map[string]any{"object": "list", "data": models})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// handleChatCompletions is the primary routing endpoint. It buffers the
// inbound request body (needs the "model" field for matching), evaluates
// the rule set, dispatches to the chosen backend, and streams the
// response back. SSE / chunked passthrough is automatic.
func (p *Proxy) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body: "+err.Error())
		return
	}

	features, isStream := extractFeatures(body, r, p.cfg.ClassificationHeader())

	decision := p.matcher.Match(&features)
	if len(decision.Backends) == 0 {
		writeError(w, http.StatusServiceUnavailable, "no rule matched and no defaultRoute configured")
		p.audit(features, decision, nil, http.StatusServiceUnavailable, "no_route", 0)
		return
	}

	if err := p.enforceFailClosed(&features, &decision); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		p.audit(features, decision, nil, http.StatusServiceUnavailable, "fail_closed", 0)
		p.observeFailClosed(&features, &decision)
		return
	}

	tracer := otel.Tracer("model_router.dispatch")
	attrs := []otelattribute.KeyValue{
		otelattribute.String("routing.classification", features.Classification),
	}
	if decision.Rule != nil {
		attrs = append(attrs, otelattribute.String("routing.rule.matched", decision.Rule.Name))
		attrs = append(attrs, otelattribute.String("routing.strategy", decision.Rule.Route.Strategy))
	}
	ctx, span := tracer.Start(r.Context(), "model_router.dispatch", oteltrace.WithAttributes(attrs...))
	defer span.End()

	start := time.Now()
	chosen, resp, err := p.dispatchWithFallback(ctx, &decision, r.Header, body, "/v1/chat/completions")
	elapsed := time.Since(start)
	if err != nil {
		// Runtime fail-closed: when every backend in a fail-closed
		// rule's pool is unreachable, return 503 with a clear reason
		// rather than 502. This is the runtime counterpart to the
		// controller's static fail-closed validation: we refuse the
		// request instead of letting it spill onto an unmatched
		// backend (which dispatchWithFallback never does anyway, but
		// the status code communicates intent — sensitive data did
		// not egress because policy said so, not because of a generic
		// upstream outage).
		if decision.FailClosed {
			writeError(w, http.StatusServiceUnavailable,
				"fail-closed: all rule backends unhealthy: "+err.Error())
			p.audit(features, decision, nil, http.StatusServiceUnavailable,
				"fail_closed_runtime", elapsed)
			p.observeFailClosed(&features, &decision)
			return
		}
		writeError(w, http.StatusBadGateway, "all backends failed: "+err.Error())
		p.audit(features, decision, nil, http.StatusBadGateway, "all_backends_failed", elapsed)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	span.SetAttributes(
		otelattribute.String("routing.backend.selected", chosen.Name),
		otelattribute.String("routing.backend.tier", chosen.Tier),
	)

	streamed := streamResponse(w, resp, isStream)
	outcome := streamedReason(streamed)
	p.audit(features, decision, chosen, resp.StatusCode, outcome, elapsed)
	p.observeRequest(&features, &decision, chosen, outcome, elapsed)

	// Record TTFT for streaming responses. We approximate first-byte
	// time as the time the response body began flowing: the proxy
	// writes the status line immediately on streamResponse entry, so
	// the elapsed window from dispatch start to the first flush is a
	// close proxy of TTFT. Non-streaming responses skip this gauge —
	// their TTFT equals their total duration and is already captured
	// by RouterRequestDuration.
	if isStream && chosen != nil {
		prommetrics.RouterFirstTokenSeconds.WithLabelValues(p.routerName, chosen.Name).Observe(elapsed.Seconds())
	}

	// Record budget utilization against the resolved per-request
	// deadline. scope=rule when a rule's timeout drove the cap, else
	// scope=proxy. Values above 1.0 mean the request consumed more
	// time than the cap allowed (shouldn't happen for successful
	// dispatches, but the gauge is float so it's safe).
	if chosen != nil {
		resolved := resolveDispatchTimeout(&decision, chosen, p.disp.ResponseHeaderTimeout())
		if resolved > 0 {
			util := elapsed.Seconds() / resolved.Seconds()
			scope := "proxy"
			if decision.Rule != nil && decision.Rule.Timeout > 0 {
				scope = "rule"
			}
			prommetrics.RouterBudgetUtilization.WithLabelValues(p.routerName, scope).Set(util)
		}
	}
}

const maxRequestBodyBytes = 32 << 20 // 32 MiB, generous for long prompts

// extractFeatures pulls the model name, stream flag, classification, and
// task complexity out of the inbound request. The model name comes from
// the JSON body; the rest come from headers per the MVP header-only
// classification mode.
func extractFeatures(body []byte, r *http.Request, classHeader string) (RequestFeatures, bool) {
	headers := make(map[string]string, len(r.Header))
	for k, vals := range r.Header {
		if len(vals) > 0 {
			headers[strings.ToLower(k)] = vals[0]
		}
	}

	var partial struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	// Body may not be valid JSON yet at this point (the upstream may be
	// more permissive). We extract what we can and proceed.
	_ = json.Unmarshal(body, &partial)

	return RequestFeatures{
		Model:          partial.Model,
		Classification: strings.ToLower(headers[strings.ToLower(classHeader)]),
		TaskComplexity: strings.ToLower(headers["x-llmkube-task-complexity"]),
		Headers:        headers,
	}, partial.Stream
}

// enforceFailClosed implements the runtime half of the fail-closed gate.
// For sensitive-data requests routing to a fail-closed rule, every
// backend in the route must be local-tier; otherwise we refuse. The
// static half of this check runs in the controller at apply time, but
// repeating it here defends against drift between controller and proxy
// config.
func (p *Proxy) enforceFailClosed(f *RequestFeatures, dec *MatchResult) error {
	if !dec.FailClosed {
		return nil
	}
	sensitive := p.cfg.SensitiveSet()
	if !sensitive[f.Classification] {
		return nil
	}
	for _, name := range dec.Backends {
		b := p.matcher.BackendByName(name)
		if b == nil {
			return fmt.Errorf("fail-closed: backend %q not configured", name)
		}
		if b.Tier != "local" {
			return fmt.Errorf("fail-closed: sensitive classification %q cannot route to %s-tier backend %q",
				f.Classification, b.Tier, name)
		}
	}
	return nil
}

// dispatchWithFallback walks the backend list in declared order and
// returns the first successful (non-5xx, non-error) response. On
// fail-closed routes with all backends unhealthy, the last error
// propagates back to the handler as HTTP 502 or 503 (the caller in
// handleChatCompletions decides the surface based on dec.FailClosed).
//
// Per-attempt deadline: resolveDispatchTimeout produces the cap for
// each backend attempt, with resolution order rule -> backend ->
// proxy default. The deadline is applied per attempt, not once for
// the whole loop, so a slow primary that timed out does NOT eat the
// fallback's budget.
func (p *Proxy) dispatchWithFallback(
	ctx context.Context,
	dec *MatchResult,
	headers http.Header,
	body []byte,
	path string,
) (*Backend, *http.Response, error) {
	var lastErr error
	tracer := otel.Tracer("model_router.dispatch")
	for i, name := range dec.Backends {
		b := p.matcher.BackendByName(name)
		if b == nil {
			lastErr = fmt.Errorf("backend %q not configured", name)
			continue
		}
		if !p.disp.IsHealthy(name) {
			lastErr = fmt.Errorf("backend %q marked unhealthy", name)
			continue
		}
		attemptCtx, cancel := context.WithTimeout(ctx,
			resolveDispatchTimeout(dec, b, p.disp.ResponseHeaderTimeout()))
		_, span := tracer.Start(attemptCtx, "backend.request",
			oteltrace.WithAttributes(
				otelattribute.String("routing.backend.selected", b.Name),
				otelattribute.String("routing.backend.provider", b.Provider),
				otelattribute.String("routing.backend.tier", b.Tier),
				otelattribute.Int("routing.fallback.depth", i),
			),
		)
		resp, err := p.disp.Dispatch(attemptCtx, b, http.MethodPost, path, headers, body)
		if err != nil {
			span.End()
			cancel()
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			// Drain and close so the connection is reusable.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			span.End()
			cancel()
			lastErr = fmt.Errorf("%s returned %d", name, resp.StatusCode)
			continue
		}
		// Successful response: wrap the body so its Close also
		// cancels the per-attempt context. The caller's existing
		// `defer resp.Body.Close()` is enough — no separate cancel
		// plumbing needed, and streaming dispatches keep the
		// deadline alive until the client finishes reading.
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
		span.End()
		return b, resp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no backends attempted")
	}
	return nil, nil, lastErr
}

// cancelOnClose pairs a response body with the cancel func of the
// per-attempt context. The proxy's caller already does
// `defer resp.Body.Close()`, so wrapping the body ensures the
// per-attempt context cancel fires no later than the response is
// fully consumed (and no sooner — streaming responses need the
// deadline to outlive the chat completion).
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// resolveDispatchTimeout computes the per-attempt context deadline
// using the resolution order: rule.Timeout || backend.Timeout ||
// proxy default. Zero values fall through to the next level. This
// is the runtime half of #458; the controller already validated
// bounds at apply time so any non-zero value reaching here is sane.
func resolveDispatchTimeout(dec *MatchResult, backend *Backend, proxyDefault time.Duration) time.Duration {
	if dec != nil && dec.Rule != nil && dec.Rule.Timeout > 0 {
		return dec.Rule.Timeout
	}
	if backend != nil && backend.Timeout > 0 {
		return backend.Timeout
	}
	return proxyDefault
}

// streamResponse copies the upstream response to the client. Returns
// true if we treated the response as a stream (set SSE-friendly headers
// and flushed after every chunk). The decision is driven by the
// request's "stream": true flag and the upstream Content-Type.
func streamResponse(w http.ResponseWriter, resp *http.Response, requestedStream bool) bool {
	upstreamCT := resp.Header.Get("Content-Type")
	isSSE := strings.HasPrefix(upstreamCT, "text/event-stream")
	isStream := requestedStream || isSSE

	// Forward upstream headers (except hop-by-hop).
	for k, vals := range resp.Header {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	if isStream && w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.WriteHeader(resp.StatusCode)

	if !isStream {
		_, _ = io.Copy(w, resp.Body)
		return false
	}

	var flush func()
	if f, ok := w.(http.Flusher); ok {
		flush = f.Flush
	}
	_, _ = PipeBody(w, resp.Body, flush)
	return true
}

// audit emits one structured log line per request. Sink configuration
// (file, OTLP) lands with #434; for the MVP we always log to the proxy
// stdout via slog.
func (p *Proxy) audit(
	f RequestFeatures,
	dec MatchResult,
	chosen *Backend,
	statusCode int,
	outcome string,
	elapsed time.Duration,
) {
	attrs := []any{
		"model", f.Model,
		"classification", f.Classification,
		"taskComplexity", f.TaskComplexity,
		"status", statusCode,
		"outcome", outcome,
		"latencyMs", elapsed.Milliseconds(),
	}
	if dec.Rule != nil {
		attrs = append(attrs, "rule", dec.Rule.Name)
	}
	if chosen != nil {
		attrs = append(attrs, "backend", chosen.Name, "backendTier", chosen.Tier)
	}
	// The resolved per-request deadline is informative regardless of
	// outcome: on success it shows the budget that DID land, on
	// timeout it shows the budget that DIDN'T suffice. Operators
	// debugging "why did this rule 504?" can grep audit logs for
	// `timeoutMs=<expected>` and reconcile vs the CRD spec.
	attrs = append(attrs, "timeoutMs",
		resolveDispatchTimeout(&dec, chosen, p.disp.ResponseHeaderTimeout()).Milliseconds())
	p.logger.Info("router.dispatch", attrs...)
}

// observeRequest records the llmkube_router_requests_total counter and
// llmkube_router_request_duration_seconds histogram for a completed
// dispatch.
func (p *Proxy) observeRequest(f *RequestFeatures, dec *MatchResult, chosen *Backend, outcome string, elapsed time.Duration) {
	ruleName := ""
	if dec.Rule != nil {
		ruleName = dec.Rule.Name
	}
	backendName := ""
	if chosen != nil {
		backendName = chosen.Name
	}
	prommetrics.RouterRequestsTotal.WithLabelValues(
		p.routerName, ruleName, backendName, f.Classification, outcome,
	).Inc()
	prommetrics.RouterRequestDuration.WithLabelValues(
		p.routerName, ruleName, backendName,
	).Observe(elapsed.Seconds())
	// Refresh the per-backend health gauge to reflect the dispatcher's
	// current quarantine state. The dispatcher already flipped the
	// atomic.Bool on Dispatch return (healthy on 2xx, unhealthy on 5xx
	// or connect failure), so this just reads the live state.
	if chosen != nil {
		p.updateBackendHealthMetrics(chosen.Name, p.disp.IsHealthy(chosen.Name))
	}
	p.updateActiveBackendsMetrics()
}

// observeFailClosed records the llmkube_router_fail_closed_total counter
// when a request is rejected by the fail-closed gate.
func (p *Proxy) observeFailClosed(f *RequestFeatures, dec *MatchResult) {
	ruleName := ""
	if dec.Rule != nil {
		ruleName = dec.Rule.Name
	}
	prommetrics.RouterFailClosedTotal.WithLabelValues(
		p.routerName, ruleName, f.Classification,
	).Inc()
}

// updateBackendHealthMetrics sets the llmkube_router_backend_health gauge
// for the named backend to 1 (healthy) or 0 (unhealthy).
func (p *Proxy) updateBackendHealthMetrics(name string, healthy bool) {
	val := 0.0
	if healthy {
		val = 1.0
	}
	prommetrics.RouterBackendHealth.WithLabelValues(p.routerName, name).Set(val)
}

// updateActiveBackendsMetrics sets the llmkube_router_active_backends
// gauge for each tier based on the current health of backends in that
// tier.
func (p *Proxy) updateActiveBackendsMetrics() {
	tierCount := map[string]int{}
	for _, b := range p.cfg.Backends {
		if p.disp.IsHealthy(b.Name) {
			tierCount[b.Tier]++
		}
	}
	for tier, count := range tierCount {
		prommetrics.RouterActiveBackends.WithLabelValues(p.routerName, tier).Set(float64(count))
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": code, "message": msg},
	})
	_, _ = w.Write(body)
}

func streamedReason(streamed bool) string {
	if streamed {
		return "ok_stream"
	}
	return "ok"
}
