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

	// Runtime selects the inference server backend.
	// "llamacpp" (default): llama.cpp server with auto-generated args and /health probes.
	// "generic": user-provided container with custom command, args, env, and probes.
	// "personaplex": NVIDIA PersonaPlex (Moshi) speech-to-speech server.
	// +kubebuilder:validation:Enum=llamacpp;personaplex;generic
	// +kubebuilder:default=llamacpp
	// +optional
	Runtime string `json:"runtime,omitempty"`

	// Replicas is the desired number of inference pods
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Autoscaling configures horizontal pod autoscaling for the inference service.
	// When set, the controller creates and manages an HPA resource targeting the
	// inference Deployment. Requires Prometheus Adapter for custom metrics.
	// Mutually exclusive with manual replica management: when autoscaling is enabled,
	// the Replicas field serves as the initial replica count only.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// Image is the container image for the inference runtime.
	// For llamacpp runtime, defaults to ghcr.io/ggml-org/llama.cpp:server.
	// For generic runtime, this field is required.
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

	// ContextSize sets the context window size for the llama.cpp server (-c flag).
	// Larger values allow processing longer inputs but require more memory.
	// If not specified, llama.cpp uses its default (typically 512 or 2048).
	// +kubebuilder:validation:Minimum=128
	// +kubebuilder:validation:Maximum=131072
	// +optional
	ContextSize *int32 `json:"contextSize,omitempty"`

	// ParallelSlots sets the number of concurrent request slots for the llama.cpp server (--parallel flag).
	// Each slot can process one request independently, enabling concurrent inference.
	// Higher values use more memory. If not specified, llama.cpp defaults to 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	// +optional
	ParallelSlots *int32 `json:"parallelSlots,omitempty"`

	// FlashAttention enables flash attention for faster prompt processing and reduced
	// VRAM usage. Requires a GPU with flash attention support (NVIDIA Ampere or newer).
	// Maps to llama.cpp --flash-attn flag. Only applied when GPU is configured.
	// +optional
	FlashAttention *bool `json:"flashAttention,omitempty"`

	// Jinja enables Jinja2 chat template rendering for tool/function calling support.
	// Required when using the OpenAI-compatible API with tools. Maps to llama.cpp --jinja flag.
	// +optional
	Jinja *bool `json:"jinja,omitempty"`

	// CacheTypeK sets the KV cache quantization type for keys.
	// Supported values depend on the llama.cpp build version.
	// Maps to llama.cpp --cache-type-k flag. Default: f16 (llama.cpp default).
	// +kubebuilder:validation:Enum=f16;f32;q8_0;q4_0;q4_1;q5_0;q5_1;iq4_nl
	// +optional
	CacheTypeK string `json:"cacheTypeK,omitempty"`

	// CacheTypeV sets the KV cache quantization type for values.
	// Maps to llama.cpp --cache-type-v flag. Default: f16 (llama.cpp default).
	// +kubebuilder:validation:Enum=f16;f32;q8_0;q4_0;q4_1;q5_0;q5_1;iq4_nl
	// +optional
	CacheTypeV string `json:"cacheTypeV,omitempty"`

	// ExtraArgs provides additional command-line arguments passed directly to the
	// llama-server process. Use for flags not yet supported as typed CRD fields.
	// Arguments are appended after all other configured flags.
	// Only used when Runtime is "llamacpp".
	// Example: ["--seed", "42", "--batch-size", "2048"]
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`

	// Command overrides the container entrypoint.
	// Only used when Runtime is "generic" or for advanced customization.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args overrides the container arguments entirely.
	// Only used when Runtime is "generic". For llamacpp, use ExtraArgs instead.
	// +optional
	Args []string `json:"args,omitempty"`

	// Env adds environment variables to the inference container.
	// Useful for HF_TOKEN, custom runtime config, etc.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// ContainerPort overrides the primary container port.
	// Each runtime has its own default (llamacpp: 8080).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	ContainerPort *int32 `json:"containerPort,omitempty"`

	// ProbeOverrides allows replacing the auto-generated health probes.
	// Useful for runtimes with non-HTTP health endpoints (e.g., TCP, WebSocket).
	// +optional
	ProbeOverrides *ProbeOverrides `json:"probeOverrides,omitempty"`

	// SkipModelInit disables the model-downloader init container.
	// Use when the model is baked into the image or downloaded by the
	// container itself (e.g., via HF_TOKEN).
	// +optional
	SkipModelInit *bool `json:"skipModelInit,omitempty"`

	// PersonaPlexConfig holds configuration for the PersonaPlex (Moshi) runtime.
	// Only used when Runtime is "personaplex".
	// +optional
	PersonaPlexConfig *PersonaPlexConfig `json:"personaPlexConfig,omitempty"`

	// ImagePullSecrets for pulling container images from private registries.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Priority determines scheduling priority for GPU allocation.
	// Higher priority services can preempt lower priority ones when GPUs are scarce.
	// +kubebuilder:validation:Enum=critical;high;normal;low;batch
	// +kubebuilder:default=normal
	// +optional
	Priority string `json:"priority,omitempty"`

	// PriorityClassName allows specifying a custom Kubernetes PriorityClass.
	// Takes precedence over the Priority field if set.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// PodSecurityContext defines pod-level security attributes for inference pods.
	// Use this to set fsGroup for volume permissions (required on OpenShift).
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`

	// SecurityContext defines container-level security attributes for the inference container.
	// +optional
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`
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

// AutoscalingSpec configures Horizontal Pod Autoscaler for the inference service.
type AutoscalingSpec struct {
	// MinReplicas is the lower limit for the number of replicas.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=1
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the upper limit for the number of replicas.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxReplicas int32 `json:"maxReplicas"`

	// Metrics defines the scaling metrics and target values.
	// If empty, defaults to llamacpp:requests_processing with target average value of 2.
	// +optional
	Metrics []MetricSpec `json:"metrics,omitempty"`
}

// MetricSpec defines a single metric for HPA scaling.
type MetricSpec struct {
	// Type is the metric source type.
	// +kubebuilder:validation:Enum=Pods;Resource
	Type string `json:"type"`

	// Name is the metric name (e.g., llamacpp:requests_processing).
	Name string `json:"name"`

	// TargetAverageValue is the target per-pod average for Pods-type metrics.
	// +optional
	TargetAverageValue *string `json:"targetAverageValue,omitempty"`

	// TargetAverageUtilization is the target utilization percentage for Resource-type metrics.
	// +optional
	TargetAverageUtilization *int32 `json:"targetAverageUtilization,omitempty"`
}

// ProbeOverrides allows custom probe configuration per-runtime.
// When set, the specified probes replace the auto-generated defaults.
type ProbeOverrides struct {
	// Startup overrides the startup probe.
	// +optional
	Startup *corev1.Probe `json:"startup,omitempty"`

	// Liveness overrides the liveness probe.
	// +optional
	Liveness *corev1.Probe `json:"liveness,omitempty"`

	// Readiness overrides the readiness probe.
	// +optional
	Readiness *corev1.Probe `json:"readiness,omitempty"`
}

// PersonaPlexConfig holds configuration for the PersonaPlex (Moshi) speech-to-speech runtime.
type PersonaPlexConfig struct {
	// Quantize4Bit enables NF4 4-bit quantization for reduced VRAM usage (~9.6 GB vs ~14 GB).
	// Requires the bitsandbytes package in the container image.
	// +optional
	Quantize4Bit *bool `json:"quantize4Bit,omitempty"`

	// CPUOffload enables model weight offloading to system RAM when GPU VRAM is insufficient.
	// Requires the accelerate package in the container image.
	// +optional
	CPUOffload *bool `json:"cpuOffload,omitempty"`

	// HFTokenSecretRef references a Secret containing the HuggingFace token for model download.
	// The Secret key must be "HF_TOKEN".
	// +optional
	HFTokenSecretRef *corev1.SecretKeySelector `json:"hfTokenSecretRef,omitempty"`
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

	// SchedulingStatus indicates why pods cannot be scheduled (e.g., "InsufficientGPU")
	// +optional
	SchedulingStatus string `json:"schedulingStatus,omitempty"`

	// SchedulingMessage provides details about scheduling issues
	// +optional
	SchedulingMessage string `json:"schedulingMessage,omitempty"`

	// QueuePosition indicates position among pending InferenceServices cluster-wide (0 = not queued)
	// +optional
	QueuePosition int32 `json:"queuePosition,omitempty"`

	// WaitingFor describes the resource constraint (e.g., "nvidia.com/gpu: 1")
	// +optional
	WaitingFor string `json:"waitingFor,omitempty"`

	// EffectivePriority shows the resolved priority value from the applied PriorityClass
	// +optional
	EffectivePriority int32 `json:"effectivePriority,omitempty"`

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
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.schedulingStatus`,priority=1
// +kubebuilder:printcolumn:name="Queue",type=integer,JSONPath=`.status.queuePosition`,priority=1
// +kubebuilder:printcolumn:name="Priority",type=string,JSONPath=`.spec.priority`,priority=1
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
