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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func placementModel(name, accelerator string) *inferencev1alpha1.Model {
	m := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testBuilderNs},
		Spec: inferencev1alpha1.ModelSpec{
			Source:       "https://huggingface.co/test/model.gguf",
			Format:       "gguf",
			Quantization: "Q4_K_M",
			Resources:    &inferencev1alpha1.ResourceRequirements{CPU: "1", Memory: "1Gi"},
		},
	}
	if accelerator != "" {
		m.Spec.Hardware = &inferencev1alpha1.HardwareSpec{Accelerator: accelerator}
	}
	return m
}

func hailoModel(name string) *inferencev1alpha1.Model {
	m := placementModel(name, "metal")
	m.Spec.Source = "hailo://qwen2.5-instruct:1.5b"
	return m
}

func placementISvc(name, modelRef, runtime string) *inferencev1alpha1.InferenceService {
	replicas := int32(1)
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testBuilderNs},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: modelRef,
			Runtime:  runtime,
			Replicas: &replicas,
			Image:    "ghcr.io/ggml-org/llama.cpp:server",
		},
	}
}

// TestValidateRuntimePlacement: a metal-agent-only runtime has no in-cluster
// backend (resolveBackend would silently fall through to llama.cpp), so it is
// only valid on a Model the metal-agent picks up (accelerator: metal).
func TestValidateRuntimePlacement(t *testing.T) {
	tests := []struct {
		name        string
		runtime     string
		model       *inferencev1alpha1.Model
		wantErrPart string
	}{
		{name: "mlx-server on metal", runtime: "mlx-server", model: placementModel("m", "metal")},
		{name: "omlx on metal", runtime: "omlx", model: placementModel("m", "metal")},
		{name: "mlx-server on cuda", runtime: "mlx-server", model: placementModel("m", "cuda"), wantErrPart: `accelerator "cuda"`},
		{name: "ollama on cpu", runtime: "ollama", model: placementModel("m", "cpu"), wantErrPart: "metal-agent"},
		{name: "vllm-swift with no hardware block", runtime: "vllm-swift", model: placementModel("m", ""), wantErrPart: "accelerator unset"},
		{name: "tensorfold on metal", runtime: "tensorfold", model: placementModel("m", "metal")},
		{name: "tensorfold on cuda", runtime: "tensorfold", model: placementModel("m", "cuda"), wantErrPart: `accelerator "cuda"`},
		{name: "llamacpp on cuda", runtime: "llamacpp", model: placementModel("m", "cuda")},
		{name: "empty on cuda", runtime: "", model: placementModel("m", "cuda")},
		{name: "empty on metal", runtime: "", model: placementModel("m", "metal")},
		{name: "unknown model is undecidable", runtime: "mlx-server", model: nil},
		{name: "ollama on a hailo source", runtime: "ollama", model: hailoModel("m")},
		{name: "empty runtime on a hailo source", runtime: "", model: hailoModel("m")},
		{name: "llamacpp on a hailo source", runtime: "llamacpp", model: hailoModel("m"), wantErrPart: "hailo://"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRuntimePlacement(placementISvc("svc", "m", tc.runtime), tc.model)
			if tc.wantErrPart == "" {
				if err != nil {
					t.Fatalf("validateRuntimePlacement() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Fatalf("validateRuntimePlacement() = %v, want error containing %q", err, tc.wantErrPart)
			}
		})
	}
}

func TestQuotaWebhookRejectsMetalOnlyRuntimeOffMetal(t *testing.T) {
	ctx := context.Background()
	scheme := builderTestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testBuilderNs}}

	t.Run("mlx-server on a CUDA Model is denied", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(ns, placementModel("gpu-model", "cuda")).Build()
		v := &InferenceServiceQuotaValidator{Client: c}
		_, err := v.ValidateCreate(ctx, placementISvc("svc", "gpu-model", "mlx-server"))
		if err == nil {
			t.Fatal("expected denial, got admission")
		}
		if !strings.Contains(err.Error(), "mlx-server") || !strings.Contains(err.Error(), "metal") {
			t.Fatalf("denial must name the runtime and the Metal requirement, got: %v", err)
		}
	})

	t.Run("mlx-server on a Metal Model is admitted", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(ns, placementModel("mac-model", "metal")).Build()
		v := &InferenceServiceQuotaValidator{Client: c}
		if _, err := v.ValidateCreate(ctx, placementISvc("svc", "mac-model", "mlx-server")); err != nil {
			t.Fatalf("metal runtime on a metal Model must be admitted, got: %v", err)
		}
	})

	t.Run("Model not created yet is admitted (reconcile backstop decides)", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
		v := &InferenceServiceQuotaValidator{Client: c}
		if _, err := v.ValidateCreate(ctx, placementISvc("svc", "not-yet", "omlx")); err != nil {
			t.Fatalf("undecidable placement must be admitted, got: %v", err)
		}
	})
}

// TestReconcileDeploymentRefusesMetalOnlyRuntimeOffMetal is the backstop for
// installs without the validating webhook: without it, resolveBackend's
// default would build a llama.cpp Deployment for a CR that asked for
// mlx-server.
func TestReconcileDeploymentRefusesMetalOnlyRuntimeOffMetal(t *testing.T) {
	ctx := context.Background()
	scheme := builderTestScheme(t)
	model := placementModel("gpu-model", "cuda")
	isvc := placementISvc("mlx-on-gpu", "gpu-model", "mlx-server")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(model, isvc).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{}).
		Build()
	r := &InferenceServiceReconciler{Client: c, Scheme: scheme, InitContainerImage: "docker.io/curlimages/curl:8.18.0"}

	dep, _, _, result, err := r.reconcileDeployment(ctx, isvc, resolveEffectiveModelCache(isvc, model), model, nil, 1, true, false)
	if err != nil {
		t.Fatalf("reconcileDeployment() error = %v", err)
	}
	if dep != nil || result == nil {
		t.Fatalf("reconcileDeployment() must stop with a result and no Deployment, got dep=%v result=%v",
			dep != nil, result)
	}

	if err := c.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: testBuilderNs}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("no Deployment may be created for a metal-only runtime off Metal, Get err = %v", err)
	}

	stored := &inferencev1alpha1.InferenceService{}
	if err := c.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: testBuilderNs}, stored); err != nil {
		t.Fatalf("get InferenceService: %v", err)
	}
	if stored.Status.Phase != PhaseFailed {
		t.Errorf("status.phase = %q, want %q", stored.Status.Phase, PhaseFailed)
	}
	degraded := meta.FindStatusCondition(stored.Status.Conditions, ConditionDegraded)
	if degraded == nil || !strings.Contains(degraded.Message, "mlx-server") {
		t.Errorf("Degraded condition = %+v, want a message naming the runtime", degraded)
	}
}

// TestEmptyRuntimeBuildsLlamaCppDeployment: with the CRD default gone, a CR
// that omits spec.runtime reaches the controller as "". It must build exactly
// the Deployment an explicit "llamacpp" builds (backend, args, probes, and the
// runtime pod label), or dropping the default silently changes every
// existing-style manifest.
func TestEmptyRuntimeBuildsLlamaCppDeployment(t *testing.T) {
	model := placementModel("m", "cuda")
	r := &InferenceServiceReconciler{ModelCachePath: "/models", InitContainerImage: "docker.io/curlimages/curl:8.18.0"}

	explicit := r.constructDeploymentForCache(placementISvc("svc", "m", "llamacpp"), model, resolveEffectiveModelCache(placementISvc("svc", "m", "llamacpp"), model), nil, 1, "", "")
	empty := r.constructDeploymentForCache(placementISvc("svc", "m", ""), model, resolveEffectiveModelCache(placementISvc("svc", "m", ""), model), nil, 1, "", "")

	if !apiequality.Semantic.DeepEqual(explicit.Spec, empty.Spec) {
		t.Errorf("Deployment spec differs between runtime \"\" and \"llamacpp\"\nllamacpp: %+v\nempty:    %+v",
			explicit.Spec.Template.Spec.Containers, empty.Spec.Template.Spec.Containers)
	}
	if got := empty.Spec.Template.Labels["inference.llmkube.dev/runtime"]; got != "llamacpp" {
		t.Errorf("runtime pod label for empty spec.runtime = %q, want llamacpp", got)
	}
}
