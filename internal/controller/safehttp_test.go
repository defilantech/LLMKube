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

package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIPIsBlocked(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		// Blocked: loopback, link-local, RFC-1918, ULA, unspecified.
		{"127.0.0.1", true},
		{"::1", true},
		{"::ffff:127.0.0.1", true}, // IPv4-in-IPv6 loopback must not slip through
		{"169.254.169.254", true},  // cloud metadata
		{"fe80::1", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"fc00::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"::ffff:10.0.0.1", true}, // IPv4-in-IPv6 RFC-1918
		// Blocked: ranges IsPrivate() does not cover (GHSA-jw3m-8q7m-f35r).
		{"100.64.0.1", true},        // CGNAT (RFC 6598)
		{"100.93.59.11", true},      // CGNAT: Tailscale address in this env
		{"192.0.0.1", true},         // IETF protocol assignments (RFC 6890)
		{"198.18.0.1", true},        // benchmarking (RFC 2544)
		{"198.19.255.1", true},      // benchmarking upper half of the /15
		{"192.88.99.1", true},       // 6to4 relay anycast (RFC 3068)
		{"64:ff9b::7f00:1", true},   // NAT64 well-known prefix (RFC 6052)
		{"64:ff9b:1::a", true},      // NAT64 local-use (RFC 8215)
		{"::169.254.169.254", true}, // IPv4-compatible IPv6 hiding metadata IP
		{"::127.0.0.1", true},       // IPv4-compatible IPv6 loopback
		{"::ffff:100.64.0.1", true}, // IPv4-mapped CGNAT
		// Not blocked: public addresses (incl. boundary neighbors of the new ranges).
		{"1.1.1.1", false},
		{"8.8.8.8", false},
		{"2606:4700:4700::1111", false},
		{"100.128.0.1", false}, // just above 100.64.0.0/10
		{"198.20.0.1", false},  // just above 198.18.0.0/15
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := netip.MustParseAddr(tc.ip)
			if got := ipIsBlocked(ip); got != tc.blocked {
				t.Errorf("ipIsBlocked(%s) = %v, want %v", tc.ip, got, tc.blocked)
			}
		})
	}
}

func TestParseRemoteHostAllowlist(t *testing.T) {
	al := parseRemoteHostAllowlist([]string{
		"10.20.0.0/16",
		"10.9.9.9",
		"Artifact.Corp",
		"  ",
		"",
	})

	ipCases := []struct {
		ip      string
		allowed bool
	}{
		{"10.20.5.5", true},        // inside CIDR
		{"10.30.5.5", false},       // outside CIDR
		{"10.9.9.9", true},         // bare IP became a /32
		{"10.9.9.10", false},       // adjacent IP not covered by the /32
		{"::ffff:10.20.5.5", true}, // 4-in-6 form of an allowlisted IPv4
	}
	for _, tc := range ipCases {
		if got := al.ipAllowed(netip.MustParseAddr(tc.ip)); got != tc.allowed {
			t.Errorf("ipAllowed(%s) = %v, want %v", tc.ip, got, tc.allowed)
		}
	}

	hostCases := []struct {
		host    string
		allowed bool
	}{
		{"artifact.corp", true}, // case-insensitive match
		{"ARTIFACT.CORP", true},
		{"other.corp", false},
		{"", false}, // blank entries were dropped, not registered
	}
	for _, tc := range hostCases {
		if got := al.hostAllowed(tc.host); got != tc.allowed {
			t.Errorf("hostAllowed(%q) = %v, want %v", tc.host, got, tc.allowed)
		}
	}
}

// TestGuardedClientBlocksLoopback is the end-to-end proof the guard fires: a
// guarded client with an empty allowlist must refuse to connect to a loopback
// httptest server (the SSRF-relevant target class), and the same client with
// 127.0.0.1 allowlisted must succeed.
func TestGuardedClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	t.Run("empty allowlist refuses loopback", func(t *testing.T) {
		client := newGuardedHTTPClient(parseRemoteHostAllowlist(nil), 5*time.Second)
		resp, err := client.Get(srv.URL)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("expected the SSRF guard to block %s, but the request succeeded", srv.URL)
		}
		if !strings.Contains(err.Error(), "SSRF guard") {
			t.Errorf("expected an SSRF-guard error, got: %v", err)
		}
	})

	t.Run("allowlisted loopback is permitted", func(t *testing.T) {
		client := newGuardedHTTPClient(parseRemoteHostAllowlist([]string{"127.0.0.1"}), 5*time.Second)
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("expected allowlisted request to succeed, got: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading body: %v", err)
		}
		if string(body) != "ok" {
			t.Errorf("unexpected body %q", body)
		}
	})

	t.Run("CIDR allowlist covering loopback is permitted", func(t *testing.T) {
		client := newGuardedHTTPClient(parseRemoteHostAllowlist([]string{"127.0.0.0/8"}), 5*time.Second)
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("expected CIDR-allowlisted request to succeed, got: %v", err)
		}
		_ = resp.Body.Close()
	})
}

// TestGuardedClientRedirects verifies redirect hops dial through the same
// guard: with loopback allowlisted a 302 chain works end-to-end, and the hop
// count is capped.
func TestGuardedClientRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("target"))
	}))
	defer target.Close()

	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer hop.Close()

	t.Run("allowlisted redirect chain succeeds", func(t *testing.T) {
		client := newGuardedHTTPClient(parseRemoteHostAllowlist([]string{"127.0.0.1"}), 5*time.Second)
		resp, err := client.Get(hop.URL)
		if err != nil {
			t.Fatalf("expected allowlisted redirect to succeed, got: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "target" {
			t.Errorf("expected to land on target, got body %q", body)
		}
	})

	t.Run("blocked redirect target is refused at dial", func(t *testing.T) {
		// Allowlist ONLY the first hop's exact host:port would not be expressible
		// (allowlist is host-level), so instead prove the guard fires on the hop:
		// with an empty allowlist even the first loopback hop is refused.
		client := newGuardedHTTPClient(parseRemoteHostAllowlist(nil), 5*time.Second)
		resp, err := client.Get(hop.URL)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected the SSRF guard to block the redirecting server")
		}
	})

	t.Run("redirect hops are capped", func(t *testing.T) {
		var loop *httptest.Server
		loop = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, loop.URL, http.StatusFound)
		}))
		defer loop.Close()

		client := newGuardedHTTPClient(parseRemoteHostAllowlist([]string{"127.0.0.1"}), 5*time.Second)
		resp, err := client.Get(loop.URL)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected the redirect loop to be capped")
		}
		if !strings.Contains(err.Error(), "redirect") {
			t.Errorf("expected a redirect-cap error, got: %v", err)
		}
	})
}

// TestGuardedClientHostnameAllowlist proves a hostname entry re-permits a name
// that resolves to a blocked IP. "localhost" resolves to loopback, so with
// "localhost" allowlisted the request must succeed even though every resolved
// IP is blocked.
func TestGuardedClientHostnameAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// Rewrite 127.0.0.1 -> localhost so the dialer takes the DNS path.
	url := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)

	t.Run("hostname not allowlisted is refused", func(t *testing.T) {
		client := newGuardedHTTPClient(parseRemoteHostAllowlist(nil), 5*time.Second)
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected the SSRF guard to block localhost")
		}
	})

	t.Run("hostname allowlisted is permitted", func(t *testing.T) {
		client := newGuardedHTTPClient(parseRemoteHostAllowlist([]string{"LocalHost"}), 5*time.Second)
		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("expected hostname-allowlisted request to succeed, got: %v", err)
		}
		_ = resp.Body.Close()
	})
}

// TestDownloadModelUsesGuardedClient proves the controller-side download sink
// goes through the SSRF-guarded metadata client (GHSA-jw3m-8q7m-f35r): with no
// allowlist configured, a source resolving to loopback must be refused before
// any bytes are fetched, surfacing on the normal download-error path.
func TestDownloadModelUsesGuardedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not a real model"))
	}))
	defer srv.Close()

	r := &ModelReconciler{} // empty AllowedRemoteHosts: secure default
	dest := filepath.Join(t.TempDir(), "model.gguf")
	_, err := r.downloadModel(context.Background(), srv.URL, dest)
	if err == nil {
		t.Fatal("expected the SSRF guard to refuse a loopback download source")
	}
	if !strings.Contains(err.Error(), "SSRF guard") {
		t.Errorf("expected an SSRF-guard error, got: %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("expected no file written for a blocked download, stat err: %v", statErr)
	}
}
