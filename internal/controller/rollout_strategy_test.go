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
	"net/http"
	"net/http/httptest"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// gpuModelWithCount builds a Ready CUDA model that requests count GPUs through
// the device plugin.
func gpuModelWithCount(name string, count int32) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/model.gguf",
			Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: "cuda",
				GPU: &inferencev1alpha1.GPUSpec{
					Enabled: true,
					Count:   count,
					Vendor:  "nvidia",
					Layers:  -1,
				},
			},
		},
		Status: inferencev1alpha1.ModelStatus{Phase: "Ready", Path: "/tmp/llmkube/models/" + name + ".gguf"},
	}
}

// cpuModelForStrategy builds a Ready CPU model, the class that already rolls
// with the Kubernetes default.
func cpuModelForStrategy(name string) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:   "https://example.com/model.gguf",
			Format:   "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
		},
		Status: inferencev1alpha1.ModelStatus{Phase: "Ready", Path: "/tmp/llmkube/models/" + name + ".gguf"},
	}
}

func strategyISVC(name, modelName string, replicas int32) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: modelName,
			Replicas: &replicas,
			Image:    "ghcr.io/ggml-org/llama.cpp:server-cuda13",
		},
	}
}

// The expected Recreate and OneAtATime shapes come from the Kubernetes
// Deployment controller, not from the constants the code writes: with
// maxSurge 0, NewRSNewReplicas caps the new ReplicaSet at
// deployment.Spec.Replicas while any old pod counts, which is what keeps an
// old pod and its replacement from holding a GPU at the same time (#192).
func expectRecreate(deployment *appsv1.Deployment) {
	Expect(deployment.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
	Expect(deployment.Spec.Strategy.RollingUpdate).To(BeNil(),
		"Recreate must not carry rollingUpdate; the API server forbids it")
}

func expectRollingOneAtATime(deployment *appsv1.Deployment) {
	Expect(deployment.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
	Expect(deployment.Spec.Strategy.RollingUpdate).NotTo(BeNil())
	Expect(deployment.Spec.Strategy.RollingUpdate.MaxSurge.IntValue()).To(Equal(0),
		"maxSurge 0 means the new ReplicaSet cannot scale up while an old pod counts")
	Expect(deployment.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue()).To(Equal(1),
		"maxUnavailable 1 replaces one pod at a time and keeps the rest serving")
}

func expectNoStrategy(deployment *appsv1.Deployment) {
	Expect(deployment.Spec.Strategy.Type).To(Equal(appsv1.DeploymentStrategyType("")),
		"a service that needs no strategy must leave the API default")
	Expect(deployment.Spec.Strategy.RollingUpdate).To(BeNil())
}

// busyIdleServer reports every replica as processing, so the drain check
// defers. Pointing the reconciler at it is how the existing drain-before-roll
// tests prove a deferral; a pod with no EndpointSlice instead falls through to
// the Service-URL path, which reports errNoEndpoints rather than busy.
func busyIdleServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots" {
			_, _ = w.Write([]byte(`[{"id":0,"is_processing":true}]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

var _ = Describe("Rollout strategy (#2131)", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			DefaultFSGroup:     102,
		}
	})

	Context("GPU device-plugin service", func() {
		It("keeps Recreate when rolloutPolicy is unset", func() {
			model := gpuModelWithCount("strategy-gpu-default", 1)
			isvc := strategyISVC("strategy-gpu-default", "strategy-gpu-default", 2)

			expectRecreate(reconciler.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 2, "", ""))
		})

		It("keeps Recreate when strategy is Recreate", func() {
			model := gpuModelWithCount("strategy-gpu-recreate", 1)
			isvc := strategyISVC("strategy-gpu-recreate", "strategy-gpu-recreate", 2)
			isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyRecreate}

			expectRecreate(reconciler.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 2, "", ""))
		})

		DescribeTable("uses a one-at-a-time rolling update when strategy is OneAtATime",
			func(replicas int32) {
				name := "strategy-gpu-oneatatime-" + strconv.Itoa(int(replicas))
				model := gpuModelWithCount(name, 4)
				isvc := strategyISVC(name, name, replicas)
				isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyOneAtATime}

				expectRollingOneAtATime(reconciler.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, replicas, "", ""))
			},
			// One replica behaves like Recreate in effect: the only pod is
			// replaced. The shape still has to be the same one, or the field
			// would silently do nothing at replicas 1.
			Entry("replicas 1", int32(1)),
			Entry("replicas 2", int32(2)),
		)

		It("does not gate the rollout on idle just because strategy is set", func() {
			model := gpuModelWithCount("strategy-not-idle", 1)
			isvc := strategyISVC("strategy-not-idle", "strategy-not-idle", 2)
			isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyOneAtATime}

			Expect(isvc.RolloutPolicyEnabled()).To(BeFalse(),
				"strategy alone must not turn on the drain-before-roll idle gate")
			Expect(isvc.ShouldDeferRollout()).To(BeFalse())

			deployment := reconciler.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 2, "", "")
			Expect(*deployment.Spec.Replicas).To(Equal(int32(2)))
			expectRollingOneAtATime(deployment)
		})
	})

	Context("workloads the field cannot apply to", func() {
		It("leaves a CPU service on the Kubernetes default", func() {
			model := cpuModelForStrategy("strategy-cpu")
			isvc := strategyISVC("strategy-cpu", "strategy-cpu", 2)
			isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyOneAtATime}

			expectNoStrategy(reconciler.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 2, "", ""))
		})

		It("keeps Recreate for a DRA service whatever the field says", func() {
			claimName := "gpu-claim"
			model := gpuModelWithCount("strategy-dra", 1)
			model.Spec.Hardware.GPU.ResourceClaims = []corev1.PodResourceClaim{
				{Name: "gpu", ResourceClaimName: &claimName},
			}
			isvc := strategyISVC("strategy-dra", "strategy-dra", 2)
			isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyOneAtATime}

			// A replacement pod cannot get the claim while the old pod holds it,
			// so DRA stays Recreate regardless (#2061).
			expectRecreate(reconciler.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 2, "", ""))
		})
	})

	It("reports a metal service's strategy as ignored even when its model declares a GPU count", func() {
		// Asserted through the caller the metal path actually reaches: the
		// metal reconcile returns before constructDeploymentForCache, so the resolver's
		// metal answer surfaces as this warning. resolveGPUCount reads the
		// Model's declared count, so without the metal case a metal model that
		// names one would take the GPU branch and be told the field was
		// honoured.
		model := gpuModelWithCount("strategy-metal", 1)
		model.Spec.Hardware.Accelerator = "metal"
		isvc := strategyISVC("strategy-metal", "strategy-metal", 2)
		isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyOneAtATime}

		ignored, msg := ignoredRolloutStrategy(isvc, model)
		Expect(ignored).To(BeTrue())
		Expect(msg).To(Equal(rolloutStrategyIgnoredMetalMessage))
	})

	Context("the ignored-field warning", func() {
		It("is silent when the field is unset", func() {
			isvc := strategyISVC("warn-unset", "m", 2)
			ignored, _ := ignoredRolloutStrategy(isvc, gpuModelWithCount("m", 1))
			Expect(ignored).To(BeFalse())
		})

		It("is silent for a GPU service that honours the field", func() {
			isvc := strategyISVC("warn-honoured", "m", 2)
			isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyOneAtATime}
			ignored, _ := ignoredRolloutStrategy(isvc, gpuModelWithCount("m", 1))
			Expect(ignored).To(BeFalse())
		})

		// Each entry names the cause that fired, so a warning that tells a
		// multiNode user about resource claims is a failure, not a pass.
		DescribeTable("fires when the field cannot apply, naming the cause",
			func(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, want string) {
				ignored, msg := ignoredRolloutStrategy(isvc, model)
				Expect(ignored).To(BeTrue())
				Expect(msg).To(Equal(want))
			},
			Entry("non-GPU service", func() *inferencev1alpha1.InferenceService {
				isvc := strategyISVC("warn-cpu", "m", 2)
				isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyOneAtATime}
				return isvc
			}(), cpuModelForStrategy("m"), rolloutStrategyIgnoredMessage),
			Entry("DRA service", func() *inferencev1alpha1.InferenceService {
				isvc := strategyISVC("warn-dra", "m", 2)
				isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyOneAtATime}
				return isvc
			}(), func() *inferencev1alpha1.Model {
				claimName := "gpu-claim"
				model := gpuModelWithCount("m", 1)
				model.Spec.Hardware.GPU.ResourceClaims = []corev1.PodResourceClaim{
					{Name: "gpu", ResourceClaimName: &claimName},
				}
				return model
			}(), rolloutStrategyIgnoredDRAMessage),
			Entry("metal service", func() *inferencev1alpha1.InferenceService {
				isvc := strategyISVC("warn-metal", "m", 2)
				isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyOneAtATime}
				return isvc
			}(), func() *inferencev1alpha1.Model {
				// The declared GPU count stays set, so this entry also pins the
				// metal case ahead of the GPU-count one in the resolver.
				model := gpuModelWithCount("m", 1)
				model.Spec.Hardware.Accelerator = "metal"
				return model
			}(), rolloutStrategyIgnoredMetalMessage),
			Entry("multiNode group with an explicit Recreate", func() *inferencev1alpha1.InferenceService {
				// The resolver answers "no strategy" for a multiNode group
				// rather than base's Recreate. Restoring Recreate there would
				// make strategyReflects call this field honoured, so a user
				// asking for Recreate on a group that runs no Deployment would
				// stop being told it has no effect.
				isvc := strategyISVC("warn-multinode-recreate", "m", 2)
				isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyRecreate}
				isvc.Spec.MultiNode = &inferencev1alpha1.MultiNodeSpec{
					Members: []inferencev1alpha1.MultiNodeMember{
						{Node: "node-a"},
						{Node: "node-b"},
					},
				}
				return isvc
			}(), gpuModelWithCount("m", 1), rolloutStrategyIgnoredMultiNodeMessage),
			Entry("multiNode group", func() *inferencev1alpha1.InferenceService {
				isvc := strategyISVC("warn-multinode", "m", 2)
				isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyOneAtATime}
				isvc.Spec.MultiNode = &inferencev1alpha1.MultiNodeSpec{
					Members: []inferencev1alpha1.MultiNodeMember{
						{Node: "node-a"},
						{Node: "node-b"},
					},
				}
				return isvc
			}(), gpuModelWithCount("m", 1), rolloutStrategyIgnoredMultiNodeMessage),
		)
	})
})

var _ = Describe("Rollout strategy envtest (#2131)", func() {
	ctx := context.Background()

	// created collects the object names a spec creates, oldest first, so the
	// AfterEach can drop them in reverse without the helper returning before the
	// spec body has used the object.
	created := []string{}

	BeforeEach(func() {
		created = nil
	})

	AfterEach(func() {
		for i := len(created) - 1; i >= 0; i-- {
			name := created[i]
			key := types.NamespacedName{Name: name, Namespace: "default"}
			owner := &inferencev1alpha1.InferenceService{}
			if err := k8sClient.Get(ctx, key, owner); err == nil {
				_ = k8sClient.Delete(ctx, owner)
			}
			dep := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, key, dep); err == nil {
				_ = k8sClient.Delete(ctx, dep)
			}
			svc := &corev1.Service{}
			if err := k8sClient.Get(ctx, key, svc); err == nil {
				_ = k8sClient.Delete(ctx, svc)
			}
			model := &inferencev1alpha1.Model{}
			if err := k8sClient.Get(ctx, key, model); err == nil {
				_ = k8sClient.Delete(ctx, model)
			}
		}
	})

	createModel := func(name string) *inferencev1alpha1.Model {
		model := gpuModelWithCount(name, 4)
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		created = append(created, name)
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
		return model
	}

	newISVC := func(name, modelName string, replicas int32) *inferencev1alpha1.InferenceService {
		isvc := strategyISVC(name, modelName, replicas)
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		created = append(created, name)
		return isvc
	}

	reconcileOnce := func(reconciler *InferenceServiceReconciler, name string) {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	It("creates a 2-replica GPU Deployment that rolls one pod at a time", func() {
		name := "strategy-envtest-create"
		createModel(name)
		newISVC(name, name, 2)
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, isvc)).To(Succeed())
		isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyOneAtATime}
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

		reconciler := &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
		reconcileOnce(reconciler, name)

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, dep)).To(Succeed())
		Expect(dep.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
		Expect(dep.Spec.Strategy.RollingUpdate.MaxSurge.IntValue()).To(Equal(0))
		Expect(dep.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue()).To(Equal(1))
		Expect(*dep.Spec.Replicas).To(Equal(int32(2)),
			"a one-at-a-time rollout runs at full size; maxSurge 0 caps the total, it does not shrink it")
	})

	It("lands a strategy-only edit on a live Deployment without touching the pod template", func() {
		// The spec below sets only rolloutPolicy.strategy, so its body and name
		// agree: the strategy reaches the live Deployment and the pod-template
		// hash does not move, so no pod is restarted.
		name := "strategy-envtest-flip"
		createModel(name)
		newISVC(name, name, 2)

		reconciler := &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
		reconcileOnce(reconciler, name)

		dep := &appsv1.Deployment{}
		key := types.NamespacedName{Name: name, Namespace: "default"}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(dep.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType),
			"unset strategy keeps the GPU default")
		hashBefore := dep.Annotations[AnnotationDesiredTemplateHash]
		Expect(hashBefore).NotTo(BeEmpty())

		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, isvc)).To(Succeed())
		isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyOneAtATime}
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

		reconcileOnce(reconciler, name)

		rolled := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, rolled)).To(Succeed())
		Expect(rolled.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType),
			"the strategy is part of the Deployment spec the reconcile writes, so it lands on the live object")
		Expect(rolled.Annotations[AnnotationDesiredTemplateHash]).To(Equal(hashBefore),
			"choosing a strategy does not touch the pod template, so it must not start a rollout")
	})

	It("accepts dropping the field on a live service and clears the rolling update", func() {
		name := "strategy-envtest-opt-out"
		createModel(name)
		newISVC(name, name, 2)

		reconciler := &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
		key := types.NamespacedName{Name: name, Namespace: "default"}

		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, isvc)).To(Succeed())
		isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: inferencev1alpha1.RolloutStrategyOneAtATime}
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
		reconcileOnce(reconciler, name)

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(dep.Spec.Strategy.RollingUpdate).NotTo(BeNil())

		// Opt back out by removing the field. The API server forbids
		// rollingUpdate under Recreate, so this is the path that catches a
		// stale RollingUpdate block surviving a switch back.
		Expect(k8sClient.Get(ctx, key, isvc)).To(Succeed())
		isvc.Spec.RolloutPolicy.Strategy = ""
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

		persisted := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, persisted)).To(Succeed())
		Expect(persisted.Spec.RolloutPolicy.Strategy).To(BeEmpty(),
			"the field must actually be removed, not set to another value")

		reconcileOnce(reconciler, name)

		reverted := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, reverted)).To(Succeed())
		expectRecreate(reverted)
	})

	It("lands a strategy-only edit while the backend reports busy", func() {
		// The strategy is not part of the hashed pod template, so on its own it
		// reports templateChanged=false and the reconcile skips the drain check
		// entirely (#2131). The busy fixture is what makes the assertion mean
		// something: with a template change, the same backend defers.
		busy := busyIdleServer()
		defer busy.Close()

		name := "strategy-envtest-strategy-only"
		createModel(name)

		isvc := strategyISVC(name, name, 2)
		isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{
			WaitForIdle:        true,
			IdleTimeoutSeconds: 600,
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		created = append(created, name)

		reconciler := &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			RolloutIdleBaseURL: busy.URL,
		}
		key := types.NamespacedName{Name: name, Namespace: "default"}
		reconcileOnce(reconciler, name)

		Expect(k8sClient.Get(ctx, key, isvc)).To(Succeed())
		isvc.Spec.RolloutPolicy.Strategy = inferencev1alpha1.RolloutStrategyOneAtATime
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
		reconcileOnce(reconciler, name)

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(dep.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType),
			"a strategy-only edit changes no pod, so it must not wait for the backend to go idle")
	})

	It("still holds a template change and its replicas while the backend reports busy", func() {
		// A deferred template change holds the whole Deployment spec, replicas
		// included: the strategy guard that once excused a template-equal pass
		// is gone, and this pins what remains (#2131).
		busy := busyIdleServer()
		defer busy.Close()

		name := "strategy-envtest-idle-gate"
		createModel(name)

		isvc := strategyISVC(name, name, 2)
		isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{
			WaitForIdle:        true,
			IdleTimeoutSeconds: 600,
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		created = append(created, name)

		reconciler := &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			RolloutIdleBaseURL: busy.URL,
		}
		key := types.NamespacedName{Name: name, Namespace: "default"}
		reconcileOnce(reconciler, name)

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		original := dep.Spec.Template.Spec.Containers[0].Image

		Expect(k8sClient.Get(ctx, key, isvc)).To(Succeed())
		isvc.Spec.Image = "ghcr.io/ggml-org/llama.cpp:server-cuda13-v2"
		wantReplicas := int32(3)
		isvc.Spec.Replicas = &wantReplicas
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
		reconcileOnce(reconciler, name)

		held := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, held)).To(Succeed())
		Expect(*held.Spec.Replicas).To(Equal(int32(2)),
			"the held update carries the replica count with the template, so a scale change waits too")
		Expect(held.Spec.Template.Spec.Containers[0].Image).To(Equal(original),
			"a busy backend keeps the live template until it goes idle")

		got := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionRolloutDeferred)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(ReasonPodsBusy))
	})

	It("defers a strategy-only edit on a Deployment that predates the template hash", func() {
		// The input class the docs name: a missing desired-template-hash
		// annotation reads as changed, so even a strategy-only edit enters the
		// drain gate and waits with the held spec. Base behaviour, pinned here
		// because the field doc and the concept page claim a strategy-only edit
		// needs no drain (#2131).
		busy := busyIdleServer()
		defer busy.Close()

		name := "strategy-envtest-legacy-no-hash"
		createModel(name)

		isvc := strategyISVC(name, name, 2)
		isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{
			WaitForIdle:        true,
			IdleTimeoutSeconds: 600,
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		created = append(created, name)

		reconciler := &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			RolloutIdleBaseURL: busy.URL,
		}
		key := types.NamespacedName{Name: name, Namespace: "default"}
		reconcileOnce(reconciler, name)

		// Strip the annotation the operator stamps, standing in for a
		// Deployment created before the annotation existed.
		legacy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, legacy)).To(Succeed())
		delete(legacy.Annotations, AnnotationDesiredTemplateHash)
		Expect(k8sClient.Update(ctx, legacy)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, isvc)).To(Succeed())
		isvc.Spec.RolloutPolicy.Strategy = inferencev1alpha1.RolloutStrategyOneAtATime
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
		reconcileOnce(reconciler, name)

		got := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionRolloutDeferred)
		Expect(cond).NotTo(BeNil(),
			"a missing template hash reads as changed, so the strategy waits with the held update")
		Expect(cond.Reason).To(Equal(ReasonPodsBusy))

		afterDep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, afterDep)).To(Succeed())
		Expect(afterDep.Spec.Strategy.Type).NotTo(Equal(appsv1.RollingUpdateDeploymentStrategyType),
			"the held pass must not write the strategy either")
	})

	It("rejects an unknown strategy at admission", func() {
		name := "strategy-envtest-invalid"
		createModel(name)
		isvc := strategyISVC(name, name, 2)
		isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{Strategy: "Bogus"}

		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("strategy"))
	})

	It("emits RolloutStrategyIgnored for a service the field cannot apply to", func() {
		name := "strategy-envtest-warn"
		modelName := "strategy-envtest-warn-model"
		model := cpuModelForStrategy(modelName)
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		created = append(created, modelName)
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		recorder := events.NewFakeRecorder(20)
		replicas := int32(2)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: modelName,
				Replicas: &replicas,
				Image:    "ghcr.io/ggml-org/llama.cpp:server",
				RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
					Strategy: inferencev1alpha1.RolloutStrategyOneAtATime,
				},
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		created = append(created, name)

		reconciler := &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			Recorder:           recorder,
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
		key := types.NamespacedName{Name: name, Namespace: "default"}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var got []string
		for {
			select {
			case e := <-recorder.Events:
				got = append(got, e)
			default:
				Expect(got).To(ContainElement(ContainSubstring(ReasonRolloutStrategyIgnored)),
					"a set strategy on a CPU service must not be accepted silently (#1503)")
				Expect(got).To(ContainElement(ContainSubstring("a non-GPU service")),
					"the event must name the cause that fired, not a generic reason")
				return
			}
		}
	})
})
