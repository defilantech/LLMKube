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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These specs exercise the GPUQuota CRD's server-side validation (OpenAPI
// enum/minimum plus the CEL rule enforcing that exactly one of selector or
// namespaceRef is set). They run against the envtest apiserver, so a failure
// here means the generated CRD schema does not enforce what the type claims.
var _ = Describe("GPUQuota CRD validation", func() {
	ctx := context.Background()

	newQuota := func(name string, spec inferencev1alpha1.GPUQuotaSpec) *inferencev1alpha1.GPUQuota {
		return &inferencev1alpha1.GPUQuota{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       spec,
		}
	}

	It("admits a quota scoped by namespaceRef only", func() {
		q := newQuota("gq-valid-ns", inferencev1alpha1.GPUQuotaSpec{
			NamespaceRef: "team-a",
			GPUCount:     4,
		})
		Expect(k8sClient.Create(ctx, q)).To(Succeed())
		Expect(k8sClient.Delete(ctx, q)).To(Succeed())
	})

	It("admits a quota scoped by selector only", func() {
		q := newQuota("gq-valid-sel", inferencev1alpha1.GPUQuotaSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "b"}},
			GPUCount: 2,
		})
		Expect(k8sClient.Create(ctx, q)).To(Succeed())
		Expect(k8sClient.Delete(ctx, q)).To(Succeed())
	})

	It("rejects a quota with neither selector nor namespaceRef", func() {
		q := newQuota("gq-neither", inferencev1alpha1.GPUQuotaSpec{GPUCount: 1})
		err := k8sClient.Create(ctx, q)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of selector or namespaceRef"))
	})

	It("rejects a quota with both selector and namespaceRef", func() {
		q := newQuota("gq-both", inferencev1alpha1.GPUQuotaSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"team": "c"}},
			NamespaceRef: "team-c",
			GPUCount:     1,
		})
		err := k8sClient.Create(ctx, q)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of selector or namespaceRef"))
	})

	It("rejects an unknown minPriority value", func() {
		q := newQuota("gq-bad-priority", inferencev1alpha1.GPUQuotaSpec{
			NamespaceRef: "team-d",
			GPUCount:     1,
			MinPriority:  "urgent",
		})
		Expect(k8sClient.Create(ctx, q)).NotTo(Succeed())
	})

	It("rejects a negative gpuCount", func() {
		q := newQuota("gq-negative", inferencev1alpha1.GPUQuotaSpec{
			NamespaceRef: "team-e",
			GPUCount:     -1,
		})
		Expect(k8sClient.Create(ctx, q)).NotTo(Succeed())
	})
})
