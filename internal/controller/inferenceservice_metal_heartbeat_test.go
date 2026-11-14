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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// metalEndpoints builds a discovery/v1 EndpointSlice fixture for the metal-agent
// path. name must be the sanitized InferenceService name (dots replaced by
// hyphens); it is used both as the slice name and as the required
// kubernetes.io/service-name label so the consumer's list-by-label finds it.
// heartbeat is stamped as the AnnotationAgentHeartbeat annotation when non-empty.
func metalEndpoints(name, heartbeat string) *discoveryv1.EndpointSlice {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"kubernetes.io/service-name": name,
				"llmkube.ai/managed-by":      "metal-agent",
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"192.0.2.10"},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name:     ptr.To("http"),
			Port:     ptr.To(int32(50051)),
			Protocol: ptr.To(corev1.ProtocolTCP),
		}},
	}
	if heartbeat != "" {
		slice.Annotations = map[string]string{
			inferencev1alpha1.AnnotationAgentHeartbeat: heartbeat,
		}
	}
	return slice
}

var _ = Describe("metalEndpointSnapshot heartbeat expiry", func() {
	var (
		reconciler *InferenceServiceReconciler
		ctx        context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
	})

	Context("direct unit tests of metalEndpointSnapshot ready replicas", func() {
		const namespace = "default"

		It("should return 1 for Endpoints with a fresh heartbeat annotation", func() {
			const isvcName = "hb-fresh"
			fresh := time.Now().UTC().Format(time.RFC3339)
			ep := metalEndpoints(isvcName, fresh)
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ep) })

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
			}
			snap := reconciler.metalEndpointSnapshot(ctx, isvc, sanitizeDNSName(isvc.Name))
			Expect(snap.ReadyReplicas).To(Equal(int32(1)))
			// Kind, not just the count: it selects the requeue interval
			// (metalHeartbeatRequeueDuration) and the SchedulingStatus
			// message. Stale and unparseable both report 0 ready, so the
			// count alone cannot tell them apart.
			Expect(snap.Kind).To(Equal(metalHBFresh))
		})

		It("should return 0 for Endpoints with a stale heartbeat annotation (10 minutes old)", func() {
			const isvcName = "hb-stale"
			stale := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
			ep := metalEndpoints(isvcName, stale)
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ep) })

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
			}
			snap := reconciler.metalEndpointSnapshot(ctx, isvc, sanitizeDNSName(isvc.Name))
			Expect(snap.ReadyReplicas).To(Equal(int32(0)))
			// Kind, not just the count: it selects the requeue interval
			// (metalHeartbeatRequeueDuration) and the SchedulingStatus
			// message. Stale and unparseable both report 0 ready, so the
			// count alone cannot tell them apart.
			Expect(snap.Kind).To(Equal(metalHBStale))
		})

		It("should return 0 for Endpoints with an unparseable heartbeat annotation", func() {
			const isvcName = "hb-bad"
			ep := metalEndpoints(isvcName, "not-a-timestamp")
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ep) })

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
			}
			snap := reconciler.metalEndpointSnapshot(ctx, isvc, sanitizeDNSName(isvc.Name))
			Expect(snap.ReadyReplicas).To(Equal(int32(0)))
			// Kind, not just the count: it selects the requeue interval
			// (metalHeartbeatRequeueDuration) and the SchedulingStatus
			// message. Stale and unparseable both report 0 ready, so the
			// count alone cannot tell them apart.
			Expect(snap.Kind).To(Equal(metalHBUnparseable))
		})

		It("should return 1 for Endpoints WITHOUT the annotation (legacy agent exemption)", func() {
			const isvcName = "hb-legacy"
			ep := metalEndpoints(isvcName, "") // no annotation
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ep) })

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
			}
			snap := reconciler.metalEndpointSnapshot(ctx, isvc, sanitizeDNSName(isvc.Name))
			Expect(snap.ReadyReplicas).To(Equal(int32(1)))
			// Kind, not just the count: it selects the requeue interval
			// (metalHeartbeatRequeueDuration) and the SchedulingStatus
			// message. Stale and unparseable both report 0 ready, so the
			// count alone cannot tell them apart.
			Expect(snap.Kind).To(Equal(metalHBLegacy))
		})
	})

	Context("Reconcile requeue for metal InferenceService with heartbeat Endpoints", func() {
		const namespace = "default"
		const modelName = "hb-metal-model"
		const isvcName = "hb-metal-isvc"

		BeforeEach(func() {
			model := &inferencev1alpha1.Model{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: namespace}, model)
			if err != nil && errors.IsNotFound(err) {
				model = &inferencev1alpha1.Model{
					ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
					Spec: inferencev1alpha1.ModelSpec{
						Source:   "https://example.com/hb-model.gguf",
						Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
					},
				}
				Expect(k8sClient.Create(ctx, model)).To(Succeed())
			}
			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			isvc := &inferencev1alpha1.InferenceService{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, isvc)
			if err != nil && errors.IsNotFound(err) {
				replicas := int32(1)
				isvc = &inferencev1alpha1.InferenceService{
					ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
					Spec: inferencev1alpha1.InferenceServiceSpec{
						ModelRef: modelName,
						Replicas: &replicas,
					},
				}
				Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			}
		})

		AfterEach(func() {
			isvc := &inferencev1alpha1.InferenceService{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, isvc); err == nil {
				_ = k8sClient.Delete(ctx, isvc)
			}

			model := &inferencev1alpha1.Model{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: namespace}, model); err == nil {
				_ = k8sClient.Delete(ctx, model)
			}

			slice := &discoveryv1.EndpointSlice{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, slice); err == nil {
				_ = k8sClient.Delete(ctx, slice)
			}
		})

		It("should return a non-zero RequeueAfter when Endpoints carry a fresh heartbeat annotation", func() {
			fresh := time.Now().UTC().Format(time.RFC3339)
			ep := metalEndpoints(isvcName, fresh)
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0),
				"expected a non-zero RequeueAfter for a metal service with heartbeat Endpoints")
			Expect(result.RequeueAfter).To(Equal(inferencev1alpha1.DefaultAgentHeartbeatTimeout / 2))
		})

		It("should leave a fresh-heartbeat EndpointSlice endpoint ready", func() {
			fresh := time.Now().UTC().Format(time.RFC3339)
			ep := metalEndpoints(isvcName, fresh)
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			got := &discoveryv1.EndpointSlice{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Endpoints).To(HaveLen(1))
			Expect(got.Endpoints[0].Conditions.Ready).To(HaveValue(BeTrue()),
				"a live agent's endpoint must not be withdrawn by the controller")
			Expect(got.Endpoints[0].Conditions.Serving).To(BeNil())
		})
	})

	Context("Reconcile with a stale metal-agent heartbeat", func() {
		const namespace = "default"
		const modelName = "hb-stale-model"
		const isvcName = "hb-stale-isvc"

		BeforeEach(func() {
			model := &inferencev1alpha1.Model{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: namespace}, model)
			if err != nil && errors.IsNotFound(err) {
				model = &inferencev1alpha1.Model{
					ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
					Spec: inferencev1alpha1.ModelSpec{
						Source:   "https://example.com/stale-model.gguf",
						Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
					},
				}
				Expect(k8sClient.Create(ctx, model)).To(Succeed())
			}
			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			isvc := &inferencev1alpha1.InferenceService{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, isvc)
			if err != nil && errors.IsNotFound(err) {
				replicas := int32(1)
				isvc = &inferencev1alpha1.InferenceService{
					ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
					Spec: inferencev1alpha1.InferenceServiceSpec{
						ModelRef: modelName,
						Replicas: &replicas,
					},
				}
				Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			}
		})

		AfterEach(func() {
			isvc := &inferencev1alpha1.InferenceService{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, isvc); err == nil {
				_ = k8sClient.Delete(ctx, isvc)
			}

			model := &inferencev1alpha1.Model{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: modelName, Namespace: namespace}, model); err == nil {
				_ = k8sClient.Delete(ctx, model)
			}

			slice := &discoveryv1.EndpointSlice{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, slice); err == nil {
				_ = k8sClient.Delete(ctx, slice)
			}
		})

		It("should report stale status, zero readyReplicas, and still requeue after a 10-minute-old heartbeat", func() {
			stale := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
			ep := metalEndpoints(isvcName, stale)
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// The re-check loop must survive staleness: a stale heartbeat still
			// needs periodic re-evaluation in case the agent comes back online.
			Expect(result.RequeueAfter).To(BeNumerically(">", 0),
				"expected a non-zero RequeueAfter so the controller re-checks once the agent recovers")

			// The operator must reflect 0 ready replicas when the heartbeat has expired.
			isvc := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, isvc)).To(Succeed())
			Expect(isvc.Status.ReadyReplicas).To(Equal(int32(0)),
				"stale heartbeat should zero out readyReplicas")

			// The scheduling status must indicate a stale heartbeat, not initial provisioning.
			Expect(isvc.Status.SchedulingStatus).To(Equal("AgentHeartbeatStale"),
				"stale heartbeat must report AgentHeartbeatStale, not WaitingForMetalAgent")
			Expect(isvc.Status.SchedulingMessage).To(ContainSubstring("last seen"),
				"stale heartbeat message must include the last-seen timestamp")
		})

		It("should mark the stale EndpointSlice endpoints not ready and not serving (#1918)", func() {
			stale := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
			ep := metalEndpoints(isvcName, stale)
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// kube-proxy routes on the slice, not on InferenceService status:
			// a Ready endpoint here keeps sending Service traffic to a host
			// that stopped heartbeating.
			got := &discoveryv1.EndpointSlice{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Endpoints).To(HaveLen(1))
			Expect(got.Endpoints[0].Conditions.Ready).To(HaveValue(BeFalse()),
				"stale heartbeat must withdraw the endpoint so kube-proxy stops routing to it")
			Expect(got.Endpoints[0].Conditions.Serving).To(HaveValue(BeFalse()))
			// The address and heartbeat stay: the agent's next re-register
			// flips the same object back to Ready.
			Expect(got.Endpoints[0].Addresses).To(Equal([]string{"192.0.2.10"}))
			Expect(got.Annotations[inferencev1alpha1.AnnotationAgentHeartbeat]).To(Equal(stale))
		})

		It("should not touch a stale slice that the metal-agent does not manage", func() {
			stale := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
			ep := metalEndpoints(isvcName, stale)
			delete(ep.Labels, "llmkube.ai/managed-by")
			Expect(k8sClient.Create(ctx, ep)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvcName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			got := &discoveryv1.EndpointSlice{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Endpoints[0].Conditions.Ready).To(HaveValue(BeTrue()))
		})
	})
})

var _ = Describe("metal service scheduling status", func() {
	const namespace = "default"

	var (
		reconciler *InferenceServiceReconciler
		ctx        context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
	})

	// readyMetalFixtures creates a Ready metal Model and a one-replica
	// InferenceService, the minimum the phase determination needs to reach
	// PhaseReady once an EndpointSlice exists.
	readyMetalFixtures := func(modelName, isvcName string) *inferencev1alpha1.InferenceService {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/sched-model.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, model) })
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		replicas := int32(1)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: modelName,
				Replicas: &replicas,
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, isvc) })
		return isvc
	}

	It("clears a stale WaitingForMetalAgent once the agent is serving", func() {
		const isvcName = "sched-clear-isvc"
		isvc := readyMetalFixtures("sched-clear-model", isvcName)

		fresh := time.Now().UTC().Format(time.RFC3339)
		slice := metalEndpoints(isvcName, fresh)
		Expect(k8sClient.Create(ctx, slice)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, slice) })

		key := types.NamespacedName{Name: isvcName, Namespace: namespace}
		Expect(k8sClient.Get(ctx, key, isvc)).To(Succeed())
		isvc.Status.SchedulingStatus = "WaitingForMetalAgent"
		isvc.Status.SchedulingMessage = "Waiting for the host metal-agent to fetch the model and register Endpoints"
		Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		got := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(PhaseReady))
		Expect(got.Status.SchedulingStatus).To(BeEmpty(),
			"a Ready metal service must not keep advertising WaitingForMetalAgent")
		Expect(got.Status.SchedulingMessage).To(BeEmpty())

		// A second pass must not re-add the cleared status.
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.SchedulingStatus).To(BeEmpty())
	})

	It("clears a stale AgentHeartbeatStale once the agent is serving", func() {
		const isvcName = "sched-hbstale-isvc"
		isvc := readyMetalFixtures("sched-hbstale-model", isvcName)

		fresh := time.Now().UTC().Format(time.RFC3339)
		slice := metalEndpoints(isvcName, fresh)
		Expect(k8sClient.Create(ctx, slice)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, slice) })

		key := types.NamespacedName{Name: isvcName, Namespace: namespace}
		Expect(k8sClient.Get(ctx, key, isvc)).To(Succeed())
		isvc.Status.SchedulingStatus = "AgentHeartbeatStale"
		isvc.Status.SchedulingMessage = "metal-agent heartbeat stale (last seen 2026-01-01T00:00:00Z); host may be offline"
		Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		got := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(PhaseReady))
		Expect(got.Status.SchedulingStatus).To(BeEmpty(),
			"a Ready metal service must not keep advertising AgentHeartbeatStale")
		Expect(got.Status.SchedulingMessage).To(BeEmpty())
	})

	// #2410: a Mac serves one replica of an InferenceService, and a second
	// Mac in the namespace overwrites the first one's registration rather
	// than adding a replica, so replicas: 2 could never become Ready. The
	// controller serves it as one replica and says why.
	It("serves a metal service with replicas above 1 as one replica and warns", func() {
		const isvcName = "sched-two-replicas-isvc"
		isvc := readyMetalFixtures("sched-two-replicas-model", isvcName)
		key := types.NamespacedName{Name: isvcName, Namespace: namespace}
		Expect(k8sClient.Get(ctx, key, isvc)).To(Succeed())
		isvc.Spec.Replicas = ptr.To(int32(2))
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())

		fresh := time.Now().UTC().Format(time.RFC3339)
		slice := metalEndpoints(isvcName, fresh)
		Expect(k8sClient.Create(ctx, slice)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, slice) })

		recorder := events.NewFakeRecorder(20)
		reconciler.Recorder = recorder
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		got := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(PhaseReady),
			"one Mac serving a replicas: 2 metal service is all a Mac can serve")
		Expect(got.Status.DesiredReplicas).To(Equal(int32(1)))

		var warned bool
		for len(recorder.Events) > 0 {
			if e := <-recorder.Events; strings.Contains(e, "Warning MetalReplicasAboveOne") &&
				strings.Contains(e, "spec.replicas is 2") {
				warned = true
			}
		}
		Expect(warned).To(BeTrue(), "the controller must say why replicas: 2 is served as one")
	})

	It("keeps WaitingForMetalAgent while the agent has not registered", func() {
		const isvcName = "sched-wait-isvc"
		readyMetalFixtures("sched-wait-model", isvcName)

		key := types.NamespacedName{Name: isvcName, Namespace: namespace}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		got := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(PhaseCreating))
		Expect(got.Status.SchedulingStatus).To(Equal("WaitingForMetalAgent"),
			"an InferenceService whose agent has not registered must still report WaitingForMetalAgent")
	})
})
