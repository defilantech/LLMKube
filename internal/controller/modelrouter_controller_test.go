/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestRouterReferencesInferenceService covers the small pure helper that
// powers the watch re-enqueue logic. The watch itself is exercised via
// envtest in a separate suite once the data plane lands; this verifies the
// matching predicate in isolation.
func TestRouterReferencesInferenceService(t *testing.T) {
	cases := []struct {
		name     string
		mr       *inferencev1alpha1.ModelRouter
		isvcName string
		want     bool
	}{
		{
			name: "single matching local backend",
			mr: &inferencev1alpha1.ModelRouter{
				Spec: inferencev1alpha1.ModelRouterSpec{
					Backends: []inferencev1alpha1.RouterBackend{
						{Name: "a", InferenceServiceRef: &corev1.LocalObjectReference{Name: "qwen"}},
					},
				},
			},
			isvcName: "qwen",
			want:     true,
		},
		{
			name: "non-matching name",
			mr: &inferencev1alpha1.ModelRouter{
				Spec: inferencev1alpha1.ModelRouterSpec{
					Backends: []inferencev1alpha1.RouterBackend{
						{Name: "a", InferenceServiceRef: &corev1.LocalObjectReference{Name: "qwen"}},
					},
				},
			},
			isvcName: "llama",
			want:     false,
		},
		{
			name: "external-only backends are skipped",
			mr: &inferencev1alpha1.ModelRouter{
				Spec: inferencev1alpha1.ModelRouterSpec{
					Backends: []inferencev1alpha1.RouterBackend{
						{Name: "a", External: &inferencev1alpha1.ExternalProvider{Provider: "anthropic", Model: "x"}},
					},
				},
			},
			isvcName: "qwen",
			want:     false,
		},
		{
			name: "match among several backends",
			mr: &inferencev1alpha1.ModelRouter{
				Spec: inferencev1alpha1.ModelRouterSpec{
					Backends: []inferencev1alpha1.RouterBackend{
						{Name: "a", External: &inferencev1alpha1.ExternalProvider{Provider: "anthropic", Model: "x"}},
						{Name: "b", InferenceServiceRef: &corev1.LocalObjectReference{Name: "qwen"}},
						{Name: "c", InferenceServiceRef: &corev1.LocalObjectReference{Name: "llama"}},
					},
				},
			},
			isvcName: "llama",
			want:     true,
		},
		{
			name:     "no backends",
			mr:       &inferencev1alpha1.ModelRouter{Spec: inferencev1alpha1.ModelRouterSpec{}},
			isvcName: "qwen",
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := routerReferencesInferenceService(tc.mr, tc.isvcName)
			if got != tc.want {
				t.Errorf("routerReferencesInferenceService(%q) = %v; want %v",
					tc.isvcName, got, tc.want)
			}
		})
	}
}
