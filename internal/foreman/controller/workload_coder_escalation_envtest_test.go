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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// These envtests exercise the coder-escalation second-pass hook end to
// end through Reconcile (mirrors the reviewer-escalation suite). A base
// code-<N> task that fails with a capability signal is re-dispatched to
// EscalationCoderAgentRef as code-<N>-esc / verify-<N>-esc.
var _ = Describe("WorkloadReconciler coder escalation (#963)", func() {
	var reconciler *WorkloadReconciler

	BeforeEach(func() {
		reconciler = &WorkloadReconciler{
			Client:              k8sClient,
			Scheme:              k8sClient.Scheme(),
			AllowCloudProviders: true,
		}
	})

	It("re-dispatches a base-coder capability failure to the escalation tier and is idempotent", func() {
		wl := newWorkload("coder-esc-happy", foremanv1alpha1.WorkloadSpec{
			Intent:                  "escalate a base coder that could not solve the issue",
			Repo:                    "defilantech/LLMKube",
			Issues:                  []int32{944, 921},
			CoderAgentRef:           &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef:        &corev1.LocalObjectReference{Name: "gate"},
			EscalationCoderAgentRef: &corev1.LocalObjectReference{Name: "coder-qwopus"},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		// First reconcile plans the base pipeline: code + verify per issue.
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())
		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Tasks).To(HaveLen(4)) // (code+verify) x 2 issues

		// (A) code-944 terminates NO-GO / MODEL-DECIDED: a capability
		//     failure that must escalate.
		set944 := foremanv1alpha1.AgenticTask{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "coder-esc-happy-code-944"}, &set944)).To(Succeed())
		p944 := client.MergeFrom(set944.DeepCopy())
		set944.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		set944.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
		set944.Status.Result = resultRaw("MODEL-DECIDED", "", "fuzzy front-runs anchor")
		Expect(k8sClient.Status().Patch(ctx, &set944, p944)).To(Succeed())

		// (B) code-921 terminates INCOMPLETE / MODEL-DECIDED (no gate
		//     outcome): the honest "gave up" bail that must NOT escalate.
		set921 := foremanv1alpha1.AgenticTask{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "coder-esc-happy-code-921"}, &set921)).To(Succeed())
		p921 := client.MergeFrom(set921.DeepCopy())
		set921.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		set921.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictIncomplete
		set921.Status.Result = resultRaw("MODEL-DECIDED", "", "ran out of turns")
		Expect(k8sClient.Status().Patch(ctx, &set921, p921)).To(Succeed())

		// Second reconcile runs the coder-escalation hook.
		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		// (A) the escalation code + verify tasks exist at the esc tier.
		var escCode foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "coder-esc-happy-code-944-esc"}, &escCode)).To(Succeed())
		Expect(escCode.Spec.Kind).To(Equal(foremanv1alpha1.AgenticTaskKindIssueFix))
		Expect(escCode.Spec.AgentRef.Name).To(Equal("coder-qwopus"))
		Expect(escCode.Spec.Payload.Branch).To(Equal("foreman/coder-esc-happy/issue-944-esc"))
		Expect(escCode.Spec.Payload.PromptPrefix).To(ContainSubstring("fuzzy front-runs anchor"))
		Expect(escCode.Labels[labelStep]).To(Equal("code-944-esc"))

		var escVerify foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "coder-esc-happy-verify-944-esc"}, &escVerify)).To(Succeed())
		Expect(escVerify.Spec.Kind).To(Equal(foremanv1alpha1.AgenticTaskKindVerify))
		Expect(escVerify.Spec.DependsOn).To(ContainElement("coder-esc-happy-code-944-esc"))

		// (B) issue 921 (INCOMPLETE, not a capability failure) is untouched.
		var noEsc921 foremanv1alpha1.AgenticTask
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "coder-esc-happy-code-921-esc"}, &noEsc921)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a model-decided INCOMPLETE must not escalate")

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		escCond := findCondition(fresh.Status.Conditions, conditionTypeCoderEscalationTriggered)
		Expect(escCond).NotTo(BeNil())
		Expect(escCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(escCond.Message).To(ContainSubstring("944"))

		// (D) idempotency: a second escalation pass must not re-emit.
		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())
		var tasks foremanv1alpha1.AgenticTaskList
		Expect(k8sClient.List(ctx, &tasks, client.InNamespace("default"),
			client.MatchingLabels{labelWorkload: "coder-esc-happy", labelStep: "code-944-esc"})).To(Succeed())
		Expect(tasks.Items).To(HaveLen(1))
	})

	It("does not escalate when EscalationCoderAgentRef is unset", func() {
		wl := newWorkload("coder-esc-off", foremanv1alpha1.WorkloadSpec{
			Intent:           "no escalation tier configured",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{944},
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

		var base foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "coder-esc-off-code-944"}, &base)).To(Succeed())
		patch := client.MergeFrom(base.DeepCopy())
		base.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		base.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
		base.Status.Result = resultRaw("MODEL-DECIDED", "", "bailed")
		Expect(k8sClient.Status().Patch(ctx, &base, patch)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())

		var escCode foremanv1alpha1.AgenticTask
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "coder-esc-off-code-944-esc"}, &escCode)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no escalation tier means no -esc tasks")

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(findCondition(fresh.Status.Conditions, conditionTypeCoderEscalationTriggered)).To(BeNil())
	})
})
