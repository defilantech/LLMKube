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
	"maps"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// DefaultMaxConsecutiveFailures is how many consecutive failed InferenceService
// lists make the watcher count the Kubernetes API as unreachable. Three
// failures over the 5-second poll interval is 15 seconds of consecutive
// trouble: long enough to rule out a single transient network blip.
const DefaultMaxConsecutiveFailures = 3

const (
	// defaultMaxPollBackoff caps the delay between lists while the API counts
	// as unreachable. It matches the heartbeat interval, so the watcher
	// notices the API's return about as soon as the heartbeat does.
	defaultMaxPollBackoff = 30 * time.Second
	// pollBackoffJitter is the largest fraction wait.Jitter adds to a
	// backoff delay, so a fleet of agents cut off by one outage does not
	// retry in lockstep when it ends.
	pollBackoffJitter = 0.2
)

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
	// requeued holds keys a poll delivers again even though their
	// resourceVersion has not changed, each with the time from which it is
	// due; see Requeue and RequeueAt. requeueMu guards it: the agent's event
	// loop sets keys while the poll loop takes them.
	requeueMu sync.Mutex
	requeued  map[string]time.Time
	// now returns the current time for RequeueAt; time.Now unless a test
	// sets a fake clock.
	now func() time.Time
	// maxPollBackoff caps the delay between lists while the API counts as
	// unreachable; zero means defaultMaxPollBackoff.
	maxPollBackoff time.Duration
	// listFailures counts consecutive failed InferenceService lists, the
	// initial list included, and failingSince is when the first of them
	// failed. unreachable is set once listFailures reaches the threshold and
	// cleared by the next successful list. Only the goroutine running Watch
	// touches them.
	listFailures int
	failingSince time.Time
	unreachable  bool
	// localNetworkHinted is set once the current outage has logged the Local
	// Network hint (see noteLocalNetworkBlock), and cleared with unreachable.
	localNetworkHinted bool
	// apiHost is the API server's host; see SetAPIServer.
	apiHost string
	// goos is the system the Local Network hint is for: runtime.GOOS unless
	// a test sets another.
	goos string
	// resetConnections and onReachable are the API hooks; see SetAPIHooks.
	resetConnections func()
	onReachable      func()
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
		now:                    time.Now,
		goos:                   runtime.GOOS,
	}
}

// SetMaxConsecutiveFailures overrides how many consecutive failed lists make
// the watcher count the API as unreachable. Values less than 1 are coerced to
// DefaultMaxConsecutiveFailures.
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

// SetAPIHooks sets what the watcher calls about the API's reachability, on the
// goroutine running Watch. reset runs after each failed list once the API
// counts as unreachable, before the retry: the agent closes its client's idle
// connections there, so the retry dials a new one, which is what restarting
// the process used to buy (#276). reachable runs once when a list succeeds
// again after that. Either may be nil. Call it before Watch.
func (w *InferenceServiceWatcher) SetAPIHooks(reset, reachable func()) {
	w.resetConnections = reset
	w.onReachable = reachable
}

// SetAPIServer names the API server the watcher lists from, as the
// kubeconfig's server URL. When its host is an IP, the Local Network hint
// falls back to it for a failed list whose error names no dialed address (see
// localNetworkBlockAddr). Call it before Watch.
func (w *InferenceServiceWatcher) SetAPIServer(server string) {
	w.apiHost = apiServerHost(server)
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
// after its initial list succeeded, so an initial list that had to be retried
// still starts from the seed.
func (w *InferenceServiceWatcher) initialSeen() map[string]string {
	seen := make(map[string]string, len(w.seeded))
	for key := range w.seeded {
		seen[key] = ""
	}
	w.seeded = nil
	return seen
}

// Watch lists the InferenceServices, emitting Created for each one it claims,
// then polls for changes until ctx ends. A failed list never ends it (#2079):
// the initial list is retried until it succeeds (an agent started during an
// outage, or before the CRDs are installed, waits there), and a failed poll
// changes nothing, so every service keeps its last-known state and nothing is
// treated as deleted. After maxConsecutiveFailures consecutive failures the
// API counts as unreachable: that is logged once, the retries back off (see
// nextPollDelay), and the first successful list logs the recovery once and
// carries on from the services seen before the outage.
func (w *InferenceServiceWatcher) Watch(ctx context.Context, eventChan chan<- InferenceServiceEvent) {
	for w.listExisting(ctx, eventChan) != nil {
		if !w.sleep(ctx, w.nextPollDelay()) {
			return
		}
	}

	// Store last seen resource versions to detect changes
	seen := w.initialSeen()
	for w.sleep(ctx, w.nextPollDelay()) {
		// A failed poll changes nothing; list has recorded the failure.
		_ = w.poll(ctx, eventChan, seen)
	}
}

// list lists the InferenceServices in the watcher's namespace and records the
// outcome as soon as the List returns: the recovery (and the heartbeat it
// wakes) must not wait for the list's events to be handled. A List that failed
// because ctx ended records nothing: the agent is shutting down, the API is
// not at fault.
func (w *InferenceServiceWatcher) list(ctx context.Context) (*inferencev1alpha1.InferenceServiceList, error) {
	list := &inferencev1alpha1.InferenceServiceList{}
	opts := []client.ListOption{}
	if w.namespace != "" {
		opts = append(opts, client.InNamespace(w.namespace))
	}
	if err := w.client.List(ctx, list, opts...); err != nil {
		if ctx.Err() == nil {
			w.noteListFailed(err)
		}
		return nil, err
	}
	w.noteListSucceeded()
	return list, nil
}

// sleep waits d and reports true, or reports false as soon as ctx ends.
func (w *InferenceServiceWatcher) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// threshold is the effective maxConsecutiveFailures.
func (w *InferenceServiceWatcher) threshold() int {
	if w.maxConsecutiveFailures < 1 {
		return DefaultMaxConsecutiveFailures
	}
	return w.maxConsecutiveFailures
}

// nextPollDelay is how long Watch waits before its next list. It is the poll
// interval until the API counts as unreachable. From the failure that makes it
// so, the delay doubles per failure from twice the interval up to
// maxPollBackoff (10 s, 20 s, then 30 s at the defaults), plus up to
// pollBackoffJitter of itself.
func (w *InferenceServiceWatcher) nextPollDelay() time.Duration {
	interval := w.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	past := w.listFailures - w.threshold()
	if past < 0 {
		return interval
	}
	limit := w.maxPollBackoff
	if limit <= 0 {
		limit = defaultMaxPollBackoff
	}
	limit = max(limit, interval)
	d := interval
	for i := 0; i <= past && d < limit; i++ {
		d *= 2
	}
	return wait.Jitter(min(d, limit), pollBackoffJitter)
}

// noteListFailed counts a failed list. Below the threshold each failure is
// logged at Warn, as a transient blip may be; the failure that reaches it
// marks the API unreachable with one Warn line, and later ones log at Debug.
// From then on every failure runs the reset hook before the retry.
func (w *InferenceServiceWatcher) noteListFailed(err error) {
	if w.listFailures == 0 {
		w.failingSince = w.clock()
	}
	w.listFailures++
	watchConsecutiveFailures.Set(float64(w.listFailures))
	threshold := w.threshold()
	switch {
	case w.listFailures < threshold:
		w.logger.Warnw("polling error",
			"error", err,
			"consecutiveFailures", w.listFailures,
			"threshold", threshold,
		)
	case !w.unreachable:
		w.unreachable = true
		watchAPIUnreachable.Set(1)
		w.logger.Warnw("Kubernetes API unreachable: InferenceService lists keep failing; "+
			"running engines keep serving with their last-known state and relay tokens, "+
			"and spec changes and deletions wait until a list succeeds; retrying with backoff",
			"error", err,
			"consecutiveFailures", w.listFailures,
			"threshold", threshold,
		)
	default:
		w.logger.Debugw("Kubernetes API still unreachable",
			"error", err,
			"consecutiveFailures", w.listFailures,
			"failingFor", w.clock().Sub(w.failingSince).Round(time.Second),
		)
	}
	if w.unreachable {
		w.noteLocalNetworkBlock(err)
	}
	if w.unreachable && w.resetConnections != nil {
		w.resetConnections()
	}
}

// noteLocalNetworkBlock checks a list that failed while the API counts as
// unreachable for the signature of a macOS Local Network block (#2422): the
// gauge follows the latest failure, and the first failure in an outage that
// matches logs the hint. Only on macOS: elsewhere the same errno is an
// ordinary routing failure, with no permission behind it.
func (w *InferenceServiceWatcher) noteLocalNetworkBlock(err error) {
	if w.goos != "darwin" {
		return
	}
	addr, blocked := localNetworkBlockAddr(err, w.apiHost)
	if !blocked {
		watchLocalNetworkBlocked.Set(0)
		return
	}
	watchLocalNetworkBlocked.Set(1)
	if w.localNetworkHinted {
		return
	}
	w.localNetworkHinted = true
	w.logger.Warnw("macOS Local Network access may be blocking the agent: the Kubernetes API server is at a "+
		"local-network address and connections to it fail with no route to host or network unreachable, "+
		"which is how macOS refuses a process without that access, even while Terminal reaches the same "+
		"address. Allow the agent in "+localNetworkSettings+"; macOS drops that access whenever the agent "+
		"binary changes, so allow it again after every agent upgrade",
		"address", addr.String(),
		"error", err,
	)
}

// noteListSucceeded clears the failure count after a successful list, logging
// the recovery once at Info and, when the API counted as unreachable, running
// the reachable hook.
func (w *InferenceServiceWatcher) noteListSucceeded() {
	if w.listFailures == 0 {
		return
	}
	failures, failingFor := w.listFailures, w.clock().Sub(w.failingSince).Round(time.Second)
	wasUnreachable := w.unreachable
	w.listFailures, w.failingSince, w.unreachable, w.localNetworkHinted = 0, time.Time{}, false, false
	watchConsecutiveFailures.Set(0)
	if !wasUnreachable {
		w.logger.Infow("polling recovered after errors", "previousFailures", failures)
		return
	}
	watchAPIUnreachable.Set(0)
	watchLocalNetworkBlocked.Set(0)
	w.logger.Infow("Kubernetes API reachable again; resuming",
		"failedLists", failures, "unreachableFor", failingFor)
	if w.onReachable != nil {
		w.onReachable()
	}
}

// listExisting lists all existing InferenceServices and sends create events
func (w *InferenceServiceWatcher) listExisting(ctx context.Context, eventChan chan<- InferenceServiceEvent) error {
	list, err := w.list(ctx)
	if err != nil {
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
	list, err := w.list(ctx)
	if err != nil {
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
		requeued := w.takeRequeue(key)

		if !exists {
			// New service
			eventChan <- InferenceServiceEvent{
				Type:             EventTypeCreated,
				InferenceService: isvc,
			}
			seen[key] = currentVersion
		} else if lastVersion != currentVersion || requeued {
			// Updated service, or one the agent asked to see again
			eventChan <- InferenceServiceEvent{
				Type:             EventTypeUpdated,
				InferenceService: isvc,
			}
			seen[key] = currentVersion
		}
	}

	w.forgetVanishedLookupFailures(current)
	w.forgetVanishedRequeues(current)

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

// Requeue makes the next poll deliver key as Updated even when its
// resourceVersion has not changed. The agent calls it after a start it
// deferred without writing anything to the InferenceService (a remote size
// probe that failed transiently, #2326): with no write the resourceVersion
// stays put, and the poll alone would never deliver the service again.
func (w *InferenceServiceWatcher) Requeue(key string) {
	w.RequeueAt(key, time.Time{})
}

// RequeueAt is Requeue for the first poll at or after at. The agent uses it
// to re-check a refused service on a backoff (#2329). A key already queued
// keeps the earlier of the two times.
func (w *InferenceServiceWatcher) RequeueAt(key string, at time.Time) {
	w.requeueMu.Lock()
	defer w.requeueMu.Unlock()
	if w.requeued == nil {
		w.requeued = make(map[string]time.Time)
	}
	if cur, ok := w.requeued[key]; ok && cur.Before(at) {
		return
	}
	w.requeued[key] = at
}

// takeRequeue reports whether key is queued and due, and clears a due
// request, so a requeue delivers the service once.
func (w *InferenceServiceWatcher) takeRequeue(key string) bool {
	w.requeueMu.Lock()
	defer w.requeueMu.Unlock()
	at, ok := w.requeued[key]
	if !ok || w.clock().Before(at) {
		return false
	}
	delete(w.requeued, key)
	return true
}

// clock returns w.now(), or the wall clock for a watcher built as a struct
// literal.
func (w *InferenceServiceWatcher) clock() time.Time {
	if w.now == nil {
		return time.Now()
	}
	return w.now()
}

// forgetVanishedRequeues drops the requeue requests of services this poll did
// not list, or found are not ours: there is nothing left to deliver.
func (w *InferenceServiceWatcher) forgetVanishedRequeues(current map[string]bool) {
	w.requeueMu.Lock()
	defer w.requeueMu.Unlock()
	for key := range w.requeued {
		if !current[key] {
			delete(w.requeued, key)
		}
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
