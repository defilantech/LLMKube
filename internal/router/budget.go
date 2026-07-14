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
	"fmt"
	"math"
	"sync"
	"time"
)

// BudgetRule defines a token or dollar cap over a rolling window.
type BudgetRule struct {
	// Name identifies this budget for metrics, status, and audit logs.
	Name string

	// ScopeKey is the scope string this budget applies to:
	// "router", "rule:<name>", or "team:<hdrval>".
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
type BudgetUsage struct {
	// Name is the budget identifier.
	Name string

	// ScopeKey is the scope this budget applies to.
	ScopeKey string

	// UsedTokens is the total tokens consumed in the current window.
	UsedTokens int64

	// UsedUSD is the total cost consumed in the current window.
	UsedUSD float64

	// MaxTokens is the token cap (0 if no token cap).
	MaxTokens int64

	// MaxUSD is the USD cap (0 if no USD cap).
	MaxUSD float64

	// Window is the rolling window duration.
	Window time.Duration
}

// BudgetStore is an in-memory rolling-window budget accounting engine.
// It is keyed by scope string ("router", "rule:<name>", "team:<hdrval>")
// and enforces ALL matching budgets on every request. The first exhausted
// budget is reported in Allowed().
//
// Usage is tracked via a ring/timestamp-bucket scheme: each bucket covers
// a fixed time slice (bucketDuration), and usage older than Window is
// evicted. This avoids unbounded memory growth while keeping the accounting
// precise enough for budget enforcement.
type BudgetStore struct {
	mu    sync.Mutex
	nowFn func() time.Time

	// rules maps scopeKey -> BudgetRule.
	rules map[string]BudgetRule

	// buckets maps scopeKey -> []bucket. Each bucket covers
	// bucketDuration and holds usage within that slice.
	buckets map[string][]bucket
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
		rule, ok := s.rules[sk]
		if !ok {
			continue
		}
		usedTokens, usedUSD, oldest := s.usedInWindow(sk, now, rule.Window)

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
	for _, sk := range scopeKeys {
		rule, ok := s.rules[sk]
		if !ok {
			continue
		}
		s.charge(sk, now, rule.Window, tokens, usd)
	}
}

// Snapshot returns per-budget used/utilization for status/metrics surfaces.
func (s *BudgetStore) Snapshot() []BudgetUsage {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn()
	result := make([]BudgetUsage, 0, len(s.rules))
	for sk, rule := range s.rules {
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
	bkts := s.buckets[sk]
	if len(bkts) == 0 {
		s.buckets[sk] = []bucket{{
			start:  now.Truncate(bucketDuration),
			tokens: tokens,
			usd:    usd,
		}}
		return
	}

	// Evict buckets outside the window.
	windowStart := now.Add(-window)
	evicted := 0
	for i := range bkts {
		if bkts[i].start.After(windowStart) {
			bkts[evicted] = bkts[i]
			evicted++
		}
	}
	bkts = bkts[:evicted]

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
