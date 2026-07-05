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

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FleetNodeHeartbeatTimeout is how long the controller waits past the most
// recent heartbeat before it marks a FleetNode NotReady. The FleetAgent
// heartbeats every 30s, so 90s tolerates two missed beats before a node is
// declared dead.
const FleetNodeHeartbeatTimeout = 90 * time.Second

// FleetNodeDrainReapTimeout is how long a node may stay Draining without a
// heartbeat before the controller deletes it. A live agent keeps heartbeating
// while it finishes its in-flight task, so this only fires for a node whose
// agent set Draining and then went away (rollout, scale-down, crash) — which
// otherwise leaks a FleetNode per restart, since the reconciler leaves
// Draining nodes alone and there is no ownerReference for garbage collection.
// Set well above the heartbeat timeout so a genuinely draining agent is never
// reaped out from under an in-flight task.
const FleetNodeDrainReapTimeout = 10 * time.Minute

// FleetNodePhase is the heartbeat-driven health state of a fleet worker.
// +kubebuilder:validation:Enum=Ready;Draining;NotReady;Unknown
type FleetNodePhase string

const (
	FleetNodePhaseReady    FleetNodePhase = "Ready"
	FleetNodePhaseDraining FleetNodePhase = "Draining"
	FleetNodePhaseNotReady FleetNodePhase = "NotReady"
	FleetNodePhaseUnknown  FleetNodePhase = "Unknown"
)

// FleetNodeAccelerator names the accelerator family the node hosts.
// "vulkan" is the AMD/Vulkan tier (e.g. Strix Halo gfx1151).
// +kubebuilder:validation:Enum=metal;cuda;vulkan;none
type FleetNodeAccelerator string

// FleetNodeCapability is what the FleetAgent advertises about its host so the
// scheduler can match incoming AgenticTasks to nodes that can serve them.
type FleetNodeCapability struct {
	// Accelerator names the accelerator family available on this node.
	// +optional
	Accelerator FleetNodeAccelerator `json:"accelerator,omitempty"`

	// TotalRAMGB is the physical RAM in GiB.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TotalRAMGB int32 `json:"totalRAMGB,omitempty"`

	// AvailableRAMGB is the FleetAgent's estimate of RAM not currently
	// committed to a running model or task. Refreshed on heartbeat.
	// +kubebuilder:validation:Minimum=0
	// +optional
	AvailableRAMGB int32 `json:"availableRAMGB,omitempty"`

	// InstalledModels is the list of Model CR names this node has locally
	// available (the model files are present and the runtime can load them).
	// +optional
	InstalledModels []string `json:"installedModels,omitempty"`

	// MaxContextTokens is the largest context window the loaded model
	// supports. Used by the scheduler to filter tasks with high
	// MinContextTokens requirements.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxContextTokens int32 `json:"maxContextTokens,omitempty"`

	// TokensPerSecond is a coarse decode throughput estimate. v0.1 takes
	// this from configuration; v0.2 will benchmark on heartbeat.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TokensPerSecond int32 `json:"tokensPerSecond,omitempty"`
}

// FleetNodeSpec is the small bit of identity the FleetAgent owns on its own
// FleetNode object. Most of the resource's interesting fields live in Status,
// which the FleetAgent updates on every heartbeat.
type FleetNodeSpec struct {
	// NodeName is the human-readable identity of the worker. Conventionally
	// matches metadata.name; required for the scheduler to address it.
	// +kubebuilder:validation:Required
	NodeName string `json:"nodeName"`

	// TailscaleAddr is the Tailscale address (IP or MagicDNS name) the
	// FleetAgent listens on. Optional; the operator does not connect to the
	// agent directly in v0.1 (dispatch is via the agent's CRD watch).
	// +optional
	TailscaleAddr string `json:"tailscaleAddr,omitempty"`

	// Roles label the node for capability-aware scheduling beyond raw
	// accelerator type. Conventionally one or more of: "worker", "verifier".
	// +optional
	Roles []string `json:"roles,omitempty"`
}

// FleetNodeAgentKind identifies which agent binary manages the FleetNode.
// +kubebuilder:validation:Enum=foreman-agent;metal-agent
type FleetNodeAgentKind string

// FleetNodeUpdateRequest is written by the AgentReleaseReconciler to
// signal the node's agent that a self-update is available. The agent
// reads this on each heartbeat (PR4) and performs the update; the
// reconciler clears the field once the node reports the target version.
type FleetNodeUpdateRequest struct {
	// TargetVersion is the version string the node should update to.
	// +kubebuilder:validation:Required
	TargetVersion string `json:"targetVersion"`

	// URL is the download location for the agent binary artifact.
	// +kubebuilder:validation:Required
	URL string `json:"url"`

	// SHA256 is the lowercase hex-encoded SHA-256 digest of the artifact.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	SHA256 string `json:"sha256"`
}

// FleetNodeStatus is the FleetAgent's live view of its host. Updated on
// every heartbeat (every 30s); the FleetNodeReconciler marks the phase
// NotReady when the heartbeat goes stale.
type FleetNodeStatus struct {
	// Phase is the heartbeat-driven health state. The scheduler treats
	// only Ready nodes as eligible.
	// +optional
	Phase FleetNodePhase `json:"phase,omitempty"`

	// LastHeartbeatTime is the most recent heartbeat the FleetAgent
	// successfully patched. The reconciler marks the phase NotReady if
	// this stalls (default threshold: 90 seconds).
	// +optional
	LastHeartbeatTime *metav1.Time `json:"lastHeartbeatTime,omitempty"`

	// Capability is what this node advertises to the scheduler.
	// +optional
	Capability FleetNodeCapability `json:"capability,omitempty"`

	// CurrentTask is the namespaced name of the AgenticTask the agent is
	// running, or empty if idle. The scheduler skips nodes with a non-empty
	// CurrentTask (v0.1 concurrency is one task per node).
	// +optional
	CurrentTask string `json:"currentTask,omitempty"`

	// AgentVersion is the observed running version of the agent, reported
	// on heartbeat. Set by the foreman-agent using the binary's build-time
	// version string; empty for agents that predate this field.
	// +optional
	AgentVersion string `json:"agentVersion,omitempty"`

	// AgentKind identifies which agent binary manages this FleetNode.
	// metal-agent does not register a FleetNode in v0.1, but the enum
	// documents intent for future use.
	// +optional
	AgentKind FleetNodeAgentKind `json:"agentKind,omitempty"`

	// OS is the operating system reported by the agent on heartbeat
	// (runtime.GOOS). Used by the AgentReleaseReconciler to select the
	// correct platform artifact for this node.
	// +optional
	OS string `json:"os,omitempty"`

	// Arch is the CPU architecture reported by the agent on heartbeat
	// (runtime.GOARCH). Used alongside OS for platform artifact selection.
	// +optional
	Arch string `json:"arch,omitempty"`

	// UpdateRequest is written by the AgentReleaseReconciler when a new
	// agent version is ready for this node to install. The agent reads
	// this field on each heartbeat (PR4) and performs the self-update.
	// The reconciler clears the field once the node reports the target
	// version and passes the health gate.
	// +optional
	UpdateRequest *FleetNodeUpdateRequest `json:"updateRequest,omitempty"`

	// Conditions track standard health signals: Ready, Draining, etc.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=fn
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.agentVersion`
// +kubebuilder:printcolumn:name="Accelerator",type=string,JSONPath=`.status.capability.accelerator`
// +kubebuilder:printcolumn:name="RAM",type=integer,JSONPath=`.status.capability.availableRAMGB`
// +kubebuilder:printcolumn:name="Current Task",type=string,JSONPath=`.status.currentTask`
// +kubebuilder:printcolumn:name="Heartbeat",type=date,JSONPath=`.status.lastHeartbeatTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FleetNode is a worker the Foreman scheduler can dispatch tasks to. It is
// cluster-scoped because nodes are global to the fleet; the resource is
// owned and updated by the FleetAgent running on that node.
type FleetNode struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the small bit of identity the agent owns.
	Spec FleetNodeSpec `json:"spec"`

	// status is the agent's heartbeat-driven view of its host. The
	// scheduler reads it; the agent writes it.
	// +optional
	Status FleetNodeStatus `json:"status,omitempty"`
}

// HeartbeatStale reports whether the node's most recent heartbeat is older
// than FleetNodeHeartbeatTimeout relative to now. A node that has never
// heart-beat (nil LastHeartbeatTime) is treated as stale: there is no
// evidence the FleetAgent is alive.
func (n *FleetNode) HeartbeatStale(now time.Time) bool {
	if n == nil || n.Status.LastHeartbeatTime == nil {
		return true
	}
	return now.Sub(n.Status.LastHeartbeatTime.Time) > FleetNodeHeartbeatTimeout
}

// DrainReapable reports whether a Draining node has been silent long enough
// (past FleetNodeDrainReapTimeout, or never heart-beat) that its agent is gone
// and the drain will never complete, so the node may be deleted. The caller is
// responsible for checking that the node is actually in the Draining phase.
func (n *FleetNode) DrainReapable(now time.Time) bool {
	if n == nil {
		return false
	}
	if n.Status.LastHeartbeatTime == nil {
		return true
	}
	return now.Sub(n.Status.LastHeartbeatTime.Time) > FleetNodeDrainReapTimeout
}

// +kubebuilder:object:root=true

// FleetNodeList is a list of FleetNodes.
type FleetNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FleetNode{}, &FleetNodeList{})
}
