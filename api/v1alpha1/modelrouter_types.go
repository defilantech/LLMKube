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

// ModelRouterDataPlane selects how a ModelRouter serves traffic.
type ModelRouterDataPlane string

const (
	// ModelRouterDataPlaneProxy provisions the managed router-proxy Deployment
	// and routes in-process. This is the default and preserves today's behavior.
	ModelRouterDataPlaneProxy ModelRouterDataPlane = "Proxy"

	// ModelRouterDataPlaneGateway compiles the router policy onto a pre-installed
	// Envoy AI Gateway instead of provisioning the router-proxy.
	ModelRouterDataPlaneGateway ModelRouterDataPlane = "Gateway"
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
	// +kubebuilder:validation:Enum=Proxy;Gateway
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

	// Backends are the candidate destinations the router can dispatch to.
	// Order is not significant; selection is rule-driven. At least one
	// backend must be declared.
	// +kubebuilder:validation:MinItems=1
	Backends []RouterBackend `json:"backends"`

	// Rules are evaluated in declaration order. The first matching rule wins.
	// If no rule matches, DefaultRoute is used. If neither a matching rule
	// nor DefaultRoute is set, the request is rejected with HTTP 503.
	// +optional
	Rules []RouterRule `json:"rules,omitempty"`

	// DefaultRoute names a backend used when no rule matches.
	// Must reference the Name of an entry in Backends.
	// +optional
	DefaultRoute string `json:"defaultRoute,omitempty"`

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
type RouterBackend struct {
	// Name is the stable identifier used by rules and observability labels.
	// Must be lowercase alphanumeric or '-'.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]{0,62}$`
	Name string `json:"name"`

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

	// Capabilities advertised by this backend. Rules can require
	// capabilities (e.g. ["tools", "vision", "long-context"]) to filter
	// candidates.
	// +optional
	Capabilities []string `json:"capabilities,omitempty"`

	// Weight is used for the "weighted" routing strategy. Higher values
	// receive proportionally more traffic. Ignored for other strategies.
	// Default 1 when unset.
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
	// capability.
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
	// "shadow", the first entry serves the response and subsequent entries
	// receive mirrored requests for evaluation only.
	// +kubebuilder:validation:MinItems=1
	Backends []string `json:"backends"`

	// Strategy selects how multiple backends are used.
	// +kubebuilder:validation:Enum=primary-fallback;weighted;shadow
	// +kubebuilder:default=primary-fallback
	// +optional
	Strategy string `json:"strategy,omitempty"`
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
}

// RouterProxySpec overrides the managed router-proxy Deployment.
type RouterProxySpec struct {
	// Replicas is the desired number of router-proxy pods. Defaults to 1.
	// The proxy is stateless for routing decisions; budget and SLO
	// counters live in memory and reset on pod restart until the
	// persistence feature lands.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

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
	// half-open probe. Default 15s when unset. Shorter windows make the
	// proxy recover faster from transient blips; longer windows reduce
	// probe load on genuinely-down upstreams. Tests can shrink this to
	// sub-second to verify recovery without sleeping the full default.
	// +optional
	QuarantineDuration *metav1.Duration `json:"quarantineDuration,omitempty"`

	// ResponseHeaderTimeout caps how long the proxy waits for the
	// upstream to begin sending response headers. For non-streaming
	// chat completions this is effectively a max-generation-time
	// cap; for streaming dispatches the first SSE chunk arrives well
	// inside the window so the cap is invisible. Default 120s when
	// unset. Per-rule and per-backend timeouts (see RouterRule.Timeout
	// and RouterBackend.Timeout) tighten this on a per-request basis
	// but cannot extend it beyond this cap.
	// +optional
	ResponseHeaderTimeout *metav1.Duration `json:"responseHeaderTimeout,omitempty"`
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
