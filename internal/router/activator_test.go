/*
Copyright 2026.

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

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
	// phaseErr, when non-nil for a member, makes WaitReady fail with it after
	// waitFor long, the way a failed API read does after the scale-up patched.
	phaseErr map[string]error
}

func newFakeMemberController() *fakeMemberController {
	return &fakeMemberController{
		phase:        make(map[string]string),
		activateN:    make(map[string]int),
		deactivateN:  make(map[string]int),
		activateFail: make(map[string]bool),
		replicas:     make(map[string]int),
		phaseErr:     make(map[string]error),
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
		perr := f.phaseErr[isvc]
		f.mu.Unlock()
		if perr != nil {
			select {
			case <-time.After(100 * time.Millisecond):
				return perr
			case <-ctx.Done():
				return ctx.Err()
			}
		}
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

// TestActivatorFreshSwapAfterUnwaitedAbandon verifies that a swap failure
// recorded while nobody was waiting is not surfaced to a request that arrives
// afterwards: it starts a fresh load instead of failing with a swap it never
// made.
func TestActivatorFreshSwapAfterUnwaitedAbandon(t *testing.T) {
	fake := newFakeMemberController()
	fake.noAutoReady = true
	fake.setPhase("coder", modelReadyPhase)
	a := NewActivator(context.Background(), fake, "r", nil)

	// The first caller gives up while the swap is still activating; the swap
	// then fails with nobody waiting on it.
	fake.activateGate = make(chan struct{})
	fake.activateFail = map[string]bool{"judge": true}
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer firstCancel()
	if _, err := a.Acquire(firstCtx, budgetPool("judge", 5*time.Second)); !errors.Is(err, ErrHoldBudgetExceeded) {
		t.Fatalf("first Acquire err = %v, want ErrHoldBudgetExceeded", err)
	}
	close(fake.activateGate)
	waitFor(t, "the unwaited swap to record its failure", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		pr := a.pools["lab/heavy-slot"]
		return !pr.swapping && len(pr.swapErr) > 0
	})

	// A later request gets its own load, not the abandoned swap's error.
	fake.mu.Lock()
	fake.activateFail = map[string]bool{}
	fake.activateGate = nil
	fake.mu.Unlock()
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer secondCancel()
	done := make(chan error, 1)
	go func() {
		rel, err := a.Acquire(secondCtx, budgetPool("judge", 5*time.Second))
		if err == nil {
			rel()
		}
		done <- err
	}()
	waitFor(t, "the second request to activate judge", func() bool {
		return fake.activateCount("judge") >= 2
	})
	fake.setPhase("judge", modelReadyPhase)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second Acquire err = %v; a request was failed with a swap it never made", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second Acquire never completed")
	}
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

// TestActivatorKeepsNewResidentForItsWaiters is the window right after a swap
// completes (#2533). The swap goroutine makes judge resident and wakes every
// held request at once. If a coder request takes the lock before the judge
// requests that waited for that swap, judge has no in-flight request yet, so
// it looked idle and coder started swapping it out again before a single
// judge request was served: the requested model loads, the other one is
// swapped in, then the requested one again. A resident with requests still
// registered for it is about to serve them, so it counts as busy.
func TestActivatorKeepsNewResidentForItsWaiters(t *testing.T) {
	for _, mode := range []string{PoolActivationWait, PoolActivationIfIdle} {
		t.Run(mode, func(t *testing.T) {
			fake := newFakeMemberController()
			fake.setPhase("judge", modelReadyPhase)
			a := NewActivator(context.Background(), fake, "r", nil)

			// The state the swap to judge leaves: judge resident and freshly
			// verified, no swap running, and one judge request woken but not yet
			// back under the lock (registered, nothing in flight).
			a.mu.Lock()
			pr := a.runtime(testPool("judge"))
			pr.resident = "judge"
			pr.residentCheckedAt = time.Now()
			pr.waiting["judge"] = 1
			a.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := a.AcquireWithMode(ctx, testPool("coder"), mode)

			if got := fake.activateCount("coder"); got != 0 {
				t.Fatalf("coder activated %d times while a judge request waited for the swap to judge", got)
			}
			want := ErrHoldBudgetExceeded
			if mode == PoolActivationIfIdle {
				want = ErrIncumbentBusy
			}
			if !errors.Is(err, want) {
				t.Errorf("coder Acquire err = %v, want %v", err, want)
			}
		})
	}
}

// TestActivatorWakesWaitersWhenResidentSelfHeals: reconcileResident can move
// the resident without a swap, when the member it believed in is gone and
// another reports Ready (a pod lost out of band, a member scaled up by hand).
// Requests already held for that member must be woken to take it. Left asleep,
// they would also count as waiting for the resident and keep every other
// member's request from swapping until their hold budget ran out (#2533).
func TestActivatorWakesWaitersWhenResidentSelfHeals(t *testing.T) {
	fake := newFakeMemberController()
	fake.setPhase("judge", modelReadyPhase)
	a := NewActivator(context.Background(), fake, "r", nil)

	// judge resident and busy; a coder request is held behind it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	judgeRel, err := a.Acquire(ctx, testPool("judge"))
	if err != nil {
		t.Fatalf("Acquire judge: %v", err)
	}
	defer judgeRel()
	held := make(chan error, 1)
	go func() {
		hctx, hcancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer hcancel()
		rel, e := a.Acquire(hctx, testPool("coder"))
		if rel != nil {
			rel()
		}
		held <- e
	}()
	select {
	case e := <-held:
		t.Fatalf("coder acquired while judge was busy (err=%v)", e)
	case <-time.After(50 * time.Millisecond):
	}

	// Out of band, judge's pod goes and coder comes up Ready. The next request
	// re-verifies the resident and adopts coder.
	fake.setPhase("judge", "")
	fake.setPhase("coder", modelReadyPhase)
	a.InvalidateResident(testPool("coder"))
	nctx, ncancel := context.WithTimeout(context.Background(), time.Second)
	defer ncancel()
	rel, err := a.Acquire(nctx, testPool("coder"))
	if err != nil {
		t.Fatalf("second coder Acquire: %v", err)
	}
	// Still in flight: its release would wake the held request on its own, so
	// only the residency change may do it here.
	defer rel()

	select {
	case e := <-held:
		if e != nil {
			t.Fatalf("held coder request: %v", e)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("the held coder request was not woken when coder became the resident")
	}
	if got := fake.activateCount("coder"); got != 0 {
		t.Errorf("coder activated %d times; it was already Ready", got)
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
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	const timeout = 3 * time.Second
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

// TestDeactivateIfUnwantedGuards pins the #1837 guard directly: a deferred
// deactivate never lands on a member the pool wants, so it can never scale back
// a member that an activation issued after it was scheduled just scaled up.
func TestDeactivateIfUnwantedGuards(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(pr *poolRuntime)
		want    int
	}{
		{
			name:    "deactivates a member nothing claims",
			prepare: func(pr *poolRuntime) {},
			want:    1,
		},
		{
			name: "keeps a member that was wanted again",
			prepare: func(pr *poolRuntime) {
				pr.waiting["coder"] = 1
			},
			want: 0,
		},
		{
			name: "keeps the resident",
			prepare: func(pr *poolRuntime) {
				pr.resident = "coder"
			},
			want: 0,
		},
		{
			name: "keeps a member an in-flight swap is activating",
			prepare: func(pr *poolRuntime) {
				pr.swapping = true
				pr.swapTarget = "coder"
				pr.swapGen = 7
			},
			want: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeMemberController()
			a := NewActivator(context.Background(), fake, "r", nil)
			a.mu.Lock()
			pr := a.runtime(testPool("coder"))
			pr.swapping = false
			pr.swapTarget = ""
			tc.prepare(pr)
			a.mu.Unlock()

			a.deactivateIfUnwanted(pr, "coder")

			if got := fake.deactivateCount("coder"); got != tc.want {
				t.Fatalf("Deactivate(coder) called %d times, want %d", got, tc.want)
			}
		})
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
	failures := prommetrics.ModelPoolSwapFailuresTotal.WithLabelValues("r", "heavy-slot", "coder", "deadline")
	before := testutil.ToFloat64(failures)

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
	if !errors.Is(err, ErrSwapDeadlineExceeded) {
		t.Fatalf("Acquire err = %v, want ErrSwapDeadlineExceeded so the proxy answers 503 + Retry-After", err)
	}
	if got := testutil.ToFloat64(failures) - before; got != 1 {
		t.Errorf("swap_failures_total{reason=deadline} delta = %v, want 1", got)
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

	waitFor(t, "abandoned target to be deactivated", func() bool {
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

// TestActivatorCleansUpFailedSwapWithNoWaiter covers the swap failure that is not
// the deadline: Activate has already scaled the target up, then the readiness
// read fails and the caller that wanted it has gone. Nothing cancels the swap
// any more, so without an explicit scale-down the target would hold the slot
// with no traffic.
func TestActivatorCleansUpFailedSwapWithNoWaiter(t *testing.T) {
	fake := newFakeMemberController()
	fake.setPhase("judge", modelReadyPhase)
	fake.mu.Lock()
	fake.phaseErr["coder"] = errors.New("get member lab/coder: connection reset")
	fake.mu.Unlock()
	a := NewActivator(context.Background(), fake, "r", nil)

	// The only caller for coder gives up while the swap is still running.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := a.Acquire(ctx, budgetPool("coder", 5*time.Second)); !errors.Is(err, ErrHoldBudgetExceeded) {
		t.Fatalf("Acquire err = %v, want ErrHoldBudgetExceeded", err)
	}

	waitFor(t, "the failed swap's target to be scaled back down", func() bool {
		return fake.deactivateCount("coder") >= 1
	})
	if got := fake.replicaCount("coder"); got != 0 {
		t.Fatalf("coder replicas = %d after the swap failed with no waiter; want 0", got)
	}
	if got := fake.activateCount("coder"); got != 1 {
		t.Fatalf("coder activate count = %d, want 1", got)
	}
}
