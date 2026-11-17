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
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// spec.modelCache.runtimeCache (#2117): the per-service directory, its mount
// path, the init container that creates it, and the reasons the field is
// declined.
const (
	// runtimeCacheMountPath is where the per-service cache directory is
	// mounted in the serving container, beside the read-only /models mount.
	// It is also the root the prep container mounts the claim at, so the two
	// render the same paths.
	runtimeCacheMountPath = "/runtime-cache"

	// runtimeCacheClaimDirName is the claim-root directory holding one
	// subdirectory per InferenceService.
	runtimeCacheClaimDirName = "_runtime-cache"

	// runtimeCachePrepContainerName creates the per-service directory and the
	// runtime's cache directories with a controlled owner.
	runtimeCachePrepContainerName = "runtime-cache-prep"

	// ReasonRuntimeCacheUnsupported reports a runtimeCache request the serving
	// Pod cannot honor: no writable cache claim, a runtime without a persistent
	// cache, or a Metal service with no serving container.
	ReasonRuntimeCacheUnsupported = "RuntimeCacheUnsupported"

	// ReasonRuntimeCacheNeedsPrepare reports a runtimeCache request declined
	// because --model-cache-prep=never leaves nothing to create the directory.
	ReasonRuntimeCacheNeedsPrepare = "RuntimeCacheNeedsPrepare"
)

// runtimeCacheClaimDir returns the per-service directory on the cache claim.
// The namespace is part of it because shared mode gives every namespace the
// same claim.
func runtimeCacheClaimDir(isvc *inferencev1alpha1.InferenceService) string {
	return path.Join(runtimeCacheClaimDirName, isvc.Namespace, isvc.Name)
}

// runtimeCacheDirsFor returns the runtime's cache directories, or nil when the
// runtime has no cache worth keeping.
func runtimeCacheDirsFor(backend RuntimeBackend) []RuntimeCacheDir {
	p, ok := backend.(RuntimeCacheProvider)
	if !ok {
		return nil
	}
	return p.RuntimeCacheDirs()
}

// runtimeCacheWhyNot reports whether the service gets a persistent runtime
// cache, and when it asked for one but cannot have it, the reason and message
// behind the warning event. The request is the effective modelCache's, so a
// Model-level block reaches every service that references it.
func (r *InferenceServiceReconciler) runtimeCacheWhyNot(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	cache effectiveModelCache,
	backend RuntimeBackend,
) (requested bool, reason, message string) {
	if !cache.runtimeCacheEnabled() {
		return false, "", ""
	}
	if isMetalModel(model) {
		return false, ReasonRuntimeCacheUnsupported, fmt.Sprintf(
			"spec.modelCache.runtimeCache.enabled is ignored: Model %q runs on the metal-agent, which starts no serving container to keep a cache from",
			model.Name)
	}
	if len(runtimeCacheDirsFor(backend)) == 0 {
		return false, ReasonRuntimeCacheUnsupported, fmt.Sprintf(
			"spec.modelCache.runtimeCache.enabled is ignored: runtime %q keeps no compile or autotune cache; only vllm and sglang do, and llama.cpp needs none",
			runtimeNameLabel(isvc))
	}
	if !cachePrepIncluded(r.ModelCachePrep) {
		return false, ReasonRuntimeCacheNeedsPrepare,
			"spec.modelCache.runtimeCache.enabled is ignored: --model-cache-prep=never omits the init container that creates the per-service cache directory, and the serving container cannot write a directory kubelet created as root"
	}
	// A user mount at the operator's own path owns the feature, as with
	// /dev/shm (#2111). Nothing is injected and nothing is declined.
	if userMountsAt(isvc.Spec.ExtraVolumeMounts, runtimeCacheMountPath) {
		return false, "", ""
	}
	switch {
	case !operatorStagesModel(isvc, backend):
		return false, ReasonRuntimeCacheUnsupported,
			"spec.modelCache.runtimeCache.enabled is ignored: spec.skipModelInit stages no model, so the Pod mounts no cache claim to keep the runtime's caches on"
	case isPVCSource(model.Spec.Source), isOCISource(model.Spec.Source):
		return false, ReasonRuntimeCacheUnsupported, fmt.Sprintf(
			"spec.modelCache.runtimeCache.enabled is ignored: model source %q is pre-staged read-only, so no cache claim is mounted",
			model.Spec.Source)
	case r.ModelCachePath == "":
		return false, ReasonRuntimeCacheUnsupported,
			"spec.modelCache.runtimeCache.enabled is ignored: model caching is disabled on the operator (--model-cache-path unset / chart modelCache.enabled=false), so the Pod mounts an ephemeral emptyDir"
	case cache.isEphemeral():
		return false, ReasonRuntimeCacheUnsupported,
			"spec.modelCache.runtimeCache.enabled is ignored: persistence is Ephemeral, so the Pod mounts an emptyDir that dies with it"
	case effectiveModelCacheKey(model) == "":
		return false, ReasonRuntimeCacheUnsupported, fmt.Sprintf(
			"spec.modelCache.runtimeCache.enabled is ignored: model %q has no cache key (local source, or fingerprinting has not completed yet), so the Pod mounts an ephemeral emptyDir",
			model.Name)
	}
	return true, "", ""
}

// runtimeCacheRequested reports whether the serving Pod gets the mount, env and
// prep container. The prep container is why the request needs the operator's
// own setup: without it nothing creates the directory with an owner the
// serving container can write as.
func (r *InferenceServiceReconciler) runtimeCacheRequested(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	cache effectiveModelCache,
	backend RuntimeBackend,
) bool {
	requested, _, _ := r.runtimeCacheWhyNot(isvc, model, cache, backend)
	return requested
}

// modelCacheVolumeFor returns the name of the volume the pod mounts at /models,
// when that volume is the resolved cache claim. Empty otherwise, which is how
// a service that stages into an emptyDir or a pre-staged source a future
// storage path does not mount as the cache is declined.
func modelCacheVolumeFor(sc modelStorageConfig, cache effectiveModelCache, mode string) string {
	for _, m := range sc.volumeMounts {
		if path.Clean(m.MountPath) != DefaultModelCachePath {
			continue
		}
		for _, v := range sc.volumes {
			if v.Name != m.Name || v.PersistentVolumeClaim == nil {
				continue
			}
			if v.PersistentVolumeClaim.ClaimName == cache.pvcName(mode) {
				return v.Name
			}
		}
	}
	return ""
}

// runtimeCacheMount returns the mount and env that keep the runtime's caches on
// the claim volume, or nils when the pod should get neither. The mount reuses
// the weights volume through a subPath, so the pod never sees a sibling
// service's cache directory.
func runtimeCacheMount(isvc *inferencev1alpha1.InferenceService, backend RuntimeBackend, volume string) (*corev1.VolumeMount, []corev1.EnvVar) {
	dirs := runtimeCacheDirsFor(backend)
	if len(dirs) == 0 || volume == "" {
		return nil, nil
	}
	mounts := isvc.Spec.ExtraVolumeMounts
	if userMountsAt(mounts, runtimeCacheMountPath) {
		return nil, nil
	}
	mount := &corev1.VolumeMount{
		Name:      volume,
		MountPath: runtimeCacheMountPath,
		SubPath:   runtimeCacheClaimDir(isvc),
	}
	var env []corev1.EnvVar
	for _, d := range dirs {
		if d.Default != "" && userMountCovers(mounts, d.Default) {
			continue
		}
		value := runtimeCacheMountPath
		if d.Value != "" {
			value = path.Join(runtimeCacheMountPath, d.Value)
		}
		env = append(env, corev1.EnvVar{Name: d.Env, Value: value})
	}
	return mount, env
}

// userMountsAt reports whether the user mounts a volume exactly at p.
func userMountsAt(mounts []corev1.VolumeMount, p string) bool {
	p = path.Clean(p)
	for _, m := range mounts {
		if path.Clean(m.MountPath) == p {
			return true
		}
	}
	return false
}

// userMountCovers reports whether the user mounts a volume at p or above it, so
// the runtime writes that cache into the user's volume instead.
func userMountCovers(mounts []corev1.VolumeMount, p string) bool {
	p = path.Clean(p)
	for _, m := range mounts {
		mp := path.Clean(m.MountPath)
		if mp == p || strings.HasPrefix(p, mp+"/") {
			return true
		}
	}
	return false
}

// runtimeCachePrepCommand creates the per-service directory and every declared
// cache directory, then hands them to the pod's group (or to the downloader's
// uid when fsGroup is disabled), mirroring cachePrepInitContainer. The chown
// and chmod are deliberately not recursive, for the same reason there: what the
// runtime already wrote keeps its owner.
func runtimeCachePrepCommand(claimDir string, dirs []RuntimeCacheDir, fsGroup, ownerUID int64) string {
	root := path.Join(runtimeCacheMountPath, claimDir)
	paths := []string{root}
	for _, d := range dirs {
		if d.Dir == "" {
			continue
		}
		paths = append(paths, path.Join(root, d.Dir))
	}
	joined := strings.Join(paths, " ")
	var owner string
	if fsGroup > 0 {
		owner = fmt.Sprintf("chown 0:%d %s && chmod g+rwX %s", fsGroup, joined, joined)
	} else {
		owner = fmt.Sprintf("chown %d:0 %s && chmod 770 %s", ownerUID, joined, joined)
	}
	return fmt.Sprintf("mkdir -p %s && %s", joined, owner)
}

// runtimeCachePrepInitContainer renders the container that creates the
// per-service directory. It mounts the claim at the same root the serving
// container mounts the per-service directory at, so claimDir is created under
// it rather than mounted as a subPath that does not exist yet.
func runtimeCachePrepInitContainer(initImage, claimDir, volume string, dirs []RuntimeCacheDir, fsGroup, ownerUID int64) corev1.Container {
	return corev1.Container{
		Name:    runtimeCachePrepContainerName,
		Image:   initImage,
		Command: []string{"sh", "-c", runtimeCachePrepCommand(claimDir, dirs, fsGroup, ownerUID)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: volume, MountPath: runtimeCacheMountPath},
		},
		SecurityContext: cachePrepSecurityContext(),
	}
}

// warnRuntimeCacheNotApplied emits the warning a service gets when it asks for
// a persistent runtime cache it cannot have.
func (r *InferenceServiceReconciler) warnRuntimeCacheNotApplied(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	cache effectiveModelCache,
	backend RuntimeBackend,
) {
	if r.Recorder == nil {
		return
	}
	requested, reason, message := r.runtimeCacheWhyNot(isvc, model, cache, backend)
	if requested || reason == "" {
		return
	}
	r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, reason, "Reconcile", "%s", message)
}
