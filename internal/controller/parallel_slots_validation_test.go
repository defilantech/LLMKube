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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These specs exercise spec.parallelSlots' bounds against the envtest
// apiserver: vLLM sizes --max-num-seqs in the hundreds, while every other
// runtime keeps the 64-slot cap.
var _ = Describe("InferenceService parallelSlots CRD validation", func() {
	ctx := context.Background()

	newISvc := func(runtime string, slots int32) *inferencev1alpha1.InferenceService {
		name := runtime
		if name == "" {
			name = "unset"
		}
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("ps-%s-%d", name, slots), Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:      "parallelslots-cel-model",
				Runtime:       runtime,
				ParallelSlots: &slots,
			},
		}
	}

	DescribeTable("admits",
		func(runtime string, slots int32) {
			isvc := newISvc(runtime, slots)
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
		},
		Entry("vllm above 64", "vllm", int32(128)),
		Entry("vllm at the new maximum", "vllm", int32(1024)),
		Entry("vllm-swift above 64", "vllm-swift", int32(128)),
		Entry("llamacpp at 64", "llamacpp", int32(64)),
	)

	DescribeTable("rejects",
		func(runtime string, slots int32, msg string) {
			err := k8sClient.Create(ctx, newISvc(runtime, slots))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(msg))
		},
		Entry("vllm above the maximum", "vllm", int32(1025), "should be less than or equal to 1024"),
		Entry("llamacpp above 64", "llamacpp", int32(65), "parallelSlots above 64 is supported for runtime vllm and vllm-swift only"),
		Entry("an unset runtime above 64", "", int32(65), "parallelSlots above 64 is supported for runtime vllm and vllm-swift only"),
		Entry("sglang above 64", "sglang", int32(65), "parallelSlots above 64 is supported for runtime vllm and vllm-swift only"),
	)
})
