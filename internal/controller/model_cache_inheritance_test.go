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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The specs below drive the InferenceService reconciler through its public
// entry point and read the result back from the API server. Nothing here names
// a helper the #2142 refactor introduces, so the same file compiles and runs on
// the branch and against the branch it replaced: they are the parity evidence
// that resolving the effective modelCache once renders what base rendered.
var _ = Describe("Model.spec.modelCache inheritance (#2142)", func() {
	var ns string

	BeforeEach(func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "llmkube-2142-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns = nsObj.Name
	})

	// The suite shares one control plane, and the federation specs count Models
	// and InferenceServices cluster-wide, so this suite removes its own before
	// the next spec runs. The namespace itself is left to the suite, because a
	// Deployment it holds can keep the namespace terminating.
	AfterEach(func() {
		for _, obj := range []client.Object{
			&inferencev1alpha1.InferenceService{},
			&inferencev1alpha1.Model{},
		} {
			Expect(client.IgnoreNotFound(k8sClient.DeleteAllOf(ctx, obj, client.InNamespace(ns)))).To(Succeed())
		}
		Eventually(func() int {
			svcs := &inferencev1alpha1.InferenceServiceList{}
			if err := k8sClient.List(ctx, svcs, client.InNamespace(ns)); err != nil {
				return -1
			}
			models := &inferencev1alpha1.ModelList{}
			if err := k8sClient.List(ctx, models, client.InNamespace(ns)); err != nil {
				return -1
			}
			return len(svcs.Items) + len(models.Items)
		}, "10s", "100ms").Should(BeZero(), "a later spec counts Models and InferenceServices cluster-wide")
	})

	createCachePVC := func(name string) {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, pvc))).To(Succeed())
	}

	readyModel := func(name, source string, mutate func(*inferencev1alpha1.Model)) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       inferencev1alpha1.ModelSpec{Source: source, Format: "gguf"},
		}
		if mutate != nil {
			mutate(m)
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		key := types.NamespacedName{Name: name, Namespace: ns}
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		m.Status.Phase = PhaseReady
		if !isPVCSource(source) {
			m.Status.CacheKey = computeCacheKey(source)
		}
		meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type: ConditionAvailable, Status: metav1.ConditionTrue, Reason: "Test", Message: "ready",
		})
		Expect(k8sClient.Status().Update(ctx, m)).To(Succeed())
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		return m
	}

	newService := func(name, modelName string, cache *inferencev1alpha1.ModelCacheSpec) *inferencev1alpha1.InferenceService {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: modelName, ModelCache: cache},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		return isvc
	}

	reconciler := func() *InferenceServiceReconciler {
		return &InferenceServiceReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			Recorder:             events.NewFakeRecorder(50),
			ModelCachePath:       "/models",
			ModelCacheMode:       ModelCacheModeShared,
			ModelCacheSize:       "10Gi",
			ModelCacheAccessMode: "ReadWriteOnce",
			InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
			DefaultFSGroup:       102,
		}
	}

	reconcileOnce := func(r *InferenceServiceReconciler, name string) *inferencev1alpha1.InferenceService {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		got := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got)).To(Succeed())
		return got
	}

	claimOf := func(spec *corev1.PodSpec) string {
		for _, v := range spec.Volumes {
			if v.Name == "model-cache" && v.PersistentVolumeClaim != nil {
				return v.PersistentVolumeClaim.ClaimName
			}
		}
		return ""
	}

	It("keeps the Model's claim across a mid-reconcile status write", func() {
		// A guard, not a reproduction: it passes on the branch this change
		// replaced too, which re-applies the inherited block after that
		// write. It pins that a service inheriting claimName from its Model
		// keeps mounting that claim when a status write lands mid-reconcile
		// (a stale RolloutDeferred condition is the known one) (#2142).
		createCachePVC("midwrite-cache")
		model := readyModel("midwrite-model", "https://example.com/models/llama.gguf", func(m *inferencev1alpha1.Model) {
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "midwrite-cache"}
		})
		isvc := newService("midwrite", model.Name, nil)
		key := types.NamespacedName{Name: isvc.Name, Namespace: ns}
		r := reconciler()

		By("the first pass creating the Deployment on the Model's claim")
		reconcileOnce(r, isvc.Name)
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())

		By("a stale RolloutDeferred condition and a template change, so the next pass defers and writes status")
		stored := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		stored.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{WaitForIdle: true, IdleTimeoutSeconds: 60}
		stored.Spec.Env = append(stored.Spec.Env, corev1.EnvVar{Name: "A_MIDWRITE_MARKER", Value: "1"})
		meta.SetStatusCondition(&stored.Status.Conditions, metav1.Condition{
			Type:               ConditionRolloutDeferred,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonPodsCrashLooping,
			LastTransitionTime: metav1.Now(),
		})
		stored.Status.Phase = PhaseReady
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, stored)).To(Succeed())

		By("the second pass resolving the claim after that status write")
		reconcileOnce(r, isvc.Name)

		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(claimOf(&dep.Spec.Template.Spec)).To(Equal("midwrite-cache"),
			"the Deployment must keep mounting the Model's claim after the mid-reconcile status write")
	})

	It("verifies the Model's claim a service inherits, and mounts it for the draft too", func() {
		// The inherited claim is the service's cache volume, so the
		// provisioning side must verify it (a user-owned claim that is missing
		// is Degraded, not silently replaced by one the pods never mount), and
		// the draft's weights ride on that same claim.
		model := readyModel("inherited-claim-model", "https://example.com/models/llama.gguf", func(m *inferencev1alpha1.Model) {
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "inherited-claim"}
		})
		readyModel("inherited-draft", "https://example.com/models/draft.gguf", nil)

		By("refusing to provision a replacement when the inherited claim is missing")
		missing := newService("inherited-missing", model.Name, nil)
		missing.Spec.SpeculativeDecoding = &inferencev1alpha1.SpeculativeDecodingSpec{Type: "draft-simple", DraftModelRef: "inherited-draft"}
		Expect(k8sClient.Update(ctx, missing)).To(Succeed())
		got := reconcileOnce(reconciler(), missing.Name)
		Expect(got.Status.Phase).To(Equal(PhaseFailed))
		Expect(got.Status.Conditions).NotTo(BeEmpty())
		Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionDegraded)).NotTo(BeNil(),
			"the Model's claim is verified as the service's cache volume; a missing one is Degraded")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: ModelCachePVCName, Namespace: ns},
			&corev1.PersistentVolumeClaim{}))).To(BeTrue(), "no shared claim is created for a service that mounts another")

		By("mounting the inherited claim for both the target and the draft once it exists")
		createCachePVC("inherited-claim")
		isvc := newService("inherited-draft-svc", model.Name, nil)
		isvc.Spec.SpeculativeDecoding = &inferencev1alpha1.SpeculativeDecodingSpec{Type: "draft-simple", DraftModelRef: "inherited-draft"}
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
		reconcileOnce(reconciler(), isvc.Name)

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, dep)).To(Succeed())
		for _, v := range dep.Spec.Template.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				Expect(v.PersistentVolumeClaim.ClaimName).To(Equal("inherited-claim"),
					"volume %q mounts the claim the service resolves, not the draft Model's own default", v.Name)
			}
		}
	})

	// A Model-level runtimeCache block is inherited like every other field in
	// the block, and the directory it names is still the service's own. A
	// service block that omits it replaces the inherited one (the block as a
	// whole is what inherits, #2142).
	It("carries a Model-level runtimeCache to the service's pod", func() {
		model := readyModel("runtime-cache-model", "https://example.com/models/llama.gguf", func(m *inferencev1alpha1.Model) {
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{
				RuntimeCache: &inferencev1alpha1.RuntimeCacheSpec{Enabled: true},
			}
		})
		mountsAt := func(name string) (han []corev1.VolumeMount) {
			GinkgoHelper()
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, dep)).To(Succeed())
			for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
				if m.MountPath == "/runtime-cache" {
					han = append(han, m)
				}
			}
			return han
		}

		inherited := newService("runtime-cache-inherited", model.Name, nil)
		inherited.Spec.Runtime = RuntimeVLLM
		Expect(k8sClient.Update(ctx, inherited)).To(Succeed())
		reconcileOnce(reconciler(), inherited.Name)
		mounts := mountsAt(inherited.Name)
		Expect(mounts).To(HaveLen(1), "the Model's runtimeCache block must reach the service's pod")
		Expect(mounts[0].SubPath).To(Equal("_runtime-cache/"+ns+"/"+inherited.Name),
			"the directory is the service's own, not the Model's")

		own := newService("runtime-cache-own", model.Name, &inferencev1alpha1.ModelCacheSpec{})
		own.Spec.Runtime = RuntimeVLLM
		Expect(k8sClient.Update(ctx, own)).To(Succeed())
		reconcileOnce(reconciler(), own.Name)
		Expect(mountsAt(own.Name)).To(BeEmpty(),
			"a service's own block replaces the inherited one, runtimeCache included")
	})

	// The upgrade property: a service that inherits the Model's block and one
	// that carries the same block on its own spec render the same pod, so
	// making the inherited block explicit rolls nothing. The pod is read back
	// from the API server after each pass, and the two passes share one
	// service in one namespace: the emptyDir path names the model file
	// /models/<namespace>-<model>.gguf, so a second namespace would compare
	// two different paths.
	//
	// Prefetch is false in both cases: it is the service's own request, and
	// neither the inherited nor the written-out block asks for it.
	DescribeTable("writes an inherited block onto the service's own spec without rolling a pod (#2142)",
		func(svcName string, block *inferencev1alpha1.ModelCacheSpec) {
			if block.ClaimName != "" {
				createCachePVC(block.ClaimName)
			}
			model := readyModel(svcName+"-model", "https://example.com/models/llama.gguf", func(m *inferencev1alpha1.Model) {
				m.Spec.ModelCache = block.DeepCopy()
			})
			isvc := newService(svcName, model.Name, nil)
			key := types.NamespacedName{Name: isvc.Name, Namespace: ns}
			r := reconciler()

			By("resolving the block from the Model")
			reconcileOnce(r, isvc.Name)
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			hashBefore := dep.Annotations[AnnotationDesiredTemplateHash]
			Expect(hashBefore).NotTo(BeEmpty(), "the create path stamps the desired-template hash")
			templateBefore := dep.Spec.Template.DeepCopy()

			By("writing the same block onto the service's own spec")
			stored := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
			Expect(stored.Spec.ModelCache).To(BeNil(), "inheritance is never written back to the service's spec")
			stored.Spec.ModelCache = model.Spec.ModelCache.DeepCopy()
			Expect(k8sClient.Update(ctx, stored)).To(Succeed())

			By("rendering the same pod for the explicit block")
			reconcileOnce(r, isvc.Name)
			after := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, after)).To(Succeed())
			Expect(after.Annotations[AnnotationDesiredTemplateHash]).To(Equal(hashBefore),
				"the template hash must not move when the inherited block is made explicit")
			Expect(after.Spec.Template).To(Equal(*templateBefore))
			Expect(hasModelDownloader(after)).To(BeTrue(),
				"neither case asks for a prefetch, so the pod still downloads at serve time")
		},
		Entry("an inherited claimName", "explicit-claim", &inferencev1alpha1.ModelCacheSpec{ClaimName: "explicit-block-cache"}),
		Entry("an inherited ephemeral opt-out", "explicit-ephemeral", &inferencev1alpha1.ModelCacheSpec{
			Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral,
		}),
	)
})

// hasModelDownloader reports whether the pod downloads at serve time.
func hasModelDownloader(dep *appsv1.Deployment) bool {
	for _, c := range dep.Spec.Template.Spec.InitContainers {
		if c.Name == "model-downloader" {
			return true
		}
	}
	return false
}
