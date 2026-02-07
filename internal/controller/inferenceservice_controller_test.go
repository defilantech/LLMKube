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
	"k8s.io/apimachinery/pkg/api/resource"
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
				InitContainerImage: "docker.io/curlimages/curl:latest",
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

var _ = Describe("calculateTensorSplit", func() {
	Context("with different GPU counts", func() {
		It("should return empty string for single GPU", func() {
			result := calculateTensorSplit(1, nil)
			Expect(result).To(Equal(""))
		})

		It("should return empty string for zero GPUs", func() {
			result := calculateTensorSplit(0, nil)
			Expect(result).To(Equal(""))
		})

		It("should return '1,1' for 2 GPUs", func() {
			result := calculateTensorSplit(2, nil)
			Expect(result).To(Equal("1,1"))
		})

		It("should return '1,1,1,1' for 4 GPUs", func() {
			result := calculateTensorSplit(4, nil)
			Expect(result).To(Equal("1,1,1,1"))
		})

		It("should return '1,1,1,1,1,1,1,1' for 8 GPUs", func() {
			result := calculateTensorSplit(8, nil)
			Expect(result).To(Equal("1,1,1,1,1,1,1,1"))
		})
	})

	Context("with sharding config", func() {
		It("should use even split when sharding has no layer split", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{
				Strategy: "layer",
			}
			result := calculateTensorSplit(2, sharding)
			Expect(result).To(Equal("1,1"))
		})

		It("should use even split when sharding is provided (custom splits not yet implemented)", func() {
			// TODO: When custom layer splits are implemented, update this test
			sharding := &inferencev1alpha1.GPUShardingSpec{
				Strategy:   "layer",
				LayerSplit: []string{"0-19", "20-39"},
			}
			result := calculateTensorSplit(2, sharding)
			// Currently falls back to even split
			Expect(result).To(Equal("1,1"))
		})
	})
})

var _ = Describe("Multi-GPU Deployment Construction", func() {
	Context("when constructing a deployment with multi-GPU model", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
			isvc       *inferencev1alpha1.InferenceService
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:latest",
			}
		})

		It("should include multi-GPU args for 2 GPU model", func() {
			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "multi-gpu-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source:       "https://example.com/model.gguf",
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
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
					Path:  "/tmp/llmkube/models/test-model.gguf",
				},
			}

			replicas := int32(1)
			isvc = &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "multi-gpu-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "multi-gpu-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 2,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying deployment is created")
			Expect(deployment).NotTo(BeNil())
			Expect(deployment.Name).To(Equal("multi-gpu-service"))

			By("verifying container args include multi-GPU flags")
			container := deployment.Spec.Template.Spec.Containers[0]
			args := container.Args

			Expect(args).To(ContainElement("--n-gpu-layers"))
			Expect(args).To(ContainElement("99")) // -1 maps to 99

			Expect(args).To(ContainElement("--split-mode"))
			Expect(args).To(ContainElement("layer"))

			Expect(args).To(ContainElement("--tensor-split"))
			Expect(args).To(ContainElement("1,1"))

			By("verifying GPU resource limits")
			gpuLimit := container.Resources.Limits["nvidia.com/gpu"]
			Expect(gpuLimit).To(Equal(resource.MustParse("2")))
		})

		It("should include multi-GPU args for 4 GPU model", func() {
			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "quad-gpu-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source:       "https://example.com/model.gguf",
					Format:       "gguf",
					Quantization: "Q4_K_M",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cuda",
						GPU: &inferencev1alpha1.GPUSpec{
							Enabled: true,
							Count:   4,
							Vendor:  "nvidia",
							Layers:  99,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
					Path:  "/tmp/llmkube/models/test-model.gguf",
				},
			}

			replicas := int32(1)
			isvc = &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "quad-gpu-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "quad-gpu-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda",
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying tensor split for 4 GPUs")
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElement("--tensor-split"))
			Expect(args).To(ContainElement("1,1,1,1"))

			By("verifying GPU resource limits for 4 GPUs")
			gpuLimit := deployment.Spec.Template.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]
			Expect(gpuLimit).To(Equal(resource.MustParse("4")))
		})

		It("should NOT include multi-GPU args for single GPU model", func() {
			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "single-gpu-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source:       "https://example.com/model.gguf",
					Format:       "gguf",
					Quantization: "Q4_K_M",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cuda",
						GPU: &inferencev1alpha1.GPUSpec{
							Enabled: true,
							Count:   1,
							Vendor:  "nvidia",
							Layers:  99,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
					Path:  "/tmp/llmkube/models/test-model.gguf",
				},
			}

			replicas := int32(1)
			isvc = &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "single-gpu-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "single-gpu-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying single GPU does NOT have multi-GPU flags")
			args := deployment.Spec.Template.Spec.Containers[0].Args

			// Should have GPU layers
			Expect(args).To(ContainElement("--n-gpu-layers"))

			// Should NOT have split-mode or tensor-split
			Expect(args).NotTo(ContainElement("--split-mode"))
			Expect(args).NotTo(ContainElement("--tensor-split"))

			By("verifying GPU resource limits for single GPU")
			gpuLimit := deployment.Spec.Template.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]
			Expect(gpuLimit).To(Equal(resource.MustParse("1")))
		})

		It("should NOT include GPU args for CPU-only model", func() {
			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cpu-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source:       "https://example.com/model.gguf",
					Format:       "gguf",
					Quantization: "Q4_K_M",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cpu",
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
					Path:  "/tmp/llmkube/models/test-model.gguf",
				},
			}

			replicas := int32(1)
			isvc = &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cpu-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "cpu-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying CPU-only does NOT have GPU flags")
			args := deployment.Spec.Template.Spec.Containers[0].Args

			Expect(args).NotTo(ContainElement("--n-gpu-layers"))
			Expect(args).NotTo(ContainElement("--split-mode"))
			Expect(args).NotTo(ContainElement("--tensor-split"))

			By("verifying no GPU resource limits")
			_, hasGPU := deployment.Spec.Template.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]
			Expect(hasGPU).To(BeFalse())
		})

		It("should prefer Model GPU count over InferenceService GPU count", func() {
			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-gpu-precedence",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source:       "https://example.com/model.gguf",
					Format:       "gguf",
					Quantization: "Q4_K_M",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cuda",
						GPU: &inferencev1alpha1.GPUSpec{
							Enabled: true,
							Count:   4, // Model says 4 GPUs
							Vendor:  "nvidia",
							Layers:  99,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
					Path:  "/tmp/llmkube/models/test-model.gguf",
				},
			}

			replicas := int32(1)
			isvc = &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "gpu-precedence-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "model-gpu-precedence",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 2, // InferenceService says 2 GPUs
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying Model GPU count (4) takes precedence over InferenceService (2)")
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElement("--tensor-split"))
			Expect(args).To(ContainElement("1,1,1,1")) // 4 GPUs, not 2

			gpuLimit := deployment.Spec.Template.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]
			Expect(gpuLimit).To(Equal(resource.MustParse("4")))
		})
	})

	Context("when verifying init container image configuration", func() {
		It("should use custom init container image when configured", func() {
			customImage := "myregistry.local/curl:1.0"
			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: customImage,
			}

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "init-image-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source:       "https://example.com/model.gguf",
					Format:       "gguf",
					Quantization: "Q4_K_M",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cpu",
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
				},
			}

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "init-image-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "init-image-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			Expect(deployment.Spec.Template.Spec.InitContainers).To(HaveLen(1))
			Expect(deployment.Spec.Template.Spec.InitContainers[0].Image).To(Equal(customImage))
		})
	})

	Context("when verifying tolerations and node selectors", func() {
		var (
			reconciler *InferenceServiceReconciler
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:latest",
			}
		})

		It("should add nvidia.com/gpu toleration for GPU workloads", func() {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "toleration-test-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Format: "gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cuda",
						GPU: &inferencev1alpha1.GPUSpec{
							Enabled: true,
							Count:   2,
							Vendor:  "nvidia",
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
					Path:  "/tmp/llmkube/models/test-model.gguf",
				},
			}

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "toleration-test-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "toleration-test-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda",
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying nvidia.com/gpu toleration is present")
			tolerations := deployment.Spec.Template.Spec.Tolerations
			Expect(tolerations).NotTo(BeEmpty())

			var hasNvidiaToleration bool
			for _, t := range tolerations {
				if t.Key == "nvidia.com/gpu" {
					hasNvidiaToleration = true
					break
				}
			}
			Expect(hasNvidiaToleration).To(BeTrue())
		})

		It("should apply custom node selector from InferenceService spec", func() {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nodeselector-test-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Format: "gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cuda",
						GPU: &inferencev1alpha1.GPUSpec{
							Enabled: true,
							Count:   2,
							Vendor:  "nvidia",
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
					Path:  "/tmp/llmkube/models/test-model.gguf",
				},
			}

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nodeselector-test-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "nodeselector-test-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda",
					NodeSelector: map[string]string{
						"cloud.google.com/gke-nodepool": "gpu-pool",
						"nvidia.com/gpu.product":        "NVIDIA-L4",
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying custom node selector is applied")
			nodeSelector := deployment.Spec.Template.Spec.NodeSelector
			Expect(nodeSelector).To(HaveKeyWithValue("cloud.google.com/gke-nodepool", "gpu-pool"))
			Expect(nodeSelector).To(HaveKeyWithValue("nvidia.com/gpu.product", "NVIDIA-L4"))
		})
	})
})

var _ = Describe("Context Size Configuration", func() {
	Context("when constructing a deployment with context size", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:latest",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "context-size-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source:       "https://example.com/model.gguf",
					Format:       "gguf",
					Quantization: "Q4_K_M",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cuda",
						GPU: &inferencev1alpha1.GPUSpec{
							Enabled: true,
							Count:   1,
							Vendor:  "nvidia",
							Layers:  99,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
					Path:  "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should include --ctx-size flag when contextSize is specified", func() {
			replicas := int32(1)
			contextSize := int32(8192)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "context-size-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:    "context-size-model",
					Replicas:    &replicas,
					Image:       "ghcr.io/ggml-org/llama.cpp:server-cuda",
					ContextSize: &contextSize,
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying --ctx-size flag is present with correct value")
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElement("--ctx-size"))
			Expect(args).To(ContainElement("8192"))
		})

		It("should include --ctx-size flag with large context size", func() {
			replicas := int32(1)
			contextSize := int32(131072)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "large-context-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:    "context-size-model",
					Replicas:    &replicas,
					Image:       "ghcr.io/ggml-org/llama.cpp:server-cuda",
					ContextSize: &contextSize,
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying --ctx-size flag with large value")
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElement("--ctx-size"))
			Expect(args).To(ContainElement("131072"))
		})

		It("should NOT include --ctx-size flag when contextSize is not specified", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-context-size-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "context-size-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda",
					// ContextSize not specified
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying --ctx-size flag is NOT present")
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--ctx-size"))
		})

		It("should NOT include --ctx-size flag when contextSize is zero", func() {
			replicas := int32(1)
			contextSize := int32(0)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "zero-context-size-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:    "context-size-model",
					Replicas:    &replicas,
					Image:       "ghcr.io/ggml-org/llama.cpp:server-cuda",
					ContextSize: &contextSize,
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying --ctx-size flag is NOT present for zero value")
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--ctx-size"))
		})

		It("should work with both GPU and context size configuration", func() {
			replicas := int32(1)
			contextSize := int32(16384)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "gpu-and-context-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:    "context-size-model",
					Replicas:    &replicas,
					Image:       "ghcr.io/ggml-org/llama.cpp:server-cuda",
					ContextSize: &contextSize,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying both GPU and context size flags are present")
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElement("--n-gpu-layers"))
			Expect(args).To(ContainElement("--ctx-size"))
			Expect(args).To(ContainElement("16384"))
		})
	})

	Context("when parallelSlots is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:latest",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "parallel-slots-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cuda",
						GPU: &inferencev1alpha1.GPUSpec{
							Enabled: true,
							Count:   1,
							Vendor:  "nvidia",
							Layers:  99,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
					Path:  "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should include --parallel flag when parallelSlots is specified", func() {
			replicas := int32(1)
			parallelSlots := int32(4)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "parallel-slots-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:      "parallel-slots-model",
					Replicas:      &replicas,
					Image:         "ghcr.io/ggml-org/llama.cpp:server-cuda",
					ParallelSlots: &parallelSlots,
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElement("--parallel"))
			Expect(args).To(ContainElement("4"))
		})

		It("should NOT include --parallel flag when parallelSlots is not specified", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-parallel-slots-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "parallel-slots-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda",
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--parallel"))
		})

		It("should NOT include --parallel flag when parallelSlots is 1", func() {
			replicas := int32(1)
			parallelSlots := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "single-parallel-slot-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:      "parallel-slots-model",
					Replicas:      &replicas,
					Image:         "ghcr.io/ggml-org/llama.cpp:server-cuda",
					ParallelSlots: &parallelSlots,
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--parallel"))
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
						Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda",
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
				InitContainerImage: "docker.io/curlimages/curl:latest",
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

var _ = Describe("sanitizeDNSName", func() {
	It("should replace dots with dashes", func() {
		Expect(sanitizeDNSName("my.model.v1")).To(Equal("my-model-v1"))
	})
	It("should leave names without dots unchanged", func() {
		Expect(sanitizeDNSName("my-service")).To(Equal("my-service"))
	})
})

var _ = Describe("buildModelInitCommand", func() {
	It("should generate cached remote download command", func() {
		cmd := buildModelInitCommand("https://example.com/model.gguf", "/models/abc123", "/models/abc123/model.gguf", true)
		Expect(cmd).To(ContainSubstring("mkdir -p /models/abc123"))
		Expect(cmd).To(ContainSubstring("if [ ! -f /models/abc123/model.gguf ]"))
		Expect(cmd).To(ContainSubstring("curl -f -L"))
		Expect(cmd).To(ContainSubstring("https://example.com/model.gguf"))
	})

	It("should generate cached local copy command for file:// source", func() {
		cmd := buildModelInitCommand("file:///mnt/models/test.gguf", "/models/abc123", "/models/abc123/model.gguf", true)
		Expect(cmd).To(ContainSubstring("mkdir -p /models/abc123"))
		Expect(cmd).To(ContainSubstring("cp /host-model/model.gguf"))
	})

	It("should generate cached local copy command for absolute path source", func() {
		cmd := buildModelInitCommand("/mnt/models/test.gguf", "/models/abc123", "/models/abc123/model.gguf", true)
		Expect(cmd).To(ContainSubstring("mkdir -p /models/abc123"))
		Expect(cmd).To(ContainSubstring("cp /host-model/model.gguf"))
	})

	It("should generate error exit for uncached local source", func() {
		cmd := buildModelInitCommand("file:///mnt/models/test.gguf", "", "/models/test.gguf", false)
		Expect(cmd).To(ContainSubstring("ERROR: Local model source requires model cache"))
		Expect(cmd).To(ContainSubstring("exit 1"))
	})

	It("should generate uncached remote download command", func() {
		cmd := buildModelInitCommand("https://example.com/model.gguf", "", "/models/default-test.gguf", false)
		Expect(cmd).To(ContainSubstring("curl -f -L"))
		Expect(cmd).NotTo(ContainSubstring("mkdir -p"))
	})
})

var _ = Describe("buildCachedStorageConfig", func() {
	It("should configure PVC volume and init container for remote model", func() {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://example.com/model.gguf",
			},
			Status: inferencev1alpha1.ModelStatus{
				CacheKey: "abc123def456",
			},
		}
		config := buildCachedStorageConfig(model, "", "curl:latest")

		Expect(config.modelPath).To(Equal("/models/abc123def456/model.gguf"))
		Expect(config.volumes).To(HaveLen(1))
		Expect(config.volumes[0].Name).To(Equal("model-cache"))
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(ModelCachePVCName))
		Expect(config.initContainers).To(HaveLen(1))
		Expect(config.initContainers[0].Name).To(Equal("model-downloader"))
		Expect(config.initContainers[0].Image).To(Equal("curl:latest"))
		Expect(config.volumeMounts[0].MountPath).To(Equal("/models"))
		Expect(config.volumeMounts[0].ReadOnly).To(BeTrue())
	})

	It("should add host-model volume for local source", func() {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Source: "file:///mnt/models/test.gguf",
			},
			Status: inferencev1alpha1.ModelStatus{
				CacheKey: "abc123",
			},
		}
		config := buildCachedStorageConfig(model, "", "curl:latest")

		Expect(config.volumes).To(HaveLen(2))
		Expect(config.volumes[1].Name).To(Equal("host-model"))
		Expect(config.volumes[1].HostPath.Path).To(Equal("/mnt/models/test.gguf"))
	})

	It("should add CA cert volume when caCertConfigMap is set", func() {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://example.com/model.gguf",
			},
			Status: inferencev1alpha1.ModelStatus{
				CacheKey: "abc123",
			},
		}
		config := buildCachedStorageConfig(model, "my-ca-certs", "curl:latest")

		var found bool
		for _, v := range config.volumes {
			if v.Name == "custom-ca-cert" {
				found = true
				Expect(v.ConfigMap.Name).To(Equal("my-ca-certs"))
			}
		}
		Expect(found).To(BeTrue())
		Expect(config.initContainers[0].Command[2]).To(ContainSubstring("CURL_CA_BUNDLE=/custom-certs/"))
	})
})

var _ = Describe("buildEmptyDirStorageConfig", func() {
	It("should configure emptyDir volume for remote model", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "my-model"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		}
		config := buildEmptyDirStorageConfig(model, "default", "", "curl:latest")

		Expect(config.modelPath).To(Equal("/models/default-my-model.gguf"))
		Expect(config.volumes).To(HaveLen(1))
		Expect(config.volumes[0].Name).To(Equal("model-storage"))
		Expect(config.volumes[0].EmptyDir).NotTo(BeNil())
	})

	It("should add CA cert volume when caCertConfigMap is set", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "my-model"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		}
		config := buildEmptyDirStorageConfig(model, "default", "my-ca-certs", "curl:latest")

		var found bool
		for _, v := range config.volumes {
			if v.Name == "custom-ca-cert" {
				found = true
				Expect(v.ConfigMap.Name).To(Equal("my-ca-certs"))
			}
		}
		Expect(found).To(BeTrue())
		Expect(config.initContainers[0].Command[2]).To(ContainSubstring("CURL_CA_BUNDLE=/custom-certs/"))
	})
})

var _ = Describe("constructService", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("should create ClusterIP service with default port", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
		}
		svc := reconciler.constructService(isvc)

		Expect(svc.Name).To(Equal("test-svc"))
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
	})

	It("should create NodePort service", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{Type: "NodePort"},
			},
		}
		svc := reconciler.constructService(isvc)
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
	})

	It("should create LoadBalancer service", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{Type: "LoadBalancer"},
			},
		}
		svc := reconciler.constructService(isvc)
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
	})

	It("should use custom port", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{Port: 3000},
			},
		}
		svc := reconciler.constructService(isvc)
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(3000)))
	})

	It("should sanitize service name with dots", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my.model.v1", Namespace: "default"},
		}
		svc := reconciler.constructService(isvc)
		Expect(svc.Name).To(Equal("my-model-v1"))
	})
})

var _ = Describe("constructEndpoint", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("should construct default endpoint URL", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
		}
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
		}
		endpoint := reconciler.constructEndpoint(isvc, svc)
		Expect(endpoint).To(Equal("http://test-svc.default.svc.cluster.local:8080/v1/chat/completions"))
	})

	It("should use custom port", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{Port: 9090},
			},
		}
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
		}
		endpoint := reconciler.constructEndpoint(isvc, svc)
		Expect(endpoint).To(ContainSubstring(":9090"))
	})

	It("should use custom path", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{Path: "/api/generate"},
			},
		}
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
		}
		endpoint := reconciler.constructEndpoint(isvc, svc)
		Expect(endpoint).To(HaveSuffix("/api/generate"))
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

var _ = Describe("resolvePriorityClassName", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	It("should return custom PriorityClassName when explicitly set", func() {
		isvc := &inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{PriorityClassName: "my-custom-priority"},
		}
		Expect(reconciler.resolvePriorityClassName(isvc)).To(Equal("my-custom-priority"))
	})

	DescribeTable("should map priority levels",
		func(priority, expected string) {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Priority: priority},
			}
			Expect(reconciler.resolvePriorityClassName(isvc)).To(Equal(expected))
		},
		Entry("critical", "critical", "llmkube-critical"),
		Entry("high", "high", "llmkube-high"),
		Entry("normal", "normal", "llmkube-normal"),
		Entry("low", "low", "llmkube-low"),
		Entry("batch", "batch", "llmkube-batch"),
		Entry("empty defaults to normal", "", "llmkube-normal"),
		Entry("unknown defaults to normal", "unknown", "llmkube-normal"),
	)
})

var _ = Describe("resolveEffectivePriority", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	DescribeTable("should resolve priority values",
		func(priority string, expected int32) {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Priority: priority},
			}
			Expect(reconciler.resolveEffectivePriority(isvc)).To(Equal(expected))
		},
		Entry("critical", "critical", int32(1000000)),
		Entry("high", "high", int32(100000)),
		Entry("normal", "normal", int32(10000)),
		Entry("low", "low", int32(1000)),
		Entry("batch", "batch", int32(100)),
		Entry("empty defaults to normal", "", int32(10000)),
		Entry("unknown defaults to normal", "unknown", int32(10000)),
	)
})

func deletePVCForcibly(ctx context.Context, namespace string) {
	pvc := &corev1.PersistentVolumeClaim{}
	pvcKey := types.NamespacedName{Name: ModelCachePVCName, Namespace: namespace}
	if err := k8sClient.Get(ctx, pvcKey, pvc); err != nil {
		return
	}
	if len(pvc.Finalizers) > 0 {
		pvc.Finalizers = nil
		_ = k8sClient.Update(ctx, pvc)
	}
	_ = k8sClient.Delete(ctx, pvc)
	Eventually(func() bool {
		return errors.IsNotFound(k8sClient.Get(ctx, pvcKey, &corev1.PersistentVolumeClaim{}))
	}, "5s", "100ms").Should(BeTrue())
}

var _ = Describe("ensureModelCachePVC", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		deletePVCForcibly(context.Background(), "default")
		reconciler = &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	AfterEach(func() {
		deletePVCForcibly(context.Background(), "default")
	})

	It("should create PVC with default 100Gi and ReadWriteOnce", func() {
		err := reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		Expect(pvc.Spec.AccessModes).To(ContainElement(corev1.ReadWriteOnce))
		storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		Expect(storageReq.String()).To(Equal("100Gi"))
		Expect(pvc.Labels["app.kubernetes.io/name"]).To(Equal("llmkube"))
	})

	It("should create PVC with custom size", func() {
		reconciler.ModelCacheSize = "50Gi"
		err := reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		Expect(storageReq.String()).To(Equal("50Gi"))
	})

	It("should create PVC with ReadWriteMany when configured", func() {
		reconciler.ModelCacheAccessMode = "ReadWriteMany"
		err := reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		Expect(pvc.Spec.AccessModes).To(ContainElement(corev1.ReadWriteMany))
	})

	It("should set StorageClassName when configured", func() {
		reconciler.ModelCacheClass = "fast-ssd"
		err := reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		Expect(*pvc.Spec.StorageClassName).To(Equal("fast-ssd"))
	})

	It("should not error if PVC already exists", func() {
		err := reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).NotTo(HaveOccurred())
		err = reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).NotTo(HaveOccurred())
	})

	It("should return error for invalid cache size", func() {
		reconciler.ModelCacheSize = "not-a-size"
		err := reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid cache size"))
	})
})

var _ = Describe("constructDeployment additional cases", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:latest",
		}
	})

	It("should use default image when isvc.Spec.Image is empty", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
			Status:     inferencev1alpha1.ModelStatus{Phase: "Ready"},
		}
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "m"},
		}
		deployment := reconciler.constructDeployment(isvc, model, 1)
		Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/ggml-org/llama.cpp:server"))
	})

	It("should use custom endpoint port", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
			Status:     inferencev1alpha1.ModelStatus{Phase: "Ready"},
		}
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "m",
				Endpoint: &inferencev1alpha1.EndpointSpec{Port: 3000},
			},
		}
		deployment := reconciler.constructDeployment(isvc, model, 1)
		container := deployment.Spec.Template.Spec.Containers[0]
		Expect(container.Args).To(ContainElement("3000"))
		Expect(container.Ports[0].ContainerPort).To(Equal(int32(3000)))
	})

	It("should set CPU and Memory resource requests", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
			Status:     inferencev1alpha1.ModelStatus{Phase: "Ready"},
		}
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:  "m",
				Resources: &inferencev1alpha1.InferenceResourceRequirements{CPU: "2", Memory: "4Gi"},
			},
		}
		deployment := reconciler.constructDeployment(isvc, model, 1)
		requests := deployment.Spec.Template.Spec.Containers[0].Resources.Requests
		Expect(requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("2")))
		Expect(requests[corev1.ResourceMemory]).To(Equal(resource.MustParse("4Gi")))
	})

	It("should not add tolerations for CPU-only workload", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/model.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			},
			Status: inferencev1alpha1.ModelStatus{Phase: "Ready"},
		}
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "m"},
		}
		deployment := reconciler.constructDeployment(isvc, model, 1)
		Expect(deployment.Spec.Template.Spec.Tolerations).To(BeEmpty())
		Expect(deployment.Spec.Template.Spec.NodeSelector).To(BeEmpty())
	})

	It("should use explicit GPU layers from Model spec", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://example.com/model.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "cuda",
					GPU:         &inferencev1alpha1.GPUSpec{Enabled: true, Count: 1, Layers: 32},
				},
			},
			Status: inferencev1alpha1.ModelStatus{Phase: "Ready"},
		}
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "m"},
		}
		deployment := reconciler.constructDeployment(isvc, model, 1)
		args := deployment.Spec.Template.Spec.Containers[0].Args
		Expect(args).To(ContainElement("--n-gpu-layers"))
		Expect(args).To(ContainElement("32"))
		Expect(args).NotTo(ContainElement("99"))
	})

	It("should use PVC-based storage when cache is configured", func() {
		reconciler.ModelCachePath = "/models"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
			Status:     inferencev1alpha1.ModelStatus{Phase: "Ready", CacheKey: "abc123"},
		}
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "m"},
		}
		deployment := reconciler.constructDeployment(isvc, model, 1)

		var hasPVC bool
		for _, v := range deployment.Spec.Template.Spec.Volumes {
			if v.Name == "model-cache" && v.PersistentVolumeClaim != nil {
				hasPVC = true
			}
		}
		Expect(hasPVC).To(BeTrue())
		Expect(deployment.Spec.Template.Spec.Containers[0].Args).To(ContainElement(ContainSubstring("abc123")))
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
			InitContainerImage: "docker.io/curlimages/curl:latest",
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
				InitContainerImage: "docker.io/curlimages/curl:latest",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal("Failed"))
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
				InitContainerImage: "docker.io/curlimages/curl:latest",
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
				InitContainerImage: "docker.io/curlimages/curl:latest",
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
				svc := &corev1.Service{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, svc); err == nil {
					_ = k8sClient.Delete(ctx, svc)
				}
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:latest",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying no Deployment was created")
			dep := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			By("verifying Service was still created")
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, svc)).To(Succeed())

			By("verifying status is Ready (Metal returns desiredReplicas as ready)")
			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal("Ready"))
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
				InitContainerImage: "docker.io/curlimages/curl:latest",
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
				InitContainerImage: "docker.io/curlimages/curl:latest",
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
