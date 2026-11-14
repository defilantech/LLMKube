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

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/defilantech/llmkube/pkg/agent/ingress"
)

// proxyBackend is one running inference child the client proxy can forward
// to.
type proxyBackend struct {
	// id is the InferenceService's namespace/name.
	id string
	// addr is the child's loopback address (host:port).
	addr string
	// runtime is the same string the ingress uses for that child
	// (ingress.Route.Runtime), so ServeHTTP can apply the identical
	// ingress.Allowed path policy.
	runtime string
	healthy bool
	// servedModel is the model ID the agent asked the engine to serve
	// (ExecutorConfig.ServedModelName): the name GET /v1/models lists and
	// clients send. Empty when the agent did not set one, for example on an
	// engine adopted from an agent that did not record it.
	servedModel string
	// modelID is the shared daemon's model identifier (oMLX, Ollama).
	modelID string
}

// backendProvider lists the inference children the client proxy can forward
// to, in a stable order. It is empty when no child is running.
type backendProvider interface {
	backends() []proxyBackend
}

// ClientProxy is a stable host-side HTTP listener that forwards requests to
// the inference children the metal-agent is running. The child's port is
// allocated dynamically per spawn, so host-side clients (opencode, aider,
// curl) point at this fixed port and never have to track the moving child
// port. In-cluster clients are unaffected; they reach the child via the
// agent's Endpoints registration. See #406.
//
// With one child running, every request goes to it, whatever model it names,
// as before. With several, a request goes to the child its model names
// (#2412); see selectBackend.
type ClientProxy struct {
	provider backendProvider
	port     int
	logger   *zap.SugaredLogger
	// transport carries proxied requests to the child; nil means
	// http.DefaultTransport. Tests set it to observe child connections.
	transport http.RoundTripper
}

// NewClientProxy builds a ClientProxy. A port <= 0 means Start is a no-op
// (the listener is disabled); ServeHTTP still works for direct testing.
func NewClientProxy(provider backendProvider, port int, logger *zap.SugaredLogger) *ClientProxy {
	return &ClientProxy{provider: provider, port: port, logger: logger}
}

// ServeHTTP applies, in order: a Host check (421) that rejects DNS rebinding
// before anything else can learn whether a child is even running, the
// no-backend check (503), then the same runtime path allowlist the TLS
// ingress enforces (403, see ingress.Allowed). With several children running
// it then picks the one the request's model names (400, 404 or 409 when it
// cannot), and checks the path against that child's runtime. Only a request
// that clears every check is proxied.
func (p *ClientProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.hostAllowed(r.Host) {
		clientProxyRequests.WithLabelValues("bad_host").Inc()
		writeClientProxyError(w, http.StatusMisdirectedRequest,
			"Host header does not match this loopback proxy")
		return
	}

	bs := p.provider.backends()
	if len(bs) == 0 {
		clientProxyRequests.WithLabelValues("no_backend").Inc()
		writeClientProxyError(w, http.StatusServiceUnavailable,
			"no inference process is currently running on this agent")
		return
	}

	if !anyAllowed(bs, r.Method, r.URL.EscapedPath()) {
		clientProxyRequests.WithLabelValues("path_forbidden").Inc()
		writeClientProxyError(w, http.StatusForbidden, "path is not allowed for this runtime")
		return
	}

	b := bs[0]
	if len(bs) > 1 {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			p.serveMergedModels(w, r, bs)
			return
		}
		var ok bool
		if b, ok = p.routeByModel(w, r, bs); !ok {
			return
		}
		if !ingress.Allowed(b.runtime, r.Method, r.URL.EscapedPath()) {
			clientProxyRequests.WithLabelValues("path_forbidden").Inc()
			writeClientProxyError(w, http.StatusForbidden, "path is not allowed for this runtime")
			return
		}
	}

	// Full duplex keeps the HTTP/1 server from closing r.Body when the first
	// response chunk is written, before the Transport's final EOF read of it;
	// see the matching call in pkg/agent/ingress/server.go's ServeHTTP. Every
	// host-side client reaches this listener over plaintext HTTP/1.1.
	_ = http.NewResponseController(w).EnableFullDuplex()
	rp := p.reverseProxy(b.addr)
	sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	rp.ServeHTTP(sw, r)
	clientProxyRequests.WithLabelValues(statusClass(sw.status)).Inc()
}

// anyAllowed reports whether some running child's runtime allows method and
// rawPath, so a path no child serves is refused before the body is read.
func anyAllowed(bs []proxyBackend, method, rawPath string) bool {
	for _, b := range bs {
		if ingress.Allowed(b.runtime, method, rawPath) {
			return true
		}
	}
	return false
}

// reverseProxy builds the reverse proxy to the current child on addr
// (127.0.0.1:<port>). This mirrors pkg/agent/ingress/server.go's Server.proxy
// (see the sibling there): a Rewrite hook is used instead of the legacy
// NewSingleHostReverseProxy Director, because only ProxyRequest.SetURL clears
// the outbound Host header. NewSingleHostReverseProxy's Director leaves the
// caller's original Host (e.g. "localhost:<client-proxy port>") on the
// outbound request, which Go's Transport then sends to the child verbatim
// instead of the child's own address.
//
// Rewrite also deletes the inbound Upgrade and Connection headers and clears
// request trailers, for the same reasons the ingress does: ReverseProxy
// re-adds Upgrade/Connection after stripping hop-by-hop headers otherwise, an
// upgraded connection is a raw byte tunnel the path allowlist can no longer
// see, and trailers are headers that arrive after the checks in ServeHTTP
// already ran. ModifyResponse shares the ingress's ingress.RefuseUpgrade, so
// an engine that upgrades unsolicited still never reaches the caller as a
// 101; ErrorHandler turns that refusal into the same 502 JSON body as any
// other upstream failure.
func (p *ClientProxy) reverseProxy(addr string) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: addr}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Header.Del("Upgrade")
			pr.Out.Header.Del("Connection")
			pr.Out.Trailer = nil
		},
		ModifyResponse: ingress.RefuseUpgrade,
		Transport:      p.transport,
		// Flush each chunk immediately so SSE / stream:true completions are
		// not buffered by the proxy.
		FlushInterval: -1,
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, err error) {
			p.logger.Warnw("client proxy upstream error", "target", addr, "err", err.Error())
			writeClientProxyError(rw, http.StatusBadGateway, "upstream inference process unreachable")
		},
	}
}

// hostAllowed reports whether host (r.Host) names this loopback proxy: an
// exact match, case-insensitive for "localhost", against "localhost",
// "127.0.0.1" or "::1", on the port this proxy is configured to listen on. A
// host with no port at all is accepted only when that port is 80. This
// refuses DNS rebinding: a page served from an attacker-controlled public
// domain that resolves to 127.0.0.1 and is fetched by a browser on this
// machine presents that domain as the Host header, not "localhost", so it is
// rejected here before the request can reach a child or even learn whether
// one is running.
func (p *ClientProxy) hostAllowed(host string) bool {
	h, portStr, err := net.SplitHostPort(host)
	if err != nil {
		return p.port == 80 && isLoopbackHost(trimBrackets(host))
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port != p.port {
		return false
	}
	return isLoopbackHost(h)
}

// isLoopbackHost reports whether h (already stripped of any port and
// brackets) is one of the loopback names this proxy answers to.
func isLoopbackHost(h string) bool {
	return strings.EqualFold(h, "localhost") || h == "127.0.0.1" || h == "::1"
}

// trimBrackets strips a literal IPv6 host's surrounding brackets, e.g. "[::1]"
// to "::1". net.SplitHostPort already does this when a port is present; this
// covers the no-port case, where the brackets are the only signal that the
// host is a literal address rather than a name.
func trimBrackets(h string) string {
	if len(h) >= 2 && h[0] == '[' && h[len(h)-1] == ']' {
		return h[1 : len(h)-1]
	}
	return h
}

// writeClientProxyError writes a JSON error body, the shape every client
// proxy failure response uses.
func writeClientProxyError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
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
