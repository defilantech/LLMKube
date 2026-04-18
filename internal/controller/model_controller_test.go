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
			Namespace: "default", // TODO(user):Modify as needed
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
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &inferencev1alpha1.Model{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Model")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			// Create a temp directory for model cache
			tempDir, err := os.MkdirTemp("", "llmkube-test-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = os.RemoveAll(tempDir) }()

			controllerReconciler := &ModelReconciler{
				Client:      k8sClient,
				Scheme:      k8sClient.Scheme(),
				StoragePath: tempDir,
			}

			// Note: This will attempt to download the model, which will fail for test URL
			// In a real scenario, you'd mock the download or use a valid test model
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			// We expect an error because the test URL doesn't exist
			// This test validates the controller handles the error gracefully
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bad status"))
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
		source := "https://example.com/cached-model.gguf"

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

	It("should download model from HTTP server and set Ready", func() {
		modelContent := []byte("fake-gguf-model-content-for-test")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(modelContent)
		}))
		defer server.Close()

		modelName := "model-download-test"
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: fmt.Sprintf("%s/model.gguf", server.URL),
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
		Expect(updated.Status.Size).NotTo(BeEmpty())
		Expect(updated.Status.CacheKey).To(HaveLen(16))
	})

	It("should set Failed and requeue on download failure", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		modelName := "model-download-fail"
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: fmt.Sprintf("%s/model.gguf", server.URL),
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
		Expect(err.Error()).To(ContainSubstring("bad status"))
		Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseFailed))
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

		source := "https://example.com/already-ready-model.gguf"
		cacheKey := computeCacheKey(source)
		modelDir := filepath.Join(tempDir, cacheKey)
		modelPath := filepath.Join(modelDir, "model.gguf")
		Expect(os.MkdirAll(modelDir, 0755)).To(Succeed())
		Expect(os.WriteFile(modelPath, []byte("cached-model-data"), 0644)).To(Succeed())

		modelName := "model-already-ready"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		// First reconcile to set the model to Ready state
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

		// Verify model is Ready
		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseReady))
		Expect(updated.Status.Path).To(Equal(modelPath))
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

	It("should re-download when model is Ready but file is missing", func() {
		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelContent := []byte("re-downloaded-model-content")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(modelContent)
		}))
		defer server.Close()

		source := fmt.Sprintf("%s/model.gguf", server.URL)
		cacheKey := computeCacheKey(source)
		modelPath := filepath.Join(tempDir, cacheKey, "model.gguf")

		modelName := "model-ready-missing-file"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: source},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, model) }()

		// Manually set the status to Ready with a path that doesn't exist
		model.Status.Phase = PhaseReady
		model.Status.Path = modelPath
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

		// Verify model was re-downloaded and is Ready again
		updated := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(PhaseReady))

		// Verify the file now exists
		_, err = os.Stat(modelPath)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should not leave partial file at final path on download failure", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		tempDir, err := os.MkdirTemp("", "llmkube-test-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		source := fmt.Sprintf("%s/model.gguf", server.URL)
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
		Expect(os.IsNotExist(err)).To(BeTrue(), "final model path should not exist after failed download")

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

	It("should pass when spec SHA256 matches downloaded file", func() {
		modelContent := []byte("sha256-test-model-content")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(modelContent)
		}))
		defer server.Close()

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
				Source: fmt.Sprintf("%s/model.gguf", server.URL),
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

	It("should fail when spec SHA256 does not match downloaded file", func() {
		modelContent := []byte("sha256-mismatch-content")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(modelContent)
		}))
		defer server.Close()

		tempDir, err := os.MkdirTemp("", "llmkube-sha-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "model-sha256-mismatch"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: fmt.Sprintf("%s/model.gguf", server.URL),
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
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(modelContent)
		}))
		defer server.Close()

		tempDir, err := os.MkdirTemp("", "llmkube-sha-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tempDir) }()

		modelName := "model-sha256-auto"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: fmt.Sprintf("%s/model.gguf", server.URL),
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
