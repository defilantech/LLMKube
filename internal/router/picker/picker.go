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

// Package picker decides which pod serves a request. It is pure: callers feed
// it candidates and the clock, and it returns an endpoint and the reason. The
// router proxy uses it today; an endpoint-picker front door can reuse it.
package picker

import (
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"time"
)

// Sample is one engine reading for one pod.
type Sample struct {
	Running, Waiting float64
	// KVUsage is the engine's KV-cache use, 0 to 1.
	KVUsage float64
	// At is when the sample was taken.
	At time.Time
	// InFlightAt is this replica's in-flight count for the pod when the
	// sample was taken, so requests sent since can be added on top.
	InFlightAt int64
}

// Candidate is one dispatchable pod.
type Candidate struct {
	Endpoint string
	// InFlight is this replica's current in-flight count for the pod.
	InFlight int64
	// Sample is the latest engine reading, or nil.
	Sample *Sample
	// Prefill is this replica's estimated uncached prompt tokens in flight to
	// the pod, counted until the pod sends its first response byte.
	Prefill int64
}

// Result names why a pod was chosen.
type Result string

const (
	LeastLoaded Result = "least_loaded"
	Sticky      Result = "sticky"
	Spill       Result = "spill"
	Fallback    Result = "fallback"
	// Migrated means the session's pod left the candidate set and the session
	// was rebound.
	Migrated Result = "migrated"
)

// Reason names what pressure moved a session off its pod.
type Reason string

const (
	ReasonNone     Reason = ""
	ReasonWaiting  Reason = "waiting"
	ReasonKVFull   Reason = "kv_full"
	ReasonNotReady Reason = "not_ready"
)

const (
	// StaleAfter is the age past which a sample is ignored.
	StaleAfter = 5 * time.Second
	// KVFullThreshold is the KV use at which a pod stops taking new work
	// while another pod is below it.
	KVFullThreshold = 0.9
	// VirtualNodes is the number of ring positions per endpoint.
	VirtualNodes = 128
	// BytesPerToken converts a request body length to estimated prompt
	// tokens.
	BytesPerToken = 4
	// TokensPerRequest is how many estimated prefill tokens weigh as one
	// request in Score.
	TokensPerRequest int64 = 16384
	// MaxRequestTokens caps one request's estimate.
	MaxRequestTokens int64 = 262144
)

// EstimateTokens returns the estimated prompt tokens of a body of bodyBytes,
// capped at MaxRequestTokens.
func EstimateTokens(bodyBytes int64) int64 {
	if bodyBytes <= 0 {
		return 0
	}
	return min(bodyBytes/BytesPerToken, MaxRequestTokens)
}

func freshSample(c Candidate, now time.Time) *Sample {
	if c.Sample == nil || now.Sub(c.Sample.At) > StaleAfter {
		return nil
	}
	return c.Sample
}

// Load estimates a pod's work: the engine's running plus waiting requests from
// a fresh sample, plus what this replica sent since that sample; without a
// fresh sample, this replica's in-flight count.
func Load(c Candidate, now time.Time) int64 {
	s := freshSample(c, now)
	if s == nil {
		return c.InFlight
	}
	sent := c.InFlight - s.InFlightAt
	if sent < 0 {
		sent = 0
	}
	return int64(math.Ceil(s.Running+s.Waiting)) + sent
}

// Eligible drops pods whose fresh KV reading is at or above KVFullThreshold,
// unless that would drop every pod.
func Eligible(cands []Candidate, now time.Time) []Candidate {
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if s := freshSample(c, now); s != nil && s.KVUsage >= KVFullThreshold {
			continue
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return cands
	}
	return out
}

// Score orders pods for placement, in estimated tokens: Load weighted by
// TokensPerRequest plus the prefill tokens in flight. Load stays the request
// measure the pressure gate uses.
func Score(c Candidate, now time.Time) int64 {
	return Load(c, now)*TokensPerRequest + c.Prefill
}

// PickLeastLoaded returns the candidate with the lowest Score; ties are broken
// by starting the scan at tieStart, so callers can rotate.
func PickLeastLoaded(cands []Candidate, now time.Time, tieStart int) (string, Result) {
	n := len(cands)
	if n == 0 {
		return "", ""
	}
	// Reduce the magnitude of tieStart with an unsigned modulo, so start is in
	// [0, n) for any tieStart, math.MinInt included (no signed negation).
	mag := uint(tieStart)
	if tieStart < 0 {
		mag = uint(-(tieStart + 1)) + 1
	}
	start := int(mag % uint(n))
	best, bestScore := start, Score(cands[start], now)
	for i := 1; i < n; i++ {
		j := (start + i) % n
		if sc := Score(cands[j], now); sc < bestScore {
			best, bestScore = j, sc
		}
	}
	return cands[best].Endpoint, LeastLoaded
}

// Ring is a consistent-hash ring over a fixed endpoint set.
type Ring struct {
	points []uint64
	owner  map[uint64]string
}

// hash64 hashes ring points and session keys. It is FNV-1a 64 followed by the
// splitmix64 finalizer. FNV-1a alone mixes its high bits poorly for strings
// that differ only in their last bytes (an endpoint's "#0".."#127" virtual node
// suffixes, or "session-N" keys), which clusters a pod's virtual nodes into a
// few contiguous ring arcs and skews how sessions are distributed. The
// finalizer avalanches those bits so the points spread across the ring.
func hash64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	x := h.Sum64()
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// SessionHash hashes a session key for the session table.
func SessionHash(key string) uint64 {
	return hash64(key)
}

// NewRing builds a ring with VirtualNodes positions per endpoint.
func NewRing(endpoints []string) *Ring {
	r := &Ring{owner: make(map[uint64]string, len(endpoints)*VirtualNodes)}
	for _, ep := range endpoints {
		for i := 0; i < VirtualNodes; i++ {
			p := hash64(ep + "#" + strconv.Itoa(i))
			if _, taken := r.owner[p]; taken {
				continue
			}
			r.owner[p] = ep
			r.points = append(r.points, p)
		}
	}
	sort.Slice(r.points, func(i, j int) bool { return r.points[i] < r.points[j] })
	return r
}

func (r *Ring) start(key string) int {
	h := hash64(key)
	i := sort.Search(len(r.points), func(i int) bool { return r.points[i] >= h })
	if i == len(r.points) {
		return 0
	}
	return i
}

// Home returns the endpoint a key maps to with no load bound.
func (r *Ring) Home(key string) string {
	if len(r.points) == 0 {
		return ""
	}
	return r.owner[r.points[r.start(key)]]
}
