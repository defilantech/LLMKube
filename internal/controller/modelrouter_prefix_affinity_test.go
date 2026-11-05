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
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var _ = Describe("ModelRouter backend prefix affinity", func() {
	newMR := func(mode string, pa *inferencev1alpha1.PrefixAffinity) *inferencev1alpha1.ModelRouter {
		return &inferencev1alpha1.ModelRouter{
			ObjectMeta: metav1.ObjectMeta{Name: "mr-prefix-" + rand.String(6), Namespace: "default"},
			Spec: inferencev1alpha1.ModelRouterSpec{
				Backends: []inferencev1alpha1.RouterBackend{{
					Name:                "q",
					InferenceServiceRef: &corev1.LocalObjectReference{Name: "q"},
					Resolution:          inferencev1alpha1.RouterBackendResolutionEndpoint,
					Balancing:           &inferencev1alpha1.BackendBalancing{Mode: mode, PrefixAffinity: pa},
				}},
				DefaultRoute: "q",
			},
		}
	}

	// reconcileBalancing creates the router (and the InferenceService it
	// names), reconciles once, and returns the compiled proxy balancing block
	// with the events the reconcile recorded.
	reconcileBalancing := func(mr *inferencev1alpha1.ModelRouter) (map[string]any, []string) {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "q"},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, isvc) })
		Expect(k8sClient.Create(ctx, mr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, mr) })

		recorder := events.NewFakeRecorder(20)
		r := &ModelRouterReconciler{Client: k8sClient, Scheme: scheme.Scheme, RouterProxyImage: "test-image", Recorder: recorder}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mr)})
		Expect(err).NotTo(HaveOccurred())

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: routerProxyResourceName(mr.Name), Namespace: mr.Namespace}, cm)).To(Succeed())
		var raw map[string]any
		Expect(json.Unmarshal([]byte(cm.Data[routerProxyConfigKey]), &raw)).To(Succeed())
		backends, ok := raw["backends"].([]any)
		Expect(ok).To(BeTrue())
		Expect(backends).To(HaveLen(1))
		bal, _ := backends[0].(map[string]any)["balancing"].(map[string]any)
		Expect(bal).NotTo(BeNil())
		var seen []string
		for len(recorder.Events) > 0 {
			seen = append(seen, <-recorder.Events)
		}
		return bal, seen
	}

	It("defaults an empty prefixAffinity and rejects out-of-range values", func() {
		mr := newMR(inferencev1alpha1.BalancingModeLoadAware, &inferencev1alpha1.PrefixAffinity{})
		Expect(k8sClient.Create(ctx, mr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, mr) })
		Expect(mr.Spec.Backends[0].Balancing.PrefixAffinity.BlockBytes).To(Equal(int32(1024)))
		Expect(mr.Spec.Backends[0].Balancing.PrefixAffinity.MaxBlocksPerEndpoint).To(Equal(int32(16384)))

		for _, pa := range []*inferencev1alpha1.PrefixAffinity{
			{BlockBytes: 128}, {BlockBytes: 32768}, {MaxBlocksPerEndpoint: 512}, {MaxBlocksPerEndpoint: 1 << 20},
		} {
			bad := newMR(inferencev1alpha1.BalancingModeLoadAware, pa)
			err := k8sClient.Create(ctx, bad)
			if err == nil {
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, bad) })
			}
			Expect(err).To(HaveOccurred(), "prefixAffinity %+v", *pa)
		}
	})

	It("a LoadAware router compiles prefixAffinity into the proxy config", func() {
		bal, _ := reconcileBalancing(newMR(inferencev1alpha1.BalancingModeLoadAware,
			&inferencev1alpha1.PrefixAffinity{BlockBytes: 512, MaxBlocksPerEndpoint: 4096}))
		Expect(bal["mode"]).To(Equal("LoadAware"))
		Expect(bal["prefixBlockBytes"]).To(BeNumerically("==", 512))
		Expect(bal["prefixMaxBlocks"]).To(BeNumerically("==", 4096))
	})

	It("a RoundRobin router with prefixAffinity compiles without it and records PrefixAffinityIgnored", func() {
		bal, seen := reconcileBalancing(newMR(inferencev1alpha1.BalancingModeRoundRobin, &inferencev1alpha1.PrefixAffinity{}))
		Expect(bal["mode"]).To(Equal("RoundRobin"))
		Expect(bal).NotTo(HaveKey("prefixBlockBytes"))
		Expect(seen).To(ContainElement(And(
			ContainSubstring("PrefixAffinityIgnored"),
			ContainSubstring(`"q"`),
			ContainSubstring("balancing.mode: LoadAware"),
		)))
	})
})
