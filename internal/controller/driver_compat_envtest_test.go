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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// End-to-end pin for the driver-compat diagnosis: a real Reconcile pass over
// a crashlooping runtime pod must persist the DriverCompatible condition on
// the InferenceService through the controller's own status write. The unit
// tests call reconcileDriverCompatCondition directly, so without this spec
// the Reconcile call site could be deleted and the suite would still pass.
var _ = Describe("InferenceService driver-compat diagnosis (Reconcile integration)", func() {
	It("persists DriverCompatible=False from a crashed runtime pod's termination message", func() {
		const name = "driver-compat-e2e"
		skipInit := true

		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "driver-compat-node",
				Labels: map[string]string{
					"nvidia.com/cuda.runtime-version.major": "12",
					"nvidia.com/cuda.runtime-version.minor": "4",
					"nvidia.com/cuda.driver-version.full":   "550.144.03",
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, node) }()

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://example.com/model.safetensors",
				Format: "safetensors",
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:      name,
				Runtime:       RuntimeVLLM,
				SkipModelInit: &skipInit,
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, isvc) }()

		// The pod a Deployment would have produced, crashlooping with the
		// real PyTorch wording in its termination message. envtest has no
		// kubelet, so the status is written directly.
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-pod",
				Namespace: "default",
				Labels: map[string]string{
					"app":                           name,
					"inference.llmkube.dev/service": name,
				},
			},
			Spec: corev1.PodSpec{
				NodeName: node.Name,
				Containers: []corev1.Container{{
					Name:  "vllm",
					Image: "vllm/vllm-openai:test",
				}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, pod) }()
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "vllm",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
				},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1,
						Message:  "RuntimeError: The NVIDIA driver on your system is too old (found version 12040).",
					},
				},
			}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		reconciler := &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		updated := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, updated)).To(Succeed())

		var cond *metav1.Condition
		for i := range updated.Status.Conditions {
			if updated.Status.Conditions[i].Type == ConditionDriverCompatible {
				cond = &updated.Status.Conditions[i]
			}
		}
		Expect(cond).NotTo(BeNil(), "DriverCompatible condition must be persisted by Reconcile")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonCUDADriverInsufficient))
		Expect(cond.Message).To(ContainSubstring("found version 12040"))
		Expect(cond.Message).To(ContainSubstring(node.Name))
		Expect(cond.Message).To(ContainSubstring("CUDA <= 12.4"))

		// The diagnosis must not have hijacked the lifecycle fields.
		Expect(updated.Status.Phase).NotTo(Equal(PhaseFailed))
	})
})
