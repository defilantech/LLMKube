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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// sharingISvc builds a minimal InferenceService with the given GPU count and
// sharing spec (nil sharing means the field is unset).
func sharingISvc(gpu int32, sharing *inferencev1alpha1.GPUSharingSpec) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "sharing-isvc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "sharing-model",
			Resources: &inferencev1alpha1.InferenceResourceRequirements{
				GPU:        gpu,
				GPUSharing: sharing,
			},
		},
	}
}

// sharingModel builds a minimal Ready Model with the given GPU hardware spec
// (nil means no hardware section at all).
func sharingModel(gpu *inferencev1alpha1.GPUSpec) *inferencev1alpha1.Model {
	m := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "sharing-model", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/model.gguf",
			Format: "gguf",
		},
		Status: inferencev1alpha1.ModelStatus{Phase: "Ready", Path: "/tmp/llmkube/models/m.gguf"},
	}
	if gpu != nil {
		m.Spec.Hardware = &inferencev1alpha1.HardwareSpec{Accelerator: "cuda", GPU: gpu}
	}
	return m
}

func TestResolveGPUSharing(t *testing.T) {
	pool := map[string]string{"llmkube.dev/gpu-pool": "shared"}

	tests := []struct {
		name    string
		isvc    *inferencev1alpha1.InferenceService
		model   *inferencev1alpha1.Model
		pool    map[string]string
		want    gpuSharingResolution
		wantErr string
	}{
		{
			name:  "unset gpuSharing is exclusive with today's defaults",
			isvc:  sharingISvc(2, nil),
			model: sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia"}),
			want: gpuSharingResolution{
				resourceName:  corev1.ResourceName("nvidia.com/gpu"),
				tolerationKey: "nvidia.com/gpu",
			},
		},
		{
			name:  "explicit exclusive matches unset",
			isvc:  sharingISvc(2, &inferencev1alpha1.GPUSharingSpec{Mode: inferencev1alpha1.GPUSharingModeExclusive}),
			model: sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia"}),
			want: gpuSharingResolution{
				resourceName:  corev1.ResourceName("nvidia.com/gpu"),
				tolerationKey: "nvidia.com/gpu",
			},
		},
		{
			name:  "exclusive AMD Vulkan keeps the dri-render resource",
			isvc:  sharingISvc(1, nil),
			model: sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "amd", Runtime: "vulkan"}),
			want: gpuSharingResolution{
				resourceName:  vulkanDRIResourceName,
				tolerationKey: string(vulkanDRIResourceName),
			},
		},
		{
			name: "partitioned resolves the MIG resource and toleration",
			isvc: sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{
				Mode:    inferencev1alpha1.GPUSharingModePartitioned,
				Profile: "1g.24gb",
			}),
			model: sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia"}),
			want: gpuSharingResolution{
				resourceName:  corev1.ResourceName("nvidia.com/mig-1g.24gb"),
				tolerationKey: "nvidia.com/mig-1g.24gb",
			},
		},
		{
			name: "partitioned respects the tolerationKey override",
			isvc: sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{
				Mode:    inferencev1alpha1.GPUSharingModePartitioned,
				Profile: "3g.90gb",
			}),
			model: sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia", TolerationKey: "b200-pool"}),
			want: gpuSharingResolution{
				resourceName:  corev1.ResourceName("nvidia.com/mig-3g.90gb"),
				tolerationKey: "b200-pool",
			},
		},
		{
			name: "partitioned with a vendorless model defaults to NVIDIA",
			isvc: sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{
				Mode:    inferencev1alpha1.GPUSharingModePartitioned,
				Profile: "2g.48gb",
			}),
			model: sharingModel(nil),
			want: gpuSharingResolution{
				resourceName:  corev1.ResourceName("nvidia.com/mig-2g.48gb"),
				tolerationKey: "nvidia.com/mig-2g.48gb",
			},
		},
		{
			name: "partitioned requires exactly one GPU",
			isvc: sharingISvc(4, &inferencev1alpha1.GPUSharingSpec{
				Mode:    inferencev1alpha1.GPUSharingModePartitioned,
				Profile: "1g.24gb",
			}),
			model:   sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia"}),
			wantErr: "requires exactly 1 GPU, got 4",
		},
		{
			name: "model hardware GPU count also violates the single-device rule",
			isvc: sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{
				Mode:    inferencev1alpha1.GPUSharingModePartitioned,
				Profile: "1g.24gb",
			}),
			model:   sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia", Count: 2}),
			wantErr: "requires exactly 1 GPU, got 2",
		},
		{
			name: "partitioned rejects non-NVIDIA vendors",
			isvc: sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{
				Mode:    inferencev1alpha1.GPUSharingModePartitioned,
				Profile: "1g.24gb",
			}),
			model:   sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "amd"}),
			wantErr: "not supported for GPU vendor",
		},
		{
			name: "partitioned rejects a resourceName override",
			isvc: sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{
				Mode:    inferencev1alpha1.GPUSharingModePartitioned,
				Profile: "1g.24gb",
			}),
			model:   sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia", ResourceName: "nvidia.com/mig-7g.192gb"}),
			wantErr: "conflicts with hardware.gpu.resourceName",
		},
		{
			name: "partitioned rejects a malformed profile",
			isvc: sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{
				Mode:    inferencev1alpha1.GPUSharingModePartitioned,
				Profile: "24gb",
			}),
			model:   sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia"}),
			wantErr: "not a valid MIG profile",
		},
		{
			name:  "shared schedules onto the configured pool",
			isvc:  sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{Mode: inferencev1alpha1.GPUSharingModeShared}),
			model: sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia"}),
			pool:  pool,
			want: gpuSharingResolution{
				resourceName:  corev1.ResourceName("nvidia.com/gpu"),
				tolerationKey: "nvidia.com/gpu",
				nodeSelector:  pool,
			},
		},
		{
			name:    "shared without a configured pool is rejected",
			isvc:    sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{Mode: inferencev1alpha1.GPUSharingModeShared}),
			model:   sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia"}),
			wantErr: "requires a configured shared pool",
		},
		{
			name:  "shared on an AMD APU needs no pool (iGPU co-location)",
			isvc:  sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{Mode: inferencev1alpha1.GPUSharingModeShared}),
			model: sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "amd", Runtime: "vulkan"}),
			want: gpuSharingResolution{
				resourceName:  vulkanDRIResourceName,
				tolerationKey: string(vulkanDRIResourceName),
			},
		},
		{
			name:    "shared requires exactly one GPU",
			isvc:    sharingISvc(2, &inferencev1alpha1.GPUSharingSpec{Mode: inferencev1alpha1.GPUSharingModeShared}),
			model:   sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia"}),
			pool:    pool,
			wantErr: "requires exactly 1 GPU, got 2",
		},
		{
			name: "sharing modes reject DRA resource claims",
			isvc: sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{Mode: inferencev1alpha1.GPUSharingModeShared}),
			model: sharingModel(&inferencev1alpha1.GPUSpec{
				Enabled:        true,
				Vendor:         "nvidia",
				ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu-claim"}},
			}),
			pool:    pool,
			wantErr: "cannot be combined with hardware.gpu.resourceClaims",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveGPUSharing(tt.isvc, tt.model, tt.pool)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got resolution %+v", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.resourceName != tt.want.resourceName {
				t.Errorf("resourceName = %q, want %q", got.resourceName, tt.want.resourceName)
			}
			if got.tolerationKey != tt.want.tolerationKey {
				t.Errorf("tolerationKey = %q, want %q", got.tolerationKey, tt.want.tolerationKey)
			}
			if len(got.nodeSelector) != len(tt.want.nodeSelector) {
				t.Errorf("nodeSelector = %v, want %v", got.nodeSelector, tt.want.nodeSelector)
			}
			for k, v := range tt.want.nodeSelector {
				if got.nodeSelector[k] != v {
					t.Errorf("nodeSelector[%q] = %q, want %q", k, got.nodeSelector[k], v)
				}
			}
		})
	}
}

func TestParseGPUSharingSharedPoolSelector(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{name: "empty means no pool", in: "", want: nil},
		{name: "whitespace only means no pool", in: "   ", want: nil},
		{name: "single pair", in: "llmkube.dev/gpu-pool=shared", want: map[string]string{"llmkube.dev/gpu-pool": "shared"}},
		{name: "multiple pairs with spaces", in: " a=b , c=d ", want: map[string]string{"a": "b", "c": "d"}},
		{name: "missing separator", in: "not-a-pair", wantErr: true},
		{name: "empty value", in: "key=", wantErr: true},
		{name: "empty key", in: "=value", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseGPUSharingSharedPoolSelector(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("got[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestConstructDeploymentGPUSharing proves the resolver's output actually
// lands on the pod: the MIG resource limit + toleration for partitioned, and
// the shared-pool nodeSelector (merged under the user's own) for shared.
func TestConstructDeploymentGPUSharing(t *testing.T) {
	t.Run("partitioned pod requests the MIG resource and tolerates its taint", func(t *testing.T) {
		r := &InferenceServiceReconciler{DefaultFSGroup: 102}
		isvc := sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{
			Mode:    inferencev1alpha1.GPUSharingModePartitioned,
			Profile: "1g.24gb",
		})
		model := sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia"})

		deployment := r.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "")

		container := deployment.Spec.Template.Spec.Containers[0]
		if _, ok := container.Resources.Limits["nvidia.com/mig-1g.24gb"]; !ok {
			t.Fatalf("expected nvidia.com/mig-1g.24gb limit, got %v", container.Resources.Limits)
		}
		if _, ok := container.Resources.Limits["nvidia.com/gpu"]; ok {
			t.Fatalf("whole-GPU limit must not be requested alongside the MIG resource: %v", container.Resources.Limits)
		}
		var found bool
		for _, tol := range deployment.Spec.Template.Spec.Tolerations {
			if tol.Key == "nvidia.com/mig-1g.24gb" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected a toleration keyed nvidia.com/mig-1g.24gb, got %v", deployment.Spec.Template.Spec.Tolerations)
		}
	})

	t.Run("shared pod carries the pool selector under the user's own", func(t *testing.T) {
		r := &InferenceServiceReconciler{
			DefaultFSGroup:       102,
			GPUSharingSharedPool: map[string]string{"llmkube.dev/gpu-pool": "shared", "zone": "pool-default"},
		}
		isvc := sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{Mode: inferencev1alpha1.GPUSharingModeShared})
		isvc.Spec.NodeSelector = map[string]string{"zone": "user-zone"}
		model := sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia"})

		deployment := r.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "")

		ns := deployment.Spec.Template.Spec.NodeSelector
		if ns["llmkube.dev/gpu-pool"] != "shared" {
			t.Fatalf("expected pool selector on the pod, got %v", ns)
		}
		if ns["zone"] != "user-zone" {
			t.Fatalf("user nodeSelector must win on key conflict, got %v", ns)
		}
		container := deployment.Spec.Template.Spec.Containers[0]
		if _, ok := container.Resources.Limits["nvidia.com/gpu"]; !ok {
			t.Fatalf("shared pod still requests the ordinary device resource, got %v", container.Resources.Limits)
		}
	})

	// memoryLimitGiB is a quota-accounting declaration, not a runtime cap: its
	// only reader is podVRAMBytes. The API doc used to claim it drove "a
	// memory-cap enforcement flag", which sent an evaluator looking for an
	// isolation guarantee that does not exist (#1312). This asserts the
	// documented behaviour rather than trusting the comment, so the claim
	// cannot come back without a test failing.
	t.Run("memoryLimitGiB reaches nothing in the pod spec", func(t *testing.T) {
		const declared = 24
		r := &InferenceServiceReconciler{
			DefaultFSGroup:       102,
			GPUSharingSharedPool: map[string]string{"llmkube.dev/gpu-pool": "shared"},
		}
		limit := int32(declared)
		isvc := sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{
			Mode:           inferencev1alpha1.GPUSharingModeShared,
			MemoryLimitGiB: &limit,
		})
		model := sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Vendor: "nvidia"})

		deployment := r.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "")
		pod := deployment.Spec.Template
		container := pod.Spec.Containers[0]

		// The declared figure must not appear as any resource quantity.
		for name, q := range container.Resources.Limits {
			if q.Value() == declared || q.Value() == declared*1024*1024*1024 {
				t.Errorf("memoryLimitGiB leaked into resources.limits[%s] = %s", name, q.String())
			}
		}
		for name, q := range container.Resources.Requests {
			if q.Value() == declared || q.Value() == declared*1024*1024*1024 {
				t.Errorf("memoryLimitGiB leaked into resources.requests[%s] = %s", name, q.String())
			}
		}

		// Nor as a runtime flag, an env var, or an annotation. Substring match
		// on "24" would be too eager, so look for the shapes a real cap would
		// take.
		needles := []string{"24Gi", "24GiB", "memoryLimitGiB", "--gpu-memory", "25769803776"}
		for _, arg := range container.Args {
			for _, n := range needles {
				if strings.Contains(arg, n) {
					t.Errorf("memoryLimitGiB leaked into container args: %q contains %q", arg, n)
				}
			}
		}
		for _, env := range container.Env {
			for _, n := range needles {
				if strings.Contains(env.Value, n) {
					t.Errorf("memoryLimitGiB leaked into env %s = %q", env.Name, env.Value)
				}
			}
		}
		for k, v := range pod.Annotations {
			for _, n := range needles {
				if strings.Contains(v, n) {
					t.Errorf("memoryLimitGiB leaked into annotation %s = %q", k, v)
				}
			}
		}
	})
}

// TestGPUSharingParallelismConflict backfills coverage for the mode-based
// parallelism rule, which had none (#1320 review).
func TestGPUSharingParallelismConflict(t *testing.T) {
	tp2 := int32(2)
	cases := []struct {
		name    string
		isvc    *inferencev1alpha1.InferenceService
		wantErr bool
	}{
		{
			name:    "exclusive mode allows explicit TP",
			isvc:    withVLLMTP(sharingISvc(2, nil), &tp2),
			wantErr: false,
		},
		{
			name: "shared mode rejects explicit TP > 1",
			isvc: withVLLMTP(sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{
				Mode: inferencev1alpha1.GPUSharingModeShared,
			}), &tp2),
			wantErr: true,
		},
		{
			name: "partitioned mode rejects explicit TP > 1",
			isvc: withVLLMTP(sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{
				Mode:    inferencev1alpha1.GPUSharingModePartitioned,
				Profile: "1g.24gb",
			}), &tp2),
			wantErr: true,
		},
		{
			name: "shared mode with no explicit parallelism is fine",
			isvc: sharingISvc(1, &inferencev1alpha1.GPUSharingSpec{
				Mode: inferencev1alpha1.GPUSharingModeShared,
			}),
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := gpuSharingParallelismConflict(tc.isvc)
			if (err != nil) != tc.wantErr {
				t.Fatalf("gpuSharingParallelismConflict() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// withVLLMTP sets an explicit vllmConfig.tensorParallelSize on a fixture.
func withVLLMTP(isvc *inferencev1alpha1.InferenceService, tp *int32) *inferencev1alpha1.InferenceService {
	isvc.Spec.VLLMConfig = &inferencev1alpha1.VLLMConfig{TensorParallelSize: tp}
	return isvc
}

// TestParallelismExceedsGPUCount is the regression test for #1320: explicit
// parallelism larger than the pod's GPU count was admitted cleanly and died
// in a crashloop (the shipped cpu-offload sample carried tensorParallelSize 8
// on a 2 GPU spec).
func TestParallelismExceedsGPUCount(t *testing.T) {
	n := func(v int32) *int32 { return &v }
	sglang := func(tp, dp, ep *int32) *inferencev1alpha1.SGLangConfig {
		return &inferencev1alpha1.SGLangConfig{
			TensorParallelSize: tp, DataParallelSize: dp, ExpertParallelSize: ep,
		}
	}
	cases := []struct {
		name    string
		isvc    *inferencev1alpha1.InferenceService
		model   *inferencev1alpha1.Model
		wantErr string // substring; empty = no error
	}{
		{
			name:  "TP nil is fine, auto-derive owns it",
			isvc:  sharingISvc(2, nil),
			model: sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Count: 2}),
		},
		{
			name:  "TP equal to count is fine",
			isvc:  withVLLMTP(sharingISvc(2, nil), n(2)),
			model: sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Count: 2}),
		},
		{
			name:    "TP 8 on 2 GPUs is the shipped-sample bug",
			isvc:    withVLLMTP(sharingISvc(2, nil), n(8)),
			model:   sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Count: 2}),
			wantErr: "tensorParallelSize 8 exceeds the 2 GPU",
		},
		{
			name:  "TP below count is legitimate headroom",
			isvc:  withVLLMTP(sharingISvc(2, nil), n(1)),
			model: sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Count: 2}),
		},
		{
			name: "count 0 skips the check entirely",
			isvc: withVLLMTP(sharingISvc(0, nil), n(8)),
		},
		{
			name:    "model count wins over resources.gpu",
			isvc:    withVLLMTP(sharingISvc(2, nil), n(4)),
			model:   sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Count: 4}),
			wantErr: "",
		},
		{
			name: "sglang tp x dp within count is fine",
			isvc: func() *inferencev1alpha1.InferenceService {
				i := sharingISvc(4, nil)
				i.Spec.SGLangConfig = sglang(n(2), n(2), nil)
				return i
			}(),
			model: sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Count: 4}),
		},
		{
			name: "sglang tp x dp over count fails",
			isvc: func() *inferencev1alpha1.InferenceService {
				i := sharingISvc(2, nil)
				i.Spec.SGLangConfig = sglang(n(2), n(2), nil)
				return i
			}(),
			model:   sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Count: 2}),
			wantErr: "needs 4 GPUs but this pod requests 2",
		},
		{
			name: "sglang ep over count fails",
			isvc: func() *inferencev1alpha1.InferenceService {
				i := sharingISvc(2, nil)
				i.Spec.SGLangConfig = sglang(n(2), nil, n(4))
				return i
			}(),
			model:   sharingModel(&inferencev1alpha1.GPUSpec{Enabled: true, Count: 2}),
			wantErr: "expertParallelSize 4 exceeds",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parallelismExceedsGPUCount(tc.isvc, tc.model)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("parallelismExceedsGPUCount() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("parallelismExceedsGPUCount() = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestParallelismGuardMultiNode: for a spec.multiNode group the world size
// (tensor x pipeline) is checked against members x GPUs per member, and every
// GPU in the group must be a rank.
func TestParallelismGuardMultiNode(t *testing.T) {
	model := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}}
	i32 := func(v int32) *int32 { return &v }
	mk := func(tp, pp *int32, members int) *inferencev1alpha1.InferenceService {
		ms := make([]inferencev1alpha1.MultiNodeMember, members)
		for i := range ms {
			ms[i] = inferencev1alpha1.MultiNodeMember{Node: "n" + string(rune('a'+i))}
		}
		return &inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				Resources:  &inferencev1alpha1.InferenceResourceRequirements{GPU: 1},
				VLLMConfig: &inferencev1alpha1.VLLMConfig{TensorParallelSize: tp, PipelineParallelSize: pp},
				MultiNode:  &inferencev1alpha1.MultiNodeSpec{Members: ms},
			},
		}
	}

	if err := parallelismExceedsGPUCount(mk(i32(2), nil, 2), model); err != nil {
		t.Errorf("TP2 over 2 members x 1 GPU must pass: %v", err)
	}
	if err := parallelismExceedsGPUCount(mk(nil, i32(2), 2), model); err != nil {
		t.Errorf("PP2 with TP auto-derived to 1 over 2 members must pass: %v", err)
	}
	if err := parallelismExceedsGPUCount(mk(nil, nil, 2), model); err != nil {
		t.Errorf("unset TP/PP auto-derives and must pass: %v", err)
	}
	if err := parallelismExceedsGPUCount(mk(i32(2), i32(2), 2), model); err == nil {
		t.Errorf("TP2 x PP2 = 4 over 2 GPUs must fail")
	}
	if err := parallelismExceedsGPUCount(mk(i32(1), nil, 2), model); err == nil {
		t.Errorf("TP1 over 2 members leaves a rank idle and must fail")
	}
}

// TestParallelismGuardSinglePodPipeline: on a single pod the world size is the
// product too, now that pipelineParallelSize exists.
func TestParallelismGuardSinglePodPipeline(t *testing.T) {
	model := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}}
	i32 := func(v int32) *int32 { return &v }
	mk := func(tp, pp *int32, gpus int32) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				Resources:  &inferencev1alpha1.InferenceResourceRequirements{GPU: gpus},
				VLLMConfig: &inferencev1alpha1.VLLMConfig{TensorParallelSize: tp, PipelineParallelSize: pp},
			},
		}
	}
	if err := parallelismExceedsGPUCount(mk(i32(1), i32(2), 2), model); err != nil {
		t.Errorf("TP1 x PP2 on 2 GPUs must pass: %v", err)
	}
	if err := parallelismExceedsGPUCount(mk(i32(2), i32(2), 2), model); err == nil {
		t.Errorf("TP2 x PP2 = 4 on 2 GPUs must fail")
	}
	if err := parallelismExceedsGPUCount(mk(nil, i32(2), 1), model); err == nil {
		t.Errorf("PP2 on 1 GPU must fail")
	}
}
