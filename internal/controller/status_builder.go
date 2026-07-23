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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

// Status-subresource writes. This file owns the path from an in-memory
// phase/condition decision to the actual Status().Update call, including:
//   - VLLMSpecValid and SGLangSpecValid condition maintenance (informational)
//   - cluster-local endpoint URL construction
//   - the omnibus updateStatusWithSchedulingInfo that writes phase,
//     replica counts, endpoint, scheduling diagnostics, priority,
//     queue position, and the Available/Progressing/Degraded/GPUAvailable
//     condition set

// reconcileVLLMSpecCondition sets or clears the VLLMSpecValid status condition
// based on ValidateVLLMConfig. This is informational only — it does not block
// Deployment creation. The controller's main Status().Update at the end of
// reconcile persists the condition.
func (r *InferenceServiceReconciler) reconcileVLLMSpecCondition(isvc *inferencev1alpha1.InferenceService) {
	if isvc.Spec.Runtime != RuntimeVLLM {
		meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionVLLMSpecValid)
		return
	}
	reason, message := ValidateVLLMConfig(isvc)
	now := metav1.NewTime(time.Now())
	if reason == "" {
		// Only emit a True condition when we previously set a False one; no
		// need to churn the status for services that have always been valid.
		if existing := meta.FindStatusCondition(isvc.Status.Conditions, ConditionVLLMSpecValid); existing != nil {
			meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
				Type:               ConditionVLLMSpecValid,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: isvc.Generation,
				LastTransitionTime: now,
				Reason:             "ConfigValid",
				Message:            "vLLM configuration is valid",
			})
		}
		return
	}
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               ConditionVLLMSpecValid,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: isvc.Generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})
}

// reconcileSGLangSpecCondition sets or clears the SGLangSpecValid status condition
// based on ValidateSGLangConfig. This is informational only — it does not block
// Deployment creation. The controller's main Status().Update at the end of
// reconcile persists the condition.
func (r *InferenceServiceReconciler) reconcileSGLangSpecCondition(isvc *inferencev1alpha1.InferenceService) {
	if isvc.Spec.Runtime != RuntimeSGLANG {
		meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionSGLangSpecValid)
		return
	}
	reason, message := ValidateSGLangConfig(isvc)
	now := metav1.NewTime(time.Now())
	if reason == "" {
		if existing := meta.FindStatusCondition(isvc.Status.Conditions, ConditionSGLangSpecValid); existing != nil {
			meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
				Type:               ConditionSGLangSpecValid,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: isvc.Generation,
				LastTransitionTime: now,
				Reason:             "ConfigValid",
				Message:            "SGLang configuration is valid",
			})
		}
		return
	}
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               ConditionSGLangSpecValid,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: isvc.Generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})
}

// setSuspendedCondition records whether the service is administratively
// suspended (spec.suspend). Uses meta.SetStatusCondition so external
// controllers' foreign condition types are never disturbed.
func setSuspendedCondition(isvc *inferencev1alpha1.InferenceService) {
	status := metav1.ConditionFalse
	reason := "NotSuspended"
	message := "Service is not suspended"
	if isvc.Spec.Suspend {
		status = metav1.ConditionTrue
		reason = "Suspended"
		message = "Service is suspended (spec.suspend=true); workload scaled to zero, spec.replicas preserved"
	}
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               "Suspended",
		Status:             status,
		ObservedGeneration: isvc.Generation,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}

func (r *InferenceServiceReconciler) constructEndpoint(isvc *inferencev1alpha1.InferenceService, svc *corev1.Service) string {
	port := int32(8080)
	path := "/v1/chat/completions"

	if isvc.Spec.Endpoint != nil {
		if isvc.Spec.Endpoint.Port > 0 {
			port = isvc.Spec.Endpoint.Port
		}
		if isvc.Spec.Endpoint.Path != "" {
			path = isvc.Spec.Endpoint.Path
		}
	}

	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d%s", svc.Name, svc.Namespace, port, path)
}

// publishInferenceServiceState exports the phase, replica and info series from
// stored status, so a reconcile that returns before the status update still
// reports what the service is. No-ops on an empty phase: nothing was observed
// (Get failed, or the object has no status yet). model is nil on the paths that
// never resolved one; the info series then falls back to cpu/llamacpp, which is
// what the call site did with a nil model.
func publishInferenceServiceState(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) {
	if isvc.Status.Phase == "" {
		return
	}

	accelerator := "cpu"
	if model != nil && model.Spec.Hardware != nil && model.Spec.Hardware.Accelerator != "" {
		accelerator = model.Spec.Hardware.Accelerator
	}
	runtime := isvc.Spec.Runtime
	if runtime == "" {
		runtime = "llamacpp"
	}

	llmkubemetrics.PublishInferenceServicePhase(isvc.Name, isvc.Namespace, isvc.Status.Phase)
	llmkubemetrics.PublishInferenceServiceReplicas(isvc.Name, isvc.Namespace, isvc.Status.ReadyReplicas, isvc.Status.DesiredReplicas)
	llmkubemetrics.PublishInferenceServiceInfo(isvc.Name, isvc.Namespace, accelerator, runtime)
}

// nolint:unparam // ctrl.Result is always zero but callers take &result to signal status-update path
func (r *InferenceServiceReconciler) updateStatusWithSchedulingInfo(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	phase string,
	modelReady bool,
	readyReplicas int32,
	desiredReplicas int32,
	endpoint string,
	errorMsg string,
	schedulingInfo *SchedulingInfo,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	now := metav1.Now()
	previousPhase := isvc.Status.Phase
	isvc.Status.Phase = phase
	isvc.Status.Mode = resolveServingMode(isvc)
	isvc.Status.ModelReady = modelReady
	isvc.Status.ReadyReplicas = readyReplicas
	isvc.Status.Replicas = desiredReplicas
	isvc.Status.DesiredReplicas = desiredReplicas
	isvc.Status.Endpoint = endpoint
	isvc.Status.LastUpdated = &now

	isvc.Status.EffectivePriority = r.resolveEffectivePriority(isvc)

	// The phase, replica and info series are published by Reconcile's defer
	// from the status written above, not here: this call site is unreachable
	// on any pass that errors earlier.

	// Track time-to-ready using creation timestamp
	if phase == PhaseReady && previousPhase != PhaseReady {
		readyDuration := time.Since(isvc.CreationTimestamp.Time).Seconds()
		llmkubemetrics.InferenceServiceReadyDuration.WithLabelValues(isvc.Name, isvc.Namespace).Observe(readyDuration)
		llmkubemetrics.ReconcileTotal.WithLabelValues("inferenceservice", "success").Inc()
	}

	if schedulingInfo != nil {
		isvc.Status.SchedulingStatus = schedulingInfo.Status
		isvc.Status.SchedulingMessage = schedulingInfo.Message
		isvc.Status.WaitingFor = schedulingInfo.WaitingFor
	}
	// When schedulingInfo is nil, preserve agent-written scheduling fields
	// (e.g. InsufficientMemory, MemoryCheckFailed) so the controller does not
	// clobber them on its next status update (#643).

	// The depths cover every namespace, so each status write refreshes the whole
	// gauge rather than only this service's series.
	queuePos, queueDepths, err := r.evaluateGPUQueue(ctx, isvc)
	if err != nil {
		log.Error(err, "Failed to evaluate GPU queue")
	} else {
		llmkubemetrics.PublishGPUQueueDepth(queueDepths)
	}
	isvc.Status.QueuePosition = queuePos

	var condition metav1.Condition
	switch phase {
	case PhaseReady:
		condition = metav1.Condition{
			Type:               "Available",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             "InferenceReady",
			Message:            "Inference service is ready and serving requests",
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, condition)
		meta.RemoveStatusCondition(&isvc.Status.Conditions, "Progressing")
		meta.RemoveStatusCondition(&isvc.Status.Conditions, "Degraded")
		meta.RemoveStatusCondition(&isvc.Status.Conditions, "GPUAvailable")

	case "Progressing", "Creating":
		condition = metav1.Condition{
			Type:               "Progressing",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             "Creating",
			Message:            fmt.Sprintf("Creating inference service (%d/%d replicas ready)", readyReplicas, desiredReplicas),
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, condition)

	case PhaseWaitingForGPU:
		condition = metav1.Condition{
			Type:               "GPUAvailable",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             "InsufficientGPU",
			Message:            fmt.Sprintf("Waiting for GPU resources: %s", isvc.Status.WaitingFor),
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, condition)

		progressCondition := metav1.Condition{
			Type:               "Progressing",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             "WaitingForGPU",
			Message:            fmt.Sprintf("Queued at position %d waiting for GPU", isvc.Status.QueuePosition),
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, progressCondition)

	case PhaseFailed:
		condition = metav1.Condition{
			Type:               ConditionDegraded,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             PhaseFailed,
			Message:            errorMsg,
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, condition)
		meta.RemoveStatusCondition(&isvc.Status.Conditions, "Available")

	case "Pending":
		condition = metav1.Condition{
			Type:               "Progressing",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             "Pending",
			Message:            errorMsg,
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, condition)
	}

	// Set unconditionally (not gated on phase) so the Suspended condition
	// flips to False on unsuspend rather than lingering True from a prior
	// suspended pass.
	setSuspendedCondition(isvc)

	if err := r.Status().Update(ctx, isvc); err != nil {
		log.Error(err, "Failed to update InferenceService status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}
