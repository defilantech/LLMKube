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
	"time"
)

const (
	// DefaultSessionIdleTimeout is how long a session binding survives without
	// a request.
	DefaultSessionIdleTimeout = 30 * time.Minute
	// MaxSessions bounds the session table per router replica.
	MaxSessions = 100000
	// SpillCooldown is the least time between pressure moves of one session.
	SpillCooldown = 60 * time.Second
	// PressureMargin is how many more requests than the least-loaded other pod
	// a bound pod with a queue must carry before the session moves.
	PressureMargin int64 = 4
	// PressureMarginNoSample is the same margin when the bound pod has no fresh
	// sample, so the load is only this replica's in-flight count.
	PressureMarginNoSample int64 = 8
	// MinPodsForLoadMoves is the fewest dispatchable pods at which a queue and
	// a load gap alone move a session. Below it every move lands on a cold
	// prefix cache, so only a KV-full pod sheds.
	MinPodsForLoadMoves = 3
)

type binding struct {
	endpoint string
	lastSeen time.Time
	movedAt  time.Time
	// promptBytes is the body length of the session's last request, so the
	// next sticky turn can count only its growth as uncached.
	promptBytes int64
}

// Sessions binds session keys to pods. A binding changes only when its pod
// leaves the candidate set or the pod is under pressure and another pod is
// clearly better, so a session is never pulled back to a pod it left.
type Sessions struct {
	mu       sync.Mutex
	bindings map[uint64]binding
	idle     time.Duration
	capacity int
	// oldest is a lower bound on every binding's lastSeen. Refreshed and new
	// bindings only raise lastSeen, so it stays valid until a scan recomputes
	// it. The zero value makes the first scan run.
	oldest time.Time
	// scans counts full-table expiry scans, for tests.
	scans int
}

// NewSessions returns an empty table. An idle of zero or less means
// DefaultSessionIdleTimeout.
func NewSessions(idle time.Duration, capacity int) *Sessions {
	if idle <= 0 {
		idle = DefaultSessionIdleTimeout
	}
	return &Sessions{bindings: make(map[uint64]binding), idle: idle, capacity: capacity}
}

func (s *Sessions) expired(b binding, now time.Time) bool {
	return now.Sub(b.lastSeen) > s.idle
}

// dropExpiredLocked removes expired bindings and recomputes the oldest bound
// from the survivors. The caller holds s.mu.
func (s *Sessions) dropExpiredLocked(now time.Time) {
	s.scans++
	var oldest time.Time
	for k, b := range s.bindings {
		if s.expired(b, now) {
			delete(s.bindings, k)
			continue
		}
		if oldest.IsZero() || b.lastSeen.Before(oldest) {
			oldest = b.lastSeen
		}
	}
	s.oldest = oldest
}

// Place decides which pod serves one request of a session. cands are the
// dispatchable candidates (quarantined pods already removed, KV-full pods not
// removed). ringHome is the key's ring home, used only when the table is full.
// The whole decision runs under the table's lock, so racing first requests for
// one key bind once. The int64 is the request's estimated uncached prompt
// tokens: on a sticky turn the growth of the body over the session's previous
// one, otherwise (or when the body shrank) the whole body.
func (s *Sessions) Place(key uint64, cands []Candidate, now time.Time, tieStart int, ringHome string, bodyBytes int64) (string, Result, Reason, int64) {
	if len(cands) == 0 {
		return "", "", ReasonNone, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	b, known := s.bindings[key]
	if known && s.expired(b, now) {
		delete(s.bindings, key)
		known = false
	}

	if !known {
		// Nothing can be expired unless the oldest binding is past idle.
		if len(s.bindings) >= s.capacity && now.Sub(s.oldest) > s.idle {
			s.dropExpiredLocked(now)
		}
		if len(s.bindings) >= s.capacity {
			for _, c := range cands {
				if c.Endpoint == ringHome {
					return ringHome, Fallback, ReasonNone, EstimateTokens(bodyBytes)
				}
			}
			ep, _ := PickLeastLoaded(Eligible(cands, now), now, tieStart)
			return ep, Fallback, ReasonNone, EstimateTokens(bodyBytes)
		}
		ep, _ := PickLeastLoaded(Eligible(cands, now), now, tieStart)
		if len(s.bindings) == 0 {
			s.oldest = now
		}
		s.bindings[key] = binding{endpoint: ep, lastSeen: now, promptBytes: bodyBytes}
		return ep, LeastLoaded, ReasonNone, EstimateTokens(bodyBytes)
	}

	var bound *Candidate
	for i := range cands {
		if cands[i].Endpoint == b.endpoint {
			bound = &cands[i]
			break
		}
	}
	if bound == nil {
		ep, _ := PickLeastLoaded(Eligible(cands, now), now, tieStart)
		s.bindings[key] = binding{endpoint: ep, lastSeen: now, movedAt: now, promptBytes: bodyBytes}
		return ep, Migrated, ReasonNotReady, EstimateTokens(bodyBytes)
	}

	if least, reason := pressure(*bound, cands, now, tieStart); reason != ReasonNone &&
		(b.movedAt.IsZero() || now.Sub(b.movedAt) >= SpillCooldown) {
		s.bindings[key] = binding{endpoint: least, lastSeen: now, movedAt: now, promptBytes: bodyBytes}
		return least, Spill, reason, EstimateTokens(bodyBytes)
	}
	uncached := bodyBytes
	if bodyBytes >= b.promptBytes {
		uncached = bodyBytes - b.promptBytes
	}
	b.lastSeen = now
	b.promptBytes = bodyBytes
	s.bindings[key] = b
	return b.endpoint, Sticky, ReasonNone, EstimateTokens(uncached)
}

// pressure reports whether the bound pod should shed the session, and to which
// pod. It returns ReasonNone when the session should stay. A target never has a
// fresh KV reading at or above KVFullThreshold. On a pool of fewer than
// MinPodsForLoadMoves dispatchable pods only a KV-full bound pod sheds.
func pressure(bound Candidate, cands []Candidate, now time.Time, tieStart int) (string, Reason) {
	others := make([]Candidate, 0, len(cands))
	for _, c := range Eligible(cands, now) {
		if c.Endpoint == bound.Endpoint {
			continue
		}
		if cs := freshSample(c, now); cs != nil && cs.KVUsage >= KVFullThreshold {
			continue
		}
		others = append(others, c)
	}
	if len(others) == 0 {
		return "", ReasonNone
	}
	leastEP, _ := PickLeastLoaded(others, now, tieStart)
	var least Candidate
	for _, c := range others {
		if c.Endpoint == leastEP {
			least = c
			break
		}
	}
	gap := Load(bound, now) - Load(least, now)
	small := len(cands) < MinPodsForLoadMoves

	bs := freshSample(bound, now)
	if bs == nil {
		if !small && gap >= PressureMarginNoSample {
			return leastEP, ReasonWaiting
		}
		return "", ReasonNone
	}
	if bs.Waiting <= 0 {
		return "", ReasonNone
	}
	full := bs.KVUsage >= KVFullThreshold
	if small && !full {
		return "", ReasonNone
	}
	reason := ReasonWaiting
	if full {
		reason = ReasonKVFull
	}
	// A full pod sheds at any gap, but only to a pod known to have KV room
	// that is no busier than itself; otherwise it needs the load gap like any
	// queued pod.
	if ls := freshSample(least, now); full && ls != nil && gap >= 0 {
		return leastEP, reason
	}
	if gap >= PressureMargin {
		return leastEP, reason
	}
	return "", ReasonNone
}

// Sweep drops expired bindings and returns the remaining count.
func (s *Sessions) Sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExpiredLocked(now)
	return len(s.bindings)
}

// Len returns the number of bindings, expired ones included until swept.
func (s *Sessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bindings)
}
