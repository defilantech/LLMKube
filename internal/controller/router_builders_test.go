/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

const testBuilderNs = "router-builder-test"

func ptrInt32B(v int32) *int32 { return &v }

func builderTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add v1alpha1: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	return s
}

// canonicalModelRouter is the "real-world shape" router builder tests
// validate against: one local backend referencing an InferenceService,
// one external backend with credentials, plus the pii fail-closed rule
// and a complex-to-cloud rule.
func canonicalModelRouter() *inferencev1alpha1.ModelRouter {
	return &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "coding-router",
			Namespace: testBuilderNs,
		},
		Spec: inferencev1alpha1.ModelRouterSpec{
			Backends: []inferencev1alpha1.RouterBackend{
				{
					Name:                "local-qwen",
					InferenceServiceRef: &corev1.LocalObjectReference{Name: "qwen3-coder"},
					Tier:                "local",
					Capabilities:        []string{"code", "tools"},
				},
				{
					Name: "cloud-opus",
					External: &inferencev1alpha1.ExternalProvider{
						Provider:             "anthropic",
						Model:                "claude-opus-4-7",
						URL:                  "https://api.anthropic.com",
						CredentialsSecretRef: &corev1.LocalObjectReference{Name: "anthropic-key"},
					},
					Tier:         "cloud",
					Capabilities: []string{"vision"},
				},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:       "pii-stays-local",
					Match:      &inferencev1alpha1.RuleMatch{DataClassification: []string{"pii"}},
					Route:      inferencev1alpha1.RuleRoute{Backends: []string{"local-qwen"}},
					FailClosed: true,
				},
				{
					Name:  "complex-to-cloud",
					Match: &inferencev1alpha1.RuleMatch{TaskComplexity: "complex"},
					Route: inferencev1alpha1.RuleRoute{Backends: []string{"cloud-opus", "local-qwen"}},
				},
			},
			DefaultRoute: "local-qwen",
			Policy: &inferencev1alpha1.RouterPolicy{
				Classification: &inferencev1alpha1.ClassificationPolicy{Mode: "header-only"},
				AuditLog:       &inferencev1alpha1.AuditLogPolicy{Sink: "stdout"},
			},
		},
	}
}

func newRouterReconcilerForTest(t *testing.T, mr *inferencev1alpha1.ModelRouter, seeds ...client.Object) *ModelRouterReconciler {
	t.Helper()
	scheme := builderTestScheme(t)
	objs := append([]client.Object{mr}, seeds...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &ModelRouterReconciler{
		Client: c,
		Scheme: scheme,
	}
}

// TestCompileRouterConfigResolvesLocalBackend verifies that a local
// InferenceServiceRef resolves to the expected in-cluster URL.
func TestCompileRouterConfigResolvesLocalBackend(t *testing.T) {
	mr := canonicalModelRouter()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-coder", Namespace: testBuilderNs},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "qwen3-coder"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic-key", Namespace: testBuilderNs},
		Data:       map[string][]byte{"ANTHROPIC_API_KEY": []byte("test")},
	}
	r := newRouterReconcilerForTest(t, mr, isvc, secret)

	compiled, err := r.compileRouterConfig(context.Background(), mr)
	if err != nil {
		t.Fatalf("compileRouterConfig: %v", err)
	}

	var cfg router.Config
	if err := json.Unmarshal(compiled.JSON, &cfg); err != nil {
		t.Fatalf("unmarshal compiled JSON: %v", err)
	}
	if len(cfg.Backends) != 2 {
		t.Fatalf("got %d backends, want 2", len(cfg.Backends))
	}
	wantPrefix := "http://qwen3-coder." + testBuilderNs + ".svc.cluster.local:"
	if !strings.HasPrefix(cfg.Backends[0].Address, wantPrefix) {
		t.Errorf("local backend address = %q, want prefix %q", cfg.Backends[0].Address, wantPrefix)
	}
	if cfg.Backends[1].CredentialsEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("cloud backend credentials env = %q, want ANTHROPIC_API_KEY", cfg.Backends[1].CredentialsEnv)
	}
	for _, b := range compiled.Backends {
		if !b.Healthy {
			t.Errorf("backend %s should be healthy after resolution, Message=%q", b.Name, b.Message)
		}
	}
}

// TestCompileRouterConfigMissingInferenceService confirms that a missing
// local InferenceService produces an unhealthy backend status. The
// proxy's Validate() rejects empty addresses, so the overall compile
// errors out; callers patch the failure into the Available condition.
func TestCompileRouterConfigMissingInferenceService(t *testing.T) {
	mr := canonicalModelRouter()
	r := newRouterReconcilerForTest(t, mr) // no InferenceService or Secret seeded
	_, err := r.compileRouterConfig(context.Background(), mr)
	if err == nil {
		t.Fatal("expected validation error when InferenceService is missing")
	}
}

// TestCompileRouterConfigMissingSecretKey surfaces a Secret-missing-key
// as a per-backend Healthy=false status. The local backend still
// resolves cleanly so the wire shape passes validation.
func TestCompileRouterConfigMissingSecretKey(t *testing.T) {
	mr := canonicalModelRouter()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-coder", Namespace: testBuilderNs},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic-key", Namespace: testBuilderNs},
		Data:       map[string][]byte{"WRONG_KEY": []byte("ignored")},
	}
	r := newRouterReconcilerForTest(t, mr, isvc, secret)

	compiled, err := r.compileRouterConfig(context.Background(), mr)
	if err != nil {
		t.Fatalf("compileRouterConfig: %v", err)
	}
	cloud := compiled.Backends[1]
	if cloud.Name != "cloud-opus" {
		t.Fatalf("unexpected ordering: %+v", compiled.Backends)
	}
	if cloud.Healthy {
		t.Error("cloud backend should be unhealthy when credentials key is missing")
	}
	if !strings.Contains(cloud.Message, "missing key") {
		t.Errorf("cloud backend Message = %q, want missing-key error", cloud.Message)
	}
}

// TestCompileRouterConfigExternalNoSecretSkipsCredentials covers the
// "auth-less external backend" path (e.g. an in-cluster OpenAI-shape
// mock, a LiteLLM proxy that handles auth on its own side, a non-
// LLMKube vLLM): when no credentialsSecretRef is provided, the
// controller must NOT inject a well-known credentials env name into
// the compiled config, or the proxy would refuse to dispatch with
// "credentials env X is unset" even though the backend never needed
// auth.
func TestCompileRouterConfigExternalNoSecretSkipsCredentials(t *testing.T) {
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-auth-router",
			Namespace: testBuilderNs,
		},
		Spec: inferencev1alpha1.ModelRouterSpec{
			Backends: []inferencev1alpha1.RouterBackend{
				{
					Name: "local-mock",
					External: &inferencev1alpha1.ExternalProvider{
						Provider: "openai",
						Model:    "stub",
						URL:      "http://mock.example.svc.cluster.local:8080",
					},
					Tier: "local",
				},
			},
			DefaultRoute: "local-mock",
		},
	}
	r := newRouterReconcilerForTest(t, mr)

	compiled, err := r.compileRouterConfig(context.Background(), mr)
	if err != nil {
		t.Fatalf("compileRouterConfig: %v", err)
	}

	var cfg router.Config
	if err := json.Unmarshal(compiled.JSON, &cfg); err != nil {
		t.Fatalf("unmarshal compiled JSON: %v", err)
	}
	if got := cfg.Backends[0].CredentialsEnv; got != "" {
		t.Errorf("external backend without secret got CredentialsEnv = %q, want empty", got)
	}
	if !compiled.Backends[0].Healthy {
		t.Errorf("backend should be Healthy=true when no secret is required, Message=%q",
			compiled.Backends[0].Message)
	}
}

// TestRouterDeploymentBuilder pins the deployment shape this PR
// contracts on for downstream callers (CI smoke tests, future
// production users).
func TestRouterDeploymentBuilder(t *testing.T) {
	mr := canonicalModelRouter()
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	dep := r.newRouterDeployment(mr, "deadbeef")

	if got := dep.Name; got != "coding-router-router-proxy" {
		t.Errorf("Deployment name = %q", got)
	}
	if *dep.Spec.Replicas != 1 {
		t.Errorf("default replicas = %d, want 1", *dep.Spec.Replicas)
	}

	pod := dep.Spec.Template
	if pod.Annotations[routerProxyConfigHashAnnotation] != "deadbeef" {
		t.Errorf("config hash annotation = %q", pod.Annotations[routerProxyConfigHashAnnotation])
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if c.Image != "ghcr.io/test/router-proxy:v1" {
		t.Errorf("image = %q, want flag-provided override", c.Image)
	}
	if !strings.Contains(strings.Join(c.Args, " "), routerProxyConfigMountPath+"/"+routerProxyConfigKey) {
		t.Errorf("--config arg missing; args=%v", c.Args)
	}
	if c.SecurityContext == nil || c.SecurityContext.RunAsNonRoot == nil || !*c.SecurityContext.RunAsNonRoot {
		t.Error("container SecurityContext must set RunAsNonRoot=true")
	}
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("container SecurityContext must set ReadOnlyRootFilesystem=true")
	}
	if !hasConfigVolume(pod.Spec.Volumes) {
		t.Error("pod spec must mount router-config volume")
	}
	if !hasEnvFromSecret(c.EnvFrom, "anthropic-key") {
		t.Errorf("container envFrom must reference anthropic-key; got %+v", c.EnvFrom)
	}
}

// TestRouterDeploymentBuilderRespectsOverrides validates that
// spec.proxy.{replicas,image} take precedence over the controller's
// flag defaults.
func TestRouterDeploymentBuilderRespectsOverrides(t *testing.T) {
	mr := canonicalModelRouter()
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{
		Replicas: ptrInt32B(3),
		Image:    "registry.internal/router-proxy:custom",
	}
	r := &ModelRouterReconciler{RouterProxyImage: "should-be-overridden"}
	dep := r.newRouterDeployment(mr, "hash")
	if *dep.Spec.Replicas != 3 {
		t.Errorf("replicas override = %d, want 3", *dep.Spec.Replicas)
	}
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != "registry.internal/router-proxy:custom" {
		t.Errorf("image = %q, want spec.proxy.image override", got)
	}
}

// TestRouterServiceBuilder confirms ClusterIP default and the
// canonical selector label.
func TestRouterServiceBuilder(t *testing.T) {
	mr := canonicalModelRouter()
	svc := newRouterService(mr)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("default type = %v, want ClusterIP", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 8080 {
		t.Errorf("ports = %+v", svc.Spec.Ports)
	}
	if got := svc.Spec.Selector["inference.llmkube.dev/model-router"]; got != mr.Name {
		t.Errorf("selector label = %q, want %q", got, mr.Name)
	}
}

func TestRouterServiceBuilderUpgradesType(t *testing.T) {
	mr := canonicalModelRouter()
	mr.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Type: "LoadBalancer"}
	svc := newRouterService(mr)
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Type = %v, want LoadBalancer", svc.Spec.Type)
	}
}

func TestRouterProxyEndpointURL(t *testing.T) {
	mr := canonicalModelRouter()
	want := "http://coding-router-router-proxy." + testBuilderNs + ".svc.cluster.local:8080/v1/chat/completions"
	if got := routerProxyEndpoint(mr); got != want {
		t.Errorf("endpoint = %q, want %q", got, want)
	}
	mr.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Port: 9090, Path: "/v1/completions"}
	if got := routerProxyEndpoint(mr); !strings.HasSuffix(got, ":9090/v1/completions") {
		t.Errorf("override endpoint = %q", got)
	}
}

func TestSummarizeBackends(t *testing.T) {
	cases := []struct {
		name      string
		backends  []inferencev1alpha1.BackendStatus
		wantReady bool
	}{
		{name: "empty", wantReady: false},
		{
			name: "all healthy",
			backends: []inferencev1alpha1.BackendStatus{
				{Name: "a", Healthy: true},
				{Name: "b", Healthy: true},
			},
			wantReady: true,
		},
		{
			name: "one unhealthy",
			backends: []inferencev1alpha1.BackendStatus{
				{Name: "a", Healthy: true},
				{Name: "b", Healthy: false, Message: "secret missing"},
			},
			wantReady: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ready, _ := summarizeBackends(tc.backends)
			if ready != tc.wantReady {
				t.Errorf("ready = %v, want %v", ready, tc.wantReady)
			}
		})
	}
}

func hasConfigVolume(vols []corev1.Volume) bool {
	for _, v := range vols {
		if v.Name == "router-config" && v.ConfigMap != nil {
			return true
		}
	}
	return false
}

func hasEnvFromSecret(envFrom []corev1.EnvFromSource, name string) bool {
	for _, e := range envFrom {
		if e.SecretRef != nil && e.SecretRef.Name == name {
			return true
		}
	}
	return false
}
