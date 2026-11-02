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
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// testClientProxyPort is the loopback port every test in this file configures
// the ClientProxy with, so Host header expectations below (Host checks care
// about the exact port a proxy is told to listen on) have a fixed value to
// test against.
const testClientProxyPort = 9443

// fakeBackend is a test backendProvider standing in for the MetalAgent's
// view of the currently-running child process.
type fakeBackend struct {
	addr    string
	runtime string
	ok      bool
}

func (f *fakeBackend) currentBackend() (string, string, bool) { return f.addr, f.runtime, f.ok }

// newRecordingBackend starts a backend that answers 200 with a small JSON
// body and counts how many requests it actually received, so a test can
// assert the engine was never called when the proxy is expected to refuse a
// request itself.
func newRecordingBackend(t *testing.T) (addr string, hits *atomic.Int32) {
	t.Helper()
	hits = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), hits
}

func TestClientProxy_ForwardsToCurrentBackend(t *testing.T) {
	addr, hits := newRecordingBackend(t)
	p := NewClientProxy(&fakeBackend{addr: addr, runtime: runtimeLlamaServer, ok: true},
		testClientProxyPort, zap.NewNop().Sugar())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "127.0.0.1:9443"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"object":"list"`) {
		t.Errorf("response not forwarded from backend: %q", rec.Body.String())
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("backend hits: want 1 got %d", got)
	}
}

func TestClientProxy_503WhenNoBackend(t *testing.T) {
	p := NewClientProxy(&fakeBackend{ok: false}, testClientProxyPort, zap.NewNop().Sugar())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Host = "localhost:9443"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: want 503 got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("503 should be JSON, got Content-Type %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("503 body should carry a JSON error, got %q", rec.Body.String())
	}
}

// TestClientProxy_HostAndPathChecks is the table for #406's DNS-rebinding
// Host check and the ingress path allowlist, both applied to the host-side
// client proxy. Each case names how many times the backend should have been
// hit, so a check that lets a refused request slip through to the engine
// fails loudly rather than merely returning the wrong status.
func TestClientProxy_HostAndPathChecks(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		method     string
		path       string
		runtime    string
		port       int // the proxy's configured port; 0 means testClientProxyPort
		wantStatus int
		wantHits   int32
	}{
		{
			name:       "common route allowed",
			host:       "127.0.0.1:9443",
			method:     http.MethodGet,
			path:       "/v1/models",
			runtime:    runtimeLlamaServer,
			wantStatus: http.StatusOK,
			wantHits:   1,
		},
		{
			name:       "path outside every allowlist is forbidden",
			host:       "127.0.0.1:9443",
			method:     http.MethodGet,
			path:       "/slots",
			runtime:    runtimeLlamaServer,
			wantStatus: http.StatusForbidden,
			wantHits:   0,
		},
		{
			name:       "rebinding host is refused before the backend is asked",
			host:       "evil.example:9999",
			method:     http.MethodGet,
			path:       "/v1/models",
			runtime:    runtimeLlamaServer,
			wantStatus: http.StatusMisdirectedRequest,
			wantHits:   0,
		},
		{
			name:       "localhost with the proxy's port is accepted",
			host:       "localhost:9443",
			method:     http.MethodGet,
			path:       "/v1/models",
			runtime:    runtimeLlamaServer,
			wantStatus: http.StatusOK,
			wantHits:   1,
		},
		{
			name:       "127.0.0.1 with the proxy's port is accepted",
			host:       "127.0.0.1:9443",
			method:     http.MethodGet,
			path:       "/v1/models",
			runtime:    runtimeLlamaServer,
			wantStatus: http.StatusOK,
			wantHits:   1,
		},
		{
			name:       "bracketed IPv6 loopback with the proxy's port is accepted",
			host:       "[::1]:9443",
			method:     http.MethodGet,
			path:       "/v1/models",
			runtime:    runtimeLlamaServer,
			wantStatus: http.StatusOK,
			wantHits:   1,
		},
		{
			name:       "the right host with the wrong port is refused",
			host:       "127.0.0.1:9444",
			method:     http.MethodGet,
			path:       "/v1/models",
			runtime:    runtimeLlamaServer,
			wantStatus: http.StatusMisdirectedRequest,
			wantHits:   0,
		},
		{
			name:       "a Host with no port is refused when the proxy is not on port 80",
			host:       "localhost",
			method:     http.MethodGet,
			path:       "/v1/models",
			runtime:    runtimeLlamaServer,
			wantStatus: http.StatusMisdirectedRequest,
			wantHits:   0,
		},
		{
			name:       "a Host with no port is accepted when the proxy is on port 80",
			host:       "localhost",
			method:     http.MethodGet,
			path:       "/v1/models",
			runtime:    runtimeLlamaServer,
			port:       80,
			wantStatus: http.StatusOK,
			wantHits:   1,
		},
		{
			name:       "a bracketed IPv6 loopback with no port is accepted on port 80",
			host:       "[::1]",
			method:     http.MethodGet,
			path:       "/v1/models",
			runtime:    runtimeLlamaServer,
			port:       80,
			wantStatus: http.StatusOK,
			wantHits:   1,
		},
		{
			name:       "a foreign Host with no port is refused even on port 80",
			host:       "evil.example",
			method:     http.MethodGet,
			path:       "/v1/models",
			runtime:    runtimeLlamaServer,
			port:       80,
			wantStatus: http.StatusMisdirectedRequest,
			wantHits:   0,
		},
		{
			name:       "uppercase LOCALHOST with the proxy's port is accepted",
			host:       "LOCALHOST:9443",
			method:     http.MethodGet,
			path:       "/v1/models",
			runtime:    runtimeLlamaServer,
			wantStatus: http.StatusOK,
			wantHits:   1,
		},
		{
			name:       "runtime-specific route allowed for the backend's runtime",
			host:       "127.0.0.1:9443",
			method:     http.MethodGet,
			path:       "/props",
			runtime:    runtimeLlamaServer,
			wantStatus: http.StatusOK,
			wantHits:   1,
		},
		{
			name:       "the same route is forbidden for a different runtime",
			host:       "127.0.0.1:9443",
			method:     http.MethodGet,
			path:       "/props",
			runtime:    runtimeOllama,
			wantStatus: http.StatusForbidden,
			wantHits:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, hits := newRecordingBackend(t)
			port := tt.port
			if port == 0 {
				port = testClientProxyPort
			}
			p := NewClientProxy(&fakeBackend{addr: addr, runtime: tt.runtime, ok: true},
				port, zap.NewNop().Sugar())

			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: want %d got %d (body=%q)", tt.wantStatus, rec.Code, rec.Body.String())
			}
			if got := hits.Load(); got != tt.wantHits {
				t.Errorf("backend hits: want %d got %d", tt.wantHits, got)
			}
			if tt.wantStatus != http.StatusOK {
				if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
					t.Errorf("%d body should be JSON, got Content-Type %q", tt.wantStatus, ct)
				}
				if !strings.Contains(rec.Body.String(), "error") {
					t.Errorf("%d body should carry a JSON error, got %q", tt.wantStatus, rec.Body.String())
				}
			}
		})
	}
}

// TestClientProxy_RewritesHostToBackend guards against the legacy
// httputil.NewSingleHostReverseProxy Director, which never clears the
// outbound Host header: it would send the caller's loopback Host (e.g.
// "localhost:<client-proxy port>") to the child instead of the child's own
// address. ClientProxy.reverseProxy uses Rewrite+SetURL instead, mirroring
// pkg/agent/ingress/server.go's Server.proxy, and SetURL clears the outbound
// Host so Go's Transport derives it from the target URL.
func TestClientProxy_RewritesHostToBackend(t *testing.T) {
	var gotHost atomic.Value
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost.Store(r.Host)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(backend.Close)
	addr := backend.Listener.Addr().String()

	p := NewClientProxy(&fakeBackend{addr: addr, runtime: runtimeLlamaServer, ok: true},
		testClientProxyPort, zap.NewNop().Sugar())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "localhost:9443"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d (body=%q)", rec.Code, rec.Body.String())
	}
	got, _ := gotHost.Load().(string)
	if got != addr {
		t.Errorf("backend saw Host %q, want its own address %q; the caller's loopback Host must not be forwarded",
			got, addr)
	}
}

// TestClientProxy_RefusesProtocolUpgrade mirrors
// pkg/agent/ingress/server_test.go's TestServer_RefusesProtocolUpgrade: an
// Upgrade request still reaches the engine, but as a plain request, since
// ClientProxy.reverseProxy's Rewrite hook strips Upgrade and Connection
// before forwarding (ReverseProxy re-adds both after stripping hop-by-hop
// headers otherwise).
func TestClientProxy_RefusesProtocolUpgrade(t *testing.T) {
	var lastUpgrade, lastConnection atomic.Value
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastUpgrade.Store(r.Header.Get("Upgrade"))
		lastConnection.Store(r.Header.Get("Connection"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(backend.Close)

	p := NewClientProxy(&fakeBackend{addr: backend.Listener.Addr().String(), runtime: runtimeLlamaServer, ok: true},
		testClientProxyPort, zap.NewNop().Sugar())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "127.0.0.1:9443"
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code == http.StatusSwitchingProtocols {
		t.Fatal("client proxy relayed a 101 Switching Protocols to the caller")
	}
	if up, _ := lastUpgrade.Load().(string); up != "" {
		t.Errorf("engine saw Upgrade: %q, want none", up)
	}
	if c, _ := lastConnection.Load().(string); strings.Contains(strings.ToLower(c), "upgrade") {
		t.Errorf("engine saw Connection: %q, want no upgrade token", c)
	}
}

// TestClientProxy_RefusesUnsolicited101 mirrors
// pkg/agent/ingress/server_test.go's TestServer_RefusesUnsolicited101: even if
// an engine answers 101 without being asked, the client proxy turns it into
// the same 502 JSON error as any other upstream failure rather than relaying
// it (ClientProxy.reverseProxy's ModifyResponse shares ingress.RefuseUpgrade).
//
// This is a regression/documentation test, not a discriminating one: given
// the Rewrite hook above unconditionally strips the outbound request's
// Upgrade/Connection headers on every request, net/http's own machinery
// already makes a real protocol-switch pass-through unreachable independent
// of ModifyResponse (Response.isProtocolSwitch requires the response to
// carry matching Upgrade/Connection headers to expose a writable Body at
// all, and httputil's handleUpgradeResponse separately refuses whenever the
// outbound request's Upgrade type, always empty here, doesn't match the
// response's). This test still documents the intended contract and guards the observable
// status/body.
func TestClientProxy_RefusesUnsolicited101(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
	}))
	t.Cleanup(backend.Close)

	p := NewClientProxy(&fakeBackend{addr: backend.Listener.Addr().String(), runtime: runtimeLlamaServer, ok: true},
		testClientProxyPort, zap.NewNop().Sugar())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "127.0.0.1:9443"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: want 502 got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("502 should be JSON, got Content-Type %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("502 body should carry a JSON error, got %q", rec.Body.String())
	}
}

func TestStatusClass(t *testing.T) {
	tests := []struct {
		code     int
		expected string
	}{
		{100, "other"},
		{199, "other"},
		{200, "2xx"},
		{204, "2xx"},
		{299, "2xx"},
		{301, "3xx"},
		{302, "3xx"},
		{399, "3xx"},
		{400, "4xx"},
		{404, "4xx"},
		{499, "4xx"},
		{500, "5xx"},
		{502, "5xx"},
		{599, "5xx"},
	}
	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			got := statusClass(tt.code)
			if got != tt.expected {
				t.Errorf("statusClass(%d) = %q, want %q", tt.code, got, tt.expected)
			}
		})
	}
}

// stallBodyConn is a child connection that holds the Transport's writing
// goroutine just after it puts the request body on the wire, before the
// Transport's final EOF read of the inbound body. It mirrors the helper in
// pkg/agent/ingress/server_test.go.
type stallBodyConn struct {
	net.Conn
	body   string
	resume <-chan struct{}
	closed chan<- struct{}
	once   sync.Once
}

func (c *stallBodyConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if string(p) == c.body {
		<-c.resume
	}
	return n, err
}

func (c *stallBodyConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// TestClientProxy_StreamSurvivesLateRequestBodyEOFCheck mirrors
// pkg/agent/ingress/server_test.go's test of the same name. Host-side
// clients reach the client proxy over HTTP/1.1, whose server closes the
// request body when the proxy writes its first response chunk unless full
// duplex is enabled; a Transport EOF read of that body after that point
// fails and the Transport closes the child connection mid-stream.
func TestClientProxy_StreamSurvivesLateRequestBodyEOFCheck(t *testing.T) {
	const reqBody = `{"stream":true}`
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	resume := make(chan struct{})
	var resumeOnce sync.Once
	closeResume := func() { resumeOnce.Do(func() { close(resume) }) }
	childConnClosed := make(chan struct{})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: 1\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: 2\n\n")
	}))
	t.Cleanup(backend.Close)

	tr := &http.Transport{}
	var dialer net.Dialer
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &stallBodyConn{Conn: c, body: reqBody, resume: resume, closed: childConnClosed}, nil
	}
	t.Cleanup(tr.CloseIdleConnections)

	front := httptest.NewUnstartedServer(nil)
	port := front.Listener.Addr().(*net.TCPAddr).Port
	p := NewClientProxy(&fakeBackend{addr: backend.Listener.Addr().String(), runtime: runtimeLlamaServer, ok: true},
		port, zap.NewNop().Sugar())
	p.transport = tr
	front.Config.Handler = p
	front.Start()
	// LIFO: unblock the Transport writer and the backend handler before the
	// servers wait on them.
	t.Cleanup(front.Close)
	t.Cleanup(closeRelease)
	t.Cleanup(closeResume)

	client := front.Client()
	client.Timeout = 5 * time.Second
	resp, err := client.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); line != "data: 1\n" {
		t.Fatalf("first line = %q (err %v)", line, err)
	}
	// The proxy has flushed the first chunk, so the server has already
	// finished with the inbound body. Let the Transport make its EOF read.
	closeResume()
	select {
	case <-childConnClosed:
		t.Fatal("the client proxy closed the child connection mid-stream after its final request-body read")
	case <-time.After(200 * time.Millisecond):
	}
	closeRelease()
	rest, err := io.ReadAll(br)
	if err != nil || !strings.Contains(string(rest), "data: 2") {
		t.Fatalf("stream tail = %q, err %v; want the second event and a clean EOF", rest, err)
	}
}
