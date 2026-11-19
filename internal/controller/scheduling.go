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
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Scheduling and priority logic. This file owns:
//   - Phase determination from Deployment/pod state (incl. WaitingForGPU)
//   - Pending-pod inspection to surface Insufficient-GPU diagnostics
//   - FIFO queue-position calculation across WaitingForGPU services
//   - Priority → PriorityClass and preemption-value mapping

// PhaseWaitingForGPU is hoisted to api/v1alpha1; aliased here for
// unqualified use within the controller package.
const PhaseWaitingForGPU = inferencev1alpha1.PhaseWaitingForGPU

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

// agentRefusalReasons are the SchedulingStatus values the metal agent writes
// and clears itself: its refusals to start a service (memory admission in
// pkg/agent/agent.go, start refusals in pkg/agent/refusal.go) and StartFailed,
// for a start that failed and is being retried on the restart backoff
// (pkg/agent/start_failure.go). The agent clears them itself once an engine
// runs (#777).
//
// A stale or unparseable slice heartbeat does not replace them while the
// Mac's MetalNode does not say HeartbeatStale (#2457). A refusal
// re-stamps only the slice the agent registers itself, so any other slice
// listed for the service (another name, no port, no managed-by label, or the
// legacy slice of an agent since switched to relay mode) stays stale while the
// agent is alive, and replacing the refusal had the two rewriting status in a
// loop. The MetalNode is found by the ingress pin the relay uses. In relay
// mode with no MetalNode reporting the pin, the slice heartbeat still replaces
// the refusal: nothing else shows the Mac is alive, and the agent re-stamps
// its own "<isvc>-agent" slice, so a loop there needs a slice it does not own.
// Legacy mode has no pin and nothing else ties a service to a Mac, so there
// the refusal stays.
// That changes only the message: phase, ready replicas and the slice
// withdrawal are the same either way, and a dead agent's refusal reads less
// precisely than AgentHeartbeatStale.
//
// Built from inferencev1alpha1.MetalAgentRefusalReasons rather than
// hand-listed here: this map and the agent's own list used to be two
// independently maintained copies of the same set, and they drifted
// (ServiceNameTooLong was added to the agent in 0.10.0 without a matching
// entry here, so determinePhase overwrote that refusal with
// "WaitingForMetalAgent" on every reconcile and the agent re-refused on every
// poll). A shared source in api/v1alpha1 makes that drift a compile-time
// impossibility.
var agentRefusalReasons = buildAgentRefusalReasons()

func buildAgentRefusalReasons() map[string]bool {
	reasons := make(map[string]bool, len(inferencev1alpha1.MetalAgentRefusalReasons))
	for _, r := range inferencev1alpha1.MetalAgentRefusalReasons {
		reasons[r] = true
	}
	return reasons
}

// SchedulingInfo contains information about pod scheduling status
type SchedulingInfo struct {
	Status     string
	Message    string
	WaitingFor string
}

func (r *InferenceServiceReconciler) determinePhase(ctx context.Context, isvc *inferencev1alpha1.InferenceService, readyReplicas, desiredReplicas int32, isMetal bool, deployment *appsv1.Deployment, snap *metalSnapshot) (string, *SchedulingInfo) {
	if isvc.Spec.Suspend {
		return PhaseSuspended, nil
	}

	log := logf.FromContext(ctx)

	if readyReplicas == desiredReplicas && readyReplicas > 0 {
		if isMetal {
			clearControllerMetalScheduling(isvc)
		}
		return PhaseReady, nil
	}
	if readyReplicas > 0 {
		return "Progressing", nil
	}
	if desiredReplicas == 0 && readyReplicas == 0 {
		return PhaseStopped, nil
	}
	if !isMetal && deployment != nil {
		schedulingInfo, err := r.getPodSchedulingInfo(ctx, isvc)
		if err != nil {
			log.Error(err, "Failed to get pod scheduling info")
		}
		if schedulingInfo != nil {
			switch schedulingInfo.Status {
			case "InsufficientGPU":
				return PhaseWaitingForGPU, schedulingInfo
			case "UnbindableModelCache":
				// Phase stays Creating: the deployment is healthy and nothing
				// about the InferenceService is wrong, so calling it Failed
				// would misattribute a storage fault and change how rollout
				// treats it. The diagnosis rides the message instead, which is
				// the part that was missing -- this branch previously fell
				// through and dropped it.
				return PhaseCreating, schedulingInfo
			}
		}
	}
	if isMetal {
		// A refused service never registers an endpoint, so it always lands
		// here. Overwriting the agent's refusal hid it from status and bumped
		// the resourceVersion, which the agent's poll saw as a change and
		// refused again every few seconds. A stale heartbeat overwrites it only
		// when no MetalNode shows the Mac is alive; see agentRefusalReasons.
		if agentRefusalReasons[isvc.Status.SchedulingStatus] && (snap == nil || !snap.refusalReplaced) {
			return PhaseCreating, nil
		}
		if snap != nil {
			switch snap.Kind {
			case metalHBStale:
				return PhaseCreating, &SchedulingInfo{
					Status:  "AgentHeartbeatStale",
					Message: fmt.Sprintf("metal-agent heartbeat stale (last seen %s); host may be offline", snap.RawHeartbeat),
				}
			case metalHBUnparseable:
				return PhaseCreating, &SchedulingInfo{
					Status:  "AgentHeartbeatStale",
					Message: fmt.Sprintf("metal-agent heartbeat unparseable (value %q); host may be offline", snap.RawHeartbeat),
				}
			}
		}
		return PhaseCreating, &SchedulingInfo{
			Status:  "WaitingForMetalAgent",
			Message: "Waiting for the host metal-agent to fetch the model and register Endpoints",
		}
	}
	return PhaseCreating, nil
}

// clearControllerMetalScheduling drops the metal scheduling diagnosis this
// controller writes (WaitingForMetalAgent, AgentHeartbeatStale) once the agent
// has registered and the service is Ready. Reasons the agent writes (memory
// admission, start refusals) are cleared by the agent itself (#777) and are
// deliberately preserved. The deployment path clears its own markers in the
// reconcile write path; see the !isMetal && phase == PhaseReady block in
// inferenceservice_controller.go.
func clearControllerMetalScheduling(isvc *inferencev1alpha1.InferenceService) {
	switch isvc.Status.SchedulingStatus {
	case "WaitingForMetalAgent", "AgentHeartbeatStale":
		isvc.Status.SchedulingStatus = ""
		isvc.Status.SchedulingMessage = ""
		isvc.Status.WaitingFor = ""
	}
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

				if gpuResourceName, ok := detectInsufficientGPUResource(condition.Message); ok {
					info.Status = "InsufficientGPU"
					info.WaitingFor = fmt.Sprintf("%s: %d", gpuResourceName, podGPULimit(&pod, gpuResourceName))
				} else if pvName, nodeName, ok := r.classifyUnbindableVolume(ctx, &pod, condition.Message); ok {
					// Checked before the generic "Insufficient" branch: this
					// message contains neither that word nor anything else that
					// hints at storage, so it would otherwise reach the user as
					// the scheduler's raw text, which blames node affinity and
					// sends you looking at the scheduler. See #1509.
					info.Status = "UnbindableModelCache"
					info.Message = unbindableVolumeMessage(pvName, nodeName, podModelCacheClaim(&pod))
				} else if strings.Contains(condition.Message, "Insufficient") {
					info.Status = "InsufficientResources"
				}

				return info, nil
			}
		}
	}

	return nil, nil
}

// evaluateGPUQueue returns isvc's 1-based position in the cluster-wide FIFO GPU
// queue, and the number of services waiting for GPU in every namespace that
// holds an InferenceService. Position is 0 when isvc is not waiting.
//
// isvc carries the authoritative phase and creation time: the cache-backed
// listing still reports the previous phase on the reconcile that changes it,
// and may not list a freshly created object at all.
func (r *InferenceServiceReconciler) evaluateGPUQueue(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
) (int32, map[string]int32, error) {
	allServices := &inferencev1alpha1.InferenceServiceList{}
	if err := r.List(ctx, allServices); err != nil {
		return 0, nil, err
	}

	// Every namespace holding an InferenceService is keyed, queued or not, so a
	// namespace that drains to 0 reports 0 instead of losing its series.
	depths := map[string]int32{isvc.Namespace: 0}

	var ahead int32
	for i := range allServices.Items {
		svc := &allServices.Items[i]
		if svc.Name == isvc.Name && svc.Namespace == isvc.Namespace {
			continue // counted once below, from isvc's own phase
		}
		depth := depths[svc.Namespace]
		if svc.Status.Phase == PhaseWaitingForGPU {
			depth++
			// CreationTimestamp is 1s resolution, so a Helm release or a
			// scripted loop can create several services within the same
			// second; break the tie by name so positions stay a strict
			// 1..N permutation instead of colliding.
			if svc.CreationTimestamp.Before(&isvc.CreationTimestamp) ||
				(svc.CreationTimestamp.Equal(&isvc.CreationTimestamp) &&
					(svc.Namespace < isvc.Namespace ||
						(svc.Namespace == isvc.Namespace && svc.Name < isvc.Name))) {
				ahead++
			}
		}
		depths[svc.Namespace] = depth
	}

	if isvc.Status.Phase != PhaseWaitingForGPU {
		return 0, depths, nil
	}
	depths[isvc.Namespace]++

	return ahead + 1, depths, nil
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
