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

package controller

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// ModelPoolReconciler enforces the exclusive-slot invariant for a ModelPool:
// at most one member InferenceService is resident (Ready) at a time, and the
// incumbent is fully drained and unloaded (VRAM freed) before the next member
// loads. The router-proxy commits a swap decision by scaling a member's
// spec.replicas to 1; this reconciler makes that member the sole slot owner.
//
// Drain-before-unload reuses the same /slots idleness contract as the
// InferenceService rollout drain (#1088): a busy incumbent is never scaled down,
// and an unreachable idle check fails closed (incumbent stays resident). Unlike
// a rollout, there is no idleTimeout force branch: the reconciler never
// force-unloads a resident member on a timer, only when it is genuinely idle.
// The bound on a waiting request is the router's hold budget (503 +
// Retry-After), not a controller-side force.
type ModelPoolReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   events.EventRecorder
	HTTPClient *http.Client

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
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
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

	// Drain every non-owner. A non-owner that is currently serving (Ready) is
	// the incumbent being displaced: idle-gate it through the shared /slots
	// contract so an in-flight request finishes before its VRAM is freed. A
	// busy incumbent, or one whose idleness cannot be established, is NEVER
	// scaled down (fail-closed, no force-unload); the swap simply defers.
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
		if memberReady(isvc) {
			idle, ierr := r.memberIdle(ctx, isvc)
			if ierr != nil {
				// Fail closed: keep the incumbent resident.
				log.Info("member idle check failed; deferring swap", "member", name, "error", ierr)
				deferReason = inferencev1alpha1.ReasonIdleCheckFailed
				othersClear = false
				continue
			}
			if !idle {
				// Busy: let the in-flight request finish; do not unload.
				deferReason = inferencev1alpha1.ReasonPodsBusy
				othersClear = false
				continue
			}
		}
		if err := r.ensureReplicas(ctx, isvc, 0); err != nil {
			return ctrl.Result{}, err
		}
		gone, perr := r.memberPodsGone(ctx, isvc)
		if perr != nil {
			return ctrl.Result{}, perr
		}
		if !gone {
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
		if err := r.ensureReplicas(ctx, members[owner], desired); err != nil {
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
	if err := r.updateStatus(ctx, pool, members, owner, swapping, deferReason); err != nil {
		if apierrors.IsConflict(err) {
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

	// requested = members currently scaled up, in declaration order.
	var requested []string
	for _, m := range pool.Spec.Members {
		name := m.InferenceServiceRef.Name
		if isvc, ok := members[name]; ok && replicasOf(isvc) >= 1 {
			requested = append(requested, name)
		}
	}
	// newly = scaled-up members other than the current resident: a fresh proxy
	// activation that wants the slot.
	var newly []string
	for _, name := range requested {
		if name != resident {
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

// ensureReplicas patches the member InferenceService's spec.replicas when it
// differs from the desired value. The reconciler is the sole owner of a pooled
// member's replica count; the proxy's scale-up is only the activation trigger.
func (r *ModelPoolReconciler) ensureReplicas(ctx context.Context, isvc *inferencev1alpha1.InferenceService, desired int32) error {
	if replicasOf(isvc) == desired {
		return nil
	}
	patched := isvc.DeepCopy()
	patched.Spec.Replicas = ptr.To(desired)
	return r.Patch(ctx, patched, client.MergeFrom(isvc))
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
	return detector.IdleProbe(isvc, r.HTTPClient)(ctx, baseURL)
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
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ModelPoolReconciler) updateStatus(ctx context.Context, pool *inferencev1alpha1.ModelPool, members map[string]*inferencev1alpha1.InferenceService, owner string, swapping bool, deferReason string) error {
	before := pool.DeepCopy()

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

	sort.Strings(missing)
	apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               inferencev1alpha1.ConditionModelPoolMembersValid,
		Status:             metav1.ConditionFalse,
		Reason:             "MissingMembers",
		Message:            fmt.Sprintf("Member InferenceServices not found: %v", missing),
		ObservedGeneration: pool.Generation,
	})

	if apiequality.Semantic.DeepEqual(before.Status, pool.Status) {
		return ctrl.Result{RequeueAfter: modelPoolSwapRequeue}, nil
	}
	if err := r.Status().Patch(ctx, pool, client.MergeFrom(before)); err != nil {
		if apierrors.IsConflict(err) {
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

// memberPodsGone reports whether a member InferenceService has no Pods left, so
// its GPU / unified-memory allocation is fully released and another member may
// safely load. It lists Pods by the same labels the InferenceService reconciler
// applies and returns true only when none remain (a Terminating Pod still holds
// the allocation, so it counts as present). This gates on real Pod termination
// rather than the Stopped phase, which flips as soon as desiredReplicas and
// readyReplicas both reach zero, i.e. while the old Pod is still terminating.
func (r *ModelPoolReconciler) memberPodsGone(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (bool, error) {
	if isvc == nil {
		return true, nil
	}
	podList := &corev1.PodList{}
	labels := client.MatchingLabels{
		"app":                           isvc.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}
	if err := r.List(ctx, podList, client.InNamespace(isvc.Namespace), labels); err != nil {
		return false, err
	}
	return len(podList.Items) == 0, nil
}
