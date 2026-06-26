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
	"path/filepath"
	"strings"

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

// ModelCachePVCName is the name of the shared, cluster-single model cache PVC.
// It is used in the default shared mode (ModelCacheModeShared); the opt-in
// per-InferenceService mode names each cache PVC after its InferenceService
// (see modelCachePVCName).
const ModelCachePVCName = "llmkube-model-cache"

// Model cache provisioning modes. shared (the default) keeps a single,
// cluster-wide llmkube-model-cache PVC that the operator mounts and every
// InferenceService init container downloads into, giving cross-isvc dedup and a
// cache `llmkube cache list` can inspect. This is the proven default; on a
// multi-node cluster it needs an RWX storage class so any node can reach it.
// perService is the opt-in escape hatch for multi-node clusters WITHOUT RWX: it
// gives each InferenceService its own RWO, WaitForFirstConsumer cache PVC that
// binds on the node the serving pod schedules to, so the GPU pod and its cache
// co-locate even when the operator runs on a different node (#728), at the cost
// of cross-isvc dedup.
const (
	ModelCacheModePerService = "perService"
	ModelCacheModeShared     = "shared"
)

// resolveCacheMode maps an unset mode to the default (shared). An empty string
// reaches the reconciler when the operator is run without --model-cache-mode
// (e.g. ad-hoc envtest), so callers must funnel through this rather than
// comparing the raw field.
func resolveCacheMode(mode string) string {
	if mode == ModelCacheModePerService {
		return ModelCacheModePerService
	}
	return ModelCacheModeShared
}

// modelCachePVCName returns the name of the model cache PVC for the given mode.
// In shared mode (the default, and the resolution of an empty mode) this is the
// single cluster-wide PVC; in perService mode it is the per-InferenceService PVC
// "<isvc>-model-cache". A nil isvc (unit tests that exercise the builder
// directly) falls back to the shared name.
func modelCachePVCName(isvc *inferencev1alpha1.InferenceService, mode string) string {
	if resolveCacheMode(mode) == ModelCacheModeShared || isvc == nil {
		return ModelCachePVCName
	}
	return fmt.Sprintf("%s-model-cache", isvc.Name)
}

// isLocalModelSource delegates to the shared isLocalSource helper in source.go.
func isLocalModelSource(source string) bool {
	return isLocalSource(source)
}

// addCACertVolume appends the custom CA cert volume and volume mount to the
// given slices, and prefixes the command with the CURL_CA_BUNDLE export.
// No-op when caCertConfigMap is empty.
func addCACertVolume(volumes *[]corev1.Volume, mounts *[]corev1.VolumeMount, cmd *string, caCertConfigMap string) {
	if caCertConfigMap == "" {
		return
	}
	*volumes = append(*volumes, corev1.Volume{
		Name: "custom-ca-cert",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: caCertConfigMap},
			},
		},
	})
	*mounts = append(*mounts, corev1.VolumeMount{
		Name:      "custom-ca-cert",
		MountPath: "/custom-certs",
		ReadOnly:  true,
	})
	*cmd = fmt.Sprintf("export CURL_CA_BUNDLE=/custom-certs/$(ls /custom-certs | grep -v '^\\.' | head -n 1) && %s", *cmd)
}

func buildModelInitCommand(isLocal, useCache bool, refreshPolicy string) string {
	if useCache {
		if isLocal {
			return `mkdir -p "$CACHE_DIR" && if [ ! -f "$MODEL_PATH" ]; then echo 'Copying model from local source...'; cp /host-model/model.gguf "$MODEL_PATH" && echo 'Model copied successfully'; else echo 'Model already cached, skipping copy'; fi`
		}
		if refreshPolicy == RefreshPolicyOnChange {
			return "mkdir -p \"$CACHE_DIR\" && " + remoteRevalidateScript
		}
		return `mkdir -p "$CACHE_DIR" && if [ ! -f "$MODEL_PATH" ]; then echo 'Downloading model...'; curl -f -L -o "$MODEL_PATH" "$MODEL_SOURCE" && echo 'Model downloaded successfully'; else echo 'Model already cached, skipping download'; fi`
	}

	if isLocal {
		return `echo 'ERROR: Local model source requires model cache to be configured.'; exit 1`
	}
	if refreshPolicy == RefreshPolicyOnChange {
		return remoteRevalidateScript
	}
	return `if [ ! -f "$MODEL_PATH" ]; then echo 'Downloading model...'; curl -f -L -o "$MODEL_PATH" "$MODEL_SOURCE" && echo 'Model downloaded successfully'; else echo 'Model already exists, skipping download'; fi`
}

// remoteRevalidateScript implements RefreshPolicy=OnChange for http/https
// sources fetched by the init container. It uses curl's native conditional
// GET (--etag-compare / --etag-save) against a marker file kept next to the
// model on the PVC: on a 304 curl leaves the cached file untouched, on a 200
// (ETag changed) it overwrites in place. The cache layout is unchanged; the
// marker is a dotfile sibling of the model file.
//
// Robustness: the init container gates pod startup, so a transient network
// failure (air-gapped, upstream 5xx, DNS) must not take down an
// InferenceService on pod restart. If revalidation fails but a cached copy
// already exists, the script logs and exits 0, keeping the cached file. Only a
// genuinely-missing file (nothing cached and the fetch failed) fails the init
// container.
//
// curlimages/curl 8.x supports --etag-compare/--etag-save (added in curl
// 7.68.0), so no HEAD-compare fallback is needed for the default image.
const remoteRevalidateScript = `ETAG_MARKER="$(dirname "$MODEL_PATH")/.$(basename "$MODEL_PATH").etag"; ` +
	`echo 'Revalidating model against upstream (RefreshPolicy=OnChange)...'; ` +
	`if curl -fsSL --etag-compare "$ETAG_MARKER" --etag-save "$ETAG_MARKER" -o "$MODEL_PATH" "$MODEL_SOURCE"; then ` +
	`echo 'Model revalidated (downloaded or unchanged)'; ` +
	`elif [ -f "$MODEL_PATH" ]; then ` +
	`echo 'Revalidation unreachable; kept cached copy'; exit 0; ` +
	`else ` +
	`echo 'ERROR: model missing and revalidation failed'; exit 1; ` +
	`fi`

func modelInitEnvVars(source, cacheDir, modelPath string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "MODEL_SOURCE", Value: source},
		{Name: "CACHE_DIR", Value: cacheDir},
		{Name: "MODEL_PATH", Value: modelPath},
	}
}

// resolveHFSourceURL converts hf://repo-id sources to their huggingface.co
// HTTPS equivalent for init container env vars. Non-hf:// sources pass through unchanged.
func resolveHFSourceURL(source string) string {
	if strings.HasPrefix(source, "hf://") {
		repo := strings.TrimPrefix(source, "hf://")
		return "https://huggingface.co/" + repo
	}
	return source
}

// hasMultiFileStaging reports whether the model uses multi-file staging via
// spec.files or spec.mmproj.
func hasMultiFileStaging(model *inferencev1alpha1.Model) bool {
	return model != nil && (len(model.Spec.Files) > 0 || model.Spec.Mmproj != "")
}

// modelStagingPlan resolves the model's declared files into a staging plan.
// Returns nil when there is no multi-file staging or resolution fails.
func modelStagingPlan(model *inferencev1alpha1.Model) *StagingPlan {
	if !hasMultiFileStaging(model) {
		return nil
	}
	plan, err := ResolveFileSet(model.Spec.Files, model.Spec.Mmproj, nil)
	if err != nil || plan == nil {
		return nil
	}
	return plan
}

// multiFileInitEnvVars returns env vars for a multi-file init container.
// MODEL_SOURCE is normalized (hf:// -> https://huggingface.co/), and
// MODEL_FILES is newline-delimited.
func multiFileInitEnvVars(source, cacheDir string, files []string) []corev1.EnvVar {
	normalized := resolveHFSourceURL(source)
	return []corev1.EnvVar{
		{Name: "MODEL_SOURCE", Value: normalized},
		{Name: "CACHE_DIR", Value: cacheDir},
		{Name: "MODEL_FILES", Value: strings.Join(files, "\n")},
	}
}

// buildMultiFileInitCommand returns a shell command that downloads each file
// listed in $MODEL_FILES from the normalized $MODEL_SOURCE. For cached storage
// (useCache=true), it creates $CACHE_DIR first. For emptyDir (useCache=false),
// it creates /models. The command uses env vars only, never embedding user
// values directly in the script.
func buildMultiFileInitCommand(useCache bool, refreshPolicy string) string {
	prefix := `mkdir -p "$CACHE_DIR" && `
	if !useCache {
		prefix = `mkdir -p /models && `
	}

	normalizeFn := `normalize_hf_source() { case "$1" in hf://*) echo "https://huggingface.co/${1#hf://}" ;; *) echo "$1" ;; esac; }` + " && "

	if refreshPolicy == RefreshPolicyOnChange {
		body := normalizeFn +
			`SOURCE="$(normalize_hf_source "$MODEL_SOURCE")" && ` +
			`printf '%s\n' "$MODEL_FILES" | while IFS= read -r rel; do ` +
			`[ -n "$rel" ] || continue; ` +
			`dest="$CACHE_DIR/$rel"; ` +
			`mkdir -p "$(dirname "$dest")"; ` +
			`url="${SOURCE%/}/resolve/main/$rel"; ` +
			`etag="$(dirname "$dest")/.$(basename "$dest").etag"; ` +
			`if curl -fsSL --etag-compare "$etag" --etag-save "$etag" -o "$dest" "$url"; then ` +
			`echo "Model artifact $rel revalidated"; ` +
			`elif [ -f "$dest" ]; then echo "Revalidation unreachable for $rel; kept cached copy"; ` +
			`else echo "ERROR: model artifact $rel missing and revalidation failed"; exit 1; fi; ` +
			`done`
		return prefix + body
	}

	body := normalizeFn +
		`SOURCE="$(normalize_hf_source "$MODEL_SOURCE")" && ` +
		`printf '%s\n' "$MODEL_FILES" | while IFS= read -r rel; do ` +
		`[ -n "$rel" ] || continue; ` +
		`dest="$CACHE_DIR/$rel"; ` +
		`mkdir -p "$(dirname "$dest")"; ` +
		`url="${SOURCE%/}/resolve/main/$rel"; ` +
		`if [ ! -f "$dest" ]; then ` +
		`echo "Downloading model artifact $rel..."; ` +
		`curl -f -L -o "$dest" "$url" || { echo "ERROR: failed to download $rel"; exit 1; }; ` +
		`else echo "Model artifact $rel already cached, skipping download"; fi; ` +
		`done`
	return prefix + body
}

type modelStorageConfig struct {
	modelPath      string
	initContainers []corev1.Container
	volumes        []corev1.Volume
	volumeMounts   []corev1.VolumeMount
}

func buildModelStorageConfig(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService, namespace string, useCache bool, cacheMode string, caCertConfigMap string, initContainerImage string) modelStorageConfig {
	if isPVCSource(model.Spec.Source) {
		return buildPVCStorageConfig(model)
	}
	if useCache {
		return buildCachedStorageConfig(model, isvc, cacheMode, caCertConfigMap, initContainerImage)
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

func buildCachedStorageConfig(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService, cacheMode string, caCertConfigMap string, initContainerImage string) modelStorageConfig {
	cacheDir := fmt.Sprintf("/models/%s", model.Status.CacheKey)

	// Multi-file staging branch: when spec.files or spec.mmproj are set, use
	// the staging plan to download all artifacts. Returns early.
	if plan := modelStagingPlan(model); plan != nil {
		modelPath := stagedCachePath(cacheDir, plan.Primary)
		cmd := buildMultiFileInitCommand(true, model.Spec.RefreshPolicy)
		env := multiFileInitEnvVars(model.Spec.Source, cacheDir, plan.Files)

		initVolumeMounts := []corev1.VolumeMount{
			{Name: "model-cache", MountPath: "/models"},
		}
		volumes := []corev1.Volume{
			{
				Name: "model-cache",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: modelCachePVCName(isvc, cacheMode),
						ReadOnly:  false,
					},
				},
			},
		}

		addCACertVolume(&volumes, &initVolumeMounts, &cmd, caCertConfigMap)

		return modelStorageConfig{
			modelPath: modelPath,
			initContainers: []corev1.Container{{
				Name:            "model-downloader",
				Image:           initContainerImage,
				Command:         []string{"sh", "-c", cmd},
				Env:             env,
				VolumeMounts:    initVolumeMounts,
				SecurityContext: initContainerSecurityContext(isvc),
			}},
			volumes:      volumes,
			volumeMounts: []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models", ReadOnly: true}},
		}
	}

	// Match the basename the Model controller renames the file to after
	// parsing GGUF metadata. If the controller has already populated
	// Status.Path, use that basename verbatim so the init container's cache
	// hit lands on the same file. Otherwise (e.g. HF repo sources where the
	// controller does no download), use the canonical basename so the init
	// container creates the file at the same path the controller would.
	basename := canonicalModelBasename(model)
	if model.Status.Path != "" {
		basename = filepath.Base(model.Status.Path)
	}
	modelPath := fmt.Sprintf("%s/%s", cacheDir, basename)

	initVolumeMounts := []corev1.VolumeMount{
		{Name: "model-cache", MountPath: "/models"},
	}

	volumes := []corev1.Volume{
		{
			Name: "model-cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: modelCachePVCName(isvc, cacheMode),
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

	cmd := buildModelInitCommand(isLocalModelSource(model.Spec.Source), true, model.Spec.RefreshPolicy)
	env := modelInitEnvVars(model.Spec.Source, cacheDir, modelPath)
	addCACertVolume(&volumes, &initVolumeMounts, &cmd, caCertConfigMap)

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
	// Multi-file staging branch for emptyDir storage.
	if plan := modelStagingPlan(model); plan != nil {
		modelPath := fmt.Sprintf("/models/%s-%s/%s", namespace, model.Name, plan.Primary)
		cmd := buildMultiFileInitCommand(false, model.Spec.RefreshPolicy)
		env := multiFileInitEnvVars(model.Spec.Source, fmt.Sprintf("/models/%s-%s", namespace, model.Name), plan.Files)

		initVolumeMounts := []corev1.VolumeMount{{Name: "model-storage", MountPath: "/models"}}
		volumes := []corev1.Volume{
			{
				Name:         "model-storage",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		}

		addCACertVolume(&volumes, &initVolumeMounts, &cmd, caCertConfigMap)

		return modelStorageConfig{
			modelPath: modelPath,
			initContainers: []corev1.Container{{
				Name:            "model-downloader",
				Image:           initContainerImage,
				Command:         []string{"sh", "-c", cmd},
				Env:             env,
				VolumeMounts:    initVolumeMounts,
				SecurityContext: initContainerSecurityContext(isvc),
			}},
			volumes:      volumes,
			volumeMounts: []corev1.VolumeMount{{Name: "model-storage", MountPath: "/models", ReadOnly: true}},
		}
	}

	modelFileName := fmt.Sprintf("%s-%s.gguf", namespace, model.Name)
	modelPath := fmt.Sprintf("/models/%s", modelFileName)

	initVolumeMounts := []corev1.VolumeMount{{Name: "model-storage", MountPath: "/models"}}
	volumes := []corev1.Volume{
		{
			Name:         "model-storage",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}

	cmd := buildModelInitCommand(isLocalModelSource(model.Spec.Source), false, model.Spec.RefreshPolicy)
	env := modelInitEnvVars(model.Spec.Source, "", modelPath)
	addCACertVolume(&volumes, &initVolumeMounts, &cmd, caCertConfigMap)

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

// ensureModelCachePVC creates the model cache PVC for an InferenceService if it
// does not already exist.
//
// In the default shared mode the single cluster-wide llmkube-model-cache PVC is
// created (no owner reference, since it outlives any one InferenceService) so
// every InferenceService shares one cache (cross-isvc dedup) and `cache list`
// can inspect it. On a multi-node cluster this needs an RWX storage class so any
// node can reach it.
//
// In the opt-in perService mode the PVC is named "<isvc>-model-cache", is RWO,
// uses the cluster default storage class (which is WaitForFirstConsumer in the
// common topology-aware case) unless an explicit class is configured, and is
// owner-ref'd to the InferenceService so it is garbage-collected with it. RWO +
// WaitForFirstConsumer is the #728 path: the PVC binds on the node the serving
// pod schedules to (the GPU node), co-locating download and serve instead of
// pinning the cache to the operator's node. Use it on multi-node clusters that
// have no RWX storage class.
func (r *InferenceServiceReconciler) ensureModelCachePVC(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	log := logf.FromContext(ctx)

	shared := resolveCacheMode(r.ModelCacheMode) == ModelCacheModeShared
	namespace := isvc.Namespace
	pvcName := modelCachePVCName(isvc, r.ModelCacheMode)

	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: namespace}, pvc)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check for existing PVC: %w", err)
	}

	log.Info("Creating model cache PVC", "namespace", namespace, "name", pvcName, "mode", r.ModelCacheMode)

	// Per-isvc caches are RWO so they can bind WaitForFirstConsumer on the
	// serving node. Only the shared cache honors the RWX opt-in.
	accessMode := corev1.ReadWriteOnce
	if shared && r.ModelCacheAccessMode == "ReadWriteMany" {
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
			Name:      pvcName,
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

	// Do NOT set volumeBindingMode here; that is a StorageClass property, not a
	// PVC one. Leaving StorageClassName unset uses the cluster default class,
	// whose binding mode (WaitForFirstConsumer for topology-aware provisioners
	// like GKE PD, EBS, local-path) defers binding to first pod schedule. An
	// explicitly-configured class is honored as-is.
	if r.ModelCacheClass != "" {
		newPVC.Spec.StorageClassName = &r.ModelCacheClass
	}

	// Owner-ref per-isvc caches to their InferenceService so they are
	// garbage-collected with it (no leaked caches). The shared cache is not
	// owner-ref'd: it intentionally outlives any single InferenceService.
	if !shared {
		if err := setControllerReferenceUnblocked(isvc, newPVC, r.Scheme); err != nil {
			return fmt.Errorf("failed to set owner reference on model cache PVC: %w", err)
		}
	}

	if err := r.Create(ctx, newPVC); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create PVC: %w", err)
	}

	log.Info("Created model cache PVC", "namespace", namespace, "name", pvcName)
	return nil
}
