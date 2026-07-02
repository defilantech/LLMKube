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
	"encoding/json"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// TestExtractGateAdvisories covers the pure helper that reads
// gateAdvisories from a coder task's status.result JSON.
func TestExtractGateAdvisories(t *testing.T) {
	makeResult := func(extra map[string]any) *runtime.RawExtension {
		envelope := map[string]any{
			"schemaVersion": "foreman.v1",
			"kind":          "issue-fix",
			"verdict":       "GO",
			"summary":       "done",
			"extra":         extra,
		}
		b, _ := json.Marshal(envelope)
		return &runtime.RawExtension{Raw: b}
	}

	t.Run("returns advisories when present", func(t *testing.T) {
		task := &foremanv1alpha1.AgenticTask{
			Status: foremanv1alpha1.AgenticTaskStatus{
				Result: makeResult(map[string]any{
					"branch": "foreman/wl/issue-510",
					"gateAdvisories": []map[string]string{
						{"check": "grounding-breadth", "detail": "cites dcgm_gpu_utilization (unknown)"},
						{"check": "scope-overlap", "detail": "diff touches unrelated files"},
					},
				}),
			},
		}
		got := extractGateAdvisories(task)
		if len(got) != 2 {
			t.Fatalf("want 2 advisories, got %d: %+v", len(got), got)
		}
		if got[0].Check != "grounding-breadth" {
			t.Errorf("advisory[0].Check = %q, want grounding-breadth", got[0].Check)
		}
		if got[1].Check != "scope-overlap" {
			t.Errorf("advisory[1].Check = %q, want scope-overlap", got[1].Check)
		}
	})

	t.Run("returns nil when gateAdvisories absent", func(t *testing.T) {
		task := &foremanv1alpha1.AgenticTask{
			Status: foremanv1alpha1.AgenticTaskStatus{
				Result: makeResult(map[string]any{
					"branch": "foreman/wl/issue-510",
				}),
			},
		}
		if got := extractGateAdvisories(task); got != nil {
			t.Errorf("want nil, got %+v", got)
		}
	})

	t.Run("returns nil when gateAdvisories is empty", func(t *testing.T) {
		task := &foremanv1alpha1.AgenticTask{
			Status: foremanv1alpha1.AgenticTaskStatus{
				Result: makeResult(map[string]any{
					"gateAdvisories": []map[string]string{},
				}),
			},
		}
		if got := extractGateAdvisories(task); got != nil {
			t.Errorf("want nil for empty advisories, got %+v", got)
		}
	})

	t.Run("returns nil when status.result is nil", func(t *testing.T) {
		task := &foremanv1alpha1.AgenticTask{}
		if got := extractGateAdvisories(task); got != nil {
			t.Errorf("want nil for nil result, got %+v", got)
		}
	})

	t.Run("returns nil for malformed JSON", func(t *testing.T) {
		task := &foremanv1alpha1.AgenticTask{
			Status: foremanv1alpha1.AgenticTaskStatus{
				Result: &runtime.RawExtension{Raw: []byte(`not-json`)},
			},
		}
		if got := extractGateAdvisories(task); got != nil {
			t.Errorf("want nil for malformed JSON, got %+v", got)
		}
	})
}

// makeAdvisoryResult is a test helper that builds a coder-task result
// RawExtension carrying the given advisories in extra["gateAdvisories"].
func makeAdvisoryResult(advisories []foremanv1alpha1.GateAdvisory) *runtime.RawExtension {
	advMaps := make([]map[string]string, len(advisories))
	for i, a := range advisories {
		advMaps[i] = map[string]string{"check": a.Check, "detail": a.Detail}
	}
	extra := map[string]any{"branch": "foreman/advisory-wl/issue-42"}
	if len(advisories) > 0 {
		extra["gateAdvisories"] = advMaps
	}
	envelope := map[string]any{
		"schemaVersion": "foreman.v1",
		"kind":          "issue-fix",
		"verdict":       "GO",
		"summary":       "done",
		"extra":         extra,
	}
	b, _ := json.Marshal(envelope)
	return &runtime.RawExtension{Raw: b}
}

// The Ginkgo spec below is the envtest-backed integration test for
// patchReviewAdvisories. It runs inside the suite so it shares the
// same envtest API server started by BeforeSuite in suite_test.go.
var _ = Describe("patchReviewAdvisories (envtest)", func() {
	var r *WorkloadReconciler

	BeforeEach(func() {
		r = &WorkloadReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("copies gateAdvisories from a completed coder task into its pending review tasks", func() {
		wl := &foremanv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "advisory-integration",
				Namespace: "default",
			},
			Spec: foremanv1alpha1.WorkloadSpec{
				Repo:   "defilantech/LLMKube",
				Intent: "advisory integration test",
				Issues: []int32{42},
			},
		}
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, wl) })

		advisories := []foremanv1alpha1.GateAdvisory{
			{Check: "grounding-breadth", Detail: "cites dcgm_gpu_utilization (unknown)"},
			{Check: "scope-overlap", Detail: "diff touches unrelated api file"},
		}

		codeTask := &foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "advisory-integration-code-42",
				Namespace: "default",
				Labels: map[string]string{
					labelWorkload: "advisory-integration",
					labelStep:     "code-42",
				},
			},
			Spec: foremanv1alpha1.AgenticTaskSpec{
				Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:  "defilantech/LLMKube",
					Issue: 42,
				},
			},
		}
		Expect(k8sClient.Create(ctx, codeTask)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, codeTask) })

		codeTask.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		codeTask.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictGo
		codeTask.Status.Result = makeAdvisoryResult(advisories)
		Expect(k8sClient.Status().Update(ctx, codeTask)).To(Succeed())

		reviewTask := &foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "advisory-integration-review-42-0",
				Namespace: "default",
				Labels: map[string]string{
					labelWorkload: "advisory-integration",
					labelStep:     "review-42-0",
				},
			},
			Spec: foremanv1alpha1.AgenticTaskSpec{
				Kind:      foremanv1alpha1.AgenticTaskKindReview,
				DependsOn: []string{"advisory-integration-verify-42"},
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:   "defilantech/LLMKube",
					Issue:  42,
					Branch: "foreman/advisory-integration/issue-42",
				},
			},
		}
		Expect(k8sClient.Create(ctx, reviewTask)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, reviewTask) })

		children := []foremanv1alpha1.AgenticTask{*codeTask, *reviewTask}
		r.patchReviewAdvisories(ctx, wl, children)

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(reviewTask), &fresh)).To(Succeed())
		Expect(fresh.Spec.Payload.GateAdvisories).To(HaveLen(2))
		Expect(fresh.Spec.Payload.GateAdvisories[0].Check).To(Equal("grounding-breadth"))
		Expect(fresh.Spec.Payload.GateAdvisories[1].Check).To(Equal("scope-overlap"))
	})

	It("skips already-patched review tasks (idempotent)", func() {
		wl := &foremanv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "advisory-idempotent",
				Namespace: "default",
			},
			Spec: foremanv1alpha1.WorkloadSpec{
				Repo:   "defilantech/LLMKube",
				Intent: "idempotent test",
				Issues: []int32{99},
			},
		}
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, wl) })

		advisories := []foremanv1alpha1.GateAdvisory{
			{Check: "grounding-breadth", Detail: "pre-populated"},
		}
		codeTask := &foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "advisory-idempotent-code-99",
				Namespace: "default",
				Labels: map[string]string{
					labelWorkload: "advisory-idempotent",
					labelStep:     "code-99",
				},
			},
			Spec: foremanv1alpha1.AgenticTaskSpec{
				Kind:    foremanv1alpha1.AgenticTaskKindIssueFix,
				Payload: foremanv1alpha1.AgenticTaskPayload{Repo: "defilantech/LLMKube", Issue: 99},
			},
		}
		Expect(k8sClient.Create(ctx, codeTask)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, codeTask) })
		codeTask.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		codeTask.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictGo
		codeTask.Status.Result = makeAdvisoryResult(advisories)
		Expect(k8sClient.Status().Update(ctx, codeTask)).To(Succeed())

		// Review task already has GateAdvisories set (previous reconcile).
		reviewTask := &foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "advisory-idempotent-review-99-0",
				Namespace: "default",
				Labels: map[string]string{
					labelWorkload: "advisory-idempotent",
					labelStep:     "review-99-0",
				},
			},
			Spec: foremanv1alpha1.AgenticTaskSpec{
				Kind: foremanv1alpha1.AgenticTaskKindReview,
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:           "defilantech/LLMKube",
					Issue:          99,
					Branch:         "foreman/advisory-idempotent/issue-99",
					GateAdvisories: []foremanv1alpha1.GateAdvisory{{Check: "already-here", Detail: "existing"}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, reviewTask)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, reviewTask) })

		children := []foremanv1alpha1.AgenticTask{*codeTask, *reviewTask}
		r.patchReviewAdvisories(ctx, wl, children)

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(reviewTask), &fresh)).To(Succeed())
		// Must stay as the original single advisory, not be replaced.
		Expect(fresh.Spec.Payload.GateAdvisories).To(HaveLen(1))
		Expect(fresh.Spec.Payload.GateAdvisories[0].Check).To(Equal("already-here"))
	})

	It("copies gateAdvisories into a pending escalate-N-* task", func() {
		// patchReviewAdvisories handles both review-N-* and escalate-N-*
		// prefixes but only review-N-* was covered by existing tests. This
		// spec exercises the escalate branch so the prefix guard stays tested.
		wl := &foremanv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "advisory-escalate",
				Namespace: "default",
			},
			Spec: foremanv1alpha1.WorkloadSpec{
				Repo:   "defilantech/LLMKube",
				Intent: "escalate advisory test",
				Issues: []int32{42},
			},
		}
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, wl) })

		advisories := []foremanv1alpha1.GateAdvisory{
			{Check: "grounding-breadth", Detail: "cites dcgm_gpu_utilization (unknown)"},
		}

		codeTask := &foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "advisory-escalate-code-42",
				Namespace: "default",
				Labels: map[string]string{
					labelWorkload: "advisory-escalate",
					labelStep:     "code-42",
				},
			},
			Spec: foremanv1alpha1.AgenticTaskSpec{
				Kind:    foremanv1alpha1.AgenticTaskKindIssueFix,
				Payload: foremanv1alpha1.AgenticTaskPayload{Repo: "defilantech/LLMKube", Issue: 42},
			},
		}
		Expect(k8sClient.Create(ctx, codeTask)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, codeTask) })
		codeTask.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		codeTask.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictGo
		codeTask.Status.Result = makeAdvisoryResult(advisories)
		Expect(k8sClient.Status().Update(ctx, codeTask)).To(Succeed())

		// Escalation task with step label "escalate-42-0".
		escalateTask := &foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "advisory-escalate-escalate-42-0",
				Namespace: "default",
				Labels: map[string]string{
					labelWorkload: "advisory-escalate",
					labelStep:     "escalate-42-0",
				},
			},
			Spec: foremanv1alpha1.AgenticTaskSpec{
				Kind: foremanv1alpha1.AgenticTaskKindReview,
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:   "defilantech/LLMKube",
					Issue:  42,
					Branch: "foreman/advisory-escalate/issue-42",
				},
			},
		}
		Expect(k8sClient.Create(ctx, escalateTask)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, escalateTask) })

		children := []foremanv1alpha1.AgenticTask{*codeTask, *escalateTask}
		r.patchReviewAdvisories(ctx, wl, children)

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(escalateTask), &fresh)).To(Succeed())
		Expect(fresh.Spec.Payload.GateAdvisories).To(HaveLen(1))
		Expect(fresh.Spec.Payload.GateAdvisories[0].Check).To(Equal("grounding-breadth"))
	})

	It("skips a non-Pending review task (phase guard)", func() {
		// A review task that has already been claimed (phase=Running) must not
		// have its spec mutated mid-flight. The phase guard in
		// patchReviewAdvisories protects against this; this spec validates it.
		wl := &foremanv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "advisory-phase-guard",
				Namespace: "default",
			},
			Spec: foremanv1alpha1.WorkloadSpec{
				Repo:   "defilantech/LLMKube",
				Intent: "phase guard test",
				Issues: []int32{77},
			},
		}
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, wl) })

		advisories := []foremanv1alpha1.GateAdvisory{
			{Check: "scope-overlap", Detail: "diff touches unrelated files"},
		}

		codeTask := &foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "advisory-phase-guard-code-77",
				Namespace: "default",
				Labels: map[string]string{
					labelWorkload: "advisory-phase-guard",
					labelStep:     "code-77",
				},
			},
			Spec: foremanv1alpha1.AgenticTaskSpec{
				Kind:    foremanv1alpha1.AgenticTaskKindIssueFix,
				Payload: foremanv1alpha1.AgenticTaskPayload{Repo: "defilantech/LLMKube", Issue: 77},
			},
		}
		Expect(k8sClient.Create(ctx, codeTask)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, codeTask) })
		codeTask.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		codeTask.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictGo
		codeTask.Status.Result = makeAdvisoryResult(advisories)
		Expect(k8sClient.Status().Update(ctx, codeTask)).To(Succeed())

		// Review task is Running; GateAdvisories must stay nil after the call.
		reviewTask := &foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "advisory-phase-guard-review-77-0",
				Namespace: "default",
				Labels: map[string]string{
					labelWorkload: "advisory-phase-guard",
					labelStep:     "review-77-0",
				},
			},
			Spec: foremanv1alpha1.AgenticTaskSpec{
				Kind: foremanv1alpha1.AgenticTaskKindReview,
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:   "defilantech/LLMKube",
					Issue:  77,
					Branch: "foreman/advisory-phase-guard/issue-77",
				},
			},
		}
		Expect(k8sClient.Create(ctx, reviewTask)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, reviewTask) })

		// Advance the review task to Running via status update.
		reviewTask.Status.Phase = foremanv1alpha1.AgenticTaskPhaseRunning
		Expect(k8sClient.Status().Update(ctx, reviewTask)).To(Succeed())

		children := []foremanv1alpha1.AgenticTask{*codeTask, *reviewTask}
		r.patchReviewAdvisories(ctx, wl, children)

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(reviewTask), &fresh)).To(Succeed())
		// Phase guard: spec must be untouched; GateAdvisories stays nil.
		Expect(fresh.Spec.Payload.GateAdvisories).To(BeNil())
	})
})
