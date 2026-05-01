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
		config := buildCachedStorageConfig(model, nil, "", "curl:8.18.0")

		Expect(config.modelPath).To(Equal("/models/abc123def456/model.gguf"))
		Expect(config.volumes).To(HaveLen(1))
		Expect(config.volumes[0].Name).To(Equal("model-cache"))
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(ModelCachePVCName))
		Expect(config.initContainers).To(HaveLen(1))
		Expect(config.initContainers[0].Name).To(Equal("model-downloader"))
		Expect(config.initContainers[0].Image).To(Equal("curl:8.18.0"))
		Expect(config.volumeMounts[0].MountPath).To(Equal("/models"))
		Expect(config.volumeMounts[0].ReadOnly).To(BeTrue())

		// Verify env vars are set on the init container
		env := config.initContainers[0].Env
		Expect(env).To(HaveLen(3))
		Expect(env[0]).To(Equal(corev1.EnvVar{Name: "MODEL_SOURCE", Value: "https://example.com/model.gguf"}))
		Expect(env[1]).To(Equal(corev1.EnvVar{Name: "CACHE_DIR", Value: "/models/abc123def456"}))
		Expect(env[2]).To(Equal(corev1.EnvVar{Name: "MODEL_PATH", Value: "/models/abc123def456/model.gguf"}))

		// Verify the command does not contain the raw source URL
		Expect(config.initContainers[0].Command[2]).NotTo(ContainSubstring("example.com"))
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
		config := buildCachedStorageConfig(model, nil, "", "curl:8.18.0")

		Expect(config.volumes).To(HaveLen(2))
		Expect(config.volumes[1].Name).To(Equal("host-model"))
		Expect(config.volumes[1].HostPath.Path).To(Equal("/mnt/models/test.gguf"))

		// Verify env vars are set
		env := config.initContainers[0].Env
		Expect(env).To(HaveLen(3))
		Expect(env[0]).To(Equal(corev1.EnvVar{Name: "MODEL_SOURCE", Value: "file:///mnt/models/test.gguf"}))
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
		config := buildCachedStorageConfig(model, nil, "my-ca-certs", "curl:8.18.0")

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
		Expect(env).To(HaveLen(3))
		Expect(env[0]).To(Equal(corev1.EnvVar{Name: "MODEL_SOURCE", Value: "https://example.com/model.gguf"}))
		Expect(env[1]).To(Equal(corev1.EnvVar{Name: "CACHE_DIR", Value: ""}))
		Expect(env[2]).To(Equal(corev1.EnvVar{Name: "MODEL_PATH", Value: "/models/default-my-model.gguf"}))

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
		config := buildModelStorageConfig(model, nil, "default", true, "", "curl:8.18.0")

		// Should use PVC config, not cached config
		Expect(config.volumes[0].Name).To(Equal("model-source"))
		Expect(config.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("my-claim"))
		Expect(config.initContainers).To(BeEmpty())
	})
})

var _ = Describe("ensureModelCachePVC", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		deletePVCForcibly(context.Background(), "default")
		reconciler = &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	AfterEach(func() {
		deletePVCForcibly(context.Background(), "default")
	})

	It("should create PVC with default 100Gi and ReadWriteOnce", func() {
		err := reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		Expect(pvc.Spec.AccessModes).To(ContainElement(corev1.ReadWriteOnce))
		storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		Expect(storageReq.String()).To(Equal("100Gi"))
		Expect(pvc.Labels["app.kubernetes.io/name"]).To(Equal("llmkube"))
	})

	It("should create PVC with custom size", func() {
		reconciler.ModelCacheSize = "50Gi"
		err := reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		Expect(storageReq.String()).To(Equal("50Gi"))
	})

	It("should create PVC with ReadWriteMany when configured", func() {
		reconciler.ModelCacheAccessMode = "ReadWriteMany"
		err := reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		Expect(pvc.Spec.AccessModes).To(ContainElement(corev1.ReadWriteMany))
	})

	It("should set StorageClassName when configured", func() {
		reconciler.ModelCacheClass = "fast-ssd"
		err := reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: ModelCachePVCName, Namespace: "default"}, pvc)
		Expect(err).NotTo(HaveOccurred())
		Expect(*pvc.Spec.StorageClassName).To(Equal("fast-ssd"))
	})

	It("should not error if PVC already exists", func() {
		err := reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).NotTo(HaveOccurred())
		err = reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).NotTo(HaveOccurred())
	})

	It("should return error for invalid cache size", func() {
		reconciler.ModelCacheSize = "not-a-size"
		err := reconciler.ensureModelCachePVC(context.Background(), "default")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid cache size"))
	})
})
