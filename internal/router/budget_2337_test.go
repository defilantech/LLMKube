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

package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

const teamRuleKey2337 = "team:x-llmkube-team"

func teamKey2337(v string) string { return teamRuleKey2337 + ":" + v }

// TestBudgetIdleTeamKeysEvicted is the reproduction from #2337: 10,000
// distinct team values, the window passes, one more request. Only the key
// with usage inside the window may remain.
func TestBudgetIdleTeamKeysEvicted(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clock, advance := nowFn(t0)
	store := NewBudgetStore([]BudgetRule{
		{Name: "per-team", ScopeKey: teamRuleKey2337, MaxTokens: 1000000, Window: time.Hour},
	}, clock)

	for i := 0; i < 10000; i++ {
		store.Charge([]string{teamKey2337(fmt.Sprintf("t%05d", i))}, 1, 0)
	}
	if got := len(store.buckets); got != 10000 {
		t.Fatalf("keys after 10,000 values = %d, want 10000", got)
	}

	advance(t0.Add(time.Hour + 2*time.Second))
	store.Charge([]string{teamKey2337("late")}, 1, 0)

	if got := len(store.buckets); got != 1 {
		t.Fatalf("keys after the window passed and one more request = %d, want 1 (only the active team)", got)
	}
	if _, ok := store.buckets[teamKey2337("late")]; !ok {
		t.Fatal("the active team's key was evicted")
	}
	if store.teamKeys != 1 {
		t.Errorf("teamKeys = %d, want 1 (the cap counter must follow eviction)", store.teamKeys)
	}

	snap := store.Snapshot()
	got := map[string]int64{}
	for _, u := range snap {
		got[u.ScopeKey] = u.UsedTokens
	}
	want := map[string]int64{teamRuleKey2337: 0, teamKey2337("late"): 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
}

// TestBudgetSnapshotEvictsIdleKeys pins that a snapshot alone (the operator's
// admin poll, with no request traffic) deletes idle keys and reports only the
// live ones, while configured rules still read as zero.
func TestBudgetSnapshotEvictsIdleKeys(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clock, advance := nowFn(t0)
	store := NewBudgetStore([]BudgetRule{
		{Name: "router-cap", ScopeKey: "router", MaxTokens: 1000, Window: time.Hour},
		{Name: "per-team", ScopeKey: teamRuleKey2337, MaxTokens: 1000, Window: time.Hour},
	}, clock)

	store.Charge([]string{"router", teamKey2337("alpha")}, 10, 0)
	store.Charge([]string{"router", teamKey2337("beta")}, 10, 0)
	advance(t0.Add(50 * time.Minute))
	store.Charge([]string{"router", teamKey2337("alpha")}, 5, 0)

	advance(t0.Add(time.Hour + time.Second))
	snap := store.Snapshot()
	got := map[string]int64{}
	for _, u := range snap {
		got[u.ScopeKey] = u.UsedTokens
	}
	want := map[string]int64{"router": 5, teamRuleKey2337: 0, teamKey2337("alpha"): 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %v, want %v (beta has no usage left in its window)", got, want)
	}
	if _, ok := store.buckets[teamKey2337("beta")]; ok {
		t.Error("beta's key is still stored after a snapshot past its window")
	}

	// Past every window the router rule still reads zero rather than vanishing.
	advance(t0.Add(3 * time.Hour))
	snap = store.Snapshot()
	got = map[string]int64{}
	for _, u := range snap {
		got[u.ScopeKey] = u.UsedTokens
	}
	want = map[string]int64{"router": 0, teamRuleKey2337: 0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	if len(store.buckets) != 0 || store.teamKeys != 0 {
		t.Errorf("store keeps %d keys (teamKeys %d), want none", len(store.buckets), store.teamKeys)
	}
}

// TestBudgetSnapshotOmitsExpiredKeyBeforeSweep pins the reader half of the
// "absent equals empty" rule: inside one sweep interval a key can have lost
// all of its usage without being deleted yet, and the snapshot must not
// report it.
func TestBudgetSnapshotOmitsExpiredKeyBeforeSweep(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clock, advance := nowFn(t0)
	store := NewBudgetStore([]BudgetRule{
		{Name: "per-team", ScopeKey: teamRuleKey2337, MaxTokens: 1000, Window: time.Hour},
	}, clock)

	store.Charge([]string{teamKey2337("alpha")}, 10, 0)
	// This charge sweeps while alpha is still inside its window.
	advance(t0.Add(time.Hour - 500*time.Millisecond))
	store.Charge([]string{teamKey2337("beta")}, 10, 0)

	// 600ms later alpha's only bucket has left the window; no sweep is due.
	advance(t0.Add(time.Hour + 100*time.Millisecond))
	snap := store.Snapshot()
	for _, u := range snap {
		if u.ScopeKey == teamKey2337("alpha") {
			t.Fatalf("snapshot reports alpha (%+v) after its usage left the window", u)
		}
	}
	if _, ok := store.buckets[teamKey2337("alpha")]; !ok {
		t.Fatal("alpha was deleted inside the sweep interval; this test no longer covers the unswept case")
	}
}

// TestBudgetSweepNeverChangesDecisions drives two stores through the same
// charges: one sweeps on every tick, the other never sweeps. Every Allowed
// answer and every snapshot must agree, including for budgets sitting at or
// one token under their cap while idle keys around them are evicted.
func TestBudgetSweepNeverChangesDecisions(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	rules := []BudgetRule{
		{Name: "router-cap", ScopeKey: "router", MaxTokens: 5000, Window: 20 * time.Second},
		{Name: "rule-cap", ScopeKey: "rule:all", MaxTokens: 50, MaxUSD: 0.5, Window: 7 * time.Second},
		{Name: "per-team", ScopeKey: teamRuleKey2337, MaxTokens: 100, Window: 10 * time.Second},
	}
	sweptClock, sweptAdvance := nowFn(t0)
	refClock, refAdvance := nowFn(t0)
	swept := NewBudgetStore(rules, sweptClock)
	ref := NewBudgetStore(rules, refClock)
	ref.lastSweep = t0.Add(1000 * time.Hour) // never due: ref keeps every key

	type charge struct {
		keys   []string
		tokens int64
		usd    float64
	}
	plan := map[int][]charge{
		0:  {{[]string{"router", teamKey2337("at-cap")}, 100, 0}},
		1:  {{[]string{"router", "rule:all", teamKey2337("idle")}, 30, 0.1}},
		3:  {{[]string{teamKey2337("near")}, 99, 0}},
		6:  {{[]string{"rule:all", teamKey2337("idle2")}, 20, 0.39}},
		9:  {{[]string{teamKey2337("at-cap")}, 0, 0}},
		12: {{[]string{teamKey2337("near")}, 0, 0}},
		15: {{[]string{"router", teamKey2337("at-cap")}, 100, 0}},
	}
	probe := []string{"router", "rule:all", teamKey2337("at-cap"), teamKey2337("near"),
		teamKey2337("idle"), teamKey2337("idle2"), teamKey2337("never")}

	for step := 0; step <= 60; step++ {
		now := t0.Add(time.Duration(step) * 500 * time.Millisecond)
		sweptAdvance(now)
		refAdvance(now)
		if step%2 == 0 {
			for _, c := range plan[step/2] {
				swept.Charge(c.keys, c.tokens, c.usd)
				ref.Charge(c.keys, c.tokens, c.usd)
			}
		}
		swept.mu.Lock()
		swept.sweepLocked(now)
		swept.mu.Unlock()

		for _, k := range probe {
			gotOK, gotRetry, gotName := swept.Allowed([]string{k})
			wantOK, wantRetry, wantName := ref.Allowed([]string{k})
			if gotOK != wantOK || gotRetry != wantRetry || gotName != wantName {
				t.Fatalf("t+%v Allowed(%s) = (%v, %v, %q) after sweep, want (%v, %v, %q) as without it",
					now.Sub(t0), k, gotOK, gotRetry, gotName, wantOK, wantRetry, wantName)
			}
		}
		if got, want := swept.Snapshot(), ref.Snapshot(); !reflect.DeepEqual(got, want) {
			t.Fatalf("t+%v snapshot after sweep = %+v, want %+v", now.Sub(t0), got, want)
		}
	}
	if len(ref.buckets) <= len(swept.buckets) {
		t.Fatalf("swept store holds %d keys, unswept %d: the sweep evicted nothing, so the test proves nothing",
			len(swept.buckets), len(ref.buckets))
	}

	// The exhausted team stays exhausted across a sweep with the same
	// Retry-After it had before it.
	exhausted := NewBudgetStore(rules, sweptClock)
	sweptAdvance(t0)
	exhausted.Charge([]string{teamKey2337("idle")}, 1, 0)
	sweptAdvance(t0.Add(5 * time.Second))
	exhausted.Charge([]string{teamKey2337("at-cap")}, 100, 0)
	sweptAdvance(t0.Add(12 * time.Second))
	beforeOK, beforeRetry, _ := exhausted.Allowed([]string{teamKey2337("at-cap")})
	exhausted.Charge([]string{"router"}, 1, 0) // a sweep is due: idle is evicted
	if _, ok := exhausted.buckets[teamKey2337("idle")]; ok {
		t.Fatal("idle team was not evicted by the sweep")
	}
	afterOK, afterRetry, afterName := exhausted.Allowed([]string{teamKey2337("at-cap")})
	if beforeOK || afterOK || afterRetry != beforeRetry || afterRetry != 3*time.Second || afterName != "per-team" {
		t.Fatalf("at-cap team: before sweep (%v, %v), after (%v, %v, %q); want exhausted with Retry-After 3s both times",
			beforeOK, beforeRetry, afterOK, afterRetry, afterName)
	}
}

// TestBudgetTeamKeyCap fills the store to maxTeamKeys and checks that a new
// value folds into the per-budget overflow key, which is still capped, while
// existing values keep their own keys and caps.
func TestBudgetTeamKeyCap(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clock, advance := nowFn(t0)
	const otherRule = "team:x-other"
	store := NewBudgetStore([]BudgetRule{
		{Name: "per-team", ScopeKey: teamRuleKey2337, MaxTokens: 100, Window: time.Hour},
		{Name: "per-other", ScopeKey: otherRule, MaxTokens: 100, Window: time.Hour},
	}, clock)
	var overflows []string
	store.onTeamOverflow = func(budget string) { overflows = append(overflows, budget) }

	for i := 0; i < maxTeamKeys; i++ {
		store.Charge([]string{teamKey2337(fmt.Sprintf("t%05d", i))}, 1, 0)
	}
	advance(t0.Add(time.Minute))
	overflowKey := teamKey2337(teamOverflowValue)

	// A new value is admitted against the empty overflow key and charged to it.
	if ok, _, name := store.Allowed([]string{teamKey2337("newcomer")}); !ok {
		t.Fatalf("first new value at the cap rejected by %q, want admitted (overflow key is empty)", name)
	}
	store.Charge([]string{teamKey2337("newcomer")}, 100, 0)
	if _, ok := store.buckets[teamKey2337("newcomer")]; ok {
		t.Fatal("a new team value created its own key past the cap")
	}
	if got := len(store.buckets); got != maxTeamKeys+1 {
		t.Fatalf("keys = %d, want %d (the cap plus the overflow key)", got, maxTeamKeys+1)
	}
	if tok, _, _ := store.usedInWindow(overflowKey, clock(), time.Hour); tok != 100 {
		t.Fatalf("overflow key used %d tokens, want 100", tok)
	}

	// The overflow key is limited: the next new value is rejected by the
	// team budget, not let through unlimited.
	ok, retry, name := store.Allowed([]string{teamKey2337("second-newcomer")})
	if ok || name != "per-team" || retry <= 0 {
		t.Fatalf("second new value = (%v, %v, %q), want rejected by per-team with a Retry-After", ok, retry, name)
	}
	if want := []string{"per-team", "per-team"}; !reflect.DeepEqual(overflows, want) {
		t.Fatalf("overflow hook calls = %v, want %v", overflows, want)
	}

	// An existing value keeps its own key and its own cap.
	if ok, _, name := store.Allowed([]string{teamKey2337("t00000")}); !ok {
		t.Fatalf("existing value rejected by %q, want admitted", name)
	}
	store.Charge([]string{teamKey2337("t00000")}, 98, 0)
	if tok, _, _ := store.usedInWindow(teamKey2337("t00000"), clock(), time.Hour); tok != 99 {
		t.Fatalf("existing value used %d tokens, want 99 on its own key", tok)
	}
	if ok, _, _ := store.Allowed([]string{teamKey2337("t00000")}); !ok {
		t.Fatal("existing value at 99/100 rejected; it must not share the exhausted overflow key")
	}
	if len(overflows) != 2 {
		t.Fatalf("overflow hook fired for an existing value: %v", overflows)
	}

	// The cap counts keys across team budgets, and each budget overflows into
	// its own key.
	store.Charge([]string{otherRule + ":x"}, 7, 0)
	if _, ok := store.buckets[otherRule+":"+teamOverflowValue]; !ok {
		t.Fatal("a new value of a second team budget did not use that budget's overflow key")
	}

	// Once the window passes the slots free up and a new value gets its own key.
	advance(t0.Add(2 * time.Hour))
	store.Charge([]string{teamKey2337("after")}, 1, 0)
	if _, ok := store.buckets[teamKey2337("after")]; !ok {
		t.Fatal("a new value after the window did not get its own key")
	}
	if store.teamKeys != 1 {
		t.Fatalf("teamKeys = %d after the window, want 1", store.teamKeys)
	}
}

// TestBudgetTeamKeyCapFreesIdleSlotsFirst pins that reaching the cap sweeps
// before folding a value into overflow, so idle keys do not push a new team
// into the shared key when a charge has not swept recently.
func TestBudgetTeamKeyCapFreesIdleSlotsFirst(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clock, advance := nowFn(t0)
	store := NewBudgetStore([]BudgetRule{
		{Name: "per-team", ScopeKey: teamRuleKey2337, MaxTokens: 100, Window: time.Minute},
	}, clock)
	for i := 0; i < maxTeamKeys; i++ {
		store.Charge([]string{teamKey2337(fmt.Sprintf("t%05d", i))}, 1, 0)
	}
	advance(t0.Add(2 * time.Minute))
	overflowed := false
	store.onTeamOverflow = func(string) { overflowed = true }
	if ok, _, _ := store.Allowed([]string{teamKey2337("fresh")}); !ok || overflowed {
		t.Fatalf("Allowed(fresh) = %v, overflowed = %v; want admitted on its own key once idle slots are swept",
			ok, overflowed)
	}
}

// TestTeamScopeKeyLengthBound pins the key form: ordinary values are kept
// verbatim, a value over maxTeamValueLen or not valid UTF-8 is hashed, and
// the key length does not grow with the header.
func TestTeamScopeKeyLengthBound(t *testing.T) {
	hashedLen := len(teamKey2337("sha256:")) + 64
	long := strings.Repeat("a", 1<<20)
	tests := []struct {
		name  string
		value string
		want  string // empty: hashed
	}{
		{"short", "research", teamKey2337("research")},
		{"colon", "research:evil", teamKey2337("research:evil")},
		{"utf8", "équipe-données", teamKey2337("équipe-données")},
		{"at bound", strings.Repeat("b", maxTeamValueLen), teamKey2337(strings.Repeat("b", maxTeamValueLen))},
		{"one over", strings.Repeat("b", maxTeamValueLen+1), ""},
		{"one MiB", long, ""},
		{"invalid utf8", "team\xff", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := teamScopeKey("x-llmkube-team", tt.value)
			if tt.want != "" {
				if got != tt.want {
					t.Fatalf("key = %q, want %q unchanged", got, tt.want)
				}
				return
			}
			if len(got) != hashedLen || !strings.HasPrefix(got, teamKey2337("sha256:")) {
				t.Fatalf("key = %.80q (len %d), want a sha256 key of length %d", got, len(got), hashedLen)
			}
		})
	}
	if teamScopeKey("x-llmkube-team", long) == teamScopeKey("x-llmkube-team", long+"z") {
		t.Fatal("two distinct long values share a key")
	}
	if teamScopeKey("x-llmkube-team", long) != teamScopeKey("x-llmkube-team", strings.Clone(long)) {
		t.Fatal("the same long value maps to different keys")
	}
}

// dropBudgetGauge removes a test budget's token gauge series when the test
// ends. The gauge is process-global, and a test that publishes thousands of
// team series would otherwise leak them into later tests' registry scans.
func dropBudgetGauge(t *testing.T, budget string) {
	t.Helper()
	t.Cleanup(func() {
		prommetrics.RouterTokenBudgetUtilization.DeletePartialMatch(prometheus.Labels{"budget": budget})
	})
}

// budgetGaugeScopes returns the token budget gauge's values by scope label
// for router "default" and the given budget.
func budgetGaugeScopes(t *testing.T, budget string) map[string]float64 {
	t.Helper()
	ch := make(chan prometheus.Metric)
	go func() {
		prommetrics.RouterTokenBudgetUtilization.Collect(ch)
		close(ch)
	}()
	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("write metric: %v", err)
		}
		labels := map[string]string{}
		for _, l := range pb.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["router"] == "default" && labels["budget"] == budget {
			out[labels["scope"]] = pb.GetGauge().GetValue()
		}
	}
	return out
}

// TestProxyTeamBudgetLongHeaderValue sends a 64 KiB and a non-UTF-8 team
// header through a real listener: each is charged under a bounded hashed
// key, and the gauge publishes it (a raw non-UTF-8 label value makes the
// Prometheus client panic, which used to drop the request's audit line).
func TestProxyTeamBudgetLongHeaderValue(t *testing.T) {
	const budget = "team-cap-2337-long"
	dropBudgetGauge(t, budget)
	proxy, mux, _ := budgetTestProxy(t, []Budget{
		{Name: budget, Scope: BudgetScopeTeam, HeaderKey: DefaultTeamHeaderKey, Window: time.Hour, MaxTokens: 1000},
	})
	srv := newBudgetServer(t, mux)

	long := strings.Repeat("L", 64<<10)
	for _, v := range []string{long, "bad\xffvalue", "research"} {
		resp := srv.post(t, map[string]string{DefaultTeamHeaderKey: v})
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d for a %d-byte team value, want 200", resp.StatusCode, len(v))
		}
	}

	wantKeys := map[string]bool{
		teamScopeKey(DefaultTeamHeaderKey, long):           true,
		teamScopeKey(DefaultTeamHeaderKey, "bad\xffvalue"): true,
		"team:" + DefaultTeamHeaderKey + ":research":       true,
	}
	for _, u := range proxy.budgets.Snapshot() {
		if u.ScopeKey == "team:"+DefaultTeamHeaderKey {
			continue
		}
		if !wantKeys[u.ScopeKey] {
			t.Errorf("unexpected scope key %.80q (len %d)", u.ScopeKey, len(u.ScopeKey))
		}
		if len(u.ScopeKey) > len("team:"+DefaultTeamHeaderKey+":")+maxTeamValueLen {
			t.Errorf("scope key length %d exceeds the bound", len(u.ScopeKey))
		}
		delete(wantKeys, u.ScopeKey)
	}
	if len(wantKeys) != 0 {
		t.Fatalf("snapshot is missing keys %v", wantKeys)
	}

	// The gauge publishes on the request after the bad value, so it must
	// hold every key; "research" proves the short value kept its name.
	gauge := budgetGaugeScopes(t, budget)
	for _, k := range []string{
		teamScopeKey(DefaultTeamHeaderKey, "bad\xffvalue"),
		"team:" + DefaultTeamHeaderKey + ":research",
	} {
		if _, ok := gauge[k]; !ok {
			t.Errorf("gauge has no series for %q; have %v", k, gauge)
		}
	}
}

// TestProxyTeamBudgetOverflowAtRequestPath fills the proxy's store to the
// cap and sends new team values through the handler: the first is served and
// charged to the overflow key, the next is refused by the same budget, an
// existing value is unaffected, and the overflow counter moves.
func TestProxyTeamBudgetOverflowAtRequestPath(t *testing.T) {
	const budget = "team-cap-2337-overflow"
	dropBudgetGauge(t, budget)
	proxy, mux, back := budgetTestProxy(t, []Budget{
		{Name: budget, Scope: BudgetScopeTeam, HeaderKey: DefaultTeamHeaderKey, Window: time.Hour, MaxTokens: 100},
	})
	for i := 0; i < maxTeamKeys; i++ {
		proxy.budgets.Charge([]string{teamScopeKey(DefaultTeamHeaderKey, fmt.Sprintf("t%05d", i))}, 1, 0)
	}
	counter := prommetrics.RouterBudgetTeamOverflowTotal.WithLabelValues("default", budget)
	before := testutil.ToFloat64(counter)

	r1 := budgetPost(t, mux, map[string]string{DefaultTeamHeaderKey: "newcomer"})
	_ = r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first new value status = %d, want 200", r1.StatusCode)
	}
	r2 := budgetPost(t, mux, map[string]string{DefaultTeamHeaderKey: "another"})
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second new value status = %d, want 429 (the overflow key holds the first one's 100 tokens)", r2.StatusCode)
	}
	if got := testutil.ToFloat64(counter) - before; got != 2 {
		t.Fatalf("overflow counter moved by %v, want 2", got)
	}
	r3 := budgetPost(t, mux, map[string]string{DefaultTeamHeaderKey: "t00000"})
	_ = r3.Body.Close()
	if r3.StatusCode != http.StatusOK {
		t.Fatalf("existing value status = %d, want 200", r3.StatusCode)
	}
	if got := back.calls.Load(); got != 2 {
		t.Fatalf("backend calls = %d, want 2 (the refused overflow request must not dispatch)", got)
	}
	used := map[string]int64{}
	for _, u := range proxy.budgets.Snapshot() {
		used[u.ScopeKey] = u.UsedTokens
	}
	if used[teamScopeKey(DefaultTeamHeaderKey, teamOverflowValue)] != 100 {
		t.Errorf("overflow key used %d, want 100", used[teamScopeKey(DefaultTeamHeaderKey, teamOverflowValue)])
	}
	if _, ok := used[teamScopeKey(DefaultTeamHeaderKey, "newcomer")]; ok {
		t.Error("newcomer got its own key past the cap")
	}
	if used[teamScopeKey(DefaultTeamHeaderKey, "t00000")] != 101 {
		t.Errorf("existing value used %d, want 101 on its own key", used[teamScopeKey(DefaultTeamHeaderKey, "t00000")])
	}
}

// TestProxyBudgetGaugeDropsEvictedKeys pins that the token budget gauge
// forgets a team value once its key is evicted, instead of keeping its last
// utilization forever and one series per value ever seen.
func TestProxyBudgetGaugeDropsEvictedKeys(t *testing.T) {
	const budget = "team-cap-2337-gauge"
	dropBudgetGauge(t, budget)
	proxy, mux, _ := budgetTestProxy(t, []Budget{
		{Name: budget, Scope: BudgetScopeTeam, HeaderKey: DefaultTeamHeaderKey, Window: time.Minute, MaxTokens: 1000},
	})
	t0 := time.Now()
	clock, advance := nowFn(t0)
	var clockMu sync.Mutex
	proxy.budgets.nowFn = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock() }
	setNow := func(at time.Time) { clockMu.Lock(); defer clockMu.Unlock(); advance(at) }

	alpha := teamScopeKey(DefaultTeamHeaderKey, "alpha")
	beta := teamScopeKey(DefaultTeamHeaderKey, "beta")
	resp := budgetPost(t, mux, map[string]string{DefaultTeamHeaderKey: "alpha"})
	_ = resp.Body.Close()
	if got := budgetGaugeScopes(t, budget); got[alpha] != 0.1 {
		t.Fatalf("gauge after alpha = %v, want alpha at 0.1", got)
	}

	setNow(t0.Add(2 * time.Minute))
	resp = budgetPost(t, mux, map[string]string{DefaultTeamHeaderKey: "beta"})
	_ = resp.Body.Close()
	got := budgetGaugeScopes(t, budget)
	if _, ok := got[alpha]; ok {
		t.Fatalf("gauge still has alpha (%v) after its window passed: %v", got[alpha], got)
	}
	if got[beta] != 0.1 {
		t.Fatalf("gauge = %v, want beta at 0.1", got)
	}
}

// TestBudgetStoreConcurrentTeamChurn runs charges with fresh team values,
// admissions, snapshots and a moving clock together, for the race detector,
// then checks the cap counter still matches the stored keys.
func TestBudgetStoreConcurrentTeamChurn(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := t0
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	store := NewBudgetStore([]BudgetRule{
		{Name: "router-cap", ScopeKey: "router", MaxTokens: 1 << 40, Window: 3 * time.Second},
		{Name: "per-team", ScopeKey: teamRuleKey2337, MaxTokens: 1000, Window: 2 * time.Second},
	}, clock)

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				keys := []string{"router", teamKey2337(fmt.Sprintf("w%d-%d", w, i%300))}
				store.Allowed(keys)
				store.Charge(keys, 1, 0)
				if i%100 == 0 {
					_ = store.Snapshot()
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			mu.Lock()
			now = now.Add(100 * time.Millisecond)
			mu.Unlock()
		}
	}()
	wg.Wait()

	store.mu.Lock()
	defer store.mu.Unlock()
	counted := 0
	for k := range store.buckets {
		if countsTowardTeamCap(k) {
			counted++
		}
		if len(store.buckets[k]) == 0 {
			t.Errorf("key %q stored with no buckets", k)
		}
	}
	if counted != store.teamKeys {
		t.Fatalf("teamKeys = %d, stored team keys = %d", store.teamKeys, counted)
	}
}

// budgetServer is a real listener in front of mux, so header bytes cross the
// net/http parser the way a client's do.
type budgetServer struct{ url string }

func newBudgetServer(t *testing.T, mux http.Handler) budgetServer {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return budgetServer{url: srv.URL}
}

func (s budgetServer) post(t *testing.T, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.url+"/v1/chat/completions", strings.NewReader(`{"model":"any"}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}
