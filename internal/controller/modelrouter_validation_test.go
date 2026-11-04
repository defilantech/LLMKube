/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	testRouterLocalBackend = "local-qwen"
	testRouterCloudBackend = "cloud-opus"
	testRouterISName       = "qwen3-coder"
	testRouterTierCloud    = "cloud"
)

func ptrInt64Local(v int64) *int64 { return &v }

// validRouter returns a baseline ModelRouter that passes every validation
// check. Subtests mutate it to exercise specific failure modes.
func validRouter() *inferencev1alpha1.ModelRouter {
	return &inferencev1alpha1.ModelRouter{
		Spec: inferencev1alpha1.ModelRouterSpec{
			Backends: []inferencev1alpha1.RouterBackend{
				{
					Name:                testRouterLocalBackend,
					InferenceServiceRef: &corev1.LocalObjectReference{Name: testRouterISName},
					Tier:                "local",
				},
				{
					Name: testRouterCloudBackend,
					External: &inferencev1alpha1.ExternalProvider{
						Provider: "anthropic",
						Model:    "claude-opus-4-7",
					},
					Tier: testRouterTierCloud,
				},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name: "pii-stays-local",
					Match: &inferencev1alpha1.RuleMatch{
						DataClassification: []string{"pii"},
					},
					Route: inferencev1alpha1.RuleRoute{
						Backends: []string{testRouterLocalBackend},
					},
					FailClosed: true,
				},
				{
					Name: "complex-to-cloud",
					Match: &inferencev1alpha1.RuleMatch{
						TaskComplexity: "complex",
					},
					Route: inferencev1alpha1.RuleRoute{
						Backends: []string{testRouterCloudBackend, testRouterLocalBackend},
					},
				},
			},
			DefaultRoute: testRouterLocalBackend,
		},
	}
}

// TestValidateModelRouterValid ensures the canonical fixture passes cleanly.
func TestValidateModelRouterValid(t *testing.T) {
	errs := validateModelRouter(validRouter())
	if len(errs) != 0 {
		t.Fatalf("expected no validation errors, got: %s", formatValidationErrors(errs))
	}
}

// TestValidateModelRouterBackendExclusivity covers the mutual-exclusion rule
// for inferenceServiceRef vs external on a RouterBackend.
func TestValidateModelRouterBackendExclusivity(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*inferencev1alpha1.ModelRouter)
		wantSubstr string
	}{
		{
			name: "both set",
			mutate: func(mr *inferencev1alpha1.ModelRouter) {
				mr.Spec.Backends[0].External = &inferencev1alpha1.ExternalProvider{
					Provider: "anthropic", Model: "x",
				}
			},
			wantSubstr: "not both",
		},
		{
			name: "neither set",
			mutate: func(mr *inferencev1alpha1.ModelRouter) {
				mr.Spec.Backends[0].InferenceServiceRef = nil
			},
			wantSubstr: "must be set",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := validRouter()
			tc.mutate(mr)
			errs := validateModelRouter(mr)
			if !errsContain(errs, tc.wantSubstr) {
				t.Errorf("expected error containing %q, got: %s",
					tc.wantSubstr, formatValidationErrors(errs))
			}
		})
	}
}

// TestValidateModelRouterTierConsistency confirms an inferenceServiceRef
// backend cannot be labelled tier=cloud. Allowing that would let users sneak
// a local backend past the fail-closed gate.
func TestValidateModelRouterTierConsistency(t *testing.T) {
	mr := validRouter()
	mr.Spec.Backends[0].Tier = testRouterTierCloud
	errs := validateModelRouter(mr)
	if !errsContain(errs, "tier=cloud is invalid") {
		t.Errorf("expected tier consistency error, got: %s", formatValidationErrors(errs))
	}
}

// TestValidateModelRouterDuplicateBackendNames ensures repeated names are
// flagged. Downstream resolution uses backend name as a map key so dupes
// would silently shadow each other.
func TestValidateModelRouterDuplicateBackendNames(t *testing.T) {
	mr := validRouter()
	mr.Spec.Backends = append(mr.Spec.Backends, inferencev1alpha1.RouterBackend{
		Name:                testRouterLocalBackend,
		InferenceServiceRef: &corev1.LocalObjectReference{Name: "other"},
		Tier:                "local",
	})
	errs := validateModelRouter(mr)
	if !errsContain(errs, "duplicate backend name") {
		t.Errorf("expected duplicate backend name error, got: %s", formatValidationErrors(errs))
	}
}

// TestValidateModelRouterDefaultRouteRef ensures defaultRoute must point at
// a real backend.
func TestValidateModelRouterDefaultRouteRef(t *testing.T) {
	mr := validRouter()
	mr.Spec.DefaultRoute = "does-not-exist"
	errs := validateModelRouter(mr)
	if !errsContain(errs, "references undefined backend") {
		t.Errorf("expected undefined-backend error, got: %s", formatValidationErrors(errs))
	}
}

// TestValidateModelRouterRuleBackendRef ensures rule.route.backends entries
// must point at real backends.
func TestValidateModelRouterRuleBackendRef(t *testing.T) {
	mr := validRouter()
	mr.Spec.Rules[1].Route.Backends = []string{"ghost"}
	errs := validateModelRouter(mr)
	if !errsContain(errs, `references undefined backend "ghost"`) {
		t.Errorf("expected undefined backend error, got: %s", formatValidationErrors(errs))
	}
}

// TestValidateModelRouterRuleEmptyBackends ensures rule.route.backends must
// be non-empty (kubebuilder MinItems is enforced at admission, but we
// double-check defensively in case of CRD bypass).
func TestValidateModelRouterRuleEmptyBackends(t *testing.T) {
	mr := validRouter()
	mr.Spec.Rules[0].Route.Backends = nil
	errs := validateModelRouter(mr)
	if !errsContain(errs, "must reference at least one backend") {
		t.Errorf("expected empty-backends error, got: %s", formatValidationErrors(errs))
	}
}

// TestValidateModelRouterSensitiveDataRequiresFailClosed is the headline
// gate: any rule matching pii/phi must have failClosed=true.
func TestValidateModelRouterSensitiveDataRequiresFailClosed(t *testing.T) {
	mr := validRouter()
	mr.Spec.Rules[0].FailClosed = false
	errs := validateModelRouter(mr)
	if !errsContain(errs, "must set failClosed=true") {
		t.Errorf("expected failClosed error, got: %s", formatValidationErrors(errs))
	}
}

// TestValidateModelRouterSensitiveDataCannotRouteCloud is the other half of
// the fail-closed gate: even with failClosed=true, a sensitive rule cannot
// reference cloud-tier backends. Otherwise "fail-closed" would mean "egress
// once, then refuse on retry," which defeats the purpose.
func TestValidateModelRouterSensitiveDataCannotRouteCloud(t *testing.T) {
	mr := validRouter()
	mr.Spec.Rules[0].Route.Backends = []string{testRouterCloudBackend}
	errs := validateModelRouter(mr)
	if !errsContain(errs, "cannot route to cloud-tier backend") {
		t.Errorf("expected sensitive-to-cloud error, got: %s", formatValidationErrors(errs))
	}
}

// TestValidateModelRouterCustomSensitiveClassifications confirms the
// per-router override of the sensitive-classification set works: a router
// that adds "secret" to the set should reject a "secret"-matching rule that
// routes to cloud.
func TestValidateModelRouterCustomSensitiveClassifications(t *testing.T) {
	mr := validRouter()
	mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
		Classification: &inferencev1alpha1.ClassificationPolicy{
			SensitiveClassifications: []string{"secret", "internal-only"},
		},
	}
	// Add a rule matching "secret" that routes to cloud. Should be rejected.
	mr.Spec.Rules = append(mr.Spec.Rules, inferencev1alpha1.RouterRule{
		Name: "secret-leak",
		Match: &inferencev1alpha1.RuleMatch{
			DataClassification: []string{"secret"},
		},
		Route: inferencev1alpha1.RuleRoute{
			Backends: []string{testRouterCloudBackend},
		},
		FailClosed: true,
	})
	errs := validateModelRouter(mr)
	if !errsContain(errs, "cannot route to cloud-tier backend") {
		t.Errorf("expected sensitive-to-cloud error for custom classification, got: %s",
			formatValidationErrors(errs))
	}

	// Also confirm that the *default* sensitive classifications no longer
	// apply when an override is set: a "pii"-matching rule under this
	// override would no longer trigger fail-closed (since pii is not in the
	// override list). Drop the pii rule's failClosed and verify no error.
	mr2 := validRouter()
	mr2.Spec.Policy = &inferencev1alpha1.RouterPolicy{
		Classification: &inferencev1alpha1.ClassificationPolicy{
			SensitiveClassifications: []string{"secret"},
		},
	}
	mr2.Spec.Rules[0].FailClosed = false
	errs = validateModelRouter(mr2)
	if errsContain(errs, "must set failClosed=true") {
		t.Errorf("override should disable the default pii gate, got: %s", formatValidationErrors(errs))
	}
}

// TestValidateModelRouterRefusesUnimplementedClassifier pins the Proxy data
// plane's refusal of classification modes that need a classifier the
// router-proxy does not have. The proxy reads a request's classification from
// the header alone, so under "detector" or "hybrid" a request whose body holds
// an SSN and carries no header would match no pii rule and fall through to the
// defaultRoute, here a cloud backend. An empty dataPlane is Proxy: the CRD
// defaults it, and the webhook sees the defaulted object.
func TestValidateModelRouterRefusesUnimplementedClassifier(t *testing.T) {
	for _, plane := range []inferencev1alpha1.ModelRouterDataPlane{"", inferencev1alpha1.ModelRouterDataPlaneProxy} {
		planeName := string(plane)
		if planeName == "" {
			planeName = "unset"
		}
		for _, mode := range []string{"detector", "hybrid"} {
			t.Run(planeName+"/"+mode, func(t *testing.T) {
				mr := validRouter()
				mr.Spec.DataPlane = plane
				mr.Spec.DefaultRoute = testRouterCloudBackend
				mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
					Classification: &inferencev1alpha1.ClassificationPolicy{Mode: mode},
				}
				errs := validateModelRouter(mr)
				var got *ModelRouterValidationError
				for i := range errs {
					if errs[i].Field == "spec.policy.classification.mode" {
						got = &errs[i]
					}
				}
				if got == nil {
					t.Fatalf("mode %q on the Proxy data plane passed validation; want an error at "+
						"spec.policy.classification.mode, got: %q", mode, formatValidationErrors(errs))
				}
				for _, want := range []string{`"` + mode + `"`, "header-only", "#441"} {
					if !strings.Contains(got.Message, want) {
						t.Errorf("message %q does not mention %s", got.Message, want)
					}
				}
			})
		}
	}
}

// TestValidateModelRouterAcceptsHeaderOnlyClassification keeps the refusal
// from reaching the implemented mode: header-only, an unset mode, and an unset
// classification block all pass.
func TestValidateModelRouterAcceptsHeaderOnlyClassification(t *testing.T) {
	for name, policy := range map[string]*inferencev1alpha1.RouterPolicy{
		"no policy":         nil,
		"no classification": {},
		"empty mode":        {Classification: &inferencev1alpha1.ClassificationPolicy{}},
		"header-only":       {Classification: &inferencev1alpha1.ClassificationPolicy{Mode: "header-only"}},
	} {
		t.Run(name, func(t *testing.T) {
			mr := validRouter()
			mr.Spec.Policy = policy
			if errs := validateModelRouter(mr); len(errs) != 0 {
				t.Errorf("expected no validation errors, got: %s", formatValidationErrors(errs))
			}
		})
	}
}

// TestValidateModelRouterClassifierRefusalIsProxyOnly pins the scope of the
// refusal. The Gateway and AgentGateway data planes keep their own check, a
// dataClassification match under detector or hybrid is refused there by
// unsupportedMatchFields, and a router on those planes that sets the mode
// without such a match compiles nothing that reads it.
func TestValidateModelRouterClassifierRefusalIsProxyOnly(t *testing.T) {
	for _, plane := range []inferencev1alpha1.ModelRouterDataPlane{
		inferencev1alpha1.ModelRouterDataPlaneGateway,
		inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
	} {
		mr := validRouter()
		mr.Spec.DataPlane = plane
		mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
			Classification: &inferencev1alpha1.ClassificationPolicy{Mode: "detector"},
		}
		if errsContain(validateModelRouter(mr), "spec.policy.classification.mode") {
			t.Errorf("%s: static validation refused the mode; that data plane refuses it in its own match check", plane)
		}
	}
}

// TestValidationFailureReason: the Validated reason is
// ClassifierNotImplemented only when the unimplemented mode is the spec's
// sole problem. A spec that is also malformed reports SpecInvalid, so the
// reason never suggests that changing the mode alone makes the router valid.
func TestValidationFailureReason(t *testing.T) {
	classifier := func(mode string) *inferencev1alpha1.ModelRouter {
		mr := validRouter()
		mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
			Classification: &inferencev1alpha1.ClassificationPolicy{Mode: mode},
		}
		return mr
	}
	alsoMalformed := classifier("detector")
	alsoMalformed.Spec.DefaultRoute = "ghost"
	malformed := validRouter()
	malformed.Spec.DefaultRoute = "ghost"

	for name, tc := range map[string]struct {
		mr   *inferencev1alpha1.ModelRouter
		want string
	}{
		"detector only":       {classifier("detector"), "ClassifierNotImplemented"},
		"hybrid only":         {classifier("hybrid"), "ClassifierNotImplemented"},
		"detector and a typo": {alsoMalformed, ModelRouterReasonSpecInvalid},
		"a typo only":         {malformed, ModelRouterReasonSpecInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			errs := validateModelRouter(tc.mr)
			if len(errs) == 0 {
				t.Fatal("test setup: expected validation errors")
			}
			if got := validationFailureReason(errs); got != tc.want {
				t.Errorf("reason = %q, want %q (errors: %s)", got, tc.want, formatValidationErrors(errs))
			}
		})
	}
}

// TestValidateModelRouterBudget covers BudgetSpec validation:
// rule-scoped budgets must reference a real rule, and every budget must set
// at least one of maxTokens / maxUSD.
func TestValidateModelRouterBudget(t *testing.T) {
	cases := []struct {
		name       string
		budget     inferencev1alpha1.BudgetSpec
		wantSubstr string
	}{
		{
			name: "rule scope without ruleName",
			budget: inferencev1alpha1.BudgetSpec{
				Name:      "b",
				Scope:     "rule",
				MaxTokens: ptrInt64Local(1000),
			},
			wantSubstr: "ruleName is required",
		},
		{
			name: "rule scope referencing unknown rule",
			budget: inferencev1alpha1.BudgetSpec{
				Name:      "b",
				Scope:     "rule",
				RuleName:  "ghost",
				MaxTokens: ptrInt64Local(1000),
			},
			wantSubstr: `references undefined rule "ghost"`,
		},
		{
			name: "neither maxTokens nor maxUSD",
			budget: inferencev1alpha1.BudgetSpec{
				Name:  "b",
				Scope: "router",
			},
			wantSubstr: "must set at least one of maxTokens or maxUSD",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := validRouter()
			mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
				Budgets: []inferencev1alpha1.BudgetSpec{tc.budget},
			}
			errs := validateModelRouter(mr)
			if !errsContain(errs, tc.wantSubstr) {
				t.Errorf("expected error containing %q, got: %s",
					tc.wantSubstr, formatValidationErrors(errs))
			}
		})
	}
}

// TestValidateModelRouterBudgetPricing pins that a maxUSD budget requires
// every backend to declare usable pricing. Without it the budget charges 0
// USD and never blocks, so the spec must fail loud instead.
func TestValidateModelRouterBudgetPricing(t *testing.T) {
	t.Run("unpriced backend rejected", func(t *testing.T) {
		mr := validRouter()
		mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
			Budgets: []inferencev1alpha1.BudgetSpec{
				{Name: "usd-cap", Scope: "router", MaxUSD: "1.50"},
			},
		}
		errs := validateModelRouter(mr)
		if !errsContain(errs, "required when any budget sets maxUSD") {
			t.Errorf("expected a pricing error, got: %s", formatValidationErrors(errs))
		}
	})

	t.Run("all backends priced accepted", func(t *testing.T) {
		mr := validRouter()
		for i := range mr.Spec.Backends {
			mr.Spec.Backends[i].CostPerMillionTokens = &inferencev1alpha1.TokenCost{PromptUSD: "0.50"}
		}
		mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
			Budgets: []inferencev1alpha1.BudgetSpec{
				{Name: "usd-cap", Scope: "router", MaxUSD: "1.50"},
			},
		}
		errs := validateModelRouter(mr)
		if errsContain(errs, "required when any budget sets maxUSD") {
			t.Errorf("did not expect a pricing error; got: %s", formatValidationErrors(errs))
		}
	})

	t.Run("token budget needs no pricing", func(t *testing.T) {
		mr := validRouter()
		mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
			Budgets: []inferencev1alpha1.BudgetSpec{
				{Name: "tok-cap", Scope: "router", MaxTokens: ptrInt64Local(1000)},
			},
		}
		errs := validateModelRouter(mr)
		if errsContain(errs, "required when any budget sets maxUSD") {
			t.Errorf("did not expect a pricing error for a token-only budget; got: %s", formatValidationErrors(errs))
		}
	})
}

// errsContain reports whether any validation error's serialized form
// contains the given substring. Helper to keep the tests above readable.
func errsContain(errs []ModelRouterValidationError, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.String(), substr) {
			return true
		}
	}
	return false
}

// TestValidateRuleTimeoutBounds enforces the 100ms..30m window the
// controller applies to spec.rules[].timeout. Too short is almost
// certainly a config error; too long is almost certainly a unit typo.
func TestValidateRuleTimeoutBounds(t *testing.T) {
	cases := []struct {
		name       string
		timeout    time.Duration
		wantErrSub string // empty -> expect no error
	}{
		{"too short rejected", 50 * time.Millisecond, "must be >="},
		{"min boundary accepted", 100 * time.Millisecond, ""},
		{"reasonable accepted", 5 * time.Second, ""},
		{"max boundary accepted", 30 * time.Minute, ""},
		{"too long rejected", 1 * time.Hour, "must be <="},
		{"unit typo (30 typed as minutes)", 31 * time.Minute, "must be <="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := validRouter()
			mr.Spec.Rules[0].Timeout = &metav1.Duration{Duration: tc.timeout}
			errs := validateModelRouter(mr)

			if tc.wantErrSub == "" {
				if errsContain(errs, "timeout") {
					t.Errorf("did not expect a timeout error; got: %s", formatValidationErrors(errs))
				}
				return
			}
			if !errsContain(errs, tc.wantErrSub) {
				t.Errorf("want error containing %q; got: %s",
					tc.wantErrSub, formatValidationErrors(errs))
			}
		})
	}
}

// TestValidateBackendTimeoutBounds enforces the same window for
// spec.backends[].timeout. Mirrors TestValidateRuleTimeoutBounds.
func TestValidateBackendTimeoutBounds(t *testing.T) {
	cases := []struct {
		name       string
		timeout    time.Duration
		wantErrSub string
	}{
		{"too short rejected", 10 * time.Millisecond, "must be >="},
		{"reasonable accepted", 60 * time.Second, ""},
		{"too long rejected", 2 * time.Hour, "must be <="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := validRouter()
			mr.Spec.Backends[0].Timeout = &metav1.Duration{Duration: tc.timeout}
			errs := validateModelRouter(mr)

			if tc.wantErrSub == "" {
				if errsContain(errs, "backends[0].timeout") {
					t.Errorf("did not expect a backend timeout error; got: %s", formatValidationErrors(errs))
				}
				return
			}
			if !errsContain(errs, tc.wantErrSub) {
				t.Errorf("want %q; got: %s", tc.wantErrSub, formatValidationErrors(errs))
			}
		})
	}
}
