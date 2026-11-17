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
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func routerScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func routerWithLocal(name, isvc string) *inferencev1alpha1.ModelRouter {
	return &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DefaultRoute: name,
			Backends: []inferencev1alpha1.RouterBackend{{
				Name: name, InferenceServiceRef: &corev1.LocalObjectReference{Name: isvc},
			}},
		},
	}
}

func TestKeyedBackends(t *testing.T) {
	keyed := keyedISvc(RuntimeVLLM)
	keyed.Name = "keyed"
	open := keyedISvc(RuntimeVLLM)
	open.Name, open.Spec.Endpoint = "open", nil
	c := fake.NewClientBuilder().WithScheme(routerScheme(t)).WithObjects(keyed, open).Build()

	mr := routerWithLocal("a", "keyed")
	mr.Spec.Backends = append(mr.Spec.Backends,
		inferencev1alpha1.RouterBackend{Name: "b", InferenceServiceRef: &corev1.LocalObjectReference{Name: "open"}},
		inferencev1alpha1.RouterBackend{Name: "c", InferenceServiceRef: &corev1.LocalObjectReference{Name: "missing"}})
	got, err := keyedBackends(context.Background(), c, mr.Namespace, mr.Spec.Backends)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"a"}) {
		t.Errorf("keyedBackends = %v, want [a]", got)
	}
	if msg := keyedBackendsMessage(got); !strings.Contains(msg, `"a"`) || !strings.Contains(msg, "apiKeySecretRef") ||
		!strings.Contains(msg, "--api-key") || !strings.Contains(msg, "VLLM_API_KEY") {
		t.Errorf("message %q does not name the backend and all keying forms", msg)
	}
}

// TestKeyedBackendsCommandLineKey pins the command-line keying forms: a
// backend keyed through --api-key must fail the router the same way
// apiKeySecretRef does, and spec.args only counts when spec.command is set
// (the runtimes drop spec.args otherwise).
func TestKeyedBackendsCommandLineKey(t *testing.T) {
	withCommandArgs := func(command []string) *inferencev1alpha1.InferenceService {
		isvc := keyedISvc(RuntimeSGLANG)
		isvc.Spec.Endpoint = nil
		isvc.Spec.Command = command
		isvc.Spec.Args = []string{"--api-key", "$(MY_KEY)"}
		return isvc
	}
	flagOnly := keyedISvc(RuntimeVLLM, "--api-key", "$(MY_KEY)")
	flagOnly.Spec.Endpoint = nil
	for _, tc := range []struct {
		name string
		isvc *inferencev1alpha1.InferenceService
		want bool
	}{
		{"--api-key in extraArgs, split form", flagOnly, true},
		{"--api-key= in extraArgs, inline form", func() *inferencev1alpha1.InferenceService {
			isvc := keyedISvc(RuntimeVLLM, "--api-key=$(MY_KEY)")
			isvc.Spec.Endpoint = nil
			return isvc
		}(), true},
		{"spec.args flag with spec.command", withCommandArgs([]string{"python3", "-m", "sglang.launch_server"}), true},
		{"spec.args flag without spec.command", withCommandArgs(nil), false},
		{"unrelated extraArgs", func() *inferencev1alpha1.InferenceService {
			isvc := keyedISvc(RuntimeVLLM, "--host", "0.0.0.0")
			isvc.Spec.Endpoint = nil
			return isvc
		}(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(routerScheme(t)).WithObjects(tc.isvc).Build()
			got, err := keyedBackends(context.Background(), c, routerWithLocal("b", "svc").Namespace, routerWithLocal("b", "svc").Spec.Backends)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want && !slices.Equal(got, []string{"b"}) {
				t.Errorf("keyedBackends = %v, want [b]", got)
			}
			if !tc.want && len(got) != 0 {
				t.Errorf("keyedBackends = %v, want none", got)
			}
		})
	}
}

// TestKeyedBackendsEnvKey pins the router-side path for the vLLM env-var
// keying form: keyedBackends must inherit the widened helper verdict.
func TestKeyedBackendsEnvKey(t *testing.T) {
	isvc := keyedISvc(RuntimeVLLM)
	isvc.Spec.Endpoint = nil
	isvc.Spec.Env = []corev1.EnvVar{{Name: vllmAPIKeyEnv, Value: "sk"}}
	c := fake.NewClientBuilder().WithScheme(routerScheme(t)).WithObjects(isvc).Build()
	got, err := keyedBackends(context.Background(), c, routerWithLocal("b", "svc").Namespace, routerWithLocal("b", "svc").Spec.Backends)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"b"}) {
		t.Errorf("keyedBackends = %v, want [b]", got)
	}
}

func TestBackendsReadyConditionKeyed(t *testing.T) {
	cc := &compiledConfig{
		Backends:      []inferencev1alpha1.BackendStatus{{Name: "a", Healthy: false}},
		KeyedBackends: []string{"a"},
	}
	ready, reason, msg := backendsReadyCondition(cc)
	if ready || reason != ModelRouterReasonBackendAPIKeyUnsupported || !strings.Contains(msg, `"a"`) {
		t.Errorf("got (%v, %q, %q), want not ready, BackendAPIKeyUnsupported, naming a", ready, reason, msg)
	}
	// The condition and the gateway controllers hand-maintain one sentence:
	// the reference-only case must stay byte-identical to keyedBackendsMessage.
	if keyedVerdictMessage([]string{"a"}, nil) != keyedBackendsMessage([]string{"a"}) {
		t.Error("keyedVerdictMessage reference-only case drifted from keyedBackendsMessage")
	}
	ready, reason, _ = backendsReadyCondition(&compiledConfig{
		Backends: []inferencev1alpha1.BackendStatus{{Name: "a", Healthy: true}},
	})
	if !ready || reason != ModelRouterReasonBackendsResolved {
		t.Errorf("healthy case = (%v, %q), want ready, BackendsResolved", ready, reason)
	}
}

// Proxy compile marks the keyed backend unhealthy with the reason in its message.
func TestCompileRouterConfigMarksKeyedBackend(t *testing.T) {
	keyed := keyedISvc(RuntimeVLLM)
	keyed.Name = "keyed"
	c := fake.NewClientBuilder().WithScheme(routerScheme(t)).WithObjects(keyed).Build()
	r := &ModelRouterReconciler{Client: c}
	cc, err := r.compileRouterConfig(context.Background(), routerWithLocal("a", "keyed"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cc.KeyedBackends, []string{"a"}) || cc.Backends[0].Healthy ||
		!strings.Contains(cc.Backends[0].Message, "apiKeySecretRef") {
		t.Errorf("compiled = keyed %v, status %+v; want backend a unhealthy and named", cc.KeyedBackends, cc.Backends[0])
	}
}

// TestModelRouterReconcileProxyKeyedBackend drives the real Proxy-mode
// Reconcile against an API server: the keyed-backend verdict must survive the
// status write and read back from the stored ModelRouter.
func TestModelRouterReconcileProxyKeyedBackend(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "keyed-vllm", Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "keyed-vllm",
			Runtime:  RuntimeVLLM,
			Endpoint: &inferencev1alpha1.EndpointSpec{
				Port: 8080,
				APIKeySecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key",
				},
			},
		},
	}
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create keyed backend isvc: %v", err)
	}
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "keyed-router", Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneProxy,
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "keyed-vllm", InferenceServiceRef: &corev1.LocalObjectReference{Name: "keyed-vllm"}},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{Name: "all", Route: inferencev1alpha1.RuleRoute{Backends: []string{"keyed-vllm"}}},
			},
			DefaultRoute: "keyed-vllm",
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := &ModelRouterReconciler{Client: c, Scheme: clientgoscheme.Scheme, RouterProxyImage: "test-image"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterConditionBackendsReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ModelRouterReasonBackendAPIKeyUnsupported {
		t.Fatalf("BackendsReady = %+v, want False/%s", cond, ModelRouterReasonBackendAPIKeyUnsupported)
	}
	if !strings.Contains(cond.Message, `"keyed-vllm"`) {
		t.Errorf("BackendsReady message %q does not name the backend", cond.Message)
	}
}

// TestModelRouterReconcileProxyKeyedExtraArgsBackend drives the same proxy
// Reconcile with the key passed on the command line instead of
// apiKeySecretRef: the verdict must survive the status write and read back
// from the stored ModelRouter.
func TestModelRouterReconcileProxyKeyedExtraArgsBackend(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "keyed-vllm", Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:  "keyed-vllm",
			Runtime:   RuntimeVLLM,
			ExtraArgs: []string{"--api-key", "$(MY_KEY)"},
			Endpoint:  &inferencev1alpha1.EndpointSpec{Port: 8080},
		},
	}
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create keyed backend isvc: %v", err)
	}
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "keyed-router", Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneProxy,
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "keyed-vllm", InferenceServiceRef: &corev1.LocalObjectReference{Name: "keyed-vllm"}},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{Name: "all", Route: inferencev1alpha1.RuleRoute{Backends: []string{"keyed-vllm"}}},
			},
			DefaultRoute: "keyed-vllm",
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := &ModelRouterReconciler{Client: c, Scheme: clientgoscheme.Scheme, RouterProxyImage: "test-image"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterConditionBackendsReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ModelRouterReasonBackendAPIKeyUnsupported {
		t.Fatalf("BackendsReady = %+v, want False/%s", cond, ModelRouterReasonBackendAPIKeyUnsupported)
	}
	if !strings.Contains(cond.Message, `"keyed-vllm"`) {
		t.Errorf("BackendsReady message %q does not name the backend", cond.Message)
	}
}

// TestCompileRouterConfigMarksKeyedPoolSibling covers the pool-expansion gap:
// the referenced member is open, but a sibling in its ModelPool requires a key
// the proxy does not forward, so the backend must fail loud.
func TestCompileRouterConfigMarksKeyedPoolSibling(t *testing.T) {
	open := keyedISvc(RuntimeVLLM)
	open.Name = "open"
	open.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Port: 8080}
	keyed := keyedISvc(RuntimeVLLM, "--api-key", "$(MY_KEY)")
	keyed.Name = "keyed"
	keyed.Spec.Endpoint = nil
	pool := &inferencev1alpha1.ModelPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
		Spec: inferencev1alpha1.ModelPoolSpec{
			SwapPolicy: inferencev1alpha1.ModelPoolSwapPolicySticky,
			Members: []inferencev1alpha1.ModelPoolMember{
				{InferenceServiceRef: corev1.LocalObjectReference{Name: "open"}},
				{InferenceServiceRef: corev1.LocalObjectReference{Name: "keyed"}},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(routerScheme(t)).WithObjects(open, keyed, pool).Build()
	r := &ModelRouterReconciler{Client: c}
	// Spec order z-pooled then a-pooled: the compile must report the pair in
	// sorted order, matching the KeyedPoolBackends contract.
	mr := routerWithLocal("z-pooled", "open")
	mr.Spec.Backends = append(mr.Spec.Backends,
		inferencev1alpha1.RouterBackend{Name: "a-pooled", InferenceServiceRef: &corev1.LocalObjectReference{Name: "open"}})
	cc, err := r.compileRouterConfig(context.Background(), mr)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cc.KeyedPoolBackends, []string{"a-pooled", "z-pooled"}) ||
		slices.Contains(cc.KeyedBackends, "a-pooled") || slices.Contains(cc.KeyedBackends, "z-pooled") {
		t.Errorf("compiled = keyed %v pool %v; want both backends keyed via pool only, in sorted order",
			cc.KeyedBackends, cc.KeyedPoolBackends)
	}
	for _, status := range cc.Backends {
		if status.Healthy || !strings.Contains(status.Message, `"pool"`) || !strings.Contains(status.Message, `"keyed"`) {
			t.Errorf("backend %q status %+v; want unhealthy, naming pool and keyed member", status.Name, status)
		}
	}
}

// TestBackendsReadyConditionKeyedPool pins the condition message for the
// pool-sibling path, alone and combined with the reference-keyed case.
func TestBackendsReadyConditionKeyedPool(t *testing.T) {
	ready, reason, msg := backendsReadyCondition(&compiledConfig{
		Backends:          []inferencev1alpha1.BackendStatus{{Name: "a", Healthy: false}},
		KeyedPoolBackends: []string{"a"},
	})
	if ready || reason != ModelRouterReasonBackendAPIKeyUnsupported ||
		!strings.Contains(msg, `"a"`) || !strings.Contains(msg, "ModelPool") ||
		!strings.Contains(msg, "VLLM_API_KEY") {
		t.Errorf("pool-only case = (%v, %q, %q), want not ready, BackendAPIKeyUnsupported, naming a and the pool", ready, reason, msg)
	}
	ready, reason, msg = backendsReadyCondition(&compiledConfig{
		Backends:          []inferencev1alpha1.BackendStatus{{Name: "a", Healthy: false}, {Name: "b", Healthy: false}},
		KeyedBackends:     []string{"a"},
		KeyedPoolBackends: []string{"b"},
	})
	if ready || reason != ModelRouterReasonBackendAPIKeyUnsupported ||
		!strings.Contains(msg, `"a"`) || !strings.Contains(msg, `"b"`) ||
		!strings.Contains(msg, "VLLM_API_KEY") {
		t.Errorf("combined case = (%v, %q, %q), want not ready, BackendAPIKeyUnsupported, naming a and b", ready, reason, msg)
	}
}

// TestModelRouterReconcileProxyKeyedPoolSibling drives the real Proxy-mode
// Reconcile against an API server with a keyed pool sibling: the verdict must
// survive the status write and read back from the stored ModelRouter.
func TestModelRouterReconcileProxyKeyedPoolSibling(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	open := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "open-vllm", Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "open-vllm",
			Runtime:  RuntimeVLLM,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 8080},
		},
	}
	keyed := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "keyed-vllm", Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:  "keyed-vllm",
			Runtime:   RuntimeVLLM,
			ExtraArgs: []string{"--api-key", "$(MY_KEY)"},
			Endpoint:  &inferencev1alpha1.EndpointSpec{Port: 8080},
		},
	}
	for _, isvc := range []*inferencev1alpha1.InferenceService{open, keyed} {
		if err := c.Create(context.Background(), isvc); err != nil {
			t.Fatalf("create %s: %v", isvc.Name, err)
		}
	}
	pool := &inferencev1alpha1.ModelPool{
		ObjectMeta: metav1.ObjectMeta{Name: "sibling-pool", Namespace: ns},
		Spec: inferencev1alpha1.ModelPoolSpec{
			SwapPolicy: inferencev1alpha1.ModelPoolSwapPolicySticky,
			Members: []inferencev1alpha1.ModelPoolMember{
				{InferenceServiceRef: corev1.LocalObjectReference{Name: "open-vllm"}},
				{InferenceServiceRef: corev1.LocalObjectReference{Name: "keyed-vllm"}},
			},
			Default: "open-vllm",
		},
	}
	if err := c.Create(context.Background(), pool); err != nil {
		t.Fatalf("create modelpool: %v", err)
	}
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-router", Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneProxy,
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "open-vllm", InferenceServiceRef: &corev1.LocalObjectReference{Name: "open-vllm"}},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{Name: "all", Route: inferencev1alpha1.RuleRoute{Backends: []string{"open-vllm"}}},
			},
			DefaultRoute: "open-vllm",
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := &ModelRouterReconciler{Client: c, Scheme: clientgoscheme.Scheme, RouterProxyImage: "test-image"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterConditionBackendsReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ModelRouterReasonBackendAPIKeyUnsupported {
		t.Fatalf("BackendsReady = %+v, want False/%s", cond, ModelRouterReasonBackendAPIKeyUnsupported)
	}
	if !strings.Contains(cond.Message, `"open-vllm"`) || !strings.Contains(cond.Message, "ModelPool") {
		t.Errorf("BackendsReady message %q does not name the backend and the pool", cond.Message)
	}
	if len(fresh.Status.Backends) != 1 || fresh.Status.Backends[0].Healthy ||
		!strings.Contains(fresh.Status.Backends[0].Message, `"keyed-vllm"`) {
		t.Errorf("status.backends = %+v, want open-vllm unhealthy with the keyed sibling named", fresh.Status.Backends)
	}
}
