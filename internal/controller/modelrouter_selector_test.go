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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

// These tests are the #2120 reproduction and base parity. They drive the
// reconciler from unstructured manifests so the same file compiles against
// the branch and against base, where ModelRouterSpec has no
// backendSelector field: base cannot even store the selector, which is what
// the reproduction asserts.

// applySelectorManifest creates the object a YAML manifest describes in ns
// and returns it unstructured, so a test can read a field the compiled branch
// type may not have (backendSelector) and mutate labels the way a user would.
func applySelectorManifest(t *testing.T, c client.Client, ns, manifest string) *unstructured.Unstructured {
	t.Helper()
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &fields); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	u := &unstructured.Unstructured{Object: fields}
	u.SetNamespace(ns)
	gvk := schema.FromAPIVersionAndKind(u.GetAPIVersion(), u.GetKind())
	u.SetGroupVersionKind(gvk)
	if err := c.Create(context.Background(), u); err != nil {
		t.Fatalf("create %s %s: %v", gvk.Kind, u.GetName(), err)
	}
	return u
}

// inferenceServiceManifest is a minimal InferenceService. The label is added by
// the caller, so one helper covers selected and unselected services.
func inferenceServiceManifest(name string, labels map[string]string) string {
	meta := "  name: " + name
	if len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		meta += "\n  labels:"
		for _, k := range keys {
			meta += "\n    " + k + ": " + labels[k]
		}
	}
	return `apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
` + meta + `
spec:
  modelRef: ` + name + `
  endpoint:
    port: 8080
`
}

func selectorReconciler(c client.Client) *ModelRouterReconciler {
	return &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}
}

func reconcileSelectorRouter(t *testing.T, r *ModelRouterReconciler, ns, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile %s/%s: %v", ns, name, err)
	}
}

// routerConfigJSON reads the compiled proxy config the reconciler wrote.
func routerConfigJSON(t *testing.T, c client.Client, ns, mrName string) map[string]any {
	t.Helper()
	cm := &corev1.ConfigMap{}
	name := routerProxyResourceName(mrName)
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, cm); err != nil {
		t.Fatalf("router-proxy ConfigMap %s/%s not found: %v", ns, name, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(cm.Data[routerProxyConfigKey]), &cfg); err != nil {
		t.Fatalf("decode %s: %v", routerProxyConfigKey, err)
	}
	return cfg
}

// configBackends returns the compiled backend objects in config order.
func configBackends(t *testing.T, cfg map[string]any) []map[string]any {
	t.Helper()
	raw, ok := cfg["backends"].([]any)
	if !ok {
		t.Fatalf("compiled config has no backends list: %v", cfg)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, b := range raw {
		m, ok := b.(map[string]any)
		if !ok {
			t.Fatalf("backend entry is not an object: %v", b)
		}
		out = append(out, m)
	}
	return out
}

func configBackendNames(t *testing.T, cfg map[string]any) []string {
	t.Helper()
	backends := configBackends(t, cfg)
	names := make([]string, len(backends))
	for i, b := range backends {
		names[i], _ = b["name"].(string)
	}
	return names
}

func configBackend(t *testing.T, cfg map[string]any, name string) map[string]any {
	t.Helper()
	for _, b := range configBackends(t, cfg) {
		if b["name"] == name {
			return b
		}
	}
	t.Fatalf("compiled config has no backend %q; got %v", name, configBackendNames(t, cfg))
	return nil
}

// modelIDsFromProxy serves GET /v1/models from the compiled config, so the
// assertion covers the id a client actually receives.
func modelIDsFromProxy(t *testing.T, cfg map[string]any) []string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("re-encode compiled config: %v", err)
	}
	var rc router.Config
	if err := json.Unmarshal(raw, &rc); err != nil {
		t.Fatalf("decode compiled config into router.Config: %v", err)
	}
	proxy := router.NewProxy(&rc, slog.Default())
	mux := http.NewServeMux()
	proxy.Mount(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	ids := make([]string, len(body.Data))
	for i, m := range body.Data {
		ids[i] = m.ID
	}
	return ids
}

func assertBackendNames(t *testing.T, cfg map[string]any, want ...string) {
	t.Helper()
	if got := configBackendNames(t, cfg); !slices.Equal(got, want) {
		t.Errorf("compiled backend names = %v, want %v", got, want)
	}
}

// routerStatusBackendNames reads the names the reconciler reports.
func routerStatusBackendNames(t *testing.T, c client.Client, ns, name string) []string {
	t.Helper()
	mr := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, mr); err != nil {
		t.Fatalf("get ModelRouter %s/%s: %v", ns, name, err)
	}
	names := make([]string, len(mr.Status.Backends))
	for i, b := range mr.Status.Backends {
		names[i] = b.Name
	}
	return names
}

// TestModelRouterSelectorOnlyManifest is the feature's core case: a router
// with a label selector and no backends entry is accepted and compiles every
// labelled InferenceService. Base rejects the manifest outright, so the
// selector-only shape cannot exist there.
func TestModelRouterSelectorOnlyManifest(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	selected := map[string]string{"llmkube.dev/router": "models"}
	applySelectorManifest(t, c, ns, inferenceServiceManifest("glm-5-3-flash", selected))
	applySelectorManifest(t, c, ns, inferenceServiceManifest("qwen2-5-0-5b", selected))
	applySelectorManifest(t, c, ns, inferenceServiceManifest("smollm2-135m", nil))

	applySelectorManifest(t, c, ns, `apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: models
spec:
  defaultRouteStrategy: BackendNameMatch
  backendSelector:
    matchLabels:
      llmkube.dev/router: models
`)
	r := selectorReconciler(c)
	reconcileSelectorRouter(t, r, ns, "models")

	cfg := routerConfigJSON(t, c, ns, "models")
	assertBackendNames(t, cfg, "glm-5-3-flash", "qwen2-5-0-5b")
	for _, name := range []string{"glm-5-3-flash", "qwen2-5-0-5b"} {
		b := configBackend(t, cfg, name)
		if b["tier"] != "local" {
			t.Errorf("backend %q tier = %v, want local", name, b["tier"])
		}
		if b["inferenceService"] != name {
			t.Errorf("backend %q inferenceService = %v, want %q", name, b["inferenceService"], name)
		}
	}
	if got := modelIDsFromProxy(t, cfg); !slices.Equal(got, []string{"glm-5-3-flash", "qwen2-5-0-5b"}) {
		t.Errorf("GET /v1/models = %v, want the two labelled services and not smollm2-135m", got)
	}
	if got := routerStatusBackendNames(t, c, ns, "models"); !slices.Equal(got, []string{"glm-5-3-flash", "qwen2-5-0-5b"}) {
		t.Errorf("status.backends = %v, want the two labelled services", got)
	}
}

// TestModelRouterSelectorAddsLabeledServices is the issue's own manifest: a
// selector plus one ref-less override entry. On base the selector is pruned
// from the stored spec and the override fails static validation, so the
// labelled service qwen2-5-0-5b never reaches the router (the reported
// symptom).
func TestModelRouterSelectorAddsLabeledServices(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	selected := map[string]string{"llmkube.dev/router": "models"}
	applySelectorManifest(t, c, ns, inferenceServiceManifest("glm-5-3-flash", selected))
	applySelectorManifest(t, c, ns, inferenceServiceManifest("qwen2-5-0-5b", selected))
	applySelectorManifest(t, c, ns, inferenceServiceManifest("smollm2-135m", nil))

	applySelectorManifest(t, c, ns, `apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: models
spec:
  defaultRouteStrategy: BackendNameMatch
  backendSelector:
    matchLabels:
      llmkube.dev/router: models
  backends:
    - name: glm-5-3-flash
      timeout: 600s
`)
	r := selectorReconciler(c)
	reconcileSelectorRouter(t, r, ns, "models")

	cfg := routerConfigJSON(t, c, ns, "models")
	assertBackendNames(t, cfg, "glm-5-3-flash", "qwen2-5-0-5b")
	glm := configBackend(t, cfg, "glm-5-3-flash")
	if glm["timeout"] != float64(600_000_000_000) {
		t.Errorf("glm-5-3-flash timeout = %v, want the override's 600s", glm["timeout"])
	}
	qwen := configBackend(t, cfg, "qwen2-5-0-5b")
	if qwen["inferenceService"] != "qwen2-5-0-5b" {
		t.Errorf("qwen2-5-0-5b inferenceService = %v, want the service it was selected from", qwen["inferenceService"])
	}
	if got := modelIDsFromProxy(t, cfg); !slices.Equal(got, []string{"glm-5-3-flash", "qwen2-5-0-5b"}) {
		t.Errorf("GET /v1/models = %v, want the two labelled services", got)
	}
}

// TestModelRouterSelectorRelabelDropsBackend pins that a label removal, a
// service deletion, and a service creation each re-render a selector router,
// which means every InferenceService event in the namespace must enqueue it.
func TestModelRouterSelectorRelabelDropsBackend(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	selected := map[string]string{"llmkube.dev/router": "models"}
	glm := applySelectorManifest(t, c, ns, inferenceServiceManifest("glm-5-3-flash", selected))
	qwen := applySelectorManifest(t, c, ns, inferenceServiceManifest("qwen2-5-0-5b", selected))
	applySelectorManifest(t, c, ns, inferenceServiceManifest("smollm2-135m", selected))

	applySelectorManifest(t, c, ns, `apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: models
spec:
  backendSelector:
    matchLabels:
      llmkube.dev/router: models
`)
	r := selectorReconciler(c)
	reconcileSelectorRouter(t, r, ns, "models")
	assertBackendNames(t, routerConfigJSON(t, c, ns, "models"), "glm-5-3-flash", "qwen2-5-0-5b", "smollm2-135m")

	// A label removal cannot be seen from the new object alone, so the mapper
	// must enqueue every selector router in the namespace.
	glm.SetLabels(nil)
	if err := c.Update(context.Background(), glm); err != nil {
		t.Fatalf("remove label from glm-5-3-flash: %v", err)
	}
	assertRouterEnqueuedForInferenceService(t, r, ns, "glm-5-3-flash", "models")

	reconcileSelectorRouter(t, r, ns, "models")
	cfg := routerConfigJSON(t, c, ns, "models")
	assertBackendNames(t, cfg, "qwen2-5-0-5b", "smollm2-135m")
	if got := routerStatusBackendNames(t, c, ns, "models"); !slices.Equal(got, []string{"qwen2-5-0-5b", "smollm2-135m"}) {
		t.Errorf("status.backends after relabel = %v, want the relabelled service gone", got)
	}

	// Deleting a selected service drops its backend the same way.
	if err := c.Delete(context.Background(), qwen); err != nil {
		t.Fatalf("delete qwen2-5-0-5b: %v", err)
	}
	assertRouterEnqueuedForInferenceService(t, r, ns, "qwen2-5-0-5b", "models")
	reconcileSelectorRouter(t, r, ns, "models")
	assertBackendNames(t, routerConfigJSON(t, c, ns, "models"), "smollm2-135m")

	// A new labelled service joins without an edit to the router.
	applySelectorManifest(t, c, ns, inferenceServiceManifest("phi-4-mini", selected))
	reconcileSelectorRouter(t, r, ns, "models")
	assertBackendNames(t, routerConfigJSON(t, c, ns, "models"), "phi-4-mini", "smollm2-135m")
}

// assertRouterEnqueuedForInferenceService checks the watch mapper returns the
// router for an InferenceService event in ns.
func assertRouterEnqueuedForInferenceService(
	t *testing.T,
	r *ModelRouterReconciler,
	ns, isvcName, mrName string,
) {
	t.Helper()
	requests := r.findModelRoutersForInferenceService(context.Background(),
		&inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: ns}})
	for _, req := range requests {
		if req.Name == mrName && req.Namespace == ns {
			return
		}
	}
	t.Fatalf("an InferenceService event in %s did not enqueue selector router %q: got %v", ns, mrName, requests)
}

// TestModelRouterSelectorHidesStoppedService pins the /v1/models behavior the
// issue asks for: a service at replicas 0 or suspended stays compiled (it is
// still dispatchable and rules can still resolve it) but is left out of model
// discovery, so client model pickers do not offer it.
func TestModelRouterSelectorHidesStoppedService(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	selected := map[string]string{"llmkube.dev/router": "models"}
	stopped := applySelectorManifest(t, c, ns, inferenceServiceManifest("glm-5-3-flash", selected))
	other := applySelectorManifest(t, c, ns, inferenceServiceManifest("qwen2-5-0-5b", selected))
	applySelectorManifest(t, c, ns, inferenceServiceManifest("smollm2-135m", selected))

	setSpecField(t, c, stopped, "replicas", int64(0))
	setSpecField(t, c, other, "suspend", true)

	applySelectorManifest(t, c, ns, `apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: models
spec:
  backendSelector:
    matchLabels:
      llmkube.dev/router: models
`)
	r := selectorReconciler(c)
	reconcileSelectorRouter(t, r, ns, "models")

	cfg := routerConfigJSON(t, c, ns, "models")
	assertBackendNames(t, cfg, "glm-5-3-flash", "qwen2-5-0-5b", "smollm2-135m")
	for _, name := range []string{"glm-5-3-flash", "qwen2-5-0-5b"} {
		b := configBackend(t, cfg, name)
		if b["hidden"] != true {
			t.Errorf("backend %q hidden = %v, want true for a stopped service", name, b["hidden"])
		}
	}
	if _, present := configBackend(t, cfg, "smollm2-135m")["hidden"]; present {
		t.Errorf("running backend smollm2-135m carries a hidden flag")
	}
	if got := modelIDsFromProxy(t, cfg); !slices.Equal(got, []string{"smollm2-135m"}) {
		t.Errorf("GET /v1/models = %v, want only the running service", got)
	}
}

// setSpecField patches one spec field on an unstructured object the way a user
// would (kubectl scale / kubectl edit), leaving the rest of the object alone.
func setSpecField(t *testing.T, c client.Client, u *unstructured.Unstructured, field string, value any) {
	t.Helper()
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(u.GroupVersionKind())
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(u), fresh); err != nil {
		t.Fatalf("get %s %s: %v", u.GetKind(), u.GetName(), err)
	}
	if err := unstructured.SetNestedField(fresh.Object, value, "spec", field); err != nil {
		t.Fatalf("set spec.%s: %v", field, err)
	}
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("update %s %s: %v", u.GetKind(), u.GetName(), err)
	}
}

// TestModelRouterExplicitRouterConfigUnchanged is the parity test for every
// router that does not set a selector: the compiled config, status, activation
// RBAC and deployment must stay exactly what base produces.
func TestModelRouterExplicitRouterConfigUnchanged(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	applySelectorManifest(t, c, ns, inferenceServiceManifest("local-a", nil))
	applySelectorManifest(t, c, ns, `apiVersion: v1
kind: Secret
metadata:
  name: cloud-creds
stringData:
  ANTHROPIC_API_KEY: test-key
`)
	applySelectorManifest(t, c, ns, `apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: explicit
spec:
  backends:
    - name: local-a
      inferenceServiceRef:
        name: local-a
      resolution: endpoint
    - name: cloud-b
      tier: cloud
      external:
        provider: anthropic
        model: claude-opus-4-7
        url: https://api.anthropic.com
        credentialsSecretRef:
          name: cloud-creds
  rules:
    - name: all
      route:
        backends: [local-a]
    - name: complex
      match:
        taskComplexity: complex
      route:
        backends: [cloud-b]
  defaultRoute: local-a
`)
	r := selectorReconciler(c)
	reconcileSelectorRouter(t, r, ns, "explicit")

	cfg := routerConfigJSON(t, c, ns, "explicit")
	assertBackendNames(t, cfg, "local-a", "cloud-b")
	for _, name := range []string{"local-a", "cloud-b"} {
		b := configBackend(t, cfg, name)
		if _, present := b["hidden"]; present {
			t.Errorf("explicit backend %q carries a hidden key: %v", name, b)
		}
	}
	if cfg["defaultRoute"] != "local-a" {
		t.Errorf("defaultRoute = %v, want local-a", cfg["defaultRoute"])
	}
	if got := cfg["defaultRouteStrategy"]; got != "Static" {
		t.Errorf("defaultRouteStrategy = %v, want Static (the API server defaults it)", got)
	}
	rules, _ := cfg["rules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("compiled %d rules, want 2: %v", len(rules), cfg["rules"])
	}
	if got := configBackend(t, cfg, "local-a")["endpointSource"]; got == nil {
		t.Errorf("endpoint-resolution backend lost its endpointSource: %v", configBackend(t, cfg, "local-a"))
	}
	if got := configBackend(t, cfg, "cloud-b")["tier"]; got != "cloud" {
		t.Errorf("cloud-b tier = %v, want cloud", got)
	}
	if got := routerStatusBackendNames(t, c, ns, "explicit"); !slices.Equal(got, []string{"local-a", "cloud-b"}) {
		t.Errorf("status.backends = %v, want spec order", got)
	}

	// Endpoint resolution needs the activation ServiceAccount and RBAC, and
	// the Deployment must run under it; that path now reads the resolved
	// backend list, so the parity test pins its output for a plain router.
	saName := routerProxyResourceName("explicit")
	for kind, obj := range map[string]client.Object{
		"ServiceAccount": &corev1.ServiceAccount{},
		"Role":           &rbacv1.Role{},
		"RoleBinding":    &rbacv1.RoleBinding{},
	} {
		if err := c.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: ns}, obj); err != nil {
			t.Errorf("%s %s/%s missing for an endpoint-resolution router: %v", kind, ns, saName, err)
		}
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: ns}, dep); err != nil {
		t.Fatalf("router-proxy Deployment: %v", err)
	}
	if dep.Spec.Template.Spec.ServiceAccountName != saName {
		t.Errorf("deployment serviceAccountName = %q, want %q", dep.Spec.Template.Spec.ServiceAccountName, saName)
	}
}

// TestModelRouterRequiresBackendsOrSelector is parity for the new CRD rule: a
// router with neither a selector nor a backend is still rejected, by base's
// required field and by the branch's CEL rule.
func TestModelRouterRequiresBackendsOrSelector(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	var fields map[string]any
	manifest := `apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: empty
spec:
  defaultRouteStrategy: BackendNameMatch
`
	if err := yaml.Unmarshal([]byte(manifest), &fields); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	u := &unstructured.Unstructured{Object: fields}
	u.SetNamespace(ns)
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(u.GetAPIVersion(), u.GetKind()))
	err := c.Create(context.Background(), u)
	if err == nil {
		t.Fatal("a ModelRouter with neither spec.backends nor spec.backendSelector was accepted")
	}
	if !apierrors.IsInvalid(err) || !strings.Contains(strings.ToLower(err.Error()), "backend") {
		t.Fatalf("create error = %v, want an Invalid error naming the missing backends", err)
	}
}

// inferenceServiceListClient counts Lists of InferenceServiceList, so the
// parity test can prove a router without a selector never scans the
// namespace's services.
type inferenceServiceListClient struct {
	client.Client
	isvcLists int
}

func (c *inferenceServiceListClient) List(ctx context.Context, l client.ObjectList, opts ...client.ListOption) error {
	if _, ok := l.(*inferencev1alpha1.InferenceServiceList); ok {
		c.isvcLists++
	}
	return c.Client.List(ctx, l, opts...)
}

// TestModelRouterNoSelectorMakesNoInferenceServiceList pins the blast radius:
// a router that does not select by label pays nothing for the feature.
func TestModelRouterNoSelectorMakesNoInferenceServiceList(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	applySelectorManifest(t, c, ns, inferenceServiceManifest("local-a", nil))
	applySelectorManifest(t, c, ns, `apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: explicit
spec:
  backends:
    - name: local-a
      inferenceServiceRef:
        name: local-a
  defaultRoute: local-a
`)
	counter := &inferenceServiceListClient{Client: c}
	r := selectorReconciler(counter)
	reconcileSelectorRouter(t, r, ns, "explicit")
	if counter.isvcLists != 0 {
		t.Errorf("a router without spec.backendSelector made %d InferenceService List calls, want 0", counter.isvcLists)
	}
}
