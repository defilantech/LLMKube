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
	"k8s.io/apimachinery/pkg/runtime"
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

	// Regression for defilantech/LLMKube#970. A Pending task whose
	// dependency ended ALREADY-RESOLVED (Phase=Succeeded +
	// Verdict=NO-GO + extra.outcome="ALREADY-RESOLVED") must NOT be
	// cascade-failed. The dependent is transitioned to
	// Phase=Succeeded + Verdict=Skipped with a Skipped condition
	// (Reason=UpstreamAlreadyResolved) so the Workload rollup
	// excludes it from every bucket (it doesn't pin the Workload to
	// Failed). Cascade-skip runs BEFORE cascade-fail in Reconcile so
	// an ALREADY-RESOLVED dep never trips the "terminal without
	// success" branch.
	It("cascade-skips a Pending task when its dependency ended ALREADY-RESOLVED (#970)", func() {
		dep := newTask("cascade-skip-dep")
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })
		// Mark the dep as ALREADY-RESOLVED via Phase=Succeeded +
		// Verdict=NO-GO + Status.Result envelope. The cascade path
		// inspects all three (via isAlreadyResolvedCoder).
		setPhase(dep, foremanv1alpha1.AgenticTaskPhaseSucceeded)
		patch := client.MergeFrom(dep.DeepCopy())
		dep.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
		dep.Status.Result = &runtime.RawExtension{
			Raw: []byte(`{"summary":"already done","extra":{"outcome":"ALREADY-RESOLVED","resolvedBy":"sha-deadbeef"}}`),
		}
		Expect(k8sClient.Status().Patch(ctx, dep, patch)).To(Succeed())

		target := newTask("cascade-skip-target")
		target.Spec.DependsOn = []string{dep.Name}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, target) })
		setPhase(target, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(target))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(target), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseSucceeded))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictSkipped))
		Expect(fresh.Status.FinishedAt).NotTo(BeNil())

		skippedCond := findCondition(fresh.Status.Conditions, "Skipped")
		Expect(skippedCond).NotTo(BeNil())
		Expect(skippedCond.Reason).To(Equal("UpstreamAlreadyResolved"))
		Expect(skippedCond.Message).To(ContainSubstring(dep.Name))

		// Skipped must NOT trigger the Failed condition — that's the
		// whole point of the skip path.
		failedCond := findCondition(fresh.Status.Conditions, "Failed")
		Expect(failedCond).To(BeNil(), "a Skipped dependent must not carry a Failed condition")
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

	It("schedules a vulkan task onto a vulkan node and not a metal node", func() {
		// The AMD/Vulkan tier advertises accelerator=vulkan; a task pinned to
		// vulkan must land on that node and never on a metal node. Creating the
		// node and task also exercises the CRD enum (envtest validates the
		// vulkan value on create).
		metalNode := newFleetNode("metal-box")
		Expect(k8sClient.Create(ctx, metalNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, metalNode) })
		setNodeReady(metalNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:  64, AvailableRAMGB: 48,
		})

		vulkanNode := newFleetNode("vulkan-box")
		Expect(k8sClient.Create(ctx, vulkanNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, vulkanNode) })
		setNodeReady(vulkanNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("vulkan"),
			TotalRAMGB:  128, AvailableRAMGB: 100,
		})

		task := newTask("vulkan-target")
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("vulkan"),
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(vulkanNode.Name))
	})

	It("spreads two Pending tasks across two Ready nodes (one task per node)", func() {
		// Regression for defilantech/LLMKube#977: the scheduler must not funnel
		// every task onto the alphabetically-first node. With two idle nodes,
		// two Pending tasks must land on different nodes, each node advertising
		// its reserved task via Status.CurrentTask.
		nodeA := newFleetNode("spread-a")
		Expect(k8sClient.Create(ctx, nodeA)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nodeA) })
		setNodeReady(nodeA, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})
		nodeB := newFleetNode("spread-b")
		Expect(k8sClient.Create(ctx, nodeB)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nodeB) })
		setNodeReady(nodeB, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})

		t1 := newTask("spread-t1")
		Expect(k8sClient.Create(ctx, t1)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t1) })
		setPhase(t1, foremanv1alpha1.AgenticTaskPhasePending)
		t2 := newTask("spread-t2")
		Expect(k8sClient.Create(ctx, t2)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t2) })
		setPhase(t2, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(t1))
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reqFor(t2))
		Expect(err).NotTo(HaveOccurred())

		var f1, f2 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(t1), &f1)).To(Succeed())
		Expect(k8sClient.Get(ctx, nn(t2), &f2)).To(Succeed())
		Expect(f1.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(f2.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(f1.Status.AssignedNode).NotTo(BeEmpty())
		Expect(f2.Status.AssignedNode).NotTo(BeEmpty())
		Expect(f1.Status.AssignedNode).NotTo(Equal(f2.Status.AssignedNode))

		var na, nb foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeA.Name}, &na)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeB.Name}, &nb)).To(Succeed())
		Expect([]string{na.Status.CurrentTask, nb.Status.CurrentTask}).
			To(ConsistOf("default/spread-t1", "default/spread-t2"))
	})

	It("leaves a second task Pending when the only matching node is busy", func() {
		// One node, two tasks: the second must wait (requeue) rather than
		// double-book the node.
		node := newFleetNode("solo-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})

		t1 := newTask("solo-t1")
		Expect(k8sClient.Create(ctx, t1)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t1) })
		setPhase(t1, foremanv1alpha1.AgenticTaskPhasePending)
		t2 := newTask("solo-t2")
		Expect(k8sClient.Create(ctx, t2)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t2) })
		setPhase(t2, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(t1))
		Expect(err).NotTo(HaveOccurred())

		// Anchor the precondition: t1 must actually hold the node, otherwise
		// this spec passes vacuously when nothing schedules at all.
		var busy foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &busy)).To(Succeed())
		Expect(busy.Status.CurrentTask).To(Equal("default/solo-t1"))

		res, err := reconciler.Reconcile(ctx, reqFor(t2))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))

		var f2 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(t2), &f2)).To(Succeed())
		Expect(f2.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(f2.Status.AssignedNode).To(BeEmpty())
	})

	It("frees the node when its task terminates, letting the next task schedule there", func() {
		// The reservation must be released on a terminal task so the freed node
		// becomes available again.
		node := newFleetNode("recycle-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})

		t1 := newTask("recycle-t1")
		Expect(k8sClient.Create(ctx, t1)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t1) })
		setPhase(t1, foremanv1alpha1.AgenticTaskPhasePending)
		_, err := reconciler.Reconcile(ctx, reqFor(t1))
		Expect(err).NotTo(HaveOccurred())

		var busy foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &busy)).To(Succeed())
		Expect(busy.Status.CurrentTask).To(Equal("default/recycle-t1"))

		// Agent finishes the task; the controller reconcile of the terminal
		// task must release the node.
		setPhase(t1, foremanv1alpha1.AgenticTaskPhaseSucceeded)
		_, err = reconciler.Reconcile(ctx, reqFor(t1))
		Expect(err).NotTo(HaveOccurred())

		var freed foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &freed)).To(Succeed())
		Expect(freed.Status.CurrentTask).To(BeEmpty())

		// A new task now schedules onto the recycled node.
		t2 := newTask("recycle-t2")
		Expect(k8sClient.Create(ctx, t2)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t2) })
		setPhase(t2, foremanv1alpha1.AgenticTaskPhasePending)
		_, err = reconciler.Reconcile(ctx, reqFor(t2))
		Expect(err).NotTo(HaveOccurred())

		var f2 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(t2), &f2)).To(Succeed())
		Expect(f2.Status.AssignedNode).To(Equal(node.Name))
	})

	It("reclaims a node whose CurrentTask points at a task that no longer exists", func() {
		// Defense against a leaked reservation (a task deleted mid-flight): a
		// node's stale CurrentTask must not wedge it busy forever.
		node := newFleetNode("stale-res-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})
		patch := client.MergeFrom(node.DeepCopy())
		node.Status.CurrentTask = "default/ghost-task-that-never-existed"
		Expect(k8sClient.Status().Patch(ctx, node, patch)).To(Succeed())

		task := newTask("reclaim-task")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)
		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))

		var reclaimed foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &reclaimed)).To(Succeed())
		Expect(reclaimed.Status.CurrentTask).To(Equal("default/reclaim-task"))
	})

	It("does not reschedule a Running task whose assigned node is fresh", func() {
		// The scheduler must leave Running tasks alone when the FleetNode is
		// healthy; only claim expiry (stale/absent node) may alter them.
		node := newFleetNode("hands-off-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})

		task := newTask("hands-off")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhaseRunning)

		patch := client.MergeFrom(task.DeepCopy())
		task.Status.AssignedNode = node.Name
		Expect(k8sClient.Status().Patch(ctx, task, patch)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseRunning))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))
	})

	It("skips a stale-heartbeat Ready node and schedules to the fresh one", func() {
		// Regression: firstFitNode must not dispatch to a node whose agent has
		// gone dark even though Phase=Ready has not yet been reconciled to
		// NotReady. The stale node is given an alphabetically-earlier name so
		// it would sort first under a naive Phase-only filter, proving the
		// heartbeat gate is active. Both nodes advertise the same metal
		// capability so only the heartbeat check distinguishes them.
		staleNode := newFleetNode("aaa-sched-stale-node")
		Expect(k8sClient.Create(ctx, staleNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, staleNode) })
		// Mark Phase=Ready but with a heartbeat 5 minutes in the past.
		setStaleNodeReady(staleNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     64,
			AvailableRAMGB: 48,
		})

		freshNode := newFleetNode("zzz-sched-fresh-node")
		Expect(k8sClient.Create(ctx, freshNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, freshNode) })
		setNodeReady(freshNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     64,
			AvailableRAMGB: 48,
		})

		task := newTask("skip-stale-target")
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		// Must land on the fresh node, not the alphabetically-first stale one.
		Expect(fresh.Status.AssignedNode).To(Equal(freshNode.Name))
	})

	It("leaves a Pending task unscheduled when all Ready nodes have stale heartbeats", func() {
		// Regression: if every Phase=Ready node has a stale heartbeat, the
		// scheduler must return no-fit and requeue rather than dispatching to
		// a dead node.
		staleNode := newFleetNode("all-stale-only-node")
		Expect(k8sClient.Create(ctx, staleNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, staleNode) })
		setStaleNodeReady(staleNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     64,
			AvailableRAMGB: 48,
		})

		task := newTask("all-stale-pending-task")
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		res, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())
		// No fit: must requeue.
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		// Task must remain Pending with no assigned node.
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(fresh.Status.AssignedNode).To(BeEmpty())
	})
})

var _ = Describe("capabilitySatisfies jobMode", func() {
	// In Job mode the model is remote (an in-cluster cuda InferenceService
	// or an external URL) and the agent loop runs in an ephemeral Job, so
	// the claiming FleetNode only needs the role + nodeSelector. The
	// accelerator / installedModels / RAM / context gates that bind a
	// node to a locally-resident model must be skipped. See #620.

	newEmptyCapNode := func() *foremanv1alpha1.FleetNode {
		// Ready-shaped node with an EMPTY capability: no accelerator,
		// AvailableRAMGB 0, no installed models, no context. Only the
		// worker role is set.
		return &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: "empty-cap-node"},
			Spec: foremanv1alpha1.FleetNodeSpec{
				NodeName: "empty-cap-node",
				Roles:    []string{"worker"},
			},
		}
	}

	It("gates on accelerator/RAM in InProcess mode (jobMode=false)", func() {
		req := foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("cuda"),
			MinRAMGB:    16,
		}
		node := newEmptyCapNode()
		Expect(capabilitySatisfies(req, "", node, false)).To(BeFalse())
	})

	It("skips accelerator/RAM gates in Job mode (jobMode=true)", func() {
		req := foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("cuda"),
			MinRAMGB:    16,
		}
		node := newEmptyCapNode()
		Expect(capabilitySatisfies(req, "", node, true)).To(BeTrue())
	})

	It("still enforces roles in Job mode", func() {
		req := foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("cuda"),
			MinRAMGB:    16,
			Roles:       []string{"verifier"},
		}
		node := newEmptyCapNode() // worker-only, no verifier role
		Expect(capabilitySatisfies(req, "", node, true)).To(BeFalse())
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

// setStaleNodeReady puts a node in Phase=Ready with the given capability but
// with a LastHeartbeatTime 5 minutes in the past, making it appear alive to a
// Phase-only check but dead to nodeSchedulable's heartbeat gate.
func setStaleNodeReady(node *foremanv1alpha1.FleetNode, cap foremanv1alpha1.FleetNodeCapability) {
	GinkgoHelper()
	patch := client.MergeFrom(node.DeepCopy())
	node.Status.Phase = foremanv1alpha1.FleetNodePhaseReady
	node.Status.Capability = cap
	stale := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	node.Status.LastHeartbeatTime = &stale
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
