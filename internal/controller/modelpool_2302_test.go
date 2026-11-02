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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

// The #2302 fixture is a warm two-member pool: a is resident, b is held at
// zero. A GitOps tool (or a person) then scales b up. The pool must still
// activate b, and must say who did it when that was not LLMKube.
var _ = Describe("ModelPool #2302 external scale", func() {
	ctx := context.Background()

	type fixture2302 struct {
		poolName string
		recorder *events.FakeRecorder
	}
	var fixtures []*fixture2302
	seq := 0

	name := func(f *fixture2302, role string) string { return f.poolName + "-" + role }

	reconcileOnce := func(f *fixture2302) {
		r := &ModelPoolReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  f.recorder,
			IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) {
				return true, nil
			},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: f.poolName, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	member := func(f *fixture2302, role string) *inferencev1alpha1.InferenceService {
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name(f, role), Namespace: "default"}, isvc)).To(Succeed())
		return isvc
	}
	poolOf := func(f *fixture2302) *inferencev1alpha1.ModelPool {
		p := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.poolName, Namespace: "default"}, p)).To(Succeed())
		return p
	}
	setPhase := func(f *fixture2302, role, phase string) {
		isvc := member(f, role)
		isvc.Status.Phase = phase
		Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())
	}
	recorded := func(f *fixture2302) []string {
		var out []string
		for {
			select {
			case e := <-f.recorder.Events:
				out = append(out, e)
			default:
				return out
			}
		}
	}
	externallyScaled := func(f *fixture2302) *metav1.Condition {
		return findPoolCondition(poolOf(f).Status.Conditions, inferencev1alpha1.ConditionModelPoolExternallyScaled)
	}

	// newWarmPool builds the fixture with a resident and b held at zero by the
	// pool. The members are created at their replica counts by the test
	// client, which is not an LLMKube field manager: the cold pool's first
	// owner must not be reported.
	newWarmPool := func() *fixture2302 {
		seq++
		f := &fixture2302{poolName: fmt.Sprintf("x2302-%d", seq), recorder: events.NewFakeRecorder(16)}
		fixtures = append(fixtures, f)
		for role, n := range map[string]int32{"a": 1, "b": 0} {
			Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name(f, role), Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "some-model",
					Replicas: ptr.To(n),
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
				},
			})).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, &inferencev1alpha1.ModelPool{
			ObjectMeta: metav1.ObjectMeta{Name: f.poolName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelPoolSpec{
				GPU:     1,
				Default: name(f, "a"),
				Members: []inferencev1alpha1.ModelPoolMember{
					{InferenceServiceRef: corev1.LocalObjectReference{Name: name(f, "a")}},
					{InferenceServiceRef: corev1.LocalObjectReference{Name: name(f, "b")}},
				},
			},
		})).To(Succeed())

		reconcileOnce(f)
		setPhase(f, "a", PhaseReady)
		reconcileOnce(f)
		Expect(poolOf(f).Status.ResidentMember).To(Equal(name(f, "a")))
		Expect(replicasOf(member(f, "b"))).To(Equal(int32(0)))
		Expect(drainedByPool(member(f, "b"))).To(BeTrue())
		Expect(recorded(f)).To(BeEmpty(), "bringing up a cold pool is not an outside scale-up")
		Expect(externallyScaled(f)).To(BeNil())
		return f
	}

	// Ways to scale a member to one replica under a given field manager.
	serverSideApply := func(f *fixture2302, role, manager string) {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(inferencev1alpha1.GroupVersion.String())
		u.SetKind("InferenceService")
		u.SetName(name(f, role))
		u.SetNamespace("default")
		Expect(unstructured.SetNestedField(u.Object, int64(1), "spec", "replicas")).To(Succeed())
		Expect(k8sClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
			client.FieldOwner(manager), client.ForceOwnership)).To(Succeed())
	}
	scaleSubresource := func(f *fixture2302, role, manager string) {
		Expect(k8sClient.SubResource("scale").Update(ctx, member(f, role),
			client.WithSubResourceBody(&autoscalingv1.Scale{Spec: autoscalingv1.ScaleSpec{Replicas: 1}}),
			client.FieldOwner(manager))).To(Succeed())
	}
	mergePatch := func(f *fixture2302, role, manager string) {
		cur := member(f, role)
		patched := cur.DeepCopy()
		patched.Spec.Replicas = ptr.To(int32(1))
		Expect(k8sClient.Patch(ctx, patched, client.MergeFrom(cur), client.FieldOwner(manager))).To(Succeed())
	}
	routerActivation := func(f *fixture2302, role, _ string) {
		Expect(router.NewKubeMemberController(k8sClient, time.Millisecond).Activate(ctx, "default", name(f, role))).To(Succeed())
	}

	replicasOwners := func(isvc *inferencev1alpha1.InferenceService) []string {
		var owners []string
		for _, e := range isvc.ManagedFields {
			if ownsSpecReplicas(e) {
				owners = append(owners, e.Manager)
			}
		}
		return owners
	}

	// expectActivated asserts the scale-up was honored: b is the pending
	// owner, a is drained under the pool's field manager, and b keeps its
	// replica.
	expectActivated := func(f *fixture2302) {
		Expect(poolOf(f).Status.PendingMember).To(Equal(name(f, "b")), "the scale-up still activates the member")
		Expect(replicasOf(member(f, "a"))).To(Equal(int32(0)))
		Expect(replicasOwners(member(f, "a"))).To(ConsistOf(inferencev1alpha1.ModelPoolFieldManager))
		Expect(replicasOf(member(f, "b"))).To(Equal(int32(1)))
	}

	AfterEach(func() {
		for _, f := range fixtures {
			for _, role := range []string{"a", "b"} {
				if isvc := (&inferencev1alpha1.InferenceService{}); k8sClient.Get(ctx, types.NamespacedName{Name: name(f, role), Namespace: "default"}, isvc) == nil {
					Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
				}
			}
			if p := (&inferencev1alpha1.ModelPool{}); k8sClient.Get(ctx, types.NamespacedName{Name: f.poolName, Namespace: "default"}, p) == nil {
				Expect(k8sClient.Delete(ctx, p)).To(Succeed())
			}
		}
		fixtures = nil
	})

	DescribeTable("reports a held member scaled up by another field manager and still activates it",
		func(scale func(*fixture2302, string, string), manager string) {
			f := newWarmPool()
			scale(f, "b", manager)
			reconcileOnce(f)

			expectActivated(f)
			cond := externallyScaled(f)
			Expect(cond).NotTo(BeNil(), "ExternallyScaled must be set")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal(reasonExternalFieldManager))
			Expect(cond.Message).To(ContainSubstring(fmt.Sprintf("%q", name(f, "b"))))
			Expect(cond.Message).To(ContainSubstring(fmt.Sprintf("%q", manager)))
			ext := poolOf(f).Status.ExternalScale
			Expect(ext).NotTo(BeNil())
			Expect(ext.Member).To(Equal(name(f, "b")))
			Expect(ext.Manager).To(Equal(manager))

			evs := recorded(f)
			Expect(evs).To(HaveLen(1))
			Expect(evs[0]).To(HavePrefix(corev1.EventTypeWarning + " " + eventReasonExternalScale + " "))
			Expect(evs[0]).To(ContainSubstring(fmt.Sprintf("%q", name(f, "b"))))
			Expect(evs[0]).To(ContainSubstring(fmt.Sprintf("%q", manager)))

			By("not reporting the same scale-up again on the next pass")
			reconcileOnce(f)
			Expect(recorded(f)).To(BeEmpty())
			Expect(externallyScaled(f).Status).To(Equal(metav1.ConditionTrue))
		},
		Entry("Flux server-side apply", serverSideApply, "kustomize-controller"),
		Entry("the scale subresource, as kubectl scale uses", scaleSubresource, "kubectl"),
		Entry("a merge patch", mergePatch, "argocd-controller"),
	)

	DescribeTable("reports nothing when LLMKube scaled the member up",
		func(scale func(*fixture2302, string, string), manager string) {
			f := newWarmPool()
			scale(f, "b", manager)
			reconcileOnce(f)

			expectActivated(f)
			Expect(recorded(f)).To(BeEmpty())
			Expect(externallyScaled(f)).To(BeNil())
			Expect(poolOf(f).Status.ExternalScale).To(BeNil())

			By("finishing the swap and swapping back through the router")
			setPhase(f, "a", "Stopped")
			setPhase(f, "b", PhaseReady)
			reconcileOnce(f)
			Expect(poolOf(f).Status.ResidentMember).To(Equal(name(f, "b")))
			routerActivation(f, "a", "")
			reconcileOnce(f)
			Expect(poolOf(f).Status.PendingMember).To(Equal(name(f, "a")))
			Expect(replicasOf(member(f, "b"))).To(Equal(int32(0)))
			Expect(recorded(f)).To(BeEmpty())
			Expect(externallyScaled(f)).To(BeNil())
		},
		Entry("the router's activation", routerActivation, inferencev1alpha1.RouterProxyFieldManager),
		Entry("the pool's own field manager", mergePatch, inferencev1alpha1.ModelPoolFieldManager),
	)

	It("raises one event per manager across a GitOps loop and clears after the quiet period", func() {
		f := newWarmPool()

		By("the first sync scales b up")
		serverSideApply(f, "b", "kustomize-controller")
		reconcileOnce(f)
		Expect(recorded(f)).To(HaveLen(1))

		By("the swap completes, then the next sync scales a back up")
		setPhase(f, "a", "Stopped")
		setPhase(f, "b", PhaseReady)
		reconcileOnce(f)
		Expect(poolOf(f).Status.ResidentMember).To(Equal(name(f, "b")))
		serverSideApply(f, "a", "kustomize-controller")
		reconcileOnce(f)
		Expect(poolOf(f).Status.PendingMember).To(Equal(name(f, "a")), "the sync still activates a")
		Expect(recorded(f)).To(BeEmpty(), "the same manager inside the quiet period is not reported twice")
		cond := externallyScaled(f)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Message).To(ContainSubstring(fmt.Sprintf("%q", name(f, "a"))), "the condition names the latest member")
		Expect(poolOf(f).Status.ExternalScale.Member).To(Equal(name(f, "a")))

		By("a different manager is reported even inside the quiet period")
		setPhase(f, "b", "Stopped")
		setPhase(f, "a", PhaseReady)
		reconcileOnce(f)
		Expect(poolOf(f).Status.ResidentMember).To(Equal(name(f, "a")))
		scaleSubresource(f, "b", "kubectl")
		reconcileOnce(f)
		evs := recorded(f)
		Expect(evs).To(HaveLen(1))
		Expect(evs[0]).To(ContainSubstring(`"kubectl"`))

		By("clearing once the quiet period passes with no outside scale-up")
		p := poolOf(f)
		p.Status.ExternalScale.ObservedAt = metav1.NewTime(time.Now().Add(-externalScaleQuietPeriod - time.Minute))
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		reconcileOnce(f)
		cond = externallyScaled(f)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(reasonExternalScaleQuiet))
		Expect(recorded(f)).To(BeEmpty())
	})
})
