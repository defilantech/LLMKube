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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("RolloutPolicy drain-before-rollout", func() {
	ctx := context.Background()

	BeforeEach(func() {
		// Cleanup any leftover resources from previous tests
	})

	Context("when RolloutPolicy is not set", func() {
		It("should proceed with deployment update normally", func() {
			modelName := "model-no-policy"
			isvcName := "isvc-no-policy"

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
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())

			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			cond := findRolloutDeferredCondition(updated.Status.Conditions)
			Expect(cond).To(BeNil())
		})
	})

	Context("when RolloutPolicy.waitForIdle=true and pods are idle", func() {
		var testServer *httptest.Server

		BeforeEach(func() {
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/slots" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`[{"slot_id":0,"processing_mode":"idle","current_user":null}]`))
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
		})

		AfterEach(func() {
			testServer.Close()
		})

		It("should proceed with deployment update when slots are idle", func() {
			modelName := "model-idle-pods"
			isvcName := "isvc-idle-pods"

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
					RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
						WaitForIdle:        true,
						IdleTimeoutSeconds: 30,
						Force:              false,
					},
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
				RolloutIdleBaseURL: testServer.URL,
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())
		})
	})

	Context("when RolloutPolicy.waitForIdle=true and force=true", func() {
		It("should proceed with deployment update regardless of idleness", func() {
			modelName := "model-force"
			isvcName := "isvc-force"

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
					RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
						WaitForIdle:        true,
						IdleTimeoutSeconds: 30,
						Force:              true,
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, isvc)
				dep := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep); err == nil {
					_ = k8sClient.Delete(ctx, dep)
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
		})

		It("should clear stale RolloutDeferred when force is enabled", func() {
			modelName := "model-force-clear"
			isvcName := "isvc-force-clear"

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
					RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
						WaitForIdle:        true,
						IdleTimeoutSeconds: 30,
						Force:              true,
					},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{
					Conditions: []metav1.Condition{
						{
							Type:               ConditionRolloutDeferred,
							Status:             metav1.ConditionTrue,
							ObservedGeneration: 1,
							LastTransitionTime: metav1.Now(),
							Reason:             ReasonPodsBusy,
							Message:            "stale condition from previous defer",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, isvc)
				dep := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep); err == nil {
					_ = k8sClient.Delete(ctx, dep)
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

			// Verify stale RolloutDeferred was cleared
			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			cond := findRolloutDeferredCondition(updated.Status.Conditions)
			Expect(cond).To(BeNil())
		})
	})

	Context("template-change gate", func() {
		var testServer *httptest.Server
		var idleCheckCalled int32
		var mu sync.Mutex

		BeforeEach(func() {
			idleCheckCalled = 0
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() {
					mu.Lock()
					idleCheckCalled++
					mu.Unlock()
				}()
				if r.URL.Path == "/slots" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`[{"slot_id":0,"processing_mode":"idle","current_user":null}]`))
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
		})

		AfterEach(func() {
			testServer.Close()
		})

		It("should skip idle check when pod template has not changed", func() {
			modelName := "model-no-change"
			isvcName := "isvc-no-change"

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
					RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
						WaitForIdle:        true,
						IdleTimeoutSeconds: 30,
						Force:              false,
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, isvc) }()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
				RolloutIdleBaseURL: testServer.URL,
			}

			// First reconcile: creates the deployment (no update needed)
			result1, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result1.RequeueAfter).To(BeZero())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())

			mu.Lock()
			callsAfterFirst := idleCheckCalled
			mu.Unlock()
			// First reconcile creates the deployment; no template update needed, so no idle check.
			Expect(callsAfterFirst).To(Equal(int32(0)))

			// Second reconcile: InferenceService spec unchanged, template should not
			// differ after normalization. No idle check should be needed.
			result2, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result2.RequeueAfter).To(BeZero())

			mu.Lock()
			callsAfterSecond := idleCheckCalled
			mu.Unlock()
			// podTemplatesDiffer should report no change for an unchanged InferenceService,
			// so no idle check should fire on second reconcile.
			Expect(callsAfterSecond).To(Equal(callsAfterFirst))
		})

		It("should call idle check when pod template changes", func() {
			modelName := "model-with-change"
			isvcName := "isvc-with-change"

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
					RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
						WaitForIdle:        true,
						IdleTimeoutSeconds: 30,
						Force:              false,
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, isvc) }()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
				RolloutIdleBaseURL: testServer.URL,
			}

			// First reconcile: creates the deployment
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())

			// Update the image — this triggers a template change
			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			updated.Spec.Image = "ghcr.io/ggml-org/llama.cpp:server-v2"
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())

			// Second reconcile: template changed, idle check should fire
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			mu.Lock()
			calls := idleCheckCalled
			mu.Unlock()
			Expect(calls).To(BeNumerically(">", 0))
		})
	})

	Context("busy-defer behavior", func() {
		var testServer *httptest.Server
		var slotsState string
		var mu sync.Mutex

		BeforeEach(func() {
			slotsState = "idle"
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/slots" {
					mu.Lock()
					state := slotsState
					mu.Unlock()

					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					switch state {
					case "busy":
						_, _ = w.Write([]byte(`[{"slot_id":0,"processing_mode":"inferencing","current_user":{"name":"test"}}]`))
					default:
						_, _ = w.Write([]byte(`[{"slot_id":0,"processing_mode":"idle","current_user":null}]`))
					}
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
		})

		AfterEach(func() {
			testServer.Close()
		})

		It("should defer rollout when busy and template changed", func() {
			modelName := "model-busy-defer"
			isvcName := "isvc-busy-defer"

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
					RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
						WaitForIdle:        true,
						IdleTimeoutSeconds: 30,
						Force:              false,
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, isvc) }()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
				RolloutIdleBaseURL: testServer.URL,
			}

			// First reconcile: creates the deployment
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())

			// Make slots busy
			mu.Lock()
			slotsState = "busy"
			mu.Unlock()

			// Update the image to trigger a template change
			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			updated.Spec.Image = "ghcr.io/ggml-org/llama.cpp:server-v2"
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())

			// Second reconcile: template changed + busy = defer
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Verify RolloutDeferred condition is set
			deferredISVC := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, deferredISVC)).To(Succeed())
			cond := findRolloutDeferredCondition(deferredISVC.Status.Conditions)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal(ReasonPodsBusy))

			// Verify deployment was NOT updated (image should still be old)
			notUpdated := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, notUpdated)).To(Succeed())
			Expect(notUpdated.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/ggml-org/llama.cpp:server"))

			// Make slots idle
			mu.Lock()
			slotsState = "idle"
			mu.Unlock()

			// Third reconcile: template still changed + idle = proceed
			result, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			// Verify deployment WAS updated
			finalDep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, finalDep)).To(Succeed())
			Expect(finalDep.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/ggml-org/llama.cpp:server-v2"))

			// Verify RolloutDeferred condition is cleared
			finalISVC := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, finalISVC)).To(Succeed())
			cond = findRolloutDeferredCondition(finalISVC.Status.Conditions)
			Expect(cond).To(BeNil())
		})

		It("should skip idle check on second reconcile when template no longer differs", func() {
			modelName := "model-busy-skip"
			isvcName := "isvc-busy-skip"

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
					RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
						WaitForIdle:        true,
						IdleTimeoutSeconds: 30,
						Force:              false,
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, isvc) }()

			var idleCheckCount int32
			countingTransport := &countingRoundTripper{
				next:  http.DefaultTransport,
				count: &idleCheckCount,
			}
			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
				RolloutIdleBaseURL: testServer.URL,
				HTTPClient:         &http.Client{Transport: countingTransport, Timeout: 5},
			}

			// First reconcile: creates the deployment
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())

			countBeforeSecond := idleCheckCount

			// Second reconcile: InferenceService spec unchanged, podTemplatesDiffer
			// should report no change after normalization. No idle check needed.
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())
			// No new idle checks on unchanged second reconcile.
			Expect(idleCheckCount).To(Equal(countBeforeSecond))
		})

		It("should clear stale RolloutDeferred when policy is disabled", func() {
			modelName := "model-policy-disabled"
			isvcName := "isvc-policy-disabled"

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
					RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
						WaitForIdle: false,
					},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{
					Conditions: []metav1.Condition{
						{
							Type:               ConditionRolloutDeferred,
							Status:             metav1.ConditionTrue,
							ObservedGeneration: 1,
							LastTransitionTime: metav1.Now(),
							Reason:             ReasonPodsBusy,
							Message:            "stale condition from previous defer",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, isvc) }()

			reconciler := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify stale RolloutDeferred was cleared when policy disabled
			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
			cond := findRolloutDeferredCondition(updated.Status.Conditions)
			Expect(cond).To(BeNil())
		})
	})

	Context("rolloutPolicyEnabled helper", func() {
		It("should return false when RolloutPolicy is nil", func() {
			isvc := &inferencev1alpha1.InferenceService{}
			Expect(isvc.RolloutPolicyEnabled()).To(BeFalse())
		})

		It("should return false when waitForIdle is false", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
						WaitForIdle: false,
					},
				},
			}
			Expect(isvc.RolloutPolicyEnabled()).To(BeFalse())
		})

		It("should return true when waitForIdle is true", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
						WaitForIdle: true,
					},
				},
			}
			Expect(isvc.RolloutPolicyEnabled()).To(BeTrue())
		})

		It("should return false when force is true", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
						WaitForIdle: true,
						Force:       true,
					},
				},
			}
			Expect(isvc.ShouldDeferRollout()).To(BeFalse())
		})

		It("should return true when waitForIdle and not force", func() {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
						WaitForIdle: true,
						Force:       false,
					},
				},
			}
			Expect(isvc.ShouldDeferRollout()).To(BeTrue())
		})
	})
})

var _ = Describe("checkServerIdle", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("should return true when all slots are idle", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"slot_id":0,"processing_mode":"idle","current_user":null},{"slot_id":1,"processing_mode":"idle","current_user":null}]`))
		}))
		defer server.Close()

		idle, err := reconciler.checkServerIdle(context.Background(), server.URL)
		Expect(err).NotTo(HaveOccurred())
		Expect(idle).To(BeTrue())
	})

	It("should return false when any slot is busy", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"slot_id":0,"processing_mode":"inferencing","current_user":{"name":"test"}}]`))
		}))
		defer server.Close()

		idle, err := reconciler.checkServerIdle(context.Background(), server.URL)
		Expect(err).NotTo(HaveOccurred())
		Expect(idle).To(BeFalse())
	})

	It("should return false when slot is in loading mode", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"slot_id":0,"processing_mode":"loading"}]`))
		}))
		defer server.Close()

		idle, err := reconciler.checkServerIdle(context.Background(), server.URL)
		Expect(err).NotTo(HaveOccurred())
		Expect(idle).To(BeFalse())
	})

	It("should return error when server is unreachable", func() {
		idle, err := reconciler.checkServerIdle(context.Background(), "http://192.0.2.1:9999")
		Expect(err).To(HaveOccurred())
		Expect(idle).To(BeFalse())
	})

	It("should return error when /slots returns non-200", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		idle, err := reconciler.checkServerIdle(context.Background(), server.URL)
		Expect(err).To(HaveOccurred())
		Expect(idle).To(BeFalse())
	})

	It("should use injected HTTPClient when set", func() {
		var requestCaptured bool
		customTransport := &captureRoundTripper{
			capture: func(req *http.Request) *http.Response {
				requestCaptured = true
				body := `[{"slot_id":0,"processing_mode":"idle","current_user":null}]`
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}
			},
		}

		reconcilerWithClient := &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			HTTPClient: &http.Client{
				Transport: customTransport,
				Timeout:   5 * time.Second,
			},
		}

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Fail("default server should not be called when HTTPClient is set")
		}))
		defer server.Close()

		idle, err := reconcilerWithClient.checkServerIdle(context.Background(), server.URL)
		Expect(err).NotTo(HaveOccurred())
		Expect(idle).To(BeTrue())
		Expect(requestCaptured).To(BeTrue())
	})
})

var _ = Describe("RolloutPolicy envtest integration", func() {
	ctx := context.Background()

	It("should proceed with rollout when idle check fails (no live backend)", func() {
		modelName := "model-deferred"
		isvcName := "isvc-deferred"

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
				RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
					WaitForIdle:        true,
					IdleTimeoutSeconds: 30,
					Force:              false,
				},
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

		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		// On first reconcile, deployment is created (no update needed), so no idle check
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())

		updated := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
		Expect(result.RequeueAfter).To(BeZero())
	})
})

type countingRoundTripper struct {
	next  http.RoundTripper
	count *int32
}

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/slots" {
		(*c.count)++
	}
	return c.next.RoundTrip(req)
}

type captureRoundTripper struct {
	capture func(*http.Request) *http.Response
}

func (c *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return c.capture(req), nil
}

func findRolloutDeferredCondition(conditions []metav1.Condition) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == ConditionRolloutDeferred {
			return &conditions[i]
		}
	}
	return nil
}
