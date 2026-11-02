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

package ingress

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// RequestsTotal counts ingress requests by response status code. pkg/agent
// registers it on the agent's Prometheus registry.
var RequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "llmkube_metal_agent_ingress_requests_total",
		Help: "Total metal-agent ingress requests, labeled by HTTP response status code.",
	},
	[]string{"code"},
)

// Route is where the ingress forwards requests for one InferenceService.
type Route struct {
	// Runtime selects the path allowlist (see Allowed).
	Runtime string
	// Port is the engine's loopback port on 127.0.0.1.
	Port int
	// Ready is true when the engine is serving.
	Ready bool
}

// Router resolves relay targets to engine routes.
type Router interface {
	// Route returns the route for namespace/name, or false when this agent
	// does not run that InferenceService.
	Route(namespace, name string) (Route, bool)
	// ServesNamespace reports whether this agent runs any InferenceService
	// in namespace. The ingress only consults the TokenStore for served
	// namespaces, so an unauthenticated caller cannot make it read (and
	// cache) Secrets in arbitrary namespaces.
	ServesNamespace(namespace string) bool
}

const (
	shutdownTimeout = 5 * time.Second
	// idleTimeout closes keep-alive connections with no request in flight;
	// it never interrupts a streaming response.
	idleTimeout = 2 * time.Minute

	msgUnauthorized = "invalid relay token or target"
)

// Server is the metal-agent's authenticated TLS ingress. In-cluster relays
// present a per-namespace token (HeaderRelayToken) and a target
// (HeaderRelayTarget, "<ns>/<name>"); allowed requests are reverse-proxied
// to the target's engine on 127.0.0.1. Token values are never logged.
type Server struct {
	identity *Identity
	tokens   *TokenStore
	routes   Router
	logger   *zap.SugaredLogger
	// transport carries proxied requests to the engine; nil means
	// http.DefaultTransport. Tests set it to observe engine connections.
	transport http.RoundTripper

	mu   sync.Mutex
	addr net.Addr // set once Start is listening; for tests
}

// NewServer returns a Server presenting identity's current certificate,
// authenticating with tokens and resolving targets through routes.
func NewServer(identity *Identity, tokens *TokenStore, routes Router, logger *zap.SugaredLogger) *Server {
	return &Server{identity: identity, tokens: tokens, routes: routes, logger: logger}
}

// ServeHTTP applies, in order: target parse (400), served-namespace gate and
// token check (both 401 with an identical response), route lookup (404),
// the local readiness answer for GET IngressReadyPath, readiness (503), the
// runtime path allowlist (403), then strips both relay headers and proxies
// to the engine.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() { RequestsTotal.WithLabelValues(strconv.Itoa(sw.status)).Inc() }()

	ns, name, ok := parseTarget(r.Header.Get(inferencev1alpha1.HeaderRelayTarget))
	if !ok {
		writeJSONError(sw, http.StatusBadRequest, "missing or malformed "+inferencev1alpha1.HeaderRelayTarget+" header")
		return
	}
	// The namespace gate runs before the TokenStore and answers exactly like
	// a bad token, so an unserved namespace never causes a Secret read and
	// gets the same response bytes. Response timing can still differ (a
	// served namespace may wait on a Secret read), so the gate is not a
	// timing-safe oracle defense.
	if !s.routes.ServesNamespace(ns) ||
		!s.tokens.Valid(r.Context(), ns, r.Header.Get(inferencev1alpha1.HeaderRelayToken)) {
		s.logger.Debugw("ingress rejected relay credentials", "namespace", ns, "name", name)
		writeJSONError(sw, http.StatusUnauthorized, msgUnauthorized)
		return
	}
	route, ok := s.routes.Route(ns, name)
	if !ok {
		writeJSONError(sw, http.StatusNotFound, "target is not running on this agent")
		return
	}
	escapedPath := r.URL.EscapedPath()
	if r.Method == http.MethodGet && escapedPath == inferencev1alpha1.IngressReadyPath {
		s.serveReady(sw, route)
		return
	}
	if !route.Ready {
		writeJSONError(sw, http.StatusServiceUnavailable, "target is not ready")
		return
	}
	if !Allowed(route.Runtime, r.Method, escapedPath) {
		writeJSONError(sw, http.StatusForbidden, "path is not allowed for this runtime")
		return
	}

	r.Header.Del(inferencev1alpha1.HeaderRelayToken)
	r.Header.Del(inferencev1alpha1.HeaderRelayTarget)
	// The proxy's Transport reads r.Body while the response streams back:
	// after an HTTP/1 Transport sends a body of known length it reads the
	// body once more to confirm EOF, and that read can land after the engine
	// answered and the first chunk was flushed to the caller. Unless full
	// duplex is on, an HTTP/1 server closes the request body when the
	// handler writes its response header, so the late read fails ("invalid
	// Read on closed Body"), the Transport closes the engine connection, and
	// the stream is cut after the chunks already sent. HTTP/2 is always full
	// duplex; a writer that cannot enable it (a test recorder) is left as is.
	_ = http.NewResponseController(sw).EnableFullDuplex()
	s.proxy(route.Port).ServeHTTP(sw, r)
}

func (s *Server) serveReady(w http.ResponseWriter, route Route) {
	if !route.Ready {
		writeJSONError(w, http.StatusServiceUnavailable, "target is not ready")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

// ErrUpgradeRefused is returned by RefuseUpgrade when an engine answers 101
// Switching Protocols. It is exported so pkg/agent's host-side client proxy
// (clientproxy.go) can share RefuseUpgrade instead of duplicating it; that is
// the only reason either is exported.
var ErrUpgradeRefused = errors.New("engine attempted a protocol upgrade; proxying does not support upgrades")

// RefuseUpgrade is an httputil.ReverseProxy ModifyResponse hook: it turns an
// engine's 101 Switching Protocols response into ErrUpgradeRefused, which the
// proxy's ErrorHandler then turns into a 502. Pairing this with a Rewrite
// hook that deletes the inbound Upgrade and Connection headers (ReverseProxy
// re-adds them after stripping hop-by-hop headers otherwise) means an engine
// only reaches this path by upgrading unsolicited. Either way the response
// must never reach the caller: an upgraded connection is a raw byte tunnel
// that a path allowlist can no longer see.
func RefuseUpgrade(resp *http.Response) error {
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return ErrUpgradeRefused
	}
	return nil
}

const (
	// metricsPath is the Prometheus scrape path the chart's inference
	// PodMonitor requests through the relay.
	metricsPath = "/metrics"
	// emptyScrapeContentType is the Prometheus text exposition format an
	// empty scrape is served as.
	emptyScrapeContentType = "text/plain; version=0.0.4"
	// maxDrainedMetrics404 bounds how much of an engine's 404 body is read
	// before the connection is closed instead of reused.
	maxDrainedMetrics404 = 64 << 10
)

// answerMissingMetrics replaces an engine's 404 for GET /metrics with an
// empty 200 scrape. Some engines (TensorFold) have no metrics endpoint, and
// a 404 would make a PodMonitor scraping the relay pod mark the service
// down. Any other status, method or path is left untouched, so a real
// metrics body passes through. resp.Request is the outbound request, whose
// escaped path is the inbound one Allowed judged.
func answerMissingMetrics(resp *http.Response) {
	if resp.StatusCode != http.StatusNotFound || resp.Request == nil ||
		resp.Request.Method != http.MethodGet || resp.Request.URL.EscapedPath() != metricsPath {
		return
	}
	if resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainedMetrics404))
		_ = resp.Body.Close()
	}
	resp.StatusCode = http.StatusOK
	resp.Status = "200 OK"
	resp.Header = http.Header{"Content-Type": []string{emptyScrapeContentType}}
	resp.Trailer = nil
	resp.ContentLength = 0
	resp.TransferEncoding = nil
	resp.Body = http.NoBody
}

// proxy builds the reverse proxy to the engine on 127.0.0.1:port. SetURL
// joins the outbound path from the inbound URL's Path and RawPath, so the
// engine receives exactly the escaped path that Allowed judged. Rewrite
// also drops inbound X-Forwarded-* headers and sets Host to the loopback
// target.
//
// Protocol upgrades are refused: ReverseProxy re-adds Upgrade and
// "Connection: Upgrade" after stripping hop-by-hop headers, so Rewrite
// deletes both, and ModifyResponse turns any 101 into a 502. An upgraded
// connection would be a raw byte tunnel that the path allowlist no longer
// sees. Client-declared request trailers are dropped for the same reason:
// they are headers that arrive after the checks ran.
//
// An engine's 404 for GET /metrics becomes an empty 200 scrape (see
// answerMissingMetrics).
func (s *Server) proxy(port int) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Header.Del("Upgrade")
			pr.Out.Header.Del("Connection")
			pr.Out.Trailer = nil
		},
		ModifyResponse: func(resp *http.Response) error {
			if err := RefuseUpgrade(resp); err != nil {
				return err
			}
			answerMissingMetrics(resp)
			return nil
		},
		Transport: s.transport,
		// Flush each chunk immediately so SSE / stream:true completions
		// are not buffered by the proxy.
		FlushInterval: -1,
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, err error) {
			s.logger.Warnw("ingress upstream error", "target", target.Host, "err", err.Error())
			writeJSONError(rw, http.StatusBadGateway, "upstream inference process unreachable")
		},
	}
}

// Start serves the ingress over TLS 1.3 on addr until ctx is cancelled.
// There is no read or write timeout (streams can run for minutes); only
// request headers are bounded.
func (s *Server) Start(ctx context.Context, addr string) error {
	ln, err := s.Listen(addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Listen binds addr without serving yet. Connections to the listener queue
// until Serve runs, so a caller can advertise the port once it is bound.
func (s *Server) Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.addr = ln.Addr()
	s.mu.Unlock()
	return ln, nil
}

// Serve serves TLS on ln, from Listen, until ctx is done.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       idleTimeout,
		ErrorLog:          zap.NewStdLog(s.logger.Desugar()),
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			// Read per handshake: BindMetalNode re-issues the certificate
			// while the ingress serves.
			GetCertificate: s.identity.GetCertificate,
		},
	}
	stopped := make(chan struct{})
	defer close(stopped)
	//nolint:gosec // G118: the parent ctx is already cancelled here; graceful
	// shutdown needs a fresh, bounded context, so context.Background is correct.
	go func() {
		select {
		case <-ctx.Done():
		case <-stopped:
			return
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			_ = srv.Close()
		}
	}()
	s.logger.Infow("ingress listening", "addr", ln.Addr().String())
	if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// parseTarget splits "<ns>/<name>"; both parts must be non-empty and name
// must not contain another slash.
func parseTarget(v string) (ns, name string, ok bool) {
	ns, name, ok = strings.Cut(v, "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return ns, name, true
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
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

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
