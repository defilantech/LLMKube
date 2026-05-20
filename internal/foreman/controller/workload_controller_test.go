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

// M0 ships the WorkloadReconciler as a logging stub. Real planner logic
// lands in M6. These smoke tests pin the stub's no-mutation contract.

var _ = Describe("WorkloadReconciler (M0 stub)", func() {
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

	It("reconciles an existing Workload without mutating status", func() {
		wl := &foremanv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: "stub-workload", Namespace: "default"},
			Spec: foremanv1alpha1.WorkloadSpec{
				Intent: "smoke test",
				Repo:   "defilantech/LLMKube",
			},
		}
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, wl)
		})

		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: "stub-workload"},
		})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "stub-workload"}, &fresh)).To(Succeed())
		Expect(string(fresh.Status.Phase)).To(BeEmpty())
		Expect(fresh.Status.Tasks).To(BeEmpty())
	})
})
