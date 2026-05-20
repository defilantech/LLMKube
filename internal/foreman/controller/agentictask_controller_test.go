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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// M0/M1 ship the AgenticTaskReconciler as a logging stub: it reads the
// object and returns without touching status. These smoke tests pin
// that contract so future M2 evolution either preserves it (until M2
// merges) or breaks it intentionally with a deliberate test update.

var _ = Describe("AgenticTaskReconciler (M0 stub)", func() {
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
		// RequeueAfter == 0 covers both "no immediate requeue" and "no
		// timer requeue" since controller-runtime deprecated the
		// boolean Requeue field in favor of the zero-RequeueAfter
		// representation.
		Expect(res.RequeueAfter).To(BeZero())
	})

	It("reconciles an existing task without erroring and without mutating status (M0 stub)", func() {
		task := &foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{Name: "stub-smoke", Namespace: "default"},
			Spec: foremanv1alpha1.AgenticTaskSpec{
				Kind:    foremanv1alpha1.AgenticTaskKindFreeform,
				Payload: foremanv1alpha1.AgenticTaskPayload{Prompt: "stub-smoke"},
			},
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, task)
		})

		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: "stub-smoke"},
		})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "stub-smoke"}, &fresh)).To(Succeed())

		// M0 stub leaves status untouched. M2 replaces this contract.
		Expect(string(fresh.Status.Phase)).To(BeEmpty())
		Expect(fresh.Status.AssignedNode).To(BeEmpty())
		Expect(fresh.Status.Verdict).To(BeEquivalentTo(""))
		Expect(fresh.Status.Result).To(BeNil())
	})
})
