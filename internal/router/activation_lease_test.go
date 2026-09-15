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

package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/prometheus/client_golang/prometheus/testutil"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

func leaseScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(s); err != nil {
		t.Fatalf("add coordination scheme: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	return s
}

// TestLeaseCoordinatorSerializesHolders verifies the cross-replica single-writer
// invariant (#1477): while one replica holds a pool's swap lease, another
// replica's Acquire reports not-owned, and once the holder releases the next
// replica takes it.
func TestLeaseCoordinatorSerializesHolders(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(leaseScheme(t)).Build()
	ctx := context.Background()

	a := NewLeaseCoordinator(c, "lab")
	a.identity = "proxy-a"
	b := NewLeaseCoordinator(c, "lab")
	b.identity = "proxy-b"

	releaseA, okA, err := a.Acquire(ctx, "lab/heavy-slot")
	if err != nil {
		t.Fatalf("a.Acquire: %v", err)
	}
	if !okA {
		t.Fatal("first Acquire = not owned, want owned on an unheld pool")
	}

	_, okB, err := b.Acquire(ctx, "lab/heavy-slot")
	if err != nil {
		t.Fatalf("b.Acquire: %v", err)
	}
	if okB {
		t.Fatal("second replica acquired a lease the first still holds; swaps are not single-writer")
	}

	releaseA()
	_, okB, err = b.Acquire(ctx, "lab/heavy-slot")
	if err != nil {
		t.Fatalf("b.Acquire after release: %v", err)
	}
	if !okB {
		t.Fatal("a released lease must be acquirable by the next replica")
	}
}

// TestLeaseCoordinatorRenewsHold verifies the hold is renewed for its whole
// life, so a cold load that outlives the lease TTL (ModelPool.spec.swapBudget
// defaults to 300s) does not let a second replica take over mid-swap (#1477).
func TestLeaseCoordinatorRenewsHold(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(leaseScheme(t)).Build()
	ctx := context.Background()

	a := NewLeaseCoordinator(c, "lab")
	a.identity = "proxy-a"
	// Short duration so the renew loop ticks quickly; renewInterval is
	// duration/3.
	a.duration = 600 * time.Millisecond

	release, owned, err := a.Acquire(ctx, "lab/heavy-slot")
	if err != nil || !owned {
		t.Fatalf("Acquire = (owned=%v, err=%v), want owned", owned, err)
	}
	defer release()

	name := leaseNamePrefix + sanitizeLeaseName("lab/heavy-slot")
	readRenew := func() time.Time {
		lease := &coordinationv1.Lease{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "lab", Name: name}, lease); err != nil {
			t.Fatalf("get lease: %v", err)
		}
		if lease.Spec.RenewTime == nil {
			t.Fatal("lease has no RenewTime")
		}
		return lease.Spec.RenewTime.Time
	}
	initial := readRenew()

	deadline := time.Now().Add(5 * time.Second)
	for !readRenew().After(initial) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !readRenew().After(initial) {
		t.Fatal("the lease RenewTime must advance while the hold is outstanding")
	}

	// A competing replica must still see the hold as live.
	b := NewLeaseCoordinator(c, "lab")
	b.identity = "proxy-b"
	if _, ownedB, err := b.Acquire(ctx, "lab/heavy-slot"); err != nil {
		t.Fatalf("b.Acquire: %v", err)
	} else if ownedB {
		t.Fatal("a renewed hold must not be takeover-eligible")
	}
}

// TestLeaseCoordinatorReleaseFreesTheLease verifies the release closure clears
// the hold outright, so the next swap need not wait out the lease duration.
func TestLeaseCoordinatorReleaseFreesTheLease(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(leaseScheme(t)).Build()
	ctx := context.Background()

	a := NewLeaseCoordinator(c, "lab")
	a.identity = "proxy-a"
	release, owned, err := a.Acquire(ctx, "lab/heavy-slot")
	if err != nil || !owned {
		t.Fatalf("Acquire = (owned=%v, err=%v), want owned", owned, err)
	}

	// Idempotent: a second call must not panic or double-clear.
	release()
	release()

	name := leaseNamePrefix + sanitizeLeaseName("lab/heavy-slot")
	lease := &coordinationv1.Lease{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lab", Name: name}, lease); err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if holder := ptr.Deref(lease.Spec.HolderIdentity, ""); holder != "" {
		t.Errorf("holder after release = %q, want empty", holder)
	}

	b := NewLeaseCoordinator(c, "lab")
	b.identity = "proxy-b"
	if _, ownedB, err := b.Acquire(ctx, "lab/heavy-slot"); err != nil {
		t.Fatalf("b.Acquire after release: %v", err)
	} else if !ownedB {
		t.Fatal("a released lease must be immediately acquirable")
	}
}

// TestLeaseCoordinatorTakesOverExpired verifies a lease whose holder stopped
// renewing is taken over, so a crashed proxy replica does not wedge the pool.
func TestLeaseCoordinatorTakesOverExpired(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(leaseScheme(t)).Build()
	ctx := context.Background()

	l := NewLeaseCoordinator(c, "lab")
	l.identity = "live"
	l.duration = time.Minute
	stale := metav1.NewMicroTime(time.Now().Add(-2 * time.Minute))
	dead := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: leaseNamePrefix + sanitizeLeaseName("lab/heavy-slot"), Namespace: "lab"},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To("dead-proxy"),
			LeaseDurationSeconds: ptr.To(int32(time.Minute.Seconds())),
			RenewTime:            &stale,
		},
	}
	if err := c.Create(ctx, dead); err != nil {
		t.Fatalf("seed lease: %v", err)
	}

	release, ok, err := l.Acquire(ctx, "lab/heavy-slot")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !ok {
		t.Fatal("Acquire = not owned, want takeover of an expired lease")
	}
	release()
}

// fakeCoordinator is an injectable SwapCoordinator for Activator tests.
type fakeCoordinator struct {
	mu       sync.Mutex
	owner    bool
	err      error
	released bool
}

func (f *fakeCoordinator) Acquire(context.Context, string) (func(), bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, false, f.err
	}
	if !f.owner {
		return nil, false, nil
	}
	return f.releaseFunc(), true, nil
}

// releaseFunc returns a closure that records the release exactly once.
func (f *fakeCoordinator) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.released = true
		})
	}
}

func (f *fakeCoordinator) wasReleased() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released
}

// TestActivatorDefersSwapToLeaseOwner verifies a replica that does not own the
// swap lease does not scale the member: it waits for the owner to bring the
// member up and, when its caller gives up, leaves the member alone rather than
// undoing the owner's activation (#1477).
func TestActivatorDefersSwapToLeaseOwner(t *testing.T) {
	memberCtrl := newFakeMemberController()
	coord := &fakeCoordinator{owner: false}
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	a := NewActivator(baseCtx, memberCtrl, "r", nil)
	a.SetSwapCoordinator(coord)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := a.Acquire(ctx, testPool("coder")); !errors.Is(err, ErrHoldBudgetExceeded) {
		t.Fatalf("Acquire = %v, want ErrHoldBudgetExceeded while deferring", err)
	}

	if got := memberCtrl.activateCount("coder"); got != 0 {
		t.Errorf("deferring replica scaled the member %d times, want 0 (only the lease owner writes)", got)
	}
	if got := memberCtrl.deactivateCount("coder"); got != 0 {
		t.Errorf("deferring replica deactivated the member %d times, want 0", got)
	}
	// A non-owner never acquired a hold, so there is nothing for it to release.
	if coord.wasReleased() {
		t.Error("a deferring replica released a lease it never acquired")
	}
}

// TestActivatorOwnerDrivesSwap is the positive control for the deferral test:
// the replica that owns the lease scales the member.
func TestActivatorOwnerDrivesSwap(t *testing.T) {
	memberCtrl := newFakeMemberController()
	coord := &fakeCoordinator{owner: true}
	a := NewActivator(context.Background(), memberCtrl, "r", nil)
	a.SetSwapCoordinator(coord)

	rel, err := a.Acquire(context.Background(), testPool("coder"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	rel()

	if got := memberCtrl.activateCount("coder"); got != 1 {
		t.Errorf("lease owner activate count = %d, want 1", got)
	}
	// The swap goroutine releases the hold when the swap ends, so a later swap
	// need not wait out the duration.
	deadline := time.Now().Add(2 * time.Second)
	for !coord.wasReleased() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !coord.wasReleased() {
		t.Error("the owner did not release the swap lease when the swap ended")
	}
}

// TestDeferringReplicaDoesNotCountSwap verifies the swap counters move only on
// the replica that drove the member write. A deferring replica reaches the same
// resident state but drove nothing, so counting it there would double the swap
// rate operators watch to confirm the thrash is gone (#1477).
func TestDeferringReplicaDoesNotCountSwap(t *testing.T) {
	// Deferring replica: another replica owns the swap, so this one only waits.
	deferCtrl := newFakeMemberController()
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	deferring := NewActivator(baseCtx, deferCtrl, "defer-test", nil)
	deferring.SetSwapCoordinator(&fakeCoordinator{owner: false})

	// Stand in for the owning replica bringing the member up.
	go func() {
		time.Sleep(20 * time.Millisecond)
		deferCtrl.setPhase("coder", modelReadyPhase)
	}()

	rel, err := deferring.Acquire(context.Background(), testPool("coder"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	rel()

	if got := testutil.ToFloat64(prommetrics.ModelPoolSwapsTotal.WithLabelValues(
		"defer-test", "heavy-slot", "", "coder")); got != 0 {
		t.Errorf("ModelPoolSwapsTotal = %v for a deferring replica, want 0 "+
			"(only the replica that drove the write counts a swap)", got)
	}

	// Control: the owning replica's swap is counted, so the assertion above is
	// not passing merely because the counter never moves.
	ownerCtrl := newFakeMemberController()
	owner := NewActivator(context.Background(), ownerCtrl, "owner-test", nil)
	owner.SetSwapCoordinator(&fakeCoordinator{owner: true})
	relOwner, err := owner.Acquire(context.Background(), testPool("coder"))
	if err != nil {
		t.Fatalf("owner Acquire: %v", err)
	}
	relOwner()

	if got := testutil.ToFloat64(prommetrics.ModelPoolSwapsTotal.WithLabelValues(
		"owner-test", "heavy-slot", "", "coder")); got != 1 {
		t.Errorf("ModelPoolSwapsTotal = %v for the owning replica, want 1", got)
	}
}

// TestActivatorSurfacesLeaseUnavailable verifies a lease API failure is kept
// distinct from a backend swap failure, so an operator can tell a control-plane
// blip from a serving outage (#1477).
func TestActivatorSurfacesLeaseUnavailable(t *testing.T) {
	memberCtrl := newFakeMemberController()
	a := NewActivator(context.Background(), memberCtrl, "r", nil)
	a.SetSwapCoordinator(&fakeCoordinator{err: ErrActivationLeaseUnavailable})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := a.Acquire(ctx, testPool("coder"))
	if !errors.Is(err, ErrActivationLeaseUnavailable) {
		t.Fatalf("Acquire = %v, want it to wrap ErrActivationLeaseUnavailable", err)
	}
	if got := memberCtrl.activateCount("coder"); got != 0 {
		t.Errorf("activate count = %d, want 0 (an unavailable lease must not drive a swap)", got)
	}
}
