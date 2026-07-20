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

// Unit tests for the optional-verify ("gateless") issue-batch shape
// (#1151): a Workload with no VerifierAgentRef decomposes to
// code -> review, with the coder's own verification and the repo's CI
// standing in for the deterministic gate.

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// TestSynthesizeIssueBatch_GatelessCodeToReview: no VerifierAgentRef
// emits code-<N> plus the reviewer fan-out only, with each review
// depending directly on the code step.
func TestSynthesizeIssueBatch_GatelessCodeToReview(t *testing.T) {
	w := &foremanv1alpha1.Workload{}
	w.Name = "wl"
	w.Spec.Repo = "defilantech/LLMKube"
	w.Spec.Issues = []int32{7}
	w.Spec.CoderAgentRef = &corev1.LocalObjectReference{Name: "coder"}
	w.Spec.ReviewerAgentRefs = []corev1.LocalObjectReference{{Name: "reviewer-a"}, {Name: "reviewer-b"}}

	steps := synthesizeIssueBatch(w)
	want := []string{"code-7", "review-7-0", "review-7-1"}
	if len(steps) != len(want) {
		t.Fatalf("got %d steps, want %d (%v)", len(steps), len(want), want)
	}
	for i, name := range want {
		if steps[i].Name != name {
			t.Fatalf("step[%d] = %q, want %q", i, steps[i].Name, name)
		}
	}
	for _, s := range steps {
		if s.Kind == foremanv1alpha1.AgenticTaskKindVerify {
			t.Fatalf("gateless batch must not emit a verify step, got %q", s.Name)
		}
		if s.Kind == foremanv1alpha1.AgenticTaskKindReview {
			if len(s.DependsOn) != 1 || s.DependsOn[0] != "code-7" {
				t.Errorf("review %q dependsOn = %v, want [code-7]", s.Name, s.DependsOn)
			}
		}
	}
}

// TestSynthesizeIssueBatch_VerifierKeepsGate pins the existing shape:
// with a VerifierAgentRef the verify step remains and reviews depend on
// it, not on the code step.
func TestSynthesizeIssueBatch_VerifierKeepsGate(t *testing.T) {
	w := &foremanv1alpha1.Workload{}
	w.Name = "wl"
	w.Spec.Repo = "defilantech/LLMKube"
	w.Spec.Issues = []int32{7}
	w.Spec.CoderAgentRef = &corev1.LocalObjectReference{Name: "coder"}
	w.Spec.VerifierAgentRef = &corev1.LocalObjectReference{Name: "gate"}
	w.Spec.ReviewerAgentRefs = []corev1.LocalObjectReference{{Name: "reviewer"}}

	steps := synthesizeIssueBatch(w)
	want := []string{"code-7", "verify-7", "review-7-0"}
	if len(steps) != len(want) {
		t.Fatalf("got %d steps, want %d (%v)", len(steps), len(want), want)
	}
	for i, name := range want {
		if steps[i].Name != name {
			t.Fatalf("step[%d] = %q, want %q", i, steps[i].Name, name)
		}
	}
	if d := steps[2].DependsOn; len(d) != 1 || d[0] != "verify-7" {
		t.Errorf("review dependsOn = %v, want [verify-7]", d)
	}
}

// TestChooseSteps_IssueBatchWithoutVerifier: the issue-batch shortcut
// accepts a nil VerifierAgentRef and still requires the coder.
func TestChooseSteps_IssueBatchWithoutVerifier(t *testing.T) {
	r := &WorkloadReconciler{}

	w := &foremanv1alpha1.Workload{}
	w.Name = "wl"
	w.Spec.Repo = "defilantech/LLMKube"
	w.Spec.Issues = []int32{7}
	w.Spec.CoderAgentRef = &corev1.LocalObjectReference{Name: "coder"}
	steps, _, modeErr := r.chooseSteps(w)
	if modeErr != nil {
		t.Fatalf("nil verifier must be accepted, got error: %v", modeErr)
	}
	if len(steps) != 1 || steps[0].Name != "code-7" {
		t.Fatalf("steps = %+v, want the single code-7 step", steps)
	}

	w2 := &foremanv1alpha1.Workload{}
	w2.Spec.Issues = []int32{7}
	if _, _, err := r.chooseSteps(w2); err == nil {
		t.Fatal("nil coder must still error")
	}
}

// TestCoderEscalationSteps_GatelessSkipsVerify: with no VerifierAgentRef
// the escalated tier emits code-<N>-esc plus the reviewer fan-out, with
// escalated reviews depending directly on the escalated code step.
func TestCoderEscalationSteps_GatelessSkipsVerify(t *testing.T) {
	w := &foremanv1alpha1.Workload{}
	w.Name = "wl"
	w.Spec.Repo = "defilantech/LLMKube"
	w.Spec.Issues = []int32{944}
	w.Spec.CoderAgentRef = &corev1.LocalObjectReference{Name: "coder-metal"}
	w.Spec.EscalationCoderAgentRef = &corev1.LocalObjectReference{Name: "coder-qwopus"}
	w.Spec.ReviewerAgentRefs = []corev1.LocalObjectReference{{Name: "reviewer"}}

	code := foremanv1alpha1.AgenticTask{}
	code.Name = "wl-code-944"
	code.Labels = map[string]string{labelStep: "code-944"}
	code.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
	code.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
	code.Status.Result = resultRaw("MODEL-DECIDED", "", "could not solve", "")

	steps, escalated, _ := coderEscalationSteps(w, []foremanv1alpha1.AgenticTask{code})
	want := []string{"code-944-esc", "review-944-esc-0"}
	if len(steps) != len(want) {
		t.Fatalf("got %d steps, want %d (%v)", len(steps), len(want), want)
	}
	for i, name := range want {
		if steps[i].Name != name {
			t.Fatalf("step[%d] = %q, want %q", i, steps[i].Name, name)
		}
	}
	if d := steps[1].DependsOn; len(d) != 1 || d[0] != "code-944-esc" {
		t.Errorf("escalated review dependsOn = %v, want [code-944-esc]", d)
	}
	if len(escalated) != 1 || escalated[0] != 944 {
		t.Errorf("escalated = %v, want [944]", escalated)
	}
}

// TestReviewIterationSteps_GatelessReviewDependsOnCode: a NO-GO round on
// a gateless Workload emits the r1 fix iteration as code -> review with
// no verify step.
func TestReviewIterationSteps_GatelessReviewDependsOnCode(t *testing.T) {
	w := iterationWorkload([]int32{641}, 1, nil)
	w.Spec.VerifierAgentRef = nil
	children := []foremanv1alpha1.AgenticTask{
		child("code-641", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictGo),
		child("review-641-0", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictNoGo),
	}

	steps, iterated := reviewIterationSteps(w, children)
	want := []string{"code-641-r1", "review-641-0-r1"}
	if len(steps) != len(want) {
		t.Fatalf("got %d steps, want %d (%v)", len(steps), len(want), want)
	}
	for i, name := range want {
		if steps[i].Name != name {
			t.Fatalf("step[%d] = %q, want %q", i, steps[i].Name, name)
		}
	}
	if d := steps[1].DependsOn; len(d) != 1 || d[0] != "code-641-r1" {
		t.Errorf("iterated review dependsOn = %v, want [code-641-r1]", d)
	}
	if len(iterated) != 1 || iterated[0] != 641 {
		t.Errorf("iterated = %v, want [641]", iterated)
	}
}
