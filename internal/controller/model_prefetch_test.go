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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

		It("is true for hf:// sources (normalized to https)", func() {
			m := newPrefetchModel("x")
			m.Spec.Source = "hf://org/repo/model.gguf"
			Expect(prefetchEligible(m)).To(BeTrue())
		})

		It("is true for https sources", func() {
			Expect(prefetchEligible(newPrefetchModel("x"))).To(BeTrue())
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

		It("creates the Job and shared cache PVC, then completes on Job success", func() {
			model := newPrefetchModel("model-prefetch-flow")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			r := prefetchReconciler()

			// First pass: creates PVC + Job, sets Downloading.
			handled, result, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(15 * time.Second))

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

			// Second pass with the Job still running: no duplicate, polls again.
			handled, result, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(15 * time.Second))

			// Mark the Job complete; next pass promotes the Model to Ready.
			// k8s >=1.31 validates the Job status subresource: a finished Job
			// needs startTime/completionTime, and Complete=True requires
			// SuccessCriteriaMet=True first.
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			now := metav1.Now()
			job.Status.StartTime = &now
			job.Status.CompletionTime = &now
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			}
			job.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			handled, _, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseReady))
			Expect(updated.Status.CacheKey).To(Equal(effectiveModelCacheKey(updated)))

			// Once Ready with a cache key, prefetch steps aside for the
			// ordinary remote-source reconcile.
			handled, _, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())
		})

		It("marks the Model Failed when the Job fails", func() {
			model := newPrefetchModel("model-prefetch-fail")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			r := prefetchReconciler()
			handled, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			// Failed=True requires FailureTarget=True and a startTime under
			// k8s >=1.31 Job status validation.
			job := &batchv1.Job{}
			jobKey := types.NamespacedName{Name: "model-prefetch-fail-prefetch", Namespace: ns}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			now := metav1.Now()
			job.Status.StartTime = &now
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "PodFailurePolicy", Message: "test"},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "PodFailurePolicy", Message: "test"},
			}
			job.Status.Failed = 3
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			updated := &inferencev1alpha1.Model{}
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			handled, _, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseFailed))
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
			Expect(result.RequeueAfter).To(Equal(15 * time.Second))

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "model-prefetch-hook-prefetch", Namespace: ns}, job)).To(Succeed())

			updated := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: model.Name, Namespace: ns}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseDownloading))
		})
	})
})
