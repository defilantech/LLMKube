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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// #1896: swapPolicy reclaim-down releases an idle pool's slot to zero, so a
// capacity-constrained cluster gets the GPU back between bursts. It is an
// explicit opt-in: a reclaim pool with no default keeps its resident, as it did
// before the policy existed.
var _ = Describe("ModelPool #1896 reclaim-down", func() {
	ctx := context.Background()
	const poolName = "repro-1896"
	memberA, memberB := poolName+"-a", poolName+"-b"

	idleAlways := func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) { return true, nil }
	newReconciler := func(idle func(context.Context, *inferencev1alpha1.InferenceService) (bool, error)) *ModelPoolReconciler {
		return &ModelPoolReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			IdleCheck: idle,
		}
	}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: poolName, Namespace: "default"}}
	reconcileOnce := func() {
		_, err := newReconciler(idleAlways).Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
	}
	member := func(name string) *inferencev1alpha1.InferenceService {
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, isvc)).To(Succeed())
		return isvc
	}
	pool := func() *inferencev1alpha1.ModelPool {
		p := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: poolName, Namespace: "default"}, p)).To(Succeed())
		return p
	}
	setPhase := func(name, phase string) {
		isvc := member(name)
		isvc.Status.Phase = phase
		Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())
	}
	poolSpec := func(policy inferencev1alpha1.ModelPoolSwapPolicy, def string) inferencev1alpha1.ModelPoolSpec {
		return inferencev1alpha1.ModelPoolSpec{
			GPU:          1,
			Default:      def,
			SwapPolicy:   policy,
			ReclaimAfter: &metav1.Duration{Duration: 5 * time.Minute},
			Members: []inferencev1alpha1.ModelPoolMember{
				{InferenceServiceRef: corev1.LocalObjectReference{Name: memberA}},
				{InferenceServiceRef: corev1.LocalObjectReference{Name: memberB}},
			},
		}
	}

	AfterEach(func() {
		for _, name := range []string{memberA, memberB} {
			if isvc := (&inferencev1alpha1.InferenceService{}); k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, isvc) == nil {
				Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
			}
		}
		if p := (&inferencev1alpha1.ModelPool{}); k8sClient.Get(ctx, types.NamespacedName{Name: poolName, Namespace: "default"}, p) == nil {
			Expect(k8sClient.Delete(ctx, p)).To(Succeed())
		}
	})

	// idleResidentB builds a pool with no default under policy, makes B the
	// resident, and runs its idle clock past reclaimAfter.
	idleResidentB := func(policy inferencev1alpha1.ModelPoolSwapPolicy) {
		for name, n := range map[string]int32{memberA: 0, memberB: 1} {
			Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "some-model",
					Replicas: ptr.To(n),
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
				},
			})).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, &inferencev1alpha1.ModelPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
			Spec:       poolSpec(policy, ""),
		})).To(Succeed())

		By("making B resident")
		reconcileOnce()
		setPhase(memberB, PhaseReady)
		reconcileOnce()
		Expect(pool().Status.ResidentMember).To(Equal(memberB))

		By("running the idle clock past reclaimAfter")
		p := pool()
		long := metav1.NewTime(time.Now().Add(-10 * time.Minute))
		p.Status.ResidentIdleSince = &long
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
	}

	It("keeps the resident of a reclaim pool with no default and no opt-in", func() {
		idleResidentB(inferencev1alpha1.ModelPoolSwapPolicyReclaim)

		reconcileOnce()
		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(1)),
			"reclaim with an empty default must stay sticky; releasing it would change existing pools on upgrade")
		Expect(member(memberB).Annotations).NotTo(HaveKey(annotationModelPoolDrained))
		Expect(replicasOf(member(memberA))).To(Equal(int32(0)))
		after := pool()
		Expect(after.Status.ResidentMember).To(Equal(memberB))
		Expect(after.Status.Phase).To(Equal(inferencev1alpha1.ModelPoolPhaseReady))
	})

	It("releases the slot of an idle reclaim-down pool and stays cold", func() {
		idleResidentB(inferencev1alpha1.ModelPoolSwapPolicyReclaimDown)

		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(0)), "the release drains the idle resident")
		Expect(member(memberB).Annotations).To(HaveKeyWithValue(annotationModelPoolDrained, "true"))
		Expect(replicasOf(member(memberA))).To(Equal(int32(0)), "no member takes the released slot")
		after := pool()
		Expect(after.Status.ResidentMember).To(BeEmpty())
		Expect(after.Status.PendingMember).To(BeEmpty())
		Expect(after.Status.Phase).To(Equal(inferencev1alpha1.ModelPoolPhasePending))

		By("staying released once B stops reporting Ready")
		setPhase(memberB, PhaseStopped)
		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(0)))
		Expect(replicasOf(member(memberA))).To(Equal(int32(0)))
		Expect(pool().Status.ResidentIdleSince).To(BeNil(), "the idle clock clears once the pool is cold")
	})

	It("serves a request that arrives after the release", func() {
		idleResidentB(inferencev1alpha1.ModelPoolSwapPolicyReclaimDown)
		reconcileOnce()
		setPhase(memberB, PhaseStopped)
		reconcileOnce()

		By("activating B from the router")
		b := member(memberB)
		b.Spec.Replicas = ptr.To(int32(1))
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(1)))
		Expect(member(memberB).Annotations).NotTo(HaveKey(annotationModelPoolDrained))
		setPhase(memberB, PhaseReady)
		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(1)))
		Expect(pool().Status.ResidentMember).To(Equal(memberB))
	})

	// The release pass's status write can fail after B was drained, leaving
	// status naming B as the resident. Once B stops reporting Ready, the retry
	// must keep it released rather than read it as the owner and scale it back.
	It("keeps the slot released when the release pass's status write fails", func() {
		idleResidentB(inferencev1alpha1.ModelPoolSwapPolicyReclaimDown)

		failing := newReconciler(idleAlways)
		failing.Client = statusFailsAfterClient{Client: k8sClient, allowed: ptr.To(0)}
		_, err := failing.Reconcile(ctx, req)
		Expect(err).To(HaveOccurred())
		Expect(replicasOf(member(memberB))).To(Equal(int32(0)))
		Expect(pool().Status.ResidentMember).To(Equal(memberB), "the failed write left B named as resident")

		setPhase(memberB, PhaseStopped)
		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(0)),
			"a resident the pool drained to release the slot must not be scaled back up")
		Expect(replicasOf(member(memberA))).To(Equal(int32(0)))
		Expect(pool().Status.ResidentMember).To(BeEmpty())
	})

	It("keeps reporting the resident when the drain defers the release", func() {
		idleResidentB(inferencev1alpha1.ModelPoolSwapPolicyReclaimDown)

		// The reclaim's probe finds B idle; a request lands before the drain's.
		probes := 0
		busyOnDrain := func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) {
			probes++
			return probes == 1, nil
		}
		_, err := newReconciler(busyOnDrain).Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(probes).To(Equal(2))

		Expect(replicasOf(member(memberB))).To(Equal(int32(1)), "a busy resident is never drained")
		after := pool()
		Expect(after.Status.ResidentMember).To(Equal(memberB),
			"B is still serving; the pool must not report itself cold")
		Expect(after.Status.Phase).To(Equal(inferencev1alpha1.ModelPoolPhaseReady))
		deferred := apimeta.FindStatusCondition(after.Status.Conditions, inferencev1alpha1.ConditionModelPoolSwapDeferred)
		Expect(deferred).NotTo(BeNil())
		Expect(deferred.Status).To(Equal(metav1.ConditionTrue))
		Expect(deferred.Reason).To(Equal(inferencev1alpha1.ReasonPodsBusy))
	})

	It("rejects reclaim-down together with a default", func() {
		err := k8sClient.Create(ctx, &inferencev1alpha1.ModelPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
			Spec:       poolSpec(inferencev1alpha1.ModelPoolSwapPolicyReclaimDown, memberA),
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("swapPolicy reclaim-down releases the slot to zero and cannot be combined with default"))
	})
})
