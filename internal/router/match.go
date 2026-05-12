/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"path"
	"strings"
)

// RequestFeatures captures everything a Rule can match against. The proxy
// extracts this once per request and feeds it through the matcher loop.
type RequestFeatures struct {
	// Model is the OpenAI "model" field from the request body. Empty if
	// the body omitted it (we still try to match other rule fields).
	Model string

	// Classification is the resolved data classification of the request.
	// Resolved by the proxy from the configured header (header-only mode)
	// before the matcher runs.
	Classification string

	// TaskComplexity is the inbound x-llmkube-task-complexity hint.
	TaskComplexity string

	// Headers is the inbound HTTP header map (canonicalized lower-case
	// keys for case-insensitive matching).
	Headers map[string]string
}

// MatchResult records the outcome of evaluating the rule set for one
// request. If no rule matched and Config.DefaultRoute is set, the proxy
// dispatches to the default backend with Rule == nil and Strategy ==
// "primary-fallback" semantics.
type MatchResult struct {
	// Rule is the matched rule, or nil if no rule matched (in which case
	// DefaultRoute applies).
	Rule *Rule

	// Backends is the ordered list of backend names this request may be
	// dispatched to. Derived from the matched rule's Route.Backends, or
	// from Config.DefaultRoute if no rule matched.
	Backends []string

	// Strategy is the dispatch strategy ("primary-fallback", "weighted",
	// "shadow"). Defaults to "primary-fallback" when omitted.
	Strategy string

	// FailClosed is true when a matched fail-closed rule rejects the
	// request rather than falling through if no backend is healthy.
	FailClosed bool
}

// Matcher pre-computes lookups over a Config so the per-request hot path
// is allocation-free. Construct once at startup, reuse across requests.
type Matcher struct {
	cfg            *Config
	backendsByName map[string]*Backend
}

// NewMatcher returns a Matcher bound to the given config. The matcher
// holds a pointer to the config and assumes it is immutable; rebuild on
// config reload.
func NewMatcher(cfg *Config) *Matcher {
	byName := make(map[string]*Backend, len(cfg.Backends))
	for i := range cfg.Backends {
		byName[cfg.Backends[i].Name] = &cfg.Backends[i]
	}
	return &Matcher{cfg: cfg, backendsByName: byName}
}

// Match evaluates the rule set against the request features and returns
// the routing decision. If no rule matches and DefaultRoute is empty,
// returns a MatchResult with nil Backends, which the proxy translates to
// HTTP 503.
func (m *Matcher) Match(features *RequestFeatures) MatchResult {
	for i := range m.cfg.Rules {
		rule := &m.cfg.Rules[i]
		if !m.ruleMatches(rule, features) {
			continue
		}
		return MatchResult{
			Rule:       rule,
			Backends:   rule.Route.Backends,
			Strategy:   strategyOrDefault(rule.Route.Strategy),
			FailClosed: rule.FailClosed,
		}
	}

	if m.cfg.DefaultRoute != "" {
		return MatchResult{
			Backends: []string{m.cfg.DefaultRoute},
			Strategy: strategyPrimaryFallback,
		}
	}
	return MatchResult{}
}

// BackendByName proxies to Config.BackendByName but uses the cached map
// so the per-request lookup is O(1).
func (m *Matcher) BackendByName(name string) *Backend {
	return m.backendsByName[name]
}

const (
	strategyPrimaryFallback = "primary-fallback"
	strategyWeighted        = "weighted"
	strategyShadow          = "shadow"
)

func strategyOrDefault(s string) string {
	if s == "" {
		return strategyPrimaryFallback
	}
	return s
}

// ruleMatches reports whether every declared Match field on the rule is
// satisfied by the request features. Empty fields are not considered
// (match-all semantics on the dimension).
func (m *Matcher) ruleMatches(rule *Rule, f *RequestFeatures) bool {
	mt := &rule.Match

	if len(mt.DataClassification) > 0 && !contains(mt.DataClassification, f.Classification) {
		return false
	}
	if mt.TaskComplexity != "" && mt.TaskComplexity != f.TaskComplexity {
		return false
	}
	if !matchModels(mt.Models, f.Model) {
		return false
	}
	if !matchHeaders(mt.Headers, f.Headers) {
		return false
	}
	if !m.satisfiesRequiredCapabilities(mt.RequiredCapabilities, rule.Route.Backends) {
		return false
	}
	return true
}

// matchModels reports whether the request model matches at least one of
// the rule's model patterns. Empty pattern list matches anything; an
// empty request model only matches if a pattern is "*" or empty list.
func matchModels(patterns []string, model string) bool {
	if len(patterns) == 0 {
		return true
	}
	if model == "" {
		// An empty model can only satisfy a literal "*" wildcard.
		for _, p := range patterns {
			if p == "*" {
				return true
			}
		}
		return false
	}
	for _, p := range patterns {
		if ok, _ := path.Match(p, model); ok {
			return true
		}
	}
	return false
}

// matchHeaders performs case-insensitive equality on the header pairs the
// rule declares. Missing headers fail the match.
func matchHeaders(want, have map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	for k, wantVal := range want {
		got, ok := have[strings.ToLower(k)]
		if !ok || got != wantVal {
			return false
		}
	}
	return true
}

// satisfiesRequiredCapabilities returns true when at least one backend
// named in the route advertises every required capability. This is the
// "any candidate matches" semantic: a rule with required ["vision"] will
// match if any of its route backends has vision, even if others don't.
func (m *Matcher) satisfiesRequiredCapabilities(required, backendNames []string) bool {
	if len(required) == 0 {
		return true
	}
	for _, name := range backendNames {
		b := m.backendsByName[name]
		if b == nil {
			continue
		}
		if hasAllCapabilities(b.Capabilities, required) {
			return true
		}
	}
	return false
}

func hasAllCapabilities(have, want []string) bool {
	set := make(map[string]bool, len(have))
	for _, c := range have {
		set[c] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
