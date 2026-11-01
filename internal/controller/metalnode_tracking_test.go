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
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

// laggingClient stands in for the manager's cache: Get returns staleMN for
// that MetalNode when it is set, and List returns no verifier pods while
// hidePods is true, the way an informer still behind the API server does.
func (f *metalNodeFixture) laggingClient(staleMN *inferencev1alpha1.MetalNode, hidePods *bool) client.Client {
	f.t.Helper()
	wc, err := client.NewWithWatch(f.cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		f.t.Fatal(err)
	}
	return interceptor.NewClient(wc, interceptor.Funcs{
		Get: func(ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if mn, ok := obj.(*inferencev1alpha1.MetalNode); ok && staleMN != nil && key.Name == staleMN.Name {
				staleMN.DeepCopyInto(mn)
				return nil
			}
			return w.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, w client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := w.List(ctx, list, opts...); err != nil {
				return err
			}
			if pods, ok := list.(*corev1.PodList); ok && hidePods != nil && *hidePods {
				pods.Items = nil
			}
			return nil
		},
	})
}

func tailnetReport(t *testing.T) string {
	return encodeReport(t, testReport(metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet}))
}

// TestMetalNodeKeepsVerifierWhenCachedMetalNodeLags: the verifier pod's
// create event reconciles again while the cache still holds the MetalNode
// from before status.verification.podUID was written. That reconcile must
// not take its own verifier for a stranger and delete it, and the verifier's
// result must still land.
func TestMetalNodeKeepsVerifierWhenCachedMetalNodeLags(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-lag", nil)
	f.putLease("mac-lag", f.now, tailnetReport(t))
	beforeStart := f.get("mac-lag")

	f.reconcile("mac-lag")
	pod := f.onlyVerifierPod("mac-lag")
	if f.get("mac-lag").Status.Verification.PodUID != string(pod.UID) {
		t.Fatalf("status.verification.podUID does not track the verifier the controller created")
	}

	f.r.Client = f.laggingClient(beforeStart, nil)
	f.reconcile("mac-lag")
	if pods := f.verifierPods("mac-lag"); len(pods) != 1 || pods[0].UID != pod.UID {
		t.Fatalf("verifier pods after a reconcile with a lagging cache = %d, want the running verifier %s kept", len(pods), pod.Name)
	}

	f.finishVerifier(f.onlyVerifierPod("mac-lag"), "100.64.0.2 0\n")
	f.reconcile("mac-lag")
	mn := f.get("mac-lag")
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonDialBackSucceeded)
	if mn.Status.HostIP != "100.64.0.2" {
		t.Errorf("status.hostIP = %q, want the address the verifier reached", mn.Status.HostIP)
	}
}

// TestMetalNodeVerifierAlreadyExistsIsARunningCheck: when the pod list has
// not caught up with a verifier the controller created, the next start finds
// the name taken. That is the running check, not a failed one: Reachable is
// not marked failed, the running verifier stays tracked, and its result
// lands once the list catches up.
func TestMetalNodeVerifierAlreadyExistsIsARunningCheck(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-exists", nil)
	f.putLease("mac-exists", f.now, tailnetReport(t))
	f.reconcile("mac-exists")
	pod := f.onlyVerifierPod("mac-exists")

	hide := true
	f.r.Client = f.laggingClient(nil, &hide)
	f.now = f.now.Add(metalNodeMinStartSpacing + time.Second)
	f.putLease("mac-exists", f.now, tailnetReport(t))
	f.reconcile("mac-exists")

	mn := f.get("mac-exists")
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionUnknown,
		inferencev1alpha1.MetalNodeReasonNotVerified)
	if v := mn.Status.Verification; v == nil || v.PodUID != string(pod.UID) || v.ConsecutiveFailures != 0 {
		t.Fatalf("status.verification = %+v, want the running verifier %s still tracked and no failure counted", v, pod.UID)
	}

	hide = false
	f.finishVerifier(f.onlyVerifierPod("mac-exists"), "100.64.0.2 0\n")
	f.reconcile("mac-exists")
	wantCondition(t, f.get("mac-exists"), inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonDialBackSucceeded)
}

// TestMetalNodeVerifierNameTakenByAnotherPod: a pod this MetalNode does not
// own holding the verifier's name blocks every check for these inputs.
// Reachable says so instead of reporting a generic create failure.
func TestMetalNodeVerifierNameTakenByAnotherPod(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-squat", nil)
	f.putLease("mac-squat", f.now, tailnetReport(t))
	hash := metalNodeInputHash(testIngressPin, f.leaseReport("mac-squat").IngressCert, 9443, []string{"100.64.0.2"}, nil)
	squatter := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: metalNodeVerifierName("mac-squat", hash), Namespace: testNS},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.local/curl:1"}}},
	}
	if err := f.c.Create(context.Background(), squatter); err != nil {
		t.Fatalf("create squatter pod: %v", err)
	}

	f.reconcile("mac-squat")
	mn := f.get("mac-squat")
	c := condition(t, mn, inferencev1alpha1.MetalNodeConditionReachable)
	if c.Status != metav1.ConditionFalse || c.Reason != inferencev1alpha1.MetalNodeReasonVerifierFailed ||
		!strings.Contains(c.Message, squatter.Name) || !strings.Contains(c.Message, "not owned") {
		t.Errorf("Reachable = %s/%s (%q), want VerifierFailed naming the pod that holds the verifier's name", c.Status, c.Reason, c.Message)
	}
}

// countingReader counts the reads it serves, by kind.
type countingReader struct {
	client.Reader
	metalNodes, leases int
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	switch obj.(type) {
	case *inferencev1alpha1.MetalNode:
		c.metalNodes++
	case *coordinationv1.Lease:
		c.leases++
	}
	return c.Reader.Get(ctx, key, obj, opts...)
}

// emptyLeaseCache is a Lease cache that does not hold the Lease, as the
// controller's label-scoped one does not hold an unlabeled Lease.
type emptyLeaseCache struct{ client.Reader }

func (emptyLeaseCache) Get(_ context.Context, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return apierrors.NewNotFound(schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"}, key.Name)
}

// createCounter counts the objects the controller creates.
type createCounter struct {
	client.Client
	creates int
}

func (c *createCounter) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.creates++
	return c.Client.Create(ctx, obj, opts...)
}

// TestMetalNodeReconcileReadsTheCaches: a reconcile that starts nothing
// reads the MetalNode and the Lease from the controller's caches, not from
// the API server. The MetalNode is read uncached once to confirm it before a
// verifier starts, and an unlabeled Lease, which the label-scoped Lease
// cache does not hold, is still found.
func TestMetalNodeReconcileReadsTheCaches(t *testing.T) {
	f := newMetalNodeFixture(t)
	const name = "mac-cached"
	uncached := &countingReader{Reader: f.c}
	f.r.APIReader = uncached
	f.r.leaseReader = f.c
	f.verifyMetalNode(name, tailnetCandidate)
	if uncached.metalNodes != 1 || uncached.leases != 0 {
		t.Errorf("uncached reads to run and record one check: %d MetalNode, %d Lease; want 1 MetalNode (confirming before the start), 0 Lease",
			uncached.metalNodes, uncached.leases)
	}

	*uncached = countingReader{Reader: f.c}
	for range 3 {
		f.now = f.now.Add(time.Minute)
		f.putLease(name, f.now, tailnetReport(t))
		f.reconcile(name)
	}
	if uncached.metalNodes != 0 || uncached.leases != 0 {
		t.Errorf("uncached reads over 3 reconciles that start nothing: %d MetalNode, %d Lease; want 0",
			uncached.metalNodes, uncached.leases)
	}

	*uncached = countingReader{Reader: f.c}
	f.now = f.now.Add(metalNodeRecheckInterval)
	f.putLease(name, f.now, tailnetReport(t))
	f.reconcile(name)
	f.onlyVerifierPod(name)
	if uncached.metalNodes != 1 {
		t.Errorf("%d uncached MetalNode reads for the reconcile that starts the recheck, want 1", uncached.metalNodes)
	}

	*uncached = countingReader{Reader: f.c}
	f.r.leaseReader = emptyLeaseCache{}
	f.reconcile(name)
	wantCondition(t, f.get(name), inferencev1alpha1.MetalNodeConditionAgentConnected, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonHeartbeatFresh)
	if uncached.leases != 1 {
		t.Errorf("%d uncached Lease reads for a Lease the Lease cache does not hold, want 1", uncached.leases)
	}
}

// TestMetalNodeStaleCachedStatusWriteIsRedone: a cached MetalNode can lag
// the controller's own last status write. A reconcile working from such a
// copy must not write its conditions over the newer ones: here the copy
// still holds an old success, which has expired by its own clock, while the
// current status records the newer failed check. The write from the copy
// is refused and redone from an uncached read, so the newer result stands.
func TestMetalNodeStaleCachedStatusWriteIsRedone(t *testing.T) {
	f := newMetalNodeFixture(t)
	const name = "mac-stalewrite"
	t.Cleanup(func() { f.releaseVerifiers(name) })
	// A held verifier is what keeps a new check from starting below, and the
	// API server stamps its deletion on the real clock. Starting the
	// controller's clock a recheck interval back keeps it under the abandon
	// bound at the end.
	f.now = f.now.Add(-metalNodeRecheckInterval)
	f.verifyMetalNode(name, tailnetCandidate)
	verifiedAt := f.now
	stale := f.get(name)

	f.now = verifiedAt.Add(metalNodeRecheckInterval + time.Second)
	f.putLease(name, f.now, tailnetReport(t))
	f.reconcile(name)
	f.holdVerifiers(name)
	f.finishVerifier(f.onlyVerifierPod(name), "100.64.0.2 7\n")
	f.reconcile(name)
	wantCondition(t, f.get(name), inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionFalse,
		inferencev1alpha1.MetalNodeReasonNoneReachable)

	f.r.Client = f.laggingClient(stale, nil)
	f.now = verifiedAt.Add(metalNodeMaxResultAge + time.Minute)
	f.putLease(name, f.now, tailnetReport(t))
	f.reconcile(name)
	wantCondition(t, f.get(name), inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionFalse,
		inferencev1alpha1.MetalNodeReasonNoneReachable)
}

// TestMetalNodeStaleCacheStartsNoVerifier: a cached MetalNode from before
// the last check would say a check is due. The verifier is not created from
// that copy: the uncached MetalNode says the last check finished moments
// ago, so nothing starts.
func TestMetalNodeStaleCacheStartsNoVerifier(t *testing.T) {
	f := newMetalNodeFixture(t)
	const name = "mac-stalestart"
	f.createMetalNode(name, nil)
	stale := f.get(name)
	f.putLease(name, f.now, tailnetReport(t))
	f.reconcile(name)
	f.finishVerifier(f.onlyVerifierPod(name), "100.64.0.2 0\n")
	f.reconcile(name)

	counter := &createCounter{Client: f.laggingClient(stale, nil)}
	f.r.Client = counter
	f.now = f.now.Add(metalNodeMinStartSpacing + time.Second)
	f.putLease(name, f.now, tailnetReport(t))
	f.reconcile(name)
	if counter.creates != 0 {
		t.Errorf("%d objects created from a cached MetalNode older than the last check, want 0", counter.creates)
	}
	mn := f.get(name)
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonDialBackSucceeded)
	if mn.Status.HostIP != "100.64.0.2" {
		t.Errorf("hostIP = %q, want the verified 100.64.0.2", mn.Status.HostIP)
	}
}

// TestMetalNodeStaleCacheKeepsTheRunningRecheck: the recheck's own create
// event reconciles while the cache still holds the MetalNode from before it
// started, which tracks the previous verifier. Nothing in status differs on
// that copy, so no write is refused; the running verifier is still not
// taken for a stranger and deleted, and its result lands.
func TestMetalNodeStaleCacheKeepsTheRunningRecheck(t *testing.T) {
	f := newMetalNodeFixture(t)
	const name = "mac-recheck"
	// The API server stamps the recheck pod's creationTimestamp on the real
	// clock; starting the controller's clock a recheck interval back keeps
	// the pod well inside metalNodeVerifierTimeout when it starts.
	f.now = f.now.Add(-metalNodeRecheckInterval)
	f.verifyMetalNode(name, tailnetCandidate)
	verifiedAt := f.now

	// The heartbeat advances just before the recheck is due, so the start
	// below changes nothing in status but the verifier it tracks.
	f.now = verifiedAt.Add(metalNodeRecheckInterval - time.Second)
	f.putLease(name, f.now, tailnetReport(t))
	f.reconcile(name)
	stale := f.get(name)
	f.now = verifiedAt.Add(metalNodeRecheckInterval + time.Second)
	f.putLease(name, f.now, tailnetReport(t))
	f.reconcile(name)
	pod := f.onlyVerifierPod(name)

	fresh := f.r.Client
	f.r.Client = f.laggingClient(stale, nil)
	f.reconcile(name)
	if pods := f.verifierPods(name); len(pods) != 1 || pods[0].UID != pod.UID || pods[0].DeletionTimestamp != nil {
		t.Fatalf("verifier pods after a reconcile from a cached MetalNode that predates the recheck = %d, want the running recheck %s kept",
			len(pods), pod.Name)
	}

	f.r.Client = fresh
	f.now = f.now.Add(2 * time.Second)
	f.finishVerifier(f.onlyVerifierPod(name), "100.64.0.2 0\n")
	f.reconcile(name)
	if v := f.get(name).Status.Verification; v == nil || v.LastCheckTime == nil || !v.LastCheckTime.After(verifiedAt) {
		t.Errorf("status.verification = %+v, want the recheck's result recorded", v)
	}
}
