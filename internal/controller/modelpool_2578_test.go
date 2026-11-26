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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

// #2578: a member that just left the slot is re-activated while its own Pod
// from before the drain is still terminating. The drain loop holds an owner
// until every other member's Pods are gone; it must hold the owner for its own
// terminating Pod too, or the Deployment starts a second Pod beside one whose
// process may still hold the memory.
var _ = Describe("ModelPool #2578", func() {
	ctx := context.Background()
	const poolName = "repro-2578"
	memberA, memberB, podB := poolName+"-default", poolName+"-on-demand", poolName+"-on-demand-pod"

	r := func() *ModelPoolReconciler {
		return &ModelPoolReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) {
				return true, nil
			},
		}
	}
	reconcileOnce := func() {
		_, err := r().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: poolName, Namespace: "default"}})
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
	markReady := func(name string) {
		isvc := member(name)
		isvc.Status.Phase = PhaseReady
		isvc.Status.ReadyReplicas = 1
		isvc.Status.Replicas = 1
		Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())
	}
	// createPodB gives B a Pod whose finalizer keeps it present after a delete,
	// the way a serving process outlives its Pod's deletion.
	createPodB := func() {
		Expect(k8sClient.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       podB,
				Namespace:  "default",
				Finalizers: []string{"test.llmkube.dev/hold"},
				Labels: map[string]string{
					"app":                           memberB,
					"inference.llmkube.dev/service": memberB,
				},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "ghcr.io/ggml-org/llama.cpp:server"}}},
		})).To(Succeed())
	}
	terminatePodB := func() {
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: podB, Namespace: "default"}, pod)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: podB, Namespace: "default"}, pod)).To(Succeed())
		Expect(pod.DeletionTimestamp).NotTo(BeNil(), "the finalizer keeps the Pod present while it terminates")
	}
	releasePodB := func() {
		pod := &corev1.Pod{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: podB, Namespace: "default"}, pod); err == nil {
			pod.Finalizers = nil
			Expect(k8sClient.Update(ctx, pod)).To(Succeed())
			_ = k8sClient.Delete(ctx, pod)
		}
		Eventually(func() bool {
			return k8sClient.Get(ctx, types.NamespacedName{Name: podB, Namespace: "default"}, &corev1.Pod{}) != nil
		}).Should(BeTrue())
	}
	// reactivateB scales B up the way the router-proxy does, through its own
	// member controller, so the pool reads the activation from the router's
	// field manager (routerTarget) as it does in a cluster.
	reactivateB := func() {
		Expect(router.NewKubeMemberController(k8sClient, time.Millisecond).Activate(ctx, "default", memberB)).To(Succeed())
		Expect(replicasOf(member(memberB))).To(Equal(int32(1)))
	}

	// makeBResident builds the pool (reclaim after 5m; default A under reclaim,
	// which reclaim-down refuses) with B resident and serving from podB.
	makeBResident := func(policy inferencev1alpha1.ModelPoolSwapPolicy) {
		def := memberA
		if policy == inferencev1alpha1.ModelPoolSwapPolicyReclaimDown {
			def = ""
		}
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
			Spec: inferencev1alpha1.ModelPoolSpec{
				GPU:          1,
				Default:      def,
				SwapPolicy:   policy,
				ReclaimAfter: &metav1.Duration{Duration: 5 * time.Minute},
				SwapBudget:   &metav1.Duration{Duration: 240 * time.Second},
				Members: []inferencev1alpha1.ModelPoolMember{
					{InferenceServiceRef: corev1.LocalObjectReference{Name: memberA}},
					{InferenceServiceRef: corev1.LocalObjectReference{Name: memberB}},
				},
			},
		})).To(Succeed())
		reconcileOnce()
		markReady(memberB)
		reconcileOnce()
		Expect(pool().Status.ResidentMember).To(Equal(memberB))
		createPodB()
	}

	// reclaimB runs B's idle clock past reclaimAfter and lets the reclaim drain
	// it, leaving its Pod terminating. With statusCaughtUp the InferenceService
	// controller has already reported B Stopped; without it B still reads Ready
	// from before the drain, as it does until that controller's next pass.
	reclaimB := func(statusCaughtUp bool) {
		p := pool()
		long := metav1.NewTime(time.Now().Add(-10 * time.Minute))
		p.Status.ResidentIdleSince = &long
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(0)), "the reclaim drains the idle resident")
		Expect(drainedByPool(member(memberB))).To(BeTrue())
		terminatePodB()
		if !statusCaughtUp {
			return
		}
		// The InferenceService controller reports a member at zero with no
		// ready replica as Stopped.
		b := member(memberB)
		b.Status.Phase = PhaseStopped
		b.Status.ReadyReplicas = 0
		b.Status.Replicas = 0
		Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
	}

	AfterEach(func() {
		releasePodB()
		for _, name := range []string{memberA, memberB} {
			if isvc := (&inferencev1alpha1.InferenceService{}); k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, isvc) == nil {
				Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
			}
		}
		if p := (&inferencev1alpha1.ModelPool{}); k8sClient.Get(ctx, types.NamespacedName{Name: poolName, Namespace: "default"}, p) == nil {
			Expect(k8sClient.Delete(ctx, p)).To(Succeed())
		}
	})

	DescribeTable("holds a re-activated member at zero until its own terminating Pod is gone",
		func(policy inferencev1alpha1.ModelPoolSwapPolicy, statusCaughtUp bool) {
			makeBResident(policy)
			reclaimB(statusCaughtUp)

			By("re-activating B from the router while its old Pod terminates")
			reactivateB()
			res, err := r().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: poolName, Namespace: "default"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(modelPoolSwapRequeue),
				"no Pod event wakes the pool, so its swap requeue is what ends the hold")
			Expect(replicasOf(member(memberB))).To(Equal(int32(0)),
				"B's previous Pod still holds its memory; a new Pod must not start beside it")
			Expect(drainedByPool(member(memberB))).To(BeTrue(), "a hold is not a scale-up: the mark stays")
			if statusCaughtUp {
				Expect(pool().Status.PendingMember).To(Equal(memberB), "the hold records the activation it defers")
			}
			Expect(replicasOf(member(memberA))).To(Equal(int32(0)))

			By("still holding on the next pass")
			reconcileOnce()
			Expect(replicasOf(member(memberB))).To(Equal(int32(0)))

			By("scaling B up once its old Pod is gone")
			releasePodB()
			reconcileOnce()
			Expect(replicasOf(member(memberB))).To(Equal(int32(1)))
			Expect(drainedByPool(member(memberB))).To(BeFalse(), "the scale-up clears the mark")
			Expect(replicasOf(member(memberA))).To(Equal(int32(0)))
		},
		Entry("reclaim", inferencev1alpha1.ModelPoolSwapPolicyReclaim, true),
		Entry("reclaim-down", inferencev1alpha1.ModelPoolSwapPolicyReclaimDown, true),
		Entry("reclaim, B's status still Ready from before the drain", inferencev1alpha1.ModelPoolSwapPolicyReclaim, false),
		Entry("reclaim-down, B's status still Ready from before the drain",
			inferencev1alpha1.ModelPoolSwapPolicyReclaimDown, false),
	)

	// An invalid pool drains nothing, but its hold for an incoming member
	// gates on the same Pods.
	It("holds a re-activated member for its own terminating Pod while a member is missing", func() {
		makeBResident(inferencev1alpha1.ModelPoolSwapPolicyReclaim)
		reclaimB(true)

		By("listing a member that does not exist, then re-activating B")
		p := pool()
		p.Spec.Members = append(p.Spec.Members, inferencev1alpha1.ModelPoolMember{
			InferenceServiceRef: corev1.LocalObjectReference{Name: poolName + "-ghost"},
		})
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
		reactivateB()
		reconcileOnce()
		Expect(pool().Status.Phase).To(Equal(inferencev1alpha1.ModelPoolPhaseDegraded))
		Expect(replicasOf(member(memberB))).To(Equal(int32(0)),
			"B's previous Pod still holds its memory; a new Pod must not start beside it")

		By("scaling B up once its old Pod is gone")
		releasePodB()
		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(1)))
	})

	// #2240: the hold zeroes the replica count that records the router's
	// activation, so the activation must be in status first. A failed status
	// write leaves B at one, and the retried pass holds it.
	It("records the activation before it holds a re-activated member", func() {
		makeBResident(inferencev1alpha1.ModelPoolSwapPolicyReclaimDown)
		reclaimB(true)
		reactivateB()

		failing := r()
		failing.Client = statusFailsAfterClient{Client: k8sClient, allowed: ptr.To(0)}
		_, err := failing.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: poolName, Namespace: "default"}})
		Expect(err).To(HaveOccurred())
		Expect(replicasOf(member(memberB))).To(Equal(int32(1)),
			"without the recorded activation, zeroing B would lose the router's request")

		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(0)))
		Expect(pool().Status.PendingMember).To(Equal(memberB))

		releasePodB()
		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(1)))
	})

	It("does not hold a resident the pool never drained when one of its Pods terminates", func() {
		makeBResident(inferencev1alpha1.ModelPoolSwapPolicyReclaim)

		By("terminating the resident's Pod outside a drain, as a rollout or an eviction does")
		terminatePodB()
		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(1)),
			"only a member the pool released waits for its own Pods; a resident keeps serving through its rollout")
		Expect(drainedByPool(member(memberB))).To(BeFalse())
	})
})
