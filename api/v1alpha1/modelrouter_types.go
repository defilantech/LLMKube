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

// ModelRouterDataPlane selects how a ModelRouter serves traffic.
type ModelRouterDataPlane string

const (
	// ModelRouterDataPlaneProxy provisions the managed router-proxy Deployment
	// and routes in-process. This is the default and preserves today's behavior.
	ModelRouterDataPlaneProxy ModelRouterDataPlane = "Proxy"

	// ModelRouterDataPlaneGateway compiles the router policy onto a pre-installed
	// Envoy AI Gateway instead of provisioning the router-proxy.
	ModelRouterDataPlaneGateway ModelRouterDataPlane = "Gateway"

	// ModelRouterDataPlaneAgentGateway compiles the router policy onto a
	// pre-installed agentgateway data plane (AgentgatewayBackend + HTTPRoute)
	// instead of provisioning the router-proxy. agentgateway is a conformant
	// Gateway API Inference Extension implementation and applies model rewriting
	// (spec.model) on its custom providers, so external-style model aliases are
	// honored here where the Envoy AI Gateway path drops them. It also routes MCP
	// and A2A natively in the same data plane. Requires the agentgateway.dev CRDs
	// and a pre-installed agentgateway Gateway; when they are absent the resources
	// are not generated and a condition explains why.
	ModelRouterDataPlaneAgentGateway ModelRouterDataPlane = "AgentGateway"
)

// Backend resolution modes for RouterBackend.Resolution. See the CRD field
// documentation for the semantics of each.
const (
	// RouterBackendResolutionService dispatches to the InferenceService's
	// ClusterIP Service DNS name. This is the default.
	RouterBackendResolutionService = "service"

	// RouterBackendResolutionEndpoint compiles one address per ready pod and
	// lets the router-proxy balance requests across them.
	RouterBackendResolutionEndpoint = "endpoint"
)

// DefaultRouteStrategy selects what the router does when no rule matches a
// request, before falling back to DefaultRoute.
type DefaultRouteStrategy string

const (
	// DefaultRouteStrategyStatic always routes unmatched requests to the named
	// DefaultRoute. This is the default.
	DefaultRouteStrategyStatic DefaultRouteStrategy = "Static"

	// DefaultRouteStrategyBackendNameMatch matches an unmatched request to a
	// backend whose Name equals the request "model" field before DefaultRoute.
	DefaultRouteStrategyBackendNameMatch DefaultRouteStrategy = "BackendNameMatch"
)

// ModelRouterSpec defines the desired state of a ModelRouter.
//
// A ModelRouter exposes a single OpenAI-compatible HTTP endpoint that
// dispatches requests across multiple InferenceService backends and external
// providers (Anthropic, OpenAI, LiteLLM passthrough) according to declarative
// routing rules. Rules can match on data classification, task complexity,
// required capabilities, request headers, or model name, and the router
// supports fail-closed semantics for sensitive-data routes so PII never
// escapes the cluster boundary.
// +kubebuilder:validation:XValidation:rule="!has(self.endpoint) || !has(self.endpoint.apiKeySecretRef)",message="endpoint.apiKeySecretRef is an InferenceService field; ModelRouter does not support it"
type ModelRouterSpec struct {
	// DataPlane selects how this ModelRouter serves traffic.
	//
	// "Proxy" (default) provisions the managed router-proxy Deployment +
	// Service and routes in-process per the rules below (today's behavior,
	// fully back-compat).
	//
	// "Gateway" compiles the backends and rules onto a pre-installed Envoy AI
	// Gateway: a Backend + AIServiceBackend per InferenceServiceRef backend, a
	// multi-rule AIGatewayRoute, and a retry/failover BackendTrafficPolicy. In
	// Gateway mode the router-proxy is NOT provisioned. Requires the aigw CRDs
	// to be installed; when they are absent the gateway resources are not
	// generated and a condition explains why.
	//
	// "AgentGateway" compiles the backends and rules onto a pre-installed
	// agentgateway data plane: one AgentgatewayBackend (custom provider targeting
	// an InferencePool) per InferenceServiceRef backend and a multi-rule HTTPRoute
	// matching on the X-Gateway-Base-Model-Name header. In AgentGateway mode the
	// router-proxy is NOT provisioned. Requires the agentgateway.dev CRDs and a
	// pre-installed agentgateway Gateway; when they are absent the resources are
	// not generated and a condition explains why.
	// +kubebuilder:validation:Enum=Proxy;Gateway;AgentGateway
	// +kubebuilder:default=Proxy
	// +optional
	DataPlane ModelRouterDataPlane `json:"dataPlane,omitempty"`

	// GatewayRef identifies the pre-installed Gateway (gateway.networking.k8s.io)
	// the generated AIGatewayRoute attaches to when DataPlane is "Gateway".
	// Required in Gateway mode; ignored in Proxy mode. The Gateway and the Envoy
	// AI Gateway stack are a documented prerequisite; LLMKube does not install
	// or own them. Cross-namespace attachment requires the Gateway listener's
	// allowedRoutes.namespaces to permit this ModelRouter's namespace.
	// +optional
	GatewayRef *GatewayReference `json:"gatewayRef,omitempty"`

	// AgentGatewayRef identifies the pre-installed agentgateway data plane the
	// generated InferencePool/HTTPRoute/model-extraction AgentgatewayPolicy
	// attach to when DataPlane is "AgentGateway". Required in AgentGateway mode;
	// ignored otherwise. The Gateway, the Gateway API Inference Extension CRDs
	// (InferencePool at inference.networking.k8s.io/v1), the agentgateway.dev
	// CRDs, and an Endpoint Picker (EPP) service are documented prerequisites;
	// LLMKube does not install or own them. The agentgateway controller must run
	// with Inference-Pool backendRef support (inferenceExtension.enabled=true in
	// the agentgateway chart). The EndpointPicker is referenced by every
	// generated InferencePool so agentgateway can select a model-server endpoint
	// per request. The Gateway may be in another namespace: the generated
	// AgentgatewayPolicy then lives in the Gateway's namespace (its targetRefs
	// are same-namespace), is not owner-referenced, and is cleaned up via a
	// finalizer on this ModelRouter. Because that policy targets the whole
	// Gateway, its body-model extraction runs on EVERY request through that
	// Gateway, including routes LLMKube did not generate. The Gateway must not
	// already carry another PreRouting transformation policy: the router is
	// refused (ConflictingPreRoutingPolicy) rather than silently shadowed.
	// agentgateway keeps the OLDEST same-specificity policy's transformation,
	// so LLMKube does not retract its policy on conflict; the condition clears
	// when the foreign policy is removed, and the reconciler rechecks every
	// two minutes (a foreign policy created after the router went Ready is
	// otherwise silently shadowed). agentgateway inspects at most 2 MiB of the
	// request body by default (its DEFAULT_BUFFER_LIMIT); larger bodies (long
	// contexts, images) are not routed on this path - the CEL extraction
	// fails and the model header is removed. A Gateway owner can raise the
	// limit with a frontend.http.maxBufferSize policy, a different field that
	// does not conflict with the extraction policy.
	// +optional
	AgentGatewayRef *AgentGatewayReference `json:"agentGatewayRef,omitempty"`

	// Backends are the candidate destinations the router can dispatch to.
	// Order is not significant; selection is rule-driven. At least one
	// backend must be declared.
	// +kubebuilder:validation:MinItems=1
	Backends []RouterBackend `json:"backends"`

	// Rules are evaluated in declaration order. The first matching rule wins.
	// If no rule matches, the fallback is governed by DefaultRouteStrategy,
	// ending at DefaultRoute. If nothing resolves, the request is rejected
	// with HTTP 503.
	// +optional
	Rules []RouterRule `json:"rules,omitempty"`

	// DefaultRoute names a backend used when no rule matches.
	// Must reference the Name of an entry in Backends.
	// +optional
	DefaultRoute string `json:"defaultRoute,omitempty"`

	// DefaultRouteStrategy decides what happens when no rule matches.
	// "Static" (default) routes to DefaultRoute. "BackendNameMatch" first
	// tries to match the request's model to a backend Name, falling back to
	// DefaultRoute only if none matches. BackendNameMatch makes every backend,
	// including cloud-tier ones, directly addressable by name; sensitive-data
	// protection still relies on a matching failClosed rule, which is evaluated
	// first and therefore continues to gate before any name match.
	// +kubebuilder:validation:Enum=Static;BackendNameMatch
	// +kubebuilder:default=Static
	// +optional
	DefaultRouteStrategy DefaultRouteStrategy `json:"defaultRouteStrategy,omitempty"`

	// Policy holds cross-cutting controls (budgets, classification, audit).
	// +optional
	Policy *RouterPolicy `json:"policy,omitempty"`

	// Endpoint defines the Kubernetes Service the router-proxy is exposed
	// through. Mirrors the shape used by InferenceService.
	// +optional
	Endpoint *EndpointSpec `json:"endpoint,omitempty"`

	// Proxy configures the managed router-proxy Deployment (replicas,
	// image override for air-gapped sites, resources). Sensible defaults
	// apply when omitted.
	// +optional
	Proxy *RouterProxySpec `json:"proxy,omitempty"`

	// MCPServer optionally exposes this router as a Model Context Protocol
	// endpoint. Inactive until the Phase 3 MCP feature lands; the field is
	// reserved in the schema for forward compatibility.
	// +optional
	MCPServer *MCPServerSpec `json:"mcpServer,omitempty"`
}

// RouterBackend is one candidate destination for routed requests.
// Exactly one of InferenceServiceRef or External must be set.
// +kubebuilder:validation:XValidation:rule="!has(self.balancing) || (has(self.inferenceServiceRef) && has(self.resolution) && self.resolution == 'endpoint')",message="balancing requires an inferenceServiceRef backend with resolution: endpoint"
type RouterBackend struct {
	// Name is the stable identifier used by rules and observability labels.
	// Must be lowercase alphanumeric or '-'.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]{0,62}$`
	Name string `json:"name"`

	// DisplayName is an optional freeform label published as the model id
	// on /v1/models and used by BackendNameMatch to resolve a request's
	// model field to a backend. When unset, Name is used for both
	// purposes (current behavior). This lets the k8s-safe Name differ
	// from the user-facing model identifier (e.g. Name "claude-opus-4"
	// with DisplayName "claude-opus-4-20250514").
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// ModelInfo describes what this backend's model accepts and how much
	// context it holds. The proxy publishes it on the backend's /v1/models
	// entry, where clients decide whether to send images and how to size
	// their context. Informational only: the proxy does not enforce it.
	// Unset, the entry carries only the model id. Applies to the Proxy data
	// plane only; the Gateway and AgentGateway data planes ignore it.
	// +optional
	ModelInfo *RouterBackendModelInfo `json:"modelInfo,omitempty"`

	// InferenceServiceRef references an in-cluster InferenceService.
	// Mutually exclusive with External.
	// +optional
	InferenceServiceRef *corev1.LocalObjectReference `json:"inferenceServiceRef,omitempty"`

	// External describes an out-of-cluster provider (Anthropic, OpenAI,
	// or a LiteLLM proxy). Mutually exclusive with InferenceServiceRef.
	// +optional
	External *ExternalProvider `json:"external,omitempty"`

	// Tier classifies the backend for rule matching. "local" backends are
	// served from inside the cluster; "cloud" backends egress the cluster
	// boundary. Fail-closed rules can only route to local-tier backends.
	// +kubebuilder:validation:Enum=local;cloud
	// +kubebuilder:default=local
	// +optional
	Tier string `json:"tier,omitempty"`

	// Resolution selects how a local (inferenceServiceRef) backend's upstream
	// address is resolved. "service" (the default) dispatches to the
	// InferenceService's ClusterIP Service DNS name, so a keep-alive client
	// can pin to one pod through conntrack. "endpoint" has the router-proxy
	// track the ready pods behind the Service itself and balance requests
	// across them.
	// External backends ignore this field.
	// +kubebuilder:validation:Enum=service;endpoint
	// +kubebuilder:default=service
	// +optional
	Resolution string `json:"resolution,omitempty"`

	// EndpointPicker selects the Endpoint Picker (EPP) that chooses a pod for
	// this backend's InferencePool on dataPlane: AgentGateway. When unset, the
	// backend uses the picker another backend on the same InferenceService
	// names, else the router-wide spec.agentGatewayRef.endpointPicker. A Gateway API
	// Inference Extension EPP binds to exactly one InferencePool, so a router
	// with backends on several InferenceServices sets a picker per backend.
	// Ignored by the Proxy and Gateway data planes.
	// +optional
	EndpointPicker *RouterEndpointPicker `json:"endpointPicker,omitempty"`

	// Balancing places requests across this backend's pods by load and keeps
	// a session on its pod until it runs hot. Requires resolution: endpoint.
	// Unset keeps round-robin.
	// Applies to the Proxy data plane only; ignored by the Gateway and
	// AgentGateway data planes.
	// +optional
	Balancing *BackendBalancing `json:"balancing,omitempty"`

	// Capabilities advertised by this backend. Rules can require
	// capabilities (e.g. ["tools", "vision", "long-context"]) to filter
	// candidates.
	// +optional
	Capabilities []string `json:"capabilities,omitempty"`

	// Weight is used for the "weighted" routing strategy. Higher values
	// receive proportionally more traffic. Ignored for other strategies.
	// Default 1 when unset. 0 drains the backend from weighted rules: it
	// receives no traffic from them, even while every other backend in the
	// rule is unavailable (the Gateway API meaning of a zero weight, which
	// both data planes follow). A weighted rule needs at least one backend
	// with a positive weight.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Weight *int32 `json:"weight,omitempty"`

	// CostPerMillionTokens is informational. Used for cost-aware routing
	// metrics and audit-log enrichment. Values are USD.
	// +optional
	CostPerMillionTokens *TokenCost `json:"costPerMillionTokens,omitempty"`

	// HealthCheck overrides the default health probe applied to this
	// backend by the router-proxy.
	// +optional
	HealthCheck *BackendHealthCheck `json:"healthCheck,omitempty"`

	// Timeout caps how long the proxy waits for this backend to begin
	// sending response headers. When set it overrides the proxy
	// default for dispatches that target this backend. Resolution
	// order at dispatch time: rule.timeout || backend.timeout ||
	// proxy default (ModelRouter.spec.proxy.responseHeaderTimeout).
	// Useful when backends in the same router have wildly different
	// P99 envelopes (in-cluster vLLM vs Anthropic global LB).
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// RouterBackendModelInfo is the client-facing model metadata a backend
// publishes on /v1/models.
type RouterBackendModelInfo struct {
	// InputModalities the model accepts. When unset, a backend whose
	// capabilities include "vision" publishes ["text", "image"], and any
	// other backend ["text"]. When set, it wins over capabilities.
	// +kubebuilder:validation:MaxItems=5
	// +kubebuilder:validation:items:Enum=text;image;audio;video;file
	// +listType=set
	// +optional
	InputModalities []string `json:"inputModalities,omitempty"`

	// OutputModalities the model produces. Defaults to ["text"].
	// +kubebuilder:validation:MaxItems=5
	// +kubebuilder:validation:items:Enum=text;image;audio;video;file
	// +listType=set
	// +optional
	OutputModalities []string `json:"outputModalities,omitempty"`

	// ContextLength is the context window one request gets, in tokens. When
	// unset, the controller fills it from the InferenceService:
	// vllmConfig.maxModelLen (or --max-model-len in extraArgs) for vLLM, and
	// contextSize divided across parallelSlots for llama.cpp. Other runtimes,
	// and llama.cpp extraArgs that set those flags, publish none.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ContextLength *int64 `json:"contextLength,omitempty"`
}

// ExternalProvider describes an upstream LLM API outside the cluster.
type ExternalProvider struct {
	// Provider identifies the upstream API surface. For "litellm", URL must
	// point at a running LiteLLM proxy speaking OpenAI-compatible API.
	// For first-party providers, URL is optional (provider defaults apply).
	// +kubebuilder:validation:Enum=anthropic;openai;bedrock;vertex_ai;litellm
	Provider string `json:"provider"`

	// URL is the base URL for the provider. Required for "litellm";
	// optional for first-party providers, which use their published default.
	// +optional
	URL string `json:"url,omitempty"`

	// Model is the upstream model identifier passed through to the
	// provider (e.g. "claude-opus-4-7", "gpt-5", a LiteLLM model alias).
	Model string `json:"model"`

	// CredentialsSecretRef points to a Kubernetes Secret containing the
	// provider credentials. Conventional keys: ANTHROPIC_API_KEY,
	// OPENAI_API_KEY, LITELLM_MASTER_KEY. The router-proxy reads these as
	// environment variables.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`
}

// TokenCost expresses USD per million tokens for prompt and completion.
type TokenCost struct {
	// PromptUSD is the cost per million prompt (input) tokens, in USD.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +optional
	PromptUSD string `json:"promptUSD,omitempty"`

	// CompletionUSD is the cost per million completion (output) tokens,
	// in USD.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +optional
	CompletionUSD string `json:"completionUSD,omitempty"`
}

// BackendHealthCheck overrides the default health probing for a backend.
type BackendHealthCheck struct {
	// Path is the HTTP path probed for health. Defaults to "/health" for
	// local backends and to the provider's documented health route for
	// external providers.
	// +optional
	Path string `json:"path,omitempty"`

	// IntervalSeconds is how often the router-proxy probes the backend.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10
	// +optional
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// TimeoutSeconds is the maximum time a single probe may take.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// RouterRule pairs a match expression with a routing action.
type RouterRule struct {
	// Name is used in audit logs and metrics labels.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]{0,62}$`
	Name string `json:"name"`

	// Match groups all match conditions. All declared conditions must be
	// true for the rule to fire (AND semantics). If Match is omitted the
	// rule always matches (useful as a catch-all before DefaultRoute).
	// +optional
	Match *RuleMatch `json:"match,omitempty"`

	// Route is the action taken when this rule matches.
	Route RuleRoute `json:"route"`

	// FailClosed: when true, if no backend in Route.Backends is healthy
	// or otherwise eligible, the router rejects the request with HTTP 503
	// rather than falling through to DefaultRoute or subsequent rules.
	// This is the regulated-data gate: a fail-closed rule guarantees that
	// matched requests are never served by any other route.
	// +optional
	FailClosed bool `json:"failClosed,omitempty"`

	// Timeout caps how long the proxy waits for the upstream to begin
	// sending response headers on dispatches matched by this rule.
	// When set it overrides RouterBackend.Timeout and the proxy
	// default. Resolution order at dispatch time:
	// rule.timeout || backend.timeout || proxy default.
	// Useful for tightening regulated-data rules (sub-10s strict
	// fail-fast) or extending long-reasoning rules (120s+).
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// RuleMatch declares the conditions under which a rule fires.
//
// All declared fields are ANDed. Empty match fields are not considered.
type RuleMatch struct {
	// DataClassification matches if the inbound request carries any of
	// these classifications. The classification source depends on
	// Policy.Classification.Mode: a request header
	// (x-llmkube-classification by default), the bundled detector, or
	// both. Common values: "public", "internal", "confidential", "pii",
	// "phi".
	// +optional
	DataClassification []string `json:"dataClassification,omitempty"`

	// TaskComplexity matches the inbound complexity hint (header
	// x-llmkube-task-complexity).
	// +kubebuilder:validation:Enum=simple;moderate;complex
	// +optional
	TaskComplexity string `json:"taskComplexity,omitempty"`

	// RequiredCapabilities filters backends. The rule only matches if at
	// least one backend in Route.Backends advertises every listed
	// capability, and on dataPlane: Proxy only those backends serve the
	// rule's requests, under every strategy. A backend that lacks one is
	// never tried, not even as a fallback.
	// +optional
	RequiredCapabilities []string `json:"requiredCapabilities,omitempty"`

	// LatencySLOMs is a P95 first-token-latency target in milliseconds.
	// When set, if the rolling P95 for the primary backend exceeds this
	// value the rule promotes its declared fallback. Honored only by the
	// "primary-fallback" strategy.
	// +kubebuilder:validation:Minimum=1
	// +optional
	LatencySLOMs *int32 `json:"latencySLOMs,omitempty"`

	// Headers performs exact-match equality on inbound HTTP headers
	// (case-insensitive header name comparison).
	// +optional
	Headers map[string]string `json:"headers,omitempty"`

	// Models matches against the OpenAI-style "model" field in the
	// request body. Glob patterns are supported (e.g. "qwen3-*").
	// +optional
	Models []string `json:"models,omitempty"`
}

// RuleRoute names the backends a matched rule should dispatch to and how.
type RuleRoute struct {
	// Backends is an ordered list of RouterBackend.Name values. For the
	// "primary-fallback" strategy, the first entry is the primary and
	// subsequent entries are tried in order on failure. For "weighted",
	// traffic is distributed across all entries by Backend.Weight. For
	// "shadow", the first entry serves the response (falling back in order
	// as primary-fallback does) and the entries after the one that served
	// receive mirrored copies for evaluation only; it needs at least two.
	// +kubebuilder:validation:MinItems=1
	Backends []string `json:"backends"`

	// Strategy selects how multiple backends are used. On dataPlane: Proxy,
	// weighted picks a backend per request in proportion to its weight among
	// the rule's backends that are healthy, not quarantined and advertise
	// every required capability; a request that fails on it fails over to
	// another backend of the rule picked the same way, never the same one
	// twice. A weighted rule there cannot name two members of one ModelPool,
	// since they share one GPU slot and would swap on every other request.
	// shadow serves the client exactly as primary-fallback does and sends a
	// copy of each request, marked x-llmkube-shadow: true, to the rule's
	// other healthy backends after the one that served; their responses are
	// discarded, their failures never quarantine them, and they are not
	// charged to budgets. Copies are best effort: at most 4 run at once per
	// backend and the rest are dropped and counted. An external backend that
	// is a shadow target receives, and bills, every request. A shadow target
	// cannot be a ModelPool member. dataPlane: Gateway rejects shadow. On dataPlane:
	// AgentGateway only primary-fallback compiles, and a rule routes to its
	// first healthy backend: when that backend's InferenceService has no
	// ready replica, the next reconcile moves the rule to the next backend.
	// Failover there follows replica readiness, not individual request errors.
	// +kubebuilder:validation:Enum=primary-fallback;weighted;shadow
	// +kubebuilder:default=primary-fallback
	// +optional
	Strategy string `json:"strategy,omitempty"`

	// PoolActivation decides what the proxy does when a backend in
	// Backends is a ModelPool member that is not resident and the resident
	// member is busy. "Wait" (default) holds the request open until the
	// incumbent drains and the target loads, bounded by the pool's
	// swapBudget. "IfIdle" only starts a swap when the incumbent is idle;
	// while it is busy the pooled backend is skipped immediately and the
	// request falls through to the next entry in Backends, so a rule such
	// as [preferred-model, resident-model] serves on whichever is warm
	// instead of queueing behind in-flight work. Non-pooled backends and
	// same-model requests are unaffected.
	// +kubebuilder:validation:Enum=Wait;IfIdle
	// +kubebuilder:default=Wait
	// +optional
	PoolActivation string `json:"poolActivation,omitempty"`
}

// RouterPolicy holds cross-cutting controls applied to all rules.
type RouterPolicy struct {
	// Budgets caps token and dollar consumption per scope over a rolling
	// window. Empty list means no budget enforcement.
	// +optional
	Budgets []BudgetSpec `json:"budgets,omitempty"`

	// Classification configures how the router determines the data
	// classification of an inbound request.
	// +optional
	Classification *ClassificationPolicy `json:"classification,omitempty"`

	// AuditLog controls structured audit emission. Auditing is always on;
	// this field tunes the destination and verbosity.
	// +optional
	AuditLog *AuditLogPolicy `json:"auditLog,omitempty"`

	// Auth configures request authentication. In dataPlane: Gateway mode it
	// compiles to an Envoy AI Gateway SecurityPolicy that validates inbound JWTs
	// and maps a verified claim onto a trusted header before any model dispatch.
	// nil means no authentication is enforced. Authentication only; per-team
	// model allowlists (authorization) are a separate surface.
	// +optional
	Auth *RouterAuthSpec `json:"auth,omitempty"`
}

// RouterAuthSpec configures request authentication for the router. Only JWT
// authentication is supported today; the struct leaves room for additional
// authentication methods without a breaking change.
type RouterAuthSpec struct {
	// JWT enables JSON Web Token validation. When set (in dataPlane: Gateway
	// mode) the gateway rejects requests without a valid token with HTTP 401
	// before any model dispatch, and maps the configured claim onto a trusted
	// header.
	// +optional
	JWT *JWTAuthSpec `json:"jwt,omitempty"`

	// Allowlists restricts which verified team may reach which model
	// (authorization), as a sibling of JWT (authentication). Each entry grants a
	// team the models it may reach. JWT proves identity; Allowlists decide what
	// that identity is permitted to do.
	//
	// Empty or nil means NO authorization is enforced: any authenticated request
	// reaches any model (the authentication-only behavior of slice 2d-core, so
	// adding this field cannot retroactively lock out an existing router). A
	// non-empty Allowlists flips the generated SecurityPolicy to default-Deny:
	// only the named teams (and, per entry, only their listed models) are
	// allowed, and every other verified team is rejected with HTTP 403.
	//
	// Authorization requires authentication: Allowlists set without JWT is
	// rejected fail-loud (you cannot authorize on an unverified claim), as is an
	// entry with an empty Team or a duplicate Team. In dataPlane: Gateway mode
	// these compile to the authorization block of the SAME SecurityPolicy JWT
	// generates: one Allow rule per entry whose principal matches the verified
	// TeamClaim value (and, when the entry lists models, the resolved
	// x-ai-eg-model header).
	// +optional
	Allowlists []TeamModelAllowlist `json:"allowlists,omitempty"`
}

// TeamModelAllowlist grants one verified team access to a set of models. It is
// the unit of the router's authorization surface (RouterAuthSpec.Allowlists),
// compiled into one Allow rule of the SecurityPolicy authorization block in
// dataPlane: Gateway mode.
type TeamModelAllowlist struct {
	// Team is the verified teamClaim value this entry grants access to.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Team string `json:"team"`

	// Models is the set of model names this team may reach. Empty means the
	// team may reach all models (identity-only allow). Each value matches the
	// resolved model name (the x-ai-eg-model header), the same value
	// spec.rules[].match.models route on.
	// +optional
	Models []string `json:"models,omitempty"`
}

// JWTAuthSpec configures JWT validation and claim-to-header mapping. In
// dataPlane: Gateway mode it compiles to a SecurityPolicy jwt provider plus a
// claimToHeaders mapping. The mapped header is the trusted tenant identity that
// downstream budget enforcement keys on, so the gateway derives team identity
// from a verified token rather than a client-supplied header.
type JWTAuthSpec struct {
	// Provider is a short name for the JWT provider (e.g. "keycloak"). It labels
	// the provider in the generated SecurityPolicy.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Provider string `json:"provider"`

	// Issuer is the OIDC issuer URL that must match the token's "iss" claim.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Issuer string `json:"issuer"`

	// JWKSURI is the remote JWKS endpoint the gateway fetches signing keys from
	// to verify token signatures.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	JWKSURI string `json:"jwksURI"`

	// TeamClaim is the JWT claim that identifies the tenant (e.g. "team"). Its
	// verified value is copied into HeaderKey.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	TeamClaim string `json:"teamClaim"`

	// HeaderKey is the request header the verified TeamClaim value lands in.
	// Downstream team-scoped budgets key on this header. Defaults to
	// "x-llmkube-team", matching the budget default.
	// +optional
	HeaderKey string `json:"headerKey,omitempty"`
}

// BudgetSpec defines a token or dollar cap over a rolling window.
type BudgetSpec struct {
	// Name identifies this budget for metrics, status, and audit logs.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]{0,62}$`
	Name string `json:"name"`

	// Scope determines what the budget applies to.
	// "router" caps all traffic through this ModelRouter.
	// "rule" caps traffic matching a single named rule (see RuleName).
	// "team" caps traffic identified by a request header (see HeaderKey).
	// +kubebuilder:validation:Enum=router;rule;team
	Scope string `json:"scope"`

	// RuleName is required when Scope=rule. References a RouterRule.Name.
	// +optional
	RuleName string `json:"ruleName,omitempty"`

	// HeaderKey is the request header carrying the team identifier when
	// Scope=team. Defaults to "x-llmkube-team".
	// +optional
	HeaderKey string `json:"headerKey,omitempty"`

	// WindowSeconds is the rolling window over which the cap is evaluated.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3600
	// +optional
	WindowSeconds int32 `json:"windowSeconds,omitempty"`

	// MaxTokens caps total tokens (prompt + completion) over the window.
	// Either MaxTokens or MaxUSD (or both) must be set.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxTokens *int64 `json:"maxTokens,omitempty"`

	// MaxUSD caps total estimated cost in USD over the window. Cost is
	// computed from RouterBackend.CostPerMillionTokens.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +optional
	MaxUSD string `json:"maxUSD,omitempty"`
}

// ClassificationPolicy configures classification of inbound requests.
type ClassificationPolicy struct {
	// Mode determines how the router determines a request's
	// classification.
	// "header-only" (default) trusts the request header
	//   (HeaderKey, defaults to x-llmkube-classification).
	// "detector" runs the bundled in-proxy detector.
	// "hybrid" prefers the header, falling back to the detector when no
	//   header is present.
	// +kubebuilder:validation:Enum=header-only;detector;hybrid
	// +kubebuilder:default=header-only
	// +optional
	Mode string `json:"mode,omitempty"`

	// HeaderKey is the request header carrying the classification.
	// Defaults to "x-llmkube-classification".
	// +optional
	HeaderKey string `json:"headerKey,omitempty"`

	// SensitiveClassifications are the classification values that trigger
	// fail-closed validation: any rule matching one of these values must
	// have FailClosed=true and reference only local-tier backends.
	// Defaults to ["pii", "phi"].
	// +optional
	SensitiveClassifications []string `json:"sensitiveClassifications,omitempty"`
}

// AuditLogPolicy configures audit log emission.
type AuditLogPolicy struct {
	// Sink selects the audit-log destination.
	// "stdout" (default) emits one JSON object per line to the proxy
	//   container stdout, where it can be collected by the cluster log
	//   stack.
	// "file" writes to FilePath inside the proxy container.
	// "otlp" forwards entries to an OTel collector as log records.
	// +kubebuilder:validation:Enum=stdout;file;otlp
	// +kubebuilder:default=stdout
	// +optional
	Sink string `json:"sink,omitempty"`

	// FilePath is the destination when Sink=file. Must be writable inside
	// the router-proxy container. Defaults to "/var/log/mlx-router/audit.log".
	// +optional
	FilePath string `json:"filePath,omitempty"`

	// IncludeRequestBody, when true, includes the OpenAI request body in
	// every audit entry. Disabled by default for size and privacy.
	// +optional
	IncludeRequestBody bool `json:"includeRequestBody,omitempty"`

	// LogClientAddress, when true, adds the address of the request's sender
	// to every audit entry as client. It is the TCP peer: in-cluster callers
	// keep their pod IP through the ClusterIP. When the peer is in
	// TrustedProxyCIDRs, it is the first address in X-Forwarded-For, read
	// from the right, that is not a trusted proxy. Off by default, because a
	// client address is personal data in some deployments.
	// +optional
	LogClientAddress bool `json:"logClientAddress,omitempty"`

	// TrustedProxyCIDRs lists the proxies in front of the router (an
	// ingress, a gateway) whose X-Forwarded-For is believed, as CIDRs or
	// single addresses. Any client can send X-Forwarded-For, so with this
	// unset the header is ignored and client is always the TCP peer.
	// Requires logClientAddress. The controller validates the entries.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=50
	// +optional
	TrustedProxyCIDRs []string `json:"trustedProxyCIDRs,omitempty"`

	// CallerHeader names a request header whose value is added to every
	// audit entry as caller, for example X-LLMKube-Client set by each
	// client. The client chooses the value, so it is a label rather than an
	// identity. It is cut to 128 bytes, and characters outside printable
	// ASCII are replaced with '?'. A header that carries a credential (Authorization,
	// Proxy-Authorization, Cookie, X-Api-Key, Api-Key, X-Goog-Api-Key) is
	// refused.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9!#$%&'*+.^_|~-]+$`
	// +optional
	CallerHeader string `json:"callerHeader,omitempty"`
}

// RouterProxySpec overrides the managed router-proxy Deployment.
type RouterProxySpec struct {
	// Replicas is the desired number of router-proxy pods. Defaults to 1.
	// The proxy is stateless for routing decisions; budget and SLO
	// counters live in memory and reset on pod restart until the
	// persistence feature lands.
	//
	// When any backend resolves to a ModelPool member, the operator pins
	// this to 1 regardless of the value set here: ModelPool activation
	// serializes swaps through a single in-process lock, so a second proxy
	// replica would race it and thrash the shared GPU slot. Multi-replica
	// pooled routers need cross-replica swap coordination (a lease), tracked
	// as a follow-up.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// RevisionHistoryLimit caps how many old ReplicaSets the proxy Deployment
	// keeps for rollback. Unset keeps 3 old ReplicaSets; 0 keeps none.
	// Useful to bound ReplicaSet buildup, since the proxy re-rolls on every
	// config change.
	// +kubebuilder:validation:Minimum=0
	// +optional
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`

	// RuntimeClassName selects a Kubernetes RuntimeClass for the router-proxy
	// pod, for example a sandboxed runtime such as gVisor or Kata. Maps
	// directly to PodSpec.RuntimeClassName. A RuntimeClass with a scheduling
	// section has its node selector and tolerations merged into the pod by the
	// RuntimeClass admission controller, so no separate fields are needed.
	// Unset leaves the pod spec unchanged.
	// +optional
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`

	// Image overrides the default router-proxy container image. Useful
	// for air-gapped clusters that pin to an internal registry digest.
	// +optional
	Image string `json:"image,omitempty"`

	// Resources sets the pod's compute resource requests and limits.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// ImagePullSecrets are passed through to the router-proxy pod spec.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// QuarantineDuration controls how long the proxy keeps a backend in
	// the "skip" state after a 5xx or network error before allowing a
	// half-open probe. A pod of an endpoint-resolution backend is
	// quarantined after two such failures in a row, or at once when a
	// stream goes silent past streamIdleTimeout. Default 15s when unset. Shorter windows make the
	// proxy recover faster from transient blips; longer windows reduce
	// probe load on genuinely-down upstreams. Tests can shrink this to
	// sub-second to verify recovery without sleeping the full default.
	// +optional
	QuarantineDuration *metav1.Duration `json:"quarantineDuration,omitempty"`

	// ResponseHeaderTimeout caps how long the proxy waits for the
	// upstream to begin sending response headers. For non-streaming
	// chat completions this is effectively a max-generation-time
	// cap, because their headers arrive when generation completes. It
	// does not bound a streaming response once its headers arrive;
	// StreamIdleTimeout guards those. Default 120s when unset.
	// Per-rule and per-backend timeouts (see RouterRule.Timeout and
	// RouterBackend.Timeout) tighten this on a per-request basis but
	// cannot extend it beyond this cap.
	// +optional
	ResponseHeaderTimeout *metav1.Duration `json:"responseHeaderTimeout,omitempty"`

	// StreamIdleTimeout caps how long a response body may go without a
	// single byte from the upstream once its headers have arrived. A
	// healthy stream emits bytes continuously; the longest gap is the
	// prefill before the first content token, which can take minutes
	// for a very long prompt on slow hardware. When the window passes
	// with no bytes, the proxy aborts the request and quarantines that
	// backend endpoint, so a wedged replica stops receiving traffic.
	// Default 300s when unset; "0s" disables the guard.
	// +optional
	StreamIdleTimeout *metav1.Duration `json:"streamIdleTimeout,omitempty"`

	// DrainTimeout is how long a router-proxy replica keeps serving its
	// in-flight requests after SIGTERM, so a rollout finishes the streams
	// it holds instead of cutting them at the proxy's own default. It
	// sets the proxy's --shutdown-timeout and the pod's
	// terminationGracePeriodSeconds, which is this value plus a fixed
	// 5s margin so the kubelet does not SIGKILL before the drain ends.
	// A stream longer than this still ends at the drain deadline. The
	// proxy's default of 30s applies when unset, and nothing is rendered
	// on the pod template, so an operator upgrade does not roll existing
	// proxies. Changing it changes the pod template, so it rolls the
	// proxy pods, and a replica that drains stays Terminating for up to
	// this long, delaying that roll. Must be between 1s and 1h.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('1h')",message="drainTimeout must be between 1s and 1h"
	// +optional
	DrainTimeout *metav1.Duration `json:"drainTimeout,omitempty"`

	// NodeSelector pins the router-proxy pod to nodes matching these labels.
	// Use it when the proxy image is only present on specific nodes (for
	// example a node-local registry) or to co-locate the proxy with its
	// backends. Passthrough to the Pod spec.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations are applied to the router-proxy pod, so it can be pinned
	// next to tainted GPU nodes together with nodeSelector.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity is passed through to the router-proxy pod, for node affinity
	// or pod anti-affinity against the proxy's own replicas. The proxy pods
	// carry the stable labels app: <router>-router-proxy and
	// inference.llmkube.dev/model-router: <router> to select on.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// TopologySpreadConstraints are passed through to the router-proxy pod,
	// for example one replica per node with topologyKey
	// kubernetes.io/hostname and the labels above. When replicas is more
	// than 1 and a constraint uses whenUnsatisfiable: DoNotSchedule (or the
	// affinity requires pod anti-affinity), the proxy rolls out with
	// maxSurge 0 and maxUnavailable 1, because a surge pod could never be
	// placed.
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// PodLabels are merged into the router-proxy pod's metadata.labels, for
	// example so label-based NetworkPolicies select the proxy. The operator's
	// own labels (app, inference.llmkube.dev/model-router) win on collision,
	// and the Deployment and Service selectors use only those, so changing
	// PodLabels rolls the pods without touching a selector. A label removed
	// here is removed from the pods; labels other tools add are kept.
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// PodAnnotations are merged into the router-proxy pod's
	// metadata.annotations. The operator's own annotations (the config hash,
	// and the prometheus.io scrape annotations when the operator emits them)
	// win on collision. An annotation removed here is removed from the pods;
	// annotations other tools add are kept.
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
}

// MCPServerSpec configures a Model Context Protocol endpoint on the
// router-proxy. Reserved for the Phase 3 MCP feature; setting it before
// that feature ships has no runtime effect.
type MCPServerSpec struct {
	// Enabled toggles MCP exposure. Default false. When true (after Phase
	// 3 lands), the router-proxy serves an MCP endpoint at /mcp using
	// Streamable HTTP transport and OAuth 2.1.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// AgentGatewayRouting selects what a dataPlane: AgentGateway ModelRouter
// compiles to.
type AgentGatewayRouting string

const (
	AgentGatewayRoutingInferencePool AgentGatewayRouting = "InferencePool"
	AgentGatewayRoutingModel         AgentGatewayRouting = "Model"
)

// AgentGatewayReference identifies the pre-installed agentgateway data plane a
// dataPlane: AgentGateway ModelRouter compiles onto. The Gateway is the
// gateway.networking.k8s.io Gateway whose GatewayClass is agentgateway; the
// EndpointPicker is the Endpoint Picker (EPP) Service that selects a model
// server endpoint per request for each generated InferencePool. The Gateway,
// the Gateway API Inference Extension CRDs, and a running EPP are documented
// prerequisites that LLMKube does not install or own. With routing: Model only
// the Gateway and agentgateway v1.6.0 or later are.
// +kubebuilder:validation:XValidation:rule="has(self.endpointPicker) == has(self.endpointPickerPort)",message="endpointPicker and endpointPickerPort must be set together"
// +kubebuilder:validation:XValidation:rule="!(has(self.routing) && self.routing == 'Model') || !has(self.endpointPicker)",message="endpointPicker and endpointPickerPort apply only to routing: InferencePool"
// +kubebuilder:validation:XValidation:rule="!(has(self.routing) && self.routing == 'Model') || has(self.sectionName) || has(self.hostnames)",message="routing: Model requires sectionName or hostnames: its HTTPRoute matches every path, so it must not take over a whole Gateway"
// +kubebuilder:validation:XValidation:rule="(has(self.routing) && self.routing == 'Model') || (!has(self.sectionName) && !has(self.hostnames))",message="sectionName and hostnames apply only to routing: Model"
type AgentGatewayReference struct {
	// Name is the agentgateway Gateway's name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Routing selects the compile target. "InferencePool" (the default) is the
	// Gateway API Inference Extension path described on spec.agentGatewayRef.
	// "Model" uses agentgateway's model router (v1.6.0+): one AgentgatewayModel
	// per backend, matched on its displayName (else name), behind one
	// catch-all HTTPRoute. It needs no Inference Extension CRDs or Endpoint
	// Picker. spec.rules, spec.defaultRoute, external backends, resolution:
	// endpoint, and per-backend endpoint pickers are refused in this mode.
	// +kubebuilder:validation:Enum=InferencePool;Model
	// +kubebuilder:default=InferencePool
	// +optional
	Routing AgentGatewayRouting `json:"routing,omitempty"`

	// SectionName names the Gateway listener the generated HTTPRoute attaches
	// to. routing: Model only, where it or hostnames is required, because the
	// route takes every path on its listeners.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	SectionName string `json:"sectionName,omitempty"`

	// Hostnames limit the generated HTTPRoute to these hosts. routing: Model
	// only, where it or sectionName is required.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^(\*\.)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	Hostnames []string `json:"hostnames,omitempty"`

	// Namespace is the Gateway's namespace. Empty means the ModelRouter's own
	// namespace. A different namespace is supported: the generated
	// model-extraction AgentgatewayPolicy then lives in the Gateway's
	// namespace (it cannot be owner-referenced across namespaces and is
	// cleaned up via a finalizer on this ModelRouter), while the pools and
	// route stay here. The pools, the referenced InferenceServices, and the
	// EndpointPicker Service all live in this ModelRouter's namespace
	// regardless.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// EndpointPicker names the router-wide default EPP Service. A backend's
	// own endpointPicker overrides it. A Gateway API Inference Extension EPP
	// binds to exactly one InferencePool, so the default serves a router whose
	// backends all reference one InferenceService; with several, set
	// spec.backends[].endpointPicker. The Service lives in this ModelRouter's
	// namespace: GIE v1's endpointPickerRef has no namespace field.
	// +optional
	EndpointPicker string `json:"endpointPicker,omitempty"`

	// EndpointPickerPort is the port the default EPP Service listens on. Set
	// together with endpointPicker.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	EndpointPickerPort int32 `json:"endpointPickerPort,omitempty"`
}

// Balancing modes for RouterBackend.balancing.mode.
const (
	BalancingModeRoundRobin = "RoundRobin"
	BalancingModeLoadAware  = "LoadAware"
)

// BackendBalancing is a backend's placement policy.
type BackendBalancing struct {
	// Mode is RoundRobin (today's behavior) or LoadAware: the pod with the
	// least work, read from the engine's own metrics where LLMKube has a
	// profile for the runtime (vLLM) and from in-flight requests otherwise.
	// +kubebuilder:validation:Enum=RoundRobin;LoadAware
	// +kubebuilder:default=RoundRobin
	// +optional
	Mode string `json:"mode,omitempty"`

	// SessionAffinity places a new session on the least-loaded pod. The session
	// stays there unless that pod is queuing and clearly busier than another
	// pod, and then it stays where it moved. It requires mode: LoadAware; with
	// RoundRobin it is ignored and the router emits a SessionAffinityIgnored
	// warning event.
	// +optional
	SessionAffinity *SessionAffinity `json:"sessionAffinity,omitempty"`
}

// SessionAffinity identifies sessions and sets how long the router remembers
// the pod each one was placed on.
type SessionAffinity struct {
	// Headers identify a session, in priority order; the first present with a
	// non-empty value is the key. Compared case-insensitively. Only a hash of
	// the value is used. Each name is an RFC 7230 header token.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern="^[A-Za-z0-9!#$%&'*+.^_|~\x60-]+$"
	Headers []string `json:"headers"`

	// IdleTimeout is how long a session may go without a request before the
	// router forgets which pod it was on. The next request from that session
	// is placed again by load. Defaults to 30m when unset. Must be between
	// 1m and 24h.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m') && duration(self) <= duration('24h')",message="idleTimeout must be between 1m and 24h"
	// +optional
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`

	// MaxLoadPercent is deprecated and has no effect since this release:
	// sessions are now placed from the session binding table, not spilled by a
	// load threshold. It is still accepted so existing manifests keep
	// applying, and will be removed in a later API version.
	// +kubebuilder:validation:Minimum=101
	// +kubebuilder:validation:Maximum=1000
	// +optional
	MaxLoadPercent int32 `json:"maxLoadPercent,omitempty"`
}

// RouterEndpointPicker selects the Endpoint Picker for one backend's
// InferencePool. Exactly one member is set.
// +kubebuilder:validation:XValidation:rule="has(self.service)",message="endpointPicker.service is required"
type RouterEndpointPicker struct {
	// Service names an EPP Service in the ModelRouter's namespace. GIE v1's
	// endpointPickerRef has no namespace field, so the pool references it
	// same-namespace.
	// +optional
	Service *EndpointPickerService `json:"service,omitempty"`
}

// EndpointPickerService is an EPP Service and the port agentgateway dials.
type EndpointPickerService struct {
	// Name is the EPP Service's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Port is the port the EPP Service listens on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// ModelRouterStatus reports the observed state of a ModelRouter.
type ModelRouterStatus struct {
	// Phase is a coarse summary of the router's state.
	// Possible values: Pending, Provisioning, Ready, Degraded, Failed.
	// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Degraded;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Endpoint is the in-cluster URL clients should hit. Populated once
	// the router-proxy Service is ready.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Backends reports the resolved address and current health of every
	// declared backend.
	// +optional
	Backends []BackendStatus `json:"backends,omitempty"`

	// ActiveRules is the count of rules that successfully validated
	// against current backend state.
	// +optional
	ActiveRules int32 `json:"activeRules,omitempty"`

	// BudgetUtilization summarises current budget consumption.
	// +optional
	BudgetUtilization []BudgetStatus `json:"budgetUtilization,omitempty"`

	// LastUpdated is the timestamp of the last status reconciliation.
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// Gateway reports the observed state of dataPlane: Gateway exposure: whether
	// the AIGatewayRoute (and its backing Backend / AIServiceBackend /
	// BackendTrafficPolicy) reconciled, and the resolved gateway endpoint. nil
	// in Proxy mode. Also surfaced via the GatewayReady condition.
	// +optional
	Gateway *GatewayStatus `json:"gateway,omitempty"`

	// AgentGateway reports the observed state of dataPlane: AgentGateway
	// exposure: whether the InferencePool / HTTPRoute / model-extraction
	// AgentgatewayPolicy reconciled
	// against the referenced agentgateway Gateway, and the resolved endpoint.
	// nil in Proxy and Gateway modes. Also surfaced via the AgentGatewayReady
	// condition.
	// +optional
	AgentGateway *AgentGatewayStatus `json:"agentGateway,omitempty"`

	// conditions represent the current state of the ModelRouter resource.
	//
	// Standard condition types:
	// - "Validated":     the spec passed static validation
	// - "BackendsReady": all referenced backends are reachable and healthy
	// - "Available":     the router-proxy is serving traffic
	// - "Degraded":      at least one backend is unhealthy but the router
	//                    can still serve other routes
	// - "GatewayReady":  (dataPlane: Gateway) the gateway resources reconciled
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// BackendStatus reports the runtime state of one declared backend.
type BackendStatus struct {
	// Name matches RouterBackend.Name.
	Name string `json:"name"`

	// Tier mirrors RouterBackend.Tier for convenience.
	// +optional
	Tier string `json:"tier,omitempty"`

	// Address is the resolved upstream URL the router-proxy dispatches to.
	// For local backends this is the InferenceService's cluster URL; for
	// external backends it is the provider's base URL.
	// +optional
	Address string `json:"address,omitempty"`

	// Healthy reflects the most recent probe result.
	// +optional
	Healthy bool `json:"healthy,omitempty"`

	// Message provides extra context, especially when Healthy is false
	// (e.g. "InferenceService not Ready", "Secret missing key
	// ANTHROPIC_API_KEY").
	// +optional
	Message string `json:"message,omitempty"`

	// LastProbeTime is when the proxy last completed a health probe for
	// this backend.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
}

// BudgetStatus reports current consumption against a declared budget.
type BudgetStatus struct {
	// Name matches BudgetSpec.Name.
	Name string `json:"name"`

	// TokensUsed is the rolling-window token count.
	// +optional
	TokensUsed int64 `json:"tokensUsed,omitempty"`

	// USDUsed is the rolling-window estimated cost in USD.
	// +optional
	USDUsed string `json:"usdUsed,omitempty"`

	// Utilization is the fraction of the budget consumed, 0.0 to 1.0.
	// When both MaxTokens and MaxUSD are set this is the maximum of the
	// two utilizations.
	// +optional
	Utilization string `json:"utilization,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mr
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Rules",type=integer,JSONPath=`.status.activeRules`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelRouter is the Schema for the modelrouters API. It exposes a single
// OpenAI-compatible HTTP endpoint that dispatches requests across multiple
// InferenceService backends and external providers per declarative routing
// rules. See docs/site/concepts/model-router.md (Phase 1) for usage.
type ModelRouter struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of ModelRouter
	// +required
	Spec ModelRouterSpec `json:"spec"`

	// status defines the observed state of ModelRouter
	// +optional
	Status ModelRouterStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ModelRouterList contains a list of ModelRouter
type ModelRouterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelRouter `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelRouter{}, &ModelRouterList{})
}
