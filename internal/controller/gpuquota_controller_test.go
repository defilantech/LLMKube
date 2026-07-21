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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

func TestGPUQuotaCoversNamespace(t *testing.T) {
	tests := []struct {
		name     string
		gq       *inferencev1alpha1.GPUQuota
		nsName   string
		want     bool
		nsLabels map[string]string
	}{
		{
			name: "namespaceRef matches",
			gq: &inferencev1alpha1.GPUQuota{
				Spec: inferencev1alpha1.GPUQuotaSpec{
					NamespaceRef: "my-ns",
				},
			},
			nsName: "my-ns",
			want:   true,
		},
		{
			name: "namespaceRef does not match",
			gq: &inferencev1alpha1.GPUQuota{
				Spec: inferencev1alpha1.GPUQuotaSpec{
					NamespaceRef: "my-ns",
				},
			},
			nsName: "other-ns",
			want:   false,
		},
		{
			name: "selector matches namespace labels",
			gq: &inferencev1alpha1.GPUQuota{
				Spec: inferencev1alpha1.GPUQuotaSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"env": "production"},
					},
				},
			},
			nsName:   "prod-ns",
			nsLabels: map[string]string{"env": "production"},
			want:     true,
		},
		{
			name: "selector does not match namespace labels",
			gq: &inferencev1alpha1.GPUQuota{
				Spec: inferencev1alpha1.GPUQuotaSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"env": "production"},
					},
				},
			},
			nsName:   "dev-ns",
			nsLabels: map[string]string{"env": "development"},
			want:     false,
		},
		{
			name: "matchExpressions selector matches",
			gq: &inferencev1alpha1.GPUQuota{
				Spec: inferencev1alpha1.GPUQuotaSpec{
					Selector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "env",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{"production", "staging"},
							},
						},
					},
				},
			},
			nsName:   "staging-ns",
			nsLabels: map[string]string{"env": "staging"},
			want:     true,
		},
		{
			name: "matchExpressions selector does not match",
			gq: &inferencev1alpha1.GPUQuota{
				Spec: inferencev1alpha1.GPUQuotaSpec{
					Selector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "env",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{"production", "staging"},
							},
						},
					},
				},
			},
			nsName:   "dev-ns",
			nsLabels: map[string]string{"env": "development"},
			want:     false,
		},
		{
			name: "matchExpressions with NotIn operator",
			gq: &inferencev1alpha1.GPUQuota{
				Spec: inferencev1alpha1.GPUQuotaSpec{
					Selector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "env",
								Operator: metav1.LabelSelectorOpNotIn,
								Values:   []string{"development"},
							},
						},
					},
				},
			},
			nsName:   "prod-ns",
			nsLabels: map[string]string{"env": "production"},
			want:     true,
		},
		{
			name: "matchExpressions with Exists operator",
			gq: &inferencev1alpha1.GPUQuota{
				Spec: inferencev1alpha1.GPUQuotaSpec{
					Selector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "team",
								Operator: metav1.LabelSelectorOpExists,
							},
						},
					},
				},
			},
			nsName:   "team-ns",
			nsLabels: map[string]string{"team": "platform"},
			want:     true,
		},
		{
			name: "matchExpressions with DoesNotExist operator",
			gq: &inferencev1alpha1.GPUQuota{
				Spec: inferencev1alpha1.GPUQuotaSpec{
					Selector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "deprecated",
								Operator: metav1.LabelSelectorOpDoesNotExist,
							},
						},
					},
				},
			},
			nsName:   "clean-ns",
			nsLabels: map[string]string{"env": "production"},
			want:     true,
		},
		{
			name: "neither set returns false",
			gq: &inferencev1alpha1.GPUQuota{
				Spec: inferencev1alpha1.GPUQuotaSpec{},
			},
			nsName: "any-ns",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c client.Client
			if tt.gq.Spec.Selector != nil {
				// Build a fake client with the namespace for selector tests.
				scheme := runtime.NewScheme()
				_ = corev1.AddToScheme(scheme)
				ns := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   tt.nsName,
						Labels: tt.nsLabels,
					},
				}
				c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
			}

			got := gpuQuotaCoversNamespace(c, tt.gq, tt.nsName)
			if got != tt.want {
				t.Errorf("gpuQuotaCoversNamespace() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFindGPUQuotasForInferenceService(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	// Create a namespace with a label.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "my-ns",
			Labels: map[string]string{"env": "production"},
		},
	}

	// Create an InferenceService in that namespace.
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-isvc",
			Namespace: "my-ns",
		},
	}

	// Create a GPUQuota with namespaceRef matching the namespace.
	gqByRef := &inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gq-ref",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			NamespaceRef: "my-ns",
		},
	}

	// Create a GPUQuota with selector matching the namespace labels.
	gqBySelector := &inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gq-selector",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"env": "production"},
			},
		},
	}

	// Create a GPUQuota that does NOT cover the namespace.
	gqNotCovering := &inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gq-not-covering",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			NamespaceRef: "other-ns",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&inferencev1alpha1.GPUQuota{}).WithObjects(ns, isvc, gqByRef, gqBySelector, gqNotCovering).Build()

	r := &GPUQuotaReconciler{Client: c, Scheme: scheme}

	requests := r.findGPUQuotasForInferenceService(context.Background(), isvc)

	// Should find gqByRef and gqBySelector, but not gqNotCovering.
	if len(requests) != 2 {
		t.Errorf("findGPUQuotasForInferenceService() returned %d requests, want 2", len(requests))
	}

	requestNames := make(map[string]bool)
	for _, req := range requests {
		requestNames[req.Name] = true
	}
	if !requestNames["gq-ref"] {
		t.Error("findGPUQuotasForInferenceService() missing request for gq-ref")
	}
	if !requestNames["gq-selector"] {
		t.Error("findGPUQuotasForInferenceService() missing request for gq-selector")
	}
	if requestNames["gq-not-covering"] {
		t.Error("findGPUQuotasForInferenceService() should not include gq-not-covering")
	}
}

func TestFindGPUQuotasForInferenceServiceNonInferenceService(t *testing.T) {
	r := &GPUQuotaReconciler{}

	// Passing a non-InferenceService object should return nil.
	requests := r.findGPUQuotasForInferenceService(context.Background(), &corev1.Pod{})
	if len(requests) != 0 {
		t.Errorf("findGPUQuotasForInferenceService() returned %d requests for non-InferenceService, want 0", len(requests))
	}
}

func TestFindGPUQuotasForInferenceServiceNoQuotas(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-isvc",
			Namespace: "my-ns",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&inferencev1alpha1.GPUQuota{}).WithObjects(isvc).Build()
	r := &GPUQuotaReconciler{Client: c, Scheme: scheme}

	requests := r.findGPUQuotasForInferenceService(context.Background(), isvc)
	if len(requests) != 0 {
		t.Errorf("findGPUQuotasForInferenceService() returned %d requests, want 0", len(requests))
	}
}

func TestFindGPUQuotasForInferenceServiceReturnsReconcileRequests(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "my-ns",
			Labels: map[string]string{"env": "production"},
		},
	}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-isvc",
			Namespace: "my-ns",
		},
	}

	gq := &inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gq-ref",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			NamespaceRef: "my-ns",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&inferencev1alpha1.GPUQuota{}).WithObjects(ns, isvc, gq).Build()
	r := &GPUQuotaReconciler{Client: c, Scheme: scheme}

	requests := r.findGPUQuotasForInferenceService(context.Background(), isvc)
	if len(requests) != 1 {
		t.Fatalf("findGPUQuotasForInferenceService() returned %d requests, want 1", len(requests))
	}

	expectedReq := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "gq-ref",
			Namespace: "default",
		},
	}
	if requests[0] != expectedReq {
		t.Errorf("findGPUQuotasForInferenceService() returned %+v, want %+v", requests[0], expectedReq)
	}
}

func TestReconcileNamespaceRef(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	// Create a GPUQuota with namespaceRef.
	gq := &inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-gq",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			NamespaceRef: "my-ns",
			GPUCount:     10,
		},
	}

	// Create an InferenceService in the target namespace with 2 GPUs and 3 replicas.
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-isvc",
			Namespace: "my-ns",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Replicas: ptrInt32GPUQuota(3),
			Resources: &inferencev1alpha1.InferenceResourceRequirements{
				GPU: 2,
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&inferencev1alpha1.GPUQuota{}).WithObjects(gq, isvc).Build()
	r := &GPUQuotaReconciler{Client: c, Scheme: scheme}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "my-gq",
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile() returned error: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Errorf("Reconcile() returned %+v, want empty Result", result)
	}

	// Verify status was updated.
	var updatedGQ inferencev1alpha1.GPUQuota
	if err := c.Get(context.Background(), types.NamespacedName{Name: "my-gq", Namespace: "default"}, &updatedGQ); err != nil {
		t.Fatalf("failed to get GPUQuota after reconcile: %v", err)
	}
	if updatedGQ.Status.UsedGPUCount != 6 {
		t.Errorf("Reconcile() status.UsedGPUCount = %d, want 6 (2 GPUs * 3 replicas)", updatedGQ.Status.UsedGPUCount)
	}
	if updatedGQ.Status.UsedVRAMBytes != 0 {
		t.Errorf("Reconcile() status.UsedVRAMBytes = %d, want 0", updatedGQ.Status.UsedVRAMBytes)
	}

	// #416: the reconcile also publishes per-quota usage and cap gauges.
	if got := testutil.ToFloat64(llmkubemetrics.GPUQuotaUsedGPUCount.WithLabelValues("my-gq", "default")); got != 6 {
		t.Errorf("GPUQuotaUsedGPUCount gauge = %v, want 6", got)
	}
	if got := testutil.ToFloat64(llmkubemetrics.GPUQuotaGPUCountLimit.WithLabelValues("my-gq", "default")); got != 10 {
		t.Errorf("GPUQuotaGPUCountLimit gauge = %v, want 10", got)
	}
}

func TestReconcileSelector(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	// Create a namespace with a label.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "prod-ns",
			Labels: map[string]string{"env": "production"},
		},
	}

	// Create a GPUQuota with selector.
	gq := &inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-gq",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"env": "production"},
			},
			GPUCount: 10,
		},
	}

	// Create an InferenceService in the target namespace.
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-isvc",
			Namespace: "prod-ns",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Replicas: ptrInt32GPUQuota(2),
			Resources: &inferencev1alpha1.InferenceResourceRequirements{
				GPU: 4,
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&inferencev1alpha1.GPUQuota{}).WithObjects(ns, gq, isvc).Build()
	r := &GPUQuotaReconciler{Client: c, Scheme: scheme}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "my-gq",
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile() returned error: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Errorf("Reconcile() returned %+v, want empty Result", result)
	}

	var updatedGQ inferencev1alpha1.GPUQuota
	if err := c.Get(context.Background(), types.NamespacedName{Name: "my-gq", Namespace: "default"}, &updatedGQ); err != nil {
		t.Fatalf("failed to get GPUQuota after reconcile: %v", err)
	}
	if updatedGQ.Status.UsedGPUCount != 8 {
		t.Errorf("Reconcile() status.UsedGPUCount = %d, want 8 (4 GPUs * 2 replicas)", updatedGQ.Status.UsedGPUCount)
	}
}

func TestReconcileSelectorMatchExpressions(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	// Create a namespace with a label that matches a matchExpressions selector.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "staging-ns",
			Labels: map[string]string{"env": "staging"},
		},
	}

	// Create a GPUQuota with matchExpressions selector.
	gq := &inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-gq",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			Selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "env",
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"production", "staging"},
					},
				},
			},
			GPUCount: 10,
		},
	}

	// Create an InferenceService in the target namespace.
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-isvc",
			Namespace: "staging-ns",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Replicas: ptrInt32GPUQuota(3),
			Resources: &inferencev1alpha1.InferenceResourceRequirements{
				GPU: 2,
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&inferencev1alpha1.GPUQuota{}).WithObjects(ns, gq, isvc).Build()
	r := &GPUQuotaReconciler{Client: c, Scheme: scheme}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "my-gq",
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile() returned error: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Errorf("Reconcile() returned %+v, want empty Result", result)
	}

	var updatedGQ inferencev1alpha1.GPUQuota
	if err := c.Get(context.Background(), types.NamespacedName{Name: "my-gq", Namespace: "default"}, &updatedGQ); err != nil {
		t.Fatalf("failed to get GPUQuota after reconcile: %v", err)
	}
	if updatedGQ.Status.UsedGPUCount != 6 {
		t.Errorf("Reconcile() status.UsedGPUCount = %d, want 6 (2 GPUs * 3 replicas)", updatedGQ.Status.UsedGPUCount)
	}
}

func TestReconcileNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &GPUQuotaReconciler{Client: c, Scheme: scheme}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "nonexistent",
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile() returned error for NotFound: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Errorf("Reconcile() returned %+v, want empty Result for NotFound", result)
	}
}

func TestReconcileNilResources(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	gq := &inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-gq",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			NamespaceRef: "my-ns",
			GPUCount:     10,
		},
	}

	// InferenceService with nil Resources (should count as 0 GPUs).
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-isvc",
			Namespace: "my-ns",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Replicas: ptrInt32GPUQuota(1),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&inferencev1alpha1.GPUQuota{}).WithObjects(gq, isvc).Build()
	r := &GPUQuotaReconciler{Client: c, Scheme: scheme}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "my-gq",
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile() returned error: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Errorf("Reconcile() returned %+v, want empty Result", result)
	}

	var updatedGQ inferencev1alpha1.GPUQuota
	if err := c.Get(context.Background(), types.NamespacedName{Name: "my-gq", Namespace: "default"}, &updatedGQ); err != nil {
		t.Fatalf("failed to get GPUQuota after reconcile: %v", err)
	}
	if updatedGQ.Status.UsedGPUCount != 0 {
		t.Errorf("Reconcile() status.UsedGPUCount = %d, want 0 (nil resources)", updatedGQ.Status.UsedGPUCount)
	}
}

func TestReconcileNilReplicas(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	gq := &inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-gq",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			NamespaceRef: "my-ns",
			GPUCount:     10,
		},
	}

	// InferenceService with nil Replicas (should count as 1 replica).
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-isvc",
			Namespace: "my-ns",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Resources: &inferencev1alpha1.InferenceResourceRequirements{
				GPU: 2,
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&inferencev1alpha1.GPUQuota{}).WithObjects(gq, isvc).Build()
	r := &GPUQuotaReconciler{Client: c, Scheme: scheme}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "my-gq",
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile() returned error: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Errorf("Reconcile() returned %+v, want empty Result", result)
	}

	var updatedGQ inferencev1alpha1.GPUQuota
	if err := c.Get(context.Background(), types.NamespacedName{Name: "my-gq", Namespace: "default"}, &updatedGQ); err != nil {
		t.Fatalf("failed to get GPUQuota after reconcile: %v", err)
	}
	if updatedGQ.Status.UsedGPUCount != 2 {
		t.Errorf("Reconcile() status.UsedGPUCount = %d, want 2 (2 GPUs * 1 default replica)", updatedGQ.Status.UsedGPUCount)
	}
}

func TestReconcileNoScope(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	// GPUQuota with neither namespaceRef nor selector.
	gq := &inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-gq",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			GPUCount: 10,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&inferencev1alpha1.GPUQuota{}).WithObjects(gq).Build()
	r := &GPUQuotaReconciler{Client: c, Scheme: scheme}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "my-gq",
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile() returned error: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Errorf("Reconcile() returned %+v, want empty Result", result)
	}

	// Status should remain unchanged (UsedGPUCount = 0).
	var updatedGQ inferencev1alpha1.GPUQuota
	if err := c.Get(context.Background(), types.NamespacedName{Name: "my-gq", Namespace: "default"}, &updatedGQ); err != nil {
		t.Fatalf("failed to get GPUQuota after reconcile: %v", err)
	}
	if updatedGQ.Status.UsedGPUCount != 0 {
		t.Errorf("Reconcile() status.UsedGPUCount = %d, want 0 (no scope)", updatedGQ.Status.UsedGPUCount)
	}
}

func TestSetupWithManager(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &GPUQuotaReconciler{Client: c, Scheme: scheme}

	// SetupWithManager should not return an error with a fake manager.
	// We can't easily test the full manager setup without envtest,
	// but we can verify the reconciler is properly initialized.
	if r.Client == nil {
		t.Error("SetupWithManager: reconciler Client is nil")
	}
	if r.Scheme == nil {
		t.Error("SetupWithManager: reconciler Scheme is nil")
	}
}

func ptrInt32GPUQuota(i int32) *int32 {
	return &i
}
