/*
Copyright 2025.

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

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"go.uber.org/zap"
)

// backendProvider returns the loopback address (host:port) of the inference
// child the client proxy should currently forward to. ok is false when no
// child is running.
type backendProvider interface {
	currentBackend() (addr string, ok bool)
}

// ClientProxy is a stable host-side HTTP listener that forwards requests to
// whichever inference child the metal-agent is currently running. The child's
// port is allocated dynamically per spawn, so host-side clients (opencode,
// aider, curl) point at this fixed port and never have to track the moving
// child port. In-cluster clients are unaffected; they reach the child via the
// agent's Endpoints registration. See #406.
type ClientProxy struct {
	provider backendProvider
	port     int
	logger   *zap.SugaredLogger
}

// NewClientProxy builds a ClientProxy. A port <= 0 means Start is a no-op
// (the listener is disabled); ServeHTTP still works for direct testing.
func NewClientProxy(provider backendProvider, port int, logger *zap.SugaredLogger) *ClientProxy {
	return &ClientProxy{provider: provider, port: port, logger: logger}
}

// ServeHTTP forwards the request to the current child. When no child is
// running it returns 503 with a JSON error body (mirroring the prior
// standalone vllm-swift-proxy.py behavior).
func (p *ClientProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	addr, ok := p.provider.currentBackend()
	if !ok {
		clientProxyRequests.WithLabelValues("no_backend").Inc()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "no inference process is currently running on this agent",
		})
		return
	}

	rp := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: addr})
	// Flush each chunk immediately so SSE / stream:true completions are not
	// buffered by the proxy.
	rp.FlushInterval = -1
	rp.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, err error) {
		p.logger.Warnw("client proxy upstream error", "target", addr, "err", err.Error())
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(rw).Encode(map[string]string{"error": "upstream inference process unreachable"})
	}

	sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	rp.ServeHTTP(sw, r)
	clientProxyRequests.WithLabelValues(statusClass(sw.status)).Inc()
}

// Start runs the listener on 127.0.0.1:<port> until ctx is cancelled. A
// non-positive port disables the proxy and returns nil immediately.
func (p *ClientProxy) Start(ctx context.Context) error {
	if p.port <= 0 {
		return nil
	}
	srv := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", p.port),
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
	}
	//nolint:gosec // G118: the parent ctx is already cancelled here; graceful
	// shutdown needs a fresh, bounded context, so context.Background is correct.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	p.logger.Infow("client proxy listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// statusRecorder captures the response status for metrics while delegating
// Flush so the reverse proxy can stream SSE responses.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "other"
	}
}
