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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/reviewer"
)

// conditionTypeReviewIterationTriggered reports that at least one
// issue's reviewer round went terminal with a NO-GO and a bounded fix
// iteration was appended (#946): a new coder task carrying the review
// feedback in payload.prompt, plus a fresh verify + reviewer fan-out
// chained behind it.
const conditionTypeReviewIterationTriggered = "ReviewIterationTriggered"

// defaultMaxReviewIterations is the per-issue fix-iteration bound when
// Workload.spec.maxReviewIterations is unset. One iteration mirrors
// what a human does with a single request-changes round; an explicit 0
// opts back into the pre-#946 fail-on-first-NO-GO behavior.
const defaultMaxReviewIterations = 1

// effectiveMaxReviewIterations resolves the *int32 three-state:
// nil defaults, explicit values (including 0) win.
func effectiveMaxReviewIterations(w *foremanv1alpha1.Workload) int {
	if w.Spec.MaxReviewIterations == nil {
		return defaultMaxReviewIterations
	}
	return int(*w.Spec.MaxReviewIterations)
}

// iterationSuffixed renders the step name for fix iteration k: the
// base name for the initial round (k=0), "<base>-r<k>" for k >= 1.
func iterationSuffixed(base string, k int) string {
	if k == 0 {
		return base
	}
	return fmt.Sprintf("%s-r%d", base, k)
}

func codeStepName(n int32, k int) string {
	return iterationSuffixed(fmt.Sprintf("code-%d", n), k)
}

func verifyStepName(n int32, k int) string {
	return iterationSuffixed(fmt.Sprintf("verify-%d", n), k)
}

func reviewStepName(n int32, i, k int) string {
	return iterationSuffixed(fmt.Sprintf("review-%d-%d", n, i), k)
}

// parseIterationTail matches "<base>" (iteration 0) or "<base>-r<k>"
// (iteration k >= 1). Returns ok=false for anything else, including
// longer names that merely share the base as a prefix (code-64 must
// not match code-641).
func parseIterationTail(step, base string) (int, bool) {
	if step == base {
		return 0, true
	}
	tail, found := strings.CutPrefix(step, base+"-r")
	if !found {
		return 0, false
	}
	k, err := strconv.Atoi(tail)
	if err != nil || k < 1 {
		return 0, false
	}
	return k, true
}

// reviewIterationOf parses a reviewer fan-out step name for issue n
// ("review-<n>-<i>" or "review-<n>-<i>-r<k>") and returns its fix
// iteration. The reviewer index digits are validated so issue 64
// never cross-matches review-641-0.
func reviewIterationOf(step string, n int32) (int, bool) {
	rest, found := strings.CutPrefix(step, fmt.Sprintf("review-%d-", n))
	if !found {
		return 0, false
	}
	idx, tail, hasIter := strings.Cut(rest, "-r")
	if idx == "" {
		return 0, false
	}
	for _, c := range idx {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	if !hasIter {
		return 0, true
	}
	k, err := strconv.Atoi(tail)
	if err != nil || k < 1 {
		return 0, false
	}
	return k, true
}

// issueStepIteration classifies a synthesized issue-batch step name
// (code / verify / review, any iteration) for issue n. Escalation
// steps and foreign names return ok=false.
func issueStepIteration(step string, n int32) (int, bool) {
	if k, ok := parseIterationTail(step, fmt.Sprintf("code-%d", n)); ok {
		return k, true
	}
	if k, ok := parseIterationTail(step, fmt.Sprintf("verify-%d", n)); ok {
		return k, true
	}
	return reviewIterationOf(step, n)
}

// noGoReviewRound inspects issue n's reviewer fan-out for fix
// iteration k. It returns the NO-GO review tasks when the round is
// complete — at least one review child exists, every one is terminal
// (Phase Succeeded or Failed), and at least one carries verdict=NO-GO
// — and nil otherwise. A cascade INCOMPLETE after GATE-FAIL (#548) is
// terminal but not a NO-GO, so a branch the gate rejected never
// triggers a fix iteration. Scanning what exists (rather than the
// expected reviewer count) keeps cloud-suppressed reviewers from
// permanently blocking the trigger, mirroring escalationSteps.
func noGoReviewRound(
	children []foremanv1alpha1.AgenticTask, n int32, k int,
) []*foremanv1alpha1.AgenticTask {
	var total, terminal int
	var noGo []*foremanv1alpha1.AgenticTask
	for i := range children {
		iter, ok := reviewIterationOf(children[i].Labels[labelStep], n)
		if !ok || iter != k {
			continue
		}
		total++
		phase := children[i].Status.Phase
		if phase == foremanv1alpha1.AgenticTaskPhaseSucceeded ||
			phase == foremanv1alpha1.AgenticTaskPhaseFailed {
			terminal++
		}
		if children[i].Status.Verdict == foremanv1alpha1.AgenticTaskVerdictNoGo {
			noGo = append(noGo, &children[i])
		}
	}
	if total == 0 || terminal < total || len(noGo) == 0 {
		return nil
	}
	return noGo
}

// reviewIterationSteps decides which code-<n>-r<k> / verify-<n>-r<k> /
// review-<n>-<i>-r<k> steps to emit NOW, given the Workload spec and
// its current children (#946).
//
// For each issue n it walks the fix iterations in order: iteration k
// (1..maxReviewIterations) is due once iteration k-1's reviewer round
// is terminal with at least one NO-GO. Steps whose child already
// exists are skipped individually, so a partial create failure is
// repaired on the next reconcile instead of stranding the tail of the
// iteration — the same idempotency-by-existence contract as
// escalationSteps. The walk stops at the first round that is still in
// flight, converged (no NO-GO), or past the bound, so exhausting
// maxReviewIterations leaves the terminal NO-GO for rollup to fail on
// as before #946.
//
// The iteration's coder step re-targets the SAME branch: the payload
// sets reviseFromBranch to the task's own branch name so the executor
// restores the prior attempt from the push remote before the model
// runs (#951), allowOverwrite so the coder can replace that attempt
// (force-with-lease push, #573/#934), and payload.prompt carries the
// distilled review feedback so the retry is not blind. The coder step
// references spec.revisionCoderAgentRef when set — a revision amends
// existing work and wants a revision-tuned profile, not the issue-fix
// forcing profile — and falls back to spec.coderAgentRef otherwise
// (the caller emits the RevisionUnderIssueFixProfile warning).
//
// Pure function: no API calls, no status writes. The caller owns
// MaxTasks accounting, sovereignty filtering, and creation, and must
// restrict invocation to issue-batch mode (user-authored Pipeline step
// names could false-match the synthesized naming scheme).
func reviewIterationSteps(
	w *foremanv1alpha1.Workload, children []foremanv1alpha1.AgenticTask,
) (steps []foremanv1alpha1.PipelineStep, iterated []int32) {
	maxIter := effectiveMaxReviewIterations(w)
	if maxIter <= 0 ||
		len(w.Spec.ReviewerAgentRefs) == 0 ||
		len(w.Spec.Issues) == 0 ||
		w.Spec.CoderAgentRef == nil ||
		w.Spec.VerifierAgentRef == nil {
		return nil, nil
	}

	existing := make(map[string]struct{}, len(children))
	for i := range children {
		if step := children[i].Labels[labelStep]; step != "" {
			existing[step] = struct{}{}
		}
	}

	coderRef := w.Spec.CoderAgentRef
	if w.Spec.RevisionCoderAgentRef != nil {
		coderRef = w.Spec.RevisionCoderAgentRef
	}

	for _, n := range w.Spec.Issues {
		branch := fmt.Sprintf("foreman/%s/issue-%d", w.Name, n)
		issueIterated := false
		for k := 1; k <= maxIter; k++ {
			noGo := noGoReviewRound(children, n, k-1)
			if len(noGo) == 0 {
				break
			}
			codeName := codeStepName(n, k)
			verifyName := verifyStepName(n, k)
			if _, ok := existing[codeName]; !ok {
				steps = append(steps, foremanv1alpha1.PipelineStep{
					Name:     codeName,
					Kind:     foremanv1alpha1.AgenticTaskKindIssueFix,
					AgentRef: *coderRef,
					Payload: foremanv1alpha1.AgenticTaskPayload{
						Repo:   w.Spec.Repo,
						Issue:  n,
						Branch: branch,
						// The prior attempt lives at the task's own branch name on
						// the push remote; the executor restores it (#951) and, under
						// the rebase strategy, replays it onto the current base so
						// this in-review revision does not revert work merged since
						// the prior attempt (#1029).
						ReviseFromBranch: branch,
						BranchStrategy:   foremanv1alpha1.BranchStrategyRebase,
						AllowOverwrite:   true,
						Prompt:           reviewFeedbackPrompt(noGo),
					},
				})
				issueIterated = true
			}
			if _, ok := existing[verifyName]; !ok {
				steps = append(steps, foremanv1alpha1.PipelineStep{
					Name:      verifyName,
					Kind:      foremanv1alpha1.AgenticTaskKindVerify,
					AgentRef:  *w.Spec.VerifierAgentRef,
					DependsOn: []string{codeName},
					Payload: foremanv1alpha1.AgenticTaskPayload{
						Repo:   w.Spec.Repo,
						Issue:  n,
						Branch: branch,
					},
				})
				issueIterated = true
			}
			// Mirrors the base-round stamp (#937): an iterated-then-approved
			// issue must still open its PR.
			openPR := w.Spec.OpenPullRequest == nil || *w.Spec.OpenPullRequest
			for i, reviewerRef := range w.Spec.ReviewerAgentRefs {
				name := reviewStepName(n, i, k)
				if _, ok := existing[name]; ok {
					continue
				}
				steps = append(steps, foremanv1alpha1.PipelineStep{
					Name:      name,
					Kind:      foremanv1alpha1.AgenticTaskKindReview,
					AgentRef:  reviewerRef,
					DependsOn: []string{verifyName},
					Payload: foremanv1alpha1.AgenticTaskPayload{
						Repo:            w.Spec.Repo,
						Issue:           n,
						Branch:          branch,
						OpenPullRequest: openPR,
					},
				})
				issueIterated = true
			}
		}
		if issueIterated {
			iterated = append(iterated, n)
		}
	}
	return steps, iterated
}

// reviewFeedbackPrompt distills the NO-GO reviewers' structured
// results into the fix iteration's coder prompt. The executor's
// buildUserPrompt drops this text into the "Issue context" slot of the
// issue-fix template, so the retry runs with the rejection in front of
// it instead of blind (#946: 4 production issues burned 3 attempts
// each reproducing the same rejected patch).
//
// The wording matches the executor's revise-from-branch restore
// (#951): because the step's payload stamps reviseFromBranch, the
// workspace really does start from the prior attempt's files, so the
// prompt directs the model to amend that work as a delta rather than
// rebuild the fix from scratch.
func reviewFeedbackPrompt(noGo []*foremanv1alpha1.AgenticTask) string {
	var b strings.Builder
	b.WriteString("The reviewer rejected the previous attempt on this issue (verdict NO-GO).\n")
	b.WriteString("Your workspace already contains that previous attempt: the working branch was\n")
	b.WriteString("restored from this task's branch on the remote, so its files and history are\n")
	b.WriteString("present. Do not rebuild the fix from scratch. Amend the existing work with the\n")
	b.WriteString("smallest changes that address every point below, then push to the same branch.\n")
	for _, t := range noGo {
		b.WriteString("\n")
		b.WriteString(reviewFeedbackSection(t))
	}
	b.WriteString("\nAddress this feedback.\n")
	return b.String()
}

// reviewFeedbackSection renders one NO-GO reviewer's summary +
// findings. The structured findings live in the review task's
// status.result at extra.modelExtra (the reviewer's submit_result
// extra; see pkg/foreman/agent/reviewer). Rendering is best-effort:
// findings that fail the strict schema fall back to their raw JSON so
// legacy map-shaped findings ("scope_creep" + "*_details" keys) still
// reach the coder, and a result-less task degrades to a one-liner.
func reviewFeedbackSection(t *foremanv1alpha1.AgenticTask) string {
	label := t.Labels[labelStep]
	if label == "" {
		label = t.Name
	}

	var envelope struct {
		Summary string `json:"summary"`
		Extra   struct {
			ModelExtra map[string]any `json:"modelExtra"`
		} `json:"extra"`
	}
	if t.Status.Result != nil && len(t.Status.Result.Raw) > 0 {
		// Best-effort: malformed JSON leaves the envelope zero-valued.
		_ = json.Unmarshal(t.Status.Result.Raw, &envelope)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Reviewer %s", label)
	if envelope.Summary != "" {
		fmt.Fprintf(&b, ": %s", envelope.Summary)
	}
	b.WriteString("\n")

	findings, _ := reviewer.ParseFindings(envelope.Extra.ModelExtra)
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintf(&b, "- [%s/%s] %s", f.Severity, f.Area, f.Message)
			if f.File != "" {
				fmt.Fprintf(&b, " (%s", f.File)
				if f.Line > 0 {
					fmt.Fprintf(&b, ":%d", f.Line)
				}
				b.WriteString(")")
			}
			if f.Suggestion != "" {
				fmt.Fprintf(&b, "; suggested fix: %s", f.Suggestion)
			}
			b.WriteString("\n")
		}
		return b.String()
	}

	// Schema-strict parse yielded nothing; pass raw findings through so
	// non-conforming shapes still inform the retry.
	if raw, ok := envelope.Extra.ModelExtra["findings"]; ok {
		if buf, err := json.Marshal(raw); err == nil && len(buf) > 0 && string(buf) != "null" {
			fmt.Fprintf(&b, "- findings: %s\n", buf)
		}
	}
	return b.String()
}

// activeChildren filters out children that a later attempt superseded,
// so rollup and the escalation trigger judge each issue by its LATEST
// attempt only. Two supersession rules apply:
//
//   - Fix iteration (#946/#959): a later code-<n>-r<k> round supersedes
//     earlier synthesized steps for issue n. Without this, iteration 0's
//     terminal NO-GO review would keep IncompleteTasks non-zero forever
//     and a converged r1 round could never complete the Workload.
//   - Coder escalation (#963): once a code-<n>-esc task exists, the base
//     attempt's steps (code-<n>, verify-<n>, review-<n>-<i>, and any
//     iteration rounds) are dropped so the base coder's terminal NO-GO
//     and the cascade-failed verify/review it stranded do not pin the
//     Workload at Failed. The issue is then judged by the ESC attempt.
//
// A superseded iteration round is by construction fully terminal (the
// k+1 trigger requires it); an escalated base attempt is likewise
// terminal (the escalation trigger requires a terminal base code task),
// so nothing in flight is ever hidden from rollup. The escalation steps
// themselves (suffix -esc, which do not parse as base issue steps) and
// anything else that does not parse as a synthesized issue step are
// always kept. Issue-batch only; explicit Pipeline mode returns the
// input unchanged.
func activeChildren(
	w *foremanv1alpha1.Workload, children []foremanv1alpha1.AgenticTask,
) []foremanv1alpha1.AgenticTask {
	if len(w.Spec.Pipeline) > 0 || len(w.Spec.Issues) == 0 {
		return children
	}

	latest := make(map[int32]int, len(w.Spec.Issues))
	escalated := make(map[int32]bool, len(w.Spec.Issues))
	for i := range children {
		step := children[i].Labels[labelStep]
		for _, n := range w.Spec.Issues {
			if step == fmt.Sprintf("code-%d-esc", n) {
				escalated[n] = true
				break
			}
			if k, ok := issueStepIteration(step, n); ok {
				if k > latest[n] {
					latest[n] = k
				}
				break
			}
		}
	}

	active := make([]foremanv1alpha1.AgenticTask, 0, len(children))
	for i := range children {
		step := children[i].Labels[labelStep]
		superseded := false
		for _, n := range w.Spec.Issues {
			if k, ok := issueStepIteration(step, n); ok {
				// A later fix iteration, or a successful escalation on this
				// issue, supersedes the base/earlier synthesized attempt.
				// issueStepIteration never matches the -esc steps, so the
				// escalation attempt is never dropped by its own rule.
				superseded = k < latest[n] || escalated[n]
				break
			}
		}
		if !superseded {
			active = append(active, children[i])
		}
	}
	return active
}

// countReviewIterations sums the latest fix-iteration index per issue
// over the given step names, for status.reviewIterations. Recomputed
// from observed state (not incremented) so reconcile echoes cannot
// inflate it.
func countReviewIterations(w *foremanv1alpha1.Workload, stepNames map[string]struct{}) int32 {
	var total int32
	for _, n := range w.Spec.Issues {
		best := 0
		for step := range stepNames {
			if k, ok := issueStepIteration(step, n); ok && k > best {
				best = k
			}
		}
		total += int32(best)
	}
	return total
}

// emitReviewIterations is the fix-iteration emission hook (#946):
// called from Reconcile's children-exist branch after the advisory
// wiring and before escalation + rollup. Synthesizes the due
// code/verify/review -r<k> steps, applies MaxTasks accounting and the
// sovereignty gates, creates the tasks, and patches status (Tasks
// list + reviewIterations + ReviewIterationTriggered +
// CloudReviewersSuppressed / Truncated as applicable). Returns the
// refreshed children slice so escalation and rollup see the new tasks
// as in-flight; on a no-op it returns the input slice.
func (r *WorkloadReconciler) emitReviewIterations(
	ctx context.Context, w *foremanv1alpha1.Workload, children []foremanv1alpha1.AgenticTask,
) ([]foremanv1alpha1.AgenticTask, error) {
	log := logf.FromContext(ctx).WithName("workload").WithValues("workload", client.ObjectKeyFromObject(w))

	if len(w.Spec.Pipeline) > 0 {
		// Explicit Pipeline mode: user-authored step names could
		// false-match the synthesized code/verify/review-<n> naming the
		// iteration walker scans. Iteration is an issue-batch feature.
		return children, nil
	}

	steps, iterated := reviewIterationSteps(w, children)
	if len(steps) == 0 {
		return children, nil
	}

	patch := client.MergeFrom(w.DeepCopy())
	now := metav1.Now()

	if w.Spec.MaxTasks > 0 && len(children)+len(steps) > int(w.Spec.MaxTasks) {
		// No silent cap: report why the fix iteration did not run. The
		// terminal NO-GO then rolls the Workload to Failed as before.
		setCondition(&w.Status.Conditions, metav1.Condition{
			Type:               conditionTypeTruncated,
			Status:             metav1.ConditionTrue,
			Reason:             "MaxTasksIterationCap",
			Message:            fmt.Sprintf("MaxTasks=%d leaves no room for %d fix-iteration task(s)", w.Spec.MaxTasks, len(steps)),
			LastTransitionTime: now,
		})
		if err := r.Status().Patch(ctx, w, patch); err != nil {
			return children, fmt.Errorf("patch iteration truncation condition: %w", err)
		}
		return children, nil
	}

	steps, suppressed, err := r.filterCloudProviders(ctx, w, steps)
	if err != nil {
		return children, fmt.Errorf("filter iteration providers: %w", err)
	}

	// Revision-vs-profile pairing (#951): a fix-iteration coder task
	// amends its restored prior attempt, and the issue-fix Agent's
	// forcing profile is tuned for building a fix from scratch — the
	// documented #951 failure mode is a revision task collapsing under
	// it. Warn (once per emission pass) when the fallback is in play so
	// the operator knows to set spec.revisionCoderAgentRef.
	if r.Recorder != nil && w.Spec.RevisionCoderAgentRef == nil {
		for _, s := range steps {
			if s.Kind != foremanv1alpha1.AgenticTaskKindIssueFix {
				continue
			}
			r.Recorder.Eventf(w, nil, corev1.EventTypeWarning, "RevisionUnderIssueFixProfile", "Reconcile",
				"fix-iteration coder step %s falls back to the issue-fix Agent %q; a revision task amends its restored prior attempt and can collapse under the issue-fix forcing profile (#951) — set spec.revisionCoderAgentRef to pair iterations with a revision-tuned profile",
				s.Name, w.Spec.CoderAgentRef.Name)
			break
		}
	}

	created, createErr := r.renderAndCreate(ctx, w, steps)
	if createErr != nil {
		log.Error(createErr, "creating fix-iteration AgenticTasks failed mid-way", "createdSoFar", len(created))
	}

	msg := fmt.Sprintf("issues %s re-dispatched to the coder after reviewer NO-GO (%d task(s) created, %d suppressed)",
		joinInt32(iterated), len(created), len(suppressed))

	// Steady-state short-circuit: when nothing was created (e.g. the
	// only missing steps are cloud-suppressed reviewers) and the
	// condition already says exactly this, re-patching every rollup
	// would only churn LastTransitionTime.
	if len(created) == 0 && createErr == nil {
		if cond := apimeta.FindStatusCondition(w.Status.Conditions, conditionTypeReviewIterationTriggered); cond != nil &&
			cond.Status == metav1.ConditionTrue && cond.Message == msg {
			return children, nil
		}
	}

	stepNames := make(map[string]struct{}, len(children)+len(created))
	for i := range children {
		if step := children[i].Labels[labelStep]; step != "" {
			stepNames[step] = struct{}{}
		}
	}
	for _, ref := range created {
		stepNames[strings.TrimPrefix(ref.Name, w.Name+"-")] = struct{}{}
	}

	w.Status.Tasks = appendNewTaskRefs(w.Status.Tasks, created)
	w.Status.ReviewIterations = countReviewIterations(w, stepNames)
	setCondition(&w.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReviewIterationTriggered,
		Status:             metav1.ConditionTrue,
		Reason:             "ReviewerNoGo",
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
		return children, fmt.Errorf("patch workload status after iteration emission: %w", err)
	}
	if createErr != nil {
		return children, createErr
	}

	// Escalation + rollup must see the new tasks as in-flight in this
	// same pass. A cache-backed List here could miss tasks created
	// microseconds ago (informer lag), so synthesize labeled
	// placeholders instead: a zero-value phase counts as in-flight, and
	// the step label lets activeChildren + the escalation trigger
	// recognize the new round immediately (without it, the superseded
	// round's terminal NO-GO would fire escalation one pass early).
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
