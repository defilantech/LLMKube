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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// M6 ships the WorkloadReconciler with a deterministic stub planner.
// These tests pin the three modes (explicit pipeline, issue-batch
// shortcut, no-planner failure) plus the status rollup behavior.

var _ = Describe("WorkloadReconciler (M6 stub planner)", func() {
	var reconciler *WorkloadReconciler

	BeforeEach(func() {
		reconciler = &WorkloadReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("returns no error when the Workload is not found", func() {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: "absent"},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("fails an intent-only Workload with NoPlannerOrPipeline", func() {
		wl := newWorkload("intent-only", foremanv1alpha1.WorkloadSpec{
			Intent: "do something the v0.2 planner will figure out",
			Repo:   "defilantech/LLMKube",
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, wl) })

		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(wl),
		})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseFailed))
		planned := findCondition(fresh.Status.Conditions, conditionTypePlanned)
		Expect(planned).NotTo(BeNil())
		Expect(planned.Reason).To(Equal("NoPlannerOrPipeline"))
		Expect(fresh.Status.Tasks).To(BeEmpty())
	})

	It("emits one AgenticTask per PipelineStep in explicit-pipeline mode and rewrites DependsOn", func() {
		wl := newWorkload("pipeline-explicit", foremanv1alpha1.WorkloadSpec{
			Intent: "explicit",
			Repo:   "defilantech/LLMKube",
			Pipeline: []foremanv1alpha1.PipelineStep{
				{
					Name:     "step-a",
					Kind:     foremanv1alpha1.AgenticTaskKindIssueFix,
					AgentRef: corev1.LocalObjectReference{Name: "coder"},
					Payload:  foremanv1alpha1.AgenticTaskPayload{Repo: "defilantech/LLMKube", Issue: 1234},
				},
				{
					Name:      "step-b",
					Kind:      foremanv1alpha1.AgenticTaskKindVerify,
					AgentRef:  corev1.LocalObjectReference{Name: "gate"},
					DependsOn: []string{"step-a"},
					Payload:   foremanv1alpha1.AgenticTaskPayload{Repo: "defilantech/LLMKube", Issue: 1234, Branch: "foreman/issue-1234"},
				},
			},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhasePlanned))
		Expect(fresh.Status.Tasks).To(HaveLen(2))
		planned := findCondition(fresh.Status.Conditions, conditionTypePlanned)
		Expect(planned).NotTo(BeNil())
		Expect(planned.Reason).To(Equal("PlannerSucceeded"))

		var stepB foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "pipeline-explicit-step-b"}, &stepB)).To(Succeed())
		Expect(stepB.Spec.DependsOn).To(ConsistOf("pipeline-explicit-step-a"))
		Expect(stepB.Labels[labelWorkload]).To(Equal("pipeline-explicit"))
		Expect(stepB.Labels[labelStep]).To(Equal("step-b"))
		Expect(stepB.OwnerReferences).To(HaveLen(1))
		Expect(stepB.OwnerReferences[0].Name).To(Equal("pipeline-explicit"))
		Expect(*stepB.OwnerReferences[0].Controller).To(BeTrue())
	})

	It("expands Issues into code+verify pairs in issue-batch mode", func() {
		wl := newWorkload("batch-shortcut", foremanv1alpha1.WorkloadSpec{
			Intent:           "batch",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{531, 510},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhasePlanned))
		Expect(fresh.Status.Tasks).To(HaveLen(4)) // 2 issues * (code + verify)

		var code531 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "batch-shortcut-code-531"}, &code531)).To(Succeed())
		Expect(code531.Spec.Kind).To(Equal(foremanv1alpha1.AgenticTaskKindIssueFix))
		Expect(code531.Spec.Payload.Issue).To(Equal(int32(531)))
		Expect(code531.Spec.Payload.Branch).To(Equal("foreman/issue-531"))
		Expect(code531.Spec.AgentRef.Name).To(Equal("coder"))

		var verify531 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "batch-shortcut-verify-531"}, &verify531)).To(Succeed())
		Expect(verify531.Spec.Kind).To(Equal(foremanv1alpha1.AgenticTaskKindVerify))
		Expect(verify531.Spec.DependsOn).To(ConsistOf("batch-shortcut-code-531"))
		Expect(verify531.Spec.AgentRef.Name).To(Equal("gate"))
	})

	It("clips with MaxTasks and sets the Truncated condition", func() {
		wl := newWorkload("max-tasks-clip", foremanv1alpha1.WorkloadSpec{
			Intent:           "clipped",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{1, 2, 3, 4, 5}, // would expand to 10 tasks (5 pairs)
			MaxTasks:         4,                      // keep only first 2 pairs after pairing
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Tasks).To(HaveLen(4))
		truncated := findCondition(fresh.Status.Conditions, conditionTypeTruncated)
		Expect(truncated).NotTo(BeNil())
		Expect(truncated.Status).To(Equal(metav1.ConditionTrue))
	})

	It("rolls up child task phases into the Workload status", func() {
		wl := newWorkload("rollup-test", foremanv1alpha1.WorkloadSpec{
			Intent:           "rollup",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{42},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		// First reconcile creates the children (both Pending).
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		// Force both children Succeeded via status patch.
		for _, name := range []string{"rollup-test-code-42", "rollup-test-verify-42"} {
			var t foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &t)).To(Succeed())
			patch := client.MergeFrom(t.DeepCopy())
			t.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
			Expect(k8sClient.Status().Patch(ctx, &t, patch)).To(Succeed())
		}

		// Second reconcile rolls up.
		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseCompleted))
		Expect(fresh.Status.SucceededTasks).To(Equal(int32(2)))
		Expect(fresh.Status.FailedTasks).To(Equal(int32(0)))
		completed := findCondition(fresh.Status.Conditions, conditionTypeCompleted)
		Expect(completed).NotTo(BeNil())
		Expect(completed.Reason).To(Equal("AllChildrenSucceeded"))
	})

	It("rolls up to Failed when any child terminally fails and none are in flight", func() {
		wl := newWorkload("rollup-fail", foremanv1alpha1.WorkloadSpec{
			Intent:           "rollup-fail",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{99},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		// One child Succeeded, the other Failed -> terminal Failed rollup.
		updates := []struct {
			name  string
			phase foremanv1alpha1.AgenticTaskPhase
		}{
			{"rollup-fail-code-99", foremanv1alpha1.AgenticTaskPhaseSucceeded},
			{"rollup-fail-verify-99", foremanv1alpha1.AgenticTaskPhaseFailed},
		}
		for _, u := range updates {
			var t foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: u.name}, &t)).To(Succeed())
			patch := client.MergeFrom(t.DeepCopy())
			t.Status.Phase = u.phase
			Expect(k8sClient.Status().Patch(ctx, &t, patch)).To(Succeed())
		}

		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseFailed))
		Expect(fresh.Status.SucceededTasks).To(Equal(int32(1)))
		Expect(fresh.Status.FailedTasks).To(Equal(int32(1)))
		completed := findCondition(fresh.Status.Conditions, conditionTypeCompleted)
		Expect(completed).NotTo(BeNil())
		Expect(completed.Reason).To(Equal("ChildrenFailed"))
	})
})

// newWorkload builds a Workload with the test-conventional shape. Lets the
// individual `It` blocks stay focused on the spec under test.
func newWorkload(name string, spec foremanv1alpha1.WorkloadSpec) *foremanv1alpha1.Workload {
	return &foremanv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spec,
	}
}

// findCondition lives in agentictask_controller_test.go; reuse it.

// cleanupChildren best-effort deletes the AgenticTasks an `It` block's
// Workload rendered. envtest does not run the garbage collector, so
// owner refs alone do not cascade-delete; the AgenticTasks must be
// removed explicitly between tests. Uses the suite-level ctx so
// DeferCleanup callbacks (which forbid re-entering DeferCleanup) can
// invoke us directly.
func cleanupChildren(w *foremanv1alpha1.Workload) {
	var list foremanv1alpha1.AgenticTaskList
	if err := k8sClient.List(ctx, &list,
		client.InNamespace(w.Namespace),
		client.MatchingLabels{labelWorkload: w.Name},
	); err != nil {
		return
	}
	for i := range list.Items {
		_ = k8sClient.Delete(ctx, &list.Items[i])
	}
}
