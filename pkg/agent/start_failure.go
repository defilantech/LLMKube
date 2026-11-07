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

package agent

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// EventReasonStartFailed is the Warning Event reason and the scheduling
// status of a start that failed after the agent did not refuse it: the engine
// exited while loading, the download failed, or the engine did not become
// healthy in time (#2404). It equals the api/v1alpha1 constant, which is in
// MetalAgentRefusalReasons so the controller keeps the status.
const EventReasonStartFailed = inferencev1alpha1.ReasonStartFailed

// failStart reports a failed start and retries it on the restart backoff.
// It writes StartFailed and the error's summary to the scheduling status, so
// the controller stops saying the agent is fetching the model, and, unless the
// start was a restart's own attempt (runRestart reports and reschedules that
// one), queues a retry and emits a Warning Event with the summary, the retry
// delay and where the engine log is. Neither carries the engine log's content
// (summarizeForEvent); the agent log gets the full error. The status names no
// retry time, so a retry does not rewrite it and one that a stop cancels
// leaves nothing stale. eventReason is the Event's reason.
//
// The first retry waits healthRestartInitialBackoff and each later one twice
// as long, up to healthRestartMaxBackoff: a retry may download or load the
// whole model again, so it does not run every poll the way a refusal's
// re-check does. A delete, scale to zero or shutdown cancels it, and a spec
// change starts the new spec at once (deferToPendingRestart), as for any
// pending restart. A start cut short by the agent stopping is neither
// reported nor retried; the next agent process starts the service.
//
// It runs on the event loop with the key's start guard held, so the retry it
// arms cannot start before the failed start has returned.
func (a *MetalAgent) failStart(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, startErr error, eventReason string,
) {
	if ctx.Err() != nil {
		return
	}
	key := types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}.String()
	summary, logPath := summarizeForEvent(startErr)
	a.writeStartFailedStatus(ctx, isvc, summary)

	a.mu.Lock()
	if st := a.restarts[key]; st != nil && st.attempting {
		a.mu.Unlock()
		return
	}
	st := a.restartStateLocked(key)
	if st.delay == 0 {
		st.delay = healthRestartInitialBackoff
	}
	st.specHash = computeSpecHash(isvc)
	retryAt := a.scheduleRestartLocked(key, st.releaseAt)
	retryIn := max(retryAt.Sub(a.now()), 0)
	a.mu.Unlock()

	a.logger.Warnw("start failed; retrying",
		"namespace", isvc.Namespace, "name", isvc.Name, "retryAt", retryAt, "retryIn", retryIn, "error", startErr)
	a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: isvc.Namespace, Name: isvc.Name},
		corev1.EventTypeWarning, eventReason, "%s",
		fmt.Sprintf("Start failed: %s; retrying in %s", summary, retryIn)+engineLogHint(logPath))
}

// writeStartFailedStatus records a failed start on the InferenceService's
// scheduling status, on a fresh copy: memory admission may have rewritten the
// status of the copy in hand, and the controller may have written its own
// since. A status that already says so is left alone.
func (a *MetalAgent) writeStartFailedStatus(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, message string,
) {
	fresh := &inferencev1alpha1.InferenceService{}
	key := types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}
	if err := a.config.K8sClient.Get(ctx, key, fresh); err != nil {
		a.logger.Warnw("failed to fetch InferenceService to record a failed start", "key", key, "error", err)
		return
	}
	if fresh.Status.SchedulingStatus == EventReasonStartFailed && fresh.Status.SchedulingMessage == message {
		return
	}
	fresh.Status.SchedulingStatus = EventReasonStartFailed
	fresh.Status.SchedulingMessage = message
	if err := a.config.K8sClient.Status().Update(ctx, fresh); err != nil {
		a.logger.Warnw("failed to record a failed start on the InferenceService status", "key", key, "error", err)
	}
}

// clearStartFailure clears a StartFailed status once an engine runs for isvc.
// Memory admission clears it before most starts, but not when it could not
// complete in memory-check-mode=warn, so a retry that succeeded would leave
// the stale failure behind.
func (a *MetalAgent) clearStartFailure(ctx context.Context, isvc *inferencev1alpha1.InferenceService) {
	if isvc.Status.SchedulingStatus != EventReasonStartFailed {
		return
	}
	key := types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}
	a.mu.RLock()
	running := a.processes[key.String()] != nil
	a.mu.RUnlock()
	if !running {
		return
	}
	fresh := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(ctx, key, fresh); err != nil {
		a.logger.Warnw("failed to fetch InferenceService to clear a failed start", "key", key, "error", err)
		return
	}
	if fresh.Status.SchedulingStatus != EventReasonStartFailed {
		return
	}
	fresh.Status.SchedulingStatus = ""
	fresh.Status.SchedulingMessage = ""
	if err := a.config.K8sClient.Status().Update(ctx, fresh); err != nil {
		a.logger.Warnw("failed to clear a failed start from the InferenceService status", "key", key, "error", err)
	}
}
