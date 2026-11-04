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
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// DefaultHealthFailureThreshold is how many consecutive failed health probes
// make the agent stop an engine and start a new one when
// MetalAgentConfig.HealthFailureThreshold is zero. The first failure already
// withdraws the endpoint (#662); the restart waits for the rest, so one slow
// probe does not reload a large model.
const DefaultHealthFailureThreshold = 3

// Health restart backoff, after the kubelet's container restart backoff: the
// first restart runs at once, each later one waits twice as long as the one
// before (10s, 20s, 40s, ...) up to healthRestartMaxBackoff, and the delay
// resets once a restarted engine has been up for healthRestartBackoffReset.
const (
	healthRestartInitialBackoff = 10 * time.Second
	healthRestartMaxBackoff     = 5 * time.Minute
	healthRestartBackoffReset   = 10 * time.Minute
)

// restartGuardRetry is how soon a due restart looks again when another start
// (or the health monitor's stop) holds its key.
const restartGuardRetry = time.Second

// Kubernetes event reasons for health restarts.
const (
	EventReasonHealthRestart = "HealthRestart"
	EventReasonRestartFailed = "RestartFailed"
)

// restartBackoff is the health-restart state of one InferenceService. It
// outlives the engines that replace each other, so the delay keeps growing
// while they keep failing. Guarded by MetalAgent.mu.
type restartBackoff struct {
	// delay is how long the next restart waits.
	delay time.Duration
	// pending is set while the service is owed a restart: its engine was
	// stopped for failing health checks, or the last restart attempt left
	// no engine running.
	pending bool
	// nextAt is the earliest time the pending restart may run.
	nextAt time.Time
	// specHash is the spec of the engine that last failed. A watch event with
	// a different spec resets the backoff (see deferToPendingRestart). Empty
	// when unknown (a queued restart), which never resets it.
	specHash string
	// releaseAt is the earliest time a new engine may start after a stop
	// that failed or had to SIGKILL, so the old engine can release its wired
	// memory first. Unlike the backoff, nothing resets it.
	releaseAt time.Time
	// timer wakes the event loop at nextAt.
	timer *time.Timer
	// attempting is set while runRestart's own start runs. A failed start
	// then leaves the retry and its Event to runRestart (failStart).
	attempting bool
}

// healthFailureThreshold resolves config.HealthFailureThreshold.
func (a *MetalAgent) healthFailureThreshold() int {
	if a.config.HealthFailureThreshold > 0 {
		return a.config.HealthFailureThreshold
	}
	return DefaultHealthFailureThreshold
}

// nextRestartDelay returns the delay that follows d: the initial backoff
// after none, then double the last one, capped at healthRestartMaxBackoff.
func nextRestartDelay(d time.Duration) time.Duration {
	if d <= 0 {
		return healthRestartInitialBackoff
	}
	return min(2*d, healthRestartMaxBackoff)
}

// restartStateLocked returns key's restart state, creating it if needed.
// Must be called with a.mu held.
func (a *MetalAgent) restartStateLocked(key string) *restartBackoff {
	if a.restarts == nil {
		a.restarts = make(map[string]*restartBackoff)
	}
	st := a.restarts[key]
	if st == nil {
		st = &restartBackoff{}
		a.restarts[key] = st
	}
	return st
}

// scheduleRestartLocked marks key as owed a restart once the current delay
// has passed (and not before notBefore), grows the delay for the restart
// after it, and arms a wake-up for the event loop. It returns when the
// restart may run. Must be called with a.mu held.
func (a *MetalAgent) scheduleRestartLocked(key string, notBefore time.Time) time.Time {
	st := a.restartStateLocked(key)
	st.pending = true
	st.nextAt = a.now().Add(st.delay)
	if st.nextAt.Before(notBefore) {
		st.nextAt = notBefore
	}
	st.delay = nextRestartDelay(st.delay)
	a.armRestartLocked(st, st.nextAt)
	return st.nextAt
}

// queueRestart asks the event loop to start key's engine once notBefore has
// passed (at once for the zero time). It is the entry point for starts other
// than a health restart: it records no health failure, does not grow the
// backoff, and never brings forward a restart the backoff already delays or
// starts an engine before an unclean stop's release floor (releaseAt).
// If the queued start fails, the retry waits the initial backoff and grows
// from there. A delete, scale-to-zero, eviction or shutdown cancels it like
// any pending restart. reason is logged.
func (a *MetalAgent) queueRestart(key string, notBefore time.Time, reason string) {
	a.mu.Lock()
	notBefore = a.queueRestartLocked(key, notBefore)
	a.mu.Unlock()

	a.logger.Infow("restart queued", "key", key, "reason", reason, "notBefore", notBefore)
}

// queueRestartLocked is queueRestart for a caller that holds a.mu, so the
// queueing is atomic with the caller's own checks. It returns when the
// restart may run. Must be called with a.mu held.
func (a *MetalAgent) queueRestartLocked(key string, notBefore time.Time) time.Time {
	st := a.restartStateLocked(key)
	if st.pending && st.nextAt.After(notBefore) {
		notBefore = st.nextAt
	}
	if st.releaseAt.After(notBefore) {
		notBefore = st.releaseAt
	}
	st.pending = true
	st.nextAt = notBefore
	if st.delay == 0 {
		st.delay = healthRestartInitialBackoff
	}
	a.armRestartLocked(st, st.nextAt)
	return notBefore
}

// armRestartLocked replaces st's wake-up with one at at, or wakes the event
// loop now when at has passed. Must be called with a.mu held.
func (a *MetalAgent) armRestartLocked(st *restartBackoff, at time.Time) {
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	if wait := at.Sub(a.now()); wait > 0 {
		st.timer = time.AfterFunc(wait, a.wakeRestarts)
	} else {
		a.wakeRestarts()
	}
}

// deferToPendingRestart reports whether a start of key must be left to its
// pending restart, which owns the start until it runs while no engine is
// tracked. Without this, any write to the InferenceService (status included)
// emits a watch event whose reconcile would start the engine at once and skip
// the backoff. A spec that differs from the one whose engine failed resets
// the backoff instead: the restart is queued to run on the next loop turn
// with the new spec, or once the release floor passes if the old engine's
// stop was unclean. Only called on the event loop.
func (a *MetalAgent) deferToPendingRestart(key, specHash string) bool {
	a.mu.Lock()
	st := a.restarts[key]
	if st == nil || !st.pending || a.processes[key] != nil {
		a.mu.Unlock()
		return false
	}
	if st.specHash == "" || st.specHash == specHash {
		nextAt := st.nextAt
		a.mu.Unlock()
		a.logger.Debugw("start left to the pending restart", "key", key, "restartAt", nextAt)
		return true
	}
	releaseAt := st.releaseAt
	a.cancelRestartLocked(key)
	a.mu.Unlock()
	a.queueRestart(key, releaseAt, "spec changed while a health restart was backing off")
	return true
}

// dropRestart cancels key's restart. When no engine is tracked for key it
// also removes the endpoint, which the health monitor's stop left withdrawn:
// nothing else removes it once nothing is tracked. A key with no restart
// state is left alone.
func (a *MetalAgent) dropRestart(ctx context.Context, key string) error {
	a.mu.Lock()
	_, had := a.restarts[key]
	a.cancelRestartLocked(key)
	_, tracked := a.processes[key]
	a.mu.Unlock()
	if !had || tracked {
		return nil
	}
	return a.unregisterKey(ctx, key)
}

// unregisterKey removes the endpoint of the InferenceService key names.
func (a *MetalAgent) unregisterKey(ctx context.Context, key string) error {
	namespace, name := parseKey(key)
	if err := a.registry.UnregisterEndpoint(ctx, namespace, name); err != nil {
		return fmt.Errorf("failed to unregister endpoint for %s: %w", key, err)
	}
	return nil
}

// cancelRestartLocked forgets key's restart state, a pending restart
// included. Must be called with a.mu held.
func (a *MetalAgent) cancelRestartLocked(key string) {
	st := a.restarts[key]
	if st == nil {
		return
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	delete(a.restarts, key)
}

// resetRestartBackoffLocked forgets key's restart backoff once its engine has
// been up for healthRestartBackoffReset and is answering health checks, so a
// failure long after a crash loop restarts at once again. Must be called with
// a.mu held.
func (a *MetalAgent) resetRestartBackoffLocked(key string, p *ManagedProcess) {
	st := a.restarts[key]
	if st == nil || st.pending || a.now().Sub(p.StartedAt) < healthRestartBackoffReset {
		return
	}
	a.cancelRestartLocked(key)
}

// wakeRestarts asks the event loop to run due restarts. It never blocks: one
// queued wake-up covers every restart that is due when the loop gets to it.
func (a *MetalAgent) wakeRestarts() {
	select {
	case a.restartWake <- struct{}{}:
	default:
	}
}

// restartUnhealthy stops proc, which has failed `failures` consecutive health
// probes, and schedules its replacement on the service's restart backoff
// (stopForRestart). It runs on the health monitor's goroutine.
func (a *MetalAgent) restartUnhealthy(ctx context.Context, key string, proc *ManagedProcess, failures int) {
	restartIn, ok := a.stopForRestart(ctx, key, proc, "consecutiveFailures", failures)
	if !ok {
		return
	}
	a.emitInferenceEvent(ctx, proc, corev1.EventTypeWarning, EventReasonHealthRestart,
		"Engine failed %d consecutive health checks and was stopped; restarting in %s", failures, restartIn)
}

// stopForRestart stops proc, an engine marked unhealthy (it failed its health
// probes, or its process exited), and schedules its replacement on the
// service's restart backoff. It returns how long until the restart, and false
// when it left proc alone or scheduled nothing. The replacement starts later
// on the event loop (runDueRestarts), never before the stop has returned.
// logKV is added to its log lines.
//
// proc is removed from a.processes and its key held in a.starting under one
// lock, so a watch event can neither start a second engine while this one is
// stopping nor stop it a second time. A proc that was replaced, removed or
// recovered since it was marked, or whose key has a start in flight, is left
// alone; the next probe looks again.
func (a *MetalAgent) stopForRestart(
	ctx context.Context, key string, proc *ManagedProcess, logKV ...interface{},
) (time.Duration, bool) {
	a.mu.Lock()
	if a.processes[key] != proc || proc.Healthy || a.starting[key] {
		a.mu.Unlock()
		return 0, false
	}
	delete(a.processes, key)
	managedProcesses.Set(float64(len(a.processes)))
	a.updateCommittedMetricLocked()
	a.starting[key] = true
	// Held from before the stop: a delete handled while the engine stops
	// cancels it, and that is how the check below sees the delete.
	st := a.restartStateLocked(key)
	st.specHash = proc.SpecHash
	a.mu.Unlock()

	a.logger.Warnw("stopping unhealthy process to restart it", append([]interface{}{
		"namespace", proc.Namespace, "name", proc.Name, "pid", proc.PID}, logKV...)...)
	var notBefore time.Time
	stopErr := a.stopManagedProcess(ctx, proc, false)
	// The record goes only once the engine is confirmed stopped.
	if stopErr == nil || engineStopped(stopErr) {
		a.removeEngineRecord(proc)
	}
	if err := stopErr; err != nil {
		// A stop that failed, or that had to SIGKILL, may not have released
		// the engine's memory yet: give it the initial backoff before a new
		// engine wires another copy of the weights.
		a.logger.Warnw("unhealthy process did not stop cleanly",
			"namespace", proc.Namespace, "name", proc.Name, "pid", proc.PID, "error", err)
		notBefore = a.now().Add(healthRestartInitialBackoff)
	}

	a.mu.Lock()
	delete(a.starting, key)
	if a.restarts[key] != st || ctx.Err() != nil {
		a.mu.Unlock()
		a.logger.Infow("service torn down or agent stopping while its unhealthy process stopped; not restarting it",
			"namespace", proc.Namespace, "name", proc.Name)
		return 0, false
	}
	if !notBefore.IsZero() {
		st.releaseAt = notBefore
	}
	restartAt := a.scheduleRestartLocked(key, notBefore)
	restartIn := max(restartAt.Sub(a.now()), 0)
	a.mu.Unlock()

	a.logger.Warnw("restart scheduled for unhealthy process",
		"namespace", proc.Namespace, "name", proc.Name, "restartAt", restartAt, "restartIn", restartIn)
	return restartIn, true
}

// runDueRestarts runs every pending health restart whose backoff has expired.
// It runs on the event loop, so a restart's start is serialized with watch
// events: a delete or scale-to-zero handled before it cancels the restart,
// and one that arrives during it is handled after it, against the new engine.
func (a *MetalAgent) runDueRestarts(ctx context.Context) {
	now := a.now()
	a.mu.Lock()
	var due []string
	for key, st := range a.restarts {
		if st.pending && !now.Before(st.nextAt) {
			due = append(due, key)
		}
	}
	a.mu.Unlock()
	slices.Sort(due)

	for _, key := range due {
		if ctx.Err() != nil {
			return
		}
		a.runRestart(ctx, key)
	}
}

// runRestart starts the engine of a service owed a restart, unless the
// restart was cancelled or another start already brought an engine up. A
// service that is gone or being deleted drops the restart, a stopped one has
// its stop applied; an attempt that leaves no engine running is retried on
// the backoff schedule.
func (a *MetalAgent) runRestart(ctx context.Context, key string) {
	a.mu.Lock()
	st := a.restarts[key]
	switch {
	case st == nil || !st.pending || a.now().Before(st.nextAt):
		// Cancelled or rescheduled.
		a.mu.Unlock()
		return
	case a.starting[key]:
		// Another start, or the health monitor's stop, holds the key. Look
		// again shortly rather than rely on the holder to reschedule.
		a.armRestartLocked(st, a.now().Add(restartGuardRetry))
		a.mu.Unlock()
		return
	case a.processes[key] != nil:
		// A watch event started the service in the meantime.
		st.pending = false
		a.mu.Unlock()
		return
	}
	// This attempt is the start deferToPendingRestart leaves to the restart,
	// so the reconcile below must not defer it again.
	st.pending = false
	st.attempting = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		st.attempting = false
		a.mu.Unlock()
	}()

	namespace, name := parseKey(key)
	isvc := &inferencev1alpha1.InferenceService{}
	err := a.config.K8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, isvc)
	var specHash string
	switch {
	case apierrors.IsNotFound(err), err == nil && isvc.DeletionTimestamp != nil:
		if dropErr := a.dropRestart(ctx, key); dropErr != nil {
			a.logger.Warnw("failed to clean up after a cancelled restart", "key", key, "error", dropErr)
		}
		return
	case err != nil:
		err = fmt.Errorf("failed to fetch InferenceService: %w", err)
	case isvcStopped(isvc):
		// A scale-to-zero or suspend whose watch event found the key held
		// by the health monitor's stop and was dropped. Apply it now: this
		// cancels the restart and records the stopped status.
		if stopErr := a.handleScaleToZero(ctx, isvc, key, false); stopErr != nil {
			a.logger.Warnw("failed to apply stop to a restarting service", "key", key, "error", stopErr)
		}
		return
	default:
		specHash = computeSpecHash(isvc)
		processRestarts.WithLabelValues(name, namespace).Inc()
		a.logger.Infow("restarting inference service",
			"namespace", namespace, "name", name)
		err = a.ensureProcess(ctx, isvc)
	}

	a.mu.Lock()
	cur := a.restarts[key]
	if cur == nil || a.processes[key] != nil {
		// Cancelled during the attempt, or the attempt brought an engine up.
		a.mu.Unlock()
		return
	}
	if specHash != "" {
		cur.specHash = specHash
	}
	retryAt := a.scheduleRestartLocked(key, time.Time{})
	retryIn := retryAt.Sub(a.now())
	a.mu.Unlock()

	if err == nil {
		err = errors.New("no engine is running after the restart")
	}
	a.logger.Warnw("restart failed; retrying",
		"namespace", namespace, "name", name, "retryAt", retryAt, "retryIn", retryIn, "error", err)
	// The engine log tail goes after the summary, bounded, as in the
	// StartFailed Event of a first start.
	reason, logPath, tail := splitLogTail(err)
	a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: namespace, Name: name}, corev1.EventTypeWarning,
		EventReasonRestartFailed, "%s", withEventLogTail(
			fmt.Sprintf("Restart did not bring the engine back: %s; retrying in %s", reason, retryIn), logPath, tail))
}
