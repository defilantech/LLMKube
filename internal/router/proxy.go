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
)

// Proxy is the router-proxy HTTP application. Construct via NewProxy and
// register handlers with Mount; the proxy is safe for concurrent use.
type Proxy struct {
	cfg     *Config
	matcher *Matcher
	disp    *Dispatcher
	logger  *slog.Logger
}

// NewProxy constructs a Proxy from a loaded Config.
func NewProxy(cfg *Config, logger *slog.Logger) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	return &Proxy{
		cfg:     cfg,
		matcher: NewMatcher(cfg),
		disp:    NewDispatcher(cfg),
		logger:  logger,
	}
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
// Model field; local backends report their backend name.
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
		if b.Model != "" {
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
		return
	}

	start := time.Now()
	chosen, resp, err := p.dispatchWithFallback(r.Context(), &decision, r.Header, body, "/v1/chat/completions")
	elapsed := time.Since(start)
	if err != nil {
		writeError(w, http.StatusBadGateway, "all backends failed: "+err.Error())
		p.audit(features, decision, nil, http.StatusBadGateway, "all_backends_failed", elapsed)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	streamed := streamResponse(w, resp, isStream)
	p.audit(features, decision, chosen, resp.StatusCode, streamedReason(streamed), elapsed)
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
// propagates back to the handler as HTTP 502.
func (p *Proxy) dispatchWithFallback(
	ctx context.Context,
	dec *MatchResult,
	headers http.Header,
	body []byte,
	path string,
) (*Backend, *http.Response, error) {
	var lastErr error
	for _, name := range dec.Backends {
		b := p.matcher.BackendByName(name)
		if b == nil {
			lastErr = fmt.Errorf("backend %q not configured", name)
			continue
		}
		if !p.disp.IsHealthy(name) {
			lastErr = fmt.Errorf("backend %q marked unhealthy", name)
			continue
		}
		resp, err := p.disp.Dispatch(ctx, b, http.MethodPost, path, headers, body)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			// Drain and close so the connection is reusable.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("%s returned %d", name, resp.StatusCode)
			continue
		}
		return b, resp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no backends attempted")
	}
	return nil, nil, lastErr
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
	p.logger.Info("router.dispatch", attrs...)
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
