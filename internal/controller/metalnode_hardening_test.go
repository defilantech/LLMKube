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

package controller

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

// holdVerifiers puts a finalizer on every verifier pod, standing in for the
// kubelet's grace period: a deleted pod stays, Terminating, until released.
func (f *metalNodeFixture) holdVerifiers(name string) {
	f.t.Helper()
	for _, p := range f.verifierPods(name) {
		if len(p.Finalizers) == 0 && p.DeletionTimestamp == nil {
			p.Finalizers = []string{"test.llmkube.dev/hold"}
			if err := f.c.Update(context.Background(), &p); err != nil {
				f.t.Fatalf("hold verifier pod %s: %v", p.Name, err)
			}
		}
	}
}

func (f *metalNodeFixture) releaseVerifiers(name string) {
	f.t.Helper()
	for _, p := range f.verifierPods(name) {
		p.Finalizers = nil
		if err := f.c.Update(context.Background(), &p); err != nil && !apierrors.IsNotFound(err) {
			f.t.Fatalf("release verifier pod %s: %v", p.Name, err)
		}
	}
}

// TestMetalNodeVerifierFloodBounded is the review's C1 repro: a Mac that
// rewrites its report in a loop must not get a verifier pod per rewrite.
// Old pods are held Terminating, as the kubelet would; at most one verifier
// may exist at any time, and starts are at least the spacing apart.
func TestMetalNodeVerifierFloodBounded(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-flood", nil)
	t.Cleanup(func() { f.releaseVerifiers("mac-flood") })

	started := map[string]time.Time{}
	record := func() {
		for _, p := range f.verifierPods("mac-flood") {
			if _, ok := started[string(p.UID)]; !ok {
				started[string(p.UID)] = f.now
			}
		}
	}
	for i := range 8 {
		// Half the rewrites change only a NotAllowed address or a source
		// string, half change a dialable address.
		report := testReport(
			metalnode.Candidate{Address: fmt.Sprintf("100.64.0.%d", 2+i/2), Source: metalnode.SourceTailnet},
			metalnode.Candidate{Address: fmt.Sprintf("203.0.113.%d", i+1), Source: fmt.Sprintf("lan-%d", i)},
		)
		f.putLease("mac-flood", f.now, encodeReport(t, report))
		f.reconcile("mac-flood")
		record()
		f.holdVerifiers("mac-flood")
		if n := len(f.verifierPods("mac-flood")); n > 1 {
			t.Fatalf("rewrite %d: %d verifier pods exist at once for one MetalNode, want at most 1", i, n)
		}
		f.now = f.now.Add(2 * time.Second)
	}
	if len(started) != 1 {
		t.Errorf("%d verifier pods started during 16s of report churn, want 1", len(started))
	}

	// Once the held pod is gone, a new one may start, but not before the
	// spacing since the last start has passed.
	f.releaseVerifiers("mac-flood")
	f.reconcile("mac-flood")
	if n := len(f.verifierPods("mac-flood")); n != 0 {
		t.Fatalf("a verifier started %s after the previous start, want none before %s", 16*time.Second, metalNodeMinStartSpacing)
	}
	f.now = f.now.Add(metalNodeMinStartSpacing)
	f.reconcile("mac-flood")
	record()
	if n := len(f.verifierPods("mac-flood")); n != 1 {
		t.Fatalf("%d verifier pods once the spacing passed, want 1", n)
	}

	// Past the spacing, new inputs still wait for the old pod to be gone:
	// it is deleted, and nothing starts while it is Terminating.
	f.holdVerifiers("mac-flood")
	f.now = f.now.Add(2 * metalNodeMinStartSpacing)
	f.putLease("mac-flood", f.now, encodeReport(t, testReport(
		metalnode.Candidate{Address: "100.64.0.99", Source: metalnode.SourceTailnet})))
	f.reconcile("mac-flood")
	pods := f.verifierPods("mac-flood")
	if len(pods) != 1 || pods[0].DeletionTimestamp == nil {
		t.Fatalf("%d verifier pods after new inputs with the old one held, want only the old one, Terminating", len(pods))
	}
	// The next reconcile sees the old pod already Terminating: it still
	// counts as busy, so nothing new starts.
	f.reconcile("mac-flood")
	if pods := f.verifierPods("mac-flood"); len(pods) != 1 || pods[0].DeletionTimestamp == nil {
		t.Fatalf("%d verifier pods on the reconcile after, with the old one Terminating, want only the old one", len(pods))
	}
	f.releaseVerifiers("mac-flood")
	f.reconcile("mac-flood")
	record()
	if n := len(f.verifierPods("mac-flood")); n != 1 {
		t.Fatalf("%d verifier pods once the old one was gone, want 1", n)
	}

	times := make([]time.Time, 0, len(started))
	for _, at := range started {
		times = append(times, at)
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < metalNodeMinStartSpacing {
			t.Errorf("verifier starts %s apart, want at least %s", gap, metalNodeMinStartSpacing)
		}
	}
}

// TestMetalNodeRewordedReportKeepsResult: a report that changes only source
// strings or addresses that are never dialed is not new input. The last
// result stands and no verifier starts, even after the spacing.
func TestMetalNodeRewordedReportKeepsResult(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-reword", nil)
	f.putLease("mac-reword", f.now, encodeReport(t, testReport(
		metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet},
		metalnode.Candidate{Address: "203.0.113.1", Source: metalnode.SourceLAN})))
	f.reconcile("mac-reword")
	f.finishVerifier(f.onlyVerifierPod("mac-reword"), "100.64.0.2 0\n")
	f.reconcile("mac-reword")

	f.now = f.now.Add(2 * metalNodeMinStartSpacing)
	f.putLease("mac-reword", f.now, encodeReport(t, testReport(
		metalnode.Candidate{Address: "100.64.0.2", Source: "vpn"},
		metalnode.Candidate{Address: "203.0.113.200", Source: metalnode.SourceLAN})))
	f.reconcile("mac-reword")
	if n := len(f.verifierPods("mac-reword")); n != 0 {
		t.Errorf("%d verifier pods after a reworded report, want 0", n)
	}
	mn := f.get("mac-reword")
	if r := candidateResults(mn)["100.64.0.2"]; r != inferencev1alpha1.CandidateReachable || mn.Status.HostIP != "100.64.0.2" {
		t.Errorf("after a reworded report: 100.64.0.2 = %s, hostIP %q; want the last result kept", r, mn.Status.HostIP)
	}
}

// TestMetalNodeInputHashIgnoresUndialedInputs: the hash that decides a new
// check covers only what changes the probe.
func TestMetalNodeInputHashIgnoresUndialedInputs(t *testing.T) {
	base := metalNodeInputHash(testIngressPin, "Y2VydA==", 9443, []string{"100.64.0.2", "10.0.0.5"}, nil)
	if got := metalNodeInputHash(testIngressPin, "Y2VydA==", 9443, []string{"100.64.0.2", "10.0.0.5"}, nil); got != base {
		t.Error("the same inputs hash differently")
	}
	for name, got := range map[string]string{
		"pin":       metalNodeInputHash("AAAAEjRWeJq83vEjRWeJq83vEjRWeJq83vEjRWeJq80=", "Y2VydA==", 9443, []string{"100.64.0.2", "10.0.0.5"}, nil),
		"cert":      metalNodeInputHash(testIngressPin, "b3RoZXI=", 9443, []string{"100.64.0.2", "10.0.0.5"}, nil),
		"port":      metalNodeInputHash(testIngressPin, "Y2VydA==", 9444, []string{"100.64.0.2", "10.0.0.5"}, nil),
		"addresses": metalNodeInputHash(testIngressPin, "Y2VydA==", 9443, []string{"100.64.0.2", "10.0.0.6"}, nil),
		"order":     metalNodeInputHash(testIngressPin, "Y2VydA==", 9443, []string{"10.0.0.5", "100.64.0.2"}, nil),
		"placement": metalNodeInputHash(testIngressPin, "Y2VydA==", 9443, []string{"100.64.0.2", "10.0.0.5"},
			&inferencev1alpha1.MetalNodeVerifier{NodeSelector: map[string]string{"a": "b"}}),
	} {
		if got == base {
			t.Errorf("changing the %s did not change the hash", name)
		}
	}

	// Through the candidate builder: a source string or a NotAllowed address
	// is not part of the hash.
	allowed := mustAllowedCIDRs(t, DefaultMetalNodeAllowedCIDRs)
	hashOf := func(c ...metalnode.Candidate) string {
		desired := metalNodeDesiredCandidates("", metalnode.Report{Candidates: c}, allowed)
		return metalNodeInputHash(testIngressPin, "Y2VydA==", 9443, dialableAddresses(desired), nil)
	}
	a := hashOf(metalnode.Candidate{Address: "100.64.0.2", Source: "tailnet"}, metalnode.Candidate{Address: "203.0.113.1", Source: "lan"})
	b := hashOf(metalnode.Candidate{Address: "100.64.0.2", Source: "other"}, metalnode.Candidate{Address: "203.0.113.9", Source: "lan"})
	if a != b {
		t.Error("a source string or a NotAllowed address changed the input hash")
	}
}

// TestMetalNodeLeaseUpdateFilter: a Lease update that only advances
// renewTime on schedule does not enqueue (the resync catches staleness); a
// changed report, label or holder, or a renewal after a gap, does.
func TestMetalNodeLeaseUpdateFilter(t *testing.T) {
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	lease := func(renew time.Time, report string) *coordinationv1.Lease {
		holder := "mac-a"
		rt := metav1.NewMicroTime(renew)
		return &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: "mac-a", Namespace: "macs",
				Labels:      map[string]string{inferencev1alpha1.LabelMetalNode: "mac-a"},
				Annotations: map[string]string{metalnode.ReportAnnotation: report}},
			Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, RenewTime: &rt},
		}
	}
	old := lease(at, `{"version":"1"}`)
	if metalNodeLeaseUpdateMatters(old, lease(at.Add(15*time.Second), `{"version":"1"}`)) {
		t.Error("an on-schedule renewal with the same report enqueued a reconcile")
	}
	if !metalNodeLeaseUpdateMatters(old, lease(at.Add(15*time.Second), `{"version":"2"}`)) {
		t.Error("a changed report did not enqueue")
	}
	if !metalNodeLeaseUpdateMatters(old, lease(at.Add(5*time.Minute), `{"version":"1"}`)) {
		t.Error("a renewal after a 5 minute gap (the agent came back) did not enqueue")
	}
	relabeled := lease(at.Add(15*time.Second), `{"version":"1"}`)
	relabeled.Labels = nil
	if !metalNodeLeaseUpdateMatters(old, relabeled) {
		t.Error("a label change did not enqueue")
	}
	never := lease(at, `{"version":"1"}`)
	never.Spec.RenewTime = nil
	if !metalNodeLeaseUpdateMatters(never, lease(at, `{"version":"1"}`)) {
		t.Error("the first renewal did not enqueue")
	}
}

// TestMetalNodeRBACCoversVerifierOwnerRef: the verifier pod's controller
// owner reference sets blockOwnerDeletion, which the
// OwnerReferencesPermissionEnforcement admission plugin (on by default in
// OpenShift) allows only to a creator that can update metalnodes/finalizers.
func TestMetalNodeRBACCoversVerifierOwnerRef(t *testing.T) {
	data, err := os.ReadFile("../../config/rbac/role.yaml")
	if err != nil {
		t.Fatalf("read config/rbac/role.yaml: %v", err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(data, role); err != nil {
		t.Fatalf("parse config/rbac/role.yaml: %v", err)
	}
	for _, want := range []struct{ group, resource, verb string }{
		{"inference.llmkube.dev", "metalnodes/finalizers", "update"},
		{"inference.llmkube.dev", "metalnodes/status", "patch"},
		{"coordination.k8s.io", "leases", "watch"},
		{"", "pods", "create"},
		{"", "pods", "delete"},
	} {
		if !clusterRoleGrants(role, want.group, want.resource, want.verb) {
			t.Errorf("generated ClusterRole does not grant %q on %s/%s; add the +kubebuilder:rbac marker and re-run make manifests",
				want.verb, want.group, want.resource)
		}
	}
}

func clusterRoleGrants(role *rbacv1.ClusterRole, group, resource, verb string) bool {
	has := func(list []string, v string) bool {
		for _, x := range list {
			if x == v || x == "*" {
				return true
			}
		}
		return false
	}
	for _, rule := range role.Rules {
		if has(rule.APIGroups, group) && has(rule.Resources, resource) && has(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

// TestMetalNodeStatusWrittenWhenVerifierCreateFails: a Forbidden pod create
// must not cost the MetalNode its status. The observed facts are stored and
// Reachable says the verifier failed, and the error still requeues.
func TestMetalNodeStatusWrittenWhenVerifierCreateFails(t *testing.T) {
	f := newMetalNodeFixture(t)
	wc, err := client.NewWithWatch(f.cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	f.r.Client = interceptor.NewClient(wc, interceptor.Funcs{
		Create: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Pod); ok {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, obj.GetName(),
					fmt.Errorf("cannot set blockOwnerDeletion"))
			}
			return w.Create(ctx, obj, opts...)
		},
	})
	f.createMetalNode("mac-forbidden", nil)
	f.putLease("mac-forbidden", f.now, encodeReport(t, testReport(metalnode.Candidate{Address: "100.64.0.2", Source: "tailnet"})))

	_, err = f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNS, Name: "mac-forbidden"}})
	if err == nil {
		t.Error("reconcile hid the failed pod create; want the error returned so it requeues")
	}
	mn := f.get("mac-forbidden")
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionAgentConnected, metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonHeartbeatFresh)
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonVerifierFailed)
	if mn.Status.AgentVersion != "0.11.0" || len(mn.Status.Candidates) != 1 {
		t.Errorf("status = %+v, want the report copied even though the verifier could not start", mn.Status)
	}
	if msg := condition(t, mn, inferencev1alpha1.MetalNodeConditionReachable).Message; msg == "" ||
		containsAny(msg, "blockOwnerDeletion", "forbidden") {
		t.Errorf("Reachable message %q: want a generic message, not the API error", msg)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestMetalNodeReachableClearedWhenAgentGone is the review's I2 repro: once
// the agent is not connected, Reachable is Unknown, hostIP is cleared, and
// verifier pods are removed.
func TestMetalNodeReachableClearedWhenAgentGone(t *testing.T) {
	f := newMetalNodeFixture(t)
	report := encodeReport(t, testReport(metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet}))
	f.createMetalNode("mac-gone", nil)
	f.putLease("mac-gone", f.now, report)
	f.reconcile("mac-gone")
	f.finishVerifier(f.onlyVerifierPod("mac-gone"), "100.64.0.2 0\n")
	f.reconcile("mac-gone")
	wantCondition(t, f.get("mac-gone"), inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonDialBackSucceeded)

	// A re-check is in flight when the agent's Lease disappears.
	f.now = f.now.Add(metalNodeRecheckInterval + time.Second)
	f.putLease("mac-gone", f.now, report)
	f.reconcile("mac-gone")
	f.onlyVerifierPod("mac-gone")
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "mac-gone", Namespace: testNS}}
	if err := f.c.Delete(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	f.reconcile("mac-gone")

	mn := f.get("mac-gone")
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionAgentConnected, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonLeaseNotFound)
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonAgentNotConnected)
	if mn.Status.HostIP != "" {
		t.Errorf("hostIP = %q with the agent gone, want empty", mn.Status.HostIP)
	}
	if pods := f.verifierPods("mac-gone"); len(pods) != 0 {
		t.Errorf("%d verifier pods left with the agent gone, want 0", len(pods))
	}

	// A stale heartbeat clears it the same way.
	f.createMetalNode("mac-stale", nil)
	f.putLease("mac-stale", f.now, report)
	f.reconcile("mac-stale")
	f.finishVerifier(f.onlyVerifierPod("mac-stale"), "100.64.0.2 0\n")
	f.reconcile("mac-stale")
	f.now = f.now.Add(time.Hour)
	f.reconcile("mac-stale")
	mn = f.get("mac-stale")
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonAgentNotConnected)
	if mn.Status.HostIP != "" {
		t.Errorf("hostIP = %q with a stale heartbeat, want empty", mn.Status.HostIP)
	}
}

// TestMetalNodeInvalidHostIP: spec.hostIP is validated by the controller, not
// by a CEL rule (isIP needs Kubernetes 1.31). A bad value is reported and
// nothing is dialed.
func TestMetalNodeInvalidHostIP(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-badip", func(s *inferencev1alpha1.MetalNodeSpec) { s.HostIP = "mac-a.local" })
	f.putLease("mac-badip", f.now, encodeReport(t, testReport(metalnode.Candidate{Address: "100.64.0.2", Source: "tailnet"})))
	f.reconcile("mac-badip")
	mn := f.get("mac-badip")
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonInvalidSpec)
	if msg := condition(t, mn, inferencev1alpha1.MetalNodeConditionReachable).Message; containsAny(msg, "mac-a.local") {
		t.Errorf("Reachable message %q quotes the spec value; want a generic message", msg)
	}
	if pods := f.verifierPods("mac-badip"); len(pods) != 0 {
		t.Errorf("%d verifier pods for an invalid spec.hostIP, want 0", len(pods))
	}
}

// TestMetalNodeHeartbeatWritesThrottled: on-schedule renewals do not each
// rewrite status; lastHeartbeatTime advances once it is a minute behind.
func TestMetalNodeHeartbeatWritesThrottled(t *testing.T) {
	f := newMetalNodeFixture(t)
	report := encodeReport(t, testReport(metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet}))
	f.createMetalNode("mac-hb", nil)
	f.putLease("mac-hb", f.now, report)
	f.reconcile("mac-hb")
	f.finishVerifier(f.onlyVerifierPod("mac-hb"), "100.64.0.2 0\n")
	f.reconcile("mac-hb")
	start := f.now
	rv := f.get("mac-hb").ResourceVersion

	writes := 0
	for i := 1; i <= 4; i++ {
		f.now = start.Add(time.Duration(i) * 14 * time.Second)
		f.putLease("mac-hb", f.now, report)
		f.reconcile("mac-hb")
		if cur := f.get("mac-hb").ResourceVersion; cur != rv {
			writes++
			rv = cur
		}
	}
	if writes > 1 {
		t.Errorf("%d MetalNode writes for 4 renewals within 60s, want at most 1", writes)
	}

	f.now = start.Add(75 * time.Second)
	f.putLease("mac-hb", f.now, report)
	f.reconcile("mac-hb")
	mn := f.get("mac-hb")
	if mn.Status.LastHeartbeatTime == nil || !mn.Status.LastHeartbeatTime.Time.Equal(f.now) {
		t.Errorf("lastHeartbeatTime = %v after a renewal more than 60s on, want %v", mn.Status.LastHeartbeatTime, f.now)
	}
}

// TestMetalNodeIgnoresForgedVerifierPod: only the verifier pod the controller
// created (recorded by UID) can report results. A look-alike pod made by
// anyone with pod create in the namespace is deleted, not believed.
func TestMetalNodeIgnoresForgedVerifierPod(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-forge", nil)
	f.putLease("mac-forge", f.now, encodeReport(t, testReport(metalnode.Candidate{Address: "100.64.0.2", Source: "tailnet"})))
	f.reconcile("mac-forge")
	real := f.onlyVerifierPod("mac-forge")

	forged := real.DeepCopy()
	forged.ObjectMeta = metav1.ObjectMeta{
		Name: "mac-forge-verify-forged", Namespace: testNS, Labels: real.Labels,
		Annotations: real.Annotations, OwnerReferences: real.OwnerReferences,
	}
	forged.Status = corev1.PodStatus{}
	if err := f.c.Create(context.Background(), forged); err != nil {
		t.Fatalf("create look-alike pod: %v", err)
	}
	f.finishVerifier(*forged, "100.64.0.2 0\n")
	f.reconcile("mac-forge")

	mn := f.get("mac-forge")
	if c := condition(t, mn, inferencev1alpha1.MetalNodeConditionReachable); c.Status == metav1.ConditionTrue || mn.Status.HostIP != "" {
		t.Errorf("Reachable = %s, hostIP %q from a pod the controller did not create", c.Status, mn.Status.HostIP)
	}
	for _, p := range f.verifierPods("mac-forge") {
		if p.Name == forged.Name {
			t.Error("the look-alike verifier pod was left in place")
		}
	}
}

// TestMetalNodeReachableNotStaleAfterInputChange is the re-review's R2
// repro: once the pin or port changes, or the verified address is no longer
// dialed, the old Reachable=True and hostIP must not stand until a new result
// lands. join reads both to report success.
func TestMetalNodeReachableNotStaleAfterInputChange(t *testing.T) {
	f := newMetalNodeFixture(t)
	verified := func(name string, c ...metalnode.Candidate) {
		f.createMetalNode(name, nil)
		f.putLease(name, f.now, encodeReport(t, testReport(c...)))
		f.reconcile(name)
		f.finishVerifier(f.onlyVerifierPod(name), "100.64.0.2 0\n")
		f.reconcile(name)
		wantCondition(t, f.get(name), inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionTrue,
			inferencev1alpha1.MetalNodeReasonDialBackSucceeded)
	}
	tailnet := metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet}

	// A new dialable address every 31 s, the verified one gone.
	verified("mac-churn", tailnet)
	start := f.now
	for i := 1; i <= 3; i++ {
		f.now = start.Add(time.Duration(31*i) * time.Second)
		f.putLease("mac-churn", f.now, encodeReport(t, testReport(
			metalnode.Candidate{Address: fmt.Sprintf("10.6.6.%d", i), Source: metalnode.SourceLAN})))
		f.reconcile("mac-churn")
		mn := f.get("mac-churn")
		wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionUnknown,
			inferencev1alpha1.MetalNodeReasonVerificationPending)
		if mn.Status.HostIP != "" {
			t.Fatalf("change %d: hostIP = %q, an address no longer dialed", i, mn.Status.HostIP)
		}
	}

	// Same addresses, new pin: the old result proved the old key.
	verified("mac-rekey", tailnet)
	rekeyed := testReport(tailnet)
	rekeyed.IngressSPKI = testIngressPin2
	f.now = f.now.Add(time.Minute)
	f.putLease("mac-rekey", f.now, encodeReport(t, rekeyed))
	f.reconcile("mac-rekey")
	mn := f.get("mac-rekey")
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionUnknown,
		inferencev1alpha1.MetalNodeReasonVerificationPending)
	if mn.Status.HostIP != "" {
		t.Errorf("hostIP = %q after the pin changed, want empty until re-verified", mn.Status.HostIP)
	}

	// A new address added beside the verified one leaves the result
	// standing until the new check lands.
	verified("mac-grow", tailnet)
	f.now = f.now.Add(time.Minute)
	f.putLease("mac-grow", f.now, encodeReport(t, testReport(tailnet,
		metalnode.Candidate{Address: "192.168.1.47", Source: metalnode.SourceLAN})))
	f.reconcile("mac-grow")
	mn = f.get("mac-grow")
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonDialBackSucceeded)
	if mn.Status.HostIP != "100.64.0.2" {
		t.Errorf("hostIP = %q after adding a candidate, want the verified 100.64.0.2 kept", mn.Status.HostIP)
	}
}

// updateSpec edits MetalNode name's spec, as an admin would, and returns the
// generation the edit made.
func (f *metalNodeFixture) updateSpec(name string, mutate func(*inferencev1alpha1.MetalNodeSpec)) int64 {
	f.t.Helper()
	mn := f.get(name)
	before := mn.Generation
	mutate(&mn.Spec)
	if err := f.c.Update(context.Background(), mn); err != nil {
		f.t.Fatalf("update MetalNode %s spec: %v", name, err)
	}
	if mn.Generation == before {
		f.t.Fatalf("the spec edit left MetalNode %s at generation %d", name, before)
	}
	return mn.Generation
}

// wantReachableAt asserts Reachable is status/reason, and that it and
// status.observedGeneration are both for generation gen.
func wantReachableAt(t *testing.T, mn *inferencev1alpha1.MetalNode, status metav1.ConditionStatus, reason string, gen int64) {
	t.Helper()
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, status, reason)
	c := condition(t, mn, inferencev1alpha1.MetalNodeConditionReachable)
	if c.ObservedGeneration != gen || mn.Status.ObservedGeneration != gen {
		t.Errorf("Reachable observedGeneration %d, status.observedGeneration %d, want both %d",
			c.ObservedGeneration, mn.Status.ObservedGeneration, gen)
	}
}

// TestMetalNodePlacementChangeWithdrawsReachable is #2265's "a placement
// change keeps the old result": a success proven from the nodes the old
// spec.verifier chose says nothing about the path from the new ones, so once
// status.observedGeneration says the new spec is seen, Reachable must not
// still be the old True and hostIP. It is Unknown until the check from the
// new placement, which starts at once, finishes. Moving spec.hostIP among the
// dialed addresses changes the check the same way.
func TestMetalNodePlacementChangeWithdrawsReachable(t *testing.T) {
	relayNodes := map[string]string{"llmkube.dev/relay": "true"}
	for name, edit := range map[string]func(*inferencev1alpha1.MetalNodeSpec){
		"mac-move": func(s *inferencev1alpha1.MetalNodeSpec) {
			s.Verifier = &inferencev1alpha1.MetalNodeVerifier{NodeSelector: relayNodes}
		},
		"mac-admin-ip": func(s *inferencev1alpha1.MetalNodeSpec) { s.HostIP = "10.1.2.3" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newMetalNodeFixture(t)
			report := encodeReport(t, testReport(tailnetCandidate))
			f.verifyMetalNode(name, tailnetCandidate)
			gen := f.updateSpec(name, edit)

			f.now = f.now.Add(time.Minute)
			f.putLease(name, f.now, report)
			f.reconcile(name)
			mn := f.get(name)
			wantReachableAt(t, mn, metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonVerificationPending, gen)
			if mn.Status.HostIP != "" {
				t.Errorf("hostIP = %q after the spec changed the check, want empty until the new check lands", mn.Status.HostIP)
			}
			if msg := condition(t, mn, inferencev1alpha1.MetalNodeConditionReachable).Message; !strings.Contains(msg, "spec") {
				t.Errorf("Reachable message %q does not point at the spec change", msg)
			}

			pod := f.onlyVerifierPod(name)
			if name == "mac-move" && pod.Spec.NodeSelector["llmkube.dev/relay"] != "true" {
				t.Errorf("verifier nodeSelector = %v, want the new placement %v", pod.Spec.NodeSelector, relayNodes)
			}
			f.finishVerifier(pod, "100.64.0.2 0\n")
			f.reconcile(name)
			mn = f.get(name)
			wantReachableAt(t, mn, metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonDialBackSucceeded, gen)
			if mn.Status.HostIP != "100.64.0.2" {
				t.Errorf("hostIP = %q after the new placement's check, want 100.64.0.2", mn.Status.HostIP)
			}
		})
	}
}

// TestMetalNodeSpecEditOutsideTheCheckKeepsReachable: a spec edit that
// changes nothing the check runs with (here the ServiceAccount) keeps the
// result, stamped for the new generation. llmkube metal join only accepts a
// Reachable whose observedGeneration is the MetalNode's, and with an
// unchanged check the next result is up to 10 minutes away. An edit before
// any check finished has no result to withdraw: Reachable stays NotVerified.
func TestMetalNodeSpecEditOutsideTheCheckKeepsReachable(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-first", nil)
	f.putLease("mac-first", f.now, encodeReport(t, testReport(tailnetCandidate)))
	f.reconcile("mac-first")
	f.onlyVerifierPod("mac-first")
	firstGen := f.updateSpec("mac-first", func(s *inferencev1alpha1.MetalNodeSpec) {
		s.Verifier = &inferencev1alpha1.MetalNodeVerifier{NodeSelector: map[string]string{"llmkube.dev/relay": "true"}}
	})
	f.now = f.now.Add(5 * time.Second)
	f.reconcile("mac-first")
	wantReachableAt(t, f.get("mac-first"), metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonNotVerified, firstGen)

	f.now = f.now.Add(time.Minute)
	f.verifyMetalNode("mac-rename", tailnetCandidate)
	gen := f.updateSpec("mac-rename", func(s *inferencev1alpha1.MetalNodeSpec) {
		s.ServiceAccountName = "llmkube-metal-renamed"
	})

	f.now = f.now.Add(time.Minute)
	f.putLease("mac-rename", f.now, encodeReport(t, testReport(tailnetCandidate)))
	f.reconcile("mac-rename")
	mn := f.get("mac-rename")
	wantReachableAt(t, mn, metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonDialBackSucceeded, gen)
	if mn.Status.HostIP != "100.64.0.2" {
		t.Errorf("hostIP = %q, want the verified 100.64.0.2 kept", mn.Status.HostIP)
	}
	if pods := f.verifierPods("mac-rename"); len(pods) != 0 {
		t.Errorf("%d verifier pods after an edit the check does not depend on, want 0", len(pods))
	}
}

// TestMetalNodeReportFactsClearedWithoutAValidReport: the agent's version,
// capacity, ingress pin and port stay in status only while a valid report
// backs them. An invalid report or a deleted Lease clears them (a deleted
// Lease is what llmkube metal remove leaves); a stale but valid report keeps
// them as the last known facts.
func TestMetalNodeReportFactsClearedWithoutAValidReport(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-facts", nil)
	report := encodeReport(t, testReport(metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet}))
	f.putLease("mac-facts", f.now, report)
	f.reconcile("mac-facts")
	wantFacts := func(step string, present bool) {
		t.Helper()
		st := f.get("mac-facts").Status
		has := st.AgentVersion != "" || st.Capacity != nil || st.IngressSPKI != "" || st.IngressPort != 0
		full := st.AgentVersion == "0.11.0" && st.Capacity != nil && st.IngressSPKI == testIngressPin && st.IngressPort == 9443
		if present && !full {
			t.Errorf("%s: version %q capacity %+v pin %q port %d, want the report's facts", step, st.AgentVersion, st.Capacity, st.IngressSPKI, st.IngressPort)
		}
		if !present && has {
			t.Errorf("%s: version %q capacity %+v pin %q port %d, want all cleared", step, st.AgentVersion, st.Capacity, st.IngressSPKI, st.IngressPort)
		}
	}
	wantFacts("valid report", true)

	f.putLease("mac-facts", f.now, "not-a-report")
	f.reconcile("mac-facts")
	wantCondition(t, f.get("mac-facts"), inferencev1alpha1.MetalNodeConditionAgentConnected, metav1.ConditionFalse,
		inferencev1alpha1.MetalNodeReasonReportInvalid)
	wantFacts("invalid report", false)

	f.putLease("mac-facts", f.now, report)
	f.reconcile("mac-facts")
	wantFacts("valid report again", true)

	f.now = f.now.Add(time.Hour)
	f.reconcile("mac-facts")
	wantCondition(t, f.get("mac-facts"), inferencev1alpha1.MetalNodeConditionAgentConnected, metav1.ConditionFalse,
		inferencev1alpha1.MetalNodeReasonHeartbeatStale)
	wantFacts("stale heartbeat", true)

	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "mac-facts", Namespace: testNS}}
	if err := f.c.Delete(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	f.reconcile("mac-facts")
	wantCondition(t, f.get("mac-facts"), inferencev1alpha1.MetalNodeConditionAgentConnected, metav1.ConditionFalse,
		inferencev1alpha1.MetalNodeReasonLeaseNotFound)
	wantFacts("Lease deleted", false)
}
