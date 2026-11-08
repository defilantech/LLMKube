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
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
		wantState string
		wantWarn  string
	}{
		{name: "LISTEN in tcp only", ports: []uint16{15008},
			tcp: procNetFixture("tcp-listen-15008"), tcp6: procNetFixture("tcp6-no-listener"),
			wantState: logPresent},
		{name: "LISTEN in tcp6 only", ports: []uint16{15008},
			tcp: procNetFixture("tcp-no-listener"), tcp6: procNetFixture("tcp6-listen-15008"),
			wantState: logPresent},
		{name: "ESTABLISHED on the port does not count", ports: []uint16{15008},
			tcp: procNetFixture("tcp-established-15008"), tcp6: procNetFixture("tcp6-no-listener"),
			want: []uint16{15008}, wantState: logMissing},
		{name: "a different port in LISTEN does not count", ports: []uint16{15008},
			tcp: procNetFixture("tcp-listen-15006"), tcp6: procNetFixture("tcp6-no-listener"),
			want: []uint16{15008}, wantState: logMissing},
		{name: "only the ports without a listener are missing", ports: []uint16{15008, 15006, 15008},
			tcp: procNetFixture("tcp-listen-15006"), tcp6: procNetFixture("tcp6-no-listener"),
			want: []uint16{15008}, wantState: logMissing},
		{name: "absent tcp6 is an empty table, not a read failure", ports: []uint16{15008},
			tcp: procNetFixture("tcp-no-listener"), tcp6: notExist,
			want: []uint16{15008}, wantState: logMissing},
		{name: "absent tcp6 still finds the tcp listener", ports: []uint16{15008},
			tcp: procNetFixture("tcp-listen-15008"), tcp6: notExist,
			wantState: logPresent},
		{name: "unreadable tcp reports ready", ports: []uint16{15008},
			tcp: unreadable, tcp6: procNetFixture("tcp6-no-listener"),
			wantState: logNotEnforced, wantWarn: unreadable},
		{name: "absent tcp reports ready", ports: []uint16{15008},
			tcp: notExist, tcp6: procNetFixture("tcp6-no-listener"),
			wantState: logNotEnforced, wantWarn: notExist},
		{name: "unreadable tcp6 reports ready", ports: []uint16{15008},
			tcp: procNetFixture("tcp-no-listener"), tcp6: unreadable,
			wantState: logNotEnforced, wantWarn: unreadable},
		{name: "unrecognized header reports ready", ports: []uint16{15008},
			tcp: badHeader, tcp6: procNetFixture("tcp6-no-listener"),
			wantState: logNotEnforced, wantWarn: badHeader},
		{name: "a port found in the readable table is present", ports: []uint16{15008},
			tcp: procNetFixture("tcp-listen-15008"), tcp6: unreadable,
			wantState: logPresent, wantWarn: unreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
			c := newListenerChecker(tc.ports, tc.tcp, tc.tcp6, logger)
			c.ttl = 0

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

			for _, state := range []string{logPresent, logMissing, logNotEnforced} {
				want := 0
				if state == tc.wantState {
					want = 1
				}
				if n := strings.Count(logs.String(), state); n != want {
					t.Errorf("%q logged %d times over two checks, want %d:\n%s", state, n, want, logs.String())
				}
			}
			if tc.wantState == logNotEnforced && !strings.Contains(logs.String(), "unreadable=["+tc.wantWarn+"]") {
				t.Errorf("%q does not name the unreadable table %s:\n%s", logNotEnforced, tc.wantWarn, logs.String())
			}
		})
	}
}

// The transition lines a listenerChecker logs.
const (
	logPresent     = "required listeners present; reporting ready"
	logMissing     = "required listeners missing; reporting not ready"
	logNotEnforced = "required listeners not enforced; reporting ready"
)

// TestListenerCheckerLogsRecovery pins that a table that becomes readable
// again is logged once, so the warning has a matching all-clear.
func TestListenerCheckerLogsRecovery(t *testing.T) {
	dir := t.TempDir()
	tcp := filepath.Join(dir, "tcp")
	var logs bytes.Buffer
	c := newListenerChecker([]uint16{15008}, tcp, procNetFixture("tcp6-no-listener"),
		slog.New(slog.NewTextHandler(&logs, nil)))
	c.ttl = 0

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
	if n := strings.Count(logs.String(), logPresent); n != 1 {
		t.Errorf("%q logged %d times after the listener was seen, want 1:\n%s", logPresent, n, logs.String())
	}
}

// fakeClock is a settable clock for the listener cache.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// countScans wraps c's table scan so a test can count how many times the
// tables were read.
func countScans(c *listenerChecker) *atomic.Int64 {
	var n atomic.Int64
	scan := c.scan
	c.scan = func(path string, want, found map[uint16]bool) error {
		n.Add(1)
		return scan(path, want, found)
	}
	return &n
}

// TestListenerCheckerCachesWithinTTL pins the bound on /readyz cost: callers
// within the TTL reuse the last result, and the first call after it rescans
// and sees a change.
func TestListenerCheckerCachesWithinTTL(t *testing.T) {
	tcp := filepath.Join(t.TempDir(), "tcp")
	copyFixture(t, "tcp-no-listener", tcp)
	c := newListenerChecker([]uint16{15008}, tcp, procNetFixture("tcp6-no-listener"), slog.New(slog.DiscardHandler))
	clock := &fakeClock{t: time.Unix(0, 0)}
	c.now = clock.now
	scans := countScans(c)

	if got := c.missing(); !slices.Equal(got, []uint16{15008}) {
		t.Fatalf("first missing() = %v, want [15008]", got)
	}
	copyFixture(t, "tcp-listen-15008", tcp)
	clock.advance(listenerCheckTTL - time.Millisecond)
	if got := c.missing(); !slices.Equal(got, []uint16{15008}) {
		t.Errorf("missing() within the TTL = %v, want the cached [15008]", got)
	}
	if n := scans.Load(); n != 2 {
		t.Errorf("tables read %d times for two calls within the TTL, want 2 (one check)", n)
	}

	clock.advance(time.Millisecond)
	if got := c.missing(); got != nil {
		t.Errorf("missing() after the TTL = %v, want nil now that 15008 listens", got)
	}
	if n := scans.Load(); n != 4 {
		t.Errorf("tables read %d times after the TTL, want 4 (two checks)", n)
	}
}

// TestListenerCheckerConcurrentCallersShareOneCheck pins that callers arriving
// while a check runs wait for it rather than scanning again, and all get the
// same answer. Run with -race.
func TestListenerCheckerConcurrentCallersShareOneCheck(t *testing.T) {
	c := newListenerChecker([]uint16{15008}, procNetFixture("tcp-listen-15006"),
		procNetFixture("tcp6-no-listener"), slog.New(slog.DiscardHandler))
	clock := &fakeClock{t: time.Unix(0, 0)}
	c.now = clock.now
	scans := countScans(c)
	release := make(chan struct{})
	scan := c.scan
	c.scan = func(path string, want, found map[uint16]bool) error {
		<-release
		return scan(path, want, found)
	}

	const callers = 32
	results := make([][]uint16, callers)
	var started, done sync.WaitGroup
	for i := range callers {
		started.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			started.Done()
			results[i] = c.missing()
		}()
	}
	started.Wait()
	// Give every caller time to reach missing() before the scan finishes.
	time.Sleep(50 * time.Millisecond)
	close(release)
	done.Wait()

	for i, got := range results {
		if !slices.Equal(got, []uint16{15008}) {
			t.Errorf("caller %d got %v, want [15008]", i, got)
		}
	}
	if n := scans.Load(); n != 2 {
		t.Errorf("tables read %d times for %d concurrent callers, want 2 (one check)", n, callers)
	}
}

func copyFixture(t *testing.T, name, dst string) {
	t.Helper()
	data, err := os.ReadFile(procNetFixture(name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
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
