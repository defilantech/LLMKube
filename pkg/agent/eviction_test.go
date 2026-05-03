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

package agent

import (
	"testing"
	"time"
)

func TestPickEvictionTarget_LowestPriorityFirst(t *testing.T) {
	processes := map[string]*ManagedProcess{
		"default/svc-low": {
			Name:      "svc-low",
			Namespace: "default",
			ModelPath: "/models/low.gguf",
			Priority:  "low",
			StartedAt: time.Now(),
		},
		"default/svc-normal": {
			Name:      "svc-normal",
			Namespace: "default",
			ModelPath: "/models/normal.gguf",
			Priority:  "normal",
			StartedAt: time.Now(),
		},
	}
	target, reason := pickEvictionTarget(processes, nil)
	if target == nil {
		t.Fatalf("pickEvictionTarget returned nil (reason=%q); expected svc-low", reason)
	}
	if target.Name != "svc-low" {
		t.Errorf("evicted %q, want %q", target.Name, "svc-low")
	}
	if reason != "" {
		t.Errorf("reason should be empty when target returned, got %q", reason)
	}
}

func TestPickEvictionTarget_TieBreakByRSS(t *testing.T) {
	processes := map[string]*ManagedProcess{
		"default/small": {
			Name: "small", Namespace: "default", Priority: "normal",
			ModelPath: "/models/small.gguf", StartedAt: time.Now(),
		},
		"default/large": {
			Name: "large", Namespace: "default", Priority: "normal",
			ModelPath: "/models/large.gguf", StartedAt: time.Now(),
		},
	}
	rss := map[string]uint64{
		"small.gguf": 1 << 30,
		"large.gguf": 10 << 30,
	}
	target, _ := pickEvictionTarget(processes, rss)
	if target == nil || target.Name != "large" {
		t.Errorf("expected eviction of larger-RSS process %q, got %v", "large", target)
	}
}

func TestPickEvictionTarget_TieBreakByStartedAt(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	processes := map[string]*ManagedProcess{
		"default/older": {
			Name: "older", Namespace: "default", Priority: "normal",
			ModelPath: "/models/m.gguf", StartedAt: older,
		},
		"default/newer": {
			Name: "newer", Namespace: "default", Priority: "normal",
			ModelPath: "/models/m.gguf", StartedAt: newer,
		},
	}
	// Same priority, same RSS lookup key (both point at same basename → same RSS),
	// so the tie-break should pick the older one.
	rss := map[string]uint64{"m.gguf": 5 << 30}
	target, _ := pickEvictionTarget(processes, rss)
	if target == nil || target.Name != "older" {
		t.Errorf("expected eviction of older process, got %v", target)
	}
}

func TestPickEvictionTarget_NilAndEmptyMaps(t *testing.T) {
	if got, reason := pickEvictionTarget(nil, nil); got != nil || reason != SkipReasonEmpty {
		t.Errorf("nil map should return (nil, %q), got (%v, %q)", SkipReasonEmpty, got, reason)
	}
	if got, reason := pickEvictionTarget(map[string]*ManagedProcess{}, nil); got != nil || reason != SkipReasonEmpty {
		t.Errorf("empty map should return (nil, %q), got (%v, %q)", SkipReasonEmpty, got, reason)
	}
}

// TestPickEvictionTarget_FloorReturnsNilForLastProcess locks down the PR-2B
// safety net for single-tenant setups. With exactly one managed process,
// the selector refuses to evict regardless of priority — killing the only
// workload is never an improvement over leaving it alone.
func TestPickEvictionTarget_FloorReturnsNilForLastProcess(t *testing.T) {
	processes := map[string]*ManagedProcess{
		"default/only": {
			Name: "only-svc", Namespace: "default", Priority: "batch",
			ModelPath: "/models/only.gguf", StartedAt: time.Now(),
		},
	}
	got, reason := pickEvictionTarget(processes, nil)
	if got != nil {
		t.Errorf("floor must refuse to evict the last managed process, got %v", got)
	}
	if reason != SkipReasonFloor {
		t.Errorf("reason = %q, want %q", reason, SkipReasonFloor)
	}
}

// TestPickEvictionTarget_FloorAllowsEvictionAtTwoUnprotected is the regression
// guard for the floor's exact semantics: counts the FULL processes map,
// not the candidate set. Two unprotected processes are both eligible, and
// the lower-priority one gets evicted as before.
func TestPickEvictionTarget_FloorAllowsEvictionAtTwoUnprotected(t *testing.T) {
	processes := map[string]*ManagedProcess{
		"default/svc-low": {
			Name: "svc-low", Namespace: "default", Priority: "low",
			ModelPath: "/models/low.gguf", StartedAt: time.Now(),
		},
		"default/svc-high": {
			Name: "svc-high", Namespace: "default", Priority: "high",
			ModelPath: "/models/high.gguf", StartedAt: time.Now(),
		},
	}
	target, reason := pickEvictionTarget(processes, nil)
	if target == nil {
		t.Fatalf("floor must permit eviction at len=2 (reason=%q)", reason)
	}
	if target.Name != "svc-low" {
		t.Errorf("evicted %q, want %q", target.Name, "svc-low")
	}
}

// TestPickEvictionTarget_SkipsProtectedFromCandidates locks down the
// per-workload opt-out: a single unprotected process alongside a protected
// one is the only eviction target, even if the protected one has lower
// priority. (Two managed processes total → floor permits eviction.)
func TestPickEvictionTarget_SkipsProtectedFromCandidates(t *testing.T) {
	processes := map[string]*ManagedProcess{
		"default/protected-low": {
			Name: "protected-low", Namespace: "default", Priority: "batch",
			ModelPath: "/models/p.gguf", StartedAt: time.Now(),
			EvictionProtection: true,
		},
		"default/unprotected-high": {
			Name: "unprotected-high", Namespace: "default", Priority: "high",
			ModelPath: "/models/u.gguf", StartedAt: time.Now(),
		},
	}
	target, _ := pickEvictionTarget(processes, nil)
	if target == nil || target.Name != "unprotected-high" {
		t.Errorf("expected eviction of the only unprotected process, got %v", target)
	}
}

// TestPickEvictionTarget_AllProtectedReturnsAllProtectedReason verifies
// that when every llama-server-eligible process is opted out, the selector
// returns nil with a reason operators can act on (review their
// evictionProtection settings). Floor passes (len=2), so this is purely
// the protected-set check.
func TestPickEvictionTarget_AllProtectedReturnsAllProtectedReason(t *testing.T) {
	processes := map[string]*ManagedProcess{
		"default/p1": {
			Name: "p1", Namespace: "default", Priority: "low",
			ModelPath: "/models/a.gguf", StartedAt: time.Now(),
			EvictionProtection: true,
		},
		"default/p2": {
			Name: "p2", Namespace: "default", Priority: "low",
			ModelPath: "/models/b.gguf", StartedAt: time.Now(),
			EvictionProtection: true,
		},
	}
	got, reason := pickEvictionTarget(processes, nil)
	if got != nil {
		t.Errorf("all-protected must return nil, got %v", got)
	}
	if reason != SkipReasonAllProtected {
		t.Errorf("reason = %q, want %q", reason, SkipReasonAllProtected)
	}
}

// TestPickEvictionTarget_AllRuntimeIneligibleReturnsRuntimeReason is the
// pre-existing oMLX/Ollama path. Reason renamed from the implicit catch-all
// to runtime_ineligible so operators see "no llama-server workloads to
// evict here" rather than thinking they have a protection-config issue.
func TestPickEvictionTarget_AllRuntimeIneligibleReturnsRuntimeReason(t *testing.T) {
	processes := map[string]*ManagedProcess{
		"default/ollama-only": {
			Name: "ollama-model", Namespace: "default", Priority: "low",
			ModelPath: "", // no path; oMLX/Ollama keep the model on the daemon side
			ModelID:   "qwen3:8b",
			StartedAt: time.Now(),
		},
		"default/omlx-only": {
			Name: "omlx-model", Namespace: "default", Priority: "low",
			ModelID: "mlx-community/Qwen3-4B-4bit", StartedAt: time.Now(),
		},
	}
	got, reason := pickEvictionTarget(processes, nil)
	if got != nil {
		t.Errorf("non-llama-server processes must be skipped, got %v", got)
	}
	if reason != SkipReasonRuntimeIneligible {
		t.Errorf("reason = %q, want %q", reason, SkipReasonRuntimeIneligible)
	}
}

func TestPickEvictionTarget_UnknownPriorityTreatedAsNormal(t *testing.T) {
	processes := map[string]*ManagedProcess{
		"default/normal": {
			Name: "normal-svc", Namespace: "default", Priority: "normal",
			ModelPath: "/models/n.gguf", StartedAt: time.Now(),
		},
		"default/garbage": {
			Name: "garbage-svc", Namespace: "default", Priority: "garbage-value",
			ModelPath: "/models/g.gguf", StartedAt: time.Now().Add(time.Hour),
		},
	}
	// A garbage value is treated as "normal", same priority as "normal",
	// so the tie-break (larger RSS, then older) picks "normal" because it
	// started earlier.
	target, _ := pickEvictionTarget(processes, nil)
	if target == nil {
		t.Fatal("expected one of the two candidates")
	}
	if target.Name != "normal-svc" {
		t.Errorf("expected older-of-equal candidates, got %q", target.Name)
	}
}

func TestShouldEvict_DisabledByConfig(t *testing.T) {
	stats := MemoryStats{TotalMemory: 100 << 30, TotalRSS: 80 << 30}
	if shouldEvict(MemoryPressureCritical, stats, false) {
		t.Error("eviction must be disabled when EvictionEnabled is false")
	}
}

func TestShouldEvict_AtWarningOnly(t *testing.T) {
	stats := MemoryStats{TotalMemory: 100 << 30, TotalRSS: 80 << 30}
	if shouldEvict(MemoryPressureWarning, stats, true) {
		t.Error("eviction must not fire at Warning level (only Critical)")
	}
	if shouldEvict(MemoryPressureNormal, stats, true) {
		t.Error("eviction must not fire at Normal level")
	}
}

func TestShouldEvict_BelowFiftyPercentGuard(t *testing.T) {
	// LLMKube using only 30% of system memory; pressure is from somewhere else.
	stats := MemoryStats{TotalMemory: 100 << 30, TotalRSS: 30 << 30}
	if shouldEvict(MemoryPressureCritical, stats, true) {
		t.Error("eviction must not fire when LLMKube is below 50% of total RSS")
	}
}

func TestShouldEvict_AboveGuardAtCritical(t *testing.T) {
	stats := MemoryStats{TotalMemory: 100 << 30, TotalRSS: 80 << 30}
	if !shouldEvict(MemoryPressureCritical, stats, true) {
		t.Error("eviction must fire when EvictionEnabled, Critical, and >50% RSS")
	}
}

func TestShouldEvict_ZeroTotalMemoryDefensiveGuard(t *testing.T) {
	// A bad provider snapshot should not panic or trigger eviction.
	stats := MemoryStats{TotalMemory: 0, TotalRSS: 100 << 30}
	if shouldEvict(MemoryPressureCritical, stats, true) {
		t.Error("zero TotalMemory must defensively return false")
	}
}
