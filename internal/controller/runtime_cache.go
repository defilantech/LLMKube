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
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

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

	// ReasonRuntimeCacheClaimReadOnly reports a runtimeCache request declined
	// because the cache claim offers no writable access mode, so the directory
	// the runtime would write could not be created on it.
	ReasonRuntimeCacheClaimReadOnly = "RuntimeCacheClaimReadOnly"

	// ReasonRuntimeCacheFSGroupMismatch reports a runtimeCache request declined
	// on a prefetched service whose own fsGroup differs from the group the
	// prefetch job writes the claim in: mounting the claim writable would make
	// kubelet re-own every staged file on each pod start.
	ReasonRuntimeCacheFSGroupMismatch = "RuntimeCacheFSGroupMismatch"
)

// runtimeCachePlan is one render's decision for spec.modelCache.runtimeCache:
// the runtime's cache directories to point at the per-service directory, and,
// when the request is declined, the reason and message behind its warning
// event. It is computed once and handed to everything that renders the feature,
// so the storage builder, the mount, the env, the prep container and the
// security context cannot disagree.
type runtimeCachePlan struct {
	dirs    []RuntimeCacheDir
	reason  string
	message string
}

// applied reports whether the serving Pod gets the mount, the env and the prep.
func (p runtimeCachePlan) applied() bool { return len(p.dirs) > 0 }

// runtimeCachePlanFor decides the feature for one render, reading the claim it
// would write once. The read happens only for a request that would otherwise be
// applied, so a service without the field costs no API call.
func (r *InferenceServiceReconciler) runtimeCachePlanFor(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	cache effectiveModelCache,
	backend RuntimeBackend,
) runtimeCachePlan {
	plan := r.runtimeCachePlan(isvc, model, cache, backend)
	if !plan.applied() {
		return plan
	}
	if modes, readOnly := r.runtimeCacheClaimAccessModes(ctx, isvc, cache); readOnly {
		return runtimeCachePlan{
			reason: ReasonRuntimeCacheClaimReadOnly,
			message: fmt.Sprintf(
				"spec.modelCache.runtimeCache.enabled is ignored: cache claim %q offers no writable access mode (%s), so the runtime's cache directory could not be written on it",
				cache.pvcName(r.ModelCacheMode), strings.Join(modes, ", ")),
		}
	}
	return plan
}

// runtimeCacheClaimAccessModes reads the claim the runtime cache would write and
// reports its access modes and whether none of them allows a write. An
// unreadable claim (no client, or a read error) reports no: a missing claim
// already fails the reconcile earlier, and a transient error must not decline a
// valid spec.
func (r *InferenceServiceReconciler) runtimeCacheClaimAccessModes(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	cache effectiveModelCache,
) (modes []string, readOnly bool) {
	if r.Client == nil {
		return nil, false
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: cache.pvcName(r.ModelCacheMode), Namespace: isvc.Namespace}, pvc); err != nil {
		return nil, false
	}
	writable := false
	modes = make([]string, 0, len(pvc.Spec.AccessModes))
	for _, m := range pvc.Spec.AccessModes {
		modes = append(modes, string(m))
		switch m {
		case corev1.ReadWriteOnce, corev1.ReadWriteOncePod, corev1.ReadWriteMany:
			writable = true
		}
	}
	return modes, len(modes) > 0 && !writable
}

// runtimeCachePlan decides the feature without reading the claim. The request is
// the effective modelCache's, so a Model-level block reaches every service that
// references it.
func (r *InferenceServiceReconciler) runtimeCachePlan(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	cache effectiveModelCache,
	backend RuntimeBackend,
) runtimeCachePlan {
	if !cache.runtimeCacheEnabled() {
		return runtimeCachePlan{}
	}
	if isMetalModel(model) {
		return runtimeCachePlan{reason: ReasonRuntimeCacheUnsupported, message: fmt.Sprintf(
			"spec.modelCache.runtimeCache.enabled is ignored: Model %q runs on the metal-agent, which starts no serving container to keep a cache from",
			model.Name)}
	}
	dirs := runtimeCacheDirsFor(backend)
	if len(dirs) == 0 {
		return runtimeCachePlan{reason: ReasonRuntimeCacheUnsupported, message: fmt.Sprintf(
			"spec.modelCache.runtimeCache.enabled is ignored: runtime %q keeps no compile or autotune cache; only vllm and sglang do, and llama.cpp needs none",
			runtimeNameLabel(isvc))}
	}
	if !cachePrepIncluded(r.ModelCachePrep) {
		return runtimeCachePlan{reason: ReasonRuntimeCacheNeedsPrepare, message: "spec.modelCache.runtimeCache.enabled is ignored: --model-cache-prep=never omits the init container that creates the per-service cache directory, and the serving container cannot write a directory kubelet created as root"}
	}
	// A user mount at the operator's own path owns the feature, as with
	// /dev/shm (#2111). Nothing is injected and nothing is declined.
	if userMountsAt(isvc.Spec.ExtraVolumeMounts, runtimeCacheMountPath) {
		return runtimeCachePlan{}
	}
	switch {
	case !operatorStagesModel(isvc, backend):
		return runtimeCachePlan{reason: ReasonRuntimeCacheUnsupported, message: "spec.modelCache.runtimeCache.enabled is ignored: spec.skipModelInit stages no model, so the Pod mounts no cache claim to keep the runtime's caches on"}
	case isPVCSource(model.Spec.Source), isOCISource(model.Spec.Source):
		return runtimeCachePlan{reason: ReasonRuntimeCacheUnsupported, message: fmt.Sprintf(
			"spec.modelCache.runtimeCache.enabled is ignored: model source %q is pre-staged read-only, so no cache claim is mounted",
			model.Spec.Source)}
	case r.ModelCachePath == "":
		return runtimeCachePlan{reason: ReasonRuntimeCacheUnsupported, message: "spec.modelCache.runtimeCache.enabled is ignored: model caching is disabled on the operator (--model-cache-path unset / chart modelCache.enabled=false), so the Pod mounts an ephemeral emptyDir"}
	case cache.isEphemeral():
		return runtimeCachePlan{reason: ReasonRuntimeCacheUnsupported, message: "spec.modelCache.runtimeCache.enabled is ignored: persistence is Ephemeral, so the Pod mounts an emptyDir that dies with it"}
	case effectiveModelCacheKey(model) == "":
		if model.Status.Phase != PhaseReady {
			// The key can still arrive while the Model is not Ready; a Ready
			// Model with no key never gets one and is declined below.
			return runtimeCachePlan{}
		}
		return runtimeCachePlan{reason: ReasonRuntimeCacheUnsupported, message: fmt.Sprintf(
			"spec.modelCache.runtimeCache.enabled is ignored: model %q has no cache key (a source the runtime resolves itself, or a local source), so the Pod mounts no cache claim and the runtime's caches stay in the container's writable layer, which dies with the Pod",
			model.Name)}
	}
	// A user volume takes a cache over only when it is mounted at that cache's
	// own default directory. A mount above it (an emptyDir at /tmp for a
	// read-only root filesystem, or one at /root/.cache) leaves the cache where
	// the runtime writes it. When every cache is taken over the plan keeps no
	// directory, so nothing is injected.
	var kept []RuntimeCacheDir
	for _, d := range dirs {
		if d.Default != "" && userMountsAt(isvc.Spec.ExtraVolumeMounts, d.Default) {
			continue
		}
		kept = append(kept, d)
	}
	if len(kept) == 0 {
		return runtimeCachePlan{}
	}
	// A prefetched claim is written by the Job in the operator's group, so a
	// service whose own fsGroup differs would make kubelet walk and re-own
	// every staged file on each pod start, because the runtime cache mounts
	// that claim writable. With the operator's own default disabled there is no
	// group the prefetch writes in, and the group-0 prep's chmod 770 clears the
	// setgid bit that skip requires, so any fsGroup of the service's own is
	// declined too.
	if g := r.prefetchGate(isvc, model, cache); g.applies && g.satisfied {
		if psc := isvc.Spec.PodSecurityContext; psc != nil && psc.FSGroup != nil &&
			(r.DefaultFSGroup <= 0 || *psc.FSGroup != r.DefaultFSGroup) {
			return runtimeCachePlan{reason: ReasonRuntimeCacheFSGroupMismatch, message: runtimeCacheFSGroupMismatchMessage(*psc.FSGroup, r.DefaultFSGroup)}
		}
	}
	return runtimeCachePlan{dirs: kept}
}

// runtimeCacheFSGroupMismatchMessage explains the prefetched-fsGroup decline.
// With the operator's fsGroup disabled there is no group to set the service's
// own to, so that message offers removal only.
func runtimeCacheFSGroupMismatchMessage(fsGroup, defaultFSGroup int64) string {
	if defaultFSGroup <= 0 {
		return fmt.Sprintf(
			"spec.modelCache.runtimeCache.enabled is ignored: podSecurityContext.fsGroup %d is set while the operator sets no fsGroup of its own (--default-fsgroup %d), and the runtime cache mounts the prefetched claim writable, so kubelet would re-own every staged file on each pod start; remove podSecurityContext.fsGroup",
			fsGroup, defaultFSGroup)
	}
	return fmt.Sprintf(
		"spec.modelCache.runtimeCache.enabled is ignored: podSecurityContext.fsGroup %d differs from the group the prefetch writes the cache claim in (--default-fsgroup %d), and the runtime cache mounts that claim writable, so kubelet would re-own every staged file on each pod start; remove podSecurityContext.fsGroup or set it to %d",
		fsGroup, defaultFSGroup, defaultFSGroup)
}

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

// runtimeCacheMount returns the mount that puts the per-service directory at
// runtimeCacheMountPath. It reuses the weights volume through a subPath, so the
// pod never sees a sibling service's cache directory.
func runtimeCacheMount(isvc *inferencev1alpha1.InferenceService, volume string) *corev1.VolumeMount {
	return &corev1.VolumeMount{
		Name:      volume,
		MountPath: runtimeCacheMountPath,
		SubPath:   runtimeCacheClaimDir(isvc),
	}
}

// runtimeCacheEnvVars points each cache the plan keeps at the per-service directory.
func runtimeCacheEnvVars(dirs []RuntimeCacheDir) []corev1.EnvVar {
	env := make([]corev1.EnvVar, 0, len(dirs))
	for _, d := range dirs {
		value := runtimeCacheMountPath
		if d.Value != "" {
			value = path.Join(runtimeCacheMountPath, d.Value)
		}
		env = append(env, corev1.EnvVar{Name: d.Env, Value: value})
	}
	return env
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
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext:          runtimeCachePrepSecurityContext(),
	}
}

// warnRuntimeCacheNotApplied emits the warning a service gets when it asks for
// a persistent runtime cache it cannot have. Only a service that sets the field
// itself is warned: a Model-level block reaches every service that references
// the Model, and warning each of those about a block nobody wrote on the
// service is noise.
func (r *InferenceServiceReconciler) warnRuntimeCacheNotApplied(
	isvc *inferencev1alpha1.InferenceService,
	plan runtimeCachePlan,
) {
	if r.Recorder == nil {
		return
	}
	if isvc.Spec.ModelCache == nil || isvc.Spec.ModelCache.RuntimeCache == nil {
		return
	}
	if plan.applied() || plan.reason == "" {
		return
	}
	r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, plan.reason, "Reconcile", "%s", plan.message)
}
