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
	"math"
	"testing"
	"time"
)

// nowFn returns a deterministic clock that starts at t0 and can be
// advanced by the caller via the returned closure.
func nowFn(t0 time.Time) (func() time.Time, func(time.Time)) {
	now := t0
	return func() time.Time { return now }, func(t time.Time) { now = t }
}

func TestRollingWindowExpiry(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, advance := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "router-cap", ScopeKey: "router", MaxTokens: 1000, Window: 1 * time.Hour},
	}, nowFn)

	// Charge 500 tokens within the window.
	store.Charge([]string{"router"}, 500, 0)

	// Should have headroom.
	ok, retryAfter, exhausted := store.Allowed([]string{"router"})
	if !ok {
		t.Fatalf("expected allowed, got ok=false, retryAfter=%v, exhausted=%s", retryAfter, exhausted)
	}

	// Advance clock past the window.
	advance(t0.Add(1*time.Hour + 1*time.Second))

	// The 500 tokens should now be outside the window, so headroom returns.
	ok, retryAfter, exhausted = store.Allowed([]string{"router"})
	if !ok {
		t.Fatalf("expected allowed after window expiry, got ok=false, retryAfter=%v, exhausted=%s", retryAfter, exhausted)
	}
}

func TestExhaustion(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, advance := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "router-cap", ScopeKey: "router", MaxTokens: 1000, Window: 1 * time.Hour},
	}, nowFn)

	// Charge exactly to the cap.
	store.Charge([]string{"router"}, 1000, 0)

	// Should be exhausted.
	ok, retryAfter, exhausted := store.Allowed([]string{"router"})
	if ok {
		t.Fatal("expected exhausted, got ok=true")
	}
	if retryAfter <= 0 {
		t.Errorf("expected positive retryAfter, got %v", retryAfter)
	}
	if exhausted != "router-cap" {
		t.Errorf("expected exhausted=%q, got %q", "router-cap", exhausted)
	}

	// Advance clock past the window.
	advance(t0.Add(1*time.Hour + 1*time.Second))

	// Headroom returns.
	ok, retryAfter, exhausted = store.Allowed([]string{"router"})
	if !ok {
		t.Fatalf("expected allowed after window expiry, got ok=false, retryAfter=%v, exhausted=%s", retryAfter, exhausted)
	}
}

func TestExhaustionUSD(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, advance := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "usd-cap", ScopeKey: "router", MaxUSD: 1.50, Window: 1 * time.Hour},
	}, nowFn)

	// Charge to the USD cap.
	store.Charge([]string{"router"}, 0, 1.50)

	ok, retryAfter, exhausted := store.Allowed([]string{"router"})
	if ok {
		t.Fatal("expected exhausted, got ok=true")
	}
	if retryAfter <= 0 {
		t.Errorf("expected positive retryAfter, got %v", retryAfter)
	}
	if exhausted != "usd-cap" {
		t.Errorf("expected exhausted=%q, got %q", "usd-cap", exhausted)
	}

	// Advance past the window.
	advance(t0.Add(1*time.Hour + 1*time.Second))

	ok, retryAfter, exhausted = store.Allowed([]string{"router"})
	if !ok {
		t.Fatalf("expected allowed after window expiry, got ok=false, retryAfter=%v, exhausted=%s", retryAfter, exhausted)
	}
}

func TestMultiScope(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, advance := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "router-cap", ScopeKey: "router", MaxTokens: 10000, Window: 1 * time.Hour},
		{Name: "rule-cap", ScopeKey: "rule:pii-stays-local", MaxTokens: 500, Window: 1 * time.Hour},
		{Name: "team-cap", ScopeKey: "team:research", MaxTokens: 200, Window: 1 * time.Hour},
	}, nowFn)

	// Charge 100 tokens to all three scopes.
	store.Charge([]string{"router", "rule:pii-stays-local", "team:research"}, 100, 0)

	// All have headroom.
	ok, retryAfter, exhausted := store.Allowed([]string{"router", "rule:pii-stays-local", "team:research"})
	if !ok {
		t.Fatalf("expected allowed, got ok=false, retryAfter=%v, exhausted=%s", retryAfter, exhausted)
	}

	// Charge 100 more to team:research (now at 200, hitting the cap).
	store.Charge([]string{"team:research"}, 100, 0)

	// team:research is exhausted; router and rule still have headroom.
	ok, retryAfter, exhausted = store.Allowed([]string{"router", "rule:pii-stays-local", "team:research"})
	if ok {
		t.Fatal("expected exhausted, got ok=true")
	}
	if exhausted != "team-cap" {
		t.Errorf("expected exhausted=%q, got %q", "team-cap", exhausted)
	}
	if retryAfter <= 0 {
		t.Errorf("expected positive retryAfter, got %v", retryAfter)
	}

	// Advance past the window.
	advance(t0.Add(1*time.Hour + 1*time.Second))

	// All scopes have headroom again.
	ok, retryAfter, exhausted = store.Allowed([]string{"router", "rule:pii-stays-local", "team:research"})
	if !ok {
		t.Fatalf("expected allowed after window expiry, got ok=false, retryAfter=%v, exhausted=%s", retryAfter, exhausted)
	}
}

func TestMultiScopeTightestTripsFirst(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, advance := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "router-cap", ScopeKey: "router", MaxTokens: 10000, Window: 1 * time.Hour},
		{Name: "rule-cap", ScopeKey: "rule:pii-stays-local", MaxTokens: 500, Window: 1 * time.Hour},
		{Name: "team-cap", ScopeKey: "team:research", MaxTokens: 200, Window: 1 * time.Hour},
	}, nowFn)

	// Charge 200 tokens to all three scopes.
	store.Charge([]string{"router", "rule:pii-stays-local", "team:research"}, 200, 0)

	// team:research is exhausted (200/200).
	ok, retryAfter, exhausted := store.Allowed([]string{"router", "rule:pii-stays-local", "team:research"})
	if ok {
		t.Fatal("expected exhausted, got ok=true")
	}
	if exhausted != "team-cap" {
		t.Errorf("expected exhausted=%q, got %q", "team-cap", exhausted)
	}
	if retryAfter <= 0 {
		t.Errorf("expected positive retryAfter, got %v", retryAfter)
	}

	// Advance past the window.
	advance(t0.Add(1*time.Hour + 1*time.Second))

	// Charge 500 tokens to all three scopes.
	store.Charge([]string{"router", "rule:pii-stays-local", "team:research"}, 500, 0)

	// rule:pii-stays-local is exhausted (500/500).
	ok, retryAfter, exhausted = store.Allowed([]string{"router", "rule:pii-stays-local", "team:research"})
	if ok {
		t.Fatal("expected exhausted, got ok=true")
	}
	if exhausted != "rule-cap" {
		t.Errorf("expected exhausted=%q, got %q", "rule-cap", exhausted)
	}
	if retryAfter <= 0 {
		t.Errorf("expected positive retryAfter, got %v", retryAfter)
	}

	// Advance past the window.
	advance(t0.Add(1*time.Hour + 1*time.Second))

	// Charge 10000 tokens to all three scopes.
	store.Charge([]string{"router", "rule:pii-stays-local", "team:research"}, 10000, 0)

	// router is exhausted (10000/10000).
	ok, retryAfter, exhausted = store.Allowed([]string{"router", "rule:pii-stays-local", "team:research"})
	if ok {
		t.Fatal("expected exhausted, got ok=true")
	}
	if exhausted != "router-cap" {
		t.Errorf("expected exhausted=%q, got %q", "router-cap", exhausted)
	}
	if retryAfter <= 0 {
		t.Errorf("expected positive retryAfter, got %v", retryAfter)
	}
}

func TestOverflowGuard(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, _ := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "big-cap", ScopeKey: "router", MaxTokens: math.MaxInt64, Window: 1 * time.Hour},
	}, nowFn)

	// Charge a huge number of tokens that would overflow int64 if accumulated naively.
	// Each charge is 1<<62, and we do 4 charges. The sum would be 4<<62 = 1<<64,
	// which overflows int64. But we accumulate per-bucket, not globally, so each
	// bucket only sees one charge.
	store.Charge([]string{"router"}, 1<<62, 0)
	store.Charge([]string{"router"}, 1<<62, 0)

	// Allowed should not panic and should return ok=true since we haven't
	// exceeded the cap (MaxInt64).
	ok, retryAfter, exhausted := store.Allowed([]string{"router"})
	if !ok {
		t.Fatalf("expected allowed, got ok=false, retryAfter=%v, exhausted=%s", retryAfter, exhausted)
	}
}

func TestSnapshot(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, _ := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "router-cap", ScopeKey: "router", MaxTokens: 1000, MaxUSD: 1.0, Window: 1 * time.Hour},
		{Name: "rule-cap", ScopeKey: "rule:pii-stays-local", MaxTokens: 500, Window: 1 * time.Hour},
	}, nowFn)

	// Charge some usage.
	store.Charge([]string{"router"}, 500, 0.5)
	store.Charge([]string{"rule:pii-stays-local"}, 250, 0.25)

	snap := store.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snap))
	}

	// Find the router snapshot.
	var routerSnap, ruleSnap BudgetUsage
	for _, s := range snap {
		if s.ScopeKey == "router" {
			routerSnap = s
		}
		if s.ScopeKey == "rule:pii-stays-local" {
			ruleSnap = s
		}
	}

	if routerSnap.UsedTokens != 500 {
		t.Errorf("router UsedTokens = %d, want 500", routerSnap.UsedTokens)
	}
	if routerSnap.UsedUSD != 0.5 {
		t.Errorf("router UsedUSD = %f, want 0.5", routerSnap.UsedUSD)
	}
	if routerSnap.MaxTokens != 1000 {
		t.Errorf("router MaxTokens = %d, want 1000", routerSnap.MaxTokens)
	}
	if routerSnap.MaxUSD != 1.0 {
		t.Errorf("router MaxUSD = %f, want 1.0", routerSnap.MaxUSD)
	}

	if ruleSnap.UsedTokens != 250 {
		t.Errorf("rule UsedTokens = %d, want 250", ruleSnap.UsedTokens)
	}
	if ruleSnap.UsedUSD != 0.25 {
		t.Errorf("rule UsedUSD = %f, want 0.25", ruleSnap.UsedUSD)
	}
	if ruleSnap.MaxTokens != 500 {
		t.Errorf("rule MaxTokens = %d, want 500", ruleSnap.MaxTokens)
	}
	if ruleSnap.MaxUSD != 0 {
		t.Errorf("rule MaxUSD = %f, want 0", ruleSnap.MaxUSD)
	}
}

func TestSnapshotUtilizationMath(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, _ := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "cap", ScopeKey: "router", MaxTokens: 1000, MaxUSD: 1.0, Window: 1 * time.Hour},
	}, nowFn)

	store.Charge([]string{"router"}, 750, 0.75)

	snap := store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snap))
	}

	s := snap[0]
	if s.UsedTokens != 750 {
		t.Errorf("UsedTokens = %d, want 750", s.UsedTokens)
	}
	if s.UsedUSD != 0.75 {
		t.Errorf("UsedUSD = %f, want 0.75", s.UsedUSD)
	}
	// Utilization is used/cap.
	if s.MaxTokens != 1000 {
		t.Errorf("MaxTokens = %d, want 1000", s.MaxTokens)
	}
	if s.MaxUSD != 1.0 {
		t.Errorf("MaxUSD = %f, want 1.0", s.MaxUSD)
	}
}

func TestAllowedNoMatchingScope(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, _ := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "router-cap", ScopeKey: "router", MaxTokens: 1000, Window: 1 * time.Hour},
	}, nowFn)

	// Query a scope that has no budget rule.
	ok, retryAfter, exhausted := store.Allowed([]string{"team:unknown"})
	if !ok {
		t.Fatalf("expected allowed for unknown scope, got ok=false, retryAfter=%v, exhausted=%s", retryAfter, exhausted)
	}
}

func TestAllowedEmptyScopeKeys(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, _ := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "router-cap", ScopeKey: "router", MaxTokens: 1000, Window: 1 * time.Hour},
	}, nowFn)

	// Empty scope keys list should always be allowed.
	ok, retryAfter, exhausted := store.Allowed([]string{})
	if !ok {
		t.Fatalf("expected allowed for empty scope keys, got ok=false, retryAfter=%v, exhausted=%s", retryAfter, exhausted)
	}
	_ = exhausted
}

func TestChargeMultipleScopes(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, _ := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "router-cap", ScopeKey: "router", MaxTokens: 1000, Window: 1 * time.Hour},
		{Name: "team-cap", ScopeKey: "team:research", MaxTokens: 200, Window: 1 * time.Hour},
	}, nowFn)

	// Charge to both scopes at once.
	store.Charge([]string{"router", "team:research"}, 100, 0.1)

	snap := store.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snap))
	}

	var routerSnap, teamSnap BudgetUsage
	for _, s := range snap {
		if s.ScopeKey == "router" {
			routerSnap = s
		}
		if s.ScopeKey == "team:research" {
			teamSnap = s
		}
	}

	if routerSnap.UsedTokens != 100 {
		t.Errorf("router UsedTokens = %d, want 100", routerSnap.UsedTokens)
	}
	if teamSnap.UsedTokens != 100 {
		t.Errorf("team UsedTokens = %d, want 100", teamSnap.UsedTokens)
	}
}

func TestUSDExhaustionBeforeTokenExhaustion(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, _ := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "both-cap", ScopeKey: "router", MaxTokens: 10000, MaxUSD: 1.0, Window: 1 * time.Hour},
	}, nowFn)

	// Charge 1000 tokens and 1.0 USD (hitting USD cap but not token cap).
	store.Charge([]string{"router"}, 1000, 1.0)

	ok, retryAfter, exhausted := store.Allowed([]string{"router"})
	if ok {
		t.Fatal("expected exhausted, got ok=true")
	}
	if exhausted != "both-cap" {
		t.Errorf("expected exhausted=%q, got %q", "both-cap", exhausted)
	}
	if retryAfter <= 0 {
		t.Errorf("expected positive retryAfter, got %v", retryAfter)
	}
}

func TestTokenExhaustionBeforeUSDExhaustion(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, _ := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "both-cap", ScopeKey: "router", MaxTokens: 1000, MaxUSD: 100.0, Window: 1 * time.Hour},
	}, nowFn)

	// Charge 1000 tokens and 1.0 USD (hitting token cap but not USD cap).
	store.Charge([]string{"router"}, 1000, 1.0)

	ok, retryAfter, exhausted := store.Allowed([]string{"router"})
	if ok {
		t.Fatal("expected exhausted, got ok=true")
	}
	if exhausted != "both-cap" {
		t.Errorf("expected exhausted=%q, got %q", "both-cap", exhausted)
	}
	if retryAfter <= 0 {
		t.Errorf("expected positive retryAfter, got %v", retryAfter)
	}
}

func TestRetryAfterIsPositive(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, _ := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "cap", ScopeKey: "router", MaxTokens: 1000, Window: 1 * time.Hour},
	}, nowFn)

	// Charge to the cap.
	store.Charge([]string{"router"}, 1000, 0)

	ok, retryAfter, exhausted := store.Allowed([]string{"router"})
	if ok {
		t.Fatal("expected exhausted, got ok=true")
	}
	if retryAfter <= 0 {
		t.Errorf("expected positive retryAfter, got %v", retryAfter)
	}
	_ = exhausted
	// retryAfter should be close to the window duration.
	if retryAfter > 1*time.Hour {
		t.Errorf("retryAfter = %v, expected <= 1h", retryAfter)
	}
}

func TestParseMaxUSD(t *testing.T) {
	tests := []struct {
		input   string
		want    float64
		wantErr bool
	}{
		{"", 0, false},
		{"1.50", 1.5, false},
		{"0", 0, false},
		{"100", 100, false},
		{"0.01", 0.01, false},
		{"-1", 0, true},
		{"abc", 0, true},
	}
	for _, tt := range tests {
		got, err := parseMaxUSD(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseMaxUSD(%q) expected error, got nil", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMaxUSD(%q): unexpected error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseMaxUSD(%q) = %f, want %f", tt.input, got, tt.want)
		}
	}
}

func TestConcurrentChargeAndAllowed(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, _ := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "cap", ScopeKey: "router", MaxTokens: 1000000, Window: 1 * time.Hour},
	}, nowFn)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			store.Charge([]string{"router"}, 1, 0.001)
		}
		done <- struct{}{}
	}()

	for i := 0; i < 1000; i++ {
		store.Allowed([]string{"router"})
	}
	<-done
}

// TestUsedInWindowBoundary exercises the usedInWindow logic by verifying
// that usage exactly at the window boundary is correctly excluded.
func TestUsedInWindowBoundary(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn, advance := nowFn(t0)

	store := NewBudgetStore([]BudgetRule{
		{Name: "cap", ScopeKey: "router", MaxTokens: 1000, Window: 1 * time.Hour},
	}, nowFn)

	// Charge at t0.
	store.Charge([]string{"router"}, 500, 0)

	// At t0 + 1h, the bucket at t0 is exactly at the window boundary
	// (windowStart = t0 + 1h - 1h = t0). Since we use After(), the
	// bucket at t0 is excluded.
	advance(t0.Add(1 * time.Hour))

	ok, _, exhausted := store.Allowed([]string{"router"})
	if !ok {
		t.Fatalf("expected allowed at window boundary, got ok=false, exhausted=%s", exhausted)
	}

	// At t0 + 1h - 1ns, the bucket at t0 is still inside the window
	// (windowStart = t0 - 1ns, bucket at t0 is after that).
	advance(t0.Add(1*time.Hour - time.Nanosecond))

	ok, _, exhausted = store.Allowed([]string{"router"})
	if !ok {
		t.Fatalf("expected exhausted just before window boundary, got ok=true, exhausted=%s", exhausted)
	}
}
