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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// podMetaFixture is a CPU model and service the fake-client tests below
// reconcile, with the reconciler and a reader for the live Deployment.
type podMetaFixture struct {
	t     *testing.T
	c     client.Client
	r     *InferenceServiceReconciler
	model *inferencev1alpha1.Model
	isvc  *inferencev1alpha1.InferenceService
}

func newPodMetaFixture(t *testing.T, emitScrape bool) *podMetaFixture {
	t.Helper()
	scheme := builderTestScheme(t)
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "podmeta-model", Namespace: testBuilderNs},
		Spec: inferencev1alpha1.ModelSpec{
			Source:       "https://huggingface.co/test/model.gguf",
			Format:       "gguf",
			Quantization: "Q4_K_M",
			Hardware:     &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			Resources:    &inferencev1alpha1.ResourceRequirements{CPU: "1", Memory: "1Gi"},
		},
	}
	replicas := int32(1)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "podmeta-svc", Namespace: testBuilderNs, UID: "uid-podmeta"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: model.Name,
			Replicas: &replicas,
			Image:    "ghcr.io/ggml-org/llama.cpp:server",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(model, isvc).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{}).Build()
	return &podMetaFixture{
		t: t, c: c, model: model, isvc: isvc,
		r: &InferenceServiceReconciler{
			Client: c, Scheme: scheme,
			InitContainerImage:    "docker.io/curlimages/curl:8.18.0",
			EmitScrapeAnnotations: emitScrape,
		},
	}
}

// reconcile sets the service's podLabels and podAnnotations, reconciles the
// Deployment, and returns the live object.
func (f *podMetaFixture) reconcile(labels, annotations map[string]string) *appsv1.Deployment {
	f.t.Helper()
	f.isvc.Spec.PodLabels = labels
	f.isvc.Spec.PodAnnotations = annotations
	if _, _, _, _, err := f.r.reconcileDeployment(context.Background(), f.isvc, resolveEffectiveModelCache(f.isvc, f.model), f.model, nil, 1, true, false, f.r.runtimeCachePlan(f.isvc, f.model, resolveEffectiveModelCache(f.isvc, f.model), resolveBackend(f.isvc))); err != nil {
		f.t.Fatalf("reconcileDeployment: %v", err)
	}
	return f.live()
}

func (f *podMetaFixture) live() *appsv1.Deployment {
	f.t.Helper()
	dep := &appsv1.Deployment{}
	if err := f.c.Get(context.Background(), types.NamespacedName{Name: f.isvc.Name, Namespace: f.isvc.Namespace}, dep); err != nil {
		f.t.Fatal(err)
	}
	return dep
}

// A key removed from spec.podLabels or spec.podAnnotations leaves the pod
// template, while keys another tool put there survive and the selector never
// changes (#2287).
func TestReconcileDeploymentPrunesRemovedPodMetadata(t *testing.T) {
	f := newPodMetaFixture(t, false)
	dep := f.reconcile(map[string]string{"team": "a", "tier": "gold"}, map[string]string{"example.com/owner": "ml"})
	if dep.Spec.Template.Labels["team"] != "a" || dep.Spec.Template.Labels["tier"] != "gold" ||
		dep.Spec.Template.Annotations["example.com/owner"] != "ml" {
		t.Fatalf("live template = %v / %v, want the user keys", dep.Spec.Template.Labels, dep.Spec.Template.Annotations)
	}
	selector := dep.Spec.Selector.MatchLabels

	// Another tool adds its own keys to the template.
	dep.Spec.Template.Labels["gitops.example.com/sync"] = "yes"
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = "now"
	if err := f.c.Update(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
	// An unchanged spec restamps the hash over the template with those keys,
	// so the next step changes the template only by its removals.
	hashBefore := f.reconcile(map[string]string{"team": "a", "tier": "gold"},
		map[string]string{"example.com/owner": "ml"}).Annotations[AnnotationDesiredTemplateHash]

	dep = f.reconcile(map[string]string{"team": "a"}, nil)
	if got := dep.Spec.Template.Labels["team"]; got != "a" {
		t.Errorf("label team = %q, want a", got)
	}
	if _, ok := dep.Spec.Template.Labels["tier"]; ok {
		t.Errorf("label tier removed from the spec is still on the template: %v", dep.Spec.Template.Labels)
	}
	if _, ok := dep.Spec.Template.Annotations["example.com/owner"]; ok {
		t.Errorf("annotation removed from the spec is still on the template: %v", dep.Spec.Template.Annotations)
	}
	if dep.Spec.Template.Labels["gitops.example.com/sync"] != "yes" ||
		dep.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] != "now" {
		t.Errorf("another tool's keys were dropped: %v / %v", dep.Spec.Template.Labels, dep.Spec.Template.Annotations)
	}
	if dep.Annotations[AnnotationDesiredTemplateHash] == hashBefore {
		t.Errorf("desired-template hash unchanged after a removal; the change would skip the RolloutPolicy gate")
	}
	if got := dep.Annotations[appliedPodLabelsAnnotation]; got != "team" {
		t.Errorf("applied labels record = %q, want team", got)
	}
	if _, ok := dep.Annotations[appliedPodAnnotationsAnnotation]; ok {
		t.Errorf("applied annotations record kept after the last annotation was removed: %v", dep.Annotations)
	}

	dep = f.reconcile(nil, nil)
	if _, ok := dep.Spec.Template.Labels["team"]; ok {
		t.Errorf("label team still on the template after podLabels was removed: %v", dep.Spec.Template.Labels)
	}
	if _, ok := dep.Annotations[appliedPodLabelsAnnotation]; ok {
		t.Errorf("applied labels record kept after podLabels was removed: %v", dep.Annotations)
	}
	if dep.Spec.Template.Labels["gitops.example.com/sync"] != "yes" {
		t.Errorf("another tool's label was dropped: %v", dep.Spec.Template.Labels)
	}
	if !reflect.DeepEqual(dep.Spec.Selector.MatchLabels, selector) {
		t.Errorf("selector = %v, want it unchanged at %v", dep.Spec.Selector.MatchLabels, selector)
	}
}

// Removing a user annotation that overrode an operator one hands the key back
// to the operator instead of deleting it, and a user label on an operator key
// is never recorded, so it is never pruned.
func TestReconcileDeploymentNeverPrunesOperatorKeys(t *testing.T) {
	f := newPodMetaFixture(t, true)
	dep := f.reconcile(map[string]string{"app": "spoofed"}, map[string]string{"prometheus.io/scrape": "false"})
	if got := dep.Spec.Template.Annotations["prometheus.io/scrape"]; got != "false" {
		t.Fatalf("prometheus.io/scrape = %q, want the user's false", got)
	}
	if _, ok := dep.Annotations[appliedPodLabelsAnnotation]; ok {
		t.Errorf("applied labels record = %q, want none: app is the operator's", dep.Annotations[appliedPodLabelsAnnotation])
	}

	dep = f.reconcile(nil, nil)
	if got := dep.Spec.Template.Annotations["prometheus.io/scrape"]; got != "true" {
		t.Errorf("prometheus.io/scrape = %q, want the operator's true back", got)
	}
	if got := dep.Spec.Template.Labels["app"]; got != f.isvc.Name {
		t.Errorf("label app = %q, want the operator's %q", got, f.isvc.Name)
	}
}
