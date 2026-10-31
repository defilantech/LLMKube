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
	"math/rand/v2"
	"slices"
)

// defaultBackendWeight is the share a backend takes in a weighted rule when
// its config carries no weight. Mirrors the CRD default and the Gateway data
// plane's backendWeights.
const defaultBackendWeight = 1

// effectiveWeight returns the backend's share in a weighted rule: its
// configured weight, or defaultBackendWeight when none is set. Zero is a real
// value (the backend is drained from weighted rules), which is why the wire
// field is a pointer.
func (b *Backend) effectiveWeight() int64 {
	if b.Weight == nil {
		return defaultBackendWeight
	}
	return int64(*b.Weight)
}

// randInt64N returns a uniform value in [0, n). It must be safe for
// concurrent use; the proxy calls it from every request goroutine.
type randInt64N func(n int64) int64

// weightedCandidate is one backend a weighted rule may dispatch to.
type weightedCandidate struct {
	name   string
	weight int64
}

// weightedCandidates returns the backends a weighted rule may dispatch to, in
// declared order: each named backend that exists, advertises every capability
// the rule requires, and has a positive weight. A name listed twice counts
// once, so a failover never retries the backend it just left.
//
// A zero weight removes the backend from the rule outright, even when every
// other candidate is down. That is the Gateway API meaning of weight 0, which
// the Gateway data plane inherits (Envoy Gateway drops zero-weight
// backendRefs), so the two data planes agree on the same spec.
func weightedCandidates(rule *Rule, backendByName func(string) *Backend) []weightedCandidate {
	out := make([]weightedCandidate, 0, len(rule.Route.Backends))
	seen := make(map[string]bool, len(rule.Route.Backends))
	for _, name := range rule.Route.Backends {
		if seen[name] {
			continue
		}
		seen[name] = true
		b := backendByName(name)
		if !rule.servableBy(b) {
			continue
		}
		if w := b.effectiveWeight(); w > 0 {
			out = append(out, weightedCandidate{name: name, weight: w})
		}
	}
	return out
}

// weightedOrder draws the order in which a weighted rule tries its backends:
// a weighted random sample without replacement. Each draw picks among the
// backends not yet drawn with probability proportional to weight.
//
// The dispatch loop walks this order and skips a backend that is quarantined,
// unhealthy, or (under IfIdle) behind a busy pool incumbent, so the first
// backend it serves from is drawn in proportion to weight among the backends
// eligible right now: a skipped draw only removes an ineligible backend and
// leaves the relative weights of the rest unchanged. A failed attempt moves on
// to the next draw, which is the weighted choice among the backends left.
func weightedOrder(cands []weightedCandidate, rnd randInt64N) []string {
	cands = slices.Clone(cands)
	var total int64
	for _, c := range cands {
		total += c.weight
	}
	order := make([]string, 0, len(cands))
	for len(cands) > 0 && total > 0 {
		r := rnd(total)
		i := 0
		for ; i < len(cands)-1; i++ {
			if r < cands[i].weight {
				break
			}
			r -= cands[i].weight
		}
		order = append(order, cands[i].name)
		total -= cands[i].weight
		cands = slices.Delete(cands, i, i+1)
	}
	return order
}

// defaultRandInt64N draws from math/rand/v2's global source, which is safe
// for concurrent use and seeded per process.
func defaultRandInt64N(n int64) int64 { return rand.Int64N(n) } //nolint:gosec // G404: traffic split, not security

// validateWeightedRule checks a weighted rule against the backends it names:
// at least one backend must be able to take its traffic, and two members of
// one ModelPool must not share it. backendsByName holds every configured
// backend; names the rule references were already checked to exist.
//
// Members of one pool share a single GPU slot, so splitting a rule's traffic
// across them would swap the slot on a large share of requests: each swap
// drains the incumbent and cold-loads the other member. The proxy refuses
// that config rather than thrash.
func validateWeightedRule(i int, r *Rule, backendsByName map[string]*Backend) error {
	lookup := func(name string) *Backend { return backendsByName[name] }
	if len(weightedCandidates(r, lookup)) == 0 {
		return fmt.Errorf("rules[%d] %s: strategy weighted needs at least one backend with a positive weight"+
			" that advertises every required capability", i, r.Name)
	}
	members := make(map[string]string)
	for _, name := range r.Route.Backends {
		b := backendsByName[name]
		if b == nil || b.Pool == nil {
			continue
		}
		key := b.Pool.Namespace + "/" + b.Pool.Name
		if prev, ok := members[key]; ok && prev != b.Pool.Member {
			return fmt.Errorf("rules[%d] %s: strategy weighted cannot split traffic across members %q and %q of ModelPool %s,"+
				" which share one GPU slot; use primary-fallback", i, r.Name, prev, b.Pool.Member, key)
		}
		members[key] = b.Pool.Member
	}
	return nil
}
