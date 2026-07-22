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

// Unit tests for operator-intent injection (#1201): Workload.spec.intent
// must reach the coder as AgenticTaskPayload.PromptPrefix at every coder
// task construction site (issue batch, review-driven iteration, coder
// escalation), instead of being a required-but-write-only field.

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func intentWorkload(intent string) *foremanv1alpha1.Workload {
	w := &foremanv1alpha1.Workload{}
	w.Name = "wl-intent"
	w.Spec.Repo = "defilantech/LLMKube"
	w.Spec.Issues = []int32{42}
	w.Spec.Intent = intent
	w.Spec.CoderAgentRef = &corev1.LocalObjectReference{Name: "coder"}
	return w
}

func TestIntentPromptPrefix(t *testing.T) {
	t.Run("renders the intent under the operator-scope header", func(t *testing.T) {
		got := intentPromptPrefix(intentWorkload("Change only these 3 files."))
		if !strings.Contains(got, "Operator intent for this run") {
			t.Fatalf("missing header: %q", got)
		}
		if !strings.Contains(got, "Change only these 3 files.") {
			t.Fatalf("missing intent body: %q", got)
		}
		if !strings.Contains(got, "the intent wins") {
			t.Fatalf("missing precedence instruction: %q", got)
		}
	})

	t.Run("empty and whitespace intents render nothing", func(t *testing.T) {
		if got := intentPromptPrefix(intentWorkload("")); got != "" {
			t.Fatalf("empty intent produced %q", got)
		}
		if got := intentPromptPrefix(intentWorkload("   \n ")); got != "" {
			t.Fatalf("whitespace intent produced %q", got)
		}
	})
}

func TestJoinPromptSections(t *testing.T) {
	if got := joinPromptSections("a", "", "  ", "b"); got != "a\n\nb" {
		t.Fatalf("got %q, want %q", got, "a\n\nb")
	}
	if got := joinPromptSections("", "  "); got != "" {
		t.Fatalf("all-empty sections produced %q", got)
	}
}

func TestSynthesizeIssueBatch_CarriesIntent(t *testing.T) {
	w := intentWorkload("SCOPED SLICE: only pin the three images.")
	w.Spec.VerifierAgentRef = &corev1.LocalObjectReference{Name: "gate"}

	steps := synthesizeIssueBatch(w)

	var code *foremanv1alpha1.PipelineStep
	for i := range steps {
		if steps[i].Kind == foremanv1alpha1.AgenticTaskKindIssueFix {
			code = &steps[i]
		}
	}
	if code == nil {
		t.Fatal("no issue-fix step synthesized")
	}
	if !strings.Contains(code.Payload.PromptPrefix, "SCOPED SLICE: only pin the three images.") {
		t.Fatalf("coder step PromptPrefix missing the intent: %q", code.Payload.PromptPrefix)
	}

	// The verify step runs the deterministic gate, not a prompt; it must
	// not grow a PromptPrefix.
	for _, s := range steps {
		if s.Kind == foremanv1alpha1.AgenticTaskKindVerify && s.Payload.PromptPrefix != "" {
			t.Fatalf("verify step unexpectedly carries a PromptPrefix: %q", s.Payload.PromptPrefix)
		}
	}
}

func TestReviewIterationSteps_CarriesIntent(t *testing.T) {
	w := iterationWorkload([]int32{641}, 1, nil)
	w.Spec.Intent = "Do not touch the chart."
	children := []foremanv1alpha1.AgenticTask{
		child("code-641", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictGo),
		child("verify-641", foremanv1alpha1.AgenticTaskPhaseSucceeded, foremanv1alpha1.AgenticTaskVerdictGatePass),
		noGoChild("review-641-0", "needs work", "[]"),
	}

	steps, _ := reviewIterationSteps(w, children)

	var found bool
	for _, s := range steps {
		if s.Kind != foremanv1alpha1.AgenticTaskKindIssueFix {
			continue
		}
		found = true
		if !strings.Contains(s.Payload.PromptPrefix, "Do not touch the chart.") {
			t.Fatalf("iteration coder step missing intent in PromptPrefix: %q", s.Payload.PromptPrefix)
		}
		// The review feedback must still arrive via Prompt: intent must not
		// displace it.
		if s.Payload.Prompt == "" {
			t.Fatal("iteration coder step lost its review-feedback Prompt")
		}
	}
	if !found {
		t.Fatal("no iteration issue-fix step synthesized")
	}
}

func TestCoderEscalationSteps_ComposesIntentWithHint(t *testing.T) {
	w := intentWorkload("Only fix the parser.")
	w.Spec.Issues = []int32{944}
	w.Spec.VerifierAgentRef = &corev1.LocalObjectReference{Name: "gate"}
	w.Spec.EscalationCoderAgentRef = &corev1.LocalObjectReference{Name: "coder-esc"}

	code := foremanv1alpha1.AgenticTask{}
	code.Name = "wl-intent-code-944"
	code.Labels = map[string]string{labelStep: "code-944"}
	code.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
	code.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
	code.Status.Result = resultRaw("MODEL-DECIDED", "", "could not solve: anchor drift", "")

	steps, _, _ := coderEscalationSteps(w, []foremanv1alpha1.AgenticTask{code})
	if len(steps) == 0 {
		t.Fatal("no escalation steps synthesized")
	}
	prefix := steps[0].Payload.PromptPrefix
	if !strings.Contains(prefix, "Only fix the parser.") {
		t.Fatalf("escalation PromptPrefix missing the intent: %q", prefix)
	}
	if !strings.Contains(prefix, "anchor drift") {
		t.Fatalf("escalation PromptPrefix lost the prior-attempt hint: %q", prefix)
	}
	if strings.Index(prefix, "Only fix the parser.") > strings.Index(prefix, "anchor drift") {
		t.Fatalf("intent should precede the escalation hint: %q", prefix)
	}
}
