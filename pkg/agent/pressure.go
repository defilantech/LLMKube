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
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// MemoryPressure status condition vocabulary.
//
// The condition lives on InferenceService.Status.Conditions and surfaces the
// agent's view of host memory pressure to operators. Reasons follow the
// "no-spaces UpperCamelCase" convention used elsewhere in our CRDs and the
// upstream metav1.Condition guidance.
const (
	ConditionMemoryPressure = "MemoryPressure"

	ReasonMemoryNormal   = "Normal"
	ReasonMemoryWarning  = "Warning"
	ReasonMemoryCritical = "Critical"
	ReasonEvicted        = "Evicted"
)

// handleMemoryPressure is the watchdog callback. It runs on the watchdog
// goroutine, so it must not block on long operations: K8s status writes are
// best-effort and failures are logged rather than retried.
//
// Behavior, in order:
//
//  1. Capture the new pressure level under the agent's mutex.
//  2. If pressure dropped to Normal, clear pressureBlocked so the next
//     ensureProcess can respawn previously evicted services.
//  3. Update the MemoryPressure condition on every managed
//     InferenceService that has not already been observed at this level
//     (covers level transitions and late-spawned processes during
//     sustained pressure).
//  4. If the level is Critical AND the friendly-fire guard says we're
//     responsible for the pressure (shouldEvict), pick one process to
//     evict, mark it blocked, and stop it. Only one eviction per watchdog
//     tick to avoid stampedes. Skip-without-evict outcomes are recorded
//     in the evictions_skipped_total counter so operators can debug why
//     no eviction happened.
func (a *MetalAgent) handleMemoryPressure(ctx context.Context, level MemoryPressureLevel, stats MemoryStats) {
	// Snapshot the candidate set under the lock. The values themselves are
	// pointers; we don't mutate them here, so a shallow copy is fine.
	a.mu.Lock()
	previous := a.lastPressureLevel
	a.lastPressureLevel = level
	if level == MemoryPressureNormal && len(a.pressureBlocked) > 0 {
		a.logger.Infow("memory pressure cleared; unblocking previously evicted services",
			"unblocked", len(a.pressureBlocked))
		a.pressureBlocked = make(map[string]bool)
	}
	// Reset observed-at-level when the level changes so each new level
	// triggers a fresh round of condition patches.
	if level != previous {
		a.pressureObserved = make(map[string]MemoryPressureLevel, len(a.processes))
	}
	snapshot := make(map[string]*ManagedProcess, len(a.processes))
	for k, p := range a.processes {
		snapshot[k] = p
	}
	// Determine which keys still need a condition patch at this level.
	// New keys (added since the last tick) and unchanged keys at a new
	// level both fall through; already-observed keys are skipped to keep
	// the status churn quiet under sustained pressure.
	needsPatch := make(map[string]*ManagedProcess, len(snapshot))
	for k, p := range snapshot {
		if a.pressureObserved[k] != level {
			needsPatch[k] = p
		}
	}
	a.mu.Unlock()

	if len(needsPatch) > 0 {
		msg := "watchdog reported " + level.String() + " memory pressure"
		for key, p := range needsPatch {
			if err := a.updateMemoryPressureCondition(ctx, p, level, msg); err != nil {
				a.logger.Warnw("failed to update MemoryPressure condition",
					"key", key, "level", level.String(), "error", err)
				continue
			}
			a.mu.Lock()
			a.pressureObserved[key] = level
			a.mu.Unlock()
		}
	}

	// Eviction path. Honored via the --eviction-enabled CLI flag (default
	// false) so that operators must explicitly opt in to having the
	// watchdog stop inference processes.
	if !a.config.EvictionEnabled {
		// Only count this when the watchdog would otherwise have fired
		// eviction (Critical level). Otherwise we'd inflate the counter
		// every Warning tick and obscure the signal operators care about.
		if level == MemoryPressureCritical {
			evictionsSkippedTotal.WithLabelValues("disabled").Inc()
		}
		return
	}
	if !shouldEvict(level, stats, a.config.EvictionEnabled) {
		// Either not Critical or below the 50% RSS friendly-fire guard.
		// Only the Critical+below-guard case is interesting; non-Critical
		// is the watchdog's normal observation, not a refusal.
		if level == MemoryPressureCritical {
			evictionsSkippedTotal.WithLabelValues("below_guard").Inc()
		}
		return
	}
	target, skipReason := pickEvictionTarget(snapshot, stats.ProcessRSS)
	if target == nil {
		evictionsSkippedTotal.WithLabelValues(skipReason).Inc()
		a.logger.Warnw("memory critical but no eligible eviction target found",
			"managed", len(snapshot), "reason", skipReason)
		return
	}

	key := types.NamespacedName{Namespace: target.Namespace, Name: target.Name}.String()
	a.logger.Warnw("evicting process under critical memory pressure",
		"key", key,
		"priority", target.Priority,
		"totalRSS", formatMemory(stats.TotalRSS),
		"totalMemory", formatMemory(stats.TotalMemory),
	)

	// Mark blocked BEFORE deleteProcess so a controller event arriving
	// during the (potentially slow) process teardown does not race ahead
	// and respawn it.
	a.mu.Lock()
	a.pressureBlocked[key] = true
	a.mu.Unlock()

	if err := a.updateMemoryPressureCondition(ctx, target, MemoryPressureCritical,
		fmt.Sprintf("Evicted by memory watchdog: total RSS %s of %s",
			formatMemory(stats.TotalRSS), formatMemory(stats.TotalMemory)),
	); err != nil {
		a.logger.Warnw("failed to mark evicted condition", "key", key, "error", err)
	}

	if err := a.deleteProcess(ctx, key); err != nil {
		// If teardown failed, don't leave the entry in pressureBlocked: the
		// process is presumably still running (or in an unknown state) and
		// blocking respawn would mask the real problem.
		a.mu.Lock()
		delete(a.pressureBlocked, key)
		a.mu.Unlock()
		a.logger.Errorw("eviction failed; cleared pressure block", "key", key, "error", err)
	}
}

// updateMemoryPressureCondition fetches the InferenceService and patches its
// MemoryPressure condition. Best-effort: the watchdog must not stall on
// transient apiserver errors, so caller logs and continues.
//
// We always re-fetch instead of relying on a cached object because the
// controller may have updated other Status fields between watchdog ticks; a
// blind write would clobber them.
func (a *MetalAgent) updateMemoryPressureCondition(
	ctx context.Context,
	p *ManagedProcess,
	level MemoryPressureLevel,
	message string,
) error {
	if a.config.K8sClient == nil {
		// Tests that don't wire a client should still exercise eviction logic.
		return nil
	}
	isvc := &inferencev1alpha1.InferenceService{}
	key := types.NamespacedName{Namespace: p.Namespace, Name: p.Name}
	if err := a.config.K8sClient.Get(ctx, key, isvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get InferenceService: %w", err)
	}

	status := metav1.ConditionFalse
	reason := ReasonMemoryNormal
	switch level {
	case MemoryPressureWarning:
		status = metav1.ConditionTrue
		reason = ReasonMemoryWarning
	case MemoryPressureCritical:
		status = metav1.ConditionTrue
		reason = ReasonMemoryCritical
	}
	// If we're patching after an eviction, override the reason: the user
	// cares more that we evicted than what level fired it.
	if a.isPressureBlocked(key.String()) {
		reason = ReasonEvicted
	}

	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               ConditionMemoryPressure,
		Status:             status,
		ObservedGeneration: isvc.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	})

	return a.config.K8sClient.Status().Update(ctx, isvc)
}

// isPressureBlocked reports whether the given namespacedName is currently
// in the pressureBlocked set. Helper exists to keep updateMemoryPressureCondition
// from having to grab the agent's lock manually.
func (a *MetalAgent) isPressureBlocked(key string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.pressureBlocked[key]
}
