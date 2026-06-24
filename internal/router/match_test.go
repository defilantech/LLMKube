/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import "testing"

func matcherFromValid() *Matcher {
	return NewMatcher(validConfig())
}

func TestMatchPIIRouteWins(t *testing.T) {
	got := matcherFromValid().Match(&RequestFeatures{Classification: "pii"})
	if got.Rule == nil {
		t.Fatal("expected pii rule to match, got default")
	}
	if got.Rule.Name != "pii-stays-local" {
		t.Errorf("matched rule = %q, want pii-stays-local", got.Rule.Name)
	}
	if !got.FailClosed {
		t.Error("matched fail-closed rule should report FailClosed=true")
	}
	if got.Strategy != strategyPrimaryFallback {
		t.Errorf("default strategy = %q, want primary-fallback", got.Strategy)
	}
}

func TestMatchFallsThroughToDefault(t *testing.T) {
	got := matcherFromValid().Match(&RequestFeatures{Classification: "public"})
	if got.Rule != nil {
		t.Errorf("expected no rule match, got %q", got.Rule.Name)
	}
	if len(got.Backends) != 1 || got.Backends[0] != "local-qwen" {
		t.Errorf("expected default-route fallback to local-qwen, got %v", got.Backends)
	}
}

func TestMatchReturnsEmptyWhenNoDefaultAndNoRule(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = nil
	cfg.DefaultRoute = ""
	got := NewMatcher(cfg).Match(&RequestFeatures{Classification: "public"})
	if len(got.Backends) != 0 {
		t.Errorf("expected no backends, got %v", got.Backends)
	}
}

func TestMatchModelGlob(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = []Rule{{
		Name:  "qwen-family",
		Match: RuleMatch{Models: []string{"qwen3-*"}},
		Route: RuleRoute{Backends: []string{"local-qwen"}},
	}}
	m := NewMatcher(cfg)
	if got := m.Match(&RequestFeatures{Model: "qwen3-coder-30b"}); got.Rule == nil {
		t.Error("qwen3-coder-30b should match qwen3-*")
	}
	if got := m.Match(&RequestFeatures{Model: "llama-3"}); got.Rule != nil {
		t.Errorf("llama-3 should not match qwen3-*, matched %q", got.Rule.Name)
	}
}

func TestMatchHeadersCaseInsensitive(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = []Rule{{
		Name:  "team-rule",
		Match: RuleMatch{Headers: map[string]string{"X-Team": "research"}},
		Route: RuleRoute{Backends: []string{"local-qwen"}},
	}}
	m := NewMatcher(cfg)
	got := m.Match(&RequestFeatures{Headers: map[string]string{"x-team": "research"}})
	if got.Rule == nil {
		t.Error("lowercase header should match canonical declaration")
	}
}

func TestMatchTaskComplexity(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = []Rule{{
		Name:  "complex-to-cloud",
		Match: RuleMatch{TaskComplexity: "complex"},
		Route: RuleRoute{Backends: []string{"cloud-opus"}},
	}}
	m := NewMatcher(cfg)
	if got := m.Match(&RequestFeatures{TaskComplexity: "complex"}); got.Rule == nil {
		t.Error("complex task should match")
	}
	if got := m.Match(&RequestFeatures{TaskComplexity: "simple"}); got.Rule != nil {
		t.Errorf("simple task should not match complex rule, matched %q", got.Rule.Name)
	}
}

func TestMatchRequiredCapabilities(t *testing.T) {
	cfg := validConfig()
	cfg.Backends[0].Capabilities = []string{"code"}
	cfg.Backends[1].Capabilities = []string{"vision", "long-context"}
	cfg.Rules = []Rule{{
		Name: "vision-rule",
		Match: RuleMatch{
			Models:               []string{"*"},
			RequiredCapabilities: []string{"vision"},
		},
		Route: RuleRoute{Backends: []string{"local-qwen", "cloud-opus"}},
	}}
	m := NewMatcher(cfg)
	// At least one route backend has vision; should match.
	if got := m.Match(&RequestFeatures{Model: "any"}); got.Rule == nil {
		t.Error("expected match: cloud-opus has vision")
	}

	// Remove vision from cloud-opus; now no backend in the route has it.
	cfg.Backends[1].Capabilities = []string{"long-context"}
	m = NewMatcher(cfg)
	if got := m.Match(&RequestFeatures{Model: "any"}); got.Rule != nil {
		t.Errorf("expected no match when no route backend has vision; matched %q", got.Rule.Name)
	}
}

func TestMatchFirstRuleWins(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = []Rule{
		{
			Name:  "first",
			Match: RuleMatch{Models: []string{"*"}},
			Route: RuleRoute{Backends: []string{"local-qwen"}},
		},
		{
			Name:  "second",
			Match: RuleMatch{Models: []string{"*"}},
			Route: RuleRoute{Backends: []string{"cloud-opus"}},
		},
	}
	got := NewMatcher(cfg).Match(&RequestFeatures{Model: "any-model"})
	if got.Rule == nil || got.Rule.Name != "first" {
		t.Errorf("expected first rule to win, got %v", got.Rule)
	}
}

func TestMatchBackendNameMatchAddressesBackendByName(t *testing.T) {
	cfg := validConfig()
	cfg.DefaultRouteStrategy = DefaultRouteStrategyBackendNameMatch
	// "public" classification matches no rule; model names a backend, so
	// BackendNameMatch routes there instead of falling back to DefaultRoute.
	got := NewMatcher(cfg).Match(&RequestFeatures{Classification: "public", Model: "cloud-opus"})
	if got.Rule != nil {
		t.Errorf("expected no rule match, got %q", got.Rule.Name)
	}
	if len(got.Backends) != 1 || got.Backends[0] != "cloud-opus" {
		t.Errorf("expected name match to cloud-opus, got %v", got.Backends)
	}
	if got.Strategy != strategyPrimaryFallback {
		t.Errorf("strategy = %q, want primary-fallback", got.Strategy)
	}
}

func TestMatchBackendNameMatchFallsBackToDefault(t *testing.T) {
	cfg := validConfig()
	cfg.DefaultRouteStrategy = DefaultRouteStrategyBackendNameMatch
	// Model names no backend, so it falls through to DefaultRoute.
	got := NewMatcher(cfg).Match(&RequestFeatures{Classification: "public", Model: "ghost-model"})
	if len(got.Backends) != 1 || got.Backends[0] != "local-qwen" {
		t.Errorf("expected fallback to default local-qwen, got %v", got.Backends)
	}
}

func TestMatchBackendNameMatchDoesNotOverrideRules(t *testing.T) {
	cfg := validConfig()
	cfg.DefaultRouteStrategy = DefaultRouteStrategyBackendNameMatch
	// A matching rule wins even when the model also names a backend; name
	// matching only fires when no rule accepts the request.
	got := NewMatcher(cfg).Match(&RequestFeatures{Classification: "pii", Model: "cloud-opus"})
	if got.Rule == nil || got.Rule.Name != "pii-stays-local" {
		t.Errorf("expected pii rule to win over name match, got %v", got.Rule)
	}
}

func TestMatchStaticIgnoresBackendName(t *testing.T) {
	cfg := validConfig()
	cfg.DefaultRouteStrategy = DefaultRouteStrategyStatic
	// Under Static, a model that names a backend is irrelevant; unmatched
	// requests go straight to DefaultRoute.
	got := NewMatcher(cfg).Match(&RequestFeatures{Classification: "public", Model: "cloud-opus"})
	if len(got.Backends) != 1 || got.Backends[0] != "local-qwen" {
		t.Errorf("expected static default to local-qwen, got %v", got.Backends)
	}
}

func TestMatchBackendNameMatchNoDefaultReturns503(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = nil
	cfg.DefaultRoute = ""
	cfg.DefaultRouteStrategy = DefaultRouteStrategyBackendNameMatch
	// No rule, no name match, no DefaultRoute: nothing resolves, which the
	// proxy surfaces as HTTP 503.
	got := NewMatcher(cfg).Match(&RequestFeatures{Model: "ghost-model"})
	if len(got.Backends) != 0 {
		t.Errorf("expected no backends (503), got %v", got.Backends)
	}
}

func TestMatchBackendNameMatchEmptyModelFallsBack(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = nil
	cfg.DefaultRouteStrategy = DefaultRouteStrategyBackendNameMatch
	// An empty model skips the name lookup entirely (no map[""] probe) and
	// falls through to DefaultRoute.
	got := NewMatcher(cfg).Match(&RequestFeatures{Model: ""})
	if len(got.Backends) != 1 || got.Backends[0] != "local-qwen" {
		t.Errorf("expected empty model to fall back to local-qwen, got %v", got.Backends)
	}
}

func TestMatchBackendNameMatchResolvesDisplayName(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = nil
	cfg.DefaultRouteStrategy = DefaultRouteStrategyBackendNameMatch
	// Give the cloud backend a DisplayName that differs from its Name.
	cfg.Backends[1].DisplayName = "claude-opus-4-20250514"
	// A request whose model matches the DisplayName should resolve to the
	// backend (returning the backend Name, not the DisplayName).
	got := NewMatcher(cfg).Match(&RequestFeatures{Model: "claude-opus-4-20250514"})
	if len(got.Backends) != 1 || got.Backends[0] != "cloud-opus" {
		t.Errorf("expected name match to cloud-opus via DisplayName, got %v", got.Backends)
	}
	// The old Name should no longer match (DisplayName takes precedence).
	got2 := NewMatcher(cfg).Match(&RequestFeatures{Model: "cloud-opus"})
	if len(got2.Backends) != 1 || got2.Backends[0] != "local-qwen" {
		t.Errorf("expected old Name to fall through to default, got %v", got2.Backends)
	}
}

func TestMatchBackendNameMatchDisplayNameUnsetUsesName(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = nil
	cfg.DefaultRouteStrategy = DefaultRouteStrategyBackendNameMatch
	// No DisplayName set; Name is used as the published id.
	got := NewMatcher(cfg).Match(&RequestFeatures{Model: "cloud-opus"})
	if len(got.Backends) != 1 || got.Backends[0] != "cloud-opus" {
		t.Errorf("expected name match to cloud-opus, got %v", got.Backends)
	}
}
