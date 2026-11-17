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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The warning wiring for spec.modelCache.runtimeCache (#2117): the reconciler
// has to say why a request was refused instead of dropping the field silently.
// The pod rendering itself is covered without the apiserver in
// runtime_cache_test.go.
var _ = Describe("runtimeCache warning wiring (#2117)", func() {
	var (
		ctx        context.Context
		ns         string
		key        types.NamespacedName
		recorder   *events.FakeRecorder
		reconciler *InferenceServiceReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "llmkube-2117-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns = nsObj.Name
		key = types.NamespacedName{Name: "rc-svc", Namespace: ns}

		recorder = events.NewFakeRecorder(50)
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			Recorder:           recorder,
			ModelCachePath:     "/models",
			ModelCacheMode:     ModelCacheModePerService,
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			DefaultFSGroup:     102,
		}
	})

	AfterEach(func() {
		for _, obj := range []client.Object{
			&inferencev1alpha1.InferenceService{},
			&inferencev1alpha1.Model{},
			&appsv1.Deployment{},
			&corev1.PersistentVolumeClaim{},
		} {
			Expect(client.IgnoreNotFound(k8sClient.DeleteAllOf(ctx, obj, client.InNamespace(ns)))).To(Succeed())
		}
	})

	createService := func(runtime, image string) {
		GinkgoHelper()
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "rc-model", Namespace: ns},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/models/llama.gguf",
				Format:   "gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		model.Status.Phase = PhaseReady
		model.Status.CacheKey = computeCacheKey(model.Spec.Source)
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "rc-model",
				Runtime:  runtime,
				Image:    image,
				ModelCache: &inferencev1alpha1.ModelCacheSpec{
					RuntimeCache: &inferencev1alpha1.RuntimeCacheSpec{Enabled: true},
				},
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
	}

	reconcileOnce := func() {
		GinkgoHelper()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}

	It("reports a runtime that keeps no compile cache", func() {
		createService("llamacpp", "ghcr.io/ggml-org/llama.cpp:server")

		reconcileOnce()

		Expect(drainEvents(recorder)).To(ContainElement(SatisfyAll(
			ContainSubstring("Warning"),
			ContainSubstring(ReasonRuntimeCacheUnsupported),
			ContainSubstring("llamacpp"),
		)))
	})

	It("reports --model-cache-prep=never declining the feature", func() {
		reconciler.ModelCachePrep = ModelCachePrepNever
		createService(RuntimeVLLM, "vllm/vllm-openai:v0.32.0")

		reconcileOnce()

		Expect(drainEvents(recorder)).To(ContainElement(SatisfyAll(
			ContainSubstring("Warning"),
			ContainSubstring(ReasonRuntimeCacheNeedsPrepare),
			ContainSubstring("--model-cache-prep=never"),
		)))
	})
})
