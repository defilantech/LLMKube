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
	"k8s.io/apimachinery/pkg/api/errors"
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
		Expect(updated.Status.Phase).To(Equal("Failed"))
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
		Expect(updated.Status.Phase).To(Equal("Failed"))

		var hasDegraded bool
		for _, cond := range updated.Status.Conditions {
			if cond.Type == "Degraded" && cond.Reason == "CopyFailed" {
				hasDegraded = true
			}
		}
		Expect(hasDegraded).To(BeTrue())
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
