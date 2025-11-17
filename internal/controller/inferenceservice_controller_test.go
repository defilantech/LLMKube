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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var _ = Describe("InferenceService Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"
		const modelName = "test-model"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		modelNamespacedName := types.NamespacedName{
			Name:      modelName,
			Namespace: "default",
		}
		inferenceservice := &inferencev1alpha1.InferenceService{}

		BeforeEach(func() {
			By("creating a Model resource first")
			model := &inferencev1alpha1.Model{}
			err := k8sClient.Get(ctx, modelNamespacedName, model)
			if err != nil && errors.IsNotFound(err) {
				modelResource := &inferencev1alpha1.Model{
					ObjectMeta: metav1.ObjectMeta{
						Name:      modelName,
						Namespace: "default",
					},
					Spec: inferencev1alpha1.ModelSpec{
						Source:       "https://huggingface.co/test/model.gguf",
						Format:       "gguf",
						Quantization: "Q4_K_M",
						Hardware:     &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
						Resources:    &inferencev1alpha1.ResourceRequirements{CPU: "1", Memory: "1Gi"},
					},
				}
				Expect(k8sClient.Create(ctx, modelResource)).To(Succeed())
			}

			By("creating the custom resource for the Kind InferenceService")
			err = k8sClient.Get(ctx, typeNamespacedName, inferenceservice)
			if err != nil && errors.IsNotFound(err) {
				replicas := int32(1)
				resource := &inferencev1alpha1.InferenceService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: inferencev1alpha1.InferenceServiceSpec{
						ModelRef: modelName,
						Replicas: &replicas,
						Image:    "ghcr.io/ggerganov/llama.cpp:server",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup the specific resource instance InferenceService")
			resource := &inferencev1alpha1.InferenceService{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("Cleanup the Model resource")
			modelResource := &inferencev1alpha1.Model{}
			err = k8sClient.Get(ctx, modelNamespacedName, modelResource)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, modelResource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &InferenceServiceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})
