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

package controller

import (
	"reflect"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// headerMatchValues returns the (name, value) pairs of the header matches in one
// AIGatewayRoute match entry, so classification cases can assert the conditions
// a match ANDs together regardless of order.
func headerMatchValues(t *testing.T, entry interface{}) map[string]string {
	t.Helper()
	m, ok := entry.(map[string]interface{})
	if !ok {
		t.Fatalf("match entry is not a map, got %T", entry)
	}
	raw, ok := m["headers"]
	if !ok {
		return map[string]string{}
	}
	headers, ok := raw.([]interface{})
	if !ok {
		t.Fatalf("match headers is not a slice, got %T", raw)
	}
	out := make(map[string]string, len(headers))
	for _, h := range headers {
		hm := h.(map[string]interface{})
		if hm["type"] != "Exact" {
			t.Errorf("header match type = %v, want Exact", hm["type"])
		}
		out[hm[metadataNameField].(string)] = hm["value"].(string)
	}
	return out
}

// TestCompileRuleMatches_DataClassificationHeader pins that a header-only
// dataClassification compiles to an Exact match on the classification header key.
func TestCompileRuleMatches_DataClassificationHeader(t *testing.T) {
	rule := routerRuleResource{
		DataClassifications:     []string{"confidential"},
		ClassificationHeaderKey: defaultClassificationHeaderKey,
	}
	matches := compileRuleMatches(rule)
	if len(matches) != 1 {
		t.Fatalf("got %d match entries, want 1", len(matches))
	}
	got := headerMatchValues(t, matches[0])
	want := map[string]string{defaultClassificationHeaderKey: "confidential"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("header matches = %v, want %v", got, want)
	}
}

// TestCompileRuleMatches_CustomClassificationHeaderKey pins that a custom header
// key is honored.
func TestCompileRuleMatches_CustomClassificationHeaderKey(t *testing.T) {
	rule := routerRuleResource{
		DataClassifications:     []string{"pii"},
		ClassificationHeaderKey: "x-acme-data-class",
	}
	matches := compileRuleMatches(rule)
	if len(matches) != 1 {
		t.Fatalf("got %d match entries, want 1", len(matches))
	}
	got := headerMatchValues(t, matches[0])
	if got["x-acme-data-class"] != "pii" {
		t.Errorf("header matches = %v, want x-acme-data-class=pii", got)
	}
}

// TestCompileRuleMatches_ModelClassificationCrossProduct pins the cross-product:
// one match entry per (model, classification) pair, each carrying both the model
// header and the classification header.
func TestCompileRuleMatches_ModelClassificationCrossProduct(t *testing.T) {
	rule := routerRuleResource{
		Models:                  []string{"qwen35-27b", "gemma3-12b"},
		DataClassifications:     []string{"confidential", "internal"},
		ClassificationHeaderKey: defaultClassificationHeaderKey,
	}
	matches := compileRuleMatches(rule)
	if len(matches) != 4 {
		t.Fatalf("got %d match entries, want 4 (2 models x 2 classifications)", len(matches))
	}

	want := map[string]bool{
		"qwen35-27b|confidential": false,
		"qwen35-27b|internal":     false,
		"gemma3-12b|confidential": false,
		"gemma3-12b|internal":     false,
	}
	for _, entry := range matches {
		hv := headerMatchValues(t, entry)
		model := hv[aiGatewayModelHeader]
		class := hv[defaultClassificationHeaderKey]
		if model == "" || class == "" {
			t.Errorf("match entry %v missing model or classification header", hv)
			continue
		}
		key := model + "|" + class
		if _, ok := want[key]; !ok {
			t.Errorf("unexpected (model, classification) pair %q", key)
			continue
		}
		want[key] = true
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("missing (model, classification) pair %q", key)
		}
	}
}

// TestCompileRuleMatches_ClassificationWithUserHeaders pins that user headers are
// ANDed into every classification match entry.
func TestCompileRuleMatches_ClassificationWithUserHeaders(t *testing.T) {
	rule := routerRuleResource{
		DataClassifications:     []string{"confidential"},
		ClassificationHeaderKey: defaultClassificationHeaderKey,
		Headers:                 map[string]string{"x-team": "platform"},
	}
	matches := compileRuleMatches(rule)
	if len(matches) != 1 {
		t.Fatalf("got %d match entries, want 1", len(matches))
	}
	got := headerMatchValues(t, matches[0])
	want := map[string]string{
		defaultClassificationHeaderKey: "confidential",
		"x-team":                       "platform",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("header matches = %v, want %v", got, want)
	}
}

// TestCompileRuleMatches_NoClassificationUnchanged is the regression guard: a
// rule with no DataClassifications compiles byte-for-byte as before across the
// model-only, header-only, and catch-all shapes.
func TestCompileRuleMatches_NoClassificationUnchanged(t *testing.T) {
	tests := []struct {
		name string
		rule routerRuleResource
	}{
		{
			name: "model-only",
			rule: routerRuleResource{Models: []string{"qwen35-27b"}},
		},
		{
			name: "header-only",
			rule: routerRuleResource{Headers: map[string]string{"x-team": "a"}},
		},
		{
			name: "catch-all",
			rule: routerRuleResource{},
		},
		{
			name: "model plus headers",
			rule: routerRuleResource{
				Models:  []string{"qwen35-27b", "gemma3-12b"},
				Headers: map[string]string{"x-team": "a"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compileRuleMatches(tt.rule)
			want := legacyCompileRuleMatches(tt.rule)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("compileRuleMatches drift:\n got  %#v\n want %#v", got, want)
			}
		})
	}
}

// legacyCompileRuleMatches reproduces the pre-2e match compiler (model-only,
// header-only, catch-all), so the regression guard asserts the new cross-product
// path reduces to it exactly when no classification is present.
func legacyCompileRuleMatches(rule routerRuleResource) []interface{} {
	headerMatches := sortedHeaderMatches(rule.Headers)

	if len(rule.Models) == 0 {
		match := map[string]interface{}{}
		if len(headerMatches) > 0 {
			match["headers"] = headerMatches
		}
		return []interface{}{match}
	}

	matches := make([]interface{}, 0, len(rule.Models))
	for _, model := range rule.Models {
		headers := make([]interface{}, 0, 1+len(headerMatches))
		headers = append(headers, modelHeaderMatch(model))
		headers = append(headers, headerMatches...)
		matches = append(matches, map[string]interface{}{"headers": headers})
	}
	return matches
}

// TestUnsupportedMatchFields_ClassificationModeAware pins the mode gating:
// dataClassification is expressible in header-only mode and fail-loud in
// detector/hybrid, while the other router-side fields stay fail-loud in all modes.
func TestUnsupportedMatchFields_ClassificationModeAware(t *testing.T) {
	classMatch := &inferencev1alpha1.RuleMatch{DataClassification: []string{"pii"}}

	if got := unsupportedMatchFields(classMatch, classificationModeHeaderOnly); len(got) != 0 {
		t.Errorf("header-only: dataClassification should be expressible, got %v", got)
	}
	for _, mode := range []string{"detector", "hybrid"} {
		got := unsupportedMatchFields(classMatch, mode)
		if len(got) != 1 {
			t.Fatalf("%s: want dataClassification unsupported, got %v", mode, got)
		}
	}

	other := &inferencev1alpha1.RuleMatch{
		TaskComplexity:       "complex",
		RequiredCapabilities: []string{"tools"},
	}
	for _, mode := range []string{classificationModeHeaderOnly, "detector", "hybrid"} {
		got := unsupportedMatchFields(other, mode)
		if len(got) != 2 {
			t.Errorf("%s: taskComplexity+requiredCapabilities should stay unsupported, got %v", mode, got)
		}
	}
}

// TestClassificationMode_Defaults pins the header-only default through a nil
// policy, a nil classification, and an empty mode.
func TestClassificationMode_Defaults(t *testing.T) {
	tests := []struct {
		name string
		mr   *inferencev1alpha1.ModelRouter
		want string
	}{
		{
			name: "nil policy",
			mr:   &inferencev1alpha1.ModelRouter{},
			want: classificationModeHeaderOnly,
		},
		{
			name: "nil classification",
			mr: &inferencev1alpha1.ModelRouter{
				Spec: inferencev1alpha1.ModelRouterSpec{Policy: &inferencev1alpha1.RouterPolicy{}},
			},
			want: classificationModeHeaderOnly,
		},
		{
			name: "explicit detector",
			mr: &inferencev1alpha1.ModelRouter{
				Spec: inferencev1alpha1.ModelRouterSpec{
					Policy: &inferencev1alpha1.RouterPolicy{
						Classification: &inferencev1alpha1.ClassificationPolicy{Mode: "detector"},
					},
				},
			},
			want: "detector",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classificationMode(tt.mr); got != tt.want {
				t.Errorf("classificationMode = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestUnsafeSensitiveRouteMessage covers the fail-closed sensitive guard: a
// sensitive rule must be failClosed and route only to local-tier backends, with
// the sensitive set defaulting to [pii, phi] and honoring a custom list.
func TestUnsafeSensitiveRouteMessage(t *testing.T) {
	localBackends := []inferencev1alpha1.RouterBackend{
		{Name: "metal-a", Tier: "local"},
		{Name: "metal-b"}, // tier unset defaults to local
	}
	withCloud := append([]inferencev1alpha1.RouterBackend{{Name: "openai", Tier: "cloud"}}, localBackends...)

	tests := []struct {
		name       string
		backends   []inferencev1alpha1.RouterBackend
		policy     *inferencev1alpha1.RouterPolicy
		rule       inferencev1alpha1.RouterRule
		wantReject bool
	}{
		{
			name:     "non-sensitive rule ignored",
			backends: localBackends,
			rule: inferencev1alpha1.RouterRule{
				Name:  "r0",
				Match: &inferencev1alpha1.RuleMatch{DataClassification: []string{"internal"}},
				Route: inferencev1alpha1.RuleRoute{Backends: []string{"metal-a"}},
			},
			wantReject: false,
		},
		{
			name:     "pii without failClosed rejected",
			backends: localBackends,
			rule: inferencev1alpha1.RouterRule{
				Name:  "r0",
				Match: &inferencev1alpha1.RuleMatch{DataClassification: []string{"pii"}},
				Route: inferencev1alpha1.RuleRoute{Backends: []string{"metal-a"}},
			},
			wantReject: true,
		},
		{
			name:     "pii failClosed local-only allowed",
			backends: localBackends,
			rule: inferencev1alpha1.RouterRule{
				Name:       "r0",
				FailClosed: true,
				Match:      &inferencev1alpha1.RuleMatch{DataClassification: []string{"pii"}},
				Route:      inferencev1alpha1.RuleRoute{Backends: []string{"metal-a", "metal-b"}},
			},
			wantReject: false,
		},
		{
			name:     "pii failClosed cloud backend rejected",
			backends: withCloud,
			rule: inferencev1alpha1.RouterRule{
				Name:       "r0",
				FailClosed: true,
				Match:      &inferencev1alpha1.RuleMatch{DataClassification: []string{"pii"}},
				Route:      inferencev1alpha1.RuleRoute{Backends: []string{"metal-a", "openai"}},
			},
			wantReject: true,
		},
		{
			name:     "custom sensitive value triggers guard",
			backends: localBackends,
			policy: &inferencev1alpha1.RouterPolicy{
				Classification: &inferencev1alpha1.ClassificationPolicy{
					SensitiveClassifications: []string{"secret"},
				},
			},
			rule: inferencev1alpha1.RouterRule{
				Name:  "r0",
				Match: &inferencev1alpha1.RuleMatch{DataClassification: []string{"secret"}},
				Route: inferencev1alpha1.RuleRoute{Backends: []string{"metal-a"}},
			},
			wantReject: true,
		},
		{
			name:     "value removed from custom list not sensitive",
			backends: localBackends,
			policy: &inferencev1alpha1.RouterPolicy{
				Classification: &inferencev1alpha1.ClassificationPolicy{
					SensitiveClassifications: []string{"secret"},
				},
			},
			rule: inferencev1alpha1.RouterRule{
				Name:  "r0",
				Match: &inferencev1alpha1.RuleMatch{DataClassification: []string{"pii"}},
				Route: inferencev1alpha1.RuleRoute{Backends: []string{"metal-a"}},
			},
			wantReject: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := &inferencev1alpha1.ModelRouter{
				Spec: inferencev1alpha1.ModelRouterSpec{
					Backends: tt.backends,
					Policy:   tt.policy,
					Rules:    []inferencev1alpha1.RouterRule{tt.rule},
				},
			}
			msg := unsafeSensitiveRouteMessage(mr)
			if tt.wantReject && msg == "" {
				t.Errorf("expected rejection, got empty message")
			}
			if !tt.wantReject && msg != "" {
				t.Errorf("expected no rejection, got: %s", msg)
			}
		})
	}
}

// TestUnsafeSensitiveRouteMessage_PerDeclaringRuleLimitation pins the documented
// SCOPE of the guard: it is per-declaring-rule, NOT a global PII-egress invariant.
// A rule that does not declare a sensitive dataClassification (a model-only rule
// or the defaultRoute) is not inspected, even when it routes to a cloud-tier
// backend, so the guard returns "" (allows) for these routers.
//
// KNOWN LIMITATION: this is only safe today because Gateway mode cannot express a
// cloud/external backend at all (resolveBackends hard-errors on External and CRD
// validation rejects tier=cloud on InferenceServiceRef backends), so the cloud
// backend below is not reachable in a real Gateway-mode router. When cloud/external
// backends become expressible in Gateway mode, these non-declaring egress paths
// (defaultRoute, model-only rules) MUST gain their own enforcement, and this test
// should flip to expect rejection. It exists so that change cannot land silently.
func TestUnsafeSensitiveRouteMessage_PerDeclaringRuleLimitation(t *testing.T) {
	backends := []inferencev1alpha1.RouterBackend{
		{Name: "metal-a", Tier: "local"},
		{Name: "cloudish", Tier: "cloud"},
	}

	t.Run("model-only rule to a cloud backend is not inspected", func(t *testing.T) {
		mr := &inferencev1alpha1.ModelRouter{
			Spec: inferencev1alpha1.ModelRouterSpec{
				Backends: backends,
				Rules: []inferencev1alpha1.RouterRule{
					{
						// A safe, sensitive-DECLARING rule: the guard inspects and passes it.
						Name:       "pii-safe",
						FailClosed: true,
						Match:      &inferencev1alpha1.RuleMatch{DataClassification: []string{"pii"}},
						Route:      inferencev1alpha1.RuleRoute{Backends: []string{"metal-a"}},
					},
					{
						// A model-only rule (declares no class) routing to a cloud backend.
						// pii-headed traffic could match THIS rule (first-match-wins), but
						// the guard does not inspect it. Documents the limitation.
						Name:  "model-only-cloud",
						Match: &inferencev1alpha1.RuleMatch{Models: []string{"gpt-4"}},
						Route: inferencev1alpha1.RuleRoute{Backends: []string{"cloudish"}},
					},
				},
			},
		}
		if msg := unsafeSensitiveRouteMessage(mr); msg != "" {
			t.Fatalf("guard is per-declaring-rule; expected no rejection for the model-only rule, got: %s", msg)
		}
	})

	t.Run("defaultRoute to a cloud backend is not inspected", func(t *testing.T) {
		mr := &inferencev1alpha1.ModelRouter{
			Spec: inferencev1alpha1.ModelRouterSpec{
				Backends:     backends,
				DefaultRoute: "cloudish",
				Rules: []inferencev1alpha1.RouterRule{
					{
						Name:  "non-sensitive",
						Match: &inferencev1alpha1.RuleMatch{Models: []string{"gpt-4"}},
						Route: inferencev1alpha1.RuleRoute{Backends: []string{"metal-a"}},
					},
				},
			},
		}
		if msg := unsafeSensitiveRouteMessage(mr); msg != "" {
			t.Fatalf("guard does not inspect defaultRoute; expected no rejection, got: %s", msg)
		}
	})
}
