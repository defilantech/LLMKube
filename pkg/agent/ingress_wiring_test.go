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
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/defilantech/llmkube/pkg/agent/ingress"
)

// testIngressPin is a well-formed SPKI pin: base64-std of 32 bytes.
var testIngressPin = base64.StdEncoding.EncodeToString(make([]byte, 32))

func seedProcess(a *MetalAgent, p *ManagedProcess) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.processes[types.NamespacedName{Namespace: p.Namespace, Name: p.Name}.String()] = p
}

func TestRoute_ReturnsRunningProcess(t *testing.T) {
	a := NewMetalAgent(MetalAgentConfig{})
	seedProcess(a, &ManagedProcess{Namespace: "team-a", Name: "llm", Port: 51234, Healthy: true, Runtime: "llamacpp"})
	seedProcess(a, &ManagedProcess{Namespace: "team-a", Name: "sick", Port: 51235, Healthy: false, Runtime: "omlx"})

	var _ ingress.Router = a

	r, ok := a.Route("team-a", "llm")
	if !ok {
		t.Fatal("Route(team-a, llm) = false, want true")
	}
	if r != (ingress.Route{Runtime: "llamacpp", Port: 51234, Ready: true}) {
		t.Errorf("Route = %+v, want llamacpp 51234 ready", r)
	}
	r, ok = a.Route("team-a", "sick")
	if !ok || r.Ready || r.Runtime != "omlx" || r.Port != 51235 {
		t.Errorf("Route(sick) = %+v, %t; want omlx 51235 not ready", r, ok)
	}
	for _, k := range [][2]string{{"team-a", "nope"}, {"team-b", "llm"}, {"", ""}} {
		if _, ok := a.Route(k[0], k[1]); ok {
			t.Errorf("Route(%q, %q) = true, want false", k[0], k[1])
		}
	}
}

func TestServesNamespace(t *testing.T) {
	a := NewMetalAgent(MetalAgentConfig{})
	if a.ServesNamespace("team-a") {
		t.Error("ServesNamespace with no processes = true, want false")
	}
	seedProcess(a, &ManagedProcess{Namespace: "team-a", Name: "llm", Port: 1, Runtime: "llamacpp"})
	if !a.ServesNamespace("team-a") {
		t.Error("ServesNamespace(team-a) = false, want true")
	}
	if a.ServesNamespace("team-b") {
		t.Error("ServesNamespace(team-b) = true, want false")
	}
}

func TestValidateIngressConfig_RejectsPortCollisions(t *testing.T) {
	cases := []struct {
		name    string
		cfg     MetalAgentConfig
		wantErr bool
	}{
		{"distinct", MetalAgentConfig{IngressPort: 9443, Port: 9090, ClientPort: 9999}, false},
		{"metrics collision", MetalAgentConfig{IngressPort: 9090, Port: 9090, ClientPort: 9999}, true},
		{"client collision", MetalAgentConfig{IngressPort: 9999, Port: 9090, ClientPort: 9999}, true},
		{"zero ingress port", MetalAgentConfig{IngressPort: 0, Port: 9090}, true},
		{"disabled client proxy", MetalAgentConfig{IngressPort: 9443, Port: 9090, ClientPort: 0}, false},
		{"legacy ignores ingress port", MetalAgentConfig{LegacyDirectEndpoints: true, IngressPort: 9090, Port: 9090}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validateIngress()
			if (err != nil) != tc.wantErr {
				t.Errorf("validateIngress() = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

// Legacy mode binds every engine on all interfaces; relay mode leaves
// BindHost empty so the executors bind loopback.
func TestEnsureProcess_BindHostFollowsMode(t *testing.T) {
	for _, tc := range []struct {
		legacy bool
		want   string
	}{{true, "0.0.0.0"}, {false, ""}} {
		isvc := refusalISVC("llm")
		a, ex, _ := refusalFixture(t, MetalAgentConfig{LegacyDirectEndpoints: tc.legacy}, isvc, refusalModel())
		if err := a.ensureProcess(context.Background(), isvc); err != nil {
			t.Fatalf("legacy=%t: ensureProcess: %v", tc.legacy, err)
		}
		if ex.lastCfg.BindHost != tc.want {
			t.Errorf("legacy=%t: BindHost = %q, want %q", tc.legacy, ex.lastCfg.BindHost, tc.want)
		}
	}
}

func TestPrepareIngress_LegacyDoesNotEnableIngress(t *testing.T) {
	isvc := refusalISVC("llm")
	stateDir := t.TempDir()
	a, _, _ := refusalFixture(t, MetalAgentConfig{
		LegacyDirectEndpoints: true, IngressPort: 9443, StateDir: stateDir,
	}, isvc)
	srv, err := a.prepareIngress()
	if err != nil {
		t.Fatalf("prepareIngress: %v", err)
	}
	if srv != nil {
		t.Error("legacy mode returned an ingress server, want nil")
	}
	if a.registry.relayMode() {
		t.Error("legacy mode put the registry in relay mode")
	}
	if entries, _ := os.ReadDir(stateDir); len(entries) != 0 {
		t.Errorf("legacy mode wrote state %v, want none", entries)
	}
}

func TestPrepareIngress_RelayEnablesIngressWithPin(t *testing.T) {
	isvc := refusalISVC("llm")
	stateDir := t.TempDir()
	a, _, _ := refusalFixture(t, MetalAgentConfig{IngressPort: 9443, StateDir: stateDir}, isvc)
	srv, err := a.prepareIngress()
	if err != nil {
		t.Fatalf("prepareIngress: %v", err)
	}
	if srv == nil {
		t.Fatal("relay mode returned no ingress server")
	}
	if !a.registry.relayMode() || a.registry.ingressPort != 9443 {
		t.Errorf("registry relay=%t port=%d, want relay on 9443", a.registry.relayMode(), a.registry.ingressPort)
	}
	raw, err := base64.StdEncoding.DecodeString(a.registry.ingressPin)
	if err != nil || len(raw) != 32 {
		t.Errorf("registry pin %q is not base64 of 32 bytes", a.registry.ingressPin)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "ingress", "ingress-key.pem")); err != nil {
		t.Errorf("identity key not written under state dir: %v", err)
	}
}

func TestPrepareIngress_RelayFailsOnBadIdentity(t *testing.T) {
	isvc := refusalISVC("llm")
	stateDir := t.TempDir()
	// A lone key file is an incomplete identity; LoadOrCreateIdentity refuses it.
	if err := os.MkdirAll(filepath.Join(stateDir, "ingress"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "ingress", "ingress-key.pem"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _, _ := refusalFixture(t, MetalAgentConfig{IngressPort: 9443, StateDir: stateDir}, isvc)
	if _, err := a.prepareIngress(); err == nil {
		t.Fatal("prepareIngress with an incomplete identity = nil error, want failure")
	}
	if a.registry.relayMode() {
		t.Error("registry entered relay mode despite the identity failure")
	}
}

func TestDefaultStateDir(t *testing.T) {
	for _, tc := range []struct{ xdg, offDarwin string }{
		{"", "/Users/x/.local/share/llmkube/metal-agent"},
		{"/xdg", "/xdg/llmkube/metal-agent"},
	} {
		t.Setenv("XDG_DATA_HOME", tc.xdg)
		want := tc.offDarwin
		if runtime.GOOS == "darwin" {
			want = "/Users/x/Library/Application Support/llmkube/metal-agent"
		}
		if got, err := resolveStateDir("", "/Users/x"); err != nil || got != want {
			t.Errorf("resolveStateDir default with XDG_DATA_HOME=%q = %q, %v; want %q", tc.xdg, got, err, want)
		}
	}
	if got, _ := resolveStateDir("/var/state", "/Users/x"); got != "/var/state" {
		t.Errorf("resolveStateDir explicit = %q, want /var/state", got)
	}
	if _, err := resolveStateDir("", ""); err == nil {
		t.Error("resolveStateDir with no home and no dir = nil error, want failure")
	}
}

// --- RelayNotAdopted watchdog

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func relayWatchdogFixture(
	t *testing.T, legacy bool, funcs *interceptor.Funcs, objs ...runtimeObject,
) (*MetalAgent, *record.FakeRecorder, *fakeClock) {
	t.Helper()
	isvc := refusalISVC("llm")
	all := append([]runtimeObject{isvc}, objs...)
	a, _, rec := refusalFixtureWithInterceptor(t, MetalAgentConfig{LegacyDirectEndpoints: legacy}, funcs, all...)
	if !legacy {
		if err := a.registry.EnableIngress(9443, testIngressPin); err != nil {
			t.Fatalf("EnableIngress: %v", err)
		}
	}
	clk := &fakeClock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	a.now = clk.now
	seedProcess(a, &ManagedProcess{Namespace: "default", Name: "llm", Port: 51234, Healthy: true, Runtime: "llamacpp"})
	return a, rec, clk
}

func controllerService(selector map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "llm", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Ports:    []corev1.ServicePort{{Name: "http", Port: 8080}},
			Selector: selector,
		},
	}
}

func relayNotAdoptedEvents(rec *record.FakeRecorder) []string {
	var out []string
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, EventReasonRelayNotAdopted) {
			out = append(out, e)
		}
	}
	return out
}

func TestRelayWatchdog_EmitsOnceForSelectorlessService(t *testing.T) {
	a, rec, clk := relayWatchdogFixture(t, false, nil, controllerService(nil))
	ctx := context.Background()

	a.heartbeatOnce(ctx) // first successful <isvc>-agent registration
	if err := a.config.K8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "llm-agent"},
		&discoveryv1.EndpointSlice{}); err != nil {
		t.Fatalf("heartbeat did not register llm-agent: %v", err)
	}
	if ev := relayNotAdoptedEvents(rec); len(ev) != 0 {
		t.Fatalf("events at registration = %v, want none", ev)
	}

	clk.t = clk.t.Add(4 * time.Minute)
	a.heartbeatOnce(ctx)
	if ev := relayNotAdoptedEvents(rec); len(ev) != 0 {
		t.Fatalf("events before 5 minutes = %v, want none", ev)
	}

	clk.t = clk.t.Add(time.Minute + time.Second)
	a.heartbeatOnce(ctx)
	ev := relayNotAdoptedEvents(rec)
	if len(ev) != 1 {
		t.Fatalf("events after 5 minutes = %v, want exactly one", ev)
	}
	want := "Warning RelayNotAdopted the controller has not adopted Service default/llm for the relay; " +
		"upgrade the LLMKube controller, or run the agent with --legacy-direct-endpoints"
	if ev[0] != want {
		t.Errorf("event = %q\nwant    %q", ev[0], want)
	}

	clk.t = clk.t.Add(10 * time.Minute)
	a.heartbeatOnce(ctx)
	if ev := relayNotAdoptedEvents(rec); len(ev) != 0 {
		t.Errorf("repeat events = %v, want none (once per ISVC per process)", ev)
	}
}

func TestRelayWatchdog_EmitsWhenServiceMissing(t *testing.T) {
	a, rec, clk := relayWatchdogFixture(t, false, nil)
	ctx := context.Background()
	a.heartbeatOnce(ctx)
	clk.t = clk.t.Add(5*time.Minute + time.Second)
	a.heartbeatOnce(ctx)
	if ev := relayNotAdoptedEvents(rec); len(ev) != 1 {
		t.Errorf("events = %v, want exactly one", ev)
	}
}

func TestRelayWatchdog_SilentWhenAdopted(t *testing.T) {
	a, rec, clk := relayWatchdogFixture(t, false, nil,
		controllerService(map[string]string{"app.kubernetes.io/name": "llm-relay"}))
	ctx := context.Background()
	a.heartbeatOnce(ctx)
	clk.t = clk.t.Add(6 * time.Minute)
	a.heartbeatOnce(ctx)
	if ev := relayNotAdoptedEvents(rec); len(ev) != 0 {
		t.Errorf("events = %v, want none for an adopted Service", ev)
	}
}

func TestRelayWatchdog_SilentOnServiceGetError(t *testing.T) {
	funcs := &interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Service); ok && key.Name == "llm" {
				return apierrors.NewServiceUnavailable("apiserver down")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	a, rec, clk := relayWatchdogFixture(t, false, funcs)
	ctx := context.Background()
	a.heartbeatOnce(ctx)
	clk.t = clk.t.Add(6 * time.Minute)
	a.heartbeatOnce(ctx)
	if ev := relayNotAdoptedEvents(rec); len(ev) != 0 {
		t.Errorf("events = %v, want none on a non-NotFound Get error", ev)
	}
}

func TestRelayWatchdog_SilentInLegacyMode(t *testing.T) {
	a, rec, clk := relayWatchdogFixture(t, true, nil)
	ctx := context.Background()
	a.heartbeatOnce(ctx)
	clk.t = clk.t.Add(6 * time.Minute)
	a.heartbeatOnce(ctx)
	if ev := relayNotAdoptedEvents(rec); len(ev) != 0 {
		t.Errorf("events = %v, want none in legacy mode", ev)
	}
}

// --state-dir resolves a leading "~" against the agent's home and makes a
// relative path absolute, so the identity never lands under a literal "~"
// directory or wherever launchd's working directory happens to be.
func TestResolveStateDir_TildeAndRelative(t *testing.T) {
	cases := []struct{ in, want string }{
		{"~", "/Users/x"},
		{"~/state", "/Users/x/state"},
		{"~/a/../b", "/Users/x/b"},
	}
	for _, tc := range cases {
		got, err := resolveStateDir(tc.in, "/Users/x")
		if err != nil || got != tc.want {
			t.Errorf("resolveStateDir(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveStateDir("rel/state", "/Users/x")
	if err != nil || got != filepath.Join(wd, "rel", "state") {
		t.Errorf("resolveStateDir(rel/state) = %q, %v; want %q", got, err, filepath.Join(wd, "rel", "state"))
	}
	if _, err := resolveStateDir("~/state", ""); err == nil {
		t.Error("resolveStateDir(~/state) with no home = nil error, want failure")
	}
	// "~user" is not expanded; it is a relative path like any other.
	if got, _ := resolveStateDir("~other/state", "/Users/x"); got != filepath.Join(wd, "~other", "state") {
		t.Errorf("resolveStateDir(~other/state) = %q, want it treated as relative", got)
	}
}

// The startup exposure probe also covers the oMLX daemon: one answering on
// the host IP bypasses the ingress, and the warning names the fix. A closed
// port stays silent.
func TestWarnIfDaemonsExposed_OMLX(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	core, logs := observer.New(zap.WarnLevel)
	a := NewMetalAgent(MetalAgentConfig{Logger: zap.New(core).Sugar(), OMLXPort: port})
	a.registry = NewServiceRegistry(nil, "127.0.0.1", newNopLogger(), "")
	a.executors[runtimeOMLX] = &recordingExecutor{}

	a.warnIfDaemonsExposed()
	entries := logs.FilterMessageSnippet("oMLX").All()
	if len(entries) != 1 {
		t.Fatalf("oMLX exposure warnings = %d, want 1 (all: %v)", len(entries), logs.All())
	}
	if !strings.Contains(entries[0].Message, "restart the oMLX daemon") {
		t.Errorf("warning %q does not name the fix", entries[0].Message)
	}

	_ = ln.Close()
	logs.TakeAll()
	a.warnIfDaemonsExposed()
	if n := logs.FilterMessageSnippet("oMLX").Len(); n != 0 {
		t.Errorf("closed oMLX port still warned %d time(s)", n)
	}
}

// TestMetalNodeReportAdvertisesIngressOnceBound: the MetalNode report carries
// the ingress pin and port only once the port is bound, and what it then
// advertises answers TLS with that pin. Advertised before the bind, the
// controller's dial-back hit a closed port after every agent restart and
// marked the Mac unreachable.
func TestMetalNodeReportAdvertisesIngressOnceBound(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	a, _, _ := refusalFixture(t, MetalAgentConfig{IngressPort: port, StateDir: t.TempDir()}, refusalISVC("llm"))
	srv, err := a.prepareIngress()
	if err != nil || srv == nil {
		t.Fatalf("prepareIngress: srv %v, err %v", srv, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if got := a.metalNodeReport(ctx); got.IngressSPKI != "" || got.IngressPort != 0 {
		t.Fatalf("report before the ingress is bound = pin %q port %d, want neither", got.IngressSPKI, got.IngressPort)
	}

	ln, err := a.listenIngress(srv)
	if err != nil {
		t.Fatalf("listenIngress: %v", err)
	}
	got := a.metalNodeReport(ctx)
	if got.IngressSPKI != a.ingressPin || int(got.IngressPort) != port {
		t.Fatalf("report once bound = pin %q port %d, want %q and %d", got.IngressSPKI, got.IngressPort, a.ingressPin, port)
	}
	fatal := make(chan error, 1)
	go a.runIngress(ctx, srv, ln, fatal)

	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(int(got.IngressPort))),
		&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) //nolint:gosec // the pin below is the check
	if err != nil {
		t.Fatalf("TLS to the advertised port: %v", err)
	}
	defer func() { _ = conn.Close() }()
	sum := sha256.Sum256(conn.ConnectionState().PeerCertificates[0].RawSubjectPublicKeyInfo)
	if pin := base64.StdEncoding.EncodeToString(sum[:]); pin != got.IngressSPKI {
		t.Errorf("the advertised port presents SPKI %q, want the advertised pin %q", pin, got.IngressSPKI)
	}
	// The reported certificate is the one the port presents: the verifier
	// trusts exactly it.
	presented := base64.StdEncoding.EncodeToString(conn.ConnectionState().PeerCertificates[0].Raw)
	if presented != got.IngressCert {
		t.Errorf("the advertised port presents a certificate other than the reported one")
	}
}

// TestListenIngressBindFailureAdvertisesNothing: when the ingress port is
// taken, the agent fails and its report never claims the port.
func TestListenIngressBindFailureAdvertisesNothing(t *testing.T) {
	// The same all-interfaces bind listenIngress makes, so it collides.
	taken, err := net.Listen("tcp", ":0") //nolint:gosec // G102: the port is held only for this test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	port := taken.Addr().(*net.TCPAddr).Port

	a, _, _ := refusalFixture(t, MetalAgentConfig{IngressPort: port, StateDir: t.TempDir()}, refusalISVC("llm"))
	srv, err := a.prepareIngress()
	if err != nil || srv == nil {
		t.Fatalf("prepareIngress: srv %v, err %v", srv, err)
	}
	if _, err := a.listenIngress(srv); err == nil {
		t.Fatal("listenIngress on a taken port succeeded, want an error")
	}
	if got := a.metalNodeReport(context.Background()); got.IngressSPKI != "" || got.IngressPort != 0 {
		t.Errorf("report after a failed bind = pin %q port %d, want neither", got.IngressSPKI, got.IngressPort)
	}
}
