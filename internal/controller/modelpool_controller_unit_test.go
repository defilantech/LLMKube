/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func mpoolFor(resident, pending, def string, names ...string) *inferencev1alpha1.ModelPool {
	members := make([]inferencev1alpha1.ModelPoolMember, 0, len(names))
	for _, n := range names {
		members = append(members, inferencev1alpha1.ModelPoolMember{
			InferenceServiceRef: corev1.LocalObjectReference{Name: n},
		})
	}
	return &inferencev1alpha1.ModelPool{
		Spec:   inferencev1alpha1.ModelPoolSpec{Members: members, Default: def},
		Status: inferencev1alpha1.ModelPoolStatus{ResidentMember: resident, PendingMember: pending},
	}
}

func isvcReplicas(r int32) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{Replicas: ptr.To(r)}}
}

func membersMap(scaled map[string]int32) map[string]*inferencev1alpha1.InferenceService {
	m := make(map[string]*inferencev1alpha1.InferenceService, len(scaled))
	for name, r := range scaled {
		m[name] = isvcReplicas(r)
	}
	return m
}

func TestResolveSlotOwner(t *testing.T) {
	tests := []struct {
		name    string
		pool    *inferencev1alpha1.ModelPool
		members map[string]int32
		want    string
	}{
		{
			name:    "cold pool, nothing scaled, spec.default warms",
			pool:    mpoolFor("", "", "coder", "coder", "judge"),
			members: map[string]int32{"coder": 0, "judge": 0},
			want:    "coder",
		},
		{
			name:    "cold pool, nothing scaled, no default stays cold",
			pool:    mpoolFor("", "", "", "coder", "judge"),
			members: map[string]int32{"coder": 0, "judge": 0},
			want:    "",
		},
		{
			name:    "cold pool, scaled member matching default wins",
			pool:    mpoolFor("", "", "judge", "coder", "judge"),
			members: map[string]int32{"coder": 1, "judge": 1},
			want:    "judge",
		},
		{
			name:    "cold pool, scaled members, no default takes declaration order",
			pool:    mpoolFor("", "", "", "coder", "judge"),
			members: map[string]int32{"coder": 1, "judge": 1},
			want:    "coder",
		},
		{
			name:    "cold pool, pending swap continues first",
			pool:    mpoolFor("", "judge", "coder", "coder", "judge"),
			members: map[string]int32{"coder": 0, "judge": 0},
			want:    "judge",
		},
		{
			name:    "warm pool, fresh activation supersedes resident",
			pool:    mpoolFor("coder", "", "", "coder", "judge"),
			members: map[string]int32{"coder": 1, "judge": 1},
			want:    "judge",
		},
		{
			name:    "warm pool, pending among newly activated wins",
			pool:    mpoolFor("coder", "gemma", "", "coder", "judge", "gemma"),
			members: map[string]int32{"coder": 1, "judge": 1, "gemma": 1},
			want:    "gemma",
		},
		{
			name:    "warm pool, no fresh activation, pending swap continues",
			pool:    mpoolFor("coder", "judge", "", "coder", "judge"),
			members: map[string]int32{"coder": 1, "judge": 0},
			want:    "judge",
		},
		{
			name:    "warm pool, quiescent keeps resident",
			pool:    mpoolFor("coder", "", "", "coder", "judge"),
			members: map[string]int32{"coder": 1, "judge": 0},
			want:    "coder",
		},
		{
			name:    "resident left the pool, nothing scaled, no default stays cold",
			pool:    mpoolFor("coder", "", "", "judge"),
			members: map[string]int32{"judge": 0},
			want:    "",
		},
		{
			name:    "resident left the pool, a fresh activation takes the slot",
			pool:    mpoolFor("coder", "", "", "judge", "gemma"),
			members: map[string]int32{"judge": 1, "gemma": 0},
			want:    "judge",
		},
		{
			name:    "resident left the pool, spec.default warms",
			pool:    mpoolFor("coder", "", "judge", "judge"),
			members: map[string]int32{"judge": 0},
			want:    "judge",
		},
		{
			name:    "resident left the pool, the pending swap continues first",
			pool:    mpoolFor("coder", "judge", "", "judge", "gemma"),
			members: map[string]int32{"judge": 0, "gemma": 1},
			want:    "judge",
		},
		{
			name:    "resident left the pool and spec.default names it, stays cold",
			pool:    mpoolFor("coder", "", "coder", "judge"),
			members: map[string]int32{"judge": 0},
			want:    "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			members := membersMap(tc.members)
			got := resolveSlotOwner(tc.pool, members)
			if got != tc.want {
				t.Errorf("resolveSlotOwner = %q, want %q", got, tc.want)
			}
			// Reconcile dereferences members[owner] without a check, so an
			// owner that is not a resolved member panics the pool (#2309).
			if _, ok := members[got]; got != "" && !ok {
				t.Fatalf("resolveSlotOwner = %q, which is not a resolved member: Reconcile would dereference a nil entry", got)
			}
		})
	}
}

// A resident that left spec.members is dropped from status, so the next owner
// can load and the pool stops naming a member it no longer tracks (#2309).
func TestClearDepartedResident(t *testing.T) {
	tests := []struct {
		name     string
		resident string
		members  map[string]int32
		want     string
	}{
		{
			name:     "resident no longer a member is cleared",
			resident: "coder",
			members:  map[string]int32{"judge": 0},
			want:     "",
		},
		{
			name:     "cold pool stays empty",
			resident: "",
			members:  map[string]int32{"judge": 0},
			want:     "",
		},
		{
			name:     "resident still a member is kept",
			resident: "judge",
			members:  map[string]int32{"judge": 1},
			want:     "judge",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool := mpoolFor(tc.resident, "", "", "judge")
			clearDepartedResident(pool, membersMap(tc.members))
			if got := pool.Status.ResidentMember; got != tc.want {
				t.Errorf("ResidentMember = %q, want %q", got, tc.want)
			}
		})
	}
}

// A resident the pool drained and the router scaled back up is a fresh
// activation: it beats the pending swap away from it, where the same replica
// count without the drained mark is steady state and the swap continues
// (#1942).
func TestResolveSlotOwnerReactivatedResident(t *testing.T) {
	pool := mpoolFor("coder", "judge", "judge", "coder", "judge")
	members := membersMap(map[string]int32{"coder": 1, "judge": 0})
	if got := resolveSlotOwner(pool, members); got != "judge" {
		t.Fatalf("unmarked resident: resolveSlotOwner = %q, want the pending judge", got)
	}
	members["coder"].Annotations = map[string]string{annotationModelPoolDrained: "true"}
	if got := resolveSlotOwner(pool, members); got != "coder" {
		t.Errorf("re-activated resident: resolveSlotOwner = %q, want coder", got)
	}
	members["coder"].Spec.Replicas = ptr.To(int32(0))
	if got := resolveSlotOwner(pool, members); got != "judge" {
		t.Errorf("drained resident still at zero: resolveSlotOwner = %q, want the pending judge", got)
	}
}

// replicasWrittenBy records manager as the writer of the member's current
// spec.replicas, the way the API server does for a merge patch that changed it.
func replicasWrittenBy(isvc *inferencev1alpha1.InferenceService, manager string) *inferencev1alpha1.InferenceService {
	return replicasWrittenAt(isvc, manager, nil)
}

// replicasWrittenAt is replicasWrittenBy with the entry's time, which the API
// server records to the second.
func replicasWrittenAt(isvc *inferencev1alpha1.InferenceService, manager string, at *metav1.Time) *inferencev1alpha1.InferenceService {
	isvc.ManagedFields = append(isvc.ManagedFields, metav1.ManagedFieldsEntry{
		Manager:   manager,
		Operation: metav1.ManagedFieldsOperationUpdate,
		Time:      at,
		FieldsV1:  metav1.NewFieldsV1(`{"f:spec":{"f:replicas":{}}}`),
	})
	return isvc
}

// The router's ownership of spec.replicas outlives its request: when the pool
// needs no hold, it never writes the member's replicas, so the member it loaded
// still reads as the router's. The router can see it Ready and move on to the
// next member before the pool records the residency, and then two members read
// as router requests with the old one still pending. The router writes one
// target at a time, so its latest write is the request it is holding now; the
// older target is drained only if it is idle, like any incumbent (#2533).
func TestResolveSlotOwnerLatestRouterRequestWins(t *testing.T) {
	router := inferencev1alpha1.RouterProxyFieldManager
	t0 := metav1.NewTime(time.Unix(1_800_000_000, 0))
	later := metav1.NewTime(t0.Add(90 * time.Second))

	p := mpoolFor("judge", "coder", "", "judge", "glimmer", "coder")
	members := membersMap(map[string]int32{"judge": 1, "glimmer": 1, "coder": 1})
	replicasWrittenAt(members["coder"], router, &t0)
	replicasWrittenAt(members["glimmer"], router, &later)
	if got := resolveSlotOwner(p, members); got != "glimmer" {
		t.Errorf("router moved on to glimmer: resolveSlotOwner = %q, want glimmer", got)
	}

	// Writes within the same second cannot be ordered: the pending one keeps it.
	members = membersMap(map[string]int32{"judge": 1, "glimmer": 1, "coder": 1})
	replicasWrittenAt(members["coder"], router, &t0)
	replicasWrittenAt(members["glimmer"], router, &t0)
	if got := resolveSlotOwner(p, members); got != "coder" {
		t.Errorf("same-second router writes: resolveSlotOwner = %q, want the pending coder", got)
	}

	// The same tie with the pending member first in declaration order.
	p = mpoolFor("judge", "glimmer", "", "judge", "glimmer", "coder")
	if got := resolveSlotOwner(p, members); got != "glimmer" {
		t.Errorf("same-second router writes, pending first: resolveSlotOwner = %q, want the pending glimmer", got)
	}
}

// A request the router is holding open names the member it scaled up. The pool
// used to prefer any pending owner over it, including one the controller chose
// by itself: spec.default warming a cold pool, or a reclaim back to the
// default. The router never scales its target up again, so the drained target
// waited out spec.swapBudget while the member nobody asked for loaded into the
// slot (#2533). A member whose replicas the router wrote, and the pool has not
// touched since, is the router's current target; it beats such a pending owner.
func TestResolveSlotOwnerRouterRequestBeatsPoolChoice(t *testing.T) {
	const pool, router = inferencev1alpha1.ModelPoolFieldManager, inferencev1alpha1.RouterProxyFieldManager
	tests := []struct {
		name     string
		resident string
		pending  string
		def      string
		writers  map[string]string // member at replicas 1 -> writer of its replicas
		want     string
	}{
		{
			name:    "cold pool warming spec.default, the router asks for another member",
			pending: "glimmer", def: "glimmer",
			writers: map[string]string{"glimmer": pool, "coder": router},
			want:    "coder",
		},
		{
			name:     "reclaim to spec.default loading, the router asks for another member",
			resident: "judge", pending: "glimmer", def: "glimmer",
			writers: map[string]string{"glimmer": pool, "coder": router},
			want:    "coder",
		},
		{
			name:    "a router from before #2302 is the router too",
			pending: "glimmer", def: "glimmer",
			writers: map[string]string{"glimmer": pool, "coder": "router-proxy"},
			want:    "coder",
		},
		{
			name:    "cold pool warming spec.default, a GitOps write is not a held request",
			pending: "glimmer", def: "glimmer",
			writers: map[string]string{"glimmer": pool, "coder": "kustomize-controller"},
			want:    "glimmer",
		},
		{
			name:     "two router requests: the pending one keeps the slot",
			resident: "judge", pending: "coder",
			writers: map[string]string{"glimmer": router, "coder": router},
			want:    "coder",
		},
		{
			name:     "two router requests, neither pending: declaration order",
			resident: "judge",
			writers:  map[string]string{"glimmer": router, "coder": router},
			want:     "glimmer",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := mpoolFor(tc.resident, tc.pending, tc.def, "judge", "glimmer", "coder")
			scaled := map[string]int32{"judge": 0, "glimmer": 0, "coder": 0}
			if tc.resident != "" {
				scaled[tc.resident] = 1
			}
			for name := range tc.writers {
				scaled[name] = 1
			}
			members := membersMap(scaled)
			for name, writer := range tc.writers {
				replicasWrittenBy(members[name], writer)
			}
			if got := resolveSlotOwner(p, members); got != tc.want {
				t.Errorf("resolveSlotOwner = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplicasOf(t *testing.T) {
	if got := replicasOf(&inferencev1alpha1.InferenceService{}); got != 1 {
		t.Errorf("replicasOf(nil replicas) = %d, want 1 (apiserver default)", got)
	}
	if got := replicasOf(isvcReplicas(0)); got != 0 {
		t.Errorf("replicasOf(0) = %d, want 0", got)
	}
	if got := replicasOf(isvcReplicas(3)); got != 3 {
		t.Errorf("replicasOf(3) = %d, want 3", got)
	}
}

func TestMemberReady(t *testing.T) {
	if memberReady(nil) {
		t.Error("memberReady(nil) = true, want false")
	}
	notReady := &inferencev1alpha1.InferenceService{}
	notReady.Status.Phase = "Pending"
	if memberReady(notReady) {
		t.Error("memberReady(Pending) = true, want false")
	}
	ready := &inferencev1alpha1.InferenceService{}
	ready.Status.Phase = PhaseReady
	if !memberReady(ready) {
		t.Error("memberReady(Ready) = false, want true")
	}
}

func TestMemberNamesDeclarationOrder(t *testing.T) {
	pool := mpoolFor("", "", "", "judge", "coder", "gemma")
	got := memberNames(pool)
	want := []string{"judge", "coder", "gemma"}
	if len(got) != len(want) {
		t.Fatalf("memberNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("memberNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// readyMember builds a Ready, single-replica member InferenceService.
func readyMember() *inferencev1alpha1.InferenceService {
	isvc := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{Replicas: ptr.To(int32(1))}}
	isvc.Status.Phase = PhaseReady
	return isvc
}

// reclaimDefault is the fixed spec.default used by reclaimPool; the on-demand
// member is "coder".
const reclaimDefault = "glimmer"

// reclaimPool builds a reclaim-policy pool (default = reclaimDefault) with the
// given resident and an optional idle-since instant. members are all Ready.
func reclaimPool(resident string, idleSince *metav1.Time, reclaimAfter time.Duration, names ...string) (*inferencev1alpha1.ModelPool, map[string]*inferencev1alpha1.InferenceService) {
	memberRefs := make([]inferencev1alpha1.ModelPoolMember, 0, len(names))
	m := make(map[string]*inferencev1alpha1.InferenceService, len(names))
	for _, n := range names {
		memberRefs = append(memberRefs, inferencev1alpha1.ModelPoolMember{InferenceServiceRef: corev1.LocalObjectReference{Name: n}})
		m[n] = readyMember()
	}
	pool := &inferencev1alpha1.ModelPool{
		Spec: inferencev1alpha1.ModelPoolSpec{
			Members:      memberRefs,
			Default:      reclaimDefault,
			SwapPolicy:   inferencev1alpha1.ModelPoolSwapPolicyReclaim,
			ReclaimAfter: &metav1.Duration{Duration: reclaimAfter},
		},
		Status: inferencev1alpha1.ModelPoolStatus{ResidentMember: resident, ResidentIdleSince: idleSince},
	}
	return pool, m
}

func TestApplyReclaimOwner(t *testing.T) {
	longAgo := metav1.NewTime(time.Now().Add(-time.Hour))
	justNow := metav1.NewTime(time.Now())

	t.Run("sticky policy never reclaims", func(t *testing.T) {
		pool, members := reclaimPool("coder", &longAgo, time.Minute, "coder", "glimmer")
		pool.Spec.SwapPolicy = inferencev1alpha1.ModelPoolSwapPolicySticky
		r := &ModelPoolReconciler{IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) { return true, nil }}
		owner, requeue := r.applyReclaimOwner(context.Background(), pool, members, "coder")
		if owner != "coder" || requeue != 0 || pool.Status.ResidentIdleSince != nil {
			t.Fatalf("sticky: owner=%q requeue=%v idleSince=%v", owner, requeue, pool.Status.ResidentIdleSince)
		}
	})

	t.Run("default already resident is a no-op", func(t *testing.T) {
		pool, members := reclaimPool("glimmer", &longAgo, time.Minute, "coder", "glimmer")
		r := &ModelPoolReconciler{IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) { return true, nil }}
		owner, requeue := r.applyReclaimOwner(context.Background(), pool, members, "glimmer")
		if owner != "glimmer" || requeue != 0 || pool.Status.ResidentIdleSince != nil {
			t.Fatalf("default-resident: owner=%q requeue=%v idleSince=%v", owner, requeue, pool.Status.ResidentIdleSince)
		}
	})

	t.Run("cross-model activation in flight is not preempted", func(t *testing.T) {
		pool, members := reclaimPool("glimmer", &longAgo, time.Minute, "coder", "glimmer")
		// owner != resident: a fresh activation for coder is already redirecting.
		r := &ModelPoolReconciler{IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) { return true, nil }}
		owner, requeue := r.applyReclaimOwner(context.Background(), pool, members, "coder")
		if owner != "coder" || requeue != 0 || pool.Status.ResidentIdleSince != nil {
			t.Fatalf("activation-in-flight: owner=%q requeue=%v idleSince=%v", owner, requeue, pool.Status.ResidentIdleSince)
		}
	})

	t.Run("busy resident resets the idle clock", func(t *testing.T) {
		pool, members := reclaimPool("coder", &longAgo, time.Minute, "coder", "glimmer")
		r := &ModelPoolReconciler{IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) { return false, nil }}
		owner, requeue := r.applyReclaimOwner(context.Background(), pool, members, "coder")
		if owner != "coder" || pool.Status.ResidentIdleSince != nil {
			t.Fatalf("busy: owner=%q idleSince=%v", owner, pool.Status.ResidentIdleSince)
		}
		if requeue != modelPoolReclaimPoll {
			t.Fatalf("busy: requeue=%v want %v", requeue, modelPoolReclaimPoll)
		}
	})

	t.Run("idle probe failure fails closed", func(t *testing.T) {
		pool, members := reclaimPool("coder", &longAgo, time.Minute, "coder", "glimmer")
		r := &ModelPoolReconciler{IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) {
			return false, errIdleUnsupported
		}}
		owner, requeue := r.applyReclaimOwner(context.Background(), pool, members, "coder")
		if owner != "coder" || requeue != modelPoolReclaimPoll {
			t.Fatalf("probe-fail: owner=%q requeue=%v", owner, requeue)
		}
		// The clock is held where it was, not reset: an elapsed window must
		// survive a poll the resident did not answer.
		if pool.Status.ResidentIdleSince == nil || !pool.Status.ResidentIdleSince.Time.Equal(longAgo.Time) {
			t.Fatalf("probe-fail: idleSince=%v want it held at %v", pool.Status.ResidentIdleSince, longAgo.Time)
		}
	})

	t.Run("first idle observation starts the clock without reclaiming", func(t *testing.T) {
		pool, members := reclaimPool("coder", nil, 10*time.Minute, "coder", "glimmer")
		r := &ModelPoolReconciler{IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) { return true, nil }}
		owner, requeue := r.applyReclaimOwner(context.Background(), pool, members, "coder")
		if owner != "coder" {
			t.Fatalf("clock-start: owner=%q want coder", owner)
		}
		if pool.Status.ResidentIdleSince == nil {
			t.Fatal("clock-start: ResidentIdleSince not set")
		}
		if requeue != modelPoolReclaimPoll { // 10m window caps at the poll interval
			t.Fatalf("clock-start: requeue=%v want %v", requeue, modelPoolReclaimPoll)
		}
	})

	t.Run("idle within the window keeps waiting", func(t *testing.T) {
		pool, members := reclaimPool("coder", &justNow, 10*time.Minute, "coder", "glimmer")
		r := &ModelPoolReconciler{IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) { return true, nil }}
		owner, requeue := r.applyReclaimOwner(context.Background(), pool, members, "coder")
		if owner != "coder" || pool.Status.ResidentIdleSince == nil || requeue != modelPoolReclaimPoll {
			t.Fatalf("within-window: owner=%q requeue=%v idleSince=%v", owner, requeue, pool.Status.ResidentIdleSince)
		}
	})

	t.Run("idle beyond the window reclaims to default", func(t *testing.T) {
		pool, members := reclaimPool("coder", &longAgo, time.Minute, "coder", "glimmer")
		r := &ModelPoolReconciler{IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) { return true, nil }}
		owner, requeue := r.applyReclaimOwner(context.Background(), pool, members, "coder")
		if owner != "glimmer" {
			t.Fatalf("reclaim: owner=%q want glimmer", owner)
		}
		if requeue != 0 {
			t.Fatalf("reclaim: requeue=%v want 0 (swap machinery takes over)", requeue)
		}
	})

	t.Run("short reclaimAfter polls within the window", func(t *testing.T) {
		if got := pollWithin(5 * time.Second); got != 5*time.Second {
			t.Fatalf("pollWithin(5s)=%v want 5s", got)
		}
		if got := pollWithin(time.Hour); got != modelPoolReclaimPoll {
			t.Fatalf("pollWithin(1h)=%v want %v", got, modelPoolReclaimPoll)
		}
	})
}

// TestApplyReclaimOwnerRejectsUnsettledPool covers the branches that reset the
// idle clock and never reclaim because the pool is not settled on a reclaimable
// non-default resident.
func TestApplyReclaimOwnerRejectsUnsettledPool(t *testing.T) {
	longAgo := metav1.NewTime(time.Now().Add(-time.Hour))

	t.Run("empty spec.default resets and never reclaims", func(t *testing.T) {
		pool, members := reclaimPool("coder", &longAgo, time.Minute, "coder", "glimmer")
		pool.Spec.Default = ""
		r := &ModelPoolReconciler{IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) { return true, nil }}
		owner, requeue := r.applyReclaimOwner(context.Background(), pool, members, "coder")
		if owner != "coder" || requeue != 0 || pool.Status.ResidentIdleSince != nil {
			t.Fatalf("empty-default: owner=%q requeue=%v idleSince=%v", owner, requeue, pool.Status.ResidentIdleSince)
		}
	})

	t.Run("resident not Ready resets and never reclaims", func(t *testing.T) {
		pool, members := reclaimPool("coder", &longAgo, time.Minute, "coder", "glimmer")
		members["coder"].Status.Phase = "" // anything but PhaseReady is not-Ready
		r := &ModelPoolReconciler{IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) { return true, nil }}
		owner, requeue := r.applyReclaimOwner(context.Background(), pool, members, "coder")
		if owner != "coder" || requeue != 0 || pool.Status.ResidentIdleSince != nil {
			t.Fatalf("resident-not-ready: owner=%q requeue=%v idleSince=%v", owner, requeue, pool.Status.ResidentIdleSince)
		}
	})

	t.Run("default absent from members resets and never reclaims", func(t *testing.T) {
		// spec.default ("glimmer") is not among the resolved members, so there is
		// nothing to reclaim to.
		pool, members := reclaimPool("coder", &longAgo, time.Minute, "coder")
		r := &ModelPoolReconciler{IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) { return true, nil }}
		owner, requeue := r.applyReclaimOwner(context.Background(), pool, members, "coder")
		if owner != "coder" || requeue != 0 || pool.Status.ResidentIdleSince != nil {
			t.Fatalf("default-not-member: owner=%q requeue=%v idleSince=%v", owner, requeue, pool.Status.ResidentIdleSince)
		}
	})
}
