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
	"path"
	"sort"
)

// Skip reasons returned alongside a nil eviction target. Surfaced to the
// evictions_skipped_total counter so operators can debug "watchdog went
// Critical but nothing was evicted" without trawling logs.
//
// These are distinct rather than collapsed into one because they imply
// different operator actions: floor means "this is single-tenant by
// design," all_protected means "review your evictionProtection settings,"
// runtime_ineligible means "no llama-server workloads on this agent."
// no managed processes at all
const SkipReasonEmpty = "empty"

// safety floor: refused to evict the last managed process
const SkipReasonFloor = "floor"

// at least one llama-server process exists, but every one has
// EvictionProtection=true so none are eligible
const SkipReasonAllProtected = "all_protected"

// every managed process is on a shared daemon (oMLX/Ollama) the agent
// does not own at the OS level, so killing them via this code path would
// have no effect on memory
const SkipReasonRuntimeIneligible = "runtime_ineligible"

// pickEvictionTarget returns the process that should be evicted first under
// memory pressure, or (nil, reason) when nothing is eligible. Selection
// rules, in order of precedence:
//
//  1. Safety floor: if there is at most one managed process total, return
//     (nil, "floor"). Killing the last workload helps no one and would
//     surprise a single-tenant operator who installed LLMKube to run one
//     big model. The floor counts ALL managed processes (including
//     protected and non-llama-server entries) on purpose so the rule is
//     simple and predictable.
//  2. Skip non-llama-server processes. oMLX and Ollama use shared daemons
//     where the agent does not own the actual process lifecycle, so evicting
//     them via this code path would have no effect on memory.
//  3. Skip processes with EvictionProtection=true so users can opt
//     individual workloads out of eviction.
//  4. Lowest InferenceService.Spec.Priority wins (batch < low < normal <
//     high < critical). Reuses the existing Priority field; see priority.go
//     for the rationale on conflating GPU-scheduling priority with eviction
//     priority.
//  5. Tie-break by largest RSS so we free the most memory per eviction.
//  6. Final tie-break by oldest StartedAt so the choice is deterministic
//     across runs and the most recently started process gets the benefit of
//     the doubt.
//
// Returns (nil, "all_protected") when every managed process is either
// non-llama-server or has EvictionProtection=true. Returns (nil, "empty")
// when the input map is empty or nil.
func pickEvictionTarget(processes map[string]*ManagedProcess, rss map[string]uint64) (*ManagedProcess, string) {
	if len(processes) == 0 {
		return nil, SkipReasonEmpty
	}
	// Rule 1: safety floor. Counted on the FULL processes map (not on the
	// filtered candidate set) so the rule is simple to reason about: "only
	// one workload? leave it alone."
	if len(processes) <= 1 {
		return nil, SkipReasonFloor
	}

	// Two-pass filter so we can distinguish "no llama-server workloads"
	// from "all workloads opted out": the first surfaces a runtime/config
	// problem, the second surfaces a settings problem the operator can fix.
	llamaServer := make([]*ManagedProcess, 0, len(processes))
	for _, p := range processes {
		if p == nil {
			continue
		}
		// Rule 2: only llama-server processes are managed at the OS level.
		// oMLX and Ollama set ModelID to a non-empty value because the agent
		// uses model IDs (not PIDs) to unload them on the shared daemon.
		if p.ModelID == "" {
			llamaServer = append(llamaServer, p)
		}
	}
	if len(llamaServer) == 0 {
		return nil, SkipReasonRuntimeIneligible
	}

	candidates := make([]*ManagedProcess, 0, len(llamaServer))
	for _, p := range llamaServer {
		// Rule 3: respect per-workload opt-out.
		if p.EvictionProtection {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return nil, SkipReasonAllProtected
	}

	sort.Slice(candidates, func(i, j int) bool {
		pi, pj := candidates[i], candidates[j]
		// Rule 4: lower priority weight first.
		wi, wj := priorityWeight(pi.Priority), priorityWeight(pj.Priority)
		if wi != wj {
			return wi < wj
		}
		// Rule 5: larger RSS first (we want to free more memory).
		ri := rss[managedProcessRSSKey(pi)]
		rj := rss[managedProcessRSSKey(pj)]
		if ri != rj {
			return ri > rj
		}
		// Rule 6: oldest StartedAt first (deterministic tie-break).
		return pi.StartedAt.Before(pj.StartedAt)
	})

	return candidates[0], ""
}

// managedProcessRSSKey returns the key used to look up a process's RSS in the
// MemoryStats.ProcessRSS map. The watchdog keys RSS by binary basename
// (filepath.Base of the executable path), so we use the basename of
// ManagedProcess.ModelPath as a proxy. This is a reasonable approximation
// for llama-server processes started by MetalExecutor since each spawns one
// llama-server per model. If the watchdog later switches to PID-keyed RSS,
// adjust here.
func managedProcessRSSKey(p *ManagedProcess) string {
	if p == nil || p.ModelPath == "" {
		return ""
	}
	return path.Base(p.ModelPath)
}

// shouldEvict returns true when the watchdog's pressure level warrants
// eviction AND the metal agent is responsible for >50% of the system's
// total RSS (the "don't friendly-fire when pressure comes from non-LLMKube
// workloads" guard).
//
// Without this guard, a build job, an IDE, or a browser process spiking
// system memory would cause us to evict production llama-server processes
// that are not actually contributing to the problem.
func shouldEvict(level MemoryPressureLevel, stats MemoryStats, evictionEnabled bool) bool {
	if !evictionEnabled {
		return false
	}
	if level != MemoryPressureCritical {
		return false
	}
	if stats.TotalMemory == 0 {
		// Defensive: a zero divisor means the watchdog gave us a malformed
		// stats snapshot; refuse to evict rather than panic.
		return false
	}
	rssShare := float64(stats.TotalRSS) / float64(stats.TotalMemory)
	return rssShare > 0.5
}
