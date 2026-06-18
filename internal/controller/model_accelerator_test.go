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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func nodeWithCapacity(name string, res corev1.ResourceName, qty string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{res: resource.MustParse(qty)},
		},
	}
}

func modelWithAccelerator(accel string) *inferencev1alpha1.Model {
	m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	if accel != "" {
		m.Spec.Hardware = &inferencev1alpha1.HardwareSpec{Accelerator: accel}
	}
	return m
}

func modelWithGPUResource(accel string, resourceName string) *inferencev1alpha1.Model {
	m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	m.Spec.Hardware = &inferencev1alpha1.HardwareSpec{
		Accelerator: accel,
		GPU: &inferencev1alpha1.GPUSpec{
			ResourceName: resourceName,
		},
	}
	return m
}

func newModelReconcilerWithNodes(t *testing.T, nodes ...client.Object) *ModelReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodes...).Build()
	return &ModelReconciler{Client: c, Scheme: scheme}
}

func TestCheckAcceleratorAvailability(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name         string
		accel        string
		resourceName string
		nodes        []client.Object
		want         bool
	}{
		{"nil hardware is available", "", "", nil, true},
		{"cpu is always available", "cpu", "", nil, true},
		{"metal is assumed available (off-cluster agent)", "metal", "", nil, true},
		{
			"cuda available when a node advertises nvidia.com/gpu", "cuda", "",
			[]client.Object{nodeWithCapacity("gpu1", "nvidia.com/gpu", "1")}, true,
		},
		{
			"cuda unavailable when no node has a GPU", "cuda", "",
			[]client.Object{nodeWithCapacity("cpu1", "cpu", "8")}, false,
		},
		{
			"rocm available when a node advertises amd.com/gpu", "rocm", "",
			[]client.Object{nodeWithCapacity("amd1", "amd.com/gpu", "1")}, true,
		},
		{
			"rocm unavailable when only an nvidia node exists", "rocm", "",
			[]client.Object{nodeWithCapacity("gpu1", "nvidia.com/gpu", "1")}, false,
		},
		{
			"rocm with resourceName override uses the override resource",
			"rocm", "squat.ai/dri-render",
			[]client.Object{nodeWithCapacity("gpu1", "squat.ai/dri-render", "1")},
			true,
		},
		{
			"rocm with resourceName override fails when override not on any node",
			"rocm", "squat.ai/dri-render",
			[]client.Object{nodeWithCapacity("gpu1", "nvidia.com/gpu", "1")},
			false,
		},
		{
			"cuda with resourceName override uses the override resource",
			"cuda", "custom.gpu.io/gpu",
			[]client.Object{nodeWithCapacity("gpu1", "custom.gpu.io/gpu", "2")},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newModelReconcilerWithNodes(t, tc.nodes...)
			var model *inferencev1alpha1.Model
			if tc.resourceName != "" {
				model = modelWithGPUResource(tc.accel, tc.resourceName)
			} else {
				model = modelWithAccelerator(tc.accel)
			}
			got := r.checkAcceleratorAvailability(ctx, model)
			if got != tc.want {
				t.Errorf("accelerator %q: want %v, got %v", tc.accel, tc.want, got)
			}
		})
	}
}
