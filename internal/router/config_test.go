/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validConfig is the canonical "everything is fine" fixture used as the
// starting point for subtests that mutate one field.
func validConfig() *Config {
	return &Config{
		Backends: []Backend{
			{Name: "local-qwen", Tier: "local", Address: "http://qwen.svc:8080"},
			{
				Name:     "cloud-opus",
				Tier:     "cloud",
				Address:  "https://api.anthropic.com",
				Provider: "anthropic",
				Model:    "claude-opus-4-7",
			},
		},
		Rules: []Rule{
			{
				Name:       "pii-stays-local",
				Match:      RuleMatch{DataClassification: []string{"pii"}},
				Route:      RuleRoute{Backends: []string{"local-qwen"}},
				FailClosed: true,
			},
		},
		DefaultRoute: "local-qwen",
		Policy: Policy{
			Classification: ClassificationPolicy{Mode: "header-only"},
			AuditLog:       AuditLogPolicy{Sink: "stdout"},
		},
	}
}

func TestConfigValidateAcceptsCanonical(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestConfigValidateRejections(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*Config)
		wantSubstr string
	}{
		{
			name:       "no backends",
			mutate:     func(c *Config) { c.Backends = nil },
			wantSubstr: "backends must be non-empty",
		},
		{
			name: "backend missing name",
			mutate: func(c *Config) {
				c.Backends[0].Name = ""
			},
			wantSubstr: "name is required",
		},
		{
			name: "duplicate backend names",
			mutate: func(c *Config) {
				c.Backends = append(c.Backends, Backend{
					Name: "local-qwen", Tier: "local", Address: "http://x",
				})
			},
			wantSubstr: "duplicate name",
		},
		{
			name: "invalid tier",
			mutate: func(c *Config) {
				c.Backends[0].Tier = "edge"
			},
			wantSubstr: "tier must be local or cloud",
		},
		{
			name: "missing address",
			mutate: func(c *Config) {
				c.Backends[0].Address = ""
			},
			wantSubstr: "address is required",
		},
		{
			name: "defaultRoute references unknown backend",
			mutate: func(c *Config) {
				c.DefaultRoute = "ghost"
			},
			wantSubstr: `defaultRoute "ghost" does not name`,
		},
		{
			name: "rule with empty backends",
			mutate: func(c *Config) {
				c.Rules[0].Route.Backends = nil
			},
			wantSubstr: "route.backends must be non-empty",
		},
		{
			name: "rule references unknown backend",
			mutate: func(c *Config) {
				c.Rules[0].Route.Backends = []string{"ghost"}
			},
			wantSubstr: `does not name an existing backend`,
		},
		{
			name: "unknown default-route strategy",
			mutate: func(c *Config) {
				c.DefaultRouteStrategy = "Whatever"
			},
			wantSubstr: `defaultRouteStrategy "Whatever" must be`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("expected error containing %q, got %v", tc.wantSubstr, err)
			}
		})
	}
}

// TestLoadConfigRoundTrip confirms that what we write to disk parses
// back to the same shape. This is the wire contract between the
// controller's ConfigMap writer and the proxy's reader.
func TestLoadConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.json")
	if err := os.WriteFile(path, []byte(canonicalJSON), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Backends) != 2 {
		t.Errorf("got %d backends, want 2", len(cfg.Backends))
	}
	if cfg.DefaultRoute != "local-qwen" {
		t.Errorf("DefaultRoute = %q, want local-qwen", cfg.DefaultRoute)
	}
	if cfg.DefaultRouteStrategy != DefaultRouteStrategyBackendNameMatch {
		t.Errorf("DefaultRouteStrategy = %q, want BackendNameMatch", cfg.DefaultRouteStrategy)
	}
	if !cfg.Rules[0].FailClosed {
		t.Error("expected FailClosed=true on pii rule")
	}
}

func TestLoadConfigSurfacesIOErrors(t *testing.T) {
	_, err := LoadConfig("/nonexistent/router.json")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "read router config") {
		t.Errorf("expected wrapped read error, got %v", err)
	}
}

func TestLoadConfigSurfacesParseErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "parse router config") {
		t.Errorf("expected wrapped parse error, got %v", err)
	}
}

func TestSensitiveSetDefaults(t *testing.T) {
	cfg := &Config{}
	got := cfg.SensitiveSet()
	for _, want := range []string{"pii", "phi"} {
		if !got[want] {
			t.Errorf("default sensitive set missing %q", want)
		}
	}
}

func TestSensitiveSetOverride(t *testing.T) {
	cfg := &Config{
		Policy: Policy{
			Classification: ClassificationPolicy{
				Sensitive: []string{"secret"},
			},
		},
	}
	got := cfg.SensitiveSet()
	if !got["secret"] {
		t.Error("override should contain secret")
	}
	if got["pii"] {
		t.Error("override should replace defaults; pii should not be sensitive")
	}
}

func TestClassificationHeaderDefault(t *testing.T) {
	cfg := &Config{}
	if got := cfg.ClassificationHeader(); got != "x-llmkube-classification" {
		t.Errorf("got %q, want default", got)
	}
}

func TestClassificationHeaderOverride(t *testing.T) {
	cfg := &Config{
		Policy: Policy{
			Classification: ClassificationPolicy{HeaderKey: "x-corp-classification"},
		},
	}
	if got := cfg.ClassificationHeader(); got != "x-corp-classification" {
		t.Errorf("got %q, want override", got)
	}
}

const canonicalJSON = `{
  "backends": [
    {"name": "local-qwen", "tier": "local", "address": "http://qwen.svc:8080"},
    {"name": "cloud-opus", "tier": "cloud", "address": "https://api.anthropic.com",
     "provider": "anthropic", "model": "claude-opus-4-7", "credentialsEnv": "ANTHROPIC_API_KEY"}
  ],
  "rules": [
    {"name": "pii-stays-local",
     "match": {"dataClassification": ["pii"]},
     "route": {"backends": ["local-qwen"]},
     "failClosed": true}
  ],
  "defaultRoute": "local-qwen",
  "defaultRouteStrategy": "BackendNameMatch",
  "policy": {
    "classification": {"mode": "header-only"},
    "auditLog": {"sink": "stdout"}
  }
}`

// TestConfigValidateRejectsUnknownPoolActivation keeps the wire enum closed so a
// typo in poolActivation fails at compile time instead of silently meaning Wait.
func TestConfigValidateRejectsUnknownPoolActivation(t *testing.T) {
	cfg := &Config{
		Backends:     []Backend{{Name: "a", Tier: "local", Address: "http://a"}},
		DefaultRoute: "a",
		Rules: []Rule{{
			Name:  "r",
			Route: RuleRoute{Backends: []string{"a"}, PoolActivation: "Sometimes"},
		}},
		Policy: Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted poolActivation \"Sometimes\"; want an error")
	}
	for _, ok := range []string{"", PoolActivationWait, PoolActivationIfIdle} {
		cfg.Rules[0].Route.PoolActivation = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate rejected poolActivation %q: %v", ok, err)
		}
	}
}

// TestConfigValidateBudgets covers the fail-loud validation of compiled
// budgets, so a hand-edited ConfigMap cannot silently under-enforce.
func TestConfigValidateBudgets(t *testing.T) {
	tests := []struct {
		name    string
		budget  Budget
		wantErr string
	}{
		{"router ok", Budget{Name: "router-cap", Scope: BudgetScopeRouter, Window: time.Hour, MaxTokens: 100}, ""},
		{"team ok", Budget{Name: "team-cap", Scope: BudgetScopeTeam, Window: time.Hour, MaxUSD: 1.5}, ""},
		{"rule known ok", Budget{Name: "rule-cap", Scope: BudgetScopeRule, RuleName: "pii-stays-local", Window: time.Hour, MaxTokens: 100}, ""},
		{"missing name", Budget{Scope: BudgetScopeRouter, Window: time.Hour, MaxTokens: 100}, "name is required"},
		{"bad scope", Budget{Name: "b", Scope: "tenant", Window: time.Hour, MaxTokens: 100}, "scope must be"},
		{"rule without ruleName", Budget{Name: "b", Scope: BudgetScopeRule, Window: time.Hour, MaxTokens: 100}, "ruleName is required"},
		{"rule unknown", Budget{Name: "b", Scope: BudgetScopeRule, RuleName: "nope", Window: time.Hour, MaxTokens: 100}, "does not name an existing rule"},
		{"no cap", Budget{Name: "b", Scope: BudgetScopeRouter, Window: time.Hour}, "at least one of"},
		{"zero window", Budget{Name: "b", Scope: BudgetScopeRouter, MaxTokens: 100}, "window must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Policy.Budgets = []Budget{tt.budget}
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}
