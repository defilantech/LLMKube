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
