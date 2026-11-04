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
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// classifierRouter builds the router the #441 assessment ran on kind: a pii
// rule fail-closed to a local backend, the defaultRoute on a cloud backend,
// and the given classification mode. Under a mode the proxy cannot honor, a
// request whose body holds an SSN and carries no classification header
// matches no rule and goes to the cloud backend.
func classifierRouter(ns, name, isvcName, mode string) *inferencev1alpha1.ModelRouter {
	return &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneProxy,
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "local", Tier: "local", InferenceServiceRef: corev1LocalRef(isvcName)},
				{
					Name: "cloud",
					Tier: "cloud",
					External: &inferencev1alpha1.ExternalProvider{
						Provider: "anthropic",
						Model:    "claude-opus-4-7",
						URL:      "https://api.anthropic.com",
					},
				},
			},
			Rules: []inferencev1alpha1.RouterRule{{
				Name:       "pii-stays-local",
				Match:      &inferencev1alpha1.RuleMatch{DataClassification: []string{"pii"}},
				Route:      inferencev1alpha1.RuleRoute{Backends: []string{"local"}},
				FailClosed: true,
			}},
			DefaultRoute: "cloud",
			Policy: &inferencev1alpha1.RouterPolicy{
				Classification: &inferencev1alpha1.ClassificationPolicy{Mode: mode},
			},
		},
	}
}

// makeLocalBackend creates the InferenceService a classifierRouter's local
// backend references, so the backend resolves to a Service address and the
// router compiles.
func makeLocalBackend(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: name,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 8080},
		},
	}
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create backend isvc %s: %v", name, err)
	}
}

// assertClassifierRefused checks the router reports the refusal: Validated is
// False with reason ClassifierNotImplemented, the message names the mode,
// header-only and #441, and the phase is Failed.
func assertClassifierRefused(t *testing.T, c client.Client, ns, name, mode string) {
	t.Helper()
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterConditionValidated)
	if cond == nil {
		t.Fatalf("no %s condition; conditions: %+v", ModelRouterConditionValidated, fresh.Status.Conditions)
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != "ClassifierNotImplemented" {
		t.Errorf("mode %q: Validated = %s/%s (%q), want False/ClassifierNotImplemented",
			mode, cond.Status, cond.Reason, cond.Message)
	}
	for _, want := range []string{`"` + mode + `"`, "header-only", "#441"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("Validated message %q does not mention %s", cond.Message, want)
		}
	}
	if fresh.Status.Phase != PhaseFailed {
		t.Errorf("phase = %q, want %q", fresh.Status.Phase, PhaseFailed)
	}
}

// compiledClassificationMode reads the classification mode out of the
// router-proxy ConfigMap, the bytes the proxy loads at startup.
func compiledClassificationMode(t *testing.T, cm *corev1.ConfigMap) string {
	t.Helper()
	var wire struct {
		Policy struct {
			Classification struct {
				Mode string `json:"mode"`
			} `json:"classification"`
		} `json:"policy"`
	}
	if err := json.Unmarshal([]byte(cm.Data[routerProxyConfigKey]), &wire); err != nil {
		t.Fatalf("parse router-proxy config: %v", err)
	}
	return wire.Policy.Classification.Mode
}

// TestModelRouterProxyRefusesUnimplementedClassifier: a new Proxy router that
// asks for the detector or hybrid classifier is not Validated, and the
// router-proxy is not provisioned, so no proxy ever serves a config that
// claims a classifier it does not run.
func TestModelRouterProxyRefusesUnimplementedClassifier(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()

	for _, mode := range []string{"detector", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			ns := newTestNamespace(t, c)
			makeLocalBackend(t, c, ns, "qwen")
			mr := classifierRouter(ns, "pii-router", "qwen", mode)
			if err := c.Create(context.Background(), mr); err != nil {
				t.Fatalf("create modelrouter: %v", err)
			}

			r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}
			reconcileRouterOnce(t, r, ns, mr.Name)

			assertClassifierRefused(t, c, ns, mr.Name, mode)
			name := routerProxyResourceName(mr.Name)
			for _, obj := range []client.Object{&corev1.ConfigMap{}, &appsv1.Deployment{}} {
				err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, obj)
				if !apierrors.IsNotFound(err) {
					t.Errorf("router-proxy %T %s/%s: want NotFound, got err=%v", obj, ns, name, err)
				}
			}
		})
	}
}

// TestModelRouterProxyKeepsLastConfigWhenClassifierRefused: an invalid spec
// leaves the router-proxy objects as they were (the reconciler's behavior for
// every static validation failure), so a header-only router edited to
// detector keeps serving its header-only config. The controller never writes
// the detector mode into the ConfigMap, and the Deployment's config hash does
// not move, so no rollout picks up the refused spec.
func TestModelRouterProxyKeepsLastConfigWhenClassifierRefused(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	makeLocalBackend(t, c, ns, "qwen")
	mr := classifierRouter(ns, "edited-router", "qwen", classificationModeHeaderOnly)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}
	reconcileRouterOnce(t, r, ns, mr.Name)

	key := types.NamespacedName{Name: routerProxyResourceName(mr.Name), Namespace: ns}
	before := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), key, before); err != nil {
		t.Fatalf("header-only router has no router-proxy ConfigMap: %v", err)
	}
	if got := compiledClassificationMode(t, before); got != classificationModeHeaderOnly {
		t.Fatalf("compiled mode = %q, want %q", got, classificationModeHeaderOnly)
	}
	deployBefore := &appsv1.Deployment{}
	if err := c.Get(context.Background(), key, deployBefore); err != nil {
		t.Fatalf("header-only router has no router-proxy Deployment: %v", err)
	}

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	fresh.Spec.Policy.Classification.Mode = "detector"
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("edit mode to detector: %v", err)
	}
	reconcileRouterOnce(t, r, ns, mr.Name)

	assertClassifierRefused(t, c, ns, mr.Name, "detector")
	after := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), key, after); err != nil {
		t.Fatalf("router-proxy ConfigMap gone after the refused edit: %v", err)
	}
	if got := compiledClassificationMode(t, after); got != classificationModeHeaderOnly {
		t.Errorf("compiled mode after the refused edit = %q, want the last good %q", got, classificationModeHeaderOnly)
	}
	if after.Data[routerProxyConfigKey] != before.Data[routerProxyConfigKey] {
		t.Errorf("router-proxy config changed after the refused edit:\nbefore: %s\nafter:  %s",
			before.Data[routerProxyConfigKey], after.Data[routerProxyConfigKey])
	}
	deployAfter := &appsv1.Deployment{}
	if err := c.Get(context.Background(), key, deployAfter); err != nil {
		t.Fatalf("router-proxy Deployment gone after the refused edit: %v", err)
	}
	hashBefore := deployBefore.Spec.Template.Annotations[routerProxyConfigHashAnnotation]
	hashAfter := deployAfter.Spec.Template.Annotations[routerProxyConfigHashAnnotation]
	if hashBefore == "" || hashAfter != hashBefore {
		t.Errorf("Deployment config hash moved from %q to %q; the refused spec would roll the proxy", hashBefore, hashAfter)
	}
}
