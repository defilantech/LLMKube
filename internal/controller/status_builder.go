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
//   - VLLMSpecValid condition maintenance (informational)
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
	isvc.Status.ModelReady = modelReady
	isvc.Status.ReadyReplicas = readyReplicas
	isvc.Status.DesiredReplicas = desiredReplicas
	isvc.Status.Endpoint = endpoint
	isvc.Status.LastUpdated = &now

	isvc.Status.EffectivePriority = r.resolveEffectivePriority(isvc)

	// Update phase gauge metric
	llmkubemetrics.InferenceServicePhase.WithLabelValues(isvc.Name, isvc.Namespace, phase).Set(1)
	if previousPhase != "" && previousPhase != phase {
		llmkubemetrics.InferenceServicePhase.WithLabelValues(isvc.Name, isvc.Namespace, previousPhase).Set(0)
	}

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
	} else {
		isvc.Status.SchedulingStatus = ""
		isvc.Status.SchedulingMessage = ""
		isvc.Status.WaitingFor = ""
	}

	if phase == PhaseWaitingForGPU {
		queuePos, err := r.calculateQueuePosition(ctx, isvc)
		if err != nil {
			log.Error(err, "Failed to calculate queue position")
		}
		isvc.Status.QueuePosition = queuePos
		llmkubemetrics.GPUQueueDepth.Set(float64(queuePos))
	} else {
		isvc.Status.QueuePosition = 0
	}

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

	if err := r.Status().Update(ctx, isvc); err != nil {
		log.Error(err, "Failed to update InferenceService status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}
