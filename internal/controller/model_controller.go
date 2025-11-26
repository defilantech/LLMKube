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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	// PhaseReady indicates the model is downloaded and ready
	PhaseReady = "Ready"
	// PhaseCached indicates the model was found in cache (no download needed)
	PhaseCached = "Cached"
	// DefaultModelCachePath is the default path for model cache
	DefaultModelCachePath = "/models"
)

// ModelReconciler reconciles a Model object
type ModelReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	StoragePath string // Base path for storing downloaded models
}

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling Model", "name", req.Name, "namespace", req.Namespace)

	// Fetch the Model instance
	model := &inferencev1alpha1.Model{}
	if err := r.Get(ctx, req.NamespacedName, model); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Model resource not found, ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get Model")
		return ctrl.Result{}, err
	}

	// Set default storage path if not configured
	if r.StoragePath == "" {
		r.StoragePath = DefaultModelCachePath
	}

	// Compute cache key and model path
	cacheKey := computeCacheKey(model.Spec.Source)
	modelDir := filepath.Join(r.StoragePath, cacheKey)
	modelPath := filepath.Join(modelDir, "model.gguf")

	logger.Info("Using cache key for model", "cacheKey", cacheKey, "path", modelPath)

	// Check if model already exists in cache
	if fileInfo, err := os.Stat(modelPath); err == nil {
		// Model exists in cache - update status and return
		logger.Info("Model found in cache, skipping download", "path", modelPath, "size", fileInfo.Size())

		model.Status.Phase = PhaseReady
		model.Status.Path = modelPath
		model.Status.Size = formatBytes(fileInfo.Size())
		model.Status.CacheKey = cacheKey
		model.Status.AcceleratorReady = r.checkAcceleratorAvailability(model.Spec.Hardware)
		now := metav1.Now()
		model.Status.LastUpdated = &now

		if err := r.updateStatus(ctx, model, "Available", metav1.ConditionTrue, "ModelCached", "Model found in cache"); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		logger.Error(err, "Failed to create cache directory", "path", modelDir)
		return ctrl.Result{}, err
	}

	// Update phase to Downloading
	if model.Status.Phase != "Downloading" {
		model.Status.Phase = "Downloading"
		model.Status.CacheKey = cacheKey
		if err := r.updateStatus(ctx, model, "Progressing", metav1.ConditionTrue, "DownloadStarted", "Started downloading model"); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Started downloading model", "source", model.Spec.Source, "cacheKey", cacheKey)
	}

	// Download the model to cache
	size, err := r.downloadModel(ctx, model.Spec.Source, modelPath)
	if err != nil {
		logger.Error(err, "Failed to download model")
		model.Status.Phase = "Failed"
		if statusErr := r.updateStatus(ctx, model, "Degraded", metav1.ConditionTrue, "DownloadFailed", err.Error()); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after download failure")
		}
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, err
	}

	// Update status to Ready
	model.Status.Phase = PhaseReady
	model.Status.Path = modelPath
	model.Status.Size = formatBytes(size)
	model.Status.CacheKey = cacheKey
	model.Status.AcceleratorReady = r.checkAcceleratorAvailability(model.Spec.Hardware)
	now := metav1.Now()
	model.Status.LastUpdated = &now

	if err := r.updateStatus(ctx, model, "Available", metav1.ConditionTrue, "ModelReady", "Model downloaded and cached"); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Model ready and cached", "path", modelPath, "size", model.Status.Size, "cacheKey", cacheKey)
	return ctrl.Result{}, nil
}

// downloadModel downloads a model from the given source URL to the destination path
func (r *ModelReconciler) downloadModel(ctx context.Context, source, dest string) (int64, error) {
	logger := log.FromContext(ctx)

	// Create the file
	out, err := os.Create(dest)
	if err != nil {
		return 0, fmt.Errorf("failed to create file: %w", err)
	}
	defer func() {
		if closeErr := out.Close(); closeErr != nil {
			logger.Error(closeErr, "Failed to close file")
		}
	}()

	// Download the file
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

	// Write to file
	size, err := io.Copy(out, resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to write file: %w", err)
	}

	return size, nil
}

// updateStatus updates the Model status with the given condition
//
//nolint:unparam // status parameter kept for future use with different condition statuses
func (r *ModelReconciler) updateStatus(ctx context.Context, model *inferencev1alpha1.Model, condType string, status metav1.ConditionStatus, reason, message string) error {
	// Set or update condition
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: model.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}

	// Find and update existing condition or append new one
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

// checkAcceleratorAvailability checks if the requested hardware accelerator is available
//
//nolint:unparam // Returns true for MVP; will implement actual checking in production
func (r *ModelReconciler) checkAcceleratorAvailability(hardware *inferencev1alpha1.HardwareSpec) bool {
	if hardware == nil {
		return true // CPU is always available
	}

	// For MVP, we'll just return true for any accelerator
	// In production, this would check for actual GPU/Metal availability
	return true
}

// formatBytes formats bytes into human-readable format
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

// computeCacheKey generates a SHA256 hash of the source URL to use as cache key
func computeCacheKey(source string) string {
	hash := sha256.Sum256([]byte(source))
	return hex.EncodeToString(hash[:])[:16] // Use first 16 chars for readability
}

// SetupWithManager sets up the controller with the Manager.
func (r *ModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.Model{}).
		Named("model").
		Complete(r)
}
