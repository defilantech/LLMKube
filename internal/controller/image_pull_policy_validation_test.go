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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These specs exercise the InferenceService CRD's server-side validation for
// spec.imagePullPolicy against the envtest apiserver, so a failure here means
// the generated CRD schema does not enforce what the type claims. A unit test
// over the Go type cannot catch that: the enum is enforced at admission.
var _ = Describe("InferenceService imagePullPolicy CRD validation", func() {
	ctx := context.Background()

	newISvc := func(name string, policy corev1.PullPolicy) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:        "image-pull-policy-model",
				ImagePullPolicy: policy,
			},
		}
	}

	It("admits an absent policy", func() {
		isvc := newISvc("ipp-valid-absent", "")
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits each policy the enum lists", func() {
		for _, tc := range []struct {
			name   string
			policy corev1.PullPolicy
		}{
			{"ipp-valid-always", corev1.PullAlways},
			{"ipp-valid-ifnotpresent", corev1.PullIfNotPresent},
			{"ipp-valid-never", corev1.PullNever},
		} {
			isvc := newISvc(tc.name, tc.policy)
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed(), "policy %q should be admitted", tc.policy)
			Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
		}
	})

	// Both cases go over the wire as unstructured objects, because the typed
	// client's omitempty silently drops an empty policy: a Go caller cannot
	// express `imagePullPolicy: ""`, but a manifest or a Helm value can, and
	// the enum must reject it rather than admit a value the builder would
	// then carry into the pod template.
	It("rejects a value outside the enum", func() {
		for _, tc := range []struct {
			name  string
			value string
		}{
			// Kubernetes' own spelling is case-sensitive, so "always" is a
			// mistake the API server should catch.
			{"ipp-invalid-lowercase", "always"},
			{"ipp-invalid-empty", ""},
		} {
			u := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "inference.llmkube.dev/v1alpha1",
				"kind":       "InferenceService",
				"metadata":   map[string]any{"name": tc.name, "namespace": "default"},
				"spec":       map[string]any{"modelRef": "image-pull-policy-model", "imagePullPolicy": tc.value},
			}}
			err := k8sClient.Create(ctx, u)
			Expect(err).To(HaveOccurred(), "policy %q should be rejected", tc.value)
			Expect(err.Error()).To(ContainSubstring("Unsupported value"))
		}
	})

	// The field is optional with no schema default, so an object that omits it
	// must come back with the policy unset. That is what keeps the rendered pod
	// template identical to base's.
	It("stores no policy when the field is omitted", func() {
		isvc := newISvc("ipp-omitted", corev1.PullPolicy(""))
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())

		stored := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace}, stored)).To(Succeed())
		Expect(stored.Spec.ImagePullPolicy).To(BeEmpty())
		Expect(k8sClient.Delete(ctx, stored)).To(Succeed())
	})
})
