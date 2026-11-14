/*
Copyright 2026.

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
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// metalEndpointSliceFixture builds an EndpointSlice the metal-agent would
// register for isvcName with the given ready addresses, labeled so the
// controller's list-by-service-name finds it.
func metalEndpointSliceFixture(isvcName string, addresses ...string) *discoveryv1.EndpointSlice {
	eps := make([]discoveryv1.Endpoint, 0, len(addresses))
	for _, a := range addresses {
		eps = append(eps, discoveryv1.Endpoint{
			Addresses:  []string{a},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		})
	}
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      isvcName,
			Namespace: "default",
			Labels: map[string]string{
				"kubernetes.io/service-name": isvcName,
				"llmkube.ai/managed-by":      "metal-agent",
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   eps,
		Ports: []discoveryv1.EndpointPort{{
			Port:     ptr.To(int32(8080)),
			Protocol: ptr.To(corev1.ProtocolTCP),
		}},
	}
}

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
						Source:       "https://example.com/model.gguf",
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
						Source:       "https://example.com/multi-gpu-model.gguf",
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
			// The Model must be Ready for reconcile to reach the Deployment
			// builder; a not-Ready Model stops at Pending and creates nothing.
			// This It asserts the created Deployment carries the requested GPU
			// count, not just that the spec holds what the test itself wrote
			// (#378 finding 2).
			model := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, modelNamespacedName, model)).To(Succeed())
			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			By("reconciling the InferenceService")
			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: serviceNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred(), "a multi-GPU reconcile must not error (#378 finding 2)")

			By("verifying the Deployment carries the requested GPU count")
			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, serviceNamespacedName, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Spec.Containers).NotTo(BeEmpty())
			gpuLimit := deployment.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]
			Expect(gpuLimit.String()).To(Equal("2"))

			// The builder's intentional multi-GPU choices must survive the
			// reconcile path, not only the constructor snapshot: a GPU
			// workload replaces rather than surging and pins to the device
			// taint. Asserting them here is what catches a builder regression
			// that the shape test would restate (#378 finding 14).
			By("verifying the Deployment carries the multi-GPU scheduling choices")
			Expect(deployment.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))

			var gpuToleration bool
			for _, t := range deployment.Spec.Template.Spec.Tolerations {
				if t.Key == "nvidia.com/gpu" {
					gpuToleration = t.Value == "present" && t.Effect == corev1.TaintEffectNoSchedule
				}
			}
			Expect(gpuToleration).To(BeTrue(), "a multi-GPU workload must tolerate the nvidia.com/gpu:present NoSchedule taint")
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
		phase, info := reconciler.determinePhase(context.Background(), isvc, 2, 2, false, &appsv1.Deployment{}, nil)
		Expect(phase).To(Equal("Ready"))
		Expect(info).To(BeNil())
	})

	It("should return Progressing when partially ready", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		}
		phase, info := reconciler.determinePhase(context.Background(), isvc, 1, 3, false, &appsv1.Deployment{}, nil)
		Expect(phase).To(Equal("Progressing"))
		Expect(info).To(BeNil())
	})

	It("should return Creating when no replicas ready and no scheduling issues", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "no-pods-test", Namespace: "default"},
		}
		phase, _ := reconciler.determinePhase(context.Background(), isvc, 0, 1, false, &appsv1.Deployment{}, nil)
		Expect(phase).To(Equal("Creating"))
	})

	It("should return Creating when deployment is nil (Metal path)", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		}
		phase, _ := reconciler.determinePhase(context.Background(), isvc, 0, 1, true, nil, nil)
		Expect(phase).To(Equal("Creating"))
	})

	// A service the agent refused never registers an endpoint, so it sits in
	// the not-ready metal branch. Replacing the refusal with
	// WaitingForMetalAgent hid it and bumped the resourceVersion, which the
	// agent's poll treated as a change and refused again every few seconds.
	DescribeTable("should keep an agent refusal on a not-ready metal service",
		func(reason string) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			}
			isvc.Status.SchedulingStatus = reason
			phase, info := reconciler.determinePhase(context.Background(), isvc, 0, 1, true, nil, nil)
			Expect(phase).To(Equal(PhaseCreating))
			Expect(info).To(BeNil())
		},
		Entry("memory admission", inferencev1alpha1.ReasonInsufficientMemory),
		Entry("memory check failure", inferencev1alpha1.ReasonMemoryCheckFailed),
		Entry("endpoint name conflict", inferencev1alpha1.ReasonEndpointNameConflict),
		Entry("model source outside the roots", inferencev1alpha1.ReasonModelSourceNotAllowed),
		Entry("rejected extraArgs", inferencev1alpha1.ReasonExtraArgsRejected),
		Entry("relay service name too long", inferencev1alpha1.ReasonServiceNameTooLong),
		Entry("model digest mismatch", inferencev1alpha1.ReasonModelDigestMismatch),
		Entry("model download over the agent's cap", inferencev1alpha1.ReasonModelDownloadTooLarge),
		Entry("sourceSecretRef Secret not allowed", inferencev1alpha1.ReasonSourceSecretNotAllowed),
		Entry("runtime with no executor on the agent", inferencev1alpha1.ReasonRuntimeNotAvailable),
		Entry("a start that failed and is being retried", inferencev1alpha1.ReasonStartFailed),
	)

	It("should replace a controller-written status with WaitingForMetalAgent on a not-ready metal service", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		}
		isvc.Status.SchedulingStatus = "AgentHeartbeatStale"
		_, info := reconciler.determinePhase(context.Background(), isvc, 0, 1, true, nil, nil)
		Expect(info).NotTo(BeNil())
		Expect(info.Status).To(Equal("WaitingForMetalAgent"))
	})

	It("should let a stale heartbeat replace an agent refusal", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		}
		isvc.Status.SchedulingStatus = "ExtraArgsRejected"
		snap := &metalSnapshot{Kind: metalHBStale, RawHeartbeat: "2026-01-01T00:00:00Z"}
		_, info := reconciler.determinePhase(context.Background(), isvc, 0, 1, true, nil, snap)
		Expect(info).NotTo(BeNil())
		Expect(info.Status).To(Equal("AgentHeartbeatStale"))
	})

	It("should return Stopped when replicas=0 on generic path", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		}
		phase, info := reconciler.determinePhase(context.Background(), isvc, 0, 0, false, &appsv1.Deployment{}, nil)
		Expect(phase).To(Equal(PhaseStopped))
		Expect(info).To(BeNil())
	})

	It("should return Stopped when replicas=0 on Metal path", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		}
		phase, info := reconciler.determinePhase(context.Background(), isvc, 0, 0, true, nil, nil)
		Expect(phase).To(Equal(PhaseStopped))
		Expect(info).To(BeNil())
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

			By("verifying no workload resources were created for the missing Model")
			depErr := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, &appsv1.Deployment{})
			Expect(errors.IsNotFound(depErr)).To(BeTrue(), "a missing Model must not create a Deployment")
			svcErr := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, &corev1.Service{})
			Expect(errors.IsNotFound(svcErr)).To(BeTrue(), "a missing Model must not create a Service")

			By("verifying the Failed condition carries the missing-Model cause")
			degraded := meta.FindStatusCondition(updated.Status.Conditions, ConditionDegraded)
			Expect(degraded).NotTo(BeNil(), "a missing Model must surface a Degraded condition")
			Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
			Expect(degraded.Message).To(ContainSubstring("Model not found"))
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

			// Ready means a request to this endpoint will succeed. A not-Ready
			// Model must therefore leave no Deployment, Service, or HPA behind,
			// not merely report a Pending phase (#378 finding 11).
			By("verifying no workload resources were created while the Model is not Ready")
			depErr := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, &appsv1.Deployment{})
			Expect(errors.IsNotFound(depErr)).To(BeTrue(), "a not-Ready Model must not create a Deployment")
			svcErr := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, &corev1.Service{})
			Expect(errors.IsNotFound(svcErr)).To(BeTrue(), "a not-Ready Model must not create a Service")
			hpaErr := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, &autoscalingv2.HorizontalPodAutoscaler{})
			Expect(errors.IsNotFound(hpaErr)).To(BeTrue(), "a not-Ready Model must not create an HPA")
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

			// The Service selector must select the Deployment's pods. An endpoint
			// string with no selector backing it is the #374 shape: correct in
			// the report, unreachable in practice (#378 finding 1).
			By("verifying the Service selector selects the Deployment pods")
			Expect(svc.Spec.Selector).NotTo(BeEmpty())
			for k, v := range svc.Spec.Selector {
				Expect(dep.Spec.Template.Labels).To(HaveKeyWithValue(k, v))
			}

			By("verifying status was updated")
			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal("Creating"))
			Expect(updated.Status.Endpoint).NotTo(BeEmpty())
		})

		It("should report Ready once the Deployment reports its ready replicas", func() {
			// Ready means a request to the endpoint will succeed. The generic
			// path reaches Ready only when the Deployment reports the desired
			// ready replicas, so this drives determinePhase through Reconcile
			// rather than calling the helper with synthetic tuples: a caller
			// passing the wrong readyReplicas (the literal #374 bug) fails
			// here (#378 finding 3).
			modelName := "model-ready-replicas"
			isvcName := "isvc-ready-replicas"

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
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// A queued workload is not serving yet. Mark the Deployment as
			// reporting its desired ready replicas, the only gate between
			// Creating and Ready on the generic path.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
			dep.Status.Replicas = replicas
			dep.Status.ReadyReplicas = replicas
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseReady))
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

			By("verifying status is Creating (no Endpoints yet from the metal-agent; issue #374)")
			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal("Creating"))
		})

		It("should be Ready once the metal-agent registers Endpoints (issue #374)", func() {
			modelName := "metal-model-with-ep"
			isvcName := "isvc-metal-with-ep"

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

			// Simulate the metal-agent registering an EndpointSlice once
			// llama-server is healthy.
			slice := metalEndpointSliceFixture(isvcName, "192.0.2.10")
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, slice)
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
			Expect(updated.Status.Phase).To(Equal("Ready"))
			Expect(updated.Status.ReadyReplicas).To(Equal(int32(1)))
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
			// The URL is only meaningful if the metal-agent actually backs it:
			// register the EndpointSlice before reconciling, then assert both
			// the Ready phase and the URL, so a fabricated URL cannot pass
			// (#378 finding 4).
			slice := metalEndpointSliceFixture(isvcName, "192.0.2.10")
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, slice) }()

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal("Ready"))
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
			// Back the URL with a real EndpointSlice before reconciling (#378
			// finding 4): Ready and the URL are asserted together. The
			// controller matches the slice by the sanitized service name, so
			// the fixture must carry it too.
			slice := metalEndpointSliceFixture(sanitizeDNSName(isvcName), "192.0.2.10")
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, slice) }()

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal("Ready"))
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
			// Back the URL with a real EndpointSlice before reconciling (#378
			// finding 4): Ready and the URL (custom port and path) are
			// asserted together.
			slice := metalEndpointSliceFixture(isvcName, "192.0.2.10")
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, slice) }()

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal("Ready"))
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

		It("should expose and accept writes via the /scale subresource", func() {
			modelName := "model-scale-subresource"
			isvcName := "isvc-scale-subresource"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
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
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, isvc) }()

			// Simulate the metal-agent registering 2 ready endpoints
			slice := metalEndpointSliceFixture(isvcName, "192.0.2.10", "192.0.2.11")
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, slice) }()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("reading status.replicas and status.selector via the /scale subresource after reconcile")
			scale := &autoscalingv1.Scale{}
			Expect(k8sClient.SubResource("scale").Get(ctx, isvc, scale)).To(Succeed())
			Expect(scale.Spec.Replicas).To(Equal(int32(2)))
			Expect(scale.Status.Replicas).To(Equal(int32(2)))
			// An HPA rejects a scale target whose selector is missing or
			// unparseable (validateAndParseSelector), so a blank selector here
			// reproduces the #1881 InvalidSelector failure.
			Expect(scale.Status.Selector).To(Equal(
				"app=" + isvcName + ",inference.llmkube.dev/service=" + isvcName))
			_, selectorErr := labels.Parse(scale.Status.Selector)
			Expect(selectorErr).NotTo(HaveOccurred())

			By("writing a new replica count via the /scale subresource")
			scale.Spec.Replicas = 3
			Expect(k8sClient.SubResource("scale").Update(ctx, isvc, client.WithSubResourceBody(scale))).To(Succeed())

			By("verifying the write propagated to spec.replicas")
			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(*updated.Spec.Replicas).To(Equal(int32(3)))
		})

		It("should support scale-to-zero via the /scale subresource", func() {
			modelName := "model-scale-to-zero"
			isvcName := "isvc-scale-to-zero"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
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
			defer func() { _ = k8sClient.Delete(ctx, isvc) }()

			slice := metalEndpointSliceFixture(isvcName, "192.0.2.20")
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, slice) }()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("scaling to zero via the /scale subresource")
			scale := &autoscalingv1.Scale{}
			Expect(k8sClient.SubResource("scale").Get(ctx, isvc, scale)).To(Succeed())
			scale.Spec.Replicas = 0
			Expect(k8sClient.SubResource("scale").Update(ctx, isvc, client.WithSubResourceBody(scale))).To(Succeed())

			By("verifying spec.replicas is now 0")
			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(*updated.Spec.Replicas).To(Equal(int32(0)))

			By("verifying status.replicas is 0 after reconcile with no endpoints")
			Expect(k8sClient.Delete(ctx, slice)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Replicas).To(Equal(int32(0)))

			By("verifying the /scale subresource reports 0 current replicas too")
			scaleAfter := &autoscalingv1.Scale{}
			Expect(k8sClient.SubResource("scale").Get(ctx, isvc, scaleAfter)).To(Succeed())
			Expect(scaleAfter.Status.Replicas).To(Equal(int32(0)))
		})

		It("should expose a parseable pod selector via the /scale subresource", func() {
			modelName := "model-scale-selector"
			isvcName := "isvc-scale-selector"

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

			scale := &autoscalingv1.Scale{}
			Expect(k8sClient.SubResource("scale").Get(ctx, isvc, scale)).To(Succeed())
			// The reported selector must match the pods the Deployment owns, so
			// the HPA resolves a real target rather than the #1881
			// SelectorRequired failure.
			Expect(scale.Status.Selector).To(Equal(
				labels.SelectorFromSet(dep.Spec.Selector.MatchLabels).String()))
			Expect(scale.Status.Selector).NotTo(BeEmpty())
			_, selectorErr := labels.Parse(scale.Status.Selector)
			Expect(selectorErr).NotTo(HaveOccurred())
		})

		It("should report observed replicas via /scale, not the desired count", func() {
			modelName := "model-scale-observed"
			isvcName := "isvc-scale-observed"

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

			replicas := int32(3)
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
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// Observed total is 2 while only 1 pod is ready and 3 are desired:
			// /scale must expose the observed total, and status.desiredReplicas
			// must still carry the intent.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
			dep.Status.Replicas = 2
			dep.Status.ReadyReplicas = 1
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			scale := &autoscalingv1.Scale{}
			Expect(k8sClient.SubResource("scale").Get(ctx, isvc, scale)).To(Succeed())
			Expect(scale.Spec.Replicas).To(Equal(int32(3)))
			Expect(scale.Status.Replicas).To(Equal(int32(2)))

			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			Expect(updated.Status.Replicas).To(Equal(int32(2)))
			Expect(updated.Status.ReadyReplicas).To(Equal(int32(1)))
			Expect(updated.Status.DesiredReplicas).To(Equal(int32(3)))
		})

		Context("observed replicas on paths that do not inspect the workload", func() {
			// A path that cannot observe the live workload (a fatal spec error, a
			// Model that is not Ready) must carry the last observed replica
			// count rather than zeroing status.replicas. An HPA reads that field
			// as currentReplicas through the /scale subresource, so a transient
			// failure must not make it look like nothing is running.
			DescribeTable("carries the last observed count",
				func(suffix, wantPhase string, trip func(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService)) {
					modelName := "model-observed-" + suffix
					isvcName := "isvc-observed-" + suffix

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

					replicas := int32(3)
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
					}()

					reconciler := &InferenceServiceReconciler{
						Client:             k8sClient,
						Scheme:             k8sClient.Scheme(),
						InitContainerImage: "docker.io/curlimages/curl:8.18.0",
					}
					req := reconcile.Request{
						NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
					}
					_, err := reconciler.Reconcile(ctx, req)
					Expect(err).NotTo(HaveOccurred())

					// Establish a nonzero observed count the way a Deployment
					// controller would: 2 observed with 1 ready of 3 desired.
					dep := &appsv1.Deployment{}
					Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
					dep.Status.Replicas = 2
					dep.Status.ReadyReplicas = 1
					Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
					_, err = reconciler.Reconcile(ctx, req)
					Expect(err).NotTo(HaveOccurred())

					updated := &inferencev1alpha1.InferenceService{}
					Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
					Expect(updated.Status.Replicas).To(Equal(int32(2)),
						"the observed count must be established before the path is tripped")

					// Re-read before the trip: the reconcile above bumped the
					// object's resourceVersion with its status write.
					live := &inferencev1alpha1.InferenceService{}
					Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, live)).To(Succeed())
					trip(model, live)
					Expect(k8sClient.Update(ctx, live)).To(Succeed())
					_, err = reconciler.Reconcile(ctx, req)
					Expect(err).NotTo(HaveOccurred())

					Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
					Expect(updated.Status.Phase).To(Equal(wantPhase))
					Expect(updated.Status.Replicas).To(Equal(int32(2)),
						"a path that cannot observe the workload must carry the last observed count, not zero it")

					scale := &autoscalingv1.Scale{}
					Expect(k8sClient.SubResource("scale").Get(ctx, isvc, scale)).To(Succeed())
					Expect(scale.Status.Replicas).To(Equal(int32(2)),
						"/scale must expose the carried count an HPA reads as currentReplicas")
				},
				Entry("invalid parallelism", "parallelism", PhaseFailed,
					func(_ *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService) {
						isvc.Spec.Resources = &inferencev1alpha1.InferenceResourceRequirements{GPU: 1}
						isvc.Spec.VLLMConfig = &inferencev1alpha1.VLLMConfig{
							TensorParallelSize: ptr.To(int32(2)),
						}
					}),
				Entry("invalid gpuSharing", "gpushing", PhaseFailed,
					func(_ *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService) {
						isvc.Spec.Resources = &inferencev1alpha1.InferenceResourceRequirements{
							GPU:        2,
							GPUSharing: &inferencev1alpha1.GPUSharingSpec{Mode: inferencev1alpha1.GPUSharingModeShared},
						}
					}),
				Entry("model not found", "notfound", PhaseFailed,
					func(_ *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService) {
						isvc.Spec.ModelRef = "missing-model"
					}),
				Entry("model not ready", "notready", "Pending",
					func(model *inferencev1alpha1.Model, _ *inferencev1alpha1.InferenceService) {
						model.Status.Phase = ""
						Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
					}),
			)
		})

		It("should accept manual scaling above 10 replicas", func() {
			modelName := "model-scale-cap"
			isvcName := "isvc-scale-cap"

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

			replicas := int32(11)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
				},
			}
			// The old Maximum=10 rejected this at admission with
			// "spec.replicas ... should be less than or equal to 10".
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
			Expect(*dep.Spec.Replicas).To(Equal(int32(11)))
		})

		// Guards pre-existing single-writer behavior (an external write to the
		// Deployment is reverted), not the #1881 selector/observed change.
		It("should revert an external Deployment replica write without spec.autoscaling", func() {
			modelName := "model-scale-revert"
			isvcName := "isvc-scale-revert"

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
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// An external scaler pointed at the Deployment writes replicas
			// directly; the operator is the single writer and puts its own
			// desired count back on the next reconcile.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
			scaled := int32(5)
			dep.Spec.Replicas = &scaled
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
		})

		// Guards the pre-existing /scale spec-path propagation, not the #1881
		// selector/observed change.
		It("should propagate a /scale write through to the Deployment", func() {
			modelName := "model-scale-propagate"
			isvcName := "isvc-scale-propagate"

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
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("writing 3 replicas through the InferenceService /scale subresource")
			scale := &autoscalingv1.Scale{}
			Expect(k8sClient.SubResource("scale").Get(ctx, isvc, scale)).To(Succeed())
			scale.Spec.Replicas = 3
			Expect(k8sClient.SubResource("scale").Update(ctx, isvc, client.WithSubResourceBody(scale))).To(Succeed())

			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(3)))
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
			model.Status.Path = "/models/abc123def456/model.gguf"
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
				// Default mode is shared: the cluster-wide PVC is created once and
				// may be reused by other specs, so leave it for suite teardown.
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
				ModelCachePath:     "/models",
				// ModelCacheMode left empty: must resolve to the shared default.
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			// In the default shared mode the operator provisions the single
			// cluster-wide "llmkube-model-cache" PVC (no owner reference, since it
			// outlives any one InferenceService), not a per-isvc PVC.
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)).To(Succeed())
			Expect(pvc.OwnerReferences).To(BeEmpty())

			// The PVC the operator provisions must be the one the workload
			// actually mounts, and the cache directory it reads must be the one
			// the Model controller wrote. A PVC nobody mounts, or a directory
			// the two sides disagree on, is the #363 class (#378 finding 18,
			// finding 9).
			By("verifying the Deployment mounts the PVC the operator created")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())

			var mounted bool
			for _, v := range dep.Spec.Template.Spec.Volumes {
				if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == ModelCachePVCName {
					mounted = true
				}
			}
			Expect(mounted).To(BeTrue(), "the workload must mount the %s PVC the operator created", ModelCachePVCName)

			By("verifying the workload cache directory agrees with the Model controller path")
			var downloader *corev1.Container
			for i := range dep.Spec.Template.Spec.InitContainers {
				if dep.Spec.Template.Spec.InitContainers[i].Name == "model-downloader" {
					downloader = &dep.Spec.Template.Spec.InitContainers[i]
				}
			}
			Expect(downloader).NotTo(BeNil())
			cacheDir := getEnvVar(downloader.Env, "CACHE_DIR")
			modelPath := getEnvVar(downloader.Env, "MODEL_PATH")
			Expect(cacheDir).NotTo(BeEmpty())
			Expect(filepath.Dir(modelPath)).To(Equal(cacheDir))
			Expect(filepath.Base(cacheDir)).To(Equal(filepath.Base(filepath.Dir(model.Status.Path))),
				"the workload cache directory must be the one the Model controller persisted (#363 round-trip)")
		})

		It("should preserve agent-written schedulingStatus on status update", func() {
			modelName := "model-sched-preserve"
			isvcName := "isvc-sched-preserve"

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

			// Simulate the metal-agent writing a scheduling rejection.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, isvc)).To(Succeed())
			isvc.Status.SchedulingStatus = "MemoryCheckFailed"
			isvc.Status.SchedulingMessage = "host memory insufficient for model"
			Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())

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
			// The controller must not clobber the agent-written scheduling fields.
			Expect(updated.Status.SchedulingStatus).To(Equal("MemoryCheckFailed"))
			Expect(updated.Status.SchedulingMessage).To(Equal("host memory insufficient for model"))
		})

		It("keeps reporting Progressing while one replica is replaced and the replacement is Pending on the GPU", func() {
			// A multi-replica GPU service mid-rollout: one replica still serves,
			// and the replacement cannot schedule because the outgoing pod has
			// not released its GPU yet. That is a reduced-capacity rollout, not
			// a service queued for GPU (#2131), so the phase must stay
			// Progressing and must not advertise WaitingForGPU.
			modelName := "model-sched-rollout-pending"
			isvcName := "isvc-sched-rollout-pending"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cuda",
						GPU:         &inferencev1alpha1.GPUSpec{Enabled: true, Count: 4, Vendor: "nvidia", Layers: -1},
					},
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
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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
				pods := &corev1.PodList{}
				_ = k8sClient.List(ctx, pods, client.InNamespace("default"),
					client.MatchingLabels{"inference.llmkube.dev/service": isvcName})
				for i := range pods.Items {
					_ = k8sClient.Delete(ctx, &pods.Items[i])
				}
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			key := types.NamespacedName{Name: isvcName, Namespace: "default"}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// envtest runs no kubelet, so drive the Deployment's counts by hand:
			// one of two replicas Ready, the other still being replaced.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			dep.Status.Replicas = 2
			dep.Status.ReadyReplicas = 1
			dep.Status.AvailableReplicas = 1
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			// The replacement pod, unschedulable because the outgoing pod has
			// not released its GPUs yet.
			pending := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pending-replacement",
					Namespace: "default",
					Labels: map[string]string{
						"app":                           isvcName,
						"inference.llmkube.dev/service": isvcName,
					},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "busybox"}}},
			}
			Expect(k8sClient.Create(ctx, pending)).To(Succeed())
			pending.Status.Phase = corev1.PodPending
			pending.Status.Conditions = []corev1.PodCondition{{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  corev1.PodReasonUnschedulable,
				Message: "0/2 nodes are available: 2 Insufficient nvidia.com/gpu.",
			}}
			Expect(k8sClient.Status().Update(ctx, pending)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			got := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal("Progressing"),
				"a replica still serving means the rollout is Progressing, not queued for GPU")
			Expect(got.Status.SchedulingStatus).To(BeEmpty(),
				"a replacement pod waiting on the GPU it is replacing must not put the service in the GPU queue")
			Expect(got.Status.WaitingFor).To(BeEmpty())
		})

		It("reports WaitingForGPU when no replica serves and the replacement is Pending on the GPU", func() {
			// The other half of the rollout phase rule: with nothing serving,
			// the scheduling probe decides, so a replacement waiting on the GPUs
			// its predecessor still holds reads as queued. At replicas 1 that is
			// the normal state mid-rollout (#2131).
			modelName := "model-sched-rollout-zero-ready"
			isvcName := "isvc-sched-rollout-zero-ready"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cuda",
						GPU:         &inferencev1alpha1.GPUSpec{Enabled: true, Count: 4, Vendor: "nvidia", Layers: -1},
					},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
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
				pods := &corev1.PodList{}
				_ = k8sClient.List(ctx, pods, client.InNamespace("default"),
					client.MatchingLabels{"inference.llmkube.dev/service": isvcName})
				for i := range pods.Items {
					_ = k8sClient.Delete(ctx, &pods.Items[i])
				}
			}()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			key := types.NamespacedName{Name: isvcName, Namespace: "default"}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// The only pod is being replaced: nothing Ready.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			dep.Status.Replicas = 1
			dep.Status.ReadyReplicas = 0
			dep.Status.AvailableReplicas = 0
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			pending := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pending-replacement-zero-ready",
					Namespace: "default",
					Labels: map[string]string{
						"app":                           isvcName,
						"inference.llmkube.dev/service": isvcName,
					},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "busybox"}}},
			}
			Expect(k8sClient.Create(ctx, pending)).To(Succeed())
			pending.Status.Phase = corev1.PodPending
			pending.Status.Conditions = []corev1.PodCondition{{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  corev1.PodReasonUnschedulable,
				Message: "0/2 nodes are available: 2 Insufficient nvidia.com/gpu.",
			}}
			Expect(k8sClient.Status().Update(ctx, pending)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			got := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(PhaseWaitingForGPU),
				"with nothing serving, a Pending replacement is all there is to report")
			Expect(got.Status.SchedulingStatus).To(Equal("InsufficientGPU"))
		})

		It("should clear the controller's own schedulingStatus once the service is Ready", func() {
			modelName := "model-sched-clear"
			isvcName := "isvc-sched-clear"

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
			key := types.NamespacedName{Name: isvcName, Namespace: "default"}

			// First pass creates the Deployment.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// envtest runs no kubelet, so drive the Deployment to Ready by hand.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			dep.Status.Replicas = 1
			dep.Status.ReadyReplicas = 1
			dep.Status.AvailableReplicas = 1
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			// Seed the diagnosis the controller itself writes on the deployment
			// path, as if the pods had been unschedulable before recovering.
			Expect(k8sClient.Get(ctx, key, isvc)).To(Succeed())
			isvc.Status.SchedulingStatus = "InsufficientGPU"
			isvc.Status.SchedulingMessage = "0/4 nodes are available: 1 Insufficient nvidia.com/gpu"
			isvc.Status.WaitingFor = "nvidia.com/gpu: 1"
			Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			recovered := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, key, recovered)).To(Succeed())
			Expect(recovered.Status.Phase).To(Equal(PhaseReady))
			// A serving service must not advertise a resource it is no longer
			// waiting on (#1632).
			Expect(recovered.Status.SchedulingStatus).To(BeEmpty())
			Expect(recovered.Status.SchedulingMessage).To(BeEmpty())
			Expect(recovered.Status.WaitingFor).To(BeEmpty())
		})

		It("should preserve agent-written schedulingStatus on a Ready metal service", func() {
			modelName := "model-sched-metal-ready"
			isvcName := "isvc-sched-metal-ready"

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/metal-model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

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
			defer func() { _ = k8sClient.Delete(ctx, isvc) }()

			// A fresh heartbeat with a ready endpoint puts the metal service at Ready.
			slice := metalEndpoints(isvcName, time.Now().UTC().Format(time.RFC3339))
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, slice) }()

			key := types.NamespacedName{Name: isvcName, Namespace: "default"}
			Expect(k8sClient.Get(ctx, key, isvc)).To(Succeed())
			isvc.Status.SchedulingStatus = "InsufficientMemory"
			isvc.Status.SchedulingMessage = "model exceeds the host memory budget"
			Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			updatedMetal := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, key, updatedMetal)).To(Succeed())
			// The agent owns these on the metal path and clears them itself
			// (#777); the #1632 clear must not reach across and do it here.
			Expect(updatedMetal.Status.SchedulingStatus).To(Equal("InsufficientMemory"))
			Expect(updatedMetal.Status.SchedulingMessage).To(Equal("model exceeds the host memory budget"))
		})
	})
})
