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
	target := pickEvictionTarget(processes, nil)
	if target == nil {
		t.Fatal("pickEvictionTarget returned nil; expected svc-low")
	}
	if target.Name != "svc-low" {
		t.Errorf("evicted %q, want %q", target.Name, "svc-low")
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
	target := pickEvictionTarget(processes, rss)
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
	target := pickEvictionTarget(processes, rss)
	if target == nil || target.Name != "older" {
		t.Errorf("expected eviction of older process, got %v", target)
	}
}

func TestPickEvictionTarget_NoEligibleCandidates(t *testing.T) {
	if got := pickEvictionTarget(nil, nil); got != nil {
		t.Errorf("nil map should return nil, got %v", got)
	}
	if got := pickEvictionTarget(map[string]*ManagedProcess{}, nil); got != nil {
		t.Errorf("empty map should return nil, got %v", got)
	}
}

func TestPickEvictionTarget_SkipsNonLlamaServerProcesses(t *testing.T) {
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
	if got := pickEvictionTarget(processes, nil); got != nil {
		t.Errorf("non-llama-server processes (ModelID set) must be skipped, got %v", got)
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
	target := pickEvictionTarget(processes, nil)
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
