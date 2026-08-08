/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

// ErrHoldBudgetExceeded is returned by Activator.Acquire when the caller's
// hold budget elapses before its target member became resident. The proxy
// maps it to a 503 + Retry-After (the standards-correct "temporarily
// unavailable" signal), never a 429.
var ErrHoldBudgetExceeded = errors.New("model pool activation hold budget exceeded")

// MemberController is the slice of Kubernetes operations the Activator needs to
// drive a ModelPool swap. It is an interface so the swap policy is unit-testable
// without a live cluster: tests inject a fake, production injects the
// controller-runtime-backed implementation in activator_k8s.go.
type MemberController interface {
	// Activate commits a swap by making isvc the pool's desired owner (it
	// scales the member to one replica). The ModelPoolReconciler drains and
	// unloads the incumbent before the target loads. Activate must be
	// idempotent.
	Activate(ctx context.Context, namespace, isvc string) error

	// Deactivate cancels a pending activation by scaling isvc back to zero
	// replicas. Called when a held request gives up (budget exceeded or client
	// disconnect) and no other caller still wants the member, so the pool does
	// not complete a useless swap. Idempotent; scaling an already-zero member is
	// a no-op.
	Deactivate(ctx context.Context, namespace, isvc string) error

	// WaitReady blocks until isvc reports the Ready phase or ctx is done.
	WaitReady(ctx context.Context, namespace, isvc string) error

	// Phase returns the member InferenceService's current status.phase, used to
	// seed which member is resident on a cold proxy start.
	Phase(ctx context.Context, namespace, isvc string) (string, error)
}

// Activator owns the cross-pod sticky swap policy for every ModelPool the proxy
// fronts. It holds cross-model requests open while the incumbent drains and the
// target loads (the activator pattern) and coalesces same-model demand to avoid
// GPU ping-pong: a cross-model request flips the slot only once the incumbent
// is fully idle. All swaps for a pool are serialized so at most one member is
// ever made resident at a time, mirroring the controller-side exclusive-slot
// invariant from the request path.
type Activator struct {
	ctrl    MemberController
	logger  *slog.Logger
	router  string
	baseCtx context.Context

	// resyncInterval bounds how stale the activator's belief about a pool's
	// resident member may be. reconcileResident re-reads member phase at most
	// once per interval per pool (the proxy's Kubernetes client is direct, so a
	// re-read is a live API call), trading a small self-heal latency for not
	// adding an API round-trip to the hot path.
	resyncInterval time.Duration

	mu    sync.Mutex
	pools map[string]*poolRuntime
}

// poolRuntime is the per-pool swap state, guarded by Activator.mu.
type poolRuntime struct {
	namespace string
	pool      string
	members   []string

	// resident is the member the activator believes owns the slot, or "" when
	// the pool is cold. residentCheckedAt is when reconcileResident last verified
	// that belief against the members' actual InferenceService phase; the zero
	// value means never (a cold pool, or a belief invalidated by the
	// dispatch-failure self-heal), which forces the next Acquire to re-verify.
	resident          string
	residentCheckedAt time.Time

	// swapping is true while a background goroutine is draining + loading a new
	// owner; swapTarget names that owner. All callers wait until the swap
	// completes so no request is dispatched to a member that is being torn down.
	// The swap runs under the Activator's baseCtx, not any caller context, so a
	// client disconnecting mid-swap never aborts an in-progress load.
	// swapCancel cancels the in-flight swap when the last caller for the target
	// gives up (cancel-on-timeout).
	swapping   bool
	swapTarget string
	swapCancel context.CancelFunc

	// swapErr records the outcome of the most recent failed swap, keyed by the
	// target member, so the waiter that requested that member surfaces the
	// error instead of hanging. Cleared when taken.
	swapErr map[string]error

	// inflight counts in-flight blocking requests per member. waiting counts
	// callers currently held open for a member that is not yet resident.
	inflight map[string]int
	waiting  map[string]int

	// change is closed and replaced on every state transition so waiters can
	// re-evaluate without a busy loop. Guarded by Activator.mu.
	change chan struct{}
}

// NewActivator constructs an Activator. baseCtx bounds swap operations
// (activate + wait-ready) and must outlive individual requests: a caller
// disconnecting mid-swap must not abort an in-progress load, so the swap runs
// under baseCtx, not the request context.
func NewActivator(baseCtx context.Context, ctrl MemberController, router string, logger *slog.Logger) *Activator {
	if logger == nil {
		logger = slog.Default()
	}
	if router == "" {
		router = "default"
	}
	return &Activator{
		ctrl:           ctrl,
		logger:         logger,
		router:         router,
		baseCtx:        baseCtx,
		resyncInterval: residentResyncInterval,
		pools:          make(map[string]*poolRuntime),
	}
}

func (a *Activator) runtime(p *BackendPool) *poolRuntime {
	key := p.Namespace + "/" + p.Name
	pr, ok := a.pools[key]
	if !ok {
		pr = &poolRuntime{
			namespace: p.Namespace,
			pool:      p.Name,
			members:   append([]string(nil), p.Members...),
			swapErr:   make(map[string]error),
			inflight:  make(map[string]int),
			waiting:   make(map[string]int),
			change:    make(chan struct{}),
		}
		a.pools[key] = pr
	}
	return pr
}

// notify wakes every waiter so it re-evaluates the pool state. Caller holds mu.
func (pr *poolRuntime) notify() {
	close(pr.change)
	pr.change = make(chan struct{})
}

// Acquire makes member the resident owner of its pool slot before the caller
// dispatches, holding the request open (coalescing behind same-model demand and
// draining the incumbent) until the swap is safe. It returns a release func the
// caller must invoke exactly once when the request completes. A non-nil error
// (ErrHoldBudgetExceeded or a context error) means the caller should not
// dispatch; an in-progress load is never aborted by a caller giving up.
func (a *Activator) Acquire(ctx context.Context, p *BackendPool) (func(), error) {
	member := p.Member

	a.mu.Lock()
	pr := a.runtime(p)
	if err := a.reconcileResident(ctx, pr, p.Members); err != nil {
		// Reconciliation is best-effort: a failed status read just means we keep
		// the current belief (or treat the pool as cold) and let the next swap or
		// resync converge residency.
		a.logger.Warn("model pool resident reconcile failed", "pool", pr.pool, "error", err)
	}

	pr.waiting[member]++
	a.observeWaiting(pr, member)
	holdStart := time.Now()

	release := func() {
		a.mu.Lock()
		if pr.inflight[member] > 0 {
			pr.inflight[member]--
		}
		if pr.inflight[member] == 0 {
			// The resident just went idle; wake waiters so a queued cross-model
			// request can flip the slot.
			pr.notify()
		}
		a.mu.Unlock()
	}

	defer func() {
		a.mu.Lock()
		pr.waiting[member]--
		a.observeWaiting(pr, member)
		// Cancel-on-timeout: if this was the last caller waiting for member and
		// member never became resident, cancel a pending activation so the pool
		// does not complete a useless swap and leave a member scaled up that
		// nobody is using. Cancelling only ever scales the target back down; the
		// controller drains the incumbent solely when idle, so this can never
		// produce a two-resident state.
		if pr.waiting[member] == 0 && pr.resident != member {
			if pr.swapping && pr.swapTarget == member && pr.swapCancel != nil {
				pr.swapCancel()
			}
			ns := pr.namespace
			ctrl := a.ctrl
			a.mu.Unlock()
			if ctrl != nil {
				go func() { _ = ctrl.Deactivate(a.baseCtx, ns, member) }()
			}
			return
		}
		a.mu.Unlock()
	}()

	for {
		// A swap this caller requested may have failed; surface it instead of
		// waiting forever.
		if err := pr.swapErr[member]; err != nil {
			delete(pr.swapErr, member)
			a.mu.Unlock()
			return nil, err
		}

		// Fast path: our member already owns the slot and no swap is unwinding
		// it. Coalesce onto it: same-model demand is always served on the warm
		// member and never triggers a swap. A cross-model request cannot preempt
		// the resident while it is busy; the flip happens only once the resident
		// is fully idle (the swap branch below).
		if !pr.swapping && pr.resident == member {
			pr.inflight[member]++
			prommetrics.ModelPoolCoalescedTotal.WithLabelValues(a.router, pr.pool, member).Inc()
			a.mu.Unlock()
			return release, nil
		}

		// Start a swap to our member when none is running and the incumbent is
		// idle (drain-before-swap; a busy incumbent is never preempted). The
		// swap itself runs in the background under baseCtx; this caller just
		// waits for it.
		if !pr.swapping && pr.resident != member {
			incumbent := pr.resident
			if incumbent == "" || pr.inflight[incumbent] == 0 {
				a.startSwap(pr, incumbent, member, holdStart)
			}
		}

		// Wait for a state change, bounded by the caller's hold budget (its
		// context deadline). A client disconnect (ctx canceled) drops this held
		// request without aborting any in-progress load.
		ch := pr.change
		a.mu.Unlock()
		select {
		case <-ch:
			a.mu.Lock()
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrHoldBudgetExceeded
			}
			return nil, ctx.Err()
		}
	}
}

// startSwap launches a background goroutine that drains the incumbent and loads
// target under the Activator's baseCtx, then publishes the result. Caller holds
// mu; startSwap sets the swapping flag and returns immediately so the caller can
// wait on the change channel. Running the swap off the caller goroutine is what
// lets a caller give up (budget exceeded / disconnect) without aborting the
// in-progress model load.
func (a *Activator) startSwap(pr *poolRuntime, incumbent, target string, holdStart time.Time) {
	swapCtx, cancel := context.WithCancel(a.baseCtx)
	pr.swapping = true
	pr.swapTarget = target
	pr.swapCancel = cancel
	delete(pr.swapErr, target)
	pr.notify()

	go func() {
		swapStart := time.Now()
		err := a.ctrl.Activate(swapCtx, pr.namespace, target)
		if err == nil {
			err = a.ctrl.WaitReady(swapCtx, pr.namespace, target)
		}

		a.mu.Lock()
		defer a.mu.Unlock()
		pr.swapping = false
		pr.swapTarget = ""
		pr.swapCancel = nil
		if err != nil {
			// A cancelled swap (last caller gave up) is not a failure: the
			// target was scaled back down by the cancel path, so just clear the
			// swap state and wake any waiters.
			if swapCtx.Err() != nil {
				pr.notify()
				return
			}
			a.logger.Error("model pool swap failed", "pool", pr.pool,
				"from", incumbent, "to", target, "error", err)
			pr.swapErr[target] = errors.New("model pool swap failed for member " + target + ": " + err.Error())
			pr.notify()
			return
		}
		pr.resident = target
		// The swap just made target Ready, so the belief is fresh: record the
		// check so reconcileResident does not immediately re-read its phase.
		pr.residentCheckedAt = time.Now()
		prommetrics.ModelPoolSwapsTotal.WithLabelValues(a.router, pr.pool, incumbent, target).Inc()
		prommetrics.ModelPoolSwapDuration.WithLabelValues(a.router, pr.pool).Observe(time.Since(swapStart).Seconds())
		prommetrics.ModelPoolHoldDuration.WithLabelValues(a.router, pr.pool, target).Observe(time.Since(holdStart).Seconds())
		a.observeResident(pr)
		pr.notify()
	}()
}

// reconcileResident refreshes the activator's belief about which member owns a
// pool's slot against the members' actual InferenceService phase, the truth the
// controller publishes. It subsumes the cold-start seed (spec.default warmed by
// the controller) and, crucially, self-heals after an out-of-band residency
// change the activator did not drive: a resident pod rolled by a spec edit, an
// OOM-kill, a node drain, or a controller fallback to spec.default. Without it
// the fast path would keep dispatching to a member whose pod is gone, returning
// instant 502s until the proxy restarts.
//
// The proxy's Kubernetes client is direct (uncached), so a phase read is a live
// API call; reconcileResident therefore verifies at most once per resyncInterval
// per pool. A zero residentCheckedAt (a cold pool, or a belief invalidated by
// InvalidateResident after a dispatch failure) forces an immediate recheck.
// Caller holds mu.
func (a *Activator) reconcileResident(ctx context.Context, pr *poolRuntime, members []string) error {
	// A swap the activator itself is driving owns pr.resident; its goroutine
	// publishes the new owner on completion, so never second-guess it mid-flight.
	if pr.swapping {
		return nil
	}
	// Trust the current belief between rechecks so a busy resident does not incur
	// an API call per request.
	if !pr.residentCheckedAt.IsZero() && time.Since(pr.residentCheckedAt) < a.resyncInterval {
		return nil
	}
	pr.residentCheckedAt = time.Now()

	// Verify the believed resident still owns the slot. A resident whose pod is
	// gone (phase != Ready) is stale; drop the belief so it is re-derived below
	// and the next request drives a corrective swap.
	if pr.resident != "" {
		phase, err := a.ctrl.Phase(ctx, pr.namespace, pr.resident)
		if err != nil {
			return err
		}
		if phase == modelReadyPhase {
			return nil
		}
		pr.resident = ""
	}
	// No trusted resident: adopt whichever member the controller currently
	// reports Ready (for example spec.default warmed after a fallback) so a
	// request for it coalesces instead of needlessly swapping.
	for _, m := range members {
		phase, err := a.ctrl.Phase(ctx, pr.namespace, m)
		if err != nil {
			return err
		}
		if phase == modelReadyPhase {
			pr.resident = m
			break
		}
	}
	a.observeResident(pr)
	return nil
}

// InvalidateResident forces the next Acquire for p's pool to re-verify which
// member owns the slot instead of trusting the cached belief. The proxy calls
// it when a dispatch to a pooled backend fails at the connection level: a
// believed-resident member that refuses connections has almost certainly lost
// its pod out of band, so the belief must be rechecked immediately rather than
// waiting up to resyncInterval for the periodic recheck. A swap the activator
// is driving is left untouched; its goroutine owns residency.
func (a *Activator) InvalidateResident(p *BackendPool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pr, ok := a.pools[p.Namespace+"/"+p.Name]
	if !ok || pr.swapping {
		return
	}
	pr.residentCheckedAt = time.Time{}
}

// residentResyncInterval bounds how long a pool may serve a stale resident
// belief before reconcileResident re-verifies it. Kept short so an out-of-band
// residency change self-heals quickly while still coalescing the common
// same-member request stream onto one API read per interval.
const residentResyncInterval = 5 * time.Second

// modelReadyPhase is the InferenceService status.phase value that means a member
// is serving. Duplicated here (rather than importing the controller package) to
// keep the proxy free of controller/kubebuilder deps in the request path.
const modelReadyPhase = "Ready"

func (a *Activator) observeWaiting(pr *poolRuntime, member string) {
	prommetrics.ModelPoolHeldRequests.WithLabelValues(a.router, pr.pool, member).Set(float64(pr.waiting[member]))
}

func (a *Activator) observeResident(pr *poolRuntime) {
	for _, m := range pr.members {
		val := 0.0
		if m == pr.resident {
			val = 1.0
		}
		prommetrics.ModelPoolResident.WithLabelValues(pr.namespace, pr.pool, m).Set(val)
	}
	if pr.resident != "" {
		prommetrics.ModelPoolResident.WithLabelValues(pr.namespace, pr.pool, pr.resident).Set(1.0)
	}
}
