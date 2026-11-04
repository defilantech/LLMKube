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

package controller

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// validGatewayRouter builds a baseline dataPlane: Gateway ModelRouter that
// passes both the static spec validation and every gateway-mode honest-boundary
// check. Each test mutates one field to exercise a single rejection branch.
func validGatewayRouter() *inferencev1alpha1.ModelRouter {
	return &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen-router", Namespace: "default"},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneGateway,
			GatewayRef: &inferencev1alpha1.GatewayReference{
				Name:      "ai-gateway",
				Namespace: "ai-gateway",
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "qwen-cuda", InferenceServiceRef: corev1LocalRef("qwen-cuda")},
				{Name: "qwen-metal", InferenceServiceRef: corev1LocalRef("qwen-metal")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "qwen",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{
						Backends: []string{"qwen-cuda", "qwen-metal"},
						Strategy: "primary-fallback",
					},
				},
			},
		},
	}
}

func TestModelRouterValidator_Create(t *testing.T) {
	v := &ModelRouterValidator{}
	ctx := context.Background()

	tests := []struct {
		name      string
		mutate    func(*inferencev1alpha1.ModelRouter)
		wantError bool
	}{
		{
			name:      "valid gateway router accepted",
			mutate:    func(*inferencev1alpha1.ModelRouter) {},
			wantError: false,
		},
		{
			name: "static spec violation (undefined defaultRoute) rejected",
			mutate: func(mr *inferencev1alpha1.ModelRouter) {
				mr.Spec.DefaultRoute = "does-not-exist"
			},
			wantError: true,
		},
		{
			name: "unsupported match (taskComplexity) rejected",
			mutate: func(mr *inferencev1alpha1.ModelRouter) {
				mr.Spec.Rules[0].Match.TaskComplexity = "high"
			},
			wantError: true,
		},
		{
			name: "unsupported strategy (shadow) rejected",
			mutate: func(mr *inferencev1alpha1.ModelRouter) {
				mr.Spec.Rules[0].Route.Strategy = "shadow"
			},
			wantError: true,
		},
		{
			name: "unsupported budget (maxUSD dollar budget) rejected",
			mutate: func(mr *inferencev1alpha1.ModelRouter) {
				mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
					Budgets: []inferencev1alpha1.BudgetSpec{
						{Name: "spend", Scope: "router", MaxUSD: "10.50"},
					},
				}
			},
			wantError: true,
		},
		{
			name: "unsupported budget scope (rule) rejected",
			mutate: func(mr *inferencev1alpha1.ModelRouter) {
				maxTokens := int64(1000)
				mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
					Budgets: []inferencev1alpha1.BudgetSpec{
						{Name: "per-rule", Scope: "rule", RuleName: "qwen", MaxTokens: &maxTokens},
					},
				}
			},
			wantError: true,
		},
		{
			name: "invalid auth (jwt missing issuer/jwksURI/teamClaim) rejected",
			mutate: func(mr *inferencev1alpha1.ModelRouter) {
				mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
					Auth: &inferencev1alpha1.RouterAuthSpec{
						JWT: &inferencev1alpha1.JWTAuthSpec{Provider: "keycloak"},
					},
				}
			},
			wantError: true,
		},
		{
			name: "invalid authorization (allowlists without jwt) rejected",
			mutate: func(mr *inferencev1alpha1.ModelRouter) {
				mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
					Auth: &inferencev1alpha1.RouterAuthSpec{
						Allowlists: []inferencev1alpha1.TeamModelAllowlist{
							{Team: "platform", Models: []string{"qwen35-27b"}},
						},
					},
				}
			},
			wantError: true,
		},
		{
			name: "auditLog in gateway mode rejected",
			mutate: func(mr *inferencev1alpha1.ModelRouter) {
				mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
					AuditLog: &inferencev1alpha1.AuditLogPolicy{Sink: "stdout"},
				}
			},
			wantError: true,
		},
		{
			name: "unsafe sensitive route (pii rule not failClosed) rejected",
			mutate: func(mr *inferencev1alpha1.ModelRouter) {
				mr.Spec.Rules[0].Match.DataClassification = []string{"pii"}
				mr.Spec.Rules[0].FailClosed = false
			},
			wantError: true,
		},
		{
			name: "safe sensitive route (pii rule failClosed, local-only) accepted",
			mutate: func(mr *inferencev1alpha1.ModelRouter) {
				mr.Spec.Rules[0].Match.DataClassification = []string{"pii"}
				mr.Spec.Rules[0].FailClosed = true
			},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := validGatewayRouter()
			tt.mutate(mr)
			_, err := v.ValidateCreate(ctx, mr)
			if tt.wantError && err == nil {
				t.Fatalf("expected validation error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tt.wantError && err != nil && !apierrors.IsInvalid(err) {
				t.Fatalf("expected an Invalid error, got %T: %v", err, err)
			}
		})
	}
}

// validAgentGatewayRouter returns a valid dataPlane: AgentGateway router with
// ONE backend, hence one generated InferencePool: the only multi-ref shape the
// agentgateway plane compiles while a single Endpoint Picker serves it
// (agentGatewayResolvePoolPickers). The Gateway reference is cross-namespace
// (the common setup), which admission accepts.
func validAgentGatewayRouter() *inferencev1alpha1.ModelRouter {
	return &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "agw-router", Namespace: "default"},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name:               "ai-gateway",
				Namespace:          "agentgateway-system",
				EndpointPicker:     "llm-d-router-epp",
				EndpointPickerPort: 9002,
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "qwen-cuda", InferenceServiceRef: corev1LocalRef("qwen-cuda")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "qwen",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{
						Backends: []string{"qwen-cuda"},
						Strategy: "primary-fallback",
					},
				},
			},
		},
	}
}

// TestModelRouterValidator_AgentGatewayModeRejectsInexpressible proves the
// agentgateway plane's honest-boundary checks are enforced at apply time, not
// just at reconcile time: a config the agentgateway data plane cannot honor is
// rejected with an Invalid error.
func TestModelRouterValidator_AgentGatewayModeRejectsInexpressible(t *testing.T) {
	v := &ModelRouterValidator{}
	ctx := context.Background()

	mr := validAgentGatewayRouter()
	// auth is not supported on the agentgateway Inference-Extension path.
	mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
		Auth: &inferencev1alpha1.RouterAuthSpec{
			JWT: &inferencev1alpha1.JWTAuthSpec{
				Provider:  "keycloak",
				Issuer:    "https://issuer.example.com",
				JWKSURI:   "https://issuer.example.com/certs",
				TeamClaim: "team",
			},
		},
	}

	_, err := v.ValidateCreate(ctx, mr)
	if err == nil {
		t.Fatal("expected AgentGateway-mode router with unsupported auth to be rejected at apply time")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "policy.auth is not supported in dataPlane: AgentGateway") {
		t.Errorf("expected the unsupported-auth message, got %v", err)
	}
}

// TestModelRouterValidator_AgentGatewayModeAcceptsExpressible proves a router
// that uses only what the agentgateway plane supports (model/header matches,
// primary-fallback, no auth/budget/audit, cross-namespace Gateway) passes
// apply-time validation.
func TestModelRouterValidator_AgentGatewayModeAcceptsExpressible(t *testing.T) {
	v := &ModelRouterValidator{}
	ctx := context.Background()

	if _, err := v.ValidateCreate(ctx, validAgentGatewayRouter()); err != nil {
		t.Fatalf("expected a valid AgentGateway-mode router to pass, got %v", err)
	}
}

// TestModelRouterValidator_AgentGatewayModeRejectsMultiplePools proves the
// multi-pool rejection is enforced at apply time: backends on two distinct
// InferenceServices compile to two InferencePools sharing one Endpoint Picker,
// which a Gateway API Inference Extension EPP cannot serve (it binds to
// exactly one pool).
func TestModelRouterValidator_AgentGatewayModeRejectsMultiplePools(t *testing.T) {
	v := &ModelRouterValidator{}
	ctx := context.Background()

	mr := validAgentGatewayRouter()
	mr.Spec.Backends = append(mr.Spec.Backends,
		inferencev1alpha1.RouterBackend{Name: "qwen-metal", InferenceServiceRef: corev1LocalRef("qwen-metal")})
	mr.Spec.Rules[0].Route.Backends = []string{"qwen-cuda", "qwen-metal"}

	_, err := v.ValidateCreate(ctx, mr)
	if err == nil {
		t.Fatal("expected a two-pool AgentGateway router to be rejected at apply time")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "exactly one pool") {
		t.Errorf("expected the single-EPP message, got %v", err)
	}
}

// TestModelRouterValidator_AgentGatewayModeRequiresAgentGatewayRef mirrors the
// reconciler's AgentGatewayRefMissing guard at apply time: a dataPlane:
// AgentGateway router without agentGatewayRef is rejected, not just marked
// not-ready after admission.
func TestModelRouterValidator_AgentGatewayModeRequiresAgentGatewayRef(t *testing.T) {
	v := &ModelRouterValidator{}
	ctx := context.Background()

	mr := validAgentGatewayRouter()
	mr.Spec.AgentGatewayRef = nil

	_, err := v.ValidateCreate(ctx, mr)
	if err == nil {
		t.Fatal("expected an AgentGateway-mode router without agentGatewayRef to be rejected at apply time")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "agentGatewayRef is unset") {
		t.Errorf("expected the missing-ref message, got %v", err)
	}
}

// TestModelRouterValidator_ProxyModeSkipsGatewayChecks proves the gateway-mode
// honest-boundary checks are NOT applied to a Proxy-mode router: a config that
// the gateway data plane cannot express (a dollar budget, an auditLog directive)
// is perfectly valid in Proxy mode, where the router-proxy enforces it directly.
func TestModelRouterValidator_ProxyModeSkipsGatewayChecks(t *testing.T) {
	v := &ModelRouterValidator{}
	ctx := context.Background()

	mr := validGatewayRouter()
	// Proxy mode (the default). The router-proxy honors dollar budgets and
	// auditLog, so neither is a violation here. A dollar budget needs backend
	// pricing, which is a Proxy-mode concern, not a gateway check.
	for i := range mr.Spec.Backends {
		mr.Spec.Backends[i].CostPerMillionTokens = &inferencev1alpha1.TokenCost{PromptUSD: "0.50"}
	}
	mr.Spec.DataPlane = inferencev1alpha1.ModelRouterDataPlaneProxy
	mr.Spec.GatewayRef = nil
	mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
		Budgets: []inferencev1alpha1.BudgetSpec{
			{Name: "spend", Scope: "router", MaxUSD: "10.50"},
		},
		AuditLog: &inferencev1alpha1.AuditLogPolicy{Sink: "stdout"},
	}
	// A taskComplexity match is inexpressible in the gateway data plane but is
	// a first-class Proxy-mode match.
	mr.Spec.Rules[0].Match.TaskComplexity = "high"

	if _, err := v.ValidateCreate(ctx, mr); err != nil {
		t.Fatalf("expected Proxy-mode router with gateway-inexpressible config to pass, got %v", err)
	}
}

// TestModelRouterValidator_ProxyModeStillRunsStaticChecks proves a Proxy-mode
// router is still subject to the static spec validation (which is data-plane
// independent): an undefined defaultRoute is rejected regardless of data plane.
func TestModelRouterValidator_ProxyModeStillRunsStaticChecks(t *testing.T) {
	v := &ModelRouterValidator{}
	ctx := context.Background()

	mr := validGatewayRouter()
	mr.Spec.DataPlane = inferencev1alpha1.ModelRouterDataPlaneProxy
	mr.Spec.GatewayRef = nil
	mr.Spec.DefaultRoute = "does-not-exist"

	_, err := v.ValidateCreate(ctx, mr)
	if err == nil {
		t.Fatalf("expected static validation to reject undefined defaultRoute in Proxy mode")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error, got %T: %v", err, err)
	}
}

// TestModelRouterValidator_ProxyModeRejectsUnimplementedClassifier: a Proxy
// router that asks for the detector or hybrid classifier is refused at apply
// time, the way the gateway planes refuse a dataClassification match under
// those modes, because the router-proxy has no classifier to run. An existing
// router that already sets the mode stays updatable: a status or metadata
// patch that leaves the spec alone is admitted, and so is the spec edit that
// moves it to header-only.
func TestModelRouterValidator_ProxyModeRejectsUnimplementedClassifier(t *testing.T) {
	v := &ModelRouterValidator{}
	ctx := context.Background()

	for _, mode := range []string{"detector", "hybrid"} {
		mr := validGatewayRouter()
		mr.Spec.DataPlane = inferencev1alpha1.ModelRouterDataPlaneProxy
		mr.Spec.GatewayRef = nil
		mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
			Classification: &inferencev1alpha1.ClassificationPolicy{Mode: mode},
		}

		_, err := v.ValidateCreate(ctx, mr)
		if err == nil {
			t.Fatalf("%s: expected the webhook to reject an unimplemented classification mode", mode)
		}
		if !apierrors.IsInvalid(err) {
			t.Fatalf("%s: expected an Invalid error, got %T: %v", mode, err, err)
		}
		if !strings.Contains(err.Error(), "spec.policy.classification.mode") {
			t.Errorf("%s: error does not name spec.policy.classification.mode: %v", mode, err)
		}

		statusPatched := mr.DeepCopy()
		statusPatched.Labels = map[string]string{"patched": "true"}
		statusPatched.Status.Phase = PhaseFailed
		if _, err := v.ValidateUpdate(ctx, mr.DeepCopy(), statusPatched); err != nil {
			t.Errorf("%s: a status-only update of an existing router was rejected: %v", mode, err)
		}

		fixed := mr.DeepCopy()
		fixed.Spec.Policy.Classification.Mode = classificationModeHeaderOnly
		if _, err := v.ValidateUpdate(ctx, mr.DeepCopy(), fixed); err != nil {
			t.Errorf("%s: the update to header-only was rejected: %v", mode, err)
		}
	}
}

// TestModelRouterValidator_Update reuses the create branches when the spec
// changes: a spec-changing update applies the same invariants.
func TestModelRouterValidator_Update(t *testing.T) {
	v := &ModelRouterValidator{}
	ctx := context.Background()

	// A spec-changing update into an invalid gateway-mode state is rejected.
	bad := validGatewayRouter()
	bad.Spec.Rules[0].Route.Strategy = "shadow"
	if _, err := v.ValidateUpdate(ctx, validGatewayRouter(), bad); err == nil {
		t.Fatalf("expected spec-changing update into an unsupported strategy to fail")
	}

	// A spec-changing update to a still-valid state passes.
	good := validGatewayRouter()
	good.Spec.Rules[0].Match.Models = []string{"qwen35-27b", "qwen35-27b-instruct"}
	if _, err := v.ValidateUpdate(ctx, validGatewayRouter(), good); err != nil {
		t.Fatalf("expected spec-changing update to a valid spec to pass, got %v", err)
	}
}

// TestModelRouterValidator_Update_Grandfathering proves that a status/metadata
// patch leaving the spec untouched is accepted even when the spec is already
// invalid, so a grandfathered bad ModelRouter cannot be wedged (the gateway
// reconciler patches status every reconcile). A patch that actually changes the
// invalid spec is still rejected.
func TestModelRouterValidator_Update_Grandfathering(t *testing.T) {
	v := &ModelRouterValidator{}
	ctx := context.Background()

	// A router that would fail create-time validation (unsupported strategy)
	// is presumed to predate the webhook.
	invalid := validGatewayRouter()
	invalid.Spec.Rules[0].Route.Strategy = "shadow"
	if _, err := v.ValidateCreate(ctx, invalid.DeepCopy()); err == nil {
		t.Fatalf("test setup: expected the invalid router to fail create validation")
	}

	// Status/metadata-only patch: spec unchanged -> accepted.
	statusPatched := invalid.DeepCopy()
	statusPatched.Labels = map[string]string{"patched": "true"}
	statusPatched.Status.Phase = "Degraded"
	if _, err := v.ValidateUpdate(ctx, invalid.DeepCopy(), statusPatched); err != nil {
		t.Fatalf("expected status-only update of a spec-invalid router to be accepted (grandfathering), got %v", err)
	}

	// Spec-changing patch into a still-invalid state -> rejected.
	specPatched := invalid.DeepCopy()
	specPatched.Spec.Rules[0].Match.TaskComplexity = "high"
	if _, err := v.ValidateUpdate(ctx, invalid.DeepCopy(), specPatched); err == nil {
		t.Fatalf("expected a spec-changing update of an invalid router to be re-validated and rejected")
	}
}

// TestModelRouterValidator_Delete proves delete is always allowed.
func TestModelRouterValidator_Delete(t *testing.T) {
	v := &ModelRouterValidator{}
	ctx := context.Background()

	// Even a thoroughly invalid router deletes cleanly.
	mr := validGatewayRouter()
	mr.Spec.Rules[0].Route.Strategy = "shadow"
	if _, err := v.ValidateDelete(ctx, mr); err != nil {
		t.Fatalf("expected ValidateDelete to always allow, got %v", err)
	}
}

// TestModelRouterValidator_AgentGatewayModeAcceptsPerBackendPickers: two
// InferenceServices with a picker each are admitted (#1991).
func TestModelRouterValidator_AgentGatewayModeAcceptsPerBackendPickers(t *testing.T) {
	v := &ModelRouterValidator{}
	mr := validAgentGatewayRouter()
	mr.Spec.AgentGatewayRef.EndpointPicker, mr.Spec.AgentGatewayRef.EndpointPickerPort = "", 0
	mr.Spec.Backends = []inferencev1alpha1.RouterBackend{
		pickerBackend("qwen-cuda", "qwen-cuda", "qwen-epp", 9002),
		pickerBackend("glm-cuda", "glm-cuda", "glm-epp", 9002),
	}
	mr.Spec.Rules[0].Route.Backends = []string{"qwen-cuda", "glm-cuda"}
	if _, err := v.ValidateCreate(context.Background(), mr); err != nil {
		t.Fatalf("per-backend pickers must be admitted: %v", err)
	}
}

// TestModelRouterValidator_AgentGatewayModeRejectsMissingPicker: a pool with no
// picker and no router default is rejected at apply time.
func TestModelRouterValidator_AgentGatewayModeRejectsMissingPicker(t *testing.T) {
	v := &ModelRouterValidator{}
	mr := validAgentGatewayRouter()
	mr.Spec.AgentGatewayRef.EndpointPicker, mr.Spec.AgentGatewayRef.EndpointPickerPort = "", 0
	_, err := v.ValidateCreate(context.Background(), mr)
	if err == nil || !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "has no Endpoint Picker") {
		t.Fatalf("want an Invalid error naming the missing picker, got %v", err)
	}
}

// affinityRouter returns a valid gateway router whose first backend carries the
// given balancing mode plus a sessionAffinity block.
func affinityRouter(mode string) *inferencev1alpha1.ModelRouter {
	mr := validGatewayRouter()
	mr.Spec.Backends[0].Balancing = &inferencev1alpha1.BackendBalancing{
		Mode:            mode,
		SessionAffinity: &inferencev1alpha1.SessionAffinity{Headers: []string{"x-session-id"}},
	}
	return mr
}

func TestModelRouterValidator_SessionAffinityNeedsLoadAware(t *testing.T) {
	v := &ModelRouterValidator{}
	ctx := context.Background()

	_, err := v.ValidateCreate(ctx, affinityRouter(inferencev1alpha1.BalancingModeRoundRobin))
	if err == nil {
		t.Fatalf("expected RoundRobin plus sessionAffinity to be rejected")
	}
	if !strings.Contains(err.Error(), "spec.backends[0].balancing.sessionAffinity") ||
		!strings.Contains(err.Error(), "sessionAffinity requires balancing.mode: LoadAware") {
		t.Fatalf("unexpected error: %v", err)
	}

	// An empty mode is not LoadAware either.
	if _, err := v.ValidateCreate(ctx, affinityRouter("")); err == nil {
		t.Fatalf("expected empty mode plus sessionAffinity to be rejected")
	}

	if _, err := v.ValidateCreate(ctx, affinityRouter(inferencev1alpha1.BalancingModeLoadAware)); err != nil {
		t.Fatalf("expected LoadAware plus sessionAffinity to be accepted, got %v", err)
	}
}

func TestModelRouterValidator_SessionAffinityUnchangedSpecGrandfathered(t *testing.T) {
	v := &ModelRouterValidator{}
	ctx := context.Background()

	old := affinityRouter(inferencev1alpha1.BalancingModeRoundRobin)

	unchanged := old.DeepCopy()
	unchanged.Labels = map[string]string{"patched": "true"}
	if _, err := v.ValidateUpdate(ctx, old.DeepCopy(), unchanged); err != nil {
		t.Fatalf("expected unchanged spec to be grandfathered, got %v", err)
	}

	changed := old.DeepCopy()
	changed.Spec.Backends[1].Name = "qwen-metal-2"
	changed.Spec.Rules[0].Route.Backends[1] = "qwen-metal-2"
	if _, err := v.ValidateUpdate(ctx, old.DeepCopy(), changed); err == nil {
		t.Fatalf("expected a spec-changing update to be re-validated and rejected")
	}
}

func TestModelRouterValidator_MaxLoadPercentWarns(t *testing.T) {
	v := &ModelRouterValidator{}
	mr := affinityRouter(inferencev1alpha1.BalancingModeLoadAware)
	mr.Spec.Backends[0].Balancing.SessionAffinity.MaxLoadPercent = 125

	warnings, err := v.ValidateCreate(context.Background(), mr)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	want := "spec.backends[0].balancing.sessionAffinity.maxLoadPercent is deprecated and has no effect; " +
		"if you never set it, it was defaulted by an older LLMKube and can be removed with a JSON patch"
	if len(warnings) != 1 || warnings[0] != want {
		t.Fatalf("expected exactly one deprecation warning %q, got %v", want, warnings)
	}
}
