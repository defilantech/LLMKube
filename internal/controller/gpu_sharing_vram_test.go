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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

func vramISvc(name, ns string, gpu int32, replicas int32, sharing *inferencev1alpha1.GPUSharingSpec) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "vram-model",
			Replicas: &replicas,
			Resources: &inferencev1alpha1.InferenceResourceRequirements{
				GPU:        gpu,
				GPUSharing: sharing,
			},
		},
	}
}

func gib32(v int32) *int32 { return &v }

func TestPodVRAMBytes(t *testing.T) {
	sharedWithLimit := &inferencev1alpha1.GPUSharingSpec{
		Mode:           inferencev1alpha1.GPUSharingModeShared,
		MemoryLimitGiB: gib32(24),
	}
	sharedNoLimit := &inferencev1alpha1.GPUSharingSpec{Mode: inferencev1alpha1.GPUSharingModeShared}
	partitioned := &inferencev1alpha1.GPUSharingSpec{
		Mode:    inferencev1alpha1.GPUSharingModePartitioned,
		Profile: "3g.90gb",
	}
	modelWithMemory := sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia", Memory: "16Gi"})

	tests := []struct {
		name      string
		isvc      *inferencev1alpha1.InferenceService
		model     *inferencev1alpha1.Model
		fleetGiB  int
		wantBytes int64
		wantKnown bool
	}{
		{
			name:      "partitioned derives from the profile",
			isvc:      vramISvc("a", "d", 1, 1, partitioned),
			wantBytes: 90 * bytesPerGiB,
			wantKnown: true,
		},
		{
			name:      "shared uses memoryLimitGiB",
			isvc:      vramISvc("b", "d", 1, 1, sharedWithLimit),
			wantBytes: 24 * bytesPerGiB,
			wantKnown: true,
		},
		{
			name:      "shared falls back to the Model's gpu memory",
			isvc:      vramISvc("c", "d", 1, 1, sharedNoLimit),
			model:     modelWithMemory,
			wantBytes: 16 * bytesPerGiB,
			wantKnown: true,
		},
		{
			name:      "shared with neither limit nor model memory is unknown",
			isvc:      vramISvc("d", "d", 1, 1, sharedNoLimit),
			wantKnown: false,
		},
		{
			name:      "exclusive multiplies count by the fleet per-device figure",
			isvc:      vramISvc("e", "d", 2, 1, nil),
			fleetGiB:  192,
			wantBytes: 2 * 192 * bytesPerGiB,
			wantKnown: true,
		},
		{
			name:      "exclusive without fleet config is unknown",
			isvc:      vramISvc("f", "d", 2, 1, nil),
			wantKnown: false,
		},
		{
			name:      "exclusive with zero GPUs is exactly zero, not unknown",
			isvc:      vramISvc("g", "d", 0, 1, nil),
			fleetGiB:  192,
			wantBytes: 0,
			wantKnown: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, known := podVRAMBytes(tt.isvc, tt.model, tt.fleetGiB)
			if known != tt.wantKnown {
				t.Fatalf("known = %v, want %v", known, tt.wantKnown)
			}
			if known && got != tt.wantBytes {
				t.Errorf("bytes = %d, want %d", got, tt.wantBytes)
			}
		})
	}
}

func TestQuotaVRAMAdmission(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vram-ns"}}

	vramQuota := func(capGiB int64) *inferencev1alpha1.GPUQuota {
		return &inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "vram-quota", Namespace: "vram-ns"},
			Spec: inferencev1alpha1.GPUQuotaSpec{
				NamespaceRef: "vram-ns",
				GPUCount:     100, // GPU-count cap never the binding constraint here
				VRAMBytes:    capGiB * bytesPerGiB,
			},
		}
	}
	shared := func(limitGiB int32) *inferencev1alpha1.GPUSharingSpec {
		return &inferencev1alpha1.GPUSharingSpec{
			Mode:           inferencev1alpha1.GPUSharingModeShared,
			MemoryLimitGiB: gib32(limitGiB),
		}
	}

	t.Run("shared within the vram cap is admitted", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vramQuota(96), ns).Build()
		v := &InferenceServiceQuotaValidator{Client: c}
		if _, err := v.ValidateCreate(ctx, vramISvc("s1", "vram-ns", 1, 1, shared(24))); err != nil {
			t.Fatalf("expected admission, got: %v", err)
		}
	})

	t.Run("shared exceeding the vram cap is denied", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vramQuota(96), ns).Build()
		v := &InferenceServiceQuotaValidator{Client: c}
		_, err := v.ValidateCreate(ctx, vramISvc("s2", "vram-ns", 1, 1, shared(128)))
		if err == nil {
			t.Fatal("expected denial (128GiB > 96GiB cap), got nil")
		}
		if !strings.Contains(err.Error(), "would exceed vramBytes") {
			t.Fatalf("expected vramBytes reason, got: %v", err)
		}
	})

	t.Run("vram accounts for replicas", func(t *testing.T) {
		// 24GiB x 5 replicas = 120GiB > 96GiB cap.
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vramQuota(96), ns).Build()
		v := &InferenceServiceQuotaValidator{Client: c}
		_, err := v.ValidateCreate(ctx, vramISvc("s3", "vram-ns", 1, 5, shared(24)))
		if err == nil {
			t.Fatal("expected denial (24*5 > 96), got nil")
		}
		if !strings.Contains(err.Error(), "would exceed vramBytes") {
			t.Fatalf("expected vramBytes reason, got: %v", err)
		}
	})

	t.Run("stored shared usage counts against the cap", func(t *testing.T) {
		existing := vramISvc("stored", "vram-ns", 1, 1, shared(80))
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vramQuota(96), ns, existing).Build()
		v := &InferenceServiceQuotaValidator{Client: c}
		// 80 stored + 24 incoming = 104 > 96.
		_, err := v.ValidateCreate(ctx, vramISvc("s4", "vram-ns", 1, 1, shared(24)))
		if err == nil {
			t.Fatal("expected denial (80+24 > 96), got nil")
		}
		if !strings.Contains(err.Error(), "would exceed vramBytes") {
			t.Fatalf("expected vramBytes reason, got: %v", err)
		}
	})

	t.Run("unknowable footprint is denied when the quota declares a cap", func(t *testing.T) {
		// Exclusive workload, no fleet VRAMPerDeviceGiB configured.
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vramQuota(96), ns).Build()
		v := &InferenceServiceQuotaValidator{Client: c}
		_, err := v.ValidateCreate(ctx, vramISvc("s5", "vram-ns", 1, 1, nil))
		if err == nil {
			t.Fatal("expected denial (unknown footprint vs declared cap), got nil")
		}
		if !strings.Contains(err.Error(), "cannot derive the VRAM footprint") {
			t.Fatalf("expected the actionable unknown-footprint reason, got: %v", err)
		}
	})

	t.Run("unknowable footprint is fine when no cap is declared", func(t *testing.T) {
		q := vramQuota(0) // vramBytes 0 = no cap
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(q, ns).Build()
		v := &InferenceServiceQuotaValidator{Client: c}
		if _, err := v.ValidateCreate(ctx, vramISvc("s6", "vram-ns", 1, 1, nil)); err != nil {
			t.Fatalf("expected admission (no vram cap), got: %v", err)
		}
	})

	t.Run("exclusive footprint derives from the fleet per-device figure", func(t *testing.T) {
		// 1 GPU x 192GiB > 96GiB cap.
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vramQuota(96), ns).Build()
		v := &InferenceServiceQuotaValidator{Client: c, VRAMPerDeviceGiB: 192}
		_, err := v.ValidateCreate(ctx, vramISvc("s7", "vram-ns", 1, 1, nil))
		if err == nil {
			t.Fatal("expected denial (192 > 96), got nil")
		}
		if !strings.Contains(err.Error(), "would exceed vramBytes") {
			t.Fatalf("expected vramBytes reason, got: %v", err)
		}
	})
}

func TestGPUQuotaReconcileVRAM(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = inferencev1alpha1.AddToScheme(scheme)

	gq := &inferencev1alpha1.GPUQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "vram-gq", Namespace: "default"},
		Spec: inferencev1alpha1.GPUQuotaSpec{
			NamespaceRef: "vram-agg-ns",
			GPUCount:     10,
			VRAMBytes:    256 * bytesPerGiB,
		},
	}
	// 24GiB shared x 2 replicas = 48GiB.
	sharedISvc := vramISvc("shared-svc", "vram-agg-ns", 1, 2, &inferencev1alpha1.GPUSharingSpec{
		Mode:           inferencev1alpha1.GPUSharingModeShared,
		MemoryLimitGiB: gib32(24),
	})
	// 90GiB partition x 1 replica.
	partISvc := vramISvc("part-svc", "vram-agg-ns", 1, 1, &inferencev1alpha1.GPUSharingSpec{
		Mode:    inferencev1alpha1.GPUSharingModePartitioned,
		Profile: "3g.90gb",
	})
	// Exclusive with no fleet config: unknown, contributes zero.
	exclISvc := vramISvc("excl-svc", "vram-agg-ns", 2, 1, nil)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&inferencev1alpha1.GPUQuota{}).
		WithObjects(gq, sharedISvc, partISvc, exclISvc).Build()
	r := &GPUQuotaReconciler{Client: c, Scheme: scheme}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "vram-gq", Namespace: "default"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile() returned error: %v", err)
	}

	got := &inferencev1alpha1.GPUQuota{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatalf("get quota: %v", err)
	}

	wantVRAM := (2*24 + 90) * bytesPerGiB // 48 shared + 90 partitioned + 0 unknown-exclusive
	if got.Status.UsedVRAMBytes != wantVRAM {
		t.Errorf("UsedVRAMBytes = %d, want %d", got.Status.UsedVRAMBytes, wantVRAM)
	}

	if used := testutil.ToFloat64(llmkubemetrics.GPUQuotaUsedVRAMBytes.WithLabelValues("vram-gq", "default")); used != float64(wantVRAM) {
		t.Errorf("used vram gauge = %v, want %v", used, float64(wantVRAM))
	}
	if limit := testutil.ToFloat64(llmkubemetrics.GPUQuotaVRAMBytesLimit.WithLabelValues("vram-gq", "default")); limit != float64(256*bytesPerGiB) {
		t.Errorf("vram limit gauge = %v, want %v", limit, float64(256*bytesPerGiB))
	}
}
