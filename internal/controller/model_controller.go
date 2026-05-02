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
	PhaseReady            = "Ready"
	PhaseFailed           = "Failed"
	PhaseCached           = "Cached"
	PhaseCreating         = "Creating"
	DefaultModelCachePath = "/models"

	ConditionAvailable = "Available"
	ConditionDegraded  = "Degraded"

	ReasonWorkloadResolved = "WorkloadResolved"
)

type ModelReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	StoragePath string
}

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch

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

	// PVC sources are handled differently: the controller validates the PVC exists
	// and sets the model as Ready without downloading anything.
	if isPVCSource(model.Spec.Source) {
		return r.reconcilePVCSource(ctx, model)
	}

	// Runtime-resolved sources (e.g., HuggingFace repo IDs) are fetched by the
	// runtime container at startup, not by the Model controller. The controller
	// marks the model Ready immediately so referencing InferenceServices can proceed.
	if isHFRepoSource(model.Spec.Source) {
		return ctrl.Result{}, r.reconcileRuntimeResolvedSource(ctx, model, "")
	}

	// Remote HTTP(S) sources are downloaded by the InferenceService Pod's
	// init container into the per-namespace model cache PVC, not by the
	// Model controller. The controller pod's storage path lives on the
	// operator-namespace PVC (e.g. llmkube-system/llmkube-model-cache) and
	// PVCs cannot be cross-namespace mounted, so a controller-side download
	// is invisible to inference Pods. Defer the fetch to the workload and
	// populate CacheKey so the init container can land the file at the
	// canonical /models/<cacheKey>/<basename> path the runtime expects.
	if isRemoteHTTPSource(model.Spec.Source) {
		return ctrl.Result{}, r.reconcileRuntimeResolvedSource(ctx, model, computeCacheKey(model.Spec.Source))
	}

	cacheKey := computeCacheKey(model.Spec.Source)
	modelDir := filepath.Join(r.StoragePath, cacheKey)
	// downloadPath is the path used during/after download. After GGUF metadata
	// parsing, the file is migrated to canonicalModelPath(modelDir, model).
	downloadPath := filepath.Join(modelDir, legacyModelFilename)

	// Early exit: if status is Ready, file exists on disk, AND it is already at
	// the canonical filename, skip the entire reconcile. Otherwise we fall through
	// so legacy "model.gguf" files get migrated to a metadata-derived basename.
	if model.Status.Phase == PhaseReady && model.Status.Path != "" {
		if _, err := os.Stat(model.Status.Path); err == nil {
			canonical := canonicalModelPath(filepath.Dir(model.Status.Path), model)
			if model.Status.Path == canonical {
				logger.Info("Model already Ready and cached at canonical path, skipping reconcile", "path", model.Status.Path)
				llmkubemetrics.ReconcileTotal.WithLabelValues("model", "success").Inc()
				return ctrl.Result{}, nil
			}
			logger.Info("Model needs filename migration", "currentPath", model.Status.Path, "canonicalPath", canonical)
		} else {
			logger.Info("Model marked Ready but file missing, will re-download", "path", model.Status.Path)
		}
	}

	logger.Info("Using cache key for model", "cacheKey", cacheKey, "dir", modelDir)

	if existingPath, fileInfo, ok := findCachedModelFile(modelDir); ok {
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
		model.Status.AcceleratorReady = r.checkAcceleratorAvailability(model.Spec.Hardware)
		now := metav1.Now()
		model.Status.LastUpdated = &now

		if err := r.updateStatus(ctx, model, "Available", metav1.ConditionTrue, "ModelCached", "Model found in cache"); err != nil {
			return ctrl.Result{}, err
		}

		llmkubemetrics.ModelStatus.WithLabelValues(model.Name, model.Namespace, "Cached").Set(1)
		llmkubemetrics.ReconcileTotal.WithLabelValues("model", "success").Inc()
		return ctrl.Result{}, nil
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
		if err := r.updateStatus(ctx, model, "Progressing", metav1.ConditionTrue, progressReason, progressMessage); err != nil {
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
	model.Status.AcceleratorReady = r.checkAcceleratorAvailability(model.Spec.Hardware)
	now := metav1.Now()
	model.Status.LastUpdated = &now

	if err := r.updateStatus(ctx, model, "Available", metav1.ConditionTrue, "ModelReady", "Model downloaded and cached"); err != nil {
		return ctrl.Result{}, err
	}

	llmkubemetrics.ModelStatus.WithLabelValues(model.Name, model.Namespace, "Ready").Set(1)
	llmkubemetrics.ReconcileTotal.WithLabelValues("model", "success").Inc()
	logger.Info("Model ready and cached", "path", finalPath, "size", model.Status.Size, "cacheKey", cacheKey)
	return ctrl.Result{}, nil
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
		if statusErr := r.updateStatus(ctx, model, "Progressing", metav1.ConditionTrue, "PVCNotBound", msg); statusErr != nil {
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
	model.Status.AcceleratorReady = r.checkAcceleratorAvailability(model.Spec.Hardware)
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
func (r *ModelReconciler) reconcileRuntimeResolvedSource(ctx context.Context, model *inferencev1alpha1.Model, cacheKey string) error {
	logger := log.FromContext(ctx)

	// Early exit if already Ready
	if model.Status.Phase == PhaseReady {
		logger.Info("Runtime-resolved model already Ready, skipping reconcile")
		llmkubemetrics.ReconcileTotal.WithLabelValues("model", "success").Inc()
		return nil
	}

	isMetal := model.Spec.Hardware != nil && model.Spec.Hardware.Accelerator == "metal"

	reason := "RuntimeResolved"
	message := "Source is runtime-resolved (e.g., HuggingFace repo ID); runtime will fetch at startup"
	if cacheKey != "" {
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

	model.Status.Phase = PhaseReady
	model.Status.Path = ""
	model.Status.CacheKey = cacheKey
	model.Status.Size = "0"
	model.Status.AcceleratorReady = r.checkAcceleratorAvailability(model.Spec.Hardware)
	now := metav1.Now()
	model.Status.LastUpdated = &now

	if err := r.updateStatus(ctx, model, "Available", metav1.ConditionTrue, reason, message); err != nil {
		return err
	}

	llmkubemetrics.ModelStatus.WithLabelValues(model.Name, model.Namespace, "ready").Set(1)
	llmkubemetrics.ReconcileTotal.WithLabelValues("model", "success").Inc()
	logger.Info("Runtime-resolved model ready", "source", model.Spec.Source)
	return nil
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

//nolint:unparam
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

//nolint:unparam
func (r *ModelReconciler) checkAcceleratorAvailability(hardware *inferencev1alpha1.HardwareSpec) bool {
	if hardware == nil {
		return true
	}
	// TODO: implement actual GPU/Metal availability checking
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
