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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// WorkloadReconciler turns a Workload into a set of AgenticTask objects
// owner-ref'd to it. v0.1 is a deterministic stub planner that supports
// two modes (precedence order):
//
//  1. Explicit pipeline (spec.Pipeline non-empty): emit one AgenticTask
//     per PipelineStep, rewriting step-local DependsOn names to absolute
//     task names.
//  2. Issue-batch shortcut (spec.Issues non-empty + Coder/Verifier refs):
//     synthesize a code+verify pair per issue and render as in (1).
//
// Intent-only Workloads fail fast with reason=NoPlannerOrPipeline. The
// LLM-driven planner branch (Anthropic API + prompt) is deferred to v0.2.
//
// On subsequent reconciles (children exist), the reconciler rolls up
// child phases into the Workload's status: counters + Phase
// (Dispatched | Completed | Failed) + Conditions.
type WorkloadReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// AllowCloudProviders is the operator-level sovereignty kill
	// switch. True (default) lets reviewer Agents with
	// spec.provider="cloud-proxy" dispatch (subject to per-Workload
	// AllowCloudReviewers gating). False makes the reconciler drop
	// any step whose Agent has a non-local provider and surface a
	// CloudReviewersSuppressed condition naming the dropped Agents.
	// Wired from the --allow-cloud-providers flag on foreman-operator
	// (charts/foreman value foreman.allowCloudProviders).
	AllowCloudProviders bool
}

// labelWorkload is the label the reconciler stamps on rendered AgenticTasks
// so we can List them efficiently when rolling up status without scanning
// every AgenticTask in the namespace.
const labelWorkload = "foreman.llmkube.dev/workload"

// labelStep records the PipelineStep.Name on rendered AgenticTasks for
// observability. Not load-bearing for the controller; useful for kubectl
// label selectors when debugging a batch.
const labelStep = "foreman.llmkube.dev/step"

// conditionTypePlanned signals the reconciler successfully rendered the
// initial AgenticTask set. Stays True once set.
const conditionTypePlanned = "Planned"

// conditionTypeTruncated is True when MaxTasks clipped the rendered set.
const conditionTypeTruncated = "Truncated"

// conditionTypeCompleted is True when all rendered AgenticTasks reached
// Succeeded; False with reason=ChildrenFailed when any reached Failed in
// a terminal way.
const conditionTypeCompleted = "Completed"

// conditionTypeCloudReviewersSuppressed is True when the sovereignty
// gates (operator --allow-cloud-providers + Workload AllowCloudReviewers)
// caused the reconciler to omit one or more reviewer Agents from the
// rendered set. The message names the suppressed Agents and which gate
// blocked each.
const conditionTypeCloudReviewersSuppressed = "CloudReviewersSuppressed"

// conditionTypeEscalationTriggered reports that at least one issue's
// base reviewers all went terminal with a NO-GO among them, so the
// escalation reviewer tier was emitted (#546). False with reason
// NoBaseReviewers flags a spec that lists escalation reviewers
// without any base reviewers to escalate from.
const conditionTypeEscalationTriggered = "EscalationTriggered"

// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=workloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=workloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=workloads/finalizers,verbs=update
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agentictasks,verbs=create;get;list;watch
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agents,verbs=get;list;watch

// Reconcile drives a Workload through Planning -> Planned -> Dispatched ->
// Completed | Failed.
func (r *WorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("workload").WithValues("workload", req.NamespacedName.String())

	var workload foremanv1alpha1.Workload
	if err := r.Get(ctx, req.NamespacedName, &workload); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// List children we already own. The label index keeps this fast and
	// scoped; child status changes re-queue us via Owns().
	children, err := r.listChildren(ctx, &workload)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list children: %w", err)
	}

	if len(children) > 0 {
		// Wire gate advisories from completed coder tasks into pending
		// review tasks so the reviewer's prompt can surface them.
		r.patchReviewAdvisories(ctx, &workload, children)

		// Second-pass emission (#546): escalation reviewers fire here,
		// after base reviewer verdicts land, before status rollup.
		children, err = r.emitEscalations(ctx, &workload, children)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("emit escalations: %w", err)
		}
		// Roll up child phases into the Workload's status.
		return r.rollup(ctx, &workload, children)
	}

	// No children yet -> first reconcile. Decide which mode and render.
	steps, truncated, modeErr := r.chooseSteps(&workload)
	if modeErr != nil {
		log.Info("workload has no actionable pipeline; failing", "reason", modeErr.Error())
		return r.failWorkload(ctx, &workload, "NoPlannerOrPipeline", modeErr.Error())
	}

	// Sovereignty filter (#540's sibling, v0.2): when either the
	// operator kill switch or the per-Workload gate blocks cloud
	// providers, drop the steps whose Agent.spec.Provider is non-
	// local. Cloud-blocked steps are returned in `suppressed` so the
	// CloudReviewersSuppressed condition can name them.
	steps, suppressed, filterErr := r.filterCloudProviders(ctx, &workload, steps)
	if filterErr != nil {
		log.Info("cloud-provider filter failed", "reason", filterErr.Error())
		return r.failWorkload(ctx, &workload, "AgentResolveFailed", filterErr.Error())
	}

	if err := r.markPlanning(ctx, &workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("mark planning: %w", err)
	}

	created, err := r.renderAndCreate(ctx, &workload, steps)
	if err != nil {
		// Partial creates are owner-ref'd already; status patch below
		// reflects what landed. A retry on the next reconcile picks up
		// where this one left off because chooseSteps is deterministic.
		log.Error(err, "rendering AgenticTasks failed mid-way", "createdSoFar", len(created))
	}

	return r.markPlanned(ctx, &workload, created, truncated, suppressed, err)
}

// filterCloudProviders drops PipelineSteps whose referenced Agent has a
// non-local provider when either sovereignty gate is closed. Returns
// the filtered steps, a list of skip messages (one per dropped step,
// shaped "<agent> (<reason>)"), and a hard error only when an Agent
// the steps reference can't be resolved.
//
// Fast path: when both gates are open, returns the input slice
// unmodified and skips Agent API lookups entirely.
func (r *WorkloadReconciler) filterCloudProviders(
	ctx context.Context, w *foremanv1alpha1.Workload, steps []foremanv1alpha1.PipelineStep,
) ([]foremanv1alpha1.PipelineStep, []string, error) {
	operatorAllows := r.AllowCloudProviders
	workloadAllows := w.Spec.AllowCloudReviewers == nil || *w.Spec.AllowCloudReviewers
	if operatorAllows && workloadAllows {
		return steps, nil, nil
	}

	kept := make([]foremanv1alpha1.PipelineStep, 0, len(steps))
	var suppressed []string
	for _, step := range steps {
		// Only inspect reviewer steps. Coder + verifier are local by
		// definition in v0.2; if a future deployment wants a cloud-
		// proxy coder, this gate widens uniformly. The tighter scope
		// also keeps the filter's API calls bounded (one Get per
		// reviewer Agent, not per task) and avoids fetching the same
		// coder/gate Agent N times per batch.
		if step.Kind != foremanv1alpha1.AgenticTaskKindReview {
			kept = append(kept, step)
			continue
		}
		var agent foremanv1alpha1.Agent
		key := types.NamespacedName{Name: step.AgentRef.Name, Namespace: w.Namespace}
		if err := r.Get(ctx, key, &agent); err != nil {
			return nil, nil, fmt.Errorf("get agent %s: %w", key, err)
		}
		if agent.Spec.Provider == "" || agent.Spec.Provider == foremanv1alpha1.AgentProviderLocal {
			kept = append(kept, step)
			continue
		}
		// Non-local provider; at least one gate is closed so we must
		// decide which one to name in the suppression message.
		switch {
		case !operatorAllows:
			suppressed = append(suppressed, fmt.Sprintf(
				"%s (provider=%s; operator --allow-cloud-providers=false)",
				agent.Name, agent.Spec.Provider,
			))
		case !workloadAllows:
			suppressed = append(suppressed, fmt.Sprintf(
				"%s (provider=%s; workload spec.allowCloudReviewers=false)",
				agent.Name, agent.Spec.Provider,
			))
		}
	}
	return kept, suppressed, nil
}

// chooseSteps returns the rendered PipelineStep slice the reconciler will
// emit as AgenticTasks, along with whether MaxTasks clipped the set.
//
// modeErr is non-nil when neither mode applies (intent-only Workload).
// We deliberately do NOT call into a planner in v0.1; the LLM-driven path
// returns a clear "deferred to v0.2" failure so the operator can see the
// CRD is reachable but the mode they wanted is not implemented yet.
func (r *WorkloadReconciler) chooseSteps(w *foremanv1alpha1.Workload) (steps []foremanv1alpha1.PipelineStep, truncated bool, modeErr error) {
	switch {
	case len(w.Spec.Pipeline) > 0:
		steps = append(steps, w.Spec.Pipeline...)
	case len(w.Spec.Issues) > 0 && w.Spec.CoderAgentRef != nil && w.Spec.VerifierAgentRef != nil:
		steps = synthesizeIssueBatch(w)
	case len(w.Spec.Issues) > 0:
		modeErr = fmt.Errorf("issues set but coderAgentRef and verifierAgentRef are required for the issue-batch shortcut")
	default:
		modeErr = fmt.Errorf("workload has no Pipeline or Issues; the LLM-driven planner is deferred to v0.2")
	}

	if w.Spec.MaxTasks > 0 && len(steps) > int(w.Spec.MaxTasks) {
		steps = steps[:w.Spec.MaxTasks]
		truncated = true
	}
	return steps, truncated, modeErr
}

// synthesizeIssueBatch turns Workload.spec.Issues into a flat PipelineStep
// slice. Each issue N produces:
//
//   - step "code-<N>" (kind issue-fix, agentRef = CoderAgentRef)
//   - step "verify-<N>" (kind verify, agentRef = VerifierAgentRef,
//     dependsOn [code-<N>])
//   - for each i in 0..len(ReviewerAgentRefs)-1:
//     step "review-<N>-<i>" (kind review, agentRef = ReviewerAgentRefs[i],
//     dependsOn [verify-<N>]). Parallel across i; the cascade-on-verdict
//     logic from #548 short-circuits these to Incomplete if verify-<N>
//     lands GATE-FAIL or GATE-ERROR rather than running the reviewer
//     loop against a branch the gate already rejected.
//
// Payload.Branch is "foreman/<workload-name>/issue-<N>" in all stages
// so each task clones the branch the coder produced (the cloneURL
// passthrough from #528 makes the gate hit the fork, not upstream).
//
// Including the workload name in the branch makes the branch unique
// across reruns on the same issue set: a second Workload on the same
// issues produces a distinct branch, the executor can push without
// fast-forward conflicts, and the empirical artifact (the foreman-
// authored branch) survives even when an earlier run already produced
// a branch on the same issue. See #573 for the motivating trace.
func synthesizeIssueBatch(w *foremanv1alpha1.Workload) []foremanv1alpha1.PipelineStep {
	tasksPerIssue := 2 + len(w.Spec.ReviewerAgentRefs)
	steps := make([]foremanv1alpha1.PipelineStep, 0, len(w.Spec.Issues)*tasksPerIssue)
	for _, n := range w.Spec.Issues {
		codeName := fmt.Sprintf("code-%d", n)
		verifyName := fmt.Sprintf("verify-%d", n)
		branch := fmt.Sprintf("foreman/%s/issue-%d", w.Name, n)
		steps = append(steps,
			foremanv1alpha1.PipelineStep{
				Name:     codeName,
				Kind:     foremanv1alpha1.AgenticTaskKindIssueFix,
				AgentRef: *w.Spec.CoderAgentRef,
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:   w.Spec.Repo,
					Issue:  n,
					Branch: branch,
				},
			},
			foremanv1alpha1.PipelineStep{
				Name:      verifyName,
				Kind:      foremanv1alpha1.AgenticTaskKindVerify,
				AgentRef:  *w.Spec.VerifierAgentRef,
				DependsOn: []string{codeName},
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:   w.Spec.Repo,
					Issue:  n,
					Branch: branch,
				},
			},
		)
		for i, reviewerRef := range w.Spec.ReviewerAgentRefs {
			steps = append(steps, foremanv1alpha1.PipelineStep{
				Name:      fmt.Sprintf("review-%d-%d", n, i),
				Kind:      foremanv1alpha1.AgenticTaskKindReview,
				AgentRef:  reviewerRef,
				DependsOn: []string{verifyName},
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:   w.Spec.Repo,
					Issue:  n,
					Branch: branch,
				},
			})
		}
	}
	return steps
}

// renderAndCreate creates one AgenticTask per PipelineStep, owner-ref'd to
// the Workload. Step-local DependsOn names are rewritten to absolute task
// names ("<workload>-<step>"). Already-existing tasks (idempotent re-run)
// are detected and skipped.
func (r *WorkloadReconciler) renderAndCreate(ctx context.Context, w *foremanv1alpha1.Workload, steps []foremanv1alpha1.PipelineStep) ([]corev1.ObjectReference, error) {
	log := logf.FromContext(ctx).WithName("workload").WithValues("workload", client.ObjectKeyFromObject(w))

	rendered := make([]*foremanv1alpha1.AgenticTask, 0, len(steps))
	refs := make([]corev1.ObjectReference, 0, len(steps))

	for _, step := range steps {
		taskName := absoluteTaskName(w.Name, step.Name)
		deps := make([]string, 0, len(step.DependsOn))
		for _, dep := range step.DependsOn {
			deps = append(deps, absoluteTaskName(w.Name, dep))
		}

		task := &foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{
				Name:      taskName,
				Namespace: w.Namespace,
				Labels: map[string]string{
					labelWorkload: w.Name,
					labelStep:     step.Name,
				},
			},
			Spec: foremanv1alpha1.AgenticTaskSpec{
				Kind:           step.Kind,
				AgentRef:       step.AgentRef.DeepCopy(),
				Payload:        *step.Payload.DeepCopy(),
				DependsOn:      deps,
				TimeoutSeconds: step.TimeoutSeconds,
				Priority:       step.Priority,
				GateProfile:    effectiveGateProfile(step, w).DeepCopy(),
			},
		}
		if err := controllerutil.SetControllerReference(w, task, r.Scheme); err != nil {
			return refs, fmt.Errorf("set owner ref on %q: %w", taskName, err)
		}
		rendered = append(rendered, task)
	}

	for _, task := range rendered {
		if err := r.Create(ctx, task); err != nil {
			if apierrors.IsAlreadyExists(err) {
				log.Info("AgenticTask already exists; skipping create", "task", task.Name)
			} else {
				return refs, fmt.Errorf("create AgenticTask %q: %w", task.Name, err)
			}
		}
		refs = append(refs, corev1.ObjectReference{
			APIVersion: foremanv1alpha1.GroupVersion.String(),
			Kind:       "AgenticTask",
			Namespace:  task.Namespace,
			Name:       task.Name,
		})
	}
	return refs, nil
}

// absoluteTaskName scopes a step name to its parent Workload to avoid
// collisions when two Workloads use the same step names. Returns the step
// name unmodified when it already starts with the workload prefix (idempotent
// across re-renders).
func absoluteTaskName(workloadName, stepName string) string {
	prefix := workloadName + "-"
	if strings.HasPrefix(stepName, prefix) {
		return stepName
	}
	return prefix + stepName
}

// effectiveGateProfile resolves the gate profile for a rendered task: the
// step's own profile when set, otherwise the Workload-level default. A nil
// result (both unset) leaves AgenticTaskSpec.GateProfile nil, which resolves
// to the "go" preset — the behavior before Workloads carried a profile.
func effectiveGateProfile(step foremanv1alpha1.PipelineStep, w *foremanv1alpha1.Workload) *foremanv1alpha1.GateProfile {
	if step.GateProfile != nil {
		return step.GateProfile
	}
	return w.Spec.GateProfile
}

// listChildren returns the AgenticTasks already owner-ref'd to this
// Workload, looked up by the labelWorkload selector. Stable ordering by
// name so callers can iterate predictably.
func (r *WorkloadReconciler) listChildren(ctx context.Context, w *foremanv1alpha1.Workload) ([]foremanv1alpha1.AgenticTask, error) {
	var list foremanv1alpha1.AgenticTaskList
	if err := r.List(ctx, &list,
		client.InNamespace(w.Namespace),
		client.MatchingLabels{labelWorkload: w.Name},
	); err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].Name < list.Items[j].Name
	})
	return list.Items, nil
}

// rollup computes the Workload's terminal-or-in-flight state from its
// children's phases AND verdicts and patches status. Triggered on every
// child phase change via the Owns() watch.
//
// Counts children in four buckets:
//   - succeeded: SucceededOnTarget() (Phase=Succeeded AND verdict in
//     {GO, GATE-PASS}) — produced a usable artifact
//   - incomplete: Phase=Succeeded but verdict in {INCOMPLETE, NO-GO,
//     GATE-FAIL, GATE-ERROR} — terminal without usable output
//   - failed: Phase=Failed
//   - inFlight: everything else (Pending / Scheduled / Running)
//
// Previous rollup counted Phase=Succeeded as a win regardless of
// verdict, which misreported the Memorial Day v5 batch as "12/12
// Succeeded" when 2 of those 12 ended INCOMPLETE and 2 ended GATE-FAIL.
// Fixes defilantech/LLMKube#541.
func (r *WorkloadReconciler) rollup(ctx context.Context, w *foremanv1alpha1.Workload, children []foremanv1alpha1.AgenticTask) (ctrl.Result, error) {
	var (
		succeeded, incomplete, failed, inFlight int32
	)
	for i := range children {
		switch {
		case children[i].SucceededOnTarget():
			succeeded++
		case children[i].Status.Phase == foremanv1alpha1.AgenticTaskPhaseSucceeded:
			// Terminal Phase=Succeeded but verdict isn't on-target.
			incomplete++
		case children[i].Status.Phase == foremanv1alpha1.AgenticTaskPhaseFailed:
			failed++
		default:
			inFlight++
		}
	}

	total := succeeded + incomplete + failed + inFlight
	patch := client.MergeFrom(w.DeepCopy())
	now := metav1.Now()
	w.Status.SucceededTasks = succeeded
	w.Status.FailedTasks = failed
	w.Status.IncompleteTasks = incomplete

	switch {
	case inFlight == 0 && failed == 0 && incomplete == 0:
		w.Status.Phase = foremanv1alpha1.WorkloadPhaseCompleted
		setCondition(&w.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCompleted,
			Status:             metav1.ConditionTrue,
			Reason:             "AllChildrenSucceeded",
			Message:            fmt.Sprintf("%d/%d child tasks on-target Succeeded", succeeded, total),
			LastTransitionTime: now,
		})
	case inFlight == 0 && (failed > 0 || incomplete > 0):
		// Any incomplete OR failed child rolls the Workload to Failed
		// terminal state. The condition message breaks out both counts
		// so the operator can distinguish "agent gave up" (incomplete)
		// from "executor errored" (failed).
		w.Status.Phase = foremanv1alpha1.WorkloadPhaseFailed
		reason := "ChildrenFailed"
		if failed == 0 {
			reason = "ChildrenIncomplete"
		}
		setCondition(&w.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCompleted,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            fmt.Sprintf("%d on-target, %d incomplete, %d failed (of %d)", succeeded, incomplete, failed, total),
			LastTransitionTime: now,
		})
	default:
		w.Status.Phase = foremanv1alpha1.WorkloadPhaseDispatched
		setCondition(&w.Status.Conditions, metav1.Condition{
			Type:               "Dispatched",
			Status:             metav1.ConditionTrue,
			Reason:             "ChildrenInFlight",
			Message:            fmt.Sprintf("%d in-flight, %d on-target, %d incomplete, %d failed", inFlight, succeeded, incomplete, failed),
			LastTransitionTime: now,
		})
	}

	if err := r.Status().Patch(ctx, w, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch workload status during rollup: %w", err)
	}
	return ctrl.Result{}, nil
}

// markPlanning patches phase=Planning the first time we touch the
// Workload. Idempotent: an Already-Planning workload stays planning until
// renderAndCreate flips it to Planned.
func (r *WorkloadReconciler) markPlanning(ctx context.Context, w *foremanv1alpha1.Workload) error {
	if w.Status.Phase == foremanv1alpha1.WorkloadPhasePlanning ||
		w.Status.Phase == foremanv1alpha1.WorkloadPhasePlanned ||
		w.Status.Phase == foremanv1alpha1.WorkloadPhaseDispatched {
		return nil
	}
	patch := client.MergeFrom(w.DeepCopy())
	w.Status.Phase = foremanv1alpha1.WorkloadPhasePlanning
	if w.Spec.PlannerModel != "" {
		w.Status.PlannerModel = w.Spec.PlannerModel
	} else {
		w.Status.PlannerModel = "stub:explicit-pipeline"
	}
	return r.Status().Patch(ctx, w, patch)
}

// markPlanned writes the post-render status: tasks list, Planned condition,
// optional Truncated condition, optional CloudReviewersSuppressed
// condition.
//
// renderErr handling distinguishes two failure shapes:
//
//   - A transient render failure (cache-lag NotFound, conflict, API
//     timeout) leaves Phase at Planning and surfaces a RenderError
//     condition; the returned error requeues and the next reconcile
//     retries because chooseSteps is deterministic and Create with
//     IsAlreadyExists is idempotent.
//   - An admission IsInvalid rejection (the validating webhook refused a
//     child AgenticTask CREATE, e.g. its agentRef names an Agent that does
//     not exist) is TERMINAL: retrying produces the same rejection every
//     reconcile, so we mark the Workload Failed instead of requeuing
//     forever. This mirrors the pre-webhook behavior where the task was
//     created and then marked terminally Failed/AgentNotFound, driving the
//     Workload to a terminal state. We reuse the AgentResolveFailed reason
//     already used for the cloud-provider resolve terminal path.
func (r *WorkloadReconciler) markPlanned(
	ctx context.Context,
	w *foremanv1alpha1.Workload,
	created []corev1.ObjectReference,
	truncated bool,
	suppressed []string,
	renderErr error,
) (ctrl.Result, error) {
	// Terminal admission rejection: a requeue can never succeed, so fail
	// the Workload rather than looping. NotFound / conflict / timeout
	// render errors fall through to the requeue path below.
	if renderErr != nil && apierrors.IsInvalid(renderErr) {
		return r.failWorkload(ctx, w, "AgentResolveFailed",
			fmt.Sprintf("AgenticTask creation rejected by admission webhook (terminal): %s", renderErr.Error()))
	}

	patch := client.MergeFrom(w.DeepCopy())
	now := metav1.Now()

	w.Status.Tasks = created

	if renderErr != nil {
		setCondition(&w.Status.Conditions, metav1.Condition{
			Type:               conditionTypePlanned,
			Status:             metav1.ConditionFalse,
			Reason:             "RenderError",
			Message:            renderErr.Error(),
			LastTransitionTime: now,
		})
	} else {
		w.Status.Phase = foremanv1alpha1.WorkloadPhasePlanned
		setCondition(&w.Status.Conditions, metav1.Condition{
			Type:               conditionTypePlanned,
			Status:             metav1.ConditionTrue,
			Reason:             "PlannerSucceeded",
			Message:            fmt.Sprintf("emitted %d AgenticTask(s)", len(created)),
			LastTransitionTime: now,
		})
	}

	if truncated {
		setCondition(&w.Status.Conditions, metav1.Condition{
			Type:               conditionTypeTruncated,
			Status:             metav1.ConditionTrue,
			Reason:             "MaxTasksCap",
			Message:            fmt.Sprintf("MaxTasks=%d clipped the rendered set", w.Spec.MaxTasks),
			LastTransitionTime: now,
		})
	}

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
		return ctrl.Result{}, fmt.Errorf("patch workload status: %w", err)
	}
	return ctrl.Result{}, renderErr
}

// failWorkload marks the Workload Failed with a reason + message. Used for
// the intent-only / no-planner case in v0.1.
func (r *WorkloadReconciler) failWorkload(ctx context.Context, w *foremanv1alpha1.Workload, reason, message string) (ctrl.Result, error) {
	patch := client.MergeFrom(w.DeepCopy())
	now := metav1.Now()
	w.Status.Phase = foremanv1alpha1.WorkloadPhaseFailed
	setCondition(&w.Status.Conditions, metav1.Condition{
		Type:               conditionTypePlanned,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
	return ctrl.Result{}, r.Status().Patch(ctx, w, patch)
}

// SetupWithManager wires the reconciler. Owns(AgenticTask) re-queues us on
// child status changes for rollup.
func (r *WorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&foremanv1alpha1.Workload{}).
		Owns(&foremanv1alpha1.AgenticTask{}).
		Named("workload").
		Complete(r)
}
