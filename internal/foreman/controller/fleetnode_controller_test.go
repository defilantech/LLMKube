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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// The FleetNodeReconciler evaluates status.lastHeartbeatTime against
// FleetNodeHeartbeatTimeout and drives the phase: a node that stops
// heart-beating transitions Ready -> NotReady (with a HeartbeatStale
// condition), and a node whose heartbeat resumes returns to Ready.
// Regression for defilantech/LLMKube#627.

var _ = Describe("FleetNodeReconciler heartbeat staleness", func() {
	var reconciler *FleetNodeReconciler

	BeforeEach(func() {
		reconciler = &FleetNodeReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("returns no error when the FleetNode is not found", func() {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "absent"},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("transitions a Ready node with a stale heartbeat to NotReady", func() {
		fn := &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: "stale-node"},
			Spec: foremanv1alpha1.FleetNodeSpec{
				NodeName: "stale-node",
				Roles:    []string{"worker"},
			},
		}
		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, fn) })

		stale := metav1.NewTime(time.Now().Add(-2 * foremanv1alpha1.FleetNodeHeartbeatTimeout))
		fn.Status.Phase = foremanv1alpha1.FleetNodePhaseReady
		fn.Status.LastHeartbeatTime = &stale
		Expect(k8sClient.Status().Update(ctx, fn)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "stale-node"},
		})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "stale-node"}, &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.FleetNodePhaseNotReady))

		cond := meta(fresh.Status.Conditions, "Ready")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("HeartbeatStale"))
	})

	It("keeps a node with a fresh heartbeat Ready", func() {
		fn := &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: "fresh-node"},
			Spec: foremanv1alpha1.FleetNodeSpec{
				NodeName: "fresh-node",
				Roles:    []string{"worker"},
			},
		}
		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, fn) })

		fresh := metav1.NewTime(time.Now().Add(-1 * time.Second))
		fn.Status.Phase = foremanv1alpha1.FleetNodePhaseReady
		fn.Status.LastHeartbeatTime = &fresh
		Expect(k8sClient.Status().Update(ctx, fn)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "fresh-node"},
		})
		Expect(err).NotTo(HaveOccurred())

		var got foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "fresh-node"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(foremanv1alpha1.FleetNodePhaseReady))
	})

	It("returns a node whose heartbeat resumes from NotReady to Ready", func() {
		fn := &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: "recovered-node"},
			Spec: foremanv1alpha1.FleetNodeSpec{
				NodeName: "recovered-node",
				Roles:    []string{"worker"},
			},
		}
		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, fn) })

		fresh := metav1.NewTime(time.Now().Add(-1 * time.Second))
		fn.Status.Phase = foremanv1alpha1.FleetNodePhaseNotReady
		fn.Status.LastHeartbeatTime = &fresh
		Expect(k8sClient.Status().Update(ctx, fn)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "recovered-node"},
		})
		Expect(err).NotTo(HaveOccurred())

		var got foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "recovered-node"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(foremanv1alpha1.FleetNodePhaseReady))
	})

	It("reaps a Draining node whose agent heartbeat is long stale", func() {
		// Regression for the FleetNode leak: an agent that sets Draining and
		// then dies (rollout/scale-down) leaves the node Draining forever —
		// nothing transitions it and there is no ownerReference GC. The
		// reconciler must delete such an orphan.
		fn := &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: "orphan-draining"},
			Spec: foremanv1alpha1.FleetNodeSpec{
				NodeName: "orphan-draining",
				Roles:    []string{"worker"},
			},
		}
		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, fn) })

		gone := metav1.NewTime(time.Now().Add(-2 * foremanv1alpha1.FleetNodeDrainReapTimeout))
		fn.Status.Phase = foremanv1alpha1.FleetNodePhaseDraining
		fn.Status.LastHeartbeatTime = &gone
		Expect(k8sClient.Status().Update(ctx, fn)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "orphan-draining"},
		})
		Expect(err).NotTo(HaveOccurred())

		var got foremanv1alpha1.FleetNode
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "orphan-draining"}, &got)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "orphaned Draining node should be deleted")
	})

	It("leaves a Draining node with a fresh heartbeat alone", func() {
		// A live agent draining its in-flight task keeps heartbeating; the
		// reconciler must not reap it out from under the task.
		fn := &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: "active-draining"},
			Spec: foremanv1alpha1.FleetNodeSpec{
				NodeName: "active-draining",
				Roles:    []string{"worker"},
			},
		}
		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, fn) })

		fresh := metav1.NewTime(time.Now().Add(-1 * time.Second))
		fn.Status.Phase = foremanv1alpha1.FleetNodePhaseDraining
		fn.Status.LastHeartbeatTime = &fresh
		Expect(k8sClient.Status().Update(ctx, fn)).To(Succeed())

		res, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "active-draining"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

		var got foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "active-draining"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(foremanv1alpha1.FleetNodePhaseDraining))
	})

	It("requeues after the heartbeat timeout so staleness is detected without an external trigger", func() {
		fn := &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: "requeue-node"},
			Spec: foremanv1alpha1.FleetNodeSpec{
				NodeName: "requeue-node",
				Roles:    []string{"worker"},
			},
		}
		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, fn) })

		fresh := metav1.NewTime(time.Now().Add(-1 * time.Second))
		fn.Status.Phase = foremanv1alpha1.FleetNodePhaseReady
		fn.Status.LastHeartbeatTime = &fresh
		Expect(k8sClient.Status().Update(ctx, fn)).To(Succeed())

		res, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "requeue-node"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Duration(0)))
	})

	It("reaps an in-cluster NotReady node whose agent heartbeat is long stale", func() {
		// Regression for defilantech/LLMKube#1778: when an in-cluster agent pod
		// terminates abruptly (node reboot, rollout, eviction, crash) without
		// completing a graceful drain, its FleetNode is orphaned in NotReady.
		// Because there is no ownerReference for garbage collection, it must be
		// reaped by the controller once silent past FleetNodeNotReadyReapTimeout.
		fn := &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: "orphan-notready"},
			Spec: foremanv1alpha1.FleetNodeSpec{
				NodeName: "orphan-notready",
				Roles:    []string{"worker"},
			},
		}
		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, fn) })

		gone := metav1.NewTime(time.Now().Add(-2 * foremanv1alpha1.FleetNodeNotReadyReapTimeout))
		fn.Status.Phase = foremanv1alpha1.FleetNodePhaseNotReady
		fn.Status.KubernetesNode = "eula"
		fn.Status.LastHeartbeatTime = &gone
		Expect(k8sClient.Status().Update(ctx, fn)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "orphan-notready"},
		})
		Expect(err).NotTo(HaveOccurred())

		var got foremanv1alpha1.FleetNode
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "orphan-notready"}, &got)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "orphaned in-cluster NotReady node should be deleted")
	})

	It("reaps an in-cluster node left in Ready whose agent heartbeat is long stale", func() {
		// When an agent pod dies abruptly, its node may still be recorded as Ready
		// if the reconciler hasn't yet marked it NotReady. The stale-Ready disjunct
		// ensures it is reaped once silence exceeds the reap timeout without
		// needing an intermediate reconcile.
		fn := &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: "orphan-stale-ready"},
			Spec: foremanv1alpha1.FleetNodeSpec{
				NodeName: "orphan-stale-ready",
				Roles:    []string{"worker"},
			},
		}
		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, fn) })

		gone := metav1.NewTime(time.Now().Add(-2 * foremanv1alpha1.FleetNodeNotReadyReapTimeout))
		fn.Status.Phase = foremanv1alpha1.FleetNodePhaseReady
		fn.Status.KubernetesNode = "eula"
		fn.Status.LastHeartbeatTime = &gone
		Expect(k8sClient.Status().Update(ctx, fn)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "orphan-stale-ready"},
		})
		Expect(err).NotTo(HaveOccurred())

		var got foremanv1alpha1.FleetNode
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "orphan-stale-ready"}, &got)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "orphaned in-cluster Ready node past timeout should be reaped")
	})

	It("leaves an off-cluster NotReady node alone even when heartbeat is long stale", func() {
		// Off-cluster nodes (metal Macs) have static identities and leave
		// status.kubernetesNode empty. They must NOT be reaped on extended
		// offline periods so they can reconnect without failing heartbeats.
		fn := &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: "orphan-offcluster"},
			Spec: foremanv1alpha1.FleetNodeSpec{
				NodeName: "orphan-offcluster",
				Roles:    []string{"worker"},
			},
		}
		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, fn) })

		gone := metav1.NewTime(time.Now().Add(-2 * foremanv1alpha1.FleetNodeNotReadyReapTimeout))
		fn.Status.Phase = foremanv1alpha1.FleetNodePhaseNotReady
		fn.Status.KubernetesNode = ""
		fn.Status.LastHeartbeatTime = &gone
		Expect(k8sClient.Status().Update(ctx, fn)).To(Succeed())

		res, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "orphan-offcluster"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

		var got foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "orphan-offcluster"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(foremanv1alpha1.FleetNodePhaseNotReady))
	})

	It("leaves an in-cluster NotReady node alone when heartbeat is only briefly stale", func() {
		// A briefly stale in-cluster node (e.g. restarting container) must not
		// be reaped before the reap timeout expires.
		fn := &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: "recent-stale-incluster"},
			Spec: foremanv1alpha1.FleetNodeSpec{
				NodeName: "recent-stale-incluster",
				Roles:    []string{"worker"},
			},
		}
		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, fn) })

		brieflyStale := metav1.NewTime(time.Now().Add(-2 * foremanv1alpha1.FleetNodeHeartbeatTimeout))
		fn.Status.Phase = foremanv1alpha1.FleetNodePhaseNotReady
		fn.Status.KubernetesNode = "eula"
		fn.Status.LastHeartbeatTime = &brieflyStale
		Expect(k8sClient.Status().Update(ctx, fn)).To(Succeed())

		res, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "recent-stale-incluster"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

		var got foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "recent-stale-incluster"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(foremanv1alpha1.FleetNodePhaseNotReady))
	})
})

// meta finds a condition by type, or nil.
func meta(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
