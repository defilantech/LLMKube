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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Model storage wiring. The controller has three paths for making a model
// visible to the inference pod: mount a pre-staged PVC, fetch through a
// shared cache PVC, or download into an ephemeral emptyDir. Each returns a
// modelStorageConfig that the deployment builder composes into pod spec.
// This file also owns provisioning of the shared cache PVC per namespace.

const ModelCachePVCName = "llmkube-model-cache"

// isLocalModelSource delegates to the shared isLocalSource helper in source.go.
func isLocalModelSource(source string) bool {
	return isLocalSource(source)
}

func buildModelInitCommand(isLocal, useCache bool) string {
	if useCache {
		if isLocal {
			return `mkdir -p "$CACHE_DIR" && if [ ! -f "$MODEL_PATH" ]; then echo 'Copying model from local source...'; cp /host-model/model.gguf "$MODEL_PATH" && echo 'Model copied successfully'; else echo 'Model already cached, skipping copy'; fi`
		}
		return `mkdir -p "$CACHE_DIR" && if [ ! -f "$MODEL_PATH" ]; then echo 'Downloading model...'; curl -f -L -o "$MODEL_PATH" "$MODEL_SOURCE" && echo 'Model downloaded successfully'; else echo 'Model already cached, skipping download'; fi`
	}

	if isLocal {
		return `echo 'ERROR: Local model source requires model cache to be configured.'; exit 1`
	}
	return `if [ ! -f "$MODEL_PATH" ]; then echo 'Downloading model...'; curl -f -L -o "$MODEL_PATH" "$MODEL_SOURCE" && echo 'Model downloaded successfully'; else echo 'Model already exists, skipping download'; fi`
}

func modelInitEnvVars(source, cacheDir, modelPath string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "MODEL_SOURCE", Value: source},
		{Name: "CACHE_DIR", Value: cacheDir},
		{Name: "MODEL_PATH", Value: modelPath},
	}
}

type modelStorageConfig struct {
	modelPath      string
	initContainers []corev1.Container
	volumes        []corev1.Volume
	volumeMounts   []corev1.VolumeMount
}

func buildModelStorageConfig(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService, namespace string, useCache bool, caCertConfigMap string, initContainerImage string) modelStorageConfig {
	if isPVCSource(model.Spec.Source) {
		return buildPVCStorageConfig(model)
	}
	if useCache {
		return buildCachedStorageConfig(model, isvc, caCertConfigMap, initContainerImage)
	}
	return buildEmptyDirStorageConfig(model, isvc, namespace, caCertConfigMap, initContainerImage)
}

// buildPVCStorageConfig mounts the user's PVC directly as a read-only volume.
// No init container is needed since the model is already on the PVC.
func buildPVCStorageConfig(model *inferencev1alpha1.Model) modelStorageConfig {
	claimName, modelFilePath, _ := parsePVCSource(model.Spec.Source)

	modelPath := fmt.Sprintf("/model-source/%s", modelFilePath)

	return modelStorageConfig{
		modelPath: modelPath,
		volumes: []corev1.Volume{
			{
				Name: "model-source",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: claimName,
						ReadOnly:  true,
					},
				},
			},
		},
		volumeMounts: []corev1.VolumeMount{
			{Name: "model-source", MountPath: "/model-source", ReadOnly: true},
		},
	}
}

func buildCachedStorageConfig(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService, caCertConfigMap string, initContainerImage string) modelStorageConfig {
	cacheDir := fmt.Sprintf("/models/%s", model.Status.CacheKey)
	modelPath := fmt.Sprintf("%s/model.gguf", cacheDir)

	initVolumeMounts := []corev1.VolumeMount{
		{Name: "model-cache", MountPath: "/models"},
	}

	volumes := []corev1.Volume{
		{
			Name: "model-cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: ModelCachePVCName,
					ReadOnly:  false,
				},
			},
		},
	}

	if isLocalModelSource(model.Spec.Source) {
		localPath := getLocalPath(model.Spec.Source)
		volumes = append(volumes, corev1.Volume{
			Name: "host-model",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: localPath,
					Type: func() *corev1.HostPathType { t := corev1.HostPathFile; return &t }(),
				},
			},
		})
		initVolumeMounts = append(initVolumeMounts, corev1.VolumeMount{
			Name:      "host-model",
			MountPath: "/host-model/model.gguf",
			ReadOnly:  true,
		})
	}

	cmd := buildModelInitCommand(isLocalModelSource(model.Spec.Source), true)
	env := modelInitEnvVars(model.Spec.Source, cacheDir, modelPath)
	if caCertConfigMap != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "custom-ca-cert",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: caCertConfigMap},
				},
			},
		})
		initVolumeMounts = append(initVolumeMounts, corev1.VolumeMount{
			Name:      "custom-ca-cert",
			MountPath: "/custom-certs",
			ReadOnly:  true,
		})
		cmd = fmt.Sprintf("export CURL_CA_BUNDLE=/custom-certs/$(ls /custom-certs | grep -v '^\\.' | head -n 1) && %s", cmd)
	}

	return modelStorageConfig{
		modelPath: modelPath,
		initContainers: []corev1.Container{
			{
				Name:            "model-downloader",
				Image:           initContainerImage,
				Command:         []string{"sh", "-c", cmd},
				Env:             env,
				VolumeMounts:    initVolumeMounts,
				SecurityContext: initContainerSecurityContext(isvc),
			},
		},
		volumes:      volumes,
		volumeMounts: []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models", ReadOnly: true}},
	}
}

func buildEmptyDirStorageConfig(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService, namespace string, caCertConfigMap string, initContainerImage string) modelStorageConfig {
	modelFileName := fmt.Sprintf("%s-%s.gguf", namespace, model.Name)
	modelPath := fmt.Sprintf("/models/%s", modelFileName)

	initVolumeMounts := []corev1.VolumeMount{{Name: "model-storage", MountPath: "/models"}}
	volumes := []corev1.Volume{
		{
			Name:         "model-storage",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}

	cmd := buildModelInitCommand(isLocalModelSource(model.Spec.Source), false)
	env := modelInitEnvVars(model.Spec.Source, "", modelPath)
	if caCertConfigMap != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "custom-ca-cert",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: caCertConfigMap},
				},
			},
		})
		initVolumeMounts = append(initVolumeMounts, corev1.VolumeMount{
			Name:      "custom-ca-cert",
			MountPath: "/custom-certs",
			ReadOnly:  true,
		})
		cmd = fmt.Sprintf("export CURL_CA_BUNDLE=/custom-certs/$(ls /custom-certs | grep -v '^\\.' | head -n 1) && %s", cmd)
	}

	return modelStorageConfig{
		modelPath: modelPath,
		initContainers: []corev1.Container{
			{
				Name:            "model-downloader",
				Image:           initContainerImage,
				Command:         []string{"sh", "-c", cmd},
				Env:             env,
				VolumeMounts:    initVolumeMounts,
				SecurityContext: initContainerSecurityContext(isvc),
			},
		},
		volumes:      volumes,
		volumeMounts: []corev1.VolumeMount{{Name: "model-storage", MountPath: "/models", ReadOnly: true}},
	}
}

// ensureModelCachePVC creates the shared llmkube-model-cache PVC in the given
// namespace if it does not already exist. Used by cached-mode deployments.
func (r *InferenceServiceReconciler) ensureModelCachePVC(ctx context.Context, namespace string) error {
	log := logf.FromContext(ctx)

	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: ModelCachePVCName, Namespace: namespace}, pvc)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check for existing PVC: %w", err)
	}

	log.Info("Creating model cache PVC in namespace", "namespace", namespace)

	accessMode := corev1.ReadWriteOnce
	if r.ModelCacheAccessMode == "ReadWriteMany" {
		accessMode = corev1.ReadWriteMany
	}

	size := "100Gi"
	if r.ModelCacheSize != "" {
		size = r.ModelCacheSize
	}
	storageSize, err := resource.ParseQuantity(size)
	if err != nil {
		return fmt.Errorf("invalid cache size %q: %w", size, err)
	}

	newPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ModelCachePVCName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "llmkube",
				"app.kubernetes.io/component":  "model-cache",
				"app.kubernetes.io/managed-by": "llmkube-controller",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageSize,
				},
			},
		},
	}

	if r.ModelCacheClass != "" {
		newPVC.Spec.StorageClassName = &r.ModelCacheClass
	}

	if err := r.Create(ctx, newPVC); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create PVC: %w", err)
	}

	log.Info("Created model cache PVC", "namespace", namespace, "name", ModelCachePVCName)
	return nil
}
