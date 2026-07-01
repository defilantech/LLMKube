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

package v1alpha1

import "time"

const (
	// AnnotationAgentHeartbeat is stamped (RFC3339) on agent-managed Endpoints
	// every heartbeat interval. The InferenceService controller treats
	// registrations whose heartbeat is older than DefaultAgentHeartbeatTimeout
	// as not ready (issue #663). Endpoints without the annotation (older
	// agents) are exempt from expiry for backward compatibility.
	AnnotationAgentHeartbeat = "llmkube.ai/agent-heartbeat"

	// AnnotationAgentVersion is stamped on agent-managed Endpoints with the
	// running version of the metal-agent binary (e.g. "v0.8.4"). Set on every
	// RegisterEndpoint call so the cluster can observe which version is
	// managing a given InferenceService. Absent on Endpoints created by older
	// agents that predate this annotation.
	AnnotationAgentVersion = "llmkube.ai/agent-version"

	// DefaultAgentHeartbeatInterval is how often the metal-agent re-asserts
	// its registrations (which also self-heals any missed update, #657).
	DefaultAgentHeartbeatInterval = 30 * time.Second

	// DefaultAgentHeartbeatTimeout is how stale a heartbeat may be before the
	// controller stops counting the registration as ready (6 intervals).
	DefaultAgentHeartbeatTimeout = 3 * time.Minute
)

const (
	// ConditionRolloutDeferred indicates whether a rollout is being deferred
	// because the InferenceService has waitForIdle enabled and pods are not yet
	// idle. When True, the Deployment pod-template update is held until all
	// backend slots report idle or the idleTimeoutSeconds expires.
	ConditionRolloutDeferred string = "RolloutDeferred"

	// ReasonPodsBusy is set when RolloutDeferred=True because one or more
	// backend slots are currently processing requests.
	ReasonPodsBusy string = "PodsBusy"

	// ReasonIdleCheckFailed is set when RolloutDeferred=True because the
	// controller could not determine idleness (e.g. /slots unreachable,
	// non-200, or the backend was started with --no-slots). The rollout is
	// still deferred (fail-closed) until the idleTimeoutSeconds budget is
	// spent.
	ReasonIdleCheckFailed string = "IdleCheckFailed"

	// ReasonIdleTimeoutExceeded is set when RolloutDeferred=False after the
	// idle timeout expired and the rollout proceeded despite busy pods.
	ReasonIdleTimeoutExceeded string = "IdleTimeoutExceeded"

	// DefaultIdleCheckInterval is how often the controller re-checks pod
	// idleness when waiting for idle before rollout.
	DefaultIdleCheckInterval = 5 * time.Second
)
