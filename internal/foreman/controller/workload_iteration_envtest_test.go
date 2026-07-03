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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// Reviewer NO-GO fix iteration (#946): instead of terminally failing
// the Workload, the reconciler re-dispatches the coder on the same
// branch with the review findings in payload.prompt, chains a fresh
// verify + review round behind it, and only fails once
// spec.maxReviewIterations is exhausted.
var _ = Describe("WorkloadReconciler review fix iteration (#946)", func() {
	var reconciler *WorkloadReconciler
	var recorder *events.FakeRecorder

	BeforeEach(func() {
		recorder = &events.FakeRecorder{Events: make(chan string, 16)}
		reconciler = &WorkloadReconciler{
			Client:              k8sClient,
			Scheme:              k8sClient.Scheme(),
			Recorder:            recorder,
			AllowCloudProviders: true,
		}
	})

	// markTerminal drives a child task to a terminal phase + verdict,
	// optionally attaching a structured result payload.
	markTerminal := func(name string, verdict foremanv1alpha1.AgenticTaskVerdict, resultJSON string) {
		var task foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &task)).To(Succeed())
		patch := client.MergeFrom(task.DeepCopy())
		task.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		task.Status.Verdict = verdict
		if resultJSON != "" {
			task.Status.Result = &runtime.RawExtension{Raw: []byte(resultJSON)}
		}
		Expect(k8sClient.Status().Patch(ctx, &task, patch)).To(Succeed())
	}

	reconcile := func(wl *foremanv1alpha1.Workload) {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())
	}

	noGoResult := `{"schemaVersion":"foreman.v1","kind":"review","verdict":"NO-GO",` +
		`"summary":"scope creep beyond the issue ask","extra":{"outcome":"MODEL-DECIDED","modelExtra":{"findings":` +
		`[{"severity":"blocker","area":"scope","message":"reduces ACCESS_TOKEN_EXPIRE_MINUTES from 10080 to 30",` +
		`"file":"config/auth.py","line":12,"suggestion":"revert the unrelated change"}]}}}`

	It("re-dispatches the coder with the review feedback on NO-GO and completes when the retry converges", func() {
		wl := newWorkload("iterate-happy", foremanv1alpha1.WorkloadSpec{
			Intent:           "fix iteration happy path",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{750},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "reviewer"},
			},
			// maxReviewIterations unset: defaults to 1 iteration.
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		// Plan the base round, then drive it to a reviewer NO-GO.
		reconcile(wl)
		markTerminal("iterate-happy-code-750", foremanv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal("iterate-happy-verify-750", foremanv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal("iterate-happy-review-750-0", foremanv1alpha1.AgenticTaskVerdictNoGo, noGoResult)

		// The rollup pass must append the r1 iteration instead of failing.
		reconcile(wl)

		var code foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-happy-code-750-r1"}, &code)).To(Succeed())
		Expect(code.Spec.Kind).To(Equal(foremanv1alpha1.AgenticTaskKindIssueFix))
		Expect(code.Spec.AgentRef.Name).To(Equal("coder"),
			"no revisionCoderAgentRef: iteration falls back to the issue-fix coder Agent")
		Expect(code.Spec.Payload.Issue).To(Equal(int32(750)))
		Expect(code.Spec.Payload.Branch).To(Equal("foreman/iterate-happy/issue-750"),
			"the retry amends the SAME branch, not a fresh one")
		Expect(code.Spec.Payload.AllowOverwrite).To(BeTrue(),
			"amending its own prior attempt needs the force-with-lease push")
		Expect(code.Spec.Payload.ReviseFromBranch).To(Equal("foreman/iterate-happy/issue-750"),
			"the executor must restore the prior attempt from the push remote (#951)")
		Expect(code.Spec.Payload.Prompt).To(ContainSubstring("NO-GO"))
		Expect(code.Spec.Payload.Prompt).To(ContainSubstring("Do not rebuild the fix from scratch"),
			"the prompt must direct a delta on the restored attempt")
		Expect(code.Spec.Payload.Prompt).To(ContainSubstring("scope creep beyond the issue ask"))
		Expect(code.Spec.Payload.Prompt).To(ContainSubstring("reduces ACCESS_TOKEN_EXPIRE_MINUTES from 10080 to 30"))
		Expect(code.Spec.DependsOn).To(BeEmpty())

		// No revisionCoderAgentRef: the fallback to the issue-fix coder
		// profile is the documented #951 failure mode, so it must warn.
		Expect(recorder.Events).To(Receive(And(
			ContainSubstring("Warning"),
			ContainSubstring("RevisionUnderIssueFixProfile"),
			ContainSubstring("revisionCoderAgentRef"),
		)))

		var verify foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-happy-verify-750-r1"}, &verify)).To(Succeed())
		Expect(verify.Spec.DependsOn).To(ConsistOf("iterate-happy-code-750-r1"))

		var review foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-happy-review-750-0-r1"}, &review)).To(Succeed())
		Expect(review.Spec.DependsOn).To(ConsistOf("iterate-happy-verify-750-r1"))
		Expect(review.Spec.Payload.OpenPullRequest).To(BeTrue(),
			"iteration review steps must carry the base-round openPullRequest stamp (#937)")

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseDispatched),
			"workload must stay in flight while the fix iteration runs")
		Expect(fresh.Status.Tasks).To(HaveLen(6))
		Expect(fresh.Status.ReviewIterations).To(Equal(int32(1)))
		cond := findCondition(fresh.Status.Conditions, conditionTypeReviewIterationTriggered)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Message).To(ContainSubstring("750"))

		// A further reconcile must NOT double-emit (idempotency by step
		// name existence, like escalation).
		reconcile(wl)
		var tasks foremanv1alpha1.AgenticTaskList
		Expect(k8sClient.List(ctx, &tasks, client.InNamespace("default"),
			client.MatchingLabels{labelWorkload: "iterate-happy"})).To(Succeed())
		Expect(tasks.Items).To(HaveLen(6))

		// Converge the retry: the superseded NO-GO round must no longer
		// pin the rollup at Failed.
		markTerminal("iterate-happy-code-750-r1", foremanv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal("iterate-happy-verify-750-r1", foremanv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal("iterate-happy-review-750-0-r1", foremanv1alpha1.AgenticTaskVerdictGo, "")
		reconcile(wl)

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseCompleted),
			"a converged fix iteration completes the Workload")
		Expect(fresh.Status.SucceededTasks).To(Equal(int32(3)), "only the latest round is counted")
		Expect(fresh.Status.IncompleteTasks).To(Equal(int32(0)))
		Expect(fresh.Status.ReviewIterations).To(Equal(int32(1)))
	})

	It("pairs iteration coder tasks with the revision coder Agent when configured (#951)", func() {
		wl := newWorkload("iterate-revision", foremanv1alpha1.WorkloadSpec{
			Intent:                "revision profile pairing",
			Repo:                  "defilantech/LLMKube",
			Issues:                []int32{753},
			CoderAgentRef:         &corev1.LocalObjectReference{Name: "coder"},
			RevisionCoderAgentRef: &corev1.LocalObjectReference{Name: "revision-coder"},
			VerifierAgentRef:      &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "reviewer"},
			},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		reconcile(wl)

		// The base round still uses the issue-fix coder profile.
		var base foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-revision-code-753"}, &base)).To(Succeed())
		Expect(base.Spec.AgentRef.Name).To(Equal("coder"))
		Expect(base.Spec.Payload.ReviseFromBranch).To(BeEmpty(),
			"the base attempt has nothing to restore")

		markTerminal("iterate-revision-code-753", foremanv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal("iterate-revision-verify-753", foremanv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal("iterate-revision-review-753-0", foremanv1alpha1.AgenticTaskVerdictNoGo, noGoResult)
		reconcile(wl)

		var code foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-revision-code-753-r1"}, &code)).To(Succeed())
		Expect(code.Spec.AgentRef.Name).To(Equal("revision-coder"),
			"the fix iteration must run under the revision-tuned profile")
		Expect(code.Spec.Payload.ReviseFromBranch).To(Equal("foreman/iterate-revision/issue-753"))

		// Verify + review keep their own refs.
		var verify foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-revision-verify-753-r1"}, &verify)).To(Succeed())
		Expect(verify.Spec.AgentRef.Name).To(Equal("gate"))

		// A configured revision profile is the intended pairing: no
		// RevisionUnderIssueFixProfile warning.
		Expect(recorder.Events).NotTo(Receive())
	})

	It("fails the Workload once the iteration budget is exhausted (today's terminal behavior)", func() {
		wl := newWorkload("iterate-exhausted", foremanv1alpha1.WorkloadSpec{
			Intent:           "fix iteration exhaustion",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{751},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "reviewer"},
			},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		reconcile(wl)
		markTerminal("iterate-exhausted-code-751", foremanv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal("iterate-exhausted-verify-751", foremanv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal("iterate-exhausted-review-751-0", foremanv1alpha1.AgenticTaskVerdictNoGo, noGoResult)
		reconcile(wl)

		// The r1 round exists; reject it too.
		markTerminal("iterate-exhausted-code-751-r1", foremanv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal("iterate-exhausted-verify-751-r1", foremanv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal("iterate-exhausted-review-751-0-r1", foremanv1alpha1.AgenticTaskVerdictNoGo, noGoResult)
		reconcile(wl)

		// No r2: the default budget of 1 is spent.
		var r2 foremanv1alpha1.AgenticTask
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-exhausted-code-751-r2"}, &r2)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "exhausted budget must not emit another iteration")

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseFailed),
			"a NO-GO on the last allowed iteration fails the Workload as before #946")
		completed := findCondition(fresh.Status.Conditions, conditionTypeCompleted)
		Expect(completed).NotTo(BeNil())
		Expect(completed.Reason).To(Equal("ChildrenIncomplete"))
		Expect(fresh.Status.ReviewIterations).To(Equal(int32(1)))
	})

	It("does not iterate when the reviewer says GO", func() {
		wl := newWorkload("iterate-go", foremanv1alpha1.WorkloadSpec{
			Intent:           "no iteration on GO",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{752},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "reviewer"},
			},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		reconcile(wl)
		markTerminal("iterate-go-code-752", foremanv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal("iterate-go-verify-752", foremanv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal("iterate-go-review-752-0", foremanv1alpha1.AgenticTaskVerdictGo, "")
		reconcile(wl)

		var r1 foremanv1alpha1.AgenticTask
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-go-code-752-r1"}, &r1)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "GO must not trigger a fix iteration")

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.WorkloadPhaseCompleted))
		Expect(fresh.Status.ReviewIterations).To(Equal(int32(0)))
		Expect(findCondition(fresh.Status.Conditions, conditionTypeReviewIterationTriggered)).To(BeNil())
	})
})
