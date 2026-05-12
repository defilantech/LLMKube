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
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Dispatcher knows how to forward an inbound request to a chosen backend.
// One Dispatcher instance is shared across all requests; it owns the
// per-backend http.Client pool and health bookkeeping.
type Dispatcher struct {
	cfg     *Config
	client  *http.Client
	healthy sync.Map // backendName -> *atomic.Bool

	// nowFn is overridable in tests.
	nowFn func() time.Time
}

// NewDispatcher returns a Dispatcher bound to the given Config. All
// backends start in a healthy state; the proxy flips them unhealthy on
// failed requests and back to healthy on the next successful request.
// More sophisticated health probing (separate goroutine pinging /health)
// lands with the production-hardening phase.
func NewDispatcher(cfg *Config) *Dispatcher {
	d := &Dispatcher{
		cfg: cfg,
		client: &http.Client{
			// Streaming requests can be long-lived. The per-request
			// context is the real deadline; this caps idle and connect
			// phases to prevent leaking goroutines on dead backends.
			Timeout: 0,
			Transport: &http.Transport{
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
		nowFn: time.Now,
	}
	for _, b := range cfg.Backends {
		var h atomic.Bool
		h.Store(true)
		d.healthy.Store(b.Name, &h)
	}
	return d
}

// IsHealthy reports the dispatcher's current view of backend health.
// Unknown backend names report unhealthy.
func (d *Dispatcher) IsHealthy(name string) bool {
	v, ok := d.healthy.Load(name)
	if !ok {
		return false
	}
	return v.(*atomic.Bool).Load()
}

// MarkHealthy flips the backend to healthy. Called on every successful
// dispatch; cheap when already healthy.
func (d *Dispatcher) MarkHealthy(name string) {
	if v, ok := d.healthy.Load(name); ok {
		v.(*atomic.Bool).Store(true)
	}
}

// MarkUnhealthy flips the backend to unhealthy. The proxy calls this on
// 5xx / network errors. Subsequent requests skip this backend during
// primary-fallback strategy until the next successful dispatch flips it
// back to healthy.
func (d *Dispatcher) MarkUnhealthy(name string) {
	if v, ok := d.healthy.Load(name); ok {
		v.(*atomic.Bool).Store(false)
	}
}

// Dispatch forwards the request to the named backend and returns the
// upstream response. Caller is responsible for streaming the body to
// the inbound client and closing it. On error the response is nil.
//
// requestBody is the already-buffered inbound body. The proxy reads the
// body once (it needs to parse the "model" field) and reuses the bytes
// across fallback attempts.
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
	url := joinURL(backend.Address, path)

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	d.copyForwardedHeaders(headers, req.Header)
	if err := d.applyCredentials(backend, req); err != nil {
		return nil, err
	}

	resp, err := d.client.Do(req)
	if err != nil {
		d.MarkUnhealthy(backend.Name)
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	if resp.StatusCode >= 500 {
		// Don't flip on 4xx: those are client errors, not backend
		// problems. 5xx however means the backend itself misbehaved.
		d.MarkUnhealthy(backend.Name)
	} else {
		d.MarkHealthy(backend.Name)
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
