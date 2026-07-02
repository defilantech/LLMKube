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

// Pins #949: reconciling a Workload that is terminating (foreground
// deletion: deletionTimestamp set, parent parked behind the
// foregroundDeletion finalizer while GC removes its children) must NOT
// re-render the pipeline. Without the guard, the reconciler sees "no
// children yet", recreates every AgenticTask, GC deletes them again,
// and the Workload never finishes terminating — external retry systems
// that delete-and-recreate Workloads hang forever on the delete.
var _ = Describe("WorkloadReconciler on a deleting Workload (#949)", func() {
	var reconciler *WorkloadReconciler

	BeforeEach(func() {
		reconciler = &WorkloadReconciler{
			Client:              k8sClient,
			Scheme:              k8sClient.Scheme(),
			AllowCloudProviders: true,
		}
	})

	It("does not re-render children once deletionTimestamp is set", func() {
		wl := newWorkload("deleting-no-resynth", foremanv1alpha1.WorkloadSpec{
			Intent:           "fix it",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{7},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		// First reconcile renders the pipeline.
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())
		Expect(listChildrenNames(wl)).NotTo(BeEmpty())

		// Foreground-delete the Workload: deletionTimestamp is set and
		// the object is parked behind the foregroundDeletion finalizer
		// (envtest runs no GC, so we simulate GC by deleting the
		// children ourselves).
		propagation := client.PropagationPolicy("Foreground")
		Expect(k8sClient.Delete(ctx, wl, propagation)).To(Succeed())
		var deleting foremanv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &deleting)).To(Succeed())
		Expect(deleting.DeletionTimestamp.IsZero()).To(BeFalse())
		cleanupChildren(wl)
		Expect(listChildrenNames(wl)).To(BeEmpty())

		// Reconciling the terminating Workload must NOT resynthesize
		// the pipeline.
		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())
		Expect(listChildrenNames(wl)).To(BeEmpty())
	})
})

// listChildrenNames returns the names of the AgenticTasks labeled as the
// Workload's children.
func listChildrenNames(wl *foremanv1alpha1.Workload) []string {
	var tasks foremanv1alpha1.AgenticTaskList
	Expect(k8sClient.List(ctx, &tasks,
		client.InNamespace(wl.Namespace),
		client.MatchingLabels{labelWorkload: wl.Name},
	)).To(Succeed())
	names := make([]string, 0, len(tasks.Items))
	for i := range tasks.Items {
		names = append(names, tasks.Items[i].Name)
	}
	return names
}
