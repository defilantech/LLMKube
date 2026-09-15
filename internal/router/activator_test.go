/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMemberController is an in-memory MemberController for activator tests. It
// records Activate calls and lets a test control when a member becomes Ready via
// setReady, so swap timing (drain-before-load, coalescing) is deterministic
// without a live cluster or sleeps.
type fakeMemberController struct {
	mu           sync.Mutex
	phase        map[string]string
	activateN    map[string]int
	deactivateN  map[string]int
	activateFail map[string]bool
	activateGate chan struct{} // when non-nil, Activate blocks until closed

	// replicas mirrors the member InferenceService's spec.replicas so a test can
	// assert what the pool was actually left at, not just how many calls landed.
	replicas map[string]int
	// noAutoReady stops Activate from making the target Ready, modelling a
	// member whose pod never comes up.
	noAutoReady bool
}

func newFakeMemberController() *fakeMemberController {
	return &fakeMemberController{
		phase:        make(map[string]string),
		activateN:    make(map[string]int),
		deactivateN:  make(map[string]int),
		activateFail: make(map[string]bool),
		replicas:     make(map[string]int),
	}
}

func (f *fakeMemberController) setPhase(name, phase string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phase[name] = phase
}

func (f *fakeMemberController) activateCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activateN[name]
}

func (f *fakeMemberController) deactivateCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deactivateN[name]
}

func (f *fakeMemberController) replicaCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replicas[name]
}

func (f *fakeMemberController) Activate(ctx context.Context, namespace, isvc string) error {
	f.mu.Lock()
	gate := f.activateGate
	fail := f.activateFail[isvc]
	f.activateN[isvc]++
	f.replicas[isvc] = 1
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fail {
		return errors.New("activate failed for " + isvc)
	}
	// Activation makes the target Ready in this fake (the real controller drains
	// the incumbent first; that invariant is covered by controller tests).
	f.mu.Lock()
	if !f.noAutoReady {
		f.phase[isvc] = modelReadyPhase
	}
	f.mu.Unlock()
	return nil
}

func (f *fakeMemberController) Deactivate(ctx context.Context, namespace, isvc string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deactivateN[isvc]++
	f.replicas[isvc] = 0
	f.phase[isvc] = ""
	return nil
}

func (f *fakeMemberController) WaitReady(ctx context.Context, namespace, isvc string) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		f.mu.Lock()
		p := f.phase[isvc]
		f.mu.Unlock()
		if p == modelReadyPhase {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (f *fakeMemberController) Phase(ctx context.Context, namespace, isvc string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.phase[isvc], nil
}

func testPool(member string) *BackendPool {
	return &BackendPool{
		Name:      "heavy-slot",
		Namespace: "lab",
		Member:    member,
		Members:   []string{"judge", "coder"},
	}
}

// TestActivatorColdActivate covers the cold-pool path: the first request to a
// member triggers exactly one activation and returns a release func.
func TestActivatorColdActivate(t *testing.T) {
	fake := newFakeMemberController()
	a := NewActivator(context.Background(), fake, "r", nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := a.Acquire(ctx, testPool("judge"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	if got := fake.activateCount("judge"); got != 1 {
		t.Errorf("judge activate count = %d, want 1", got)
	}
}

// TestActivatorSeedsResidentDefault verifies that when a member is already Ready
// (the controller warmed spec.default), the first request coalesces onto it
// without triggering a swap.
func TestActivatorSeedsResidentDefault(t *testing.T) {
	fake := newFakeMemberController()
	fake.setPhase("judge", modelReadyPhase)
	a := NewActivator(context.Background(), fake, "r", nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := a.Acquire(ctx, testPool("judge"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	if got := fake.activateCount("judge"); got != 0 {
		t.Errorf("judge activate count = %d, want 0 (already resident)", got)
	}
}

// TestActivatorCoalescesSameModel verifies that concurrent same-model requests
// share the resident member and trigger at most one activation (no per-request
// swap).
func TestActivatorCoalescesSameModel(t *testing.T) {
	fake := newFakeMemberController()
	a := NewActivator(context.Background(), fake, "r", nil)

	const n = 8
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			release, err := a.Acquire(ctx, testPool("judge"))
			if err != nil {
				failures.Add(1)
				return
			}
			release()
		}()
	}
	wg.Wait()

	if failures.Load() != 0 {
		t.Fatalf("%d concurrent same-model requests failed", failures.Load())
	}
	if got := fake.activateCount("judge"); got != 1 {
		t.Errorf("judge activate count = %d, want 1 (coalesced)", got)
	}
}

// TestActivatorSwapWhenIdle verifies a cross-model request flips the slot once
// the incumbent is idle, and that the swap drains before loading (idle
// preempts).
func TestActivatorSwapWhenIdle(t *testing.T) {
	fake := newFakeMemberController()
	a := NewActivator(context.Background(), fake, "r", nil)

	// Warm judge and release it (idle).
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rel, err := a.Acquire(ctx, testPool("judge"))
	if err != nil {
		t.Fatalf("Acquire judge: %v", err)
	}
	rel()

	// A coder request should flip the slot to coder.
	rel2, err := a.Acquire(ctx, testPool("coder"))
	if err != nil {
		t.Fatalf("Acquire coder: %v", err)
	}
	defer rel2()

	if got := fake.activateCount("coder"); got != 1 {
		t.Errorf("coder activate count = %d, want 1", got)
	}
}

// TestActivatorHoldBudgetExceeded verifies that when the target never becomes
// ready within the caller's budget, Acquire returns ErrHoldBudgetExceeded (which
// the proxy maps to 503 + Retry-After) and, with no other caller waiting, the
// pending activation is cancelled (Deactivate) so the pool does not complete a
// useless swap.
func TestActivatorHoldBudgetExceeded(t *testing.T) {
	fake := newFakeMemberController()
	// Block Activate so the swap never completes within the caller budget.
	fake.activateGate = make(chan struct{})
	a := NewActivator(context.Background(), fake, "r", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := a.Acquire(ctx, testPool("judge"))
	if !errors.Is(err, ErrHoldBudgetExceeded) {
		t.Fatalf("Acquire err = %v, want ErrHoldBudgetExceeded", err)
	}
	// Unblock so the goroutine can unwind cleanly.
	close(fake.activateGate)

	// The sole caller gave up: the pending activation for judge must be
	// cancelled so the pool does not leave judge scaled up for nobody.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if fake.deactivateCount("judge") >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("judge deactivate count = %d, want >=1 (cancel-on-timeout)", fake.deactivateCount("judge"))
}

// TestActivatorCoalesceHoldsCrossModelUntilDrain verifies the anti-thrash core:
// while the incumbent has in-flight work, a cross-model request is held open
// (not rejected) and the slot only flips once the incumbent drains.
func TestActivatorCoalesceHoldsCrossModelUntilDrain(t *testing.T) {
	fake := newFakeMemberController()
	a := NewActivator(context.Background(), fake, "r", nil)

	// judge resident with one in-flight request.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	judgeRel, err := a.Acquire(ctx, testPool("judge"))
	if err != nil {
		t.Fatalf("Acquire judge: %v", err)
	}

	// coder request starts; it must block until judge drains.
	coderDone := make(chan error, 1)
	var coderRelease func()
	go func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer ccancel()
		rel, e := a.Acquire(cctx, testPool("coder"))
		coderRelease = rel
		coderDone <- e
	}()

	// Give the coder goroutine time to reach its held/waiting state.
	select {
	case e := <-coderDone:
		t.Fatalf("coder acquired before judge drained (err=%v)", e)
	case <-time.After(50 * time.Millisecond):
	}

	// judge is still resident and coder has not been activated yet.
	if fake.activateCount("coder") != 0 {
		t.Errorf("coder activated while judge still had in-flight work")
	}

	// Drain judge; coder should now flip in.
	judgeRel()

	select {
	case e := <-coderDone:
		if e != nil {
			t.Fatalf("coder Acquire after drain: %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("coder did not acquire after judge drained")
	}
	if coderRelease != nil {
		coderRelease()
	}
	if got := fake.activateCount("coder"); got != 1 {
		t.Errorf("coder activate count = %d, want 1", got)
	}
}

// TestActivatorSwapFailurePropagates verifies a failed activation surfaces an
// error to the caller rather than hanging.
func TestActivatorSwapFailurePropagates(t *testing.T) {
	fake := newFakeMemberController()
	fake.activateFail = map[string]bool{"judge": true}
	a := NewActivator(context.Background(), fake, "r", nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := a.Acquire(ctx, testPool("judge"))
	if err == nil {
		t.Fatal("expected error from failed activation, got nil")
	}
	if errors.Is(err, ErrHoldBudgetExceeded) {
		t.Fatalf("failed activation should not be a budget error: %v", err)
	}
}

// stalePool builds a two-member pool whose member list matches its slot, used by
// the #1123 self-heal tests (testPool's Member is intentionally outside its
// Members list, which those tests must not inherit).
func stalePool() *BackendPool {
	return &BackendPool{
		Name:      "heavy-slot",
		Namespace: "lab",
		Member:    "gemma-longctx",
		Members:   []string{"coder", "gemma-longctx"},
	}
}

// TestActivatorSelfHealsStaleResident is the #1123 regression: after the
// resident member loses its pod out of band (a spec edit rolls it, an OOM-kill,
// a node drain, or a controller fallback to spec.default), the activator must
// not keep coalescing requests for that member onto its now-dead backend.
// reconcileResident re-derives residency from member phase so the next request
// drives a corrective swap instead of an instant 502. Before the fix pr.resident
// was authored only by seed() and the activator's own swaps, so this returned
// immediately with no new activation.
func TestActivatorSelfHealsStaleResident(t *testing.T) {
	fake := newFakeMemberController()
	a := NewActivator(context.Background(), fake, "r", nil)
	a.resyncInterval = 0 // re-verify residency on every Acquire

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// gemma-longctx becomes resident via a cold activation.
	rel, err := a.Acquire(ctx, stalePool())
	if err != nil {
		t.Fatalf("Acquire(gemma-longctx): %v", err)
	}
	rel()
	if got := fake.activateCount("gemma-longctx"); got != 1 {
		t.Fatalf("gemma-longctx activate count = %d, want 1", got)
	}

	// Out-of-band residency change: gemma-longctx's pod is gone and the
	// controller has fallen back to another Ready member.
	fake.setPhase("gemma-longctx", "Terminating")
	fake.setPhase("coder", modelReadyPhase)

	// A fresh request for the (now pod-less) gemma-longctx must re-derive
	// residency and drive a swap back to it, not blindly coalesce onto the dead
	// backend.
	rel2, err := a.Acquire(ctx, stalePool())
	if err != nil {
		t.Fatalf("Acquire(gemma-longctx) after stale residency: %v", err)
	}
	defer rel2()
	if got := fake.activateCount("gemma-longctx"); got != 2 {
		t.Errorf("gemma-longctx activate count = %d, want 2 (corrective swap)", got)
	}
}

// TestActivatorInvalidateResidentForcesRecheck covers the dispatch-failure
// self-heal (#1123, fix 3): within resyncInterval the activator trusts its
// cached resident (one API read per interval, not per request), but
// InvalidateResident — called by the proxy on a connection-level dispatch
// failure — forces the next Acquire to re-derive residency immediately rather
// than serving stale 502s until the periodic resync.
func TestActivatorInvalidateResidentForcesRecheck(t *testing.T) {
	fake := newFakeMemberController()
	a := NewActivator(context.Background(), fake, "r", nil)
	a.resyncInterval = time.Hour // never auto-rechecks during the test

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rel, err := a.Acquire(ctx, stalePool())
	if err != nil {
		t.Fatalf("Acquire(gemma-longctx): %v", err)
	}
	rel()

	// Out-of-band change, but the resync interval has not elapsed.
	fake.setPhase("gemma-longctx", "Terminating")
	fake.setPhase("coder", modelReadyPhase)

	// Without invalidation the belief is trusted: the request coalesces onto the
	// cached resident with no new activation. This is the deliberate rate limit.
	rel2, err := a.Acquire(ctx, stalePool())
	if err != nil {
		t.Fatalf("Acquire(gemma-longctx) within interval: %v", err)
	}
	rel2()
	if got := fake.activateCount("gemma-longctx"); got != 1 {
		t.Fatalf("gemma-longctx activate count = %d, want 1 (belief trusted within interval)", got)
	}

	// The proxy signals a connection-level failure; the next Acquire must
	// re-derive residency and drive a corrective swap.
	a.InvalidateResident(stalePool())
	rel3, err := a.Acquire(ctx, stalePool())
	if err != nil {
		t.Fatalf("Acquire(gemma-longctx) after invalidate: %v", err)
	}
	defer rel3()
	if got := fake.activateCount("gemma-longctx"); got != 2 {
		t.Errorf("gemma-longctx activate count = %d, want 2 (invalidation forced swap)", got)
	}
}

// TestActivatorIfIdleSkipsBusyIncumbent pins the IfIdle contract: a cross-model
// request against a busy incumbent returns ErrIncumbentBusy at once, starts no
// swap and leaves the incumbent resident. Once the incumbent drains, the same
// request swaps exactly as it would under Wait.
func TestActivatorIfIdleSkipsBusyIncumbent(t *testing.T) {
	fake := newFakeMemberController()
	a := NewActivator(context.Background(), fake, "r", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	judgeRel, err := a.Acquire(ctx, testPool("judge"))
	if err != nil {
		t.Fatalf("Acquire judge: %v", err)
	}

	start := time.Now()
	rel, err := a.AcquireWithMode(ctx, testPool("coder"), PoolActivationIfIdle)
	if !errors.Is(err, ErrIncumbentBusy) {
		t.Fatalf("IfIdle against busy incumbent: err = %v, want ErrIncumbentBusy", err)
	}
	if rel != nil {
		t.Error("IfIdle returned a release func alongside an error")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("IfIdle held for %v; must return without waiting on the incumbent", elapsed)
	}
	if got := fake.activateCount("coder"); got != 0 {
		t.Errorf("coder activate count = %d, want 0 (no swap while incumbent busy)", got)
	}
	a.mu.Lock()
	resident := a.pools["lab/heavy-slot"].resident
	swapping := a.pools["lab/heavy-slot"].swapping
	a.mu.Unlock()
	if resident != "judge" || swapping {
		t.Errorf("resident = %q swapping = %v after IfIdle skip; want judge resident, no swap", resident, swapping)
	}

	// Drain the incumbent: IfIdle now behaves like Wait and flips the slot.
	judgeRel()
	rel, err = a.AcquireWithMode(ctx, testPool("coder"), PoolActivationIfIdle)
	if err != nil {
		t.Fatalf("IfIdle against idle incumbent: %v", err)
	}
	rel()
	if got := fake.activateCount("coder"); got != 1 {
		t.Errorf("coder activate count = %d, want 1 after the incumbent went idle", got)
	}
}

// TestActivatorIfIdleWaitsOnInFlightSwap guards the one case IfIdle must NOT
// short-circuit: a swap towards the target is already running. The incumbent is
// unloading and cannot serve, so the request waits for the swap like Wait does.
func TestActivatorIfIdleWaitsOnInFlightSwap(t *testing.T) {
	fake := newFakeMemberController()
	gate := make(chan struct{})
	fake.activateGate = gate
	a := NewActivator(context.Background(), fake, "r", nil)

	// Cold pool: the first Wait request starts a swap that blocks on the gate.
	waitDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		rel, e := a.Acquire(ctx, testPool("coder"))
		if rel != nil {
			rel()
		}
		waitDone <- e
	}()
	deadline := time.Now().Add(time.Second)
	for {
		a.mu.Lock()
		pr, ok := a.pools["lab/heavy-slot"]
		swapping := ok && pr.swapping
		a.mu.Unlock()
		if swapping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("swap never started")
		}
		time.Sleep(time.Millisecond)
	}

	ifIdleDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		rel, e := a.AcquireWithMode(ctx, testPool("coder"), PoolActivationIfIdle)
		if rel != nil {
			rel()
		}
		ifIdleDone <- e
	}()
	select {
	case e := <-ifIdleDone:
		t.Fatalf("IfIdle returned %v during an in-flight swap; must wait for it", e)
	case <-time.After(50 * time.Millisecond):
	}

	close(gate)
	for _, ch := range []chan error{waitDone, ifIdleDone} {
		select {
		case e := <-ch:
			if e != nil {
				t.Fatalf("Acquire after swap completed: %v", e)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("request did not complete after the swap finished")
		}
	}
	if got := fake.activateCount("coder"); got != 1 {
		t.Errorf("coder activate count = %d, want 1 (one swap shared by both requests)", got)
	}
}

// waitFor polls cond until it holds or the timeout elapses, so tests can
// synchronise on activator state without fixed sleeps.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func budgetPool(member string, budget time.Duration) *BackendPool {
	return &BackendPool{
		Name:       "heavy-slot",
		Namespace:  "lab",
		Member:     member,
		Members:    []string{"judge", "coder"},
		SwapBudget: budget,
	}
}

// TestActivatorStaleDeactivateDoesNotUndoNewActivation is the #1837 wedge, half
// one: the last waiter for a member leaves and schedules a deactivate, a new
// request for the same member arrives in that window and activates it, and the
// stale deactivate must not scale it back to zero. It used to, and because
// Activate is idempotent-by-check and never re-issued, the new swap then waited
// forever on a member that could never become Ready.
func TestActivatorStaleDeactivateDoesNotUndoNewActivation(t *testing.T) {
	fake := newFakeMemberController()
	fake.noAutoReady = true
	a := NewActivator(context.Background(), fake, "r", nil)

	secondDone := make(chan error, 1)
	var once sync.Once
	a.beforeDeactivate = func(member string) {
		if member != "coder" {
			return
		}
		once.Do(func() {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				release, err := a.Acquire(ctx, testPool("coder"))
				if err == nil {
					release()
				}
				secondDone <- err
			}()
			// Hold the deactivate in its old losing window until the second
			// waiter has actually issued its Activate.
			waitFor(t, 3*time.Second, "second Activate for coder", func() bool {
				return fake.activateCount("coder") >= 2
			})
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := a.Acquire(ctx, testPool("coder")); !errors.Is(err, ErrHoldBudgetExceeded) {
		t.Fatalf("first Acquire err = %v, want ErrHoldBudgetExceeded", err)
	}

	waitFor(t, 3*time.Second, "second waiter to activate coder", func() bool {
		return fake.activateCount("coder") >= 2
	})
	// Give the stale deactivate time to land if it is going to.
	time.Sleep(250 * time.Millisecond)

	if got := fake.deactivateCount("coder"); got != 0 {
		t.Fatalf("Deactivate(coder) called %d times; the stale deactivate undid a live activation", got)
	}
	if got := fake.replicaCount("coder"); got != 1 {
		t.Fatalf("coder replicas = %d, want 1", got)
	}

	fake.setPhase("coder", modelReadyPhase)
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second Acquire: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second waiter never served: the swap is waiting on a member that was scaled back to zero")
	}
}

// TestActivatorAbandonsSwapAtDeadline is the #1837 wedge, half two: a swap whose
// target never becomes Ready must be abandoned at the pool's swap budget rather
// than pinning swapping=true forever. Before the fix the swap ran under an
// unbounded context, so the resident's fast path stayed closed and every request
// on the pool burned its hold budget and 503ed until the proxy restarted.
func TestActivatorAbandonsSwapAtDeadline(t *testing.T) {
	fake := newFakeMemberController()
	fake.noAutoReady = true
	fake.setPhase("judge", modelReadyPhase)
	a := NewActivator(context.Background(), fake, "r", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := a.Acquire(ctx, budgetPool("coder", 150*time.Millisecond))
	if err == nil {
		t.Fatal("Acquire succeeded; want the swap to be abandoned at its deadline")
	}
	if errors.Is(err, ErrHoldBudgetExceeded) {
		t.Fatalf("Acquire err = %v after %s; the swap was never bounded, the caller just ran out of hold budget",
			err, time.Since(start))
	}

	a.mu.Lock()
	pr := a.pools["lab/heavy-slot"]
	swapping := pr.swapping
	resident := pr.resident
	a.mu.Unlock()
	if swapping {
		t.Fatal("pool still marked swapping after the swap was abandoned")
	}
	if resident != "judge" {
		t.Fatalf("resident = %q, want judge to still own the slot", resident)
	}

	waitFor(t, 3*time.Second, "abandoned target to be deactivated", func() bool {
		return fake.deactivateCount("coder") >= 1
	})

	// The resident's fast path must be open again: no swap, no activation, and
	// well inside the hold budget.
	fastCtx, fastCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer fastCancel()
	release, err := a.Acquire(fastCtx, budgetPool("judge", 150*time.Millisecond))
	if err != nil {
		t.Fatalf("Acquire for the resident member after an abandoned swap: %v", err)
	}
	release()
	if got := fake.activateCount("judge"); got != 0 {
		t.Fatalf("Activate(judge) called %d times; the resident did not take the fast path", got)
	}
}
