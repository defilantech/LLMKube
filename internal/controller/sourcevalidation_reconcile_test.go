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
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// GHSA-jw3m-8q7m-f35r: both sinks for an unvalidated local model source must
// be closed. Sink 1 is os.Open in the Model controller's copyLocalModel; sink
// 2 is the HostPathVolumeSource in the InferenceService's generated pod. The
// specs below prove a source that fails validateLocalSourceAllowed reaches
// neither.
var _ = Describe("Model controller host-path allowlist (GHSA-jw3m-8q7m-f35r)", func() {
	It("rejects a local source when no roots are configured and never opens the file", func() {
		// A real, readable file: the guard must reject it WITHOUT touching it,
		// proving rejection is policy-based, not fetch-failure-based.
		srcDir, err := os.MkdirTemp("", "llmkube-ghsa-src-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()
		srcFile := filepath.Join(srcDir, "secret.gguf")
		Expect(os.WriteFile(srcFile, []byte("sensitive-bytes"), 0644)).To(Succeed())

		tempDir, err := os.MkdirTemp("", "llmkube-ghsa-cache-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "model-hostpath-denied"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: fmt.Sprintf("file://%s", srcFile),
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
			// Secure default: no roots.
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred(), "allowlist rejection is unrecoverable; must not feed the rate-limited workqueue")
		Expect(result).To(Equal(reconcile.Result{}))

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseFailed))

		var degraded *metav1.Condition
		for i, cond := range updated.Status.Conditions {
			if cond.Type == ConditionDegraded {
				degraded = &updated.Status.Conditions[i]
			}
		}
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Reason).To(Equal("SourceNotAllowed"))
		Expect(degraded.Message).To(ContainSubstring("GHSA-jw3m-8q7m-f35r"))

		// Sink 1 closed: the controller never copied (never os.Open'ed) the
		// file — its cache directory stays empty.
		entries, err := os.ReadDir(tempDir)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty(), "a rejected local source must never reach copyLocalModel/os.Open")
	})

	It("rejects a local source outside the configured roots", func() {
		tempDir, err := os.MkdirTemp("", "llmkube-ghsa-cache-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "model-hostpath-outside-roots"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "/etc/passwd",
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			StoragePath:          tempDir,
			AllowedHostPathRoots: []string{"/srv/models"},
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseFailed))

		entries, err := os.ReadDir(tempDir)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty())
	})

	It("gates the metal local-path branch too (no Ready for a disallowed source)", func() {
		modelName := "model-hostpath-metal-denied"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "/Users/someone/models/m.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseFailed), "the metal local branch must not mark a disallowed source Ready")
	})

	It("copies a local source that lies within an allowed root", func() {
		srcDir, err := os.MkdirTemp("", "llmkube-ghsa-allowed-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()
		srcFile := filepath.Join(srcDir, "allowed.gguf")
		Expect(os.WriteFile(srcFile, []byte("model-bytes"), 0644)).To(Succeed())

		tempDir, err := os.MkdirTemp("", "llmkube-ghsa-cache-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "model-hostpath-allowed"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: fmt.Sprintf("file://%s", srcFile),
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			StoragePath:          tempDir,
			AllowedHostPathRoots: []string{srcDir},
		}
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseReady))
		Expect(updated.Status.Path).NotTo(BeEmpty())
		_, err = os.Stat(updated.Status.Path)
		Expect(err).NotTo(HaveOccurred(), "the allowed source must actually be copied into the cache")
	})
})

var _ = Describe("InferenceService controller host-path allowlist (GHSA-jw3m-8q7m-f35r)", func() {
	It("blocks the InferenceService and creates no Deployment for a disallowed local source", func() {
		modelName := "model-isvc-hostpath-denied"
		isvcName := "isvc-hostpath-denied"

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "/var/run/secrets/kubernetes.io/serviceaccount/token",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		// Simulate a Model that reached Ready before the allowlist existed
		// (sticky status): the InferenceService-side gate must still block.
		model.Status.Phase = PhaseReady
		model.Status.Path = "/models/somewhere/model.gguf"
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

		reconciler := &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			// Secure default: no roots.
		}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		updated := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseFailed))

		var degraded *metav1.Condition
		for i, cond := range updated.Status.Conditions {
			if cond.Type == ConditionDegraded {
				degraded = &updated.Status.Conditions[i]
			}
		}
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Message).To(ContainSubstring("GHSA-jw3m-8q7m-f35r"))

		// Sink 2 closed: no Deployment exists, so no pod spec (and no
		// HostPathVolumeSource) was ever generated.
		dep := &appsv1.Deployment{}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no Deployment may be created for a disallowed local source")
	})

	It("creates the Deployment without any hostPath volume when the local source is allowed", func() {
		modelName := "model-isvc-hostpath-allowed"
		isvcName := "isvc-hostpath-allowed"

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "/srv/models/m.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		model.Status.Phase = PhaseReady
		model.Status.Path = "/models/somewhere/m.gguf"
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
		}()

		reconciler := &InferenceServiceReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
			AllowedHostPathRoots: []string{"/srv/models"},
		}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: isvcName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: "default"}, dep)).To(Succeed())

		// Without the cache (no ModelCachePath on the reconciler) the local
		// source lands on the emptyDir path, which never mounts a hostPath;
		// the allowlist gate must not have replaced the normal init container
		// with the fail-loud SourceNotAllowed one.
		for _, v := range dep.Spec.Template.Spec.Volumes {
			Expect(v.HostPath).To(BeNil())
		}
		Expect(dep.Spec.Template.Spec.InitContainers).NotTo(BeEmpty())
		Expect(dep.Spec.Template.Spec.InitContainers[0].Command[2]).NotTo(ContainSubstring("SourceNotAllowed"))
	})
})
