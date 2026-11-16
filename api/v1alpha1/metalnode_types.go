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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MetalNode condition types.
const (
	// MetalNodeConditionAgentConnected is True while the agent keeps renewing
	// its Lease and the report on it is valid.
	MetalNodeConditionAgentConnected = "AgentConnected"
	// MetalNodeConditionReachable is True when a verifier pod in the cluster
	// completed a TLS handshake with the agent's ingress at status.hostIP,
	// with a server whose certificate is the one the agent reported, names
	// this MetalNode's UID and carries the reported SPKI pin.
	MetalNodeConditionReachable = "Reachable"
	// MetalNodeConditionAgentVersionSupported is True when the agent's
	// version is inside the supported skew window of the controller's: the
	// same minor release, and not newer than the controller. False names the
	// side to upgrade; Unknown means either version is not a release version
	// (a development build) or the agent has not reported one.
	MetalNodeConditionAgentVersionSupported = "AgentVersionSupported"
)

// MetalNode condition reasons.
const (
	MetalNodeReasonHeartbeatFresh    = "HeartbeatFresh"
	MetalNodeReasonHeartbeatStale    = "HeartbeatStale"
	MetalNodeReasonLeaseNotFound     = "LeaseNotFound"
	MetalNodeReasonReportInvalid     = "ReportInvalid"
	MetalNodeReasonNotVerified       = "NotVerified"
	MetalNodeReasonDialBackSucceeded = "DialBackSucceeded"
	MetalNodeReasonNoCandidates      = "NoCandidates"
	MetalNodeReasonNoneReachable     = "NoneReachable"
	MetalNodeReasonPinMissing        = "PinMissing"
	MetalNodeReasonVerifierFailed    = "VerifierFailed"
	MetalNodeReasonAgentNotConnected = "AgentNotConnected"
	MetalNodeReasonInvalidSpec       = "InvalidSpec"
	// MetalNodeReasonVerificationPending: the pin, port or verified address
	// changed, a spec edit changed the check (spec.verifier or spec.hostIP),
	// or the last success is too old to stand for the current address, so a
	// new check is due.
	MetalNodeReasonVerificationPending = "VerificationPending"
	// MetalNodeReasonAgentUpgradeRequired: the agent reports an ingress pin
	// but no ingress certificate. A pin alone does not prove the address is
	// this Mac, so nothing is verified until the agent is upgraded.
	MetalNodeReasonAgentUpgradeRequired = "AgentUpgradeRequired"
	// MetalNodeReasonCertificateUnbound: the agent's ingress certificate does
	// not name this MetalNode's UID (yet), so no server can pass the check.
	MetalNodeReasonCertificateUnbound = "CertificateUnbound"
	// MetalNodeReasonVersionInSkewWindow: the agent's version is inside the
	// supported skew window.
	MetalNodeReasonVersionInSkewWindow = "InSkewWindow"
	// MetalNodeReasonAgentNewerThanController: the agent is a newer release
	// than the controller. Upgrade the chart first, then the agents.
	MetalNodeReasonAgentNewerThanController = "AgentNewerThanController"
	// MetalNodeReasonAgentMinorBehind: the agent is from an older minor
	// release than the controller. Upgrade the agent.
	MetalNodeReasonAgentMinorBehind = "AgentMinorBehind"
	// MetalNodeReasonVersionUnknown: the controller or the agent version is
	// not a release version, or the agent has not reported one.
	MetalNodeReasonVersionUnknown = "VersionUnknown"
)

// LabelMetalNode names the MetalNode an object belongs to. The controller
// sets it on its verifier pods. On the agent's Lease it is what lets the
// controller watch the Lease: the controller's Lease informer only sees Leases
// that carry this label (any value), so a Lease without it is still read, but
// only on the controller's periodic resync.
const LabelMetalNode = "inference.llmkube.dev/metal-node"

// MetalNodeSpec is written by the cluster admin (llmkube metal token create).
// The agent has no write access to it.
type MetalNodeSpec struct {
	// ServiceAccountName is the ServiceAccount the Mac's agent authenticates
	// as. Deleting that ServiceAccount revokes every token the agent holds.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	ServiceAccountName string `json:"serviceAccountName"`

	// LeaseName is the coordination.k8s.io Lease, in this namespace, that the
	// agent renews as its heartbeat and annotates with its report.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	LeaseName string `json:"leaseName"`

	// HostIP is an address the admin knows reaches this Mac. It is tried
	// before the agent's own candidates and is still verified by the
	// dial-back; it is never trusted on its own.
	// The controller validates it (Reachable=False, reason InvalidSpec, when
	// it is not an IP address) rather than a CEL rule, whose isIP() would
	// need Kubernetes 1.31.
	// +optional
	// +kubebuilder:validation:MaxLength=45
	HostIP string `json:"hostIP,omitempty"`

	// Verifier places the dial-back verifier pod. Point it at the nodes the
	// relay pods for this Mac run on, so the check tests the network path
	// the relay will use.
	// +optional
	Verifier *MetalNodeVerifier `json:"verifier,omitempty"`
}

// MetalNodeVerifier is the placement of the dial-back verifier pod.
type MetalNodeVerifier struct {
	// NodeSelector is copied to the verifier pod.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations are copied to the verifier pod.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// MetalNodeCapacity is the hardware the agent reports. All of it is the Mac's
// own claim.
type MetalNodeCapacity struct {
	// MemoryBytes is the Mac's unified memory in bytes.
	// +optional
	MemoryBytes int64 `json:"memoryBytes,omitempty"`

	// Memory is MemoryBytes as a quantity (for example 128Gi), for display.
	// +optional
	Memory string `json:"memory,omitempty"`

	// Chip is the CPU brand string (for example "Apple M5 Max").
	// +optional
	Chip string `json:"chip,omitempty"`

	// GPUCores is the GPU core count.
	// +optional
	GPUCores int32 `json:"gpuCores,omitempty"`

	// MetalVersion is the Metal family the GPU supports (for example "Metal 4").
	// +optional
	MetalVersion string `json:"metalVersion,omitempty"`
}

// MetalNodeCandidateResult is the dial-back outcome for one address.
// +kubebuilder:validation:Enum=Pending;Reachable;Timeout;Refused;PinMismatch;CertificateMismatch;NotAllowed;Error
type MetalNodeCandidateResult string

const (
	// CandidatePending has not been checked against the current pin and
	// candidate list yet.
	CandidatePending MetalNodeCandidateResult = "Pending"
	// CandidateReachable completed a TLS handshake with the agent's reported
	// certificate, which names this MetalNode, against the pinned key.
	CandidateReachable MetalNodeCandidateResult = "Reachable"
	// CandidateTimeout did not answer within the probe timeout.
	CandidateTimeout MetalNodeCandidateResult = "Timeout"
	// CandidateRefused refused the connection or had no route.
	CandidateRefused MetalNodeCandidateResult = "Refused"
	// CandidatePinMismatch answered TLS with a key other than the pinned one.
	CandidatePinMismatch MetalNodeCandidateResult = "PinMismatch"
	// CandidateCertificateMismatch answered TLS with a certificate that is
	// not the agent's reported one or does not name this MetalNode: another
	// server at that address, or one whose certificate is outside its
	// validity period on the verifier's clock.
	CandidateCertificateMismatch MetalNodeCandidateResult = "CertificateMismatch"
	// CandidateNotAllowed is outside the controller's allowed CIDRs and was
	// never dialed.
	CandidateNotAllowed MetalNodeCandidateResult = "NotAllowed"
	// CandidateError failed in any other way.
	CandidateError MetalNodeCandidateResult = "Error"
)

// MetalNodeCandidate is one address and what the last dial-back found there.
type MetalNodeCandidate struct {
	// Address is an IP address.
	Address string `json:"address"`

	// Source says where the address came from: override (spec.hostIP or the
	// agent's --host-ip), tailnet, lan or docker-host.
	// +optional
	Source string `json:"source,omitempty"`

	// Result is the dial-back outcome.
	Result MetalNodeCandidateResult `json:"result"`

	// CheckedAt is when the verifier finished the check that produced Result.
	// +optional
	CheckedAt *metav1.Time `json:"checkedAt,omitempty"`
}

// MetalNodeVerification is the controller's bookkeeping for the dial-back.
type MetalNodeVerification struct {
	// InputHash identifies the pin, ingress certificate, port, candidate
	// list and verifier placement the last completed check ran against. A
	// change starts a new check.
	// +optional
	InputHash string `json:"inputHash,omitempty"`

	// LastCheckTime is when the last completed check finished.
	// +optional
	LastCheckTime *metav1.Time `json:"lastCheckTime,omitempty"`

	// ConsecutiveFailures counts checks in a row that verified no address;
	// it sets the retry backoff.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`

	// LastStartTime is when the controller last started a verifier pod.
	// Starts are at least 30 seconds apart, whatever the Mac reports.
	// +optional
	LastStartTime *metav1.Time `json:"lastStartTime,omitempty"`

	// PodUID is the UID of the verifier pod the controller last created.
	// Only that pod's results are read.
	// +optional
	PodUID string `json:"podUID,omitempty"`
}

// MetalNodeStatus is written only by the controller.
type MetalNodeStatus struct {
	// AgentVersion is the version the agent reports.
	// +optional
	AgentVersion string `json:"agentVersion,omitempty"`

	// Capacity is the hardware the agent reports.
	// +optional
	Capacity *MetalNodeCapacity `json:"capacity,omitempty"`

	// IngressPort is the agent's TLS ingress port.
	// +optional
	IngressPort int32 `json:"ingressPort,omitempty"`

	// IngressSPKI is the base64 SHA-256 digest of the agent ingress
	// certificate's public key, as the agent reports it. The dial-back and
	// the relay complete TLS only against this key; the dial-back also
	// requires the certificate the agent reports, which must name this
	// MetalNode's UID.
	// +optional
	IngressSPKI string `json:"ingressSPKI,omitempty"`

	// Candidates are the addresses tried, in preference order, with the last
	// dial-back result for each.
	// +optional
	// +kubebuilder:validation:MaxItems=17
	// +listType=atomic
	Candidates []MetalNodeCandidate `json:"candidates,omitempty"`

	// HostIP is the first candidate the dial-back verified. It is set only
	// while Reachable is True.
	// +optional
	HostIP string `json:"hostIP,omitempty"`

	// LastHeartbeatTime is the Lease's renewTime, accurate to about a
	// minute: it advances when AgentConnected changes or when it is more
	// than 60 seconds behind. The Lease holds the exact time.
	// +optional
	LastHeartbeatTime *metav1.Time `json:"lastHeartbeatTime,omitempty"`

	// Verification is the dial-back bookkeeping.
	// +optional
	Verification *MetalNodeVerification `json:"verification,omitempty"`

	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: AgentConnected and Reachable.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=metalnodes,shortName=mn
// +kubebuilder:printcolumn:name="Host IP",type=string,JSONPath=`.status.hostIP`
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.status.capacity.memory`
// +kubebuilder:printcolumn:name="Chip",type=string,JSONPath=`.status.capacity.chip`
// +kubebuilder:printcolumn:name="Reachable",type=string,JSONPath=`.status.conditions[?(@.type=="Reachable")].status`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.agentVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MetalNode is one Mac running the metal-agent, joined to the cluster. The
// admin creates it (with its ServiceAccount and Lease); the agent reports
// through the Lease; the controller fills status and verifies the Mac is
// reachable with a dial-back.
type MetalNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MetalNodeSpec   `json:"spec"`
	Status MetalNodeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MetalNodeList contains a list of MetalNode.
type MetalNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MetalNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MetalNode{}, &MetalNodeList{})
}
