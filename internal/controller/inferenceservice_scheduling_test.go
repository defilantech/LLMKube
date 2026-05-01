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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var _ = Describe("resolvePriorityClassName", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	It("should return custom PriorityClassName when explicitly set", func() {
		isvc := &inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{PriorityClassName: "my-custom-priority"},
		}
		Expect(reconciler.resolvePriorityClassName(isvc)).To(Equal("my-custom-priority"))
	})

	DescribeTable("should map priority levels",
		func(priority, expected string) {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Priority: priority},
			}
			Expect(reconciler.resolvePriorityClassName(isvc)).To(Equal(expected))
		},
		Entry("critical", "critical", "llmkube-critical"),
		Entry("high", "high", "llmkube-high"),
		Entry("normal", "normal", "llmkube-normal"),
		Entry("low", "low", "llmkube-low"),
		Entry("batch", "batch", "llmkube-batch"),
		Entry("empty defaults to normal", "", "llmkube-normal"),
		Entry("unknown defaults to normal", "unknown", "llmkube-normal"),
	)
})

var _ = Describe("resolveEffectivePriority", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	DescribeTable("should resolve priority values",
		func(priority string, expected int32) {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Priority: priority},
			}
			Expect(reconciler.resolveEffectivePriority(isvc)).To(Equal(expected))
		},
		Entry("critical", "critical", int32(1000000)),
		Entry("high", "high", int32(100000)),
		Entry("normal", "normal", int32(10000)),
		Entry("low", "low", int32(1000)),
		Entry("batch", "batch", int32(100)),
		Entry("empty defaults to normal", "", int32(10000)),
		Entry("unknown defaults to normal", "unknown", int32(10000)),
	)
})

func deletePVCForcibly(ctx context.Context, namespace string) {
	pvc := &corev1.PersistentVolumeClaim{}
	pvcKey := types.NamespacedName{Name: ModelCachePVCName, Namespace: namespace}
	if err := k8sClient.Get(ctx, pvcKey, pvc); err != nil {
		return
	}
	if len(pvc.Finalizers) > 0 {
		pvc.Finalizers = nil
		_ = k8sClient.Update(ctx, pvc)
	}
	_ = k8sClient.Delete(ctx, pvc)
	Eventually(func() bool {
		return errors.IsNotFound(k8sClient.Get(ctx, pvcKey, &corev1.PersistentVolumeClaim{}))
	}, "5s", "100ms").Should(BeTrue())
}
