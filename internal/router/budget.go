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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// BudgetRule defines a token or dollar cap over a rolling window.
type BudgetRule struct {
	// Name identifies this budget for metrics, status, and audit logs.
	Name string

	// ScopeKey is the scope string this budget applies to:
	// "router", "rule:<name>", or (for a team cap) "team:<headerKey>".
	// A team rule governs every concrete "team:<headerKey>:<value>"
	// request key by prefix; see ruleFor.
	ScopeKey string

	// MaxTokens caps total tokens (prompt + completion) over the window.
	// Zero means no token cap.
	MaxTokens int64

	// MaxUSD caps total estimated cost in USD over the window.
	// Zero means no USD cap.
	MaxUSD float64

	// Window is the rolling window duration over which the cap is evaluated.
	Window time.Duration
}

// BudgetUsage reports per-budget used/utilization for status/metrics surfaces.
// The json tags are the wire contract the router-proxy admin endpoint serves
// and the operator's status poller reads.
type BudgetUsage struct {
	// Name is the budget identifier.
	Name string `json:"name"`

	// ScopeKey is the scope this budget applies to.
	ScopeKey string `json:"scopeKey"`

	// UsedTokens is the total tokens consumed in the current window.
	UsedTokens int64 `json:"usedTokens"`

	// UsedUSD is the total cost consumed in the current window.
	UsedUSD float64 `json:"usedUSD"`

	// MaxTokens is the token cap (0 if no token cap).
	MaxTokens int64 `json:"maxTokens"`

	// MaxUSD is the USD cap (0 if no USD cap).
	MaxUSD float64 `json:"maxUSD"`

	// Window is the rolling window duration.
	Window time.Duration `json:"windowNanos"`
}

// BudgetStore is an in-memory rolling-window budget accounting engine.
// It is keyed by scope string ("router", "rule:<name>", "team:<hdrval>")
// and enforces ALL matching budgets on every request. The first exhausted
// budget is reported in Allowed().
//
// Team scopes are resolved by prefix rather than exact match: see ruleFor.
//
// Usage is tracked via a ring/timestamp-bucket scheme: each bucket covers
// a fixed time slice (bucketDuration), and usage older than Window is
// evicted. A key left with no bucket inside its window is deleted by a
// periodic sweep (see maybeSweepLocked); an absent key and a key with no
// usage in the window mean the same thing to every reader. Team keys come
// from a client header, so the number of live team keys is capped (see
// maxTeamKeys and resolveLocked).
type BudgetStore struct {
	mu    sync.Mutex
	nowFn func() time.Time

	// rules maps scopeKey -> BudgetRule.
	rules map[string]BudgetRule

	// buckets maps scopeKey -> []bucket. Each bucket covers
	// bucketDuration and holds usage within that slice. A stored slice is
	// never empty.
	buckets map[string][]bucket

	// teamKeys counts the keys in buckets that count toward maxTeamKeys.
	teamKeys int

	// lastSweep is when the idle-key sweep last ran.
	lastSweep time.Time

	// onTeamOverflow, when set, is called (under mu) with the budget name
	// each time Allowed folds a new team value into the overflow key.
	onTeamOverflow func(budget string)
}

// bucket is a single time slice within a rolling window.
type bucket struct {
	start  time.Time
	tokens int64
	usd    float64
}

// bucketDuration is the granularity of each bucket. A 1-second bucket
// gives sub-second precision for budget enforcement while keeping the
// number of buckets per window manageable (e.g. 3600 buckets for a
// 1-hour window).
const bucketDuration = time.Second

// sweepInterval paces the sweep that deletes keys with no usage left in
// their window. Usage is kept in bucketDuration slices, so sweeping more
// often than one slice frees memory barely sooner; pacing it keeps the
// O(keys) walk off the per-request path.
const sweepInterval = bucketDuration

// maxTeamKeys caps the live concrete team keys ("team:<headerKey>:<value>")
// across all team budgets. The value comes from a request header the proxy
// does not authenticate, so without a cap each distinct value a caller sends
// inside one window is a new map entry. When the cap is reached, a request
// with a value that has no key yet is charged to, and limited by, its
// budget's shared overflow key (see teamOverflowValue) instead of creating
// one. It is not unlimited: the overflow key carries the same cap as any
// single team value.
const maxTeamKeys = 10000

// teamOverflowValue is the team value that every new value is folded into
// once maxTeamKeys is reached, giving "team:<headerKey>:__overflow__". A
// client that sends this value itself only lands in the same shared bucket,
// which the header already lets any caller choose; it gains nothing.
const teamOverflowValue = "__overflow__"

// maxTeamValueLen bounds the header value a team key embeds verbatim. Longer
// values, and values that are not valid UTF-8 (which a Prometheus label and
// the JSON status cannot carry), are replaced by "sha256:<hex>" of the
// value, so a key's size is bounded however large the header is, while
// ordinary team names keep their readable key.
const maxTeamValueLen = 128

// NewBudgetStore creates a BudgetStore from the given rules and clock.
// The clock function is injected so tests can advance time deterministically.
func NewBudgetStore(rules []BudgetRule, nowFn func() time.Time) *BudgetStore {
	s := &BudgetStore{
		nowFn:   nowFn,
		rules:   make(map[string]BudgetRule, len(rules)),
		buckets: make(map[string][]bucket),
	}
	for _, r := range rules {
		s.rules[r.ScopeKey] = r
	}
	return s
}

// ruleFor resolves the BudgetRule governing a concrete scope key. Router
// and rule scopes match a rule's ScopeKey exactly. Team scopes do not: the
// controller cannot know the header values in advance, so a team rule is
// keyed by its header name ("team:<headerKey>") and each request resolves
// to a concrete "team:<headerKey>:<value>" key that the rule governs by
// prefix. Usage buckets are keyed by the concrete key, so each team value
// gets its own cap.
func (s *BudgetStore) ruleFor(sk string) (BudgetRule, bool) {
	if r, ok := s.rules[sk]; ok {
		return r, true
	}
	if hk, _, ok := splitTeamKey(sk); ok {
		if r, ok := s.rules["team:"+hk]; ok {
			return r, true
		}
	}
	return BudgetRule{}, false
}

// splitTeamKey splits a concrete team scope key "team:<headerKey>:<value>"
// into its header name and value. It returns false for the two-segment
// "team:<x>" form, which only ever matches a rule exactly. A header name
// cannot contain a colon, so the first colon after the "team:" prefix
// delimits the name; the value is opaque and may contain colons.
func splitTeamKey(sk string) (headerKey, value string, ok bool) {
	rest, ok := strings.CutPrefix(sk, "team:")
	if !ok {
		return "", "", false
	}
	hk, value, ok := strings.Cut(rest, ":")
	if !ok || hk == "" || value == "" {
		return "", "", false
	}
	return hk, value, true
}

// teamScopeKey builds the concrete scope key for a team header value. A
// value longer than maxTeamValueLen or not valid UTF-8 is replaced by its
// SHA-256, so the key's length is bounded; any other value is kept verbatim.
func teamScopeKey(headerKey, value string) string {
	if len(value) > maxTeamValueLen || !utf8.ValidString(value) {
		sum := sha256.Sum256([]byte(value))
		value = "sha256:" + hex.EncodeToString(sum[:])
	}
	return "team:" + headerKey + ":" + value
}

// countsTowardTeamCap reports whether a key is a concrete team key that
// maxTeamKeys bounds. The per-budget overflow keys are excluded: there is at
// most one per team budget, and they must always be creatable.
func countsTowardTeamCap(sk string) bool {
	_, value, ok := splitTeamKey(sk)
	return ok && value != teamOverflowValue
}

// resolveLocked returns the key a request scope key's usage is kept under,
// and whether it was folded into its budget's overflow key. Every key keeps
// its own identity except a team key that has no usage yet while
// maxTeamKeys live team keys exist: it resolves to the overflow key, so a
// caller cycling header values cannot add keys, and is still held to a cap.
// A key that already exists is never redirected, so a team with usage keeps
// its own cap however many other values arrive.
func (s *BudgetStore) resolveLocked(sk string, now time.Time) (string, bool) {
	if !countsTowardTeamCap(sk) {
		return sk, false
	}
	if _, ok := s.buckets[sk]; ok {
		return sk, false
	}
	if s.teamKeys >= maxTeamKeys {
		// Idle keys may be holding the slots; free them before refusing.
		s.maybeSweepLocked(now)
	}
	if s.teamKeys < maxTeamKeys {
		return sk, false
	}
	hk, _, _ := splitTeamKey(sk)
	return "team:" + hk + ":" + teamOverflowValue, true
}

// maybeSweepLocked runs sweepLocked at most once per sweepInterval.
func (s *BudgetStore) maybeSweepLocked(now time.Time) {
	if !s.lastSweep.IsZero() && now.Sub(s.lastSweep) < sweepInterval {
		return
	}
	s.sweepLocked(now)
}

// sweepLocked drops every bucket outside its key's window and deletes the
// keys left with none. It removes exactly the usage usedInWindow already
// ignores, so no Allowed decision or Snapshot value changes.
func (s *BudgetStore) sweepLocked(now time.Time) {
	s.lastSweep = now
	for sk, bkts := range s.buckets {
		if rule, ok := s.ruleFor(sk); ok {
			bkts = pruneBuckets(bkts, now.Add(-rule.Window))
		} else {
			bkts = nil
		}
		if len(bkts) == 0 {
			delete(s.buckets, sk)
			if countsTowardTeamCap(sk) {
				s.teamKeys--
			}
			continue
		}
		s.buckets[sk] = bkts
	}
}

// pruneBuckets drops, in place, the buckets that start at or before
// windowStart: the same test usedInWindow applies.
func pruneBuckets(bkts []bucket, windowStart time.Time) []bucket {
	kept := 0
	for i := range bkts {
		if bkts[i].start.After(windowStart) {
			bkts[kept] = bkts[i]
			kept++
		}
	}
	return bkts[:kept]
}

// hasBucketInWindow reports whether any bucket of a key still counts toward
// the window starting after windowStart.
func hasBucketInWindow(bkts []bucket, windowStart time.Time) bool {
	for i := len(bkts) - 1; i >= 0; i-- {
		if bkts[i].start.After(windowStart) {
			return true
		}
	}
	return false
}

// Allowed reports whether all matching budgets have headroom. If any is
// exhausted it returns ok=false, the retry-after until the window frees
// the oldest usage, and the name of the first exhausted budget.
//
// Multi-scope precedence: ALL matching budgets are enforced; the first
// exhausted one is reported.
func (s *BudgetStore) Allowed(scopeKeys []string) (ok bool, retryAfter time.Duration, exhausted string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn()
	for _, sk := range scopeKeys {
		rule, ok := s.ruleFor(sk)
		if !ok {
			continue
		}
		key, overflowed := s.resolveLocked(sk, now)
		if overflowed && s.onTeamOverflow != nil {
			s.onTeamOverflow(rule.Name)
		}
		usedTokens, usedUSD, oldest := s.usedInWindow(key, now, rule.Window)

		// Check token cap.
		if rule.MaxTokens > 0 && usedTokens >= rule.MaxTokens {
			// retryAfter is how long until the oldest bucket expires.
			retryAfter = oldest.Add(rule.Window).Sub(now)
			if retryAfter < 0 {
				retryAfter = 0
			}
			return false, retryAfter, rule.Name
		}

		// Check USD cap.
		if rule.MaxUSD > 0 && usedUSD >= rule.MaxUSD {
			retryAfter = oldest.Add(rule.Window).Sub(now)
			if retryAfter < 0 {
				retryAfter = 0
			}
			return false, retryAfter, rule.Name
		}
	}
	return true, 0, ""
}

// Charge records usage against every matching budget.
func (s *BudgetStore) Charge(scopeKeys []string, tokens int64, usd float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn()
	s.maybeSweepLocked(now)
	for _, sk := range scopeKeys {
		rule, ok := s.ruleFor(sk)
		if !ok {
			continue
		}
		key, _ := s.resolveLocked(sk, now)
		s.charge(key, now, rule.Window, tokens, usd)
	}
}

// Snapshot returns per-budget used/utilization for status/metrics surfaces.
// Every configured rule appears (so a budget with no traffic reads as zero),
// and every concrete team value with usage inside its window appears under
// its own resolved key. A team value whose usage has all left the window is
// omitted, whether or not the sweep has deleted its key yet. Output is
// sorted by ScopeKey so callers see a stable order.
func (s *BudgetStore) Snapshot() []BudgetUsage {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn()
	s.maybeSweepLocked(now)
	keys := make(map[string]struct{}, len(s.rules)+len(s.buckets))
	for sk := range s.rules {
		keys[sk] = struct{}{}
	}
	for sk := range s.buckets {
		keys[sk] = struct{}{}
	}

	scopeKeys := make([]string, 0, len(keys))
	for sk := range keys {
		scopeKeys = append(scopeKeys, sk)
	}
	sort.Strings(scopeKeys)

	result := make([]BudgetUsage, 0, len(scopeKeys))
	for _, sk := range scopeKeys {
		rule, ok := s.ruleFor(sk)
		if !ok {
			continue
		}
		if _, configured := s.rules[sk]; !configured &&
			!hasBucketInWindow(s.buckets[sk], now.Add(-rule.Window)) {
			continue
		}
		usedTokens, usedUSD, _ := s.usedInWindow(sk, now, rule.Window)
		result = append(result, BudgetUsage{
			Name:       rule.Name,
			ScopeKey:   sk,
			UsedTokens: usedTokens,
			UsedUSD:    usedUSD,
			MaxTokens:  rule.MaxTokens,
			MaxUSD:     rule.MaxUSD,
			Window:     rule.Window,
		})
	}
	return result
}

// usedInWindow returns the total tokens and USD consumed within the
// rolling window ending at now, plus the timestamp of the oldest bucket
// that contributes to the window.
func (s *BudgetStore) usedInWindow(sk string, now time.Time, window time.Duration) (tokens int64, usd float64, oldest time.Time) {
	bkts := s.buckets[sk]
	if len(bkts) == 0 {
		return 0, 0, now
	}

	// Find the oldest bucket that falls within the window.
	windowStart := now.Add(-window)
	oldest = now // sentinel: no buckets in window
	for i := range bkts {
		if bkts[i].start.After(windowStart) {
			tokens += bkts[i].tokens
			usd += bkts[i].usd
			if oldest.IsZero() || bkts[i].start.Before(oldest) {
				oldest = bkts[i].start
			}
		}
	}
	return tokens, usd, oldest
}

// charge records usage against a single budget's buckets.
func (s *BudgetStore) charge(sk string, now time.Time, window time.Duration, tokens int64, usd float64) {
	bkts, exists := s.buckets[sk]
	if !exists {
		if countsTowardTeamCap(sk) {
			s.teamKeys++
		}
		s.buckets[sk] = []bucket{{
			start:  now.Truncate(bucketDuration),
			tokens: tokens,
			usd:    usd,
		}}
		return
	}

	// Evict buckets outside the window.
	bkts = pruneBuckets(bkts, now.Add(-window))

	// Find or create the bucket for now.
	bucketStart := now.Truncate(bucketDuration)
	if len(bkts) > 0 && bkts[len(bkts)-1].start.Equal(bucketStart) {
		// Append to the current bucket.
		bkts[len(bkts)-1].tokens += tokens
		bkts[len(bkts)-1].usd += usd
	} else {
		// Create a new bucket.
		bkts = append(bkts, bucket{
			start:  bucketStart,
			tokens: tokens,
			usd:    usd,
		})
	}
	s.buckets[sk] = bkts
}

// parseMaxUSD converts a MaxUSD string (e.g. "1.50") to a float64.
// Returns an error if the string is not a valid non-negative decimal.
func parseMaxUSD(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
		return 0, fmt.Errorf("parse MaxUSD %q: %w", s, err)
	}
	if f < 0 {
		return 0, fmt.Errorf("MaxUSD must be non-negative, got %s", s)
	}
	// Guard against NaN/Inf from malformed input.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("MaxUSD must be finite, got %s", s)
	}
	return f, nil
}
