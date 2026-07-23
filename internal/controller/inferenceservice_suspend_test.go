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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("InferenceService suspend", func() {
	ctx := context.Background()

	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
	})

	It("scales the Deployment to zero and preserves spec.replicas when suspended", func() {
		modelName := "suspend-model"
		isvcName := "suspend-isvc"

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/model.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		replicas := int32(2)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: modelName,
				Replicas: &replicas,
				Image:    "ghcr.io/ggml-org/llama.cpp:server",
				Suspend:  false,
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, isvc)
			dep := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep); err == nil {
				_ = k8sClient.Delete(ctx, dep)
			}
			svc := &corev1.Service{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, svc); err == nil {
				_ = k8sClient.Delete(ctx, svc)
			}
		}()

		// First reconcile with suspend=false: Deployment should run at 2 replicas.
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(2)))

		// Patch suspend=true and reconcile again.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, isvc)).To(Succeed())
		isvc.Spec.Suspend = true
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(0)))

		updated := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
		Expect(*updated.Spec.Replicas).To(Equal(int32(2)))
		Expect(updated.Status.Phase).To(Equal(PhaseSuspended))
		Expect(updated.Status.DesiredReplicas).To(Equal(int32(0)))

		cond := meta.FindStatusCondition(updated.Status.Conditions, "Suspended")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.ObservedGeneration).To(Equal(updated.Generation))
	})

	It("restores the Deployment to spec.replicas on unsuspend", func() {
		modelName := "unsuspend-model"
		isvcName := "unsuspend-isvc"

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/model.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		replicas := int32(2)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: modelName,
				Replicas: &replicas,
				Image:    "ghcr.io/ggml-org/llama.cpp:server",
				Suspend:  true,
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, isvc)
			dep := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep); err == nil {
				_ = k8sClient.Delete(ctx, dep)
			}
			svc := &corev1.Service{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, svc); err == nil {
				_ = k8sClient.Delete(ctx, svc)
			}
		}()

		// First reconcile while suspended: Deployment starts at 0 replicas.
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(0)))

		// Unsuspend and reconcile again.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, isvc)).To(Succeed())
		isvc.Spec.Suspend = false
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(2)))

		updated := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
		cond := meta.FindStatusCondition(updated.Status.Conditions, "Suspended")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	})

	It("deletes the HPA and forces zero replicas when a suspended service has autoscaling", func() {
		modelName := "suspend-hpa-model"
		isvcName := "suspend-hpa-isvc"

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/model.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		replicas := int32(2)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: modelName,
				Replicas: &replicas,
				Image:    "ghcr.io/ggml-org/llama.cpp:server",
				Suspend:  false,
				Autoscaling: &inferencev1alpha1.AutoscalingSpec{
					MaxReplicas: 3,
				},
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		hpaKey := types.NamespacedName{Name: isvcName, Namespace: "default"}
		defer func() {
			_ = k8sClient.Delete(ctx, isvc)
			dep := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep); err == nil {
				_ = k8sClient.Delete(ctx, dep)
			}
			svc := &corev1.Service{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, svc); err == nil {
				_ = k8sClient.Delete(ctx, svc)
			}
			hpa := &autoscalingv2.HorizontalPodAutoscaler{}
			if err := k8sClient.Get(ctx, hpaKey, hpa); err == nil {
				_ = k8sClient.Delete(ctx, hpa)
			}
		}()

		// First reconcile with suspend=false: autoscaling is active, HPA should exist.
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, hpaKey, &autoscalingv2.HorizontalPodAutoscaler{})).To(Succeed())

		// Patch suspend=true and reconcile again.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, isvc)).To(Succeed())
		isvc.Spec.Suspend = true
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			return errors.IsNotFound(k8sClient.Get(ctx, hpaKey, &autoscalingv2.HorizontalPodAutoscaler{}))
		}, "5s", "100ms").Should(BeTrue())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(0)))

		// Unsuspend and reconcile again: the HPA should be recreated.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, isvc)).To(Succeed())
		isvc.Spec.Suspend = false
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, hpaKey, &autoscalingv2.HorizontalPodAutoscaler{})).To(Succeed())
	})
})
