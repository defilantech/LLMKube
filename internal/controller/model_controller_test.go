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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var _ = Describe("Model Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		model := &inferencev1alpha1.Model{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Model")
			err := k8sClient.Get(ctx, typeNamespacedName, model)
			if err != nil && errors.IsNotFound(err) {
				resource := &inferencev1alpha1.Model{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: inferencev1alpha1.ModelSpec{
						Source:       "https://huggingface.co/test/model.gguf",
						Format:       "gguf",
						Quantization: "Q4_K_M",
						Hardware:     &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
						Resources:    &inferencev1alpha1.ResourceRequirements{CPU: "1", Memory: "1Gi"},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &inferencev1alpha1.Model{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Model")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should defer HTTPS sources to the workload init container without downloading", func() {
			By("Reconciling the created resource")
			tempDir, err := os.MkdirTemp("", "llmkube-test-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = os.RemoveAll(tempDir) }()

			controllerReconciler := &ModelReconciler{
				Client:      k8sClient,
				Scheme:      k8sClient.Scheme(),
				StoragePath: tempDir,
			}

			// HTTPS sources are downloaded by the InferenceService Pod's init
			// container into the per-namespace model cache PVC, not by the
			// controller (whose StoragePath lives on the operator-namespace PVC).
			// The controller marks the model Ready immediately so the workload
			// can proceed to download.
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			updated := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseReady))
			Expect(updated.Status.CacheKey).To(HaveLen(16))
			// Controller did not write to its filesystem.
			entries, _ := os.ReadDir(tempDir)
			Expect(entries).To(BeEmpty(), "controller must not write under StoragePath for HTTPS sources")
		})
	})
})

var _ = Describe("Model Controller Reconcile", func() {
	ctx := context.Background()

	It("should return empty result when Model is not found", func() {
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))
	})

	It("should use DefaultModelCachePath when StoragePath is empty", func() {
		modelName := "model-default-path"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://example.com/no-such-model.gguf",
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, model)
		}()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: "",
		}
		// Will fail to download but that's fine — we just check StoragePath was defaulted
		_, _ = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(reconciler.StoragePath).To(Equal(DefaultModelCachePath))
	})

	It("should set Ready when model file already exists in cache", func() {
		modelName := "model-cached"
		// Use a file:// source so the test exercises the controller's cache-hit
		// path. HTTPS sources are deferred to the workload init container and
		// don't go through the in-process cache logic.
		srcDir, err := os.MkdirTemp("", "llmkube-src-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()
		srcFile := filepath.Join(srcDir, "src-model.gguf")
		Expect(os.WriteFile(srcFile, []byte("fake-model-data"), 0644)).To(Succeed())
		source := fmt.Sprintf("file://%s", srcFile)

		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		cacheKey := computeCacheKey(source)
		modelDir := filepath.Join(tempDir, cacheKey)
		Expect(os.MkdirAll(modelDir, 0755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(modelDir, "model.gguf"), []byte("fake-model-data"), 0644)).To(Succeed())

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: source,
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, model)
		}()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseReady))
		Expect(updated.Status.CacheKey).To(Equal(cacheKey))
		Expect(updated.Status.Path).To(ContainSubstring(cacheKey))
		Expect(updated.Status.Size).NotTo(BeEmpty())
		Expect(updated.Status.AcceleratorReady).To(BeTrue())
		Expect(updated.Status.LastUpdated).NotTo(BeNil())
	})

	// HTTPS sources deferred to the workload init container — see issue #363.
	// The controller does NOT download HTTP(S) sources because its filesystem
	// (StoragePath) lives on the operator-namespace cache PVC, which Pods in
	// user namespaces cannot mount. Downloading there means the workload's
	// init container would still re-fetch the model from scratch. The
	// equivalent "controller fetches a non-PVC source and lands at the
	// expected cache path" coverage for sources that DO go through the
	// in-process path lives at "should copy local model file and set Ready"
	// below (file:// sources).
	It("should defer HTTPS source to the InferenceService init container", func() {
		modelName := "model-https-deferred"
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		source := "https://example.com/some-model.gguf"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseReady))
		// CacheKey populated so the InferenceService init container can build
		// the canonical /models/<cacheKey>/<basename> path.
		Expect(updated.Status.CacheKey).To(Equal(computeCacheKey(source)))
		// Controller-side fields stay empty: the workload populates these when
		// it actually downloads the model.
		Expect(updated.Status.Path).To(BeEmpty())
		Expect(updated.Status.SHA256).To(BeEmpty())

		// Critical regression check (issue #363): the controller must not
		// write anything under its own StoragePath for HTTP(S) sources, since
		// that storage is invisible to inference Pods in other namespaces.
		entries, readErr := os.ReadDir(tempDir)
		Expect(readErr).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty(), "controller must not write under StoragePath for HTTPS sources (issue #363)")

		// Condition is Available with the WorkloadResolved reason so users can
		// distinguish runtime-internal HF resolution from init-container
		// HTTP fetches.
		var hasWorkloadResolved bool
		for _, cond := range updated.Status.Conditions {
			if cond.Type == "Available" && cond.Reason == "WorkloadResolved" {
				hasWorkloadResolved = true
			}
		}
		Expect(hasWorkloadResolved).To(BeTrue(), "expected Available/WorkloadResolved condition")
	})

	It("should copy local model file and set Ready", func() {
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		srcDir, err := os.MkdirTemp("", "llmkube-src-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()

		srcFile := filepath.Join(srcDir, "local-model.gguf")
		Expect(os.WriteFile(srcFile, []byte("local-model-data"), 0644)).To(Succeed())

		modelName := "model-local-copy"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: fmt.Sprintf("file://%s", srcFile),
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, model)
		}()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseReady))
	})

	It("should set Failed with CopyFailed for nonexistent local file", func() {
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "model-local-fail"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "file:///nonexistent/path.gguf",
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, model)
		}()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).To(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseFailed))

		var hasDegraded bool
		for _, cond := range updated.Status.Conditions {
			if cond.Type == ConditionDegraded && cond.Reason == "CopyFailed" {
				hasDegraded = true
			}
		}
		Expect(hasDegraded).To(BeTrue())
	})
})

var _ = Describe("Model Controller - Cache Bug Fixes", func() {
	ctx := context.Background()

	It("should skip reconcile when model is already Ready and file exists", func() {
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		// file:// source so the test exercises the controller's in-process
		// cache + migration path. HTTPS sources are deferred to the workload.
		srcDir, err := os.MkdirTemp("", "llmkube-src-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()
		srcFile := filepath.Join(srcDir, "src.gguf")
		Expect(os.WriteFile(srcFile, []byte("cached-model-data"), 0644)).To(Succeed())
		source := fmt.Sprintf("file://%s", srcFile)
		cacheKey := computeCacheKey(source)
		modelDir := filepath.Join(tempDir, cacheKey)
		legacyPath := filepath.Join(modelDir, "model.gguf")
		Expect(os.MkdirAll(modelDir, 0755)).To(Succeed())
		Expect(os.WriteFile(legacyPath, []byte("cached-model-data"), 0644)).To(Succeed())

		modelName := "model-already-ready"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		// First reconcile finds the legacy "model.gguf" in the cache, parses
		// (fake) GGUF metadata (which fails non-fatally), and migrates the file
		// to the canonical filename derived from Model.Name.
		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		// Verify model is Ready and file was migrated to the canonical name.
		expectedPath := filepath.Join(modelDir, modelName+".gguf")
		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseReady))
		Expect(updated.Status.Path).To(Equal(expectedPath))
		_, err = os.Stat(expectedPath)
		Expect(err).NotTo(HaveOccurred())
		_, err = os.Stat(legacyPath)
		Expect(os.IsNotExist(err)).To(BeTrue(), "legacy model.gguf should have been renamed away")
		lastUpdated := updated.Status.LastUpdated.DeepCopy()

		// Second reconcile should return immediately without updating status
		result, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		// Verify status was NOT updated (LastUpdated unchanged)
		afterSecond := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, afterSecond)).To(Succeed())
		Expect(afterSecond.Status.Phase).To(Equal(PhaseReady))
		Expect(afterSecond.Status.LastUpdated.Equal(lastUpdated)).To(BeTrue())
	})

	It("should re-fetch when model is Ready but file is missing", func() {
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		// Use a file:// source so the test exercises the controller's
		// re-fetch behavior on stale status. HTTPS is deferred to the
		// workload init container.
		srcDir, err := os.MkdirTemp("", "llmkube-src-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()
		srcFile := filepath.Join(srcDir, "src.gguf")
		Expect(os.WriteFile(srcFile, []byte("re-fetched-model-content"), 0644)).To(Succeed())

		source := fmt.Sprintf("file://%s", srcFile)
		cacheKey := computeCacheKey(source)
		stalePath := filepath.Join(tempDir, cacheKey, "model.gguf")

		modelName := "model-ready-missing-file"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		// Manually set the status to Ready with a path that doesn't exist on
		// disk to simulate a controller restart with stale status.
		model.Status.Phase = PhaseReady
		model.Status.Path = stalePath
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		// Verify model was re-downloaded and landed at the canonical filename
		// (Model.Name fallback because fake GGUF data fails parsing).
		expectedPath := filepath.Join(tempDir, cacheKey, modelName+".gguf")
		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseReady))
		Expect(updated.Status.Path).To(Equal(expectedPath))
		_, err = os.Stat(expectedPath)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should not leave partial file at final path on fetch failure", func() {
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		// Point at a file:// source whose target does not exist. This
		// exercises the controller's atomic-rename guarantee for non-PVC,
		// non-HTTP sources (HTTP sources are deferred to the workload init
		// container, see issue #363).
		source := "file:///nonexistent/path/to/source-model.gguf"
		cacheKey := computeCacheKey(source)
		modelDir := filepath.Join(tempDir, cacheKey)
		modelPath := filepath.Join(modelDir, "model.gguf")

		modelName := "model-no-partial"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).To(HaveOccurred())

		// Final model path must NOT exist
		_, err = os.Stat(modelPath)
		Expect(os.IsNotExist(err)).To(BeTrue(), "final model path should not exist after failed fetch")

		// No temp files should remain in the cache directory
		if entries, dirErr := os.ReadDir(modelDir); dirErr == nil {
			for _, entry := range entries {
				Expect(entry.Name()).NotTo(HavePrefix(".model-"), "temp file should have been cleaned up")
			}
		}
	})

	It("should detect Content-Length mismatch on truncated download", func() {
		fullContent := []byte("this is a complete model file with enough data")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Advertise full Content-Length but send truncated data
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fullContent)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(fullContent[:10]) // only send 10 bytes
		}))
		defer server.Close()

		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		dest := filepath.Join(tempDir, "model.gguf")
		reconciler := &ModelReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err = reconciler.downloadModel(context.Background(), server.URL+"/model.gguf", dest)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(SatisfyAny(
			ContainSubstring("download incomplete"),
			ContainSubstring("unexpected EOF"),
		))

		// Final file should not exist
		_, statErr := os.Stat(dest)
		Expect(os.IsNotExist(statErr)).To(BeTrue(), "final path should not exist after truncated download")
	})
})

var _ = Describe("formatBytes", func() {
	DescribeTable("should format byte sizes correctly",
		func(bytes int64, expected string) {
			Expect(formatBytes(bytes)).To(Equal(expected))
		},
		Entry("zero", int64(0), "0 B"),
		Entry("bytes", int64(500), "500 B"),
		Entry("kibibytes", int64(1024), "1.0 KiB"),
		Entry("kibibytes fractional", int64(1536), "1.5 KiB"),
		Entry("mebibytes", int64(1048576), "1.0 MiB"),
		Entry("gibibytes", int64(1073741824), "1.0 GiB"),
		Entry("large gibibytes", int64(5368709120), "5.0 GiB"),
	)
})

var _ = Describe("getLocalPath", func() {
	It("should strip file:// prefix", func() {
		Expect(getLocalPath("file:///mnt/models/test.gguf")).To(Equal("/mnt/models/test.gguf"))
	})
	It("should return absolute path as-is", func() {
		Expect(getLocalPath("/mnt/models/test.gguf")).To(Equal("/mnt/models/test.gguf"))
	})
})

var _ = Describe("computeCacheKey", func() {
	It("should return 16 hex char string", func() {
		key := computeCacheKey("https://example.com/model.gguf")
		Expect(key).To(HaveLen(16))
	})
	It("should be deterministic", func() {
		key1 := computeCacheKey("https://example.com/model.gguf")
		key2 := computeCacheKey("https://example.com/model.gguf")
		Expect(key1).To(Equal(key2))
	})
	It("should differ for different sources", func() {
		key1 := computeCacheKey("https://example.com/model-a.gguf")
		key2 := computeCacheKey("https://example.com/model-b.gguf")
		Expect(key1).NotTo(Equal(key2))
	})
})

var _ = Describe("checkAcceleratorAvailability", func() {
	It("should return true when hardware is nil", func() {
		reconciler := &ModelReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Expect(reconciler.checkAcceleratorAvailability(nil)).To(BeTrue())
	})
	It("should return true when hardware is non-nil", func() {
		reconciler := &ModelReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Expect(reconciler.checkAcceleratorAvailability(&inferencev1alpha1.HardwareSpec{Accelerator: "cuda"})).To(BeTrue())
	})
})

var _ = Describe("copyLocalModel", func() {
	It("should copy file successfully", func() {
		srcDir, err := os.MkdirTemp("", "llmkube-src-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()

		dstDir, err := os.MkdirTemp("", "llmkube-dst-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(dstDir) }()

		srcFile := filepath.Join(srcDir, "model.gguf")
		content := []byte("test model content here")
		Expect(os.WriteFile(srcFile, content, 0644)).To(Succeed())

		reconciler := &ModelReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		size, err := reconciler.copyLocalModel(context.Background(), fmt.Sprintf("file://%s", srcFile), filepath.Join(dstDir, "model.gguf"))
		Expect(err).NotTo(HaveOccurred())
		Expect(size).To(Equal(int64(len(content))))
	})

	It("should return error for nonexistent source", func() {
		dstDir, err := os.MkdirTemp("", "llmkube-dst-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(dstDir) }()

		reconciler := &ModelReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err = reconciler.copyLocalModel(context.Background(), "file:///nonexistent/file.gguf", filepath.Join(dstDir, "model.gguf"))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to open local model file"))
	})
})

var _ = Describe("downloadModel", func() {
	It("should download from HTTP server successfully", func() {
		content := []byte("downloaded model data")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(content)
		}))
		defer server.Close()

		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		reconciler := &ModelReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		size, err := reconciler.downloadModel(context.Background(), server.URL+"/model.gguf", filepath.Join(tempDir, "model.gguf"))
		Expect(err).NotTo(HaveOccurred())
		Expect(size).To(Equal(int64(len(content))))
	})

	It("should return error for non-200 status", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		reconciler := &ModelReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err = reconciler.downloadModel(context.Background(), server.URL+"/model.gguf", filepath.Join(tempDir, "model.gguf"))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("bad status"))
	})
})

var _ = Describe("PVC Source Reconcile", func() {
	ctx := context.Background()

	It("should set Ready immediately for PVC source with Bound PVC", func() {
		pvcName := "test-pvc-bound"
		modelName := "model-pvc-source"

		// Create a PVC in envtest
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: "default"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("10Gi"),
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, pvc) }()

		// Manually set PVC status to Bound (envtest doesn't have a volume provisioner)
		pvc.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: fmt.Sprintf("pvc://%s/models/llama.gguf", pvcName),
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
		Expect(updated.Status.Phase).To(Equal(PhaseReady))
		Expect(updated.Status.Path).To(Equal("/model-source/models/llama.gguf"))
	})

	It("should set Failed when referenced PVC does not exist", func() {
		modelName := "model-pvc-missing"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "pvc://nonexistent-pvc/model.gguf",
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
		Expect(err).NotTo(HaveOccurred()) // Returns nil error with requeue
		Expect(result.RequeueAfter).To(Equal(30 * time.Second))

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseFailed))

		var hasDegraded bool
		for _, cond := range updated.Status.Conditions {
			if cond.Type == ConditionDegraded && cond.Reason == "PVCNotFound" {
				hasDegraded = true
			}
		}
		Expect(hasDegraded).To(BeTrue())
	})
})

var _ = Describe("Model Controller - GGUF Filename Migration", func() {
	ctx := context.Background()

	It("renames fetched model to GGUF metadata name", func() {
		// HTTPS sources defer to the workload init container; the controller's
		// canonical-name migration runs only for in-process source types like
		// file://. The init container performs its own naming on the
		// per-namespace cache PVC.
		ggufBytes := buildMinimalGGUF("Gemma-4-E4B-It")
		srcDir, err := os.MkdirTemp("", "llmkube-src-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()
		srcFile := filepath.Join(srcDir, "src.gguf")
		Expect(os.WriteFile(srcFile, ggufBytes, 0644)).To(Succeed())

		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "gemma-test"
		source := fmt.Sprintf("file://%s", srcFile)
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseReady))
		Expect(updated.Status.GGUF).NotTo(BeNil())
		Expect(updated.Status.GGUF.ModelName).To(Equal("Gemma-4-E4B-It"))

		cacheKey := computeCacheKey(source)
		expectedPath := filepath.Join(tempDir, cacheKey, "Gemma-4-E4B-It.gguf")
		Expect(updated.Status.Path).To(Equal(expectedPath))
		_, err = os.Stat(expectedPath)
		Expect(err).NotTo(HaveOccurred())

		legacyPath := filepath.Join(tempDir, cacheKey, "model.gguf")
		_, err = os.Stat(legacyPath)
		Expect(os.IsNotExist(err)).To(BeTrue(), "legacy model.gguf should not exist alongside canonical name")
	})

	It("sanitizes unsafe characters from GGUF metadata name", func() {
		ggufBytes := buildMinimalGGUF("Llama 3.1/Instruct")
		srcDir, err := os.MkdirTemp("", "llmkube-src-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()
		srcFile := filepath.Join(srcDir, "src.gguf")
		Expect(os.WriteFile(srcFile, ggufBytes, 0644)).To(Succeed())

		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "llama-test"
		source := fmt.Sprintf("file://%s", srcFile)
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		cacheKey := computeCacheKey(source)
		expectedPath := filepath.Join(tempDir, cacheKey, "Llama-3.1-Instruct.gguf")
		Expect(updated.Status.Path).To(Equal(expectedPath))
		_, err = os.Stat(expectedPath)
		Expect(err).NotTo(HaveOccurred())
	})

	It("early-exits on second reconcile after rename to canonical path", func() {
		ggufBytes := buildMinimalGGUF("Test-Model")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(ggufBytes)
		}))
		defer server.Close()

		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "test-rename-then-skip"
		source := fmt.Sprintf("%s/model.gguf", server.URL)
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		first := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, first)).To(Succeed())
		firstUpdated := first.Status.LastUpdated.DeepCopy()

		// Second reconcile must early-exit without bumping LastUpdated.
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		second := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, second)).To(Succeed())
		Expect(second.Status.LastUpdated.Equal(firstUpdated)).To(BeTrue())
	})

	It("falls back to Model.Name when GGUF metadata name is missing", func() {
		// Valid GGUF with no general.name key, served via a file:// source.
		// HTTP(S) sources are deferred to the workload init container, so the
		// controller's GGUF parsing + canonical-name migration only runs for
		// in-process source types (file://, etc.).
		ggufBytes := buildMinimalGGUF("")
		srcDir, err := os.MkdirTemp("", "llmkube-src-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()
		srcFile := filepath.Join(srcDir, "src.gguf")
		Expect(os.WriteFile(srcFile, ggufBytes, 0644)).To(Succeed())

		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "fallback-by-cr-name"
		source := fmt.Sprintf("file://%s", srcFile)
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		cacheKey := computeCacheKey(source)
		expectedPath := filepath.Join(tempDir, cacheKey, modelName+".gguf")
		Expect(updated.Status.Path).To(Equal(expectedPath))
	})

	It("migrates an existing legacy model.gguf to canonical name on next reconcile", func() {
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		// Pre-stage a real GGUF file at the legacy path. file:// source so the
		// controller's migration path runs (HTTP(S) is deferred to the
		// workload init container).
		ggufBytes := buildMinimalGGUF("Legacy-Cached-Model")
		srcDir, err := os.MkdirTemp("", "llmkube-src-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()
		srcFile := filepath.Join(srcDir, "src.gguf")
		Expect(os.WriteFile(srcFile, ggufBytes, 0644)).To(Succeed())
		source := fmt.Sprintf("file://%s", srcFile)
		cacheKey := computeCacheKey(source)
		modelDir := filepath.Join(tempDir, cacheKey)
		legacyPath := filepath.Join(modelDir, "model.gguf")
		Expect(os.MkdirAll(modelDir, 0755)).To(Succeed())
		Expect(os.WriteFile(legacyPath, ggufBytes, 0644)).To(Succeed())

		modelName := "model-with-legacy-cache"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		// Simulate post-upgrade state: status was Ready under the legacy path.
		model.Status.Phase = PhaseReady
		model.Status.Path = legacyPath
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		expectedPath := filepath.Join(modelDir, "Legacy-Cached-Model.gguf")
		Expect(updated.Status.Path).To(Equal(expectedPath))

		_, err = os.Stat(expectedPath)
		Expect(err).NotTo(HaveOccurred())
		_, err = os.Stat(legacyPath)
		Expect(os.IsNotExist(err)).To(BeTrue(), "legacy model.gguf should have been renamed away")
	})
})

var _ = Describe("Runtime-Resolved Source Reconcile", func() {
	ctx := context.Background()

	It("should set Ready immediately for HuggingFace repo ID source", func() {
		modelName := "model-hf-repo"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "TinyLlama/TinyLlama-1.1B-Chat-v1.0",
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseReady))
		Expect(updated.Status.Path).To(BeEmpty())
		Expect(updated.Status.CacheKey).To(BeEmpty())
		Expect(updated.Status.Size).To(Equal("0"))
		Expect(updated.Status.LastUpdated).NotTo(BeNil())

		// Verify Available condition with RuntimeResolved reason
		var hasRuntimeResolved bool
		for _, cond := range updated.Status.Conditions {
			if cond.Type == "Available" && cond.Reason == "RuntimeResolved" {
				hasRuntimeResolved = true
			}
		}
		Expect(hasRuntimeResolved).To(BeTrue())
	})

	It("should skip reconcile when runtime-resolved model is already Ready", func() {
		modelName := "model-hf-repo-ready"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "Qwen/Qwen3.6-35B-A3B",
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		// First reconcile to set Ready
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify model is Ready
		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseReady))
		lastUpdated := updated.Status.LastUpdated.DeepCopy()

		// Second reconcile should return immediately
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		// Verify status was NOT re-updated
		afterSecond := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, afterSecond)).To(Succeed())
		Expect(afterSecond.Status.LastUpdated.Equal(lastUpdated)).To(BeTrue())
	})

	It("should not create any files in cache for runtime-resolved source", func() {
		modelName := "model-hf-no-cache"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "bartowski/Qwen_Qwen3.6-35B-A3B-GGUF",
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		// Cache directory should be empty (no model downloaded)
		entries, err := os.ReadDir(tempDir)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty())
	})
})

var _ = Describe("SHA256 Verification", func() {
	ctx := context.Background()

	It("should pass when spec SHA256 matches the fetched file", func() {
		// HTTP(S) sources are deferred to the workload init container, so the
		// controller's SHA256 verification path runs only for in-process
		// sources (file://, etc.). Workload-side SHA verification is the
		// responsibility of the runtime / init container.
		modelContent := []byte("sha256-test-model-content")
		srcDir, err := os.MkdirTemp("", "llmkube-src-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()
		srcFile := filepath.Join(srcDir, "src.gguf")
		Expect(os.WriteFile(srcFile, modelContent, 0644)).To(Succeed())

		tempDir, err := os.MkdirTemp("", "llmkube-sha-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		// Pre-compute the expected hash
		filePath := filepath.Join(tempDir, "pre-hash.gguf")
		Expect(os.WriteFile(filePath, modelContent, 0644)).To(Succeed())
		expectedHash, err := computeFileSHA256(filePath)
		Expect(err).NotTo(HaveOccurred())
		_ = os.Remove(filePath)

		modelName := "model-sha256-match"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: fmt.Sprintf("file://%s", srcFile),
				SHA256: expectedHash,
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseReady))
		Expect(updated.Status.SHA256).To(Equal(expectedHash))
	})

	It("should fail when spec SHA256 does not match the fetched file", func() {
		modelContent := []byte("sha256-mismatch-content")
		srcDir, err := os.MkdirTemp("", "llmkube-src-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()
		srcFile := filepath.Join(srcDir, "src.gguf")
		Expect(os.WriteFile(srcFile, modelContent, 0644)).To(Succeed())

		tempDir, err := os.MkdirTemp("", "llmkube-sha-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "model-sha256-mismatch"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: fmt.Sprintf("file://%s", srcFile),
				SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("SHA256 mismatch"))
		Expect(result).To(Equal(reconcile.Result{}))

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseFailed))

		var hasIntegrity bool
		for _, cond := range updated.Status.Conditions {
			if cond.Type == ConditionDegraded && cond.Reason == "IntegrityCheckFailed" {
				hasIntegrity = true
			}
		}
		Expect(hasIntegrity).To(BeTrue())

		// Verify the file was cleaned up
		cacheKey := computeCacheKey(model.Spec.Source)
		modelPath := filepath.Join(tempDir, cacheKey, "model.gguf")
		_, statErr := os.Stat(modelPath)
		Expect(os.IsNotExist(statErr)).To(BeTrue(), "model file should be removed after integrity check failure")
	})

	It("should compute and store SHA256 even when not specified in spec", func() {
		modelContent := []byte("sha256-auto-compute-content")
		srcDir, err := os.MkdirTemp("", "llmkube-src-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(srcDir) }()
		srcFile := filepath.Join(srcDir, "src.gguf")
		Expect(os.WriteFile(srcFile, modelContent, 0644)).To(Succeed())

		tempDir, err := os.MkdirTemp("", "llmkube-sha-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "model-sha256-auto"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: fmt.Sprintf("file://%s", srcFile),
				// SHA256 intentionally not set
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseReady))
		Expect(updated.Status.SHA256).To(HaveLen(64))
	})
})

var _ = Describe("computeFileSHA256", func() {
	It("should compute correct hash", func() {
		tempDir, err := os.MkdirTemp("", "llmkube-hash-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		filePath := filepath.Join(tempDir, "test.gguf")
		Expect(os.WriteFile(filePath, []byte("hello"), 0644)).To(Succeed())

		hash, err := computeFileSHA256(filePath)
		Expect(err).NotTo(HaveOccurred())
		// SHA256 of "hello" is 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
		Expect(hash).To(Equal("2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"))
	})

	It("should return error for nonexistent file", func() {
		_, err := computeFileSHA256("/nonexistent/file.gguf")
		Expect(err).To(HaveOccurred())
	})
})

// Regression coverage for issue #363 — "Model cache PVC created in
// llmkube-system instead of the Model CR's namespace".
//
// The original disconnect: the Model controller downloaded HTTPS sources
// in-process to its own pod's filesystem (operator-namespace cache PVC),
// while the InferenceService init container read from a per-namespace cache
// PVC. Because PVCs cannot be cross-namespace mounted, the controller's
// download was invisible to the inference Pod, which silently re-fetched
// from source on every fresh InferenceService.
//
// These tests assert the architectural invariants that prevent that
// disconnect from coming back:
//  1. For HTTP(S) sources, the controller writes nothing under StoragePath.
//  2. The Model.Status.CacheKey produced by Reconcile() is the same key the
//     InferenceService Pod's storage config uses to build the init
//     container's MODEL_PATH. If those ever desync, the inference Pod's
//     "skip download if file exists" cache check fails silently and
//     every Pod re-downloads.
//  3. CacheKey is populated only for source types that flow through the
//     per-namespace cache PVC (HTTP(S)), not for runtime-internal sources
//     (HF repo IDs that vLLM resolves itself).
var _ = Describe("Issue #363 regression — controller / workload cache disconnect", func() {
	ctx := context.Background()

	It("does not write under StoragePath for HTTPS sources", func() {
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "issue-363-no-fs-write"
		source := "https://huggingface.co/example-org/example-repo/resolve/main/m.gguf"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		// The controller must not have created any directory or file under
		// its StoragePath. Any write here would land on the operator-
		// namespace PVC, which Pods in user namespaces cannot mount.
		entries, readErr := os.ReadDir(tempDir)
		Expect(readErr).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty(), "operator-namespace cache PVC must stay untouched for HTTPS sources")
	})

	It("emits a CacheKey that matches the InferenceService init container's MODEL_PATH for HTTPS sources", func() {
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "issue-363-cachekey-roundtrip"
		source := "https://huggingface.co/example-org/example-repo/resolve/main/m.gguf"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.CacheKey).NotTo(BeEmpty(), "HTTPS source must populate CacheKey so the init container can build /models/<cacheKey>/<basename>")

		// Build the InferenceService Pod's storage config from this Model and
		// assert the init container's MODEL_PATH lines up with the Model's
		// CacheKey. The init container's `if [ ! -f "$MODEL_PATH" ]` check
		// only works when both sides agree on the path.
		config := buildCachedStorageConfig(updated, nil, "", "curl:8.18.0")
		expectedPrefix := "/models/" + updated.Status.CacheKey + "/"
		Expect(config.modelPath).To(HavePrefix(expectedPrefix),
			"init container MODEL_PATH must live under /models/<Status.CacheKey>/")

		var modelPathEnv string
		for _, e := range config.initContainers[0].Env {
			if e.Name == "MODEL_PATH" {
				modelPathEnv = e.Value
			}
		}
		Expect(modelPathEnv).To(Equal(config.modelPath))
		Expect(modelPathEnv).To(HavePrefix(expectedPrefix),
			"MODEL_PATH env on the init container must use Status.CacheKey from Reconcile()")
	})

	It("leaves CacheKey empty for HuggingFace repo IDs (runtime-internal resolution)", func() {
		// HF repo IDs are resolved by vLLM/llama.cpp at runtime, not by the
		// init container. CacheKey is irrelevant in that path; this test
		// guards against accidentally populating it (which would cause the
		// init container to try to materialize a file at a synthetic path).
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "issue-363-hf-no-cachekey"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "Qwen/Qwen3.6-35B-A3B"},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		reconciler := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: tempDir,
		}
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.CacheKey).To(BeEmpty(), "HF repo IDs must not populate CacheKey — runtime resolves them internally")
	})
})
