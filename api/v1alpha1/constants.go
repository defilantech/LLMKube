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

	// Metal relay wire protocol (S4). The controller runs a relay pod per
	// Metal InferenceService; the relay authenticates to the metal-agent's
	// TLS ingress on the Mac, which forwards to the engine on loopback.

	// AnnotationAgentIngressSPKI is set by the metal-agent on its
	// "<isvc>-agent" EndpointSlice: the base64 (std) SHA-256 of the ingress
	// certificate's SubjectPublicKeyInfo. The relay accepts only a server
	// certificate matching it.
	AnnotationAgentIngressSPKI = "llmkube.ai/agent-ingress-spki"

	// AnnotationAgentEnginePort is set by the metal-agent on its
	// "<isvc>-agent" EndpointSlice in relay mode: the engine's own port on
	// the agent's loopback interface (127.0.0.1). The "<isvc>" Service and
	// slice front the relay pod once the controller adopts them, so their
	// port is the relay's listener, not the engine's; an off-cluster
	// foreman-agent using --inference-base-url-host-override reads this
	// annotation to find the engine directly on the same host instead.
	// Loopback-only and not sensitive. Absent on EndpointSlices written by
	// older agents that predate relay mode.
	AnnotationAgentEnginePort = "llmkube.ai/agent-engine-port"

	// HeaderRelayToken carries the per-namespace relay token from the relay
	// to the ingress. It is not Authorization, so a client's own
	// Authorization header passes through untouched.
	//
	//nolint:gosec // G101: header name, not a credential value
	HeaderRelayToken = "X-LLMKube-Relay-Token"
	// HeaderRelayTarget names the target InferenceService as "<ns>/<name>".
	HeaderRelayTarget = "X-LLMKube-Target"

	// MetalRelaySecretName is the per-namespace Secret holding the relay
	// token under MetalRelaySecretKey (64 hex chars).
	//
	//nolint:gosec // G101: Secret object name, not a credential value
	MetalRelaySecretName = "llmkube-metal-relay"
	MetalRelaySecretKey  = "token"

	// MetalAgentServiceSuffix names the agent-written Service and
	// EndpointSlice ("<isvc>-agent") that point at the ingress.
	MetalAgentServiceSuffix = "-agent"
	// MetalRelayDeploymentSuffix names the controller-owned relay Deployment.
	MetalRelayDeploymentSuffix = "-relay"
	// MetalAgentServicePort is the port of the "<isvc>-agent" Service.
	MetalAgentServicePort int32 = 8443

	// IngressReadyPath is answered by the ingress itself (never proxied):
	// 200 when the target named in HeaderRelayTarget is running on this
	// agent.
	IngressReadyPath = "/_llmkube/ready"

	// LabelManagedBy marks objects by writer.
	LabelManagedBy      = "llmkube.ai/managed-by"
	ManagedByMetalAgent = "metal-agent"
	ManagedByController = "llmkube-controller"

	// AnnotationIdleEndpoint lets operators declare a custom HTTP path that
	// returns 2xx when a replica is idle. Used by the generic runtime to opt
	// in to drain-before-roll. Set on InferenceService metadata.annotations.
	AnnotationIdleEndpoint = "inference.llmkube.dev/idle-endpoint"

	// AnnotationIdleMetric names a Prometheus gauge exposed on the generic
	// runtime's metrics endpoint whose sum reports in-flight work (for example
	// vLLM's vllm:num_requests_running or SGLang's sglang:num_running_reqs).
	// When set, the generic idle probe scrapes the metrics path and reports the
	// member idle when the gauge sum is at or below AnnotationIdleMetricThreshold,
	// giving a custom-image server the same precise drain the native runtimes
	// get instead of a 2xx health check that is always "idle". Takes precedence
	// over AnnotationIdleEndpoint. Set on InferenceService metadata.annotations.
	AnnotationIdleMetric = "inference.llmkube.dev/idle-metric"

	// AnnotationIdleMetricThreshold is the gauge sum at or below which the
	// member counts as idle for AnnotationIdleMetric. Parsed as a float;
	// defaults to 0 when unset or unparseable.
	AnnotationIdleMetricThreshold = "inference.llmkube.dev/idle-metric-threshold"

	// AnnotationIdleMetricPath overrides the HTTP path scraped for
	// AnnotationIdleMetric. Defaults to /metrics.
	AnnotationIdleMetricPath = "inference.llmkube.dev/idle-metric-path"

	// DefaultAgentHeartbeatInterval is how often the metal-agent re-asserts
	// its registrations (which also self-heals any missed update, #657).
	DefaultAgentHeartbeatInterval = 30 * time.Second

	// DefaultAgentHeartbeatTimeout is how stale a heartbeat may be before the
	// controller stops counting the registration as ready (6 intervals).
	DefaultAgentHeartbeatTimeout = 3 * time.Minute
)

// InferenceService spec.runtime values served only by the metal-agent on
// Apple Silicon hosts (#525). They have no in-cluster backend, so the
// controller refuses them on a Model whose hardware.accelerator is not
// "metal". Shared here because the metal-agent keys its executors on the same
// strings and pkg/agent cannot import internal/controller. The values are
// CRD-visible (spec.runtime enum) and must not change without an API bump.
const (
	RuntimeMLXServer  = "mlx-server"
	RuntimeOMLX       = "omlx"
	RuntimeOllama     = "ollama"
	RuntimeVLLMSwift  = "vllm-swift"
	RuntimeTensorFold = "tensorfold"
)

// IsMetalOnlyRuntime reports whether runtime is served only by the
// metal-agent. The empty runtime is not: it means llama.cpp in-cluster and the
// agent's --runtime flag on a Mac.
func IsMetalOnlyRuntime(runtime string) bool {
	switch runtime {
	case RuntimeMLXServer, RuntimeOMLX, RuntimeOllama, RuntimeVLLMSwift, RuntimeTensorFold:
		return true
	default:
		return false
	}
}

// InferenceService and Model lifecycle phase strings written to
// status.phase by both the operator (internal/controller) and the
// metal-agent (pkg/agent). Hoisted here so a rename on either side is a
// compile error rather than a silent status-thrash bug: the two writers
// previously disagreed on the phase string for a suspended service (#1254)
// because pkg/agent could not import internal/controller and carried its
// own copy. The string values are CRD- and client-visible and must not
// change without a corresponding API bump.
const (
	// PhaseReady means the workload is serving (InferenceService) or the
	// model is cached and available (Model).
	PhaseReady = "Ready"
	// PhaseFailed means the workload or model could not be brought up.
	PhaseFailed = "Failed"
	// PhaseCached means the model file is present in the cache but not
	// yet referenced by a ready InferenceService.
	PhaseCached = "Cached"
	// PhaseDownloading means the model is being fetched or copied into
	// the cache.
	PhaseDownloading = "Downloading"
	// PhaseCreating means the InferenceService is being provisioned
	// (deployment/service creation, or waiting for the metal-agent).
	PhaseCreating = "Creating"
	// PhaseStopped means the InferenceService has been scaled to zero
	// (spec.replicas=0) and the workload torn down.
	PhaseStopped = "Stopped"
	// PhaseSuspended means the InferenceService has spec.suspend=true and
	// the workload torn down while preserving spec.replicas for resume.
	PhaseSuspended = "Suspended"
	// PhaseWaitingForGPU means the InferenceService is queued waiting for
	// GPU resources to become available.
	PhaseWaitingForGPU = "WaitingForGPU"
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

	// ReasonIdleCheckUnsupported is set when the runtime backend does not
	// implement IdleDetector, so drain-before-roll cannot probe idleness.
	ReasonIdleCheckUnsupported string = "IdleCheckUnsupported"

	// ReasonPodsCrashLooping is set when RolloutDeferred=True because some
	// old-generation pods are crashlooping (not Ready) while others are Ready
	// and serving. The rollout is deferred to protect in-flight work on the
	// Ready pods; when ALL old pods are unready the rollout proceeds instead
	// (no work to protect).
	ReasonPodsCrashLooping string = "PodsCrashLooping"

	// DefaultIdleCheckInterval is how often the controller re-checks pod
	// idleness when waiting for idle before rollout.
	DefaultIdleCheckInterval = 5 * time.Second
)

// Metal-agent refusal reasons: the Status.SchedulingStatus (and matching
// Kubernetes Event reason) values the metal-agent writes when it declines to
// start a service on a Metal InferenceService. These are exported here, and
// pkg/agent's EventReason constants equal them, so both the agent (which
// writes them) and the controller's determinePhase (internal/controller/
// scheduling.go, which must not overwrite a refusal with "WaitingForMetalAgent"
// on the next reconcile) share one list instead of two hand-maintained ones
// that silently drift out of sync. They did once: ServiceNameTooLong was
// added to the agent in 0.10.0 without a matching controller-side entry, so
// the controller cleared the refusal every poll and the agent re-refused
// every poll, in a loop.
const (
	// ReasonInsufficientMemory is set when a Model does not fit the host's
	// memory budget (memory admission, pkg/agent/agent.go).
	ReasonInsufficientMemory string = "InsufficientMemory"

	// ReasonMemoryCheckFailed is set when the memory admission check itself
	// could not complete (resolution or estimation failure) and the agent
	// fails closed rather than starting an unchecked process.
	ReasonMemoryCheckFailed string = "MemoryCheckFailed"

	// ReasonEndpointNameConflict is set when the "<isvc>[-agent]" Service or
	// EndpointSlice name the agent would register is already owned by another
	// object.
	ReasonEndpointNameConflict string = "EndpointNameConflict"

	// ReasonModelSourceNotAllowed is set when a Model is Failed, or its
	// source (or pagedSSDCacheDir) resolves outside the agent's allowed
	// filesystem roots.
	ReasonModelSourceNotAllowed string = "ModelSourceNotAllowed"

	// ReasonExtraArgsRejected is set when spec.extraArgs fails the runtime's
	// extra-args policy (an unrecognized or dangerous flag).
	ReasonExtraArgsRejected string = "ExtraArgsRejected"

	// ReasonServiceNameTooLong is set when relay mode's "-agent"-suffixed
	// Service name would exceed the DNS label length limit.
	ReasonServiceNameTooLong string = "ServiceNameTooLong"

	// ReasonModelDigestMismatch is set when a downloaded model's SHA256 does
	// not match Model.spec.sha256.
	ReasonModelDigestMismatch string = "ModelDigestMismatch"

	// ReasonModelDownloadTooLarge is set when a model download exceeds the
	// agent's --max-download-size cap, by its declared size or by the bytes
	// it streamed.
	ReasonModelDownloadTooLarge string = "ModelDownloadTooLarge"
)

// ReasonModelAttestationRejected is set on a Model the attestation policy
// refuses: its Socair attestation is missing, does not verify against the
// trusted keys, is for another digest, or is not admitted by the policy.
const ReasonModelAttestationRejected = "ModelAttestationRejected"

// MetalAgentRefusalReasons lists every SchedulingStatus value the metal agent
// writes when it refuses to start a service. internal/controller/scheduling.go
// builds its agentRefusalReasons set from this list rather than maintaining a
// second, independent copy; pkg/agent/refusal.go and pkg/agent/pressure.go's
// EventReason constants for these reasons are defined equal to the constants
// above, and a pkg/agent test asserts every reason actually used with
// refuseStart or the memory refusal path appears here.
var MetalAgentRefusalReasons = []string{
	ReasonInsufficientMemory,
	ReasonMemoryCheckFailed,
	ReasonEndpointNameConflict,
	ReasonModelSourceNotAllowed,
	ReasonExtraArgsRejected,
	ReasonServiceNameTooLong,
	ReasonModelDigestMismatch,
	ReasonModelDownloadTooLarge,
}

// ReasonRelaySecretNotManaged is the Status.SchedulingStatus (and Warning
// Event reason) the controller writes on a Metal InferenceService in relay
// mode when the namespace's llmkube-metal-relay Secret exists without the
// controller's managed-by label. The controller did not create that Secret,
// so it neither uses it nor creates or updates the relay Deployment until the
// Secret is deleted (#1957). It is controller-written, so it is deliberately
// not in MetalAgentRefusalReasons, which lists only agent refusals.
const ReasonRelaySecretNotManaged string = "RelaySecretNotManaged"

// ReasonDeploymentNameTaken is the Status.SchedulingStatus (and Warning Event
// reason) the controller writes when a Deployment already holds the name the
// InferenceService generates and this service does not control it. The
// controller refuses to overwrite, adopt, or delete that Deployment (#2011).
// It is controller-written, so it is deliberately not in
// MetalAgentRefusalReasons, which lists only agent refusals.
const ReasonDeploymentNameTaken string = "DeploymentNameTaken"
