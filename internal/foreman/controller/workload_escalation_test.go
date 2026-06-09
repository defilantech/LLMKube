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
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// child builds a minimal AgenticTask carrying only what escalationSteps
// reads: the step label, phase, and verdict.
func child(step string, phase foremanv1alpha1.AgenticTaskPhase, verdict foremanv1alpha1.AgenticTaskVerdict) foremanv1alpha1.AgenticTask {
	return foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "wl-" + step,
			Labels: map[string]string{labelWorkload: "wl", labelStep: step},
		},
		Status: foremanv1alpha1.AgenticTaskStatus{Phase: phase, Verdict: verdict},
	}
}

func escalationWorkload(issues []int32, baseRefs, escRefs int) *foremanv1alpha1.Workload {
	spec := foremanv1alpha1.WorkloadSpec{
		Intent: "escalation unit test",
		Repo:   "defilantech/LLMKube",
		Issues: issues,
	}
	for i := 0; i < baseRefs; i++ {
		spec.ReviewerAgentRefs = append(spec.ReviewerAgentRefs, corev1.LocalObjectReference{Name: "base-reviewer"})
	}
	for i := 0; i < escRefs; i++ {
		spec.EscalationReviewerAgentRefs = append(spec.EscalationReviewerAgentRefs, corev1.LocalObjectReference{Name: "big-reviewer"})
	}
	w := &foremanv1alpha1.Workload{Spec: spec}
	w.Name = "wl"
	return w
}

func TestEscalationSteps(t *testing.T) {
	succeeded := foremanv1alpha1.AgenticTaskPhaseSucceeded
	running := foremanv1alpha1.AgenticTaskPhaseRunning
	failed := foremanv1alpha1.AgenticTaskPhaseFailed
	noGo := foremanv1alpha1.AgenticTaskVerdictNoGo
	gateFail := foremanv1alpha1.AgenticTaskVerdictGateFail
	incomplete := foremanv1alpha1.AgenticTaskVerdictIncomplete
	gatePass := foremanv1alpha1.AgenticTaskVerdictGatePass
	goVerdict := foremanv1alpha1.AgenticTaskVerdictGo

	cases := []struct {
		name          string
		w             *foremanv1alpha1.Workload
		children      []foremanv1alpha1.AgenticTask
		wantSteps     []string // expected step names, in order
		wantEscalated []int32
	}{
		{
			name: "single base reviewer NO-GO triggers one escalation per ref",
			w:    escalationWorkload([]int32{641}, 1, 2),
			children: []foremanv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				child("review-641-0", succeeded, noGo),
			},
			wantSteps:     []string{"escalate-641-0", "escalate-641-1"},
			wantEscalated: []int32{641},
		},
		{
			name: "all base reviewers GO emits nothing",
			w:    escalationWorkload([]int32{641}, 1, 1),
			children: []foremanv1alpha1.AgenticTask{
				child("review-641-0", succeeded, goVerdict),
			},
		},
		{
			name: "waits for every base reviewer to be terminal",
			w:    escalationWorkload([]int32{641}, 2, 1),
			children: []foremanv1alpha1.AgenticTask{
				child("review-641-0", succeeded, noGo),
				child("review-641-1", running, ""),
			},
		},
		{
			name: "emits once both base reviewers are terminal with one NO-GO",
			w:    escalationWorkload([]int32{641}, 2, 1),
			children: []foremanv1alpha1.AgenticTask{
				child("review-641-0", succeeded, noGo),
				child("review-641-1", succeeded, goVerdict),
			},
			wantSteps:     []string{"escalate-641-0"},
			wantEscalated: []int32{641},
		},
		{
			name: "existing escalation child blocks re-emission (idempotency)",
			w:    escalationWorkload([]int32{641}, 1, 1),
			children: []foremanv1alpha1.AgenticTask{
				child("review-641-0", succeeded, noGo),
				child("escalate-641-0", running, ""),
			},
		},
		{
			name: "partial create failure repaired: only the missing escalation step is re-emitted",
			w:    escalationWorkload([]int32{641}, 1, 2),
			children: []foremanv1alpha1.AgenticTask{
				child("review-641-0", succeeded, noGo),
				child("escalate-641-0", running, ""),
			},
			wantSteps:     []string{"escalate-641-1"},
			wantEscalated: []int32{641},
		},
		{
			name: "cascade INCOMPLETE after GATE-FAIL does not escalate",
			w:    escalationWorkload([]int32{641}, 1, 1),
			children: []foremanv1alpha1.AgenticTask{
				child("verify-641", succeeded, gateFail),
				child("review-641-0", succeeded, incomplete),
			},
		},
		{
			name: "base reviewer Phase=Failed without NO-GO does not escalate",
			w:    escalationWorkload([]int32{641}, 1, 1),
			children: []foremanv1alpha1.AgenticTask{
				child("review-641-0", failed, ""),
			},
		},
		{
			name: "no escalation refs is a no-op",
			w:    escalationWorkload([]int32{641}, 1, 0),
			children: []foremanv1alpha1.AgenticTask{
				child("review-641-0", succeeded, noGo),
			},
		},
		{
			name: "no base refs is a no-op (validated separately by the controller)",
			w:    escalationWorkload([]int32{641}, 0, 1),
		},
		{
			name: "per-issue isolation: only the NO-GO issue escalates",
			w:    escalationWorkload([]int32{641, 642}, 1, 1),
			children: []foremanv1alpha1.AgenticTask{
				child("review-641-0", succeeded, noGo),
				child("review-642-0", succeeded, goVerdict),
			},
			wantSteps:     []string{"escalate-641-0"},
			wantEscalated: []int32{641},
		},
		{
			name: "two issues escalate simultaneously in spec.Issues order",
			w:    escalationWorkload([]int32{641, 642}, 1, 1),
			children: []foremanv1alpha1.AgenticTask{
				child("review-642-0", succeeded, noGo),
				child("review-641-0", succeeded, noGo),
			},
			wantSteps:     []string{"escalate-641-0", "escalate-642-0"},
			wantEscalated: []int32{641, 642},
		},
		{
			name: "issue number prefixes do not cross-match (64 vs 641)",
			w:    escalationWorkload([]int32{64}, 1, 1),
			children: []foremanv1alpha1.AgenticTask{
				child("review-641-0", succeeded, noGo),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps, escalated := escalationSteps(tc.w, tc.children)

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
			if len(escalated) != len(tc.wantEscalated) {
				t.Fatalf("escalated = %v, want %v", escalated, tc.wantEscalated)
			}
			for i := range tc.wantEscalated {
				if escalated[i] != tc.wantEscalated[i] {
					t.Fatalf("escalated = %v, want %v", escalated, tc.wantEscalated)
				}
			}

			// Every emitted step must be a review step carrying the
			// issue payload and the workload-scoped branch. The
			// expected issue number is parsed from the step name
			// (escalate-<N>-<j>) so multi-issue cases stay honest.
			for _, s := range steps {
				if s.Kind != foremanv1alpha1.AgenticTaskKindReview {
					t.Errorf("step %s kind = %s, want review", s.Name, s.Kind)
				}
				if s.Payload.Repo != "defilantech/LLMKube" {
					t.Errorf("step %s repo = %q", s.Name, s.Payload.Repo)
				}
				var issue, idx int32
				if _, err := fmt.Sscanf(s.Name, "escalate-%d-%d", &issue, &idx); err != nil {
					t.Fatalf("step name %q does not match escalate-<N>-<j>: %v", s.Name, err)
				}
				wantBranch := fmt.Sprintf("foreman/wl/issue-%d", issue)
				if s.Payload.Issue != issue || s.Payload.Branch != wantBranch {
					t.Errorf("step %s payload = issue %d branch %q, want %d %q",
						s.Name, s.Payload.Issue, s.Payload.Branch, issue, wantBranch)
				}
				if s.AgentRef.Name != "big-reviewer" {
					t.Errorf("step %s agentRef = %q, want big-reviewer", s.Name, s.AgentRef.Name)
				}
			}
		})
	}
}

// The explicit-Pipeline guard returns before any client call, so a
// zero-value reconciler suffices: user-authored pipeline step names
// (here review-9-0) must never trigger escalation.
func TestEmitEscalationsSkipsPipelineMode(t *testing.T) {
	r := &WorkloadReconciler{}
	w := escalationWorkload([]int32{9}, 1, 1)
	w.Spec.Pipeline = []foremanv1alpha1.PipelineStep{{
		Name:     "review-9-0",
		Kind:     foremanv1alpha1.AgenticTaskKindReview,
		AgentRef: corev1.LocalObjectReference{Name: "user-authored"},
	}}
	children := []foremanv1alpha1.AgenticTask{
		child("review-9-0", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictNoGo),
	}

	got, err := r.emitEscalations(context.Background(), w, children)
	if err != nil {
		t.Fatalf("emitEscalations: %v", err)
	}
	if len(got) != 1 || got[0].Name != "wl-review-9-0" {
		t.Fatalf("children mutated: %v", got)
	}
}
