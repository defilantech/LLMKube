package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestRefuseCrossHostRedirect pins the fix for the MCP auth-header replay
// risk: Go's net/http only strips sensitive headers on cross-domain
// redirects when those headers were set on the original request, not when
// they're injected by a custom RoundTripper (as headerRoundTripper does
// for ServerConfig.Headers, which typically carries a secret-sourced auth
// token). Without CheckRedirect refusing cross-host hops, a 3xx from the
// configured (or MITM'd) MCP endpoint to a different host would replay
// the auth header to that host.
func TestRefuseCrossHostRedirect(t *testing.T) {
	mustReq := func(t *testing.T, raw string) *http.Request {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", raw, err)
		}
		return &http.Request{URL: u}
	}

	t.Run("same_host_allowed", func(t *testing.T) {
		original := mustReq(t, "https://mcp.example.com/v1")
		next := mustReq(t, "https://mcp.example.com/v1/redirected")
		if err := refuseCrossHostRedirect(next, []*http.Request{original}); err != nil {
			t.Fatalf("same-host redirect should be allowed, got err: %v", err)
		}
	})

	t.Run("cross_host_refused", func(t *testing.T) {
		original := mustReq(t, "https://mcp.example.com/v1")
		next := mustReq(t, "https://evil.attacker.com/steal")
		err := refuseCrossHostRedirect(next, []*http.Request{original})
		if err == nil {
			t.Fatal("cross-host redirect should be refused, got nil error")
		}
		if !strings.Contains(err.Error(), "evil.attacker.com") {
			t.Errorf("error should name the offending host, got: %v", err)
		}
	})

	t.Run("cross_host_different_port_refused", func(t *testing.T) {
		// url.URL.Host includes the port, so a same-hostname-different-port
		// redirect is also a different "host" and must be refused: a
		// service on a different port is a different origin/service.
		original := mustReq(t, "https://mcp.example.com:8443/v1")
		next := mustReq(t, "https://mcp.example.com:9999/v1")
		if err := refuseCrossHostRedirect(next, []*http.Request{original}); err == nil {
			t.Fatal("cross-port redirect should be refused, got nil error")
		}
	})

	t.Run("empty_via_allowed", func(t *testing.T) {
		// No prior request (the first hop, not a redirect) -- nothing to
		// compare against, so CheckRedirect must not fire here.
		next := mustReq(t, "https://mcp.example.com/v1")
		if err := refuseCrossHostRedirect(next, nil); err != nil {
			t.Fatalf("empty via (not a redirect) should be allowed, got err: %v", err)
		}
	})
}

func TestSession_ListAndCall(t *testing.T) {
	srv := startFakeMCP(t) // from fake_server_test.go; returns URL
	s, err := dial(context.Background(), ServerConfig{Name: "fake", URL: srv})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = s.Close() }()

	tools, err := s.listTools(context.Background())
	if err != nil || len(tools) != 2 {
		t.Fatalf("listTools = %v, %v", tools, err)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}
	if !names["echo"] || !names["boom"] {
		t.Fatalf("listTools names = %v, want echo and boom", names)
	}
	text, isErr, err := s.callTool(context.Background(), "echo", json.RawMessage(`{"msg":"hi"}`))
	if err != nil || isErr || text != "hi" {
		t.Fatalf("callTool = %q, %v, %v", text, isErr, err)
	}
}

// TestSession_CallTool_IsError verifies that a tool-level error result from
// the remote server surfaces as isError=true with a nil Go error, not as a
// Go error. Later callers rely on this distinction to tell "the tool ran
// and reported failure" apart from "the call itself failed" (transport,
// protocol, or unknown-tool errors).
func TestSession_CallTool_IsError(t *testing.T) {
	srv := startFakeMCP(t)
	s, err := dial(context.Background(), ServerConfig{Name: "fake", URL: srv})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = s.Close() }()

	text, isErr, err := s.callTool(context.Background(), "boom", nil)
	if err != nil {
		t.Fatalf("callTool returned Go error: %v", err)
	}
	if !isErr {
		t.Fatalf("callTool isError = false, want true")
	}
	if !strings.Contains(text, "kaboom") {
		t.Fatalf("callTool text = %q, want it to contain %q", text, "kaboom")
	}
}
