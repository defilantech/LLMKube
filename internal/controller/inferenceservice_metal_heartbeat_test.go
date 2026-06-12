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
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// metalEndpoints builds a corev1.Endpoints fixture for the metal-agent path.
// name must be the sanitized InferenceService name (dots replaced by hyphens).
// heartbeat is stamped as the AnnotationAgentHeartbeat annotation when non-empty.
//
//nolint:staticcheck // SA1019: v1 Endpoints; see metalReadyEndpoints comment
func metalEndpoints(name, heartbeat string) *corev1.Endpoints {
	ep := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"llmkube.ai/managed-by": "metal-agent",
			},
		},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{IP: "192.0.2.10"}},
			Ports:     []corev1.EndpointPort{{Port: 50051, Name: "http", Protocol: corev1.ProtocolTCP}},
		}},
	}
	if heartbeat != "" {
		ep.Annotations = map[string]string{
			inferencev1alpha1.AnnotationAgentHeartbeat: heartbeat,
		}
	}
	return ep
}

var _ = Describe("metalReadyEndpoints heartbeat expiry", func() {
	var (
		reconciler *InferenceServiceReconciler
		ctx        context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
	})

	Context("direct unit tests of metalReadyEndpoints", func() {
		const namespace = "default"

		It("should return 1 for Endpoints with a fresh heartbeat annotation", func() {
			const isvcName = "hb-fresh"
			fresh := time.Now().UTC().Format(time.RFC3339)
			ep := metalEndpoints(isvcName, fresh)
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ep) })

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
			}
			Expect(reconciler.metalReadyEndpoints(ctx, isvc)).To(Equal(int32(1)))
		})

		It("should return 0 for Endpoints with a stale heartbeat annotation (10 minutes old)", func() {
			const isvcName = "hb-stale"
			stale := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
			ep := metalEndpoints(isvcName, stale)
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ep) })

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
			}
			Expect(reconciler.metalReadyEndpoints(ctx, isvc)).To(Equal(int32(0)))
		})

		It("should return 0 for Endpoints with an unparseable heartbeat annotation", func() {
			const isvcName = "hb-bad"
			ep := metalEndpoints(isvcName, "not-a-timestamp")
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ep) })

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
			}
			Expect(reconciler.metalReadyEndpoints(ctx, isvc)).To(Equal(int32(0)))
		})

		It("should return 1 for Endpoints WITHOUT the annotation (legacy agent exemption)", func() {
			const isvcName = "hb-legacy"
			ep := metalEndpoints(isvcName, "") // no annotation
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ep) })

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
			}
			Expect(reconciler.metalReadyEndpoints(ctx, isvc)).To(Equal(int32(1)))
		})
	})

	Context("Reconcile requeue for metal InferenceService with heartbeat Endpoints", func() {
		const namespace = "default"
		const modelName = "hb-metal-model"
		const isvcName = "hb-metal-isvc"

		BeforeEach(func() {
			model := &inferencev1alpha1.Model{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: namespace}, model)
			if err != nil && errors.IsNotFound(err) {
				model = &inferencev1alpha1.Model{
					ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
					Spec: inferencev1alpha1.ModelSpec{
						Source:   "https://example.com/hb-model.gguf",
						Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
					},
				}
				Expect(k8sClient.Create(ctx, model)).To(Succeed())
			}
			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			isvc := &inferencev1alpha1.InferenceService{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, isvc)
			if err != nil && errors.IsNotFound(err) {
				replicas := int32(1)
				isvc = &inferencev1alpha1.InferenceService{
					ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
					Spec: inferencev1alpha1.InferenceServiceSpec{
						ModelRef: modelName,
						Replicas: &replicas,
					},
				}
				Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			}
		})

		AfterEach(func() {
			isvc := &inferencev1alpha1.InferenceService{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, isvc); err == nil {
				_ = k8sClient.Delete(ctx, isvc)
			}

			model := &inferencev1alpha1.Model{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: namespace}, model); err == nil {
				_ = k8sClient.Delete(ctx, model)
			}

			ep := &corev1.Endpoints{} //nolint:staticcheck
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, ep); err == nil {
				_ = k8sClient.Delete(ctx, ep)
			}
		})

		It("should return a non-zero RequeueAfter when Endpoints carry a fresh heartbeat annotation", func() {
			fresh := time.Now().UTC().Format(time.RFC3339)
			ep := metalEndpoints(isvcName, fresh)
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0),
				"expected a non-zero RequeueAfter for a metal service with heartbeat Endpoints")
			Expect(result.RequeueAfter).To(Equal(inferencev1alpha1.DefaultAgentHeartbeatTimeout / 2))
		})
	})

	Context("Reconcile with a stale metal-agent heartbeat", func() {
		const namespace = "default"
		const modelName = "hb-stale-model"
		const isvcName = "hb-stale-isvc"

		BeforeEach(func() {
			model := &inferencev1alpha1.Model{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: namespace}, model)
			if err != nil && errors.IsNotFound(err) {
				model = &inferencev1alpha1.Model{
					ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
					Spec: inferencev1alpha1.ModelSpec{
						Source:   "https://example.com/stale-model.gguf",
						Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
					},
				}
				Expect(k8sClient.Create(ctx, model)).To(Succeed())
			}
			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			isvc := &inferencev1alpha1.InferenceService{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, isvc)
			if err != nil && errors.IsNotFound(err) {
				replicas := int32(1)
				isvc = &inferencev1alpha1.InferenceService{
					ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
					Spec: inferencev1alpha1.InferenceServiceSpec{
						ModelRef: modelName,
						Replicas: &replicas,
					},
				}
				Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			}
		})

		AfterEach(func() {
			isvc := &inferencev1alpha1.InferenceService{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, isvc); err == nil {
				_ = k8sClient.Delete(ctx, isvc)
			}

			model := &inferencev1alpha1.Model{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: namespace}, model); err == nil {
				_ = k8sClient.Delete(ctx, model)
			}

			ep := &corev1.Endpoints{} //nolint:staticcheck
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, ep); err == nil {
				_ = k8sClient.Delete(ctx, ep)
			}
		})

		It("should report stale status, zero readyReplicas, and still requeue after a 10-minute-old heartbeat", func() {
			stale := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
			ep := metalEndpoints(isvcName, stale)
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// The re-check loop must survive staleness: a stale heartbeat still
			// needs periodic re-evaluation in case the agent comes back online.
			Expect(result.RequeueAfter).To(BeNumerically(">", 0),
				"expected a non-zero RequeueAfter so the controller re-checks once the agent recovers")

			// The operator must reflect 0 ready replicas when the heartbeat has expired.
			isvc := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, isvc)).To(Succeed())
			Expect(isvc.Status.ReadyReplicas).To(Equal(int32(0)),
				"stale heartbeat should zero out readyReplicas")

			// The scheduling status must indicate a stale heartbeat, not initial provisioning.
			Expect(isvc.Status.SchedulingStatus).To(Equal("AgentHeartbeatStale"),
				"stale heartbeat must report AgentHeartbeatStale, not WaitingForMetalAgent")
			Expect(isvc.Status.SchedulingMessage).To(ContainSubstring("last seen"),
				"stale heartbeat message must include the last-seen timestamp")
		})
	})
})
