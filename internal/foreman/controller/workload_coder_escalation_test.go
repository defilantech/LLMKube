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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// resultRaw builds a status.result RawExtension with the given top-level
// and nested modelExtra outcome, matching the executor's envelope shape.
// nolint:unparam // topOutcome is parameterized to mirror the envelope shape even though current tests all pass "MODEL-DECIDED"
func resultRaw(topOutcome, modelOutcome, summary string) *runtime.RawExtension {
	me := ""
	if modelOutcome != "" {
		me = `,"modelExtra":{"outcome":"` + modelOutcome + `"}`
	}
	j := `{"summary":"` + summary + `","extra":{"outcome":"` + topOutcome + `"` + me + `}}`
	return &runtime.RawExtension{Raw: []byte(j)}
}

func TestShouldEscalateCoder(t *testing.T) {
	cases := []struct {
		name         string
		verdict      foremanv1alpha1.AgenticTaskVerdict
		topOutcome   string
		modelOutcome string
		want         bool
	}{
		{"model NO-GO (like #944)", foremanv1alpha1.AgenticTaskVerdictNoGo, "MODEL-DECIDED", "", true},
		{"gate-failed (like #911)", foremanv1alpha1.AgenticTaskVerdictIncomplete, "MODEL-DECIDED", "CODER-GATE-FAILED", true},
		{"model gave up / stuck (like #921)", foremanv1alpha1.AgenticTaskVerdictIncomplete, "MODEL-DECIDED", "", false},
		{"stuck-loop detected", foremanv1alpha1.AgenticTaskVerdictIncomplete, "STUCK-LOOP-DETECTED", "", false},
		{"NO-GO but no-changes (trivial)", foremanv1alpha1.AgenticTaskVerdictNoGo, "NO-CHANGES", "", false},
		{"GO", foremanv1alpha1.AgenticTaskVerdictGo, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldEscalateCoder(tc.verdict, tc.topOutcome, tc.modelOutcome); got != tc.want {
				t.Errorf("shouldEscalateCoder(%s,%q,%q)=%v want %v",
					tc.verdict, tc.topOutcome, tc.modelOutcome, got, tc.want)
			}
		})
	}
}

func TestCoderTerminalOutcome_ReadsNestedGateOutcome(t *testing.T) {
	task := &foremanv1alpha1.AgenticTask{}
	task.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictIncomplete
	task.Status.Result = resultRaw("MODEL-DECIDED", "CODER-GATE-FAILED", "gate failed")
	v, top, model := coderTerminalOutcome(task)
	if v != foremanv1alpha1.AgenticTaskVerdictIncomplete || top != "MODEL-DECIDED" || model != "CODER-GATE-FAILED" {
		t.Errorf("got (%s,%q,%q)", v, top, model)
	}
}

func TestCoderEscalationSteps_EmitsOnNoGo(t *testing.T) {
	ref := corev1.LocalObjectReference{Name: "coder-qwopus"}
	verifier := corev1.LocalObjectReference{Name: "gate"}
	w := &foremanv1alpha1.Workload{}
	w.Name = "wl"
	w.Spec.Repo = "defilantech/LLMKube"
	w.Spec.Issues = []int32{944}
	w.Spec.CoderAgentRef = &corev1.LocalObjectReference{Name: "coder-metal"}
	w.Spec.VerifierAgentRef = &verifier
	w.Spec.EscalationCoderAgentRef = &ref

	code := foremanv1alpha1.AgenticTask{}
	code.Name = "wl-code-944"
	code.Labels = map[string]string{labelStep: "code-944"}
	code.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
	code.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
	code.Status.Result = resultRaw("MODEL-DECIDED", "", "could not solve: fuzzy front-runs anchor")

	steps, escalated := coderEscalationSteps(w, []foremanv1alpha1.AgenticTask{code})
	if len(steps) != 2 {
		t.Fatalf("want 2 steps (code-esc+verify-esc), got %d", len(steps))
	}
	if steps[0].Name != "code-944-esc" || steps[0].AgentRef.Name != "coder-qwopus" {
		t.Errorf("code step wrong: %+v", steps[0])
	}
	if steps[0].Payload.Branch != "foreman/wl/issue-944-esc" {
		t.Errorf("branch wrong: %q", steps[0].Payload.Branch)
	}
	if !strings.Contains(steps[0].Payload.PromptPrefix, "fuzzy front-runs anchor") {
		t.Errorf("hint missing prior summary: %q", steps[0].Payload.PromptPrefix)
	}
	if steps[1].Name != "verify-944-esc" || len(steps[1].DependsOn) != 1 || steps[1].DependsOn[0] != "code-944-esc" {
		t.Errorf("verify step wrong: %+v", steps[1])
	}
	if len(escalated) != 1 || escalated[0] != 944 {
		t.Errorf("escalated wrong: %v", escalated)
	}
}

func TestCoderEscalationSteps_EmitsReviewerStepsOnEscBranch(t *testing.T) {
	newBase := func() foremanv1alpha1.AgenticTask {
		code := foremanv1alpha1.AgenticTask{}
		code.Name = "wl-code-944"
		code.Labels = map[string]string{labelStep: "code-944"}
		code.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		code.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
		code.Status.Result = resultRaw("MODEL-DECIDED", "", "bailed")
		return code
	}

	// findStep returns the emitted step by name, or a zero step if absent.
	findStep := func(steps []foremanv1alpha1.PipelineStep, name string) (foremanv1alpha1.PipelineStep, bool) {
		for _, s := range steps {
			if s.Name == name {
				return s, true
			}
		}
		return foremanv1alpha1.PipelineStep{}, false
	}

	newWorkload := func() *foremanv1alpha1.Workload {
		w := &foremanv1alpha1.Workload{}
		w.Name = "wl"
		w.Spec.Repo = "defilantech/LLMKube"
		w.Spec.Issues = []int32{944}
		w.Spec.CoderAgentRef = &corev1.LocalObjectReference{Name: "coder-metal"}
		w.Spec.VerifierAgentRef = &corev1.LocalObjectReference{Name: "gate"}
		w.Spec.EscalationCoderAgentRef = &corev1.LocalObjectReference{Name: "coder-qwopus"}
		return w
	}

	t.Run("reviewer set, openPullRequest nil -> review-esc step with openPR true", func(t *testing.T) {
		w := newWorkload()
		w.Spec.ReviewerAgentRefs = []corev1.LocalObjectReference{{Name: "reviewer-a"}}

		steps, _ := coderEscalationSteps(w, []foremanv1alpha1.AgenticTask{newBase()})
		// code-esc + verify-esc + one review-esc.
		if len(steps) != 3 {
			t.Fatalf("want 3 steps (code+verify+review esc), got %d: %+v", len(steps), steps)
		}
		rev, ok := findStep(steps, "review-944-esc-0")
		if !ok {
			t.Fatalf("review-944-esc-0 not emitted: %+v", steps)
		}
		if rev.Kind != foremanv1alpha1.AgenticTaskKindReview {
			t.Errorf("review-esc kind wrong: %q", rev.Kind)
		}
		if rev.AgentRef.Name != "reviewer-a" {
			t.Errorf("review-esc agentRef wrong: %q", rev.AgentRef.Name)
		}
		if len(rev.DependsOn) != 1 || rev.DependsOn[0] != "verify-944-esc" {
			t.Errorf("review-esc dependsOn wrong: %v", rev.DependsOn)
		}
		if rev.Payload.Branch != "foreman/wl/issue-944-esc" {
			t.Errorf("review-esc branch wrong: %q", rev.Payload.Branch)
		}
		if rev.Payload.Issue != 944 {
			t.Errorf("review-esc issue wrong: %d", rev.Payload.Issue)
		}
		if !rev.Payload.OpenPullRequest {
			t.Errorf("review-esc openPullRequest must default true when spec.openPullRequest is nil")
		}
	})

	t.Run("openPullRequest explicit false -> review-esc carries false", func(t *testing.T) {
		w := newWorkload()
		w.Spec.ReviewerAgentRefs = []corev1.LocalObjectReference{{Name: "reviewer-a"}}
		no := false
		w.Spec.OpenPullRequest = &no

		steps, _ := coderEscalationSteps(w, []foremanv1alpha1.AgenticTask{newBase()})
		rev, ok := findStep(steps, "review-944-esc-0")
		if !ok {
			t.Fatalf("review-944-esc-0 not emitted: %+v", steps)
		}
		if rev.Payload.OpenPullRequest {
			t.Errorf("review-esc openPullRequest must honor spec.openPullRequest=false")
		}
	})

	t.Run("no reviewers -> code+verify only (backward-compat)", func(t *testing.T) {
		w := newWorkload() // ReviewerAgentRefs left nil

		steps, _ := coderEscalationSteps(w, []foremanv1alpha1.AgenticTask{newBase()})
		if len(steps) != 2 {
			t.Fatalf("want 2 steps (code+verify only), got %d: %+v", len(steps), steps)
		}
		if _, ok := findStep(steps, "review-944-esc-0"); ok {
			t.Errorf("no reviewers configured must emit no review-esc step: %+v", steps)
		}
	})
}

func TestCoderEscalationSteps_SkipsStuckLoopAndExisting(t *testing.T) {
	ref := corev1.LocalObjectReference{Name: "coder-qwopus"}
	w := &foremanv1alpha1.Workload{}
	w.Name = "wl"
	w.Spec.Repo = "defilantech/LLMKube"
	w.Spec.Issues = []int32{921, 944}
	w.Spec.VerifierAgentRef = &corev1.LocalObjectReference{Name: "gate"}
	w.Spec.EscalationCoderAgentRef = &ref

	c921 := foremanv1alpha1.AgenticTask{}
	c921.Name = "wl-code-921"
	c921.Labels = map[string]string{labelStep: "code-921"}
	c921.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
	c921.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictIncomplete
	c921.Status.Result = resultRaw("MODEL-DECIDED", "", "ran out of turns")

	c944 := foremanv1alpha1.AgenticTask{}
	c944.Name = "wl-code-944"
	c944.Labels = map[string]string{labelStep: "code-944"}
	c944.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
	c944.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
	c944.Status.Result = resultRaw("MODEL-DECIDED", "", "bailed")
	esc944 := foremanv1alpha1.AgenticTask{}
	esc944.Name = "wl-code-944-esc"
	esc944.Labels = map[string]string{labelStep: "code-944-esc"}

	steps, escalated := coderEscalationSteps(w,
		[]foremanv1alpha1.AgenticTask{c921, c944, esc944})
	if len(steps) != 0 {
		t.Errorf("want no steps (921 not eligible, 944 already escalated), got %d: %+v", len(steps), steps)
	}
	if len(escalated) != 0 {
		t.Errorf("want none escalated, got %v", escalated)
	}
}

func TestCoderEscalationSteps_OffWhenUnset(t *testing.T) {
	w := &foremanv1alpha1.Workload{}
	w.Name = "wl"
	w.Spec.Issues = []int32{944}
	w.Spec.VerifierAgentRef = &corev1.LocalObjectReference{Name: "gate"}
	c := foremanv1alpha1.AgenticTask{}
	c.Name = "wl-code-944"
	c.Labels = map[string]string{labelStep: "code-944"}
	c.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
	c.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
	c.Status.Result = resultRaw("MODEL-DECIDED", "", "bailed")
	steps, _ := coderEscalationSteps(w, []foremanv1alpha1.AgenticTask{c})
	if len(steps) != 0 {
		t.Errorf("feature must be off when EscalationCoderAgentRef is nil, got %d steps", len(steps))
	}
}
