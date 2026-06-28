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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var _ = Describe("buildCachedStorageConfig", func() {
	It("should configure PVC volume and init container for remote model", func() {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://example.com/model.gguf",
			},
			Status: inferencev1alpha1.ModelStatus{
				CacheKey: "abc123def456",
			},
		}
		config := buildCachedStorageConfig(model, nil, "", "", "curl:8.18.0", 102)

		Expect(config.modelPath).To(Equal("/models/abc123def456/model.gguf"))
		Expect(config.volumes).To(HaveLen(1))
		Expect(config.volumes[0].Name).To(Equal("model-cache"))
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(ModelCachePVCName))
		Expect(config.initContainers).To(HaveLen(2))
		Expect(config.initContainers[0].Name).To(Equal("model-cache-prep"))
		Expect(config.initContainers[1].Name).To(Equal("model-downloader"))
		Expect(config.initContainers[1].Image).To(Equal("curl:8.18.0"))
		Expect(config.volumeMounts[0].MountPath).To(Equal("/models"))
		Expect(config.volumeMounts[0].ReadOnly).To(BeTrue())

		// Verify env vars are set on the init container
		env := config.initContainers[1].Env
		Expect(getEnvVar(env, "MODEL_SOURCE")).To(Equal("https://example.com/model.gguf"))
		Expect(getEnvVar(env, "CACHE_DIR")).To(Equal("/models/abc123def456"))
		Expect(getEnvVar(env, "MODEL_PATH")).To(Equal("/models/abc123def456/model.gguf"))

		// Verify the command does not contain the raw source URL
		Expect(config.initContainers[1].Command[2]).NotTo(ContainSubstring("example.com"))
	})

	It("should add host-model volume for local source", func() {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Source: "file:///mnt/models/test.gguf",
			},
			Status: inferencev1alpha1.ModelStatus{
				CacheKey: "abc123",
			},
		}
		config := buildCachedStorageConfig(model, nil, "", "", "curl:8.18.0", 102)

		Expect(config.volumes).To(HaveLen(2))
		Expect(config.volumes[1].Name).To(Equal("host-model"))
		Expect(config.volumes[1].HostPath.Path).To(Equal("/mnt/models/test.gguf"))

		// Verify env vars are set on the downloader (initContainers[1])
		env := config.initContainers[1].Env
		Expect(getEnvVar(env, "MODEL_SOURCE")).To(Equal("file:///mnt/models/test.gguf"))
	})

	It("should add CA cert volume when caCertConfigMap is set", func() {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://example.com/model.gguf",
			},
			Status: inferencev1alpha1.ModelStatus{
				CacheKey: "abc123",
			},
		}
		config := buildCachedStorageConfig(model, nil, "", "my-ca-certs", "curl:8.18.0", 102)

		var found bool
		for _, v := range config.volumes {
			if v.Name == "custom-ca-cert" {
				found = true
				Expect(v.ConfigMap.Name).To(Equal("my-ca-certs"))
			}
		}
		Expect(found).To(BeTrue())
		Expect(config.initContainers[1].Command[2]).To(ContainSubstring("CURL_CA_BUNDLE=/custom-certs/"))
	})
})

var _ = Describe("buildCachedStorageConfig multi-file staging", func() {
	It("uses primary staged path and MODEL_FILES env for multi-file model", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "gemma", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "hf://unsloth/gemma-4-31B-it-GGUF",
				Files: []string{
					"gemma-4-31B-it-UD-Q4_K_XL.gguf",
					"MTP/gemma-4-31B-it-Q8_0-MTP.gguf",
				},
				Mmproj: "mmproj-F16.gguf",
			},
			Status: inferencev1alpha1.ModelStatus{CacheKey: "abc123"},
		}

		config := buildCachedStorageConfig(model, nil, "", "", "curl:8.18.0", 102)

		Expect(config.modelPath).To(Equal("/models/abc123/gemma-4-31B-it-UD-Q4_K_XL.gguf"))
		cmd := config.initContainers[1].Command[2]
		Expect(cmd).To(ContainSubstring("MODEL_FILES"))
		Expect(cmd).To(ContainSubstring(`printf '%s\n'`))

		env := config.initContainers[1].Env
		modelFiles := getEnvVar(env, "MODEL_FILES")
		Expect(modelFiles).NotTo(BeEmpty())
		Expect(modelFiles).To(ContainSubstring("gemma-4-31B-it-UD-Q4_K_XL.gguf"))
		Expect(modelFiles).To(ContainSubstring("MTP/gemma-4-31B-it-Q8_0-MTP.gguf"))
		Expect(modelFiles).To(ContainSubstring("mmproj-F16.gguf"))
	})

	It("preserves subdirectories in multi-file staging", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "hf://org/multi-repo",
				Files: []string{
					"model.gguf",
					"MTP/weights.gguf",
				},
			},
			Status: inferencev1alpha1.ModelStatus{CacheKey: "key1"},
		}

		config := buildCachedStorageConfig(model, nil, "", "", "curl:8.18.0", 102)
		cmd := config.initContainers[1].Command[2]
		Expect(cmd).To(ContainSubstring(`mkdir -p "$(dirname "$dest")"`))

		env := config.initContainers[1].Env
		modelFiles := getEnvVar(env, "MODEL_FILES")
		Expect(modelFiles).To(ContainSubstring("MTP/weights.gguf"))
	})

	It("normalizes hf:// source to huggingface.co URL in multi-file command", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "hf-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "hf://unsloth/gemma-4-31B-it-GGUF",
				Files:  []string{"model.gguf"},
			},
			Status: inferencev1alpha1.ModelStatus{CacheKey: "key2"},
		}

		config := buildCachedStorageConfig(model, nil, "", "", "curl:8.18.0", 102)
		env := config.initContainers[1].Env
		source := getEnvVar(env, "MODEL_SOURCE")
		Expect(source).To(Equal("https://huggingface.co/unsloth/gemma-4-31B-it-GGUF"))
	})

	It("includes custom CA cert volume in multi-file cached storage", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "ca-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "hf://org/repo",
				Files:  []string{"model.gguf"},
			},
			Status: inferencev1alpha1.ModelStatus{CacheKey: "key3"},
		}

		config := buildCachedStorageConfig(model, nil, "", "my-ca-certs", "curl:8.18.0", 102)

		var foundCA bool
		for _, v := range config.volumes {
			if v.Name == "custom-ca-cert" {
				foundCA = true
				Expect(v.ConfigMap.Name).To(Equal("my-ca-certs"))
			}
		}
		Expect(foundCA).To(BeTrue())
		Expect(config.initContainers[1].Command[2]).To(ContainSubstring("CURL_CA_BUNDLE=/custom-certs/"))
	})

	It("uses OnChange per-file etag revalidation for multi-file model", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "refresh-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:        "hf://org/repo",
				Files:         []string{"model.gguf", "extra.gguf"},
				RefreshPolicy: RefreshPolicyOnChange,
			},
			Status: inferencev1alpha1.ModelStatus{CacheKey: "key4"},
		}

		config := buildCachedStorageConfig(model, nil, "", "", "curl:8.18.0", 102)
		cmd := config.initContainers[1].Command[2]
		Expect(cmd).To(ContainSubstring("--etag-compare"))
		Expect(cmd).To(ContainSubstring("--etag-save"))
		Expect(cmd).To(ContainSubstring("kept cached copy"))
	})

	It("preserves legacy single-file behavior when no files/mmproj", func() {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://example.com/model.gguf",
			},
			Status: inferencev1alpha1.ModelStatus{CacheKey: "abc123def456"},
		}
		config := buildCachedStorageConfig(model, nil, "", "", "curl:8.18.0", 102)

		Expect(config.modelPath).To(Equal("/models/abc123def456/model.gguf"))
		env := config.initContainers[1].Env
		Expect(getEnvVar(env, "MODEL_PATH")).To(Equal("/models/abc123def456/model.gguf"))
		Expect(getEnvVar(env, "MODEL_FILES")).To(BeEmpty())
		cmd := config.initContainers[1].Command[2]
		Expect(cmd).NotTo(ContainSubstring("MODEL_FILES"))
		Expect(cmd).To(ContainSubstring(`"$MODEL_PATH"`))
	})
})

var _ = Describe("buildEmptyDirStorageConfig multi-file staging", func() {
	It("stages multiple files in emptyDir storage", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "empty-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "hf://org/repo",
				Files:  []string{"model.gguf", "extra.gguf"},
			},
		}

		config := buildEmptyDirStorageConfig(model, nil, "default", "", "curl:8.18.0")

		Expect(config.modelPath).To(Equal("/models/default-empty-model/model.gguf"))
		cmd := config.initContainers[0].Command[2]
		Expect(cmd).To(ContainSubstring("MODEL_FILES"))

		env := config.initContainers[0].Env
		modelFiles := getEnvVar(env, "MODEL_FILES")
		Expect(modelFiles).To(ContainSubstring("extra.gguf"))
	})

	It("uses OnChange per-file etag revalidation in emptyDir storage", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "empty-refresh", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:        "hf://org/repo",
				Files:         []string{"model.gguf", "extra.gguf"},
				RefreshPolicy: RefreshPolicyOnChange,
			},
		}

		config := buildEmptyDirStorageConfig(model, nil, "default", "", "curl:8.18.0")
		cmd := config.initContainers[0].Command[2]
		Expect(cmd).To(ContainSubstring("--etag-compare"))
		Expect(cmd).To(ContainSubstring("--etag-save"))
		Expect(cmd).To(ContainSubstring("kept cached copy"))
	})
})

var _ = Describe("buildMultiFileInitCommand", func() {
	It("generates download loop for IfNotPresent policy", func() {
		cmd := buildMultiFileInitCommand(true, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring(`mkdir -p "$CACHE_DIR"`))
		Expect(cmd).To(ContainSubstring("printf '%s\\n' \"$MODEL_FILES\""))
		Expect(cmd).To(ContainSubstring(`mkdir -p "$(dirname "$dest")"`))
		Expect(cmd).To(ContainSubstring(`curl -f -L -o "$dest" "$url"`))
		Expect(cmd).To(ContainSubstring("already cached, skipping download"))
	})

	It("fails init container if any curl fails in IfNotPresent policy", func() {
		cmd := buildMultiFileInitCommand(true, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring(`exit 1`))
		Expect(cmd).To(ContainSubstring("failed to download"))
	})

	It("generates etag revalidation for OnChange policy", func() {
		cmd := buildMultiFileInitCommand(true, RefreshPolicyOnChange)
		Expect(cmd).To(ContainSubstring(`mkdir -p "$CACHE_DIR"`))
		Expect(cmd).To(ContainSubstring("--etag-compare"))
		Expect(cmd).To(ContainSubstring("--etag-save"))
		Expect(cmd).To(ContainSubstring("kept cached copy"))
	})

	It("uses emptyDir prefix without cache dir for non-cached storage", func() {
		cmd := buildMultiFileInitCommand(false, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring(`mkdir -p /models`))
		Expect(cmd).NotTo(ContainSubstring(`"$CACHE_DIR"`))
	})

	It("normalizes hf:// URLs via MODEL_SOURCE in the generated command", func() {
		cmd := buildMultiFileInitCommand(true, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring("normalize_hf_source"))
	})

	It("uses POSIX-compatible shell (no bashisms)", func() {
		cmd := buildMultiFileInitCommand(true, RefreshPolicyIfNotPresent)
		Expect(cmd).NotTo(ContainSubstring("[["))
		Expect(cmd).To(ContainSubstring("case"))
		Expect(cmd).To(ContainSubstring("esac"))
	})
})

var _ = Describe("multiFileInitEnvVars", func() {
	It("sets MODEL_FILES as newline-delimited list", func() {
		env := multiFileInitEnvVars("hf://org/repo", "/models/abc", []string{"a.gguf", "b.gguf"})
		Expect(getEnvVar(env, "MODEL_SOURCE")).To(Equal("https://huggingface.co/org/repo"))
		Expect(getEnvVar(env, "CACHE_DIR")).To(Equal("/models/abc"))
		Expect(getEnvVar(env, "MODEL_FILES")).To(Equal("a.gguf\nb.gguf"))
	})

	It("passes through https sources unchanged", func() {
		env := multiFileInitEnvVars("https://example.com/model.gguf", "/models/abc", []string{"model.gguf"})
		Expect(getEnvVar(env, "MODEL_SOURCE")).To(Equal("https://example.com/model.gguf"))
	})
})

var _ = Describe("hasMultiFileStaging", func() {
	It("returns false for nil model", func() {
		Expect(hasMultiFileStaging(nil)).To(BeFalse())
	})

	It("returns false when no files and no mmproj", func() {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		}
		Expect(hasMultiFileStaging(model)).To(BeFalse())
	})

	It("returns true when files are set", func() {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{Files: []string{"model.gguf"}},
		}
		Expect(hasMultiFileStaging(model)).To(BeTrue())
	})

	It("returns true when only mmproj is set", func() {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{Mmproj: "mmproj.gguf"},
		}
		Expect(hasMultiFileStaging(model)).To(BeTrue())
	})
})

var _ = Describe("resolveHFSourceURL", func() {
	It("converts hf:// to https://huggingface.co/", func() {
		Expect(resolveHFSourceURL("hf://unsloth/gemma-4-31B-it-GGUF")).To(Equal("https://huggingface.co/unsloth/gemma-4-31B-it-GGUF"))
	})

	It("passes through https URLs unchanged", func() {
		Expect(resolveHFSourceURL("https://example.com/model.gguf")).To(Equal("https://example.com/model.gguf"))
	})

	It("passes through http URLs unchanged", func() {
		Expect(resolveHFSourceURL("http://example.com/model.gguf")).To(Equal("http://example.com/model.gguf"))
	})

	It("passes through file:// URLs unchanged", func() {
		Expect(resolveHFSourceURL("file:///mnt/model.gguf")).To(Equal("file:///mnt/model.gguf"))
	})
})

var _ = Describe("buildEmptyDirStorageConfig", func() {
	It("should configure emptyDir volume for remote model", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "my-model"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		}
		config := buildEmptyDirStorageConfig(model, nil, "default", "", "curl:8.18.0")

		Expect(config.modelPath).To(Equal("/models/default-my-model.gguf"))
		Expect(config.volumes).To(HaveLen(1))
		Expect(config.volumes[0].Name).To(Equal("model-storage"))
		Expect(config.volumes[0].EmptyDir).NotTo(BeNil())

		// Verify env vars are set on the init container
		env := config.initContainers[0].Env
		Expect(getEnvVar(env, "MODEL_SOURCE")).To(Equal("https://example.com/model.gguf"))
		Expect(getEnvVar(env, "CACHE_DIR")).To(Equal(""))
		Expect(getEnvVar(env, "MODEL_PATH")).To(Equal("/models/default-my-model.gguf"))

		// Verify the command does not contain the raw source URL
		Expect(config.initContainers[0].Command[2]).NotTo(ContainSubstring("example.com"))
	})

	It("should add CA cert volume when caCertConfigMap is set", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "my-model"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		}
		config := buildEmptyDirStorageConfig(model, nil, "default", "my-ca-certs", "curl:8.18.0")

		var found bool
		for _, v := range config.volumes {
			if v.Name == "custom-ca-cert" {
				found = true
				Expect(v.ConfigMap.Name).To(Equal("my-ca-certs"))
			}
		}
		Expect(found).To(BeTrue())
		Expect(config.initContainers[0].Command[2]).To(ContainSubstring("CURL_CA_BUNDLE=/custom-certs/"))
	})

	It("should inherit runAsUser/runAsGroup in emptyDir storage", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "my-model"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		}
		customUID := int64(2000)
		customGID := int64(2000)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "test-isvc"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser:  &customUID,
					RunAsGroup: &customGID,
				},
			},
		}
		config := buildEmptyDirStorageConfig(model, isvc, "default", "", "curl:8.18.0")

		initSecCtx := config.initContainers[0].SecurityContext
		Expect(initSecCtx).NotTo(BeNil())
		Expect(initSecCtx.RunAsUser).NotTo(BeNil())
		Expect(*initSecCtx.RunAsUser).To(Equal(int64(2000)))
		Expect(initSecCtx.RunAsGroup).NotTo(BeNil())
		Expect(*initSecCtx.RunAsGroup).To(Equal(int64(2000)))
	})
})

var _ = Describe("buildPVCStorageConfig", func() {
	It("should configure PVC volume with correct claim name and path", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-model"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "pvc://my-models/llama/model.gguf"},
		}
		config := buildPVCStorageConfig(model)

		Expect(config.modelPath).To(Equal("/model-source/llama/model.gguf"))
		Expect(config.initContainers).To(BeEmpty())
		Expect(config.volumes).To(HaveLen(1))
		Expect(config.volumes[0].Name).To(Equal("model-source"))
		Expect(config.volumes[0].PersistentVolumeClaim).NotTo(BeNil())
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("my-models"))
		Expect(config.volumes[0].PersistentVolumeClaim.ReadOnly).To(BeTrue())
		Expect(config.volumeMounts).To(HaveLen(1))
		Expect(config.volumeMounts[0].Name).To(Equal("model-source"))
		Expect(config.volumeMounts[0].MountPath).To(Equal("/model-source"))
		Expect(config.volumeMounts[0].ReadOnly).To(BeTrue())
	})

	It("should handle simple file at root of PVC", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-model-simple"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "pvc://storage/model.gguf"},
		}
		config := buildPVCStorageConfig(model)

		Expect(config.modelPath).To(Equal("/model-source/model.gguf"))
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("storage"))
	})
})

var _ = Describe("buildModelStorageConfig PVC dispatch", func() {
	It("should dispatch to PVC storage config when source is pvc://", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "dispatch-test"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "pvc://my-claim/model.gguf"},
			Status:     inferencev1alpha1.ModelStatus{CacheKey: "abc123"},
		}
		config := buildModelStorageConfig(model, nil, "default", true, "", "", "curl:8.18.0", 102)

		// Should use PVC config, not cached config
		Expect(config.volumes[0].Name).To(Equal("model-source"))
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("my-claim"))
		Expect(config.initContainers).To(BeEmpty())
	})
})

var _ = Describe("ensureModelCachePVC (shared mode)", func() {
	var reconciler *InferenceServiceReconciler
	var isvc *inferencev1alpha1.InferenceService

	BeforeEach(func() {
		deletePVCForcibly(context.Background(), "default")
		reconciler = &InferenceServiceReconciler{
			Client:         k8sClient,
			Scheme:         k8sClient.Scheme(),
			ModelCacheMode: ModelCacheModeShared,
		}
		isvc = &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "shared-isvc", Namespace: "default"},
		}
	})

	AfterEach(func() {
		deletePVCForcibly(context.Background(), "default")
	})

	It("should create the cluster-wide shared PVC with default 100Gi and ReadWriteOnce", func() {
		err := reconciler.ensureModelCachePVC(context.Background(), isvc)
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		Expect(pvc.Spec.AccessModes).To(ContainElement(corev1.ReadWriteOnce))
		storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		Expect(storageReq.String()).To(Equal("100Gi"))
		Expect(pvc.Labels["app.kubernetes.io/name"]).To(Equal("llmkube"))
		// The shared cache outlives any single InferenceService; no owner ref.
		Expect(pvc.OwnerReferences).To(BeEmpty())
	})

	It("should create PVC with custom size", func() {
		reconciler.ModelCacheSize = "50Gi"
		err := reconciler.ensureModelCachePVC(context.Background(), isvc)
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		Expect(storageReq.String()).To(Equal("50Gi"))
	})

	It("should create PVC with ReadWriteMany when configured", func() {
		reconciler.ModelCacheAccessMode = "ReadWriteMany"
		err := reconciler.ensureModelCachePVC(context.Background(), isvc)
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		Expect(pvc.Spec.AccessModes).To(ContainElement(corev1.ReadWriteMany))
	})

	It("should set StorageClassName when configured", func() {
		reconciler.ModelCacheClass = "fast-ssd"
		err := reconciler.ensureModelCachePVC(context.Background(), isvc)
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		Expect(*pvc.Spec.StorageClassName).To(Equal("fast-ssd"))
	})

	It("should not error if PVC already exists", func() {
		err := reconciler.ensureModelCachePVC(context.Background(), isvc)
		Expect(err).NotTo(HaveOccurred())
		err = reconciler.ensureModelCachePVC(context.Background(), isvc)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should return error for invalid cache size", func() {
		reconciler.ModelCacheSize = "not-a-size"
		err := reconciler.ensureModelCachePVC(context.Background(), isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid cache size"))
	})
})

var _ = Describe("ensureModelCachePVC (perService mode, opt-in, #728)", func() {
	var reconciler *InferenceServiceReconciler
	var isvc *inferencev1alpha1.InferenceService
	var pvcName string

	deletePerServicePVC := func(name string) {
		ctx := context.Background()
		pvc := &corev1.PersistentVolumeClaim{}
		key := types.NamespacedName{Name: name, Namespace: "default"}
		if err := k8sClient.Get(ctx, key, pvc); err != nil {
			return
		}
		if len(pvc.Finalizers) > 0 {
			pvc.Finalizers = nil
			_ = k8sClient.Update(ctx, pvc)
		}
		_ = k8sClient.Delete(ctx, pvc)
		Eventually(func() bool {
			return errors.IsNotFound(k8sClient.Get(ctx, key, &corev1.PersistentVolumeClaim{}))
		}, "5s", "100ms").Should(BeTrue())
	}

	BeforeEach(func() {
		// Clear any shared PVC a sibling (shared-default) spec may have left in
		// the namespace, so the "does not create the shared PVC" assertion below
		// is not polluted by test ordering.
		deletePVCForcibly(context.Background(), "default")
		// A real InferenceService gives the PVC a valid owner UID/GVK.
		isvc = &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "perservice-isvc-",
				Namespace:    "default",
			},
			Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: "some-model"},
		}
		Expect(k8sClient.Create(context.Background(), isvc)).To(Succeed())
		pvcName = isvc.Name + "-model-cache"
		// perService is the opt-in mode; it must be set explicitly now that the
		// default (empty mode) resolves to shared.
		reconciler = &InferenceServiceReconciler{
			Client:         k8sClient,
			Scheme:         k8sClient.Scheme(),
			ModelCacheMode: ModelCacheModePerService,
		}
	})

	AfterEach(func() {
		deletePerServicePVC(pvcName)
		_ = k8sClient.Delete(context.Background(), isvc)
	})

	It("should create a per-isvc RWO PVC named <isvc>-model-cache owner-ref'd to the InferenceService", func() {
		err := reconciler.ensureModelCachePVC(context.Background(), isvc)
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: pvcName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		Expect(pvc.Spec.AccessModes).To(ContainElement(corev1.ReadWriteOnce))
		// No explicit StorageClassName: the cluster default class (whose binding
		// mode is WaitForFirstConsumer in the topology-aware case) is used so the
		// PVC binds on the serving node, not immediately on the operator's node.
		Expect(pvc.Spec.StorageClassName).To(BeNil())

		Expect(pvc.OwnerReferences).To(HaveLen(1))
		Expect(pvc.OwnerReferences[0].Kind).To(Equal("InferenceService"))
		Expect(pvc.OwnerReferences[0].Name).To(Equal(isvc.Name))
		Expect(pvc.OwnerReferences[0].UID).To(Equal(isvc.UID))
		Expect(*pvc.OwnerReferences[0].Controller).To(BeTrue())
	})

	It("should force RWO even when ModelCacheAccessMode=ReadWriteMany (RWX only applies to shared)", func() {
		reconciler.ModelCacheAccessMode = "ReadWriteMany"
		err := reconciler.ensureModelCachePVC(context.Background(), isvc)
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: pvcName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		Expect(pvc.Spec.AccessModes).To(ContainElement(corev1.ReadWriteOnce))
		Expect(pvc.Spec.AccessModes).NotTo(ContainElement(corev1.ReadWriteMany))
	})

	It("should not create the cluster-wide shared PVC in perService mode", func() {
		err := reconciler.ensureModelCachePVC(context.Background(), isvc)
		Expect(err).NotTo(HaveOccurred())

		shared := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, shared)
		Expect(errors.IsNotFound(err)).To(BeTrue())
	})

	It("should be idempotent", func() {
		Expect(reconciler.ensureModelCachePVC(context.Background(), isvc)).To(Succeed())
		Expect(reconciler.ensureModelCachePVC(context.Background(), isvc)).To(Succeed())
	})
})

var _ = Describe("buildCachedStorageConfig cache mode selection (#728)", func() {
	model := &inferencev1alpha1.Model{
		Spec:   inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		Status: inferencev1alpha1.ModelStatus{CacheKey: "abc123def456"},
	}

	It("references the per-isvc PVC in perService mode", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-isvc"},
		}
		config := buildCachedStorageConfig(model, isvc, ModelCacheModePerService, "", "curl:8.18.0", 102)
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("my-isvc-model-cache"))
	})

	It("references the shared PVC in shared mode (default)", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-isvc"},
		}
		config := buildCachedStorageConfig(model, isvc, ModelCacheModeShared, "", "curl:8.18.0", 102)
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(ModelCachePVCName))
	})

	It("empty mode defaults to the shared PVC", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-isvc"},
		}
		config := buildCachedStorageConfig(model, isvc, "", "", "curl:8.18.0", 102)
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(ModelCachePVCName))
	})
})

var _ = Describe("resolveCacheMode", func() {
	It("maps an empty mode to the shared default", func() {
		Expect(resolveCacheMode("")).To(Equal(ModelCacheModeShared))
	})

	It("maps an unknown mode to the shared default", func() {
		Expect(resolveCacheMode("bogus")).To(Equal(ModelCacheModeShared))
	})

	It("preserves an explicit perService mode", func() {
		Expect(resolveCacheMode(ModelCacheModePerService)).To(Equal(ModelCacheModePerService))
	})

	It("preserves an explicit shared mode", func() {
		Expect(resolveCacheMode(ModelCacheModeShared)).To(Equal(ModelCacheModeShared))
	})
})

var _ = Describe("buildModelInitCommand", func() {
	It("should generate cached remote download command with env var references", func() {
		cmd := buildModelInitCommand(false, true, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring(`mkdir -p "$CACHE_DIR"`))
		Expect(cmd).To(ContainSubstring(`"$MODEL_PATH"`))
		Expect(cmd).To(ContainSubstring("curl -f -L"))
		Expect(cmd).To(ContainSubstring(`"$MODEL_SOURCE"`))
	})

	It("should generate cached local copy command", func() {
		cmd := buildModelInitCommand(true, true, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring(`mkdir -p "$CACHE_DIR"`))
		Expect(cmd).To(ContainSubstring("cp /host-model/model.gguf"))
		Expect(cmd).To(ContainSubstring(`"$MODEL_PATH"`))
	})

	It("should generate error exit for uncached local source", func() {
		cmd := buildModelInitCommand(true, false, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring("ERROR: Local model source requires model cache"))
		Expect(cmd).To(ContainSubstring("exit 1"))
	})

	It("should generate uncached remote download command with env var references", func() {
		cmd := buildModelInitCommand(false, false, RefreshPolicyIfNotPresent)
		Expect(cmd).To(ContainSubstring("curl -f -L"))
		Expect(cmd).To(ContainSubstring(`"$MODEL_SOURCE"`))
		Expect(cmd).To(ContainSubstring(`"$MODEL_PATH"`))
		Expect(cmd).NotTo(ContainSubstring("mkdir -p"))
	})

	It("should not contain user-controlled values in the command string", func() {
		// Verify that a malicious source cannot appear in the shell script.
		// The command is a static template with env var references only.
		maliciousSource := `https://evil.com/$(touch /pwned).gguf`
		cmd := buildModelInitCommand(false, true, RefreshPolicyIfNotPresent)
		Expect(cmd).NotTo(ContainSubstring(maliciousSource))
		Expect(cmd).NotTo(ContainSubstring("touch"))
		Expect(cmd).NotTo(ContainSubstring("evil.com"))

		// Env vars carry the value safely outside the shell script
		env := modelInitEnvVars(maliciousSource, "/models/abc123", "/models/abc123/model.gguf")
		Expect(env[0].Name).To(Equal("MODEL_SOURCE"))
		Expect(env[0].Value).To(Equal(maliciousSource))
	})

	Context("RefreshPolicy=OnChange (http/https revalidation, issue #619)", func() {
		It("cached: emits curl conditional GET against an etag marker beside the model", func() {
			cmd := buildModelInitCommand(false, true, RefreshPolicyOnChange)
			// Still provisions the cache dir like IfNotPresent.
			Expect(cmd).To(ContainSubstring(`mkdir -p "$CACHE_DIR"`))
			// Conditional GET via curl's native ETag flags.
			Expect(cmd).To(ContainSubstring("--etag-compare"))
			Expect(cmd).To(ContainSubstring("--etag-save"))
			// Marker is a dotfile sibling derived from the model path.
			Expect(cmd).To(ContainSubstring(`.etag`))
			Expect(cmd).To(ContainSubstring(`"$MODEL_PATH"`))
			Expect(cmd).To(ContainSubstring(`"$MODEL_SOURCE"`))
			// It is NOT the existence-only path.
			Expect(cmd).NotTo(ContainSubstring("skipping download"))
		})

		It("uncached: emits the same conditional GET without the cache dir mkdir", func() {
			cmd := buildModelInitCommand(false, false, RefreshPolicyOnChange)
			Expect(cmd).To(ContainSubstring("--etag-compare"))
			Expect(cmd).To(ContainSubstring("--etag-save"))
			Expect(cmd).To(ContainSubstring(`"$MODEL_SOURCE"`))
			Expect(cmd).NotTo(ContainSubstring("mkdir -p"))
			Expect(cmd).NotTo(ContainSubstring("skipping download"))
		})

		It("keeps the cached file and exits 0 when revalidation is unreachable", func() {
			cmd := buildModelInitCommand(false, true, RefreshPolicyOnChange)
			// Robustness guard: a network blip must not take down a running
			// InferenceService on pod restart.
			Expect(cmd).To(ContainSubstring(`[ -f "$MODEL_PATH" ]`))
			Expect(cmd).To(ContainSubstring("exit 0"))
			Expect(cmd).To(ContainSubstring("kept cached copy"))
			// A genuinely-missing file still fails the init container.
			Expect(cmd).To(ContainSubstring("exit 1"))
		})

		It("does not change the local (file://) init path", func() {
			// file:// sources are owned by the controller (#635); the init
			// container path must be identical regardless of RefreshPolicy.
			ifNotPresent := buildModelInitCommand(true, true, RefreshPolicyIfNotPresent)
			onChange := buildModelInitCommand(true, true, RefreshPolicyOnChange)
			Expect(onChange).To(Equal(ifNotPresent))
			Expect(onChange).NotTo(ContainSubstring("--etag-compare"))
		})

		It("does not contain user-controlled values in the OnChange command string", func() {
			cmd := buildModelInitCommand(false, true, RefreshPolicyOnChange)
			Expect(cmd).NotTo(ContainSubstring("evil.com"))
			Expect(cmd).NotTo(ContainSubstring("touch"))
		})
	})
})

var _ = Describe("buildCachedStorageConfig RefreshPolicy plumbing", func() {
	It("threads Model.Spec.RefreshPolicy=OnChange into the init command", func() {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Source:        "https://example.com/model.gguf",
				RefreshPolicy: RefreshPolicyOnChange,
			},
			Status: inferencev1alpha1.ModelStatus{CacheKey: "abc123def456"},
		}
		config := buildCachedStorageConfig(model, nil, "", "", "curl:8.18.0", 102)
		cmd := config.initContainers[1].Command[2]
		Expect(cmd).To(ContainSubstring("--etag-compare"))
		Expect(cmd).To(ContainSubstring("kept cached copy"))
	})

	It("keeps existence-only behavior when RefreshPolicy is unset (IfNotPresent default)", func() {
		model := &inferencev1alpha1.Model{
			Spec:   inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
			Status: inferencev1alpha1.ModelStatus{CacheKey: "abc123def456"},
		}
		config := buildCachedStorageConfig(model, nil, "", "", "curl:8.18.0", 102)
		cmd := config.initContainers[1].Command[2]
		Expect(cmd).NotTo(ContainSubstring("--etag-compare"))
		Expect(cmd).To(ContainSubstring("skipping download"))
	})
})

var _ = Describe("cache prep init container (#855)", func() {
	// Helper: build a cache-backed single-file model (no multi-file staging).
	cacheModel := func() *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			Spec:   inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
			Status: inferencev1alpha1.ModelStatus{CacheKey: "abc123"},
		}
	}

	It("prep is present and ordered BEFORE model-downloader in the single-file path", func() {
		config := buildCachedStorageConfig(cacheModel(), nil, "", "", "curl:8.18.0", 102)
		Expect(config.initContainers).To(HaveLen(2))
		Expect(config.initContainers[0].Name).To(Equal("model-cache-prep"))
		Expect(config.initContainers[1].Name).To(Equal("model-downloader"))
	})

	It("prep is present and ordered BEFORE model-downloader in the multi-file staging path", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "gemma", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "hf://unsloth/gemma-4-31B-it-GGUF",
				Files:  []string{"gemma-4-31B-it-UD-Q4_K_XL.gguf", "MTP/gemma-4-31B-it-Q8_0-MTP.gguf"},
				Mmproj: "mmproj-F16.gguf",
			},
			Status: inferencev1alpha1.ModelStatus{CacheKey: "abc123"},
		}
		config := buildCachedStorageConfig(model, nil, "", "", "curl:8.18.0", 102)
		Expect(config.initContainers).To(HaveLen(2))
		Expect(config.initContainers[0].Name).To(Equal("model-cache-prep"))
		Expect(config.initContainers[1].Name).To(Equal("model-downloader"))
	})

	It("DEFAULT case (no explicit podSecurityContext, defaultFSGroup 102): prep command is exactly 'chown 0:102 /models && chmod g+rwX /models'", func() {
		config := buildCachedStorageConfig(cacheModel(), nil, "", "", "curl:8.18.0", 102)
		prep := config.initContainers[0]
		Expect(prep.Command).To(Equal([]string{"sh", "-c", "chown 0:102 /models && chmod g+rwX /models"}))
		// No recursive flag anywhere in the command.
		Expect(prep.Command[2]).NotTo(ContainSubstring("-R"))
	})

	It("EXPLICIT OVERRIDE case: isvc FSGroup=3000, defaultFSGroup=102 -> prep command contains 'chown 0:3000' and NOT '102'", func() {
		isvc := &inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				PodSecurityContext: &corev1.PodSecurityContext{
					FSGroup: int64Ptr(3000),
				},
			},
		}
		config := buildCachedStorageConfig(cacheModel(), isvc, "", "", "curl:8.18.0", 102)
		prep := config.initContainers[0]
		cmd := prep.Command[2]
		Expect(cmd).To(ContainSubstring("chown 0:3000"))
		Expect(cmd).NotTo(ContainSubstring("102"))
	})

	It("fsGroup<=0 case: prep command is 'chown 100:100 /models && chmod 770 /models'", func() {
		config := buildCachedStorageConfig(cacheModel(), nil, "", "", "curl:8.18.0", 0)
		prep := config.initContainers[0]
		Expect(prep.Command).To(Equal([]string{"sh", "-c", "chown 100:100 /models && chmod 770 /models"}))
	})

	It("prep reuses initContainerImage (no hardcoded image)", func() {
		config := buildCachedStorageConfig(cacheModel(), nil, "", "", "my-registry.io/init:v1.2.3", 102)
		prep := config.initContainers[0]
		Expect(prep.Image).To(Equal("my-registry.io/init:v1.2.3"))
		// And the downloader also uses the same image.
		dl := config.initContainers[1]
		Expect(dl.Image).To(Equal("my-registry.io/init:v1.2.3"))
	})

	It("prep SecurityContext: RunAsUser=0, AllowPrivilegeEscalation=false, Capabilities.Drop=[ALL], Capabilities.Add has CHOWN and FOWNER, ReadOnlyRootFilesystem=true, SeccompProfile.Type=RuntimeDefault", func() {
		config := buildCachedStorageConfig(cacheModel(), nil, "", "", "curl:8.18.0", 102)
		prep := config.initContainers[0]
		sc := prep.SecurityContext
		Expect(sc).NotTo(BeNil())
		// Regression guard (0.8.20): the prep MUST run as root. Non-root with
		// capabilities.add does not work -- containerd clears caps when sh
		// execs chown (no ambient caps), so chown fails EPERM and the init
		// container errors out on fsGroupPolicy=None CSIs. Do not flip to
		// non-root without ambient-capability support.
		Expect(*sc.RunAsUser).To(Equal(int64(0)))
		Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())
		Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue())

		Expect(sc.Capabilities).NotTo(BeNil())
		Expect(sc.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))
		Expect(sc.Capabilities.Add).To(ContainElement(corev1.Capability("CHOWN")))
		Expect(sc.Capabilities.Add).To(ContainElement(corev1.Capability("FOWNER")))

		Expect(sc.SeccompProfile).NotTo(BeNil())
		Expect(sc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
	})

	It("prep NOT emitted for the invalid-fileset fail-closed path", func() {
		// Force an invalid fileset by setting Files to a pattern that ResolveFileSet rejects.
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "hf://org/repo",
				Files:  []string{"../../etc/passwd"}, // path escape -> invalid
			},
			Status: inferencev1alpha1.ModelStatus{CacheKey: "abc123"},
		}
		config := buildCachedStorageConfig(model, nil, "", "", "curl:8.18.0", 102)
		// The fail-closed path returns only the invalid-fileset init container,
		// no prep.
		Expect(config.initContainers).To(HaveLen(1))
		Expect(config.initContainers[0].Name).To(Equal("model-downloader"))
		// And its command exits 1 with the InvalidFileSet message.
		Expect(config.initContainers[0].Command[2]).To(ContainSubstring("ERROR: InvalidFileSet"))
	})

	It("prep NOT emitted for the emptyDir path (buildEmptyDirStorageConfig)", func() {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		}
		config := buildEmptyDirStorageConfig(model, nil, "default", "", "curl:8.18.0")
		Expect(config.initContainers).To(HaveLen(1))
		Expect(config.initContainers[0].Name).To(Equal("model-downloader"))
	})
})

var _ = Describe("shouldWarnMissingSkipModelInit", func() {
	tt := func(modelPhase, source string, skipInit *bool) bool {
		model := &inferencev1alpha1.Model{
			Spec:   inferencev1alpha1.ModelSpec{Source: source},
			Status: inferencev1alpha1.ModelStatus{Phase: modelPhase},
		}
		isvc := &inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{SkipModelInit: skipInit},
		}
		return shouldWarnMissingSkipModelInit(model, isvc)
	}
	pTrue := func() *bool { v := true; return &v }
	pFalse := func() *bool { v := false; return &v }

	It("warns: HuggingFace repo ID + Ready Model + skipModelInit unset", func() {
		Expect(tt(PhaseReady, "Qwen/Qwen3.6-35B-A3B", nil)).To(BeTrue())
	})
	It("warns: HuggingFace repo ID + Ready Model + skipModelInit explicitly false", func() {
		Expect(tt(PhaseReady, "Qwen/Qwen3.6-35B-A3B", pFalse())).To(BeTrue())
	})
	It("does not warn: HuggingFace repo ID + skipModelInit=true (correctly configured)", func() {
		Expect(tt(PhaseReady, "Qwen/Qwen3.6-35B-A3B", pTrue())).To(BeFalse())
	})
	It("does not warn: HTTPS source — init container is required to populate the per-namespace cache PVC (issue #363)", func() {
		Expect(tt(PhaseReady, "https://huggingface.co/example/repo/resolve/main/m.gguf", nil)).To(BeFalse())
	})
	It("does not warn: HTTP source — same as HTTPS", func() {
		Expect(tt(PhaseReady, "http://example.com/m.gguf", nil)).To(BeFalse())
	})
	It("does not warn: file:// source — controller copies in-process and Status.Path is populated", func() {
		Expect(tt(PhaseReady, "file:///mnt/models/m.gguf", nil)).To(BeFalse())
	})
	It("does not warn: pvc:// source — model is mounted directly, no init container needed", func() {
		Expect(tt(PhaseReady, "pvc://my-claim/path/m.gguf", nil)).To(BeFalse())
	})
	It("does not warn: Model not yet Ready (irrelevant — warning waits until Status is settled)", func() {
		Expect(tt("Downloading", "Qwen/Qwen3.6-35B-A3B", nil)).To(BeFalse())
	})
})

func getEnvVar(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}
