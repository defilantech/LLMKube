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
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatalf("add appsv1: %v", err)
	}
	if err := rbacv1.AddToScheme(s); err != nil {
		t.Fatalf("add rbacv1: %v", err)
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

// TestResolveExternalURLProviderDefaults covers the URL-defaulting
// matrix added for issue #438: first-party providers get their
// published endpoint when url is omitted, litellm gets the operator-
// configured cluster default (or a clear error when neither is set),
// and providers without a built-in default require an explicit url.
func TestResolveExternalURLProviderDefaults(t *testing.T) {
	cases := []struct {
		name       string
		provider   string
		url        string
		litellmDef string
		wantAddr   string
		wantMsg    string
	}{
		{
			name:     "explicit url always wins",
			provider: "anthropic",
			url:      "https://eu.api.anthropic.com",
			wantAddr: "https://eu.api.anthropic.com",
		},
		{
			name:     "anthropic default",
			provider: "anthropic",
			wantAddr: "https://api.anthropic.com",
		},
		{
			name:     "openai default",
			provider: "openai",
			wantAddr: "https://api.openai.com",
		},
		{
			name:       "litellm with operator default",
			provider:   "litellm",
			litellmDef: "http://litellm.litellm.svc.cluster.local:4000",
			wantAddr:   "http://litellm.litellm.svc.cluster.local:4000",
		},
		{
			name:     "litellm without default returns error",
			provider: "litellm",
			wantMsg:  "default-litellm-url",
		},
		{
			name:     "bedrock requires explicit url",
			provider: "bedrock",
			wantMsg:  "no built-in default",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &ModelRouterReconciler{DefaultLiteLLMURL: tc.litellmDef}
			gotAddr, gotMsg := r.resolveExternalURL(&inferencev1alpha1.ExternalProvider{
				Provider: tc.provider,
				URL:      tc.url,
			})
			if gotAddr != tc.wantAddr {
				t.Errorf("address = %q, want %q", gotAddr, tc.wantAddr)
			}
			if tc.wantMsg == "" {
				if gotMsg != "" {
					t.Errorf("unexpected message: %q", gotMsg)
				}
			} else if !strings.Contains(gotMsg, tc.wantMsg) {
				t.Errorf("message = %q, want substring %q", gotMsg, tc.wantMsg)
			}
		})
	}
}

// TestCompileRouterConfigDefaultsAnthropicURL exercises the full
// compile path with an Anthropic backend missing url: the wire config
// and BackendStatus.Address must both pick up https://api.anthropic.com.
func TestCompileRouterConfigDefaultsAnthropicURL(t *testing.T) {
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-url-router",
			Namespace: testBuilderNs,
		},
		Spec: inferencev1alpha1.ModelRouterSpec{
			Backends: []inferencev1alpha1.RouterBackend{
				{
					Name: "cloud-anthropic",
					External: &inferencev1alpha1.ExternalProvider{
						Provider: "anthropic",
						Model:    "claude-opus-4-7",
					},
					Tier: "cloud",
				},
			},
			DefaultRoute: "cloud-anthropic",
		},
	}
	r := newRouterReconcilerForTest(t, mr)

	compiled, err := r.compileRouterConfig(context.Background(), mr)
	if err != nil {
		t.Fatalf("compileRouterConfig: %v", err)
	}
	var cfg router.Config
	if err := json.Unmarshal(compiled.JSON, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := cfg.Backends[0].Address; got != "https://api.anthropic.com" {
		t.Errorf("wire Address = %q, want https://api.anthropic.com", got)
	}
	if got := compiled.Backends[0].Address; got != "https://api.anthropic.com" {
		t.Errorf("status Address = %q, want https://api.anthropic.com", got)
	}
	if !compiled.Backends[0].Healthy {
		t.Errorf("backend should be Healthy, Message=%q", compiled.Backends[0].Message)
	}
}

// TestCompileRouterConfigLiteLLMMissingURLFails confirms a litellm
// backend without url AND no operator default surfaces a clear
// Healthy=false status; the controller doesn't silently emit an empty
// Address that the proxy would later reject with a worse error.
func TestCompileRouterConfigLiteLLMMissingURLFails(t *testing.T) {
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "litellm-missing-router",
			Namespace: testBuilderNs,
		},
		Spec: inferencev1alpha1.ModelRouterSpec{
			Backends: []inferencev1alpha1.RouterBackend{
				{
					Name: "cloud-litellm",
					External: &inferencev1alpha1.ExternalProvider{
						Provider: "litellm",
						Model:    "claude-opus-4-7",
					},
					Tier: "cloud",
				},
			},
			DefaultRoute: "cloud-litellm",
		},
	}
	r := newRouterReconcilerForTest(t, mr) // no DefaultLiteLLMURL set

	_, err := r.compileRouterConfig(context.Background(), mr)
	if err == nil {
		t.Fatal("expected validation error for empty address")
	}
	if !strings.Contains(err.Error(), "address") &&
		!strings.Contains(err.Error(), "url") {
		t.Errorf("error %q should mention address/url shape", err.Error())
	}
}

// TestRouterDeploymentBuilder pins the deployment shape this PR
// contracts on for downstream callers (CI smoke tests, future
// production users).
func TestRouterDeploymentBuilder(t *testing.T) {
	mr := canonicalModelRouter()
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	dep := r.newRouterDeployment(mr, "deadbeef", false)

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
	dep := r.newRouterDeployment(mr, "hash", false)
	if *dep.Spec.Replicas != 3 {
		t.Errorf("replicas override = %d, want 3", *dep.Spec.Replicas)
	}
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != "registry.internal/router-proxy:custom" {
		t.Errorf("image = %q, want spec.proxy.image override", got)
	}
}

// TestRouterDeploymentBuilderNodeSelector checks that spec.proxy.nodeSelector
// pins the proxy pod: the map reaches the Pod spec so operators can keep the
// proxy on nodes that actually have its image (e.g. a node-local registry).
func TestRouterDeploymentBuilderNodeSelector(t *testing.T) {
	mr := canonicalModelRouter()
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{
		NodeSelector: map[string]string{"kubernetes.io/hostname": "corsair"},
	}
	r := &ModelRouterReconciler{RouterProxyImage: "img"}
	dep := r.newRouterDeployment(mr, "hash", false)
	got := dep.Spec.Template.Spec.NodeSelector
	if got["kubernetes.io/hostname"] != "corsair" {
		t.Errorf("pod NodeSelector = %v, want kubernetes.io/hostname=corsair", got)
	}
}

// TestRouterDeploymentBuilderNodeSelectorDefault confirms an unset
// spec.proxy.nodeSelector leaves the Pod spec's NodeSelector nil rather than an
// empty map, so the proxy schedules anywhere by default.
func TestRouterDeploymentBuilderNodeSelectorDefault(t *testing.T) {
	dep := (&ModelRouterReconciler{RouterProxyImage: "img"}).
		newRouterDeployment(canonicalModelRouter(), "hash", false)
	if dep.Spec.Template.Spec.NodeSelector != nil {
		t.Errorf("pod NodeSelector = %v, want nil when spec.proxy.nodeSelector unset",
			dep.Spec.Template.Spec.NodeSelector)
	}
}

// TestRouterDeploymentBuilderRevisionHistoryLimit checks that
// spec.proxy.revisionHistoryLimit reaches the Deployment, and that
// omitting it leaves the field nil (apiserver default).
func TestRouterDeploymentBuilderRevisionHistoryLimit(t *testing.T) {
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}

	depDefault := r.newRouterDeployment(canonicalModelRouter(), "hash", false)
	if depDefault.Spec.RevisionHistoryLimit != nil {
		t.Errorf("revisionHistoryLimit = %v, want nil when unset",
			*depDefault.Spec.RevisionHistoryLimit)
	}

	for _, want := range []int32{0, 3} {
		mr := canonicalModelRouter()
		mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{
			RevisionHistoryLimit: ptrInt32B(want),
		}
		dep := r.newRouterDeployment(mr, "hash", false)
		if dep.Spec.RevisionHistoryLimit == nil {
			t.Fatalf("revisionHistoryLimit = nil, want %d", want)
		}
		if got := *dep.Spec.RevisionHistoryLimit; got != want {
			t.Errorf("revisionHistoryLimit = %d, want %d", got, want)
		}
	}
}

// TestRouterDeploymentBuilderQuarantineDuration covers the
// spec.proxy.quarantineDuration plumbing: the controller has to
// render the value as a --quarantine-duration flag on the proxy
// container so the operator-side knob actually reaches the binary.
func TestRouterDeploymentBuilderQuarantineDuration(t *testing.T) {
	mr := canonicalModelRouter()
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{
		QuarantineDuration: &metav1.Duration{Duration: 2 * time.Second},
	}
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	dep := r.newRouterDeployment(mr, "hash", false)

	args := dep.Spec.Template.Spec.Containers[0].Args
	var found bool
	for i, a := range args {
		if a == "--quarantine-duration" && i+1 < len(args) {
			found = true
			if args[i+1] != "2s" {
				t.Errorf("--quarantine-duration value = %q, want 2s", args[i+1])
			}
		}
	}
	if !found {
		t.Errorf("--quarantine-duration flag not rendered; args = %v", args)
	}
}

// TestRouterDeploymentBuilderQuarantineDefault confirms the proxy
// keeps its compiled-in 15s default when the user did NOT set
// spec.proxy.quarantineDuration. We explicitly do NOT pass the flag
// in that case so future default changes (eg moving to 30s) take
// effect without the user having to redeploy.
func TestRouterDeploymentBuilderQuarantineDefault(t *testing.T) {
	mr := canonicalModelRouter()
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	dep := r.newRouterDeployment(mr, "hash", false)

	for _, a := range dep.Spec.Template.Spec.Containers[0].Args {
		if a == "--quarantine-duration" {
			t.Errorf("--quarantine-duration must be omitted when unset; args = %v",
				dep.Spec.Template.Spec.Containers[0].Args)
		}
	}
}

// TestRouterDeploymentBuilderResponseHeaderTimeoutRendered covers the
// spec.proxy.responseHeaderTimeout plumbing added for #457.
func TestRouterDeploymentBuilderResponseHeaderTimeoutRendered(t *testing.T) {
	mr := canonicalModelRouter()
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{
		ResponseHeaderTimeout: &metav1.Duration{Duration: 90 * time.Second},
	}
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	dep := r.newRouterDeployment(mr, "hash", false)

	args := dep.Spec.Template.Spec.Containers[0].Args
	var found bool
	for i, a := range args {
		if a == "--response-header-timeout" && i+1 < len(args) {
			found = true
			if args[i+1] != "1m30s" {
				t.Errorf("--response-header-timeout value = %q, want 1m30s", args[i+1])
			}
		}
	}
	if !found {
		t.Errorf("--response-header-timeout flag not rendered; args = %v", args)
	}
}

// TestRouterDeploymentBuilderResponseHeaderTimeoutDefault is the
// inverse: with spec.proxy.responseHeaderTimeout unset the flag must
// be omitted so the proxy honors its compiled-in 120s default and
// future default tweaks land without manifest churn.
func TestRouterDeploymentBuilderResponseHeaderTimeoutDefault(t *testing.T) {
	mr := canonicalModelRouter()
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	dep := r.newRouterDeployment(mr, "hash", false)

	for _, a := range dep.Spec.Template.Spec.Containers[0].Args {
		if a == "--response-header-timeout" {
			t.Errorf("--response-header-timeout must be omitted when unset; args = %v",
				dep.Spec.Template.Spec.Containers[0].Args)
		}
	}
}

// TestCompileRouterConfigCopiesRuleTimeout pins that
// ModelRouter.spec.rules[].timeout flows through translateRule into
// the wire-shape Rule.Timeout the proxy reads. Without this the
// per-rule override is invisible at dispatch time.
func TestCompileRouterConfigCopiesRuleTimeout(t *testing.T) {
	mr := canonicalModelRouter()
	// Annotate the first rule with a timeout the proxy would honor.
	mr.Spec.Rules[0].Timeout = &metav1.Duration{Duration: 7 * time.Second}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-coder", Namespace: testBuilderNs},
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
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Rules[0].Timeout != 7*time.Second {
		t.Errorf("rule timeout = %v, want 7s", cfg.Rules[0].Timeout)
	}
}

// TestCompileRouterConfigCopiesBackendTimeout pins the equivalent for
// per-backend timeouts.
func TestCompileRouterConfigCopiesBackendTimeout(t *testing.T) {
	mr := canonicalModelRouter()
	mr.Spec.Backends[1].Timeout = &metav1.Duration{Duration: 3 * time.Minute}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-coder", Namespace: testBuilderNs},
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
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Backends[1].Timeout != 3*time.Minute {
		t.Errorf("backend timeout = %v, want 3m", cfg.Backends[1].Timeout)
	}
}

// TestCompileRouterConfigCopiesDisplayName guards the controller half of
// the per-backend displayName feature (#826): the CRD stores DisplayName
// and the proxy publishes/matches on it, but only if the controller
// actually emits it into config.json. resolveBackend used to drop it, so
// the published id silently fell back to Name. A backend with a
// DisplayName must surface it in the compiled config; one without keeps
// it empty (the proxy then uses Name).
func TestCompileRouterConfigCopiesDisplayName(t *testing.T) {
	mr := canonicalModelRouter()
	mr.Spec.Backends[1].DisplayName = "cloud/opus"

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-coder", Namespace: testBuilderNs},
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
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Backends[1].DisplayName != "cloud/opus" {
		t.Errorf("backend displayName = %q, want cloud/opus", cfg.Backends[1].DisplayName)
	}
	if cfg.Backends[0].DisplayName != "" {
		t.Errorf("backend without displayName = %q, want empty", cfg.Backends[0].DisplayName)
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

// TestReconcileRouterDeploymentPreservesExternalAnnotations exercises the
// #456 fix end to end through the fake client: seed a router-proxy
// Deployment with both operator-owned and externally-set annotations,
// run reconcileRouterDeployment, and assert the externally-set keys
// survive the update.
//
// Without the fix the reconciler does `existing.Spec.Template =
// desired.Spec.Template`, which strips every external annotation
// (sidecar injectors, `kubectl rollout restart`'s restartedAt,
// GitOps tool sync labels) on every reconcile. That manifests as
// flapping ReplicaSets and truncated in-flight requests.
func TestReconcileRouterDeploymentPreservesExternalAnnotations(t *testing.T) {
	mr := canonicalModelRouter()

	// Build the initial Deployment the way an earlier reconcile would
	// have, then layer external metadata on top to simulate sidecar
	// injection / kubectl rollout-restart / GitOps annotations.
	const gitopsInstance = "coding-router-fleet"
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	initial := r.newRouterDeployment(mr, "oldhash", false)
	initial.Spec.Template.Annotations["sidecar.istio.io/inject"] = "true"
	initial.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = "2026-05-13T20:00:00Z"
	initial.Spec.Template.Labels["external-team-label"] = "ml-platform"
	initial.Labels["argocd.argoproj.io/instance"] = gitopsInstance

	scheme := builderTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mr, initial).
		Build()
	rec := &ModelRouterReconciler{
		Client:           c,
		Scheme:           scheme,
		RouterProxyImage: "ghcr.io/test/router-proxy:v1",
	}

	// Reconcile with a NEW config hash so the operator's owned
	// annotation needs to change.
	if err := rec.reconcileRouterDeployment(context.Background(), mr, "newhash", false); err != nil {
		t.Fatalf("reconcileRouterDeployment: %v", err)
	}

	updated := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name:      routerProxyResourceName(mr.Name),
		Namespace: mr.Namespace,
	}, updated); err != nil {
		t.Fatalf("Get updated Deployment: %v", err)
	}

	// 1. Operator-owned annotation reflects the new hash.
	gotHash := updated.Spec.Template.Annotations[routerProxyConfigHashAnnotation]
	if gotHash != "newhash" {
		t.Errorf("config hash annotation = %q, want newhash", gotHash)
	}

	// 2. External pod-template annotations survive.
	if got := updated.Spec.Template.Annotations["sidecar.istio.io/inject"]; got != "true" {
		t.Errorf("sidecar.istio.io/inject = %q, want true (sidecar injector annotation must survive reconcile)", got)
	}
	if got := updated.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"]; got != "2026-05-13T20:00:00Z" {
		t.Errorf("kubectl.kubernetes.io/restartedAt = %q, want preserved value (kubectl rollout restart must survive reconcile)", got)
	}

	// 3. External pod-template label survives.
	if got := updated.Spec.Template.Labels["external-team-label"]; got != "ml-platform" {
		t.Errorf("external-team-label = %q, want ml-platform (foreign template labels must pass through)", got)
	}

	// 4. External top-level Deployment label survives.
	if got := updated.Labels["argocd.argoproj.io/instance"]; got != gitopsInstance {
		t.Errorf("argocd instance label = %q, want %q (GitOps labels must pass through)", got, gitopsInstance)
	}

	// 5. Operator-owned selector labels remain intact (regression
	// guard against the selector going out of sync with the
	// template).
	wantSelector := routerProxySelectorLabels(mr)
	for k, v := range wantSelector {
		if got := updated.Spec.Template.Labels[k]; got != v {
			t.Errorf("selector label %q = %q, want %q", k, got, v)
		}
	}
}

// TestRouterDeploymentMetricsPort confirms the router-proxy Deployment
// exposes a dedicated metrics container port and passes the
// --metrics-bind-address flag so Prometheus scrapers can reach the
// llmkube_router_* metrics. Regression test for #1427.
func TestRouterDeploymentMetricsPort(t *testing.T) {
	mr := canonicalModelRouter()
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	dep := r.newRouterDeployment(mr, "hash", false)

	c := dep.Spec.Template.Spec.Containers[0]

	// The container must have exactly two ports: http and metrics.
	if len(c.Ports) != 2 {
		t.Fatalf("container ports = %d, want 2", len(c.Ports))
	}

	ports := map[string]int32{}
	for _, p := range c.Ports {
		ports[p.Name] = p.ContainerPort
	}

	// Pin the literal rather than comparing against the constant the builder
	// used: that assertion holds for any value, including one that collides
	// with the data-plane port and stops the proxy binding at all.
	got, ok := ports["metrics"]
	if !ok {
		t.Fatal("no metrics container port found")
	}
	if got != 9090 {
		t.Errorf("metrics containerPort = %d, want 9090", got)
	}
	if got == ports["http"] {
		t.Errorf("metrics port collides with the data-plane port (%d); both listeners would bind the same address", ports["http"])
	}

	// The --metrics-bind-address flag must be present and agree with the port.
	var foundFlag bool
	for i, a := range c.Args {
		if a == "--metrics-bind-address" && i+1 < len(c.Args) {
			foundFlag = true
			want := fmt.Sprintf(":%d", got)
			if c.Args[i+1] != want {
				t.Errorf("--metrics-bind-address value = %q, want %q", c.Args[i+1], want)
			}
		}
	}
	if !foundFlag {
		t.Errorf("--metrics-bind-address flag not rendered; args = %v", c.Args)
	}
}

// modelPoolFor builds a ModelPool in the builder test namespace whose members
// are the given InferenceService names.
func modelPoolFor(name string, memberNames ...string) *inferencev1alpha1.ModelPool {
	members := make([]inferencev1alpha1.ModelPoolMember, 0, len(memberNames))
	for _, m := range memberNames {
		members = append(members, inferencev1alpha1.ModelPoolMember{
			InferenceServiceRef: corev1.LocalObjectReference{Name: m},
		})
	}
	return &inferencev1alpha1.ModelPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testBuilderNs},
		Spec: inferencev1alpha1.ModelPoolSpec{
			SwapPolicy: inferencev1alpha1.ModelPoolSwapPolicySticky,
			SwapBudget: &metav1.Duration{Duration: 6 * time.Minute},
			Members:    members,
		},
	}
}

// TestCompileRouterConfigResolvesBackendPool verifies the CRD->proxy seam
// (gap 1): a backend whose InferenceService is a ModelPool member compiles a
// BackendPool into the wire config, and the compiled result reports HasPools.
func TestCompileRouterConfigResolvesBackendPool(t *testing.T) {
	mr := canonicalModelRouter()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-coder", Namespace: testBuilderNs},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic-key", Namespace: testBuilderNs},
		Data:       map[string][]byte{"ANTHROPIC_API_KEY": []byte("test")},
	}
	pool := modelPoolFor("heavy-slot", "qwen3-coder", "gemma-judge")
	r := newRouterReconcilerForTest(t, mr, isvc, secret, pool)

	compiled, err := r.compileRouterConfig(context.Background(), mr)
	if err != nil {
		t.Fatalf("compileRouterConfig: %v", err)
	}
	if !compiled.HasPools {
		t.Fatal("compiled.HasPools = false, want true when a backend is a ModelPool member")
	}

	var cfg router.Config
	if err := json.Unmarshal(compiled.JSON, &cfg); err != nil {
		t.Fatalf("unmarshal compiled JSON: %v", err)
	}
	// local-qwen is backend[0] and references qwen3-coder.
	local := cfg.Backends[0]
	if local.Pool == nil {
		t.Fatalf("backend %q Pool = nil, want compiled BackendPool", local.Name)
	}
	if local.Pool.Name != "heavy-slot" || local.Pool.Namespace != testBuilderNs {
		t.Errorf("Pool name/ns = %q/%q, want heavy-slot/%s", local.Pool.Name, local.Pool.Namespace, testBuilderNs)
	}
	if local.Pool.Member != "qwen3-coder" {
		t.Errorf("Pool.Member = %q, want qwen3-coder", local.Pool.Member)
	}
	if len(local.Pool.Members) != 2 || local.Pool.Members[0] != "qwen3-coder" || local.Pool.Members[1] != "gemma-judge" {
		t.Errorf("Pool.Members = %v, want [qwen3-coder gemma-judge]", local.Pool.Members)
	}
	if local.Pool.SwapBudget != 6*time.Minute {
		t.Errorf("Pool.SwapBudget = %s, want 6m (compiled from ModelPool.spec.swapBudget)", local.Pool.SwapBudget)
	}
	// The cloud backend is not pooled.
	if cfg.Backends[1].Pool != nil {
		t.Errorf("cloud backend Pool = %+v, want nil", cfg.Backends[1].Pool)
	}
}

// TestCompileRouterConfigNoPoolWhenUnpooled confirms a backend whose
// InferenceService is not a member of any ModelPool compiles no BackendPool and
// HasPools stays false.
func TestCompileRouterConfigNoPoolWhenUnpooled(t *testing.T) {
	mr := canonicalModelRouter()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-coder", Namespace: testBuilderNs},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic-key", Namespace: testBuilderNs},
		Data:       map[string][]byte{"ANTHROPIC_API_KEY": []byte("test")},
	}
	// A pool exists but does NOT list qwen3-coder.
	pool := modelPoolFor("other-slot", "some-other-isvc")
	r := newRouterReconcilerForTest(t, mr, isvc, secret, pool)

	compiled, err := r.compileRouterConfig(context.Background(), mr)
	if err != nil {
		t.Fatalf("compileRouterConfig: %v", err)
	}
	if compiled.HasPools {
		t.Error("compiled.HasPools = true, want false when no backend is a pool member")
	}
	var cfg router.Config
	if err := json.Unmarshal(compiled.JSON, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Backends[0].Pool != nil {
		t.Errorf("backend Pool = %+v, want nil", cfg.Backends[0].Pool)
	}
}

// TestReconcileRouterActivationRBAC verifies the proxy activation RBAC (gap 2):
// with pools present, a ServiceAccount, Role (get/list/watch/update/patch on
// inferenceservices; get on status), and RoleBinding are provisioned, all
// owner-referenced to the ModelRouter.
func TestReconcileRouterActivationRBAC(t *testing.T) {
	mr := canonicalModelRouter()
	scheme := builderTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr).Build()
	r := &ModelRouterReconciler{Client: c, Scheme: scheme}

	if err := r.reconcileRouterActivationRBAC(context.Background(), mr, true); err != nil {
		t.Fatalf("reconcileRouterActivationRBAC: %v", err)
	}

	name := routerProxyResourceName(mr.Name)
	nn := types.NamespacedName{Name: name, Namespace: mr.Namespace}

	sa := &corev1.ServiceAccount{}
	if err := c.Get(context.Background(), nn, sa); err != nil {
		t.Fatalf("ServiceAccount not created: %v", err)
	}
	if len(sa.OwnerReferences) == 0 {
		t.Error("ServiceAccount has no owner reference to the ModelRouter")
	}

	role := &rbacv1.Role{}
	if err := c.Get(context.Background(), nn, role); err != nil {
		t.Fatalf("Role not created: %v", err)
	}
	if !roleGrants(role, "inferenceservices", "patch") {
		t.Errorf("Role missing patch on inferenceservices: %+v", role.Rules)
	}
	if !roleGrants(role, "inferenceservices", "get") {
		t.Errorf("Role missing get on inferenceservices: %+v", role.Rules)
	}
	if !roleGrants(role, "inferenceservices/status", "get") {
		t.Errorf("Role missing get on inferenceservices/status: %+v", role.Rules)
	}
	// The proxy must NOT be able to create or delete InferenceServices.
	if roleGrants(role, "inferenceservices", "delete") {
		t.Error("Role over-grants: proxy should not be able to delete inferenceservices")
	}

	binding := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), nn, binding); err != nil {
		t.Fatalf("RoleBinding not created: %v", err)
	}
	if binding.RoleRef.Name != name || binding.RoleRef.Kind != "Role" {
		t.Errorf("RoleBinding roleRef = %+v, want Role/%s", binding.RoleRef, name)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != name || binding.Subjects[0].Kind != rbacv1.ServiceAccountKind {
		t.Errorf("RoleBinding subjects = %+v, want ServiceAccount/%s", binding.Subjects, name)
	}
}

// TestReconcileRouterActivationRBACNoopWhenUnpooled confirms no RBAC is
// provisioned for a router with no pooled backends.
func TestReconcileRouterActivationRBACNoopWhenUnpooled(t *testing.T) {
	mr := canonicalModelRouter()
	scheme := builderTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr).Build()
	r := &ModelRouterReconciler{Client: c, Scheme: scheme}

	if err := r.reconcileRouterActivationRBAC(context.Background(), mr, false); err != nil {
		t.Fatalf("reconcileRouterActivationRBAC: %v", err)
	}
	nn := types.NamespacedName{Name: routerProxyResourceName(mr.Name), Namespace: mr.Namespace}
	if err := c.Get(context.Background(), nn, &corev1.ServiceAccount{}); err == nil {
		t.Error("ServiceAccount created for an unpooled router; activation RBAC should be a no-op")
	}
}

// roleGrants reports whether a Role has a rule granting verb on a resource in
// the inference.llmkube.dev API group.
func roleGrants(role *rbacv1.Role, resource, verb string) bool {
	const group = "inference.llmkube.dev"
	for _, rule := range role.Rules {
		if !strSliceHas(rule.APIGroups, group) || !strSliceHas(rule.Resources, resource) {
			continue
		}
		if strSliceHas(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

func strSliceHas(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestNewRouterDeploymentActivationWiring verifies the deployment activation
// wiring (gap 3): with pools the proxy container gets --enable-activation, the
// dedicated ServiceAccount, and the ROUTER_NAME env; without pools it gets none
// of them.
func TestNewRouterDeploymentActivationWiring(t *testing.T) {
	mr := canonicalModelRouter()
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}

	withPools := r.newRouterDeployment(mr, "hash", true)
	wc := withPools.Spec.Template.Spec.Containers[0]
	if !strSliceHas(wc.Args, "--enable-activation") {
		t.Errorf("pooled deployment args = %v, want --enable-activation", wc.Args)
	}
	if withPools.Spec.Template.Spec.ServiceAccountName != routerProxyResourceName(mr.Name) {
		t.Errorf("pooled ServiceAccountName = %q, want %q",
			withPools.Spec.Template.Spec.ServiceAccountName, routerProxyResourceName(mr.Name))
	}
	if got := envValue(wc.Env, "ROUTER_NAME"); got != mr.Name {
		t.Errorf("pooled ROUTER_NAME env = %q, want %q", got, mr.Name)
	}

	noPools := r.newRouterDeployment(mr, "hash", false)
	nc := noPools.Spec.Template.Spec.Containers[0]
	if strSliceHas(nc.Args, "--enable-activation") {
		t.Error("unpooled deployment must not set --enable-activation")
	}
	if noPools.Spec.Template.Spec.ServiceAccountName != "" {
		t.Errorf("unpooled ServiceAccountName = %q, want empty (namespace default)",
			noPools.Spec.Template.Spec.ServiceAccountName)
	}
	if envValue(nc.Env, "ROUTER_NAME") != "" {
		t.Error("unpooled deployment must not set ROUTER_NAME env")
	}
}

// TestNewRouterDeploymentPinsReplicasWhenPooled verifies the single-writer
// constraint (#1393 review): a pooled router is pinned to one proxy replica
// even when spec.proxy.replicas asks for more, so ModelPool swaps stay
// serialized; an unpooled router honors the requested replica count.
func TestNewRouterDeploymentPinsReplicasWhenPooled(t *testing.T) {
	r := &ModelRouterReconciler{RouterProxyImage: "ghcr.io/test/router-proxy:v1"}
	mr := canonicalModelRouter()
	three := int32(3)
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{Replicas: &three}

	pooled := r.newRouterDeployment(mr, "hash", true)
	if pooled.Spec.Replicas == nil || *pooled.Spec.Replicas != 1 {
		t.Errorf("pooled replicas = %v, want 1 (pinned)", pooled.Spec.Replicas)
	}

	unpooled := r.newRouterDeployment(mr, "hash", false)
	if unpooled.Spec.Replicas == nil || *unpooled.Spec.Replicas != 3 {
		t.Errorf("unpooled replicas = %v, want 3 (spec.proxy.replicas honored)", unpooled.Spec.Replicas)
	}
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// TestFindModelRoutersForModelPool verifies the watch mapping (gap 7): a changed
// ModelPool enqueues every ModelRouter in its namespace that references one of
// its members, and none that do not.
func TestFindModelRoutersForModelPool(t *testing.T) {
	mr := canonicalModelRouter() // references qwen3-coder via local-qwen
	other := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-router", Namespace: testBuilderNs},
		Spec: inferencev1alpha1.ModelRouterSpec{
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "b", InferenceServiceRef: &corev1.LocalObjectReference{Name: "some-other-isvc"}, Tier: "local"},
			},
		},
	}
	scheme := builderTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr, other).Build()
	r := &ModelRouterReconciler{Client: c, Scheme: scheme}

	pool := modelPoolFor("heavy-slot", "qwen3-coder")
	reqs := r.findModelRoutersForModelPool(context.Background(), pool)

	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1 (only the router referencing qwen3-coder)", len(reqs))
	}
	if reqs[0].Name != mr.Name || reqs[0].Namespace != mr.Namespace {
		t.Errorf("enqueued %s/%s, want %s/%s", reqs[0].Namespace, reqs[0].Name, mr.Namespace, mr.Name)
	}
}
