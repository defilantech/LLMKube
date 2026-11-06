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

package router

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// procNetFixture returns the path of a /proc/net/tcp{,6} fixture. Each one is
// a header plus rows in the kernel's own column layout.
func procNetFixture(name string) string {
	return filepath.Join("testdata", "procnet", name)
}

// withProcNet points the proxy's listener check at fixture tables instead of
// the host's /proc.
func withProcNet(tcp, tcp6 string) ProxyOption {
	return func(p *Proxy) {
		p.procNetTCP = tcp
		p.procNetTCP6 = tcp6
	}
}

func TestListenerCheckerMissing(t *testing.T) {
	notExist := filepath.Join(t.TempDir(), "absent")
	// Opening a directory succeeds and reading it fails, which stands in for
	// a table that exists but cannot be read.
	unreadable := t.TempDir()
	badHeader := filepath.Join(t.TempDir(), "tcp")
	if err := os.WriteFile(badHeader, []byte("garbage\n   0: 00000000:3A9E 00000000:0000 0A\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		ports     []uint16
		tcp, tcp6 string
		want      []uint16
		wantWarn  string
	}{
		{name: "LISTEN in tcp only", ports: []uint16{15008},
			tcp: procNetFixture("tcp-listen-15008"), tcp6: procNetFixture("tcp6-no-listener")},
		{name: "LISTEN in tcp6 only", ports: []uint16{15008},
			tcp: procNetFixture("tcp-no-listener"), tcp6: procNetFixture("tcp6-listen-15008")},
		{name: "ESTABLISHED on the port does not count", ports: []uint16{15008},
			tcp: procNetFixture("tcp-established-15008"), tcp6: procNetFixture("tcp6-no-listener"),
			want: []uint16{15008}},
		{name: "a different port in LISTEN does not count", ports: []uint16{15008},
			tcp: procNetFixture("tcp-listen-15006"), tcp6: procNetFixture("tcp6-no-listener"),
			want: []uint16{15008}},
		{name: "only the ports without a listener are missing", ports: []uint16{15008, 15006, 15008},
			tcp: procNetFixture("tcp-listen-15006"), tcp6: procNetFixture("tcp6-no-listener"),
			want: []uint16{15008}},
		{name: "absent tcp6 is an empty table, not a read failure", ports: []uint16{15008},
			tcp: procNetFixture("tcp-no-listener"), tcp6: notExist,
			want: []uint16{15008}},
		{name: "absent tcp6 still finds the tcp listener", ports: []uint16{15008},
			tcp: procNetFixture("tcp-listen-15008"), tcp6: notExist},
		{name: "unreadable tcp reports ready", ports: []uint16{15008},
			tcp: unreadable, tcp6: procNetFixture("tcp6-no-listener"),
			wantWarn: unreadable},
		{name: "absent tcp reports ready", ports: []uint16{15008},
			tcp: notExist, tcp6: procNetFixture("tcp6-no-listener"),
			wantWarn: notExist},
		{name: "unreadable tcp6 reports ready", ports: []uint16{15008},
			tcp: procNetFixture("tcp-no-listener"), tcp6: unreadable,
			wantWarn: unreadable},
		{name: "unrecognized header reports ready", ports: []uint16{15008},
			tcp: badHeader, tcp6: procNetFixture("tcp6-no-listener"),
			wantWarn: badHeader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
			c := newListenerChecker(tc.ports, tc.tcp, tc.tcp6, logger)

			// Twice, so the transition-only logging is exercised too.
			for range 2 {
				if got := c.missing(); !slices.Equal(got, tc.want) {
					t.Fatalf("missing() = %v, want %v", got, tc.want)
				}
			}

			warns := 0
			for line := range strings.Lines(logs.String()) {
				if strings.Contains(line, "level=WARN") && strings.Contains(line, "cannot read TCP socket table") {
					warns++
					if !strings.Contains(line, tc.wantWarn) {
						t.Errorf("warning does not name %s: %s", tc.wantWarn, line)
					}
				}
			}
			switch {
			case tc.wantWarn != "" && warns != 1:
				t.Errorf("got %d read-failure warnings over two checks, want exactly 1:\n%s", warns, logs.String())
			case tc.wantWarn == "" && warns != 0:
				t.Errorf("got %d read-failure warnings, want none:\n%s", warns, logs.String())
			}
		})
	}
}

// TestListenerCheckerLogsRecovery pins that a table that becomes readable
// again is logged once, so the warning has a matching all-clear.
func TestListenerCheckerLogsRecovery(t *testing.T) {
	dir := t.TempDir()
	tcp := filepath.Join(dir, "tcp")
	var logs bytes.Buffer
	c := newListenerChecker([]uint16{15008}, tcp, procNetFixture("tcp6-no-listener"),
		slog.New(slog.NewTextHandler(&logs, nil)))

	if got := c.missing(); got != nil {
		t.Fatalf("missing() with tcp absent = %v, want nil", got)
	}
	data, err := os.ReadFile(procNetFixture("tcp-listen-15008"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tcp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := c.missing(); got != nil {
		t.Fatalf("missing() with tcp listening = %v, want nil", got)
	}
	if n := strings.Count(logs.String(), "TCP socket table readable again"); n != 1 {
		t.Errorf("recovery logged %d times, want 1:\n%s", n, logs.String())
	}
}

// TestProxyReadyz drives GET /readyz through the mux Mount builds, the path a
// kubelet probe takes.
func TestProxyReadyz(t *testing.T) {
	h := newProxyHarness(t)
	for _, tc := range []struct {
		name        string
		opts        []ProxyOption
		wantCode    int
		wantMissing []uint16
	}{
		{name: "none required", wantCode: http.StatusOK},
		{name: "none required ignores /proc", opts: []ProxyOption{
			withProcNet(procNetFixture("tcp-no-listener"), procNetFixture("tcp6-no-listener")),
		}, wantCode: http.StatusOK},
		{name: "required listener present", opts: []ProxyOption{
			WithReadinessRequiredListeners([]uint16{15008}),
			withProcNet(procNetFixture("tcp-listen-15008"), procNetFixture("tcp6-no-listener")),
		}, wantCode: http.StatusOK},
		{name: "required listener absent", opts: []ProxyOption{
			WithReadinessRequiredListeners([]uint16{15008, 15006}),
			withProcNet(procNetFixture("tcp-listen-15006"), procNetFixture("tcp6-no-listener")),
		}, wantCode: http.StatusServiceUnavailable, wantMissing: []uint16{15008}},
		{name: "unreadable /proc fails open", opts: []ProxyOption{
			WithReadinessRequiredListeners([]uint16{15008}),
			withProcNet(filepath.Join(t.TempDir(), "absent"), procNetFixture("tcp6-no-listener")),
		}, wantCode: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProxy(h.cfg, slog.New(slog.DiscardHandler), tc.opts...)
			mux := http.NewServeMux()
			p.Mount(mux)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("/readyz = %d, want %d; body %s", rec.Code, tc.wantCode, rec.Body)
			}
			if tc.wantCode == http.StatusOK {
				return
			}
			var body struct {
				MissingListeners []uint16 `json:"missingListeners"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode /readyz body %q: %v", rec.Body, err)
			}
			if !slices.Equal(body.MissingListeners, tc.wantMissing) {
				t.Errorf("/readyz missingListeners = %v, want %v; body %s",
					body.MissingListeners, tc.wantMissing, rec.Body)
			}

			// /health stays the liveness answer whatever /readyz says.
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("/health = %d while /readyz is unready, want 200", rec.Code)
			}
		})
	}
}
