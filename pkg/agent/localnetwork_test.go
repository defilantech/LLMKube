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
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	restclient "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// localNetworkHint is a snippet of the watcher's Local Network hint.
const localNetworkHint = "Local Network access may be blocking"

// dialFailure is the error net/http returns for a TCP connect to ip that
// failed with errno: a *url.Error around the dial's *net.OpError.
func dialFailure(ip string, errno syscall.Errno) error {
	addr := netip.MustParseAddr(ip)
	return &url.Error{Op: "Get", URL: "https://api.example:6443/api", Err: &net.OpError{
		Op: "dial", Net: "tcp",
		Addr: &net.TCPAddr{IP: addr.AsSlice(), Zone: addr.Zone(), Port: 6443},
		Err:  os.NewSyscallError("connect", errno),
	}}
}

// realListError returns the error a List returns from a client built the way
// the agent's is (newKubeClient: client.New on rest.HTTPClientFor and no
// RESTMapper, so the first request is discovery) when every connection to
// server fails with errno. A real net.Dialer makes the dial, and its Control
// hook fails it before connect, so nothing leaves the host. A non-empty dialIP
// replaces the host the dialer is handed, as a resolver would for a hostname.
func realListError(t *testing.T, server, dialIP string, errno syscall.Errno) error {
	t.Helper()
	d := &net.Dialer{Control: func(string, string, syscall.RawConn) error { return errno }}
	cfg := &restclient.Config{
		Host:  server,
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if dialIP != "" {
				_, port, err := net.SplitHostPort(address)
				if err != nil {
					return nil, err
				}
				address = net.JoinHostPort(dialIP, port)
			}
			return d.DialContext(ctx, network, address)
		},
	}
	httpClient, err := restclient.HTTPClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: newTestScheme(), HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	err = c.List(context.Background(), &inferencev1alpha1.InferenceServiceList{}, client.InNamespace("default"))
	if err == nil {
		t.Fatal("List succeeded, want the dial failure")
	}
	return err
}

func TestLocalNetworkBlockAddr(t *testing.T) {
	proxied := &url.Error{Op: "Get", URL: "https://10.1.2.3:6443/api", Err: &net.OpError{
		Op: "proxyconnect", Net: "tcp",
		Err: dialFailure("192.168.7.7", syscall.EHOSTUNREACH).(*url.Error).Err,
	}}
	tests := []struct {
		name string
		err  error
		host string
		want string // the refused address, "" when the error is not a block
	}{
		{"RFC 1918 10/8", dialFailure("10.0.0.5", syscall.EHOSTUNREACH), "10.0.0.5", "10.0.0.5"},
		{"RFC 1918 172.16/12", dialFailure("172.20.3.4", syscall.EHOSTUNREACH), "172.20.3.4", "172.20.3.4"},
		{"RFC 1918 192.168/16", dialFailure("192.168.1.151", syscall.EHOSTUNREACH), "192.168.1.151", "192.168.1.151"},
		{"network unreachable", dialFailure("192.168.1.151", syscall.ENETUNREACH), "192.168.1.151", "192.168.1.151"},
		{"IPv4 link-local", dialFailure("169.254.10.20", syscall.EHOSTUNREACH), "", "169.254.10.20"},
		{"IPv6 ULA", dialFailure("fd12:3456:789a::1", syscall.EHOSTUNREACH), "", "fd12:3456:789a::1"},
		{"IPv6 link-local with zone", dialFailure("fe80::1%en0", syscall.EHOSTUNREACH), "", "fe80::1%en0"},
		{"IPv4-mapped private", dialFailure("::ffff:192.168.1.5", syscall.EHOSTUNREACH), "", "192.168.1.5"},
		{"IPv4 loopback", dialFailure("127.0.0.1", syscall.EHOSTUNREACH), "127.0.0.1", ""},
		{"IPv6 loopback", dialFailure("::1", syscall.EHOSTUNREACH), "::1", ""},
		{"CGNAT, a tailnet's IPv4", dialFailure("100.100.1.2", syscall.EHOSTUNREACH), "100.100.1.2", ""},
		{"a tailnet's IPv6 ULA", dialFailure("fd7a:115c:a1e0::1", syscall.EHOSTUNREACH), "", ""},
		{"public IPv4", dialFailure("8.8.8.8", syscall.EHOSTUNREACH), "8.8.8.8", ""},
		{"public IPv6", dialFailure("2606:4700::1111", syscall.EHOSTUNREACH), "", ""},
		{"connection refused", dialFailure("192.168.1.151", syscall.ECONNREFUSED), "192.168.1.151", ""},
		{"timed out", dialFailure("192.168.1.151", syscall.ETIMEDOUT), "192.168.1.151", ""},
		{"host down", dialFailure("192.168.1.151", syscall.EHOSTDOWN), "192.168.1.151", ""},
		{"not a network error", errors.New("forbidden"), "192.168.1.151", ""},
		{"nil", nil, "192.168.1.151", ""},
		{
			"wrapped twice",
			fmt.Errorf("failed to get server groups: %w", dialFailure("10.9.8.7", syscall.EHOSTUNREACH)),
			"", "10.9.8.7",
		},
		{
			"hostname: the dialed address decides",
			dialFailure("192.168.1.151", syscall.EHOSTUNREACH), "k8s.lan", "192.168.1.151",
		},
		{
			"hostname without a dialed address: no guess",
			fmt.Errorf("list: %w", syscall.EHOSTUNREACH), "k8s.lan", "",
		},
		{"IP host without a dialed address", fmt.Errorf("list: %w", syscall.EHOSTUNREACH), "10.0.0.5", "10.0.0.5"},
		{"public IP host without a dialed address", fmt.Errorf("list: %w", syscall.EHOSTUNREACH), "8.8.8.8", ""},
		{"no host, no dialed address", fmt.Errorf("list: %w", syscall.ENETUNREACH), "", ""},
		// The dial macOS refused is the one to the proxy.
		{"through a proxy: the proxy's address", proxied, "10.1.2.3", "192.168.7.7"},
		{"dialed public address over a private host", dialFailure("8.8.8.8", syscall.EHOSTUNREACH), "10.0.0.5", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr, blocked := localNetworkBlockAddr(tc.err, tc.host)
			if want := tc.want != ""; blocked != want {
				t.Fatalf("localNetworkBlockAddr(%v, %q) blocked = %v (%v), want %v", tc.err, tc.host, blocked, addr, want)
			}
			if blocked && addr.String() != tc.want {
				t.Errorf("address = %s, want %s", addr, tc.want)
			}
		})
	}
}

// The classifier reads the errors the agent's own client returns, not only
// hand-built ones: net, net/http, client-go and controller-runtime each wrap
// the dial's failure on the way up.
func TestLocalNetworkBlockAddr_RealClientErrors(t *testing.T) {
	tests := []struct {
		name, server, dialIP, host string
		errno                      syscall.Errno
		want                       string
	}{
		{"private IP, no route", "https://192.168.1.151:16443", "", "192.168.1.151", syscall.EHOSTUNREACH, "192.168.1.151"},
		{"private IP, network unreachable", "https://10.0.0.5:6443", "", "10.0.0.5", syscall.ENETUNREACH, "10.0.0.5"},
		{"hostname resolved to a private IP", "https://k8s.lan:16443", "192.168.1.151", "k8s.lan",
			syscall.EHOSTUNREACH, "192.168.1.151"},
		{"private IP, refused", "https://192.168.1.151:16443", "", "192.168.1.151", syscall.ECONNREFUSED, ""},
		{"loopback", "https://127.0.0.1:6443", "", "127.0.0.1", syscall.EHOSTUNREACH, ""},
		{"public IP", "https://203.0.113.10:6443", "", "203.0.113.10", syscall.EHOSTUNREACH, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := realListError(t, tc.server, tc.dialIP, tc.errno)
			addr, blocked := localNetworkBlockAddr(err, tc.host)
			if want := tc.want != ""; blocked != want {
				t.Fatalf("localNetworkBlockAddr(%v) blocked = %v (%v), want %v", err, blocked, addr, want)
			}
			if blocked && addr.String() != tc.want {
				t.Errorf("address = %s, want %s", addr, tc.want)
			}
		})
	}
}

// hintSample is the Local Network gauge and the number of hints logged so far,
// taken on the watcher's goroutine right after the given List call.
type hintSample struct {
	call  int32
	gauge float64
	hints int
}

// TestWatch_LocalNetworkHintOncePerOutage: on macOS, an outage whose failed
// lists say "no route to host" to a private API server address logs the
// Local Network hint once, naming the address, and holds the gauge at 1 while
// its failures look like that; the recovery clears it, and the next such
// outage hints again. On another system the same failures log no hint and
// leave the gauge at 0.
func TestWatch_LocalNetworkHintOncePerOutage(t *testing.T) {
	const server = "https://192.168.1.151:16443"
	blocked := realListError(t, server, "", syscall.EHOSTUNREACH)
	refused := realListError(t, server, "", syscall.ECONNREFUSED)
	// Lists 1 and 2 succeed. Outage one: 3 to 5 have no route (3 is below
	// the threshold of 2), 6 is refused; 7 succeeds. Outage two: 8 to 10
	// have no route; 11 on succeed.
	script := func(n int32) error {
		switch {
		case n >= 3 && n <= 5, n >= 8 && n <= 10:
			return blocked
		case n == 6:
			return refused
		}
		return nil
	}
	tests := []struct {
		goos                string
		failures, recovered []hintSample
	}{
		{
			goos:      "darwin",
			failures:  []hintSample{{4, 1, 1}, {5, 1, 1}, {6, 0, 1}, {9, 1, 2}, {10, 1, 2}},
			recovered: []hintSample{{7, 0, 1}, {11, 0, 2}},
		},
		{
			goos:      "linux",
			failures:  []hintSample{{4, 0, 0}, {5, 0, 0}, {6, 0, 0}, {9, 0, 0}, {10, 0, 0}},
			recovered: []hintSample{{7, 0, 0}, {11, 0, 0}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.goos, func(t *testing.T) {
			watchLocalNetworkBlocked.Set(0)
			c, calls := scriptedListErrors(t, script, metalServiceObjects()...)
			core, logs := observer.New(zap.DebugLevel)
			w := NewInferenceServiceWatcher(c, "default", zap.New(core).Sugar())
			w.goos = tc.goos
			w.SetAPIServer(server)
			w.SetPollInterval(time.Millisecond)
			w.maxPollBackoff = 5 * time.Millisecond
			w.SetMaxConsecutiveFailures(2)
			sample := func() hintSample {
				return hintSample{calls.Load(), testutil.ToFloat64(watchLocalNetworkBlocked),
					logs.FilterMessageSnippet(localNetworkHint).Len()}
			}
			// The reset hook runs after each failure once the API counts as
			// unreachable, the reachable hook after each recovery.
			var failures, recovered []hintSample
			w.SetAPIHooks(
				func() { failures = append(failures, sample()) },
				func() { recovered = append(recovered, sample()) },
			)

			stop := runWatch(t, w, make(chan InferenceServiceEvent, 16))
			waitUntil(t, "both outages to end", func() bool { return calls.Load() >= 12 })
			stop()

			if !slices.Equal(failures, tc.failures) {
				t.Errorf("after each failed list while unreachable {call, gauge, hints} = %v, want %v",
					failures, tc.failures)
			}
			if !slices.Equal(recovered, tc.recovered) {
				t.Errorf("after each recovery {call, gauge, hints} = %v, want %v", recovered, tc.recovered)
			}
			for _, e := range logs.FilterMessageSnippet(localNetworkHint).All() {
				if e.Level != zap.WarnLevel {
					t.Errorf("hint logged at %v, want warn", e.Level)
				}
				if got := e.ContextMap()["address"]; got != "192.168.1.151" {
					t.Errorf("hint address = %v, want 192.168.1.151", got)
				}
				for _, want := range []string{localNetworkSettings, "every agent upgrade"} {
					if !strings.Contains(e.Message, want) {
						t.Errorf("hint %q does not say %q", e.Message, want)
					}
				}
			}
		})
	}
}

// TestWatch_NoLocalNetworkHintForABlip: failures that stop short of the
// threshold are not an outage, so they give no hint even on macOS: the first
// LAN connection of a freshly started agent can fail this way while macOS
// evaluates it (#2326).
func TestWatch_NoLocalNetworkHintForABlip(t *testing.T) {
	const server = "https://192.168.1.151:16443"
	blocked := realListError(t, server, "", syscall.EHOSTUNREACH)
	watchLocalNetworkBlocked.Set(0)
	// Lists 3 and 4 have no route, one short of the threshold of 3. List 5
	// samples the gauge as the blip's last failure left it.
	afterBlip := -1.0
	c, calls := scriptedListErrors(t, func(n int32) error {
		switch n {
		case 3, 4:
			return blocked
		case 5:
			afterBlip = testutil.ToFloat64(watchLocalNetworkBlocked)
		}
		return nil
	}, metalServiceObjects()...)
	core, logs := observer.New(zap.DebugLevel)
	w := NewInferenceServiceWatcher(c, "default", zap.New(core).Sugar())
	w.goos = "darwin"
	w.SetAPIServer(server)
	w.SetPollInterval(time.Millisecond)
	w.SetMaxConsecutiveFailures(3)

	stop := runWatch(t, w, make(chan InferenceServiceEvent, 16))
	waitUntil(t, "polls after the blip", func() bool { return calls.Load() >= 6 })
	stop()

	if n := logs.FilterMessageSnippet(localNetworkHint).Len(); n != 0 {
		t.Errorf("logged the Local Network hint %d times for a blip, want never", n)
	}
	if afterBlip != 0 {
		t.Errorf("local_network_blocked gauge after the blip's failures = %v, want 0", afterBlip)
	}
}

// TestNewWatcher_LocalNetworkHintFallsBackToTheAPIServer: the watcher Start
// runs knows the API server from the agent's config, so a failed list whose
// error names no dialed address is still matched against an IP host.
func TestNewWatcher_LocalNetworkHintFallsBackToTheAPIServer(t *testing.T) {
	bare := fmt.Errorf("list inferenceservices: %w", syscall.EHOSTUNREACH)
	c, calls := scriptedListErrors(t, func(n int32) error {
		if n <= 3 {
			return bare
		}
		return nil
	})
	core, logs := observer.New(zap.InfoLevel)
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient:        c,
		Namespace:        "default",
		ModelStorePath:   t.TempDir(),
		StateDir:         t.TempDir(),
		MaxWatchFailures: 2,
		APIServerHost:    "https://192.168.1.151:16443",
		Logger:           zap.New(core).Sugar(),
	})
	a.tuneWatcher = func(w *InferenceServiceWatcher) {
		w.goos = "darwin"
		w.SetPollInterval(time.Millisecond)
		w.maxPollBackoff = 5 * time.Millisecond
	}

	stop := runWatch(t, a.newWatcher(), make(chan InferenceServiceEvent, 4))
	waitUntil(t, "the outage to end", func() bool { return calls.Load() >= 5 })
	stop()

	hints := logs.FilterMessageSnippet(localNetworkHint).All()
	if len(hints) != 1 {
		t.Fatalf("logged the Local Network hint %d times, want once", len(hints))
	}
	if got := hints[0].ContextMap()["address"]; got != "192.168.1.151" {
		t.Errorf("hint address = %v, want the configured API server's 192.168.1.151", got)
	}
}
