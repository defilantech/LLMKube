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
	"errors"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var _ = Describe("Model Prefetch", func() {
	ctx := context.Background()
	const ns = "prefetch-test"

	prefetchReconciler := func() *ModelReconciler {
		return &ModelReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
			DefaultFSGroup:       102,
			ModelCacheSize:       "10Gi",
			ModelCacheAccessMode: "ReadWriteOnce",
			ModelCacheMode:       ModelCacheModeShared,
			ModelCachePath:       "/models",
		}
	}

	newPrefetchModel := func(name string) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/models/llama.gguf",
				Prefetch: true,
			},
		}
	}

	createPVC := func(name string) { createCachePVC(ctx, ns, name) }
	completeJob := func(key types.NamespacedName) { markJobComplete(ctx, key) }
	failJob := func(key types.NamespacedName) { markJobFailed(ctx, key) }
	jobClaim := func(job *batchv1.Job) string { return cacheClaimOf(&job.Spec.Template.Spec) }

	BeforeEach(func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		err := k8sClient.Create(ctx, nsObj)
		if err != nil {
			Expect(client.IgnoreAlreadyExists(err)).To(Succeed())
		}
	})

	Describe("prefetchEligible", func() {
		It("is false when the field is unset", func() {
			m := newPrefetchModel("x")
			m.Spec.Prefetch = false
			Expect(prefetchEligible(m)).To(BeFalse())
		})

		It("is false for pvc:// sources even with prefetch set", func() {
			m := newPrefetchModel("x")
			m.Spec.Source = "pvc://some-pvc/model.gguf"
			Expect(prefetchEligible(m)).To(BeFalse())
		})

		It("is true for hf:// sources (normalized to an https resolve URL, so prefetchable)", func() {
			m := newPrefetchModel("x")
			m.Spec.Source = "hf://org/repo"
			Expect(prefetchEligible(m)).To(BeTrue())
		})

		It("is true for https sources", func() {
			Expect(prefetchEligible(newPrefetchModel("x"))).To(BeTrue())
		})

		// #2136: the prefetch path expands the glob from the repository
		// listing before building the Job, so a glob no longer disables it.
		It("is true with a glob in spec.files", func() {
			m := newPrefetchModel("x")
			m.Spec.Source = "hf://org/repo"
			m.Spec.Files = []string{"config.json", "*.safetensors"}
			Expect(prefetchEligible(m)).To(BeTrue())
		})

		It("is true when only modelCache.prefetch, the alias, is set", func() {
			m := newPrefetchModel("x")
			m.Spec.Prefetch = false
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{Prefetch: true}
			Expect(prefetchEligible(m)).To(BeTrue())
		})
	})

	Describe("buildPrefetchJob scheduling", func() {
		// #1621: a fleet whose GPU nodes are tainted needs the prefetch Job
		// to tolerate them, because the shared cache can live on node-local
		// storage pinned to exactly those nodes. The Model carries its own
		// tolerations: inheriting from an InferenceService is ill-defined
		// here, since prefetch runs before the first one exists by design.
		It("carries the Model's tolerations onto the Job pod", func() {
			m := newPrefetchModel("tolerated")
			m.Spec.PrefetchTolerations = []corev1.Toleration{{
				Key:      "nvidia.com/gpu",
				Operator: corev1.TolerationOpEqual,
				Value:    "present",
				Effect:   corev1.TaintEffectNoSchedule,
			}}
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.Tolerations).To(HaveLen(1))
			Expect(job.Spec.Template.Spec.Tolerations[0].Key).To(Equal("nvidia.com/gpu"))
		})

		It("emits no tolerations when the Model declares none", func() {
			m := newPrefetchModel("plain")
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.Tolerations).To(BeEmpty())
		})

		// #1964: a node-pinned claim (#1676) binds wherever the prefetch pod
		// schedules, so the prefetch placement has to be steerable.
		It("carries the Model's nodeSelector onto the Job pod", func() {
			m := newPrefetchModel("node-selected")
			m.Spec.PrefetchNodeSelector = map[string]string{"example.com/node-pool": "gpu"}
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("example.com/node-pool", "gpu"))
		})

		It("emits no node selector when the Model declares none", func() {
			m := newPrefetchModel("plain-placement")
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.NodeSelector).To(BeEmpty())
		})

		// #2136: a selector cannot say "not on a GPU node"; an affinity can.
		It("carries the Model's prefetchAffinity onto the Job pod", func() {
			m := newPrefetchModel("affine")
			m.Spec.PrefetchAffinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "nvidia.com/gpu.present", Operator: corev1.NodeSelectorOpDoesNotExist,
					}}}},
				},
			}}
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.Affinity).To(Equal(m.Spec.PrefetchAffinity))
		})

		It("emits no affinity when the Model declares none", func() {
			m := newPrefetchModel("plain-affinity")
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.Affinity).To(BeNil())
		})

		It("stamps the fingerprint and claim it was built for", func() {
			m := newPrefetchModel("stamped")
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "node-a-cache", "abc123")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Annotations).To(HaveKeyWithValue(annotationPrefetchFingerprint, "abc123"))
			Expect(job.Annotations).To(HaveKeyWithValue(annotationPrefetchClaim, "node-a-cache"))
			Expect(jobClaim(job)).To(Equal("node-a-cache"))
		})
	})

	// #2058: the prefetch Job mounts the same shared cache as the serving pods,
	// so it gets the same fsGroupChangePolicy whenever it gets an fsGroup.
	Describe("buildPrefetchJob security context", func() {
		It("sets fsGroupChangePolicy OnRootMismatch alongside the default fsGroup", func() {
			m := newPrefetchModel("fsgroup-policy")
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			psc := job.Spec.Template.Spec.SecurityContext
			Expect(psc).NotTo(BeNil())
			Expect(psc.FSGroup).To(HaveValue(Equal(int64(102))))
			Expect(psc.FSGroupChangePolicy).To(HaveValue(Equal(corev1.FSGroupChangeOnRootMismatch)))
		})

		It("sets no pod security context when the default fsGroup is disabled", func() {
			m := newPrefetchModel("fsgroup-disabled")
			seedPrefetchCacheKey(m)
			r := prefetchReconciler()
			r.DefaultFSGroup = 0
			job, err := r.buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.SecurityContext).To(BeNil())
		})
	})

	Describe("reconcilePrefetch", func() {
		It("does not handle models without prefetch", func() {
			m := newPrefetchModel("no-prefetch")
			m.Spec.Prefetch = false
			handled, _, err := prefetchReconciler().reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())
		})

		It("waits for foreground deletion before replacing a multi-file prefetch writer", func() {
			model := newPrefetchModel("model-prefetch-writer")
			model.Spec.Source = "https://example.com/weights"
			model.Spec.Format = "safetensors"
			model.Spec.Files = []string{"model.safetensors"}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, model) })
			r := prefetchReconciler()
			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			jobKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			oldUID := job.UID
			oldFingerprint := job.Annotations[annotationPrefetchFingerprint]

			modelKey := client.ObjectKeyFromObject(model)
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			model.Spec.Files = append(model.Spec.Files, "config.json")
			Expect(k8sClient.Update(ctx, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed(), "the old Job must remain until its writer pods are gone")
			Expect(job.DeletionTimestamp).NotTo(BeNil())
			Expect(job.Finalizers).To(ContainElement(metav1.FinalizerDeleteDependents))

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			Expect(job.UID).To(Equal(oldUID))
			Expect(job.Annotations[annotationPrefetchFingerprint]).To(Equal(oldFingerprint))

			// envtest has no garbage collector or Job controller. No pods
			// were started, so finish foreground deletion explicitly.
			job.Finalizers = nil
			Expect(k8sClient.Update(ctx, job)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			Expect(job.UID).NotTo(Equal(oldUID))
			Expect(job.Annotations[annotationPrefetchFingerprint]).NotTo(Equal(oldFingerprint))
		})

		It("creates the Job and shared cache PVC, then completes on Job success", func() {
			model := newPrefetchModel("model-prefetch-flow")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			r := prefetchReconciler()

			// First pass: creates PVC + Job, sets Downloading.
			handled, result, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(prefetchPollInterval))

			job := &batchv1.Job{}
			jobKey := types.NamespacedName{Name: "model-prefetch-flow-prefetch", Namespace: ns}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			Expect(job.OwnerReferences).To(HaveLen(1))
			Expect(job.OwnerReferences[0].Kind).To(Equal("Model"))
			Expect(job.Spec.Template.Spec.InitContainers).NotTo(BeEmpty())
			Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ModelCachePVCName, Namespace: ns}, pvc)).To(Succeed())

			updated := &inferencev1alpha1.Model{}
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseDownloading))
			Expect(updated.Status.CacheKey).NotTo(BeEmpty())
			Expect(updated.Status.Prefetch).To(HaveLen(1))
			Expect(updated.Status.Prefetch[0].ClaimName).To(Equal(ModelCachePVCName))
			Expect(updated.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			Expect(updated.Status.Prefetch[0].JobName).To(Equal(jobKey.Name))
			Expect(updated.Status.Prefetch[0].CacheKey).To(Equal(updated.Status.CacheKey))

			// Second pass with the Job still running: no duplicate, polls again.
			handled, result, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(prefetchPollInterval))

			// Mark the Job complete; next pass promotes the Model to Ready.
			completeJob(jobKey)

			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			handled, _, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseReady))
			Expect(updated.Status.CacheKey).To(Equal(effectiveModelCacheKey(updated)))
			Expect(updated.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
			Expect(updated.Status.Prefetch[0].ObservedGeneration).To(Equal(updated.Generation))
			available := meta.FindStatusCondition(updated.Status.Conditions, ConditionAvailable)
			Expect(available).NotTo(BeNil())
			Expect(available.ObservedGeneration).To(Equal(updated.Generation))

			// Once Ready with a cache key, prefetch steps aside for the
			// ordinary remote-source reconcile.
			handled, _, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())
		})

		// Pins that the new fields survive the API server round trip (CRD schema +
		// DeepCopy) and land on the created Job, not just the built object.
		It("persists prefetchNodeSelector and prefetchAffinity onto the created Job", func() {
			model := newPrefetchModel("model-prefetch-selector")
			model.Spec.PrefetchNodeSelector = map[string]string{"kubernetes.io/hostname": "worker-1"}
			model.Spec.PrefetchAffinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "nvidia.com/gpu.present", Operator: corev1.NodeSelectorOpDoesNotExist,
					}}}},
				},
			}}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			handled, _, err := prefetchReconciler().reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "model-prefetch-selector-prefetch", Namespace: ns}, job)).To(Succeed())
			Expect(job.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("kubernetes.io/hostname", "worker-1"))
			Expect(job.Spec.Template.Spec.Affinity).NotTo(BeNil())
			terms := job.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
			Expect(terms).To(HaveLen(1))
			Expect(terms[0].MatchExpressions[0].Key).To(Equal("nvidia.com/gpu.present"))
			Expect(terms[0].MatchExpressions[0].Operator).To(Equal(corev1.NodeSelectorOpDoesNotExist))
		})

		It("marks the Model Failed when the Job fails", func() {
			model := newPrefetchModel("model-prefetch-fail")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			r := prefetchReconciler()
			handled, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			failJob(types.NamespacedName{Name: "model-prefetch-fail-prefetch", Namespace: ns})

			updated := &inferencev1alpha1.Model{}
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			handled, _, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseFailed))
			Expect(updated.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseFailed))
			progressing := meta.FindStatusCondition(updated.Status.Conditions, ConditionProgressing)
			Expect(progressing).NotTo(BeNil())
			Expect(progressing.Reason).To(Equal(ReasonPrefetchFailed))
		})

		// A placement that keeps the download off the GPU nodes cannot reach a
		// claim that lives on one of them. The Job then never completes and
		// never fails, so the status must say why instead of "downloading".
		It("names the scheduler's reason while the Job's pod cannot be scheduled", func() {
			model := newPrefetchModel("model-prefetch-unschedulable")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			jobName := prefetchJobName(model)
			r := prefetchReconciler()
			recorder := events.NewFakeRecorder(20)
			r.Recorder = recorder

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())

			// envtest runs no Job controller or scheduler: stand in for both.
			const schedulerMsg = "0/2 nodes are available: 1 node(s) had volume node affinity conflict, 1 node(s) didn't match Pod's node affinity/selector."
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: jobName + "-abcde", Namespace: ns, Labels: map[string]string{"batch.kubernetes.io/job-name": jobName}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "busybox"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pod) }()
			pod.Status.Phase = corev1.PodPending
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: schedulerMsg}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseDownloading))
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("cannot be scheduled"))
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("volume node affinity conflict"))
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("delete the Job to retry"))
			progressing := meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing)
			Expect(progressing).NotTo(BeNil())
			Expect(progressing.Reason).To(Equal(ReasonPrefetchUnschedulable))
			Expect(progressing.Message).To(ContainSubstring("volume node affinity conflict"))
			Expect(drainEvents(recorder)).To(ContainElement(HavePrefix("Warning " + ReasonPrefetchUnschedulable)))

			By("clearing once the pod schedules")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: ns}, pod)).To(Succeed())
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Prefetch[0].Message).NotTo(ContainSubstring("cannot be scheduled"))
			progressing = meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing)
			Expect(progressing.Reason).To(Equal(ReasonPrefetchRunning))
		})

		// The status records that a claim was filled; it cannot see the claim.
		// A Job that is gone (its TTL, or a deliberate delete to retry) is
		// run again as a check, with the Model's Ready left standing.
		It("re-verifies the claim when its Job is gone, without leaving Ready", func() {
			model := newPrefetchModel("model-prefetch-reverify")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
			r := prefetchReconciler()

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			completeJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseReady))

			By("the Job going away")
			Expect(k8sClient.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: ns}},
				client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{}))
			}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			handled, result, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse(), "a Ready Model keeps its ordinary reconcile during a re-verification")
			Expect(result.RequeueAfter).To(Equal(prefetchPollInterval))
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseReady), "a re-verification does not take Ready away")
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded), "consumers are not held for a re-verification")
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("re-verifying"))
			progressing := meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing)
			Expect(progressing).NotTo(BeNil())
			Expect(progressing.Status).To(Equal(metav1.ConditionTrue))
			Expect(progressing.Reason).To(Equal(ReasonPrefetchReverifying))
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			Expect(job.Annotations).To(HaveKeyWithValue(annotationPrefetchReverify, "true"))

			By("a later pass keeping the re-verification context")
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseReady))
			Expect(meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing).Reason).To(Equal(ReasonPrefetchReverifying))

			By("the check completing")
			stagedAt := model.Status.Prefetch[0].LastTransitionTime
			time.Sleep(1100 * time.Millisecond)
			completeJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			handled, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseReady))
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
			Expect(model.Status.Prefetch[0].Message).To(BeEmpty())
			Expect(model.Status.Prefetch[0].LastTransitionTime.After(stagedAt.Time)).To(BeTrue(), "a passed check is a new staging")
			progressing = meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing)
			Expect(progressing.Status).To(Equal(metav1.ConditionFalse))
			Expect(progressing.Reason).To(Equal(ReasonPrefetchComplete))

			By("a failed check moving the entry to Failed")
			Expect(k8sClient.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: ns}},
				client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{}))
			}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			failJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseFailed))
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("re-verification"))
		})

		// pruneStalePrefetch compacts status.prefetch in place, so a pointer
		// taken before it can name another claim's entry afterwards.
		It("reads its own entry after stale entries are pruned", func() {
			model := newPrefetchModel("model-prefetch-stale-order")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			r := prefetchReconciler()
			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			own := model.Status.Prefetch[0]
			Expect(own.Own).To(BeTrue())

			// A stale service entry ahead of the own one, and a current
			// Succeeded one behind it: the prune drops the first and shifts
			// the third into the own entry's slot.
			model.Status.Prefetch = []inferencev1alpha1.PrefetchStatus{
				{ClaimName: "stale-cache", Phase: inferencev1alpha1.PrefetchPhaseRunning, JobName: "gone-job", Fingerprint: own.Fingerprint, CacheKey: own.CacheKey},
				own,
				{ClaimName: "svc-cache", Phase: inferencev1alpha1.PrefetchPhaseSucceeded, Fingerprint: own.Fingerprint, CacheKey: own.CacheKey, ObservedGeneration: model.Generation},
			}
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseDownloading), "the own Job is still running; a Succeeded neighbour must not complete the Model")
			Expect(model.Status.Prefetch).To(HaveLen(2))
			Expect(findPrefetchStatus(model, ModelCachePVCName).Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			Expect(findPrefetchStatus(model, "stale-cache")).To(BeNil())
		})

		// Two targets must never resolve one Job name: after the own target
		// moves claims, the claim it left keeps its Job and the new claim gets
		// its own.
		It("gives a second own target a claim-scoped Job when the claim-less name is in use", func() {
			createPVC("cache-s")
			createPVC("cache-b")
			m := newPrefetchModel("model-prefetch-move")
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "cache-s"}
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()
			svc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "stays-on-s", Namespace: ns},
				Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: m.Name, ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "cache-s", Prefetch: true}},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, svc) }()
			modelKey := types.NamespacedName{Name: m.Name, Namespace: ns}
			legacyKey := types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}
			r := prefetchReconciler()

			_, _, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			completeJob(legacyKey)
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseReady))
			Expect(findPrefetchStatus(m, "cache-s").JobName).To(Equal(legacyKey.Name))

			m.Spec.ModelCache.ClaimName = "cache-b"
			Expect(k8sClient.Update(ctx, m)).To(Succeed())
			for i := 0; i < 3; i++ {
				Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
				_, _, err = r.reconcilePrefetch(ctx, m)
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			entryB := findPrefetchStatus(m, "cache-b")
			Expect(entryB).NotTo(BeNil())
			Expect(entryB.Own).To(BeTrue())
			Expect(entryB.JobName).To(Equal(prefetchTargetJobName(m, "cache-b")))
			Expect(entryB.Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			entryS := findPrefetchStatus(m, "cache-s")
			Expect(entryS.Own).To(BeFalse())
			Expect(entryS.JobName).To(Equal(legacyKey.Name))
			Expect(entryS.Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded), "the claim the own target left keeps its staging")
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, legacyKey, job)).To(Succeed(), "the service target's Job is not replaced")
			Expect(job.Annotations).To(HaveKeyWithValue(annotationPrefetchClaim, "cache-s"))
			Expect(m.Status.Phase).To(Equal(PhaseDownloading))
		})

		// A failed InferenceService list leaves the service requests unknown;
		// acting on "no services" would delete their in-flight Jobs.
		It("keeps service-requested Jobs when the InferenceService listing fails", func() {
			createPVC("svc-cache-lf")
			model := newPrefetchModel("model-prefetch-listfail")
			model.Spec.Prefetch = false
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			svc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "lf-svc", Namespace: ns},
				Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name, ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "svc-cache-lf", Prefetch: true}},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, svc) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchTargetJobName(model, "svc-cache-lf"), Namespace: ns}

			_, _, err := prefetchReconciler().reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed())

			base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			failing := prefetchReconciler()
			failing.Client = interceptor.NewClient(base, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*inferencev1alpha1.InferenceServiceList); ok {
						return errors.New("informer cache not synced")
					}
					return c.List(ctx, list, opts...)
				},
			})
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			handled, result, err := failing.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())
			Expect(result.RequeueAfter).To(Equal(prefetchClaimWait))
			Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed(), "an in-flight download survives a listing failure")
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(findPrefetchStatus(model, "svc-cache-lf")).NotTo(BeNil())
		})

		// Without the listing the agreed claim is unknown: a target on record
		// stays, and a Model with none holds. Falling back to the shared cache
		// would start a second own Job there, which a cache hit finishes
		// first, pinning the own target to the shared cache.
		It("keeps the own target on record through a listing failure, and holds without one", func() {
			createPVC("node-b-lf")
			model := newPrefetchModel("model-prefetch-lf-own")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			svc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "lf-own-svc", Namespace: ns},
				Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name, ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "node-b-lf"}},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, svc) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			ownKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
			sharedKey := types.NamespacedName{Name: prefetchTargetJobName(model, ModelCachePVCName), Namespace: ns}

			_, _, err := prefetchReconciler().reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, ownKey, job)).To(Succeed())
			Expect(jobClaim(job)).To(Equal("node-b-lf"), "the single referencing service's claim is the agreed target")

			base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			failing := prefetchReconciler()
			failing.Client = interceptor.NewClient(base, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*inferencev1alpha1.InferenceServiceList); ok {
						return errors.New("informer cache not synced")
					}
					return c.List(ctx, list, opts...)
				},
			})
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = failing.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, sharedKey, &batchv1.Job{}))).To(BeTrue(), "no second own Job into the shared cache")
			Expect(findPrefetchStatus(model, ModelCachePVCName)).To(BeNil())
			own := findPrefetchStatus(model, "node-b-lf")
			Expect(own).NotTo(BeNil())
			Expect(own.Own).To(BeTrue())
			Expect(model.Status.Phase).To(Equal(PhaseDownloading))

			By("a Model with no target on record holding instead of guessing")
			fresh := newPrefetchModel("model-prefetch-lf-fresh")
			Expect(k8sClient.Create(ctx, fresh)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, fresh) }()
			freshKey := types.NamespacedName{Name: fresh.Name, Namespace: ns}
			handled, result, err := failing.reconcilePrefetch(ctx, fresh)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(prefetchClaimWait))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: prefetchJobName(fresh), Namespace: ns}, &batchv1.Job{}))).To(BeTrue())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: prefetchTargetJobName(fresh, ModelCachePVCName), Namespace: ns}, &batchv1.Job{}))).To(BeTrue())
			Expect(k8sClient.Get(ctx, freshKey, fresh)).To(Succeed())
			Expect(fresh.Status.Prefetch).To(BeEmpty())
			Expect(fresh.Status.Phase).To(Equal("Pending"))
			progressing := meta.FindStatusCondition(fresh.Status.Conditions, ConditionProgressing)
			Expect(progressing).NotTo(BeNil())
			Expect(progressing.Reason).To(Equal(ReasonPrefetchListingFailed))
		})

		It("records the staged names for a Job adopted from a controller that wrote none", func() {
			model := newPrefetchModel("model-prefetch-adopt")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: ns,
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(model, inferencev1alpha1.GroupVersion.WithKind("Model"))}},
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "download", Image: "busybox"}},
				}}},
			}
			Expect(k8sClient.Create(ctx, job)).To(Succeed())
			completeJob(jobKey)
			_, _, err := prefetchReconciler().reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			entry := findPrefetchStatus(model, ModelCachePVCName)
			Expect(entry).NotTo(BeNil())
			Expect(entry.JobName).To(Equal(jobKey.Name))
			Expect(entry.Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
			Expect(entry.Files).To(Equal([]string{canonicalModelBasename(model)}), "an upgraded Model gets the recorded-names protection too")
		})

		It("creates the shared claim with the Model's storageClassName", func() {
			classNs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prefetch-class-test"}}
			Expect(k8sClient.Create(ctx, classNs)).To(Succeed())
			model := newPrefetchModel("model-prefetch-class")
			model.Namespace = classNs.Name
			model.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{StorageClassName: "fast-nvme"}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			_, _, err := prefetchReconciler().reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ModelCachePVCName, Namespace: classNs.Name}, pvc)).To(Succeed())
			Expect(pvc.Spec.StorageClassName).NotTo(BeNil())
			Expect(*pvc.Spec.StorageClassName).To(Equal("fast-nvme"), "the class the inheriting services ask for, so none gets ModelCacheStorageClassIgnored")
		})

		// A claim a service requested is not the Model's own target: in
		// perService mode that claim goes away with the service.
		It("does not adopt a service's staged claim as its own target", func() {
			m := newPrefetchModel("model-prefetch-own-sticky")
			m.Spec.Prefetch = false
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()
			svc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "ps-svc", Namespace: ns},
				Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: m.Name, ModelCache: &inferencev1alpha1.ModelCacheSpec{Prefetch: true}},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, svc) }()
			createPVC("ps-svc-model-cache")
			modelKey := types.NamespacedName{Name: m.Name, Namespace: ns}
			r := prefetchReconciler()
			r.ModelCacheMode = ModelCacheModePerService

			_, _, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			completeJob(types.NamespacedName{Name: prefetchTargetJobName(m, "ps-svc-model-cache"), Namespace: ns})
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			svcEntry := findPrefetchStatus(m, "ps-svc-model-cache")
			Expect(svcEntry.Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
			Expect(svcEntry.Own).To(BeFalse())

			By("the Model asking for its own prefetch afterwards")
			m.Spec.Prefetch = true
			Expect(k8sClient.Update(ctx, m)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseDownloading), "the Model does not read itself as prefetched off a service's claim")
			ownEntry := findPrefetchStatus(m, ModelCachePVCName)
			Expect(ownEntry).NotTo(BeNil())
			Expect(ownEntry.Own).To(BeTrue())
			Expect(ownEntry.Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
		})

		// Consumers serve the names the entry recorded; a re-verification
		// keeps them, and names that would differ mean a fresh staging.
		It("keeps recorded names through a re-verification, and checks the recorded file when the derived name moves", func() {
			model := newPrefetchModel("model-prefetch-names")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
			r := prefetchReconciler()
			deleteJob := func() {
				Expect(k8sClient.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: ns}},
					client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{}))
				}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())
			}

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			completeJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			recorded := append([]string(nil), model.Status.Prefetch[0].Files...)
			Expect(recorded).To(Equal([]string{canonicalModelBasename(model)}))

			By("a re-verification keeping the recorded names")
			deleteJob()
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
			Expect(model.Status.Prefetch[0].Files).To(Equal(recorded))

			By("metadata parsed after the staging moving the derived name: the check still targets the recorded file")
			completeJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			model.Status.GGUF = &inferencev1alpha1.GGUFMetadata{ModelName: "Renamed By Metadata"}
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			derived := canonicalModelBasename(model)
			Expect(derived).NotTo(Equal(recorded[0]))
			deleteJob()
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded), "a re-verification, not a fresh staging and a rollout")
			Expect(model.Status.Prefetch[0].Files).To(Equal(recorded), "consumers keep serving the recorded name")
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			Expect(job.Annotations).To(HaveKeyWithValue(annotationPrefetchReverify, "true"))
			paths := []string{}
			for _, c := range job.Spec.Template.Spec.InitContainers {
				for _, e := range c.Env {
					if e.Name == "MODEL_PATH" {
						paths = append(paths, e.Value)
					}
				}
			}
			Expect(paths).NotTo(BeEmpty())
			for _, p := range paths {
				Expect(p).To(HaveSuffix("/"+recorded[0]), "the Job checks the file consumers serve")
				Expect(p).NotTo(HaveSuffix("/" + derived))
			}
		})

		// A single-writer claim a pod has attached cannot take a check Job on
		// another node; a routine re-verification waits, a re-stage a service
		// asked for does not.
		It("waits to re-verify a single-writer claim a pod mounts, but re-stages on a service's report", func() {
			createPVC("rwo-cache")
			model := newPrefetchModel("model-prefetch-rwo")
			model.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "rwo-cache"}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
			r := prefetchReconciler()
			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			completeJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "rwo-consumer", Namespace: ns},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "x", Image: "busybox", VolumeMounts: []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models"}}}},
					Volumes:    []corev1.Volume{{Name: "model-cache", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "rwo-cache"}}}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pod) }()

			Expect(k8sClient.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: ns}},
				client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{}))
			}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, result, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(prefetchReverifyRetry))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{}))).To(BeTrue(), "no check Job while a pod has the single-writer claim")
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("rwo-consumer"))

			By("a Model edit outside the fingerprint being confirmed while the claim is held")
			gen := model.Generation
			model.Spec.PrefetchTolerations = []corev1.Toleration{{Key: "held", Operator: corev1.TolerationOpExists}}
			Expect(k8sClient.Update(ctx, model)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Generation).To(BeNumerically(">", gen))
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
			Expect(model.Status.Prefetch[0].ObservedGeneration).To(Equal(model.Generation),
				"the claim holds what the fingerprint describes, so a consumer is not held on the new generation")
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{}))).To(BeTrue(), "still no check Job while the claim is held")

			By("a consuming service reporting the files missing")
			svc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "rwo-svc", Namespace: ns},
				Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name, ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "rwo-cache", Prefetch: true}},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, svc) }()
			meta.SetStatusCondition(&svc.Status.Conditions, metav1.Condition{Type: ConditionModelTransferHealthy, Status: metav1.ConditionFalse, Reason: ReasonPrefetchedFilesMissing, Message: "x"})
			Expect(k8sClient.Status().Update(ctx, svc)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed(), "the re-stage the service asked for runs")
		})

		// A Job emits no event for a pod that cannot schedule or start; the
		// poll that notices it must survive the own target finishing.
		It("keeps a service target's poll when the own target completes or fails", func() {
			for _, tc := range []struct {
				suffix string
				end    func(types.NamespacedName)
				phase  string
			}{
				{"done", completeJob, PhaseReady},
				{"failed", func(key types.NamespacedName) { markJobFailed(ctx, key) }, PhaseFailed},
			} {
				createPVC("own-" + tc.suffix)
				createPVC("svc-" + tc.suffix)
				model := newPrefetchModel("model-prefetch-poll-" + tc.suffix)
				model.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "own-" + tc.suffix}
				Expect(k8sClient.Create(ctx, model)).To(Succeed())
				defer func() { _ = k8sClient.Delete(ctx, model) }()
				svc := &inferencev1alpha1.InferenceService{
					ObjectMeta: metav1.ObjectMeta{Name: "poll-" + tc.suffix, Namespace: ns},
					Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name, ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "svc-" + tc.suffix, Prefetch: true}},
				}
				Expect(k8sClient.Create(ctx, svc)).To(Succeed())
				defer func() { _ = k8sClient.Delete(ctx, svc) }()
				modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
				ownKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
				svcKey := types.NamespacedName{Name: prefetchTargetJobName(model, "svc-"+tc.suffix), Namespace: ns}
				r := prefetchReconciler()

				_, _, err := r.reconcilePrefetch(ctx, model)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, ownKey, &batchv1.Job{})).To(Succeed())
				Expect(k8sClient.Get(ctx, svcKey, &batchv1.Job{})).To(Succeed(), "two targets, two Jobs")

				tc.end(ownKey)
				Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
				_, result, err := r.reconcilePrefetch(ctx, model)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
				Expect(model.Status.Phase).To(Equal(tc.phase))
				Expect(findPrefetchStatus(model, "svc-"+tc.suffix).Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
				Expect(result.RequeueAfter).To(Equal(prefetchPollInterval),
					"the service target's Job is still running; without the poll an unschedulable pod of it is never named")
			}
		})

		It("names a scheduled pod that has not started as stalled", func() {
			model := newPrefetchModel("model-prefetch-stalled")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			jobName := prefetchJobName(model)
			r := prefetchReconciler()
			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())

			previous := prefetchPodStartGrace
			prefetchPodStartGrace = 0
			defer func() { prefetchPodStartGrace = previous }()
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: jobName + "-stuck", Namespace: ns, Labels: map[string]string{"batch.kubernetes.io/job-name": jobName}},
				Spec:       corev1.PodSpec{NodeName: "worker-1", Containers: []corev1.Container{{Name: "x", Image: "busybox"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pod) }()
			pod.Status.Phase = corev1.PodPending
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "model-cache-prep", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ContainerCreating", Message: "Multi-Attach error for volume pvc-1: Volume is already used by pod(s) serving-0"}}}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("has been Pending"))
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("Multi-Attach"))
			first, firstRV := model.Status.Prefetch[0].Message, model.ResourceVersion
			time.Sleep(1100 * time.Millisecond)
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Prefetch[0].Message).To(Equal(first), "a stalled pod's message must not change every pass: each change is a status write and a new event")
			Expect(model.ResourceVersion).To(Equal(firstRV))
			Expect(meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing).Reason).To(Equal(ReasonPrefetchStalled))
		})

		// #2189: the kubelet holds a pod at Pending until every init container
		// has finished, so a multi-file download spends its whole run there.
		// Seen at the Downloading transition, the pod is progressing.
		It("keeps a Pending pod whose downloader init container is running as downloading", func() {
			model := newPrefetchModel("model-prefetch-downloading")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			r := prefetchReconciler()
			recorder := events.NewFakeRecorder(20)
			r.Recorder = recorder

			previous := prefetchPodStartGrace
			prefetchPodStartGrace = 0
			defer func() { prefetchPodStartGrace = previous }()

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())

			pod := pendingPrefetchPod(model, "downloading")
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pod) }()
			prefetchPodStatus(pod, corev1.PodPending, append(terminatedPrefetchInitStatuses(pod, 1), corev1.ContainerStatus{
				Name:  pod.Spec.InitContainers[len(pod.Spec.InitContainers)-1].Name,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
			}), waitingPrefetchMainStatuses(pod, "PodInitializing"))

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, result, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			assertNotStalled(model, result)
			Expect(drainEvents(recorder)).NotTo(ContainElement(HavePrefix("Warning " + ReasonPrefetchStalled)))
		})

		// The same pod on the ordering the issue describes: the Model is
		// already Downloading when the downloader starts, so the verdict is
		// recomputed every poll with no transition to carry it.
		It("keeps downloading when the downloader starts after the Downloading transition", func() {
			model := newPrefetchModel("model-prefetch-late-running")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			r := prefetchReconciler()
			recorder := events.NewFakeRecorder(20)
			r.Recorder = recorder

			previous := prefetchPodStartGrace
			prefetchPodStartGrace = 0
			defer func() { prefetchPodStartGrace = previous }()

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseDownloading), "fixture: Downloading is set before the pod appears")

			pod := pendingPrefetchPod(model, "late")
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pod) }()
			prefetchPodStatus(pod, corev1.PodPending, append(terminatedPrefetchInitStatuses(pod, 1), corev1.ContainerStatus{
				Name:  pod.Spec.InitContainers[len(pod.Spec.InitContainers)-1].Name,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
			}), waitingPrefetchMainStatuses(pod, "PodInitializing"))

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, result, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			assertNotStalled(model, result)
			Expect(meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing).Reason).To(Equal(ReasonPrefetchStarted))
			Expect(drainEvents(recorder)).NotTo(ContainElement(HavePrefix("Warning " + ReasonPrefetchStalled)))
		})

		// Past the grace with nothing running, the only waiting status can be
		// the kubelet's placeholder for a container behind one it has not
		// reached: that names a position in the init sequence, not a cause.
		It("does not name PodInitializing as the cause for a Pending pod that has not started", func() {
			model := newPrefetchModel("model-prefetch-initializing")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			r := prefetchReconciler()

			previous := prefetchPodStartGrace
			prefetchPodStartGrace = 0
			defer func() { prefetchPodStartGrace = previous }()

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())

			pod := pendingPrefetchPod(model, "initializing")
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pod) }()
			prefetchPodStatus(pod, corev1.PodPending, terminatedPrefetchInitStatuses(pod, len(pod.Spec.InitContainers)),
				waitingPrefetchMainStatuses(pod, "PodInitializing"))

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("no container has started"))
			Expect(model.Status.Prefetch[0].Message).NotTo(ContainSubstring("PodInitializing"))
			Expect(meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing).Reason).To(Equal(ReasonPrefetchStalled))
		})

		// The stalled classes the fix keeps: a real waiting reason, in an init
		// container or in the job's own container, and no report inside the
		// grace period.
		DescribeTable("still names the container that is actually waiting",
			func(slug string, stage func(pod *corev1.Pod), grace time.Duration, wantContainer string) {
				model := newPrefetchModel("model-prefetch-" + slug)
				Expect(k8sClient.Create(ctx, model)).To(Succeed())
				defer func() { _ = k8sClient.Delete(ctx, model) }()
				modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
				r := prefetchReconciler()

				previous := prefetchPodStartGrace
				prefetchPodStartGrace = grace
				defer func() { prefetchPodStartGrace = previous }()

				_, _, err := r.reconcilePrefetch(ctx, model)
				Expect(err).NotTo(HaveOccurred())

				pod := pendingPrefetchPod(model, "waiting")
				Expect(k8sClient.Create(ctx, pod)).To(Succeed())
				defer func() { _ = k8sClient.Delete(ctx, pod) }()
				stage(pod)
				prefetchPodStatus(pod, corev1.PodPending, pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses)

				Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
				_, _, err = r.reconcilePrefetch(ctx, model)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
				if wantContainer == "" {
					Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("downloading into"),
						"a pod inside the grace period has not stalled")
					return
				}
				Expect(model.Status.Prefetch[0].Message).To(ContainSubstring(wantContainer + " is waiting"))
				Expect(meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing).Reason).To(Equal(ReasonPrefetchStalled))
			},
			Entry("the first init container cannot attach its volume", "waiting-first-init",
				func(pod *corev1.Pod) {
					pod.Status.InitContainerStatuses = waitingPrefetchInitStatuses(pod, "ContainerCreating")
					pod.Status.InitContainerStatuses[0].State.Waiting.Message = "Multi-Attach error for volume pvc-1: Volume is already used by pod(s) serving-0"
				}, time.Duration(0), "model-cache-prep"),
			Entry("a later init container cannot pull its image", "waiting-later-init",
				func(pod *corev1.Pod) {
					pod.Status.InitContainerStatuses = append(terminatedPrefetchInitStatuses(pod, 1), corev1.ContainerStatus{
						Name:  pod.Spec.InitContainers[len(pod.Spec.InitContainers)-1].Name,
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "pull access denied"}},
					})
				}, time.Duration(0), "model-downloader"),
			Entry("every init container finished but the job's container cannot start", "waiting-main",
				func(pod *corev1.Pod) {
					pod.Status.InitContainerStatuses = terminatedPrefetchInitStatuses(pod, len(pod.Spec.InitContainers))
					pod.Status.ContainerStatuses = waitingPrefetchMainStatuses(pod, "ContainerCreating")
				}, time.Duration(0), "prefetch-done"),
			Entry("inside the grace period", "waiting-within-grace",
				func(pod *corev1.Pod) {
					pod.Status.InitContainerStatuses = waitingPrefetchInitStatuses(pod, "ContainerCreating")
				}, time.Hour, ""),
		)

		It("clears PrefetchStalled when the pod leaves Pending", func() {
			model := newPrefetchModel("model-prefetch-cleared")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			r := prefetchReconciler()

			previous := prefetchPodStartGrace
			prefetchPodStartGrace = 0
			defer func() { prefetchPodStartGrace = previous }()

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())

			pod := pendingPrefetchPod(model, "cleared")
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pod) }()
			prefetchPodStatus(pod, corev1.PodPending, waitingPrefetchInitStatuses(pod, "ContainerCreating"), nil)

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing).Reason).To(Equal(ReasonPrefetchStalled))

			prefetchPodStatus(pod, corev1.PodRunning, waitingPrefetchInitStatuses(pod, "ContainerCreating"), nil)

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("downloading into"))
			Expect(meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing).Reason).To(Equal(ReasonPrefetchRunning))
		})

		// A stalled pod observed after the Model is already Downloading still
		// reports the stall, on the entry and on Progressing. The fix must not
		// change this class: nothing in the pod is Running there.
		It("reports the stall when a stalled pod is observed after Downloading", func() {
			model := newPrefetchModel("model-prefetch-late-stall")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			r := prefetchReconciler()

			previous := prefetchPodStartGrace
			prefetchPodStartGrace = 0
			defer func() { prefetchPodStartGrace = previous }()

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseDownloading), "fixture: the Model has reached Downloading before the pod appears")

			pod := pendingPrefetchPod(model, "late")
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pod) }()
			prefetchPodStatus(pod, corev1.PodPending, waitingPrefetchInitStatuses(pod, "ErrImagePull"), nil)

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("has been Pending"))
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("model-cache-prep is waiting"))
			Expect(meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing).Reason).To(Equal(ReasonPrefetchStalled))
		})

		// The Job name is deterministic, so a user's Job can sit at it. It is
		// neither adopted as the prefetch nor deleted to make room.
		It("neither adopts nor deletes a Job at the prefetch name that the Model does not own", func() {
			model := newPrefetchModel("model-prefetch-foreign")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			jobKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
			foreign := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: ns},
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "x", Image: "busybox"}},
				}}},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, foreign) }()
			// Complete it, so adopting it would wrongly mark the Model Ready.
			completeJob(jobKey)

			r := prefetchReconciler()
			for i := 0; i < 2; i++ {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: model.Name, Namespace: ns}, model)).To(Succeed())
				_, _, err := r.reconcilePrefetch(ctx, model)
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: model.Name, Namespace: ns}, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseFailed))
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("not owned by this Model"))
			still := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, still)).To(Succeed())
			Expect(still.UID).To(Equal(foreign.UID), "the foreign Job is left alone")
		})

		// #2136: a prefetch is keyed by what it staged. A source change needs
		// a new download; a placement change does not.
		It("replaces a completed Job when the spec changes what is staged", func() {
			model := newPrefetchModel("model-prefetch-respec")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
			r := prefetchReconciler()

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			completeJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseReady))
			oldFingerprint := model.Status.Prefetch[0].Fingerprint
			oldKey := model.Status.CacheKey

			By("changing the source")
			model.Spec.Source = "https://example.com/models/llama-v2.gguf"
			Expect(k8sClient.Update(ctx, model)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())

			handled, result, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(prefetchRetryInterval))
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).NotTo(Equal(PhaseReady), "a Model whose source changed is not Ready on the old files")
			Expect(model.Status.CacheKey).NotTo(Equal(oldKey), "the key moves with the source")

			By("creating a new Job for the new source once the old one is gone")
			Eventually(func() error {
				Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
				_, _, err := r.reconcilePrefetch(ctx, model)
				if err != nil {
					return err
				}
				job := &batchv1.Job{}
				if err := k8sClient.Get(ctx, jobKey, job); err != nil {
					return err
				}
				if job.DeletionTimestamp != nil {
					return apierrors.NewNotFound(batchv1.Resource("jobs"), job.Name)
				}
				if job.Annotations[annotationPrefetchFingerprint] == oldFingerprint {
					return apierrors.NewConflict(batchv1.Resource("jobs"), job.Name, nil)
				}
				return nil
			}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseDownloading))
			Expect(model.Status.Prefetch).To(HaveLen(1))
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			Expect(model.Status.Prefetch[0].Fingerprint).NotTo(Equal(oldFingerprint))
		})

		It("re-records the generation without a new Job when the change does not alter the files", func() {
			model := newPrefetchModel("model-prefetch-regen")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
			r := prefetchReconciler()

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			completeJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			generationBefore := model.Generation
			jobBefore := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, jobBefore)).To(Succeed())

			model.Spec.PrefetchTolerations = []corev1.Toleration{{Key: "downloads", Operator: corev1.TolerationOpExists}}
			Expect(k8sClient.Update(ctx, model)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Generation).To(BeNumerically(">", generationBefore))

			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseReady))
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
			Expect(model.Status.Prefetch[0].ObservedGeneration).To(Equal(model.Generation))
			available := meta.FindStatusCondition(model.Status.Conditions, ConditionAvailable)
			Expect(available.ObservedGeneration).To(Equal(model.Generation))
			jobAfter := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, jobAfter)).To(Succeed())
			Expect(jobAfter.UID).To(Equal(jobBefore.UID), "the completed Job is kept, not replaced")
		})
	})

	Describe("Reconcile integration", func() {
		It("intercepts a prefetch model before the remote download path", func() {
			model := newPrefetchModel("model-prefetch-hook")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			result, err := prefetchReconciler().Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: model.Name, Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(prefetchPollInterval))

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "model-prefetch-hook-prefetch", Namespace: ns}, job)).To(Succeed())

			updated := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: model.Name, Namespace: ns}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseDownloading))
		})
	})

	Describe("PrefetchSkipped", func() {
		It("reports spec.prefetch on a source the downloader does not stage, and clears it when the field goes", func() {
			model := newPrefetchModel("model-prefetch-skipped")
			model.Spec.Source = "pvc://staged-models/llama.gguf"
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			r := prefetchReconciler()
			recorder := events.NewFakeRecorder(20)
			r.Recorder = recorder

			handled, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse(), "an unstageable source is left to its own reconcile")

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			cond := meta.FindStatusCondition(model.Status.Conditions, ConditionPrefetchSkipped)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal(ReasonSourceNotPrefetchable))
			Expect(cond.Message).To(ContainSubstring("pvc://staged-models/llama.gguf"))
			Expect(drainEvents(recorder)).To(ContainElement(HavePrefix("Warning " + ConditionPrefetchSkipped)))
			var jobs batchv1.JobList
			Expect(k8sClient.List(ctx, &jobs, client.InNamespace(ns), client.MatchingLabels{"inference.llmkube.dev/model": model.Name})).To(Succeed())
			Expect(jobs.Items).To(BeEmpty())

			model.Spec.Prefetch = false
			Expect(k8sClient.Update(ctx, model)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(meta.FindStatusCondition(model.Status.Conditions, ConditionPrefetchSkipped)).To(BeNil())
		})
	})

	Describe("#1676 per-service cache claim", func() {
		// Prefetch must stage into the PVC the InferenceService will actually
		// read, not always the shared cache.

		newIsvc := func(modelName, name, claim string) *inferencev1alpha1.InferenceService {
			return &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					ModelCache: &inferencev1alpha1.ModelCacheSpec{
						ClaimName: claim,
					},
				},
			}
		}

		// cleanupNamespacedObjects drains the InferenceServices and PVCs this
		// block creates. It runs BOTH before each case (so a previous case
		// cannot skew a resolution) and after the block (so these objects do
		// not leak into the shared envtest cluster). The AfterEach is
		// load-bearing: federation_edge_controller.go lists InferenceServices
		// across ALL namespaces, and its spec asserts absolute counts, so a
		// leaked service here fails an unrelated test.
		cleanupNamespacedObjects := func() {
			list := &inferencev1alpha1.InferenceServiceList{}
			Expect(k8sClient.List(ctx, list, client.InNamespace(ns))).To(Succeed())
			for i := range list.Items {
				_ = k8sClient.Delete(ctx, &list.Items[i])
			}
			pvcList := &corev1.PersistentVolumeClaimList{}
			Expect(k8sClient.List(ctx, pvcList, client.InNamespace(ns))).To(Succeed())
			for i := range pvcList.Items {
				_ = k8sClient.Delete(ctx, &pvcList.Items[i])
			}
		}

		AfterEach(cleanupNamespacedObjects)
		BeforeEach(cleanupNamespacedObjects)

		It("stages into the single referencing service's claim", func() {
			createPVC("llmkube-node-a-cache")
			Expect(k8sClient.Create(ctx, newIsvc("per-service-model", "svc-a", "llmkube-node-a-cache"))).To(Succeed())
			m := newPrefetchModel("per-service-model")
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			handled, _, err := prefetchReconciler().reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}, job)).To(Succeed())
			Expect(jobClaim(job)).To(Equal("llmkube-node-a-cache"))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: ns}, m)).To(Succeed())
			Expect(m.Status.Prefetch).To(HaveLen(1))
			Expect(m.Status.Prefetch[0].ClaimName).To(Equal("llmkube-node-a-cache"))
		})

		It("resolves the single distinct claim among referencing services", func() {
			r := prefetchReconciler()
			m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "resolve-model", Namespace: ns}}

			Expect(k8sClient.Create(ctx, newIsvc("resolve-model", "svc-a", "llmkube-cache-a"))).To(Succeed())
			services, err := r.referencingServices(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			claim, reason := resolveOwnPrefetchClaim(m, services, nil, "")
			Expect(reason).To(BeEmpty())
			Expect(claim).To(Equal("llmkube-cache-a"))
		})

		It("falls back to the shared cache when services disagree on claimName", func() {
			r := prefetchReconciler()
			m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "conflict-model", Namespace: ns}}

			Expect(k8sClient.Create(ctx, newIsvc("conflict-model", "svc-a", "llmkube-cache-a"))).To(Succeed())
			Expect(k8sClient.Create(ctx, newIsvc("conflict-model", "svc-b", "llmkube-cache-b"))).To(Succeed())

			services, err := r.referencingServices(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			claim, reason := resolveOwnPrefetchClaim(m, services, nil, "")
			Expect(claim).To(Equal(ModelCachePVCName))
			Expect(reason).NotTo(BeEmpty())
		})

		It("falls back to the shared cache when no service references the model", func() {
			r := prefetchReconciler()
			m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "no-svc-model", Namespace: ns}}

			// A service that references a different model must not match.
			Expect(k8sClient.Create(ctx, newIsvc("other-model", "svc-other", "llmkube-cache-a"))).To(Succeed())

			services, err := r.referencingServices(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(services).To(BeEmpty())
			claim, reason := resolveOwnPrefetchClaim(m, services, nil, "")
			Expect(claim).To(Equal(ModelCachePVCName))
			Expect(reason).NotTo(BeEmpty())
		})

		// #2136: once the download has landed, a service that appears later
		// is reported, not re-downloaded for. Its own modelCache.prefetch is
		// the way to ask for that.
		It("keeps the claim already staged when a service appears afterwards, and reports the mismatch", func() {
			m := newPrefetchModel("late-service-model")
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()
			modelKey := types.NamespacedName{Name: m.Name, Namespace: ns}
			r := prefetchReconciler()

			_, _, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			completeJob(types.NamespacedName{Name: prefetchJobName(m), Namespace: ns})
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseReady))

			By("a service naming another claim appearing")
			late := newIsvc(m.Name, "svc-late", "llmkube-cache-late")
			Expect(k8sClient.Create(ctx, late)).To(Succeed())
			handled, _, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse(), "the Model stays Ready on the claim it was staged into")
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Prefetch).To(HaveLen(1))
			Expect(m.Status.Prefetch[0].ClaimName).To(Equal(ModelCachePVCName))
			cond := meta.FindStatusCondition(m.Status.Conditions, ConditionPrefetchTargetMismatch)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Message).To(ContainSubstring("svc-late"))
			Expect(cond.Message).To(ContainSubstring("llmkube-cache-late"))
			var jobs batchv1.JobList
			Expect(k8sClient.List(ctx, &jobs, client.InNamespace(ns), client.MatchingLabels{"inference.llmkube.dev/model": m.Name})).To(Succeed())
			Expect(jobs.Items).To(HaveLen(1), "no second download for a claim nobody asked to prefetch into")

			By("the service asking for its own prefetch")
			createPVC("llmkube-cache-late")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: late.Name, Namespace: ns}, late)).To(Succeed())
			late.Spec.ModelCache.Prefetch = true
			Expect(k8sClient.Update(ctx, late)).To(Succeed())
			handled, result, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())
			Expect(result.RequeueAfter).To(Equal(prefetchPollInterval), "a service-requested prefetch is polled without holding the Model")
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseReady))
			Expect(meta.FindStatusCondition(m.Status.Conditions, ConditionPrefetchTargetMismatch)).To(BeNil())
			Expect(m.Status.Prefetch).To(HaveLen(2))
			lateEntry := findPrefetchStatus(m, "llmkube-cache-late")
			Expect(lateEntry).NotTo(BeNil())
			Expect(lateEntry.Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: prefetchTargetJobName(m, "llmkube-cache-late"), Namespace: ns}, job)).To(Succeed())
			Expect(jobClaim(job)).To(Equal("llmkube-cache-late"))
			Expect(job.Name).To(Equal(lateEntry.JobName))
		})

		// #2136: when a Model and its service are applied together, the Model
		// can reconcile first and start into the shared cache. The service's
		// claim then moves the Job rather than leaving the download where no
		// pod will read it.
		It("retargets a running Job when the service that will read it appears", func() {
			m := newPrefetchModel("retarget-model")
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()
			modelKey := types.NamespacedName{Name: m.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}
			r := prefetchReconciler()

			_, _, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			Expect(jobClaim(job)).To(Equal(ModelCachePVCName))

			createPVC("llmkube-node-a-cache")
			Expect(k8sClient.Create(ctx, newIsvc(m.Name, "svc-a", "llmkube-node-a-cache"))).To(Succeed())

			// The entry names the Job that stages its claim; the abandoned
			// shared-cache Job is deleted rather than reused.
			Eventually(func() string {
				Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
				_, _, err := r.reconcilePrefetch(ctx, m)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
				entry := findPrefetchStatus(m, "llmkube-node-a-cache")
				if entry == nil || entry.JobName == "" {
					return ""
				}
				job := &batchv1.Job{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: entry.JobName, Namespace: ns}, job); err != nil || job.DeletionTimestamp != nil {
					return ""
				}
				return jobClaim(job)
			}, 10*time.Second, 200*time.Millisecond).Should(Equal("llmkube-node-a-cache"))

			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseDownloading))
			Expect(m.Status.Prefetch).To(HaveLen(1), "the abandoned shared-cache entry is dropped")
			Expect(m.Status.Prefetch[0].ClaimName).To(Equal("llmkube-node-a-cache"))
			Expect(m.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{}))).To(BeTrue(), "the shared-cache Job is gone")
		})
	})

	Describe("#2136 spec.modelCache on the Model", func() {
		AfterEach(func() {
			pvcList := &corev1.PersistentVolumeClaimList{}
			Expect(k8sClient.List(ctx, pvcList, client.InNamespace(ns))).To(Succeed())
			for i := range pvcList.Items {
				_ = k8sClient.Delete(ctx, &pvcList.Items[i])
			}
		})

		It("stages into spec.modelCache.claimName with no InferenceService present", func() {
			createPVC("model-cache")
			m := newPrefetchModel("explicit-claim-model")
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "model-cache"}
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			handled, result, err := prefetchReconciler().reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(prefetchPollInterval))

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}, job)).To(Succeed())
			Expect(jobClaim(job)).To(Equal("model-cache"))
			Expect(job.Annotations).To(HaveKeyWithValue(annotationPrefetchClaim, "model-cache"))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: ns}, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseDownloading))
			Expect(m.Status.Prefetch).To(HaveLen(1))
			Expect(m.Status.Prefetch[0].ClaimName).To(Equal("model-cache"))
		})

		It("holds the Job while the named claim does not exist, and starts it once it does", func() {
			m := newPrefetchModel("missing-claim-model")
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "not-yet-created"}
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()
			modelKey := types.NamespacedName{Name: m.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}
			r := prefetchReconciler()

			handled, result, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(prefetchClaimWait))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{}))).To(BeTrue(),
				"a Job pod mounting a claim that does not exist sits Pending forever")
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal("Pending"))
			progressing := meta.FindStatusCondition(m.Status.Conditions, ConditionProgressing)
			Expect(progressing).NotTo(BeNil())
			Expect(progressing.Reason).To(Equal(ReasonPrefetchClaimNotFound))
			Expect(progressing.Message).To(ContainSubstring("not-yet-created"))
			Expect(m.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhasePending))

			createPVC("not-yet-created")
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseDownloading))
		})

		It("is honoured over a service that names another claim, which is then reported", func() {
			createPVC("model-cache")
			svc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "svc-elsewhere", Namespace: ns},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:   "honoured-claim-model",
					ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "somewhere-else"},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, svc) }()
			m := newPrefetchModel("honoured-claim-model")
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "model-cache"}
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			_, _, err := prefetchReconciler().reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}, job)).To(Succeed())
			Expect(jobClaim(job)).To(Equal("model-cache"))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: ns}, m)).To(Succeed())
			cond := meta.FindStatusCondition(m.Status.Conditions, ConditionPrefetchTargetMismatch)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(ContainSubstring("svc-elsewhere"))
			Expect(cond.Message).To(ContainSubstring("somewhere-else"))
		})

		// A claim the Model was staged into stays recorded, but a Job built for
		// it must never be mistaken for the new claim's: the files are not
		// there.
		It("stages again when spec.modelCache.claimName moves to another claim", func() {
			createPVC("cache-a")
			createPVC("cache-b")
			m := newPrefetchModel("moving-claim-model")
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "cache-a"}
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()
			modelKey := types.NamespacedName{Name: m.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}
			r := prefetchReconciler()

			_, _, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			completeJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseReady))

			m.Spec.ModelCache.ClaimName = "cache-b"
			Expect(k8sClient.Update(ctx, m)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			handled, _, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).NotTo(Equal(PhaseReady), "nothing has been staged into cache-b yet")
			entryB := findPrefetchStatus(m, "cache-b")
			Expect(entryB).NotTo(BeNil())
			Expect(entryB.Phase).NotTo(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))

			// cache-b gets a Job of its own; cache-a's Job and record stay.
			Eventually(func() string {
				Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
				_, _, err := r.reconcilePrefetch(ctx, m)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
				entry := findPrefetchStatus(m, "cache-b")
				if entry == nil || entry.JobName == "" {
					return ""
				}
				job := &batchv1.Job{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: entry.JobName, Namespace: ns}, job); err != nil {
					return ""
				}
				return jobClaim(job)
			}, 10*time.Second, 200*time.Millisecond).Should(Equal("cache-b"))
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseDownloading))
			Expect(findPrefetchStatus(m, "cache-b").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			Expect(findPrefetchStatus(m, "cache-b").JobName).NotTo(Equal(jobKey.Name), "a second own claim never reuses a name another entry drives")
			entryA := findPrefetchStatus(m, "cache-a")
			Expect(entryA).NotTo(BeNil(), "the claim already staged stays on record")
			Expect(entryA.Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
			Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed(), "cache-a's Job is not replaced")
		})

		It("rejects prefetch with modelCache.persistence Ephemeral at admission", func() {
			m := newPrefetchModel("ephemeral-prefetch")
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral}
			err := k8sClient.Create(ctx, m)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("prefetch cannot be combined with modelCache.persistence Ephemeral"))

			alias := newPrefetchModel("ephemeral-alias-prefetch")
			alias.Spec.Prefetch = false
			alias.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral, Prefetch: true}
			err = k8sClient.Create(ctx, alias)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("prefetch cannot be set when persistence is Ephemeral"))

			ok := newPrefetchModel("claim-prefetch")
			ok.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "model-cache", Prefetch: true}
			Expect(k8sClient.Create(ctx, ok)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ok)).To(Succeed())
		})
	})

	Describe("prefetchTargets", func() {
		It("skips a service request its pods could not consume", func() {
			r := prefetchReconciler()
			m := newPrefetchModel("targets-model")
			m.Spec.Prefetch = false
			seedPrefetchCacheKey(m)
			svc := func(name, runtime string) *inferencev1alpha1.InferenceService {
				return &inferencev1alpha1.InferenceService{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
					Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: m.Name, Runtime: runtime,
						ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: name + "-cache", Prefetch: true}},
				}
			}
			targets, _ := r.prefetchTargets(m, []*inferencev1alpha1.InferenceService{svc("tgi-svc", "tgi"), svc("llama-svc", "")}, nil, "")
			claims := make([]string, 0, len(targets))
			for _, t := range targets {
				claims = append(claims, t.claim)
			}
			Expect(claims).To(Equal([]string{"llama-svc-cache"}), "a runtime the operator does not stage gets no Job")

			r.ModelCachePath = ""
			targets, _ = r.prefetchTargets(m, []*inferencev1alpha1.InferenceService{svc("llama-svc", "")}, nil, "")
			Expect(targets).To(BeEmpty(), "with caching disabled no claim is ever mounted")
		})
	})

	Describe("prefetch Job names", func() {
		It("fit the pod label a Job's name feeds", func() {
			long := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("llama-3-1-405b-instruct-", 3) + "q4", Namespace: ns}}
			Expect(len(long.Name)).To(BeNumerically(">", 46))
			own := prefetchJobName(long)
			Expect(len(own)).To(BeNumerically("<=", 63))
			Expect(own).To(HaveSuffix("-prefetch"))
			scoped := prefetchTargetJobName(long, "some-claim")
			Expect(len(scoped)).To(BeNumerically("<=", 63))
			Expect(scoped).To(MatchRegexp(`-prefetch-[0-9a-f]{8}$`))
			Expect(scoped).NotTo(ContainSubstring("--prefetch"))

			short := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "llama", Namespace: ns}}
			Expect(prefetchJobName(short)).To(Equal("llama-prefetch"))

			a := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("x", 60) + "-one", Namespace: ns}}
			b := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("x", 60) + "-two", Namespace: ns}}
			Expect(prefetchJobName(a)).NotTo(Equal(prefetchJobName(b)), "names that share their kept prefix must not share a Job")
			Expect(len(prefetchJobName(a))).To(BeNumerically("<=", 63))
			Expect(prefetchTargetJobName(a, "c")).NotTo(Equal(prefetchTargetJobName(b, "c")))
		})
	})

	Describe("prefetchFingerprint", func() {
		It("changes with a pinned spec.sha256", func() {
			m := newPrefetchModel("fp-model")
			seedPrefetchCacheKey(m)
			before := prefetchFingerprint(m, nil)
			m.Spec.SHA256 = strings.Repeat("ab", 32)
			Expect(prefetchFingerprint(m, nil)).NotTo(Equal(before))
			m.Spec.PrefetchTolerations = []corev1.Toleration{{Key: "x", Operator: corev1.TolerationOpExists}}
			Expect(prefetchFingerprint(m, nil)).To(Equal(prefetchFingerprint(m, nil)))
		})
	})

	Describe("InferenceService watch", func() {
		It("maps a service to the Model it references", func() {
			r := prefetchReconciler()
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: ns},
				Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "the-model"},
			}
			Expect(r.findModelForInferenceService(ctx, isvc)).To(ConsistOf(reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "the-model", Namespace: ns},
			}))
			isvc.Spec.ModelRef = ""
			Expect(r.findModelForInferenceService(ctx, isvc)).To(BeEmpty())
		})
	})
})
