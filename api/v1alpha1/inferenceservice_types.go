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
	// "vllm": vLLM OpenAI-compatible server with PagedAttention.
	// "tgi": HuggingFace Text Generation Inference server.
	// +kubebuilder:validation:Enum=llamacpp;personaplex;vllm;tgi;generic
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

	// MoeCPUOffload offloads all MoE expert layers to CPU for reduced VRAM usage.
	// Enables running large MoE models (e.g., Qwen3-30B, Mixtral) on VRAM-constrained
	// hardware by keeping attention layers on GPU while expert weights use system RAM.
	// Maps to llama.cpp --cpu-moe flag. Requires sufficient system RAM via resources.memory.
	// +optional
	MoeCPUOffload *bool `json:"moeCPUOffload,omitempty"`

	// MoeCPULayers sets the number of MoE layers to offload to CPU.
	// When set, only the specified number of MoE layers run on CPU rather than all.
	// Maps to llama.cpp --n-cpu-moe flag.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MoeCPULayers *int32 `json:"moeCPULayers,omitempty"`

	// NoKvOffload keeps the KV cache in system RAM instead of VRAM.
	// Useful for extended context windows when VRAM is constrained by model weights.
	// Maps to llama.cpp --no-kv-offload flag. Requires sufficient system RAM via resources.memory.
	// +optional
	NoKvOffload *bool `json:"noKvOffload,omitempty"`

	// NoWarmup skips the llama.cpp startup warmup inference pass.
	// Reduces pod ready time at the cost of slightly higher first-request latency.
	// Useful for scale-to-zero and quick redeployment patterns.
	// Maps to llama.cpp --no-warmup flag.
	// +optional
	NoWarmup *bool `json:"noWarmup,omitempty"`

	// ReasoningBudget caps the number of reasoning tokens the model is allowed to
	// emit per response. Zero disables visible thinking output entirely; the model
	// still reasons internally but does not emit thinking tokens. Critical for
	// production agentic workloads on thinking models (Qwen 3.6, GLM-5) where
	// runaway reasoning can burn compute.
	// Maps to llama.cpp --reasoning-budget flag.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ReasoningBudget *int32 `json:"reasoningBudget,omitempty"`

	// ReasoningBudgetMessage is injected when the reasoning budget is exhausted,
	// forcing the model to conclude. Ignored unless ReasoningBudget is also set.
	// Maps to llama.cpp --reasoning-budget-message flag.
	// +optional
	ReasoningBudgetMessage string `json:"reasoningBudgetMessage,omitempty"`

	// MetadataOverrides overrides GGUF metadata key-value pairs at model load time.
	// Each entry is passed as a separate --override-kv flag. Format: key=type:value
	// (e.g., "qwen35moe.context_length=int:1048576" to extend context window, or
	// "tokenizer.chat_template.thinking=bool:false" to tweak tokenizer behavior).
	// Maps to llama.cpp --override-kv flag (one flag per entry).
	// +optional
	MetadataOverrides []string `json:"metadataOverrides,omitempty"`

	// TensorOverrides provides fine-grained tensor placement overrides for power users.
	// Each entry specifies a tensor name and target device (e.g., "exps=CPU", "token_embd=CUDA0").
	// Maps to llama.cpp --override-tensor flag (one flag per entry).
	// +optional
	TensorOverrides []string `json:"tensorOverrides,omitempty"`

	// BatchSize sets the token batch size for prompt processing.
	// Larger values improve throughput but use more memory.
	// Maps to llama.cpp --batch-size flag.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16384
	// +optional
	BatchSize *int32 `json:"batchSize,omitempty"`

	// UBatchSize sets the micro-batch size for decoding.
	// Smaller micro-batches reduce memory usage during generation.
	// Maps to llama.cpp --ubatch-size flag.
	// +kubebuilder:validation:Minimum=1
	// +optional
	UBatchSize *int32 `json:"uBatchSize,omitempty"`

	// ExtraArgs provides additional command-line arguments passed directly to the
	// runtime process. Use for flags not yet supported as typed CRD fields.
	// Arguments are appended after all other configured flags.
	// Supported by the "llamacpp" and "vllm" runtimes. Ignored by others.
	// Example: ["--seed", "42", "--log-disable"]
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

	// VLLMConfig holds configuration for the vLLM runtime.
	// Only used when Runtime is "vllm".
	// +optional
	VLLMConfig *VLLMConfig `json:"vllmConfig,omitempty"`

	// TGIConfig holds configuration for the TGI runtime.
	// Only used when Runtime is "tgi".
	// +optional
	TGIConfig *TGIConfig `json:"tgiConfig,omitempty"`

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

	// HostMemory specifies the system RAM required for hybrid GPU/CPU offloading (e.g., "64Gi").
	// Used when MoE expert weights or KV cache are offloaded to CPU via moeCPUOffload or noKvOffload.
	// Translated to pod resources.requests.memory, taking precedence over Memory when set.
	// Without this, the K8s scheduler has no visibility into the pod's actual RAM consumption,
	// which can lead to OOM kills after model load.
	// +optional
	HostMemory string `json:"hostMemory,omitempty"`

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

// VLLMConfig holds configuration for the vLLM inference server.
type VLLMConfig struct {
	// TensorParallelSize sets the number of GPUs for tensor parallelism.
	// +optional
	TensorParallelSize *int32 `json:"tensorParallelSize,omitempty"`

	// MaxModelLen sets the maximum model context length.
	// +optional
	MaxModelLen *int32 `json:"maxModelLen,omitempty"`

	// Quantization method (awq, gptq, squeezellm).
	// +kubebuilder:validation:Enum=awq;gptq;squeezellm
	// +optional
	Quantization string `json:"quantization,omitempty"`

	// Dtype sets the model data type (auto, float16, bfloat16).
	// +kubebuilder:validation:Enum=auto;float16;bfloat16
	// +optional
	Dtype string `json:"dtype,omitempty"`

	// EnablePrefixCaching turns on vLLM's automatic prefix caching for repeated prompts.
	// Significantly reduces time-to-first-token for conversational and agentic workloads
	// where requests share a common system prompt.
	// Maps to vLLM --enable-prefix-caching flag.
	// +optional
	EnablePrefixCaching *bool `json:"enablePrefixCaching,omitempty"`

	// AttentionBackend selects the attention implementation used by vLLM.
	// flashinfer is typically fastest on recent NVIDIA GPUs; flash_attn is a solid
	// default; torch_sdpa and xformers are portability fallbacks. Requires a vLLM
	// version that supports the chosen backend.
	// Maps to vLLM --attention-backend flag.
	// +kubebuilder:validation:Enum=flashinfer;flash_attn;xformers;torch_sdpa
	// +optional
	AttentionBackend string `json:"attentionBackend,omitempty"`

	// HFTokenSecretRef references a Secret containing the HuggingFace token.
	// +optional
	HFTokenSecretRef *corev1.SecretKeySelector `json:"hfTokenSecretRef,omitempty"`
}

// TGIConfig holds configuration for the HuggingFace Text Generation Inference server.
type TGIConfig struct {
	// Quantize sets the quantization method (bitsandbytes, gptq, awq, eetq).
	// +kubebuilder:validation:Enum=bitsandbytes;gptq;awq;eetq
	// +optional
	Quantize string `json:"quantize,omitempty"`

	// MaxInputLength sets the maximum input token length.
	// +optional
	MaxInputLength *int32 `json:"maxInputLength,omitempty"`

	// MaxTotalTokens sets the maximum total tokens (input + output).
	// +optional
	MaxTotalTokens *int32 `json:"maxTotalTokens,omitempty"`

	// Dtype sets the model data type (float16, bfloat16).
	// +kubebuilder:validation:Enum=float16;bfloat16
	// +optional
	Dtype string `json:"dtype,omitempty"`

	// HFTokenSecretRef references a Secret containing the HuggingFace token.
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
