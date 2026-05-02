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

// pickEvictionTarget returns the process that should be evicted first under
// memory pressure. Selection rules, in order of precedence:
//
//  1. Skip non-llama-server processes. oMLX and Ollama use shared daemons
//     where the agent does not own the actual process lifecycle, so evicting
//     them via this code path would have no effect on memory.
//  2. Lowest InferenceService.Spec.Priority wins (batch < low < normal < high
//     < critical). Reuses the existing Priority field; see priority.go for
//     the rationale on conflating GPU-scheduling priority with eviction
//     priority.
//  3. Tie-break by largest RSS so we free the most memory per eviction.
//  4. Final tie-break by oldest StartedAt so the choice is deterministic
//     across runs and the most recently started process gets the benefit of
//     the doubt.
//
// Returns nil when no eligible candidate exists (empty or all-non-llama).
// That is a hard guard against evicting our last running process and
// producing a cascading eviction loop with nothing to gain.
func pickEvictionTarget(processes map[string]*ManagedProcess, rss map[string]uint64) *ManagedProcess {
	candidates := make([]*ManagedProcess, 0, len(processes))
	for _, p := range processes {
		if p == nil {
			continue
		}
		// Rule 1: only llama-server processes are managed at the OS level.
		// oMLX and Ollama set ModelID to a non-empty value because the agent
		// uses model IDs (not PIDs) to unload them on the shared daemon.
		if p.ModelID != "" {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		pi, pj := candidates[i], candidates[j]
		// Rule 2: lower priority weight first.
		wi, wj := priorityWeight(pi.Priority), priorityWeight(pj.Priority)
		if wi != wj {
			return wi < wj
		}
		// Rule 3: larger RSS first (we want to free more memory).
		ri := rss[managedProcessRSSKey(pi)]
		rj := rss[managedProcessRSSKey(pj)]
		if ri != rj {
			return ri > rj
		}
		// Rule 4: oldest StartedAt first (deterministic tie-break).
		return pi.StartedAt.Before(pj.StartedAt)
	})

	return candidates[0]
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
