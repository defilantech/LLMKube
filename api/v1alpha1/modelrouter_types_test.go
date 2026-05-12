/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Test fixture constants. golangci-lint's goconst complains otherwise.
const (
	testLocalBackendName = "local-qwen"
	testMutationMarker   = "MUTATED"
)

// ptrInt32 and ptrInt64 keep the test fixtures readable.
func ptrInt32(v int32) *int32 { return &v }
func ptrInt64(v int64) *int64 { return &v }

// fullyPopulatedModelRouter returns a ModelRouter that exercises every
// non-trivial spec and status path. Used as the canonical test fixture so
// round-trip and deep-copy coverage stays in sync as the type evolves.
func fullyPopulatedModelRouter() *ModelRouter {
	return &ModelRouter{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "inference.llmkube.dev/v1alpha1",
			Kind:       "ModelRouter",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "coding-router",
			Namespace: "platform",
		},
		Spec: ModelRouterSpec{
			Backends: []RouterBackend{
				{
					Name: testLocalBackendName,
					InferenceServiceRef: &corev1.LocalObjectReference{
						Name: "qwen3-coder",
					},
					Tier:         "local",
					Capabilities: []string{"code", "tools"},
					Weight:       ptrInt32(80),
					CostPerMillionTokens: &TokenCost{
						PromptUSD:     "0",
						CompletionUSD: "0",
					},
					HealthCheck: &BackendHealthCheck{
						Path:            "/health",
						IntervalSeconds: 10,
						TimeoutSeconds:  2,
					},
				},
				{
					Name: "cloud-opus",
					External: &ExternalProvider{
						Provider: "anthropic",
						Model:    "claude-opus-4-7",
						CredentialsSecretRef: &corev1.LocalObjectReference{
							Name: "anthropic-key",
						},
					},
					Tier:         "cloud",
					Capabilities: []string{"code", "vision", "long-context"},
					Weight:       ptrInt32(20),
					CostPerMillionTokens: &TokenCost{
						PromptUSD:     "15.00",
						CompletionUSD: "75.00",
					},
				},
			},
			Rules: []RouterRule{
				{
					Name: "pii-stays-local",
					Match: &RuleMatch{
						DataClassification: []string{"pii", "phi"},
					},
					Route: RuleRoute{
						Backends: []string{testLocalBackendName},
						Strategy: "primary-fallback",
					},
					FailClosed: true,
				},
				{
					Name: "hard-cases-to-cloud",
					Match: &RuleMatch{
						TaskComplexity:       "complex",
						RequiredCapabilities: []string{"long-context"},
						LatencySLOMs:         ptrInt32(2000),
						Headers:              map[string]string{"x-team": "research"},
						Models:               []string{"qwen3-*", "claude-*"},
					},
					Route: RuleRoute{
						Backends: []string{"cloud-opus", testLocalBackendName},
						Strategy: "primary-fallback",
					},
				},
			},
			DefaultRoute: testLocalBackendName,
			Policy: &RouterPolicy{
				Budgets: []BudgetSpec{
					{
						Name:          "monthly-team-cap",
						Scope:         "team",
						HeaderKey:     "x-llmkube-team",
						WindowSeconds: 2592000,
						MaxTokens:     ptrInt64(10_000_000),
						MaxUSD:        "1000.00",
					},
				},
				Classification: &ClassificationPolicy{
					Mode:                     "hybrid",
					HeaderKey:                "x-llmkube-classification",
					SensitiveClassifications: []string{"pii", "phi"},
				},
				AuditLog: &AuditLogPolicy{
					Sink:               "stdout",
					IncludeRequestBody: false,
				},
			},
			Endpoint: &EndpointSpec{
				Port: 8080,
				Path: "/v1/chat/completions",
				Type: "ClusterIP",
			},
			Proxy: &RouterProxySpec{
				Replicas: ptrInt32(2),
				Image:    "ghcr.io/defilantech/llmkube-router-proxy:v0.1.0",
				ImagePullSecrets: []corev1.LocalObjectReference{
					{Name: "ghcr-creds"},
				},
			},
			MCPServer: &MCPServerSpec{
				Enabled: false,
			},
		},
		Status: ModelRouterStatus{
			Phase:    "Ready",
			Endpoint: "http://coding-router.platform.svc.cluster.local:8080/v1/chat/completions",
			Backends: []BackendStatus{
				{
					Name:    testLocalBackendName,
					Tier:    "local",
					Address: "http://qwen3-coder.platform.svc.cluster.local:8080",
					Healthy: true,
				},
				{
					Name:    "cloud-opus",
					Tier:    "cloud",
					Address: "https://api.anthropic.com",
					Healthy: true,
				},
			},
			ActiveRules: 2,
			BudgetUtilization: []BudgetStatus{
				{
					Name:        "monthly-team-cap",
					TokensUsed:  4_200_000,
					USDUsed:     "412.50",
					Utilization: "0.42",
				},
			},
			Conditions: []metav1.Condition{
				{Type: "Validated", Status: metav1.ConditionTrue, Reason: "OK"},
				{Type: "Available", Status: metav1.ConditionTrue, Reason: "AllBackendsHealthy"},
			},
		},
	}
}

// TestModelRouterDeepCopyIndependence verifies the generated DeepCopy methods
// produce a fully independent clone: mutating slices, maps, and pointer fields
// on the copy must not be visible on the original.
func TestModelRouterDeepCopyIndependence(t *testing.T) {
	orig := fullyPopulatedModelRouter()
	clone := orig.DeepCopy()

	if !reflect.DeepEqual(orig, clone) {
		t.Fatalf("clone differs from original immediately after DeepCopy")
	}

	// Mutate slices on the clone.
	clone.Spec.Backends[0].Capabilities[0] = testMutationMarker
	clone.Spec.Backends = append(clone.Spec.Backends, RouterBackend{Name: "appended"})
	clone.Spec.Rules[0].Route.Backends[0] = testMutationMarker
	clone.Status.Backends[0].Name = testMutationMarker
	clone.Status.Conditions[0].Reason = testMutationMarker

	if got := orig.Spec.Backends[0].Capabilities[0]; got != "code" {
		t.Errorf("original Spec.Backends[0].Capabilities[0] = %q; want %q", got, "code")
	}
	if len(orig.Spec.Backends) != 2 {
		t.Errorf("len(original Spec.Backends) = %d; want 2", len(orig.Spec.Backends))
	}
	if got := orig.Spec.Rules[0].Route.Backends[0]; got != testLocalBackendName {
		t.Errorf("original Spec.Rules[0].Route.Backends[0] = %q; want %q", got, testLocalBackendName)
	}
	if got := orig.Status.Backends[0].Name; got != testLocalBackendName {
		t.Errorf("original Status.Backends[0].Name = %q; want %q", got, testLocalBackendName)
	}
	if got := orig.Status.Conditions[0].Reason; got != "OK" {
		t.Errorf("original Status.Conditions[0].Reason = %q; want %q", got, "OK")
	}

	// Mutate maps and pointer fields on the clone.
	clone.Spec.Rules[1].Match.Headers["x-team"] = testMutationMarker
	*clone.Spec.Backends[0].Weight = 0

	if got := orig.Spec.Rules[1].Match.Headers["x-team"]; got != "research" {
		t.Errorf("original Spec.Rules[1].Match.Headers[x-team] = %q; want %q", got, "research")
	}
	if got := *orig.Spec.Backends[0].Weight; got != 80 {
		t.Errorf("original Spec.Backends[0].Weight = %d; want 80", got)
	}
}

// TestModelRouterJSONRoundTrip confirms every field marshals and unmarshals
// through JSON without loss. This catches missing or incorrect json tags.
func TestModelRouterJSONRoundTrip(t *testing.T) {
	orig := fullyPopulatedModelRouter()

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var got ModelRouter
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if !reflect.DeepEqual(orig, &got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", orig, &got)
	}
}

// TestModelRouterListDeepCopy verifies the list type's DeepCopy is independent.
func TestModelRouterListDeepCopy(t *testing.T) {
	list := &ModelRouterList{
		Items: []ModelRouter{
			*fullyPopulatedModelRouter(),
		},
	}
	clone := list.DeepCopy()

	if !reflect.DeepEqual(list, clone) {
		t.Fatal("ModelRouterList clone differs from original")
	}

	clone.Items[0].Spec.DefaultRoute = testMutationMarker
	if got := list.Items[0].Spec.DefaultRoute; got != testLocalBackendName {
		t.Errorf("original Items[0].Spec.DefaultRoute = %q; want %q", got, testLocalBackendName)
	}
}

// TestRouterBackendMutualExclusion documents (not enforces) the intended
// invariant that exactly one of InferenceServiceRef or External is set. The
// runtime enforcement lives in the ModelRouterReconciler validation (#426);
// this test just confirms the type permits both fields independently so the
// reconciler has a real choice to make.
func TestRouterBackendMutualExclusion(t *testing.T) {
	cases := []struct {
		name string
		b    RouterBackend
	}{
		{
			name: "local-only",
			b: RouterBackend{
				Name:                "x",
				InferenceServiceRef: &corev1.LocalObjectReference{Name: "y"},
			},
		},
		{
			name: "external-only",
			b: RouterBackend{
				Name:     "x",
				External: &ExternalProvider{Provider: "anthropic", Model: "m"},
			},
		},
		{
			name: "both-set-permitted-by-type-rejected-by-reconciler",
			b: RouterBackend{
				Name:                "x",
				InferenceServiceRef: &corev1.LocalObjectReference{Name: "y"},
				External:            &ExternalProvider{Provider: "anthropic", Model: "m"},
			},
		},
		{
			name: "neither-set-permitted-by-type-rejected-by-reconciler",
			b:    RouterBackend{Name: "x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.b)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got RouterBackend
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(tc.b, got) {
				t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", tc.b, got)
			}
		})
	}
}

// TestRuleMatchEmptyJSON ensures a rule with no match expression marshals
// without producing a non-omitempty empty match object. Rules without a match
// are valid (they fire for every request) and the wire shape should reflect
// that.
func TestRuleMatchEmptyJSON(t *testing.T) {
	rule := RouterRule{
		Name:  "catch-all",
		Route: RuleRoute{Backends: []string{"only-backend"}},
	}
	data, err := json.Marshal(rule)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got, ok := jsonHasKey(data, "match"); ok {
		t.Errorf("RouterRule with no Match emitted \"match\" key: %s", got)
	}
}

// TestPolicyOmitemptyShape verifies that an absent Policy block is omitted
// entirely from the marshaled spec (not emitted as `"policy":null` or
// `"policy":{}`). This keeps the wire shape clean for minimal manifests.
func TestPolicyOmitemptyShape(t *testing.T) {
	r := &ModelRouter{
		Spec: ModelRouterSpec{
			Backends: []RouterBackend{
				{Name: "b", InferenceServiceRef: &corev1.LocalObjectReference{Name: "s"}},
			},
		},
	}
	data, err := json.Marshal(r.Spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, ok := jsonHasKey(data, "policy"); ok {
		t.Errorf("ModelRouterSpec emitted \"policy\" key when Policy was nil: %s", data)
	}
	if _, ok := jsonHasKey(data, "rules"); ok {
		t.Errorf("ModelRouterSpec emitted \"rules\" key when Rules was nil: %s", data)
	}
	if _, ok := jsonHasKey(data, "endpoint"); ok {
		t.Errorf("ModelRouterSpec emitted \"endpoint\" key when Endpoint was nil: %s", data)
	}
}

// TestSchemeRegistration confirms the ModelRouter and ModelRouterList types
// are registered with the package's SchemeBuilder (i.e. init() ran).
func TestSchemeRegistration(t *testing.T) {
	// SchemeBuilder.Build() returns a Scheme with the registered types. If
	// init() registered ModelRouter, the type's GVK will resolve.
	scheme, err := SchemeBuilder.Build()
	if err != nil {
		t.Fatalf("SchemeBuilder.Build: %v", err)
	}
	gvks, _, err := scheme.ObjectKinds(&ModelRouter{})
	if err != nil {
		t.Fatalf("scheme.ObjectKinds(ModelRouter): %v", err)
	}
	if len(gvks) == 0 {
		t.Fatal("ModelRouter not registered in scheme")
	}
	if gvks[0].Group != "inference.llmkube.dev" || gvks[0].Version != "v1alpha1" || gvks[0].Kind != "ModelRouter" {
		t.Errorf("unexpected GVK %s", gvks[0])
	}
}

// jsonHasKey returns (value, true) if the top-level JSON object has the given
// key; (nil, false) otherwise. Used to assert omitempty behavior.
func jsonHasKey(data []byte, key string) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	v, ok := m[key]
	return v, ok
}
