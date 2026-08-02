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
}

func newFakeMemberController() *fakeMemberController {
	return &fakeMemberController{
		phase:        make(map[string]string),
		activateN:    make(map[string]int),
		deactivateN:  make(map[string]int),
		activateFail: make(map[string]bool),
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

func (f *fakeMemberController) Activate(ctx context.Context, namespace, isvc string) error {
	f.mu.Lock()
	gate := f.activateGate
	fail := f.activateFail[isvc]
	f.activateN[isvc]++
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
	f.phase[isvc] = modelReadyPhase
	f.mu.Unlock()
	return nil
}

func (f *fakeMemberController) Deactivate(ctx context.Context, namespace, isvc string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deactivateN[isvc]++
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
