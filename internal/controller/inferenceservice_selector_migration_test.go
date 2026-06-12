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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Regression for #606: a Deployment created by an older operator carries a
// smaller pod selector (pre-0.8: {app: <name>}) than the current operator
// generates ({app, inference.llmkube.dev/service}). Deployment.spec.selector
// is immutable, so an in-place Update fails forever with "field is immutable".
// The controller must migrate it by recreating the Deployment.
var _ = Describe("InferenceService Deployment selector migration (#606)", func() {
	const modelName = "sel-mig-model"
	const serviceName = "sel-mig-svc"
	ctx := context.Background()
	nn := types.NamespacedName{Name: serviceName, Namespace: "default"}

	BeforeEach(func() {
		model := &inferencev1alpha1.Model{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, model); err != nil && errors.IsNotFound(err) {
			Expect(k8sClient.Create(ctx, &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:       "https://huggingface.co/test/model.gguf",
					Format:       "gguf",
					Quantization: "Q4_K_M",
					Hardware:     &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
					Resources:    &inferencev1alpha1.ResourceRequirements{CPU: "1", Memory: "1Gi"},
				},
			})).To(Succeed())
		}
		isvc := &inferencev1alpha1.InferenceService{}
		if err := k8sClient.Get(ctx, nn, isvc); err != nil && errors.IsNotFound(err) {
			replicas := int32(1)
			Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
				},
			})).To(Succeed())
		}
	})

	AfterEach(func() {
		isvc := &inferencev1alpha1.InferenceService{}
		if err := k8sClient.Get(ctx, nn, isvc); err == nil {
			Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
		}
		model := &inferencev1alpha1.Model{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, model); err == nil {
			Expect(k8sClient.Delete(ctx, model)).To(Succeed())
		}
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, nn, dep); err == nil {
			Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
		}
	})

	It("recreates a Deployment whose selector predates the inference.llmkube.dev/service label", func() {
		By("seeding a pre-0.8 Deployment with the old {app} selector only")
		replicas := int32(1)
		oldSelector := map[string]string{"app": serviceName}
		Expect(k8sClient.Create(ctx, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: "default"},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: oldSelector},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: oldSelector},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name:  "server",
						Image: "ghcr.io/ggml-org/llama.cpp:server",
					}}},
				},
			},
		})).To(Succeed())

		By("reconciling the Deployment with the model marked ready")
		reconciler := &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, nn, isvc)).To(Succeed())
		model := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, model)).To(Succeed())
		_, _, _, _, err := reconciler.reconcileDeployment(ctx, isvc, model, 1, true, false)
		Expect(err).NotTo(HaveOccurred(), "controller must migrate the selector, not loop on an immutable update")

		By("the Deployment now carries the current selector")
		migrated := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nn, migrated)).To(Succeed())
		Expect(migrated.Spec.Selector.MatchLabels).To(
			HaveKeyWithValue("inference.llmkube.dev/service", serviceName),
			"selector should have been migrated to the operator-managed label set",
		)
	})
})
