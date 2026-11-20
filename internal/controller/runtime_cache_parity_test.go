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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The specs in this file use only API that exists without
// spec.modelCache.runtimeCache, so the same file runs on the branch and on the
// branch it replaced. They are the parity evidence that a service which does
// not set the field renders the pod base rendered, and that the prefetched
// claim stays read-only for it (#2117).

// runtimeCacheEnvNames are the variables the feature sets. Written out here
// rather than read from the backends so this file compiles on both trees.
var runtimeCacheEnvNames = []string{
	"VLLM_CACHE_ROOT",
	"TRITON_CACHE_DIR",
	"TILELANG_CACHE_DIR",
	"TORCHINDUCTOR_CACHE_DIR",
	"FLASHINFER_WORKSPACE_BASE",
	"SGLANG_CACHE_DIR",
}

func TestRuntimeCacheDefaultsUnchanged(t *testing.T) {
	const cacheKey = "0123456789abcdef"
	model := func(mutate func(*inferencev1alpha1.Model)) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/models/llama.gguf", Format: "gguf"},
			Status:     inferencev1alpha1.ModelStatus{CacheKey: cacheKey},
		}
		if mutate != nil {
			mutate(m)
		}
		return m
	}
	tests := []struct {
		name           string
		runtime        string
		model          *inferencev1alpha1.Model
		wantModelsPath bool
	}{
		{"vllm", RuntimeVLLM, model(nil), true},
		{"sglang", RuntimeSGLANG, model(nil), true},
		{"llamacpp", "llamacpp", model(nil), true},
		{"vllm on a pvc:// source", RuntimeVLLM, model(func(m *inferencev1alpha1.Model) {
			m.Spec.Source = "pvc://weights/llama.gguf"
		}), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
				Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: tc.runtime},
			}
			r := &InferenceServiceReconciler{
				ModelCachePath:     "/models",
				ModelCacheMode:     ModelCacheModePerService,
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
				DefaultFSGroup:     102,
			}
			pod := r.deploymentFor(isvc, tc.model, resolveEffectiveModelCache(isvc, tc.model), nil, 1, "", "").Spec.Template.Spec

			for _, m := range pod.Containers[0].VolumeMounts {
				if m.MountPath == "/runtime-cache" {
					t.Errorf("got a /runtime-cache mount without the field: %+v", m)
				}
			}
			for _, c := range pod.InitContainers {
				if c.Name == "runtime-cache-prep" {
					t.Error("got a runtime-cache-prep init container without the field")
				}
			}
			for _, name := range runtimeCacheEnvNames {
				for _, e := range pod.Containers[0].Env {
					if e.Name == name {
						t.Errorf("got env %s=%q without the field", e.Name, e.Value)
					}
				}
			}
			var found bool
			for _, m := range pod.Containers[0].VolumeMounts {
				if m.MountPath == "/models" {
					found = true
					if !m.ReadOnly {
						t.Errorf("/models mount = %+v, want read-only", m)
					}
				}
			}
			if tc.wantModelsPath && !found {
				t.Fatal("no /models mount")
			}
		})
	}
}

func TestPrefetchedCacheStaysReadOnly(t *testing.T) {
	const cacheKey = "0123456789abcdef"
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/models/llama.gguf", Format: "gguf"},
		Status: inferencev1alpha1.ModelStatus{
			CacheKey: cacheKey,
			Prefetch: []inferencev1alpha1.PrefetchStatus{{
				ClaimName:          "svc-model-cache",
				CacheKey:           cacheKey,
				Phase:              inferencev1alpha1.PrefetchPhaseSucceeded,
				JobName:            "svc-prefetch",
				ObservedGeneration: 0,
			}},
		},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:    RuntimeVLLM,
			ModelCache: &inferencev1alpha1.ModelCacheSpec{Prefetch: true},
		},
	}
	r := &InferenceServiceReconciler{
		ModelCachePath:     "/models",
		ModelCacheMode:     ModelCacheModePerService,
		InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		DefaultFSGroup:     102,
	}
	pod := r.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template.Spec

	var vol *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == "model-cache" {
			vol = &pod.Volumes[i]
		}
	}
	if vol == nil || vol.PersistentVolumeClaim == nil {
		t.Fatal("no model-cache claim volume")
	}
	if !vol.PersistentVolumeClaim.ReadOnly {
		t.Error("the prefetched cache volume source is writable without the field")
	}
}
