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
	"path/filepath"
	"testing"

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
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

type schemaGVK = schema.GroupVersionKind

// These tests are plain Go tests (not the Ginkgo suite) because they need
// precise control over whether the Envoy AI Gateway CRDs are present: the
// happy-path cases want them registered, while the CRDs-absent no-op case wants
// them deliberately missing. Each case spins up its own envtest with the
// appropriate CRDDirectoryPaths so the two worlds never bleed together.

const (
	baseCRDPath    = "../../config/crd/bases"
	aigwTestCRDDir = "../../test/crd/aigateway"
)

// startGatewayTestEnv boots an envtest with the LLMKube base CRDs and,
// optionally, the aigw test-stub CRDs. It returns a client plus a stop func.
func startGatewayTestEnv(t *testing.T, withGatewayCRDs bool) (client.Client, *rest.Config, func()) {
	t.Helper()

	crdPaths := []string{filepath.Join(baseCRDPath)}
	if withGatewayCRDs {
		crdPaths = append(crdPaths, filepath.Join(aigwTestCRDDir))
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     crdPaths,
		ErrorIfCRDPathMissing: true,
	}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}

	s := scheme.Scheme
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		_ = env.Stop()
		t.Fatalf("new client: %v", err)
	}

	return c, cfg, func() { _ = env.Stop() }
}

// newGatewayReconciler builds a reconciler backed by a client whose RESTMapper
// is dynamic, so the CRD-presence gate reflects the env it runs against.
func newGatewayReconciler(t *testing.T, cfg *rest.Config) *InferenceServiceGatewayReconciler {
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
	return &InferenceServiceGatewayReconciler{Client: c, Scheme: scheme.Scheme}
}

const testNS = "default"

func makeInferenceService(name string, gw *inferencev1alpha1.GatewaySpec) *inferencev1alpha1.InferenceService {
	endpoint := &inferencev1alpha1.EndpointSpec{Port: 8080}
	if gw != nil {
		endpoint.Gateway = gw
	}
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "qwen36-35b",
			Endpoint: endpoint,
		},
	}
}

func reconcileISvc(t *testing.T, r *InferenceServiceGatewayReconciler, isvc *inferencev1alpha1.InferenceService) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
}

func getUnstructured(t *testing.T, c client.Client, gvk schemaGVK, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: testNS}, u)
	if err != nil {
		t.Fatalf("get %s/%s: %v", gvk.Kind, name, err)
	}
	return u
}

// TestGatewayReconcile_OptedInGeneratesResources covers case (a): an opted-in
// InferenceService produces all three resources with correct owner refs, the
// route rule matches the resolved model name, and the Backend targets the
// InferenceService's Service.
func TestGatewayReconcile_OptedInGeneratesResources(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	isvc := makeInferenceService("qwen-isvc", &inferencev1alpha1.GatewaySpec{
		Enabled: true,
		GatewayRef: inferencev1alpha1.GatewayReference{
			Name:      "ai-gateway",
			Namespace: "ai-gateway",
		},
	})
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create isvc: %v", err)
	}

	r := newGatewayReconciler(t, cfg)
	reconcileISvc(t, r, isvc)

	name := "qwen-isvc"

	// Backend targets the InferenceService Service fqdn + port.
	backend := getUnstructured(t, c, backendGVK(), name)
	if host := backendHostname(t, backend); host != "qwen-isvc.default.svc.cluster.local" {
		t.Errorf("backend hostname = %q, want qwen-isvc.default.svc.cluster.local", host)
	}
	assertOwnedBy(t, backend, isvc)

	// AIServiceBackend wraps the Backend with the OpenAI schema.
	asb := getUnstructured(t, c, aiServiceBackendGVK(), name)
	schemaName, _, _ := unstructured.NestedString(asb.Object, "spec", "schema", "name")
	if schemaName != "OpenAI" {
		t.Errorf("aiservicebackend schema.name = %q, want OpenAI", schemaName)
	}
	asbBackend, _, _ := unstructured.NestedString(asb.Object, "spec", "backendRef", "name")
	if asbBackend != name {
		t.Errorf("aiservicebackend backendRef.name = %q, want %q", asbBackend, name)
	}
	assertOwnedBy(t, asb, isvc)

	// AIGatewayRoute matches the resolved model name (ModelRef = qwen36-35b).
	route := getUnstructured(t, c, aiGatewayRouteGVK(), name)
	gotModel := routeMatchValue(t, route)
	if gotModel != "qwen36-35b" {
		t.Errorf("route header match value = %q, want qwen36-35b", gotModel)
	}
	assertOwnedBy(t, route, isvc)

	// status.gateway reflects the exposure.
	fresh := &inferencev1alpha1.InferenceService{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get isvc status: %v", err)
	}
	if fresh.Status.Gateway == nil || !fresh.Status.Gateway.RouteReady {
		t.Errorf("status.gateway.routeReady not set, got %+v", fresh.Status.Gateway)
	}
	if fresh.Status.Gateway != nil && fresh.Status.Gateway.ModelName != "qwen36-35b" {
		t.Errorf("status.gateway.modelName = %q, want qwen36-35b", fresh.Status.Gateway.ModelName)
	}
	if cond := apimeta.FindStatusCondition(fresh.Status.Conditions, GatewayConditionReady); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("GatewayReady condition not True, got %+v", cond)
	}
}

// TestGatewayReconcile_OwnerRefsEnableGC covers case (b): the generated
// resources carry a controller owner reference to the InferenceService, so
// Kubernetes garbage collection removes them on InferenceService delete. (In
// envtest the GC controller does not run, so we assert the owner refs that GC
// keys on rather than the cascade itself.)
func TestGatewayReconcile_OwnerRefsEnableGC(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	isvc := makeInferenceService("gc-isvc", &inferencev1alpha1.GatewaySpec{
		Enabled:    true,
		GatewayRef: inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
	})
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create isvc: %v", err)
	}

	r := newGatewayReconciler(t, cfg)
	reconcileISvc(t, r, isvc)

	for _, gvk := range []schemaGVK{backendGVK(), aiServiceBackendGVK(), aiGatewayRouteGVK()} {
		u := getUnstructured(t, c, gvk, "gc-isvc")
		assertOwnedBy(t, u, isvc)
	}
}

// TestGatewayReconcile_CRDsAbsentIsCleanNoOp covers case (c): with the aigw
// CRDs not installed, reconcile does not error/crash, creates nothing, and sets
// the disabled GatewayReady condition.
func TestGatewayReconcile_CRDsAbsentIsCleanNoOp(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, false)
	defer stop()

	isvc := makeInferenceService("absent-isvc", &inferencev1alpha1.GatewaySpec{
		Enabled:    true,
		GatewayRef: inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
	})
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create isvc: %v", err)
	}

	r := newGatewayReconciler(t, cfg)
	// Must not error or panic.
	reconcileISvc(t, r, isvc)

	fresh := &inferencev1alpha1.InferenceService{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "absent-isvc", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get isvc: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, GatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != gatewayReasonCRDsMissing {
		t.Errorf("expected GatewayReady=False/%s, got %+v", gatewayReasonCRDsMissing, cond)
	}
	if fresh.Status.Gateway != nil && fresh.Status.Gateway.RouteReady {
		t.Errorf("status.gateway.routeReady should be false when CRDs absent")
	}
}

// TestGatewayReconcile_NotEnabledProducesNothing covers case (d): an
// InferenceService that did not opt in (no gateway block, or enabled=false)
// produces no resources and no gateway status.
func TestGatewayReconcile_NotEnabledProducesNothing(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	// Sub-case 1: no gateway block at all.
	noGW := makeInferenceService("no-gw-isvc", nil)
	if err := c.Create(context.Background(), noGW); err != nil {
		t.Fatalf("create no-gw isvc: %v", err)
	}
	// Sub-case 2: gateway block present but enabled=false.
	disabled := makeInferenceService("disabled-isvc", &inferencev1alpha1.GatewaySpec{
		Enabled:    false,
		GatewayRef: inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
	})
	if err := c.Create(context.Background(), disabled); err != nil {
		t.Fatalf("create disabled isvc: %v", err)
	}

	r := newGatewayReconciler(t, cfg)
	reconcileISvc(t, r, noGW)
	reconcileISvc(t, r, disabled)

	for _, name := range []string{"no-gw-isvc", "disabled-isvc"} {
		for _, gvk := range []schemaGVK{backendGVK(), aiServiceBackendGVK(), aiGatewayRouteGVK()} {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(gvk)
			err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: testNS}, u)
			if !apierrors.IsNotFound(err) {
				t.Errorf("expected %s/%s to not exist, get err = %v", gvk.Kind, name, err)
			}
		}
		fresh := &inferencev1alpha1.InferenceService{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: testNS}, fresh); err != nil {
			t.Fatalf("get isvc %s: %v", name, err)
		}
		if fresh.Status.Gateway != nil {
			t.Errorf("%s: expected nil status.gateway, got %+v", name, fresh.Status.Gateway)
		}
	}
}

// backendHostname extracts spec.endpoints[0].fqdn.hostname from a Backend.
// unstructured.NestedString cannot index into slices, so we walk manually.
func backendHostname(t *testing.T, backend *unstructured.Unstructured) string {
	t.Helper()
	endpoints, found, err := unstructured.NestedSlice(backend.Object, "spec", "endpoints")
	if err != nil || !found || len(endpoints) == 0 {
		t.Fatalf("backend has no spec.endpoints (found=%v err=%v)", found, err)
	}
	ep, ok := endpoints[0].(map[string]interface{})
	if !ok {
		t.Fatalf("backend endpoint[0] is not a map: %T", endpoints[0])
	}
	host, _, _ := unstructured.NestedString(ep, "fqdn", "hostname")
	return host
}

// routeMatchValue extracts the x-ai-eg-model header match value from the first
// rule of an AIGatewayRoute.
func routeMatchValue(t *testing.T, route *unstructured.Unstructured) string {
	t.Helper()
	rules, found, err := unstructured.NestedSlice(route.Object, "spec", "rules")
	if err != nil || !found || len(rules) == 0 {
		t.Fatalf("route has no spec.rules (found=%v err=%v)", found, err)
	}
	rule := rules[0].(map[string]interface{})
	matches := rule["matches"].([]interface{})
	headers := matches[0].(map[string]interface{})["headers"].([]interface{})
	header := headers[0].(map[string]interface{})
	if header["name"] != aiGatewayModelHeader {
		t.Fatalf("route header name = %v, want %s", header["name"], aiGatewayModelHeader)
	}
	val, _ := header["value"].(string)
	return val
}

// assertOwnedBy verifies obj carries a controller owner reference to isvc.
func assertOwnedBy(t *testing.T, obj *unstructured.Unstructured, isvc *inferencev1alpha1.InferenceService) {
	t.Helper()
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "InferenceService" && ref.Name == isvc.Name {
			if ref.Controller == nil || !*ref.Controller {
				t.Errorf("%s/%s owner ref to %s is not a controller ref", obj.GetKind(), obj.GetName(), isvc.Name)
			}
			return
		}
	}
	t.Errorf("%s/%s missing owner reference to InferenceService %s", obj.GetKind(), obj.GetName(), isvc.Name)
}
