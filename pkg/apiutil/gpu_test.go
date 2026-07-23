package apiutil

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func modelWithGPU(gpu *inferencev1alpha1.GPUSpec) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Hardware: &inferencev1alpha1.HardwareSpec{GPU: gpu},
		},
	}
}

func TestGPUResourceName(t *testing.T) {
	cases := []struct {
		name  string
		model *inferencev1alpha1.Model
		want  corev1.ResourceName
	}{
		{"nil model defaults to nvidia", nil, corev1.ResourceName("nvidia.com/gpu")},
		{"explicit override wins", modelWithGPU(&inferencev1alpha1.GPUSpec{ResourceName: "squat.ai/dri-render", Vendor: "amd"}), corev1.ResourceName("squat.ai/dri-render")},
		{"amd vulkan uses dri-render", modelWithGPU(&inferencev1alpha1.GPUSpec{Vendor: "amd", Runtime: "vulkan"}), corev1.ResourceName("devic.es/dri-render")},
		{"amd rocm uses dri-render", modelWithGPU(&inferencev1alpha1.GPUSpec{Vendor: "amd", Runtime: "rocm"}), corev1.ResourceName("devic.es/dri-render")},
		{"amd default uses amd.com/gpu", modelWithGPU(&inferencev1alpha1.GPUSpec{Vendor: "amd"}), corev1.ResourceName("amd.com/gpu")},
		{"intel uses i915", modelWithGPU(&inferencev1alpha1.GPUSpec{Vendor: "intel"}), corev1.ResourceName("gpu.intel.com/i915")},
		{"unknown vendor defaults to nvidia", modelWithGPU(&inferencev1alpha1.GPUSpec{Vendor: "other"}), corev1.ResourceName("nvidia.com/gpu")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GPUResourceName(tc.model); got != tc.want {
				t.Fatalf("GPUResourceName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGPUCount(t *testing.T) {
	isvcWith := func(gpu int32) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Resources: &inferencev1alpha1.InferenceResourceRequirements{GPU: gpu},
			},
		}
	}
	cases := []struct {
		name  string
		isvc  *inferencev1alpha1.InferenceService
		model *inferencev1alpha1.Model
		want  int32
	}{
		{"model count wins", isvcWith(1), modelWithGPU(&inferencev1alpha1.GPUSpec{Count: 2}), 2},
		{"isvc count when model has none", isvcWith(3), modelWithGPU(&inferencev1alpha1.GPUSpec{}), 3},
		{"zero when neither set", &inferencev1alpha1.InferenceService{}, modelWithGPU(nil), 0},
		{"nil model is safe", isvcWith(2), nil, 2},
		{"nil isvc is safe", nil, modelWithGPU(&inferencev1alpha1.GPUSpec{Count: 4}), 4},
		{"both nil is zero", nil, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GPUCount(tc.isvc, tc.model); got != tc.want {
				t.Fatalf("GPUCount() = %d, want %d", got, tc.want)
			}
		})
	}
}
