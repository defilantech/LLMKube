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

package controller

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

// modelPoolSwapRequeue is how long the reconciler waits before re-checking a
// pool that is mid-swap (draining an incumbent before loading the target). The
// member InferenceService watch also re-triggers reconciliation when a drained
// member reaches Stopped, so this is a backstop, not the primary signal.
const modelPoolSwapRequeue = 3 * time.Second

// modelPoolReclaimPoll bounds how often the reclaim policy re-probes the
// resident member's idle state. A request completing does not change the member
// InferenceService or its Pods, so neither watch fires when a busy resident goes
// idle; the reconciler must poll to notice it. Capped low enough that reclaim
// fires promptly after reclaimAfter elapses, high enough not to hammer /slots.
const modelPoolReclaimPoll = 30 * time.Second

// defaultReclaimAfter is used when SwapPolicy is "reclaim" but spec.reclaimAfter
// is unset (the CRD default also supplies 300s; this guards direct API writes
// and unit construction).
const defaultReclaimAfter = 300 * time.Second

// ModelPoolReconciler enforces the exclusive-slot invariant for a ModelPool:
// at most one member InferenceService is resident (Ready) at a time, and the
// incumbent is fully drained and unloaded (VRAM freed) before the next member
// loads. The router-proxy commits a swap decision by scaling a member's
// spec.replicas to 1; this reconciler makes that member the sole slot owner.
//
// Drain-before-unload reuses the same /slots idleness contract as the
// InferenceService rollout drain (#1088): a busy incumbent is never scaled down,
// and an idle check that cannot establish idleness fails closed (incumbent stays
// resident). For a member that reports Ready, a refused probe is not ambiguity:
// it is decided together with the member's Pod state, so a member that is
// terminating, has no running container, or has a single Pod no longer listening
// is drained instead of holding the slot. A member that does not report Ready is
// not probed at all. Unlike a rollout, there is no idleTimeout force branch: the
// reconciler never force-unloads a resident member on a timer. The bound on a
// waiting request is the router's hold budget (503 + Retry-After), not a
// controller-side force.
type ModelPoolReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   events.EventRecorder
	HTTPClient *http.Client

	// APIReader reads the ModelPool uncached. status.pendingMember is the only
	// record of an activation while its member is held at zero replicas, so a
	// reconcile that read it from a cache lagging the previous status patch
	// would hand the slot back to the incumbent (#2240). Nil falls back to the
	// cached client.
	APIReader client.Reader

	// RolloutIdleBaseURL overrides the member /slots base URL. Used in tests to
	// point the idle check at an httptest server; empty in production (the URL
	// is derived from the member Service).
	RolloutIdleBaseURL string

	// IdleCheck, when set, replaces the /slots HTTP probe. Tests inject a fake
	// keyed by member name so swap ordering is deterministic without a live
	// llama-server. Nil in production (the real /slots probe is used).
	IdleCheck func(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (bool, error)
}

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=modelpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=modelpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=modelpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices/status,verbs=get
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models,verbs=get;list;watch

func (r *ModelPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pool := &inferencev1alpha1.ModelPool{}
	if err := r.poolReader().Get(ctx, req.NamespacedName, pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve every member InferenceService. A missing reference is a spec
	// error the operator must fix; the pool is Degraded until then and the
	// reconciler makes no replica changes (it will not scale-to-zero a live
	// member just because a sibling reference is broken).
	members, missing, err := r.resolveMembers(ctx, pool)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(missing) > 0 {
		return r.markDegraded(ctx, pool, members, missing)
	}
	if autoscaled := autoscaledMembers(pool, members); len(autoscaled) > 0 {
		return r.markInvalidMembers(ctx, pool, members, autoscaled, "MemberAutoscaling", "ModelPool members must not set spec.autoscaling: %v")
	}

	// Metal members have no device-plugin GPU gating, so the metal-agent (not
	// this reconciler) would have to enforce unload-before-load. That is a
	// follow-up; v1 is k8s-GPU-only. Detect metal-backed members and refuse to
	// manage the pool rather than silently risk two-resident on a Mac host.
	if metalMembers, err := r.metalMembers(ctx, pool, members); err != nil {
		return ctrl.Result{}, err
	} else if len(metalMembers) > 0 {
		return r.markMetalUnsupported(ctx, pool, members, metalMembers)
	}

	owner := resolveSlotOwner(pool, members)

	// Capture the status baseline before applyReclaimOwner mutates
	// status.ResidentIdleSince. updateStatus patches with MergeFrom against this
	// baseline; taken after the mutation it would diff the idle clock away, so
	// the clock would never persist and the reclaim would never fire.
	statusBefore := pool.DeepCopy()
	// After the baseline, before the reclaim logic reads the resident: a
	// departed resident must not be named, and must not hold a reclaim.
	clearDepartedResident(pool, members)

	// Reclaim policy: once the resident non-default member has been idle for
	// spec.reclaimAfter, return the slot to spec.default. This overrides the
	// desired owner and then rides the ordinary drain-and-swap machinery below
	// (the idle resident is drained like any displaced incumbent). reclaimRequeue
	// keeps the reconciler polling idle state, which no InferenceService or Pod
	// event would otherwise surface.
	restartIdleClockOnReactivation(pool, members, owner)
	owner, reclaimRequeue := r.applyReclaimOwner(ctx, pool, members, owner)

	// Drain every non-owner. A non-owner that reports Ready could still be
	// serving, so it is the incumbent being displaced: idle-gate it through
	// memberIdleVerdict, which asks the shared /slots probe first and reads the
	// member's Pods only when the probe is refused, so an in-flight request
	// finishes before its VRAM is freed. A member that does not report Ready is
	// not serving at all (llama.cpp answers 503 on every path while it loads,
	// and a terminated one has no listener), so probing it could only establish
	// nothing and would hold the incumbent. A busy incumbent, or one whose
	// idleness cannot be established, is NEVER scaled down (fail-closed, no
	// force-unload); the swap simply defers.
	//
	// othersClear tracks whether every non-owner has fully released the GPU. It
	// gates on the incumbent's Pods being *gone*, not merely on the Stopped
	// phase: on a unified-memory node the device plugin frees the gpu:1 resource
	// when the Pod terminates, but the actual allocation is only released when
	// the incumbent's process exits. Gating the owner's start on Pod
	// termination (below) closes the reclaim-lag window where the scheduler
	// could otherwise admit the owner before the incumbent's process is gone and
	// briefly co-reside (OOM).
	deferReason := ""
	othersClear := true
	for _, name := range memberNames(pool) {
		if name == owner {
			continue
		}
		isvc, ok := members[name]
		if !ok {
			continue
		}
		// One Pod list answers both questions below: a Terminating Pod counts as
		// present for othersClear (its allocation is not released yet) and is
		// not serving for the verdict.
		pods, perr := r.memberPods(ctx, isvc)
		if perr != nil {
			return ctrl.Result{}, perr
		}
		if memberReady(isvc) {
			switch r.memberIdleVerdict(ctx, isvc, pods) {
			case idleVerdictIdle:
			case idleVerdictBusy:
				// Busy: let the in-flight request finish; do not unload.
				deferReason = inferencev1alpha1.ReasonPodsBusy
				othersClear = false
				continue
			default:
				// Fail closed: keep the incumbent resident.
				log.Info("member idleness unestablished; deferring swap", "member", name)
				deferReason = inferencev1alpha1.ReasonIdleCheckFailed
				othersClear = false
				continue
			}
		}
		if err := r.drainMember(ctx, isvc); err != nil {
			return ctrl.Result{}, err
		}
		if len(pods) > 0 {
			// Incumbent scaled to zero but its Pods (and the GPU/unified-memory
			// allocation) are not yet released; the owner must wait so the two
			// never co-reside.
			othersClear = false
		}
	}

	// The owner is held at zero replicas until every incumbent's Pods are gone.
	// Only then is it scaled to one, so its Pod cannot start before the
	// incumbent's process (and its allocation) is released. This is a
	// controller-side gate rather than pure scheduler delegation, because on a
	// unified-memory node the device plugin can admit the owner in the
	// reclaim-lag window between the incumbent Pod terminating and its process
	// exiting.
	swapping := deferReason != ""
	if owner != "" {
		desired := int32(0)
		if othersClear {
			desired = 1
		} else {
			swapping = true
		}
		if desired == 0 && replicasOf(members[owner]) >= 1 {
			if err := r.recordPendingOwner(ctx, pool, owner); err != nil {
				return ctrl.Result{}, err
			}
		}
		if err := r.scaleOwner(ctx, statusBefore, members, owner, desired); err != nil {
			return ctrl.Result{}, err
		}
		if !memberReady(members[owner]) {
			swapping = true
		}
	}

	result := ctrl.Result{}
	if swapping || (owner != "" && !memberReady(members[owner])) {
		result.RequeueAfter = modelPoolSwapRequeue
	}
	// The reclaim poll is a low-frequency heartbeat; the swap requeue (when a
	// swap is in flight) is shorter and wins.
	if reclaimRequeue > 0 && (result.RequeueAfter == 0 || reclaimRequeue < result.RequeueAfter) {
		result.RequeueAfter = reclaimRequeue
	}
	if err := r.updateStatus(ctx, pool, statusBefore, members, owner, swapping, deferReason); err != nil {
		if apierrors.IsConflict(err) {
			//nolint:staticcheck // SA1019: keep the rate-limited retry on a write conflict; RequeueAfter would swap backoff for a fixed delay.
			return ctrl.Result{Requeue: true}, nil
		}
		log.Error(err, "update ModelPool status")
		return ctrl.Result{}, err
	}
	return result, nil
}

// resolveMembers loads the member InferenceServices, returning the resolved set
// keyed by name and the list of member names whose reference does not resolve.
func (r *ModelPoolReconciler) resolveMembers(ctx context.Context, pool *inferencev1alpha1.ModelPool) (map[string]*inferencev1alpha1.InferenceService, []string, error) {
	members := make(map[string]*inferencev1alpha1.InferenceService, len(pool.Spec.Members))
	var missing []string
	for _, m := range pool.Spec.Members {
		name := m.InferenceServiceRef.Name
		if name == "" {
			continue
		}
		isvc := &inferencev1alpha1.InferenceService{}
		err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: name}, isvc)
		switch {
		case apierrors.IsNotFound(err):
			missing = append(missing, name)
		case err != nil:
			return nil, nil, err
		default:
			members[name] = isvc
		}
	}
	return members, missing, nil
}

// autoscaledMembers returns members whose replica count is owned by an HPA.
// ModelPool must own that count to drain a member before loading another.
func autoscaledMembers(pool *inferencev1alpha1.ModelPool, members map[string]*inferencev1alpha1.InferenceService) []string {
	var names []string
	for _, m := range pool.Spec.Members {
		name := m.InferenceServiceRef.Name
		if isvc, ok := members[name]; ok && isvc.Spec.Autoscaling != nil {
			names = append(names, name)
		}
	}
	return names
}

// resolveSlotOwner decides which member should own the shared GPU slot. The
// anti-thrash policy (coalesce until the incumbent is idle) lives in the
// router-proxy, which commits a flip by scaling its chosen member's
// spec.replicas to 1. This reconciler treats a scaled-up member that is not the
// current resident as a fresh activation and makes it the desired owner.
//
// Ownership cannot be inferred from replica counts alone: during a swap the
// reconciler holds the incoming owner at replicas 0 (until the incumbent's Pods
// are gone), which would erase the activation signal. The in-progress owner is
// therefore remembered in status.PendingMember and honored here even while it is
// held at zero, so a deferred swap does not revert to the incumbent.
func resolveSlotOwner(pool *inferencev1alpha1.ModelPool, members map[string]*inferencev1alpha1.InferenceService) string {
	resident := pool.Status.ResidentMember
	pending := pool.Status.PendingMember

	// A resident that left spec.members takes its residency with it: the pool
	// no longer resolves it, so members[resident] would be nil downstream, and
	// the slot resolves the way a cold pool resolves it (#2309).
	if _, ok := members[resident]; !ok && resident != "" {
		resident = ""
	}

	// requested = members currently scaled up, in declaration order.
	var requested []string
	for _, m := range pool.Spec.Members {
		name := m.InferenceServiceRef.Name
		if isvc, ok := members[name]; ok && replicasOf(isvc) >= 1 {
			requested = append(requested, name)
		}
	}
	// newly = scaled-up members other than the current resident: a fresh proxy
	// activation that wants the slot. The resident counts too once this
	// reconciler has drained it and something scaled it back up: at replicas 1
	// it otherwise looks like steady state, and a pending swap away from it
	// (a reclaim) would strand the request that re-activated it (#1942).
	var newly []string
	for _, name := range requested {
		if name != resident || drainedByPool(members[name]) {
			newly = append(newly, name)
		}
	}

	if resident == "" {
		// Cold pool (no member has become resident yet).
		// Continue an in-progress swap first, then honor a fresh activation,
		// then spec.default, then declaration order, else stay cold.
		if pending != "" {
			if _, ok := members[pending]; ok {
				return pending
			}
		}
		if len(requested) == 0 {
			if def := pool.Spec.Default; def != "" {
				if _, ok := members[def]; ok {
					return def
				}
			}
			return ""
		}
		if def := pool.Spec.Default; def != "" {
			for _, name := range requested {
				if name == def {
					return def
				}
			}
		}
		return requested[0]
	}

	// Warm pool with a recorded resident.
	if len(newly) > 0 {
		// A fresh activation supersedes. Prefer the in-progress pending swap
		// when it is still among the scaled-up members, else the first newly
		// activated member.
		if pending != "" {
			for _, name := range newly {
				if name == pending {
					return pending
				}
			}
		}
		return newly[0]
	}
	// No fresh activation. Continue an in-progress swap (the pending owner is
	// held at zero during the incumbent drain), else keep the resident.
	if pending != "" {
		if _, ok := members[pending]; ok {
			return pending
		}
	}
	return resident
}

// clearDepartedResident drops a resident that left spec.members. Status is the
// only record of the old owner, and the pool can no longer drain or scale a
// member it does not list, so status stops naming it (#2309). The caller must
// already have captured its patch baseline: a baseline taken after this call
// diffs the clear away.
func clearDepartedResident(pool *inferencev1alpha1.ModelPool, members map[string]*inferencev1alpha1.InferenceService) {
	if pool.Status.ResidentMember == "" {
		return
	}
	if _, ok := members[pool.Status.ResidentMember]; !ok {
		pool.Status.ResidentMember = ""
	}
}

// applyReclaimOwner implements the "reclaim" swap policy. When a non-default
// member has held the slot and been continuously idle for spec.reclaimAfter, it
// overrides owner with spec.default so the ordinary drain-and-swap machinery
// returns the slot to the background model. It returns the (possibly overridden)
// owner and a requeue interval that keeps the reconciler polling idle state
// while a non-default member is resident (no watch fires on a busy->idle
// transition). Idle-probe failure is fail-closed: the clock holds its value and
// the slot is not reclaimed on that poll. The idle clock lives in
// status.ResidentIdleSince, which updateStatus persists untouched; it
// self-clears once the default is resident.
func (r *ModelPoolReconciler) applyReclaimOwner(
	ctx context.Context,
	pool *inferencev1alpha1.ModelPool,
	members map[string]*inferencev1alpha1.InferenceService,
	owner string,
) (string, time.Duration) {
	log := logf.FromContext(ctx)

	def := pool.Spec.Default
	if pool.Spec.SwapPolicy != inferencev1alpha1.ModelPoolSwapPolicyReclaim || def == "" {
		pool.Status.ResidentIdleSince = nil
		return owner, 0
	}

	// Reclaim only applies to a pool that is settled on a non-default resident:
	// resolveSlotOwner returned that same resident (no cross-model activation is
	// already redirecting the slot), the resident is Ready, and spec.default is
	// still a member to reclaim to. Any other shape means a real swap is already
	// in motion or there is nothing to reclaim.
	resident := pool.Status.ResidentMember
	if owner == "" || owner != resident || resident == def {
		pool.Status.ResidentIdleSince = nil
		return owner, 0
	}
	residentISVC, ok := members[resident]
	if !ok || !memberReady(residentISVC) {
		pool.Status.ResidentIdleSince = nil
		return owner, 0
	}
	if _, ok := members[def]; !ok {
		pool.Status.ResidentIdleSince = nil
		return owner, 0
	}

	idle, err := r.memberIdle(ctx, residentISVC)
	if err != nil {
		// Fail closed: an unestablished idle state never reclaims. The clock is
		// held rather than reset because this returns before the elapsed check,
		// so a reclaim still only fires on a reconcile whose probe answered
		// idle, and the drain that follows probes again before unloading.
		log.Info("reclaim idle check failed; holding idle clock", "member", resident, "error", err)
		return owner, modelPoolReclaimPoll
	}
	if !idle {
		// Busy: a request is in flight. Reset the clock and keep polling so the
		// next busy->idle transition is noticed (no watch fires for it).
		pool.Status.ResidentIdleSince = nil
		return owner, modelPoolReclaimPoll
	}

	reclaimAfter := reclaimAfterDuration(pool)
	now := time.Now()
	if pool.Status.ResidentIdleSince == nil {
		t := metav1.NewTime(now)
		pool.Status.ResidentIdleSince = &t
		return owner, pollWithin(reclaimAfter)
	}
	if elapsed := now.Sub(pool.Status.ResidentIdleSince.Time); elapsed >= reclaimAfter {
		log.Info("reclaiming pool slot to default after idle",
			"pool", pool.Name, "from", resident, "to", def, "idleFor", elapsed.String())
		llmkubemetrics.ModelPoolReclaimsTotal.WithLabelValues(pool.Namespace, pool.Name, resident, def).Inc()
		// Leave ResidentIdleSince set; it clears on the next reconcile once the
		// default is resident (owner == resident == def branch above).
		return def, 0
	}
	return owner, pollWithin(reclaimAfter)
}

// pollWithin returns how long to wait before re-checking a running idle clock:
// the shorter of the reclaim window and the poll cap, so a small reclaimAfter
// still fires promptly while a large one polls at a steady cadence.
func pollWithin(reclaimAfter time.Duration) time.Duration {
	if reclaimAfter < modelPoolReclaimPoll {
		return reclaimAfter
	}
	return modelPoolReclaimPoll
}

// reclaimAfterDuration is spec.reclaimAfter, or defaultReclaimAfter when unset
// or non-positive.
func reclaimAfterDuration(pool *inferencev1alpha1.ModelPool) time.Duration {
	if pool.Spec.ReclaimAfter != nil && pool.Spec.ReclaimAfter.Duration > 0 {
		return pool.Spec.ReclaimAfter.Duration
	}
	return defaultReclaimAfter
}

// ensureReplicas patches the member InferenceService's spec.replicas when it
// differs from the desired value. The reconciler is the sole owner of a pooled
// member's replica count; the proxy's scale-up is only the activation trigger.
// poolReader returns the uncached reader when one is wired, so resolveSlotOwner
// always sees the newest status.pendingMember.
func (r *ModelPoolReconciler) poolReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// recordPendingOwner persists owner as status.pendingMember before the caller
// holds it at zero replicas. Until then the owner's replica count is the only
// record of the activation, so erasing it first would lose the activation if
// this write failed (#2240). It patches a copy: pool carries status changes
// (the reclaim idle clock) that updateStatus diffs against its own baseline.
func (r *ModelPoolReconciler) recordPendingOwner(ctx context.Context, pool *inferencev1alpha1.ModelPool, owner string) error {
	if pool.Status.PendingMember == owner {
		return nil
	}
	recorded := pool.DeepCopy()
	recorded.Status.PendingMember = owner
	if err := r.Status().Patch(ctx, recorded, client.MergeFrom(pool)); err != nil {
		return fmt.Errorf("record pending owner %q: %w", owner, err)
	}
	return nil
}

func (r *ModelPoolReconciler) ensureReplicas(ctx context.Context, isvc *inferencev1alpha1.InferenceService, desired int32) error {
	if replicasOf(isvc) == desired {
		return nil
	}
	patched := isvc.DeepCopy()
	patched.Spec.Replicas = ptr.To(desired)
	return r.Patch(ctx, patched, client.MergeFrom(isvc))
}

// annotationModelPoolDrained marks a member this reconciler scaled to zero to
// release the slot. Replicas alone cannot tell the resident's steady state from
// a re-activation of it after a drain: both read replicas 1. The mark is set in
// the same patch as the scale-down and cleared in the same patch as the
// owner's scale-up, so a marked member at replicas 1 was scaled up by someone
// else, the router's activation, since the drain (#1942).
const annotationModelPoolDrained = "inference.llmkube.dev/modelpool-drained"

func drainedByPool(isvc *inferencev1alpha1.InferenceService) bool {
	return isvc != nil && isvc.Annotations[annotationModelPoolDrained] == "true"
}

// restartIdleClockOnReactivation clears the reclaim idle clock when the owner
// is a resident re-activated after the pool drained it. It is serving a fresh
// request, so the idle time that led to the drain no longer counts; left
// running, the clock would reclaim the slot again on the same pass (#1942).
func restartIdleClockOnReactivation(pool *inferencev1alpha1.ModelPool, members map[string]*inferencev1alpha1.InferenceService, owner string) {
	if res := pool.Status.ResidentMember; owner != "" && owner == res && drainedByPool(members[res]) {
		pool.Status.ResidentIdleSince = nil
	}
}

// drainMember scales a displaced member to zero and marks it drained.
func (r *ModelPoolReconciler) drainMember(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	if replicasOf(isvc) == 0 && drainedByPool(isvc) {
		return nil
	}
	patched := isvc.DeepCopy()
	patched.Spec.Replicas = ptr.To(int32(0))
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[annotationModelPoolDrained] = "true"
	return r.Patch(ctx, patched, client.MergeFrom(isvc))
}

// scaleOwner sets the slot owner's replicas. Scaling it up also clears its
// drained mark, in the same patch, so the activation it answers is not read
// as a fresh one again. Holding it at zero during a swap leaves the mark: the
// hold is not a drain.
func (r *ModelPoolReconciler) scaleOwner(
	ctx context.Context,
	base *inferencev1alpha1.ModelPool,
	members map[string]*inferencev1alpha1.InferenceService,
	owner string,
	desired int32,
) error {
	isvc := members[owner]
	if desired == 0 {
		return r.ensureReplicas(ctx, isvc, 0)
	}
	if replicasOf(isvc) == desired && !drainedByPool(isvc) {
		return nil
	}
	if err := r.persistReactivation(ctx, base, isvc, owner); err != nil {
		return err
	}
	patched := isvc.DeepCopy()
	patched.Spec.Replicas = ptr.To(desired)
	delete(patched.Annotations, annotationModelPoolDrained)
	return r.Patch(ctx, patched, client.MergeFrom(isvc))
}

// persistReactivation writes a re-activated resident's cancellation of the
// pending swap (pendingMember cleared, idle clock restarted) to status before
// scaleOwner clears the drained mark. The mark is the only record of the
// re-activation: if the status write at the end of the pass failed after the
// mark was gone, the retry would read the old pendingMember and the old idle
// clock and drain the resident again.
func (r *ModelPoolReconciler) persistReactivation(
	ctx context.Context, base *inferencev1alpha1.ModelPool, isvc *inferencev1alpha1.InferenceService, owner string,
) error {
	if owner != base.Status.ResidentMember || !drainedByPool(isvc) {
		return nil
	}
	if base.Status.PendingMember == "" && base.Status.ResidentIdleSince == nil {
		return nil
	}
	recorded := base.DeepCopy()
	recorded.Status.PendingMember = ""
	recorded.Status.ResidentIdleSince = nil
	if err := r.Status().Patch(ctx, recorded, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("record re-activation of %q: %w", owner, err)
	}
	return nil
}

// memberNames returns the pool's member names in declaration order, giving the
// drain loop a deterministic iteration (map order is not stable).
func memberNames(pool *inferencev1alpha1.ModelPool) []string {
	names := make([]string, 0, len(pool.Spec.Members))
	for _, m := range pool.Spec.Members {
		names = append(names, m.InferenceServiceRef.Name)
	}
	return names
}

// memberIdle reports whether a serving member has no in-flight requests, using
// the same runtime idle-probe abstraction (IdleDetector) as the InferenceService
// rollout drain. A test hook (IdleCheck) overrides the probe; otherwise the
// member's backend probe polls its Service URL. Any error is returned so the
// caller fails closed (keeps the incumbent resident) rather than force-unloading
// a possibly-busy model.
func (r *ModelPoolReconciler) memberIdle(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (bool, error) {
	if r.IdleCheck != nil {
		return r.IdleCheck(ctx, isvc)
	}
	backend := resolveBackend(isvc)
	detector, ok := backend.(IdleDetector)
	if !ok {
		return false, errIdleUnsupported
	}
	baseURL := r.RolloutIdleBaseURL
	if baseURL == "" {
		port := int32(8080)
		if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
			port = isvc.Spec.Endpoint.Port
		} else if isvc.Spec.ContainerPort != nil {
			port = *isvc.Spec.ContainerPort
		}
		baseURL = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", sanitizeDNSName(isvc.Name), isvc.Namespace, port)
	}
	// HTTPClient is optional on the struct and is not wired at any construction
	// site, so it is nil in the running operator. Passing nil straight through
	// panics inside net/http.(*Client).do the first time a swap needs an idle
	// check, which takes the whole reconcile down and wedges the pool in
	// Swapping: every request against it then burns the full swapBudget and
	// returns 503.
	//
	// It also defeats the fail-closed contract this function documents. The
	// probe cannot defer a swap if it panics before it can return an error.
	//
	// Same guard the rollout drain already uses (drain_before_rollout.go), and
	// the same default timeout, so both users of this probe behave alike.
	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return detector.IdleProbe(isvc, httpClient)(ctx, baseURL)
}

// metalMembers returns the names of members whose referenced Model targets a
// metal accelerator. Metal hosts have no device-plugin GPU gating, so v1 refuses
// to manage such pools (see markMetalUnsupported).
func (r *ModelPoolReconciler) metalMembers(ctx context.Context, pool *inferencev1alpha1.ModelPool, members map[string]*inferencev1alpha1.InferenceService) ([]string, error) {
	var metal []string
	for _, name := range memberNames(pool) {
		isvc, ok := members[name]
		if !ok || isvc.Spec.ModelRef == "" {
			continue
		}
		model := &inferencev1alpha1.Model{}
		err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: isvc.Spec.ModelRef}, model)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if model.Spec.Hardware != nil && model.Spec.Hardware.Accelerator == acceleratorMetal {
			metal = append(metal, name)
		}
	}
	sort.Strings(metal)
	return metal, nil
}

// markMetalUnsupported reports a pool as unmanageable because one or more members
// are metal-backed. The reconciler makes no replica changes in this state, so it
// cannot cause a two-resident situation on a host it cannot GPU-gate.
func (r *ModelPoolReconciler) markMetalUnsupported(ctx context.Context, pool *inferencev1alpha1.ModelPool, members map[string]*inferencev1alpha1.InferenceService, metal []string) (ctrl.Result, error) {
	before := pool.DeepCopy()

	memberStatuses := make([]inferencev1alpha1.ModelPoolMemberStatus, 0, len(pool.Spec.Members))
	for _, m := range pool.Spec.Members {
		name := m.InferenceServiceRef.Name
		ms := inferencev1alpha1.ModelPoolMemberStatus{Name: name}
		if isvc, ok := members[name]; ok {
			ms.Phase = isvc.Status.Phase
		}
		memberStatuses = append(memberStatuses, ms)
	}
	pool.Status.Members = memberStatuses
	pool.Status.Phase = inferencev1alpha1.ModelPoolPhaseDegraded
	pool.Status.ObservedGeneration = pool.Generation

	apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               inferencev1alpha1.ConditionModelPoolMetalSupported,
		Status:             metav1.ConditionFalse,
		Reason:             "MetalMembersUnsupported",
		Message:            fmt.Sprintf("Metal-backed members are not supported in v1 (k8s GPU gating only): %v", metal),
		ObservedGeneration: pool.Generation,
	})

	if apiequality.Semantic.DeepEqual(before.Status, pool.Status) {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Patch(ctx, pool, client.MergeFrom(before)); err != nil {
		if apierrors.IsConflict(err) {
			//nolint:staticcheck // SA1019: keep the rate-limited retry on a write conflict; RequeueAfter would swap backoff for a fixed delay.
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// updateStatus patches the pool's status with MergeFrom(before). The caller
// must capture before BEFORE applyReclaimOwner runs: applyReclaimOwner mutates
// status.ResidentIdleSince, and a baseline taken after that mutation diffs the
// idle clock out of the patch, so it never persists and the reclaim never fires
// (the clock resets every reconcile). See the Reconcile call site.
func (r *ModelPoolReconciler) updateStatus(ctx context.Context, pool, before *inferencev1alpha1.ModelPool, members map[string]*inferencev1alpha1.InferenceService, owner string, swapping bool, deferReason string) error {
	memberStatuses := make([]inferencev1alpha1.ModelPoolMemberStatus, 0, len(pool.Spec.Members))
	for _, m := range pool.Spec.Members {
		name := m.InferenceServiceRef.Name
		ms := inferencev1alpha1.ModelPoolMemberStatus{Name: name}
		if isvc, ok := members[name]; ok {
			ms.Phase = isvc.Status.Phase
			ms.Resident = name == owner && memberReady(isvc)
		}
		memberStatuses = append(memberStatuses, ms)
	}
	pool.Status.Members = memberStatuses

	ownerReady := owner != "" && memberReady(members[owner])
	switch {
	case ownerReady:
		pool.Status.Phase = inferencev1alpha1.ModelPoolPhaseReady
		pool.Status.ResidentMember = owner
		pool.Status.PendingMember = ""
	case owner != "":
		if swapping {
			pool.Status.Phase = inferencev1alpha1.ModelPoolPhaseSwapping
		} else {
			pool.Status.Phase = inferencev1alpha1.ModelPoolPhasePending
		}
		pool.Status.PendingMember = owner
	default:
		pool.Status.Phase = inferencev1alpha1.ModelPoolPhasePending
		pool.Status.ResidentMember = ""
		pool.Status.PendingMember = ""
	}

	slotStatus := metav1.ConditionFalse
	slotReason := "Swapping"
	slotMsg := "No member resident yet"
	if ownerReady {
		slotStatus = metav1.ConditionTrue
		slotReason = "Resident"
		slotMsg = fmt.Sprintf("Member %q owns the shared GPU slot", owner)
	}
	apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               inferencev1alpha1.ConditionModelPoolSlotAllocated,
		Status:             slotStatus,
		Reason:             slotReason,
		Message:            slotMsg,
		ObservedGeneration: pool.Generation,
	})
	apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               inferencev1alpha1.ConditionModelPoolMembersValid,
		Status:             metav1.ConditionTrue,
		Reason:             "AllResolved",
		Message:            "All member references resolve",
		ObservedGeneration: pool.Generation,
	})

	// SwapDeferred surfaces a held swap: the incumbent is still serving
	// (PodsBusy) or its idleness cannot be established (IdleCheckFailed). There
	// is no force-unload timeout, so this stays True until the incumbent is
	// genuinely idle.
	if deferReason != "" {
		msg := "Swap deferred: incumbent still serving in-flight requests"
		if deferReason == inferencev1alpha1.ReasonIdleCheckFailed {
			msg = "Swap deferred: incumbent idleness could not be established (fail-closed)"
		}
		apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               inferencev1alpha1.ConditionModelPoolSwapDeferred,
			Status:             metav1.ConditionTrue,
			Reason:             deferReason,
			Message:            msg,
			ObservedGeneration: pool.Generation,
		})
	} else {
		apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               inferencev1alpha1.ConditionModelPoolSwapDeferred,
			Status:             metav1.ConditionFalse,
			Reason:             "NotDeferred",
			Message:            "No swap is being deferred",
			ObservedGeneration: pool.Generation,
		})
	}

	// A managed (non-metal) pool always reports MetalSupported=True; the
	// metal-unsupported path is handled separately in markMetalUnsupported.
	apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               inferencev1alpha1.ConditionModelPoolMetalSupported,
		Status:             metav1.ConditionTrue,
		Reason:             "KubernetesGPU",
		Message:            "All members are k8s GPU-gated",
		ObservedGeneration: pool.Generation,
	})

	pool.Status.ObservedGeneration = pool.Generation
	r.observeResidency(pool, owner)

	if apiequality.Semantic.DeepEqual(before.Status, pool.Status) {
		return nil
	}
	return r.Status().Patch(ctx, pool, client.MergeFrom(before))
}

func (r *ModelPoolReconciler) markDegraded(ctx context.Context, pool *inferencev1alpha1.ModelPool, members map[string]*inferencev1alpha1.InferenceService, missing []string) (ctrl.Result, error) {
	return r.markInvalidMembers(ctx, pool, members, missing, "MissingMembers", "Member InferenceServices not found: %v")
}

func (r *ModelPoolReconciler) markInvalidMembers(ctx context.Context, pool *inferencev1alpha1.ModelPool, members map[string]*inferencev1alpha1.InferenceService, invalid []string, reason, messageFormat string) (ctrl.Result, error) {
	before := pool.DeepCopy()

	memberStatuses := make([]inferencev1alpha1.ModelPoolMemberStatus, 0, len(pool.Spec.Members))
	for _, m := range pool.Spec.Members {
		name := m.InferenceServiceRef.Name
		ms := inferencev1alpha1.ModelPoolMemberStatus{Name: name}
		if isvc, ok := members[name]; ok {
			ms.Phase = isvc.Status.Phase
		}
		memberStatuses = append(memberStatuses, ms)
	}
	pool.Status.Members = memberStatuses
	pool.Status.Phase = inferencev1alpha1.ModelPoolPhaseDegraded
	pool.Status.ObservedGeneration = pool.Generation

	sort.Strings(invalid)
	apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               inferencev1alpha1.ConditionModelPoolMembersValid,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            fmt.Sprintf(messageFormat, invalid),
		ObservedGeneration: pool.Generation,
	})

	if apiequality.Semantic.DeepEqual(before.Status, pool.Status) {
		return ctrl.Result{RequeueAfter: modelPoolSwapRequeue}, nil
	}
	if err := r.Status().Patch(ctx, pool, client.MergeFrom(before)); err != nil {
		if apierrors.IsConflict(err) {
			//nolint:staticcheck // SA1019: keep the rate-limited retry on a write conflict; RequeueAfter would swap backoff for a fixed delay.
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: modelPoolSwapRequeue}, nil
}

func (r *ModelPoolReconciler) observeResidency(pool *inferencev1alpha1.ModelPool, owner string) {
	for _, m := range pool.Spec.Members {
		name := m.InferenceServiceRef.Name
		val := 0.0
		if name == owner && pool.Status.Phase == inferencev1alpha1.ModelPoolPhaseReady {
			val = 1.0
		}
		llmkubemetrics.ModelPoolResident.WithLabelValues(pool.Namespace, pool.Name, name).Set(val)
	}
}

func (r *ModelPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.ModelPool{}).
		Watches(
			&inferencev1alpha1.InferenceService{},
			handler.EnqueueRequestsFromMapFunc(r.findModelPoolsForInferenceService),
		).
		Named("modelpool").
		Complete(r)
}

// findModelPoolsForInferenceService enqueues every ModelPool in the changed
// InferenceService's namespace that lists it as a member. Pools do not own
// their members (members are independent, user-managed InferenceServices), so a
// mapfunc watch replaces owner-reference propagation.
func (r *ModelPoolReconciler) findModelPoolsForInferenceService(ctx context.Context, obj client.Object) []reconcile.Request {
	isvc, ok := obj.(*inferencev1alpha1.InferenceService)
	if !ok {
		return nil
	}
	pools := &inferencev1alpha1.ModelPoolList{}
	if err := r.List(ctx, pools, client.InNamespace(isvc.Namespace)); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range pools.Items {
		for _, m := range pools.Items[i].Spec.Members {
			if m.InferenceServiceRef.Name == isvc.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      pools.Items[i].Name,
						Namespace: pools.Items[i].Namespace,
					},
				})
				break
			}
		}
	}
	return requests
}

// replicasOf returns the member's desired replica count, treating an unset
// spec.replicas as 1 (the InferenceService default).
func replicasOf(isvc *inferencev1alpha1.InferenceService) int32 {
	if isvc.Spec.Replicas == nil {
		return 1
	}
	return *isvc.Spec.Replicas
}

// memberReady reports whether a member InferenceService is serving.
func memberReady(isvc *inferencev1alpha1.InferenceService) bool {
	return isvc != nil && isvc.Status.Phase == PhaseReady
}
