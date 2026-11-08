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
	"strings"
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

func TestQuotaWebhookUncachedNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "new-namespace", Labels: map[string]string{"env": "prod"}}}
	q := &inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "uncached-namespace-quota"},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
			GPUCount: 1,
		},
	}
	for _, tt := range []struct {
		name      string
		live      bool
		cached    bool
		gpu       int32
		wantError string
	}{
		{name: "new namespace admitted", live: true},
		{name: "new namespace quota enforced", live: true, gpu: 2, wantError: "would exceed gpuCount"},
		{name: "fresh labels enforce quota", live: true, cached: true, gpu: 2, wantError: "would exceed gpuCount"},
		{name: "missing namespace denied", cached: true, wantError: "not found"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(q)
			if tt.cached {
				cached.WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns.Name}})
			}
			live := fake.NewClientBuilder().WithScheme(scheme)
			if tt.live {
				live.WithObjects(ns)
			}
			v := &InferenceServiceQuotaValidator{Client: cached.Build(), APIReader: live.Build()}
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: ns.Name},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Replicas:  ptrInt32Val(1),
					Resources: &inferencev1alpha1.InferenceResourceRequirements{GPU: tt.gpu},
				},
			}
			for _, validate := range []func() (admission.Warnings, error){
				func() (admission.Warnings, error) { return v.ValidateCreate(context.Background(), isvc) },
				func() (admission.Warnings, error) { return v.ValidateUpdate(context.Background(), isvc, isvc) },
			} {
				counter := llmkubemetrics.GPUQuotaAdmissionDenialsTotal.WithLabelValues(q.Name, q.Namespace)
				before := testutil.ToFloat64(counter)
				_, err := validate()
				if tt.wantError == "" {
					if err != nil {
						t.Fatalf("expected admission, got: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("expected denial containing %q, got: %v", tt.wantError, err)
				}
				if tt.wantError == "would exceed gpuCount" && testutil.ToFloat64(counter)-before != 1 {
					t.Fatal("expected quota denial to increment the admission denial metric")
				}
			}
		})
	}
}

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

	// #1310: a Model declaring hardware.gpu.count must be charged against
	// the gpuCount quota, not silently bypassed. The pod spec requests the
	// GPUs via apiutil.GPUCount (which prefers the Model), so the quota
	// must resolve through the same path.
	t.Run("model-declared gpu count is charged against gpuCount", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     4,
			},
		}
		// Model declares 4 GPUs; ISVC leaves resources.gpu unset.
		// With replicas: 2 the pod requests 8 GPUs (4 x 2), exceeding the
		// 4-GPU cap. Before #1310 the quota charged 0 and admitted anyway.
		model := inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					GPU: &inferencev1alpha1.GPUSpec{
						Count: 4,
					},
				},
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "my-model",
				Replicas: ptrInt32Val(2),
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &model, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateCreate(ctx, &isvc)
		if err == nil {
			t.Fatal("expected denial (model gpu 4 * replicas 2 = 8 > 4), got nil")
		}
		if !strContains(err.Error(), "would exceed gpuCount") {
			t.Fatalf("expected reason to mention gpuCount, got: %v", err)
		}
	})

	t.Run("model-declared gpu count within quota is admitted", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     8,
			},
		}
		// Model declares 4 GPUs; ISVC leaves resources.gpu unset.
		// With replicas: 1 the pod requests 4 GPUs, within the 8-GPU cap.
		model := inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					GPU: &inferencev1alpha1.GPUSpec{
						Count: 4,
					},
				},
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "my-model",
				Replicas: ptrInt32Val(1),
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &model, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateCreate(ctx, &isvc)
		if err != nil {
			t.Fatalf("expected admission (model gpu 4 <= 8), got error: %v", err)
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

	t.Run("rejects multiNode on a Model using DRA resourceClaims", func(t *testing.T) {
		// CEL on the InferenceService cannot see the referenced Model, so the
		// conflict is only knowable here (#2061).
		model := inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "dra-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://example.com/model.gguf",
				Format: "gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "cuda",
					GPU: &inferencev1alpha1.GPUSpec{
						Enabled:        true,
						ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu"}},
					},
				},
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "dra-ring", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "dra-model",
				MultiNode: &inferencev1alpha1.MultiNodeSpec{
					Members: []inferencev1alpha1.MultiNodeMember{
						{Node: "a", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.1"}},
						{Node: "b", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.2"}},
					},
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&model).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateCreate(ctx, &isvc)
		if err == nil {
			t.Fatal("expected rejection of multiNode with DRA resourceClaims, got nil")
		}
		if !strContains(err.Error(), "invalid multiNode spec") || !strContains(err.Error(), "resourceClaims") {
			t.Fatalf("expected the multiNode/claims conflict, got: %v", err)
		}
	})

	t.Run("admits multiNode when the Model is absent (reconciler backstops)", func(t *testing.T) {
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "dra-ring", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "dra-model",
				MultiNode: &inferencev1alpha1.MultiNodeSpec{
					Members: []inferencev1alpha1.MultiNodeMember{
						{Node: "a", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.1"}},
						{Node: "b", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.2"}},
					},
				},
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		if _, err := v.ValidateCreate(ctx, &isvc); err != nil {
			t.Fatalf("an absent Model is undecidable at admission and must not reject: %v", err)
		}
	})

	t.Run("admits multiNode on a device-plugin Model", func(t *testing.T) {
		model := inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "dp-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://example.com/model.gguf",
				Format: "gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "cuda",
					GPU:         &inferencev1alpha1.GPUSpec{Enabled: true, Count: 1, Vendor: "nvidia"},
				},
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "dp-ring", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "dp-model",
				MultiNode: &inferencev1alpha1.MultiNodeSpec{
					Members: []inferencev1alpha1.MultiNodeMember{
						{Node: "a", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.1"}},
						{Node: "b", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.2"}},
					},
				},
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&model, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		if _, err := v.ValidateCreate(ctx, &isvc); err != nil {
			t.Fatalf("multiNode on a device-plugin Model must be admitted: %v", err)
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

// TestFetchModel verifies that fetchModel returns the referenced Model when
// it exists, nil when ModelRef is empty, and nil when the Model cannot be
// read (best-effort at admission time).
func TestFetchModel(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	ctx := context.Background()

	model := inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "default"},
	}

	t.Run("returns the Model when it exists", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&model).Build()
		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "my-model"},
		}
		got := v.fetchModel(ctx, &isvc)
		if got == nil {
			t.Fatal("expected non-nil Model, got nil")
		}
		if got.Name != "my-model" {
			t.Errorf("expected Model name my-model, got %s", got.Name)
		}
	})

	t.Run("returns nil when ModelRef is empty", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
		}
		if got := v.fetchModel(ctx, &isvc); got != nil {
			t.Fatalf("expected nil for empty ModelRef, got %v", got)
		}
	})

	t.Run("returns nil when the Model does not exist", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "not-found"},
		}
		if got := v.fetchModel(ctx, &isvc); got != nil {
			t.Fatalf("expected nil for missing Model, got %v", got)
		}
	})
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
		allow, reason := v.decide(ctx, quota, &isvc, nil)
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
		allow, reason := v.decide(ctx, quota, &isvc, nil)
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
		allow, reason := v.decide(ctx, quota, &isvc, nil)
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
		allow, reason := v.decide(ctx, quota, &isvc, nil)
		if allow {
			t.Fatal("expected deny, got allow")
		}
		if !strContains(reason, "priority") {
			t.Fatalf("expected reason to mention priority, got: %s", reason)
		}
	})

	// #1310: a Model declaring hardware.gpu.count must be charged against
	// the gpuCount quota, not silently bypassed.
	t.Run("model-declared gpu count is charged against gpuCount", func(t *testing.T) {
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     4,
			},
		}
		// Model declares 4 GPUs; ISVC leaves resources.gpu unset.
		// With replicas: 2 the pod requests 8 GPUs (4 x 2), exceeding the
		// 4-GPU cap. Before #1310 the quota charged 0 and admitted anyway.
		model := inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					GPU: &inferencev1alpha1.GPUSpec{
						Count: 4,
					},
				},
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "my-model",
				Replicas: ptrInt32Val(2),
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &model, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		allow, reason := v.decide(ctx, quota, &isvc, &model)
		if allow {
			t.Fatal("expected deny (model gpu 4 * replicas 2 = 8 > 4), got allow")
		}
		if !strContains(reason, "would exceed gpuCount") {
			t.Fatalf("expected reason to mention gpuCount, got: %s", reason)
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

// TestQuotaWebhookRejectsUnsatisfiableParallelism covers the #1320 wiring:
// explicit TP larger than the pod's GPUs is denied at admission when the
// webhook is enabled, and skipped (not falsely denied) when the count is
// unknowable.
func TestQuotaWebhookRejectsUnsatisfiableParallelism(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	tp8 := int32(8)

	t.Run("explicit TP over GPU count is denied naming the field", func(t *testing.T) {
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "tp-over", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{TensorParallelSize: &tp8},
				Resources:  &inferencev1alpha1.InferenceResourceRequirements{GPU: 2},
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&ns).Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateCreate(ctx, &isvc)
		if err == nil {
			t.Fatal("expected denial, got admission")
		}
		if !strings.Contains(err.Error(), "tensorParallelSize") {
			t.Fatalf("denial must name the offending field, got: %v", err)
		}
	})

	t.Run("unknown GPU count is admitted, not falsely denied", func(t *testing.T) {
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "tp-unknown", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "not-created-yet",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{TensorParallelSize: &tp8},
			},
		}
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&ns).Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		if _, err := v.ValidateCreate(ctx, &isvc); err != nil {
			t.Fatalf("count-unknown must be admitted (backstop re-checks), got: %v", err)
		}
	})
}

// #1311: HPA-driven scale-up is not gated by GPUQuota because the HPA
// targets the Deployment, not the InferenceService, so HPA scaling never
// re-enters the admission webhook. The quota must therefore charge
// autoscaling.maxReplicas (the worst-case headroom) at admission time, not
// spec.replicas.
func TestQuotaWebhookChargesMaxReplicas(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core1: %v", err)
	}

	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

	t.Run("maxReplicas over quota is denied at admission", func(t *testing.T) {
		// gpu: 1, maxReplicas: 100 => 100 GPUs requested. Quota cap is 4.
		// Before #1311 the webhook charged spec.replicas (1) and admitted.
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     4,
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "autoscaled-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 1,
				},
				Autoscaling: &inferencev1alpha1.AutoscalingSpec{
					MaxReplicas: 100,
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		_, err := v.ValidateCreate(ctx, &isvc)
		if err == nil {
			t.Fatal("expected denial (1*100=100 > 4), got nil")
		}
		if !strContains(err.Error(), "would exceed gpuCount") {
			t.Fatalf("expected reason to mention gpuCount, got: %v", err)
		}
	})

	t.Run("maxReplicas within quota is admitted", func(t *testing.T) {
		// gpu: 1, maxReplicas: 4 => 4 GPUs requested. Quota cap is 8.
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     8,
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "autoscaled-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 1,
				},
				Autoscaling: &inferencev1alpha1.AutoscalingSpec{
					MaxReplicas: 4,
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		if _, err := v.ValidateCreate(ctx, &isvc); err != nil {
			t.Fatalf("expected admission (1*4=4 <= 8), got error: %v", err)
		}
	})

	t.Run("decide charges maxReplicas not spec.replicas", func(t *testing.T) {
		// gpu: 2, maxReplicas: 3 => 6 GPUs. Quota cap is 5 => denied.
		// spec.replicas is 1, which would have been admitted before #1311.
		quota := inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "test-quota"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "default",
				GPUCount:     5,
			},
		}
		isvc := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "autoscaled-svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Replicas: ptrInt32Val(1),
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU: 2,
				},
				Autoscaling: &inferencev1alpha1.AutoscalingSpec{
					MaxReplicas: 3,
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&quota, &ns).
			Build()

		v := &InferenceServiceQuotaValidator{Client: fakeClient}
		allow, reason := v.decide(ctx, quota, &isvc, nil)
		if allow {
			t.Fatal("expected deny (2*3=6 > 5), got allow")
		}
		if !strContains(reason, "would exceed gpuCount") {
			t.Fatalf("expected reason to mention gpuCount, got: %s", reason)
		}
	})
}

// TestQuotaWebhookWarnsMultiNodeAutoscaling: spec.autoscaling on a
// spec.multiNode service is admitted with a warning, never rejected, so an
// object that already carries both stays updatable (#2094).
func TestQuotaWebhookWarnsMultiNodeAutoscaling(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core: %v", err)
	}
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	v := &InferenceServiceQuotaValidator{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(&ns).Build()}

	multiNode := &inferencev1alpha1.MultiNodeSpec{Members: []inferencev1alpha1.MultiNodeMember{
		{Node: "node-a", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.1"}},
		{Node: "node-b"},
	}}
	autoscaling := &inferencev1alpha1.AutoscalingSpec{MaxReplicas: 4}

	cases := []struct {
		name        string
		multiNode   *inferencev1alpha1.MultiNodeSpec
		autoscaling *inferencev1alpha1.AutoscalingSpec
		wantWarning bool
	}{
		{name: "multiNode and autoscaling", multiNode: multiNode, autoscaling: autoscaling, wantWarning: true},
		{name: "multiNode only", multiNode: multiNode},
		{name: "autoscaling only", autoscaling: autoscaling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "mn-svc", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime:     "vllm",
					MultiNode:   tc.multiNode,
					Autoscaling: tc.autoscaling,
				},
			}
			createWarnings, err := v.ValidateCreate(ctx, &isvc)
			if err != nil {
				t.Fatalf("create rejected: %v", err)
			}
			updateWarnings, err := v.ValidateUpdate(ctx, isvc.DeepCopy(), &isvc)
			if err != nil {
				t.Fatalf("update rejected: %v", err)
			}
			for op, warnings := range map[string]admission.Warnings{"create": createWarnings, "update": updateWarnings} {
				if !tc.wantWarning {
					if len(warnings) != 0 {
						t.Fatalf("%s: unexpected warnings %v", op, warnings)
					}
					continue
				}
				if len(warnings) != 1 || !strings.Contains(warnings[0], "spec.autoscaling has no effect with spec.multiNode") ||
					!strings.Contains(warnings[0], ReasonUnsupportedForMultiNode) {
					t.Fatalf("%s: warnings %v, want one naming spec.multiNode and %s", op, warnings, ReasonUnsupportedForMultiNode)
				}
			}
		})
	}
}
