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

// ModelSpec defines the desired state of Model
type ModelSpec struct {
	// Source defines where to obtain the model.
	// For GGUF models: URL or path to a .gguf file.
	// For MLX models: local directory path containing the model (config.json, weights).
	// Supported schemes: http://, https://, file://, pvc://, or absolute paths.
	// Examples:
	//   - https://huggingface.co/org/repo/resolve/main/model.gguf
	//   - file:///mnt/models/model.gguf
	//   - /mnt/models/model.gguf (air-gapped deployments)
	//   - pvc://my-models-pvc/path/to/model.gguf (pre-staged on a PersistentVolumeClaim)
	//   - /mnt/models/Llama-3.2-3B-Instruct-4bit (MLX model directory)
	//
	// file:// caveat for hybrid topologies: the controller pod must be
	// able to read the path. In Mac kind / k3s / GKE deployments where
	// the metal-agent runs on the host and the controller runs inside a
	// container, /Users/... and other host paths are invisible to the
	// controller and will fail to fetch. The controller marks the Model
	// Failed and backs off to a 5-minute requeue rather than retrying
	// tightly (#405). Workaround: pre-stage on a pvc://, or use the
	// equivalent https://huggingface.co/.../<filename>.gguf URL which
	// the runtime/init container resolves at deploy time.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(https?|file|pvc)://.*|^/[^\s]+$|^[a-zA-Z0-9][\w\-\.\/]+$`
	Source string `json:"source"`

	// SHA256 is the expected SHA256 hash of the model file for integrity verification.
	// When set, the controller verifies the downloaded/copied file matches this hash.
	// +kubebuilder:validation:Pattern=`^[a-fA-F0-9]{64}$`
	// +optional
	SHA256 string `json:"sha256,omitempty"`

	// RefreshPolicy controls whether a cached model file is re-fetched when the
	// upstream source changes.
	//
	// - "IfNotPresent" (default): download only if the cached file is missing.
	//   Upstream changes are still detected and surfaced via the SourceDrifted
	//   condition, but the cached file is never re-fetched on its own. This
	//   preserves the historical behavior so an operator upgrade triggers no
	//   surprise re-pulls.
	// - "OnChange": re-download when the upstream bytes differ from what was
	//   cached (HTTP ETag/Content-Length for remote sources, file size/mtime for
	//   local sources). The re-download overwrites the file in the existing cache
	//   directory; the cache key is unchanged.
	// +kubebuilder:validation:Enum=IfNotPresent;OnChange
	// +kubebuilder:default=IfNotPresent
	// +optional
	RefreshPolicy string `json:"refreshPolicy,omitempty"`

	// Format specifies the model file format.
	// "gguf" is used with the llama-server runtime; "mlx" is used with the oMLX runtime;
	// "safetensors", "pytorch", and "custom" are used with the generic runtime.
	// +kubebuilder:validation:Enum=gguf;mlx;safetensors;pytorch;custom
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
	// Accelerator specifies the type of hardware acceleration.
	// "vulkan" covers AMD and Intel GPUs using the Vulkan runtime
	// (gpu.vendor: amd/intel + gpu.runtime: vulkan). When set to
	// "vulkan" the readiness-check path uses devic.es/dri-render as
	// the GPU resource name instead of amd.com/gpu or nvidia.com/gpu.
	// +kubebuilder:validation:Enum=cpu;metal;cuda;rocm;intel;vulkan
	// +kubebuilder:default=cpu
	// +optional
	Accelerator string `json:"accelerator,omitempty"`

	// GPU specifies GPU device requirements
	// +optional
	GPU *GPUSpec `json:"gpu,omitempty"`

	// MemoryBudget is an absolute memory limit for the model process
	// (e.g., "24Gi", "8192Mi"). When set, it takes precedence over
	// MemoryFraction and the agent-level --memory-fraction flag.
	// Parsed via resource.ParseQuantity().
	// +optional
	MemoryBudget string `json:"memoryBudget,omitempty"`

	// MemoryFraction is the fraction of total system memory to budget for
	// this model's inference process (0.0–1.0). Takes precedence over the
	// agent-level --memory-fraction flag but not MemoryBudget.
	// +optional
	MemoryFraction *float64 `json:"memoryFraction,omitempty"`
}

// GPUSpec defines GPU-specific requirements.
// +kubebuilder:validation:XValidation:rule="!(has(self.resourceName) && has(self.resourceClaims) && self.resourceClaims.size() > 0)",message="resourceClaims and resourceName are mutually exclusive: use one or the other for GPU scheduling"
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

	// ResourceName overrides the extended resource the operator requests for
	// this Model's pods. Defaults are derived from Vendor:
	//   nvidia -> nvidia.com/gpu
	//   amd    -> amd.com/gpu
	//   intel  -> gpu.intel.com/i915
	// Set this for non-default device plugins (e.g. squat/generic-device-plugin
	// advertising `squat.ai/dri-render`, NVIDIA MIG slices, or DRA-backed
	// resources). When set, this value also drives the GPU toleration unless
	// TolerationKey is provided explicitly.
	// +kubebuilder:validation:Pattern=`^[a-z0-9.\-]+/[a-z0-9._\-]+$`
	// +optional
	ResourceName string `json:"resourceName,omitempty"`

	// TolerationKey overrides the taint key the operator tolerates when
	// scheduling GPU pods. Defaults to ResourceName (or the vendor default
	// resource name when ResourceName is unset), so in most cases this can
	// be left empty.
	// +kubebuilder:validation:Pattern=`^[a-z0-9.\-]+/[a-z0-9._\-]+$`
	// +optional
	TolerationKey string `json:"tolerationKey,omitempty"`

	// Runtime selects the GPU compute backend the operator schedules for this
	// Model, independent of the Vendor field. It exists so `vendor: amd` is not
	// overloaded to mean both "ROCm" and "Vulkan".
	//
	// For the llama.cpp inference backend with `vendor: amd`:
	//   - "vulkan": schedule LLMKube's Vulkan llama.cpp image and request the
	//     generic-device-plugin resource `devic.es/dri-render` (unless
	//     ResourceName overrides it). The plugin injects /dev/dri; the non-root
	//     container still needs the host render group, supplied via
	//     InferenceService.spec.podSecurityContext.supplementalGroups.
	//   - "rocm": the historical behavior (amd -> amd.com/gpu, stock image).
	//   - "" (empty): back-compatible, identical to "rocm".
	//
	// Ignored for non-AMD vendors and non-llama.cpp backends.
	// +kubebuilder:validation:Enum=vulkan;rocm
	// +optional
	Runtime string `json:"runtime,omitempty"`

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

	// ResourceClaims defines DRA (Dynamic Resource Allocation) claims for GPU devices.
	// Uses resource.k8s.io/v1 PodResourceClaim format. Each claim must have exactly
	// one of resourceClaimName or resourceClaimTemplateName set.
	// Mutually exclusive with resourceName.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:XValidation:rule="self.size() == 0 || self.all(c, (has(c.resourceClaimName) && !has(c.resourceClaimTemplateName)) || (!has(c.resourceClaimName) && has(c.resourceClaimTemplateName)))",message="each claim must have exactly one of resourceClaimName or resourceClaimTemplateName"
	ResourceClaims []corev1.PodResourceClaim `json:"resourceClaims,omitempty"`
}

// GPUShardingSpec defines multi-GPU sharding strategy
type GPUShardingSpec struct {
	// Strategy defines the sharding approach for multi-GPU model execution.
	// - "layer" (default): shard by transformer layers. llama.cpp --split-mode layer.
	// - "tensor" (alias: "row"): true tensor parallelism. llama.cpp --split-mode row.
	//   Splits each tensor operation across GPUs rather than assigning whole layers
	//   to each. Performance varies by workload; typically better on compute-bound ops.
	// - "none": disable multi-GPU sharding (single GPU). llama.cpp --split-mode none.
	// - "pipeline": accepted for forward compatibility but currently falls back to
	//   "layer" with a reconciler warning; llama.cpp has no pipeline split-mode.
	// +kubebuilder:validation:Enum=layer;tensor;row;pipeline;none
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

	// License is the license identifier extracted from the GGUF file metadata
	// +optional
	License string `json:"license,omitempty"`
}

// ModelStatus defines the observed state of Model.
type ModelStatus struct {
	// Phase represents the current lifecycle phase of the model.
	// Possible values: Pending, Downloading, Copying, Ready, Failed.
	// +kubebuilder:validation:Enum=Pending;Downloading;Copying;Ready;Failed
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

	// SHA256 is the computed SHA256 hash of the model file.
	// Populated after download/copy for integrity tracking.
	// +optional
	SHA256 string `json:"sha256,omitempty"`

	// SourceETag is the HTTP ETag recorded for the upstream source at the last
	// revalidation. Used to detect upstream changes for http/https sources
	// (HuggingFace serves the blob SHA as the ETag, so a moved branch is caught).
	// +optional
	SourceETag string `json:"sourceETag,omitempty"`

	// SourceContentLength is the upstream size recorded at the last revalidation.
	// For http/https sources it is the Content-Length reported by a HEAD request;
	// for local sources it is the file size on disk. Used together with
	// SourceETag (or mtime for local sources) to detect upstream changes.
	// +optional
	SourceContentLength int64 `json:"sourceContentLength,omitempty"`

	// LastRevalidated is the timestamp of the last upstream revalidation check.
	// Revalidation is cadence-gated so the controller does not issue a HEAD on
	// every reconcile.
	// +optional
	LastRevalidated *metav1.Time `json:"lastRevalidated,omitempty"`

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
	// - "SourceDrifted": the upstream source bytes differ from the cached copy
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
