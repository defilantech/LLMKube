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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ModelSpec defines the desired state of Model
type ModelSpec struct {
	// Source defines where to obtain the model file (GGUF format)
	// Supported schemes: http://, https://, file://, or absolute paths
	// Examples:
	//   - https://huggingface.co/org/repo/resolve/main/model.gguf
	//   - file:///mnt/models/model.gguf
	//   - /mnt/models/model.gguf (air-gapped deployments)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(https?|file)://.*\.gguf$|^/.*\.gguf$`
	Source string `json:"source"`

	// Format specifies the model file format (currently only GGUF is supported)
	// +kubebuilder:validation:Enum=gguf
	// +kubebuilder:default=gguf
	// +optional
	Format string `json:"format,omitempty"`

	// Quantization describes the quantization level (e.g., Q4_0, Q5_K_M, F16)
	// +optional
	Quantization string `json:"quantization,omitempty"`

	// Hardware specifies hardware acceleration preferences
	// +optional
	Hardware *HardwareSpec `json:"hardware,omitempty"`

	// Resources defines resource requirements for running the model
	// +optional
	Resources *ResourceRequirements `json:"resources,omitempty"`
}

// HardwareSpec defines hardware acceleration settings
type HardwareSpec struct {
	// Accelerator specifies the type of hardware acceleration
	// +kubebuilder:validation:Enum=cpu;metal;cuda;rocm
	// +kubebuilder:default=cpu
	// +optional
	Accelerator string `json:"accelerator,omitempty"`

	// GPU specifies GPU device requirements
	// +optional
	GPU *GPUSpec `json:"gpu,omitempty"`
}

// GPUSpec defines GPU-specific requirements
type GPUSpec struct {
	// Enabled indicates whether GPU acceleration is enabled
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Count specifies the number of GPUs required
	// Supports multi-GPU for model sharding (future feature)
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=8
	// +optional
	Count int32 `json:"count,omitempty"`

	// Memory specifies minimum GPU memory required per GPU (e.g., "8Gi", "16Gi")
	// +optional
	Memory string `json:"memory,omitempty"`

	// Vendor specifies GPU vendor preference (nvidia, amd, intel)
	// Future-proof for multi-vendor support
	// +kubebuilder:validation:Enum=nvidia;amd;intel
	// +kubebuilder:default=nvidia
	// +optional
	Vendor string `json:"vendor,omitempty"`

	// Layers specifies layer offloading configuration for multi-GPU
	// Format: number of layers to offload to GPU (e.g., 32 for full offload on 7B model)
	// -1 means auto-detect optimal layer split
	// +kubebuilder:validation:Minimum=-1
	// +optional
	Layers int32 `json:"layers,omitempty"`

	// Sharding defines how to shard the model across multiple GPUs
	// Only applicable when Count > 1
	// +optional
	Sharding *GPUShardingSpec `json:"sharding,omitempty"`
}

// GPUShardingSpec defines multi-GPU sharding strategy
type GPUShardingSpec struct {
	// Strategy defines the sharding approach
	// - "layer": Shard by transformer layers (default)
	// - "tensor": Tensor parallelism (future)
	// - "pipeline": Pipeline parallelism (future)
	// +kubebuilder:validation:Enum=layer;tensor;pipeline
	// +kubebuilder:default=layer
	// +optional
	Strategy string `json:"strategy,omitempty"`

	// LayerSplit defines custom layer splits per GPU
	// Example: [0-15, 16-31] for 2-GPU split of 32-layer model
	// If empty, auto-calculate even split
	// +optional
	LayerSplit []string `json:"layerSplit,omitempty"`
}

// ResourceRequirements defines compute resource requirements
type ResourceRequirements struct {
	// CPU specifies CPU requirements (e.g., "2" or "2000m")
	// +optional
	CPU string `json:"cpu,omitempty"`

	// Memory specifies memory requirements (e.g., "4Gi")
	// +optional
	Memory string `json:"memory,omitempty"`
}

// GGUFMetadata contains metadata extracted from a parsed GGUF file header.
type GGUFMetadata struct {
	// Architecture is the model architecture (e.g., "llama", "mistral", "phi")
	// +optional
	Architecture string `json:"architecture,omitempty"`

	// ModelName is the model name as stored in the GGUF file
	// +optional
	ModelName string `json:"modelName,omitempty"`

	// Quantization is the quantization type (e.g., "Q4_K_M", "Q5_K_M")
	// +optional
	Quantization string `json:"quantization,omitempty"`

	// ContextLength is the maximum context length (tokens)
	// +optional
	ContextLength uint64 `json:"contextLength,omitempty"`

	// EmbeddingSize is the embedding dimension size
	// +optional
	EmbeddingSize uint64 `json:"embeddingSize,omitempty"`

	// LayerCount is the number of transformer layers/blocks
	// +optional
	LayerCount uint64 `json:"layerCount,omitempty"`

	// HeadCount is the number of attention heads
	// +optional
	HeadCount uint64 `json:"headCount,omitempty"`

	// TensorCount is the number of tensors in the model
	// +optional
	TensorCount uint64 `json:"tensorCount,omitempty"`

	// FileVersion is the GGUF file format version
	// +optional
	FileVersion uint32 `json:"fileVersion,omitempty"`
}

// ModelStatus defines the observed state of Model.
type ModelStatus struct {
	// Phase represents the current lifecycle phase of the model
	// Possible values: Pending, Downloading, Ready, Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Size represents the size of the downloaded model file
	// +optional
	Size string `json:"size,omitempty"`

	// Path represents the local path where the model is stored
	// +optional
	Path string `json:"path,omitempty"`

	// CacheKey is the SHA256 hash prefix of the source URL used for cache storage
	// Models with the same source URL share the same cache entry
	// +optional
	CacheKey string `json:"cacheKey,omitempty"`

	// AcceleratorReady indicates if hardware acceleration is configured and ready
	// +optional
	AcceleratorReady bool `json:"acceleratorReady,omitempty"`

	// GGUF contains metadata extracted from the GGUF file header
	// +optional
	GGUF *GGUFMetadata `json:"gguf,omitempty"`

	// LastUpdated is the timestamp of the last status update
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// conditions represent the current state of the Model resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the model is downloaded and ready for use
	// - "Progressing": the model is being downloaded or processed
	// - "Degraded": the model download or setup failed
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.status.size`
// +kubebuilder:printcolumn:name="Accelerator",type=string,JSONPath=`.spec.hardware.accelerator`
// +kubebuilder:printcolumn:name="Arch",type=string,JSONPath=`.status.gguf.architecture`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:shortName=mdl

// Model is the Schema for the models API
type Model struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Model
	// +required
	Spec ModelSpec `json:"spec"`

	// status defines the observed state of Model
	// +optional
	Status ModelStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ModelList contains a list of Model
type ModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Model `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Model{}, &ModelList{})
}
