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

package picker

import (
	"sync"
	"testing"
	"time"
)

// pod builds a candidate with a fresh sample at t0.
func pod(ep string, running, waiting, kv float64) Candidate {
	return Candidate{Endpoint: ep, Sample: &Sample{Running: running, Waiting: waiting, KVUsage: kv, At: t0}}
}

// bare builds a candidate with no sample and the given in-flight count.
func bare(ep string, inflight int64) Candidate {
	return Candidate{Endpoint: ep, InFlight: inflight}
}

func place(t *testing.T, s *Sessions, key uint64, cands []Candidate, now time.Time, want string, wantRes Result, wantReason Reason) {
	t.Helper()
	ep, res, reason, _ := s.Place(key, cands, now, 0, "", 0)
	if ep != want || res != wantRes || reason != wantReason {
		t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)", ep, res, reason, want, wantRes, wantReason)
	}
}

func TestSessionsNewKeyGoesLeastLoadedAndBinds(t *testing.T) {
	s := NewSessions(0, 10)
	cands := []Candidate{pod("a", 10, 0, 0.1), pod("b", 2, 0, 0.1), pod("c", 7, 0, 0.1)}
	place(t, s, 1, cands, t0, "b", LeastLoaded, ReasonNone)
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
	place(t, s, 1, cands, t0, "b", Sticky, ReasonNone)
}

func TestSessionsStaysWhileNothingQueues(t *testing.T) {
	s := NewSessions(0, 10)
	cands := []Candidate{pod("a", 0, 0, 0.1), pod("b", 1, 0, 0.1)}
	place(t, s, 1, cands, t0, "a", LeastLoaded, ReasonNone)
	cands = []Candidate{pod("a", 30, 0, 0.1), pod("b", 1, 0, 0.1)}
	place(t, s, 1, cands, t0, "a", Sticky, ReasonNone)
}

// bindToA binds key 1 to pod "a" by making it the only candidate.
func bindToA(t *testing.T, s *Sessions, now time.Time) {
	t.Helper()
	place(t, s, 1, []Candidate{pod("a", 0, 0, 0.1)}, now, "a", LeastLoaded, ReasonNone)
}

func TestSessionsSpillsOnQueueAndMargin(t *testing.T) {
	cases := []struct {
		leastRunning float64
		want         string
		res          Result
		reason       Reason
	}{
		{15, "b", Spill, ReasonWaiting},
		{18, "b", Spill, ReasonWaiting},
		{19, "a", Sticky, ReasonNone},
	}
	for _, c := range cases {
		s := NewSessions(0, 10)
		bindToA(t, s, t0)
		cands := []Candidate{pod("a", 20, 2, 0.1), pod("b", c.leastRunning, 0, 0.1), pod("z", 50, 0, 0.1)}
		place(t, s, 1, cands, t0, c.want, c.res, c.reason)
	}
}

func TestSessionsDoesNotReturnHomeAfterSpill(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 2, 0.1), pod("b", 15, 0, 0.1), pod("z", 50, 0, 0.1)}, t0, "b", Spill, ReasonWaiting)
	place(t, s, 1, []Candidate{pod("a", 0, 0, 0.1), pod("b", 15, 0, 0.1), pod("z", 50, 0, 0.1)}, t0.Add(time.Second), "b", Sticky, ReasonNone)
}

func TestSessionsCooldown(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 2, 0.1), pod("b", 0, 0, 0.1), pod("z", 50, 0, 0.1)}, t0, "b", Spill, ReasonWaiting)
	// b is now under pressure with a gap of 6.
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	mk := func(d time.Duration) []Candidate {
		now := at(d)
		a := pod("a", 0, 0, 0.1)
		b := pod("b", 3, 3, 0.1)
		z := pod("z", 50, 0, 0.1)
		a.Sample.At, b.Sample.At, z.Sample.At = now, now, now
		return []Candidate{a, b, z}
	}
	place(t, s, 1, mk(30*time.Second), at(30*time.Second), "b", Sticky, ReasonNone)
	place(t, s, 1, mk(61*time.Second), at(61*time.Second), "a", Spill, ReasonWaiting)
}

func TestSessionsMigratesWhenPodLeaves(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 2, 0.1), pod("b", 0, 0, 0.1), pod("z", 50, 0, 0.1)}, t0, "b", Spill, ReasonWaiting)
	// b leaves inside the cooldown.
	now := t0.Add(5 * time.Second)
	c := pod("c", 1, 0, 0.1)
	c.Sample.At = now
	place(t, s, 1, []Candidate{c}, now, "c", Migrated, ReasonNotReady)
	place(t, s, 1, []Candidate{c}, now, "c", Sticky, ReasonNone)
}

func TestSessionsKVFullAloneDoesNotMove(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 5, 0, 0.95), pod("b", 0, 0, 0.4)}, t0, "a", Sticky, ReasonNone)
}

func TestSessionsKVFullAndQueuedSpillsAtLevelLoad(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 4, 1, 0.95), pod("b", 5, 0, 0.4)}, t0, "b", Spill, ReasonKVFull)
}

func TestSessionsAllKVFullStays(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	cands := []Candidate{pod("a", 5, 2, 0.95), pod("b", 5, 2, 0.95)}
	place(t, s, 1, cands, t0, "a", Sticky, ReasonNone)
	place(t, s, 1, cands, t0.Add(time.Second), "a", Sticky, ReasonNone)
}

func TestSessionsSinglePodNeverSpills(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 5, 10, 0.5)}, t0, "a", Sticky, ReasonNone)
}

func TestSessionsNoSampleUsesInFlightMargin(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{bare("a", 12), bare("b", 5), bare("z", 50)}, t0, "a", Sticky, ReasonNone)
	place(t, s, 1, []Candidate{bare("a", 13), bare("b", 5), bare("z", 50)}, t0, "b", Spill, ReasonWaiting)
}

func TestSessionsExpiredKeyIsNew(t *testing.T) {
	s := NewSessions(0, 10)
	cands := []Candidate{pod("a", 5, 0, 0.1), pod("b", 1, 0, 0.1)}
	place(t, s, 1, cands, t0, "b", LeastLoaded, ReasonNone)
	later := t0.Add(31 * time.Minute)
	a, b := pod("a", 1, 0, 0.1), pod("b", 5, 0, 0.1)
	a.Sample.At, b.Sample.At = later, later
	place(t, s, 1, []Candidate{a, b}, later, "a", LeastLoaded, ReasonNone)
}

func TestSessionsSweep(t *testing.T) {
	s := NewSessions(10*time.Minute, 10)
	cands := []Candidate{pod("a", 0, 0, 0.1)}
	for i, d := range []time.Duration{0, 5 * time.Minute, 9 * time.Minute} {
		now := t0.Add(d)
		place(t, s, uint64(i+1), cands, now, "a", LeastLoaded, ReasonNone)
	}
	if got := s.Sweep(t0.Add(12 * time.Minute)); got != 2 {
		t.Fatalf("Sweep = %d, want 2", got)
	}
	if got := s.Sweep(t0.Add(20 * time.Minute)); got != 0 {
		t.Fatalf("Sweep = %d, want 0", got)
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
}

func TestSessionsFullTableFallsBackToRingHome(t *testing.T) {
	s := NewSessions(0, 2)
	cands := []Candidate{pod("a", 0, 0, 0.1), pod("b", 5, 0, 0.1)}
	place(t, s, 1, cands, t0, "a", LeastLoaded, ReasonNone)
	place(t, s, 2, cands, t0, "a", LeastLoaded, ReasonNone)
	ep, res, reason, _ := s.Place(3, cands, t0, 0, "b", 0)
	if ep != "b" || res != Fallback || reason != ReasonNone {
		t.Fatalf("got (%q, %q, %q), want (b, fallback, none)", ep, res, reason)
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	place(t, s, 1, cands, t0, "a", Sticky, ReasonNone)
	ep, res, _, _ = s.Place(4, cands, t0, 0, "gone", 0)
	if ep != "a" || res != Fallback {
		t.Fatalf("got (%q, %q), want (a, fallback)", ep, res)
	}
}

func TestSessionsFullOfExpiredStillBinds(t *testing.T) {
	s := NewSessions(10*time.Minute, 2)
	place(t, s, 1, []Candidate{pod("a", 0, 0, 0.1)}, t0, "a", LeastLoaded, ReasonNone)
	place(t, s, 2, []Candidate{pod("a", 0, 0, 0.1)}, t0, "a", LeastLoaded, ReasonNone)
	later := t0.Add(11 * time.Minute)
	c := pod("a", 0, 0, 0.1)
	c.Sample.At = later
	place(t, s, 3, []Candidate{c}, later, "a", LeastLoaded, ReasonNone)
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
}

func TestSessionsNoCandidates(t *testing.T) {
	s := NewSessions(0, 10)
	place(t, s, 1, nil, t0, "", "", ReasonNone)
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
}

func TestSessionsConcurrentFirstRequestsBindOnce(t *testing.T) {
	s := NewSessions(0, 10)
	cands := []Candidate{pod("a", 1, 0, 0.1), pod("b", 1, 0, 0.1), pod("c", 1, 0, 0.1)}
	const n = 64
	got := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _, _, _ = s.Place(7, cands, t0, i, "", 0)
		}(i)
	}
	wg.Wait()
	for i, ep := range got {
		if ep != got[0] {
			t.Fatalf("call %d got %q, want %q", i, ep, got[0])
		}
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
}

func TestSessionsKVFullQueuedShedsToStaleTargetByGap(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 10, 0.95), bare("b", 0)}, t0, "b", Spill, ReasonKVFull)
}

func TestSessionsKVFullQueuedStaysWhenStaleTargetGapSmall(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 10, 0.95), bare("b", 27)}, t0, "a", Sticky, ReasonNone)
}

func TestSessionsAllKVFullStaysEvenWithLargeGap(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 10, 0.95), pod("b", 2, 0, 0.95)}, t0, "a", Sticky, ReasonNone)
}

func TestSessionsQueuedNotKVFullNeverTargetsFullPod(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	cands := []Candidate{pod("a", 15, 5, 0.5), pod("b", 1, 0, 0.95), pod("c", 15, 0, 0.5)}
	place(t, s, 1, cands, t0, "c", Spill, ReasonWaiting)
}

func TestSessionsFullTableSkipsScanWhenNothingCanExpire(t *testing.T) {
	s := NewSessions(10*time.Minute, 3)
	cands := []Candidate{pod("a", 0, 0, 0.1), pod("b", 5, 0, 0.1)}
	for k := uint64(1); k <= 3; k++ {
		place(t, s, k, cands, t0, "a", LeastLoaded, ReasonNone)
	}
	before := s.scans
	now := t0.Add(5 * time.Minute)
	c := []Candidate{pod("a", 0, 0, 0.1), pod("b", 5, 0, 0.1)}
	c[0].Sample.At, c[1].Sample.At = now, now
	for k := uint64(100); k < 200; k++ {
		ep, res, _, _ := s.Place(k, c, now, 0, "", 0)
		if ep != "a" || res != Fallback {
			t.Fatalf("key %d: got (%q, %q), want (a, fallback)", k, ep, res)
		}
	}
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3", s.Len())
	}
	if s.scans != before {
		t.Fatalf("scans = %d, want %d (no scan when nothing can expire)", s.scans, before)
	}
}

func TestSessionsFullTableScansWhenSomeExpire(t *testing.T) {
	s := NewSessions(10*time.Minute, 3)
	one := []Candidate{pod("a", 0, 0, 0.1)}
	place(t, s, 1, one, t0, "a", LeastLoaded, ReasonNone)
	later := t0.Add(8 * time.Minute)
	for k := uint64(2); k <= 3; k++ {
		c := pod("a", 0, 0, 0.1)
		c.Sample.At = later
		place(t, s, k, []Candidate{c}, later, "a", LeastLoaded, ReasonNone)
	}
	now := t0.Add(11 * time.Minute)
	c := pod("a", 0, 0, 0.1)
	c.Sample.At = now
	// Key 1 is expired, keys 2 and 3 are live: the scan frees one slot.
	place(t, s, 4, []Candidate{c}, now, "a", LeastLoaded, ReasonNone)
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3", s.Len())
	}
	// The table is full of live keys again and the oldest bound moved up.
	ep, res, _, _ := s.Place(5, []Candidate{c}, now, 0, "", 0)
	if ep != "a" || res != Fallback {
		t.Fatalf("got (%q, %q), want (a, fallback)", ep, res)
	}
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3", s.Len())
	}
	// Keys 2 and 3 expire later; the recomputed bound lets the scan run.
	later2 := t0.Add(19 * time.Minute)
	c.Sample.At = later2
	place(t, s, 6, []Candidate{c}, later2, "a", LeastLoaded, ReasonNone)
}

func TestSessionsKVFullDoesNotMoveOntoBusierPod(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 14, 1, 0.95), pod("b", 17, 0, 0.5)}, t0, "a", Sticky, ReasonNone)
}

func TestSessionsKVFullSpillRespectsCooldown(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 4, 1, 0.95), pod("b", 5, 0, 0.4)}, t0, "b", Spill, ReasonKVFull)
	mk := func(d time.Duration) []Candidate {
		now := t0.Add(d)
		a, b := pod("a", 5, 0, 0.4), pod("b", 5, 2, 0.95)
		a.Sample.At, b.Sample.At = now, now
		return []Candidate{a, b}
	}
	place(t, s, 1, mk(30*time.Second), t0.Add(30*time.Second), "b", Sticky, ReasonNone)
	place(t, s, 1, mk(61*time.Second), t0.Add(61*time.Second), "a", Spill, ReasonKVFull)
}

func TestSessionsKVFullHerdSelfLimits(t *testing.T) {
	s := NewSessions(0, 100)
	const sessions = 48
	for k := uint64(1); k <= sessions; k++ {
		place(t, s, k, []Candidate{pod("p0", 15, 1, 0.95)}, t0, "p0", LeastLoaded, ReasonNone)
	}
	cands := make([]Candidate, 0, 8)
	cands = append(cands, pod("p0", 15, 1, 0.95))
	for i, running := range []float64{14, 15, 16, 17, 14, 15, 16} {
		cands = append(cands, pod("p"+string(rune('1'+i)), running, 0, 0.6))
	}
	moved := 0
	for k := uint64(1); k <= sessions; k++ {
		ep, res, _, _ := s.Place(k, cands, t0, 0, "", 0)
		if res != Spill {
			continue
		}
		moved++
		for i := range cands {
			if cands[i].Endpoint == ep {
				cands[i].InFlight++
				if got, limit := Load(cands[i], t0), Load(cands[0], t0)+1; got > limit {
					t.Fatalf("session %d: target %s load %d exceeds pod 0 load+1 = %d", k, ep, got, limit)
				}
			}
		}
	}
	if moved == 0 || moved == sessions {
		t.Fatalf("moved = %d of %d, want some but not all", moved, sessions)
	}
}

// On a pool under MinPodsForLoadMoves, every move lands on a cold prefix
// cache, so a queue and a load gap alone keep the session where it is.
func TestSessionsSmallPoolGapAloneStays(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 2, 0.1), pod("b", 0, 0, 0.1)}, t0, "a", Sticky, ReasonNone)
}

func TestSessionsSmallPoolNoSampleGapStays(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{bare("a", 30), bare("b", 0)}, t0, "a", Sticky, ReasonNone)
}

func TestSessionsSmallPoolKVFullStillSheds(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 10, 0.95), pod("b", 0, 0, 0.4)}, t0, "b", Spill, ReasonKVFull)
}

func TestSessionsSmallPoolKVFullShedsToStaleTargetByGap(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 10, 0.95), bare("b", 0)}, t0, "b", Spill, ReasonKVFull)
}

func TestSessionsSmallPoolMigratesWhenPodLeaves(t *testing.T) {
	s := NewSessions(0, 10)
	place(t, s, 1, []Candidate{pod("a", 0, 0, 0.1), pod("b", 3, 0, 0.1)}, t0, "a", LeastLoaded, ReasonNone)
	place(t, s, 1, []Candidate{pod("b", 3, 0, 0.1)}, t0.Add(time.Second), "b", Migrated, ReasonNotReady)
}

func TestSessionsThreePodsGapMoves(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	cands := []Candidate{pod("a", 20, 2, 0.1), pod("b", 0, 0, 0.1), pod("c", 30, 0, 0.1)}
	place(t, s, 1, cands, t0, "b", Spill, ReasonWaiting)
}

func placeTok(t *testing.T, s *Sessions, key uint64, cands []Candidate, now time.Time, body int64) (string, Result, int64) {
	t.Helper()
	ep, res, _, tok := s.Place(key, cands, now, 0, "", body)
	return ep, res, tok
}

func TestSessionsNewKeyAvoidsPodMidColdPrefill(t *testing.T) {
	s := NewSessions(0, 10)
	a := pod("a", 5, 0, 0.1)
	a.Prefill = 140000
	if ep, res, _ := placeTok(t, s, 1, []Candidate{a, pod("b", 5, 0, 0.1)}, t0, 1000); ep != "b" || res != LeastLoaded {
		t.Fatalf("got (%q, %q), want (b, least_loaded)", ep, res)
	}
}

func TestSessionsSpillTargetAvoidsPodMidColdPrefill(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	b := pod("b", 2, 0, 0.1)
	b.Prefill = 140000
	ep, res, reason, _ := s.Place(1, []Candidate{pod("a", 20, 2, 0.1), b, pod("c", 3, 0, 0.1)}, t0, 0, "", 0)
	if ep != "c" || res != Spill || reason != ReasonWaiting {
		t.Fatalf("got (%q, %q, %q), want (c, spill, waiting)", ep, res, reason)
	}
}

func TestSessionsMigrationTargetAvoidsPodMidColdPrefill(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	b := pod("b", 2, 0, 0.1)
	b.Prefill = 140000
	ep, res, _, _ := s.Place(1, []Candidate{b, pod("c", 3, 0, 0.1)}, t0, 0, "", 0)
	if ep != "c" || res != Migrated {
		t.Fatalf("got (%q, %q), want (c, migrated)", ep, res)
	}
}

func TestSessionsEstimatesGrowthOnStickyTurns(t *testing.T) {
	s := NewSessions(0, 10)
	cands := []Candidate{pod("a", 0, 0, 0.1), pod("b", 9, 0, 0.1), pod("c", 9, 0, 0.1)}
	steps := []struct {
		body int64
		res  Result
		tok  int64
	}{
		{300000, LeastLoaded, 75000}, // first turn: whole prompt
		{308000, Sticky, 2000},       // growth only
		{120000, Sticky, 30000},      // compacted: whole prompt
		{121000, Sticky, 250},        // growth over the compacted length
	}
	for i, st := range steps {
		_, res, tok := placeTok(t, s, 1, cands, t0.Add(time.Duration(i)*time.Second), st.body)
		if res != st.res || tok != st.tok {
			t.Fatalf("step %d: got (%q, %d), want (%q, %d)", i, res, tok, st.res, st.tok)
		}
	}
}

func TestSessionsMovedSessionEstimatesWholePrompt(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	_, res, tok := placeTok(t, s, 1, []Candidate{pod("a", 20, 2, 0.1), pod("b", 0, 0, 0.1), pod("c", 30, 0, 0.1)}, t0, 400000)
	if res != Spill || tok != 100000 {
		t.Fatalf("spill: got (%q, %d), want (spill, 100000)", res, tok)
	}
	// The next turn on the new pod counts only its growth.
	_, res, tok = placeTok(t, s, 1, []Candidate{pod("a", 20, 2, 0.1), pod("b", 0, 0, 0.1), pod("c", 30, 0, 0.1)}, t0.Add(time.Second), 404000)
	if res != Sticky || tok != 1000 {
		t.Fatalf("after spill: got (%q, %d), want (sticky, 1000)", res, tok)
	}
}

func TestSessionsFallbackEstimatesWholePrompt(t *testing.T) {
	s := NewSessions(0, 1)
	placeTok(t, s, 1, []Candidate{pod("a", 0, 0, 0.1)}, t0, 4000)
	_, res, tok := placeTok(t, s, 2, []Candidate{pod("a", 0, 0, 0.1)}, t0, 8000)
	if res != Fallback || tok != 2000 {
		t.Fatalf("got (%q, %d), want (fallback, 2000)", res, tok)
	}
}

// Guards: tokens never make a session move.
func TestSessionsTokensAloneDoNotMoveASession(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	a := pod("a", 5, 0, 0.1)
	a.Prefill = 140000
	if ep, res, _ := placeTok(t, s, 1, []Candidate{a, pod("b", 0, 0, 0.1), pod("c", 0, 0, 0.1)}, t0, 0); ep != "a" || res != Sticky {
		t.Fatalf("no queue: got (%q, %q), want (a, sticky)", ep, res)
	}
	q := pod("a", 2, 1, 0.1) // load 3: one under PressureMargin
	q.Prefill = 262144
	if ep, res, _ := placeTok(t, s, 1, []Candidate{q, pod("b", 0, 0, 0.1), pod("c", 0, 0, 0.1)}, t0, 0); ep != "a" || res != Sticky {
		t.Fatalf("request gap 3: got (%q, %q), want (a, sticky)", ep, res)
	}
}

// Prefill tokens choose the spill target, and the gap and KV rules are then
// measured to that target. Here the token-light target has a fresh sample with
// KV room, so a KV-full pod sheds where, without tokens, the stale pod would
// have been the target and the session would have stayed.
func TestSessionsTokensPickTheTargetTheKVRuleMeasures(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	x := bare("x", 3)
	x.Prefill = 100000
	ep, res, reason, _ := s.Place(1, []Candidate{pod("a", 5, 1, 0.95), x, pod("y", 5, 0, 0.5)}, t0, 0, "", 0)
	if ep != "y" || res != Spill || reason != ReasonKVFull {
		t.Fatalf("got (%q, %q, %q), want (y, spill, kv_full)", ep, res, reason)
	}
	x.Prefill = 0
	s2 := NewSessions(0, 10)
	bindToA(t, s2, t0)
	ep, res, _, _ = s2.Place(1, []Candidate{pod("a", 5, 1, 0.95), x, pod("y", 5, 0, 0.5)}, t0, 0, "", 0)
	if ep != "a" || res != Sticky {
		t.Fatalf("without tokens: got (%q, %q), want (a, sticky)", ep, res)
	}
}
