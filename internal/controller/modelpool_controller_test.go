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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// modelPoolMemberFixture builds a minimal member InferenceService. Members are
// ordinary single-model services; the pool only manages their replica count.
func modelPoolMemberFixture(name string, replicas int32) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "some-model",
			Replicas: ptr.To(replicas),
			Image:    "ghcr.io/ggml-org/llama.cpp:server",
		},
	}
}

func replicasOfMember(ctx context.Context, name string) int32 {
	isvc := &inferencev1alpha1.InferenceService{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, isvc)).To(Succeed())
	return replicasOf(isvc)
}

func setMemberPhase(ctx context.Context, name, phase string, ready int32) {
	isvc := &inferencev1alpha1.InferenceService{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, isvc)).To(Succeed())
	isvc.Status.Phase = phase
	isvc.Status.ReadyReplicas = ready
	isvc.Status.Replicas = ready
	Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())
}

// createMemberPod creates a Pod labeled the way the InferenceService reconciler
// labels member Pods, so ModelPoolReconciler.memberPodsGone counts it as
// present. Used to simulate an incumbent whose Pod is still terminating (its
// GPU / unified-memory allocation not yet released) after being scaled to zero.
func createMemberPod(ctx context.Context, member string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      member + "-pod",
			Namespace: "default",
			Labels: map[string]string{
				"app":                           member,
				"inference.llmkube.dev/service": member,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "server", Image: "ghcr.io/ggml-org/llama.cpp:server"}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	return pod
}

var _ = Describe("ModelPool Controller", func() {
	const poolName = "heavy-slot"
	ctx := context.Background()
	poolKey := types.NamespacedName{Name: poolName, Namespace: "default"}

	// idleReport controls what the injected /slots idle check returns per member
	// name. Default (absent) is idle=true so drains proceed; tests set an entry
	// to false (busy) or an error to exercise defer / fail-closed.
	var idleReport map[string]bool
	var idleErr map[string]error

	BeforeEach(func() {
		idleReport = map[string]bool{}
		idleErr = map[string]error{}
	})

	newReconciler := func() *ModelPoolReconciler {
		return &ModelPoolReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			IdleCheck: func(_ context.Context, isvc *inferencev1alpha1.InferenceService) (bool, error) {
				if err := idleErr[isvc.Name]; err != nil {
					return false, err
				}
				if busy, ok := idleReport[isvc.Name]; ok {
					return busy, nil
				}
				return true, nil
			},
		}
	}

	reconcile := func() {
		r := newReconciler()
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: poolKey})
		Expect(err).NotTo(HaveOccurred())
	}

	AfterEach(func() {
		pool := &inferencev1alpha1.ModelPool{}
		if err := k8sClient.Get(ctx, poolKey, pool); err == nil {
			Expect(k8sClient.Delete(ctx, pool)).To(Succeed())
		}
		for _, n := range []string{"judge", "coder"} {
			isvc := &inferencev1alpha1.InferenceService{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: "default"}, isvc); err == nil {
				Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
			}
			pod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: n + "-pod", Namespace: "default"}, pod); err == nil {
				Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
			}
		}
	})

	createPool := func(swap inferencev1alpha1.ModelPoolSwapPolicy, def string) {
		pool := &inferencev1alpha1.ModelPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelPoolSpec{
				GPU:        1,
				SwapPolicy: swap,
				Members: []inferencev1alpha1.ModelPoolMember{
					{InferenceServiceRef: corev1.LocalObjectReference{Name: "judge"}},
					{InferenceServiceRef: corev1.LocalObjectReference{Name: "coder"}},
				},
				Default: def,
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	}

	It("holds all but the default member at zero replicas on a cold pool", func() {
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("judge", 1))).To(Succeed())
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("coder", 1))).To(Succeed())
		createPool(inferencev1alpha1.ModelPoolSwapPolicySticky, "judge")

		reconcile()

		// judge is the default owner; coder must be drained to zero.
		Expect(replicasOfMember(ctx, "coder")).To(Equal(int32(0)))
		Expect(replicasOfMember(ctx, "judge")).To(Equal(int32(1)))
	})

	It("enforces at most one resident member (exclusive slot)", func() {
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("judge", 1))).To(Succeed())
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("coder", 0))).To(Succeed())
		createPool(inferencev1alpha1.ModelPoolSwapPolicySticky, "judge")

		reconcile()
		setMemberPhase(ctx, "judge", PhaseReady, 1)
		reconcile()

		pool := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, poolKey, pool)).To(Succeed())
		Expect(pool.Status.ResidentMember).To(Equal("judge"))
		Expect(pool.Status.Phase).To(Equal(inferencev1alpha1.ModelPoolPhaseReady))
		Expect(replicasOfMember(ctx, "coder")).To(Equal(int32(0)))
	})

	It("drains the idle incumbent before loading the target (no co-residency)", func() {
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("judge", 1))).To(Succeed())
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("coder", 0))).To(Succeed())
		createPool(inferencev1alpha1.ModelPoolSwapPolicySticky, "judge")

		reconcile()
		setMemberPhase(ctx, "judge", PhaseReady, 1)
		reconcile()

		// Proxy activates coder by scaling it up while judge is still Ready. The
		// incumbent judge reports idle, so the swap may proceed.
		coder := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "coder", Namespace: "default"}, coder)).To(Succeed())
		coder.Spec.Replicas = ptr.To(int32(1))
		Expect(k8sClient.Update(ctx, coder)).To(Succeed())

		// Reconcile: coder is the newly activated owner; judge (idle) drains to
		// zero. coder stays scaled up (Pending) but never Ready until judge is
		// gone: on a real cluster k8s gates co-scheduling on the shared GPU.
		reconcile()
		Expect(replicasOfMember(ctx, "judge")).To(Equal(int32(0)))

		pool := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, poolKey, pool)).To(Succeed())
		Expect(pool.Status.ResidentMember).NotTo(Equal("coder"),
			"coder must not be reported resident while judge is still terminating")

		// judge finishes unloading; coder can now become resident.
		setMemberPhase(ctx, "judge", PhaseStopped, 0)
		setMemberPhase(ctx, "coder", PhaseReady, 1)
		reconcile()
		Expect(k8sClient.Get(ctx, poolKey, pool)).To(Succeed())
		Expect(pool.Status.ResidentMember).To(Equal("coder"))
	})

	It("defers the swap while the incumbent is busy (never force-unloads)", func() {
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("judge", 1))).To(Succeed())
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("coder", 0))).To(Succeed())
		createPool(inferencev1alpha1.ModelPoolSwapPolicySticky, "judge")

		reconcile()
		setMemberPhase(ctx, "judge", PhaseReady, 1)
		reconcile()

		// judge is busy: its /slots reports a request in flight.
		idleReport["judge"] = false

		// coder is activated while judge is busy.
		coder := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "coder", Namespace: "default"}, coder)).To(Succeed())
		coder.Spec.Replicas = ptr.To(int32(1))
		Expect(k8sClient.Update(ctx, coder)).To(Succeed())

		reconcile()

		// The busy incumbent must NOT be scaled down: no traffic-triggered
		// force-unload.
		Expect(replicasOfMember(ctx, "judge")).To(Equal(int32(1)),
			"a busy incumbent must never be force-unloaded")
		pool := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, poolKey, pool)).To(Succeed())
		cond := findPoolCondition(pool.Status.Conditions, inferencev1alpha1.ConditionModelPoolSwapDeferred)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonPodsBusy))

		// Once judge goes idle, the swap proceeds and judge drains.
		idleReport["judge"] = true
		reconcile()
		Expect(replicasOfMember(ctx, "judge")).To(Equal(int32(0)))
	})

	It("fails closed when the incumbent idle check is unreachable", func() {
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("judge", 1))).To(Succeed())
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("coder", 0))).To(Succeed())
		createPool(inferencev1alpha1.ModelPoolSwapPolicySticky, "judge")

		reconcile()
		setMemberPhase(ctx, "judge", PhaseReady, 1)
		reconcile()

		idleErr["judge"] = fmt.Errorf("connection refused")

		coder := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "coder", Namespace: "default"}, coder)).To(Succeed())
		coder.Spec.Replicas = ptr.To(int32(1))
		Expect(k8sClient.Update(ctx, coder)).To(Succeed())

		reconcile()

		Expect(replicasOfMember(ctx, "judge")).To(Equal(int32(1)),
			"idle-check failure must fail closed and keep the incumbent resident")
		pool := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, poolKey, pool)).To(Succeed())
		cond := findPoolCondition(pool.Status.Conditions, inferencev1alpha1.ConditionModelPoolSwapDeferred)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonIdleCheckFailed))
	})

	It("never reports two members resident across a fast interleaved swap", func() {
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("judge", 1))).To(Succeed())
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("coder", 0))).To(Succeed())
		createPool(inferencev1alpha1.ModelPoolSwapPolicySticky, "judge")

		reconcile()
		setMemberPhase(ctx, "judge", PhaseReady, 1)
		reconcile()

		assertNotBothReady := func() {
			j := &inferencev1alpha1.InferenceService{}
			c := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "judge", Namespace: "default"}, j)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "coder", Namespace: "default"}, c)).To(Succeed())
			Expect(memberReady(j) && memberReady(c)).To(BeFalse(),
				"the exclusive slot must never have two Ready members")
		}

		// Rapidly interleave judge -> coder -> judge activations, advancing the
		// drained member to Stopped between flips, asserting exclusivity holds.
		activate := func(name string) {
			isvc := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, isvc)).To(Succeed())
			isvc.Spec.Replicas = ptr.To(int32(1))
			Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
		}

		activate("coder")
		reconcile()
		assertNotBothReady()
		setMemberPhase(ctx, "judge", PhaseStopped, 0)
		setMemberPhase(ctx, "coder", PhaseReady, 1)
		reconcile()
		assertNotBothReady()

		activate("judge")
		reconcile()
		assertNotBothReady()
		setMemberPhase(ctx, "coder", PhaseStopped, 0)
		setMemberPhase(ctx, "judge", PhaseReady, 1)
		reconcile()
		assertNotBothReady()

		pool := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, poolKey, pool)).To(Succeed())
		Expect(pool.Status.ResidentMember).To(Equal("judge"))
	})

	It("holds the owner at zero until the incumbent's pods are gone (unified-memory reclaim-lag gate)", func() {
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("judge", 1))).To(Succeed())
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("coder", 0))).To(Succeed())
		createPool(inferencev1alpha1.ModelPoolSwapPolicySticky, "judge")

		reconcile()
		setMemberPhase(ctx, "judge", PhaseReady, 1)
		reconcile()

		// The incumbent judge has a running Pod. On a unified-memory node the
		// device plugin frees gpu:1 when this Pod terminates, but the actual
		// allocation is only released when the process exits, so the owner must
		// gate on the Pod being *gone*, not merely on judge going Stopped.
		judgePod := createMemberPod(ctx, "judge")

		// Proxy activates coder while judge is idle.
		coder := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "coder", Namespace: "default"}, coder)).To(Succeed())
		coder.Spec.Replicas = ptr.To(int32(1))
		Expect(k8sClient.Update(ctx, coder)).To(Succeed())

		// judge drains to zero and reports Stopped, but its Pod still exists
		// (Terminating). coder must be HELD at zero, not scaled up, so the two
		// never co-reside in the reclaim-lag window.
		reconcile()
		setMemberPhase(ctx, "judge", PhaseStopped, 0)
		reconcile()

		Expect(replicasOfMember(ctx, "judge")).To(Equal(int32(0)))
		Expect(replicasOfMember(ctx, "coder")).To(Equal(int32(0)),
			"owner must stay at zero while the incumbent Pod is still terminating")
		pool := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, poolKey, pool)).To(Succeed())
		Expect(pool.Status.Phase).To(Equal(inferencev1alpha1.ModelPoolPhaseSwapping))

		// The incumbent Pod finally terminates; the allocation is released.
		Expect(k8sClient.Delete(ctx, judgePod, client.GracePeriodSeconds(0))).To(Succeed())

		reconcile()
		Expect(replicasOfMember(ctx, "coder")).To(Equal(int32(1)),
			"owner loads only once the incumbent Pod is gone")
	})

	It("keeps the resident sticky across same-model demand", func() {
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("judge", 0))).To(Succeed())
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("coder", 1))).To(Succeed())
		createPool(inferencev1alpha1.ModelPoolSwapPolicySticky, "")

		reconcile()
		setMemberPhase(ctx, "coder", PhaseReady, 1)
		reconcile()

		pool := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, poolKey, pool)).To(Succeed())
		Expect(pool.Status.ResidentMember).To(Equal("coder"))

		// A no-op reconcile must not disturb the resident (no auto-restore of a
		// default).
		reconcile()
		Expect(replicasOfMember(ctx, "coder")).To(Equal(int32(1)))
		Expect(replicasOfMember(ctx, "judge")).To(Equal(int32(0)))
	})

	It("marks the pool Degraded when a member reference is missing", func() {
		Expect(k8sClient.Create(ctx, modelPoolMemberFixture("judge", 0))).To(Succeed())
		// coder intentionally not created.
		createPool(inferencev1alpha1.ModelPoolSwapPolicySticky, "")

		reconcile()

		pool := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, poolKey, pool)).To(Succeed())
		Expect(pool.Status.Phase).To(Equal(inferencev1alpha1.ModelPoolPhaseDegraded))
		cond := findPoolCondition(pool.Status.Conditions, inferencev1alpha1.ConditionModelPoolMembersValid)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	})
})

// findPoolCondition mirrors apimachinery meta.FindStatusCondition without an
// import alias collision in this test file.
func findPoolCondition(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

var _ = Describe("ModelPool member mapping", func() {
	It("enqueues pools that reference a changed InferenceService", func() {
		ctx := context.Background()
		pool := &inferencev1alpha1.ModelPool{
			ObjectMeta: metav1.ObjectMeta{Name: "map-pool", Namespace: "default"},
			Spec: inferencev1alpha1.ModelPoolSpec{
				Members: []inferencev1alpha1.ModelPoolMember{
					{InferenceServiceRef: corev1.LocalObjectReference{Name: "mapped-isvc"}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, pool)).To(Succeed()) }()

		r := &ModelPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reqs := r.findModelPoolsForInferenceService(ctx, &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "mapped-isvc", Namespace: "default"},
		})
		Expect(reqs).To(ContainElement(reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "map-pool", Namespace: "default"},
		}))
	})
})

var _ = Describe("ModelPool client guard", func() {
	It("ignores a not-found pool", func() {
		ctx := context.Background()
		r := &ModelPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "ghost", Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("ModelPool metal guard", func() {
	ctx := context.Background()

	It("refuses to manage a pool with a metal-backed member", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "metal-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/model.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: acceleratorMetal},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		member := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "metal-member", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "metal-model",
				Replicas: ptr.To(int32(1)),
				Image:    "ghcr.io/ggml-org/llama.cpp:server",
			},
		}
		Expect(k8sClient.Create(ctx, member)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, member) }()

		pool := &inferencev1alpha1.ModelPool{
			ObjectMeta: metav1.ObjectMeta{Name: "metal-pool", Namespace: "default"},
			Spec: inferencev1alpha1.ModelPoolSpec{
				Members: []inferencev1alpha1.ModelPoolMember{
					{InferenceServiceRef: corev1.LocalObjectReference{Name: "metal-member"}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, pool) }()

		r := &ModelPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "metal-pool", Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		got := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "metal-pool", Namespace: "default"}, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(inferencev1alpha1.ModelPoolPhaseDegraded))
		cond := findPoolCondition(got.Status.Conditions, inferencev1alpha1.ConditionModelPoolMetalSupported)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))

		// The reconciler must not have scaled the metal member (it is unmanaged).
		Expect(replicasOfMember(ctx, "metal-member")).To(Equal(int32(1)))
	})
})
