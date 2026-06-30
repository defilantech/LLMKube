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
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/defilantech/llmkube/pkg/gguf"
	"github.com/defilantech/llmkube/pkg/license"
)

const (
	PhaseReady    = "Ready"
	PhaseFailed   = "Failed"
	PhaseCached   = "Cached"
	PhaseCreating = "Creating"
	PhaseStopped  = "Stopped"
	// acceleratorMetal is the Model.Spec.Hardware.Accelerator value for the
	// host metal-agent path.
	acceleratorMetal      = "metal"
	acceleratorCUDA       = "cuda"
	acceleratorROCm       = "rocm"
	acceleratorCPU        = "cpu"
	DefaultModelCachePath = "/models"

	ConditionAvailable   = "Available"
	ConditionDegraded    = "Degraded"
	ConditionProgressing = "Progressing"
	// ConditionSourceDrifted is set True when the upstream source bytes differ
	// from the cached copy, regardless of RefreshPolicy, so drift is always
	// visible even when the controller does not act on it.
	ConditionSourceDrifted = "SourceDrifted"

	// ConditionRolloutDeferred indicates whether a rollout is being deferred
	// because the InferenceService has waitForIdle enabled and pods are not yet
	// idle. When True, the Deployment pod-template update is held until all
	// backend slots report idle or the idleTimeoutSeconds expires.
	ConditionRolloutDeferred = inferencev1alpha1.ConditionRolloutDeferred

	ReasonPodsBusy            = inferencev1alpha1.ReasonPodsBusy
	ReasonIdleTimeoutExceeded = inferencev1alpha1.ReasonIdleTimeoutExceeded

	ReasonWorkloadResolved = "WorkloadResolved"

	// RefreshPolicyIfNotPresent downloads only when the cached file is missing
	// (the default; preserves historical behavior).
	RefreshPolicyIfNotPresent = "IfNotPresent"
	// RefreshPolicyOnChange re-downloads when the upstream bytes differ from the
	// cached copy.
	RefreshPolicyOnChange = "OnChange"

	// DefaultRevalidateInterval is the minimum time between upstream
	// revalidation checks for a given Model. Bounds the HEAD traffic the
	// controller generates and serves as the RequeueAfter so drift is detected
	// without an external trigger.
	DefaultRevalidateInterval = time.Hour
)

type ModelReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	StoragePath string
	// RevalidateInterval is the minimum time between upstream revalidation
	// checks for a Model. Zero means DefaultRevalidateInterval.
	RevalidateInterval time.Duration
}

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

func (r *ModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reconcileStart := time.Now()
	defer func() {
		llmkubemetrics.ReconcileDuration.WithLabelValues("model").Observe(time.Since(reconcileStart).Seconds())
	}()

	logger := log.FromContext(ctx)
	logger.Info("Reconciling Model", "name", req.Name, "namespace", req.Namespace)

	model := &inferencev1alpha1.Model{}
	if err := r.Get(ctx, req.NamespacedName, model); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get Model")
		return ctrl.Result{}, err
	}

	if r.StoragePath == "" {
		r.StoragePath = DefaultModelCachePath
	}

	if handled, result := r.validateMultiFileStagingSource(ctx, model); handled {
		return result, nil
	}

	// Sources that need no controller-side download (PVC, HuggingFace repo,
	// remote HTTP, Metal local-path) are dispatched here. handled=false means
	// the source is a local path the controller must copy itself.
	if handled, result, err := r.reconcileBySourceType(ctx, model); handled {
		return result, err
	}

	cacheKey := computeCacheKey(model.Spec.Source)
	modelDir := filepath.Join(r.StoragePath, cacheKey)
	// downloadPath is the path used during/after download. After GGUF metadata
	// parsing, the file is migrated to canonicalModelPath(modelDir, model).
	downloadPath := filepath.Join(modelDir, legacyModelFilename)

	// Early exit: if status is Ready and the cached file is current, skip the
	// rest of the reconcile (a drift check may schedule a requeue). Otherwise we
	// fall through to (re-)download or migrate the cached file.
	if skip, result, err := r.handleReadyCachedModel(ctx, model); skip || err != nil {
		return result, err
	}

	logger.Info("Using cache key for model", "cacheKey", cacheKey, "dir", modelDir)

	if existingPath, fileInfo, ok := findCachedModelFile(modelDir); ok {
		// Cadence-gated drift check on the cached file. Under OnChange a drifted
		// source re-downloads (overwrites); under IfNotPresent it only records
		// the SourceDrifted condition and keeps serving the cache. Status writes
		// here merge into the Ready update below (same in-memory object).
		reDownload, _, err := r.handleRevalidation(ctx, model)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !reDownload {
			logger.Info("Model found in cache, skipping download", "path", existingPath, "size", fileInfo.Size())

			// Parse GGUF metadata first (non-fatal) so we have the metadata-derived
			// name available for the rename below.
			if model.Status.GGUF == nil {
				if ggufMeta, err := r.parseGGUFMetadata(existingPath); err != nil {
					logger.Info("Failed to parse GGUF metadata (non-fatal)", "error", err)
				} else {
					model.Status.GGUF = ggufMeta
				}
			}

			finalPath, err := r.migrateModelFilename(existingPath, modelDir, model)
			if err != nil {
				logger.Error(err, "Failed to migrate model filename, keeping existing")
				finalPath = existingPath
			}

			model.Status.Phase = PhaseReady
			model.Status.Path = finalPath
			model.Status.Size = formatBytes(fileInfo.Size())
			model.Status.CacheKey = cacheKey
			model.Status.AcceleratorReady = r.checkAcceleratorAvailability(ctx, model)
			now := metav1.Now()
			model.Status.LastUpdated = &now

			if err := r.updateStatus(ctx, model, "Available", metav1.ConditionTrue, "ModelCached", "Model found in cache"); err != nil {
				return ctrl.Result{}, err
			}

			llmkubemetrics.ModelStatus.WithLabelValues(model.Name, model.Namespace, "Cached").Set(1)
			llmkubemetrics.ReconcileTotal.WithLabelValues("model", "success").Inc()
			return ctrl.Result{}, nil
		}

		logger.Info("Source drifted under OnChange; re-downloading over cached file", "dir", modelDir)
		if err := r.removeCachedFiles(ctx, modelDir); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := os.MkdirAll(modelDir, 0755); err != nil {
		logger.Error(err, "Failed to create cache directory", "path", modelDir)
		return ctrl.Result{}, err
	}

	isLocal := isLocalSource(model.Spec.Source)
	progressPhase := "Downloading"
	progressReason := "DownloadStarted"
	progressMessage := "Started downloading model"
	failReason := "DownloadFailed"
	if isLocal {
		progressPhase = "Copying"
		progressReason = "CopyStarted"
		progressMessage = "Started copying local model"
		failReason = "CopyFailed"
	}

	if model.Status.Phase != progressPhase {
		model.Status.Phase = progressPhase
		model.Status.CacheKey = cacheKey
		if err := r.updateStatus(ctx, model, ConditionProgressing, metav1.ConditionTrue, progressReason, progressMessage); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info(progressMessage, "source", model.Spec.Source, "cacheKey", cacheKey)
	}

	fetchStart := time.Now()
	sourceType := "remote"
	if isLocal {
		sourceType = "local"
	}
	size, err := r.fetchModel(ctx, model.Spec.Source, downloadPath)
	fetchDuration := time.Since(fetchStart).Seconds()
	llmkubemetrics.ModelDownloadDuration.WithLabelValues(model.Name, model.Namespace, sourceType).Observe(fetchDuration)
	if err != nil {
		logger.Error(err, "Failed to fetch model")
		llmkubemetrics.ReconcileTotal.WithLabelValues("model", "error").Inc()
		llmkubemetrics.ModelStatus.WithLabelValues(model.Name, model.Namespace, PhaseFailed).Set(1)
		model.Status.Phase = PhaseFailed
		if statusErr := r.updateStatus(ctx, model, ConditionDegraded, metav1.ConditionTrue, failReason, err.Error()); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after fetch failure")
		}

		// Unrecoverable failures (missing path, permission denied) get
		// requeued on a fixed 5-minute interval WITHOUT returning the
		// error to controller-runtime. Returning err here invokes the
		// rate-limited workqueue, which under sustained failure starts at
		// ~5ms and ramps to a cap, doing expensive reconcile work
		// (status updates, GGUF parses, metric churn) hundreds of times
		// per second along the way. That's the #405 hot-spin: a Mac kind
		// cluster pinned a CPU core for 35 hours when a file:// source
		// referenced a host path invisible to the controller pod.
		// Returning nil here keeps RequeueAfter as the only retry signal
		// and makes the steady-state cost a single reconcile every
		// 5 minutes until the operator fixes the spec.
		if isUnrecoverableFetchError(err) {
			logger.Info(
				"Fetch error is terminal; deferring retry to RequeueAfter without rate-limited backoff",
				"source", model.Spec.Source,
				"reason", "unrecoverable fetch error (ENOENT or EACCES)",
			)
			return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
		}

		return ctrl.Result{RequeueAfter: 5 * time.Minute}, err
	}

	// SHA256 integrity verification
	if err := r.verifySHA256(ctx, model, downloadPath); err != nil {
		logger.Error(err, "SHA256 integrity check failed")
		_ = os.Remove(downloadPath)
		llmkubemetrics.ReconcileTotal.WithLabelValues("model", "error").Inc()
		llmkubemetrics.ModelStatus.WithLabelValues(model.Name, model.Namespace, PhaseFailed).Set(1)
		model.Status.Phase = PhaseFailed
		if statusErr := r.updateStatus(ctx, model, ConditionDegraded, metav1.ConditionTrue, "IntegrityCheckFailed", err.Error()); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after integrity check failure")
		}
		return ctrl.Result{}, err
	}

	// Parse GGUF metadata (non-fatal). Done before the rename so the metadata-
	// derived name is available to canonicalModelPath.
	if ggufMeta, err := r.parseGGUFMetadata(downloadPath); err != nil {
		logger.Info("Failed to parse GGUF metadata (non-fatal)", "error", err)
	} else {
		model.Status.GGUF = ggufMeta
	}

	finalPath, err := r.migrateModelFilename(downloadPath, modelDir, model)
	if err != nil {
		logger.Error(err, "Failed to rename model to canonical filename, keeping download path")
		finalPath = downloadPath
	}

	model.Status.Phase = PhaseReady
	model.Status.Path = finalPath
	model.Status.Size = formatBytes(size)
	model.Status.CacheKey = cacheKey
	model.Status.AcceleratorReady = r.checkAcceleratorAvailability(ctx, model)
	now := metav1.Now()
	model.Status.LastUpdated = &now

	// Record the upstream fingerprint so the next revalidation has a baseline,
	// and clear any SourceDrifted condition now that the cache matches upstream.
	r.recordSourceFingerprint(ctx, model)
	r.setSourceDrifted(model, false)

	if err := r.updateStatus(ctx, model, "Available", metav1.ConditionTrue, "ModelReady", "Model downloaded and cached"); err != nil {
		return ctrl.Result{}, err
	}

	llmkubemetrics.ModelStatus.WithLabelValues(model.Name, model.Namespace, "Ready").Set(1)
	llmkubemetrics.ReconcileTotal.WithLabelValues("model", "success").Inc()
	logger.Info("Model ready and cached", "path", finalPath, "size", model.Status.Size, "cacheKey", cacheKey)
	return ctrl.Result{}, nil
}

func (r *ModelReconciler) validateMultiFileStagingSource(ctx context.Context, model *inferencev1alpha1.Model) (bool, ctrl.Result) {
	if !hasMultiFileStaging(model) {
		return false, ctrl.Result{}
	}
	if valErr := validateHFRepoSource(model.Spec.Source); valErr != nil {
		return true, r.failInvalidFileSet(ctx, model, valErr.Error())
	}
	if !isHFRepoSource(model.Spec.Source) {
		msg := fmt.Sprintf("multi-file staging requires a HuggingFace repo source, but got: %s", model.Spec.Source)
		return true, r.failInvalidFileSet(ctx, model, msg)
	}
	return false, ctrl.Result{}
}

func (r *ModelReconciler) failInvalidFileSet(ctx context.Context, model *inferencev1alpha1.Model, message string) ctrl.Result {
	logger := log.FromContext(ctx)
	model.Status.Phase = PhaseFailed
	if updateErr := r.updateStatus(ctx, model, ConditionDegraded, metav1.ConditionTrue, "InvalidFileSet", message); updateErr != nil {
		logger.Error(updateErr, "Failed to update status after invalid file set")
	}
	llmkubemetrics.ReconcileTotal.WithLabelValues("model", "error").Inc()
	llmkubemetrics.ModelStatus.WithLabelValues(model.Name, model.Namespace, PhaseFailed).Set(1)
	return ctrl.Result{RequeueAfter: 5 * time.Minute}
}

// handleReadyCachedModel handles a Model that is already Ready with a cached
// file on disk. It returns skip=true when the reconcile can stop here (the file
// is current); the returned result may carry a RequeueAfter scheduling the next
// drift revalidation. It returns skip=false to let Reconcile fall through and
// (re-)download or migrate the file: either because the file is missing, needs
// a filename migration, or drifted under RefreshPolicy=OnChange.
func (r *ModelReconciler) handleReadyCachedModel(
	ctx context.Context, model *inferencev1alpha1.Model,
) (skip bool, result ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	if model.Status.Phase != PhaseReady || model.Status.Path == "" {
		return false, ctrl.Result{}, nil
	}
	if _, statErr := os.Stat(model.Status.Path); statErr != nil {
		logger.Info("Model marked Ready but file missing, will re-download", "path", model.Status.Path)
		return false, ctrl.Result{}, nil
	}

	canonical := canonicalModelPath(filepath.Dir(model.Status.Path), model)
	if model.Status.Path != canonical {
		logger.Info("Model needs filename migration", "currentPath", model.Status.Path, "canonicalPath", canonical)
		return false, ctrl.Result{}, nil
	}

	// Cadence-gated drift check. Under OnChange a drifted source re-downloads
	// (skip=false so Reconcile falls through); under IfNotPresent it only
	// records the SourceDrifted condition and keeps serving the cache.
	reDownload, requeueAfter, err := r.handleRevalidation(ctx, model)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if reDownload {
		logger.Info("Source drifted under OnChange; re-downloading over cached file", "path", model.Status.Path)
		if rmErr := r.removeCachedFiles(ctx, filepath.Dir(model.Status.Path)); rmErr != nil {
			return false, ctrl.Result{}, rmErr
		}
		return false, ctrl.Result{}, nil
	}

	logger.Info("Model already Ready and cached at canonical path, skipping reconcile", "path", model.Status.Path)
	llmkubemetrics.ReconcileTotal.WithLabelValues("model", "success").Inc()
	return true, ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// reconcilePVCSource handles PVC-based model sources. It validates the referenced
// PVC exists and is Bound, then sets the model to Ready without downloading.
func (r *ModelReconciler) reconcilePVCSource(ctx context.Context, model *inferencev1alpha1.Model) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Early exit if already Ready
	if model.Status.Phase == PhaseReady {
		logger.Info("PVC model already Ready, skipping reconcile")
		llmkubemetrics.ReconcileTotal.WithLabelValues("model", "success").Inc()
		return ctrl.Result{}, nil
	}

	claimName, modelFilePath, err := parsePVCSource(model.Spec.Source)
	if err != nil {
		model.Status.Phase = PhaseFailed
		if statusErr := r.updateStatus(ctx, model, ConditionDegraded, metav1.ConditionTrue, "InvalidSource", err.Error()); statusErr != nil {
			logger.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{}, err
	}

	// Validate the PVC exists and is Bound
	pvc := &corev1.PersistentVolumeClaim{}
	pvcKey := types.NamespacedName{Name: claimName, Namespace: model.Namespace}
	if err := r.Get(ctx, pvcKey, pvc); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Referenced PVC not found", "pvc", claimName)
			model.Status.Phase = PhaseFailed
			msg := fmt.Sprintf("PVC %q not found in namespace %q", claimName, model.Namespace)
			if statusErr := r.updateStatus(ctx, model, ConditionDegraded, metav1.ConditionTrue, "PVCNotFound", msg); statusErr != nil {
				logger.Error(statusErr, "Failed to update status")
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	if pvc.Status.Phase != corev1.ClaimBound {
		logger.Info("PVC not yet bound", "pvc", claimName, "phase", pvc.Status.Phase)
		model.Status.Phase = "Pending"
		msg := fmt.Sprintf("PVC %q is %s, waiting for it to be Bound", claimName, pvc.Status.Phase)
		if statusErr := r.updateStatus(ctx, model, ConditionProgressing, metav1.ConditionTrue, "PVCNotBound", msg); statusErr != nil {
			logger.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// PVC is valid and bound — set model as Ready
	cacheKey := computeCacheKey(model.Spec.Source)
	mountPath := fmt.Sprintf("/model-source/%s", modelFilePath)

	model.Status.Phase = PhaseReady
	model.Status.Path = mountPath
	model.Status.CacheKey = cacheKey
	model.Status.AcceleratorReady = r.checkAcceleratorAvailability(ctx, model)
	now := metav1.Now()
	model.Status.LastUpdated = &now

	if err := r.updateStatus(ctx, model, "Available", metav1.ConditionTrue, "PVCModelReady", fmt.Sprintf("Model available on PVC %q", claimName)); err != nil {
		return ctrl.Result{}, err
	}

	llmkubemetrics.ModelStatus.WithLabelValues(model.Name, model.Namespace, "Ready").Set(1)
	llmkubemetrics.ReconcileTotal.WithLabelValues("model", "success").Inc()
	logger.Info("PVC model ready", "pvc", claimName, "path", mountPath)
	return ctrl.Result{}, nil
}

// reconcileRuntimeResolvedSource handles model sources whose actual fetch is
// performed outside the Model controller — either by the runtime container
// itself (HuggingFace repo IDs resolved by vLLM/llama.cpp at startup) or by
// the InferenceService Pod's init container (remote HTTP(S) URLs downloaded
// into the per-namespace model cache PVC). In both cases the controller
// marks the model Ready immediately so referencing InferenceServices can
// proceed.
//
// cacheKey is populated for sources that flow through the per-namespace cache
// PVC (HTTP(S) URLs). It is empty for runtime-internal sources like HF repo
// IDs where the runtime resolves and stores the model on its own. A non-empty
// cacheKey lets InferenceService.spec build the canonical
// /models/<cacheKey>/<basename> path that the init container will populate.
// isMetalModel reports whether the model targets the host metal-agent path.
func isMetalModel(model *inferencev1alpha1.Model) bool {
	return model.Spec.Hardware != nil && model.Spec.Hardware.Accelerator == acceleratorMetal
}

// reconcileBySourceType handles the model sources that need no controller-side
// download. It returns handled=true when it owns the reconcile (so Reconcile
// returns immediately); handled=false means the source is a local path the
// controller must copy itself.
func (r *ModelReconciler) reconcileBySourceType(
	ctx context.Context, model *inferencev1alpha1.Model,
) (handled bool, result ctrl.Result, err error) {
	switch {
	// PVC sources: validate the PVC exists, mark Ready, no download.
	case isPVCSource(model.Spec.Source):
		result, err = r.reconcilePVCSource(ctx, model)
		return true, result, err

	// HuggingFace repo IDs: the runtime container fetches at startup; the
	// controller marks the model Ready so referencing InferenceServices can
	// proceed.
	case isHFRepoSource(model.Spec.Source):
		result, err = r.reconcileRuntimeResolvedSource(ctx, model, "")
		return true, result, err

	// Remote HTTP(S): the InferenceService Pod's init container downloads
	// into the per-namespace cache PVC. A controller-side download writes to
	// the operator-namespace PVC, which inference Pods cannot mount, so the
	// fetch is deferred to the workload.
	case isRemoteHTTPSource(model.Spec.Source):
		result, err = r.reconcileRuntimeResolvedSource(
			ctx, model, computeCacheKey(model.Spec.Source))
		return true, result, err

	// Metal-accelerated models with a local-path source live on the Metal
	// node's own filesystem and are loaded directly by the host metal-agent.
	// The in-cluster controller cannot see that path, so it neither downloads
	// nor copies — it marks the model Ready and lets the agent be the source
	// of truth. A bad path then surfaces as the InferenceService failing to
	// come up, not as a Model wedged in Copying forever.
	case isMetalModel(model) && isLocalSource(model.Spec.Source):
		result, err = r.reconcileRuntimeResolvedSource(ctx, model, "")
		return true, result, err
	}

	return false, ctrl.Result{}, nil
}

func (r *ModelReconciler) reconcileRuntimeResolvedSource(ctx context.Context, model *inferencev1alpha1.Model, cacheKey string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Early exit if already Ready. For sources with a controller-observable
	// fingerprint (http/https) run a cadence-gated drift check so the
	// SourceDrifted condition stays current even though the controller itself
	// does not fetch these (the InferenceService init container does). Re-fetch
	// of the workload-owned copy is out of scope here; the controller only makes
	// drift visible and requeues so it is detected without an external trigger.
	if model.Status.Phase == PhaseReady {
		if isRemoteHTTPSource(model.Spec.Source) {
			if _, requeueAfter, err := r.handleRevalidation(ctx, model); err != nil {
				return ctrl.Result{}, err
			} else {
				llmkubemetrics.ReconcileTotal.WithLabelValues("model", "success").Inc()
				return ctrl.Result{RequeueAfter: requeueAfter}, nil
			}
		}
		logger.Info("Runtime-resolved model already Ready, skipping reconcile")
		llmkubemetrics.ReconcileTotal.WithLabelValues("model", "success").Inc()
		return ctrl.Result{}, nil
	}

	isMetal := isMetalModel(model)

	reason := "RuntimeResolved"
	message := "Source is runtime-resolved (e.g., HuggingFace repo ID); runtime will fetch at startup"
	if cacheKey == "" && isMetal && isLocalSource(model.Spec.Source) {
		// Local-path model on the Metal node: the host metal-agent loads it
		// directly. The controller marks it Ready without copying.
		reason = "MetalAgentManaged"
		message = "Source is a local path on the Metal node; the host metal-agent loads the model directly"
	} else if cacheKey != "" {
		reason = ReasonWorkloadResolved
		if isMetal {
			// On the metal accelerator path there is no Pod and no init container.
			// The host metal-agent fetches the file into its model store when it
			// reconciles the InferenceService that references this Model.
			message = "Source is a remote URL; the host metal-agent will fetch the model into its model store when the InferenceService is reconciled"
		} else {
			message = "Source is a remote URL; the InferenceService Pod's init container will fetch the model into the per-namespace model cache PVC at startup"
		}
	}

	logger.Info("Source is runtime-resolved, skipping controller-side download", "source", model.Spec.Source, "cacheKey", cacheKey)

	// Validate multi-file staging before marking Ready.
	if hasMultiFileStaging(model) {
		plan, err := ResolveFileSet(model.Spec.Files, model.Spec.Mmproj, nil)
		if err != nil {
			model.Status.Phase = PhaseFailed
			if updateErr := r.updateStatus(ctx, model, ConditionDegraded, metav1.ConditionTrue, "InvalidFileSet", err.Error()); updateErr != nil {
				logger.Error(updateErr, "Failed to update status after invalid file set")
			}
			llmkubemetrics.ReconcileTotal.WithLabelValues("model", "error").Inc()
			llmkubemetrics.ModelStatus.WithLabelValues(model.Name, model.Namespace, PhaseFailed).Set(1)
			return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
		}
		model.Status.StagedFiles = plan.Files
	} else {
		model.Status.StagedFiles = nil
	}

	model.Status.Phase = PhaseReady
	model.Status.Path = ""
	model.Status.CacheKey = cacheKey
	model.Status.Size = "0"

	// For remote http(s) GGUF sources, read the metadata via an HTTP range
	// (header-only) request so Status carries architecture/layers/size without
	// the controller downloading the whole file. The full model bytes are
	// fetched only by the per-isvc init container. Non-fatal: a metadata read
	// failure (air-gapped, unreachable, non-GGUF) must not block the model from
	// reaching Ready, since the workload still resolves the source itself.
	if isRemoteHTTPSource(model.Spec.Source) && model.Status.GGUF == nil {
		if ggufMeta, size, err := r.parseRemoteGGUFMetadata(ctx, model.Spec.Source); err != nil {
			logger.Info("Failed to read remote GGUF metadata (non-fatal)", "source", model.Spec.Source, "error", err)
		} else {
			model.Status.GGUF = ggufMeta
			if size > 0 {
				model.Status.Size = formatBytes(size)
			}
		}
	}

	model.Status.AcceleratorReady = r.checkAcceleratorAvailability(ctx, model)
	now := metav1.Now()
	model.Status.LastUpdated = &now

	// A model reaching Ready through this no-download path may still carry
	// Progressing/Degraded conditions from an earlier copy or download
	// attempt; clear them so a Ready model is not also reported Degraded.
	if err := r.clearProgressConditions(ctx, model); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.updateStatus(ctx, model, "Available", metav1.ConditionTrue, reason, message); err != nil {
		return ctrl.Result{}, err
	}

	llmkubemetrics.ModelStatus.WithLabelValues(model.Name, model.Namespace, "Ready").Set(1)
	llmkubemetrics.ReconcileTotal.WithLabelValues("model", "success").Inc()
	logger.Info("Runtime-resolved model ready", "source", model.Spec.Source)
	return ctrl.Result{}, nil
}

// verifySHA256 computes the SHA256 hash of the file and verifies it against the
// spec if provided. The computed hash is always stored in status.
func (r *ModelReconciler) verifySHA256(ctx context.Context, model *inferencev1alpha1.Model, filePath string) error {
	logger := log.FromContext(ctx)

	computedHash, err := computeFileSHA256(filePath)
	if err != nil {
		return fmt.Errorf("failed to compute SHA256: %w", err)
	}

	model.Status.SHA256 = computedHash
	logger.Info("Computed model file SHA256", "sha256", computedHash)

	if model.Spec.SHA256 != "" {
		if !strings.EqualFold(computedHash, model.Spec.SHA256) {
			return fmt.Errorf("SHA256 mismatch: expected %s, got %s", model.Spec.SHA256, computedHash)
		}
		logger.Info("SHA256 integrity check passed")
	}

	return nil
}

func (r *ModelReconciler) fetchModel(ctx context.Context, source, dest string) (int64, error) {
	if isLocalSource(source) {
		return r.copyLocalModel(ctx, source, dest)
	}
	return r.downloadModel(ctx, source, dest)
}

func (r *ModelReconciler) copyLocalModel(ctx context.Context, source, dest string) (int64, error) {
	logger := log.FromContext(ctx)

	localPath := getLocalPath(source)
	logger.Info("Copying local model", "source", localPath, "dest", dest)

	srcFile, err := os.Open(localPath)
	if err != nil {
		return 0, fmt.Errorf("failed to open local model file: %w", err)
	}
	defer func() {
		if closeErr := srcFile.Close(); closeErr != nil {
			logger.Error(closeErr, "Failed to close source file")
		}
	}()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("failed to stat local model file: %w", err)
	}

	// Write to temp file in the same directory, then atomic rename
	destDir := filepath.Dir(dest)
	tmpFile, err := os.CreateTemp(destDir, ".model-*.tmp")
	if err != nil {
		return 0, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }() // clean up on any failure

	size, err := io.Copy(tmpFile, srcFile)
	if closeErr := tmpFile.Close(); closeErr != nil {
		logger.Error(closeErr, "Failed to close temp file")
	}
	if err != nil {
		return 0, fmt.Errorf("failed to copy model file: %w", err)
	}

	if size != srcInfo.Size() {
		return 0, fmt.Errorf("copy incomplete: expected %d bytes, got %d", srcInfo.Size(), size)
	}

	if err := os.Rename(tmpPath, dest); err != nil {
		return 0, fmt.Errorf("failed to rename temp file to final path: %w", err)
	}

	logger.Info("Local model copied successfully", "size", size)
	return size, nil
}

func (r *ModelReconciler) downloadModel(ctx context.Context, source, dest string) (int64, error) {
	logger := log.FromContext(ctx)

	logger.Info("Downloading model", "source", source, "dest", dest)
	resp, err := http.Get(source)
	if err != nil {
		return 0, fmt.Errorf("failed to download: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error(closeErr, "Failed to close response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("bad status: %s", resp.Status)
	}

	// Write to temp file in the same directory, then atomic rename
	destDir := filepath.Dir(dest)
	tmpFile, err := os.CreateTemp(destDir, ".model-*.tmp")
	if err != nil {
		return 0, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }() // clean up on any failure

	size, err := io.Copy(tmpFile, resp.Body)
	if closeErr := tmpFile.Close(); closeErr != nil {
		logger.Error(closeErr, "Failed to close temp file")
	}
	if err != nil {
		return 0, fmt.Errorf("failed to write file: %w", err)
	}

	// Validate Content-Length if the server provided one
	if resp.ContentLength > 0 && size != resp.ContentLength {
		return 0, fmt.Errorf("download incomplete: expected %d bytes, got %d", resp.ContentLength, size)
	}

	if err := os.Rename(tmpPath, dest); err != nil {
		return 0, fmt.Errorf("failed to rename temp file to final path: %w", err)
	}

	return size, nil
}

// clearProgressConditions flips any Progressing or Degraded conditions on the
// model to False. A model that reaches Ready through a no-download path can
// still carry Progressing/Degraded conditions written by an earlier copy or
// download attempt (for example a Metal local-path model wedged in Copying
// under an operator version that predates the no-download path). Leaving them
// set reports a Ready model as also Degraded, which is contradictory.
func (r *ModelReconciler) clearProgressConditions(ctx context.Context, model *inferencev1alpha1.Model) error {
	for _, condType := range []string{ConditionProgressing, ConditionDegraded} {
		for _, c := range model.Status.Conditions {
			if c.Type == condType && c.Status != metav1.ConditionFalse {
				if err := r.updateStatus(ctx, model, condType, metav1.ConditionFalse,
					"Superseded", "Model reached Ready without a controller-side download"); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

func (r *ModelReconciler) updateStatus(ctx context.Context, model *inferencev1alpha1.Model, condType string, status metav1.ConditionStatus, reason, message string) error {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: model.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}

	found := false
	for i, cond := range model.Status.Conditions {
		if cond.Type == condType {
			model.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		model.Status.Conditions = append(model.Status.Conditions, condition)
	}

	return r.Status().Update(ctx, model)
}

// checkAcceleratorAvailability reports whether the accelerator the Model
// requests is actually present in the cluster, so status.acceleratorReady
// reflects reality instead of always being true (#230). CPU and an unset
// accelerator are always available. Metal runs on an off-cluster metal-agent
// for which the operator has no reliable in-cluster liveness signal yet, so it
// is treated as available (probing the agent is a follow-up). GPU accelerators
// (cuda/rocm/intel) require at least one Node advertising the matching
// extended resource in its capacity.
//
//nolint:unparam
func (r *ModelReconciler) checkAcceleratorAvailability(ctx context.Context, model *inferencev1alpha1.Model) bool {
	if model == nil || model.Spec.Hardware == nil {
		return true
	}
	accel := strings.ToLower(strings.TrimSpace(model.Spec.Hardware.Accelerator))
	switch accel {
	case "", acceleratorCPU:
		return true
	case acceleratorMetal:
		return true
	}

	// DRA path: if the model uses resourceClaims, check that the referenced
	// ResourceClaim or ResourceClaimTemplate exists. Return false on NotFound
	// so AcceleratorReady reflects reality; fail-open (true) for transient or
	// RBAC errors.
	if model.Spec.Hardware.GPU != nil && len(model.Spec.Hardware.GPU.ResourceClaims) > 0 {
		return r.hasDRAAvailability(ctx, model.Namespace, model.Spec.Hardware.GPU.ResourceClaims)
	}

	// Honor the GPU resourceName override so the readiness check validates
	// the same extended resource the pod will actually request.
	if model.Spec.Hardware.GPU != nil {
		if override := strings.TrimSpace(model.Spec.Hardware.GPU.ResourceName); override != "" {
			return r.nodeHasResource(ctx, corev1.ResourceName(override))
		}
	}

	var resName corev1.ResourceName
	switch accel {
	case acceleratorROCm:
		resName = amdGPUResourceName
	case acceleratorCUDA:
		resName = nvidiaGPUResourceName
	default:
		// intel (and any other GPU vendor resolveGPUResourceName knows).
		resName = resolveGPUResourceName(model)
	}

	return r.nodeHasResource(ctx, resName)
}

// nodeHasResource reports whether any node in the cluster advertises the
// given extended resource in its capacity. Returns true on a node-list error
// (fail-open) so a transient or RBAC failure does not spuriously mark the
// accelerator unavailable.
func (r *ModelReconciler) nodeHasResource(ctx context.Context, res corev1.ResourceName) bool {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		log.FromContext(ctx).Error(err,
			"nodeHasResource: listing nodes failed; assuming available",
			"resource", res)
		return true
	}
	for i := range nodes.Items {
		if q, ok := nodes.Items[i].Status.Capacity[res]; ok && !q.IsZero() {
			return true
		}
	}
	return false
}

// hasDRAAvailability checks whether the DRA ResourceClaim or ResourceClaimTemplate
// referenced by each claim in the model's GPU spec exists. Returns false if any
// claim is NotFound; returns true if all claims are found or on transient/RBAC
// errors (fail-open).
func (r *ModelReconciler) hasDRAAvailability(ctx context.Context, namespace string, claims []corev1.PodResourceClaim) bool {
	for _, claim := range claims {
		if claim.ResourceClaimName != nil {
			var rc resourcev1.ResourceClaim
			if err := r.Get(ctx, types.NamespacedName{Name: *claim.ResourceClaimName, Namespace: namespace}, &rc); err != nil {
				if errors.IsNotFound(err) {
					log.FromContext(ctx).Info("DRA ResourceClaim not found; accelerator not ready",
						"resourceClaim", *claim.ResourceClaimName)
					return false
				}
				// Transient or RBAC error: fail-open.
				log.FromContext(ctx).Error(err,
					"hasDRAAvailability: getting ResourceClaim failed; assuming available",
					"resourceClaim", *claim.ResourceClaimName)
				return true
			}
		} else if claim.ResourceClaimTemplateName != nil {
			var rct resourcev1.ResourceClaimTemplate
			if err := r.Get(ctx, types.NamespacedName{Name: *claim.ResourceClaimTemplateName, Namespace: namespace}, &rct); err != nil {
				if errors.IsNotFound(err) {
					log.FromContext(ctx).Info("DRA ResourceClaimTemplate not found; accelerator not ready",
						"resourceClaimTemplate", *claim.ResourceClaimTemplateName)
					return false
				}
				// Transient or RBAC error: fail-open.
				log.FromContext(ctx).Error(err,
					"hasDRAAvailability: getting ResourceClaimTemplate failed; assuming available",
					"resourceClaimTemplate", *claim.ResourceClaimTemplateName)
				return true
			}
		}
	}
	return true
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func computeCacheKey(source string) string {
	hash := sha256.Sum256([]byte(source))
	return hex.EncodeToString(hash[:])[:16]
}

func (r *ModelReconciler) parseGGUFMetadata(path string) (*inferencev1alpha1.GGUFMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open model file: %w", err)
	}
	defer func() { _ = f.Close() }()

	parsed, err := gguf.Parse(bufio.NewReader(f))
	if err != nil {
		return nil, fmt.Errorf("failed to parse GGUF: %w", err)
	}

	return &inferencev1alpha1.GGUFMetadata{
		Architecture:  parsed.Architecture(),
		ModelName:     parsed.Name(),
		Quantization:  parsed.Quantization(),
		ContextLength: parsed.ContextLength(),
		EmbeddingSize: parsed.EmbeddingLength(),
		LayerCount:    parsed.BlockCount(),
		HeadCount:     parsed.HeadCount(),
		TensorCount:   parsed.Header.TensorCount,
		FileVersion:   parsed.Header.Version,
		License:       license.Normalize(parsed.License()),
	}, nil
}

// parseRemoteGGUFMetadata reads GGUF metadata from a remote http(s) URL using a
// header-only range read (pkg/gguf.ParseFromURL) so the controller never
// downloads the whole model. It also issues a HEAD to learn the object size for
// Status.Size; a missing/zero Content-Length yields size 0 (the caller leaves
// Size unchanged). The init container, not the controller, owns the full fetch.
// remoteMetadataTimeout bounds the header-only metadata read so a hung or slow
// model-source host cannot block a reconcile worker (controller-runtime
// contexts carry no default deadline).
const remoteMetadataTimeout = 30 * time.Second

func (r *ModelReconciler) parseRemoteGGUFMetadata(ctx context.Context, source string) (*inferencev1alpha1.GGUFMetadata, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteMetadataTimeout)
	defer cancel()
	parsed, err := gguf.ParseFromURL(ctx, source)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to parse remote GGUF: %w", err)
	}

	meta := &inferencev1alpha1.GGUFMetadata{
		Architecture:  parsed.Architecture(),
		ModelName:     parsed.Name(),
		Quantization:  parsed.Quantization(),
		ContextLength: parsed.ContextLength(),
		EmbeddingSize: parsed.EmbeddingLength(),
		LayerCount:    parsed.BlockCount(),
		HeadCount:     parsed.HeadCount(),
		TensorCount:   parsed.Header.TensorCount,
		FileVersion:   parsed.Header.Version,
		License:       license.Normalize(parsed.License()),
	}

	return meta, remoteContentLength(ctx, source), nil
}

// remoteContentLength returns the object size from a HEAD request, or 0 when the
// server does not report a usable Content-Length. Best-effort: any error yields
// 0 so the caller leaves Status.Size untouched rather than failing the reconcile.
func remoteContentLength(ctx context.Context, source string) int64 {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, source, nil)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.ContentLength <= 0 {
		return 0
	}
	return resp.ContentLength
}

// computeFileSHA256 streams a file through SHA256 and returns the hex-encoded hash.
func computeFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("failed to open file for hashing: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("failed to read file for hashing: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func (r *ModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.Model{}).
		Named("model").
		Complete(r)
}
