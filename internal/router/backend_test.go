/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"testing"
	"time"
)

// TestDispatcherHalfOpenRecovery covers the core circuit-breaker
// invariant: MarkUnhealthy quarantines the backend for the configured
// duration, after which IsHealthy returns true again (half-open
// probe), and MarkHealthy clears the quarantine outright.
//
// Regression test for the bug where a single transient timeout
// permanently quarantined a backend and the dispatcher's IsHealthy
// check stayed false forever (the only "back to healthy" path was a
// successful Dispatch, which IsHealthy itself gated).
func TestDispatcherHalfOpenRecovery(t *testing.T) {
	cfg := &Config{Backends: []Backend{{Name: "primary", Tier: "local"}}}

	now := time.Unix(0, 0)
	disp := NewDispatcher(cfg,
		WithQuarantineDuration(100*time.Millisecond),
		WithNowFunc(func() time.Time { return now }),
	)

	if !disp.IsHealthy("primary") {
		t.Fatal("fresh backend should be healthy")
	}

	disp.MarkUnhealthy("primary")
	if disp.IsHealthy("primary") {
		t.Fatal("backend is quarantined; IsHealthy must return false until the window expires")
	}

	// Advance just shy of the window: still quarantined.
	now = now.Add(99 * time.Millisecond)
	if disp.IsHealthy("primary") {
		t.Fatal("inside the quarantine window IsHealthy must remain false")
	}

	// Advance past the window: half-open probe permitted.
	now = now.Add(2 * time.Millisecond)
	if !disp.IsHealthy("primary") {
		t.Fatal("after quarantineDuration IsHealthy must return true so the next dispatch probes")
	}

	// A successful Dispatch would call MarkHealthy. Simulate that and
	// verify quarantine is cleared.
	disp.MarkHealthy("primary")
	if !disp.IsHealthy("primary") {
		t.Fatal("MarkHealthy should clear quarantine")
	}
}

// TestDispatcherRepeatedFailureExtendsQuarantine covers the "still
// dead after probe" case. A probe that hits MarkUnhealthy again must
// push the quarantine deadline forward, not leave the backend
// immediately eligible for another probe.
func TestDispatcherRepeatedFailureExtendsQuarantine(t *testing.T) {
	cfg := &Config{Backends: []Backend{{Name: "flaky", Tier: "cloud"}}}

	now := time.Unix(0, 0)
	disp := NewDispatcher(cfg,
		WithQuarantineDuration(100*time.Millisecond),
		WithNowFunc(func() time.Time { return now }),
	)

	disp.MarkUnhealthy("flaky")
	now = now.Add(150 * time.Millisecond) // past the first window
	if !disp.IsHealthy("flaky") {
		t.Fatal("first quarantine window should have expired")
	}

	// Probe failed; MarkUnhealthy fires again.
	disp.MarkUnhealthy("flaky")
	if disp.IsHealthy("flaky") {
		t.Fatal("a fresh MarkUnhealthy must re-quarantine; backend is not immediately probeable")
	}

	now = now.Add(50 * time.Millisecond) // inside the new window
	if disp.IsHealthy("flaky") {
		t.Fatal("inside the second quarantine window IsHealthy must remain false")
	}

	now = now.Add(60 * time.Millisecond) // past the new window
	if !disp.IsHealthy("flaky") {
		t.Fatal("after the extended window IsHealthy must return true again")
	}
}

// TestDispatcherUnknownBackendIsUnhealthy preserves the pre-existing
// invariant that a name not in the config registers as unhealthy
// (defensive in the controller-restart race where a backend was
// renamed and the proxy hasn't picked up the new config yet).
func TestDispatcherUnknownBackendIsUnhealthy(t *testing.T) {
	disp := NewDispatcher(&Config{})
	if disp.IsHealthy("does-not-exist") {
		t.Error("unknown backend should be reported unhealthy")
	}
}
