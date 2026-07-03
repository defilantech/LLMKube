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
	"fmt"
	"strings"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// coderResultEnvelope is the subset of an AgenticTask's status.result we
// need to classify a coder failure. The executor writes the machine
// outcome at extra.outcome (top-level; "MODEL-DECIDED" for a model-
// terminated run) and, for a coder-gate failure, at extra.modelExtra.
// outcome ("CODER-GATE-FAILED"). Summary is the model's own one-liner.
type coderResultEnvelope struct {
	Summary string `json:"summary"`
	Extra   struct {
		Outcome    string `json:"outcome"`
		ModelExtra struct {
			Outcome string `json:"outcome"`
		} `json:"modelExtra"`
	} `json:"extra"`
}

// coderTerminalOutcome extracts the fields that decide escalation from a
// terminal coder task: the typed verdict plus the two outcome strings in
// the result envelope. A missing/unparseable result yields empty
// outcome strings (treated as non-escalating).
func coderTerminalOutcome(task *foremanv1alpha1.AgenticTask) (
	verdict foremanv1alpha1.AgenticTaskVerdict, topOutcome, modelOutcome string,
) {
	verdict = task.Status.Verdict
	if task.Status.Result == nil || len(task.Status.Result.Raw) == 0 {
		return verdict, "", ""
	}
	var env coderResultEnvelope
	if err := json.Unmarshal(task.Status.Result.Raw, &env); err != nil {
		return verdict, "", ""
	}
	return verdict, env.Extra.Outcome, env.Extra.ModelExtra.Outcome
}

// coderSummary returns the model's own terminal summary for the hint, or
// "" if unavailable.
func coderSummary(task *foremanv1alpha1.AgenticTask) string {
	if task.Status.Result == nil || len(task.Status.Result.Raw) == 0 {
		return ""
	}
	var env coderResultEnvelope
	if err := json.Unmarshal(task.Status.Result.Raw, &env); err != nil {
		return ""
	}
	return env.Summary
}

// shouldEscalateCoder is the escalation trigger: escalate only on
// capability failures. A model-decided NO-GO (the honest "I could not
// solve this" bail) or a coder-gate failure (wrote code, could not pass
// the gate) escalate; a model-decided INCOMPLETE (gave up / ran out of
// turns), a harness STUCK-LOOP-DETECTED, a trivial NO-CHANGES NO-GO, an
// ERROR, or a GO do not.
func shouldEscalateCoder(
	verdict foremanv1alpha1.AgenticTaskVerdict, topOutcome, modelOutcome string,
) bool {
	if verdict == foremanv1alpha1.AgenticTaskVerdictNoGo && topOutcome == "MODEL-DECIDED" {
		return true
	}
	if modelOutcome == "CODER-GATE-FAILED" {
		return true
	}
	return false
}

// escalationHintPrompt renders the PromptPrefix given to the escalation
// coder. It carries the prior model's summary as a hint, framed so the
// larger model does not treat the failed attempt's conclusions as
// authoritative.
func escalationHintPrompt(priorSummary string) string {
	var b strings.Builder
	b.WriteString("A previous attempt to fix this issue with a smaller model did not succeed.")
	if strings.TrimSpace(priorSummary) != "" {
		b.WriteString(" Its own summary was:\n\n\"")
		b.WriteString(strings.TrimSpace(priorSummary))
		b.WriteString("\"\n\n")
	} else {
		b.WriteString("\n\n")
	}
	b.WriteString("Approach the issue fresh. Do not assume the previous attempt's ")
	b.WriteString("conclusions are correct; use the summary only as a hint about ")
	b.WriteString("what proved difficult.")
	return b.String()
}

// coderEscalationSteps decides which code-<N>-esc / verify-<N>-esc steps
// to emit NOW (mirrors escalationSteps for reviewers, #546). For each
// issue N the trigger is: the base code-<N> task is terminal (Phase
// Succeeded or Failed) AND shouldEscalateCoder is true for it. Steps
// whose code-<N>-esc child already exists are skipped, so a partial
// create failure is repaired on the next reconcile. The escalation
// tasks themselves (step suffix "-esc") are never scanned as base tasks,
// so the tier is exactly one level deep.
//
// Pure: no API calls, no status writes. The caller owns MaxTasks
// accounting, sovereignty filtering, and creation. Issue-batch mode
// only (the caller guards against explicit Pipeline mode).
func coderEscalationSteps(
	w *foremanv1alpha1.Workload, children []foremanv1alpha1.AgenticTask,
) (steps []foremanv1alpha1.PipelineStep, escalated []int32) {
	if w.Spec.EscalationCoderAgentRef == nil ||
		w.Spec.VerifierAgentRef == nil ||
		len(w.Spec.Issues) == 0 {
		return nil, nil
	}

	// Index base code tasks and existing -esc tasks by step label.
	baseCode := map[string]*foremanv1alpha1.AgenticTask{}
	existingEsc := map[string]struct{}{}
	for i := range children {
		step := children[i].Labels[labelStep]
		switch {
		case strings.HasSuffix(step, "-esc"):
			existingEsc[step] = struct{}{}
		case strings.HasPrefix(step, "code-"):
			baseCode[step] = &children[i]
		}
	}

	for _, n := range w.Spec.Issues {
		codeStep := fmt.Sprintf("code-%d", n)
		escCodeStep := fmt.Sprintf("code-%d-esc", n)
		escVerifyStep := fmt.Sprintf("verify-%d-esc", n)

		base, ok := baseCode[codeStep]
		if !ok {
			continue
		}
		phase := base.Status.Phase
		if phase != foremanv1alpha1.AgenticTaskPhaseSucceeded &&
			phase != foremanv1alpha1.AgenticTaskPhaseFailed {
			continue // not terminal yet
		}
		verdict, topOutcome, modelOutcome := coderTerminalOutcome(base)
		if !shouldEscalateCoder(verdict, topOutcome, modelOutcome) {
			continue
		}
		if _, exists := existingEsc[escCodeStep]; exists {
			continue // already escalated this issue
		}

		branch := fmt.Sprintf("foreman/%s/issue-%d-esc", w.Name, n)
		steps = append(steps,
			foremanv1alpha1.PipelineStep{
				Name:     escCodeStep,
				Kind:     foremanv1alpha1.AgenticTaskKindIssueFix,
				AgentRef: *w.Spec.EscalationCoderAgentRef,
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:         w.Spec.Repo,
					Issue:        n,
					Branch:       branch,
					PromptPrefix: escalationHintPrompt(coderSummary(base)),
				},
			},
			foremanv1alpha1.PipelineStep{
				Name:      escVerifyStep,
				Kind:      foremanv1alpha1.AgenticTaskKindVerify,
				AgentRef:  *w.Spec.VerifierAgentRef,
				DependsOn: []string{escCodeStep},
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:   w.Spec.Repo,
					Issue:  n,
					Branch: branch,
				},
			},
		)
		escalated = append(escalated, n)
	}
	return steps, escalated
}
