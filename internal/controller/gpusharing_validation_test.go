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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These specs exercise the InferenceService CRD's server-side validation for
// spec.resources.gpuSharing (mode enum plus the CEL rules tying profile to
// partitioned and memoryLimitGiB to shared). They run against the envtest
// apiserver, so a failure here means the generated CRD schema does not
// enforce what the type claims.
var _ = Describe("InferenceService gpuSharing CRD validation", func() {
	ctx := context.Background()

	newISvc := func(name string, sharing *inferencev1alpha1.GPUSharingSpec) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "gpusharing-cel-model",
				Resources: &inferencev1alpha1.InferenceResourceRequirements{
					GPU:        1,
					GPUSharing: sharing,
				},
			},
		}
	}

	limit := int32(24)

	It("admits an empty gpuSharing (defaults to exclusive)", func() {
		isvc := newISvc("gs-valid-empty", &inferencev1alpha1.GPUSharingSpec{})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(isvc.Spec.Resources.GPUSharing.Mode).To(Equal(inferencev1alpha1.GPUSharingModeExclusive),
			"the API server should default mode to exclusive")
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits partitioned with a profile", func() {
		isvc := newISvc("gs-valid-partitioned", &inferencev1alpha1.GPUSharingSpec{
			Mode:    inferencev1alpha1.GPUSharingModePartitioned,
			Profile: "1g.24gb",
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits shared with a memory limit", func() {
		isvc := newISvc("gs-valid-shared", &inferencev1alpha1.GPUSharingSpec{
			Mode:           inferencev1alpha1.GPUSharingModeShared,
			MemoryLimitGiB: &limit,
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("rejects partitioned without a profile", func() {
		isvc := newISvc("gs-invalid-noprofile", &inferencev1alpha1.GPUSharingSpec{
			Mode: inferencev1alpha1.GPUSharingModePartitioned,
		})
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("profile is required when mode is partitioned"))
	})

	It("rejects a profile without partitioned mode (defaulted exclusive)", func() {
		isvc := newISvc("gs-invalid-profile-excl", &inferencev1alpha1.GPUSharingSpec{
			Profile: "1g.24gb",
		})
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("profile is only valid when mode is partitioned"))
	})

	It("rejects a profile on shared mode", func() {
		isvc := newISvc("gs-invalid-profile-shared", &inferencev1alpha1.GPUSharingSpec{
			Mode:    inferencev1alpha1.GPUSharingModeShared,
			Profile: "1g.24gb",
		})
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("profile is only valid when mode is partitioned"))
	})

	It("rejects memoryLimitGiB outside shared mode", func() {
		isvc := newISvc("gs-invalid-memlimit", &inferencev1alpha1.GPUSharingSpec{
			Mode:           inferencev1alpha1.GPUSharingModePartitioned,
			Profile:        "1g.24gb",
			MemoryLimitGiB: &limit,
		})
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("memoryLimitGiB is only valid when mode is shared"))
	})

	It("rejects an unknown mode", func() {
		isvc := newISvc("gs-invalid-mode", &inferencev1alpha1.GPUSharingSpec{
			Mode: "fractional",
		})
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("Unsupported value"))
	})
})
