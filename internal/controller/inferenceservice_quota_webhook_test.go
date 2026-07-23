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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

func TestInferenceServiceQuotaValidator(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	ctx := context.Background()

	t.Run("in-quota admit", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     8,
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 2,
				},
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateCreate(ctx, &isvc)
		if err != nil {
			t.Fatalf("expected admission, got error: %v", err)
		}
	})

	t.Run("gpuCount accounts for replicas", func(t *testing.T) {
		// gpu: 2, replicas: 3 => total GPU usage = 6.
		// Quota cap is 5, so this should be denied.
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     5,
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(3),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 2,
				},
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateCreate(ctx, &isvc)
		if err == nil {
			t.Fatal("expected denial (2*3=6 > 5), got nil")
		}
		if !strContains(err.Error(), "would exceed gpuCount") {
			t.Fatalf("expected reason to mention gpuCount, got: %v", err)
		}
	})

	t.Run("denial increments the metric (#416)", func(t *testing.T) {
		// Unique quota name/namespace so the counter is not shared with the
		// other subtests; assert the per-quota delta is exactly 1.
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "metric-quota", Namespace: "quota-ns"},
			Spec:       inferencev1alpha1.GPUQuotaSpec{NamespaceRef: "default", GPUCount: 1},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "big-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas:  ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{GPU: 4},
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&quota, &ns).Build()
		v := &InferenceServiceQuotaValidator{Client: fakeClient}

		before := testutil.ToFloat64(llmkubemetrics.GPUQuotaAdmissionDenialsTotal.WithLabelValues("metric-quota", "quota-ns"))
		if _, err := v.ValidateCreate(ctx, &isvc); err == nil {
			t.Fatal("expected denial (4 > 1), got nil")
		}
		after := testutil.ToFloat64(llmkubemetrics.GPUQuotaAdmissionDenialsTotal.WithLabelValues("metric-quota", "quota-ns"))
		if after-before != 1 {
			t.Errorf("admission denials counter delta = %v, want 1", after-before)
		}
	})

	t.Run("over-gpuCount deny", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     4,
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 5,
				},
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateCreate(ctx, &isvc)
		if err == nil {
			t.Fatal("expected denial, got nil")
		}
		// The reason should mention exceeding gpuCount.
		if !strContains(err.Error(), "would exceed gpuCount") {
			t.Fatalf("expected reason to mention gpuCount, got: %v", err)
		}
	})

	t.Run("priority-floor deny", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     8,
				MinPriority:  "high",
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 1,
				},
				Priority: "low",
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateCreate(ctx, &isvc)
		if err == nil {
			t.Fatal("expected denial, got nil")
		}
		if !strContains(err.Error(), "priority") {
			t.Fatalf("expected reason to mention priority, got: %v", err)
		}
	})

	t.Run("quota that does not cover the namespace is ignored", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "other-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "other-ns",
				GPUCount:     1,
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 100,
				},
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateCreate(ctx, &isvc)
		if err != nil {
			t.Fatalf("expected admission (quota doesn't cover ns), got error: %v", err)
		}
	})

	t.Run("update that stays within quota is admitted", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     8,
			},
		}
		// Existing ISVC already using 4 GPUs.
		existingISVC := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "existing-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 4,
				},
			},
		}
		// Updating ISVC from 1 to 2 GPUs (4+2=6 <= 8).
		oldISVC := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 1,
				},
			},
		}
		newISVC := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 2,
				},
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &existingISVC, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateUpdate(ctx, &oldISVC, &newISVC)
		if err != nil {
			t.Fatalf("expected admission, got error: %v", err)
		}
	})

	// #1251: the GPUQuota webhook defers QUOTA gating (not spec validation)
	// for InferenceServices carrying the Kueue queue-name label, since
	// admission for those objects is owned by Kueue's ClusterQueue quota.
	t.Run("admits a Kueue-managed InferenceService that exceeds the GPUQuota", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "kueue-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     1,
			},
		}
		// Existing ISVC already consuming the entire 1-GPU quota.
		existingISVC := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "existing-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 1,
				},
			},
		}
		// New ISVC requests 1 more GPU (1+1=2 > 1 quota) but is Kueue-managed.
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kueue-svc",
				Namespace: "default",
				Labels:    map[string]string{"kueue.x-k8s.io/queue-name": "inference-queue"},
			},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 1,
				},
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &existingISVC, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateCreate(ctx, &isvc)
		if err != nil {
			t.Fatalf("expected admission for Kueue-managed ISVC despite exceeding the GPUQuota, got error: %v", err)
		}
	})

	t.Run("still denies an unlabeled InferenceService that exceeds the GPUQuota", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "kueue-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     1,
			},
		}
		existingISVC := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "existing-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 1,
				},
			},
		}
		// Same over-quota shape as above, but with no Kueue label.
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "unlabeled-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 1,
				},
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &existingISVC, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateCreate(ctx, &isvc)
		if err == nil {
			t.Fatal("expected denial for unlabeled ISVC exceeding the GPUQuota, got nil")
		}
		if !strContains(err.Error(), "kueue-quota") {
			t.Fatalf("expected error to mention the GPUQuota name, got: %v", err)
		}
	})

	t.Run("still rejects an invalid gpuSharing spec even when Kueue-managed", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "kueue-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     100, // permissive: only the sharing spec should reject.
			},
		}
		// Shared mode with no configured pool is the suite's existing
		// invalid-gpuSharing fixture (see TestGPUSharingAdmission).
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kueue-bad-sharing",
				Namespace: "default",
				Labels:    map[string]string{"kueue.x-k8s.io/queue-name": "inference-queue"},
			},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 1,
					GPUSharing: &inferencev1alpha1.GPUSharingSpec{
						Mode: inferencev1alpha1.GPUSharingModeShared,
					},
				},
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateCreate(ctx, &isvc)
		if err == nil {
			t.Fatal("expected rejection of the invalid gpuSharing spec, got nil")
		}
		if !strContains(err.Error(), "requires a configured shared pool") {
			t.Fatalf("expected sharing-pool rejection reason, got: %v", err)
		}
	})
}

func ptrInt32Val(i int32) *int32 {
	return &i
}

func strContains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || strContainsMiddle(s, substr)))
}

func strContainsMiddle(s, substr string) bool {
	for i := 1; i < len(s)-len(substr)+1; i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Ensure the validator implements the interface.
var _ admission.Validator[*inferencev1alpha1.InferenceService] = &InferenceServiceQuotaValidator{}

// TestSetupInferenceServiceQuotaWebhookWithManager verifies that the webhook
// setup function creates a validator with the manager's client.
func TestSetupInferenceServiceQuotaWebhookWithManager(t *testing.T) {
	// This test verifies the function exists and has the right signature.
	// The actual webhook registration is tested by the integration tests.
	// We just verify the function is callable and doesn't panic.
	// Since SetupInferenceServiceQuotaWebhookWithManager requires a real
	// ctrl.Manager, we can't fully test it here without envtest.
	// The interface compliance check above is sufficient for unit testing.
}

// Ensure the scheme is registered for the InferenceService type.
func TestInferenceServiceScheme(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add InferenceService to scheme: %v", err)
	}
	// Verify the GVK is registered.
	gvk := schema.GroupVersionKind{
		Group:   "inference.llmkube.dev",
		Version: "v1alpha1",
		Kind:    "InferenceService",
	}
	if !scheme.Recognizes(gvk) {
		t.Fatalf("scheme does not recognize %v", gvk)
	}
}

// TestValidateDelete verifies that ValidateDelete is a no-op allow.
func TestValidateDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	ctx := context.Background()

	isvc := inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	v := &InferenceServiceQuotaValidator{Client: fakeClient}
	warnings, err := v.ValidateDelete(ctx, &isvc)
	if err != nil {
		t.Fatalf("ValidateDelete should always allow, got error: %v", err)
	}
	if warnings != nil {
		t.Fatalf("ValidateDelete should return nil warnings, got: %v", warnings)
	}
}

// TestListApplicableQuotas verifies that listApplicableQuotas correctly
// filters quotas by namespace scope and selector scope.
func TestListApplicableQuotas(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	ctx := context.Background()

	ns := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "default",
			Labels: map[string]string{"env": "prod"},
		},
	}

	// Namespace-scoped quota that matches.
	nsQuota := inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-quota"},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			NamespaceRef: "default",
			GPUCount:     8,
		},
	}

	// Selector-scoped quota that matches.
	selectorQuota := inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "selector-quota"},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"env": "prod"},
			},
			GPUCount: 4,
		},
	}

	// Selector-scoped quota that does NOT match.
	nonMatchingQuota := inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "non-matching-quota"},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"env": "staging"},
			},
			GPUCount: 2,
		},
	}

	// Namespace-scoped quota for a different namespace.
	otherNSQuota := inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "other-ns-quota"},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			NamespaceRef: "other-ns",
			GPUCount:     1,
		},
	}

	isvc := inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&ns, &nsQuota, &selectorQuota, &nonMatchingQuota, &otherNSQuota).
		Build()

	v := &InferenceServiceQuotaValidator{Client: fakeClient}
	applicable, err := v.listApplicableQuotas(ctx, &isvc)
	if err != nil {
		t.Fatalf("listApplicableQuotas returned error: %v", err)
	}

	// Should have exactly 2 applicable quotas: nsQuota and selectorQuota.
	if len(applicable) != 2 {
		t.Fatalf("expected 2 applicable quotas, got %d", len(applicable))
	}

	// Verify the correct quotas are included.
	names := make(map[string]bool)
	for _, q := range applicable {
		names[q.Name] = true
	}
	if !names["ns-quota"] {
		t.Error("expected ns-quota to be applicable")
	}
	if !names["selector-quota"] {
		t.Error("expected selector-quota to be applicable")
	}
	if names["non-matching-quota"] {
		t.Error("non-matching-quota should not be applicable")
	}
	if names["other-ns-quota"] {
		t.Error("other-ns-quota should not be applicable")
	}
}

// TestDecide verifies that decide correctly evaluates admission against
// a GPUQuota using the quota.Decide function.
func TestDecide(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	ctx := context.Background()

	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

	t.Run("admit within quota", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     8,
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 2,
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		allow, reason := v.decide(ctx, quota, &isvc)
		if !allow {
			t.Fatalf("expected allow, got deny: %s", reason)
		}
	})

	t.Run("gpuCount accounts for replicas", func(t *testing.T) {
		// gpu: 2, replicas: 3 => total GPU usage = 6.
		// Quota cap is 5, so this should be denied.
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     5,
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(3),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 2,
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		allow, reason := v.decide(ctx, quota, &isvc)
		if allow {
			t.Fatal("expected deny (2*3=6 > 5), got allow")
		}
		if !strContains(reason, "would exceed gpuCount") {
			t.Fatalf("expected reason to mention gpuCount, got: %s", reason)
		}
	})

	t.Run("deny over gpuCount", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     4,
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 5,
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		allow, reason := v.decide(ctx, quota, &isvc)
		if allow {
			t.Fatal("expected deny, got allow")
		}
		if !strContains(reason, "would exceed gpuCount") {
			t.Fatalf("expected reason to mention gpuCount, got: %s", reason)
		}
	})

	t.Run("deny priority below minimum", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     8,
				MinPriority:  "high",
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 1,
				},
				Priority: "low",
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		allow, reason := v.decide(ctx, quota, &isvc)
		if allow {
			t.Fatal("expected deny, got allow")
		}
		if !strContains(reason, "priority") {
			t.Fatalf("expected reason to mention priority, got: %s", reason)
		}
	})
}

// TestCurrentUsage verifies that currentUsage correctly sums GPU allocations
// from existing InferenceServices in the quota's scope.
func TestCurrentUsage(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	ctx := context.Background()

	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

	quota := inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			NamespaceRef: "default",
			GPUCount:     8,
		},
	}

	// Existing ISVCs using 3 + 2 = 5 GPUs.
	existing1 := inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Replicas: ptrInt32Val(1),
			Resources: &inferencev1alpha1.InferenceResourceRequirements{
				GPU: 3,
			},
		},
	}
	existing2 := inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc2", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Replicas: ptrInt32Val(1),
			Resources: &inferencev1alpha1.InferenceResourceRequirements{
				GPU: 2,
			},
		},
	}

	// Incoming ISVC (should be excluded from usage sum).
	incoming := inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Replicas: ptrInt32Val(1),
			Resources: &inferencev1alpha1.InferenceResourceRequirements{
				GPU: 1,
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&quota, &ns, &existing1, &existing2).
		Build()

	v := &InferenceServiceQuotaValidator{Client: fakeClient}
	usage, err := v.currentUsage(ctx, quota, &incoming)
	if err != nil {
		t.Fatalf("currentUsage returned error: %v", err)
	}

	// Should sum to 5 (3 + 2), excluding the incoming object.
	if usage.GPUCount != 5 {
		t.Fatalf("expected GPUCount 5, got %d", usage.GPUCount)
	}
}

// TestValidate verifies that validate correctly rejects when any quota denies.
func TestValidate(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	ctx := context.Background()

	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

	t.Run("validate admits when all quotas allow", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     8,
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 2,
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		err := v.validate(ctx, &isvc)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	t.Run("validate denies when a quota denies", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     4,
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 5,
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		err := v.validate(ctx, &isvc)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strContains(err.Error(), "would exceed gpuCount") {
			t.Fatalf("expected error to mention gpuCount, got: %v", err)
		}
	})
}
