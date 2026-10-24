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
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// ErrWatchStalled is returned from Watch when the polling loop has accumulated
// more consecutive list errors than the configured threshold. The agent treats
// this as a fatal condition and exits non-zero so the process supervisor
// (launchd / systemd) can recycle it with a fresh Kubernetes client. This
// recovers from situations like the underlying controller-runtime cache
// silently serving stale data after its watch connection drops.
var ErrWatchStalled = errors.New("watcher polling stalled: consecutive list failures exceeded threshold")

// DefaultMaxConsecutiveFailures is the threshold at which the watcher gives
// up on its current Kubernetes connection. Three failures over the 5-second
// poll interval is 15 seconds of consecutive trouble — long enough to rule
// out a single transient network blip and short enough that the agent does
// not silently sit on a dead watch for hours.
const DefaultMaxConsecutiveFailures = 3

// EventType represents the type of InferenceService event
type EventType string

const (
	EventTypeCreated EventType = "CREATED"
	EventTypeUpdated EventType = "UPDATED"
	EventTypeDeleted EventType = "DELETED"
)

// InferenceServiceEvent represents a change to an InferenceService
type InferenceServiceEvent struct {
	Type             EventType
	InferenceService *inferencev1alpha1.InferenceService
}

// defaultPollInterval is how often the watcher re-lists InferenceServices
// looking for changes. Five seconds is a balance between responsiveness for
// scaling events and load on the API server when many agents share a cluster.
const defaultPollInterval = 5 * time.Second

// InferenceServiceWatcher watches for InferenceService resources
type InferenceServiceWatcher struct {
	client                 client.Client
	namespace              string
	logger                 *zap.SugaredLogger
	maxConsecutiveFailures int
	pollInterval           time.Duration
	// nameAllowlist optionally restricts which InferenceServices the
	// watcher claims by name. When nil or empty the watcher claims
	// every metal-accelerator InferenceService in its namespace
	// (v0.1 behavior); when non-empty only InferenceServices whose
	// name is a member are surfaced as events. Set via
	// SetNameAllowlist; v0.2 #524 (per-node partition for multi-Mac
	// fleets sharing one cluster).
	nameAllowlist map[string]struct{}
	// lookupFailing maps the key of each InferenceService whose latest
	// Model lookup failed (other than NotFound) to when it started failing,
	// so the failure is logged at Warn once and at Info when it clears
	// rather than on every poll (#2076). lookupMu guards it and the gauge
	// updates derived from it: shouldWatch may run on more than one
	// goroutine (the poll loop, and callers outside the watcher that use it
	// as a claim guard).
	lookupMu      sync.Mutex
	lookupFailing map[string]time.Time
	// seeded holds keys the first poll treats as already seen (engines
	// adopted at startup, #2079); see SeedSeen.
	seeded map[string]bool
}

// NewInferenceServiceWatcher creates a new watcher with the default failure
// threshold (DefaultMaxConsecutiveFailures). Use SetMaxConsecutiveFailures to
// override.
func NewInferenceServiceWatcher(
	k8sClient client.Client,
	namespace string,
	logger *zap.SugaredLogger,
) *InferenceServiceWatcher {
	return &InferenceServiceWatcher{
		client:                 k8sClient,
		namespace:              namespace,
		logger:                 logger,
		maxConsecutiveFailures: DefaultMaxConsecutiveFailures,
		pollInterval:           defaultPollInterval,
	}
}

// SetMaxConsecutiveFailures overrides the default failure threshold. Values
// less than 1 are coerced to DefaultMaxConsecutiveFailures.
func (w *InferenceServiceWatcher) SetMaxConsecutiveFailures(n int) {
	if n < 1 {
		n = DefaultMaxConsecutiveFailures
	}
	w.maxConsecutiveFailures = n
}

// SetPollInterval overrides the default 5s polling interval. Intended for
// tests that want to drive the loop quickly; production code should leave
// the default alone. Zero or negative values are ignored.
func (w *InferenceServiceWatcher) SetPollInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	w.pollInterval = d
}

// SetNameAllowlist restricts which InferenceServices the watcher
// surfaces, by name. Empty / nil disables the filter (v0.1 behavior:
// every metal-accelerator InferenceService in the namespace is
// claimed). When set, only InferenceServices whose name is a member
// produce events; everything else is silently skipped at the
// shouldWatch gate.
//
// Use this on multi-Mac fleets sharing one cluster so each node
// claims only its own InferenceServices instead of racing with peers
// for every metal ISvc in the namespace (#524).
func (w *InferenceServiceWatcher) SetNameAllowlist(names []string) {
	if len(names) == 0 {
		w.nameAllowlist = nil
		return
	}
	allow := make(map[string]struct{}, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		allow[n] = struct{}{}
	}
	if len(allow) == 0 {
		w.nameAllowlist = nil
		return
	}
	w.nameAllowlist = allow
}

// SeedSeen makes the first poll treat keys as already seen, at no resource
// version. The agent seeds the engines it adopted at startup: an adopted key
// the poll watches then gets Updated (a no-op while its spec hash matches),
// and one it does not watch (deleted, off the allowlist, its Model gone or no
// longer metal) gets Deleted, which stops the engine. Call it before Watch.
func (w *InferenceServiceWatcher) SeedSeen(keys map[string]bool) {
	w.seeded = maps.Clone(keys)
}

// initialSeen returns the seen map the first poll starts from, holding the
// seeded keys, and drops the seed so it is used once. Watch takes it only
// after its initial list succeeded, so a Watch retried after that list failed
// still starts from the seed.
func (w *InferenceServiceWatcher) initialSeen() map[string]string {
	seen := make(map[string]string, len(w.seeded))
	for key := range w.seeded {
		seen[key] = ""
	}
	w.seeded = nil
	return seen
}

// Watch starts watching for InferenceService changes. It returns nil on clean
// context cancellation, ErrWatchStalled when consecutive list failures exceed
// the configured threshold, or a wrapped error on initial-list failure.
func (w *InferenceServiceWatcher) Watch(ctx context.Context, eventChan chan<- InferenceServiceEvent) error {
	// List existing InferenceServices on startup
	if err := w.listExisting(ctx, eventChan); err != nil {
		return fmt.Errorf("failed to list existing services: %w", err)
	}

	// Watch for changes
	interval := w.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Store last seen resource versions to detect changes
	seen := w.initialSeen()

	threshold := w.maxConsecutiveFailures
	if threshold < 1 {
		threshold = DefaultMaxConsecutiveFailures
	}
	consecutiveFailures := 0
	watchConsecutiveFailures.Set(0)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.poll(ctx, eventChan, seen); err != nil {
				consecutiveFailures++
				watchConsecutiveFailures.Set(float64(consecutiveFailures))
				w.logger.Warnw("polling error",
					"error", err,
					"consecutiveFailures", consecutiveFailures,
					"threshold", threshold,
				)
				if consecutiveFailures >= threshold {
					w.logger.Errorw(
						"watcher stalled — exceeded consecutive failure threshold, "+
							"returning ErrWatchStalled so the agent can exit and "+
							"be restarted by its supervisor",
						"consecutiveFailures", consecutiveFailures,
						"threshold", threshold,
						"lastError", err,
					)
					return ErrWatchStalled
				}
				continue
			}
			if consecutiveFailures > 0 {
				w.logger.Infow("polling recovered after errors", "previousFailures", consecutiveFailures)
				consecutiveFailures = 0
				watchConsecutiveFailures.Set(0)
			}
		}
	}
}

// listExisting lists all existing InferenceServices and sends create events
func (w *InferenceServiceWatcher) listExisting(ctx context.Context, eventChan chan<- InferenceServiceEvent) error {
	list := &inferencev1alpha1.InferenceServiceList{}
	opts := []client.ListOption{}
	if w.namespace != "" {
		opts = append(opts, client.InNamespace(w.namespace))
	}

	if err := w.client.List(ctx, list, opts...); err != nil {
		return err
	}

	for i := range list.Items {
		// Only watch services with Metal accelerator. A service whose Model
		// lookup failed is not started yet: the first poll finds it unseen
		// and emits Created once a lookup succeeds.
		if watch, err := w.shouldWatch(ctx, &list.Items[i]); err != nil || !watch {
			continue
		}

		eventChan <- InferenceServiceEvent{
			Type:             EventTypeCreated,
			InferenceService: &list.Items[i],
		}
	}

	return nil
}

// poll checks for changes to InferenceServices
func (w *InferenceServiceWatcher) poll(
	ctx context.Context,
	eventChan chan<- InferenceServiceEvent,
	seen map[string]string,
) error {
	list := &inferencev1alpha1.InferenceServiceList{}
	opts := []client.ListOption{}
	if w.namespace != "" {
		opts = append(opts, client.InNamespace(w.namespace))
	}

	if err := w.client.List(ctx, list, opts...); err != nil {
		return err
	}

	// Track current services
	current := make(map[string]bool)

	for i := range list.Items {
		isvc := &list.Items[i]
		key := watchKey(isvc)

		watch, err := w.shouldWatch(ctx, isvc)
		if err != nil {
			// Ownership is unknown this round (#2076). Counting the service
			// as current keeps a seen one out of the deletion pass below,
			// with its recorded version, so the next successful poll
			// compares against that as usual; an unseen one is not added.
			current[key] = true
			continue
		}
		if !watch {
			continue
		}
		current[key] = true

		lastVersion, exists := seen[key]
		currentVersion := isvc.ResourceVersion

		if !exists {
			// New service
			eventChan <- InferenceServiceEvent{
				Type:             EventTypeCreated,
				InferenceService: isvc,
			}
			seen[key] = currentVersion
		} else if lastVersion != currentVersion {
			// Updated service
			eventChan <- InferenceServiceEvent{
				Type:             EventTypeUpdated,
				InferenceService: isvc,
			}
			seen[key] = currentVersion
		}
	}

	w.forgetVanishedLookupFailures(current)

	// Check for deleted services
	for key := range seen {
		if !current[key] {
			// Service was deleted (we don't have the object anymore)
			// Create a minimal object for the delete event
			namespace, name := parseKey(key)
			eventChan <- InferenceServiceEvent{
				Type: EventTypeDeleted,
				InferenceService: &inferencev1alpha1.InferenceService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: namespace,
					},
				},
			}
			delete(seen, key)
		}
	}

	return nil
}

// shouldWatch determines if this InferenceService should be watched by the Metal Agent.
// It applies (in order):
//  1. nameAllowlist filter -- cheap; skips the API call below if the
//     InferenceService isn't on this node's list. v0.2 #524: lets
//     multi-Mac fleets partition without colliding.
//  2. Model.spec.hardware.accelerator check -- looks up the referenced
//     Model and returns true only when its accelerator is "metal".
//
// A NotFound Model means the service is not ours. Any other lookup error (a
// timeout, client-side throttling, a 5xx while the API server restarts) is
// returned instead, because the answer is unknown: callers must leave the
// service as it was rather than treat it as not ours, or one transient error
// stops a healthy engine and deletes its endpoint (#2076).
func (w *InferenceServiceWatcher) shouldWatch(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService,
) (bool, error) {
	watch, err := w.decideWatch(ctx, isvc)
	switch {
	case err == nil:
		w.noteLookupDecided(isvc)
	case errors.Is(ctx.Err(), context.Canceled):
		// The agent is shutting down and the Get failed because of it, not
		// because the API server is unhealthy: still unknown, but not worth
		// a log line or a change to the failure tracking. A caller's expired
		// deadline is not this case: a Get that outlived it points at a slow
		// API server, so it is logged and counted like any other failure.
	default:
		w.noteLookupFailed(isvc, err)
	}
	return watch, err
}

// decideWatch is shouldWatch's decision without the failure tracking.
func (w *InferenceServiceWatcher) decideWatch(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService,
) (bool, error) {
	if isvc.Spec.ModelRef == "" {
		return false, nil
	}

	if w.nameAllowlist != nil {
		if _, ok := w.nameAllowlist[isvc.Name]; !ok {
			w.logger.Debugw(
				"skipping inference service: not in this agent's name allowlist (#524)",
				"namespace", isvc.Namespace,
				"inferenceService", isvc.Name,
			)
			return false, nil
		}
	}

	model := &inferencev1alpha1.Model{}
	if err := w.client.Get(ctx, types.NamespacedName{
		Namespace: isvc.Namespace,
		Name:      isvc.Spec.ModelRef,
	}, model); err != nil {
		if apierrors.IsNotFound(err) {
			w.logger.Debugw(
				"skipping inference service because referenced model does not exist",
				"namespace", isvc.Namespace,
				"inferenceService", isvc.Name,
				"modelRef", isvc.Spec.ModelRef,
			)
			return false, nil
		}
		return false, fmt.Errorf("get model %s/%s: %w", isvc.Namespace, isvc.Spec.ModelRef, err)
	}

	return model.Spec.Hardware != nil && model.Spec.Hardware.Accelerator == "metal", nil
}

// noteLookupFailed records a failed Model lookup for isvc: a Warn when this
// starts a failure, a Debug line while it continues.
func (w *InferenceServiceWatcher) noteLookupFailed(isvc *inferencev1alpha1.InferenceService, err error) {
	key := watchKey(isvc)
	w.lookupMu.Lock()
	since, failing := w.lookupFailing[key]
	if !failing {
		if w.lookupFailing == nil {
			w.lookupFailing = make(map[string]time.Time)
		}
		w.lookupFailing[key] = time.Now()
		watchModelLookupFailingServices.Set(float64(len(w.lookupFailing)))
	}
	w.lookupMu.Unlock()

	if failing {
		w.logger.Debugw("referenced model lookup still failing; keeping the inference service's previous state",
			"namespace", isvc.Namespace,
			"inferenceService", isvc.Name,
			"modelRef", isvc.Spec.ModelRef,
			"failingFor", time.Since(since).Round(time.Second),
			"error", err,
		)
		return
	}
	w.logger.Warnw("referenced model lookup failed; keeping the inference service's previous state",
		"namespace", isvc.Namespace,
		"inferenceService", isvc.Name,
		"modelRef", isvc.Spec.ModelRef,
		"error", err,
	)
}

// noteLookupDecided clears a recorded lookup failure for isvc once shouldWatch
// reaches an answer for it again, logging the recovery at Info.
func (w *InferenceServiceWatcher) noteLookupDecided(isvc *inferencev1alpha1.InferenceService) {
	key := watchKey(isvc)
	w.lookupMu.Lock()
	since, failing := w.lookupFailing[key]
	if failing {
		delete(w.lookupFailing, key)
		watchModelLookupFailingServices.Set(float64(len(w.lookupFailing)))
	}
	w.lookupMu.Unlock()

	if !failing {
		return
	}
	w.logger.Infow("referenced model lookup recovered",
		"namespace", isvc.Namespace,
		"inferenceService", isvc.Name,
		"modelRef", isvc.Spec.ModelRef,
		"failedFor", time.Since(since).Round(time.Second),
	)
}

// forgetVanishedLookupFailures drops the recorded lookup failure of every
// service the latest poll did not list, so a service deleted while its lookup
// failed does not hold the gauge up forever. A listed service whose lookup is
// still failing is in current (poll counts it there), and one whose lookup
// succeeded was already cleared by noteLookupDecided. A failure another
// goroutine records meanwhile for a listed service this poll found not ours
// is dropped as well, which only means its next failure warns again.
func (w *InferenceServiceWatcher) forgetVanishedLookupFailures(current map[string]bool) {
	w.lookupMu.Lock()
	defer w.lookupMu.Unlock()
	pruned := false
	for key := range w.lookupFailing {
		if !current[key] {
			delete(w.lookupFailing, key)
			pruned = true
		}
	}
	if pruned {
		watchModelLookupFailingServices.Set(float64(len(w.lookupFailing)))
	}
}

// owns adapts shouldWatch to the ownership predicate the startup cleanup
// passes take (ServiceRegistry.WithdrawOwnedEndpoints and
// RemoveRelayRegistrations). They act only on services the predicate claims,
// so reporting an unknown answer as false leaves that service's objects as
// they are: the same "keep the previous state" the poll loop applies.
func (w *InferenceServiceWatcher) owns(ctx context.Context, isvc *inferencev1alpha1.InferenceService) bool {
	watch, err := w.shouldWatch(ctx, isvc)
	return err == nil && watch
}

// watchKey is the "namespace/name" key poll tracks isvc under.
func watchKey(isvc *inferencev1alpha1.InferenceService) string {
	return isvc.Namespace + "/" + isvc.Name
}

// parseKey splits "namespace/name" into components
func parseKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return "", key
}
