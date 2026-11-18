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
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/agent/ingress"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

// The Mac of #2399: a LAN address, a second LAN address, and a Tailscale
// address. selectHostIP picks the Tailscale one; a cluster on the LAN cannot
// reach it.
var lanAndTailnetIfaces = []hostIPCandidate{
	cand("en0", "192.168.1.47"),
	cand("en1", "10.0.0.47"),
	cand("utun4", "100.64.0.2"),
}

// lanCandidates is what buildMetalNodeCandidates reports for those
// interfaces with the API server on the LAN: 192.168.1.47, 10.0.0.47,
// 100.64.0.2.
func lanCandidates() []metalnode.Candidate {
	return buildMetalNodeCandidates(candidateEnv{
		serverIP: net.ParseIP("192.168.1.10"),
		sourceIP: net.ParseIP("192.168.1.47"),
		ifaces:   lanAndTailnetIfaces,
	})
}

const (
	hostIPSwitchMsg = "host IP for the agent's endpoints changed; re-registering them"
	hostIPKeptMsg   = "keeping the last verified host IP after verification was withdrawn"
)

// hostIPFixture is a relay-mode agent with --metal-node mac-a (and
// --host-ip hostIP when set) on the #2399 Mac, the reporter it would start,
// the client and the logs of the agent and its registry. The reporter
// reports cands, the Mac's interfaces are ifaces (setIfaces), the
// reporter's clock is *now, and InferenceService macs/llm exists.
type hostIPFixture struct {
	a     *MetalAgent
	r     *metalNodeReporter
	c     client.Client
	rec   *verbRecorder
	logs  *observer.ObservedLogs
	now   *time.Time
	id    *ingress.Identity
	isvc  *inferencev1alpha1.InferenceService
	ctx   context.Context
	cands []metalnode.Candidate

	mu     sync.Mutex
	ifaces []hostIPCandidate
}

// setIfaces changes the Mac's interfaces; safe while the heartbeat loop runs.
func (f *hostIPFixture) setIfaces(ifaces ...hostIPCandidate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ifaces = ifaces
}

func newHostIPFixture(t *testing.T, hostIP string, node func(pin string) *inferencev1alpha1.MetalNode) *hostIPFixture {
	t.Helper()
	id, err := ingress.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "llm", Namespace: "macs"}}
	objs := []client.Object{adminLease("mac-a"), isvc}
	if node != nil {
		objs = append(objs, node(id.Pin()))
	}
	c, rec := recordingClient(t, objs...)
	core, logs := observer.New(zapcore.InfoLevel)
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient: c, Namespace: "macs", MetalNode: "mac-a", HostIP: hostIP, IngressPort: 9443,
		Version: "0.11.0", StateDir: t.TempDir(), ModelStorePath: t.TempDir(), MemoryFraction: 0.5,
		Logger: zap.New(core).Sugar(),
	})
	a.ingressPin, a.ingressIdentity = id.Pin(), id
	a.ingressListening.Store(true)
	f := &hostIPFixture{a: a, c: c, rec: rec, logs: logs, id: id, isvc: isvc, ctx: context.Background(),
		cands: lanCandidates(), ifaces: lanAndTailnetIfaces}
	a.registry = NewServiceRegistry(c, hostIP, zap.New(core).Sugar(), "")
	a.registry.interfaces = func() []hostIPCandidate {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.ifaces
	}
	if err := a.registry.EnableIngress(9443, id.Pin()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	f.now = &now
	f.r = a.newMetalNodeReporter()
	f.r.now = func() time.Time { return *f.now }
	f.r.report = func(ctx context.Context) metalnode.Report {
		report := a.metalNodeReport(ctx)
		report.Candidates = f.cands
		return report
	}
	return f
}

// verifiedNode is MetalNode macs/mac-a as the controller leaves it after a
// dial-back verified hostIP for the agent's pin and port 9443.
func verifiedNode(hostIP string) func(pin string) *inferencev1alpha1.MetalNode {
	return func(pin string) *inferencev1alpha1.MetalNode {
		mn := testMetalNode(testMetalNodeUID)
		mn.Status = inferencev1alpha1.MetalNodeStatus{HostIP: hostIP, IngressSPKI: pin, IngressPort: 9443}
		setReachable(mn, metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonDialBackSucceeded)
		return mn
	}
}

func setReachable(mn *inferencev1alpha1.MetalNode, status metav1.ConditionStatus, reason string) {
	meta.SetStatusCondition(&mn.Status.Conditions, metav1.Condition{
		Type: inferencev1alpha1.MetalNodeConditionReachable, Status: status, Reason: reason, Message: reason,
	})
}

// sliceAddress is the address on the agent's EndpointSlice macs/llm-agent.
func (f *hostIPFixture) sliceAddress(t *testing.T) string {
	t.Helper()
	slice := &discoveryv1.EndpointSlice{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Namespace: "macs", Name: "llm-agent"}, slice); err != nil {
		t.Fatalf("get the agent's EndpointSlice: %v", err)
	}
	if len(slice.Endpoints) != 1 || len(slice.Endpoints[0].Addresses) != 1 {
		t.Fatalf("EndpointSlice endpoints = %+v, want one address", slice.Endpoints)
	}
	return slice.Endpoints[0].Addresses[0]
}

// renewAndRegister runs one renewal and then registers macs/llm, as an
// engine start after it would.
func (f *hostIPFixture) renewAndRegister(t *testing.T) string {
	t.Helper()
	f.r.renew(f.ctx)
	if err := f.a.registry.RegisterEndpoint(f.ctx, f.isvc, 51234); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}
	return f.sliceAddress(t)
}

// updateStatus rewrites MetalNode macs/mac-a's status, as the controller
// would.
func (f *hostIPFixture) updateStatus(t *testing.T, mutate func(mn *inferencev1alpha1.MetalNode)) {
	t.Helper()
	mn := &inferencev1alpha1.MetalNode{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Namespace: "macs", Name: "mac-a"}, mn); err != nil {
		t.Fatal(err)
	}
	mutate(mn)
	if err := f.c.Update(f.ctx, mn); err != nil {
		t.Fatal(err)
	}
}

func (f *hostIPFixture) switchLogs() []observer.LoggedEntry {
	return f.logs.FilterMessage(hostIPSwitchMsg).All()
}

func (f *hostIPFixture) keptLogs() []observer.LoggedEntry {
	return f.logs.FilterMessage(hostIPKeptMsg).All()
}

// renewAndReregister is renewAndRegister one renewal interval later: the
// registration stands in for the heartbeat pass a switch wakes.
func (f *hostIPFixture) renewAndReregister(t *testing.T) string {
	t.Helper()
	*f.now = f.now.Add(metalNodeRenewInterval)
	return f.renewAndRegister(t)
}

// withdraw clears the verification as the controller does after a failed
// recheck or once the result expires.
func withdraw(reason string) func(mn *inferencev1alpha1.MetalNode) {
	return func(mn *inferencev1alpha1.MetalNode) {
		mn.Status.HostIP = ""
		status := metav1.ConditionFalse
		if reason == inferencev1alpha1.MetalNodeReasonVerificationPending {
			status = metav1.ConditionUnknown
		}
		setReachable(mn, status, reason)
	}
}

// TestMetalNodeVerifiedHostIPIsRegistered is #2399: the MetalNode's dial-back
// verified the Mac's LAN address, the Mac also has a Tailscale address, and
// no --host-ip is set. The agent's EndpointSlice, which the relay dials,
// must carry the verified address, not the Tailscale one auto-detect picks.
func TestMetalNodeVerifiedHostIPIsRegistered(t *testing.T) {
	f := newHostIPFixture(t, "", verifiedNode("192.168.1.47"))
	if got := f.renewAndRegister(t); got != "192.168.1.47" {
		t.Errorf("EndpointSlice address = %s, want the MetalNode's verified status.hostIP 192.168.1.47 "+
			"(auto-detect alone picks the Tailscale address 100.64.0.2)", got)
	}
}

// TestMetalNodeVerifiedHostIPWinsOverDialBackOrder: when the dial-back's
// first candidate failed and a later one verified, the verified one is
// registered, not the first one tried nor the auto-detected one.
func TestMetalNodeVerifiedHostIPWinsOverDialBackOrder(t *testing.T) {
	f := newHostIPFixture(t, "", verifiedNode("10.0.0.47"))
	if got := f.renewAndRegister(t); got != "10.0.0.47" {
		t.Errorf("EndpointSlice address = %s, want the verified 10.0.0.47 "+
			"(not the first candidate 192.168.1.47 nor the auto-detected 100.64.0.2)", got)
	}
	if got := f.rec.callsOn("*v1alpha1.MetalNode"); len(got) != 1 {
		t.Errorf("MetalNode calls = %v, want one get for the one renewal", got)
	}
}

// TestMetalNodeExplicitHostIPWins: --host-ip is registered verbatim whatever
// the MetalNode verified, and the reporter reads the MetalNode exactly as it
// did before (for the certificate binding: once, then every
// metalNodeBindRecheck).
func TestMetalNodeExplicitHostIPWins(t *testing.T) {
	f := newHostIPFixture(t, "10.9.8.7", verifiedNode("192.168.1.47"))
	if f.r.follow != nil {
		t.Fatal("the reporter follows the MetalNode's address with --host-ip set")
	}
	if got := f.renewAndRegister(t); got != "10.9.8.7" {
		t.Errorf("EndpointSlice address = %s, want --host-ip 10.9.8.7", got)
	}
	for range 3 {
		*f.now = f.now.Add(metalNodeRenewInterval)
		f.r.renew(f.ctx)
	}
	if got := f.rec.callsOn("*v1alpha1.MetalNode"); len(got) != 1 {
		t.Errorf("MetalNode calls over a minute of renewals with --host-ip = %v, want one get, as before", got)
	}
	if n := len(f.switchLogs()); n != 0 {
		t.Errorf("%d host-IP switch logs with --host-ip, want none", n)
	}
}

// TestNoMetalNodeKeepsAutoDetect: without --metal-node nothing follows a
// MetalNode, and the registered address is auto-detect's, as before; legacy
// mode (no ingress for the dial-back to verify) does not follow either.
func TestNoMetalNodeKeepsAutoDetect(t *testing.T) {
	f := newHostIPFixture(t, "", verifiedNode("192.168.1.47"))
	f.a.config.MetalNode = ""
	if r := f.a.newMetalNodeReporter(); r != nil {
		t.Fatal("a reporter exists without --metal-node")
	}
	if err := f.a.registry.RegisterEndpoint(f.ctx, f.isvc, 51234); err != nil {
		t.Fatal(err)
	}
	if got := f.sliceAddress(t); got != "100.64.0.2" {
		t.Errorf("EndpointSlice address without --metal-node = %s, want auto-detect's 100.64.0.2", got)
	}
	if got := f.rec.callsOn("*v1alpha1.MetalNode"); len(got) != 0 {
		t.Errorf("MetalNode calls without --metal-node = %v, want none", got)
	}

	legacy := newHostIPFixture(t, "", verifiedNode("192.168.1.47"))
	legacy.a.config.LegacyDirectEndpoints = true
	legacy.a.ingressPin = ""
	legacy.a.registry = NewServiceRegistry(legacy.c, "", newNopLogger(), "")
	if r := legacy.a.newMetalNodeReporter(); r == nil || r.follow != nil {
		t.Errorf("legacy mode reporter = %+v, want one that does not follow the MetalNode's address", r)
	}
}

// TestMetalNodeUnverifiedRegistersFirstDialBackAddress: with no verified
// address for this agent's ingress, the agent registers the first address
// the dial-back tries (spec.hostIP, else its first reported candidate), not
// status.hostIP and not auto-detect's pick; with no candidate either, it
// auto-detects.
func TestMetalNodeUnverifiedRegistersFirstDialBackAddress(t *testing.T) {
	unverified := func(mutate func(mn *inferencev1alpha1.MetalNode)) func(pin string) *inferencev1alpha1.MetalNode {
		return func(pin string) *inferencev1alpha1.MetalNode {
			mn := verifiedNode("10.0.0.47")(pin)
			mutate(mn)
			return mn
		}
	}
	for _, tc := range []struct {
		name  string
		node  func(pin string) *inferencev1alpha1.MetalNode
		cands []metalnode.Candidate
		want  string
	}{
		{"Reachable False with a stale hostIP", unverified(func(mn *inferencev1alpha1.MetalNode) {
			setReachable(mn, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonNoneReachable)
		}), lanCandidates(), "192.168.1.47"},
		{"Reachable Unknown", unverified(func(mn *inferencev1alpha1.MetalNode) {
			mn.Status.HostIP = ""
			setReachable(mn, metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonVerificationPending)
		}), lanCandidates(), "192.168.1.47"},
		{"no Reachable yet", unverified(func(mn *inferencev1alpha1.MetalNode) {
			mn.Status = inferencev1alpha1.MetalNodeStatus{}
		}), lanCandidates(), "192.168.1.47"},
		{"verified for another pin", unverified(func(mn *inferencev1alpha1.MetalNode) {
			mn.Status.IngressSPKI = testIngressPin
		}), lanCandidates(), "192.168.1.47"},
		{"verified for another port", unverified(func(mn *inferencev1alpha1.MetalNode) {
			mn.Status.IngressPort = 9444
		}), lanCandidates(), "192.168.1.47"},
		{"verified an IPv6 address", unverified(func(mn *inferencev1alpha1.MetalNode) {
			mn.Status.HostIP = "fd00::47"
		}), lanCandidates(), "192.168.1.47"},
		{"spec.hostIP is tried first", unverified(func(mn *inferencev1alpha1.MetalNode) {
			mn.Spec.HostIP = "10.0.0.47"
			mn.Status.HostIP = ""
			setReachable(mn, metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonVerificationPending)
		}), lanCandidates(), "10.0.0.47"},
		{"the MetalNode does not exist", nil, lanCandidates(), "192.168.1.47"},
		{"no candidate either", unverified(func(mn *inferencev1alpha1.MetalNode) {
			setReachable(mn, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonNoneReachable)
		}), nil, "100.64.0.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHostIPFixture(t, "", tc.node)
			f.cands = tc.cands
			if got := f.renewAndRegister(t); got != tc.want {
				t.Errorf("EndpointSlice address = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestMetalNodeHostIPChangeReregisters: when the verified address appears,
// changes, or is withdrawn and then leaves the Mac's interfaces, the
// heartbeat loop re-registers the running endpoints at once (its tick is 30
// seconds away) with the new address, and each switch is logged once,
// naming both addresses and why. A withdrawal alone, with the address still
// local, switches nothing.
func TestMetalNodeHostIPChangeReregisters(t *testing.T) {
	f := newHostIPFixture(t, "", func(pin string) *inferencev1alpha1.MetalNode {
		mn := testMetalNode(testMetalNodeUID)
		setReachable(mn, metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonVerificationPending)
		return mn
	})
	f.cands = nil // auto-detect when nothing is verified: 100.64.0.2
	seedProcess(f.a, &ManagedProcess{Namespace: "macs", Name: "llm", Port: 51234, Healthy: true, Runtime: "llamacpp"})
	if err := f.a.registry.RegisterEndpoint(f.ctx, f.isvc, 51234); err != nil {
		t.Fatal(err)
	}
	if got := f.sliceAddress(t); got != "100.64.0.2" {
		t.Fatalf("EndpointSlice address before any verification = %s, want auto-detect's 100.64.0.2", got)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() {
		f.a.runHeartbeatLoop(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	steps := []struct {
		name, from, to, reason string
		status                 func(mn *inferencev1alpha1.MetalNode)
		ifaces                 []hostIPCandidate
		switches               int
	}{
		{"appears", "100.64.0.2", "10.0.0.47", "verified it", func(mn *inferencev1alpha1.MetalNode) {
			mn.Status = verifiedNode("10.0.0.47")(f.id.Pin()).Status
		}, nil, 1},
		{"changes", "10.0.0.47", "192.168.1.47", "verified it", func(mn *inferencev1alpha1.MetalNode) {
			mn.Status.HostIP = "192.168.1.47"
		}, nil, 2},
		{"is withdrawn, still local", "", "192.168.1.47", "",
			withdraw(inferencev1alpha1.MetalNodeReasonNoneReachable), nil, 2},
		{"left the interfaces", "192.168.1.47", "100.64.0.2", "192.168.1.47 is no longer on any of this host's interfaces",
			nil, []hostIPCandidate{cand("en1", "10.0.0.47"), cand("utun4", "100.64.0.2")}, 3},
	}
	for _, step := range steps {
		if step.status != nil {
			f.updateStatus(t, step.status)
		}
		if step.ifaces != nil {
			f.setIfaces(step.ifaces...)
		}
		*f.now = f.now.Add(metalNodeRenewInterval)
		f.r.renew(f.ctx)
		deadline := time.Now().Add(5 * time.Second)
		for f.sliceAddress(t) != step.to {
			if time.Now().After(deadline) {
				t.Fatalf("verified address %s: EndpointSlice address = %s 5s after the renewal, want %s "+
					"(re-registered without waiting for the heartbeat tick)", step.name, f.sliceAddress(t), step.to)
			}
			time.Sleep(10 * time.Millisecond)
		}
		// More renewals with the same status switch nothing.
		for range 2 {
			*f.now = f.now.Add(metalNodeRenewInterval)
			f.r.renew(f.ctx)
		}
		if got := f.sliceAddress(t); got != step.to {
			t.Fatalf("verified address %s: EndpointSlice address = %s after more renewals, want %s", step.name, got, step.to)
		}
		logs := f.switchLogs()
		if len(logs) != step.switches {
			t.Fatalf("verified address %s: %d switch logs, want %d (one per switch)", step.name, len(logs), step.switches)
		}
		if step.from == "" {
			continue
		}
		fields := logs[len(logs)-1].ContextMap()
		if fields["from"] != step.from || fields["to"] != step.to ||
			!strings.Contains(fields["reason"].(string), step.reason) {
			t.Errorf("verified address %s: switch logged %v, want from %s to %s with a reason naming %q",
				step.name, fields, step.from, step.to, step.reason)
		}
	}
}

// TestMetalNodeKeepsLastVerifiedHostIPWhileLocal: once an address has been
// verified, a withdrawn verification (a failed recheck, then the expired
// result) does not move the endpoints to the first address the dial-back
// tries, which may be the one that failed: the verified address stays while
// it is on one of the Mac's interfaces, and keeping it is logged once.
func TestMetalNodeKeepsLastVerifiedHostIPWhileLocal(t *testing.T) {
	f := newHostIPFixture(t, "", verifiedNode("10.0.0.47"))
	if got := f.renewAndRegister(t); got != "10.0.0.47" {
		t.Fatalf("EndpointSlice address = %s, want the verified 10.0.0.47", got)
	}
	for _, reason := range []string{
		inferencev1alpha1.MetalNodeReasonNoneReachable,
		inferencev1alpha1.MetalNodeReasonVerificationPending,
	} {
		f.updateStatus(t, withdraw(reason))
		for range 3 {
			if got := f.renewAndReregister(t); got != "10.0.0.47" {
				t.Fatalf("after the verification was withdrawn (%s): EndpointSlice address = %s, "+
					"want the last verified 10.0.0.47 kept (not the first candidate 192.168.1.47)", reason, got)
			}
		}
	}
	kept := f.keptLogs()
	if len(kept) != 1 {
		t.Fatalf("%d logs of keeping the last verified address over 6 renewals, want 1", len(kept))
	}
	if fields := kept[0].ContextMap(); fields["hostIP"] != "10.0.0.47" ||
		!strings.Contains(fields["reason"].(string), "Reachable=False (NoneReachable)") {
		t.Errorf("kept-address log = %v, want hostIP 10.0.0.47 and the withdrawal's reason", fields)
	}
	if n := len(f.switchLogs()); n != 1 {
		t.Errorf("%d switch logs, want only the first verification's", n)
	}

	// Verified again and withdrawn again: a new withdrawal, logged again.
	f.updateStatus(t, func(mn *inferencev1alpha1.MetalNode) {
		mn.Status = verifiedNode("10.0.0.47")(f.id.Pin()).Status
	})
	f.renewAndReregister(t)
	f.updateStatus(t, withdraw(inferencev1alpha1.MetalNodeReasonNoneReachable))
	if got := f.renewAndReregister(t); got != "10.0.0.47" {
		t.Fatalf("after a second withdrawal: EndpointSlice address = %s, want 10.0.0.47 kept", got)
	}
	if n := len(f.keptLogs()); n != 2 {
		t.Errorf("%d kept-address logs after a second withdrawal, want 2", n)
	}
}

// TestMetalNodeLastVerifiedHostIPLeftInterfacesFallsBack: a withdrawn
// verified address that is no longer on any of the Mac's interfaces (a
// network change, a new DHCP lease) is not kept: the endpoints move to the
// first address the dial-back tries now.
func TestMetalNodeLastVerifiedHostIPLeftInterfacesFallsBack(t *testing.T) {
	f := newHostIPFixture(t, "", verifiedNode("192.168.1.47"))
	if got := f.renewAndRegister(t); got != "192.168.1.47" {
		t.Fatalf("EndpointSlice address = %s, want the verified 192.168.1.47", got)
	}
	f.updateStatus(t, withdraw(inferencev1alpha1.MetalNodeReasonVerificationPending))
	if got := f.renewAndReregister(t); got != "192.168.1.47" {
		t.Fatalf("withdrawn, still on en0: EndpointSlice address = %s, want 192.168.1.47 kept", got)
	}

	// en0 moved to another network: 192.168.1.47 is gone, and the agent now
	// reports the candidates for the interfaces it has.
	f.setIfaces(cand("en0", "192.168.7.20"), cand("en1", "10.0.0.47"), cand("utun4", "100.64.0.2"))
	f.cands = buildMetalNodeCandidates(candidateEnv{
		serverIP: net.ParseIP("10.0.0.1"), sourceIP: net.ParseIP("10.0.0.47"), ifaces: f.ifaces,
	})
	if got := f.renewAndReregister(t); got != "10.0.0.47" {
		t.Fatalf("withdrawn and no longer local: EndpointSlice address = %s, want the first dial-back candidate 10.0.0.47",
			got)
	}
	logs := f.switchLogs()
	if len(logs) == 0 {
		t.Fatal("no switch log for the move off the address that left the interfaces")
	}
	fields := logs[len(logs)-1].ContextMap()
	if fields["from"] != "192.168.1.47" || fields["to"] != "10.0.0.47" ||
		!strings.Contains(fields["reason"].(string), "192.168.1.47 is no longer on any of this host's interfaces") {
		t.Errorf("switch logged %v, want 192.168.1.47 -> 10.0.0.47 naming the address that left", fields)
	}

	// The address is forgotten: it coming back does not bring it back
	// unverified.
	f.setIfaces(lanAndTailnetIfaces...)
	if got := f.renewAndReregister(t); got != "10.0.0.47" {
		t.Errorf("after 192.168.1.47 returned unverified: EndpointSlice address = %s, want the first candidate 10.0.0.47",
			got)
	}
}

// TestMetalNodeNewlyVerifiedHostIPWinsOverKept: while a withdrawn address is
// kept, a different address the dial-back verifies takes over.
func TestMetalNodeNewlyVerifiedHostIPWinsOverKept(t *testing.T) {
	f := newHostIPFixture(t, "", verifiedNode("10.0.0.47"))
	f.renewAndRegister(t)
	f.updateStatus(t, withdraw(inferencev1alpha1.MetalNodeReasonNoneReachable))
	if got := f.renewAndReregister(t); got != "10.0.0.47" {
		t.Fatalf("withdrawn: EndpointSlice address = %s, want 10.0.0.47 kept", got)
	}
	f.updateStatus(t, func(mn *inferencev1alpha1.MetalNode) {
		mn.Status = verifiedNode("192.168.1.47")(f.id.Pin()).Status
	})
	if got := f.renewAndReregister(t); got != "192.168.1.47" {
		t.Fatalf("a different address verified: EndpointSlice address = %s, want the newly verified 192.168.1.47", got)
	}
	logs := f.switchLogs()
	fields := logs[len(logs)-1].ContextMap()
	if fields["from"] != "10.0.0.47" || fields["to"] != "192.168.1.47" ||
		!strings.Contains(fields["reason"].(string), "verified it") {
		t.Errorf("switch logged %v, want 10.0.0.47 -> 192.168.1.47 because it was verified", fields)
	}
}

// TestMetalNodeReadErrorKeepsHostIP: a MetalNode read that fails says
// nothing about the verified address, so the endpoints keep it; the failure
// is warned about at most once a minute.
func TestMetalNodeReadErrorKeepsHostIP(t *testing.T) {
	f := newHostIPFixture(t, "", verifiedNode("10.0.0.47"))
	if got := f.renewAndRegister(t); got != "10.0.0.47" {
		t.Fatalf("EndpointSlice address = %s, want the verified 10.0.0.47", got)
	}
	core, warns := observer.New(zapcore.WarnLevel)
	f.r.logger = zap.New(core).Sugar()
	f.r.client = failingMetalNodeGets{f.c}
	for range 3 {
		*f.now = f.now.Add(metalNodeRenewInterval)
		if got := f.renewAndRegister(t); got != "10.0.0.47" {
			t.Fatalf("EndpointSlice address after a failed MetalNode read = %s, want the verified 10.0.0.47 kept", got)
		}
	}
	if n := warns.FilterMessageSnippet("cannot read the MetalNode").Len(); n != 1 {
		t.Errorf("%d warnings over 45s of failed MetalNode reads, want 1", n)
	}
}

// failingMetalNodeGets fails every MetalNode get and passes the rest through.
type failingMetalNodeGets struct{ client.Client }

func (c failingMetalNodeGets) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if _, ok := obj.(*inferencev1alpha1.MetalNode); ok {
		return errors.New("the API server is unavailable")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
