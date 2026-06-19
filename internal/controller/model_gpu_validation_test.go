package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These specs exercise the GPUSpec mutual-exclusivity CEL rule against the
// envtest apiserver (where CRD validation actually runs), not the in-process
// deployment builder.
var _ = Describe("Model GPU resourceName/resourceClaims CEL validation", func() {
	newGPUModel := func(name string, gpu *inferencev1alpha1.GPUSpec) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:       "https://example.com/model.gguf",
				Format:       "gguf",
				Quantization: "Q4_K_M",
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "rocm",
					GPU:         gpu,
				},
			},
		}
	}

	It("admits a Model with resourceName and no resourceClaims", func() {
		// Regression for the CEL rule evaluating resourceClaims.size() on an
		// absent field: device-plugin Models (resourceName set, resourceClaims
		// unset) were rejected with "no such key: resourceClaims".
		model := newGPUModel("gpu-resourcename-only", &inferencev1alpha1.GPUSpec{
			Enabled:      true,
			Vendor:       "amd",
			Count:        1,
			ResourceName: "devic.es/rocm",
		})
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		Expect(k8sClient.Delete(ctx, model)).To(Succeed())
	})

	It("admits a Model with resourceClaims and no resourceName", func() {
		model := newGPUModel("gpu-claims-only", &inferencev1alpha1.GPUSpec{
			Enabled: true,
			Vendor:  "intel",
			ResourceClaims: []corev1.PodResourceClaim{
				{Name: "gpu", ResourceClaimTemplateName: ptr.To("gpu-template")},
			},
		})
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		Expect(k8sClient.Delete(ctx, model)).To(Succeed())
	})

	It("rejects a Model that sets both resourceName and resourceClaims", func() {
		model := newGPUModel("gpu-both", &inferencev1alpha1.GPUSpec{
			Enabled:      true,
			Vendor:       "amd",
			ResourceName: "devic.es/rocm",
			ResourceClaims: []corev1.PodResourceClaim{
				{Name: "gpu", ResourceClaimTemplateName: ptr.To("gpu-template")},
			},
		})
		err := k8sClient.Create(ctx, model)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
	})
})
