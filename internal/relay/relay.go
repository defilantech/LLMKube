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

// Package relay implements the in-cluster half of the Metal S4 transport: a
// reverse proxy that forwards to the metal-agent's TLS ingress, pins the
// ingress certificate by SPKI, and presents the per-namespace relay token.
package relay

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	apiv1 "github.com/defilantech/llmkube/api/v1alpha1"
	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

var (
	// requestsTotal counts every request this relay served, by the status
	// code returned to the caller (including the relay's own 502/503
	// failures, not just codes the ingress produced).
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmkube_relay_requests_total",
			Help: "Total number of relay requests, by response status code.",
		},
		[]string{"code"},
	)

	// pinMismatchTotal counts TLS handshakes to the ingress rejected because
	// the presented certificate's SPKI did not match the pinned value. A
	// nonzero rate means either the agent rotated its ingress certificate
	// without updating the pinned annotation, or something is attempting to
	// intercept the connection.
	pinMismatchTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "llmkube_relay_pin_mismatch_total",
			Help: "Total number of ingress TLS handshakes rejected for an SPKI pin mismatch.",
		},
	)
)

func init() {
	prommetrics.RouterRegistry.MustRegister(requestsTotal, pinMismatchTotal)
}

// Config configures a Relay.
type Config struct {
	// Target is the InferenceService this relay serves, as "<ns>/<name>".
	Target string
	// Upstream is the metal-agent ingress this relay forwards to, e.g.
	// https://<isvc>-agent.<ns>.svc:8443.
	Upstream string
	// SPKIPin is the base64 (std) SHA-256 of the ingress certificate's
	// SubjectPublicKeyInfo. Only a server certificate matching this pin is
	// accepted; there is no CA trust or hostname check.
	SPKIPin string
	// TokenFile is the path to the per-namespace relay token (a projected
	// Secret volume). Re-read whenever its mtime changes, so a rotated
	// token is picked up without a restart.
	TokenFile string
}

// Relay is a reverse proxy that forwards to a metal-agent's TLS ingress,
// pinning its certificate by SPKI and authenticating with a bearer token
// read from TokenFile.
type Relay struct {
	cfg    Config
	pin    []byte
	up     *url.URL
	proxy  *httputil.ReverseProxy
	client *http.Client
	logger *slog.Logger

	mu       sync.Mutex
	token    string
	tokenMod time.Time
}

// PinFromCert computes the base64 (std) SHA-256 SPKI pin of cert, in the same
// form the metal-agent writes to AnnotationAgentIngressSPKI.
func PinFromCert(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// New validates cfg and constructs a Relay. Target must be "<namespace>/<name>",
// Upstream must be an https:// URL, SPKIPin must decode to a 32-byte SHA-256
// digest, and TokenFile must be non-empty.
func New(cfg Config, logger *slog.Logger) (*Relay, error) {
	ns, name, ok := strings.Cut(cfg.Target, "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("relay target %q must be <namespace>/<name>", cfg.Target)
	}
	up, err := url.Parse(cfg.Upstream)
	if err != nil || up.Scheme != "https" || up.Host == "" {
		return nil, fmt.Errorf("relay upstream %q must be an https URL", cfg.Upstream)
	}
	pin, err := base64.StdEncoding.DecodeString(cfg.SPKIPin)
	if err != nil || len(pin) != sha256.Size {
		return nil, fmt.Errorf("relay SPKI pin must be base64 of a SHA-256 digest")
	}
	if cfg.TokenFile == "" {
		return nil, errors.New("relay token file is required")
	}

	r := &Relay{cfg: cfg, pin: pin, up: up, logger: logger}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			// The pin is the identity: no CA and no hostname check, but
			// VerifyConnection refuses any leaf whose SPKI does not match.
			InsecureSkipVerify: true, //nolint:gosec // G402: replaced by SPKI pinning in VerifyConnection
			VerifyConnection:   r.verifyPin,
		},
		// Bound connection setup so an unreachable Mac fails fast; the
		// response header wait stays unbounded for long first tokens.
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: 0,
		IdleConnTimeout:       90 * time.Second,
	}
	r.client = &http.Client{Transport: tr, Timeout: 5 * time.Second}
	r.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.Out.Host = up.Host
			// Appends this connection's peer to the inbound X-Forwarded-For
			// chain, which ReverseProxy strips from Out before Rewrite. With
			// externalTrafficPolicy Local that peer is the real client; only
			// the rightmost entry is the relay's own observation.
			pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"]
			pr.SetXForwarded()
			pr.Out.Header.Del(apiv1.HeaderRelayToken)
			pr.Out.Header.Del(apiv1.HeaderRelayTarget)
			pr.Out.Header.Set(apiv1.HeaderRelayTarget, cfg.Target)
			pr.Out.Header.Set(apiv1.HeaderRelayToken, tokenFromContext(pr.In.Context()))
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			r.logger.Warn("relay upstream error", "target", cfg.Target, "error", err)
			writeJSONError(w, http.StatusBadGateway, "metal agent ingress unreachable or not trusted")
		},
	}
	return r, nil
}

// verifyPin is the tls.Config.VerifyConnection callback. It is invoked even
// though the transport also sets InsecureSkipVerify: Go calls
// VerifyConnection regardless of InsecureSkipVerify, and this is where trust
// actually comes from for this connection.
func (r *Relay) verifyPin(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("ingress presented no certificate")
	}
	sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
	if subtle.ConstantTimeCompare(sum[:], r.pin) != 1 {
		pinMismatchTotal.Inc()
		return errors.New("ingress certificate does not match the pinned SPKI")
	}
	return nil
}

// currentToken re-reads TokenFile when its mtime changes, so a rotated
// Secret (kubelet updates the projected file) is picked up without a
// restart.
func (r *Relay) currentToken() (string, error) {
	st, err := os.Stat(r.cfg.TokenFile)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token != "" && st.ModTime().Equal(r.tokenMod) {
		return r.token, nil
	}
	b, err := os.ReadFile(r.cfg.TokenFile)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("relay token file is empty")
	}
	r.token, r.tokenMod = tok, st.ModTime()
	return tok, nil
}

// ServeHTTP forwards req to the pinned ingress with the current relay token
// and the configured target header, streaming the response back without
// buffering.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	tok, err := r.currentToken()
	if err != nil {
		r.logger.Warn("relay token unavailable", "error", err)
		writeJSONError(sw, http.StatusServiceUnavailable, "relay token unavailable")
		requestsTotal.WithLabelValues(strconv.Itoa(sw.status)).Inc()
		return
	}
	r.proxy.ServeHTTP(sw, req.WithContext(withToken(req.Context(), tok)))
	requestsTotal.WithLabelValues(strconv.Itoa(sw.status)).Inc()
}

// Ready reports whether the ingress considers Target running, by GETting
// IngressReadyPath with the current token and target. It returns nil only on
// a 200 response.
func (r *Relay) Ready(ctx context.Context) error {
	tok, err := r.currentToken()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.up.JoinPath(apiv1.IngressReadyPath).String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set(apiv1.HeaderRelayToken, tok)
	req.Header.Set(apiv1.HeaderRelayTarget, r.cfg.Target)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ingress readiness returned %d", resp.StatusCode)
	}
	return nil
}

// statusWriter records the status code written to an http.ResponseWriter so
// ServeHTTP can label the requestsTotal metric, while passing Flush through
// so httputil.ReverseProxy's streaming path (FlushInterval: -1) still works.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// contextKey is unexported so no other package can collide with it.
type contextKey int

const tokenContextKey contextKey = iota

func withToken(ctx context.Context, tok string) context.Context {
	return context.WithValue(ctx, tokenContextKey, tok)
}

func tokenFromContext(ctx context.Context) string {
	tok, _ := ctx.Value(tokenContextKey).(string)
	return tok
}

// writeJSONError writes a JSON error body {"error": msg} with the given
// status code.
func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
