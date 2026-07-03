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
	"strings"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// escalationSteps decides which escalate-<N>-<j> review steps to emit
// NOW, given the Workload spec and its current children (#546).
//
// For each issue N the trigger is: every base reviewer child
// (step label "review-<N>-<i>") is terminal (Phase Succeeded or
// Failed) AND at least one carries verdict=NO-GO. A cascade
// INCOMPLETE after GATE-FAIL/GATE-ERROR (#548) is terminal but is
// not a NO-GO, so a branch the gate already rejected never burns
// escalation compute. Steps whose escalate-<N>-<j> child already
// exists are skipped individually, so a partial create failure is
// repaired on the next reconcile instead of stranding the missing
// reviewers. Because the trigger only scans review-<N>-* children,
// an escalation reviewer's own NO-GO can never trigger another round
// (the tier is exactly two levels deep).
//
// Pure function: no API calls, no status writes. The caller owns
// MaxTasks accounting, sovereignty filtering, and creation.
// Callers must also restrict invocation to issue-batch mode: an
// explicit spec.Pipeline takes precedence over Issues at planning
// time, and user-authored pipeline step names could otherwise
// false-match the review-<N>-* prefixes scanned here.
func escalationSteps(
	w *foremanv1alpha1.Workload, children []foremanv1alpha1.AgenticTask,
) (steps []foremanv1alpha1.PipelineStep, escalated []int32) {
	if len(w.Spec.EscalationReviewerAgentRefs) == 0 ||
		len(w.Spec.ReviewerAgentRefs) == 0 ||
		len(w.Spec.Issues) == 0 {
		return nil, nil
	}

	for _, n := range w.Spec.Issues {
		// The trailing dash keeps issue 64 from matching review-641-0.
		basePrefix := fmt.Sprintf("review-%d-", n)
		escPrefix := fmt.Sprintf("escalate-%d-", n)
		// The coder-escalation tier emits its own reviews (review-<N>-esc-<i>)
		// which also carry basePrefix but belong to the escalated attempt, not
		// the base reviewer set this tier escalates. Exclude them so the two
		// tiers compose (they only coexist when both EscalationReviewerAgentRefs
		// and EscalationCoderAgentRef are set); otherwise their NO-GO would fan
		// out escalate steps against the wrong (base) branch.
		coderEscReviewPrefix := fmt.Sprintf("review-%d-esc-", n)

		var baseTotal, baseTerminal, baseNoGo int
		existingEsc := map[string]struct{}{}
		for i := range children {
			step := children[i].Labels[labelStep]
			switch {
			case strings.HasPrefix(step, escPrefix):
				existingEsc[step] = struct{}{}
			case strings.HasPrefix(step, coderEscReviewPrefix):
				// coder-escalation branch review; not part of the base set
			case strings.HasPrefix(step, basePrefix):
				baseTotal++
				phase := children[i].Status.Phase
				if phase == foremanv1alpha1.AgenticTaskPhaseSucceeded ||
					phase == foremanv1alpha1.AgenticTaskPhaseFailed {
					baseTerminal++
				}
				if children[i].Status.Verdict == foremanv1alpha1.AgenticTaskVerdictNoGo {
					baseNoGo++
				}
			}
		}

		if baseTotal == 0 || baseTerminal < baseTotal || baseNoGo == 0 {
			continue
		}

		branch := fmt.Sprintf("foreman/%s/issue-%d", w.Name, n)
		issueEscalated := false
		for j, ref := range w.Spec.EscalationReviewerAgentRefs {
			name := fmt.Sprintf("escalate-%d-%d", n, j)
			if _, exists := existingEsc[name]; exists {
				continue
			}
			steps = append(steps, foremanv1alpha1.PipelineStep{
				Name:     name,
				Kind:     foremanv1alpha1.AgenticTaskKindReview,
				AgentRef: ref,
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:   w.Spec.Repo,
					Issue:  n,
					Branch: branch,
				},
			})
			issueEscalated = true
		}
		if issueEscalated {
			escalated = append(escalated, n)
		}
	}
	return steps, escalated
}

// emitEscalations is the second-pass emission hook (#546): called from
// Reconcile's children-exist branch before rollup. Synthesizes the due
// escalate-<N>-<j> steps, applies MaxTasks accounting and the
// sovereignty gates, creates the tasks, and patches status (Tasks list
// + EscalationTriggered + CloudReviewersSuppressed / Truncated as
// applicable). Returns the refreshed children slice so rollup counts
// the new tasks as in-flight; on a no-op it returns the input slice.
func (r *WorkloadReconciler) emitEscalations(
	ctx context.Context, w *foremanv1alpha1.Workload, children []foremanv1alpha1.AgenticTask,
) ([]foremanv1alpha1.AgenticTask, error) {
	log := logf.FromContext(ctx).WithName("workload").WithValues("workload", client.ObjectKeyFromObject(w))

	if len(w.Spec.Pipeline) > 0 {
		// Explicit Pipeline mode: the user authors their own DAG, and
		// user-authored step names could false-match the review-<N>-*
		// prefixes escalationSteps scans. Escalation is an issue-batch
		// feature (see the field's CRD doc).
		return children, nil
	}

	if len(w.Spec.EscalationReviewerAgentRefs) > 0 && len(w.Spec.ReviewerAgentRefs) == 0 {
		// There is no base tier to escalate from; surface the
		// misconfiguration once and continue as a no-reviewer batch.
		if cond := apimeta.FindStatusCondition(w.Status.Conditions, conditionTypeEscalationTriggered); cond != nil &&
			cond.Status == metav1.ConditionFalse && cond.Reason == "NoBaseReviewers" {
			return children, nil
		}
		patch := client.MergeFrom(w.DeepCopy())
		setCondition(&w.Status.Conditions, metav1.Condition{
			Type:               conditionTypeEscalationTriggered,
			Status:             metav1.ConditionFalse,
			Reason:             "NoBaseReviewers",
			Message:            "spec.escalationReviewerAgentRefs is set but spec.reviewerAgentRefs is empty; there is no base reviewer tier to escalate from",
			LastTransitionTime: metav1.Now(),
		})
		if err := r.Status().Patch(ctx, w, patch); err != nil {
			return children, fmt.Errorf("patch NoBaseReviewers condition: %w", err)
		}
		return children, nil
	}

	steps, escalated := escalationSteps(w, children)
	if len(steps) == 0 {
		return children, nil
	}

	patch := client.MergeFrom(w.DeepCopy())
	now := metav1.Now()

	if w.Spec.MaxTasks > 0 && len(children)+len(steps) > int(w.Spec.MaxTasks) {
		// No silent cap: report why the escalation tier did not run.
		setCondition(&w.Status.Conditions, metav1.Condition{
			Type:               conditionTypeTruncated,
			Status:             metav1.ConditionTrue,
			Reason:             "MaxTasksEscalationCap",
			Message:            fmt.Sprintf("MaxTasks=%d leaves no room for %d escalation task(s)", w.Spec.MaxTasks, len(steps)),
			LastTransitionTime: now,
		})
		if err := r.Status().Patch(ctx, w, patch); err != nil {
			return children, fmt.Errorf("patch escalation truncation condition: %w", err)
		}
		return children, nil
	}

	steps, suppressed, err := r.filterCloudProviders(ctx, w, steps)
	if err != nil {
		return children, fmt.Errorf("filter escalation providers: %w", err)
	}

	created, createErr := r.renderAndCreate(ctx, w, steps)
	if createErr != nil {
		log.Error(createErr, "creating escalation AgenticTasks failed mid-way", "createdSoFar", len(created))
	}

	msg := fmt.Sprintf("issues %s escalated after base reviewer NO-GO (%d task(s) created, %d suppressed)",
		joinInt32(escalated), len(created), len(suppressed))

	// Steady-state short-circuit: when nothing was created (e.g. every
	// escalation ref is cloud-suppressed) and the condition already
	// says exactly this, re-patching every rollup would only churn
	// LastTransitionTime. escalationSteps re-proposes these steps on
	// every reconcile because no escalate child ever exists.
	if len(created) == 0 && createErr == nil {
		if cond := apimeta.FindStatusCondition(w.Status.Conditions, conditionTypeEscalationTriggered); cond != nil &&
			cond.Status == metav1.ConditionTrue && cond.Message == msg {
			return children, nil
		}
	}

	w.Status.Tasks = appendNewTaskRefs(w.Status.Tasks, created)
	setCondition(&w.Status.Conditions, metav1.Condition{
		Type:               conditionTypeEscalationTriggered,
		Status:             metav1.ConditionTrue,
		Reason:             "BaseReviewerNoGo",
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
		return children, fmt.Errorf("patch workload status after escalation emission: %w", err)
	}
	if createErr != nil {
		return children, createErr
	}

	// Rollup must see the new tasks as in-flight in this same pass. A
	// cache-backed List here could miss tasks created microseconds ago
	// (informer lag) and let rollup mark the Workload Failed before the
	// create's watch event arrives, so synthesize placeholders instead:
	// rollup buckets by Status.Phase and a zero-value phase counts as
	// in-flight.
	existing := make(map[string]struct{}, len(children))
	for i := range children {
		existing[children[i].Name] = struct{}{}
	}
	for _, ref := range created {
		if _, ok := existing[ref.Name]; ok {
			continue
		}
		children = append(children, foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace},
		})
	}
	return children, nil
}

// joinInt32 renders issue numbers as "641, 643" for condition messages.
func joinInt32(ns []int32) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = fmt.Sprintf("%d", n)
	}
	return strings.Join(parts, ", ")
}

// appendNewTaskRefs appends only refs not already present by name, so
// a reconcile echo against a stale AgenticTask cache cannot duplicate
// Status.Tasks bookkeeping (the tasks themselves are AlreadyExists-
// deduped by renderAndCreate).
func appendNewTaskRefs(existing, created []corev1.ObjectReference) []corev1.ObjectReference {
	seen := make(map[string]struct{}, len(existing))
	for _, ref := range existing {
		seen[ref.Name] = struct{}{}
	}
	for _, ref := range created {
		if _, dup := seen[ref.Name]; !dup {
			existing = append(existing, ref)
		}
	}
	return existing
}
