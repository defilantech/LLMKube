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
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/relay"
)

// fakeRouter serves the namespaces in ns and the targets in routes
// ("ns/name" -> Route).
type fakeRouter struct {
	mu     sync.Mutex
	ns     map[string]bool
	routes map[string]Route
}

func (f *fakeRouter) Route(namespace, name string) (Route, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.routes[namespace+"/"+name]
	return r, ok
}

func (f *fakeRouter) ServesNamespace(namespace string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ns[namespace]
}

func (f *fakeRouter) set(target string, r Route) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[target] = r
}

// engineRecorder is a loopback httptest engine that records what reached it.
type engineRecorder struct {
	srv   *httptest.Server
	port  int
	calls atomic.Int32

	mu   sync.Mutex
	last *http.Request
}

func newEngine(t *testing.T, h http.HandlerFunc) *engineRecorder {
	t.Helper()
	e := &engineRecorder{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.calls.Add(1)
		e.mu.Lock()
		e.last = r.Clone(context.Background())
		e.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(e.srv.Close)
	u, err := url.Parse(e.srv.URL)
	if err != nil {
		t.Fatalf("parse engine URL: %v", err)
	}
	if u.Hostname() != "127.0.0.1" {
		t.Fatalf("engine listens on %s, want 127.0.0.1", u.Hostname())
	}
	e.port, err = strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("engine port: %v", err)
	}
	return e
}

func (e *engineRecorder) lastRequest() *http.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.last
}

type fixture struct {
	srv     *Server
	secrets *fakeSecrets
	router  *fakeRouter
	engine  *engineRecorder
}

// newFixture serves namespaces "a" and "b" (tokens tokA and tokB), with
// ready llamacpp targets "a/qwen" and "b/qwen" on the engine, and namespace
// "c" present in the Secret store but NOT served by the router.
func newFixture(t *testing.T, h http.HandlerFunc) *fixture {
	t.Helper()
	e := newEngine(t, h)
	f := newFakeSecrets()
	f.set("a", tokA)
	f.set("b", tokB)
	f.set("c", tokA)
	r := &fakeRouter{
		ns: map[string]bool{"a": true, "b": true},
		routes: map[string]Route{
			"a/qwen": {Runtime: "llamacpp", Port: e.port, Ready: true},
			"b/qwen": {Runtime: "llamacpp", Port: e.port, Ready: true},
		},
	}
	ts := NewTokenStore(f, time.Minute, time.Minute, nil)
	return &fixture{
		srv:     NewServer(nil, ts, r, zap.NewNop().Sugar()),
		secrets: f,
		router:  r,
		engine:  e,
	}
}

func newReq(method, target, token, rawPath string) *http.Request {
	req := httptest.NewRequest(method, "http://ingress.local"+rawPath, nil)
	if target != "" {
		req.Header.Set(inferencev1alpha1.HeaderRelayTarget, target)
	}
	if token != "" {
		req.Header.Set(inferencev1alpha1.HeaderRelayToken, token)
	}
	return req
}

func (fx *fixture) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	fx.srv.ServeHTTP(rec, req)
	return rec
}

func assertJSONError(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
		t.Fatalf("body %q is not a JSON error: %v", rec.Body.String(), err)
	}
}

func TestServer_TargetHeaderMustParse(t *testing.T) {
	fx := newFixture(t, nil)
	for _, target := range []string{"", "a", "a/", "/qwen", "a/qwen/x", "/"} {
		t.Run(strconv.Quote(target), func(t *testing.T) {
			rec := fx.do(newReq(http.MethodGet, target, tokA, "/v1/models"))
			assertJSONError(t, rec, http.StatusBadRequest)
		})
	}
	if n := fx.engine.calls.Load(); n != 0 {
		t.Fatalf("engine called %d times on malformed targets", n)
	}
	if n := fx.secrets.total(); n != 0 {
		t.Fatalf("token store read %d Secrets on malformed targets", n)
	}
}

func TestServer_BadTokenIs401(t *testing.T) {
	fx := newFixture(t, nil)
	for _, tok := range []string{"", "nope", tokB} {
		rec := fx.do(newReq(http.MethodGet, "a/qwen", tok, "/v1/models"))
		assertJSONError(t, rec, http.StatusUnauthorized)
	}
	if n := fx.engine.calls.Load(); n != 0 {
		t.Fatalf("engine called %d times with a bad token", n)
	}
}

// A token that is valid for namespace b must not open namespace a.
func TestServer_OtherNamespaceTokenIs401(t *testing.T) {
	fx := newFixture(t, nil)
	rec := fx.do(newReq(http.MethodGet, "a/qwen", tokB, "/v1/models"))
	assertJSONError(t, rec, http.StatusUnauthorized)
	rec = fx.do(newReq(http.MethodGet, "b/qwen", tokA, "/v1/models"))
	assertJSONError(t, rec, http.StatusUnauthorized)
	if n := fx.engine.calls.Load(); n != 0 {
		t.Fatalf("engine called %d times with a cross-namespace token", n)
	}
}

// An unserved namespace is refused before the token store is consulted
// (no Secret read, no cache entry), and the refusal is byte-identical to a
// bad token so it does not reveal which namespaces this agent serves.
func TestServer_UnservedNamespaceIs401WithoutTokenLookup(t *testing.T) {
	fx := newFixture(t, nil)
	// "c" has a real Secret holding tokA; only the router gate stops it.
	unserved := fx.do(newReq(http.MethodGet, "c/qwen", tokA, "/v1/models"))
	assertJSONError(t, unserved, http.StatusUnauthorized)
	if n := fx.secrets.count("c"); n != 0 {
		t.Fatalf("token store read namespace c's Secret %d times; it must never be called for an unserved namespace", n)
	}
	// Also an invalid-but-parsable namespace never reaches the store.
	_ = fx.do(newReq(http.MethodGet, "zz/qwen", tokA, "/v1/models"))
	if n := fx.secrets.total(); n != 0 {
		t.Fatalf("token store read %d Secrets for unserved namespaces", n)
	}

	badToken := fx.do(newReq(http.MethodGet, "a/qwen", "nope", "/v1/models"))
	assertJSONError(t, badToken, http.StatusUnauthorized)
	if unserved.Body.String() != badToken.Body.String() {
		t.Fatalf("unserved body %q differs from bad-token body %q", unserved.Body.String(), badToken.Body.String())
	}
	if len(unserved.Header()) != len(badToken.Header()) {
		t.Fatalf("unserved headers %v differ from bad-token headers %v", unserved.Header(), badToken.Header())
	}
	for k := range badToken.Header() {
		if unserved.Header().Get(k) != badToken.Header().Get(k) {
			t.Fatalf("header %s: unserved %q, bad token %q", k, unserved.Header().Get(k), badToken.Header().Get(k))
		}
	}
	if n := fx.engine.calls.Load(); n != 0 {
		t.Fatalf("engine called %d times", n)
	}
}

func TestServer_UnknownTargetIs404(t *testing.T) {
	fx := newFixture(t, nil)
	rec := fx.do(newReq(http.MethodGet, "a/missing", tokA, "/v1/models"))
	assertJSONError(t, rec, http.StatusNotFound)
	if n := fx.engine.calls.Load(); n != 0 {
		t.Fatalf("engine called %d times", n)
	}
}

func TestServer_NotReadyIs503(t *testing.T) {
	fx := newFixture(t, nil)
	fx.router.set("a/qwen", Route{Runtime: "llamacpp", Port: fx.engine.port, Ready: false})
	rec := fx.do(newReq(http.MethodGet, "a/qwen", tokA, "/v1/models"))
	assertJSONError(t, rec, http.StatusServiceUnavailable)
	if n := fx.engine.calls.Load(); n != 0 {
		t.Fatalf("engine called %d times for a not-ready target", n)
	}
}

func TestServer_BlockedPathIs403(t *testing.T) {
	fx := newFixture(t, nil)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/slots"},
		{http.MethodPost, "/props"},
		{http.MethodGet, "/v1/models/a%2Fb"},
		{http.MethodGet, "/v1/chat/../models"},
		{http.MethodGet, "//v1/models"},
		{http.MethodGet, "/v1/models/"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			rec := fx.do(newReq(c.method, "a/qwen", tokA, c.path))
			assertJSONError(t, rec, http.StatusForbidden)
		})
	}
	if n := fx.engine.calls.Load(); n != 0 {
		t.Fatalf("engine called %d times for blocked paths", n)
	}
}

func TestServer_ForwardsAllowedAndStripsRelayHeaders(t *testing.T) {
	fx := newFixture(t, nil)
	req := newReq(http.MethodPost, "a/qwen", tokA, "/v1/chat/completions?x=1")
	req.Header.Set("Authorization", "Bearer client-key")
	rec := fx.do(req)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("status %d body %q, want 200 from the engine", rec.Code, rec.Body.String())
	}
	got := fx.engine.lastRequest()
	if got == nil {
		t.Fatal("engine was not called")
	}
	for _, h := range []string{inferencev1alpha1.HeaderRelayToken, inferencev1alpha1.HeaderRelayTarget} {
		if v, ok := got.Header[http.CanonicalHeaderKey(h)]; ok {
			t.Fatalf("engine saw relay header %s = %q", h, v)
		}
	}
	if a := got.Header.Get("Authorization"); a != "Bearer client-key" {
		t.Fatalf("engine Authorization = %q, want the client's", a)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v1/chat/completions" || got.URL.RawQuery != "x=1" {
		t.Fatalf("engine saw %s %s?%s", got.Method, got.URL.Path, got.URL.RawQuery)
	}
}

// The engine must receive exactly the escaped path that Allowed judged, not
// a decoded-and-re-encoded variant.
func TestServer_UpstreamPathIsClientEscapedPath(t *testing.T) {
	fx := newFixture(t, nil)
	const p = "/v1/models/abc%2Edef"
	if !Allowed("llamacpp", http.MethodGet, p) {
		t.Fatalf("precondition: %s must be allowed", p)
	}
	rec := fx.do(newReq(http.MethodGet, "a/qwen", tokA, p))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	got := fx.engine.lastRequest()
	if got == nil {
		t.Fatal("engine was not called")
	}
	if ep := got.URL.EscapedPath(); ep != p {
		t.Fatalf("engine EscapedPath = %q, want %q", ep, p)
	}
	if got.RequestURI != p {
		t.Fatalf("engine RequestURI = %q, want %q", got.RequestURI, p)
	}
}

func TestServer_ReadyPathAnsweredLocally(t *testing.T) {
	fx := newFixture(t, nil)
	rec := fx.do(newReq(http.MethodGet, "a/qwen", tokA, inferencev1alpha1.IngressReadyPath))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready (Ready=true) status = %d, want 200", rec.Code)
	}
	fx.router.set("a/qwen", Route{Runtime: "llamacpp", Port: fx.engine.port, Ready: false})
	rec = fx.do(newReq(http.MethodGet, "a/qwen", tokA, inferencev1alpha1.IngressReadyPath))
	assertJSONError(t, rec, http.StatusServiceUnavailable)
	// Still authenticated: a bad token cannot probe readiness.
	rec = fx.do(newReq(http.MethodGet, "a/qwen", "nope", inferencev1alpha1.IngressReadyPath))
	assertJSONError(t, rec, http.StatusUnauthorized)
	// Unknown target: 404, not a readiness answer.
	rec = fx.do(newReq(http.MethodGet, "a/missing", tokA, inferencev1alpha1.IngressReadyPath))
	assertJSONError(t, rec, http.StatusNotFound)
	if n := fx.engine.calls.Load(); n != 0 {
		t.Fatalf("engine called %d times; the ready path must never be proxied", n)
	}
}

// TestServer_StreamsChunks proves each engine chunk reaches the client as it
// is written, for both SSE and NDJSON (Ollama's streaming format), served
// behind a real net/http server whose buffered writer only reaches the
// socket when flushed.
func TestServer_StreamsChunks(t *testing.T) {
	for _, ct := range []string{"text/event-stream", "application/x-ndjson"} {
		t.Run(ct, func(t *testing.T) { testStreamsChunks(t, ct) })
	}
}

func testStreamsChunks(t *testing.T, contentType string) {
	release := make(chan struct{})
	var once sync.Once
	closeRelease := func() { once.Do(func() { close(release) }) }

	fx := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("engine ResponseWriter does not support flushing")
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: 1\n\n"))
		flusher.Flush()
		<-release
		_, _ = w.Write([]byte("data: 2\n\n"))
		flusher.Flush()
	})
	front := httptest.NewServer(fx.srv)
	// LIFO: release the engine handler before the servers wait on it.
	t.Cleanup(front.Close)
	t.Cleanup(closeRelease)

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set(inferencev1alpha1.HeaderRelayTarget, "a/qwen")
	req.Header.Set(inferencev1alpha1.HeaderRelayToken, tokA)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	br := bufio.NewReader(resp.Body)
	lineCh := make(chan string, 1)
	go func() {
		line, _ := br.ReadString('\n')
		lineCh <- line
	}()
	select {
	case line := <-lineCh:
		if line != "data: 1\n" {
			t.Fatalf("first line = %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first SSE event did not arrive before the engine finished; the ingress is buffering")
	}
	closeRelease()
	// Report the read error too: a stream the ingress aborted mid-copy
	// shows up as a short read plus an unexpected EOF, not just a short read.
	rest, err := io.ReadAll(br)
	if err != nil || !strings.Contains(string(rest), "data: 2") {
		t.Fatalf("second event missing or stream aborted: got %q, err %v", rest, err)
	}
}

// stallBodyConn is an engine connection that holds the Transport's writing
// goroutine just after it puts the request body on the wire. The Transport
// copies a known-length body straight to the socket and only then reads the
// inbound body once more to confirm EOF; stalling here widens that window
// so the engine can answer, and the ingress flush its first chunk, inside it.
type stallBodyConn struct {
	net.Conn
	body   []byte
	resume <-chan struct{}
	closed chan<- struct{}
	once   sync.Once
}

func (c *stallBodyConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if string(p) == string(c.body) {
		<-c.resume
	}
	return n, err
}

func (c *stallBodyConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// TestServer_StreamSurvivesLateRequestBodyEOFCheck pins the ingress to
// full-duplex proxying. Without it, the HTTP/1 server closes the inbound
// request body when the ingress writes its first response chunk; if the
// Transport has not yet made its final EOF read of that same body, the read
// fails with "invalid Read on closed Body" and the Transport closes the
// engine connection, cutting the stream after the first chunk.
func TestServer_StreamSurvivesLateRequestBodyEOFCheck(t *testing.T) {
	const reqBody = `{"stream":true}`
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	resume := make(chan struct{})
	var resumeOnce sync.Once
	closeResume := func() { resumeOnce.Do(func() { close(resume) }) }
	engineConnClosed := make(chan struct{})

	fx := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: 1\n\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("data: 2\n\n"))
	})

	tr := &http.Transport{}
	var dialer net.Dialer
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &stallBodyConn{Conn: c, body: []byte(reqBody), resume: resume, closed: engineConnClosed}, nil
	}
	t.Cleanup(tr.CloseIdleConnections)
	fx.srv.transport = tr

	front := httptest.NewServer(fx.srv)
	// LIFO: unblock the Transport writer and the engine handler before the
	// servers wait on them.
	t.Cleanup(front.Close)
	t.Cleanup(closeRelease)
	t.Cleanup(closeResume)

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set(inferencev1alpha1.HeaderRelayTarget, "a/qwen")
	req.Header.Set(inferencev1alpha1.HeaderRelayToken, tokA)
	client := front.Client()
	client.Timeout = 5 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); line != "data: 1\n" {
		t.Fatalf("first line = %q (err %v)", line, err)
	}
	// The ingress has flushed the first chunk, so the server has already
	// finished with the inbound body. Let the Transport make its EOF read.
	closeResume()
	select {
	case <-engineConnClosed:
		t.Fatal("the ingress closed the engine connection mid-stream after its final request-body read")
	case <-time.After(200 * time.Millisecond):
	}
	closeRelease()
	rest, err := io.ReadAll(br)
	if err != nil || !strings.Contains(string(rest), "data: 2") {
		t.Fatalf("stream tail = %q, err %v; want the second event and a clean EOF", rest, err)
	}
}

func TestServer_ClientCancelReachesEngine(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	fx := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(10 * time.Second):
		}
	})
	front := httptest.NewServer(fx.srv)
	t.Cleanup(front.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set(inferencev1alpha1.HeaderRelayTarget, "a/qwen")
	req.Header.Set(inferencev1alpha1.HeaderRelayToken, tokA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("engine never started")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("engine did not observe the client's cancellation within 2s")
	}
}

func TestServer_CountsRequestsByCode(t *testing.T) {
	fx := newFixture(t, nil)
	before401 := testutil.ToFloat64(RequestsTotal.WithLabelValues("401"))
	before200 := testutil.ToFloat64(RequestsTotal.WithLabelValues("200"))
	before403 := testutil.ToFloat64(RequestsTotal.WithLabelValues("403"))
	fx.do(newReq(http.MethodGet, "a/qwen", "nope", "/v1/models"))
	fx.do(newReq(http.MethodGet, "c/qwen", tokA, "/v1/models"))
	fx.do(newReq(http.MethodGet, "a/qwen", tokA, "/v1/models"))
	fx.do(newReq(http.MethodGet, "a/qwen", tokA, "/slots"))
	if d := testutil.ToFloat64(RequestsTotal.WithLabelValues("401")) - before401; d != 2 {
		t.Fatalf("401 count delta = %v, want 2", d)
	}
	if d := testutil.ToFloat64(RequestsTotal.WithLabelValues("200")) - before200; d != 1 {
		t.Fatalf("200 count delta = %v, want 1", d)
	}
	if d := testutil.ToFloat64(RequestsTotal.WithLabelValues("403")) - before403; d != 1 {
		t.Fatalf("403 count delta = %v, want 1", d)
	}
}

// Start serves the persisted identity over TLS 1.3; a client pinned with
// relay.PinFromCert to LoadOrCreateIdentity's pin completes the handshake
// and reaches the engine, and ctx cancellation stops Start cleanly.
func TestServer_StartServesPinnedTLS(t *testing.T) {
	id, err := LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	pin := id.Pin()
	fx := newFixture(t, nil)
	srv := NewServer(id, fx.srv.tokens, fx.router, zap.NewNop().Sugar())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx, "127.0.0.1:0") }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Start returned %v after cancel, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Start did not return after ctx cancel")
		}
	})
	addr := waitAddr(t, srv)

	var seenPin atomic.Value
	pinned := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // G402: trust comes from the SPKI pin below
			VerifyConnection: func(cs tls.ConnectionState) error {
				got := relay.PinFromCert(cs.PeerCertificates[0])
				seenPin.Store(got)
				if got != pin {
					return errors.New("pin mismatch")
				}
				if cs.Version != tls.VersionTLS13 {
					return errors.New("not TLS 1.3")
				}
				return nil
			},
		}},
	}

	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/v1/models", nil)
	req.Header.Set(inferencev1alpha1.HeaderRelayTarget, "a/qwen")
	req.Header.Set(inferencev1alpha1.HeaderRelayToken, tokA)
	resp, err := pinned.Do(req)
	if err != nil {
		t.Fatalf("pinned GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if got, _ := seenPin.Load().(string); got != pin {
		t.Fatalf("handshake pin %q, want %q", got, pin)
	}

	// Binding to a MetalNode while serving: the next handshake presents the
	// re-issued certificate, with the same pin, without a restart.
	if _, err := id.BindMetalNode("6f1c2e3d-4a5b-4c6d-8e7f-90a1b2c3d4e5"); err != nil {
		t.Fatalf("BindMetalNode: %v", err)
	}
	bound, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // G402: reading the presented certificate
		MinVersion:         tls.VersionTLS13,
	})
	if err != nil {
		t.Fatalf("TLS after binding: %v", err)
	}
	leaf := bound.ConnectionState().PeerCertificates[0]
	_ = bound.Close()
	want := "6f1c2e3d-4a5b-4c6d-8e7f-90a1b2c3d4e5.metal-node.llmkube.invalid"
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != want || relay.PinFromCert(leaf) != pin {
		t.Errorf("after binding the ingress presents names %v pin %q, want %s and the unchanged pin", leaf.DNSNames,
			relay.PinFromCert(leaf), want)
	}

	// A TLS 1.2-only client cannot complete a handshake at all.
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // G402: probing the version floor only
		MaxVersion:         tls.VersionTLS12,
	})
	if err == nil {
		_ = conn.Close()
		t.Fatal("TLS 1.2 client completed a handshake; the ingress must require TLS 1.3")
	}
}

func waitAddr(t *testing.T, s *Server) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a := s.listenAddr(); a != nil {
			if _, _, err := net.SplitHostPort(a.String()); err == nil {
				return a.String()
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not start listening")
	return ""
}

// The counter follows the agent's llmkube_metal_agent_* naming.
func TestRequestsTotalName(t *testing.T) {
	ch := make(chan *prometheus.Desc, 1)
	RequestsTotal.Describe(ch)
	if d := (<-ch).String(); !strings.Contains(d, `fqName: "llmkube_metal_agent_ingress_requests_total"`) {
		t.Errorf("RequestsTotal desc = %s, want fqName llmkube_metal_agent_ingress_requests_total", d)
	}
}

// upgradingEngine answers any request carrying an Upgrade header with a raw
// 101, as an engine exposing a WebSocket or h2c endpoint would.
func upgradingEngine(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") == "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	_ = buf.Flush()
}

// A protocol upgrade is never proxied: the engine sees neither the Upgrade
// header nor a Connection: upgrade token, and no 101 reaches the client.
func TestServer_RefusesProtocolUpgrade(t *testing.T) {
	fx := newFixture(t, upgradingEngine)
	front := httptest.NewServer(fx.srv)
	t.Cleanup(front.Close)

	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	raw := "GET /v1/models HTTP/1.1\r\nHost: ingress.local\r\n" +
		inferencev1alpha1.HeaderRelayTarget + ": a/qwen\r\n" +
		inferencev1alpha1.HeaderRelayToken + ": " + tokA + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n\r\n"
	if _, err := conn.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("ingress passed a 101 Switching Protocols to the client")
	}
	last := fx.engine.lastRequest()
	if last == nil {
		t.Fatal("engine was not called")
	}
	if up := last.Header.Get("Upgrade"); up != "" {
		t.Errorf("engine saw Upgrade: %q, want none", up)
	}
	if c := last.Header.Get("Connection"); strings.Contains(strings.ToLower(c), "upgrade") {
		t.Errorf("engine saw Connection: %q, want no upgrade token", c)
	}
}

// Even if an engine answers 101 without being asked, the ingress refuses it.
func TestServer_RefusesUnsolicited101(t *testing.T) {
	fx := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
	})
	rec := fx.do(newReq(http.MethodGet, "a/qwen", tokA, "/v1/models"))
	assertJSONError(t, rec, http.StatusBadGateway)
}

// Client-declared request trailers are dropped before proxying.
func TestServer_DropsRequestTrailers(t *testing.T) {
	var gotTrailer atomic.Value
	fx := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		gotTrailer.Store(r.Trailer.Clone())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	req := newReq(http.MethodPost, "a/qwen", tokA, "/v1/chat/completions")
	req.Body = io.NopCloser(strings.NewReader(`{"messages":[]}`))
	req.ContentLength = -1
	req.Trailer = http.Header{"X-Injected": {"1"}}
	rec := fx.do(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	tr, _ := gotTrailer.Load().(http.Header)
	if len(tr) != 0 {
		t.Errorf("engine saw request trailers %v, want none", tr)
	}
}

// metricsEngine answers /metrics with status and body, and 404 with a JSON
// body everywhere else, like an engine without a metrics endpoint.
func metricsEngine(metricsStatus int, metricsBody string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			w.Header().Set("X-Engine", "yes")
			w.WriteHeader(metricsStatus)
			_, _ = io.WriteString(w, metricsBody)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
	}
}

// An engine without /metrics (TensorFold) must scrape as an empty 200, not a
// 404 that marks the PodMonitor target down.
func TestServer_MissingEngineMetricsScrapesEmpty(t *testing.T) {
	fx := newFixture(t, metricsEngine(http.StatusNotFound, "404 page not found\n"))
	rec := fx.do(newReq(http.MethodGet, "a/qwen", tokA, "/metrics"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4" {
		t.Fatalf("Content-Type = %q, want the Prometheus text format", ct)
	}
	if v := rec.Header().Get("X-Engine"); v != "" {
		t.Fatalf("engine 404 header X-Engine = %q leaked into the replacement", v)
	}
	if n := fx.engine.calls.Load(); n != 1 {
		t.Fatalf("engine called %d times, want 1", n)
	}
}

func TestServer_EngineMetricsPassThrough(t *testing.T) {
	const body = "# TYPE llamacpp:requests_processing gauge\nllamacpp:requests_processing 0\n"
	fx := newFixture(t, metricsEngine(http.StatusOK, body))
	rec := fx.do(newReq(http.MethodGet, "a/qwen", tokA, "/metrics"))
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("status %d body %q, want the engine's 200 metrics body", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want the engine's", ct)
	}
	if v := rec.Header().Get("X-Engine"); v != "yes" {
		t.Fatalf("X-Engine = %q, want the engine's headers untouched", v)
	}
}

// Only GET /metrics answering 404 is rewritten: another path's 404, and a
// non-404 error on /metrics, reach the client unchanged.
func TestServer_OnlyMissingMetricsIsRewritten(t *testing.T) {
	t.Run("404 on another path", func(t *testing.T) {
		fx := newFixture(t, metricsEngine(http.StatusOK, ""))
		rec := fx.do(newReq(http.MethodGet, "a/qwen", tokA, "/v1/models"))
		if rec.Code != http.StatusNotFound || rec.Body.String() != `{"error":"not found"}` {
			t.Fatalf("status %d body %q, want the engine's 404", rec.Code, rec.Body.String())
		}
	})
	t.Run("500 on /metrics", func(t *testing.T) {
		fx := newFixture(t, metricsEngine(http.StatusInternalServerError, "boom"))
		rec := fx.do(newReq(http.MethodGet, "a/qwen", tokA, "/metrics"))
		if rec.Code != http.StatusInternalServerError || rec.Body.String() != "boom" {
			t.Fatalf("status %d body %q, want the engine's 500", rec.Code, rec.Body.String())
		}
	})
}

// Methods and paths the allowlist rejects before the proxy cannot reach
// answerMissingMetrics through ServeHTTP, so its match is checked directly.
func TestAnswerMissingMetrics_MatchesOnlyGetMetrics404(t *testing.T) {
	cases := []struct {
		method, path string
		status       int
		rewritten    bool
	}{
		{http.MethodGet, "/metrics", http.StatusNotFound, true},
		{http.MethodHead, "/metrics", http.StatusNotFound, false},
		{http.MethodPost, "/metrics", http.StatusNotFound, false},
		{http.MethodGet, "/metrics/", http.StatusNotFound, false},
		{http.MethodGet, "/v1/metrics", http.StatusNotFound, false},
		{http.MethodGet, "/metrics", http.StatusOK, false},
		{http.MethodGet, "/metrics", http.StatusServiceUnavailable, false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, "http://127.0.0.1:1"+tc.path, nil)
		resp := &http.Response{
			StatusCode: tc.status,
			Header:     http.Header{"X-Engine": []string{"yes"}},
			Body:       io.NopCloser(strings.NewReader("engine body")),
			Request:    req,
		}
		answerMissingMetrics(resp)
		if got := resp.StatusCode == http.StatusOK && resp.Header.Get("X-Engine") == ""; got != tc.rewritten {
			t.Errorf("%s %s %d: rewritten = %v, want %v", tc.method, tc.path, tc.status, got, tc.rewritten)
		}
	}
}
