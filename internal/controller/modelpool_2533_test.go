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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

// #2533: a request comes through the router for member t while the pool is
// loading spec.default (d) on its own, either into a cold pool or as a reclaim.
// The router scales t up once and then waits for it. The pool used to keep d,
// the pending owner, drain t, and leave the request waiting out swapBudget for
// a member that never loaded. The router's scale-up is read from the real
// managedFields the API server records for its patch.
var _ = Describe("ModelPool #2533 router request during a pool-chosen load", func() {
	ctx := context.Background()

	var pools []string
	var memberNamesCreated []string
	seq := 0

	reconcileOnce := func(pool string) {
		r := &ModelPoolReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(16),
			IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) {
				return true, nil
			},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: pool, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	member := func(n string) *inferencev1alpha1.InferenceService {
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: "default"}, isvc)).To(Succeed())
		return isvc
	}
	poolOf := func(pool string) *inferencev1alpha1.ModelPool {
		p := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pool, Namespace: "default"}, p)).To(Succeed())
		return p
	}
	setPhase := func(n, phase string) {
		isvc := member(n)
		isvc.Status.Phase = phase
		Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())
	}
	createMembers := func(replicas map[string]int32) {
		for n, r := range replicas {
			Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "some-model",
					Replicas: ptr.To(r),
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
				},
			})).To(Succeed())
			memberNamesCreated = append(memberNamesCreated, n)
		}
	}
	refs := func(names ...string) []inferencev1alpha1.ModelPoolMember {
		out := make([]inferencev1alpha1.ModelPoolMember, 0, len(names))
		for _, n := range names {
			out = append(out, inferencev1alpha1.ModelPoolMember{InferenceServiceRef: corev1.LocalObjectReference{Name: n}})
		}
		return out
	}
	routerRequests := func(n string) {
		Expect(router.NewKubeMemberController(k8sClient, time.Millisecond).Activate(ctx, "default", n)).To(Succeed())
	}
	expectRequestHonored := func(pool, d, t string) {
		GinkgoHelper()
		Expect(poolOf(pool).Status.PendingMember).To(Equal(t), "the router's target must become the pending owner")
		Expect(replicasOf(member(t))).To(Equal(int32(1)), "the router's target must not be drained")
		Expect(replicasOf(member(d))).To(Equal(int32(0)), "the member nobody asked for is drained")
		Expect(drainedByPool(member(d))).To(BeTrue())

		setPhase(t, PhaseReady)
		reconcileOnce(pool)
		Expect(poolOf(pool).Status.ResidentMember).To(Equal(t))
	}

	AfterEach(func() {
		for _, n := range memberNamesCreated {
			if isvc := (&inferencev1alpha1.InferenceService{}); k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: "default"}, isvc) == nil {
				Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
			}
		}
		for _, n := range pools {
			if p := (&inferencev1alpha1.ModelPool{}); k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: "default"}, p) == nil {
				Expect(k8sClient.Delete(ctx, p)).To(Succeed())
			}
		}
		pools, memberNamesCreated = nil, nil
	})

	It("lets the router's target take a cold pool that is warming spec.default", func() {
		seq++
		pool := fmt.Sprintf("x2533-cold-%d", seq)
		d, t := pool+"-d", pool+"-t"
		createMembers(map[string]int32{d: 0, t: 0})
		Expect(k8sClient.Create(ctx, &inferencev1alpha1.ModelPool{
			ObjectMeta: metav1.ObjectMeta{Name: pool, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelPoolSpec{GPU: 1, Default: d, Members: refs(d, t)},
		})).To(Succeed())
		pools = append(pools, pool)

		// The cold pool warms spec.default on its own.
		reconcileOnce(pool)
		Expect(poolOf(pool).Status.PendingMember).To(Equal(d))
		Expect(replicasOf(member(d))).To(Equal(int32(1)))

		// A request for t arrives through the router before d is Ready.
		routerRequests(t)
		reconcileOnce(pool)
		expectRequestHonored(pool, d, t)
	})

	It("lets the router's target take the slot from a reclaim to spec.default", func() {
		seq++
		pool := fmt.Sprintf("x2533-reclaim-%d", seq)
		d, a, t := pool+"-d", pool+"-a", pool+"-t"
		createMembers(map[string]int32{d: 0, a: 1, t: 0})
		Expect(k8sClient.Create(ctx, &inferencev1alpha1.ModelPool{
			ObjectMeta: metav1.ObjectMeta{Name: pool, Namespace: "default"},
			Spec: inferencev1alpha1.ModelPoolSpec{
				GPU:          1,
				Default:      d,
				SwapPolicy:   inferencev1alpha1.ModelPoolSwapPolicyReclaim,
				ReclaimAfter: &metav1.Duration{Duration: 20 * time.Millisecond},
				Members:      refs(d, a, t),
			},
		})).To(Succeed())
		pools = append(pools, pool)

		// a becomes resident, then sits idle past reclaimAfter: the pool
		// reclaims the slot for d and starts loading it.
		reconcileOnce(pool)
		setPhase(a, PhaseReady)
		reconcileOnce(pool)
		Expect(poolOf(pool).Status.ResidentMember).To(Equal(a))
		reconcileOnce(pool) // starts the idle clock
		time.Sleep(50 * time.Millisecond)
		reconcileOnce(pool) // reclaims: drains a, scales d up
		Expect(poolOf(pool).Status.PendingMember).To(Equal(d))
		Expect(replicasOf(member(d))).To(Equal(int32(1)))
		Expect(replicasOf(member(a))).To(Equal(int32(0)))

		// A request for t arrives through the router while d loads.
		routerRequests(t)
		reconcileOnce(pool)
		expectRequestHonored(pool, d, t)
	})
})
