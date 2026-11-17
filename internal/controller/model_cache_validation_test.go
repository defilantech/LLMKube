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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These specs exercise the InferenceService CRD's server-side validation for
// spec.modelCache (the persistence enum, and the CEL rule making persistence
// Ephemeral mutually exclusive with claimName). They run against the envtest
// apiserver, so a failure here means the generated CRD schema does not enforce
// what the type claims. Unit tests over the Go predicates cannot catch that:
// the conflict is a static contradiction and is rejected at admission, never
// reaching the reconciler.
var _ = Describe("InferenceService modelCache CRD validation", func() {
	ctx := context.Background()

	newISvc := func(name string, cache *inferencev1alpha1.ModelCacheSpec) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:   "modelcache-cel-model",
				ModelCache: cache,
			},
		}
	}

	It("admits an absent modelCache block", func() {
		isvc := newISvc("mc-valid-absent", nil)
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits Ephemeral on its own", func() {
		isvc := newISvc("mc-valid-ephemeral", &inferencev1alpha1.ModelCacheSpec{
			Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral,
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits claimName with persistence unset", func() {
		isvc := newISvc("mc-valid-claim", &inferencev1alpha1.ModelCacheSpec{
			ClaimName: "user-owned-cache",
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	// Cached is the resolution of an empty persistence, so pairing it with a
	// claim must stay legal: that is the pre-#1451 behaviour spelled out.
	It("admits claimName together with Cached", func() {
		isvc := newISvc("mc-valid-claim-cached", &inferencev1alpha1.ModelCacheSpec{
			Persistence: inferencev1alpha1.ModelCachePersistenceCached,
			ClaimName:   "user-owned-cache",
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("rejects Ephemeral together with claimName", func() {
		isvc := newISvc("mc-invalid-both", &inferencev1alpha1.ModelCacheSpec{
			Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral,
			ClaimName:   "user-owned-cache",
		})
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(
			"claimName cannot be set when persistence is Ephemeral"))
	})

	// The quantity pattern exists because the value reaches resource parsing.
	// Its sibling fields (cpu, memory, hostMemory) carry no pattern and reach
	// resource.MustParse, which panics on malformed input and takes the
	// controller down for every workload rather than failing the one object at
	// fault. Rejecting at admission keeps that blast radius at zero here.
	It("admits a well-formed ephemeralStorage quantity", func() {
		isvc := newISvc("mc-valid-storage", &inferencev1alpha1.ModelCacheSpec{
			Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral,
		})
		isvc.Spec.Resources = &inferencev1alpha1.InferenceResourceRequirements{
			EphemeralStorage: "40Gi",
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("rejects a malformed ephemeralStorage quantity", func() {
		isvc := newISvc("mc-invalid-storage", &inferencev1alpha1.ModelCacheSpec{
			Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral,
		})
		isvc.Spec.Resources = &inferencev1alpha1.InferenceResourceRequirements{
			EphemeralStorage: "40GB",
		}
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
	})

	It("rejects an unknown persistence value", func() {
		isvc := newISvc("mc-invalid-enum", &inferencev1alpha1.ModelCacheSpec{
			Persistence: inferencev1alpha1.ModelCachePersistence("Sometimes"),
		})
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("Unsupported value"))
	})

	It("admits runtimeCache enabled on its own", func() {
		isvc := newISvc("mc-valid-runtime-cache", &inferencev1alpha1.ModelCacheSpec{
			RuntimeCache: &inferencev1alpha1.RuntimeCacheSpec{Enabled: true},
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits runtimeCache with enabled false", func() {
		isvc := newISvc("mc-valid-runtime-cache-off", &inferencev1alpha1.ModelCacheSpec{
			RuntimeCache: &inferencev1alpha1.RuntimeCacheSpec{Enabled: false},
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits runtimeCache enabled false together with Ephemeral", func() {
		isvc := newISvc("mc-valid-runtime-cache-ephemeral-off", &inferencev1alpha1.ModelCacheSpec{
			Persistence:  inferencev1alpha1.ModelCachePersistenceEphemeral,
			RuntimeCache: &inferencev1alpha1.RuntimeCacheSpec{Enabled: false},
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("rejects runtimeCache enabled together with Ephemeral", func() {
		// The two contradict each other statically: there is no claim to keep
		// a compiled kernel on, so the feature would have nothing to point at.
		isvc := newISvc("mc-invalid-runtime-cache-ephemeral", &inferencev1alpha1.ModelCacheSpec{
			Persistence:  inferencev1alpha1.ModelCachePersistenceEphemeral,
			RuntimeCache: &inferencev1alpha1.RuntimeCacheSpec{Enabled: true},
		})
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(
			"runtimeCache cannot be enabled when persistence is Ephemeral"))
	})

	It("rejects an empty runtimeCache block", func() {
		// The field is required inside the struct, so a block that asks for
		// nothing is refused at admission instead of silently doing nothing.
		// Written as unstructured because the typed client always serializes
		// enabled, even as false, and so cannot express an empty block.
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(inferencev1alpha1.GroupVersion.WithKind("InferenceService"))
		u.SetName("mc-invalid-runtime-cache-empty")
		u.SetNamespace("default")
		u.Object["spec"] = map[string]interface{}{
			"modelRef":   "modelcache-cel-model",
			"modelCache": map[string]interface{}{"runtimeCache": map[string]interface{}{}},
		}
		err := k8sClient.Create(ctx, u)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("runtimeCache.enabled"))
	})
})
