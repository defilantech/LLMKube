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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase values for FederatedClusterStatus.Phase. Datacenter-owned: computed
// by the datacenter controller from LastHeartbeatTime staleness relative to
// a multiple of HeartbeatIntervalSeconds.
const (
	// FederatedClusterConnected means a heartbeat was received within the
	// expected interval.
	FederatedClusterConnected = "Connected"

	// FederatedClusterStale means no heartbeat was received for 3x the
	// expected interval.
	FederatedClusterStale = "Stale"

	// FederatedClusterUnreachable means no heartbeat was received for 10x
	// the expected interval.
	FederatedClusterUnreachable = "Unreachable"
)

// FederatedClusterSpec is admin-owned. displayName and dataResidencyTier are
// set at registration; the edge and the datacenter controller only write status.
type FederatedClusterSpec struct {
	// DisplayName is a human label for the site.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// DataResidencyTier is recorded now and enforced later by the federation
	// router (#1237). It is a free-form tier label (for example "eu", "floor-3").
	// +optional
	DataResidencyTier string `json:"dataResidencyTier,omitempty"`

	// HeartbeatIntervalSeconds is how often the edge is expected to push status.
	// The datacenter derives staleness thresholds from it (3x Stale, 10x Unreachable).
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=5
	HeartbeatIntervalSeconds int32 `json:"heartbeatIntervalSeconds,omitempty"`
}

// ClusterCapacity is edge-written node/GPU capacity.
type ClusterCapacity struct {
	Nodes           int32 `json:"nodes"`
	GPUsTotal       int32 `json:"gpusTotal"`
	GPUsAllocatable int32 `json:"gpusAllocatable"`
}

// ClusterInferenceSummary is edge-written inference health.
type ClusterInferenceSummary struct {
	ServicesReady  int32 `json:"servicesReady"`
	ServicesFailed int32 `json:"servicesFailed"`
	ServicesTotal  int32 `json:"servicesTotal"`
	Models         int32 `json:"models"`
}

// FederatedClusterStatus has two writers. The edge writes everything except
// Phase, over an RBAC-scoped status-subresource client. The datacenter
// controller writes ONLY Phase, from LastHeartbeatTime staleness.
type FederatedClusterStatus struct {
	// Phase is datacenter-owned: Connected, Stale, or Unreachable.
	// +optional
	Phase string `json:"phase,omitempty"`

	// LastHeartbeatTime is edge-owned: set to now on every successful push.
	// +optional
	LastHeartbeatTime *metav1.Time `json:"lastHeartbeatTime,omitempty"`

	// ObservedVersion is edge-owned: the LLMKube/operator version on the site.
	// +optional
	ObservedVersion string `json:"observedVersion,omitempty"`

	// Capacity is edge-owned node/GPU capacity.
	// +optional
	Capacity *ClusterCapacity `json:"capacity,omitempty"`

	// Inference is edge-owned inference health.
	// +optional
	Inference *ClusterInferenceSummary `json:"inference,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=fedcluster;fc
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.dataResidencyTier`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Last Heartbeat",type=date,JSONPath=`.status.lastHeartbeatTime`
// +kubebuilder:printcolumn:name="GPUs",type=string,JSONPath=`.status.capacity.gpusAllocatable`

// FederatedCluster registers one edge site on the datacenter cluster.
type FederatedCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FederatedClusterSpec   `json:"spec,omitempty"`
	Status FederatedClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FederatedClusterList contains a list of FederatedCluster.
type FederatedClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FederatedCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FederatedCluster{}, &FederatedClusterList{})
}
