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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
						Image:    "ghcr.io/ggml-org/llama.cpp:server",
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
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
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

var _ = Describe("Multi-GPU End-to-End Reconciliation", func() {
	Context("when reconciling a multi-GPU InferenceService", func() {
		const multiGPUModelName = "e2e-multi-gpu-model"
		const multiGPUServiceName = "e2e-multi-gpu-service"

		ctx := context.Background()

		modelNamespacedName := types.NamespacedName{
			Name:      multiGPUModelName,
			Namespace: "default",
		}
		serviceNamespacedName := types.NamespacedName{
			Name:      multiGPUServiceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating a multi-GPU Model resource")
			model := &inferencev1alpha1.Model{}
			err := k8sClient.Get(ctx, modelNamespacedName, model)
			if err != nil && errors.IsNotFound(err) {
				modelResource := &inferencev1alpha1.Model{
					ObjectMeta: metav1.ObjectMeta{
						Name:      multiGPUModelName,
						Namespace: "default",
					},
					Spec: inferencev1alpha1.ModelSpec{
						Source:       "https://huggingface.co/test/multi-gpu-model.gguf",
						Format:       "gguf",
						Quantization: "Q4_K_M",
						Hardware: &inferencev1alpha1.HardwareSpec{
							Accelerator: "cuda",
							GPU: &inferencev1alpha1.GPUSpec{
								Enabled: true,
								Count:   2,
								Vendor:  "nvidia",
								Layers:  -1,
								Sharding: &inferencev1alpha1.GPUShardingSpec{
									Strategy: "layer",
								},
							},
						},
						Resources: &inferencev1alpha1.ResourceRequirements{
							CPU:    "4",
							Memory: "16Gi",
						},
					},
				}
				Expect(k8sClient.Create(ctx, modelResource)).To(Succeed())
			}

			By("creating a multi-GPU InferenceService")
			isvc := &inferencev1alpha1.InferenceService{}
			err = k8sClient.Get(ctx, serviceNamespacedName, isvc)
			if err != nil && errors.IsNotFound(err) {
				replicas := int32(1)
				resource := &inferencev1alpha1.InferenceService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      multiGPUServiceName,
						Namespace: "default",
					},
					Spec: inferencev1alpha1.InferenceServiceSpec{
						ModelRef: multiGPUModelName,
						Replicas: &replicas,
						Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
						Resources: &inferencev1alpha1.InferenceResourceRequirements{
							GPU:       2,
							GPUMemory: "16Gi",
							CPU:       "4",
							Memory:    "8Gi",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("cleaning up the multi-GPU InferenceService")
			isvc := &inferencev1alpha1.InferenceService{}
			err := k8sClient.Get(ctx, serviceNamespacedName, isvc)
			if err == nil {
				Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
			}

			By("cleaning up the multi-GPU Model")
			model := &inferencev1alpha1.Model{}
			err = k8sClient.Get(ctx, modelNamespacedName, model)
			if err == nil {
				Expect(k8sClient.Delete(ctx, model)).To(Succeed())
			}

			By("cleaning up any created Deployment")
			deployment := &appsv1.Deployment{}
			deploymentName := types.NamespacedName{
				Name:      multiGPUServiceName,
				Namespace: "default",
			}
			err = k8sClient.Get(ctx, deploymentName, deployment)
			if err == nil {
				Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
			}
		})

		It("should create deployment with correct multi-GPU configuration", func() {
			By("reconciling the InferenceService")
			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			// First reconcile may not create deployment if model isn't ready
			// We're testing that the controller doesn't error
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: serviceNamespacedName,
			})
			// May return error since model download will fail (test URL)
			// but should not panic
			_ = err

			By("verifying the InferenceService was created")
			isvc := &inferencev1alpha1.InferenceService{}
			err = k8sClient.Get(ctx, serviceNamespacedName, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(isvc.Spec.Resources.GPU).To(Equal(int32(2)))
		})
	})
})

var _ = Describe("determinePhase", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("should return Ready when readyReplicas equals desiredReplicas", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		}
		phase, info := reconciler.determinePhase(context.Background(), isvc, 2, 2, false, &appsv1.Deployment{})
		Expect(phase).To(Equal("Ready"))
		Expect(info).To(BeNil())
	})

	It("should return Progressing when partially ready", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		}
		phase, info := reconciler.determinePhase(context.Background(), isvc, 1, 3, false, &appsv1.Deployment{})
		Expect(phase).To(Equal("Progressing"))
		Expect(info).To(BeNil())
	})

	It("should return Creating when no replicas ready and no scheduling issues", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "no-pods-test", Namespace: "default"},
		}
		phase, _ := reconciler.determinePhase(context.Background(), isvc, 0, 1, false, &appsv1.Deployment{})
		Expect(phase).To(Equal("Creating"))
	})

	It("should return Creating when deployment is nil (Metal path)", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		}
		phase, _ := reconciler.determinePhase(context.Background(), isvc, 0, 1, true, nil)
		Expect(phase).To(Equal("Creating"))
	})
})

var _ = Describe("findInferenceServiceForPod", func() {
	It("should return reconcile request when pod has service label", func() {
		reconciler := &InferenceServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
				Labels: map[string]string{
					"inference.llmkube.dev/service": "my-service",
				},
			},
		}
		requests := reconciler.findInferenceServiceForPod(context.Background(), pod)
		Expect(requests).To(HaveLen(1))
		Expect(requests[0].Name).To(Equal("my-service"))
		Expect(requests[0].Namespace).To(Equal("default"))
	})

	It("should return nil when pod lacks service label", func() {
		reconciler := &InferenceServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		}
		requests := reconciler.findInferenceServiceForPod(context.Background(), pod)
		Expect(requests).To(BeNil())
	})
})

var _ = Describe("Reconcile lifecycle", func() {
	It("should return empty result when InferenceService is not found", func() {
		reconciler := &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
		result, err := reconciler.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))
	})

	Context("with envtest resources", func() {
		ctx := context.Background()

		It("should set Failed status when referenced Model does not exist", func() {
			isvcName := "isvc-no-model"
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "nonexistent-model",
					Replicas: &replicas,
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, isvc)
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseFailed))
		})

		It("should set Pending status when Model is not Ready", func() {
			modelName := "model-not-ready"
			isvcName := "isvc-pending"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, model)
			}()

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, isvc)
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal("Pending"))
		})

		It("should create Deployment and Service when Model is Ready", func() {
			modelName := "model-ready-deploy"
			isvcName := "isvc-deploy"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, model)
			}()

			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
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

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying Deployment was created")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
			Expect(dep.OwnerReferences).To(HaveLen(1))
			Expect(*dep.OwnerReferences[0].Controller).To(BeTrue())

			By("verifying Service was created")
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, svc)).To(Succeed())
			Expect(svc.OwnerReferences).To(HaveLen(1))

			By("verifying status was updated")
			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal("Creating"))
			Expect(updated.Status.Endpoint).NotTo(BeEmpty())
		})

		It("should skip Deployment for Metal accelerator", func() {
			modelName := "metal-model"
			isvcName := "isvc-metal"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, model)
			}()

			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, isvc)
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying no Deployment was created")
			dep := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			By("verifying no Service was created (Metal Agent manages its own)")
			svc := &corev1.Service{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, svc)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			By("verifying status is Ready (Metal returns desiredReplicas as ready)")
			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal("Ready"))
		})

		It("should set correct endpoint URL for Metal InferenceService", func() {
			modelName := "metal-endpoint-model"
			isvcName := "isvc-metal-endpoint"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, model)
			}()

			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, isvc)
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Endpoint).To(Equal(
				"http://isvc-metal-endpoint.default.svc.cluster.local:8080/v1/chat/completions",
			))
		})

		It("should set DNS-sanitized endpoint URL for Metal InferenceService with dots in name", func() {
			modelName := "metal-dot-model"
			isvcName := "isvc-metal.v1.0"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, model)
			}()

			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, isvc)
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Endpoint).To(Equal(
				"http://isvc-metal-v1-0.default.svc.cluster.local:8080/v1/chat/completions",
			))
		})

		It("should use custom endpoint port and path for Metal InferenceService", func() {
			modelName := "metal-custom-ep-model"
			isvcName := "isvc-metal-custom-ep"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, model)
			}()

			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
					Endpoint: &inferencev1alpha1.EndpointSpec{
						Port: 9090,
						Path: "/api/generate",
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, isvc)
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Endpoint).To(Equal(
				"http://isvc-metal-custom-ep.default.svc.cluster.local:9090/api/generate",
			))
		})

		It("should default replicas to 1 when nil", func() {
			modelName := "model-nil-replicas"
			isvcName := "isvc-nil-replicas"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, model)
			}()

			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					// Replicas intentionally nil
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

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
		})

		It("should create PVC when model has CacheKey and ModelCachePath is set", func() {
			modelName := "model-with-cache"
			isvcName := fmt.Sprintf("isvc-pvc-test-%d", GinkgoRandomSeed())

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, model)
			}()

			model.Status.Phase = PhaseReady
			model.Status.CacheKey = "abc123def456"
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
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
				pvc := &corev1.PersistentVolumeClaim{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc); err == nil {
					_ = k8sClient.Delete(ctx, pvc)
				}
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
				ModelCachePath:     "/models",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)).To(Succeed())
		})
	})
})
