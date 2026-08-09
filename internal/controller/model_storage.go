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
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
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

// userModelCacheClaimName returns the user-supplied cache PVC name from
// spec.modelCache.claimName, or "" when the InferenceService does not override
// the operator-global cache mode.
func userModelCacheClaimName(isvc *inferencev1alpha1.InferenceService) string {
	if isvc == nil || isvc.Spec.ModelCache == nil {
		return ""
	}
	return isvc.Spec.ModelCache.ClaimName
}

// warnIgnoredModelCacheClaim emits a ModelCacheClaimIgnored warning event when
// spec.modelCache.claimName is set but has no effect. The field targets the
// download-into-cache path, so it is meaningless whenever that path is
// inactive; warn in each such case instead of silently dropping the field:
//   - pvc:// sources are pre-staged (mounted read-only, no download);
//   - with caching disabled on the operator, or a model without an effective
//     cache key (local file:// source, or a remote model whose fingerprint has
//     not landed in Status.CacheKey yet), the pod falls back to an ephemeral
//     emptyDir and re-downloads on every restart (this mirrors the useCache
//     gate in constructDeployment).
func (r *InferenceServiceReconciler) warnIgnoredModelCacheClaim(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
) {
	if r.Recorder == nil || userModelCacheClaimName(isvc) == "" {
		return
	}
	switch {
	case isPVCSource(model.Spec.Source):
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, "ModelCacheClaimIgnored", "Reconcile",
			"spec.modelCache.claimName is ignored: model source %q is a pre-staged pvc:// volume (read-only, no download)",
			model.Spec.Source)
	case r.ModelCachePath == "":
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, "ModelCacheClaimIgnored", "Reconcile",
			"spec.modelCache.claimName is ignored: model caching is disabled on the operator "+
				"(--model-cache-path unset / chart modelCache.enabled=false); "+
				"the model downloads into an ephemeral emptyDir and re-downloads on every pod restart")
	case effectiveModelCacheKey(model) == "":
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, "ModelCacheClaimIgnored", "Reconcile",
			"spec.modelCache.claimName is ignored: model %q has no cache key "+
				"(local source, or fingerprinting has not completed yet); "+
				"the model downloads into an ephemeral emptyDir and re-downloads on every pod restart",
			model.Name)
	}
}

// warnUnboundedEphemeralCache emits a warning when a service opts out of the
// cache but declares no ephemeral-storage budget, leaving the emptyDir the
// weights land in unbounded.
//
// Not a hard failure: refusing to reconcile would take down a workload that
// runs perfectly well on a node with room, and how much room a node has varies
// by an order of magnitude across distributions and machine types. But it is
// silent otherwise, and it is only visible once a node fills and the kubelet
// starts evicting by QoS class, potentially removing unrelated workloads first.
func (r *InferenceServiceReconciler) warnUnboundedEphemeralCache(
	isvc *inferencev1alpha1.InferenceService,
) {
	if r.Recorder == nil || !modelCacheIsEphemeral(isvc) {
		return
	}
	if isvc.Spec.Resources != nil && isvc.Spec.Resources.EphemeralStorage != "" {
		return
	}
	r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, "UnboundedEphemeralCache", "Reconcile",
		"modelCache.persistence is Ephemeral but spec.resources.ephemeralStorage is unset: "+
			"model weights download to node local disk with no size limit and no scheduler "+
			"accounting. Set spec.resources.ephemeralStorage above the model size so an "+
			"overrun evicts this pod rather than filling the node")
}

// modelNeedsCachePVC reports whether the operator should provision a model
// cache PVC for this reconcile. Caching must be enabled on the operator
// (modelCachePath set) and the model must have a cache key. It must NOT be a
// pvc:// source: those are pre-staged and mounted read-only
// (buildModelStorageConfig dispatches to buildPVCStorageConfig, never the
// cache), so provisioning a cache PVC for them only leaves an unused,
// ISVC-owned claim. Kept as its own predicate so the mount side (isPVCSource
// in buildModelStorageConfig) and the provisioning side agree on pvc://.
func modelNeedsCachePVC(
	model *inferencev1alpha1.Model,
	isvc *inferencev1alpha1.InferenceService,
	modelCachePath string,
) bool {
	return modelWantsCacheVolume(model, isvc, modelCachePath) &&
		!isPVCSource(model.Spec.Source)
}

// modelWantsCacheVolume reports whether this workload should download into the
// model cache volume rather than an ephemeral emptyDir. It is the MOUNT-side
// question, and modelNeedsCachePVC (the provisioning side) is defined in terms
// of it so the two cannot drift: a mount without a claim leaves the Pod Pending
// on a volume nobody creates, and a claim without a mount leaves an orphaned,
// ISVC-owned PVC behind.
//
// The pvc:// exclusion lives only on the provisioning side because those
// sources are dispatched to buildPVCStorageConfig, which never consults this.
func modelWantsCacheVolume(
	model *inferencev1alpha1.Model,
	isvc *inferencev1alpha1.InferenceService,
	modelCachePath string,
) bool {
	return modelCachePath != "" &&
		effectiveModelCacheKey(model) != "" &&
		!modelCacheIsEphemeral(isvc)
}

// ephemeralCacheSizeLimit returns the sizeLimit to put on the emptyDir that
// backs an ephemeral model cache, or nil to leave it unbounded.
//
// Bounded is strongly preferred. An emptyDir with no sizeLimit is written to
// the node's ephemeral storage, where the kubelet will not charge it to this
// Pod alone: a multi-gigabyte model on a node with a small boot disk crosses
// the node's DiskPressure threshold, and eviction then proceeds by QoS class,
// which can remove unrelated workloads before this one. With a sizeLimit the
// kubelet evicts the Pod that actually overran.
//
// nil when the user declared no budget. Guessing is worse than not bounding:
// the model's size on disk is not reliably known (Status.Size comes from the
// GGUF metadata range read, which only runs for remote http(s) sources, so it
// is absent for s3:// among others), and a guess that lands under the real size
// evicts the Pod for exceeding a number nobody chose.
func ephemeralCacheSizeLimit(isvc *inferencev1alpha1.InferenceService) *resource.Quantity {
	if !modelCacheIsEphemeral(isvc) || isvc.Spec.Resources == nil {
		return nil
	}
	if isvc.Spec.Resources.EphemeralStorage == "" {
		return nil
	}
	q, err := resource.ParseQuantity(isvc.Spec.Resources.EphemeralStorage)
	if err != nil {
		// A CRD pattern rejects malformed values at admission, so this is not
		// reachable through the API. Handled rather than asserted anyway: an
		// unbounded emptyDir still serves, while panicking would take the
		// controller down for every workload.
		return nil
	}
	return &q
}

// modelCacheIsEphemeral reports whether the InferenceService declined the cache
// (#1451). Only an explicit Ephemeral opts out: a nil isvc, an absent
// modelCache block, and an empty persistence all resolve to Cached, so the
// default behaviour is unchanged.
func modelCacheIsEphemeral(isvc *inferencev1alpha1.InferenceService) bool {
	return isvc != nil && isvc.Spec.ModelCache != nil &&
		isvc.Spec.ModelCache.Persistence == inferencev1alpha1.ModelCachePersistenceEphemeral
}

// modelCachePVCName returns the name of the model cache PVC for the given mode.
// A per-InferenceService spec.modelCache.claimName override (#928) wins over
// the operator-global mode: that user-owned PVC becomes the cache volume for
// this workload only. Otherwise, in shared mode (the default, and the
// resolution of an empty mode) this is the single cluster-wide PVC; in
// perService mode it is the per-InferenceService PVC "<isvc>-model-cache". A
// nil isvc (unit tests that exercise the builder directly) falls back to the
// shared name.
func modelCachePVCName(isvc *inferencev1alpha1.InferenceService, mode string) string {
	if claim := userModelCacheClaimName(isvc); claim != "" {
		return claim
	}
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
// given slices, and prefixes the command with a CURL_CA_BUNDLE export.
// No-op when caCertConfigMap is empty.
//
// The bundle is ADDITIVE: the system trust store is concatenated with the
// custom CA into a scratch file, and CURL_CA_BUNDLE points at that. Setting
// CURL_CA_BUNDLE to the custom cert alone REPLACES the system roots, so
// enabling this flag for one private endpoint silently breaks every public
// source on the fleet. Observed 2026-08-09: a Hugging Face pull failed with
// "unable to get local issuer certificate" while private-endpoint downloads
// kept working, which is what made it hard to spot.
//
// Written to /tmp rather than over /etc/ssl/certs: the init container may run
// read-only or as a non-root user, and a download command should not mutate
// the image's trust store. The system bundle is globbed rather than assumed,
// so an image that keeps its roots elsewhere still gets the custom CA.
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
	// cat both into a scratch bundle; the || keeps a missing system store from
	// failing the download, since the custom CA alone is still better than none.
	const caPrelude = "export LLMKUBE_CA_BUNDLE=/tmp/llmkube-ca-bundle.crt && " +
		"cat /etc/ssl/certs/ca-certificates.crt /custom-certs/* > \"$LLMKUBE_CA_BUNDLE\" 2>/dev/null || " +
		"cat /custom-certs/* > \"$LLMKUBE_CA_BUNDLE\" && " +
		"export CURL_CA_BUNDLE=\"$LLMKUBE_CA_BUNDLE\" && "
	*cmd = caPrelude + *cmd
}

// All transfers write to "$MODEL_PATH.tmp" and mv onto "$MODEL_PATH" on
// success: the guard is a bare existence check, so publishing the file
// non-atomically would let an interrupted transfer (OOM-kill, eviction,
// node reboot) leave a truncated artifact that every subsequent restart
// treats as cached. See remoteRevalidateScript for the same pattern.
func buildModelInitCommand(isLocal, isS3, useCache bool, refreshPolicy string) string {
	if useCache {
		if isLocal {
			return `mkdir -p "$CACHE_DIR" && rm -f "$MODEL_PATH.tmp" && if [ ! -f "$MODEL_PATH" ]; then echo 'Copying model from local source...'; cp /host-model/model.gguf "$MODEL_PATH.tmp" && mv "$MODEL_PATH.tmp" "$MODEL_PATH" && echo 'Model copied successfully'; else echo 'Model already cached, skipping copy'; fi`
		}
		if isS3 {
			return `mkdir -p "$CACHE_DIR" && rm -f "$MODEL_PATH.tmp" && if [ ! -f "$MODEL_PATH" ]; then echo 'Downloading model from S3...'; curl --aws-sigv4 "aws:amz:${AWS_REGION}:s3" -u "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" -f -L -o "$MODEL_PATH.tmp" "${AWS_ENDPOINT_URL}/${S3_BUCKET}/${S3_KEY}" && mv "$MODEL_PATH.tmp" "$MODEL_PATH" && echo 'Model downloaded successfully'; else echo 'Model already cached, skipping download'; fi`
		}
		if refreshPolicy == RefreshPolicyOnChange {
			return "mkdir -p \"$CACHE_DIR\" && rm -f \"$MODEL_PATH.tmp\" && " + remoteRevalidateScript
		}
		return `mkdir -p "$CACHE_DIR" && rm -f "$MODEL_PATH.tmp" && if [ ! -f "$MODEL_PATH" ]; then echo 'Downloading model...'; curl -f -L -o "$MODEL_PATH.tmp" "$MODEL_SOURCE" && mv "$MODEL_PATH.tmp" "$MODEL_PATH" && echo 'Model downloaded successfully'; else echo 'Model already cached, skipping download'; fi`
	}

	if isLocal {
		return `echo 'ERROR: Local model source requires model cache to be configured.'; exit 1`
	}
	if isS3 {
		return `if [ ! -f "$MODEL_PATH" ]; then echo 'Downloading model from S3...'; curl --aws-sigv4 "aws:amz:${AWS_REGION}:s3" -u "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" -f -L -o "$MODEL_PATH.tmp" "${AWS_ENDPOINT_URL}/${S3_BUCKET}/${S3_KEY}" && mv "$MODEL_PATH.tmp" "$MODEL_PATH" && echo 'Model downloaded successfully'; else echo 'Model already exists, skipping download'; fi`
	}
	if refreshPolicy == RefreshPolicyOnChange {
		return remoteRevalidateScript
	}
	return `if [ ! -f "$MODEL_PATH" ]; then echo 'Downloading model...'; curl -f -L -o "$MODEL_PATH.tmp" "$MODEL_SOURCE" && mv "$MODEL_PATH.tmp" "$MODEL_PATH" && echo 'Model downloaded successfully'; else echo 'Model already exists, skipping download'; fi`
}

// remoteRevalidateScript implements RefreshPolicy=OnChange for http/https
// sources fetched by the init container. It revalidates the cached artifact
// before transferring, so a warm cache skips the download entirely.
//
// Why not curl --etag-compare/--etag-save: HuggingFace serves LFS-backed
// weights via a 302 redirect to a signed CDN URL on a different host, and the
// If-None-Match conditional header is not forwarded across that redirect hop,
// so the CDN always streams the full body. A 304 is therefore unreachable for
// any LFS artifact, and --etag-compare can only ever short-circuit the small
// origin-served config files. On top of that, `curl -o "$dest"` truncates the
// cached file at request start, so a failed or full-200 transfer destroys the
// good cache before any decision is made.
//
// Strategy (works for both origin and CDN-served files):
//  1. HEAD the artifact and read Content-Length. This MUST use curl's
//     -w '%header{content-length}' (curl 7.84+, satisfied by curlimages/curl):
//     a HEAD has no response body, so -w '%{size_download}' would always report
//     0 and the skip branch below could never fire. -L makes %header report the
//     final CDN response's Content-Length across HuggingFace's 302 redirect.
//     If the header is absent or the HEAD is rejected, remote_size is empty/0
//     and the script downloads (size-equality is a truncation guard, not an
//     integrity check).
//  2. If the local file exists and its size matches Content-Length, the cache
//     is current: log "revalidated" and skip the transfer.
//  3. Otherwise download to "$dest.tmp" and `mv` it onto "$dest" on success,
//     so a redundant or failed transfer can never truncate a good cache. The
//     rename also makes the artifact publish atomically.
//
// Robustness: the init container gates pod startup, so a transient network
// failure (air-gapped, upstream 5xx, DNS) must not take down an
// InferenceService on pod restart. If revalidation fails but a cached copy
// already exists, the script logs and exits 0, keeping the cached file. Only a
// genuinely-missing file (nothing cached and the fetch failed) fails the init
// container.
const remoteRevalidateScript = `echo 'Revalidating model against upstream (RefreshPolicy=OnChange)...'; ` +
	`remote_size=$(curl -fsSL -I "$MODEL_SOURCE" -o /dev/null -w '%header{content-length}' 2>/dev/null || echo 0); ` +
	`if [ -f "$MODEL_PATH" ] && [ "$(stat -c %s "$MODEL_PATH" 2>/dev/null || echo 0)" = "$remote_size" ] && [ "$remote_size" != "0" ]; then ` +
	`echo 'Model revalidated (unchanged, skipped download)'; ` +
	`else ` +
	`if curl -fsSL -o "$MODEL_PATH.tmp" "$MODEL_SOURCE" && mv "$MODEL_PATH.tmp" "$MODEL_PATH"; then ` +
	`echo 'Model revalidated (downloaded)'; ` +
	`elif [ -f "$MODEL_PATH" ]; then echo 'Revalidation unreachable; kept cached copy'; exit 0; ` +
	`else echo 'ERROR: model missing and revalidation failed'; exit 1; fi; ` +
	`fi`

func modelInitEnvVars(source, cacheDir, modelPath string) []corev1.EnvVar {
	envs := []corev1.EnvVar{
		{Name: "MODEL_SOURCE", Value: source},
		{Name: "CACHE_DIR", Value: cacheDir},
		{Name: "MODEL_PATH", Value: modelPath},
	}
	if isS3Source(source) {
		bucket, key, err := parseS3Source(source)
		if err == nil {
			envs = append(envs, corev1.EnvVar{Name: "S3_BUCKET", Value: bucket}, corev1.EnvVar{Name: "S3_KEY", Value: key})
		}
	}
	return envs
}

// modelEnvFrom returns EnvFrom entries for the model-downloader init container.
// When the model has a SourceSecretRef, it pulls AWS_* env vars (credentials,
// endpoint, region) from the referenced Secret. Returns nil when no secret ref
// is configured.
func modelEnvFrom(model *inferencev1alpha1.Model) []corev1.EnvFromSource {
	if model.Spec.SourceSecretRef == nil {
		return nil
	}
	return []corev1.EnvFromSource{
		{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: *model.Spec.SourceSecretRef,
			},
		},
	}
}

// resolveHFSourceURL converts hf://repo-id sources to their huggingface.co
// HTTPS equivalent for init container env vars. Non-hf:// sources pass through unchanged.
// This is now a thin wrapper around normalizeHFSource for backward compatibility.
func resolveHFSourceURL(source string) string {
	return normalizeHFSource(source)
}

// hasMultiFileStaging reports whether the model uses multi-file staging via
// spec.files or spec.mmproj.
func hasMultiFileStaging(model *inferencev1alpha1.Model) bool {
	return model != nil && (len(model.Spec.Files) > 0 || model.Spec.Mmproj != "")
}

// effectiveModelCacheKey returns the key used to namespace a model's
// files inside the cache PVC, and is the single source of truth for
// whether the in-cluster serving pod should use the cache (non-empty)
// or an emptyDir (empty). It delegates to cachekey.EffectiveKey so the
// controller and the CLI can never disagree about caching.
func effectiveModelCacheKey(model *inferencev1alpha1.Model) string {
	return cachekey.EffectiveKey(model)
}

// modelStagingPlan resolves the model's declared files into a staging plan.
// Returns nil when there is no multi-file staging. Returns an error when
// multi-file fields are set but resolution fails (fail-closed).
func modelStagingPlan(model *inferencev1alpha1.Model) (*StagingPlan, error) {
	if !hasMultiFileStaging(model) {
		return nil, nil
	}
	plan, err := ResolveFileSet(model.Spec.Files, model.Spec.Mmproj, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid fileset: %w", err)
	}
	if plan == nil || plan.Primary == "" {
		return nil, nil
	}
	return plan, nil
}

// invalidFileSetInitContainer returns an init container that immediately exits
// with a clear error message when multi-file staging is requested but
// ResolveFileSet fails. This prevents silent fallback to legacy single-file
// mode when the user's config is wrong.
func invalidFileSetInitContainer(initImage string) corev1.Container {
	return corev1.Container{
		Name:    "model-downloader",
		Image:   initImage,
		Command: []string{"sh", "-c", `echo "ERROR: InvalidFileSet - model spec.files/spec.mmproj configuration is invalid. Check file paths, directory escapes, and glob patterns."; exit 1`},
	}
}

// cachePrepInitContainer returns the root-run prep init container that runs
// BEFORE model-downloader in the cache-backed path. CSI drivers with
// fsGroupPolicy=None (CephFS, NFS) never apply the pod fsGroup to the volume,
// so the PVC root stays root:root 0755 and the non-root downloader (uid 100)
// gets "permission denied" on mkdir. The prep chowns/chmods the mount root so
// the downloader can write, without recursing over /models.
//
// When resolvedFSGroup > 0 the prep chowns to 0:<fsGroup> and grants group
// rw on existing and future files/dirs (g+rwX). When resolvedFSGroup <= 0
// (fsGroup disabled, e.g. OpenShift) the prep chowns to 100:100 (the
// downloader's UID/GID) and sets 770 so only the downloader can write.
//
// Security: the prep runs as root (uid 0) with ALL capabilities dropped and
// only CHOWN+FOWNER added, and is NOT privileged. Root is required, not
// optional: the container runs `sh -c "chown ... && chmod ..."`, and in
// containerd a non-root process clears its capabilities across execve (there
// are no ambient capabilities in the Kubernetes securityContext), so the
// `chown` the shell execs would run with an empty effective set and fail with
// EPERM. Root retains its capabilities across exec, so chown/chmod succeed.
// Running this init non-root broke model-cache-prep on fsGroupPolicy=None CSIs
// in 0.8.20; see the regression note below. It still cannot satisfy PSA
// "restricted" (which forbids adding any capability except NET_BIND_SERVICE,
// and the chown of an unowned mount fundamentally needs CHOWN); see
// docs/MODEL-CACHE.md "Security Considerations" for the restricted-PSA
// alternatives (fsGroupPolicy=File CSI, emptyDir store, or a laxer policy).
//
// The prep reuses the configurable initContainerImage (no hardcoded busybox)
// so air-gapped clusters that mirror initContainerImage are covered.
func cachePrepInitContainer(initImage string, resolvedFSGroup int64) corev1.Container {
	var cmd string
	if resolvedFSGroup > 0 {
		cmd = fmt.Sprintf("chown 0:%d /models && chmod g+rwX /models", resolvedFSGroup)
	} else {
		cmd = "chown 100:100 /models && chmod 770 /models"
	}
	return corev1.Container{
		Name:    "model-cache-prep",
		Image:   initImage,
		Command: []string{"sh", "-c", cmd},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "model-cache", MountPath: "/models"},
		},
		SecurityContext: &corev1.SecurityContext{
			// Root (uid 0), required for the exec'd chown to keep CAP_CHOWN.
			// Non-root + capabilities.add does NOT work here: containerd does
			// not set ambient caps, so caps are cleared when sh execs chown
			// (EPERM). Not privileged: ALL caps dropped, only CHOWN+FOWNER
			// added, no privilege escalation. See the doc comment above.
			RunAsUser:                int64Ptr(0),
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"CHOWN", "FOWNER"},
			},
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
	}
}

// multiFileInitEnvVars returns env vars for a multi-file init container.
// MODEL_SOURCE is normalized (hf:// -> https://huggingface.co/), and
// MODEL_FILES is newline-delimited. For s3:// sources, S3_BUCKET and
// S3_PREFIX are emitted (the key prefix from the s3:// source); per-file
// object keys are constructed as "${S3_PREFIX}/$rel" in the shell command.
func multiFileInitEnvVars(source, cacheDir string, files []string) []corev1.EnvVar {
	normalized := resolveHFSourceURL(source)
	envs := []corev1.EnvVar{
		{Name: "MODEL_SOURCE", Value: normalized},
		{Name: "CACHE_DIR", Value: cacheDir},
		{Name: "MODEL_FILES", Value: strings.Join(files, "\n")},
	}
	if isS3Source(source) {
		bucket, key, err := parseS3Source(source)
		if err == nil {
			envs = append(envs, corev1.EnvVar{Name: "S3_BUCKET", Value: bucket}, corev1.EnvVar{Name: "S3_PREFIX", Value: key})
		}
	}
	return envs
}

// buildMultiFileInitCommand returns a shell command that downloads each file
// listed in $MODEL_FILES from the normalized $MODEL_SOURCE. For cached storage
// (useCache=true), it creates $CACHE_DIR first. For emptyDir (useCache=false),
// it creates /models. The command uses env vars only, never embedding user
// values directly in the script. When isS3 is true, the curl command is signed
// with --aws-sigv4 and uses ${AWS_ENDPOINT_URL}/${S3_BUCKET}/${S3_PREFIX}/$rel
// as the per-file URL (S3_PREFIX may be empty for bare-bucket sources).
func buildMultiFileInitCommand(useCache, isS3 bool, refreshPolicy string) string {
	prefix := `mkdir -p "$CACHE_DIR" && find "$CACHE_DIR" -name '*.tmp' -delete && `
	if !useCache {
		prefix = `mkdir -p /models && find /models -name '*.tmp' -delete && `
	}

	normalizeFn := `normalize_hf_source() { case "$1" in hf://*) src="${1#hf://}"; rev="${src#*@}"; if [ "$rev" != "$src" ]; then echo "https://huggingface.co/${src%%@*}/resolve/$rev/"; else echo "https://huggingface.co/$src/resolve/main/"; fi ;; *) echo "$1" ;; esac; }` + " && "

	if isS3 {
		if refreshPolicy == RefreshPolicyOnChange {
			body := normalizeFn +
				`SOURCE="$(normalize_hf_source "$MODEL_SOURCE")" && ` +
				`printf '%s\n' "$MODEL_FILES" | while IFS= read -r rel; do ` +
				`[ -n "$rel" ] || continue; ` +
				`dest="$CACHE_DIR/$rel"; ` +
				`mkdir -p "$(dirname "$dest")"; ` +
				`key="${S3_PREFIX:+${S3_PREFIX}/}$rel"; ` +
				`url="${AWS_ENDPOINT_URL}/${S3_BUCKET}/${key}"; ` +
				`remote_size=$(curl --aws-sigv4 "aws:amz:${AWS_REGION}:s3" -u "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" -fsSL -I "$url" -o /dev/null -w '%header{content-length}' 2>/dev/null || echo 0); ` +
				`if [ -f "$dest" ] && [ "$(stat -c %s "$dest" 2>/dev/null || echo 0)" = "$remote_size" ] && [ "$remote_size" != "0" ]; then ` +
				`echo "Model artifact $rel revalidated (unchanged, skipped download)"; ` +
				`else ` +
				`if curl --aws-sigv4 "aws:amz:${AWS_REGION}:s3" -u "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" -f -L -o "$dest.tmp" "$url" && mv "$dest.tmp" "$dest"; then ` +
				`echo "Model artifact $rel revalidated (downloaded)"; ` +
				`elif [ -f "$dest" ]; then echo "Revalidation unreachable for $rel; kept cached copy"; ` +
				`else echo "ERROR: model artifact $rel missing and revalidation failed"; exit 1; fi; ` +
				`fi; ` +
				`done`
			return prefix + body
		}

		body := normalizeFn +
			`SOURCE="$(normalize_hf_source "$MODEL_SOURCE")" && ` +
			`printf '%s\n' "$MODEL_FILES" | while IFS= read -r rel; do ` +
			`[ -n "$rel" ] || continue; ` +
			`dest="$CACHE_DIR/$rel"; ` +
			`mkdir -p "$(dirname "$dest")"; ` +
			`key="${S3_PREFIX:+${S3_PREFIX}/}$rel"; ` +
			`url="${AWS_ENDPOINT_URL}/${S3_BUCKET}/${key}"; ` +
			`if [ ! -f "$dest" ]; then ` +
			`echo "Downloading model artifact $rel..."; ` +
			`curl --aws-sigv4 "aws:amz:${AWS_REGION}:s3" -u "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" -f -L -o "$dest.tmp" "$url" && mv "$dest.tmp" "$dest" || { echo "ERROR: failed to download $rel"; exit 1; }; ` +
			`else echo "Model artifact $rel already cached, skipping download"; fi; ` +
			`done`
		return prefix + body
	}

	if refreshPolicy == RefreshPolicyOnChange {
		body := normalizeFn +
			`SOURCE="$(normalize_hf_source "$MODEL_SOURCE")" && ` +
			`printf '%s\n' "$MODEL_FILES" | while IFS= read -r rel; do ` +
			`[ -n "$rel" ] || continue; ` +
			`dest="$CACHE_DIR/$rel"; ` +
			`mkdir -p "$(dirname "$dest")"; ` +
			`url="${SOURCE%/}/$rel"; ` +
			`remote_size=$(curl -fsSL -I "$url" -o /dev/null -w '%header{content-length}' 2>/dev/null || echo 0); ` +
			`if [ -f "$dest" ] && [ "$(stat -c %s "$dest" 2>/dev/null || echo 0)" = "$remote_size" ] && [ "$remote_size" != "0" ]; then ` +
			`echo "Model artifact $rel revalidated (unchanged, skipped download)"; ` +
			`else ` +
			`if curl -fsSL -o "$dest.tmp" "$url" && mv "$dest.tmp" "$dest"; then ` +
			`echo "Model artifact $rel revalidated (downloaded)"; ` +
			`elif [ -f "$dest" ]; then echo "Revalidation unreachable for $rel; kept cached copy"; ` +
			`else echo "ERROR: model artifact $rel missing and revalidation failed"; exit 1; fi; ` +
			`fi; ` +
			`done`
		return prefix + body
	}

	body := normalizeFn +
		`SOURCE="$(normalize_hf_source "$MODEL_SOURCE")" && ` +
		`printf '%s\n' "$MODEL_FILES" | while IFS= read -r rel; do ` +
		`[ -n "$rel" ] || continue; ` +
		`dest="$CACHE_DIR/$rel"; ` +
		`mkdir -p "$(dirname "$dest")"; ` +
		`url="${SOURCE%/}/$rel"; ` +
		`if [ ! -f "$dest" ]; then ` +
		`echo "Downloading model artifact $rel..."; ` +
		`curl -f -L -o "$dest.tmp" "$url" && mv "$dest.tmp" "$dest" || { echo "ERROR: failed to download $rel"; exit 1; }; ` +
		`else echo "Model artifact $rel already cached, skipping download"; fi; ` +
		`done`
	return prefix + body
}

type modelStorageConfig struct {
	modelPath      string
	stagedDir      string // staged model directory for multi-file staging; empty for single-file/GGUF
	initContainers []corev1.Container
	volumes        []corev1.Volume
	volumeMounts   []corev1.VolumeMount
}

// applyInitContainerDiagnostics makes every generated init container report its
// own fatal error, mirroring what #1425 did for the runtime container.
//
// On a fresh deploy the downloader is the container most likely to fail (bad
// credentials, a 404, a full disk, eviction for exceeding an emptyDir
// sizeLimit) and it wrote nothing to the termination message, so the failure
// surfaced only as a non-zero exit code. FallbackToLogsOnError makes the log
// tail the termination message on failure.
//
// Applying it to every container the operator generates also removes the
// asymmetry that forced normalizeContainers to strip the field: when the
// operator sets it everywhere, desired and live agree and there is nothing to
// normalise away.
func applyInitContainerDiagnostics(containers []corev1.Container) {
	for i := range containers {
		containers[i].TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	}
}

func buildModelStorageConfig(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService, namespace string, useCache bool, cacheMode string, caCertConfigMap string, initContainerImage string, defaultFSGroup int64, allowedHostPathRoots []string) (cfg modelStorageConfig) {
	// Stamp crash diagnostics on whatever the branches below return. Done
	// once here rather than at each construction site so a future storage
	// path cannot be added without it (#1437).
	defer func() { applyInitContainerDiagnostics(cfg.initContainers) }()
	// Host-path allowlist gate (GHSA-jw3m-8q7m-f35r), belt-and-suspenders to
	// the upfront check in the InferenceService reconcile: a local source
	// outside the allowed roots must never yield a HostPathVolumeSource, even
	// if a future caller forgets the reconcile-time validation. Fail loudly
	// with an init container that exits instead of silently serving nothing.
	if err := validateLocalSourceAllowed(model.Spec.Source, allowedHostPathRoots); err != nil {
		return disallowedLocalSourceStorageConfig(initContainerImage)
	}
	if isPVCSource(model.Spec.Source) {
		return buildPVCStorageConfig(model)
	}
	if useCache {
		return buildCachedStorageConfig(model, isvc, cacheMode, caCertConfigMap, initContainerImage, defaultFSGroup)
	}
	return buildEmptyDirStorageConfig(model, isvc, namespace, caCertConfigMap, initContainerImage)
}

// disallowedLocalSourceStorageConfig returns a storage config whose init
// container immediately exits with a clear error, and which mounts no volumes
// beyond an ephemeral emptyDir. Used when the model's local source fails the
// host-path allowlist (GHSA-jw3m-8q7m-f35r) so that no HostPathVolumeSource is
// ever emitted for a disallowed source.
func disallowedLocalSourceStorageConfig(initImage string) modelStorageConfig {
	return modelStorageConfig{
		modelPath: "/models/model.gguf",
		initContainers: []corev1.Container{
			{
				Name:  "model-downloader",
				Image: initImage,
				Command: []string{"sh", "-c",
					`echo "ERROR: SourceNotAllowed - the model's local/hostPath source is not within the operator's --allowed-host-path-roots (GHSA-jw3m-8q7m-f35r)."; exit 1`},
			},
		},
		volumes: []corev1.Volume{
			{Name: "model-storage", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
		volumeMounts: []corev1.VolumeMount{{Name: "model-storage", MountPath: "/models", ReadOnly: true}},
	}
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

func buildCachedStorageConfig(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService, cacheMode string, caCertConfigMap string, initContainerImage string, defaultFSGroup int64) modelStorageConfig {
	cacheDir := fmt.Sprintf("/models/%s", effectiveModelCacheKey(model))

	// Resolve the fsGroup that the CSI will actually apply to the volume.
	// When the InferenceService sets its own FSGroup, that wins over the
	// operator default (see inferPodSecurityContext in deployment_builder.go);
	// chown'ing to the wrong GID would leave the downloader unable to write.
	// A value <= 0 means the operator disabled fsGroup (e.g. OpenShift), so
	// the prep must chown to the downloader's own UID instead.
	resolvedFSGroup := defaultFSGroup
	if isvc != nil && isvc.Spec.PodSecurityContext != nil && isvc.Spec.PodSecurityContext.FSGroup != nil {
		resolvedFSGroup = *isvc.Spec.PodSecurityContext.FSGroup
	}

	// Multi-file staging branch: when spec.files or spec.mmproj are set, use
	// the staging plan to download all artifacts. Returns early.
	plan, err := modelStagingPlan(model)
	if err != nil {
		return modelStorageConfig{
			modelPath: stagedCachePath(cacheDir, "model.gguf"),
			initContainers: []corev1.Container{
				invalidFileSetInitContainer(initContainerImage),
			},
			volumes: []corev1.Volume{
				{
					Name: "model-cache",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: modelCachePVCName(isvc, cacheMode),
							ReadOnly:  false,
						},
					},
				},
			},
			volumeMounts: []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models", ReadOnly: true}},
		}
	}
	if plan != nil {
		modelPath := stagedCachePath(cacheDir, plan.Primary)
		cmd := buildMultiFileInitCommand(true, isS3Source(model.Spec.Source), model.Spec.RefreshPolicy)
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

		initContainers := []corev1.Container{
			cachePrepInitContainer(initContainerImage, resolvedFSGroup),
			{
				Name:            "model-downloader",
				Image:           initContainerImage,
				Command:         []string{"sh", "-c", cmd},
				Env:             env,
				EnvFrom:         modelEnvFrom(model),
				VolumeMounts:    initVolumeMounts,
				SecurityContext: initContainerSecurityContext(isvc),
			},
		}

		return modelStorageConfig{
			modelPath:      modelPath,
			stagedDir:      cacheDir,
			initContainers: initContainers,
			volumes:        volumes,
			volumeMounts:   []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models", ReadOnly: true}},
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

	cmd := buildModelInitCommand(isLocalModelSource(model.Spec.Source), isS3Source(model.Spec.Source), true, model.Spec.RefreshPolicy)
	env := modelInitEnvVars(model.Spec.Source, cacheDir, modelPath)
	addCACertVolume(&volumes, &initVolumeMounts, &cmd, caCertConfigMap)

	initContainers := []corev1.Container{
		cachePrepInitContainer(initContainerImage, resolvedFSGroup),
		{
			Name:            "model-downloader",
			Image:           initContainerImage,
			Command:         []string{"sh", "-c", cmd},
			Env:             env,
			EnvFrom:         modelEnvFrom(model),
			VolumeMounts:    initVolumeMounts,
			SecurityContext: initContainerSecurityContext(isvc),
		},
	}

	return modelStorageConfig{
		modelPath:      modelPath,
		initContainers: initContainers,
		volumes:        volumes,
		volumeMounts:   []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models", ReadOnly: true}},
	}
}

func buildEmptyDirStorageConfig(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService, namespace string, caCertConfigMap string, initContainerImage string) modelStorageConfig {
	// Multi-file staging branch for emptyDir storage.
	plan, err := modelStagingPlan(model)
	if err != nil {
		return modelStorageConfig{
			modelPath: fmt.Sprintf("/models/%s-%s/model.gguf", namespace, model.Name),
			initContainers: []corev1.Container{
				invalidFileSetInitContainer(initContainerImage),
			},
			volumes: []corev1.Volume{
				{Name: "model-storage", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			volumeMounts: []corev1.VolumeMount{{Name: "model-storage", MountPath: "/models", ReadOnly: true}},
		}
	}
	if plan != nil {
		stagedDir := fmt.Sprintf("/models/%s-%s", namespace, model.Name)
		modelPath := fmt.Sprintf("%s/%s", stagedDir, plan.Primary)
		cmd := buildMultiFileInitCommand(false, isS3Source(model.Spec.Source), model.Spec.RefreshPolicy)
		env := multiFileInitEnvVars(model.Spec.Source, stagedDir, plan.Files)

		initVolumeMounts := []corev1.VolumeMount{{Name: "model-storage", MountPath: "/models"}}
		volumes := []corev1.Volume{
			{
				Name: "model-storage",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: ephemeralCacheSizeLimit(isvc),
				}},
			},
		}

		addCACertVolume(&volumes, &initVolumeMounts, &cmd, caCertConfigMap)

		return modelStorageConfig{
			modelPath: modelPath,
			stagedDir: stagedDir,
			initContainers: []corev1.Container{{
				Name:            "model-downloader",
				Image:           initContainerImage,
				Command:         []string{"sh", "-c", cmd},
				Env:             env,
				EnvFrom:         modelEnvFrom(model),
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
			Name: "model-storage",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: ephemeralCacheSizeLimit(isvc),
			}},
		},
	}

	cmd := buildModelInitCommand(isLocalModelSource(model.Spec.Source), isS3Source(model.Spec.Source), false, model.Spec.RefreshPolicy)
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
				EnvFrom:         modelEnvFrom(model),
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
// ensureSharedModelCachePVC creates the namespace's shared llmkube-model-cache
// PVC if absent. Extracted from the InferenceService reconciler's shared-mode
// branch so the Model prefetch path (#904) can guarantee the same PVC exists
// before its download Job mounts it. No owner reference: the shared cache
// outlives any one consumer.
func ensureSharedModelCachePVC(ctx context.Context, c client.Client, namespace, size, class, accessModeCfg string) error {
	pvc := &corev1.PersistentVolumeClaim{}
	err := c.Get(ctx, types.NamespacedName{Name: ModelCachePVCName, Namespace: namespace}, pvc)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check for existing PVC: %w", err)
	}

	accessMode := corev1.ReadWriteOnce
	if accessModeCfg == "ReadWriteMany" {
		accessMode = corev1.ReadWriteMany
	}
	if size == "" {
		size = "100Gi"
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
				Requests: corev1.ResourceList{corev1.ResourceStorage: storageSize},
			},
		},
	}
	if class != "" {
		newPVC.Spec.StorageClassName = &class
	}
	if err := c.Create(ctx, newPVC); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create shared model cache PVC: %w", err)
	}
	return nil
}

func (r *InferenceServiceReconciler) ensureModelCachePVC(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	log := logf.FromContext(ctx)

	// Bring-your-own cache PVC (#928): spec.modelCache.claimName names a
	// user-owned claim, so the operator never creates, mutates, or deletes
	// it — it only verifies the claim exists. A missing claim is surfaced as
	// an error (-> Degraded condition + event) rather than silently falling
	// back to the shared cache.
	if claim := userModelCacheClaimName(isvc); claim != "" {
		pvc := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, types.NamespacedName{Name: claim, Namespace: isvc.Namespace}, pvc)
		if err == nil {
			return nil
		}
		if apierrors.IsNotFound(err) {
			if r.Recorder != nil {
				r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, "ModelCachePVCNotFound", "Reconcile",
					"spec.modelCache.claimName %q does not exist in namespace %q; create the PVC or remove the field",
					claim, isvc.Namespace)
			}
			return fmt.Errorf(
				"model cache PVC %q (spec.modelCache.claimName) not found in namespace %q: the claim is user-owned and must be created before use",
				claim, isvc.Namespace)
		}
		return fmt.Errorf("failed to check user model cache PVC %q: %w", claim, err)
	}

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
