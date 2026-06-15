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
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These tests mirror the plain-Go envtest style of the InferenceService gateway
// tests (inferenceservice_gateway_test.go): each case spins up its own envtest
// with or without the aigw CRD stubs so the CRDs-present and CRDs-absent worlds
// never bleed together. They reuse startGatewayTestEnv / assertOwnedBy from that
// file (same package).

// newModelRouterGatewayReconciler builds a ModelRouter gateway reconciler backed
// by a client whose RESTMapper is dynamic, so the CRD-presence gate reflects the
// env it runs against.
func newModelRouterGatewayReconciler(t *testing.T, cfg *rest.Config) *ModelRouterGatewayReconciler {
	t.Helper()
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatalf("http client: %v", err)
	}
	mapper, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	if err != nil {
		t.Fatalf("rest mapper: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme, Mapper: mapper})
	if err != nil {
		t.Fatalf("new mapped client: %v", err)
	}
	return &ModelRouterGatewayReconciler{Client: c, Scheme: scheme.Scheme}
}

func reconcileRouter(t *testing.T, r *ModelRouterGatewayReconciler, mr *inferencev1alpha1.ModelRouter) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
}

// makeBackendISvc creates a minimal InferenceService a ModelRouter backend can
// reference, so resolveBackends finds a real Service FQDN/port.
func makeBackendISvc(t *testing.T, c client.Client, name string) {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: name,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 8080},
		},
	}
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create backend isvc %s: %v", name, err)
	}
}

// assertNotExists asserts a resource of the given GVK/name is absent.
func assertNotExists(t *testing.T, c client.Client, gvk schema.GroupVersionKind, name string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: testNS}, u)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected %s/%s to not exist, get err = %v", gvk.Kind, name, err)
	}
}

// TestModelRouterGateway_FailoverProducesResources covers case (a): a
// dataPlane: Gateway ModelRouter with two backends and a primary-fallback rule
// produces a Backend + AIServiceBackend per backend, a multi-rule AIGatewayRoute
// with priority backendRefs, and the retry BackendTrafficPolicy, all owner-ref'd
// to the ModelRouter.
func TestModelRouterGateway_FailoverProducesResources(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "qwen-cuda")
	makeBackendISvc(t, c, "qwen-metal")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen-router", Namespace: testNS},
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
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	// A Backend + AIServiceBackend per backend, named after the RouterBackend.
	for _, name := range []string{"qwen-cuda", "qwen-metal"} {
		backend := getUnstructured(t, c, backendGVK(), name)
		if host := backendHostname(t, backend); host != name+".default.svc.cluster.local" {
			t.Errorf("backend %s hostname = %q, want %s.default.svc.cluster.local", name, host, name)
		}
		assertOwnedByRouter(t, backend, mr)

		asb := getUnstructured(t, c, aiServiceBackendGVK(), name)
		schemaName, _, _ := unstructured.NestedString(asb.Object, "spec", "schema", "name")
		if schemaName != "OpenAI" {
			t.Errorf("aiservicebackend %s schema.name = %q, want OpenAI", name, schemaName)
		}
		assertOwnedByRouter(t, asb, mr)
	}

	// One AIGatewayRoute named after the ModelRouter, attached to the Gateway,
	// its single rule matching qwen35-27b with priority backendRefs (0 = primary
	// cuda, 1 = fallback metal).
	route := getUnstructured(t, c, aiGatewayRouteGVK(), "qwen-router")
	assertOwnedByRouter(t, route, mr)
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	if len(rules) != 1 {
		t.Fatalf("route has %d rules, want 1", len(rules))
	}
	rule0 := rules[0].(map[string]interface{})
	if got := routeModelOfRule(t, rule0); got != "qwen35-27b" {
		t.Errorf("rule model match = %q, want qwen35-27b", got)
	}
	refs := rule0["backendRefs"].([]interface{})
	if len(refs) != 2 {
		t.Fatalf("rule has %d backendRefs, want 2", len(refs))
	}
	assertBackendRefPriority(t, refs[0], "qwen-cuda", 0)
	assertBackendRefPriority(t, refs[1], "qwen-metal", 1)

	// The retry BackendTrafficPolicy targets the generated HTTPRoute (shares the
	// route name) and carries the retry + passive healthCheck config.
	btp := getUnstructured(t, c, btpGVK(), "qwen-router")
	assertOwnedByRouter(t, btp, mr)
	targetRefs, _, _ := unstructured.NestedSlice(btp.Object, "spec", "targetRefs")
	if len(targetRefs) != 1 {
		t.Fatalf("btp has %d targetRefs, want 1", len(targetRefs))
	}
	tr := targetRefs[0].(map[string]interface{})
	if tr["kind"] != "HTTPRoute" || tr["name"] != "qwen-router" {
		t.Errorf("btp targetRef = %+v, want HTTPRoute/qwen-router", tr)
	}
	if _, found, _ := unstructured.NestedMap(btp.Object, "spec", "retry"); !found {
		t.Error("btp missing spec.retry")
	}
	if _, found, _ := unstructured.NestedMap(btp.Object, "spec", "healthCheck", "passive"); !found {
		t.Error("btp missing spec.healthCheck.passive")
	}
	// 2b adds rateLimit to THIS BTP; 2a must NOT include it.
	if _, found, _ := unstructured.NestedMap(btp.Object, "spec", "rateLimit"); found {
		t.Error("btp should not carry rateLimit in slice 2a")
	}

	// status.gateway + GatewayReady=True.
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "qwen-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter status: %v", err)
	}
	if fresh.Status.Gateway == nil || !fresh.Status.Gateway.RouteReady {
		t.Errorf("status.gateway.routeReady not set, got %+v", fresh.Status.Gateway)
	}
	if cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("GatewayReady condition not True, got %+v", cond)
	}
}

// TestModelRouterGateway_UnsupportedMatchFailsLoud covers case (b): a rule using
// dataClassification (a match the gateway data plane cannot express) sets
// GatewayReady=False with reason UnsupportedMatchInGatewayMode and generates
// NOTHING.
func TestModelRouterGateway_UnsupportedMatchFailsLoud(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "local-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "pii-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane:  inferencev1alpha1.ModelRouterDataPlaneGateway,
			GatewayRef: &inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "local-cuda", InferenceServiceRef: corev1LocalRef("local-cuda")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:       "pii",
					Match:      &inferencev1alpha1.RuleMatch{DataClassification: []string{"pii"}},
					FailClosed: true,
					Route:      inferencev1alpha1.RuleRoute{Backends: []string{"local-cuda"}},
				},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	// Generates NOTHING: no Backend, AIServiceBackend, route, or BTP.
	assertNotExists(t, c, backendGVK(), "local-cuda")
	assertNotExists(t, c, aiServiceBackendGVK(), "local-cuda")
	assertNotExists(t, c, aiGatewayRouteGVK(), "pii-router")
	assertNotExists(t, c, btpGVK(), "pii-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "pii-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterGatewayReasonUnsupported {
		t.Errorf("expected GatewayReady=False/%s, got %+v", modelRouterGatewayReasonUnsupported, cond)
	}
	if fresh.Status.Gateway != nil && fresh.Status.Gateway.RouteReady {
		t.Errorf("status.gateway.routeReady should be false on unsupported match")
	}
}

// TestModelRouterGateway_ProxyModeProducesNothing covers case (c): a
// dataPlane: Proxy (default) ModelRouter generates no gateway resources (the
// gateway reconciler no-ops; the proxy path is owned by ModelRouterReconciler
// and is unaffected).
func TestModelRouterGateway_ProxyModeProducesNothing(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "proxy-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			// DataPlane omitted -> defaults to Proxy at the API server.
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "proxy-cuda", InferenceServiceRef: corev1LocalRef("proxy-cuda")},
			},
			DefaultRoute: "proxy-cuda",
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	// Confirm the default landed as Proxy.
	created := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "proxy-router", Namespace: testNS}, created); err != nil {
		t.Fatalf("get created router: %v", err)
	}
	if created.Spec.DataPlane != inferencev1alpha1.ModelRouterDataPlaneProxy {
		t.Fatalf("expected default DataPlane Proxy, got %q", created.Spec.DataPlane)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, created)

	assertNotExists(t, c, backendGVK(), "proxy-cuda")
	assertNotExists(t, c, aiServiceBackendGVK(), "proxy-cuda")
	assertNotExists(t, c, aiGatewayRouteGVK(), "proxy-router")
	assertNotExists(t, c, btpGVK(), "proxy-router")

	// The gateway reconciler must not have written status.gateway in Proxy mode.
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "proxy-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if fresh.Status.Gateway != nil {
		t.Errorf("expected nil status.gateway in Proxy mode, got %+v", fresh.Status.Gateway)
	}
}

// TestModelRouterGateway_CRDsAbsentIsCleanNoOp covers case (d): with the aigw
// CRDs not installed, a dataPlane: Gateway ModelRouter does not error/crash,
// creates nothing, and sets the disabled GatewayReady condition.
func TestModelRouterGateway_CRDsAbsentIsCleanNoOp(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, false)
	defer stop()

	makeBackendISvc(t, c, "absent-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "absent-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane:  inferencev1alpha1.ModelRouterDataPlaneGateway,
			GatewayRef: &inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "absent-cuda", InferenceServiceRef: corev1LocalRef("absent-cuda")},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	// Must not error or panic.
	reconcileRouter(t, r, mr)

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "absent-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterGatewayReasonCRDsMissing {
		t.Errorf("expected GatewayReady=False/%s, got %+v", modelRouterGatewayReasonCRDsMissing, cond)
	}
}

// authedRouter builds a dataPlane: Gateway ModelRouter with one backend, one
// simple model rule, and the given (possibly nil) JWT auth block.
func authedRouter(name string, jwt *inferencev1alpha1.JWTAuthSpec) *inferencev1alpha1.ModelRouter {
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane:  inferencev1alpha1.ModelRouterDataPlaneGateway,
			GatewayRef: &inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "qwen-cuda", InferenceServiceRef: corev1LocalRef("qwen-cuda")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "qwen",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{Backends: []string{"qwen-cuda"}},
				},
			},
		},
	}
	if jwt != nil {
		mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
			Auth: &inferencev1alpha1.RouterAuthSpec{JWT: jwt},
		}
	}
	return mr
}

// newFakeRouterReconcilerWithGateway builds a ModelRouter gateway reconciler
// backed by a fake client (no CRD field validation) whose RESTMapper reports the
// gateway GVKs as present, so the CRD-presence gate passes and the reconcile body
// runs. Used to exercise reconcile-orchestration paths (like the auth fail-loud
// guard) with object shapes a real CRD would reject at apply time.
func newFakeRouterReconcilerWithGateway(t *testing.T, objs ...client.Object) *ModelRouterGatewayReconciler {
	t.Helper()
	s := scheme.Scheme
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	mapper := apimeta.NewDefaultRESTMapper(nil)
	for _, gvk := range modelRouterGatewayGVKs() {
		mapper.Add(gvk, apimeta.RESTScopeNamespace)
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithRESTMapper(mapper).
		WithObjects(objs...).
		WithStatusSubresource(&inferencev1alpha1.ModelRouter{}).
		Build()
	return &ModelRouterGatewayReconciler{Client: c, Scheme: s}
}

// assertNotExistsClient asserts a resource of the given GVK/name is absent in the
// given client (the envtest variant, assertNotExists, uses the package client c).
func assertNotExistsClient(t *testing.T, c client.Client, gvk schema.GroupVersionKind, name string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: testNS}, u)
	if err == nil {
		t.Errorf("expected %s/%s to not exist, but it does", gvk.Kind, name)
	}
}

// claimToHeaderOf extracts the single claim->header mapping from a SecurityPolicy
// jwt provider, returning (claim, header).
func claimToHeaderOf(t *testing.T, sp *unstructured.Unstructured) (string, string) {
	t.Helper()
	providers, found, err := unstructured.NestedSlice(sp.Object, "spec", "jwt", "providers")
	if err != nil || !found || len(providers) == 0 {
		t.Fatalf("securitypolicy has no jwt.providers (found=%v err=%v)", found, err)
	}
	p := providers[0].(map[string]interface{})
	cths := p["claimToHeaders"].([]interface{})
	if len(cths) != 1 {
		t.Fatalf("jwt provider has %d claimToHeaders, want 1", len(cths))
	}
	cth := cths[0].(map[string]interface{})
	claim, _ := cth["claim"].(string)
	header, _ := cth["header"].(string)
	return claim, header
}

// TestModelRouterGateway_JWTAuthProducesSecurityPolicy covers case (a): a
// ModelRouter with policy.auth.jwt set produces a SecurityPolicy owner-ref'd to
// the router, targeting the generated HTTPRoute (shares the route name), with the
// provider's issuer + remoteJWKS.uri and a claimToHeaders mapping the teamClaim
// onto the default x-llmkube-team header.
func TestModelRouterGateway_JWTAuthProducesSecurityPolicy(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "qwen-cuda")

	mr := authedRouter("auth-router", &inferencev1alpha1.JWTAuthSpec{
		Provider:  "keycloak",
		Issuer:    "https://issuer.example/realms/lab",
		JWKSURI:   "https://issuer.example/realms/lab/protocol/openid-connect/certs",
		TeamClaim: "team",
	})
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	sp := getUnstructured(t, c, securityPolicyGVK(), "auth-router")
	assertOwnedByRouter(t, sp, mr)

	// Targets the generated HTTPRoute by the router's name.
	targetRefs, _, _ := unstructured.NestedSlice(sp.Object, "spec", "targetRefs")
	if len(targetRefs) != 1 {
		t.Fatalf("securitypolicy has %d targetRefs, want 1", len(targetRefs))
	}
	tr := targetRefs[0].(map[string]interface{})
	if tr["kind"] != "HTTPRoute" || tr["name"] != "auth-router" {
		t.Errorf("securitypolicy targetRef = %+v, want HTTPRoute/auth-router", tr)
	}

	// Provider carries issuer + remoteJWKS.uri.
	providers, _, _ := unstructured.NestedSlice(sp.Object, "spec", "jwt", "providers")
	if len(providers) != 1 {
		t.Fatalf("jwt has %d providers, want 1", len(providers))
	}
	p := providers[0].(map[string]interface{})
	if p["name"] != "keycloak" {
		t.Errorf("provider name = %v, want keycloak", p["name"])
	}
	if p["issuer"] != "https://issuer.example/realms/lab" {
		t.Errorf("provider issuer = %v", p["issuer"])
	}
	remoteURI, _ := p["remoteJWKS"].(map[string]interface{})["uri"].(string)
	if remoteURI != "https://issuer.example/realms/lab/protocol/openid-connect/certs" {
		t.Errorf("provider remoteJWKS.uri = %q", remoteURI)
	}

	// claimToHeaders maps team -> default x-llmkube-team.
	claim, header := claimToHeaderOf(t, sp)
	if claim != "team" {
		t.Errorf("claimToHeaders claim = %q, want team", claim)
	}
	if header != "x-llmkube-team" {
		t.Errorf("claimToHeaders header = %q, want x-llmkube-team (default)", header)
	}

	// The authorization stanza of the spike manifest is slice 2d.2; 2d-core must
	// NOT compile it.
	if _, found, _ := unstructured.NestedMap(sp.Object, "spec", "authorization"); found {
		t.Error("securitypolicy should not carry spec.authorization in slice 2d-core")
	}

	// status surfaces auth enabled.
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "auth-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter status: %v", err)
	}
	if fresh.Status.Gateway == nil || !fresh.Status.Gateway.AuthEnabled {
		t.Errorf("status.gateway.authEnabled not set, got %+v", fresh.Status.Gateway)
	}
	if cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("GatewayReady not True, got %+v", cond)
	}
}

// TestModelRouterGateway_JWTAuthHonorsCustomHeaderKey covers case (b): an
// explicit headerKey overrides the x-llmkube-team default in the claimToHeaders
// mapping.
func TestModelRouterGateway_JWTAuthHonorsCustomHeaderKey(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "qwen-cuda")

	mr := authedRouter("custom-hdr-router", &inferencev1alpha1.JWTAuthSpec{
		Provider:  "keycloak",
		Issuer:    "https://issuer.example/realms/lab",
		JWKSURI:   "https://issuer.example/realms/lab/certs",
		TeamClaim: "tenant",
		HeaderKey: "x-tenant-id",
	})
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	sp := getUnstructured(t, c, securityPolicyGVK(), "custom-hdr-router")
	claim, header := claimToHeaderOf(t, sp)
	if claim != "tenant" {
		t.Errorf("claimToHeaders claim = %q, want tenant", claim)
	}
	if header != "x-tenant-id" {
		t.Errorf("claimToHeaders header = %q, want x-tenant-id", header)
	}
}

// TestModelRouterGateway_InvalidAuthRejectedByCRD covers case (c) primary guard:
// an auth.jwt block missing a required field (here, jwksURI) is rejected by the
// CRD's Required+MinLength validation at apply time, so a half-configured
// SecurityPolicy can never be persisted.
func TestModelRouterGateway_InvalidAuthRejectedByCRD(t *testing.T) {
	c, _, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "qwen-cuda")

	mr := authedRouter("badauth-router", &inferencev1alpha1.JWTAuthSpec{
		Provider:  "keycloak",
		Issuer:    "https://issuer.example/realms/lab",
		JWKSURI:   "", // missing required field
		TeamClaim: "team",
	})
	err := c.Create(context.Background(), mr)
	if err == nil {
		t.Fatalf("expected CRD to reject auth.jwt with empty jwksURI, but create succeeded")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error from CRD validation, got %v", err)
	}
}

// TestModelRouterGateway_InvalidAuthFailsLoud covers case (c) defense-in-depth:
// the reconciler's fail-loud guard. If a half-configured auth.jwt somehow reaches
// the reconciler (e.g. a future CRD relaxation, or an object written before a
// validation tightening), it sets GatewayReady=False/InvalidAuth and generates
// NOTHING rather than emitting a partial SecurityPolicy. The CRD-validated env
// cannot persist such an object, so this drives the reconcile against a
// validation-free fake client. The CRD-presence gate is satisfied by registering
// the gateway GVKs in the fake client's RESTMapper.
func TestModelRouterGateway_InvalidAuthFailsLoud(t *testing.T) {
	mr := authedRouter("badauth-router", &inferencev1alpha1.JWTAuthSpec{
		Provider:  "keycloak",
		Issuer:    "https://issuer.example/realms/lab",
		JWKSURI:   "", // missing required field
		TeamClaim: "team",
	})

	r := newFakeRouterReconcilerWithGateway(t, mr)
	reconcileRouter(t, r, mr)

	// Generates NOTHING: no SecurityPolicy, and none of the route resources.
	assertNotExistsClient(t, r.Client, securityPolicyGVK(), "badauth-router")
	assertNotExistsClient(t, r.Client, backendGVK(), "qwen-cuda")
	assertNotExistsClient(t, r.Client, aiServiceBackendGVK(), "qwen-cuda")
	assertNotExistsClient(t, r.Client, aiGatewayRouteGVK(), "badauth-router")
	assertNotExistsClient(t, r.Client, btpGVK(), "badauth-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "badauth-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterGatewayReasonInvalidAuth {
		t.Errorf("expected GatewayReady=False/%s, got %+v", modelRouterGatewayReasonInvalidAuth, cond)
	}
	if fresh.Status.Gateway != nil && fresh.Status.Gateway.RouteReady {
		t.Errorf("status.gateway.routeReady should be false on invalid auth")
	}
}

// TestModelRouterGateway_NoAuthProducesNoSecurityPolicy covers case (d): a
// ModelRouter with no policy.auth generates the slice-2a resources but NO
// SecurityPolicy, and status.gateway.authEnabled is false. This is the #693
// non-regression: the rest of the output is unchanged.
func TestModelRouterGateway_NoAuthProducesNoSecurityPolicy(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "qwen-cuda")

	mr := authedRouter("noauth-router", nil)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	// The 2a resources still exist (non-regression).
	getUnstructured(t, c, backendGVK(), "qwen-cuda")
	getUnstructured(t, c, aiServiceBackendGVK(), "qwen-cuda")
	getUnstructured(t, c, aiGatewayRouteGVK(), "noauth-router")
	getUnstructured(t, c, btpGVK(), "noauth-router")

	// But NO SecurityPolicy.
	assertNotExists(t, c, securityPolicyGVK(), "noauth-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "noauth-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if fresh.Status.Gateway == nil || fresh.Status.Gateway.AuthEnabled {
		t.Errorf("status.gateway.authEnabled should be false with no auth, got %+v", fresh.Status.Gateway)
	}
}

// allowlistRouter builds a Gateway-mode ModelRouter with the given JWT auth and
// allowlists. A nil jwt leaves policy.auth.jwt unset (the AuthorizationRequiresJWT
// case); a nil allowlists leaves authorization off.
func allowlistRouter(
	name string,
	jwt *inferencev1alpha1.JWTAuthSpec,
	allowlists []inferencev1alpha1.TeamModelAllowlist,
) *inferencev1alpha1.ModelRouter {
	mr := authedRouter(name, jwt)
	if len(allowlists) > 0 {
		if mr.Spec.Policy == nil {
			mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{}
		}
		if mr.Spec.Policy.Auth == nil {
			mr.Spec.Policy.Auth = &inferencev1alpha1.RouterAuthSpec{}
		}
		mr.Spec.Policy.Auth.Allowlists = allowlists
	}
	return mr
}

// TestModelRouterGateway_AllowlistsWithoutJWTFailLoud covers the
// AuthorizationRequiresJWT boundary: allowlists set with no policy.auth.jwt sets
// GatewayReady=False/AuthorizationRequiresJWT and generates NOTHING (you cannot
// authorize on an unverified claim). The CRD permits this shape (jwt is
// optional), so it runs against a real envtest.
func TestModelRouterGateway_AllowlistsWithoutJWTFailLoud(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "qwen-cuda")

	mr := allowlistRouter("noauthz-router", nil, []inferencev1alpha1.TeamModelAllowlist{
		{Team: "platform"},
	})
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	// Generates NOTHING.
	assertNotExists(t, c, securityPolicyGVK(), "noauthz-router")
	assertNotExists(t, c, aiGatewayRouteGVK(), "noauthz-router")
	assertNotExists(t, c, btpGVK(), "noauthz-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "noauthz-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterGatewayReasonAuthzRequiresJWT {
		t.Errorf("expected GatewayReady=False/%s, got %+v", modelRouterGatewayReasonAuthzRequiresJWT, cond)
	}
}

// TestModelRouterGateway_EmptyTeamFailsLoud covers the InvalidAuthorization
// boundary for an empty team. The CRD's MinLength rejects an empty team at apply
// time, so (like the invalid-auth defense-in-depth test) this drives the
// reconciler's fail-loud guard against a validation-free fake client.
func TestModelRouterGateway_EmptyTeamFailsLoud(t *testing.T) {
	mr := allowlistRouter("emptyteam-router", jwtForAllowlist(), []inferencev1alpha1.TeamModelAllowlist{
		{Team: ""},
	})

	r := newFakeRouterReconcilerWithGateway(t, mr)
	reconcileRouter(t, r, mr)

	assertNotExistsClient(t, r.Client, securityPolicyGVK(), "emptyteam-router")
	assertNotExistsClient(t, r.Client, aiGatewayRouteGVK(), "emptyteam-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "emptyteam-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterGatewayReasonInvalidAuthz {
		t.Errorf("expected GatewayReady=False/%s, got %+v", modelRouterGatewayReasonInvalidAuthz, cond)
	}
}

// TestModelRouterGateway_DuplicateTeamFailsLoud covers the InvalidAuthorization
// boundary for a duplicate team. The CRD does not constrain this (no uniqueness
// validation), so it runs against a real envtest.
func TestModelRouterGateway_DuplicateTeamFailsLoud(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "qwen-cuda")

	mr := allowlistRouter("duputeam-router", jwtForAllowlist(), []inferencev1alpha1.TeamModelAllowlist{
		{Team: "team-b", Models: []string{"qwen35-27b"}},
		{Team: "team-b", Models: []string{"llama-8b"}},
	})
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	assertNotExists(t, c, securityPolicyGVK(), "duputeam-router")
	assertNotExists(t, c, aiGatewayRouteGVK(), "duputeam-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "duputeam-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterGatewayReasonInvalidAuthz {
		t.Errorf("expected GatewayReady=False/%s, got %+v", modelRouterGatewayReasonInvalidAuthz, cond)
	}
}

// TestModelRouterGateway_ValidAllowlistsProduceAuthorization covers the happy
// path: valid allowlists paired with JWT produce a Ready router whose
// SecurityPolicy carries the authorization block (default-Deny + an Allow rule
// per team), and whose ready message names both the JWT and the allowlist count.
func TestModelRouterGateway_ValidAllowlistsProduceAuthorization(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "qwen-cuda")

	mr := allowlistRouter("authz-router", jwtForAllowlist(), []inferencev1alpha1.TeamModelAllowlist{
		{Team: "platform"},
		{Team: "team-b", Models: []string{"qwen35-27b"}},
	})
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	sp := getUnstructured(t, c, securityPolicyGVK(), "authz-router")
	assertOwnedByRouter(t, sp, mr)
	rules := securityPolicyAuthzRules(t, sp)
	if len(rules) != 2 {
		t.Fatalf("got %d authorization rules, want 2", len(rules))
	}
	allowlistRuleByName(t, rules, "allow-platform")
	teamB := allowlistRuleByName(t, rules, "allow-team-b")
	if _, hasHeaders := teamB["principal"].(map[string]interface{})["headers"]; !hasHeaders {
		t.Error("model-scoped allow-team-b rule should carry principal.headers")
	}

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "authz-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("GatewayReady not True, got %+v", cond)
	}
	if !contains(cond.Message, "JWT authentication enforced") {
		t.Errorf("ready message should mention JWT, got %q", cond.Message)
	}
	if !contains(cond.Message, "2 team model allowlist(s) enforced") {
		t.Errorf("ready message should mention 2 allowlists, got %q", cond.Message)
	}
}

// --- slice 2d.2 builder unit tests: per-team model allowlists ---

// jwtForAllowlist is the JWT block the allowlist builder tests pair with: a
// fully-configured keycloak provider whose teamClaim ("team") and provider name
// ("keycloak") the authorization principals reference.
func jwtForAllowlist() *inferencev1alpha1.JWTAuthSpec {
	return &inferencev1alpha1.JWTAuthSpec{
		Provider:  "keycloak",
		Issuer:    "https://issuer.example/realms/lab",
		JWKSURI:   "https://issuer.example/realms/lab/certs",
		TeamClaim: "team",
	}
}

// securityPolicyAuthzRules extracts spec.authorization.rules from a built
// SecurityPolicy, asserting defaultAction is Deny.
func securityPolicyAuthzRules(t *testing.T, sp *unstructured.Unstructured) []interface{} {
	t.Helper()
	authz, found, err := unstructured.NestedMap(sp.Object, "spec", "authorization")
	if err != nil || !found {
		t.Fatalf("securitypolicy has no spec.authorization (found=%v err=%v)", found, err)
	}
	if authz["defaultAction"] != "Deny" {
		t.Errorf("authorization.defaultAction = %v, want Deny", authz["defaultAction"])
	}
	rules, ok := authz["rules"].([]interface{})
	if !ok {
		t.Fatalf("authorization.rules is not a slice: %T", authz["rules"])
	}
	return rules
}

// allowlistRuleByName finds the authorization rule with the given name.
func allowlistRuleByName(t *testing.T, rules []interface{}, name string) map[string]interface{} {
	t.Helper()
	for _, r := range rules {
		rule := r.(map[string]interface{})
		if rule["name"] == name {
			return rule
		}
	}
	t.Fatalf("authorization rules have no entry named %q", name)
	return nil
}

// jwtPrincipalOf returns the principal.jwt block of an authorization rule.
func jwtPrincipalOf(t *testing.T, rule map[string]interface{}) map[string]interface{} {
	t.Helper()
	principal, ok := rule["principal"].(map[string]interface{})
	if !ok {
		t.Fatalf("rule %v has no principal map", rule["name"])
	}
	jwt, ok := principal["jwt"].(map[string]interface{})
	if !ok {
		t.Fatalf("rule %v principal has no jwt map", rule["name"])
	}
	return jwt
}

// TestNewRouterSecurityPolicy_NoAllowlistsNoAuthorization covers builder case
// (a): a SecurityPolicy built with an empty allowlist carries NO
// spec.authorization key, byte-identical to the slice 2d-core output. This is
// the back-compat contract: turning on JWT without allowlists must not flip the
// router to default-Deny.
func TestNewRouterSecurityPolicy_NoAllowlistsNoAuthorization(t *testing.T) {
	mr := authedRouter("plain-auth", jwtForAllowlist())

	withAllowlists := newRouterSecurityPolicy(mr, jwtForAllowlist(), nil)
	if _, found, _ := unstructured.NestedMap(withAllowlists.Object, "spec", "authorization"); found {
		t.Error("securitypolicy with no allowlists must NOT carry spec.authorization")
	}
}

// TestNewRouterSecurityPolicy_EmptyModelsIdentityOnly covers builder case (b): a
// team entry with no models compiles to an Allow rule whose principal is a
// jwt-claim match on the teamClaim, and carries NO header restriction (the team
// may reach all models).
func TestNewRouterSecurityPolicy_EmptyModelsIdentityOnly(t *testing.T) {
	mr := authedRouter("identity-only", jwtForAllowlist())
	allowlists := []inferencev1alpha1.TeamModelAllowlist{
		{Team: "platform"},
	}

	sp := newRouterSecurityPolicy(mr, jwtForAllowlist(), allowlists)
	rules := securityPolicyAuthzRules(t, sp)
	if len(rules) != 1 {
		t.Fatalf("got %d authorization rules, want 1", len(rules))
	}
	rule := allowlistRuleByName(t, rules, "allow-platform")
	if rule["action"] != "Allow" {
		t.Errorf("rule action = %v, want Allow", rule["action"])
	}

	jwt := jwtPrincipalOf(t, rule)
	if jwt["provider"] != "keycloak" {
		t.Errorf("principal.jwt.provider = %v, want keycloak", jwt["provider"])
	}
	claims := jwt["claims"].([]interface{})
	if len(claims) != 1 {
		t.Fatalf("got %d claims, want 1", len(claims))
	}
	claim := claims[0].(map[string]interface{})
	if claim["name"] != "team" {
		t.Errorf("claim name = %v, want team (the teamClaim)", claim["name"])
	}
	vals := claim["values"].([]interface{})
	if len(vals) != 1 || vals[0] != "platform" {
		t.Errorf("claim values = %v, want [platform]", vals)
	}

	// Identity-only: NO header restriction on the principal.
	if _, hasHeaders := rule["principal"].(map[string]interface{})["headers"]; hasHeaders {
		t.Error("identity-only allowlist entry must NOT carry principal.headers")
	}
}

// TestNewRouterSecurityPolicy_ModelsAddHeaderMatch covers builder case (c): a
// team entry WITH models compiles to an Allow rule whose principal carries both
// the jwt-claim match and an x-ai-eg-model header match listing exactly those
// models.
func TestNewRouterSecurityPolicy_ModelsAddHeaderMatch(t *testing.T) {
	mr := authedRouter("model-scoped", jwtForAllowlist())
	allowlists := []inferencev1alpha1.TeamModelAllowlist{
		{Team: "team-b", Models: []string{"qwen35-27b", "llama-8b"}},
	}

	sp := newRouterSecurityPolicy(mr, jwtForAllowlist(), allowlists)
	rules := securityPolicyAuthzRules(t, sp)
	rule := allowlistRuleByName(t, rules, "allow-team-b")

	principal := rule["principal"].(map[string]interface{})
	headers, ok := principal["headers"].([]interface{})
	if !ok || len(headers) != 1 {
		t.Fatalf("principal.headers = %v, want exactly one header match", principal["headers"])
	}
	h := headers[0].(map[string]interface{})
	if h["name"] != aiGatewayModelHeader {
		t.Errorf("header name = %v, want %s", h["name"], aiGatewayModelHeader)
	}
	vals := h["values"].([]interface{})
	if len(vals) != 2 || vals[0] != "qwen35-27b" || vals[1] != "llama-8b" {
		t.Errorf("header values = %v, want [qwen35-27b llama-8b]", vals)
	}
}

// TestNewRouterSecurityPolicy_MultipleTeams covers builder case (d): multiple
// allowlist entries compile to multiple Allow rules under a defaultAction Deny,
// each rule named after its (sanitized) team.
func TestNewRouterSecurityPolicy_MultipleTeams(t *testing.T) {
	mr := authedRouter("multi-team", jwtForAllowlist())
	allowlists := []inferencev1alpha1.TeamModelAllowlist{
		{Team: "platform"},
		{Team: "team-b", Models: []string{"qwen35-27b"}},
	}

	sp := newRouterSecurityPolicy(mr, jwtForAllowlist(), allowlists)
	rules := securityPolicyAuthzRules(t, sp)
	if len(rules) != 2 {
		t.Fatalf("got %d authorization rules, want 2", len(rules))
	}
	allowlistRuleByName(t, rules, "allow-platform")
	allowlistRuleByName(t, rules, "allow-team-b")
}

// --- helpers ---

// corev1LocalRef builds a *LocalObjectReference inline (avoids repeating the
// struct literal at every backend call site).
func corev1LocalRef(name string) *corev1.LocalObjectReference {
	return &corev1.LocalObjectReference{Name: name}
}

// assertOwnedByRouter verifies obj carries a controller owner reference to mr.
func assertOwnedByRouter(t *testing.T, obj *unstructured.Unstructured, mr *inferencev1alpha1.ModelRouter) {
	t.Helper()
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "ModelRouter" && ref.Name == mr.Name {
			if ref.Controller == nil || !*ref.Controller {
				t.Errorf("%s/%s owner ref to %s is not a controller ref", obj.GetKind(), obj.GetName(), mr.Name)
			}
			return
		}
	}
	t.Errorf("%s/%s missing owner reference to ModelRouter %s", obj.GetKind(), obj.GetName(), mr.Name)
}

// routeModelOfRule extracts the x-ai-eg-model header match value from the first
// match of a route rule map.
func routeModelOfRule(t *testing.T, rule map[string]interface{}) string {
	t.Helper()
	matches := rule["matches"].([]interface{})
	headers := matches[0].(map[string]interface{})["headers"].([]interface{})
	for _, h := range headers {
		header := h.(map[string]interface{})
		if header["name"] == aiGatewayModelHeader {
			val, _ := header["value"].(string)
			return val
		}
	}
	t.Fatalf("rule match has no %s header", aiGatewayModelHeader)
	return ""
}

// assertBackendRefPriority verifies a backendRef has the given name and priority.
func assertBackendRefPriority(t *testing.T, ref interface{}, wantName string, wantPriority int64) {
	t.Helper()
	m := ref.(map[string]interface{})
	if m["name"] != wantName {
		t.Errorf("backendRef name = %v, want %s", m["name"], wantName)
	}
	got, ok := m["priority"].(int64)
	if !ok {
		t.Fatalf("backendRef %s priority is %T, want int64", wantName, m["priority"])
	}
	if got != wantPriority {
		t.Errorf("backendRef %s priority = %d, want %d", wantName, got, wantPriority)
	}
}

// TestModelRouterGateway_RouterBudgetProducesRateLimit covers 2b case (a): a
// router-scope MaxTokens budget extends the SAME BackendTrafficPolicy 2a
// generates so it carries BOTH the retry stanza AND a global rateLimit rule
// (token limit + window unit), and the AIGatewayRoute carries the
// llmRequestCosts (TotalToken) metadata that charges the limit at response
// completion.
func TestModelRouterGateway_RouterBudgetProducesRateLimit(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "qwen-cuda")

	maxTokens := int64(5000000)
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "budget-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane:  inferencev1alpha1.ModelRouterDataPlaneGateway,
			GatewayRef: &inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "qwen-cuda", InferenceServiceRef: corev1LocalRef("qwen-cuda")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "qwen",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{Backends: []string{"qwen-cuda"}},
				},
			},
			Policy: &inferencev1alpha1.RouterPolicy{
				Budgets: []inferencev1alpha1.BudgetSpec{
					{
						Name:          "fleet-cap",
						Scope:         "router",
						WindowSeconds: 3600,
						MaxTokens:     &maxTokens,
					},
				},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	// The BTP keeps the 2a retry + healthCheck AND now carries a Global rateLimit.
	btp := getUnstructured(t, c, btpGVK(), "budget-router")
	if _, found, _ := unstructured.NestedMap(btp.Object, "spec", "retry"); !found {
		t.Error("btp missing spec.retry (2a stanza must remain)")
	}
	if _, found, _ := unstructured.NestedMap(btp.Object, "spec", "healthCheck", "passive"); !found {
		t.Error("btp missing spec.healthCheck.passive (2a stanza must remain)")
	}
	rlType, _, _ := unstructured.NestedString(btp.Object, "spec", "rateLimit", "type")
	if rlType != "Global" {
		t.Errorf("btp rateLimit.type = %q, want Global", rlType)
	}
	rlRules, found, _ := unstructured.NestedSlice(btp.Object, "spec", "rateLimit", "global", "rules")
	if !found || len(rlRules) != 1 {
		t.Fatalf("btp rateLimit.global.rules = %v (found=%v), want 1 rule", rlRules, found)
	}
	rule0 := rlRules[0].(map[string]interface{})
	limit := rule0["limit"].(map[string]interface{})
	if got, _ := limit["requests"].(int64); got != maxTokens {
		t.Errorf("rateLimit limit.requests = %v, want %d", limit["requests"], maxTokens)
	}
	if unit, _ := limit["unit"].(string); unit != "Hour" {
		t.Errorf("rateLimit limit.unit = %q, want Hour (3600s)", unit)
	}
	// router scope is global: no clientSelectors keyed on a header.
	if _, hasSel := rule0["clientSelectors"]; hasSel {
		t.Errorf("router-scope rule must not carry clientSelectors, got %+v", rule0["clientSelectors"])
	}
	// cost charges tokens at response completion (check-only on request path).
	cost := rule0["cost"].(map[string]interface{})
	reqCost := cost["request"].(map[string]interface{})
	if n, _ := reqCost["number"].(int64); n != 0 {
		t.Errorf("cost.request.number = %v, want 0 (check-only)", reqCost["number"])
	}
	respMeta := cost["response"].(map[string]interface{})["metadata"].(map[string]interface{})
	if respMeta["namespace"] != aiGatewayMetadataNamespace || respMeta["key"] != aiGatewayTotalTokenKey() {
		t.Errorf("cost.response.metadata = %+v, want namespace=%s key=%s", respMeta, aiGatewayMetadataNamespace, aiGatewayTotalTokenKey())
	}

	// The route carries llmRequestCosts (TotalToken) so the limit is token-denominated.
	route := getUnstructured(t, c, aiGatewayRouteGVK(), "budget-router")
	costs, found, _ := unstructured.NestedSlice(route.Object, "spec", "llmRequestCosts")
	if !found || len(costs) == 0 {
		t.Fatalf("route missing spec.llmRequestCosts")
	}
	hasTotalToken := false
	for _, cst := range costs {
		m := cst.(map[string]interface{})
		if m["type"] == "TotalToken" && m["metadataKey"] == aiGatewayTotalTokenKey() {
			hasTotalToken = true
		}
	}
	if !hasTotalToken {
		t.Errorf("route llmRequestCosts missing a TotalToken entry keyed on %s, got %+v", aiGatewayTotalTokenKey(), costs)
	}

	// status: ready, and the condition message mentions the compiled budget.
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "budget-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("GatewayReady not True, got %+v", cond)
	}
}

// TestModelRouterGateway_TeamBudgetKeysOnHeader covers 2b case (b): a team-scope
// budget compiles a rateLimit rule whose clientSelector keys on the request
// header (default x-llmkube-team), and a custom HeaderKey is honored.
func TestModelRouterGateway_TeamBudgetKeysOnHeader(t *testing.T) {
	tests := []struct {
		name       string
		headerKey  string
		wantHeader string
	}{
		{name: "default header", headerKey: "", wantHeader: "x-llmkube-team"},
		{name: "custom header", headerKey: "x-org-id", wantHeader: "x-org-id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cfg, stop := startGatewayTestEnv(t, true)
			defer stop()

			makeBackendISvc(t, c, "team-cuda")

			maxTokens := int64(1000000)
			mr := &inferencev1alpha1.ModelRouter{
				ObjectMeta: metav1.ObjectMeta{Name: "team-router", Namespace: testNS},
				Spec: inferencev1alpha1.ModelRouterSpec{
					DataPlane:  inferencev1alpha1.ModelRouterDataPlaneGateway,
					GatewayRef: &inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
					Backends: []inferencev1alpha1.RouterBackend{
						{Name: "team-cuda", InferenceServiceRef: corev1LocalRef("team-cuda")},
					},
					Rules: []inferencev1alpha1.RouterRule{
						{
							Name:  "team",
							Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
							Route: inferencev1alpha1.RuleRoute{Backends: []string{"team-cuda"}},
						},
					},
					Policy: &inferencev1alpha1.RouterPolicy{
						Budgets: []inferencev1alpha1.BudgetSpec{
							{
								Name:          "per-team",
								Scope:         "team",
								HeaderKey:     tt.headerKey,
								WindowSeconds: 60,
								MaxTokens:     &maxTokens,
							},
						},
					},
				},
			}
			if err := c.Create(context.Background(), mr); err != nil {
				t.Fatalf("create modelrouter: %v", err)
			}

			r := newModelRouterGatewayReconciler(t, cfg)
			reconcileRouter(t, r, mr)

			btp := getUnstructured(t, c, btpGVK(), "team-router")
			rlRules, found, _ := unstructured.NestedSlice(btp.Object, "spec", "rateLimit", "global", "rules")
			if !found || len(rlRules) != 1 {
				t.Fatalf("team rateLimit.global.rules = %v, want 1", rlRules)
			}
			rule0 := rlRules[0].(map[string]interface{})
			sels, ok := rule0["clientSelectors"].([]interface{})
			if !ok || len(sels) != 1 {
				t.Fatalf("team rule clientSelectors = %v, want 1", rule0["clientSelectors"])
			}
			headers := sels[0].(map[string]interface{})["headers"].([]interface{})
			h0 := headers[0].(map[string]interface{})
			if h0["name"] != tt.wantHeader {
				t.Errorf("team selector header name = %v, want %s", h0["name"], tt.wantHeader)
			}
			if h0["type"] != "Distinct" {
				t.Errorf("team selector header type = %v, want Distinct (independent bucket per value)", h0["type"])
			}
			if unit, _ := rule0["limit"].(map[string]interface{})["unit"].(string); unit != "Minute" {
				t.Errorf("team rateLimit unit = %q, want Minute (60s)", unit)
			}
		})
	}
}

// TestModelRouterGateway_DollarBudgetFailsLoud covers 2b case (c): a MaxUSD
// budget sets GatewayReady=False with reason UnsupportedBudgetField and
// generates NOTHING (no partial route/BTP).
func TestModelRouterGateway_DollarBudgetFailsLoud(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "usd-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "usd-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane:  inferencev1alpha1.ModelRouterDataPlaneGateway,
			GatewayRef: &inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "usd-cuda", InferenceServiceRef: corev1LocalRef("usd-cuda")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "qwen",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{Backends: []string{"usd-cuda"}},
				},
			},
			Policy: &inferencev1alpha1.RouterPolicy{
				Budgets: []inferencev1alpha1.BudgetSpec{
					{Name: "dollar-cap", Scope: "router", WindowSeconds: 3600, MaxUSD: "100.00"},
				},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	assertNotExists(t, c, backendGVK(), "usd-cuda")
	assertNotExists(t, c, aiServiceBackendGVK(), "usd-cuda")
	assertNotExists(t, c, aiGatewayRouteGVK(), "usd-router")
	assertNotExists(t, c, btpGVK(), "usd-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "usd-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterGatewayReasonUnsupportedBudgetField {
		t.Errorf("expected GatewayReady=False/%s, got %+v", modelRouterGatewayReasonUnsupportedBudgetField, cond)
	}
	if cond != nil && !contains(cond.Message, "dollar-cap") {
		t.Errorf("condition message should name the offending budget, got %q", cond.Message)
	}
}

// TestModelRouterGateway_RuleScopeBudgetFailsLoud covers 2b case (d): a
// rule-scope budget sets GatewayReady=False with reason UnsupportedBudgetScope
// and generates NOTHING.
func TestModelRouterGateway_RuleScopeBudgetFailsLoud(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "rule-cuda")

	maxTokens := int64(1000)
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "rulescope-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane:  inferencev1alpha1.ModelRouterDataPlaneGateway,
			GatewayRef: &inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "rule-cuda", InferenceServiceRef: corev1LocalRef("rule-cuda")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "qwen",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{Backends: []string{"rule-cuda"}},
				},
			},
			Policy: &inferencev1alpha1.RouterPolicy{
				Budgets: []inferencev1alpha1.BudgetSpec{
					{Name: "rule-cap", Scope: "rule", RuleName: "qwen", WindowSeconds: 3600, MaxTokens: &maxTokens},
				},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	assertNotExists(t, c, aiGatewayRouteGVK(), "rulescope-router")
	assertNotExists(t, c, btpGVK(), "rulescope-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "rulescope-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterGatewayReasonUnsupportedBudgetScope {
		t.Errorf("expected GatewayReady=False/%s, got %+v", modelRouterGatewayReasonUnsupportedBudgetScope, cond)
	}
}

// TestModelRouterGateway_NoBudgetsUnchangedFromSliceA covers 2b case (e): a
// ModelRouter with NO budgets produces the exact 2a BTP (no rateLimit key) and
// route (no llmRequestCosts key), guarding the #693 non-regression contract.
func TestModelRouterGateway_NoBudgetsUnchangedFromSliceA(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "plain-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "plain-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane:  inferencev1alpha1.ModelRouterDataPlaneGateway,
			GatewayRef: &inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "plain-cuda", InferenceServiceRef: corev1LocalRef("plain-cuda")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "qwen",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{Backends: []string{"plain-cuda"}},
				},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	// No budgets -> BTP carries no rateLimit key (byte-identical to 2a).
	btp := getUnstructured(t, c, btpGVK(), "plain-router")
	if _, found, _ := unstructured.NestedMap(btp.Object, "spec", "rateLimit"); found {
		t.Error("no-budget BTP must NOT carry spec.rateLimit (2a non-regression)")
	}
	// No budgets -> route carries no llmRequestCosts key.
	route := getUnstructured(t, c, aiGatewayRouteGVK(), "plain-router")
	if _, found, _ := unstructured.NestedSlice(route.Object, "spec", "llmRequestCosts"); found {
		t.Error("no-budget route must NOT carry spec.llmRequestCosts (2a non-regression)")
	}
}

// contains is a tiny substring helper for condition-message assertions.
func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// TestUnsupportedMatchMessage_GlobModel pins the fail-loud boundary for glob
// model patterns: Proxy mode matches "qwen3-*" via path.Match, but the gateway
// data plane can only do an Exact x-ai-eg-model header match, so a glob must be
// rejected loudly rather than compiled to a literal that never fires. A literal
// model and headers must still compile (empty message).
func TestUnsupportedMatchMessage_GlobModel(t *testing.T) {
	tests := []struct {
		name       string
		match      *inferencev1alpha1.RuleMatch
		wantReject bool
	}{
		{
			name:       "literal model compiles",
			match:      &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
			wantReject: false,
		},
		{
			name:       "header-only compiles",
			match:      &inferencev1alpha1.RuleMatch{Headers: map[string]string{"x-team": "a"}},
			wantReject: false,
		},
		{
			name:       "star glob rejected",
			match:      &inferencev1alpha1.RuleMatch{Models: []string{"qwen3-*"}},
			wantReject: true,
		},
		{
			name:       "question glob rejected",
			match:      &inferencev1alpha1.RuleMatch{Models: []string{"gpt-?"}},
			wantReject: true,
		},
		{
			name:       "bracket glob rejected",
			match:      &inferencev1alpha1.RuleMatch{Models: []string{"llama[0-9]"}},
			wantReject: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := &inferencev1alpha1.ModelRouter{
				Spec: inferencev1alpha1.ModelRouterSpec{
					Rules: []inferencev1alpha1.RouterRule{{Name: "r0", Match: tt.match}},
				},
			}
			msg := unsupportedMatchMessage(mr)
			if tt.wantReject && msg == "" {
				t.Errorf("expected rule rejected, got empty message")
			}
			if !tt.wantReject && msg != "" {
				t.Errorf("expected rule compiled, got rejection: %s", msg)
			}
		})
	}
}

// TestCompileBudgetRule_WindowScaling pins the window-to-unit scaling: Envoy
// expresses a limit as "N per ONE unit", so MaxTokens (the cap over
// WindowSeconds) must be scaled to the chosen unit. Exact-unit windows keep
// requests == MaxTokens; non-exact windows scale so the enforced average rate
// stays faithful (a raw MaxTokens with a sub-window unit silently under-enforces).
func TestCompileBudgetRule_WindowScaling(t *testing.T) {
	maxTok := int64(5_000_000)
	tests := []struct {
		window   int32
		wantUnit string
		wantReq  int64
	}{
		{3600, "Hour", 5_000_000},  // exact: 1 hour
		{60, "Minute", 5_000_000},  // exact: 1 minute
		{86400, "Day", 5_000_000},  // exact: 1 day
		{120, "Minute", 2_500_000}, // 5M*60/120
		{7200, "Hour", 2_500_000},  // 5M*3600/7200
		{90, "Minute", 3_333_333},  // 5M*60/90 floored
		{3601, "Hour", 4_998_611},  // 5M*3600/3601 floored (was ~3600x too loose)
		{30, "Second", 166_666},    // 5M/30 floored
	}
	for _, tt := range tests {
		b := inferencev1alpha1.BudgetSpec{Scope: "router", WindowSeconds: tt.window, MaxTokens: &maxTok}
		limit := compileBudgetRule(b)["limit"].(map[string]interface{})
		if limit["unit"] != tt.wantUnit {
			t.Errorf("window %ds: unit = %v, want %s", tt.window, limit["unit"], tt.wantUnit)
		}
		if got, _ := limit["requests"].(int64); got != tt.wantReq {
			t.Errorf("window %ds: requests = %d, want %d", tt.window, got, tt.wantReq)
		}
	}
}
