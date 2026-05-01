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
	"k8s.io/apimachinery/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

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
