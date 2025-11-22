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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// InferenceServiceSpec defines the desired state of InferenceService
type InferenceServiceSpec struct {
	// ModelRef references the Model CR that contains the model to serve
	// +kubebuilder:validation:Required
	ModelRef string `json:"modelRef"`

	// Replicas is the desired number of inference pods
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Image is the container image for the llama.cpp runtime
	// +kubebuilder:default="ghcr.io/ggerganov/llama.cpp:server"
	// +optional
	Image string `json:"image,omitempty"`

	// Endpoint defines the service endpoint configuration
	// +optional
	Endpoint *EndpointSpec `json:"endpoint,omitempty"`

	// Resources defines compute resources for inference pods
	// +optional
	Resources *InferenceResourceRequirements `json:"resources,omitempty"`

	// Tolerations for pod scheduling (e.g., GPU taints, spot instances)
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// NodeSelector for pod placement (e.g., specific node pools)
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// EndpointSpec defines the service endpoint configuration
type EndpointSpec struct {
	// Port is the service port
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8080
	// +optional
	Port int32 `json:"port,omitempty"`

	// Path is the HTTP path for the inference endpoint
	// +kubebuilder:default="/v1/chat/completions"
	// +optional
	Path string `json:"path,omitempty"`

	// Type is the Kubernetes service type (ClusterIP, NodePort, LoadBalancer)
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +kubebuilder:default=ClusterIP
	// +optional
	Type string `json:"type,omitempty"`
}

// InferenceResourceRequirements defines resource requirements for inference
type InferenceResourceRequirements struct {
	// GPU count required per pod
	// For multi-GPU inference, each pod gets this many GPUs
	// Note: Multi-GPU sharding config comes from Model CRD
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=8
	// +optional
	GPU int32 `json:"gpu,omitempty"`

	// CPU requests (e.g., "2" or "2000m")
	// +optional
	CPU string `json:"cpu,omitempty"`

	// Memory requests (e.g., "4Gi")
	// +optional
	Memory string `json:"memory,omitempty"`

	// GPUMemory specifies GPU memory limit per pod (e.g., "16Gi")
	// Used for scheduling and validation
	// +optional
	GPUMemory string `json:"gpuMemory,omitempty"`
}

// InferenceServiceStatus defines the observed state of InferenceService.
type InferenceServiceStatus struct {
	// Phase represents the current lifecycle phase (Pending, Creating, Ready, Failed)
	// +optional
	Phase string `json:"phase,omitempty"`

	// Replicas tracks the number of ready vs desired pods
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// DesiredReplicas is the desired number of replicas
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// Endpoint is the service URL where inference requests can be sent
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// ModelReady indicates if the referenced Model is in Ready state
	// +optional
	ModelReady bool `json:"modelReady,omitempty"`

	// LastUpdated is the timestamp of the last status update
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// conditions represent the current state of the InferenceService resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=isvc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.modelRef`
// +kubebuilder:printcolumn:name="Replicas",type=string,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// InferenceService is the Schema for the inferenceservices API
type InferenceService struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of InferenceService
	// +required
	Spec InferenceServiceSpec `json:"spec"`

	// status defines the observed state of InferenceService
	// +optional
	Status InferenceServiceStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// InferenceServiceList contains a list of InferenceService
type InferenceServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InferenceService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InferenceService{}, &InferenceServiceList{})
}
