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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
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
			// Default to permissive in tests; the cloud-suppression
			// scenarios flip this explicitly per-It.
			AllowCloudProviders: true,
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
		// #573: branch now includes the workload name to disambiguate
		// reruns on the same issue across Workloads.
		Expect(code531.Spec.Payload.Branch).To(Equal("foreman/batch-shortcut/issue-531"))
		Expect(code531.Spec.AgentRef.Name).To(Equal("coder"))

		var verify531 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "batch-shortcut-verify-531"}, &verify531)).To(Succeed())
		Expect(verify531.Spec.Kind).To(Equal(foremanv1alpha1.AgenticTaskKindVerify))
		Expect(verify531.Spec.DependsOn).To(ConsistOf("batch-shortcut-code-531"))
		Expect(verify531.Spec.AgentRef.Name).To(Equal("gate"))
	})

	It("propagates a Workload-level GateProfile to every decomposed task", func() {
		wl := newWorkload("batch-node-gate", foremanv1alpha1.WorkloadSpec{
			Intent:           "batch on a node repo",
			Repo:             "misospace/miso-chat",
			Issues:           []int32{42},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			GateProfile: &foremanv1alpha1.GateProfile{
				Language: foremanv1alpha1.GateLanguageNode,
			},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		// Both the coder task (drives the in-loop self-gate) and the verify
		// task (the clean-room gate Job) must carry the node profile, or the
		// pipeline falls back to the Go gate on a non-Go repo.
		var code42 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "batch-node-gate-code-42"}, &code42)).To(Succeed())
		Expect(code42.Spec.GateProfile).NotTo(BeNil())
		Expect(code42.Spec.GateProfile.Language).To(Equal(foremanv1alpha1.GateLanguageNode))

		var verify42 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "batch-node-gate-verify-42"}, &verify42)).To(Succeed())
		Expect(verify42.Spec.GateProfile).NotTo(BeNil())
		Expect(verify42.Spec.GateProfile.Language).To(Equal(foremanv1alpha1.GateLanguageNode))
	})

	It("fans out one review task per ReviewerAgentRef, each depending on the verify task (v0.2)", func() {
		wl := newWorkload("batch-with-reviewers", foremanv1alpha1.WorkloadSpec{
			Intent:           "batch + reviewers",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{510, 531},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "reviewer-validator"},
				{Name: "reviewer-falsification"},
				{Name: "reviewer-thinking"},
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
		// 2 issues * (code + verify + 3 reviewers) = 10 tasks total.
		Expect(fresh.Status.Tasks).To(HaveLen(10))

		// review-510-1 (the second reviewer of the first issue): kind +
		// agentRef + dependsOn must all line up.
		var review5101 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: "default", Name: "batch-with-reviewers-review-510-1",
		}, &review5101)).To(Succeed())
		Expect(review5101.Spec.Kind).To(Equal(foremanv1alpha1.AgenticTaskKindReview))
		Expect(review5101.Spec.AgentRef.Name).To(Equal("reviewer-falsification"))
		Expect(review5101.Spec.DependsOn).To(ConsistOf("batch-with-reviewers-verify-510"))
		Expect(review5101.Spec.Payload.Issue).To(Equal(int32(510)))
		Expect(review5101.Spec.Payload.Branch).To(Equal("foreman/batch-with-reviewers/issue-510"))

		// review-531-2: the third reviewer of the second issue. Confirms
		// the cross-product expansion (per-issue x per-reviewer).
		var review5312 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: "default", Name: "batch-with-reviewers-review-531-2",
		}, &review5312)).To(Succeed())
		Expect(review5312.Spec.AgentRef.Name).To(Equal("reviewer-thinking"))
		Expect(review5312.Spec.DependsOn).To(ConsistOf("batch-with-reviewers-verify-531"))

		// review-510-0 must NOT depend on review-510-1 or any other
		// reviewer: parallel-after-gate is the whole point.
		var review5100 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: "default", Name: "batch-with-reviewers-review-510-0",
		}, &review5100)).To(Succeed())
		Expect(review5100.Spec.DependsOn).To(ConsistOf("batch-with-reviewers-verify-510"))
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

	It("suppresses cloud-provider reviewers when the operator kill switch is off (v0.2 #115)", func() {
		// Seed two reviewer Agents: one local (existing pattern), one
		// cloud-proxy. Operator-level allowCloudProviders=false should
		// keep the cloud Agent out of the rendered set and write a
		// CloudReviewersSuppressed condition.
		reconciler.AllowCloudProviders = false
		DeferCleanup(func() { reconciler.AllowCloudProviders = true })

		Expect(k8sClient.Create(ctx, &foremanv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "local-validator", Namespace: "default"},
			Spec: foremanv1alpha1.AgentSpec{
				Role:                foremanv1alpha1.AgentRoleReviewer,
				Provider:            foremanv1alpha1.AgentProviderLocal,
				InferenceServiceRef: corev1.LocalObjectReference{Name: "test-svc"},
				SystemPrompt:        "you are a reviewer",
				Tools:               []string{"submit_result"},
				MaxTurns:            5,
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &foremanv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "cloud-sonnet", Namespace: "default"},
			Spec: foremanv1alpha1.AgentSpec{
				Role:     foremanv1alpha1.AgentRoleReviewer,
				Provider: foremanv1alpha1.AgentProviderCloudProxy,
				ProviderConfig: &foremanv1alpha1.ProviderConfig{
					BaseURL: "http://foundation-router.lan:4000/v1",
					Model:   "claude-sonnet-4-6",
				},
				SystemPrompt: "you are a cloud reviewer",
				Tools:        []string{"submit_result"},
				MaxTurns:     5,
			},
		})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &foremanv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "local-validator", Namespace: "default"}})
			_ = k8sClient.Delete(ctx, &foremanv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "cloud-sonnet", Namespace: "default"}})
		})

		wl := newWorkload("cloud-suppressed-operator", foremanv1alpha1.WorkloadSpec{
			Intent:           "should suppress cloud reviewer",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{777},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "local-validator"},
				{Name: "cloud-sonnet"},
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
		// code-777 + verify-777 + review-777-0 (local) = 3 tasks; cloud
		// reviewer was filtered out.
		Expect(fresh.Status.Tasks).To(HaveLen(3))
		cond := findCondition(fresh.Status.Conditions, conditionTypeCloudReviewersSuppressed)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Message).To(ContainSubstring("cloud-sonnet"))
		Expect(cond.Message).To(ContainSubstring("operator --allow-cloud-providers=false"))
	})

	It("suppresses cloud-provider reviewers when spec.allowCloudReviewers is false (v0.2 #115)", func() {
		// Operator allows cloud; workload explicitly opts out. The
		// suppression message should name the workload-level gate as
		// the blocker, not the operator.
		Expect(k8sClient.Create(ctx, &foremanv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "cloud-sonnet-w", Namespace: "default"},
			Spec: foremanv1alpha1.AgentSpec{
				Role:     foremanv1alpha1.AgentRoleReviewer,
				Provider: foremanv1alpha1.AgentProviderCloudProxy,
				ProviderConfig: &foremanv1alpha1.ProviderConfig{
					BaseURL: "http://foundation-router.lan:4000/v1",
					Model:   "claude-sonnet-4-6",
				},
				SystemPrompt: "you are a cloud reviewer",
				Tools:        []string{"submit_result"},
				MaxTurns:     5,
			},
		})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &foremanv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "cloud-sonnet-w", Namespace: "default"}})
		})

		falseVal := false
		wl := newWorkload("cloud-suppressed-workload", foremanv1alpha1.WorkloadSpec{
			Intent:              "workload opts out of cloud",
			Repo:                "defilantech/LLMKube",
			Issues:              []int32{888},
			CoderAgentRef:       &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef:    &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs:   []corev1.LocalObjectReference{{Name: "cloud-sonnet-w"}},
			AllowCloudReviewers: &falseVal,
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
		Expect(fresh.Status.Tasks).To(HaveLen(2)) // code + verify only
		cond := findCondition(fresh.Status.Conditions, conditionTypeCloudReviewersSuppressed)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Message).To(ContainSubstring("workload spec.allowCloudReviewers=false"))
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

		// Force both children Succeeded via status patch. As of #541
		// the rollup distinguishes on-target Succeeded (verdict in
		// {GO, GATE-PASS}) from terminal-without-output Succeeded;
		// the coder gets verdict=GO and the verifier verdict=GATE-PASS
		// so SucceededOnTarget() returns true for both.
		on := []struct {
			name    string
			verdict foremanv1alpha1.AgenticTaskVerdict
		}{
			{"rollup-test-code-42", foremanv1alpha1.AgenticTaskVerdictGo},
			{"rollup-test-verify-42", foremanv1alpha1.AgenticTaskVerdictGatePass},
		}
		for _, e := range on {
			var t foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: e.name}, &t)).To(Succeed())
			patch := client.MergeFrom(t.DeepCopy())
			t.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
			t.Status.Verdict = e.verdict
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

		// One child on-target Succeeded (GO), the other Failed.
		// Terminal Failed rollup.
		updates := []struct {
			name    string
			phase   foremanv1alpha1.AgenticTaskPhase
			verdict foremanv1alpha1.AgenticTaskVerdict
		}{
			{"rollup-fail-code-99", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictGo},
			{"rollup-fail-verify-99", foremanv1alpha1.AgenticTaskPhaseFailed, ""},
		}
		for _, u := range updates {
			var t foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: u.name}, &t)).To(Succeed())
			patch := client.MergeFrom(t.DeepCopy())
			t.Status.Phase = u.phase
			t.Status.Verdict = u.verdict
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

	// Regression for defilantech/LLMKube#541. Phase=Succeeded with a
	// non-positive verdict (INCOMPLETE, NO-GO, GATE-FAIL, GATE-ERROR)
	// must NOT count as a win in the rollup. The Memorial Day v5 batch
	// reported "12/12 child tasks Succeeded" + Phase=Completed when
	// 2 children were INCOMPLETE and 2 were GATE-FAIL; the new
	// IncompleteTasks counter + Failed-on-incomplete phase logic
	// makes that report honest.
	It("rolls up Phase=Succeeded + verdict=INCOMPLETE into incompleteTasks and Failed phase (#541)", func() {
		wl := newWorkload("rollup-incomplete", foremanv1alpha1.WorkloadSpec{
			Intent:           "rollup-incomplete",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{1234},
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

		// Coder Succeeded but INCOMPLETE (MaxTurnsExhausted shape);
		// verifier Succeeded with GATE-FAIL (it ran but the gate's
		// `make` checks did not pass — also a terminal-without-output).
		updates := []struct {
			name    string
			phase   foremanv1alpha1.AgenticTaskPhase
			verdict foremanv1alpha1.AgenticTaskVerdict
		}{
			{"rollup-incomplete-code-1234", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictIncomplete},
			{"rollup-incomplete-verify-1234", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictGateFail},
		}
		for _, u := range updates {
			var t foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: u.name}, &t)).To(Succeed())
			patch := client.MergeFrom(t.DeepCopy())
			t.Status.Phase = u.phase
			t.Status.Verdict = u.verdict
			Expect(k8sClient.Status().Patch(ctx, &t, patch)).To(Succeed())
		}

		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		// Previously: Phase=Completed, SucceededTasks=2, FailedTasks=0.
		// Now: Phase=Failed, SucceededTasks=0, FailedTasks=0,
		//      IncompleteTasks=2, reason=ChildrenIncomplete.
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseFailed),
			"workload with all-incomplete children must NOT roll to Completed")
		Expect(fresh.Status.SucceededTasks).To(Equal(int32(0)))
		Expect(fresh.Status.FailedTasks).To(Equal(int32(0)))
		Expect(fresh.Status.IncompleteTasks).To(Equal(int32(2)))
		completed := findCondition(fresh.Status.Conditions, conditionTypeCompleted)
		Expect(completed).NotTo(BeNil())
		Expect(completed.Reason).To(Equal("ChildrenIncomplete"))
	})

	// Regression for defilantech/LLMKube#970. A coder that ends
	// Phase=Succeeded + Verdict=NO-GO with extra.outcome="ALREADY-RESOLVED"
	// must NOT count as incomplete and must NOT pin the Workload to
	// Failed. The Workload rolls to Completed with reason
	// AllAlreadyResolved, and a CoderAlreadyResolved condition surfaces
	// the issue numbers so the operator can close them.
	It("rolls up ALREADY-RESOLVED children to Completed with AllAlreadyResolved (#970)", func() {
		wl := newWorkload("rollup-already-resolved", foremanv1alpha1.WorkloadSpec{
			Intent:           "already-resolved at run time",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{152, 365},
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

		// Both coder children end NO-GO + ALREADY-RESOLVED. In production
		// the cascade-fail logic in AgenticTaskReconciler would fail the
		// verify children (Phase=Failed via cascadeFailIfDepFailed) — the
		// envtest here runs only the WorkloadReconciler, so the verify
		// children would stay Pending and pin the rollup to Dispatched.
		// To isolate the rollup behavior under test, delete the verify
		// children explicitly. The production cascade-fail interaction
		// with ALREADY-RESOLVED is a known gap tracked for follow-up.
		for _, n := range []string{"rollup-already-resolved-code-152", "rollup-already-resolved-code-365"} {
			var t foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: n}, &t)).To(Succeed())
			patch := client.MergeFrom(t.DeepCopy())
			t.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
			t.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
			t.Status.Result = resultRaw("ALREADY-RESOLVED", "", "already resolved by prior fix")
			Expect(k8sClient.Status().Patch(ctx, &t, patch)).To(Succeed())
		}
		for _, n := range []string{"rollup-already-resolved-verify-152", "rollup-already-resolved-verify-365"} {
			var t foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: n}, &t)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &t)).To(Succeed())
		}

		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseCompleted))
		Expect(fresh.Status.IncompleteTasks).To(Equal(int32(0)))
		Expect(fresh.Status.FailedTasks).To(Equal(int32(0)))
		completed := findCondition(fresh.Status.Conditions, conditionTypeCompleted)
		Expect(completed).NotTo(BeNil())
		Expect(completed.Reason).To(Equal("AllAlreadyResolved"))
		Expect(completed.Message).To(ContainSubstring("152"))
		Expect(completed.Message).To(ContainSubstring("365"))

		resolved := findCondition(fresh.Status.Conditions, conditionTypeCoderAlreadyResolved)
		Expect(resolved).NotTo(BeNil())
		Expect(resolved.Status).To(Equal(metav1.ConditionTrue))
		Expect(resolved.Reason).To(Equal("AlreadyResolved"))
		Expect(resolved.Message).To(ContainSubstring("#152"))
		Expect(resolved.Message).To(ContainSubstring("#365"))
	})

	// Mixed-case for #970: one issue gets a real fix (coder GO +
	// verify GATE-PASS), one is ALREADY-RESOLVED at run time. The
	// Workload rolls to Completed with reason AllChildrenSucceeded and
	// the message names both buckets.
	It("rolls up a mixed already-resolved + succeeded Workload to Completed (#970)", func() {
		wl := newWorkload("rollup-mixed-ar", foremanv1alpha1.WorkloadSpec{
			Intent:           "mixed already-resolved + succeeded",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{11, 22},
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

		// Issue 11: real fix path.
		for _, u := range []struct {
			name    string
			phase   foremanv1alpha1.AgenticTaskPhase
			verdict foremanv1alpha1.AgenticTaskVerdict
		}{
			{"rollup-mixed-ar-code-11", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictGo},
			{"rollup-mixed-ar-verify-11", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictGatePass},
		} {
			var t foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: u.name}, &t)).To(Succeed())
			patch := client.MergeFrom(t.DeepCopy())
			t.Status.Phase = u.phase
			t.Status.Verdict = u.verdict
			Expect(k8sClient.Status().Patch(ctx, &t, patch)).To(Succeed())
		}

		// Issue 22: ALREADY-RESOLVED. Same cascade-fail isolation as above —
// delete the verify-22 child so the rollup sees only the resolved coder.
		var ar foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "rollup-mixed-ar-code-22"}, &ar)).To(Succeed())
		patch := client.MergeFrom(ar.DeepCopy())
		ar.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		ar.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
		ar.Status.Result = resultRaw("ALREADY-RESOLVED", "", "already done")
		Expect(k8sClient.Status().Patch(ctx, &ar, patch)).To(Succeed())
		var v22 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "rollup-mixed-ar-verify-22"}, &v22)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &v22)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseCompleted))
		Expect(fresh.Status.SucceededTasks).To(Equal(int32(2)))
		Expect(fresh.Status.IncompleteTasks).To(Equal(int32(0)))
		completed := findCondition(fresh.Status.Conditions, conditionTypeCompleted)
		Expect(completed).NotTo(BeNil())
		Expect(completed.Reason).To(Equal("AllChildrenSucceeded"))
		Expect(completed.Message).To(ContainSubstring("already-resolved"))

		resolved := findCondition(fresh.Status.Conditions, conditionTypeCoderAlreadyResolved)
		Expect(resolved).NotTo(BeNil())
		Expect(resolved.Message).To(ContainSubstring("#22"))
		Expect(resolved.Message).NotTo(ContainSubstring("#11"))
	})

	It("emits a per-issue AlreadyResolved event via the Recorder (#970)", func() {
		recorder := &events.FakeRecorder{Events: make(chan string, 16)}
		rec := &WorkloadReconciler{
			Client:              k8sClient,
			Scheme:              k8sClient.Scheme(),
			Recorder:            recorder,
			AllowCloudProviders: true,
		}
		wl := newWorkload("rollup-ar-events", foremanv1alpha1.WorkloadSpec{
			Intent:           "already-resolved events",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{777},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		_, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var c foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "rollup-ar-events-code-777"}, &c)).To(Succeed())
		patch := client.MergeFrom(c.DeepCopy())
		c.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		c.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
		c.Status.Result = resultRaw("ALREADY-RESOLVED", "", "already done")
		Expect(k8sClient.Status().Patch(ctx, &c, patch)).To(Succeed())
		// Same cascade-fail isolation: delete the verify child.
		var v777 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "rollup-ar-events-verify-777"}, &v777)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &v777)).To(Succeed())

		_, err = rec.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		// One event per resolved issue. Async via channel, so read with timeout.
		Eventually(recorder.Events).Should(Receive(And(
			ContainSubstring("AlreadyResolved"),
			ContainSubstring("#777"),
		)))
	})

	It("fails the Workload when gate passes but any reviewer emits NO-GO (#575)", func() {
		// v0.4 regression test: this lock-in confirms the
		// cascade-on-verdict behavior from #541 also catches the
		// reviewer-NO-GO + gate-PASS case. The reviewer is the
		// design-quality second pass after the gate's mechanical
		// `make` checks; REQUEST-CHANGES from any reviewer MUST
		// drive the Workload to Failed even when the gate said the
		// commit was technically buildable.
		wl := newWorkload("reviewer-blocks", foremanv1alpha1.WorkloadSpec{
			Intent:           "reviewer NO-GO blocks workload completion",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{1234},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "reviewer"},
			},
			// Explicit 0 opts out of the #946 fix iteration so this
			// test keeps pinning the immediate fail-on-NO-GO terminal.
			MaxReviewIterations: ptr.To(int32(0)),
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		// Coder GO, verifier GATE-PASS, reviewer NO-GO (the
		// "design quality" failure mode the v0.4 reviewer is
		// designed to catch).
		updates := []struct {
			name    string
			phase   foremanv1alpha1.AgenticTaskPhase
			verdict foremanv1alpha1.AgenticTaskVerdict
		}{
			{"reviewer-blocks-code-1234", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictGo},
			{"reviewer-blocks-verify-1234", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictGatePass},
			{"reviewer-blocks-review-1234-0", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictNoGo},
		}
		for _, u := range updates {
			var t foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: u.name}, &t)).To(Succeed())
			patch := client.MergeFrom(t.DeepCopy())
			t.Status.Phase = u.phase
			t.Status.Verdict = u.verdict
			Expect(k8sClient.Status().Patch(ctx, &t, patch)).To(Succeed())
		}

		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		// Two on-target (coder GO, verify GATE-PASS) + one
		// reviewer NO-GO = workload Failed with one IncompleteTask.
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseFailed),
			"reviewer NO-GO must drive Workload to Failed even when gate passed")
		Expect(fresh.Status.SucceededTasks).To(Equal(int32(2)))
		Expect(fresh.Status.IncompleteTasks).To(Equal(int32(1)))
		Expect(fresh.Status.FailedTasks).To(Equal(int32(0)))
		reviewerBlocksCompleted := findCondition(fresh.Status.Conditions, conditionTypeCompleted)
		Expect(reviewerBlocksCompleted).NotTo(BeNil())
		Expect(reviewerBlocksCompleted.Reason).To(Equal("ChildrenIncomplete"))
	})

	It("emits escalation reviewers only after a base reviewer NO-GO, advisory verdict (#546)", func() {
		wl := newWorkload("escalation-happy", foremanv1alpha1.WorkloadSpec{
			Intent:           "escalate on base NO-GO",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{641},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "base-reviewer"},
			},
			EscalationReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "big-reviewer"},
			},
			// Opt out of the #946 fix iteration: this test pins the
			// #546 escalate-on-first-NO-GO semantics in isolation.
			MaxReviewIterations: ptr.To(int32(0)),
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		// First reconcile plans the base pipeline only: no escalate task.
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())
		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Tasks).To(HaveLen(3)) // code + verify + review

		// Drive the base pipeline to a reviewer NO-GO.
		updates := []struct {
			name    string
			phase   foremanv1alpha1.AgenticTaskPhase
			verdict foremanv1alpha1.AgenticTaskVerdict
		}{
			{"escalation-happy-code-641", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictGo},
			{"escalation-happy-verify-641", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictGatePass},
			{"escalation-happy-review-641-0", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictNoGo},
		}
		for _, u := range updates {
			var task foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: u.name}, &task)).To(Succeed())
			patch := client.MergeFrom(task.DeepCopy())
			task.Status.Phase = u.phase
			task.Status.Verdict = u.verdict
			Expect(k8sClient.Status().Patch(ctx, &task, patch)).To(Succeed())
		}

		// Second reconcile (the rollup pass) must emit the escalation
		// task and keep the Workload in flight while it runs.
		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var esc foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "escalation-happy-escalate-641-0"}, &esc)).To(Succeed())
		Expect(esc.Spec.Kind).To(Equal(foremanv1alpha1.AgenticTaskKindReview))
		Expect(esc.Spec.AgentRef.Name).To(Equal("big-reviewer"))
		Expect(esc.Spec.Payload.Issue).To(Equal(int32(641)))
		Expect(esc.Spec.Payload.Branch).To(Equal("foreman/escalation-happy/issue-641"))
		Expect(esc.Spec.DependsOn).To(BeEmpty())
		Expect(esc.Labels[labelStep]).To(Equal("escalate-641-0"))
		Expect(esc.OwnerReferences).To(HaveLen(1))
		Expect(esc.OwnerReferences[0].Name).To(Equal("escalation-happy"))

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseDispatched),
			"workload must stay in flight while the escalation reviewer runs")
		Expect(fresh.Status.Tasks).To(HaveLen(4))
		escCond := findCondition(fresh.Status.Conditions, conditionTypeEscalationTriggered)
		Expect(escCond).NotTo(BeNil())
		Expect(escCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(escCond.Message).To(ContainSubstring("641"))

		// A third reconcile must NOT double-emit (idempotency).
		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())
		var tasks foremanv1alpha1.AgenticTaskList
		Expect(k8sClient.List(ctx, &tasks, client.InNamespace("default"),
			client.MatchingLabels{labelWorkload: "escalation-happy"})).To(Succeed())
		Expect(tasks.Items).To(HaveLen(4))

		// Escalation GO is advisory in v0.2: the base NO-GO still rolls
		// the Workload to Failed for human review.
		var escTask foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "escalation-happy-escalate-641-0"}, &escTask)).To(Succeed())
		patch := client.MergeFrom(escTask.DeepCopy())
		escTask.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		escTask.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictGo
		Expect(k8sClient.Status().Patch(ctx, &escTask, patch)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseFailed),
			"advisory: escalation GO does not clear the base NO-GO in v0.2")
		Expect(fresh.Status.SucceededTasks).To(Equal(int32(3)))  // code, verify, escalate
		Expect(fresh.Status.IncompleteTasks).To(Equal(int32(1))) // base reviewer NO-GO
	})

	It("suppresses a cloud-proxy escalation reviewer when the operator kill switch is off (#546)", func() {
		reconciler.AllowCloudProviders = false
		DeferCleanup(func() { reconciler.AllowCloudProviders = true })

		Expect(k8sClient.Create(ctx, &foremanv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "esc-local-base", Namespace: "default"},
			Spec: foremanv1alpha1.AgentSpec{
				Role:                foremanv1alpha1.AgentRoleReviewer,
				Provider:            foremanv1alpha1.AgentProviderLocal,
				InferenceServiceRef: corev1.LocalObjectReference{Name: "test-svc"},
				SystemPrompt:        "you are a reviewer",
				Tools:               []string{"submit_result"},
				MaxTurns:            5,
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &foremanv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "esc-cloud-big", Namespace: "default"},
			Spec: foremanv1alpha1.AgentSpec{
				Role:     foremanv1alpha1.AgentRoleReviewer,
				Provider: foremanv1alpha1.AgentProviderCloudProxy,
				ProviderConfig: &foremanv1alpha1.ProviderConfig{
					BaseURL: "http://foundation-router.lan:4000/v1",
					Model:   "claude-sonnet-4-6",
				},
				SystemPrompt: "you are an escalation reviewer",
				Tools:        []string{"submit_result"},
				MaxTurns:     5,
			},
		})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &foremanv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "esc-local-base", Namespace: "default"}})
			_ = k8sClient.Delete(ctx, &foremanv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "esc-cloud-big", Namespace: "default"}})
		})

		wl := newWorkload("escalation-gated", foremanv1alpha1.WorkloadSpec{
			Intent:           "cloud escalation suppressed by operator gate",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{642},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "esc-local-base"},
			},
			EscalationReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "esc-cloud-big"},
			},
			// Opt out of the #946 fix iteration: this test pins the
			// #546 escalate-on-first-NO-GO semantics in isolation.
			MaxReviewIterations: ptr.To(int32(0)),
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		updates := []struct {
			name    string
			verdict foremanv1alpha1.AgenticTaskVerdict
		}{
			{"escalation-gated-code-642", foremanv1alpha1.AgenticTaskVerdictGo},
			{"escalation-gated-verify-642", foremanv1alpha1.AgenticTaskVerdictGatePass},
			{"escalation-gated-review-642-0", foremanv1alpha1.AgenticTaskVerdictNoGo},
		}
		for _, u := range updates {
			var task foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: u.name}, &task)).To(Succeed())
			patch := client.MergeFrom(task.DeepCopy())
			task.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
			task.Status.Verdict = u.verdict
			Expect(k8sClient.Status().Patch(ctx, &task, patch)).To(Succeed())
		}

		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		// The cloud escalation reviewer must NOT have been created.
		var escTask foremanv1alpha1.AgenticTask
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "escalation-gated-escalate-642-0"}, &escTask)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "cloud escalation task must be suppressed")

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		cond := findCondition(fresh.Status.Conditions, conditionTypeCloudReviewersSuppressed)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Message).To(ContainSubstring("esc-cloud-big"))
		Expect(cond.Message).To(ContainSubstring("operator --allow-cloud-providers=false"))

		escCond := findCondition(fresh.Status.Conditions, conditionTypeEscalationTriggered)
		Expect(escCond).NotTo(BeNil())
		Expect(escCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(escCond.Message).To(ContainSubstring("0 task(s) created, 1 suppressed"))
	})

	It("suppresses escalation when MaxTasks leaves no room and reports it (#546)", func() {
		wl := newWorkload("escalation-capped", foremanv1alpha1.WorkloadSpec{
			Intent:           "maxTasks blocks escalation",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{643},
			MaxTasks:         3, // code + verify + review fills the cap
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "base-reviewer"},
			},
			EscalationReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "big-reviewer"},
			},
			// Opt out of the #946 fix iteration: this test pins the
			// #546 escalate-on-first-NO-GO semantics in isolation.
			MaxReviewIterations: ptr.To(int32(0)),
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		updates := []struct {
			name    string
			verdict foremanv1alpha1.AgenticTaskVerdict
		}{
			{"escalation-capped-code-643", foremanv1alpha1.AgenticTaskVerdictGo},
			{"escalation-capped-verify-643", foremanv1alpha1.AgenticTaskVerdictGatePass},
			{"escalation-capped-review-643-0", foremanv1alpha1.AgenticTaskVerdictNoGo},
		}
		for _, u := range updates {
			var task foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: u.name}, &task)).To(Succeed())
			patch := client.MergeFrom(task.DeepCopy())
			task.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
			task.Status.Verdict = u.verdict
			Expect(k8sClient.Status().Patch(ctx, &task, patch)).To(Succeed())
		}

		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var escTask foremanv1alpha1.AgenticTask
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "escalation-capped-escalate-643-0"}, &escTask)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "MaxTasks must suppress escalation emission")

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		trunc := findCondition(fresh.Status.Conditions, conditionTypeTruncated)
		Expect(trunc).NotTo(BeNil())
		Expect(trunc.Status).To(Equal(metav1.ConditionTrue))
		Expect(trunc.Reason).To(Equal("MaxTasksEscalationCap"))
		// And the workload still terminates Failed on the base NO-GO.
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseFailed))
	})

	It("flags escalation refs without base reviewers via EscalationTriggered=False (#546)", func() {
		wl := newWorkload("escalation-nobase", foremanv1alpha1.WorkloadSpec{
			Intent:           "escalation without base tier",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{644},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			EscalationReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "big-reviewer"},
			},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		// Plan, then trigger one rollup pass.
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var escTask foremanv1alpha1.AgenticTask
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "escalation-nobase-escalate-644-0"}, &escTask)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no escalation may be emitted without a base reviewer tier")

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		cond := findCondition(fresh.Status.Conditions, conditionTypeEscalationTriggered)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("NoBaseReviewers"))
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
