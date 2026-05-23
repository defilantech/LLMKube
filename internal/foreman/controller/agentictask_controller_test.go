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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// The M2 AgenticTaskReconciler is the foreman v0.1 scheduler. Its
// contract:
//
//   empty status.phase           -> Pending (initial normalization)
//   Pending + no fit             -> requeue with no status mutation
//   Pending + matching FleetNode -> Scheduled with assignedNode set
//   Pending + dep Failed         -> cascade-fail (phase=Failed,
//                                   verdict=INCOMPLETE,
//                                   condition Failed/UpstreamFailed)
//   Pending + dep pre-terminal   -> requeue with no status mutation
//   Scheduled/Running/...        -> no-op (FleetAgent's domain)
//
// Each It block creates its own resources and DeferCleanup-removes them
// so the cluster-scoped FleetNode does not leak across tests.

var _ = Describe("AgenticTaskReconciler scheduler", func() {
	var reconciler *AgenticTaskReconciler

	BeforeEach(func() {
		reconciler = &AgenticTaskReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("returns no error and no requeue when the task is not found", func() {
		res, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: "no-such-task"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero())
	})

	It("normalizes an empty phase to Pending", func() {
		task := newTask("normalize-empty")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(fresh.Status.AssignedNode).To(BeEmpty())
	})

	It("schedules a Pending task to the first-fit Ready FleetNode", func() {
		task := newTask("schedule-target")
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
			MinRAMGB:    32,
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })

		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		node := newFleetNode("schedule-target-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     128,
			AvailableRAMGB: 96,
		})

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))
	})

	It("requeues without mutating status when no FleetNode satisfies the capability", func() {
		task := newTask("no-fit")
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
			MinRAMGB:    256, // bigger than any node will advertise
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })

		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		res, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(fresh.Status.AssignedNode).To(BeEmpty())
	})

	It("cascade-fails a Pending task when its dependency is Failed", func() {
		dep := newTask("cascade-dep")
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })
		setPhase(dep, foremanv1alpha1.AgenticTaskPhaseFailed)

		task := newTask("cascade-target")
		task.Spec.DependsOn = []string{dep.Name}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseFailed))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictIncomplete))

		failedCond := findCondition(fresh.Status.Conditions, "Failed")
		Expect(failedCond).NotTo(BeNil())
		Expect(failedCond.Reason).To(Equal("UpstreamFailed"))
	})

	It("waits with requeue when a dependency is still pre-terminal", func() {
		dep := newTask("wait-dep") // status stays empty == pre-terminal
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })

		task := newTask("wait-target")
		task.Spec.DependsOn = []string{dep.Name}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		res, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(fresh.Status.AssignedNode).To(BeEmpty())
	})

	It("schedules a task using the referenced Agent's RequiredCapability", func() {
		agent := newAgent("ref-coder")
		agent.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
			MinRAMGB:    32,
		}
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })

		task := newTask("ref-target")
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		node := newFleetNode("ref-target-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     128,
			AvailableRAMGB: 96,
		})

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))
	})

	It("fails fast with AgentNotFound when AgentRef points at a missing Agent", func() {
		task := newTask("missing-agent")
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: "does-not-exist"}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseFailed))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictIncomplete))
		failedCond := findCondition(fresh.Status.Conditions, "Failed")
		Expect(failedCond).NotTo(BeNil())
		Expect(failedCond.Reason).To(Equal("AgentNotFound"))
		Expect(failedCond.Message).To(ContainSubstring("does-not-exist"))
	})

	It("ignores the task's own RequiredCapability when AgentRef is set", func() {
		// The task asks for an unsatisfiable RAM size; the Agent it
		// references only requires what the test node advertises. The
		// locked M3 contract says AgentRef wins, so the task should
		// schedule successfully despite the task's larger RAM ask.
		agent := newAgent("override-coder")
		agent.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
			MinRAMGB:    16,
		}
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })

		task := newTask("override-target")
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
			MinRAMGB:    1024, // would not fit any test node
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		node := newFleetNode("override-target-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     32,
			AvailableRAMGB: 24,
		})

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))
	})

	It("filters FleetNodes by spec.roles when RequiredCapability.Roles is set", func() {
		// Two nodes: one advertises 'verifier', one only 'worker'. A task
		// requiring roles=[verifier] must land on the verifier node and
		// not on the worker-only one even though both are Ready.
		workerNode := newFleetNode("worker-only")
		workerNode.Spec.Roles = []string{"worker"}
		Expect(k8sClient.Create(ctx, workerNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, workerNode) })
		setNodeReady(workerNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("cuda"),
			TotalRAMGB:  64, AvailableRAMGB: 48,
		})

		verifierNode := newFleetNode("verifier-node")
		verifierNode.Spec.Roles = []string{"worker", "verifier"}
		Expect(k8sClient.Create(ctx, verifierNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, verifierNode) })
		setNodeReady(verifierNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("cuda"),
			TotalRAMGB:  64, AvailableRAMGB: 48,
		})

		task := newTask("roles-target")
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Roles: []string{"verifier"},
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(verifierNode.Name))
	})

	It("does not touch a task already past Pending (FleetAgent's domain)", func() {
		task := newTask("hands-off")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhaseRunning)

		// Also set assignedNode to confirm the scheduler doesn't clobber.
		patch := client.MergeFrom(task.DeepCopy())
		task.Status.AssignedNode = "some-node"
		Expect(k8sClient.Status().Patch(ctx, task, patch)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseRunning))
		Expect(fresh.Status.AssignedNode).To(Equal("some-node"))
	})
})

// --- test helpers ---

func newTask(name string) *foremanv1alpha1.AgenticTask {
	return &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind:    foremanv1alpha1.AgenticTaskKindFreeform,
			Payload: foremanv1alpha1.AgenticTaskPayload{Prompt: "test"},
		},
	}
}

func newAgent(name string) *foremanv1alpha1.Agent {
	return &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:                foremanv1alpha1.AgentRoleCoder,
			InferenceServiceRef: corev1.LocalObjectReference{Name: "any-svc"},
			SystemPrompt:        "test system prompt",
			Tools:               []string{"submit_result"},
		},
	}
}

func newFleetNode(name string) *foremanv1alpha1.FleetNode {
	return &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: foremanv1alpha1.FleetNodeSpec{
			NodeName: name,
			Roles:    []string{"worker"},
		},
	}
}

func setPhase(task *foremanv1alpha1.AgenticTask, phase foremanv1alpha1.AgenticTaskPhase) {
	GinkgoHelper()
	patch := client.MergeFrom(task.DeepCopy())
	task.Status.Phase = phase
	Expect(k8sClient.Status().Patch(ctx, task, patch)).To(Succeed())
}

func setNodeReady(node *foremanv1alpha1.FleetNode, cap foremanv1alpha1.FleetNodeCapability) {
	GinkgoHelper()
	patch := client.MergeFrom(node.DeepCopy())
	node.Status.Phase = foremanv1alpha1.FleetNodePhaseReady
	node.Status.Capability = cap
	now := metav1.Now()
	node.Status.LastHeartbeatTime = &now
	Expect(k8sClient.Status().Patch(ctx, node, patch)).To(Succeed())
}

func reqFor(obj client.Object) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}}
}

func nn(obj client.Object) types.NamespacedName {
	return types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
}

func findCondition(conds []metav1.Condition, kind string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == kind {
			return &conds[i]
		}
	}
	return nil
}
