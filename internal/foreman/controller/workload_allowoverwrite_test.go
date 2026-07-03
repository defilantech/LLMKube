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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// Pins the #573-shaped opt-in from #934: Workload.spec.allowOverwrite is
// stamped onto the synthesized issue-fix payload (and ONLY there — the
// default stays false so a plain Workload never force-replaces refs).
var _ = Describe("WorkloadReconciler allowOverwrite stamping", func() {
	var reconciler *WorkloadReconciler

	BeforeEach(func() {
		reconciler = &WorkloadReconciler{
			Client:              k8sClient,
			Scheme:              k8sClient.Scheme(),
			AllowCloudProviders: true,
		}
	})

	reconcile := func(wl *foremanv1alpha1.Workload) {
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())
	}

	codeTask := func(name string) foremanv1alpha1.AgenticTask {
		var t foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &t)).To(Succeed())
		return t
	}

	It("stamps allowOverwrite=true onto the issue-fix payload when set", func() {
		wl := newWorkload("overwrite-on", foremanv1alpha1.WorkloadSpec{
			Intent:           "retry",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{7},
			AllowOverwrite:   true,
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
		})
		reconcile(wl)
		task := codeTask("overwrite-on-code-7")
		Expect(task.Spec.Payload.AllowOverwrite).To(BeTrue())
	})

	It("defaults to false when unset", func() {
		wl := newWorkload("overwrite-off", foremanv1alpha1.WorkloadSpec{
			Intent:           "fresh",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{8},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
		})
		reconcile(wl)
		task := codeTask("overwrite-off-code-8")
		Expect(task.Spec.Payload.AllowOverwrite).To(BeFalse())
	})
})

// Pins #937's default: an issue-batch Workload opens a PR on review GO
// unless spec.openPullRequest is explicitly false.
var _ = Describe("WorkloadReconciler openPullRequest stamping", func() {
	var reconciler *WorkloadReconciler

	BeforeEach(func() {
		reconciler = &WorkloadReconciler{
			Client:              k8sClient,
			Scheme:              k8sClient.Scheme(),
			AllowCloudProviders: true,
		}
	})

	reviewTask := func(name string) foremanv1alpha1.AgenticTask {
		var t foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &t)).To(Succeed())
		return t
	}

	makeWL := func(name string, openPR *bool, issue int32) *foremanv1alpha1.Workload {
		return newWorkload(name, foremanv1alpha1.WorkloadSpec{
			Intent:            "fix",
			Repo:              "defilantech/LLMKube",
			Issues:            []int32{issue},
			OpenPullRequest:   openPR,
			CoderAgentRef:     &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef:  &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{{Name: "reviewer"}},
		})
	}

	run := func(wl *foremanv1alpha1.Workload) {
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())
	}

	It("defaults to true (nil) on review payloads", func() {
		run(makeWL("openpr-default", nil, 21))
		Expect(reviewTask("openpr-default-review-21-0").Spec.Payload.OpenPullRequest).To(BeTrue())
	})

	It("false opts out", func() {
		f := false
		run(makeWL("openpr-off", &f, 22))
		Expect(reviewTask("openpr-off-review-22-0").Spec.Payload.OpenPullRequest).To(BeFalse())
	})

	It("does not stamp code or verify payloads", func() {
		run(makeWL("openpr-scope", nil, 23))
		var t foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "openpr-scope-code-23"}, &t)).To(Succeed())
		Expect(t.Spec.Payload.OpenPullRequest).To(BeFalse())
	})
})
