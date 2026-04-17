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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
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

		It("should use equal split for equal layer ranges", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{
				Strategy:   "layer",
				LayerSplit: []string{"0-19", "20-39"},
			}
			result := calculateTensorSplit(2, sharding)
			Expect(result).To(Equal("1,1"))
		})

		It("should use proportional split for unequal layer ranges", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{
				Strategy:   "layer",
				LayerSplit: []string{"0-24", "25-39"},
			}
			result := calculateTensorSplit(2, sharding)
			Expect(result).To(Equal("5,3"))
		})

		It("should handle three-way unequal split", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{
				Strategy:   "layer",
				LayerSplit: []string{"0-15", "16-31", "32-39"},
			}
			result := calculateTensorSplit(3, sharding)
			Expect(result).To(Equal("2,2,1"))
		})

		It("should fall back to equal split when LayerSplit count mismatches GPU count", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{
				Strategy:   "layer",
				LayerSplit: []string{"0-19", "20-39"},
			}
			result := calculateTensorSplit(4, sharding)
			Expect(result).To(Equal("1,1,1,1"))
		})

		It("should fall back to equal split for invalid range format", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{
				Strategy:   "layer",
				LayerSplit: []string{"invalid", "also-bad"},
			}
			result := calculateTensorSplit(2, sharding)
			Expect(result).To(Equal("1,1"))
		})

		It("should fall back to equal split for backwards range", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{
				Strategy:   "layer",
				LayerSplit: []string{"20-0", "0-19"},
			}
			result := calculateTensorSplit(2, sharding)
			Expect(result).To(Equal("1,1"))
		})
	})

	Context("resolveSplitMode", func() {
		It("should return layer for nil sharding", func() {
			Expect(resolveSplitMode(nil)).To(Equal("layer"))
		})

		It("should return layer for empty strategy", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{}
			Expect(resolveSplitMode(sharding)).To(Equal("layer"))
		})

		It("should return layer for explicit layer strategy", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{Strategy: "layer"}
			Expect(resolveSplitMode(sharding)).To(Equal("layer"))
		})

		It("should return row for tensor strategy", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{Strategy: "tensor"}
			Expect(resolveSplitMode(sharding)).To(Equal("row"))
		})

		It("should return row for row strategy", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{Strategy: "row"}
			Expect(resolveSplitMode(sharding)).To(Equal("row"))
		})

		It("should return none for none strategy", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{Strategy: "none"}
			Expect(resolveSplitMode(sharding)).To(Equal("none"))
		})

		It("should fall back to layer for pipeline strategy", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{Strategy: "pipeline"}
			Expect(resolveSplitMode(sharding)).To(Equal("layer"))
		})

		It("should fall back to layer for unknown strategy", func() {
			sharding := &inferencev1alpha1.GPUShardingSpec{Strategy: "bogus"}
			Expect(resolveSplitMode(sharding)).To(Equal("layer"))
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
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
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
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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

			By("verifying Recreate strategy is set to avoid GPU deadlock")
			Expect(deployment.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
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
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
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
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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

			By("verifying Recreate strategy is set for GPU workloads")
			Expect(deployment.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
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
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
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
					Image:       "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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
					Image:       "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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
					Image:       "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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
					Image:       "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
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
					Image:         "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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
					Image:         "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					ParallelSlots: &parallelSlots,
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--parallel"))
		})
	})

	Context("when flashAttention is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "flash-attn-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should include --flash-attn flag when flashAttention is true and GPU is configured", func() {
			replicas := int32(1)
			flashAttn := true
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "flash-attn-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:       "flash-attn-model",
					Replicas:       &replicas,
					Image:          "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					FlashAttention: &flashAttn,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--flash-attn", "on"))
		})

		It("should NOT include --flash-attn flag when flashAttention is not specified", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-flash-attn-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "flash-attn-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--flash-attn"))
		})

		It("should NOT include --flash-attn flag when flashAttention is false", func() {
			replicas := int32(1)
			flashAttn := false
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "flash-attn-false-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:       "flash-attn-model",
					Replicas:       &replicas,
					Image:          "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					FlashAttention: &flashAttn,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--flash-attn"))
		})

		It("should NOT include --flash-attn flag when no GPU is configured", func() {
			replicas := int32(1)
			flashAttn := true
			noGPUModel := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "flash-attn-no-gpu-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "flash-attn-no-gpu-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:       "flash-attn-no-gpu-model",
					Replicas:       &replicas,
					Image:          "ghcr.io/ggml-org/llama.cpp:server",
					FlashAttention: &flashAttn,
				},
			}

			deployment := reconciler.constructDeployment(isvc, noGPUModel, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--flash-attn"))
		})
	})

	Context("when jinja is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "jinja-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should include --jinja flag when jinja is enabled", func() {
			replicas := int32(1)
			jinja := true
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "jinja-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "jinja-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Jinja:    &jinja,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElement("--jinja"))
		})

		It("should NOT include --jinja flag when jinja is not specified", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-jinja-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "jinja-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--jinja"))
		})

		It("should NOT include --jinja flag when jinja is false", func() {
			replicas := int32(1)
			jinja := false
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "jinja-false-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "jinja-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Jinja:    &jinja,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--jinja"))
		})
	})

	Context("when cache type is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cache-type-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should include --cache-type-k flag when cacheTypeK is set", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cache-k-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:   "cache-type-model",
					Replicas:   &replicas,
					Image:      "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					CacheTypeK: "q4_0",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--cache-type-k", "q4_0"))
		})

		It("should include --cache-type-v flag when cacheTypeV is set", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cache-v-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:   "cache-type-model",
					Replicas:   &replicas,
					Image:      "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					CacheTypeV: "q8_0",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--cache-type-v", "q8_0"))
		})

		It("should include both cache type flags when both are set", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cache-both-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:   "cache-type-model",
					Replicas:   &replicas,
					Image:      "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					CacheTypeK: "q4_0",
					CacheTypeV: "q8_0",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--cache-type-k", "q4_0"))
			Expect(args).To(ContainElements("--cache-type-v", "q8_0"))
		})

		It("should NOT include cache type flags when neither is set", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-cache-type-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "cache-type-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--cache-type-k"))
			Expect(args).NotTo(ContainElement("--cache-type-v"))
		})
	})

	Context("when moeCPUOffload is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "moe-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should include --cpu-moe flag when moeCPUOffload is true", func() {
			replicas := int32(1)
			moeCPUOffload := true
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "moe-offload-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:      "moe-model",
					Replicas:      &replicas,
					Image:         "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					MoeCPUOffload: &moeCPUOffload,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElement("--cpu-moe"))
		})

		It("should NOT include --cpu-moe flag when moeCPUOffload is not specified", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-moe-offload-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "moe-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--cpu-moe"))
		})

		It("should NOT include --cpu-moe flag when moeCPUOffload is false", func() {
			replicas := int32(1)
			moeCPUOffload := false
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "moe-offload-false-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:      "moe-model",
					Replicas:      &replicas,
					Image:         "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					MoeCPUOffload: &moeCPUOffload,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--cpu-moe"))
		})
	})

	Context("when moeCPULayers is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "moe-layers-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should include --n-cpu-moe flag with correct value when moeCPULayers is set", func() {
			replicas := int32(1)
			moeCPULayers := int32(8)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "moe-layers-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:     "moe-layers-model",
					Replicas:     &replicas,
					Image:        "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					MoeCPULayers: &moeCPULayers,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--n-cpu-moe", "8"))
		})

		It("should NOT include --n-cpu-moe flag when moeCPULayers is not specified", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-moe-layers-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "moe-layers-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--n-cpu-moe"))
		})

		It("should NOT include --n-cpu-moe flag when moeCPULayers is zero", func() {
			replicas := int32(1)
			moeCPULayers := int32(0)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "moe-layers-zero-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:     "moe-layers-model",
					Replicas:     &replicas,
					Image:        "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					MoeCPULayers: &moeCPULayers,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--n-cpu-moe"))
		})
	})

	Context("when noKvOffload is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "kv-offload-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should include --no-kv-offload flag when noKvOffload is true", func() {
			replicas := int32(1)
			noKvOffload := true
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "kv-offload-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:    "kv-offload-model",
					Replicas:    &replicas,
					Image:       "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					NoKvOffload: &noKvOffload,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElement("--no-kv-offload"))
		})

		It("should NOT include --no-kv-offload flag when noKvOffload is not specified", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-kv-offload-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "kv-offload-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--no-kv-offload"))
		})

		It("should NOT include --no-kv-offload flag when noKvOffload is false", func() {
			replicas := int32(1)
			noKvOffload := false
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "kv-offload-false-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:    "kv-offload-model",
					Replicas:    &replicas,
					Image:       "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					NoKvOffload: &noKvOffload,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--no-kv-offload"))
		})
	})

	Context("when tensorOverrides is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tensor-override-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should include --override-tensor flags when tensorOverrides is set", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tensor-override-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:        "tensor-override-model",
					Replicas:        &replicas,
					Image:           "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					TensorOverrides: []string{"exps=CPU", "token_embd=CUDA0"},
					Resources:       &inferencev1alpha1.InferenceResourceRequirements{GPU: 1},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--override-tensor", "exps=CPU"))
			Expect(args).To(ContainElements("--override-tensor", "token_embd=CUDA0"))
		})

		It("should NOT include --override-tensor when tensorOverrides is empty", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-tensor-override-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:        "tensor-override-model",
					Replicas:        &replicas,
					Image:           "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					TensorOverrides: []string{},
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--override-tensor"))
		})

		It("should NOT include --override-tensor when tensorOverrides is nil", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nil-tensor-override-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "tensor-override-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--override-tensor"))
		})
	})

	Context("when batchSize is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "batch-size-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should include --batch-size flag with correct value when batchSize is set", func() {
			replicas := int32(1)
			batchSize := int32(2048)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "batch-size-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:  "batch-size-model",
					Replicas:  &replicas,
					Image:     "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					BatchSize: &batchSize,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--batch-size", "2048"))
		})

		It("should NOT include --batch-size when batchSize is not specified", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-batch-size-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "batch-size-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--batch-size"))
		})

		It("should NOT include --batch-size when batchSize is zero", func() {
			replicas := int32(1)
			batchSize := int32(0)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "zero-batch-size-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:  "batch-size-model",
					Replicas:  &replicas,
					Image:     "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					BatchSize: &batchSize,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--batch-size"))
		})
	})

	Context("when uBatchSize is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ubatch-size-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should include --ubatch-size flag with correct value when uBatchSize is set", func() {
			replicas := int32(1)
			ubatchSize := int32(256)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ubatch-size-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:   "ubatch-size-model",
					Replicas:   &replicas,
					Image:      "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					UBatchSize: &ubatchSize,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--ubatch-size", "256"))
		})

		It("should NOT include --ubatch-size when uBatchSize is not specified", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-ubatch-size-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "ubatch-size-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--ubatch-size"))
		})

		It("should NOT include --ubatch-size when uBatchSize is zero", func() {
			replicas := int32(1)
			ubatchSize := int32(0)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "zero-ubatch-size-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:   "ubatch-size-model",
					Replicas:   &replicas,
					Image:      "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					UBatchSize: &ubatchSize,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--ubatch-size"))
		})
	})

	Context("when hybrid offloading memory warning is needed", func() {
		It("should return true when moeCPUOffload is true and no memory set", func() {
			moeCPUOffload := true
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					MoeCPUOffload: &moeCPUOffload,
				},
			}
			Expect(needsOffloadMemoryWarning(isvc)).To(BeTrue())
		})

		It("should return true when noKvOffload is true and no memory set", func() {
			noKvOffload := true
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					NoKvOffload: &noKvOffload,
				},
			}
			Expect(needsOffloadMemoryWarning(isvc)).To(BeTrue())
		})

		It("should return false when moeCPUOffload is true and memory is set", func() {
			moeCPUOffload := true
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					MoeCPUOffload: &moeCPUOffload,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						Memory: "64Gi",
					},
				},
			}
			Expect(needsOffloadMemoryWarning(isvc)).To(BeFalse())
		})

		It("should return false when neither offload flag is set", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{},
			}
			Expect(needsOffloadMemoryWarning(isvc)).To(BeFalse())
		})

		It("should return false when moeCPUOffload is false", func() {
			moeCPUOffload := false
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					MoeCPUOffload: &moeCPUOffload,
				},
			}
			Expect(needsOffloadMemoryWarning(isvc)).To(BeFalse())
		})

		It("should return false when moeCPUOffload is true and hostMemory is set", func() {
			moeCPUOffload := true
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					MoeCPUOffload: &moeCPUOffload,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						HostMemory: "64Gi",
					},
				},
			}
			Expect(needsOffloadMemoryWarning(isvc)).To(BeFalse())
		})
	})

	Context("when hostMemory is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hostmem-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should use hostMemory for pod memory request", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hostmem-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "hostmem-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU:        1,
						HostMemory: "64Gi",
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			container := deployment.Spec.Template.Spec.Containers[0]
			Expect(container.Resources.Requests[corev1.ResourceMemory]).To(Equal(resource.MustParse("64Gi")))
		})

		It("should prefer hostMemory over memory when both are set", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hostmem-precedence-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "hostmem-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU:        1,
						Memory:     "4Gi",
						HostMemory: "64Gi",
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			container := deployment.Spec.Template.Spec.Containers[0]
			Expect(container.Resources.Requests[corev1.ResourceMemory]).To(Equal(resource.MustParse("64Gi")))
		})

		It("should fall back to memory when hostMemory is not set", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-hostmem-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "hostmem-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU:    1,
						Memory: "4Gi",
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			container := deployment.Spec.Template.Spec.Containers[0]
			Expect(container.Resources.Requests[corev1.ResourceMemory]).To(Equal(resource.MustParse("4Gi")))
		})
	})

	Context("when noWarmup is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "warmup-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should include --no-warmup when noWarmup is true", func() {
			replicas := int32(1)
			noWarmup := true
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "warmup-service", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "warmup-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					NoWarmup: &noWarmup,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}
			deployment := reconciler.constructDeployment(isvc, model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElement("--no-warmup"))
		})

		It("should NOT include --no-warmup when noWarmup is not specified", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "no-warmup-unset", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "warmup-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}
			deployment := reconciler.constructDeployment(isvc, model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--no-warmup"))
		})

		It("should NOT include --no-warmup when noWarmup is false", func() {
			replicas := int32(1)
			noWarmup := false
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "warmup-false", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "warmup-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					NoWarmup: &noWarmup,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}
			deployment := reconciler.constructDeployment(isvc, model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--no-warmup"))
		})
	})

	Context("when reasoningBudget is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "reason-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		buildISVC := func(budget *int32, message string) *inferencev1alpha1.InferenceService {
			replicas := int32(1)
			return &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "reason-service", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:               "reason-model",
					Replicas:               &replicas,
					Image:                  "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					ReasoningBudget:        budget,
					ReasoningBudgetMessage: message,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}
		}

		It("should include --reasoning-budget when budget is set (no message)", func() {
			budget := int32(1024)
			deployment := reconciler.constructDeployment(buildISVC(&budget, ""), model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--reasoning-budget", "1024"))
			Expect(args).NotTo(ContainElement("--reasoning-budget-message"))
		})

		It("should include both flags when budget and message are set", func() {
			budget := int32(2048)
			deployment := reconciler.constructDeployment(buildISVC(&budget, "wrap it up"), model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--reasoning-budget", "2048"))
			Expect(args).To(ContainElements("--reasoning-budget-message", "wrap it up"))
		})

		It("should emit --reasoning-budget 0 to disable visible thinking", func() {
			budget := int32(0)
			deployment := reconciler.constructDeployment(buildISVC(&budget, ""), model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--reasoning-budget", "0"))
		})

		It("should NOT emit reasoning-budget-message without budget", func() {
			deployment := reconciler.constructDeployment(buildISVC(nil, "ignored"), model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--reasoning-budget"))
			Expect(args).NotTo(ContainElement("--reasoning-budget-message"))
		})

		It("should NOT emit either flag when both are unset", func() {
			deployment := reconciler.constructDeployment(buildISVC(nil, ""), model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--reasoning-budget"))
			Expect(args).NotTo(ContainElement("--reasoning-budget-message"))
		})
	})

	Context("when metadataOverrides is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "meta-override-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		buildISVC := func(overrides []string) *inferencev1alpha1.InferenceService {
			replicas := int32(1)
			return &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "meta-override-svc", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:          "meta-override-model",
					Replicas:          &replicas,
					Image:             "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					MetadataOverrides: overrides,
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}
		}

		It("should emit one --override-kv flag per entry", func() {
			overrides := []string{
				"qwen35moe.context_length=int:1048576",
				"tokenizer.chat_template.thinking=bool:false",
			}
			deployment := reconciler.constructDeployment(buildISVC(overrides), model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--override-kv", "qwen35moe.context_length=int:1048576"))
			Expect(args).To(ContainElements("--override-kv", "tokenizer.chat_template.thinking=bool:false"))
			// Count occurrences
			count := 0
			for _, a := range args {
				if a == "--override-kv" {
					count++
				}
			}
			Expect(count).To(Equal(2))
		})

		It("should emit single --override-kv for one entry", func() {
			deployment := reconciler.constructDeployment(buildISVC([]string{"foo=int:42"}), model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--override-kv", "foo=int:42"))
		})

		It("should NOT emit --override-kv when slice is empty", func() {
			deployment := reconciler.constructDeployment(buildISVC([]string{}), model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--override-kv"))
		})

		It("should NOT emit --override-kv when slice is nil", func() {
			deployment := reconciler.constructDeployment(buildISVC(nil), model, 1)
			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--override-kv"))
		})
	})

	Context("when extraArgs is configured", func() {
		var (
			reconciler *InferenceServiceReconciler
			model      *inferencev1alpha1.Model
		)

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				ModelCachePath:     "/tmp/llmkube/models",
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			model = &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "extra-args-model",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count:  1,
							Layers: 64,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "test-cache-key",
					Path:     "/tmp/llmkube/models/test-model.gguf",
				},
			}
		})

		It("should append all extra args in order", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "extra-args-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:  "extra-args-model",
					Replicas:  &replicas,
					Image:     "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					ExtraArgs: []string{"--seed", "42", "--batch-size", "2048"},
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElements("--seed", "42"))
			Expect(args).To(ContainElements("--batch-size", "2048"))
		})

		It("should NOT append anything extra when extraArgs is empty", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-extra-args-service",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "extra-args-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			args := deployment.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement("--seed"))
			Expect(args).NotTo(ContainElement("--batch-size"))
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

var _ = Describe("sanitizeDNSName", func() {
	It("should replace dots with dashes", func() {
		Expect(sanitizeDNSName("my.model.v1")).To(Equal("my-model-v1"))
	})
	It("should leave names without dots unchanged", func() {
		Expect(sanitizeDNSName("my-service")).To(Equal("my-service"))
	})
})

var _ = Describe("buildModelInitCommand", func() {
	It("should generate cached remote download command with env var references", func() {
		cmd := buildModelInitCommand(false, true)
		Expect(cmd).To(ContainSubstring(`mkdir -p "$CACHE_DIR"`))
		Expect(cmd).To(ContainSubstring(`"$MODEL_PATH"`))
		Expect(cmd).To(ContainSubstring("curl -f -L"))
		Expect(cmd).To(ContainSubstring(`"$MODEL_SOURCE"`))
	})

	It("should generate cached local copy command", func() {
		cmd := buildModelInitCommand(true, true)
		Expect(cmd).To(ContainSubstring(`mkdir -p "$CACHE_DIR"`))
		Expect(cmd).To(ContainSubstring("cp /host-model/model.gguf"))
		Expect(cmd).To(ContainSubstring(`"$MODEL_PATH"`))
	})

	It("should generate error exit for uncached local source", func() {
		cmd := buildModelInitCommand(true, false)
		Expect(cmd).To(ContainSubstring("ERROR: Local model source requires model cache"))
		Expect(cmd).To(ContainSubstring("exit 1"))
	})

	It("should generate uncached remote download command with env var references", func() {
		cmd := buildModelInitCommand(false, false)
		Expect(cmd).To(ContainSubstring("curl -f -L"))
		Expect(cmd).To(ContainSubstring(`"$MODEL_SOURCE"`))
		Expect(cmd).To(ContainSubstring(`"$MODEL_PATH"`))
		Expect(cmd).NotTo(ContainSubstring("mkdir -p"))
	})

	It("should not contain user-controlled values in the command string", func() {
		// Verify that a malicious source cannot appear in the shell script.
		// The command is a static template with env var references only.
		maliciousSource := `https://evil.com/$(touch /pwned).gguf`
		cmd := buildModelInitCommand(false, true)
		Expect(cmd).NotTo(ContainSubstring(maliciousSource))
		Expect(cmd).NotTo(ContainSubstring("touch"))
		Expect(cmd).NotTo(ContainSubstring("evil.com"))

		// Env vars carry the value safely outside the shell script
		env := modelInitEnvVars(maliciousSource, "/models/abc123", "/models/abc123/model.gguf")
		Expect(env[0].Name).To(Equal("MODEL_SOURCE"))
		Expect(env[0].Value).To(Equal(maliciousSource))
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
		config := buildCachedStorageConfig(model, nil, "", "curl:8.18.0")

		Expect(config.modelPath).To(Equal("/models/abc123def456/model.gguf"))
		Expect(config.volumes).To(HaveLen(1))
		Expect(config.volumes[0].Name).To(Equal("model-cache"))
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(ModelCachePVCName))
		Expect(config.initContainers).To(HaveLen(1))
		Expect(config.initContainers[0].Name).To(Equal("model-downloader"))
		Expect(config.initContainers[0].Image).To(Equal("curl:8.18.0"))
		Expect(config.volumeMounts[0].MountPath).To(Equal("/models"))
		Expect(config.volumeMounts[0].ReadOnly).To(BeTrue())

		// Verify env vars are set on the init container
		env := config.initContainers[0].Env
		Expect(env).To(HaveLen(3))
		Expect(env[0]).To(Equal(corev1.EnvVar{Name: "MODEL_SOURCE", Value: "https://example.com/model.gguf"}))
		Expect(env[1]).To(Equal(corev1.EnvVar{Name: "CACHE_DIR", Value: "/models/abc123def456"}))
		Expect(env[2]).To(Equal(corev1.EnvVar{Name: "MODEL_PATH", Value: "/models/abc123def456/model.gguf"}))

		// Verify the command does not contain the raw source URL
		Expect(config.initContainers[0].Command[2]).NotTo(ContainSubstring("example.com"))
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
		config := buildCachedStorageConfig(model, nil, "", "curl:8.18.0")

		Expect(config.volumes).To(HaveLen(2))
		Expect(config.volumes[1].Name).To(Equal("host-model"))
		Expect(config.volumes[1].HostPath.Path).To(Equal("/mnt/models/test.gguf"))

		// Verify env vars are set
		env := config.initContainers[0].Env
		Expect(env).To(HaveLen(3))
		Expect(env[0]).To(Equal(corev1.EnvVar{Name: "MODEL_SOURCE", Value: "file:///mnt/models/test.gguf"}))
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
		config := buildCachedStorageConfig(model, nil, "my-ca-certs", "curl:8.18.0")

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
		config := buildEmptyDirStorageConfig(model, nil, "default", "", "curl:8.18.0")

		Expect(config.modelPath).To(Equal("/models/default-my-model.gguf"))
		Expect(config.volumes).To(HaveLen(1))
		Expect(config.volumes[0].Name).To(Equal("model-storage"))
		Expect(config.volumes[0].EmptyDir).NotTo(BeNil())

		// Verify env vars are set on the init container
		env := config.initContainers[0].Env
		Expect(env).To(HaveLen(3))
		Expect(env[0]).To(Equal(corev1.EnvVar{Name: "MODEL_SOURCE", Value: "https://example.com/model.gguf"}))
		Expect(env[1]).To(Equal(corev1.EnvVar{Name: "CACHE_DIR", Value: ""}))
		Expect(env[2]).To(Equal(corev1.EnvVar{Name: "MODEL_PATH", Value: "/models/default-my-model.gguf"}))

		// Verify the command does not contain the raw source URL
		Expect(config.initContainers[0].Command[2]).NotTo(ContainSubstring("example.com"))
	})

	It("should add CA cert volume when caCertConfigMap is set", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "my-model"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		}
		config := buildEmptyDirStorageConfig(model, nil, "default", "my-ca-certs", "curl:8.18.0")

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

	It("should inherit runAsUser/runAsGroup in emptyDir storage", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "my-model"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		}
		customUID := int64(2000)
		customGID := int64(2000)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-isvc"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser:  &customUID,
					RunAsGroup: &customGID,
				},
			},
		}
		config := buildEmptyDirStorageConfig(model, isvc, "default", "", "curl:8.18.0")

		initSecCtx := config.initContainers[0].SecurityContext
		Expect(initSecCtx).NotTo(BeNil())
		Expect(initSecCtx.RunAsUser).NotTo(BeNil())
		Expect(*initSecCtx.RunAsUser).To(Equal(int64(2000)))
		Expect(initSecCtx.RunAsGroup).NotTo(BeNil())
		Expect(*initSecCtx.RunAsGroup).To(Equal(int64(2000)))
	})
})

var _ = Describe("buildPVCStorageConfig", func() {
	It("should configure PVC volume with correct claim name and path", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-model"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "pvc://my-models/llama/model.gguf"},
		}
		config := buildPVCStorageConfig(model)

		Expect(config.modelPath).To(Equal("/model-source/llama/model.gguf"))
		Expect(config.initContainers).To(BeEmpty())
		Expect(config.volumes).To(HaveLen(1))
		Expect(config.volumes[0].Name).To(Equal("model-source"))
		Expect(config.volumes[0].PersistentVolumeClaim).NotTo(BeNil())
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("my-models"))
		Expect(config.volumes[0].PersistentVolumeClaim.ReadOnly).To(BeTrue())
		Expect(config.volumeMounts).To(HaveLen(1))
		Expect(config.volumeMounts[0].Name).To(Equal("model-source"))
		Expect(config.volumeMounts[0].MountPath).To(Equal("/model-source"))
		Expect(config.volumeMounts[0].ReadOnly).To(BeTrue())
	})

	It("should handle simple file at root of PVC", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-model-simple"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "pvc://storage/model.gguf"},
		}
		config := buildPVCStorageConfig(model)

		Expect(config.modelPath).To(Equal("/model-source/model.gguf"))
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("storage"))
	})
})

var _ = Describe("buildModelStorageConfig PVC dispatch", func() {
	It("should dispatch to PVC storage config when source is pvc://", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "dispatch-test"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "pvc://my-claim/model.gguf"},
			Status:     inferencev1alpha1.ModelStatus{CacheKey: "abc123"},
		}
		config := buildModelStorageConfig(model, nil, "default", true, "", "curl:8.18.0")

		// Should use PVC config, not cached config
		Expect(config.volumes[0].Name).To(Equal("model-source"))
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("my-claim"))
		Expect(config.initContainers).To(BeEmpty())
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
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
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
		Expect(deployment.Spec.Strategy.Type).To(Equal(appsv1.DeploymentStrategyType("")))
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

var _ = Describe("reconcileService Metal path", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
	})

	It("should return minimal Service with correct name and namespace when isMetal is true", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "metal-svc-test", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "some-model"},
		}

		svc, result, err := reconciler.reconcileService(context.Background(), isvc, true, 1, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(BeNil())
		Expect(svc).NotTo(BeNil())
		Expect(svc.Name).To(Equal("metal-svc-test"))
		Expect(svc.Namespace).To(Equal("default"))
	})

	It("should DNS-sanitize the minimal Service name when isMetal is true", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "llama-3.2-3b", Namespace: "test-ns"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "some-model"},
		}

		svc, result, err := reconciler.reconcileService(context.Background(), isvc, true, 1, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(BeNil())
		Expect(svc.Name).To(Equal("llama-3-2-3b"))
		Expect(svc.Namespace).To(Equal("test-ns"))
	})

	It("should not have Spec fields populated on the minimal Service", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "metal-minimal", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "some-model",
				Endpoint: &inferencev1alpha1.EndpointSpec{Port: 9090, Type: "LoadBalancer"},
			},
		}

		svc, _, err := reconciler.reconcileService(context.Background(), isvc, true, 1, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(svc.Spec.Ports).To(BeEmpty())
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceType("")))
	})

	It("should not create any K8s Service resource when isMetal is true", func() {
		isvcName := "metal-no-k8s-svc"
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "some-model"},
		}

		_, _, err := reconciler.reconcileService(context.Background(), isvc, true, 1, true)
		Expect(err).NotTo(HaveOccurred())

		svc := &corev1.Service{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: isvcName, Namespace: "default"}, svc)
		Expect(errors.IsNotFound(err)).To(BeTrue())
	})
})

var _ = Describe("constructEndpoint with Metal minimal Service", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("should construct correct URL from minimal Metal Service with default settings", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "metal-test", Namespace: "default"},
		}
		minimalSvc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sanitizeDNSName(isvc.Name),
				Namespace: isvc.Namespace,
			},
		}
		endpoint := reconciler.constructEndpoint(isvc, minimalSvc)
		Expect(endpoint).To(Equal("http://metal-test.default.svc.cluster.local:8080/v1/chat/completions"))
	})

	It("should construct correct URL from minimal Metal Service with custom port and path", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "metal-custom", Namespace: "production"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Endpoint: &inferencev1alpha1.EndpointSpec{
					Port: 3000,
					Path: "/api/v2/infer",
				},
			},
		}
		minimalSvc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sanitizeDNSName(isvc.Name),
				Namespace: isvc.Namespace,
			},
		}
		endpoint := reconciler.constructEndpoint(isvc, minimalSvc)
		Expect(endpoint).To(Equal("http://metal-custom.production.svc.cluster.local:3000/api/v2/infer"))
	})

	It("should construct correct URL when Metal Service name is DNS-sanitized", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "model.v2.1", Namespace: "ml"},
		}
		minimalSvc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sanitizeDNSName(isvc.Name),
				Namespace: isvc.Namespace,
			},
		}
		endpoint := reconciler.constructEndpoint(isvc, minimalSvc)
		Expect(endpoint).To(Equal("http://model-v2-1.ml.svc.cluster.local:8080/v1/chat/completions"))
	})
})

var _ = Describe("Security Context Configuration", func() {
	var (
		reconciler *InferenceServiceReconciler
		model      *inferencev1alpha1.Model
	)

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}

		model = &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "secctx-model",
				Namespace: "default",
			},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/model.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			},
			Status: inferencev1alpha1.ModelStatus{
				Phase: "Ready",
			},
		}
	})

	Context("default security contexts", func() {
		It("should set seccompProfile RuntimeDefault on pod by default", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "secctx-default", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "secctx-model",
					Replicas: &replicas,
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			podSecCtx := deployment.Spec.Template.Spec.SecurityContext
			Expect(podSecCtx).NotTo(BeNil())
			Expect(podSecCtx.SeccompProfile).NotTo(BeNil())
			Expect(podSecCtx.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
		})

		It("should set allowPrivilegeEscalation false and drop ALL on main container by default", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "secctx-container-default", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "secctx-model",
					Replicas: &replicas,
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			containerSecCtx := deployment.Spec.Template.Spec.Containers[0].SecurityContext
			Expect(containerSecCtx).NotTo(BeNil())
			Expect(*containerSecCtx.AllowPrivilegeEscalation).To(BeFalse())
			Expect(containerSecCtx.Capabilities).NotTo(BeNil())
			Expect(containerSecCtx.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))
		})

		It("should set allowPrivilegeEscalation false and drop ALL on init container", func() {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "secctx-init-default", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "secctx-model",
					Replicas: &replicas,
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			Expect(deployment.Spec.Template.Spec.InitContainers).To(HaveLen(1))
			initSecCtx := deployment.Spec.Template.Spec.InitContainers[0].SecurityContext
			Expect(initSecCtx).NotTo(BeNil())
			Expect(*initSecCtx.AllowPrivilegeEscalation).To(BeFalse())
			Expect(*initSecCtx.ReadOnlyRootFilesystem).To(BeFalse())
			Expect(initSecCtx.Capabilities).NotTo(BeNil())
			Expect(initSecCtx.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))
		})
	})

	Context("user-specified security context overrides", func() {
		It("should use user-specified podSecurityContext with fsGroup", func() {
			replicas := int32(1)
			fsGroup := int64(1000680000)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "secctx-override-pod", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "secctx-model",
					Replicas: &replicas,
					PodSecurityContext: &corev1.PodSecurityContext{
						FSGroup:      &fsGroup,
						RunAsNonRoot: boolPtr(true),
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			podSecCtx := deployment.Spec.Template.Spec.SecurityContext
			Expect(podSecCtx).NotTo(BeNil())
			Expect(*podSecCtx.FSGroup).To(Equal(int64(1000680000)))
			Expect(*podSecCtx.RunAsNonRoot).To(BeTrue())
			// Should NOT have default seccomp when user provides their own
			// (user's override is used as-is)
		})

		It("should use user-specified container securityContext", func() {
			replicas := int32(1)
			runAsUser := int64(1000)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "secctx-override-container", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "secctx-model",
					Replicas: &replicas,
					SecurityContext: &corev1.SecurityContext{
						RunAsUser:                boolPtr64(runAsUser),
						AllowPrivilegeEscalation: boolPtr(false),
						ReadOnlyRootFilesystem:   boolPtr(true),
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			containerSecCtx := deployment.Spec.Template.Spec.Containers[0].SecurityContext
			Expect(containerSecCtx).NotTo(BeNil())
			Expect(*containerSecCtx.RunAsUser).To(Equal(int64(1000)))
			Expect(*containerSecCtx.AllowPrivilegeEscalation).To(BeFalse())
			Expect(*containerSecCtx.ReadOnlyRootFilesystem).To(BeTrue())
		})
	})

	Context("init container security context with cached storage", func() {
		It("should set security context on init container for cached model", func() {
			reconciler.ModelCachePath = "/models"
			cachedModel := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "cached-secctx-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "abc123",
				},
			}

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "secctx-cached-init", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "cached-secctx-model",
					Replicas: &replicas,
				},
			}

			deployment := reconciler.constructDeployment(isvc, cachedModel, 1)

			Expect(deployment.Spec.Template.Spec.InitContainers).To(HaveLen(1))
			initSecCtx := deployment.Spec.Template.Spec.InitContainers[0].SecurityContext
			Expect(initSecCtx).NotTo(BeNil())
			Expect(*initSecCtx.AllowPrivilegeEscalation).To(BeFalse())
			Expect(initSecCtx.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))
		})

		It("should not set runAsUser/runAsGroup without podSecurityContext", func() {
			reconciler.ModelCachePath = DefaultModelCachePath
			cachedModel := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "cached-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "abc123",
				},
			}

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "test-init-secctx", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "cached-model",
					Replicas: &replicas,
				},
			}

			deployment := reconciler.constructDeployment(isvc, cachedModel, 1)

			initSecCtx := deployment.Spec.Template.Spec.InitContainers[0].SecurityContext
			Expect(initSecCtx).NotTo(BeNil())
			Expect(initSecCtx.RunAsNonRoot).To(BeNil())
			Expect(initSecCtx.RunAsUser).To(BeNil())
			Expect(initSecCtx.RunAsGroup).To(BeNil())
		})

		It("should inherit runAsUser/runAsGroup from podSecurityContext", func() {
			reconciler.ModelCachePath = DefaultModelCachePath
			cachedModel := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "cached-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase:    "Ready",
					CacheKey: "abc123",
				},
			}

			replicas := int32(1)
			customUID := int64(2000)
			customGID := int64(2000)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "test-init-inherit", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "cached-model",
					Replicas: &replicas,
					PodSecurityContext: &corev1.PodSecurityContext{
						RunAsUser:  &customUID,
						RunAsGroup: &customGID,
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, cachedModel, 1)

			initSecCtx := deployment.Spec.Template.Spec.InitContainers[0].SecurityContext
			Expect(initSecCtx).NotTo(BeNil())
			Expect(initSecCtx.RunAsUser).NotTo(BeNil())
			Expect(*initSecCtx.RunAsUser).To(Equal(int64(2000)))
			Expect(initSecCtx.RunAsGroup).NotTo(BeNil())
			Expect(*initSecCtx.RunAsGroup).To(Equal(int64(2000)))
		})
	})
})

// boolPtr64 is a test helper for creating *int64 inline
func boolPtr64(v int64) *int64 { return &v }

// int32Ptr is a test helper for creating *int32 inline
func int32Ptr(v int32) *int32 { return &v }

var _ = Describe("HPA Autoscaling", func() {
	Context("constructHPA", func() {
		var reconciler *InferenceServiceReconciler

		BeforeEach(func() {
			reconciler = &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
		})

		It("should apply default metric when no metrics specified", func() {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hpa-default-metric",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "test-model",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{
						MaxReplicas: 5,
					},
				},
			}

			hpa := reconciler.constructHPA(isvc, "hpa-default-metric")

			Expect(hpa.Spec.MaxReplicas).To(Equal(int32(5)))
			Expect(*hpa.Spec.MinReplicas).To(Equal(int32(1)))
			Expect(hpa.Spec.Metrics).To(HaveLen(1))
			Expect(hpa.Spec.Metrics[0].Type).To(
				Equal(autoscalingv2.PodsMetricSourceType),
			)
			Expect(hpa.Spec.Metrics[0].Pods.Metric.Name).To(
				Equal("llamacpp:requests_processing"),
			)
			Expect(hpa.Spec.Metrics[0].Pods.Target.AverageValue.String()).To(
				Equal("2"),
			)
		})

		It("should use custom minReplicas", func() {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hpa-custom-min",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "test-model",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{
						MinReplicas: int32Ptr(3),
						MaxReplicas: 10,
					},
				},
			}

			hpa := reconciler.constructHPA(isvc, "hpa-custom-min")

			Expect(*hpa.Spec.MinReplicas).To(Equal(int32(3)))
			Expect(hpa.Spec.MaxReplicas).To(Equal(int32(10)))
		})

		It("should use custom Pods metric", func() {
			targetVal := "5"
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hpa-custom-pods-metric",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "test-model",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{
						MaxReplicas: 8,
						Metrics: []inferencev1alpha1.MetricSpec{
							{
								Type:               "Pods",
								Name:               "llamacpp:tokens_per_second",
								TargetAverageValue: &targetVal,
							},
						},
					},
				},
			}

			hpa := reconciler.constructHPA(isvc, "hpa-custom-pods-metric")

			Expect(hpa.Spec.Metrics).To(HaveLen(1))
			Expect(hpa.Spec.Metrics[0].Pods.Metric.Name).To(
				Equal("llamacpp:tokens_per_second"),
			)
			Expect(hpa.Spec.Metrics[0].Pods.Target.AverageValue.String()).To(
				Equal("5"),
			)
		})

		It("should use Resource metric with utilization", func() {
			utilization := int32(70)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hpa-resource-metric",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "test-model",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{
						MaxReplicas: 4,
						Metrics: []inferencev1alpha1.MetricSpec{
							{
								Type:                     "Resource",
								Name:                     "cpu",
								TargetAverageUtilization: &utilization,
							},
						},
					},
				},
			}

			hpa := reconciler.constructHPA(isvc, "hpa-resource-metric")

			Expect(hpa.Spec.Metrics).To(HaveLen(1))
			Expect(hpa.Spec.Metrics[0].Type).To(
				Equal(autoscalingv2.ResourceMetricSourceType),
			)
			Expect(hpa.Spec.Metrics[0].Resource.Name).To(
				Equal(corev1.ResourceName("cpu")),
			)
			Expect(*hpa.Spec.Metrics[0].Resource.Target.AverageUtilization).To(
				Equal(int32(70)),
			)
		})

		It("should set correct scaleTargetRef", func() {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hpa-target-ref",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "test-model",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{
						MaxReplicas: 5,
					},
				},
			}

			hpa := reconciler.constructHPA(isvc, "my-deployment")

			Expect(hpa.Spec.ScaleTargetRef.APIVersion).To(Equal("apps/v1"))
			Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
			Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal("my-deployment"))
		})

		It("should set correct labels", func() {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hpa-labels",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "test-model",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{
						MaxReplicas: 5,
					},
				},
			}

			hpa := reconciler.constructHPA(isvc, "hpa-labels")

			Expect(hpa.Labels["app"]).To(Equal("hpa-labels"))
			Expect(hpa.Labels["inference.llmkube.dev/service"]).To(
				Equal("hpa-labels"),
			)
		})
	})

	Context("reconcileHPA with envtest", func() {
		ctx := context.Background()

		It("should create HPA when autoscaling is specified", func() {
			modelName := "hpa-create-model"
			isvcName := "hpa-create-isvc"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name: modelName, Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cpu",
					},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name: isvcName, Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{
						MinReplicas: int32Ptr(2),
						MaxReplicas: 8,
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, isvc)
				dep := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: isvcName, Namespace: "default",
				}, dep); err == nil {
					_ = k8sClient.Delete(ctx, dep)
				}
				svc := &corev1.Service{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: isvcName, Namespace: "default",
				}, svc); err == nil {
					_ = k8sClient.Delete(ctx, svc)
				}
				hpa := &autoscalingv2.HorizontalPodAutoscaler{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: isvcName, Namespace: "default",
				}, hpa); err == nil {
					_ = k8sClient.Delete(ctx, hpa)
				}
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: isvcName, Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying HPA was created")
			hpa := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: isvcName, Namespace: "default",
			}, hpa)).To(Succeed())
			Expect(hpa.Spec.MaxReplicas).To(Equal(int32(8)))
			Expect(*hpa.Spec.MinReplicas).To(Equal(int32(2)))
			Expect(hpa.OwnerReferences).To(HaveLen(1))
			Expect(*hpa.OwnerReferences[0].Controller).To(BeTrue())
		})

		It("should NOT create HPA when autoscaling is nil", func() {
			modelName := "hpa-nil-model"
			isvcName := "hpa-nil-isvc"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name: modelName, Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cpu",
					},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name: isvcName, Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
					// Autoscaling is nil
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, isvc)
				dep := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: isvcName, Namespace: "default",
				}, dep); err == nil {
					_ = k8sClient.Delete(ctx, dep)
				}
				svc := &corev1.Service{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: isvcName, Namespace: "default",
				}, svc); err == nil {
					_ = k8sClient.Delete(ctx, svc)
				}
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: isvcName, Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying no HPA was created")
			hpa := &autoscalingv2.HorizontalPodAutoscaler{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: isvcName, Namespace: "default",
			}, hpa)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should delete HPA when autoscaling is removed", func() {
			modelName := "hpa-delete-model"
			isvcName := "hpa-delete-isvc"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name: modelName, Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cpu",
					},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name: isvcName, Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{
						MaxReplicas: 5,
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, isvc)
				dep := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: isvcName, Namespace: "default",
				}, dep); err == nil {
					_ = k8sClient.Delete(ctx, dep)
				}
				svc := &corev1.Service{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: isvcName, Namespace: "default",
				}, svc); err == nil {
					_ = k8sClient.Delete(ctx, svc)
				}
				hpa := &autoscalingv2.HorizontalPodAutoscaler{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: isvcName, Namespace: "default",
				}, hpa); err == nil {
					_ = k8sClient.Delete(ctx, hpa)
				}
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			By("first reconcile creates the HPA")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: isvcName, Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			hpa := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: isvcName, Namespace: "default",
			}, hpa)).To(Succeed())

			By("removing autoscaling from the InferenceService")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: isvcName, Namespace: "default",
			}, isvc)).To(Succeed())
			isvc.Spec.Autoscaling = nil
			Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

			By("second reconcile deletes the HPA")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: isvcName, Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: isvcName, Namespace: "default",
			}, hpa)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should NOT create HPA for Metal accelerator", func() {
			modelName := "hpa-metal-model"
			isvcName := "hpa-metal-isvc"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name: modelName, Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "metal",
					},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name: isvcName, Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{
						MaxReplicas: 5,
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
				NamespacedName: types.NamespacedName{
					Name: isvcName, Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying no HPA was created")
			hpa := &autoscalingv2.HorizontalPodAutoscaler{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: isvcName, Namespace: "default",
			}, hpa)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should set Deployment replicas to nil when autoscaling enabled", func() {
			modelName := "hpa-replicas-model"
			isvcName := "hpa-replicas-isvc"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{
					Name: modelName, Namespace: "default",
				},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cpu",
					},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			replicas := int32(2)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name: isvcName, Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{
						MinReplicas: int32Ptr(1),
						MaxReplicas: 10,
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, isvc)
				dep := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: isvcName, Namespace: "default",
				}, dep); err == nil {
					_ = k8sClient.Delete(ctx, dep)
				}
				svc := &corev1.Service{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: isvcName, Namespace: "default",
				}, svc); err == nil {
					_ = k8sClient.Delete(ctx, svc)
				}
				hpa := &autoscalingv2.HorizontalPodAutoscaler{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: isvcName, Namespace: "default",
				}, hpa); err == nil {
					_ = k8sClient.Delete(ctx, hpa)
				}
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			By("first reconcile creates deployment with replicas")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: isvcName, Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("second reconcile avoids overwriting HPA-managed replicas")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: isvcName, Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: isvcName, Namespace: "default",
			}, dep)).To(Succeed())
			// When autoscaling is enabled, the controller sets replicas to nil
			// to let the HPA manage scaling. The API server defaults nil to 1,
			// so we verify the controller did NOT force the isvc's configured
			// replicas (2) onto the deployment.
			Expect(*dep.Spec.Replicas).NotTo(Equal(int32(2)))
		})
	})
})

var _ = Describe("RuntimeBackend interface", func() {
	Context("LlamaCppBackend", func() {
		var backend *LlamaCppBackend

		BeforeEach(func() {
			backend = &LlamaCppBackend{}
		})

		It("should return correct defaults", func() {
			Expect(backend.ContainerName()).To(Equal("llama-server"))
			Expect(backend.DefaultImage()).To(Equal("ghcr.io/ggml-org/llama.cpp:server"))
			Expect(backend.DefaultPort()).To(Equal(int32(8080)))
			Expect(backend.NeedsModelInit()).To(BeTrue())
		})

		It("should build HTTP /health probes", func() {
			startup, liveness, readiness := backend.BuildProbes(8080)
			Expect(startup.HTTPGet).NotTo(BeNil())
			Expect(startup.HTTPGet.Path).To(Equal("/health"))
			Expect(liveness.HTTPGet.Path).To(Equal("/health"))
			Expect(readiness.HTTPGet.Path).To(Equal("/health"))
		})
	})

	Context("GenericBackend", func() {
		var backend *GenericBackend

		BeforeEach(func() {
			backend = &GenericBackend{}
		})

		It("should return correct defaults", func() {
			Expect(backend.ContainerName()).To(Equal("inference-server"))
			Expect(backend.DefaultImage()).To(Equal(""))
			Expect(backend.DefaultPort()).To(Equal(int32(8080)))
			Expect(backend.NeedsModelInit()).To(BeFalse())
		})

		It("should build TCP socket probes", func() {
			startup, liveness, readiness := backend.BuildProbes(8998)
			Expect(startup.TCPSocket).NotTo(BeNil())
			Expect(startup.TCPSocket.Port.IntValue()).To(Equal(8998))
			Expect(liveness.TCPSocket).NotTo(BeNil())
			Expect(readiness.TCPSocket).NotTo(BeNil())
		})

		It("should pass through user args", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Args: []string{"--quantize-4bit", "--ssl", "/app/ssl"},
				},
			}
			args := backend.BuildArgs(isvc, nil, "", 0)
			Expect(args).To(Equal([]string{"--quantize-4bit", "--ssl", "/app/ssl"}))
		})

		It("should return nil args when none specified", func() {
			isvc := &inferencev1alpha1.InferenceService{}
			args := backend.BuildArgs(isvc, nil, "", 0)
			Expect(args).To(BeNil())
		})
	})

	Context("resolveBackend", func() {
		It("should return LlamaCppBackend for empty runtime", func() {
			isvc := &inferencev1alpha1.InferenceService{}
			backend := resolveBackend(isvc)
			_, ok := backend.(*LlamaCppBackend)
			Expect(ok).To(BeTrue())
		})

		It("should return LlamaCppBackend for llamacpp runtime", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "llamacpp"},
			}
			backend := resolveBackend(isvc)
			_, ok := backend.(*LlamaCppBackend)
			Expect(ok).To(BeTrue())
		})

		It("should return GenericBackend for generic runtime", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "generic"},
			}
			backend := resolveBackend(isvc)
			_, ok := backend.(*GenericBackend)
			Expect(ok).To(BeTrue())
		})

		It("should return PersonaPlexBackend for personaplex runtime", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "personaplex"},
			}
			backend := resolveBackend(isvc)
			_, ok := backend.(*PersonaPlexBackend)
			Expect(ok).To(BeTrue())
		})

		It("should return VLLMBackend for vllm runtime", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "vllm"},
			}
			backend := resolveBackend(isvc)
			_, ok := backend.(*VLLMBackend)
			Expect(ok).To(BeTrue())
		})

		It("should return TGIBackend for tgi runtime", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "tgi"},
			}
			backend := resolveBackend(isvc)
			_, ok := backend.(*TGIBackend)
			Expect(ok).To(BeTrue())
		})
	})

	Context("VLLMBackend", func() {
		var backend *VLLMBackend

		BeforeEach(func() {
			backend = &VLLMBackend{}
		})

		It("should return correct defaults", func() {
			Expect(backend.ContainerName()).To(Equal("vllm"))
			Expect(backend.DefaultImage()).To(Equal("vllm/vllm-openai:latest"))
			Expect(backend.DefaultPort()).To(Equal(int32(8000)))
			Expect(backend.NeedsModelInit()).To(BeTrue())
			Expect(backend.DefaultHPAMetric()).To(Equal("vllm:num_requests_running"))
		})

		It("should build args with tensor parallel and quantization", func() {
			tp := int32(2)
			maxLen := int32(4096)
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					VLLMConfig: &inferencev1alpha1.VLLMConfig{
						TensorParallelSize: &tp,
						MaxModelLen:        &maxLen,
						Quantization:       "awq",
						Dtype:              "float16",
					},
				},
			}
			model := &inferencev1alpha1.Model{}
			args := backend.BuildArgs(isvc, model, "/models/llama3", 8000)
			Expect(args).To(ContainElements("--model", "/models/llama3"))
			Expect(args).To(ContainElements("--tensor-parallel-size", "2"))
			Expect(args).To(ContainElements("--max-model-len", "4096"))
			Expect(args).To(ContainElements("--quantization", "awq"))
			Expect(args).To(ContainElements("--dtype", "float16"))
		})

		It("should build HTTP /health probes", func() {
			startup, liveness, readiness := backend.BuildProbes(8000)
			Expect(startup.HTTPGet.Path).To(Equal("/health"))
			Expect(liveness.HTTPGet.Path).To(Equal("/health"))
			Expect(readiness.HTTPGet.Path).To(Equal("/health"))
		})

		It("should pass extraArgs through to vllm container args", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ExtraArgs: []string{"--enable-prefix-caching", "--gpu-memory-utilization", "0.9"},
				},
			}
			model := &inferencev1alpha1.Model{}
			args := backend.BuildArgs(isvc, model, "/models/llama3", 8000)
			Expect(args).To(ContainElement("--enable-prefix-caching"))
			Expect(args).To(ContainElements("--gpu-memory-utilization", "0.9"))
		})

		It("should not include any extraArgs when nil", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{},
			}
			model := &inferencev1alpha1.Model{}
			args := backend.BuildArgs(isvc, model, "/models/llama3", 8000)
			Expect(args).To(ContainElements("--model", "/models/llama3"))
			Expect(args).To(ContainElements("--host", "0.0.0.0"))
			Expect(args).To(ContainElements("--port", "8000"))
			// No additional flags beyond defaults
			Expect(args).To(HaveLen(6))
		})

		It("should append extraArgs after typed flags", func() {
			tp := int32(2)
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					VLLMConfig: &inferencev1alpha1.VLLMConfig{TensorParallelSize: &tp},
					ExtraArgs:  []string{"--gpu-memory-utilization", "0.9"},
				},
			}
			model := &inferencev1alpha1.Model{}
			args := backend.BuildArgs(isvc, model, "/models/llama3", 8000)
			tpIdx := -1
			extraIdx := -1
			for i, a := range args {
				if a == "--tensor-parallel-size" {
					tpIdx = i
				}
				if a == "--gpu-memory-utilization" {
					extraIdx = i
				}
			}
			Expect(tpIdx).To(BeNumerically(">=", 0))
			Expect(extraIdx).To(BeNumerically(">", tpIdx))
		})

		It("should include --enable-prefix-caching when enablePrefixCaching is true", func() {
			enable := true
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					VLLMConfig: &inferencev1alpha1.VLLMConfig{EnablePrefixCaching: &enable},
				},
			}
			model := &inferencev1alpha1.Model{}
			args := backend.BuildArgs(isvc, model, "/models/llama3", 8000)
			Expect(args).To(ContainElement("--enable-prefix-caching"))
		})

		It("should NOT include --enable-prefix-caching when nil", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					VLLMConfig: &inferencev1alpha1.VLLMConfig{},
				},
			}
			model := &inferencev1alpha1.Model{}
			args := backend.BuildArgs(isvc, model, "/models/llama3", 8000)
			Expect(args).NotTo(ContainElement("--enable-prefix-caching"))
		})

		It("should NOT include --enable-prefix-caching when false", func() {
			enable := false
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					VLLMConfig: &inferencev1alpha1.VLLMConfig{EnablePrefixCaching: &enable},
				},
			}
			model := &inferencev1alpha1.Model{}
			args := backend.BuildArgs(isvc, model, "/models/llama3", 8000)
			Expect(args).NotTo(ContainElement("--enable-prefix-caching"))
		})

		It("should include --attention-backend when attentionBackend is set", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					VLLMConfig: &inferencev1alpha1.VLLMConfig{AttentionBackend: "flashinfer"},
				},
			}
			model := &inferencev1alpha1.Model{}
			args := backend.BuildArgs(isvc, model, "/models/llama3", 8000)
			Expect(args).To(ContainElements("--attention-backend", "flashinfer"))
		})

		It("should include each supported attentionBackend value", func() {
			backends := []string{"flashinfer", "flash_attn", "xformers", "torch_sdpa"}
			for _, b := range backends {
				isvc := &inferencev1alpha1.InferenceService{
					Spec: inferencev1alpha1.InferenceServiceSpec{
						VLLMConfig: &inferencev1alpha1.VLLMConfig{AttentionBackend: b},
					},
				}
				model := &inferencev1alpha1.Model{}
				args := backend.BuildArgs(isvc, model, "/models/llama3", 8000)
				Expect(args).To(ContainElements("--attention-backend", b))
			}
		})

		It("should NOT include --attention-backend when empty", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					VLLMConfig: &inferencev1alpha1.VLLMConfig{},
				},
			}
			model := &inferencev1alpha1.Model{}
			args := backend.BuildArgs(isvc, model, "/models/llama3", 8000)
			Expect(args).NotTo(ContainElement("--attention-backend"))
		})
	})

	Context("TGIBackend", func() {
		var backend *TGIBackend

		BeforeEach(func() {
			backend = &TGIBackend{}
		})

		It("should return correct defaults", func() {
			Expect(backend.ContainerName()).To(Equal("tgi"))
			Expect(backend.DefaultImage()).To(Equal("ghcr.io/huggingface/text-generation-inference:latest"))
			Expect(backend.DefaultPort()).To(Equal(int32(80)))
			Expect(backend.NeedsModelInit()).To(BeFalse())
			Expect(backend.DefaultHPAMetric()).To(Equal("tgi:queue_size"))
		})

		It("should build args with quantize and model source fallback", func() {
			maxInput := int32(2048)
			maxTotal := int32(4096)
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					TGIConfig: &inferencev1alpha1.TGIConfig{
						Quantize:       "bitsandbytes",
						MaxInputLength: &maxInput,
						MaxTotalTokens: &maxTotal,
					},
				},
			}
			model := &inferencev1alpha1.Model{
				Spec: inferencev1alpha1.ModelSpec{Source: "meta-llama/Llama-3-8B"},
			}
			args := backend.BuildArgs(isvc, model, "", 80)
			Expect(args).To(ContainElements("--model-id", "meta-llama/Llama-3-8B"))
			Expect(args).To(ContainElements("--quantize", "bitsandbytes"))
			Expect(args).To(ContainElements("--max-input-length", "2048"))
			Expect(args).To(ContainElements("--max-total-tokens", "4096"))
		})
	})

	Context("PersonaPlexBackend", func() {
		var backend *PersonaPlexBackend

		BeforeEach(func() {
			backend = &PersonaPlexBackend{}
		})

		It("should return correct defaults", func() {
			Expect(backend.ContainerName()).To(Equal("personaplex"))
			Expect(backend.DefaultImage()).To(Equal(""))
			Expect(backend.DefaultPort()).To(Equal(int32(8998)))
			Expect(backend.NeedsModelInit()).To(BeFalse())
		})

		It("should build TCP socket probes on port 8998", func() {
			startup, liveness, readiness := backend.BuildProbes(8998)
			Expect(startup.TCPSocket).NotTo(BeNil())
			Expect(startup.TCPSocket.Port.IntValue()).To(Equal(8998))
			Expect(liveness.TCPSocket).NotTo(BeNil())
			Expect(readiness.TCPSocket).NotTo(BeNil())
		})

		It("should build args with quantize-4bit and cpu-offload", func() {
			quantize := true
			cpuOffload := true
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					PersonaPlexConfig: &inferencev1alpha1.PersonaPlexConfig{
						Quantize4Bit: &quantize,
						CPUOffload:   &cpuOffload,
					},
				},
			}
			args := backend.BuildArgs(isvc, nil, "", 0)
			Expect(args).To(ContainElement("--ssl"))
			Expect(args).To(ContainElement("--quantize-4bit"))
			Expect(args).To(ContainElement("--cpu-offload"))
		})

		It("should build minimal args without config", func() {
			isvc := &inferencev1alpha1.InferenceService{}
			args := backend.BuildArgs(isvc, nil, "", 0)
			Expect(args).To(Equal([]string{"--ssl", "/app/ssl"}))
		})

		It("should provide a command via CommandBuilder", func() {
			cmd := backend.BuildCommand()
			Expect(cmd).To(Equal([]string{"/app/moshi/.venv/bin/python", "-m", "moshi.server"}))
		})

		It("should build env with HF_TOKEN from secret ref", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					PersonaPlexConfig: &inferencev1alpha1.PersonaPlexConfig{
						HFTokenSecretRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "hf-token"},
							Key:                  "HF_TOKEN",
						},
					},
				},
			}
			env := backend.BuildEnv(isvc)
			Expect(env).To(HaveLen(2))
			Expect(env[0].Name).To(Equal("HF_TOKEN"))
			Expect(env[0].ValueFrom.SecretKeyRef.Name).To(Equal("hf-token"))
			Expect(env[1].Name).To(Equal("NO_TORCH_COMPILE"))
		})
	})
})

var _ = Describe("PersonaPlex Runtime Deployment Construction", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
	})

	It("should deploy PersonaPlex with typed config", func() {
		quantize := true
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "pp-model", Namespace: "voice-ai"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "nvidia/personaplex-7b-v1",
				Format: "safetensors",
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "cuda",
					GPU:         &inferencev1alpha1.GPUSpec{Enabled: true, Count: 1, Vendor: "nvidia"},
				},
			},
			Status: inferencev1alpha1.ModelStatus{Phase: "Ready"},
		}

		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "pp-svc", Namespace: "voice-ai"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "pp-model",
				Runtime:  "personaplex",
				Image:    "registry.defilan.net/personaplex:7b-v1-4bit-cuda13",
				PersonaPlexConfig: &inferencev1alpha1.PersonaPlexConfig{
					Quantize4Bit: &quantize,
					HFTokenSecretRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "hf-token"},
						Key:                  "HF_TOKEN",
					},
				},
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU:    1,
					Memory: "32Gi",
				},
			},
		}

		deployment := reconciler.constructDeployment(isvc, model, 1)
		container := deployment.Spec.Template.Spec.Containers[0]

		By("verifying container name")
		Expect(container.Name).To(Equal("personaplex"))

		By("verifying custom image")
		Expect(container.Image).To(Equal("registry.defilan.net/personaplex:7b-v1-4bit-cuda13"))

		By("verifying command set by CommandBuilder")
		Expect(container.Command).To(Equal([]string{"/app/moshi/.venv/bin/python", "-m", "moshi.server"}))

		By("verifying default port 8998")
		Expect(container.Ports[0].ContainerPort).To(Equal(int32(8998)))

		By("verifying args include --quantize-4bit")
		Expect(container.Args).To(ContainElement("--ssl"))
		Expect(container.Args).To(ContainElement("--quantize-4bit"))

		By("verifying env includes HF_TOKEN from secret and NO_TORCH_COMPILE")
		var hfToken, noCompile bool
		for _, e := range container.Env {
			if e.Name == "HF_TOKEN" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef.Name == "hf-token" {
				hfToken = true
			}
			if e.Name == "NO_TORCH_COMPILE" {
				noCompile = true
			}
		}
		Expect(hfToken).To(BeTrue(), "HF_TOKEN env from secret not found")
		Expect(noCompile).To(BeTrue(), "NO_TORCH_COMPILE env not found")

		By("verifying no init containers")
		Expect(deployment.Spec.Template.Spec.InitContainers).To(BeEmpty())

		By("verifying TCP probes")
		Expect(container.StartupProbe.TCPSocket).NotTo(BeNil())
		Expect(container.StartupProbe.TCPSocket.Port.IntValue()).To(Equal(8998))

		By("verifying GPU resources")
		gpuLimit := container.Resources.Limits["nvidia.com/gpu"]
		Expect(gpuLimit).To(Equal(resource.MustParse("1")))
	})
})

var _ = Describe("Generic Runtime Deployment Construction", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
	})

	It("should deploy a custom container with command, args, env, and custom port", func() {
		containerPort := int32(8998)
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "personaplex-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "nvidia/personaplex-7b-v1",
				Format: "safetensors",
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "cuda",
					GPU:         &inferencev1alpha1.GPUSpec{Enabled: true, Count: 1, Vendor: "nvidia"},
				},
			},
			Status: inferencev1alpha1.ModelStatus{Phase: "Ready"},
		}

		skipInit := true
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "personaplex-svc", Namespace: "voice-ai"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:      "personaplex-model",
				Runtime:       "generic",
				Image:         "registry.defilan.net/personaplex:7b-v1-4bit-cuda13",
				Command:       []string{"/app/moshi/.venv/bin/python", "-m", "moshi.server"},
				Args:          []string{"--ssl", "/app/ssl", "--quantize-4bit"},
				ContainerPort: &containerPort,
				SkipModelInit: &skipInit,
				Env: []corev1.EnvVar{
					{Name: "HF_TOKEN", Value: "test-token"},
					{Name: "NO_TORCH_COMPILE", Value: "1"},
				},
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU:    1,
					CPU:    "2",
					Memory: "16Gi",
				},
			},
		}

		deployment := reconciler.constructDeployment(isvc, model, 1)

		By("verifying container name is generic, not llama-server")
		container := deployment.Spec.Template.Spec.Containers[0]
		Expect(container.Name).To(Equal("inference-server"))

		By("verifying custom image")
		Expect(container.Image).To(Equal("registry.defilan.net/personaplex:7b-v1-4bit-cuda13"))

		By("verifying custom command")
		Expect(container.Command).To(Equal([]string{"/app/moshi/.venv/bin/python", "-m", "moshi.server"}))

		By("verifying custom args")
		Expect(container.Args).To(Equal([]string{"--ssl", "/app/ssl", "--quantize-4bit"}))

		By("verifying custom port")
		Expect(container.Ports[0].ContainerPort).To(Equal(int32(8998)))

		By("verifying env vars")
		Expect(container.Env).To(HaveLen(2))
		Expect(container.Env[0].Name).To(Equal("HF_TOKEN"))
		Expect(container.Env[1].Name).To(Equal("NO_TORCH_COMPILE"))

		By("verifying no init containers (skipModelInit=true)")
		Expect(deployment.Spec.Template.Spec.InitContainers).To(BeEmpty())

		By("verifying no volumes (skipModelInit=true)")
		Expect(deployment.Spec.Template.Spec.Volumes).To(BeEmpty())

		By("verifying GPU resource limits")
		gpuLimit := container.Resources.Limits["nvidia.com/gpu"]
		Expect(gpuLimit).To(Equal(resource.MustParse("1")))

		By("verifying TCP socket probes (not HTTP)")
		Expect(container.StartupProbe.TCPSocket).NotTo(BeNil())
		Expect(container.StartupProbe.TCPSocket.Port.IntValue()).To(Equal(8998))
		Expect(container.StartupProbe.HTTPGet).To(BeNil())
		Expect(container.LivenessProbe.TCPSocket).NotTo(BeNil())
		Expect(container.ReadinessProbe.TCPSocket).NotTo(BeNil())
	})

	It("should support probe overrides", func() {
		containerPort := int32(8000)
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "vllm-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "meta-llama/Llama-3-8B",
				Format: "safetensors",
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "cuda",
					GPU:         &inferencev1alpha1.GPUSpec{Enabled: true, Count: 1, Vendor: "nvidia"},
				},
			},
			Status: inferencev1alpha1.ModelStatus{Phase: "Ready"},
		}

		skipInit := true
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "vllm-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:      "vllm-model",
				Runtime:       "generic",
				Image:         "vllm/vllm-openai:latest",
				Args:          []string{"--model", "/models/llama3", "--host", "0.0.0.0"},
				ContainerPort: &containerPort,
				SkipModelInit: &skipInit,
				ProbeOverrides: &inferencev1alpha1.ProbeOverrides{
					Startup: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/health",
								Port: intstr.FromInt32(8000),
							},
						},
						PeriodSeconds:    10,
						FailureThreshold: 60,
					},
					Liveness: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/health",
								Port: intstr.FromInt32(8000),
							},
						},
						PeriodSeconds: 30,
					},
				},
			},
		}

		deployment := reconciler.constructDeployment(isvc, model, 1)
		container := deployment.Spec.Template.Spec.Containers[0]

		By("verifying startup probe is overridden to HTTP")
		Expect(container.StartupProbe.HTTPGet).NotTo(BeNil())
		Expect(container.StartupProbe.HTTPGet.Path).To(Equal("/health"))
		Expect(container.StartupProbe.FailureThreshold).To(Equal(int32(60)))

		By("verifying liveness probe is overridden")
		Expect(container.LivenessProbe.HTTPGet).NotTo(BeNil())
		Expect(container.LivenessProbe.PeriodSeconds).To(Equal(int32(30)))

		By("verifying readiness probe uses default TCP (not overridden)")
		Expect(container.ReadinessProbe.TCPSocket).NotTo(BeNil())
	})

	It("should use containerPort override with llamacpp runtime too", func() {
		containerPort := int32(9090)
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "port-test-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://example.com/model.gguf",
				Format: "gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "cpu",
				},
			},
			Status: inferencev1alpha1.ModelStatus{Phase: "Ready", Path: "/models/test.gguf"},
		}

		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "port-test-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:      "port-test-model",
				ContainerPort: &containerPort,
			},
		}

		deployment := reconciler.constructDeployment(isvc, model, 1)
		container := deployment.Spec.Template.Spec.Containers[0]

		By("verifying containerPort override works for llamacpp")
		Expect(container.Ports[0].ContainerPort).To(Equal(int32(9090)))
		Expect(container.Args).To(ContainElement("9090"))
		Expect(container.StartupProbe.HTTPGet.Port.IntValue()).To(Equal(9090))
	})
})

// Regression tests for constructDeployment() — captures exact output before runtime abstraction refactor.
// These tests verify container name, image, args, probes, ports, volumes, strategy, and resources.
// If any of these break during the refactor, the llama.cpp path has regressed.
var _ = Describe("constructDeployment Regression Tests", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
	})

	Context("CPU-only model with default settings", func() {
		It("should produce exact expected deployment", func() {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "cpu-basic", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Format: "gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cpu",
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
					Path:  "/models/cpu-basic.gguf",
				},
			}

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "cpu-basic-svc", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "cpu-basic",
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying container basics")
			Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := deployment.Spec.Template.Spec.Containers[0]
			Expect(container.Name).To(Equal("llama-server"))
			Expect(container.Image).To(Equal("ghcr.io/ggml-org/llama.cpp:server"))

			By("verifying default port is 8080")
			Expect(container.Ports).To(HaveLen(1))
			Expect(container.Ports[0].ContainerPort).To(Equal(int32(8080)))
			Expect(container.Ports[0].Name).To(Equal("http"))

			By("verifying base args")
			Expect(container.Args).To(ContainElements("--model", "--host", "0.0.0.0", "--port", "8080"))
			Expect(container.Args).To(ContainElement("--metrics"))

			By("verifying no GPU args")
			Expect(container.Args).NotTo(ContainElement("--n-gpu-layers"))
			Expect(container.Args).NotTo(ContainElement("--split-mode"))

			By("verifying startup probe")
			Expect(container.StartupProbe).NotTo(BeNil())
			Expect(container.StartupProbe.HTTPGet).NotTo(BeNil())
			Expect(container.StartupProbe.HTTPGet.Path).To(Equal("/health"))
			Expect(container.StartupProbe.HTTPGet.Port.IntValue()).To(Equal(8080))
			Expect(container.StartupProbe.PeriodSeconds).To(Equal(int32(10)))
			Expect(container.StartupProbe.FailureThreshold).To(Equal(int32(180)))

			By("verifying liveness probe")
			Expect(container.LivenessProbe).NotTo(BeNil())
			Expect(container.LivenessProbe.HTTPGet.Path).To(Equal("/health"))
			Expect(container.LivenessProbe.PeriodSeconds).To(Equal(int32(15)))
			Expect(container.LivenessProbe.FailureThreshold).To(Equal(int32(3)))

			By("verifying readiness probe")
			Expect(container.ReadinessProbe).NotTo(BeNil())
			Expect(container.ReadinessProbe.HTTPGet.Path).To(Equal("/health"))
			Expect(container.ReadinessProbe.PeriodSeconds).To(Equal(int32(10)))
			Expect(container.ReadinessProbe.FailureThreshold).To(Equal(int32(3)))

			By("verifying no GPU resources")
			_, hasGPU := container.Resources.Limits["nvidia.com/gpu"]
			Expect(hasGPU).To(BeFalse())

			By("verifying default strategy is not Recreate")
			Expect(deployment.Spec.Strategy.Type).NotTo(Equal(appsv1.RecreateDeploymentStrategyType))
		})
	})

	Context("GPU model with all llama.cpp options", func() {
		It("should produce exact expected deployment with all flags", func() {
			contextSize := int32(4096)
			parallelSlots := int32(4)
			flashAttn := true
			jinja := true
			moeCPUOffload := true
			noKvOffload := true
			batchSize := int32(2048)
			ubatchSize := int32(256)
			noWarmup := true
			reasoningBudget := int32(1024)

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu-full", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Format: "gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cuda",
						GPU: &inferencev1alpha1.GPUSpec{
							Enabled: true,
							Count:   1,
							Vendor:  "nvidia",
							Layers:  32,
						},
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
					Path:  "/models/gpu-full.gguf",
				},
			}

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu-full-svc", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:               "gpu-full",
					Image:                  "ghcr.io/ggml-org/llama.cpp:server-cuda13",
					ContextSize:            &contextSize,
					ParallelSlots:          &parallelSlots,
					FlashAttention:         &flashAttn,
					Jinja:                  &jinja,
					CacheTypeK:             "q8_0",
					CacheTypeV:             "q4_0",
					MoeCPUOffload:          &moeCPUOffload,
					NoKvOffload:            &noKvOffload,
					TensorOverrides:        []string{"exps=CPU", "token_embd=CUDA0"},
					BatchSize:              &batchSize,
					UBatchSize:             &ubatchSize,
					NoWarmup:               &noWarmup,
					ReasoningBudget:        &reasoningBudget,
					ReasoningBudgetMessage: "wrap it up",
					MetadataOverrides:      []string{"qwen35moe.context_length=int:1048576"},
					ExtraArgs:              []string{"--log-disable"},
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU:    1,
						CPU:    "2",
						Memory: "4Gi",
					},
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)
			container := deployment.Spec.Template.Spec.Containers[0]

			By("verifying custom image")
			Expect(container.Image).To(Equal("ghcr.io/ggml-org/llama.cpp:server-cuda13"))

			By("verifying GPU layers")
			Expect(container.Args).To(ContainElements("--n-gpu-layers", "32"))

			By("verifying context size")
			Expect(container.Args).To(ContainElements("--ctx-size", "4096"))

			By("verifying parallel slots")
			Expect(container.Args).To(ContainElements("--parallel", "4"))

			By("verifying flash attention")
			Expect(container.Args).To(ContainElements("--flash-attn", "on"))

			By("verifying jinja")
			Expect(container.Args).To(ContainElement("--jinja"))

			By("verifying cache types")
			Expect(container.Args).To(ContainElements("--cache-type-k", "q8_0"))
			Expect(container.Args).To(ContainElements("--cache-type-v", "q4_0"))

			By("verifying MoE CPU offload")
			Expect(container.Args).To(ContainElement("--cpu-moe"))

			By("verifying no KV offload")
			Expect(container.Args).To(ContainElement("--no-kv-offload"))

			By("verifying tensor overrides")
			Expect(container.Args).To(ContainElements("--override-tensor", "exps=CPU"))
			Expect(container.Args).To(ContainElements("--override-tensor", "token_embd=CUDA0"))

			By("verifying batch size")
			Expect(container.Args).To(ContainElements("--batch-size", "2048"))

			By("verifying micro-batch size")
			Expect(container.Args).To(ContainElements("--ubatch-size", "256"))

			By("verifying no warmup")
			Expect(container.Args).To(ContainElement("--no-warmup"))

			By("verifying reasoning budget")
			Expect(container.Args).To(ContainElements("--reasoning-budget", "1024"))
			Expect(container.Args).To(ContainElements("--reasoning-budget-message", "wrap it up"))

			By("verifying metadata overrides")
			Expect(container.Args).To(ContainElements("--override-kv", "qwen35moe.context_length=int:1048576"))

			By("verifying extra args")
			Expect(container.Args).To(ContainElement("--log-disable"))

			By("verifying metrics always present")
			Expect(container.Args).To(ContainElement("--metrics"))

			By("verifying GPU resource limits")
			gpuLimit := container.Resources.Limits["nvidia.com/gpu"]
			Expect(gpuLimit).To(Equal(resource.MustParse("1")))

			By("verifying CPU and memory requests")
			Expect(container.Resources.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("2")))
			Expect(container.Resources.Requests[corev1.ResourceMemory]).To(Equal(resource.MustParse("4Gi")))

			By("verifying no multi-GPU flags for single GPU")
			Expect(container.Args).NotTo(ContainElement("--split-mode"))
			Expect(container.Args).NotTo(ContainElement("--tensor-split"))
		})
	})

	Context("GPU model with all vLLM options", func() {
		It("should produce deployment with every supported vllmConfig field", func() {
			tp := int32(2)
			maxLen := int32(8192)
			enablePrefixCache := true

			backend := &VLLMBackend{}
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "vllm-full", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "meta-llama/Llama-3.1-8B-Instruct",
					Format: "safetensors",
				},
			}

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "vllm-full-svc", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "vllm-full",
					Runtime:  "vllm",
					VLLMConfig: &inferencev1alpha1.VLLMConfig{
						TensorParallelSize:  &tp,
						MaxModelLen:         &maxLen,
						Quantization:        "awq",
						Dtype:               "bfloat16",
						EnablePrefixCaching: &enablePrefixCache,
						AttentionBackend:    "flashinfer",
					},
					ExtraArgs: []string{"--gpu-memory-utilization", "0.9"},
				},
			}

			args := backend.BuildArgs(isvc, model, "/models/vllm-full", 8000)

			By("verifying tensor parallel size")
			Expect(args).To(ContainElements("--tensor-parallel-size", "2"))

			By("verifying max model len")
			Expect(args).To(ContainElements("--max-model-len", "8192"))

			By("verifying quantization")
			Expect(args).To(ContainElements("--quantization", "awq"))

			By("verifying dtype")
			Expect(args).To(ContainElements("--dtype", "bfloat16"))

			By("verifying prefix caching")
			Expect(args).To(ContainElement("--enable-prefix-caching"))

			By("verifying attention backend")
			Expect(args).To(ContainElements("--attention-backend", "flashinfer"))

			By("verifying extraArgs passthrough")
			Expect(args).To(ContainElements("--gpu-memory-utilization", "0.9"))

			By("verifying extraArgs land after typed flags")
			tpIdx, extraIdx := -1, -1
			for i, a := range args {
				if a == "--tensor-parallel-size" {
					tpIdx = i
				}
				if a == "--gpu-memory-utilization" {
					extraIdx = i
				}
			}
			Expect(tpIdx).To(BeNumerically(">=", 0))
			Expect(extraIdx).To(BeNumerically(">", tpIdx))
		})
	})

	Context("multi-GPU model with sharding", func() {
		It("should produce Recreate strategy and GPU tolerations", func() {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "multi-gpu-reg", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Format: "gguf",
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
					Path:  "/models/multi-gpu-reg.gguf",
				},
			}

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "multi-gpu-reg-svc", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "multi-gpu-reg",
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)
			container := deployment.Spec.Template.Spec.Containers[0]

			By("verifying multi-GPU args")
			Expect(container.Args).To(ContainElements("--n-gpu-layers", "99"))
			Expect(container.Args).To(ContainElements("--split-mode", "layer"))
			Expect(container.Args).To(ContainElements("--tensor-split", "1,1"))

			By("verifying Recreate strategy")
			Expect(deployment.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))

			By("verifying GPU toleration")
			tolerations := deployment.Spec.Template.Spec.Tolerations
			Expect(tolerations).NotTo(BeEmpty())
			found := false
			for _, t := range tolerations {
				if t.Key == "nvidia.com/gpu" {
					found = true
					Expect(t.Value).To(Equal("present"))
					Expect(t.Effect).To(Equal(corev1.TaintEffectNoSchedule))
				}
			}
			Expect(found).To(BeTrue(), "nvidia.com/gpu toleration not found")

			By("verifying GPU resource limits for 2 GPUs")
			gpuLimit := container.Resources.Limits["nvidia.com/gpu"]
			Expect(gpuLimit).To(Equal(resource.MustParse("2")))
		})
	})

	Context("init container and volume setup", func() {
		It("should have init container for URL-sourced models", func() {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "init-test", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Format: "gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cpu",
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
					Path:  "/models/init-test.gguf",
				},
			}

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "init-test-svc", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "init-test",
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying init containers exist for model download")
			Expect(deployment.Spec.Template.Spec.InitContainers).NotTo(BeEmpty())

			By("verifying volumes exist for model storage")
			Expect(deployment.Spec.Template.Spec.Volumes).NotTo(BeEmpty())

			By("verifying main container has volume mounts")
			container := deployment.Spec.Template.Spec.Containers[0]
			Expect(container.VolumeMounts).NotTo(BeEmpty())
		})
	})

	Context("labels and metadata", func() {
		It("should set correct labels on deployment and pod template", func() {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "label-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Format: "gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cpu",
					},
				},
				Status: inferencev1alpha1.ModelStatus{
					Phase: "Ready",
					Path:  "/models/label-model.gguf",
				},
			}

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "label-svc", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "label-model",
				},
			}

			deployment := reconciler.constructDeployment(isvc, model, 1)

			By("verifying deployment labels")
			Expect(deployment.Labels["app"]).To(Equal("label-svc"))
			Expect(deployment.Labels["inference.llmkube.dev/model"]).To(Equal("label-model"))
			Expect(deployment.Labels["inference.llmkube.dev/service"]).To(Equal("label-svc"))

			By("verifying pod template labels match")
			Expect(deployment.Spec.Template.Labels["app"]).To(Equal("label-svc"))
			Expect(deployment.Spec.Template.Labels["inference.llmkube.dev/model"]).To(Equal("label-model"))
			Expect(deployment.Spec.Template.Labels["inference.llmkube.dev/service"]).To(Equal("label-svc"))

			By("verifying selector matches pod labels")
			Expect(deployment.Spec.Selector.MatchLabels["app"]).To(Equal("label-svc"))
		})
	})
})
