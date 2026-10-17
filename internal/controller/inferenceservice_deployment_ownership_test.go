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
	"maps"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// generatedDeploymentSelector is the selector constructDeployment writes; a
// foreign fixture carrying it lands on the in-place update path, one without
// it lands on the selector-migration recreate path.
func generatedDeploymentSelector(name string) map[string]string {
	return map[string]string{
		"app":                           name,
		"inference.llmkube.dev/service": name,
	}
}

// foreignISVCDeployment builds a Deployment holding the generated name that
// this suite's InferenceService did not create.
func foreignISVCDeployment(name string, selector map[string]string) *appsv1.Deployment {
	replicas := int32(2)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{"foreign": "true"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selector},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "someone-elses-server",
					Image: "registry.example.com/someone-else:latest",
				}}},
			},
		},
	}
}

// setupDeploymentOwnershipFixture creates a Ready Model and an InferenceService
// named name, mirroring the reconcileDeployment envtest fixtures, and returns
// a reconciler for it.
func setupDeploymentOwnershipFixture(ctx context.Context, name string) *InferenceServiceReconciler {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/model.gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: "cpu",
			},
		},
	}
	Expect(k8sClient.Create(ctx, model)).To(Succeed())
	model.Status.Phase = PhaseReady
	Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

	replicas := int32(1)
	Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: model.Name,
			Replicas: &replicas,
			Image:    "ghcr.io/ggml-org/llama.cpp:server",
		},
	})).To(Succeed())

	return &InferenceServiceReconciler{
		Client:             k8sClient,
		Scheme:             k8sClient.Scheme(),
		InitContainerImage: "docker.io/curlimages/curl:8.18.0",
	}
}

// cleanupDeploymentOwnershipFixture removes everything a spec created; the
// ignored NotFound errors keep it safe when the reconciler never created the
// workload or a previous step already removed it.
func cleanupDeploymentOwnershipFixture(ctx context.Context, name string) {
	_ = k8sClient.Delete(ctx, &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
	_ = k8sClient.Delete(ctx, &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"}})
	_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
	_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sanitizeDNSName(name), Namespace: "default"}})
}

// reconcileDeploymentOwnershipFixture runs one reconcile of the named service.
func reconcileDeploymentOwnershipFixture(ctx context.Context, r *InferenceServiceReconciler, name string) (reconcile.Result, error) {
	return r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
	})
}

// getDeploymentOwnershipISVC reads the service status back after a reconcile.
func getDeploymentOwnershipISVC(ctx context.Context, name string) *inferencev1alpha1.InferenceService {
	isvc := &inferencev1alpha1.InferenceService{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, isvc)).To(Succeed())
	return isvc
}

// expectDeploymentNameTaken asserts the refusal on the service status: the
// Failed phase, the dedicated scheduling marker, and Degraded=True naming it.
func expectDeploymentNameTaken(ctx context.Context, name string) {
	isvc := getDeploymentOwnershipISVC(ctx, name)
	Expect(isvc.Status.Phase).To(Equal(PhaseFailed))
	Expect(isvc.Status.SchedulingStatus).To(Equal(inferencev1alpha1.ReasonDeploymentNameTaken))
	Expect(isvc.Status.SchedulingMessage).To(ContainSubstring("Deployment"))
	Expect(isvc.Status.SchedulingMessage).To(ContainSubstring(name))
	cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDegraded)
	Expect(cond).NotTo(BeNil())
	Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	// The Failed phase drives the Degraded reason; the dedicated
	// DeploymentNameTaken reason rides SchedulingStatus and the event.
	Expect(cond.Reason).To(Equal(PhaseFailed))
}

var _ = Describe("InferenceService Deployment ownership (#2011)", func() {
	Context("refuses a foreign same-name Deployment", func() {
		ctx := context.Background()

		It("refuses to overwrite it when the selector matches", func() {
			name := "dep-foreign-update"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			recorder := events.NewFakeRecorder(20)
			r.Recorder = recorder

			foreign := foreignISVCDeployment(name, generatedDeploymentSelector(name))
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
			seeded := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, seeded)).To(Succeed())
			foreignRV := seeded.ResourceVersion

			result, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))
			Expect(drainEvents(recorder)).To(ConsistOf(HavePrefix("Warning " + inferencev1alpha1.ReasonDeploymentNameTaken)))

			// The foreign Deployment is untouched: spec, labels, owner
			// references, and resourceVersion (no write of any kind).
			got := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			Expect(got.ResourceVersion).To(Equal(foreignRV))
			Expect(got.Spec.Template.Spec.Containers[0].Image).To(Equal("registry.example.com/someone-else:latest"))
			Expect(*got.Spec.Replicas).To(Equal(int32(2)))
			Expect(got.Labels).To(HaveKeyWithValue("foreign", "true"))
			Expect(got.OwnerReferences).To(BeEmpty())

			expectDeploymentNameTaken(ctx, name)

			// The sequence stops at the refusal: nothing after the Deployment
			// step is provisioned.
			svcErr := k8sClient.Get(ctx, types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, &corev1.Service{})
			Expect(svcErr).To(HaveOccurred())
			Expect(apierrors.IsNotFound(svcErr)).To(BeTrue())

			// One Warning per episode: a requeue that finds the same
			// collision updates nothing and emits nothing.
			_, err = reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(drainEvents(recorder)).To(BeEmpty())
		})

		It("refuses to delete-and-recreate it when the selector differs", func() {
			name := "dep-foreign-migrate"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			oldSelector := map[string]string{"app": name}
			foreign := foreignISVCDeployment(name, oldSelector)
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			result, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))

			got := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			Expect(got.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app", name))
			Expect(got.Spec.Selector.MatchLabels).NotTo(HaveKey("inference.llmkube.dev/service"))
			Expect(got.Spec.Template.Spec.Containers[0].Image).To(Equal("registry.example.com/someone-else:latest"))
			Expect(got.OwnerReferences).To(BeEmpty())

			expectDeploymentNameTaken(ctx, name)
		})

		It("names the foreign InferenceService that controls it", func() {
			name := "dep-foreign-isvc"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			controller := true
			foreign := foreignISVCDeployment(name, generatedDeploymentSelector(name))
			foreign.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: inferencev1alpha1.GroupVersion.String(),
				Kind:       "InferenceService",
				Name:       "other-isvc",
				UID:        "uid-other",
				Controller: &controller,
			}}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			isvc := getDeploymentOwnershipISVC(ctx, name)
			Expect(isvc.Status.SchedulingMessage).To(ContainSubstring("another InferenceService (uid uid-other)"))

			got := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			Expect(got.OwnerReferences).To(HaveLen(1))
			Expect(got.OwnerReferences[0].Name).To(Equal("other-isvc"))
		})

		It("routes a create-time name race into the refusal", func() {
			name := "dep-create-race"
			scheme := k8sClient.Scheme()

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
				Status: inferencev1alpha1.ModelStatus{Phase: PhaseReady},
			}
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: model.Name,
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
				},
			}
			foreign := foreignISVCDeployment(name, generatedDeploymentSelector(name))

			// The race: the gate's Get runs before the foreign object exists
			// (hidden by the interceptor), the Create loses to it, and the
			// re-Get finds it. Without the race branch the reconcile wedges
			// on a generic create error that no requeue clears.
			deploymentGets := 0
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(model, isvc, foreign).
				WithStatusSubresource(&inferencev1alpha1.InferenceService{}, &inferencev1alpha1.Model{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ictx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*appsv1.Deployment); ok {
							deploymentGets++
							if deploymentGets == 1 {
								return apierrors.NewNotFound(appsv1.Resource("deployments"), key.Name)
							}
						}
						return cl.Get(ictx, key, obj, opts...)
					},
					Create: func(ictx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						if _, ok := obj.(*appsv1.Deployment); ok {
							return apierrors.NewAlreadyExists(appsv1.Resource("deployments"), obj.GetName())
						}
						return cl.Create(ictx, obj, opts...)
					},
				}).
				Build()
			r := &InferenceServiceReconciler{
				Client:             c,
				Scheme:             scheme,
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			dep, _, _, result, err := r.reconcileDeployment(context.Background(), isvc, model, nil, 1, true, false)
			Expect(err).NotTo(HaveOccurred(), "the refusal is not a reconcile error")
			Expect(dep).To(BeNil())
			Expect(result).NotTo(BeNil())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))

			got := &inferencev1alpha1.InferenceService{}
			Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			Expect(got.Status.SchedulingStatus).To(Equal(inferencev1alpha1.ReasonDeploymentNameTaken))
			Expect(got.Status.Phase).To(Equal(PhaseFailed))

			survivor := &appsv1.Deployment{}
			Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, survivor)).To(Succeed())
			Expect(survivor.OwnerReferences).To(BeEmpty())
			Expect(survivor.Spec.Template.Spec.Containers[0].Image).To(Equal("registry.example.com/someone-else:latest"))
		})

		It("removes the operator-owned HPA scaling a refused name", func() {
			name := "dep-foreign-hpa"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cpu",
					},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
			replicas := int32(1)
			Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:    model.Name,
					Replicas:    &replicas,
					Image:       "ghcr.io/ggml-org/llama.cpp:server",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(1), MaxReplicas: 4},
				},
			})).To(Succeed())
			defer cleanupDeploymentOwnershipFixture(ctx, name)
			r := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			// The first pass provisions the Deployment, the Service, and the
			// operator-owned HPA.
			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			hpaNN := types.NamespacedName{Name: name, Namespace: "default"}
			hpa := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, hpaNN, hpa)).To(Succeed())
			Expect(hpa.OwnerReferences).To(HaveLen(1))
			cond := meta.FindStatusCondition(getDeploymentOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(ReasonHPAManaged))

			// The name changes hands under a serving, autoscaled service.
			owned := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, hpaNN, owned)).To(Succeed())
			Expect(k8sClient.Delete(ctx, owned)).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, hpaNN, &appsv1.Deployment{}))
			}, "10s", "100ms").Should(BeTrue())
			foreign := foreignISVCDeployment(name, generatedDeploymentSelector(name))
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			result, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))

			// The HPA must not keep scaling the foreign object the operator
			// just refused to touch: the refusal removes the operator-owned
			// HPA and reports the removal on AutoscalingReady=False.
			err = k8sClient.Get(ctx, hpaNN, &autoscalingv2.HorizontalPodAutoscaler{})
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			cond = meta.FindStatusCondition(getDeploymentOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonDeploymentNameTaken))
			expectDeploymentNameTaken(ctx, name)

			// Recovery: the name frees, the refusal lifts, and the HPA comes
			// back with the condition returning to managed.
			Expect(k8sClient.Delete(ctx, foreign)).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, hpaNN, &appsv1.Deployment{}))
			}, "10s", "100ms").Should(BeTrue())
			_, err = reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			returned := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, hpaNN, returned)).To(Succeed())
			Expect(returned.OwnerReferences).To(HaveLen(1))
			Expect(*returned.OwnerReferences[0].Controller).To(BeTrue())
			cond = meta.FindStatusCondition(getDeploymentOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal(ReasonHPAManaged))
		})

		It("reports an unreadable HPA as HPAStateUnknown while refusing", func() {
			name := "dep-hpa-get-fails"
			scheme := k8sClient.Scheme()

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
				Status: inferencev1alpha1.ModelStatus{Phase: PhaseReady},
			}
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:    model.Name,
					Replicas:    &replicas,
					Image:       "ghcr.io/ggml-org/llama.cpp:server",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(1), MaxReplicas: 4},
				},
				Status: inferencev1alpha1.InferenceServiceStatus{
					Conditions: []metav1.Condition{{
						Type:               ConditionAutoscalingReady,
						Status:             metav1.ConditionTrue,
						ObservedGeneration: 1,
						Reason:             ReasonHPAManaged,
						Message:            "from a previous pass",
					}},
				},
			}
			foreign := foreignISVCDeployment(name, generatedDeploymentSelector(name))

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(model, isvc, foreign).
				WithStatusSubresource(&inferencev1alpha1.InferenceService{}, &inferencev1alpha1.Model{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ictx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler); ok {
							return fmt.Errorf("injected HPA get failure")
						}
						return cl.Get(ictx, key, obj, opts...)
					},
				}).
				Build()
			r := &InferenceServiceReconciler{
				Client:             c,
				Scheme:             scheme,
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			_, _, _, result, err := r.reconcileDeployment(context.Background(), isvc, model, nil, 1, true, false)
			Expect(err).NotTo(HaveOccurred(), "the refusal is not a reconcile error")
			Expect(result).NotTo(BeNil())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))

			got := &inferencev1alpha1.InferenceService{}
			Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			Expect(got.Status.SchedulingStatus).To(Equal(inferencev1alpha1.ReasonDeploymentNameTaken))
			cond := meta.FindStatusCondition(got.Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonHPAStateUnknown),
				"a prior True/HPAManaged must not stand when the HPA cannot be read")
		})

		It("corrects HPAStateUnknown to HPANameTaken once the HPA reads as foreign", func() {
			name := "dep-hpa-unknown-then-foreign"
			scheme := k8sClient.Scheme()

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
				Status: inferencev1alpha1.ModelStatus{Phase: PhaseReady},
			}
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:    model.Name,
					Replicas:    &replicas,
					Image:       "ghcr.io/ggml-org/llama.cpp:server",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(1), MaxReplicas: 4},
				},
			}
			foreign := foreignISVCDeployment(name, generatedDeploymentSelector(name))
			foreignHPAObj := foreignHPA(name)

			// Pass 1: the HPA Get fails, so the state can only be reported as
			// unknown. Pass 2: the read succeeds and the HPA is foreign, so
			// the unknown reason must be corrected to HPANameTaken.
			hpaGets := 0
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(model, isvc, foreign, foreignHPAObj).
				WithStatusSubresource(&inferencev1alpha1.InferenceService{}, &inferencev1alpha1.Model{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ictx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler); ok {
							hpaGets++
							if hpaGets == 1 {
								return fmt.Errorf("injected HPA get failure")
							}
						}
						return cl.Get(ictx, key, obj, opts...)
					},
				}).
				Build()
			r := &InferenceServiceReconciler{
				Client:             c,
				Scheme:             scheme,
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			_, _, _, result, err := r.reconcileDeployment(context.Background(), isvc, model, nil, 1, true, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))
			got := &inferencev1alpha1.InferenceService{}
			Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			cond := meta.FindStatusCondition(got.Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(ReasonHPAStateUnknown))

			_, _, _, result, err = r.reconcileDeployment(context.Background(), isvc, model, nil, 1, true, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))
			Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			cond = meta.FindStatusCondition(got.Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(ReasonHPANameTaken),
				"a known-foreign HPA must replace the unknown state")

			survivor := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, survivor)).To(Succeed())
			Expect(survivor.OwnerReferences).To(BeEmpty())
		})

		It("still refuses when the owned HPA's delete is rejected", func() {
			name := "dep-hpa-delete-fails"
			scheme := k8sClient.Scheme()

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
				Status: inferencev1alpha1.ModelStatus{Phase: PhaseReady},
			}
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: "uid-hpa-fails"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:    model.Name,
					Replicas:    &replicas,
					Image:       "ghcr.io/ggml-org/llama.cpp:server",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(1), MaxReplicas: 4},
				},
			}
			foreign := foreignISVCDeployment(name, generatedDeploymentSelector(name))
			hpa := &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: name},
					MaxReplicas:    4,
				},
			}
			Expect(setControllerReferenceUnblocked(isvc, hpa, scheme)).To(Succeed())

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(model, isvc, foreign, hpa).
				WithStatusSubresource(&inferencev1alpha1.InferenceService{}, &inferencev1alpha1.Model{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Delete: func(ictx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if _, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler); ok {
							return fmt.Errorf("injected HPA delete failure")
						}
						return cl.Delete(ictx, obj, opts...)
					},
				}).
				Build()
			r := &InferenceServiceReconciler{
				Client:             c,
				Scheme:             scheme,
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			dep, _, _, result, err := r.reconcileDeployment(context.Background(), isvc, model, nil, 1, true, false)
			Expect(err).NotTo(HaveOccurred(), "a failed HPA delete must not abort the refusal")
			Expect(dep).To(BeNil())
			Expect(result).NotTo(BeNil())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))

			got := &inferencev1alpha1.InferenceService{}
			Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			Expect(got.Status.SchedulingStatus).To(Equal(inferencev1alpha1.ReasonDeploymentNameTaken))
			Expect(got.Status.Phase).To(Equal(PhaseFailed))

			// The delete failed, so the HPA is still there and its condition
			// is left for the next pass, which retries the delete.
			survivor := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, survivor)).To(Succeed())
		})

		It("leaves a foreign same-name HPA alone while refusing the Deployment", func() {
			name := "dep-foreign-hpa-untouched"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cpu",
					},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
			replicas := int32(1)
			Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:    model.Name,
					Replicas:    &replicas,
					Image:       "ghcr.io/ggml-org/llama.cpp:server",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(1), MaxReplicas: 4},
				},
			})).To(Succeed())
			defer cleanupDeploymentOwnershipFixture(ctx, name)
			r := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			// First refusal with no HPA at the name: AutoscalingReady reports
			// the removed HPA.
			Expect(k8sClient.Create(ctx, foreignISVCDeployment(name, generatedDeploymentSelector(name)))).To(Succeed())
			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			cond := meta.FindStatusCondition(getDeploymentOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))

			// A foreign HPA takes the generated name; the next pass must
			// refuse the Deployment and leave the foreign HPA untouched.
			foreignHPA := foreignHPA(name)
			Expect(k8sClient.Create(ctx, foreignHPA)).To(Succeed())
			hpaNN := types.NamespacedName{Name: name, Namespace: "default"}
			seeded := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, hpaNN, seeded)).To(Succeed())
			foreignHpaRV := seeded.ResourceVersion

			_, err = reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			got := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, hpaNN, got)).To(Succeed())
			Expect(got.ResourceVersion).To(Equal(foreignHpaRV))
			Expect(got.Spec.MaxReplicas).To(Equal(int32(3)))
			Expect(got.OwnerReferences).To(BeEmpty())
			// The refusal reports the foreign HPA the way reconcileHPA would
			// have (#2010), since the stopped sequence cannot reach it.
			cond = meta.FindStatusCondition(getDeploymentOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonHPANameTaken))
			expectDeploymentNameTaken(ctx, name)
		})

		It("drops AutoscalingReady when autoscaling is cleared during a refusal", func() {
			name := "dep-foreign-autoscale-off"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "cpu",
					},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
			replicas := int32(1)
			Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:    model.Name,
					Replicas:    &replicas,
					Image:       "ghcr.io/ggml-org/llama.cpp:server",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(1), MaxReplicas: 4},
				},
			})).To(Succeed())
			defer cleanupDeploymentOwnershipFixture(ctx, name)
			r := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			Expect(k8sClient.Create(ctx, foreignISVCDeployment(name, generatedDeploymentSelector(name)))).To(Succeed())
			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			cond := meta.FindStatusCondition(getDeploymentOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(inferencev1alpha1.ReasonDeploymentNameTaken))

			// The refusal stops the sequence before reconcileHPA, which owns
			// the condition's removal when autoscaling is not configured; a
			// mode switch during the refusal must not strand a stale False.
			isvc := getDeploymentOwnershipISVC(ctx, name)
			isvc.Spec.Autoscaling = nil
			Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
			_, err = reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			isvc = getDeploymentOwnershipISVC(ctx, name)
			Expect(meta.FindStatusCondition(isvc.Status.Conditions, ConditionAutoscalingReady)).To(BeNil())
			Expect(isvc.Status.SchedulingStatus).To(Equal(inferencev1alpha1.ReasonDeploymentNameTaken),
				"clearing autoscaling must not clear the name-taken diagnosis")
		})

		It("keeps reporting the refusal while another fault fails the reconcile", func() {
			name := "dep-foreign-compound"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			Expect(k8sClient.Create(ctx, foreignISVCDeployment(name, generatedDeploymentSelector(name)))).To(Succeed())
			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			expectDeploymentNameTaken(ctx, name)

			// Break the spec in a way that fails reconcileDeployment before
			// the ownership gate is reached: a metal-only runtime off Metal
			// is rejected up front and its failure write carries no
			// SchedulingInfo, so the status writer preserves the in-memory
			// scheduling fields (#643). The marker must survive that pass:
			// the foreign Deployment still holds the name, and the runbook
			// names it as the symptom.
			isvc := getDeploymentOwnershipISVC(ctx, name)
			isvc.Spec.Runtime = "mlx-server"
			Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
			_, err = reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			isvc = getDeploymentOwnershipISVC(ctx, name)
			Expect(isvc.Status.Phase).To(Equal(PhaseFailed))
			Expect(isvc.Status.SchedulingStatus).To(Equal(inferencev1alpha1.ReasonDeploymentNameTaken),
				"an unrelated spec fault must not erase the name-taken marker")
			Expect(isvc.Status.SchedulingMessage).To(ContainSubstring(name))
		})

		It("refuses when the name changes hands after provisioning", func() {
			name := "dep-foreign-handover"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			// The first pass provisions the operator's own Deployment and
			// Service; the service was working before the name changed hands.
			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			owned := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, owned)).To(Succeed())
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, svc)).To(Succeed())
			svcRV := svc.ResourceVersion
			svcSelector := maps.Clone(svc.Spec.Selector)

			// The name changes hands: the operator's Deployment is gone and a
			// foreign one takes the generated name.
			Expect(k8sClient.Delete(ctx, owned)).To(Succeed())
			nn := types.NamespacedName{Name: name, Namespace: "default"}
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, nn, &appsv1.Deployment{}))
			}, "10s", "100ms").Should(BeTrue())
			foreign := foreignISVCDeployment(name, generatedDeploymentSelector(name))
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
			seeded := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, nn, seeded)).To(Succeed())
			foreignRV := seeded.ResourceVersion

			result, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))

			got := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
			Expect(got.ResourceVersion).To(Equal(foreignRV))
			Expect(got.OwnerReferences).To(BeEmpty())
			expectDeploymentNameTaken(ctx, name)

			// The refusal's early return stops the sequence before the
			// Service step, so the Service provisioned by the earlier pass
			// survives untouched: no rewrite, no delete, no adoption of the
			// foreign workload behind it.
			svcNow := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, svcNow)).To(Succeed())
			Expect(svcNow.ResourceVersion).To(Equal(svcRV))
			Expect(svcNow.Spec.Selector).To(Equal(svcSelector))
		})
	})

	Context("a Deployment this InferenceService controls", func() {
		ctx := context.Background()

		It("updates on a spec change with exactly one controller reference", func() {
			name := "dep-own-update"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, dep)).To(Succeed())
			Expect(dep.OwnerReferences).To(HaveLen(1))
			Expect(*dep.OwnerReferences[0].Controller).To(BeTrue())
			Expect(getDeploymentOwnershipISVC(ctx, name).Status.SchedulingStatus).NotTo(Equal(inferencev1alpha1.ReasonDeploymentNameTaken))

			isvc := getDeploymentOwnershipISVC(ctx, name)
			replicas := int32(3)
			isvc.Spec.Replicas = &replicas
			Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
			_, err = reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			dep = &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(3)))
			Expect(dep.OwnerReferences).To(HaveLen(1))
			Expect(getDeploymentOwnershipISVC(ctx, name).Status.SchedulingStatus).NotTo(Equal(inferencev1alpha1.ReasonDeploymentNameTaken))
		})

		It("recovers once the foreign Deployment is gone, clearing the marker", func() {
			name := "dep-own-recovery"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			// First a refusal, so the pass after recovery must also clear the
			// marker the status writer would otherwise preserve (#643).
			foreign := foreignISVCDeployment(name, generatedDeploymentSelector(name))
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			expectDeploymentNameTaken(ctx, name)

			Expect(k8sClient.Delete(ctx, foreign)).To(Succeed())
			_, err = reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, dep)).To(Succeed())
			Expect(dep.Spec.Selector.MatchLabels).To(HaveKeyWithValue("inference.llmkube.dev/service", name))
			Expect(dep.OwnerReferences).To(HaveLen(1))
			Expect(*dep.OwnerReferences[0].Controller).To(BeTrue())
			Expect(getDeploymentOwnershipISVC(ctx, name).Status.SchedulingStatus).To(BeEmpty())
			Expect(getDeploymentOwnershipISVC(ctx, name).Status.Phase).NotTo(Equal(PhaseFailed))
		})
	})

	Context("a metal transition drops the marker", func() {
		ctx := context.Background()

		// The metal path persists status with no SchedulingInfo of its own
		// when the service is suspended (determinePhase returns
		// PhaseSuspended, nil), so the writer preserves whatever the
		// in-memory object carries (#643). reconcileDeployment's metal
		// branch must clear the stale marker before that write, or the
		// refusal a previous deployment-path pass wrote would freeze.
		It("clears a stale marker when the service no longer wants a Deployment", func() {
			name := "dep-metal-marker"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{
						Accelerator: "metal",
					},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
			replicas := int32(1)
			Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: model.Name,
					Replicas: &replicas,
					Suspend:  true,
				},
			})).To(Succeed())
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			isvc := getDeploymentOwnershipISVC(ctx, name)
			isvc.Status.SchedulingStatus = inferencev1alpha1.ReasonDeploymentNameTaken
			isvc.Status.SchedulingMessage = "Deployment default/" + name + " already exists and is not controlled by this InferenceService"
			Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())

			r := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			Expect(getDeploymentOwnershipISVC(ctx, name).Status.SchedulingStatus).To(BeEmpty(),
				"a metal service wants no Deployment; the marker must not survive the transition")
			Expect(getDeploymentOwnershipISVC(ctx, name).Status.Phase).To(Equal(PhaseSuspended))
		})
	})

	Context("states that want no Deployment write", func() {
		ctx := context.Background()

		// A multiNode group runs no Deployment either; its dispatch branch
		// must clear a stale marker the same way the metal branch does, or
		// the #643 preserve freezes it (the group's own status writes carry
		// no SchedulingInfo). The member claim referencing a missing PVC
		// keeps the spec admission-valid while giving the group a
		// deterministic controller-side fatal write that persists status.
		It("drops the marker when the service switches to multiNode", func() {
			name := "dep-multinode-marker"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			isvc := getDeploymentOwnershipISVC(ctx, name)
			isvc.Spec.Runtime = "vllm"
			isvc.Spec.MultiNode = &inferencev1alpha1.MultiNodeSpec{
				Members: []inferencev1alpha1.MultiNodeMember{
					{Node: "node-a", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.1"},
						ModelCache: &inferencev1alpha1.MultiNodeMemberCache{ClaimName: "missing-claim"}},
					{Node: "node-b", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.2"}},
				},
			}
			Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

			isvc = getDeploymentOwnershipISVC(ctx, name)
			isvc.Status.SchedulingStatus = inferencev1alpha1.ReasonDeploymentNameTaken
			isvc.Status.SchedulingMessage = "Deployment default/" + name + " already exists and is not controlled by this InferenceService"
			Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())

			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			isvc = getDeploymentOwnershipISVC(ctx, name)
			Expect(isvc.Status.SchedulingStatus).To(BeEmpty(),
				"a multiNode service wants no Deployment; the marker must not survive the switch")
		})

		// A suspended service with a foreign Deployment at the generated name
		// is deliberately off: the refusal must report Suspended with the
		// marker, not Failed. The running counterpart reporting Failed is
		// pinned by the overwrite spec above.
		It("reports Suspended, not Failed, while the foreign object holds the name", func() {
			name := "dep-foreign-suspended"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			isvc := getDeploymentOwnershipISVC(ctx, name)
			isvc.Spec.Suspend = true
			Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
			Expect(k8sClient.Create(ctx, foreignISVCDeployment(name, generatedDeploymentSelector(name)))).To(Succeed())

			result, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))

			isvc = getDeploymentOwnershipISVC(ctx, name)
			Expect(isvc.Status.Phase).To(Equal(PhaseSuspended),
				"suspending is the user's intent; the refusal must not turn it into Failed")
			Expect(isvc.Status.SchedulingStatus).To(Equal(inferencev1alpha1.ReasonDeploymentNameTaken),
				"the diagnosis rides the marker even while suspended")
			cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDegraded)
			Expect(cond).To(BeNil(), "Suspended removes Degraded; the refusal must not re-set it")
		})
	})

	Context("the refusal message branches", func() {
		It("reports a deleted same-name predecessor as awaiting garbage collection", func() {
			isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-new"}}
			predecessor := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "s1", UID: "uid-old"}}
			dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"}}
			Expect(setControllerReferenceUnblocked(predecessor, dep, k8sClient.Scheme())).To(Succeed())

			err := ensureDeploymentOwned(isvc, dep)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("previous InferenceService named s1"))
			Expect(err.Error()).To(ContainSubstring("uid-old"))
			Expect(err.Error()).To(ContainSubstring("garbage collection"))
			Expect(err.Error()).NotTo(ContainSubstring("rename one of the services"))
		})

		It("names a foreign InferenceService owner and not a foreign-group impostor", func() {
			isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"}}
			other := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "s2", UID: "uid-2"}}
			dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "default"}}
			Expect(setControllerReferenceUnblocked(other, dep, k8sClient.Scheme())).To(Succeed())

			err := ensureDeploymentOwned(isvc, dep)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("another InferenceService"))
			Expect(err.Error()).To(ContainSubstring("uid-2"))

			impostor := true
			foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "other.example.com/v1", Kind: "InferenceService",
					Name: "impostor", UID: "uid-9", Controller: &impostor,
				}}}}
			err = ensureDeploymentOwned(isvc, foreign)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring("another InferenceService"))

			// A reference to this very service without the controller role
			// does not pass the gate either: IsControlledBy requires
			// Controller: true, and GetControllerOf only returns
			// controller-role references, so the generic message applies.
			notController := false
			selfRef := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: inferencev1alpha1.GroupVersion.String(), Kind: "InferenceService",
					Name: "s1", UID: "uid-1", Controller: &notController,
				}}}}
			err = ensureDeploymentOwned(isvc, selfRef)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not controlled by this InferenceService"))
			Expect(err.Error()).NotTo(ContainSubstring("another InferenceService"))
		})
	})
})
