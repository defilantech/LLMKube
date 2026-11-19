/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The default proxy limit must carry a few hundred long-context agent streams
// (#2194: 448 streams OOM-killed the proxy at 256Mi). The request stays small,
// since an idle proxy sits near 14Mi.
func TestRouterProxyDefaultResources(t *testing.T) {
	r := &ModelRouterReconciler{RouterProxyImage: "img"}
	got := r.newRouterDeployment(canonicalModelRouter(), &compiledConfig{Hash: "hash"}).Spec.Template.Spec.Containers[0].Resources
	if lim := got.Limits[corev1.ResourceMemory]; lim.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("default memory limit = %s, want 1Gi", lim.String())
	}
	if req := got.Requests[corev1.ResourceMemory]; req.Cmp(resource.MustParse("64Mi")) != 0 {
		t.Errorf("default memory request = %s, want 64Mi", req.String())
	}

	mr := canonicalModelRouter()
	mr.Spec.Proxy = &inferencev1alpha1.RouterProxySpec{Resources: &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}}
	set := r.newRouterDeployment(mr, &compiledConfig{Hash: "hash"}).Spec.Template.Spec.Containers[0].Resources
	if lim := set.Limits[corev1.ResourceMemory]; lim.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Errorf("user-set memory limit = %s, want 2Gi", lim.String())
	}
}
