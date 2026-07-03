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
	"k8s.io/utils/ptr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// iterationWorkload builds the issue-batch spec reviewIterationSteps
// consumes: coder + verifier refs plus `reviewers` base reviewer refs.
func iterationWorkload(issues []int32, reviewers int, maxIter *int32) *foremanv1alpha1.Workload {
	spec := foremanv1alpha1.WorkloadSpec{
		Intent:              "iteration unit test",
		Repo:                "defilantech/LLMKube",
		Issues:              issues,
		CoderAgentRef:       &corev1.LocalObjectReference{Name: "coder"},
		VerifierAgentRef:    &corev1.LocalObjectReference{Name: "gate"},
		MaxReviewIterations: maxIter,
	}
	for i := 0; i < reviewers; i++ {
		spec.ReviewerAgentRefs = append(spec.ReviewerAgentRefs, corev1.LocalObjectReference{Name: "reviewer"})
	}
	w := &foremanv1alpha1.Workload{Spec: spec}
	w.Name = "wl"
	return w
}

// noGoChild is a terminal NO-GO review child carrying a structured
// review result so the feedback-prompt path is exercised end to end.
func noGoChild(step, summary, findingsJSON string) foremanv1alpha1.AgenticTask {
	c := child(step, foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictNoGo)
	raw := `{"schemaVersion":"foreman.v1","kind":"review","verdict":"NO-GO","summary":"` + summary + `",` +
		`"extra":{"outcome":"MODEL-DECIDED","modelExtra":{"findings":` + findingsJSON + `}}}`
	c.Status.Result = &runtime.RawExtension{Raw: []byte(raw)}
	return c
}

func TestReviewIterationSteps(t *testing.T) {
	succeeded := foremanv1alpha1.AgenticTaskPhaseSucceeded
	running := foremanv1alpha1.AgenticTaskPhaseRunning
	noGo := foremanv1alpha1.AgenticTaskVerdictNoGo
	gateFail := foremanv1alpha1.AgenticTaskVerdictGateFail
	incomplete := foremanv1alpha1.AgenticTaskVerdictIncomplete
	gatePass := foremanv1alpha1.AgenticTaskVerdictGatePass
	goVerdict := foremanv1alpha1.AgenticTaskVerdictGo

	baseRound := func(reviewVerdict foremanv1alpha1.AgenticTaskVerdict) []foremanv1alpha1.AgenticTask {
		return []foremanv1alpha1.AgenticTask{
			child("code-641", succeeded, goVerdict),
			child("verify-641", succeeded, gatePass),
			child("review-641-0", succeeded, reviewVerdict),
		}
	}

	cases := []struct {
		name         string
		w            *foremanv1alpha1.Workload
		children     []foremanv1alpha1.AgenticTask
		wantSteps    []string // expected step names, in order
		wantIterated []int32
	}{
		{
			name:         "reviewer NO-GO triggers the full r1 triple",
			w:            iterationWorkload([]int32{641}, 1, nil),
			children:     baseRound(noGo),
			wantSteps:    []string{"code-641-r1", "verify-641-r1", "review-641-0-r1"},
			wantIterated: []int32{641},
		},
		{
			name:     "reviewer GO converges: no iteration",
			w:        iterationWorkload([]int32{641}, 1, nil),
			children: baseRound(goVerdict),
		},
		{
			name: "waits for every reviewer in the round to be terminal",
			w:    iterationWorkload([]int32{641}, 2, nil),
			children: []foremanv1alpha1.AgenticTask{
				child("review-641-0", succeeded, noGo),
				child("review-641-1", running, ""),
			},
		},
		{
			name: "cascade INCOMPLETE after GATE-FAIL does not iterate",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []foremanv1alpha1.AgenticTask{
				child("verify-641", succeeded, gateFail),
				child("review-641-0", succeeded, incomplete),
			},
		},
		{
			name: "existing r1 children block re-emission (idempotency)",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: append(baseRound(noGo),
				child("code-641-r1", running, ""),
				child("verify-641-r1", "", ""),
				child("review-641-0-r1", "", ""),
			),
		},
		{
			name: "partial create failure repaired: only the missing r1 steps re-emit",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: append(baseRound(noGo),
				child("code-641-r1", running, ""),
			),
			wantSteps:    []string{"verify-641-r1", "review-641-0-r1"},
			wantIterated: []int32{641},
		},
		{
			name: "r1 NO-GO with budget left chains r2",
			w:    iterationWorkload([]int32{641}, 1, ptr.To(int32(2))),
			children: append(baseRound(noGo),
				child("code-641-r1", succeeded, goVerdict),
				child("verify-641-r1", succeeded, gatePass),
				child("review-641-0-r1", succeeded, noGo),
			),
			wantSteps:    []string{"code-641-r2", "verify-641-r2", "review-641-0-r2"},
			wantIterated: []int32{641},
		},
		{
			name: "r1 NO-GO with budget exhausted emits nothing (fails as today)",
			w:    iterationWorkload([]int32{641}, 1, nil), // nil -> 1 iteration
			children: append(baseRound(noGo),
				child("code-641-r1", succeeded, goVerdict),
				child("verify-641-r1", succeeded, gatePass),
				child("review-641-0-r1", succeeded, noGo),
			),
		},
		{
			name:     "explicit 0 disables iteration",
			w:        iterationWorkload([]int32{641}, 1, ptr.To(int32(0))),
			children: baseRound(noGo),
		},
		{
			name: "per-issue isolation: only the NO-GO issue iterates",
			w:    iterationWorkload([]int32{641, 642}, 1, nil),
			children: []foremanv1alpha1.AgenticTask{
				child("review-641-0", succeeded, noGo),
				child("review-642-0", succeeded, goVerdict),
			},
			wantSteps:    []string{"code-641-r1", "verify-641-r1", "review-641-0-r1"},
			wantIterated: []int32{641},
		},
		{
			name: "issue number prefixes do not cross-match (64 vs 641)",
			w:    iterationWorkload([]int32{64}, 1, nil),
			children: []foremanv1alpha1.AgenticTask{
				child("review-641-0", succeeded, noGo),
			},
		},
		{
			name: "no reviewers configured is inert",
			w:    iterationWorkload([]int32{641}, 0, nil),
			children: []foremanv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps, iterated := reviewIterationSteps(tc.w, tc.children)

			var gotNames []string
			for _, s := range steps {
				gotNames = append(gotNames, s.Name)
			}
			if len(gotNames) != len(tc.wantSteps) {
				t.Fatalf("steps = %v, want %v", gotNames, tc.wantSteps)
			}
			for i := range tc.wantSteps {
				if gotNames[i] != tc.wantSteps[i] {
					t.Fatalf("steps = %v, want %v", gotNames, tc.wantSteps)
				}
			}
			if len(iterated) != len(tc.wantIterated) {
				t.Fatalf("iterated = %v, want %v", iterated, tc.wantIterated)
			}
			for i := range tc.wantIterated {
				if iterated[i] != tc.wantIterated[i] {
					t.Fatalf("iterated = %v, want %v", iterated, tc.wantIterated)
				}
			}

			// Every emitted coder step must re-target the same branch
			// with allowOverwrite + a non-empty feedback prompt; verify
			// and review steps chain behind it within the iteration.
			for _, s := range steps {
				if s.Payload.Branch == "" || !strings.HasPrefix(s.Payload.Branch, "foreman/wl/issue-") {
					t.Errorf("step %s branch = %q, want the original issue branch", s.Name, s.Payload.Branch)
				}
				switch s.Kind {
				case foremanv1alpha1.AgenticTaskKindIssueFix:
					if !s.Payload.AllowOverwrite {
						t.Errorf("step %s must set allowOverwrite to amend its own branch", s.Name)
					}
					if s.Payload.ReviseFromBranch != s.Payload.Branch {
						t.Errorf("step %s reviseFromBranch = %q, want %q so the executor restores the prior attempt (#951)",
							s.Name, s.Payload.ReviseFromBranch, s.Payload.Branch)
					}
					if !strings.Contains(s.Payload.Prompt, "NO-GO") {
						t.Errorf("step %s prompt must carry the review feedback, got %q", s.Name, s.Payload.Prompt)
					}
					if len(s.DependsOn) != 0 {
						t.Errorf("step %s dependsOn = %v, want none (prior round is terminal)", s.Name, s.DependsOn)
					}
				case foremanv1alpha1.AgenticTaskKindVerify, foremanv1alpha1.AgenticTaskKindReview:
					if len(s.DependsOn) != 1 {
						t.Errorf("step %s dependsOn = %v, want exactly one upstream", s.Name, s.DependsOn)
					}
				}
			}
		})
	}
}

// TestReviewIterationCoderRef covers the revision profile pairing
// (#951): iteration coder steps reference spec.revisionCoderAgentRef
// when set (a revision amends restored work and wants a revision-tuned
// profile) and fall back to spec.coderAgentRef otherwise. Verify and
// review steps keep their own refs either way.
func TestReviewIterationCoderRef(t *testing.T) {
	noGoRound := []foremanv1alpha1.AgenticTask{
		child("code-641", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictGo),
		child("verify-641", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictGatePass),
		child("review-641-0", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictNoGo),
	}

	cases := []struct {
		name          string
		revisionRef   *corev1.LocalObjectReference
		wantCoderName string
	}{
		{"revisionCoderAgentRef set selects the revision coder", &corev1.LocalObjectReference{Name: "revision-coder"}, "revision-coder"},
		{"unset falls back to coderAgentRef", nil, "coder"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := iterationWorkload([]int32{641}, 1, nil)
			w.Spec.RevisionCoderAgentRef = tc.revisionRef

			steps, _ := reviewIterationSteps(w, noGoRound)
			refs := map[string]string{}
			for _, s := range steps {
				refs[s.Name] = s.AgentRef.Name
			}
			if got := refs["code-641-r1"]; got != tc.wantCoderName {
				t.Errorf("code-641-r1 agentRef = %q, want %q", got, tc.wantCoderName)
			}
			if got := refs["verify-641-r1"]; got != "gate" {
				t.Errorf("verify-641-r1 agentRef = %q, want %q", got, "gate")
			}
			if got := refs["review-641-0-r1"]; got != "reviewer" {
				t.Errorf("review-641-0-r1 agentRef = %q, want %q", got, "reviewer")
			}
		})
	}
}

func TestReviewFeedbackPrompt(t *testing.T) {
	structured := noGoChild("review-641-0", "scope creep beyond the issue ask",
		`[{"severity":"blocker","area":"scope","message":"reduces ACCESS_TOKEN_EXPIRE_MINUTES from 10080 to 30, unrelated to the issue","file":"config/auth.py","line":12,"suggestion":"revert the unrelated change"}]`)
	prompt := reviewFeedbackPrompt([]*foremanv1alpha1.AgenticTask{&structured})
	for _, want := range []string{
		"rejected the previous attempt",
		"NO-GO",
		// The executor's revise-from-branch restore (#951) means the
		// workspace really does start from the prior attempt; the prompt
		// must direct a delta, not a rebuild.
		"restored from this task's branch",
		"Do not rebuild the fix from scratch",
		"Amend the existing work",
		"scope creep beyond the issue ask",
		"[blocker/scope] reduces ACCESS_TOKEN_EXPIRE_MINUTES from 10080 to 30",
		"config/auth.py:12",
		"revert the unrelated change",
		"Address this feedback",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}

	// Legacy map-shaped findings (the boolean + *_details shape from the
	// issue report) fail the strict schema and must fall back to raw JSON
	// rather than vanishing.
	legacy := noGoChild("review-641-0", "missing tests",
		`{"missing_tests":true,"missing_tests_details":"no unit test covers the new branch"}`)
	prompt = reviewFeedbackPrompt([]*foremanv1alpha1.AgenticTask{&legacy})
	if !strings.Contains(prompt, "no unit test covers the new branch") {
		t.Errorf("legacy findings must surface via the raw-JSON fallback:\n%s", prompt)
	}

	// A result-less NO-GO still yields a usable prompt.
	bare := child("review-641-1", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictNoGo)
	prompt = reviewFeedbackPrompt([]*foremanv1alpha1.AgenticTask{&bare})
	if !strings.Contains(prompt, "Reviewer review-641-1") {
		t.Errorf("result-less reviewer must still be named:\n%s", prompt)
	}
}

func TestActiveChildren(t *testing.T) {
	succeeded := foremanv1alpha1.AgenticTaskPhaseSucceeded
	w := iterationWorkload([]int32{641}, 1, nil)

	children := []foremanv1alpha1.AgenticTask{
		child("code-641", succeeded, foremanv1alpha1.AgenticTaskVerdictGo),
		child("verify-641", succeeded, foremanv1alpha1.AgenticTaskVerdictGatePass),
		child("review-641-0", succeeded, foremanv1alpha1.AgenticTaskVerdictNoGo),
		child("code-641-r1", succeeded, foremanv1alpha1.AgenticTaskVerdictGo),
		child("verify-641-r1", succeeded, foremanv1alpha1.AgenticTaskVerdictGatePass),
		child("review-641-0-r1", succeeded, foremanv1alpha1.AgenticTaskVerdictGo),
		child("escalate-641-0", succeeded, foremanv1alpha1.AgenticTaskVerdictGo),
	}

	active := activeChildren(w, children)
	names := make([]string, 0, len(active))
	for i := range active {
		names = append(names, active[i].Labels[labelStep])
	}
	want := []string{"code-641-r1", "verify-641-r1", "review-641-0-r1", "escalate-641-0"}
	if len(names) != len(want) {
		t.Fatalf("active = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("active = %v, want %v", names, want)
		}
	}

	// Without a later iteration nothing is filtered.
	base := children[:3]
	if got := activeChildren(w, base); len(got) != 3 {
		t.Fatalf("no-iteration input must pass through, got %d children", len(got))
	}

	// Explicit Pipeline mode passes through untouched even when names
	// resemble the synthesized scheme.
	pw := iterationWorkload([]int32{641}, 1, nil)
	pw.Spec.Pipeline = []foremanv1alpha1.PipelineStep{{Name: "code-641"}}
	if got := activeChildren(pw, children); len(got) != len(children) {
		t.Fatalf("pipeline mode must not filter, got %d of %d", len(got), len(children))
	}
}

// TestActiveChildren_EscalationSupersedesBase proves the #963 rule: once
// a code-<n>-esc task exists, the failed base attempt (code/verify/review
// -<n>) is dropped from the active slice so its cascade-failure does not
// pin the rollup at Failed, while the -esc attempt and unescalated issues
// are untouched.
func TestActiveChildren_EscalationSupersedesBase(t *testing.T) {
	succeeded := foremanv1alpha1.AgenticTaskPhaseSucceeded
	failed := foremanv1alpha1.AgenticTaskPhaseFailed
	noGo := foremanv1alpha1.AgenticTaskVerdictNoGo
	gatePass := foremanv1alpha1.AgenticTaskVerdictGatePass
	goVerdict := foremanv1alpha1.AgenticTaskVerdictGo

	w := iterationWorkload([]int32{944, 921}, 1, nil)

	children := []foremanv1alpha1.AgenticTask{
		// Issue 944: base coder NO-GO, verify/review cascade-failed, then
		// escalated. The base triple must be superseded.
		child("code-944", succeeded, noGo),
		child("verify-944", failed, ""),
		child("review-944-0", failed, ""),
		child("code-944-esc", succeeded, goVerdict),
		child("verify-944-esc", succeeded, gatePass),
		child("review-944-esc-0", succeeded, goVerdict),
		// Issue 921: not escalated, must pass through untouched.
		child("code-921", succeeded, goVerdict),
		child("verify-921", succeeded, gatePass),
		child("review-921-0", succeeded, goVerdict),
	}

	active := activeChildren(w, children)
	got := make(map[string]bool, len(active))
	for i := range active {
		got[active[i].Labels[labelStep]] = true
	}

	dropped := []string{"code-944", "verify-944", "review-944-0"}
	for _, name := range dropped {
		if got[name] {
			t.Errorf("base step %q must be superseded by the escalation, but it is still active", name)
		}
	}
	kept := []string{
		"code-944-esc", "verify-944-esc", "review-944-esc-0",
		"code-921", "verify-921", "review-921-0",
	}
	for _, name := range kept {
		if !got[name] {
			t.Errorf("step %q must remain active, but it was filtered", name)
		}
	}
	if len(active) != len(kept) {
		t.Fatalf("active = %d steps, want %d (%v)", len(active), len(kept), kept)
	}
}
