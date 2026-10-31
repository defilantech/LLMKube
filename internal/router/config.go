/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package router implements the LLMKube router-proxy: a small HTTP server
// that fronts multiple LLM backends (in-cluster InferenceServices, external
// providers like Anthropic / OpenAI / a LiteLLM proxy) and dispatches
// OpenAI-compatible chat completion requests across them according to
// declarative rules.
//
// The router-proxy is intentionally decoupled from the ModelRouter CRD
// types in api/v1alpha1. The ModelRouterReconciler (internal/controller)
// compiles a ModelRouter spec into a flat JSON config shape and writes it
// to a ConfigMap; the proxy reads that config from disk on startup. This
// keeps the binary small (no kubebuilder/client-go in the inference hot
// path) and makes the proxy testable in isolation.
package router

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// Default-route strategies. These mirror the CRD enum and decide what the
// matcher does for a request that no rule accepted.
const (
	// DefaultRouteStrategyStatic routes unmatched requests straight to
	// DefaultRoute. The zero value, so an absent field behaves this way.
	DefaultRouteStrategyStatic = "Static"

	// DefaultRouteStrategyBackendNameMatch tries to match the request model
	// to a backend Name before falling back to DefaultRoute.
	DefaultRouteStrategyBackendNameMatch = "BackendNameMatch"
)

// Config is the on-disk representation read by the router-proxy. The
// controller produces this from a ModelRouter spec; the wire shape is a
// stable contract between the two components.
type Config struct {
	// Backends is the resolved list of dispatch destinations.
	Backends []Backend `json:"backends"`

	// Rules are evaluated in declaration order on every request. The first
	// rule whose Match expression accepts the request wins.
	Rules []Rule `json:"rules,omitempty"`

	// DefaultRoute names the backend used when no rule matches.
	DefaultRoute string `json:"defaultRoute,omitempty"`

	// DefaultRouteStrategy decides how unmatched requests resolve before
	// DefaultRoute. Empty or "Static" routes straight to DefaultRoute;
	// "BackendNameMatch" first tries to match the request model to a backend
	// Name. See the DefaultRouteStrategy* constants.
	DefaultRouteStrategy string `json:"defaultRouteStrategy,omitempty"`

	// Policy holds cross-cutting controls (classification, audit, budgets).
	// Budget persistence lands in #440.
	Policy Policy `json:"policy"`
}

// Backend is one dispatch destination. The controller resolves an
// InferenceServiceRef to an in-cluster URL before writing the ConfigMap;
// the proxy only ever sees resolved addresses.
type Backend struct {
	Name string `json:"name"`

	// DisplayName is the user-facing model id published on /v1/models
	// and used by BackendNameMatch to resolve a request's model field.
	// When empty, Name is used for both purposes (current behavior).
	DisplayName string `json:"displayName,omitempty"`

	// ModelInfo is published on this backend's /v1/models entry. Nil
	// publishes the bare entry.
	ModelInfo *ModelInfo `json:"modelInfo,omitempty"`

	// Tier is "local" or "cloud". Used by the fail-closed gate: sensitive-
	// data rules cannot route to cloud-tier backends.
	Tier string `json:"tier"`

	// Address is the upstream base URL the proxy sends requests to. For
	// local backends this is the InferenceService cluster URL (e.g.
	// http://qwen3-coder.platform.svc.cluster.local:8080). For external
	// providers it is the provider base URL.
	Address string `json:"address"`

	// Endpoints are the per-pod base URLs the proxy balances requests
	// across when a backend declares endpoint resolution. When non-empty
	// they supersede Address for dispatch; Address remains the fallback
	// (the Service DNS) for a backend whose endpoint set is empty.
	Endpoints []string `json:"endpoints,omitempty"`

	// EndpointSource names the Service whose ready pods the proxy balances
	// across, for a backend that declares endpoint resolution. The proxy
	// watches the Service's EndpointSlices and keeps the set current; Address
	// stays the fallback while the set is empty or the watch has not synced.
	// When both are present, EndpointSource wins over Endpoints.
	EndpointSource *EndpointSource `json:"endpointSource,omitempty"`

	// Balancing selects how the proxy places requests across an
	// endpoint-resolution backend's pods. Nil keeps round-robin.
	Balancing *Balancing `json:"balancing,omitempty"`

	// Metrics names the engine metrics the proxy scrapes from each pod for
	// LoadAware placement. Set by the controller only for runtimes with a
	// known profile; without it, LoadAware uses in-flight counts alone.
	Metrics *MetricsProfile `json:"metrics,omitempty"`

	// Provider identifies the upstream API surface for external backends
	// ("anthropic", "openai", "litellm", etc.). Empty for local backends.
	Provider string `json:"provider,omitempty"`

	// Model is the upstream model identifier passed to the provider.
	// Empty for local backends (the request body carries the model name).
	Model string `json:"model,omitempty"`

	// ServedModel is the model identifier the upstream engine actually
	// serves. Set for vLLM and SGLang backends (their --served-model-name,
	// by default the modelRef) and for oMLX backends, whose engine serves
	// the model-store directory basename. Empty for other backends, where
	// InferenceService is the fallback.
	ServedModel string `json:"servedModel,omitempty"`

	// InferenceService is the name of the InferenceService this backend
	// resolves to. The proxy uses it to rewrite the outbound "model" field
	// when a request reaches this backend under a different client-facing
	// alias (a rule fall-through, an IfIdle skip) and ServedModel is empty;
	// llama.cpp ignores the field. Empty for external backends, which carry
	// Model instead.
	InferenceService string `json:"inferenceService,omitempty"`

	// Capabilities advertised by this backend (e.g. ["tools", "vision"]).
	// Rules can require capabilities to filter candidates.
	Capabilities []string `json:"capabilities,omitempty"`

	// Weight is this backend's share of a "weighted" rule's traffic. Nil
	// (absent) means 1, the CRD default. 0 drains the backend from weighted
	// rules: it takes no traffic from them, even when the rule's other
	// backends are all down, the Gateway API meaning of a zero weight. Other
	// strategies ignore it. Compiled from ModelRouter.spec.backends[].weight.
	Weight *int32 `json:"weight,omitempty"`

	// CredentialsEnv names an environment variable holding the bearer or
	// API key for this backend. The proxy reads the value at request time
	// and injects it into the upstream request as Authorization or
	// x-api-key, depending on Provider. Empty for local backends.
	CredentialsEnv string `json:"credentialsEnv,omitempty"`

	// Timeout caps how long the proxy waits for THIS backend to begin
	// sending response headers. Zero means "use the proxy default".
	// Compiled from ModelRouter.spec.backends[].timeout. Per-rule
	// timeouts (see Rule.Timeout) win over this.
	Timeout time.Duration `json:"timeout,omitempty"`

	// Pool is set when this backend's InferenceService is a member of a
	// ModelPool: N single-model services sharing one exclusive GPU slot.
	// When present and the proxy runs with activation enabled, the proxy
	// makes this member resident (scaling it up and draining the incumbent)
	// before dispatching. Nil for backends that are not pooled.
	Pool *BackendPool `json:"pool,omitempty"`

	// CostPerMillionTokens prices this backend's traffic for USD budget
	// accounting. Nil means the proxy cannot compute cost for this backend
	// and charges 0 USD against dollar budgets. Compiled from
	// ModelRouter.spec.backends[].costPerMillionTokens.
	CostPerMillionTokens *TokenCost `json:"costPerMillionTokens,omitempty"`
}

// EndpointSource identifies the Service behind an endpoint-resolution backend.
type EndpointSource struct {
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	Port      int32  `json:"port"`
}

// Balancing modes.
const (
	BalancingRoundRobin = "RoundRobin"
	BalancingLoadAware  = "LoadAware"
)

// Bounds on Balancing.SessionIdleTimeout, mirroring the CRD's validation.
const (
	minSessionIdleTimeout = time.Minute
	maxSessionIdleTimeout = 24 * time.Hour
)

// Balancing is a backend's placement policy.
type Balancing struct {
	// Mode is RoundRobin (also when empty) or LoadAware.
	Mode string `json:"mode,omitempty"`
	// SessionHeaders name the request headers that identify a session, in
	// priority order; the first present with a non-empty value is the key.
	SessionHeaders []string `json:"sessionHeaders,omitempty"`
	// SessionIdleTimeout is how long a session's pod binding survives with
	// no request. 0 means the picker's default. A 0.10.7 controller also
	// writes maxLoadPercent; the field is no longer read and is ignored on
	// decode.
	SessionIdleTimeout time.Duration `json:"sessionIdleTimeout,omitempty"`
}

// HasLoadAware reports whether any backend balances LoadAware, so the proxy
// needs the prefill janitor.
func (c *Config) HasLoadAware() bool {
	for _, b := range c.Backends {
		if b.Balancing != nil && b.Balancing.Mode == BalancingLoadAware {
			return true
		}
	}
	return false
}

// HasSessionAffinity reports whether any LoadAware backend names session
// headers, so the proxy needs the session sweeper.
func (c *Config) HasSessionAffinity() bool {
	for _, b := range c.Backends {
		if b.Balancing != nil && b.Balancing.Mode == BalancingLoadAware && len(b.Balancing.SessionHeaders) > 0 {
			return true
		}
	}
	return false
}

// MetricsProfile names the engine gauges the proxy reads from each pod.
type MetricsProfile struct {
	Path    string `json:"path"`
	Running string `json:"running"`
	Waiting string `json:"waiting"`
	KVUsage string `json:"kvUsage,omitempty"`
}

// TokenCost prices prompt and completion tokens per million, in USD. The
// controller parses the CRD's decimal-string form into these floats.
type TokenCost struct {
	// PromptUSD is the cost per million prompt (input) tokens.
	PromptUSD float64 `json:"promptUSD,omitempty"`

	// CompletionUSD is the cost per million completion (output) tokens.
	CompletionUSD float64 `json:"completionUSD,omitempty"`
}

// ModelInfo is client-facing model metadata, compiled from
// ModelRouter.spec.backends[].modelInfo. The controller fills both modality
// lists, so the proxy publishes them as they are.
type ModelInfo struct {
	InputModalities  []string `json:"inputModalities,omitempty"`
	OutputModalities []string `json:"outputModalities,omitempty"`
	ContextLength    int64    `json:"contextLength,omitempty"`
}

// BackendPool carries the ModelPool membership a pooled backend belongs to.
// The controller compiles it from the ModelPool CR; the proxy uses it to
// activate the member on demand and to enforce the sticky, anti-thrash swap
// policy across the shared slot.
type BackendPool struct {
	// Name is the ModelPool name.
	Name string `json:"name"`

	// Namespace is the ModelPool (and member InferenceService) namespace.
	Namespace string `json:"namespace"`

	// Member is this backend's member InferenceService name within the pool.
	Member string `json:"member"`

	// Members lists every member InferenceService name in the pool. The proxy
	// uses it to seed which member is currently resident on a cold start.
	Members []string `json:"members,omitempty"`

	// SwapBudget bounds how long the proxy holds a cross-model request open
	// while it drains the incumbent and cold-loads this member, decoupled from
	// the response-header (generation) timeout so a slow cold load does not
	// force operators to inflate the generation cap. Compiled from
	// ModelPool.spec.swapBudget. Zero means fall back to the dispatch default.
	SwapBudget time.Duration `json:"swapBudget,omitempty"`
}

// Rule pairs a Match expression with a Route action.
type Rule struct {
	Name  string    `json:"name"`
	Match RuleMatch `json:"match,omitempty"`
	Route RuleRoute `json:"route"`

	// FailClosed, when true, rejects the request with HTTP 503 if no
	// backend in Route.Backends is healthy or eligible. The regulated-
	// data gate that prevents fallback to cloud on local outage.
	FailClosed bool `json:"failClosed,omitempty"`

	// Timeout caps how long the proxy waits for the upstream to begin
	// sending response headers on dispatches matched by this rule.
	// Zero means "fall back to backend.Timeout, then proxy default".
	// Compiled from ModelRouter.spec.rules[].timeout. Per-rule timeout
	// is the strictest level of override (wins over backend + proxy
	// default) so platform policy (eg PII fail-fast) can't be defeated
	// by a generous backend-level timeout.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// RuleMatch declares the conditions under which a Rule fires. All declared
// fields are ANDed. Empty fields are not considered.
type RuleMatch struct {
	// DataClassification matches when the request's classification (from
	// header, see Policy.Classification) is one of these values.
	DataClassification []string `json:"dataClassification,omitempty"`

	// TaskComplexity matches when the inbound x-llmkube-task-complexity
	// header equals this value.
	TaskComplexity string `json:"taskComplexity,omitempty"`

	// RequiredCapabilities filters Route.Backends to those advertising
	// every listed capability. The rule matches when at least one backend
	// qualifies, and every strategy dispatches only to the ones that do.
	RequiredCapabilities []string `json:"requiredCapabilities,omitempty"`

	// Headers performs exact-match equality on inbound headers.
	Headers map[string]string `json:"headers,omitempty"`

	// Models matches against the OpenAI "model" field in the request
	// body. Glob patterns are supported (e.g. "qwen3-*").
	Models []string `json:"models,omitempty"`
}

// RuleRoute names the backends a matched rule dispatches to and how.
type RuleRoute struct {
	Backends []string `json:"backends"`

	// Strategy is "primary-fallback" (default), "weighted", or "shadow".
	// primary-fallback tries Backends in declared order. weighted draws a
	// backend per request in proportion to Backend.Weight among the ones
	// that can serve now, and on a failure fails over to another drawn the
	// same way (see weightedOrder). shadow is not implemented by the proxy
	// yet and dispatches as primary-fallback.
	Strategy string `json:"strategy,omitempty"`

	// PoolActivation is "Wait" (default) or "IfIdle". Under IfIdle a pooled
	// backend whose incumbent is busy is skipped rather than held, so the
	// dispatch loop falls through to the next backend in Backends. Compiled
	// from ModelRouter.spec.rules[].route.poolActivation.
	PoolActivation string `json:"poolActivation,omitempty"`
}

// Pool activation modes for RuleRoute.PoolActivation. See the CRD field
// documentation for the semantics of each.
const (
	PoolActivationWait   = "Wait"
	PoolActivationIfIdle = "IfIdle"
)

// Budget scope kinds. These mirror the ModelRouterBudgetSpec.Scope enum and
// decide how the proxy resolves a request into scope keys.
const (
	BudgetScopeRouter = "router"
	BudgetScopeRule   = "rule"
	BudgetScopeTeam   = "team"
)

// DefaultTeamHeaderKey is the request header carrying the team identifier
// for a team-scoped budget when Budget.HeaderKey is unset. Mirrors the CRD
// default.
const DefaultTeamHeaderKey = "x-llmkube-team"

// Policy holds cross-cutting controls.
type Policy struct {
	Classification ClassificationPolicy `json:"classification"`
	AuditLog       AuditLogPolicy       `json:"auditLog"`

	// Budgets are the compiled caps the proxy enforces synchronously in the
	// request path. Empty means no budget enforcement. Compiled from
	// ModelRouter.spec.policy.budgets.
	Budgets []Budget `json:"budgets,omitempty"`
}

// Budget is one compiled cap over a rolling window. The proxy enforces it
// in the request path: a request whose resolved scope keys exhaust any cap
// is rejected with HTTP 429 before dispatch.
type Budget struct {
	// Name identifies this budget for metrics and audit logs.
	Name string `json:"name"`

	// Scope is "router", "rule", or "team".
	Scope string `json:"scope"`

	// RuleName names the rule a rule-scoped budget caps. Only set for
	// Scope=rule.
	RuleName string `json:"ruleName,omitempty"`

	// HeaderKey names the request header carrying the team identifier for a
	// team-scoped budget. The proxy resolves it per request into a concrete
	// "team:<headerKey>:<value>" scope key. Defaults to
	// DefaultTeamHeaderKey when empty.
	HeaderKey string `json:"headerKey,omitempty"`

	// Window is the rolling window over which the cap is evaluated.
	Window time.Duration `json:"window"`

	// MaxTokens caps total tokens (prompt + completion) over the window.
	// Zero means no token cap.
	MaxTokens int64 `json:"maxTokens,omitempty"`

	// MaxUSD caps total estimated cost in USD over the window. Zero means no
	// USD cap.
	MaxUSD float64 `json:"maxUSD,omitempty"`
}

// ClassificationPolicy configures how the proxy determines the
// classification of an inbound request. Currently "header-only" is the
// only fully-implemented mode; "detector" / "hybrid" land in #441.
type ClassificationPolicy struct {
	Mode      string   `json:"mode"`
	HeaderKey string   `json:"headerKey,omitempty"`
	Sensitive []string `json:"sensitiveClassifications,omitempty"`
}

// AuditLogPolicy configures audit log emission. The proxy always emits
// one structured log line per routing decision; this configures the sink
// and the opt-in client attribution on that line.
type AuditLogPolicy struct {
	Sink               string   `json:"sink"`
	FilePath           string   `json:"filePath,omitempty"`
	IncludeRequestBody bool     `json:"includeRequestBody,omitempty"`
	LogClientAddress   bool     `json:"logClientAddress,omitempty"`
	TrustedProxyCIDRs  []string `json:"trustedProxyCIDRs,omitempty"`
	CallerHeader       string   `json:"callerHeader,omitempty"`
}

// LoadConfig reads and parses a router-proxy config from path. The proxy
// reads its config once at startup; hot-reload via file-watcher lands in
// a follow-up (the controller already triggers a pod rollout when the
// ConfigMap changes, so for the MVP a restart is sufficient).
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from a flag, not user input
	if err != nil {
		return nil, fmt.Errorf("read router config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse router config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid router config %s: %w", path, err)
	}
	return &cfg, nil
}

// Validate performs runtime sanity checks beyond what the controller's
// static validation enforces. The controller validates the CRD; this
// catches drift between controller and proxy (e.g. a manually edited
// ConfigMap on a debugging cluster) and protects the dispatch loop from
// surprises.
func (c *Config) Validate() error {
	if len(c.Backends) == 0 {
		return fmt.Errorf("backends must be non-empty")
	}
	names := make(map[string]bool, len(c.Backends))
	for i, b := range c.Backends {
		if b.Name == "" {
			return fmt.Errorf("backends[%d]: name is required", i)
		}
		if names[b.Name] {
			return fmt.Errorf("backends[%d]: duplicate name %q", i, b.Name)
		}
		names[b.Name] = true
		if b.Tier != "local" && b.Tier != "cloud" {
			return fmt.Errorf("backends[%d] %s: tier must be local or cloud, got %q", i, b.Name, b.Tier)
		}
		if b.Address == "" && len(b.Endpoints) == 0 {
			return fmt.Errorf("backends[%d] %s: address is required", i, b.Name)
		}
		if s := b.EndpointSource; s != nil && (s.Namespace == "" || s.Service == "" || s.Port <= 0) {
			return fmt.Errorf("backends[%d] %s: endpointSource needs namespace, service and a positive port", i, b.Name)
		}
		if err := validateBalancing(i, b); err != nil {
			return err
		}
		if b.Weight != nil && *b.Weight < 0 {
			return fmt.Errorf("backends[%d] %s: weight must not be negative, got %d", i, b.Name, *b.Weight)
		}
	}
	if c.DefaultRoute != "" && !names[c.DefaultRoute] {
		return fmt.Errorf("defaultRoute %q does not name an existing backend", c.DefaultRoute)
	}
	switch c.DefaultRouteStrategy {
	case "", DefaultRouteStrategyStatic, DefaultRouteStrategyBackendNameMatch:
	default:
		return fmt.Errorf("defaultRouteStrategy %q must be %q or %q",
			c.DefaultRouteStrategy, DefaultRouteStrategyStatic, DefaultRouteStrategyBackendNameMatch)
	}
	ruleNames := make(map[string]bool, len(c.Rules))
	for i, r := range c.Rules {
		if r.Name == "" {
			return fmt.Errorf("rules[%d]: name is required", i)
		}
		ruleNames[r.Name] = true
		if len(r.Route.Backends) == 0 {
			return fmt.Errorf("rules[%d] %s: route.backends must be non-empty", i, r.Name)
		}
		for j, name := range r.Route.Backends {
			if !names[name] {
				return fmt.Errorf("rules[%d] %s: route.backends[%d] %q does not name an existing backend",
					i, r.Name, j, name)
			}
		}
		switch r.Route.PoolActivation {
		case "", PoolActivationWait, PoolActivationIfIdle:
		default:
			return fmt.Errorf("rules[%d] %s: route.poolActivation %q must be %q or %q",
				i, r.Name, r.Route.PoolActivation, PoolActivationWait, PoolActivationIfIdle)
		}
		if err := c.validateRuleStrategy(i, &c.Rules[i]); err != nil {
			return err
		}
	}
	if err := c.Policy.AuditLog.validate(); err != nil {
		return err
	}
	return c.validateBudgets(ruleNames)
}

// validateRuleStrategy checks a rule's route.strategy, and a weighted rule's
// backends (see validateWeightedRule).
func (c *Config) validateRuleStrategy(i int, r *Rule) error {
	switch r.Route.Strategy {
	case "", strategyPrimaryFallback, strategyShadow:
		return nil
	case strategyWeighted:
		byName := make(map[string]*Backend, len(c.Backends))
		for j := range c.Backends {
			byName[c.Backends[j].Name] = &c.Backends[j]
		}
		return validateWeightedRule(i, r, byName)
	default:
		return fmt.Errorf("rules[%d] %s: route.strategy %q must be %q, %q or %q",
			i, r.Name, r.Route.Strategy, strategyPrimaryFallback, strategyWeighted, strategyShadow)
	}
}

// validate checks the client attribution settings. The controller rejects
// the same values at admission; this guards a config written by hand.
func (a AuditLogPolicy) validate() error {
	if _, err := ParseTrustedProxies(a.TrustedProxyCIDRs); err != nil {
		return fmt.Errorf("policy.auditLog.%w", err)
	}
	if IsCredentialHeader(a.CallerHeader) {
		return fmt.Errorf("policy.auditLog.callerHeader %q carries a credential and must not be logged", a.CallerHeader)
	}
	return nil
}

// validateBalancing checks a backend's balancing and metrics-profile settings.
// maxSessionHeaderNameLen matches the CRD's items MaxLength on
// sessionAffinity.headers.
const maxSessionHeaderNameLen = 256

func validateBalancing(i int, b Backend) error {
	if bal := b.Balancing; bal != nil {
		switch bal.Mode {
		case "", BalancingRoundRobin, BalancingLoadAware:
		default:
			return fmt.Errorf("backends[%d] %s: balancing mode %q must be %s or %s", i, b.Name, bal.Mode, BalancingRoundRobin, BalancingLoadAware)
		}
		if t := bal.SessionIdleTimeout; t != 0 && (t < minSessionIdleTimeout || t > maxSessionIdleTimeout) {
			return fmt.Errorf("backends[%d] %s: balancing sessionIdleTimeout %s must be between %s and %s",
				i, b.Name, t, minSessionIdleTimeout, maxSessionIdleTimeout)
		}
		for _, h := range bal.SessionHeaders {
			// Mirrors the CRD's items pattern and length bound: an RFC 7230
			// token, so surrounding whitespace or a stray character fails
			// here rather than never matching a request header.
			if len(h) > maxSessionHeaderNameLen || !httpguts.ValidHeaderFieldName(h) {
				return fmt.Errorf("backends[%d] %s: balancing session header name %q must be an HTTP header token of at most %d characters",
					i, b.Name, h, maxSessionHeaderNameLen)
			}
		}
	}
	if m := b.Metrics; m != nil && (m.Path == "" || m.Running == "" || m.Waiting == "") {
		return fmt.Errorf("backends[%d] %s: metrics profile needs path, running and waiting", i, b.Name)
	}
	return nil
}

// validateBudgets mirrors the controller's validateBudgets so a hand-edited
// ConfigMap fails loudly here rather than silently under-enforcing at request
// time. A maxUSD budget also requires every backend to declare usable pricing,
// because an unpriced backend charges 0 USD and would never trip the cap.
func (c *Config) validateBudgets(ruleNames map[string]bool) error {
	budgetNames := make(map[string]bool, len(c.Policy.Budgets))
	for i, b := range c.Policy.Budgets {
		if b.Name == "" {
			return fmt.Errorf("policy.budgets[%d]: name is required", i)
		}
		if budgetNames[b.Name] {
			return fmt.Errorf("policy.budgets[%d]: duplicate name %q", i, b.Name)
		}
		budgetNames[b.Name] = true
		switch b.Scope {
		case BudgetScopeRouter, BudgetScopeTeam:
		case BudgetScopeRule:
			if b.RuleName == "" {
				return fmt.Errorf("policy.budgets[%d] %s: ruleName is required when scope=rule", i, b.Name)
			}
			if !ruleNames[b.RuleName] {
				return fmt.Errorf("policy.budgets[%d] %s: ruleName %q does not name an existing rule",
					i, b.Name, b.RuleName)
			}
		default:
			return fmt.Errorf("policy.budgets[%d] %s: scope must be %q, %q, or %q, got %q",
				i, b.Name, BudgetScopeRouter, BudgetScopeRule, BudgetScopeTeam, b.Scope)
		}
		if b.Window <= 0 {
			return fmt.Errorf("policy.budgets[%d] %s: window must be positive", i, b.Name)
		}
		if b.MaxTokens <= 0 && b.MaxUSD <= 0 {
			return fmt.Errorf("policy.budgets[%d] %s: must set at least one of maxTokens or maxUSD", i, b.Name)
		}
		if b.MaxUSD > 0 {
			for j := range c.Backends {
				if !c.Backends[j].pricesTokens() {
					return fmt.Errorf("backends[%d] %s: costPerMillionTokens is required when any budget sets maxUSD",
						j, c.Backends[j].Name)
				}
			}
		}
	}
	return nil
}

// pricesTokens reports whether this backend's declared pricing can charge a
// nonzero USD amount. A nil cost, or one whose prompt and completion rates
// are both zero, prices every request at zero and cannot feed a maxUSD
// budget.
func (b *Backend) pricesTokens() bool {
	return b.CostPerMillionTokens != nil &&
		(b.CostPerMillionTokens.PromptUSD > 0 || b.CostPerMillionTokens.CompletionUSD > 0)
}

// BackendByName returns the backend with the given name, or nil. The
// dispatch loop uses this on every request, so it does a linear scan
// against typical config sizes (10s of backends). Switch to a map cache
// if profiles ever show it matters.
func (c *Config) BackendByName(name string) *Backend {
	for i := range c.Backends {
		if c.Backends[i].Name == name {
			return &c.Backends[i]
		}
	}
	return nil
}

// SensitiveSet returns the set of classification values that trigger the
// fail-closed gate. Defaults to {pii, phi}; overridable via policy.
func (c *Config) SensitiveSet() map[string]bool {
	out := make(map[string]bool, 2)
	if len(c.Policy.Classification.Sensitive) > 0 {
		for _, s := range c.Policy.Classification.Sensitive {
			out[s] = true
		}
		return out
	}
	out["pii"] = true
	out["phi"] = true
	return out
}

// ClassificationHeader returns the request header name carrying the
// classification, defaulting to x-llmkube-classification.
func (c *Config) ClassificationHeader() string {
	if h := strings.TrimSpace(c.Policy.Classification.HeaderKey); h != "" {
		return h
	}
	return "x-llmkube-classification"
}
