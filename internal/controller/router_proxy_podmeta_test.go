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
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// spec.proxy podLabels and podAnnotations reach the pod template under the
// operator's own keys, and never the selector (#2282).
func TestRouterDeploymentPodLabelsAndAnnotations(t *testing.T) {
	mr := canonicalModelRouter()
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{
		PodLabels: map[string]string{
			"netpol.example.com/apiserver": "allow",
			"app":                          "spoofed",
		},
		PodAnnotations: map[string]string{
			"example.com/team":              "llm",
			routerProxyConfigHashAnnotation: "spoofed",
		},
	}
	rec := &ModelRouterReconciler{RouterProxyImage: "img"}
	dep := rec.newRouterDeployment(mr, &compiledConfig{Hash: "hash"})

	selector := routerProxySelectorLabels(mr)
	if !reflect.DeepEqual(dep.Spec.Selector.MatchLabels, selector) {
		t.Errorf("selector = %v, want only the operator labels %v", dep.Spec.Selector.MatchLabels, selector)
	}
	labels := dep.Spec.Template.Labels
	if labels["netpol.example.com/apiserver"] != "allow" {
		t.Errorf("template labels = %v, want the user label", labels)
	}
	if labels["app"] != selector["app"] {
		t.Errorf("template app = %q, want the operator's %q", labels["app"], selector["app"])
	}
	annotations := dep.Spec.Template.Annotations
	if annotations["example.com/team"] != "llm" {
		t.Errorf("template annotations = %v, want the user annotation", annotations)
	}
	if annotations[routerProxyConfigHashAnnotation] != "hash" {
		t.Errorf("config hash = %q, want the operator's value", annotations[routerProxyConfigHashAnnotation])
	}
	// The record lists only the keys the user's value landed on.
	if got := dep.Annotations[appliedPodLabelsAnnotation]; got != "netpol.example.com/apiserver" {
		t.Errorf("applied labels record = %q, want netpol.example.com/apiserver", got)
	}
	if got := dep.Annotations[appliedPodAnnotationsAnnotation]; got != "example.com/team" {
		t.Errorf("applied annotations record = %q, want example.com/team", got)
	}
}

// Adding, changing and removing podLabels and podAnnotations reaches the live
// Deployment, a removed key leaves the pods, and keys another tool put on the
// template survive throughout.
func TestReconcileRouterDeploymentPodMetadataLifecycle(t *testing.T) {
	mr := canonicalModelRouter()
	scheme := builderTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr).Build()
	rec := &ModelRouterReconciler{Client: c, Scheme: scheme, RouterProxyImage: "img"}
	ctx := context.Background()
	key := types.NamespacedName{Name: routerProxyResourceName(mr.Name), Namespace: mr.Namespace}
	live := func() *appsv1.Deployment {
		dep := &appsv1.Deployment{}
		if err := c.Get(ctx, key, dep); err != nil {
			t.Fatal(err)
		}
		return dep
	}
	reconcile := func(labels, annotations map[string]string) *appsv1.Deployment {
		t.Helper()
		mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{PodLabels: labels, PodAnnotations: annotations}
		if err := rec.reconcileRouterDeployment(ctx, mr, &compiledConfig{Hash: "hash"}); err != nil {
			t.Fatal(err)
		}
		return live()
	}
	selector := routerProxySelectorLabels(mr)

	dep := reconcile(map[string]string{"a": "1", "b": "2"}, map[string]string{"x": "1"})
	if dep.Spec.Template.Labels["a"] != "1" || dep.Spec.Template.Labels["b"] != "2" ||
		dep.Spec.Template.Annotations["x"] != "1" {
		t.Fatalf("live template = %v / %v, want the user keys", dep.Spec.Template.Labels, dep.Spec.Template.Annotations)
	}

	// Another tool adds its own keys to the template.
	dep.Spec.Template.Labels["gitops.example.com/sync"] = "yes"
	dep.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = "now"
	if err := c.Update(ctx, dep); err != nil {
		t.Fatal(err)
	}

	dep = reconcile(map[string]string{"a": "changed"}, nil)
	if got := dep.Spec.Template.Labels["a"]; got != "changed" {
		t.Errorf("label a = %q, want changed", got)
	}
	if _, ok := dep.Spec.Template.Labels["b"]; ok {
		t.Errorf("label b removed from the spec is still on the template: %v", dep.Spec.Template.Labels)
	}
	if _, ok := dep.Spec.Template.Annotations["x"]; ok {
		t.Errorf("annotation x removed from the spec is still on the template: %v", dep.Spec.Template.Annotations)
	}
	if dep.Spec.Template.Labels["gitops.example.com/sync"] != "yes" ||
		dep.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] != "now" {
		t.Errorf("another tool's keys were dropped: %v / %v", dep.Spec.Template.Labels, dep.Spec.Template.Annotations)
	}
	if got := dep.Annotations[appliedPodLabelsAnnotation]; got != "a" {
		t.Errorf("applied labels record = %q, want a", got)
	}
	if _, ok := dep.Annotations[appliedPodAnnotationsAnnotation]; ok {
		t.Errorf("applied annotations record kept after the last annotation was removed: %v", dep.Annotations)
	}

	dep = reconcile(nil, nil)
	if _, ok := dep.Spec.Template.Labels["a"]; ok {
		t.Errorf("label a still on the template after podLabels was removed: %v", dep.Spec.Template.Labels)
	}
	if _, ok := dep.Annotations[appliedPodLabelsAnnotation]; ok {
		t.Errorf("applied labels record kept after podLabels was removed: %v", dep.Annotations)
	}
	if dep.Spec.Template.Labels["gitops.example.com/sync"] != "yes" {
		t.Errorf("another tool's label was dropped: %v", dep.Spec.Template.Labels)
	}
	for k, v := range selector {
		if dep.Spec.Template.Labels[k] != v {
			t.Errorf("operator label %s = %q, want %q", k, dep.Spec.Template.Labels[k], v)
		}
	}
	if !reflect.DeepEqual(dep.Spec.Selector.MatchLabels, selector) {
		t.Errorf("selector = %v, want it unchanged at %v", dep.Spec.Selector.MatchLabels, selector)
	}
}

// With the operator not emitting scrape annotations, a prometheus.io key the
// user sets through podAnnotations is theirs and must not be cleaned up as a
// leftover of an earlier --emit-scrape-annotations run.
func TestReconcileRouterDeploymentKeepsUserScrapeAnnotations(t *testing.T) {
	mr := canonicalModelRouter()
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{
		PodAnnotations: map[string]string{"prometheus.io/scrape": "true", "prometheus.io/path": "/metrics"},
	}
	scheme := builderTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mr).Build()
	rec := &ModelRouterReconciler{Client: c, Scheme: scheme, RouterProxyImage: "img"}
	ctx := context.Background()
	for i := 0; i < 2; i++ { // create, then the update path
		if err := rec.reconcileRouterDeployment(ctx, mr, &compiledConfig{Hash: "hash"}); err != nil {
			t.Fatal(err)
		}
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Name: routerProxyResourceName(mr.Name), Namespace: mr.Namespace}, dep); err != nil {
		t.Fatal(err)
	}
	if got := dep.Spec.Template.Annotations["prometheus.io/scrape"]; got != "true" {
		t.Errorf("prometheus.io/scrape = %q, want the user's true", got)
	}
	if got := dep.Spec.Template.Annotations["prometheus.io/path"]; got != "/metrics" {
		t.Errorf("prometheus.io/path = %q, want the user's /metrics", got)
	}
}
