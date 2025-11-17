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
		r.StoragePath = "/tmp/llmkube/models"
	}

	// Ensure storage directory exists
	if err := os.MkdirAll(r.StoragePath, 0755); err != nil {
		logger.Error(err, "Failed to create storage directory")
		return ctrl.Result{}, err
	}

	// Check if model is already downloaded
	if model.Status.Phase == PhaseReady && model.Status.Path != "" {
		if _, err := os.Stat(model.Status.Path); err == nil {
			logger.Info("Model already downloaded and ready", "path", model.Status.Path)
			return ctrl.Result{}, nil
		}
	}

	// Update phase to Downloading if not set
	if model.Status.Phase == "" || model.Status.Phase == "Pending" {
		model.Status.Phase = "Downloading"
		if err := r.updateStatus(ctx, model, "Progressing", metav1.ConditionTrue, "DownloadStarted", "Started downloading model"); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Started downloading model", "source", model.Spec.Source)
	}

	// Download the model
	modelPath := filepath.Join(r.StoragePath, fmt.Sprintf("%s-%s.gguf", req.Namespace, req.Name))
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
	model.Status.AcceleratorReady = r.checkAcceleratorAvailability(model.Spec.Hardware)
	now := metav1.Now()
	model.Status.LastUpdated = &now

	if err := r.updateStatus(ctx, model, "Available", metav1.ConditionTrue, "ModelReady", "Model downloaded and ready"); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Model ready", "path", modelPath, "size", model.Status.Size)
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

// SetupWithManager sets up the controller with the Manager.
func (r *ModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.Model{}).
		Named("model").
		Complete(r)
}
