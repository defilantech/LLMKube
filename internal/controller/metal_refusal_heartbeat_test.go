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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	metalagent "github.com/defilantech/llmkube/pkg/agent"
)

// A refused Metal service with a stale EndpointSlice had the controller and
// the agent overwrite each other's scheduling status for as long as the slice
// stayed (#2457). The specs below run the agent's own withdrawal between
// reconciles, so a slice the agent re-stamps and one it leaves stale both
// take their real path.
var _ = Describe("metal agent refusal with a stale EndpointSlice", func() {
	const (
		refusal    = inferencev1alpha1.ReasonMemoryCheckFailed
		refusalMsg = "memory estimation failed: cannot determine model size: file not found"
		// base64 of 32 bytes, so both pass as SPKI pins.
		pin      = "aXNzdWUtMjQ1Ny1pbmdyZXNzLXNwa2ktcGluLTMyYiE="
		otherPin = "aXNzdWUtMjQ1Ny1hbm90aGVyLW1hYy1zcGtpLTMyYiE="
	)

	var (
		ctx        context.Context
		reconciler *InferenceServiceReconciler
		namespace  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &InferenceServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "metal-refusal-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name
	})

	// envtest deletes neither namespaces nor owned objects, so every object a
	// spec or its reconciles create is removed by name. create puts each
	// fixture in the spec's namespace, including the slices metalEndpoints and
	// agentSlice build in "default".
	deleteLater := func(obj client.Object) {
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
	}
	create := func(obj client.Object) {
		obj.SetNamespace(namespace)
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		deleteLater(obj)
	}
	deleteServiceLater := func(name string) {
		deleteLater(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
	}
	tenMinutesAgo := func() string { return time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339) }

	// refusedService creates a Ready metal Model and an InferenceService whose
	// status carries the agent's refusal.
	refusedService := func(name string) *inferencev1alpha1.InferenceService {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: namespace},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/model.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
			},
		}
		create(model)
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		replicas := int32(1)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name, Replicas: &replicas},
		}
		create(isvc)
		isvc.Status.SchedulingStatus = refusal
		isvc.Status.SchedulingMessage = refusalMsg
		Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())
		return isvc
	}

	reconcileISVC := func(key types.NamespacedName) (reconcile.Result, *inferencev1alpha1.InferenceService) {
		res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		got := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		return res, got
	}

	// agentRefusesAgain is what the agent does when the service's
	// resourceVersion moves and the start is still refused: it writes the
	// refusal back unless the status still says so (publishRefusal), then,
	// with no engine running, withdraws the slice it finds for the service
	// (withdrawEndpointFor, WithdrawEndpointIfPresent).
	agentRefusesAgain := func(reg *metalagent.ServiceRegistry, key types.NamespacedName) {
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, isvc)).To(Succeed())
		if isvc.Status.SchedulingStatus != refusal || isvc.Status.SchedulingMessage != refusalMsg {
			isvc.Status.SchedulingStatus = refusal
			isvc.Status.SchedulingMessage = refusalMsg
			Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())
		}
		_, err := reg.WithdrawEndpointIfPresent(ctx, isvc)
		Expect(err).NotTo(HaveOccurred())
	}

	DescribeTable("keeps the refusal while the agent keeps refusing",
		func(name string, relayAgent bool, mutate func(*discoveryv1.EndpointSlice)) {
			isvc := refusedService(name)
			key := client.ObjectKeyFromObject(isvc)
			slice := metalEndpoints(name, tenMinutesAgo())
			mutate(slice)
			create(slice)
			deleteServiceLater(name)

			reg := metalagent.NewServiceRegistry(k8sClient, "192.0.2.10", zap.NewNop().Sugar(), "")
			if relayAgent {
				Expect(reg.EnableIngress(9443, pin)).To(Succeed())
			}
			const rounds = 3
			seen := make([]string, 0, rounds)
			for range rounds {
				_, got := reconcileISVC(key)
				seen = append(seen, got.Status.SchedulingStatus)
				agentRefusesAgain(reg, key)
			}
			Expect(seen).To(HaveEach(Equal(refusal)), "scheduling status after each reconcile")
		},
		Entry("on the agent's own slice", "refusal-own-slice", false,
			func(*discoveryv1.EndpointSlice) {}),
		Entry("on a slice under another name", "refusal-other-name", false,
			func(s *discoveryv1.EndpointSlice) { s.Name += "-x7k2p" }),
		Entry("on a slice with no port", "refusal-no-port", false,
			func(s *discoveryv1.EndpointSlice) { s.Ports = nil }),
		Entry("on a slice without the managed-by label", "refusal-no-label", false,
			func(s *discoveryv1.EndpointSlice) { delete(s.Labels, inferencev1alpha1.LabelManagedBy) }),
		// The agent was switched to relay mode while refused: it looks for
		// "<isvc>-agent", finds none and returns before the relay-mode write
		// that removes its old "<isvc>" slice.
		Entry("on a legacy slice of an agent now in relay mode", "refusal-mode-switch", true,
			func(*discoveryv1.EndpointSlice) {}),
	)

	Context("in relay mode", func() {
		// relayService is a refused service whose agent went offline after
		// serving it through the relay: an owned relay Deployment pinned to
		// pin and an "<isvc>-agent" slice carrying heartbeat.
		relayService := func(name, heartbeat string) types.NamespacedName {
			isvc := refusedService(name)
			dep := reconciler.newRelayDeployment(isvc, pin)
			Expect(setControllerReferenceUnblocked(isvc, dep, k8sClient.Scheme())).To(Succeed())
			create(dep)
			deleteServiceLater(name)
			deleteLater(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: inferencev1alpha1.MetalRelaySecretName, Namespace: namespace}})
			create(agentSlice(name, heartbeat, pin))
			return client.ObjectKeyFromObject(isvc)
		}

		setAgentConnected := func(mn *inferencev1alpha1.MetalNode, reason string) {
			status := metav1.ConditionTrue
			if reason == inferencev1alpha1.MetalNodeReasonHeartbeatStale {
				status = metav1.ConditionFalse
			}
			meta.SetStatusCondition(&mn.Status.Conditions, metav1.Condition{
				Type:    inferencev1alpha1.MetalNodeConditionAgentConnected,
				Status:  status,
				Reason:  reason,
				Message: "set by the test",
			})
			Expect(k8sClient.Status().Update(ctx, mn)).To(Succeed())
		}

		metalNode := func(name, ingressPin, reason string) *inferencev1alpha1.MetalNode {
			mn := &inferencev1alpha1.MetalNode{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       inferencev1alpha1.MetalNodeSpec{ServiceAccountName: name, LeaseName: name},
			}
			create(mn)
			mn.Status.IngressSPKI = ingressPin
			setAgentConnected(mn, reason)
			return mn
		}

		It("keeps the refusal while the Mac's MetalNode heartbeat is fresh", func() {
			key := relayService("refusal-node-fresh", tenMinutesAgo())
			metalNode("refusal-node-fresh-mac", pin, inferencev1alpha1.MetalNodeReasonHeartbeatFresh)

			_, got := reconcileISVC(key)
			Expect(got.Status.SchedulingStatus).To(Equal(refusal))
			Expect(got.Status.SchedulingMessage).To(Equal(refusalMsg))
		})

		It("reports AgentHeartbeatStale once the Mac's MetalNode is stale too", func() {
			key := relayService("refusal-node-stale", tenMinutesAgo())
			metalNode("refusal-node-stale-mac", pin, inferencev1alpha1.MetalNodeReasonHeartbeatStale)

			_, got := reconcileISVC(key)
			Expect(got.Status.SchedulingStatus).To(Equal("AgentHeartbeatStale"))
			Expect(got.Status.SchedulingMessage).To(ContainSubstring("last seen"))
		})

		It("keeps the refusal when the stale MetalNode reports another Mac's pin", func() {
			key := relayService("refusal-node-other", tenMinutesAgo())
			metalNode("refusal-node-other-mac", otherPin, inferencev1alpha1.MetalNodeReasonHeartbeatStale)

			_, got := reconcileISVC(key)
			Expect(got.Status.SchedulingStatus).To(Equal(refusal))
		})

		// This controller does not watch MetalNodes and an unparseable
		// heartbeat never expires, so only a requeue notices the Mac going
		// away.
		It("requeues a refusal held over an unparseable heartbeat until the MetalNode goes stale", func() {
			key := relayService("refusal-node-goes-stale", "not-a-timestamp")
			mn := metalNode("refusal-node-goes-stale-mac", pin, inferencev1alpha1.MetalNodeReasonHeartbeatFresh)

			res, got := reconcileISVC(key)
			Expect(got.Status.SchedulingStatus).To(Equal(refusal))
			Expect(res.RequeueAfter).To(Equal(inferencev1alpha1.DefaultAgentHeartbeatTimeout / 2))

			setAgentConnected(mn, inferencev1alpha1.MetalNodeReasonHeartbeatStale)
			_, got = reconcileISVC(key)
			Expect(got.Status.SchedulingStatus).To(Equal("AgentHeartbeatStale"))
			Expect(got.Status.SchedulingMessage).To(ContainSubstring("unparseable"))
		})
	})
})
