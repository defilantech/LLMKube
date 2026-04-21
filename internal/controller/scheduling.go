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
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Scheduling and priority logic. This file owns:
//   - Phase determination from Deployment/pod state (incl. WaitingForGPU)
//   - Pending-pod inspection to surface Insufficient-GPU diagnostics
//   - FIFO queue-position calculation across WaitingForGPU services
//   - Priority → PriorityClass and preemption-value mapping

const PhaseWaitingForGPU = "WaitingForGPU"

// Priority class mapping from priority level to Kubernetes PriorityClass name
var priorityClassMap = map[string]string{
	"critical": "llmkube-critical",
	"high":     "llmkube-high",
	"normal":   "llmkube-normal",
	"low":      "llmkube-low",
	"batch":    "llmkube-batch",
}

// Priority values corresponding to each level
var priorityValues = map[string]int32{
	"critical": 1000000,
	"high":     100000,
	"normal":   10000,
	"low":      1000,
	"batch":    100,
}

// SchedulingInfo contains information about pod scheduling status
type SchedulingInfo struct {
	Status     string
	Message    string
	WaitingFor string
}

func (r *InferenceServiceReconciler) determinePhase(ctx context.Context, isvc *inferencev1alpha1.InferenceService, readyReplicas, desiredReplicas int32, isMetal bool, deployment *appsv1.Deployment) (string, *SchedulingInfo) {
	log := logf.FromContext(ctx)

	if readyReplicas == desiredReplicas && readyReplicas > 0 {
		return PhaseReady, nil
	}
	if readyReplicas > 0 {
		return "Progressing", nil
	}
	if !isMetal && deployment != nil {
		schedulingInfo, err := r.getPodSchedulingInfo(ctx, isvc)
		if err != nil {
			log.Error(err, "Failed to get pod scheduling info")
		}
		if schedulingInfo != nil && schedulingInfo.Status == "InsufficientGPU" {
			return PhaseWaitingForGPU, schedulingInfo
		}
	}
	return "Creating", nil
}

func (r *InferenceServiceReconciler) getPodSchedulingInfo(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (*SchedulingInfo, error) {
	podList := &corev1.PodList{}
	labels := client.MatchingLabels{
		"app":                           isvc.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}
	if err := r.List(ctx, podList, client.InNamespace(isvc.Namespace), labels); err != nil {
		return nil, err
	}

	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodPending {
			continue
		}

		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
				info := &SchedulingInfo{
					Status:  condition.Reason,
					Message: condition.Message,
				}

				if strings.Contains(condition.Message, "Insufficient nvidia.com/gpu") {
					info.Status = "InsufficientGPU"
					gpuCount := int32(0)
					if isvc.Spec.Resources != nil && isvc.Spec.Resources.GPU > 0 {
						gpuCount = isvc.Spec.Resources.GPU
					}
					info.WaitingFor = fmt.Sprintf("nvidia.com/gpu: %d", gpuCount)
				} else if strings.Contains(condition.Message, "Insufficient") {
					info.Status = "InsufficientResources"
				}

				return info, nil
			}
		}
	}

	return nil, nil
}

func (r *InferenceServiceReconciler) calculateQueuePosition(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (int32, error) {
	if isvc.Status.Phase != PhaseWaitingForGPU {
		return 0, nil
	}

	allServices := &inferencev1alpha1.InferenceServiceList{}
	if err := r.List(ctx, allServices); err != nil {
		return 0, err
	}

	type queueEntry struct {
		name      string
		namespace string
		created   metav1.Time
	}

	var waitingServices []queueEntry
	for _, svc := range allServices.Items {
		if svc.Status.Phase == PhaseWaitingForGPU {
			waitingServices = append(waitingServices, queueEntry{
				name:      svc.Name,
				namespace: svc.Namespace,
				created:   svc.CreationTimestamp,
			})
		}
	}

	// Sort by creation timestamp (FIFO)
	for i := 0; i < len(waitingServices)-1; i++ {
		for j := i + 1; j < len(waitingServices); j++ {
			if waitingServices[j].created.Before(&waitingServices[i].created) {
				waitingServices[i], waitingServices[j] = waitingServices[j], waitingServices[i]
			}
		}
	}

	for pos, entry := range waitingServices {
		if entry.name == isvc.Name && entry.namespace == isvc.Namespace {
			return int32(pos + 1), nil //nolint:gosec // G115: queue index bounded by waitingServices size
		}
	}

	return 0, nil
}

func (r *InferenceServiceReconciler) resolvePriorityClassName(isvc *inferencev1alpha1.InferenceService) string {
	if isvc.Spec.PriorityClassName != "" {
		return isvc.Spec.PriorityClassName
	}

	priority := isvc.Spec.Priority
	if priority == "" {
		priority = "normal"
	}

	if className, ok := priorityClassMap[priority]; ok {
		return className
	}

	return "llmkube-normal"
}

func (r *InferenceServiceReconciler) resolveEffectivePriority(isvc *inferencev1alpha1.InferenceService) int32 {
	priority := isvc.Spec.Priority
	if priority == "" {
		priority = "normal"
	}

	if value, ok := priorityValues[priority]; ok {
		return value
	}

	return priorityValues["normal"]
}
