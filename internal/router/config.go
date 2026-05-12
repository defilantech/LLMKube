/*
Copyright 2025.

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

	// Policy holds cross-cutting controls (classification, audit). Budget
	// enforcement and persistence land in #434 / #440.
	Policy Policy `json:"policy"`
}

// Backend is one dispatch destination. The controller resolves an
// InferenceServiceRef to an in-cluster URL before writing the ConfigMap;
// the proxy only ever sees resolved addresses.
type Backend struct {
	Name string `json:"name"`

	// Tier is "local" or "cloud". Used by the fail-closed gate: sensitive-
	// data rules cannot route to cloud-tier backends.
	Tier string `json:"tier"`

	// Address is the upstream base URL the proxy sends requests to. For
	// local backends this is the InferenceService cluster URL (e.g.
	// http://qwen3-coder.platform.svc.cluster.local:8080). For external
	// providers it is the provider base URL.
	Address string `json:"address"`

	// Provider identifies the upstream API surface for external backends
	// ("anthropic", "openai", "litellm", etc.). Empty for local backends.
	Provider string `json:"provider,omitempty"`

	// Model is the upstream model identifier passed to the provider.
	// Empty for local backends (the request body carries the model name).
	Model string `json:"model,omitempty"`

	// Capabilities advertised by this backend (e.g. ["tools", "vision"]).
	// Rules can require capabilities to filter candidates.
	Capabilities []string `json:"capabilities,omitempty"`

	// Weight informs the "weighted" routing strategy (#432). Currently
	// unused; the MVP strategy is primary-fallback.
	Weight int `json:"weight,omitempty"`

	// CredentialsEnv names an environment variable holding the bearer or
	// API key for this backend. The proxy reads the value at request time
	// and injects it into the upstream request as Authorization or
	// x-api-key, depending on Provider. Empty for local backends.
	CredentialsEnv string `json:"credentialsEnv,omitempty"`
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
	// every listed capability.
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
	// Only primary-fallback is implemented in the MVP; the other two land
	// in #432.
	Strategy string `json:"strategy,omitempty"`
}

// Policy holds cross-cutting controls.
type Policy struct {
	Classification ClassificationPolicy `json:"classification"`
	AuditLog       AuditLogPolicy       `json:"auditLog"`
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
// one structured log line per routing decision; this configures the sink.
type AuditLogPolicy struct {
	Sink               string `json:"sink"`
	FilePath           string `json:"filePath,omitempty"`
	IncludeRequestBody bool   `json:"includeRequestBody,omitempty"`
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
		if b.Address == "" {
			return fmt.Errorf("backends[%d] %s: address is required", i, b.Name)
		}
	}
	if c.DefaultRoute != "" && !names[c.DefaultRoute] {
		return fmt.Errorf("defaultRoute %q does not name an existing backend", c.DefaultRoute)
	}
	for i, r := range c.Rules {
		if r.Name == "" {
			return fmt.Errorf("rules[%d]: name is required", i)
		}
		if len(r.Route.Backends) == 0 {
			return fmt.Errorf("rules[%d] %s: route.backends must be non-empty", i, r.Name)
		}
		for j, name := range r.Route.Backends {
			if !names[name] {
				return fmt.Errorf("rules[%d] %s: route.backends[%d] %q does not name an existing backend",
					i, r.Name, j, name)
			}
		}
	}
	return nil
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
