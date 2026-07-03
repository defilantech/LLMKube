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
	"encoding/json"
	"fmt"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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

// coderEscalationSteps decides which code-<N>-esc / verify-<N>-esc /
// review-<N>-esc-<i> steps to emit NOW (mirrors escalationSteps for
// reviewers, #546). The reviewer fan-out is emitted only when the
// Workload sets ReviewerAgentRefs, so the escalated branch gets a
// verdict and a PR carrier rather than ending unreviewed. For each
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
		// Reviewer fan-out on the escalated branch: without it the
		// escalated coder can GO and gate-pass but nothing produces a
		// verdict or (post-#956) the openPullRequest carrier, so the
		// fix ends as a pushed, unreviewed branch with no PR. Mirrors
		// the base review-<N>-<i> emission in synthesizeIssueBatch. No
		// reviewers configured means code+verify only (unchanged).
		openPR := w.Spec.OpenPullRequest == nil || *w.Spec.OpenPullRequest
		for i, reviewerRef := range w.Spec.ReviewerAgentRefs {
			steps = append(steps, foremanv1alpha1.PipelineStep{
				Name:      fmt.Sprintf("review-%d-esc-%d", n, i),
				Kind:      foremanv1alpha1.AgenticTaskKindReview,
				AgentRef:  reviewerRef,
				DependsOn: []string{escVerifyStep},
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:            w.Spec.Repo,
					Issue:           n,
					Branch:          branch,
					OpenPullRequest: openPR,
				},
			})
		}
		escalated = append(escalated, n)
	}
	return steps, escalated
}

// emitCoderEscalations is the coder-side second-pass emission hook,
// called from Reconcile's children-exist branch BEFORE emitEscalations
// (a coder that did not GO has no branch for reviewers to escalate on).
// Mirrors emitEscalations: synthesize due code-<N>-esc/verify-<N>-esc
// steps, apply MaxTasks + sovereignty gates, create, patch status, and
// synthesize placeholders so rollup counts the new tasks as in-flight.
// Returns the refreshed children slice; a no-op returns the input.
func (r *WorkloadReconciler) emitCoderEscalations(
	ctx context.Context, w *foremanv1alpha1.Workload, children []foremanv1alpha1.AgenticTask,
) ([]foremanv1alpha1.AgenticTask, error) {
	log := logf.FromContext(ctx).WithName("workload").WithValues("workload", client.ObjectKeyFromObject(w))

	if len(w.Spec.Pipeline) > 0 {
		// Explicit Pipeline mode: escalation is an issue-batch feature.
		return children, nil
	}
	if w.Spec.EscalationCoderAgentRef == nil {
		return children, nil
	}

	steps, escalated := coderEscalationSteps(w, children)
	if len(steps) == 0 {
		return children, nil
	}

	patch := client.MergeFrom(w.DeepCopy())
	now := metav1.Now()

	if w.Spec.MaxTasks > 0 && len(children)+len(steps) > int(w.Spec.MaxTasks) {
		// No silent cap: report why the coder escalation tier did not run.
		setCondition(&w.Status.Conditions, metav1.Condition{
			Type:               conditionTypeTruncated,
			Status:             metav1.ConditionTrue,
			Reason:             "MaxTasksCoderEscalationCap",
			Message:            fmt.Sprintf("MaxTasks=%d leaves no room for %d coder escalation task(s)", w.Spec.MaxTasks, len(steps)),
			LastTransitionTime: now,
		})
		if err := r.Status().Patch(ctx, w, patch); err != nil {
			return children, fmt.Errorf("patch coder-escalation truncation condition: %w", err)
		}
		return children, nil
	}

	steps, suppressed, err := r.filterCloudProviders(ctx, w, steps)
	if err != nil {
		return children, fmt.Errorf("filter coder-escalation providers: %w", err)
	}

	created, createErr := r.renderAndCreate(ctx, w, steps)
	if createErr != nil {
		log.Error(createErr, "creating coder escalation AgenticTasks failed mid-way", "createdSoFar", len(created))
	}

	msg := fmt.Sprintf("issues %s re-dispatched to the escalation coder after a capability failure (%d task(s) created, %d suppressed)",
		joinInt32(escalated), len(created), len(suppressed))

	// Steady-state short-circuit: coderEscalationSteps re-proposes these
	// steps until the -esc child exists in cache, so when nothing was
	// created and the condition already says exactly this, re-patching
	// would only churn LastTransitionTime.
	if len(created) == 0 && createErr == nil {
		if cond := apimeta.FindStatusCondition(w.Status.Conditions, conditionTypeCoderEscalationTriggered); cond != nil &&
			cond.Status == metav1.ConditionTrue && cond.Message == msg {
			return children, nil
		}
	}

	w.Status.Tasks = appendNewTaskRefs(w.Status.Tasks, created)
	setCondition(&w.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCoderEscalationTriggered,
		Status:             metav1.ConditionTrue,
		Reason:             "BaseCoderCapabilityFailure",
		Message:            msg,
		LastTransitionTime: now,
	})
	if len(suppressed) > 0 {
		setCondition(&w.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCloudReviewersSuppressed,
			Status:             metav1.ConditionTrue,
			Reason:             "SovereigntyGate",
			Message:            fmt.Sprintf("skipped %d cloud-provider Agent(s): %s", len(suppressed), strings.Join(suppressed, "; ")),
			LastTransitionTime: now,
		})
	}
	if err := r.Status().Patch(ctx, w, patch); err != nil {
		return children, fmt.Errorf("patch workload status after coder escalation: %w", err)
	}
	if createErr != nil {
		return children, createErr
	}

	// Rollup and activeChildren must see the new tasks as in-flight in
	// this same pass; a cache-backed List could miss tasks created
	// microseconds ago, so synthesize labeled placeholders (a zero-value
	// phase counts as in-flight). The step label lets activeChildren
	// recognize the escalation immediately and supersede the failed base
	// attempt in the same reconcile (#963), matching emitReviewIterations.
	existing := make(map[string]struct{}, len(children))
	for i := range children {
		existing[children[i].Name] = struct{}{}
	}
	for _, ref := range created {
		if _, ok := existing[ref.Name]; ok {
			continue
		}
		children = append(children, foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ref.Name,
				Namespace: ref.Namespace,
				Labels: map[string]string{
					labelWorkload: w.Name,
					labelStep:     strings.TrimPrefix(ref.Name, w.Name+"-"),
				},
			},
		})
	}
	return children, nil
}
