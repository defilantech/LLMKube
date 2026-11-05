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
	"bytes"
	"testing"
	"time"
)

func blocks(parts ...string) []byte {
	var b bytes.Buffer
	for _, p := range parts {
		b.WriteString(p)
	}
	return b.Bytes()
}

func TestBlockHashesFullBlocksOnly(t *testing.T) {
	if got := BlockHashes([]byte("abcdefg"), 4); len(got) != 1 {
		t.Errorf("7 bytes in 4-byte blocks = %d hashes, want 1 (the partial block is not hashed)", len(got))
	}
	if got := BlockHashes([]byte("abc"), 4); len(got) != 0 {
		t.Errorf("3 bytes in 4-byte blocks = %d hashes, want 0", len(got))
	}
	if got := BlockHashes([]byte("abcdefgh"), 0); got != nil {
		t.Errorf("block size 0 = %v, want nil", got)
	}
}

// A block matches only after an identical prefix: the same bytes behind a
// different first block hash differently.
func TestBlockHashesChainOnlyMatchesAsPrefix(t *testing.T) {
	a := BlockHashes(blocks("AAAA", "BBBB", "CCCC"), 4)
	x := BlockHashes(blocks("XXXX", "BBBB", "CCCC"), 4)
	if a[0] == x[0] {
		t.Fatal("different first blocks hashed equal")
	}
	if a[1] == x[1] || a[2] == x[2] {
		t.Error("identical later blocks behind different prefixes hashed equal: the chain is not chained")
	}
	again := BlockHashes(blocks("AAAA", "BBBB", "CCCC", "DD"), 4)
	for i := range a {
		if a[i] != again[i] {
			t.Errorf("block %d of an extended stream changed hash", i)
		}
	}
}

func TestPrefixIndexMatchedIsTheLeadingRun(t *testing.T) {
	x := NewPrefixIndex(1024)
	turn1 := BlockHashes(blocks("sys1", "usr1", "ast1"), 4)
	x.Record("a", turn1)
	turn2 := BlockHashes(blocks("sys1", "usr1", "ast1", "usr2", "ast2"), 4)
	if got := x.Matched("a", turn2); got != 3 {
		t.Errorf("follow-up turn matched %d blocks on a, want 3", got)
	}
	if got := x.Matched("b", turn2); got != 0 {
		t.Errorf("matched %d blocks on an endpoint that never saw the prefix, want 0", got)
	}
	edited := BlockHashes(blocks("sys1", "EDIT", "ast1"), 4)
	if got := x.Matched("a", edited); got != 1 {
		t.Errorf("a prompt that diverges at block 1 matched %d, want 1", got)
	}
}

func TestPrefixIndexEvictsLeastRecentlyRecorded(t *testing.T) {
	x := NewPrefixIndex(4)
	old := BlockHashes(blocks("old1", "old2"), 4)
	x.Record("a", old)
	x.Record("a", BlockHashes(blocks("new1", "new2"), 4))
	if got := x.Matched("a", old); got != 2 {
		t.Fatalf("at capacity, nothing should be evicted yet; matched %d, want 2", got)
	}
	x.Record("a", BlockHashes(blocks("more", "more"), 4))
	if got := x.Matched("a", old); got != 0 {
		t.Errorf("the oldest blocks should be evicted past capacity; matched %d, want 0", got)
	}
	if got := x.Len("a"); got != 4 {
		t.Errorf("len = %d, want the capacity 4", got)
	}
}

func TestPrefixIndexRecordRefreshesRecency(t *testing.T) {
	x := NewPrefixIndex(4)
	shared := BlockHashes(blocks("sys1", "sys2"), 4)
	x.Record("a", shared)
	x.Record("a", BlockHashes(blocks("one1", "one2"), 4))
	x.Record("a", shared) // a reused prefix becomes the most recent again
	x.Record("a", BlockHashes(blocks("two1", "two2"), 4))
	if got := x.Matched("a", shared); got != 2 {
		t.Errorf("a re-recorded prefix was evicted: matched %d, want 2", got)
	}
}

func TestPrefixIndexRetainDropsDepartedEndpoints(t *testing.T) {
	x := NewPrefixIndex(16)
	chain := BlockHashes(blocks("sys1", "usr1"), 4)
	x.Record("a", chain)
	x.Record("b", chain)
	x.Retain([]string{"b"})
	if got := x.Matched("a", chain); got != 0 {
		t.Errorf("a left the endpoint set but still matched %d blocks", got)
	}
	if got := x.Matched("b", chain); got != 2 {
		t.Errorf("b stayed but matched %d, want 2", got)
	}
}

func TestPickLeastLoadedPrefersTheCachedPodAtEqualLoad(t *testing.T) {
	now := time.Unix(1000, 0)
	cands := []Candidate{
		{Endpoint: "a", Sample: &Sample{Running: 2, At: now}},
		{Endpoint: "b", Sample: &Sample{Running: 2, At: now}, Cached: 3000},
	}
	for tie := 0; tie < 2; tie++ {
		ep, res := PickLeastLoaded(cands, now, tie)
		if ep != "b" || res != PrefixMatch {
			t.Errorf("tie %d: pick = %s/%s, want b prefix_match", tie, ep, res)
		}
	}
}

func TestPickLeastLoadedBusyPodLosesDespiteItsCache(t *testing.T) {
	now := time.Unix(1000, 0)
	cands := []Candidate{
		{Endpoint: "a", Sample: &Sample{Running: 1, At: now}},
		// One more request queued outweighs a cached prefix shorter than
		// TokensPerRequest.
		{Endpoint: "b", Sample: &Sample{Running: 2, At: now}, Cached: TokensPerRequest / 2},
	}
	if ep, res := PickLeastLoaded(cands, now, 1); ep != "a" || res != LeastLoaded {
		t.Errorf("pick = %s/%s, want a least_loaded", ep, res)
	}
}

func TestScoreSubtractsCachedTokens(t *testing.T) {
	now := time.Unix(1000, 0)
	c := Candidate{Endpoint: "a", Sample: &Sample{Running: 1, At: now}, Prefill: 500}
	base := Score(c, now)
	c.Cached = 200
	if got := Score(c, now); got != base-200 {
		t.Errorf("Score with 200 cached tokens = %d, want %d", got, base-200)
	}
}
