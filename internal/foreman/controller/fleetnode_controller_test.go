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

// M0/M1 ship the FleetNodeReconciler as a logging stub. The
// stale-heartbeat -> NotReady logic lands in M2. These smoke tests pin
// the no-mutation contract today.

var _ = Describe("FleetNodeReconciler (M0 stub)", func() {
	var reconciler *FleetNodeReconciler

	BeforeEach(func() {
		reconciler = &FleetNodeReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("returns no error when the FleetNode is not found", func() {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "absent"},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("reconciles an existing FleetNode without mutating status (M0 stub)", func() {
		fn := &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: "stub-fleetnode"},
			Spec: foremanv1alpha1.FleetNodeSpec{
				NodeName: "stub-fleetnode",
				Roles:    []string{"worker"},
			},
		}
		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, fn)
		})

		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "stub-fleetnode"},
		})
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "stub-fleetnode"}, &fresh)).To(Succeed())
		Expect(string(fresh.Status.Phase)).To(BeEmpty())
	})
})
