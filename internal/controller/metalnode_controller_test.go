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
	"crypto"
	"slices"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

type metalNodeFixture struct {
	t   *testing.T
	c   client.Client
	cfg *rest.Config
	r   *MetalNodeReconciler
	now time.Time
}

func newMetalNodeFixture(t *testing.T) *metalNodeFixture {
	t.Helper()
	c, cfg, _ := sharedTestEnv(t, baseCRDPath)
	f := &metalNodeFixture{t: t, c: c, cfg: cfg, now: time.Now().UTC().Truncate(time.Second)}
	f.r = &MetalNodeReconciler{
		Client:             c,
		Scheme:             scheme.Scheme,
		APIReader:          c,
		InitContainerImage: "registry.local/curl:1",
		AllowedCIDRs:       mustAllowedCIDRs(t, DefaultMetalNodeAllowedCIDRs),
		Now:                func() time.Time { return f.now },
	}
	return f
}

func (f *metalNodeFixture) createMetalNode(name string, mutate func(*inferencev1alpha1.MetalNodeSpec)) {
	f.t.Helper()
	mn := &inferencev1alpha1.MetalNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec:       inferencev1alpha1.MetalNodeSpec{ServiceAccountName: "llmkube-metal-" + name, LeaseName: name},
	}
	if mutate != nil {
		mutate(&mn.Spec)
	}
	if err := f.c.Create(context.Background(), mn); err != nil {
		f.t.Fatalf("create MetalNode %s: %v", name, err)
	}
}

func testReport(candidates ...metalnode.Candidate) metalnode.Report {
	return metalnode.Report{
		Version:      "0.11.0",
		Chip:         "Apple M5 Max",
		MemoryBytes:  128 << 30,
		GPUCores:     40,
		MetalVersion: "Metal 4",
		IngressPort:  9443,
		IngressSPKI:  testIngressPin,
		Candidates:   candidates,
	}
}

func encodeReport(t *testing.T, r metalnode.Report) string {
	t.Helper()
	raw, err := metalnode.Encode(r)
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	return raw
}

// putLease creates or updates the Lease the way a current agent leaves it:
// renewed at renew, holder the MetalNode, the report in the annotation. A
// report that pins one of the test ingress keys and carries no certificate
// gets that key's certificate bound to MetalNode name, as the agent binds its
// certificate before it reports (see putLeaseAsIs for any other report).
func (f *metalNodeFixture) putLease(name string, renew time.Time, annotation string) {
	f.t.Helper()
	if report, err := metalnode.Decode(annotation); err == nil && report.IngressCert == "" {
		key := map[string]crypto.Signer{testIngressPin: testIngressKey, testIngressPin2: testIngressKey2}[report.IngressSPKI]
		mn := &inferencev1alpha1.MetalNode{}
		if key != nil && f.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, mn) == nil {
			report.IngressCert = testIngressCert(f.t, key, mn.UID)
			annotation = encodeReport(f.t, report)
		}
	}
	f.putLeaseAsIs(name, renew, annotation)
}

// putLeaseAsIs writes annotation unchanged: an older agent's report, or one
// whose certificate the test chose.
func (f *metalNodeFixture) putLeaseAsIs(name string, renew time.Time, annotation string) {
	f.t.Helper()
	ctx := context.Background()
	lease := &coordinationv1.Lease{}
	err := f.c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: name}, lease)
	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		f.t.Fatalf("get Lease: %v", err)
	}
	lease.Name, lease.Namespace = name, testNS
	duration := int32(45)
	holder := name
	renewTime := metav1.NewMicroTime(renew)
	lease.Spec = coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &duration, RenewTime: &renewTime}
	lease.Annotations = map[string]string{metalnode.ReportAnnotation: annotation}
	if create {
		err = f.c.Create(ctx, lease)
	} else {
		err = f.c.Update(ctx, lease)
	}
	if err != nil {
		f.t.Fatalf("write Lease: %v", err)
	}
}

// wantVerifierIdentityArgs are the verifier's arguments before the addresses
// for MetalNode name: the script's $0, the pin, the port, the hostname from
// the MetalNode's UID and the certificate on its Lease as PEM.
func (f *metalNodeFixture) wantVerifierIdentityArgs(name string) []string {
	f.t.Helper()
	report := f.leaseReport(name)
	return []string{"metalnode-verify", report.IngressSPKI, "9443",
		metalnode.IngressHostname(string(f.get(name).UID)), certPEM(f.t, report.IngressCert)}
}

// leaseReport is the report on Lease name, as putLease left it.
func (f *metalNodeFixture) leaseReport(name string) metalnode.Report {
	f.t.Helper()
	lease := &coordinationv1.Lease{}
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, lease); err != nil {
		f.t.Fatalf("get Lease %s: %v", name, err)
	}
	report, err := metalnode.Decode(lease.Annotations[metalnode.ReportAnnotation])
	if err != nil {
		f.t.Fatalf("decode the report on Lease %s: %v", name, err)
	}
	return report
}

func (f *metalNodeFixture) reconcile(name string) ctrl.Result {
	f.t.Helper()
	res, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNS, Name: name}})
	if err != nil {
		f.t.Fatalf("reconcile %s: %v", name, err)
	}
	return res
}

func (f *metalNodeFixture) get(name string) *inferencev1alpha1.MetalNode {
	f.t.Helper()
	mn := &inferencev1alpha1.MetalNode{}
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, mn); err != nil {
		f.t.Fatalf("get MetalNode %s: %v", name, err)
	}
	return mn
}

func (f *metalNodeFixture) verifierPods(name string) []corev1.Pod {
	f.t.Helper()
	pods := &corev1.PodList{}
	if err := f.c.List(context.Background(), pods, client.InNamespace(testNS),
		client.MatchingLabels{inferencev1alpha1.LabelMetalNode: name}); err != nil {
		f.t.Fatalf("list verifier pods: %v", err)
	}
	return pods.Items
}

func (f *metalNodeFixture) onlyVerifierPod(name string) corev1.Pod {
	f.t.Helper()
	pods := f.verifierPods(name)
	if len(pods) != 1 {
		f.t.Fatalf("%d verifier pods for %s, want exactly 1", len(pods), name)
	}
	return pods[0]
}

// finishVerifier stands in for the kubelet: the verifier container exited and
// left msg in its termination message.
func (f *metalNodeFixture) finishVerifier(pod corev1.Pod, msg string) {
	f.t.Helper()
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  metalNodeVerifierContainer,
		Image: pod.Spec.Containers[0].Image,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode:   0,
			Message:    msg,
			StartedAt:  metav1.NewTime(f.now),
			FinishedAt: metav1.NewTime(f.now),
		}},
	}}
	if err := f.c.Status().Update(context.Background(), &pod); err != nil {
		f.t.Fatalf("finish verifier pod: %v", err)
	}
}

func condition(t *testing.T, mn *inferencev1alpha1.MetalNode, condType string) metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(mn.Status.Conditions, condType)
	if c == nil {
		t.Fatalf("MetalNode %s has no %s condition; conditions %+v", mn.Name, condType, mn.Status.Conditions)
	}
	return *c
}

func wantCondition(t *testing.T, mn *inferencev1alpha1.MetalNode, condType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := condition(t, mn, condType)
	if c.Status != status || c.Reason != reason {
		t.Errorf("%s = %s/%s (%q), want %s/%s", condType, c.Status, c.Reason, c.Message, status, reason)
	}
}

func candidateResults(mn *inferencev1alpha1.MetalNode) map[string]inferencev1alpha1.MetalNodeCandidateResult {
	out := map[string]inferencev1alpha1.MetalNodeCandidateResult{}
	for _, c := range mn.Status.Candidates {
		out[c.Address] = c.Result
	}
	return out
}

// happyPathReport is the agent report the happy-path test renews with.
func happyPathReport(t *testing.T) string {
	return encodeReport(t, testReport(
		metalnode.Candidate{Address: "192.168.65.254", Source: metalnode.SourceDockerHost},
		metalnode.Candidate{Address: "203.0.113.7", Source: metalnode.SourceLAN},
		metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet},
	))
}

// TestMetalNodeReconcileCopiesReportAndVerifies walks the happy path: the
// agent's Lease report lands in status, a verifier pod dials only the allowed
// candidates against the pin, and its termination message decides hostIP and
// Reachable before the pod is removed. An unchanged report then waits for the
// re-check interval; a changed one starts a new verifier at once.
func TestMetalNodeReconcileCopiesReportAndVerifies(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-a", func(s *inferencev1alpha1.MetalNodeSpec) {
		s.HostIP = "10.1.2.3"
		s.Verifier = &inferencev1alpha1.MetalNodeVerifier{
			NodeSelector: map[string]string{"llmkube.dev/relay": "true"},
			Tolerations: []corev1.Toleration{
				{Key: "relay", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
			},
		}
	})
	f.putLease("mac-a", f.now.Add(-5*time.Second), happyPathReport(t))

	res := f.reconcile("mac-a")
	if res.RequeueAfter <= 0 || res.RequeueAfter > metalNodeResyncInterval {
		t.Errorf("RequeueAfter = %s, want a requeue within %s", res.RequeueAfter, metalNodeResyncInterval)
	}
	mn := f.get("mac-a")
	assertReportCopied(t, mn, f.now.Add(-5*time.Second))
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionUnknown,
		inferencev1alpha1.MetalNodeReasonNotVerified)

	pod := f.onlyVerifierPod("mac-a")
	assertVerifierPod(t, mn, pod, f.wantVerifierIdentityArgs("mac-a"))

	// A reconcile while the verifier runs starts no second pod.
	f.reconcile("mac-a")
	f.onlyVerifierPod("mac-a")

	f.finishVerifier(pod, "10.1.2.3 28\n192.168.65.254 7\n100.64.0.2 0\n")
	f.reconcile("mac-a")
	assertVerifierResults(t, f.get("mac-a"), f.now)
	if pods := f.verifierPods("mac-a"); len(pods) != 0 {
		t.Errorf("%d verifier pods left after the result was recorded, want 0", len(pods))
	}

	// Same candidates and pin within 10 minutes: no new check.
	f.now = f.now.Add(5 * time.Minute)
	f.putLease("mac-a", f.now, happyPathReport(t))
	f.reconcile("mac-a")
	if pods := f.verifierPods("mac-a"); len(pods) != 0 {
		t.Fatalf("an unchanged candidate set started a verifier %s after the last check, want none before 10m", 5*time.Minute)
	}

	// A changed candidate set starts a new verifier at once.
	f.putLease("mac-a", f.now, encodeReport(t, testReport(
		metalnode.Candidate{Address: "192.168.1.47", Source: metalnode.SourceLAN},
	)))
	f.reconcile("mac-a")
	next := f.onlyVerifierPod("mac-a")
	if next.Name == pod.Name {
		t.Errorf("the new verifier reuses the old name %s; the name must follow the inputs", pod.Name)
	}
	wantArgs := append(f.wantVerifierIdentityArgs("mac-a"), "10.1.2.3", "192.168.1.47")
	if args := next.Spec.Containers[0].Command[3:]; !slices.Equal(args, wantArgs) {
		t.Errorf("new verifier args = %q, want %q", args, wantArgs)
	}
	if r := candidateResults(f.get("mac-a"))["192.168.1.47"]; r != inferencev1alpha1.CandidatePending {
		t.Errorf("new candidate result before its check = %s, want Pending", r)
	}
}

// assertReportCopied checks the first reconcile's status: the report's facts,
// the heartbeat, AgentConnected, and the candidate list in order with the
// disallowed address marked.
func assertReportCopied(t *testing.T, mn *inferencev1alpha1.MetalNode, renewed time.Time) {
	t.Helper()
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionAgentConnected, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonHeartbeatFresh)
	st := mn.Status
	if st.AgentVersion != "0.11.0" || st.IngressPort != 9443 || st.IngressSPKI != testIngressPin {
		t.Errorf("status version/port/pin = %q/%d/%q, want the report's", st.AgentVersion, st.IngressPort, st.IngressSPKI)
	}
	wantCapacity := inferencev1alpha1.MetalNodeCapacity{
		MemoryBytes: 128 << 30, Memory: "128Gi", Chip: "Apple M5 Max", GPUCores: 40, MetalVersion: "Metal 4",
	}
	if st.Capacity == nil || *st.Capacity != wantCapacity {
		t.Errorf("status capacity = %+v, want %+v", st.Capacity, wantCapacity)
	}
	if st.LastHeartbeatTime == nil || !st.LastHeartbeatTime.Time.Equal(renewed) {
		t.Errorf("lastHeartbeatTime = %v, want the Lease renewTime %v", st.LastHeartbeatTime, renewed)
	}
	if st.ObservedGeneration != mn.Generation {
		t.Errorf("observedGeneration = %d, want %d", st.ObservedGeneration, mn.Generation)
	}
	wantOrder := []string{"10.1.2.3", "192.168.65.254", "203.0.113.7", "100.64.0.2"}
	gotOrder := make([]string, 0, len(st.Candidates))
	for _, c := range st.Candidates {
		gotOrder = append(gotOrder, c.Address)
	}
	if !slices.Equal(gotOrder, wantOrder) {
		t.Errorf("candidate order = %v, want spec.hostIP first then the report's order %v", gotOrder, wantOrder)
	}
	if r := candidateResults(mn)["203.0.113.7"]; r != inferencev1alpha1.CandidateNotAllowed {
		t.Errorf("203.0.113.7 result = %s, want NotAllowed (outside the allowed CIDRs)", r)
	}
}

// assertVerifierPod checks the created verifier pod as the API server
// stored it: owner, deadline, image, token, placement and arguments.
func assertVerifierPod(t *testing.T, mn *inferencev1alpha1.MetalNode, pod corev1.Pod, identityArgs []string) {
	t.Helper()
	owner := metav1.GetControllerOf(&pod)
	if owner == nil || owner.Kind != "MetalNode" || owner.Name != mn.Name || owner.UID != mn.UID {
		t.Errorf("verifier pod controller owner = %+v, want MetalNode %s", owner, mn.Name)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != 60 ||
		pod.Spec.Containers[0].Image != "registry.local/curl:1" ||
		pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("verifier pod deadline/image/token = %v/%s/%v", pod.Spec.ActiveDeadlineSeconds,
			pod.Spec.Containers[0].Image, pod.Spec.AutomountServiceAccountToken)
	}
	// The API server's DefaultTolerationSeconds admission appends its own
	// not-ready/unreachable tolerations, so look for ours among them.
	hasRelayToleration := slices.ContainsFunc(pod.Spec.Tolerations, func(tol corev1.Toleration) bool {
		return tol.Key == "relay" && tol.Operator == corev1.TolerationOpExists && tol.Effect == corev1.TaintEffectNoSchedule
	})
	if pod.Spec.NodeSelector["llmkube.dev/relay"] != "true" || !hasRelayToleration {
		t.Errorf("verifier placement nodeSelector=%v tolerations=%v, want spec.verifier's",
			pod.Spec.NodeSelector, pod.Spec.Tolerations)
	}
	cmd := pod.Spec.Containers[0].Command
	wantArgs := append(identityArgs, "10.1.2.3", "192.168.65.254", "100.64.0.2")
	if len(cmd) < 3 || !slices.Equal(cmd[3:], wantArgs) {
		t.Errorf("verifier args = %q, want %q (the pin, the certificate for this MetalNode and only the allowed candidates)",
			cmd, wantArgs)
	}
	if strings.Contains(strings.Join(cmd, " "), "203.0.113.7") {
		t.Error("the verifier pod dials a candidate outside the allowed CIDRs")
	}
}

// assertVerifierResults checks the status after the verifier's message
// "10.1.2.3 28 / 192.168.65.254 7 / 100.64.0.2 0" was read.
func assertVerifierResults(t *testing.T, mn *inferencev1alpha1.MetalNode, finished time.Time) {
	t.Helper()
	got := candidateResults(mn)
	for addr, want := range map[string]inferencev1alpha1.MetalNodeCandidateResult{
		"10.1.2.3":       inferencev1alpha1.CandidateTimeout,
		"192.168.65.254": inferencev1alpha1.CandidateRefused,
		"203.0.113.7":    inferencev1alpha1.CandidateNotAllowed,
		"100.64.0.2":     inferencev1alpha1.CandidateReachable,
	} {
		if got[addr] != want {
			t.Errorf("candidate %s result = %s, want %s", addr, got[addr], want)
		}
	}
	for _, c := range mn.Status.Candidates {
		if c.Result != inferencev1alpha1.CandidateNotAllowed && (c.CheckedAt == nil || !c.CheckedAt.Time.Equal(finished)) {
			t.Errorf("candidate %s checkedAt = %v, want the verifier's finish time %v", c.Address, c.CheckedAt, finished)
		}
	}
	if mn.Status.HostIP != "100.64.0.2" {
		t.Errorf("hostIP = %q, want 100.64.0.2 (the first Reachable)", mn.Status.HostIP)
	}
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonDialBackSucceeded)
}

// TestMetalNodeReconcileRechecksAfterTenMinutes pins the periodic re-check of
// an unchanged, reachable Mac.
func TestMetalNodeReconcileRechecksAfterTenMinutes(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-p", nil)
	report := encodeReport(t, testReport(metalnode.Candidate{Address: "100.64.0.9", Source: metalnode.SourceTailnet}))
	f.putLease("mac-p", f.now, report)
	f.reconcile("mac-p")
	f.finishVerifier(f.onlyVerifierPod("mac-p"), "100.64.0.9 0\n")
	f.reconcile("mac-p")
	wantCondition(t, f.get("mac-p"), inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonDialBackSucceeded)

	f.now = f.now.Add(metalNodeRecheckInterval + time.Second)
	f.putLease("mac-p", f.now, report)
	f.reconcile("mac-p")
	f.onlyVerifierPod("mac-p")
}

// TestMetalNodeReconcileNoneReachableBacksOff: every candidate timed out, so
// Reachable is False, hostIP is empty, and the retry waits for the backoff.
func TestMetalNodeReconcileNoneReachableBacksOff(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-t", nil)
	report := encodeReport(t, testReport(
		metalnode.Candidate{Address: "192.168.1.47", Source: metalnode.SourceLAN},
		metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet},
	))
	f.putLease("mac-t", f.now, report)
	f.reconcile("mac-t")
	f.finishVerifier(f.onlyVerifierPod("mac-t"), "192.168.1.47 28\n100.64.0.2 28\n")
	f.reconcile("mac-t")

	mn := f.get("mac-t")
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonNoneReachable)
	if mn.Status.HostIP != "" {
		t.Errorf("hostIP = %q with nothing reachable, want empty", mn.Status.HostIP)
	}
	if mn.Status.Verification == nil || mn.Status.Verification.ConsecutiveFailures != 1 {
		t.Errorf("verification = %+v, want one consecutive failure", mn.Status.Verification)
	}

	f.now = f.now.Add(10 * time.Second)
	f.putLease("mac-t", f.now, report)
	f.reconcile("mac-t")
	if pods := f.verifierPods("mac-t"); len(pods) != 0 {
		t.Fatalf("retried 10s after a failed check, want to wait the 30s backoff")
	}
	f.now = f.now.Add(25 * time.Second)
	f.putLease("mac-t", f.now, report)
	f.reconcile("mac-t")
	f.onlyVerifierPod("mac-t")
}

// TestMetalNodeReconcileVerifierNeverFinishes: a verifier that cannot be
// scheduled never reaches its activeDeadlineSeconds, so the controller gives
// up on it, says so, and removes it.
func TestMetalNodeReconcileVerifierNeverFinishes(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-s", nil)
	f.putLease("mac-s", f.now, encodeReport(t, testReport(metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet})))
	f.reconcile("mac-s")
	f.onlyVerifierPod("mac-s")

	// Pod creationTimestamp is the API server's clock, a moment after f.now.
	f.now = f.now.Add(metalNodeVerifierTimeout + 30*time.Second)
	f.putLease("mac-s", f.now, encodeReport(t, testReport(metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet})))
	f.reconcile("mac-s")
	wantCondition(t, f.get("mac-s"), inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonVerifierFailed)
	if pods := f.verifierPods("mac-s"); len(pods) != 0 {
		t.Errorf("%d verifier pods left after giving up, want 0", len(pods))
	}
}

// TestMetalNodeReconcileAgentConnectedReasons covers the ways the Lease can
// fail to show a live, valid agent. None of them starts a verifier, and
// Reachable says why: a connected agent with an invalid report is sent to
// the report, not to the Mac's network.
func TestMetalNodeReconcileAgentConnectedReasons(t *testing.T) {
	f := newMetalNodeFixture(t)
	valid := encodeReport(t, testReport(metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet}))
	badPin := testReport(metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet})
	badPin.IngressSPKI = "not-a-sha256-pin"
	// A privileged port is refused, so the dial-back cannot be pointed at
	// 22, 443 and the like.
	lowPort := testReport(metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet})
	lowPort.IngressPort = 443

	cases := []struct {
		name       string
		lease      bool
		renew      time.Duration
		annotation string
		reason     string
	}{
		{name: "stale", lease: true, renew: -10 * time.Minute, annotation: valid, reason: inferencev1alpha1.MetalNodeReasonHeartbeatStale},
		{name: "missing", lease: false, reason: inferencev1alpha1.MetalNodeReasonLeaseNotFound},
		{name: "malformed", lease: true, annotation: `{"version":`, reason: inferencev1alpha1.MetalNodeReasonReportInvalid},
		{name: "badpin", lease: true, annotation: encodeReport(t, badPin), reason: inferencev1alpha1.MetalNodeReasonReportInvalid},
		{name: "lowport", lease: true, annotation: encodeReport(t, lowPort), reason: inferencev1alpha1.MetalNodeReasonReportInvalid},
		// A renewTime well in the future never goes stale on its own, so it
		// counts as stale now.
		{name: "future", lease: true, renew: time.Hour, annotation: valid, reason: inferencev1alpha1.MetalNodeReasonHeartbeatStale},
	}
	// wantReachable is the Reachable reason for an AgentConnected reason.
	wantReachable := map[string]string{
		inferencev1alpha1.MetalNodeReasonHeartbeatStale: inferencev1alpha1.MetalNodeReasonAgentNotConnected,
		inferencev1alpha1.MetalNodeReasonLeaseNotFound:  inferencev1alpha1.MetalNodeReasonAgentNotConnected,
		inferencev1alpha1.MetalNodeReasonReportInvalid:  inferencev1alpha1.MetalNodeReasonReportInvalid,
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "mac-" + tc.name
			f.createMetalNode(name, nil)
			if tc.lease {
				f.putLease(name, f.now.Add(tc.renew), tc.annotation)
			}
			f.reconcile(name)
			mn := f.get(name)
			wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionAgentConnected, metav1.ConditionFalse, tc.reason)
			if msg := condition(t, mn, inferencev1alpha1.MetalNodeConditionAgentConnected).Message; strings.Contains(msg, "not-a-sha256-pin") {
				t.Errorf("AgentConnected message copies the agent's raw pin: %q", msg)
			}
			wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionUnknown, wantReachable[tc.reason])
			reachable := condition(t, mn, inferencev1alpha1.MetalNodeConditionReachable).Message
			if tc.reason == inferencev1alpha1.MetalNodeReasonReportInvalid &&
				(!strings.Contains(reachable, "report") || strings.Contains(reachable, "not connected")) {
				t.Errorf("Reachable message %q for an invalid report, want one that points at the report, not the connection", reachable)
			}
			if strings.Contains(reachable, "not-a-sha256-pin") {
				t.Errorf("Reachable message copies the agent's raw pin: %q", reachable)
			}
			if pods := f.verifierPods(name); len(pods) != 0 {
				t.Errorf("%d verifier pods for an agent that is not connected, want 0", len(pods))
			}
			if hb := mn.Status.LastHeartbeatTime; hb != nil && hb.Time.After(f.now) {
				t.Errorf("lastHeartbeatTime = %v, a time in the future copied from the Lease", hb)
			}
		})
	}
}

// TestMetalNodeReconcileNoDialBackPreconditions: a connected agent with no
// pin, or with only disallowed candidates, gets a False Reachable that says
// why, and no verifier pod.
func TestMetalNodeReconcileNoDialBackPreconditions(t *testing.T) {
	f := newMetalNodeFixture(t)

	noPin := testReport(metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet})
	noPin.IngressSPKI = ""
	f.createMetalNode("mac-nopin", nil)
	f.putLease("mac-nopin", f.now, encodeReport(t, noPin))
	f.reconcile("mac-nopin")
	mn := f.get("mac-nopin")
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionAgentConnected, metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonHeartbeatFresh)
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonPinMissing)

	f.createMetalNode("mac-public", nil)
	f.putLease("mac-public", f.now, encodeReport(t, testReport(metalnode.Candidate{Address: "203.0.113.7", Source: metalnode.SourceLAN})))
	f.reconcile("mac-public")
	wantCondition(t, f.get("mac-public"), inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonNoCandidates)

	for _, name := range []string{"mac-nopin", "mac-public"} {
		if pods := f.verifierPods(name); len(pods) != 0 {
			t.Errorf("%s: %d verifier pods, want 0", name, len(pods))
		}
	}
}

// TestMetalNodeLeaseMapsToMetalNode pins the Lease watch's mapping: a Lease
// event reconciles the MetalNode in the same namespace whose spec.leaseName
// names it, and nothing else.
func TestMetalNodeLeaseMapsToMetalNode(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-x", func(s *inferencev1alpha1.MetalNodeSpec) { s.LeaseName = "lease-x" })
	f.createMetalNode("mac-y", nil)

	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "lease-x", Namespace: testNS}}
	got := f.r.metalNodesForLease(context.Background(), lease)
	if len(got) != 1 || got[0].Name != "mac-x" || got[0].Namespace != testNS {
		t.Errorf("requests for Lease lease-x = %v, want only %s/mac-x", got, testNS)
	}
	other := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "lease-x", Namespace: "elsewhere"}}
	if got := f.r.metalNodesForLease(context.Background(), other); len(got) != 0 {
		t.Errorf("a Lease in another namespace mapped to %v, want nothing", got)
	}
}
