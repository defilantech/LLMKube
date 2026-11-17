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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

// newStatusBuilderReconciler builds a minimal InferenceServiceReconciler backed
// by a fake client for unit-testing updateStatusWithSchedulingInfo without
// standing up envtest.
func newStatusBuilderReconciler(t *testing.T, isvc *inferencev1alpha1.InferenceService) *InferenceServiceReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{}).
		WithObjects(isvc).
		Build()
	return &InferenceServiceReconciler{
		Client: c,
		Scheme: scheme,
	}
}

// seedReadyAvailable writes a stale Available: True condition onto the
// InferenceService, simulating a service that was previously Ready. The
// status update is persisted so updateStatusWithSchedulingInfo reads it back.
func seedReadyAvailable(t *testing.T, ctx context.Context, c client.Client, isvc *inferencev1alpha1.InferenceService) {
	t.Helper()
	isvc.Status.Phase = PhaseReady
	isvc.Status.ReadyReplicas = 1
	isvc.Status.DesiredReplicas = 1
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               "Available",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: isvc.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             "InferenceReady",
		Message:            "Inference service is ready and serving requests",
	})
	if err := c.Status().Update(ctx, isvc); err != nil {
		t.Fatalf("seed status update: %v", err)
	}
}

// TestUpdateStatusStoppedClearsStaleAvailable verifies that a previously-Ready
// InferenceService scaled to zero (Phase=Stopped) reports Available: False
// with reason ManuallyScaledToZero, rather than keeping the stale Available:
// True from the Ready era. This mirrors the metal-agent's markStopped
// convention so the two status writers agree.
func TestUpdateStatusStoppedClearsStaleAvailable(t *testing.T) {
	ctx := context.Background()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "stopped-isvc", Namespace: "default", Generation: 1},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "some-model",
			Replicas: ptrInt32(0),
		},
	}
	r := newStatusBuilderReconciler(t, isvc)
	seedReadyAvailable(t, ctx, r.Client, isvc)

	if _, err := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseStopped, false, 0, 0, 0, "", "", nil); err != nil {
		t.Fatalf("updateStatusWithSchedulingInfo: %v", err)
	}

	got := &inferencev1alpha1.InferenceService{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace}, got); err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Status.Phase != PhaseStopped {
		t.Fatalf("phase = %q, want %q", got.Status.Phase, PhaseStopped)
	}

	cond := meta.FindStatusCondition(got.Status.Conditions, "Available")
	if cond == nil {
		t.Fatal("Available condition not set on Stopped InferenceService")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Available condition status = %q, want False", cond.Status)
	}
	if cond.Reason != "ManuallyScaledToZero" {
		t.Errorf("Available condition reason = %q, want ManuallyScaledToZero", cond.Reason)
	}
}

// TestUpdateStatusSuspendedClearsStaleAvailable verifies that a previously-Ready
// InferenceService suspended (Phase=Suspended) reports Available: False with
// reason Suspended, rather than keeping the stale Available: True from the
// Ready era. This mirrors the metal-agent's markStopped convention so the two
// status writers agree.
func TestUpdateStatusSuspendedClearsStaleAvailable(t *testing.T) {
	ctx := context.Background()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "suspended-isvc", Namespace: "default", Generation: 1},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "some-model",
			Replicas: ptrInt32(2),
			Suspend:  true,
		},
	}
	r := newStatusBuilderReconciler(t, isvc)
	seedReadyAvailable(t, ctx, r.Client, isvc)

	if _, err := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseSuspended, false, 0, 0, 0, "", "", nil); err != nil {
		t.Fatalf("updateStatusWithSchedulingInfo: %v", err)
	}

	got := &inferencev1alpha1.InferenceService{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace}, got); err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Status.Phase != PhaseSuspended {
		t.Fatalf("phase = %q, want %q", got.Status.Phase, PhaseSuspended)
	}

	cond := meta.FindStatusCondition(got.Status.Conditions, "Available")
	if cond == nil {
		t.Fatal("Available condition not set on Suspended InferenceService")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Available condition status = %q, want False", cond.Status)
	}
	if cond.Reason != "Suspended" {
		t.Errorf("Available condition reason = %q, want Suspended", cond.Reason)
	}
}

// TestUpdateStatusStoppedAndSuspendedReasonsDiffer asserts the two scaled-away
// phases cannot silently collapse into one: Stopped and Suspended must use
// distinct reasons so downstream observers can tell them apart.
func TestUpdateStatusStoppedAndSuspendedReasonsDiffer(t *testing.T) {
	ctx := context.Background()

	stopped := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "stopped-isvc", Namespace: "default", Generation: 1},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "some-model",
			Replicas: ptrInt32(0),
		},
	}
	rStopped := newStatusBuilderReconciler(t, stopped)
	seedReadyAvailable(t, ctx, rStopped.Client, stopped)
	if _, err := rStopped.updateStatusWithSchedulingInfo(ctx, stopped, PhaseStopped, false, 0, 0, 0, "", "", nil); err != nil {
		t.Fatalf("stopped updateStatusWithSchedulingInfo: %v", err)
	}

	suspended := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "suspended-isvc", Namespace: "default", Generation: 1},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "some-model",
			Replicas: ptrInt32(2),
			Suspend:  true,
		},
	}
	rSuspended := newStatusBuilderReconciler(t, suspended)
	seedReadyAvailable(t, ctx, rSuspended.Client, suspended)
	if _, err := rSuspended.updateStatusWithSchedulingInfo(ctx, suspended, PhaseSuspended, false, 0, 0, 0, "", "", nil); err != nil {
		t.Fatalf("suspended updateStatusWithSchedulingInfo: %v", err)
	}

	stoppedCond := meta.FindStatusCondition(stopped.Status.Conditions, "Available")
	suspendedCond := meta.FindStatusCondition(suspended.Status.Conditions, "Available")
	if stoppedCond == nil || suspendedCond == nil {
		t.Fatal("both Available conditions must be set")
	}
	if stoppedCond.Reason == suspendedCond.Reason {
		t.Errorf("Stopped and Suspended reasons collapsed to %q; they must differ", stoppedCond.Reason)
	}
}

// TestUpdateStatusAvailableFalseWhenNoReadyReplicas is the regression test for
// #1303: Available must not stay True on a service that has stopped serving.
//
// determinePhase returns "Creating" whenever readyReplicas==0 &&
// desiredReplicas>0, which covers the unplanned-outage path (drained node,
// CrashLoopBackOff, metal host offline), so this is not a cold-start-only
// concern. Before the fix, the Creating/Progressing/Pending/WaitingForGPU
// cases left Available untouched and a previously-Ready service reported
// Available=True/"ready and serving requests" for as long as it stayed down.
//
// Table covers every phase that can be reached with zero ready replicas so a
// phase added later cannot regress the invariant unnoticed.
func TestUpdateStatusAvailableFalseWhenNoReadyReplicas(t *testing.T) {
	cases := []struct {
		name       string
		phase      string
		wantReason string
	}{
		{"creating is the outage path", "Creating", "NoReadyReplicas"},
		{"progressing with nothing ready", "Progressing", "NoReadyReplicas"},
		{"pending", "Pending", "NoReadyReplicas"},
		{"waiting for gpu", PhaseWaitingForGPU, "NoReadyReplicas"},
		// Phases that set Available themselves keep their specific reason.
		{"stopped keeps its own reason", PhaseStopped, "ManuallyScaledToZero"},
		{"suspended keeps its own reason", PhaseSuspended, "Suspended"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name: "outage-isvc", Namespace: "default", Generation: 1,
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "some-model",
					Replicas: ptrInt32(1),
				},
			}
			r := newStatusBuilderReconciler(t, isvc)
			// Seed the stale True that the bug preserved.
			seedReadyAvailable(t, ctx, r.Client, isvc)

			if _, err := r.updateStatusWithSchedulingInfo(
				ctx, isvc, tc.phase, true, 0, 0, 1, "", "", nil); err != nil {
				t.Fatalf("updateStatusWithSchedulingInfo: %v", err)
			}

			got := &inferencev1alpha1.InferenceService{}
			if err := r.Client.Get(ctx, types.NamespacedName{
				Name: isvc.Name, Namespace: isvc.Namespace,
			}, got); err != nil {
				t.Fatalf("get: %v", err)
			}

			cond := meta.FindStatusCondition(got.Status.Conditions, "Available")
			if cond == nil {
				t.Fatalf("Available condition missing for phase %q", tc.phase)
			}
			if cond.Status != metav1.ConditionFalse {
				t.Errorf("phase %q: Available = %q, want False (0 ready replicas)",
					tc.phase, cond.Status)
			}
			if cond.Reason != tc.wantReason {
				t.Errorf("phase %q: Available reason = %q, want %q",
					tc.phase, cond.Reason, tc.wantReason)
			}
		})
	}
}

// TestUpdateStatusAvailableStaysTrueWithReadyReplicas guards the other
// direction: the #1303 backstop must not fire when replicas are serving. A
// partially-rolled deployment with some Ready replicas is still available,
// matching Deployment semantics.
func TestUpdateStatusAvailableStaysTrueWithReadyReplicas(t *testing.T) {
	ctx := context.Background()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name: "partial-isvc", Namespace: "default", Generation: 1,
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "some-model",
			Replicas: ptrInt32(3),
		},
	}
	r := newStatusBuilderReconciler(t, isvc)
	seedReadyAvailable(t, ctx, r.Client, isvc)

	// 1 of 3 ready: still serving, so Available must remain True.
	if _, err := r.updateStatusWithSchedulingInfo(
		ctx, isvc, "Progressing", true, 1, 1, 3, "", "", nil); err != nil {
		t.Fatalf("updateStatusWithSchedulingInfo: %v", err)
	}

	got := &inferencev1alpha1.InferenceService{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name: isvc.Name, Namespace: isvc.Namespace,
	}, got); err != nil {
		t.Fatalf("get: %v", err)
	}

	cond := meta.FindStatusCondition(got.Status.Conditions, "Available")
	if cond == nil {
		t.Fatal("Available condition missing")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("Available = %q, want True (1/3 replicas serving)", cond.Status)
	}
	if cond.Reason != "InferenceReady" {
		t.Errorf("Available reason = %q, want InferenceReady", cond.Reason)
	}
}

// TestPublishInferenceServiceConfig pins what the reconcile's defer exports
// for capacity: the limits from the spec on every pass, the GPUs and image
// only once a Model resolved.
func TestPublishInferenceServiceConfig(t *testing.T) {
	const name, ns = "publish-config", "default"
	limit := int32(104)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 7},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:       RuntimeVLLM,
			Image:         "vllm/vllm-openai:nightly",
			ParallelSlots: &limit,
			VLLMConfig:    &inferencev1alpha1.VLLMConfig{MaxModelLen: ptrInt32B(524288)},
		},
		Status: inferencev1alpha1.InferenceServiceStatus{Phase: PhaseReady},
	}
	llmkubemetrics.DeleteInferenceServiceSeries(name, ns)
	t.Cleanup(func() { llmkubemetrics.DeleteInferenceServiceSeries(name, ns) })
	labels := prometheus.Labels{"inferenceservice": name, "namespace": ns}

	publishInferenceServiceConfig(isvc, nil, nil)
	if got := testutil.ToFloat64(llmkubemetrics.InferenceServiceConcurrencyLimit.WithLabelValues(name, ns)); got != 104 {
		t.Errorf("concurrency limit = %f, want 104", got)
	}
	if got := testutil.ToFloat64(llmkubemetrics.InferenceServiceContextLength.WithLabelValues(name, ns)); got != 524288 {
		t.Errorf("context length = %f, want 524288", got)
	}
	if got := llmkubemetrics.InferenceServiceConfigInfo.DeletePartialMatch(labels); got != 0 {
		t.Errorf("config_info without a Model = %d series, want 0", got)
	}

	model := &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{
		Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cuda", GPU: &inferencev1alpha1.GPUSpec{Enabled: true, Count: 4}},
	}}
	publishInferenceServiceConfig(isvc, model, nil)
	if got := testutil.ToFloat64(llmkubemetrics.InferenceServiceGPUsPerReplica.WithLabelValues(name, ns)); got != 4 {
		t.Errorf("gpus per replica = %f, want 4", got)
	}
	if got := testutil.ToFloat64(llmkubemetrics.InferenceServiceConfigInfo.WithLabelValues(name, ns, "7", "vllm", "vllm/vllm-openai:nightly")); got != 1 {
		t.Errorf("config_info = %f, want 1", got)
	}
}
