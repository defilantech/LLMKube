/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

const selectorLabelKey = "llmkube.dev/router"

func routerSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{selectorLabelKey: "models"}}
}

func selectorLabels() map[string]string {
	return map[string]string{selectorLabelKey: "models"}
}

// createSelectedInferenceService creates a minimal InferenceService with the
// given labels, in ns.
func createSelectedInferenceService(
	t *testing.T,
	c client.Client,
	ns, name string,
	labels map[string]string,
	mutate ...func(*inferencev1alpha1.InferenceService),
) {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: name,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 8080},
		},
	}
	for _, m := range mutate {
		m(isvc)
	}
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create InferenceService %s/%s: %v", ns, name, err)
	}
}

// createTypedSelectorRouter creates a ModelRouter and returns it as the API
// server stored it, so the tests see the same field defaults a user's object
// gets.
func createTypedSelectorRouter(
	t *testing.T,
	c client.Client,
	ns, name string,
	selector *metav1.LabelSelector,
	backends []inferencev1alpha1.RouterBackend,
	rules []inferencev1alpha1.RouterRule,
) *inferencev1alpha1.ModelRouter {
	t.Helper()
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane:            inferencev1alpha1.ModelRouterDataPlaneProxy,
			BackendSelector:      selector,
			Backends:             backends,
			Rules:                rules,
			DefaultRouteStrategy: inferencev1alpha1.DefaultRouteStrategyBackendNameMatch,
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create ModelRouter %s/%s: %v", ns, name, err)
	}
	return fetchTypedRouter(t, c, ns, name)
}

func fetchTypedRouter(t *testing.T, c client.Client, ns, name string) *inferencev1alpha1.ModelRouter {
	t.Helper()
	mr := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, mr); err != nil {
		t.Fatalf("get ModelRouter %s/%s: %v", ns, name, err)
	}
	return mr
}

func resolveForTest(
	t *testing.T,
	c client.Client,
	mr *inferencev1alpha1.ModelRouter,
) ([]inferencev1alpha1.RouterBackend, []ModelRouterValidationError) {
	t.Helper()
	resolved, _, errs, err := selectorReconciler(c).resolveRouterBackends(context.Background(), mr)
	if err != nil {
		t.Fatalf("resolveRouterBackends: %v", err)
	}
	return resolved, errs
}

func resolvedNames(resolved []inferencev1alpha1.RouterBackend) []string {
	names := make([]string, len(resolved))
	for i, b := range resolved {
		names[i] = b.Name
	}
	return names
}

// TestResolveRouterBackendsOverlay pins the merge contract: identity comes
// from the selection and every field the explicit entry sets is overlaid, with
// explicit entries in declaration order followed by selector-only services
// sorted by name.
func TestResolveRouterBackendsOverlay(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	createSelectedInferenceService(t, c, ns, "glm-5-3-flash", selectorLabels())
	createSelectedInferenceService(t, c, ns, "qwen2-5-0-5b", selectorLabels())

	timeout := metav1.Duration{Duration: 600 * time.Second}
	weight := int32(7)
	contextLength := int64(131072)
	mr := createTypedSelectorRouter(t, c, ns, "models", routerSelector(),
		[]inferencev1alpha1.RouterBackend{{
			Name:                 "glm-5-3-flash",
			DisplayName:          "glm-flash",
			Timeout:              &timeout,
			Capabilities:         []string{"vision"},
			Weight:               &weight,
			CostPerMillionTokens: &inferencev1alpha1.TokenCost{PromptUSD: "0.5", CompletionUSD: "1.5"},
			ModelInfo:            &inferencev1alpha1.RouterBackendModelInfo{ContextLength: &contextLength},
			HealthCheck:          &inferencev1alpha1.BackendHealthCheck{Path: "/readyz", IntervalSeconds: 5, TimeoutSeconds: 3},
		}}, nil)

	resolved, errs := resolveForTest(t, c, mr)
	if len(errs) > 0 {
		t.Fatalf("resolution errors: %v", errs)
	}
	if got := resolvedNames(resolved); !slices.Equal(got, []string{"glm-5-3-flash", "qwen2-5-0-5b"}) {
		t.Fatalf("resolved order = %v, want the explicit entry then the selector-only service", got)
	}

	glm := resolved[0]
	if glm.InferenceServiceRef == nil || glm.InferenceServiceRef.Name != "glm-5-3-flash" {
		t.Errorf("overlay identity lost its inferenceServiceRef: %+v", glm.InferenceServiceRef)
	}
	if glm.Tier != backendTierLocal {
		t.Errorf("overlay tier = %q, want local from the selection", glm.Tier)
	}
	if glm.Resolution != inferencev1alpha1.RouterBackendResolutionService {
		t.Errorf("overlay resolution = %q, want service from the selection", glm.Resolution)
	}
	if glm.Timeout == nil || glm.Timeout.Duration != 600*time.Second {
		t.Errorf("overlay timeout = %v, want the entry's 600s", glm.Timeout)
	}
	if glm.DisplayName != "glm-flash" {
		t.Errorf("overlay displayName = %q, want glm-flash", glm.DisplayName)
	}
	if !slices.Equal(glm.Capabilities, []string{"vision"}) {
		t.Errorf("overlay capabilities = %v, want the entry's [vision]", glm.Capabilities)
	}
	if glm.Weight == nil || *glm.Weight != 7 {
		t.Errorf("overlay weight = %v, want the entry's 7", glm.Weight)
	}
	if glm.CostPerMillionTokens == nil || glm.CostPerMillionTokens.PromptUSD != "0.5" ||
		glm.CostPerMillionTokens.CompletionUSD != "1.5" {
		t.Errorf("overlay costPerMillionTokens = %+v, want the entry's 0.5/1.5", glm.CostPerMillionTokens)
	}
	if glm.ModelInfo == nil || glm.ModelInfo.ContextLength == nil || *glm.ModelInfo.ContextLength != 131072 {
		t.Errorf("overlay modelInfo = %+v, want the entry's contextLength 131072", glm.ModelInfo)
	}
	if glm.HealthCheck == nil || glm.HealthCheck.Path != "/readyz" || glm.HealthCheck.IntervalSeconds != 5 {
		t.Errorf("overlay healthCheck = %+v, want the entry's /readyz at 5s", glm.HealthCheck)
	}

	qwen := resolved[1]
	if qwen.InferenceServiceRef == nil || qwen.InferenceServiceRef.Name != "qwen2-5-0-5b" {
		t.Errorf("selector-only backend identity = %+v, want inferenceServiceRef qwen2-5-0-5b", qwen.InferenceServiceRef)
	}
	if qwen.Tier != backendTierLocal || qwen.Resolution != inferencev1alpha1.RouterBackendResolutionService {
		t.Errorf("selector-only backend tier/resolution = %q/%q, want local/service", qwen.Tier, qwen.Resolution)
	}
}

// TestResolveRouterBackendsSuppression pins that an explicit entry keeps
// today's meaning and claims its name, so the selector does not add a second
// backend of that name (which Config.Validate refuses).
func TestResolveRouterBackendsSuppression(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	createSelectedInferenceService(t, c, ns, "glm-5-3-flash", selectorLabels())
	createSelectedInferenceService(t, c, ns, "qwen2-5-0-5b", selectorLabels())
	createSelectedInferenceService(t, c, ns, "other-isvc", nil)

	mr := createTypedSelectorRouter(t, c, ns, "models", routerSelector(),
		[]inferencev1alpha1.RouterBackend{
			{
				Name: "glm-5-3-flash",
				Tier: backendTierCloud,
				External: &inferencev1alpha1.ExternalProvider{
					Provider: "anthropic",
					Model:    "claude-opus-4-7",
					URL:      "https://api.anthropic.com",
				},
			},
			{
				Name:                "qwen2-5-0-5b",
				InferenceServiceRef: &corev1.LocalObjectReference{Name: "other-isvc"},
			},
		}, nil)

	compiled, err := selectorReconciler(c).compileRouterConfig(context.Background(), mr)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(compiled.JSON, &cfg); err != nil {
		t.Fatalf("decode compiled config: %v", err)
	}
	assertBackendNames(t, cfg, "glm-5-3-flash", "qwen2-5-0-5b")

	glm := configBackend(t, cfg, "glm-5-3-flash")
	if glm["tier"] != "cloud" || glm["provider"] != "anthropic" {
		t.Errorf("explicit entry lost its meaning: %v", glm)
	}
	qwen := configBackend(t, cfg, "qwen2-5-0-5b")
	if qwen["inferenceService"] != "other-isvc" {
		t.Errorf("explicit entry's inferenceService = %v, want other-isvc", qwen["inferenceService"])
	}
}

// TestResolveRouterBackendsUnmatchedRefLess pins that a ref-less entry the
// selector does not match is a Validated failure naming the entry, not a
// compile failure about a missing address.
func TestResolveRouterBackendsUnmatchedRefLess(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	createSelectedInferenceService(t, c, ns, "glm-5-3-flash", selectorLabels())
	timeout := metav1.Duration{Duration: 600 * time.Second}
	mr := createTypedSelectorRouter(t, c, ns, "models", routerSelector(),
		[]inferencev1alpha1.RouterBackend{{Name: "nope", Timeout: &timeout}}, nil)

	_, errs := resolveForTest(t, c, mr)
	if len(errs) != 1 {
		t.Fatalf("resolution errors = %v, want exactly one", errs)
	}
	if errs[0].Field != "spec.backends[0].name" {
		t.Errorf("error field = %q, want spec.backends[0].name", errs[0].Field)
	}
	if !strings.Contains(errs[0].Message, "matches no InferenceService selected by spec.backendSelector") {
		t.Errorf("error message = %q, want it to name the selector", errs[0].Message)
	}

	reconcileSelectorRouter(t, selectorReconciler(c), ns, "models")
	stored := fetchTypedRouter(t, c, ns, "models")
	validated := apimeta.FindStatusCondition(stored.Status.Conditions, ModelRouterConditionValidated)
	if validated == nil || validated.Status != metav1.ConditionFalse {
		t.Fatalf("Validated = %+v, want False", validated)
	}
	if validated.Reason != ModelRouterReasonBackendResolutionFailed {
		t.Errorf("Validated reason = %q, want %s: cluster state, not the spec, failed",
			validated.Reason, ModelRouterReasonBackendResolutionFailed)
	}
	if !strings.Contains(validated.Message, "spec.backends[0].name") {
		t.Errorf("Validated message = %q, want the failing entry's path", validated.Message)
	}
	available := apimeta.FindStatusCondition(stored.Status.Conditions, ConditionAvailable)
	if available == nil || available.Status != metav1.ConditionFalse || available.Reason != ModelRouterReasonCompileFailed {
		t.Errorf("Available = %+v, want False/%s", available, ModelRouterReasonCompileFailed)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: routerProxyResourceName("models"), Namespace: ns,
	}, cm); !apierrors.IsNotFound(err) {
		t.Errorf("ConfigMap get err = %v, want NotFound (a resolution failure provisions nothing)", err)
	}
}

// TestResolveRouterBackendsSelectorSelectsNothing covers the empty-selector
// semantics and the empty-resolution failure.
func TestResolveRouterBackendsSelectorSelectsNothing(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	createSelectedInferenceService(t, c, ns, "alpha", nil)
	createSelectedInferenceService(t, c, ns, "beta", nil)
	otherNS := newTestNamespace(t, c)
	createSelectedInferenceService(t, c, otherNS, "gamma", nil)

	t.Run("empty selector selects every service in the namespace", func(t *testing.T) {
		mr := createTypedSelectorRouter(t, c, ns, "all", &metav1.LabelSelector{}, nil, nil)
		resolved, errs := resolveForTest(t, c, mr)
		if len(errs) > 0 {
			t.Fatalf("resolution errors: %v", errs)
		}
		if got := resolvedNames(resolved); !slices.Equal(got, []string{"alpha", "beta"}) {
			t.Errorf("resolved = %v, want every service in %s and none from %s", got, ns, otherNS)
		}
	})

	t.Run("a selector matching nothing is a resolution failure", func(t *testing.T) {
		mr := createTypedSelectorRouter(t, c, ns, "none",
			&metav1.LabelSelector{MatchLabels: map[string]string{selectorLabelKey: "absent"}}, nil, nil)
		_, errs := resolveForTest(t, c, mr)
		if len(errs) != 1 || errs[0].Field != "spec.backendSelector" {
			t.Fatalf("resolution errors = %v, want one at spec.backendSelector", errs)
		}
		reconcileSelectorRouter(t, selectorReconciler(c), ns, "none")
		stored := fetchTypedRouter(t, c, ns, "none")
		validated := apimeta.FindStatusCondition(stored.Status.Conditions, ModelRouterConditionValidated)
		if validated == nil || validated.Status != metav1.ConditionFalse ||
			!strings.Contains(validated.Message, "selects no InferenceService") {
			t.Errorf("Validated = %+v, want False naming the empty selector", validated)
		}
	})
}

// TestValidateModelRouterSelector covers the static checks whose result
// depends on spec.backendSelector, and the no-selector row that must keep
// base's message.
func TestValidateModelRouterSelector(t *testing.T) {
	local := func(name string) *corev1.LocalObjectReference {
		return &corev1.LocalObjectReference{Name: name}
	}
	timeout := &metav1.Duration{Duration: 600 * time.Second}
	selected := routerSelector()

	for _, tc := range []struct {
		name      string
		mr        *inferencev1alpha1.ModelRouter
		wantField string
		wantMsg   string
	}{
		{
			name: "ref-less entry without a selector keeps base's message",
			mr: &inferencev1alpha1.ModelRouter{Spec: inferencev1alpha1.ModelRouterSpec{
				Backends: []inferencev1alpha1.RouterBackend{{Name: "x", Timeout: timeout}},
			}},
			wantField: "spec.backends[0]",
			wantMsg:   "exactly one of inferenceServiceRef or external must be set",
		},
		{
			name: "a selector with an unknown operator is refused",
			mr: &inferencev1alpha1.ModelRouter{Spec: inferencev1alpha1.ModelRouterSpec{
				BackendSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "llmkube.dev/router",
					Operator: "Equals",
					Values:   []string{"models"},
				}}},
				Backends: []inferencev1alpha1.RouterBackend{{Name: "b", InferenceServiceRef: local("b")}},
			}},
			wantField: "spec.backendSelector",
			wantMsg:   "not a valid label selector operator",
		},
		{
			name: "a selector with an In expression and no values is refused",
			mr: &inferencev1alpha1.ModelRouter{Spec: inferencev1alpha1.ModelRouterSpec{
				BackendSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "llmkube.dev/router",
					Operator: metav1.LabelSelectorOpIn,
				}}},
				Backends: []inferencev1alpha1.RouterBackend{{Name: "b", InferenceServiceRef: local("b")}},
			}},
			wantField: "spec.backendSelector",
			wantMsg:   "values set can't be empty",
		},
		{
			name: "failClosed naming a selected-only backend is refused",
			mr: &inferencev1alpha1.ModelRouter{Spec: inferencev1alpha1.ModelRouterSpec{
				BackendSelector: selected,
				Backends:        []inferencev1alpha1.RouterBackend{{Name: "b", InferenceServiceRef: local("b")}},
				Rules: []inferencev1alpha1.RouterRule{{
					Name:       "pii",
					FailClosed: true,
					Match:      &inferencev1alpha1.RuleMatch{DataClassification: []string{"pii"}},
					Route:      inferencev1alpha1.RuleRoute{Backends: []string{"selected-only"}},
				}},
			}},
			wantField: "spec.rules[0].route.backends[0]",
			wantMsg:   `"selected-only" comes from spec.backendSelector`,
		},
		{
			name: "failClosed naming an overlay entry is refused",
			mr: &inferencev1alpha1.ModelRouter{Spec: inferencev1alpha1.ModelRouterSpec{
				BackendSelector: selected,
				Backends:        []inferencev1alpha1.RouterBackend{{Name: "overlay", Timeout: timeout}},
				Rules: []inferencev1alpha1.RouterRule{{
					Name:       "pii",
					FailClosed: true,
					Route:      inferencev1alpha1.RuleRoute{Backends: []string{"overlay"}},
				}},
			}},
			wantField: "spec.rules[0].route.backends[0]",
			wantMsg:   `"overlay" comes from spec.backendSelector`,
		},
		{
			name: "failClosed naming an explicit backend is accepted",
			mr: &inferencev1alpha1.ModelRouter{Spec: inferencev1alpha1.ModelRouterSpec{
				BackendSelector: selected,
				Backends:        []inferencev1alpha1.RouterBackend{{Name: "b", InferenceServiceRef: local("b")}},
				Rules: []inferencev1alpha1.RouterRule{{
					Name:       "pii",
					FailClosed: true,
					Route:      inferencev1alpha1.RuleRoute{Backends: []string{"b"}},
				}},
			}},
		},
		{
			name: "a maxUSD budget is refused with a selector",
			mr: &inferencev1alpha1.ModelRouter{Spec: inferencev1alpha1.ModelRouterSpec{
				BackendSelector: selected,
				Backends:        []inferencev1alpha1.RouterBackend{{Name: "b", InferenceServiceRef: local("b")}},
				Policy: &inferencev1alpha1.RouterPolicy{Budgets: []inferencev1alpha1.BudgetSpec{{
					Name: "cap", Scope: "router", MaxUSD: "10",
				}}},
			}},
			wantField: "spec.policy.budgets[0].maxUSD",
			wantMsg:   "cannot be combined with spec.backendSelector",
		},
		{
			name: "a weighted rule over a selected backend is not refused statically",
			mr: &inferencev1alpha1.ModelRouter{Spec: inferencev1alpha1.ModelRouterSpec{
				BackendSelector: selected,
				Rules: []inferencev1alpha1.RouterRule{{
					Name:  "spread",
					Route: inferencev1alpha1.RuleRoute{Backends: []string{"selected-only"}, Strategy: "weighted"},
				}},
			}},
		},
		{
			name: "tier cloud on an overlay entry is refused",
			mr: &inferencev1alpha1.ModelRouter{Spec: inferencev1alpha1.ModelRouterSpec{
				BackendSelector: selected,
				Backends:        []inferencev1alpha1.RouterBackend{{Name: "b", Tier: backendTierCloud, Timeout: timeout}},
			}},
			wantField: "spec.backends[0].tier",
			wantMsg:   "overrides a backend selected by spec.backendSelector",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateModelRouter(tc.mr)
			if tc.wantField == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected validation errors: %v", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("no validation error, want one at %s", tc.wantField)
			}
			found := false
			for _, e := range errs {
				if e.Field == tc.wantField && strings.Contains(e.Message, tc.wantMsg) {
					found = true
				}
			}
			if !found {
				t.Fatalf("validation errors = %v, want %s: %q", errs, tc.wantField, tc.wantMsg)
			}
		})
	}
}

// TestModelRouterSelectorRefusedOnGateway pins the webhook and the reconciler
// refusing spec.backendSelector on the Gateway data plane with the same
// message.
func TestModelRouterSelectorRefusedOnGateway(t *testing.T) {
	c, cfg, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-selector", Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane:       inferencev1alpha1.ModelRouterDataPlaneGateway,
			GatewayRef:      &inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
			BackendSelector: routerSelector(),
		},
	}
	err := (&ModelRouterValidator{}).validate(mr)
	if err == nil || !strings.Contains(err.Error(), "backendSelector") {
		t.Fatalf("webhook error = %v, want a backendSelector refusal", err)
	}

	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create ModelRouter: %v", err)
	}
	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	stored := fetchTypedRouter(t, c, ns, "gw-selector")
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterGatewayReasonUnsupported {
		t.Fatalf("GatewayReady = %+v, want False/%s", cond, modelRouterGatewayReasonUnsupported)
	}
	if !strings.Contains(cond.Message, "backendSelector") {
		t.Errorf("GatewayReady message = %q, want it to name backendSelector", cond.Message)
	}
}

// TestModelRouterSelectorRefusedOnAgentGateway is the AgentGateway sibling.
func TestModelRouterSelectorRefusedOnAgentGateway(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, false)
	defer stop()
	ns := newTestNamespace(t, c)

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "agw-selector", Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane:       inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
			BackendSelector: routerSelector(),
		},
	}
	err := (&ModelRouterValidator{}).validate(mr)
	if err == nil || !strings.Contains(err.Error(), "backendSelector") {
		t.Fatalf("webhook error = %v, want a backendSelector refusal", err)
	}

	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create ModelRouter: %v", err)
	}
	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)

	stored := fetchTypedRouter(t, c, ns, "agw-selector")
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonUnsupported {
		t.Fatalf("AgentGatewayReady = %+v, want False/%s", cond, modelRouterAgentGatewayReasonUnsupported)
	}
	if !strings.Contains(cond.Message, "backendSelector") {
		t.Errorf("AgentGatewayReady message = %q, want it to name backendSelector", cond.Message)
	}
}

// TestModelRouterSelectorPooledMemberStaysVisible pins the ModelPool
// exemption: a selected service the pool holds at replicas 0 stays in the
// config, not hidden, and the activation RBAC is provisioned from the
// resolved list.
func TestModelRouterSelectorPooledMemberStaysVisible(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	createSelectedInferenceService(t, c, ns, "pooled-a", selectorLabels(),
		func(isvc *inferencev1alpha1.InferenceService) {
			zero := int32(0)
			isvc.Spec.Replicas = &zero
		})
	pool := &inferencev1alpha1.ModelPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-1", Namespace: ns},
		Spec: inferencev1alpha1.ModelPoolSpec{
			SwapPolicy: inferencev1alpha1.ModelPoolSwapPolicySticky,
			SwapBudget: &metav1.Duration{Duration: 6 * time.Minute},
			Members: []inferencev1alpha1.ModelPoolMember{
				{InferenceServiceRef: corev1.LocalObjectReference{Name: "pooled-a"}},
			},
		},
	}
	if err := c.Create(context.Background(), pool); err != nil {
		t.Fatalf("create ModelPool: %v", err)
	}

	createTypedSelectorRouter(t, c, ns, "models", routerSelector(), nil, nil)
	r := selectorReconciler(c)
	reconcileSelectorRouter(t, r, ns, "models")

	cfg := routerConfigJSON(t, c, ns, "models")
	b := configBackend(t, cfg, "pooled-a")
	if _, hidden := b["hidden"]; hidden {
		t.Errorf("pooled backend carries hidden=true: it must stay on /v1/models so a client can activate it")
	}
	poolWire, ok := b["pool"].(map[string]any)
	if !ok || poolWire["name"] != "pool-1" {
		t.Errorf("pooled backend pool = %v, want pool-1", b["pool"])
	}

	name := routerProxyResourceName("models")
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, &rbacv1.Role{}); err != nil {
		t.Errorf("activation Role missing for a selected pooled member: %v", err)
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, dep); err != nil {
		t.Fatalf("router-proxy Deployment: %v", err)
	}
	if dep.Spec.Template.Spec.ServiceAccountName != name {
		t.Errorf("deployment serviceAccountName = %q, want %q from the resolved pool", dep.Spec.Template.Spec.ServiceAccountName, name)
	}
}

// TestModelRouterSelectorHidesOnlySelectorSourcedBackends pins that the hidden
// flag follows the backend's source: a backend the selector produced drops out
// of /v1/models when its service is stopped, while an explicit entry keeps the
// entry it has today.
func TestModelRouterSelectorHidesOnlySelectorSourcedBackends(t *testing.T) {
	stopped := func(isvc *inferencev1alpha1.InferenceService) {
		zero := int32(0)
		isvc.Spec.Replicas = &zero
	}

	t.Run("an explicit entry naming a selected stopped service stays listed", func(t *testing.T) {
		c, _, stop := startBudgetTestEnv(t)
		defer stop()
		ns := newTestNamespace(t, c)
		createSelectedInferenceService(t, c, ns, "glm-5-3-flash", selectorLabels(), stopped)
		createTypedSelectorRouter(t, c, ns, "models", routerSelector(),
			[]inferencev1alpha1.RouterBackend{{
				Name:                "glm-5-3-flash",
				InferenceServiceRef: &corev1.LocalObjectReference{Name: "glm-5-3-flash"},
			}}, nil)
		reconcileSelectorRouter(t, selectorReconciler(c), ns, "models")

		cfg := routerConfigJSON(t, c, ns, "models")
		if hidden, present := configBackend(t, cfg, "glm-5-3-flash")["hidden"]; present {
			t.Errorf("explicit backend carries hidden=%v: an explicit entry keeps today's /v1/models entry", hidden)
		}
		if got := modelIDsFromProxy(t, cfg); !slices.Equal(got, []string{"glm-5-3-flash"}) {
			t.Errorf("GET /v1/models = %v, want the explicit entry listed", got)
		}
	})

	t.Run("an overlay of a stopped selected service is hidden", func(t *testing.T) {
		c, _, stop := startBudgetTestEnv(t)
		defer stop()
		ns := newTestNamespace(t, c)
		createSelectedInferenceService(t, c, ns, "glm-5-3-flash", selectorLabels(), stopped)
		timeout := metav1.Duration{Duration: 600 * time.Second}
		createTypedSelectorRouter(t, c, ns, "models", routerSelector(),
			[]inferencev1alpha1.RouterBackend{{Name: "glm-5-3-flash", Timeout: &timeout}}, nil)
		reconcileSelectorRouter(t, selectorReconciler(c), ns, "models")

		cfg := routerConfigJSON(t, c, ns, "models")
		if hidden := configBackend(t, cfg, "glm-5-3-flash")["hidden"]; hidden != true {
			t.Errorf("overlay of a stopped selected service carries hidden=%v, want true", hidden)
		}
		if got := modelIDsFromProxy(t, cfg); len(got) != 0 {
			t.Errorf("GET /v1/models = %v, want the stopped selected service left out", got)
		}
	})

	t.Run("an explicit entry to an unselected stopped service stays listed", func(t *testing.T) {
		c, _, stop := startBudgetTestEnv(t)
		defer stop()
		ns := newTestNamespace(t, c)
		createSelectedInferenceService(t, c, ns, "runner", selectorLabels())
		createSelectedInferenceService(t, c, ns, "other-isvc", nil, stopped)
		createTypedSelectorRouter(t, c, ns, "models", routerSelector(),
			[]inferencev1alpha1.RouterBackend{{
				Name:                "renamed-glm",
				InferenceServiceRef: &corev1.LocalObjectReference{Name: "other-isvc"},
			}}, nil)
		reconcileSelectorRouter(t, selectorReconciler(c), ns, "models")

		cfg := routerConfigJSON(t, c, ns, "models")
		if hidden, present := configBackend(t, cfg, "renamed-glm")["hidden"]; present {
			t.Errorf("explicit backend to an unselected stopped service carries hidden=%v, want none", hidden)
		}
		if got := modelIDsFromProxy(t, cfg); !slices.Equal(got, []string{"renamed-glm", "runner"}) {
			t.Errorf("GET /v1/models = %v, want the explicit entry and the running selected service", got)
		}
	})
}

// TestModelRouterSelectorEndpointOverlayGetsActivationRBAC pins that the
// deployment ServiceAccount and the endpointslices Role are provisioned from
// the resolved list: a ref-less overlay with resolution: endpoint needs both,
// and only the merge knows the InferenceService its name resolves to.
func TestModelRouterSelectorEndpointOverlayGetsActivationRBAC(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	createSelectedInferenceService(t, c, ns, "glm-5-3-flash", selectorLabels())
	createTypedSelectorRouter(t, c, ns, "models", routerSelector(),
		[]inferencev1alpha1.RouterBackend{{
			Name:       "glm-5-3-flash",
			Resolution: inferencev1alpha1.RouterBackendResolutionEndpoint,
		}}, nil)
	reconcileSelectorRouter(t, selectorReconciler(c), ns, "models")

	name := routerProxyResourceName("models")
	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, dep); err != nil {
		t.Fatalf("router-proxy Deployment: %v", err)
	}
	if dep.Spec.Template.Spec.ServiceAccountName != name {
		t.Errorf("deployment serviceAccountName = %q, want %q from the endpoint overlay", dep.Spec.Template.Spec.ServiceAccountName, name)
	}
	role := &rbacv1.Role{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, role); err != nil {
		t.Fatalf("activation Role: %v", err)
	}
	endpointSlices := false
	for _, rule := range role.Rules {
		if slices.Contains(rule.APIGroups, "discovery.k8s.io") && slices.Contains(rule.Resources, "endpointslices") {
			endpointSlices = true
		}
	}
	if !endpointSlices {
		t.Errorf("activation Role rules = %+v, want endpointslices access for the endpoint overlay", role.Rules)
	}
}

// TestModelRouterSelectorKeyedSelectedService pins that the keyed-backend
// check runs over the resolved list, so a selected keyed service fails loud.
func TestModelRouterSelectorKeyedSelectedService(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	createSelectedInferenceService(t, c, ns, "keyed-a", selectorLabels(),
		func(isvc *inferencev1alpha1.InferenceService) {
			isvc.Spec.Runtime = RuntimeVLLM
			isvc.Spec.Endpoint.APIKeySecretRef = &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "keyed-a-key"},
				Key:                  "api-key",
			}
		})
	createTypedSelectorRouter(t, c, ns, "models", routerSelector(), nil, nil)

	reconcileSelectorRouter(t, selectorReconciler(c), ns, "models")
	stored := fetchTypedRouter(t, c, ns, "models")
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, ModelRouterConditionBackendsReady)
	if cond == nil || cond.Reason != ModelRouterReasonBackendAPIKeyUnsupported {
		t.Fatalf("BackendsReady = %+v, want reason %s", cond, ModelRouterReasonBackendAPIKeyUnsupported)
	}
	if len(stored.Status.Backends) != 1 || stored.Status.Backends[0].Healthy {
		t.Errorf("status.backends = %+v, want the keyed service reported unhealthy", stored.Status.Backends)
	}
}

// TestModelRouterSelectorRuleNamesVanishedBackendFailsCompile pins the
// cluster-state failure: a rule naming a backend the selector no longer
// selects stops the compile and the previous config keeps serving.
func TestModelRouterSelectorRuleNamesVanishedBackendFailsCompile(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	createSelectedInferenceService(t, c, ns, "glm-5-3-flash", selectorLabels())
	createSelectedInferenceService(t, c, ns, "qwen2-5-0-5b", selectorLabels())
	createTypedSelectorRouter(t, c, ns, "models", routerSelector(), nil,
		[]inferencev1alpha1.RouterRule{{
			Name:  "all",
			Route: inferencev1alpha1.RuleRoute{Backends: []string{"glm-5-3-flash"}},
		}})
	r := selectorReconciler(c)
	reconcileSelectorRouter(t, r, ns, "models")
	before := routerConfigJSON(t, c, ns, "models")
	assertBackendNames(t, before, "glm-5-3-flash", "qwen2-5-0-5b")

	// Relabel the rule's backend away: the rule now names a backend nothing
	// produces.
	isvc := &inferencev1alpha1.InferenceService{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "glm-5-3-flash", Namespace: ns}, isvc); err != nil {
		t.Fatalf("get glm-5-3-flash: %v", err)
	}
	isvc.Labels = nil
	if err := c.Update(context.Background(), isvc); err != nil {
		t.Fatalf("relabel glm-5-3-flash: %v", err)
	}

	reconcileSelectorRouter(t, r, ns, "models")
	stored := fetchTypedRouter(t, c, ns, "models")
	available := apimeta.FindStatusCondition(stored.Status.Conditions, ConditionAvailable)
	if available == nil || available.Status != metav1.ConditionFalse || available.Reason != ModelRouterReasonCompileFailed {
		t.Fatalf("Available = %+v, want False/%s", available, ModelRouterReasonCompileFailed)
	}
	for _, want := range []string{"glm-5-3-flash", "all", "backendSelector"} {
		if !strings.Contains(available.Message, want) {
			t.Errorf("Available message = %q, want it to name %q", available.Message, want)
		}
	}
	assertBackendNames(t, routerConfigJSON(t, c, ns, "models"), "glm-5-3-flash", "qwen2-5-0-5b")
}

// TestModelRouterSelectorDottedNameVerbatim pins D7: a selected service name
// with dots is published verbatim and resolved by BackendNameMatch.
func TestModelRouterSelectorDottedNameVerbatim(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	createSelectedInferenceService(t, c, ns, "qwen2.5-0.5b", selectorLabels())
	createTypedSelectorRouter(t, c, ns, "models", routerSelector(), nil, nil)

	reconcileSelectorRouter(t, selectorReconciler(c), ns, "models")
	cfg := routerConfigJSON(t, c, ns, "models")
	b := configBackend(t, cfg, "qwen2.5-0.5b")
	if b["inferenceService"] != "qwen2.5-0.5b" {
		t.Errorf("dotted backend inferenceService = %v, want the service name verbatim", b["inferenceService"])
	}
	if got := modelIDsFromProxy(t, cfg); !slices.Equal(got, []string{"qwen2.5-0.5b"}) {
		t.Fatalf("GET /v1/models = %v, want the dotted id verbatim", got)
	}

	// BackendNameMatch must resolve the published id to that backend.
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("re-encode config: %v", err)
	}
	var rc router.Config
	if err := json.Unmarshal(raw, &rc); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	rc.Backends[0].Address = upstream.URL
	rc.DefaultRouteStrategy = router.DefaultRouteStrategyBackendNameMatch
	proxy := router.NewProxy(&rc, slog.Default())
	mux := http.NewServeMux()
	proxy.Mount(mux)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen2.5-0.5b","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || upstreamHits.Load() != 1 {
		t.Errorf("BackendNameMatch to %q = %d with %d upstream hits, want 200 and 1", "qwen2.5-0.5b", rec.Code, upstreamHits.Load())
	}
}

// TestModelRouterSelectorEndpointOverlayEnqueued pins that a ref-less overlay
// entry declaring endpoint resolution re-renders the router on EndpointSlice
// changes: the mapper cannot see the selector's services, so it must count the
// entry's stated resolution.
func TestModelRouterSelectorEndpointOverlayEnqueued(t *testing.T) {
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "models", Namespace: "ns"},
		Spec: inferencev1alpha1.ModelRouterSpec{
			BackendSelector: routerSelector(),
			Backends: []inferencev1alpha1.RouterBackend{{
				Name:       "glm-5-3-flash",
				Resolution: inferencev1alpha1.RouterBackendResolutionEndpoint,
			}},
		},
	}
	if !routerHasEndpointBackend(mr) {
		t.Fatal("a ref-less overlay with resolution: endpoint was not counted as an endpoint backend")
	}
	plain := mr.DeepCopy()
	plain.Spec.Backends[0].Resolution = inferencev1alpha1.RouterBackendResolutionService
	if routerHasEndpointBackend(plain) {
		t.Error("a ref-less overlay with resolution: service was counted as an endpoint backend")
	}
	refLessNoSelector := &inferencev1alpha1.ModelRouter{
		Spec: inferencev1alpha1.ModelRouterSpec{
			Backends: []inferencev1alpha1.RouterBackend{{
				Name:       "glm-5-3-flash",
				Resolution: inferencev1alpha1.RouterBackendResolutionEndpoint,
			}},
		},
	}
	if routerHasEndpointBackend(refLessNoSelector) {
		t.Error("a ref-less entry on a router without a selector was counted: base matches no backend")
	}

	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)
	mr.Namespace = ns
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create ModelRouter: %v", err)
	}
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "slices", Namespace: ns}}
	requests := selectorReconciler(c).findModelRoutersForEndpointSlice(context.Background(), slice)
	found := false
	for _, req := range requests {
		if req.Name == mr.Name && req.Namespace == ns {
			found = true
		}
	}
	if !found {
		t.Errorf("an EndpointSlice event did not enqueue the endpoint-overlay router: got %v", requests)
	}
}

// TestModelRouterSelectorModelPoolEventEnqueuesRouter pins the ModelPool
// fan-out: a pool edit re-renders every selector router in the namespace even
// when the pool shares no member with it, because the selector can pick a pool
// member and its compiled BackendPool changes with the pool.
func TestModelRouterSelectorModelPoolEventEnqueuesRouter(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	createTypedSelectorRouter(t, c, ns, "models", routerSelector(), nil, nil)
	createTypedSelectorRouter(t, c, ns, "explicit", nil,
		[]inferencev1alpha1.RouterBackend{{
			Name:                "explicit-a",
			InferenceServiceRef: &corev1.LocalObjectReference{Name: "explicit-a"},
		}}, nil)

	pool := &inferencev1alpha1.ModelPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-1", Namespace: ns},
		Spec: inferencev1alpha1.ModelPoolSpec{
			SwapPolicy: inferencev1alpha1.ModelPoolSwapPolicySticky,
			Members: []inferencev1alpha1.ModelPoolMember{{
				InferenceServiceRef: corev1.LocalObjectReference{Name: "someone-else"},
			}},
		},
	}
	requests := selectorReconciler(c).findModelRoutersForModelPool(context.Background(), pool)
	got := make([]string, 0, len(requests))
	for _, req := range requests {
		got = append(got, req.Name)
	}
	if !slices.Equal(got, []string{"models"}) {
		t.Fatalf("ModelPool event enqueued %v, want only the selector router", got)
	}
}

// mapperEnqueued reports whether a watch mapper's result names the router.
func mapperEnqueued(reqs []reconcile.Request, name, ns string) bool {
	for _, req := range reqs {
		if req.Name == name && req.Namespace == ns {
			return true
		}
	}
	return false
}

// TestModelRouterSelectorOverlayBalancing pins that an overlay entry may carry
// balancing: the CEL rule admits it on a ref-less, non-external entry with
// endpoint resolution, and the merged backend keeps it.
func TestModelRouterSelectorOverlayBalancing(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	createSelectedInferenceService(t, c, ns, "glm-5-3-flash", selectorLabels())
	createTypedSelectorRouter(t, c, ns, "models", routerSelector(),
		[]inferencev1alpha1.RouterBackend{{
			Name:       "glm-5-3-flash",
			Resolution: inferencev1alpha1.RouterBackendResolutionEndpoint,
			Balancing:  &inferencev1alpha1.BackendBalancing{Mode: "LoadAware"},
		}}, nil)
	reconcileSelectorRouter(t, selectorReconciler(c), ns, "models")

	b := configBackend(t, routerConfigJSON(t, c, ns, "models"), "glm-5-3-flash")
	balancing, ok := b["balancing"].(map[string]any)
	if !ok || balancing["mode"] != "LoadAware" {
		t.Errorf("overlay backend balancing = %v, want mode LoadAware from the entry", b["balancing"])
	}

	// The loosened rule reaches overlays only: without a selector the same
	// entry is still refused, because nothing gives it an InferenceService.
	noSelector := &inferencev1alpha1.ModelRouter{
		Spec: inferencev1alpha1.ModelRouterSpec{
			Backends: []inferencev1alpha1.RouterBackend{{
				Name:       "glm-5-3-flash",
				Resolution: inferencev1alpha1.RouterBackendResolutionEndpoint,
				Balancing:  &inferencev1alpha1.BackendBalancing{Mode: "LoadAware"},
			}},
		},
	}
	err := (&ModelRouterValidator{}).validate(noSelector)
	if err == nil || !strings.Contains(err.Error(), "exactly one of inferenceServiceRef or external must be set") {
		t.Fatalf("webhook error = %v, want the ref-less entry refused without a selector", err)
	}
}

// TestModelRouterSelectorCompileErrorHint pins that the deselection hint goes
// only on the unknown-backend-name error, not on every validation failure of a
// selector router's compiled config.
func TestModelRouterSelectorCompileErrorHint(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	createSelectedInferenceService(t, c, ns, "glm-5-3-flash", selectorLabels())
	zero := int32(0)
	createTypedSelectorRouter(t, c, ns, "models", routerSelector(),
		[]inferencev1alpha1.RouterBackend{{Name: "glm-5-3-flash", Weight: &zero}},
		[]inferencev1alpha1.RouterRule{{
			Name:  "spread",
			Route: inferencev1alpha1.RuleRoute{Backends: []string{"glm-5-3-flash"}, Strategy: "weighted"},
		}})
	reconcileSelectorRouter(t, selectorReconciler(c), ns, "models")

	stored := fetchTypedRouter(t, c, ns, "models")
	available := apimeta.FindStatusCondition(stored.Status.Conditions, ConditionAvailable)
	if available == nil || available.Status != metav1.ConditionFalse || available.Reason != ModelRouterReasonCompileFailed {
		t.Fatalf("Available = %+v, want False/%s", available, ModelRouterReasonCompileFailed)
	}
	if !strings.Contains(available.Message, "positive weight") {
		t.Fatalf("Available message = %q, want the weighted-rule failure", available.Message)
	}
	if strings.Contains(available.Message, "backendSelector") {
		t.Errorf("Available message = %q carries the deselection hint, but the failure is a weighted rule, not a missing backend",
			available.Message)
	}
}

// TestModelRouterSelectorDisplayNameClaimed pins that an explicit entry's
// displayName claims that model id: a selected service of the same name is not
// added, so /v1/models never publishes an id twice.
// TestModelRouterSelectorDisplayNameClaimed pins that an explicit entry's
// displayName claims its published id exactly as its name does, so a selected
// service of that id is not added and GET /v1/models lists one id once.
func TestModelRouterSelectorDisplayNameClaimed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry inferencev1alpha1.RouterBackend
	}{
		{
			name: "referenced entry",
			entry: inferencev1alpha1.RouterBackend{
				Name:                "glm-5-3-flash",
				DisplayName:         "llama-70b",
				InferenceServiceRef: &corev1.LocalObjectReference{Name: "glm-5-3-flash"},
			},
		},
		{
			name: "overlay",
			entry: inferencev1alpha1.RouterBackend{
				Name:        "glm-5-3-flash",
				DisplayName: "llama-70b",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, stop := startBudgetTestEnv(t)
			defer stop()
			ns := newTestNamespace(t, c)

			createSelectedInferenceService(t, c, ns, "llama-70b", selectorLabels())
			createSelectedInferenceService(t, c, ns, "glm-5-3-flash", selectorLabels())
			createTypedSelectorRouter(t, c, ns, "models", routerSelector(),
				[]inferencev1alpha1.RouterBackend{tc.entry}, nil)
			reconcileSelectorRouter(t, selectorReconciler(c), ns, "models")

			cfg := routerConfigJSON(t, c, ns, "models")
			assertBackendNames(t, cfg, "glm-5-3-flash")
			if got := modelIDsFromProxy(t, cfg); !slices.Equal(got, []string{"llama-70b"}) {
				t.Errorf("GET /v1/models = %v, want the explicit entry's displayName once", got)
			}
		})
	}
}

// TestModelRouterSelectorInferenceServiceEventMatchesLabels pins the watch
// filter: only an event whose object the selector matches re-renders a
// selector router, except a selector that does not parse, which always does so
// the reconcile can report it.
func TestModelRouterSelectorInferenceServiceEventMatchesLabels(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	createTypedSelectorRouter(t, c, ns, "models", routerSelector(), nil, nil)
	createTypedSelectorRouter(t, c, ns, "broken", &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      "llmkube.dev/router",
			Operator: "Equals",
			Values:   []string{"models"},
		}},
	}, nil, nil)
	r := selectorReconciler(c)

	matched := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{
		Name: "matched", Namespace: ns, Labels: selectorLabels(),
	}}
	unmatched := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{
		Name: "unmatched", Namespace: ns, Labels: map[string]string{"other": "label"},
	}}

	if reqs := r.findModelRoutersForInferenceService(context.Background(), matched); !mapperEnqueued(reqs, "models", ns) {
		t.Errorf("a matching InferenceService event enqueued %v, want the selector router", reqs)
	}
	reqs := r.findModelRoutersForInferenceService(context.Background(), unmatched)
	if mapperEnqueued(reqs, "models", ns) {
		t.Errorf("an unrelated InferenceService event enqueued %v, want no selector router", reqs)
	}
	if !mapperEnqueued(reqs, "broken", ns) {
		t.Errorf("an unrelated InferenceService event enqueued %v, want the router whose selector does not parse", reqs)
	}

	// A label removal is an update: controller-runtime runs the mapper on the
	// old object as well, and its labels still match.
	relabelled := matched.DeepCopy()
	relabelled.Labels = nil
	if reqs := r.findModelRoutersForInferenceService(context.Background(), relabelled); mapperEnqueued(reqs, "models", ns) {
		t.Errorf("the relabelled object alone enqueued %v, want nothing", reqs)
	}
	if reqs := r.findModelRoutersForInferenceService(context.Background(), matched); !mapperEnqueued(reqs, "models", ns) {
		t.Errorf("removing the label did not enqueue the selector router through the old object: got %v", reqs)
	}
}
