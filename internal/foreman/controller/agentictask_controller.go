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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/audit"
)

// claimExpiriesAnnotation tracks how many times this task has been released
// back to Pending due to a stale or absent FleetNode. The 3-strike ladder
// (>= 2 prior expiries) terminal-fails the task to bound poison loops.
const claimExpiriesAnnotation = "foreman.llmkube.dev/claim-expiries"

// claimExpiryLimit is the maximum number of prior expiries before the task is
// terminal-failed. At count >= claimExpiryLimit this would be the
// (claimExpiryLimit+1)th expiry, which we refuse.
const claimExpiryLimit = 2

// AgenticTaskReconciler is the Foreman v0.1 scheduler. It watches
// AgenticTask resources and routes each Pending task to the first Ready
// FleetNode whose advertised capability satisfies the task's
// RequiredCapability (first-fit, alphabetical-by-name for determinism).
//
// The reconciler never touches the task while the FleetAgent owns it
// (Scheduled / Running / terminal phases). The agent owns the
// Scheduled -> Running -> Succeeded|Failed transitions; the scheduler
// only owns "no phase" -> Pending and Pending -> Scheduled. Cascade
// failure from a Failed dependency is the one exception: the scheduler
// short-circuits a downstream task with phase=Failed before it ever
// reaches Scheduled.
type AgenticTaskReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// AuditNamespace is where durable audit-record ConfigMaps are written.
	// Empty means each record lands in its task's own namespace.
	AuditNamespace string
}

// requeueNoFit is the backoff when no FleetNode satisfies the task's
// capability today. Long enough that a busy cluster does not get spammed,
// short enough that a node coming Ready triggers dispatch within seconds
// of the next reconcile (the FleetNode watch also re-enqueues directly).
const requeueNoFit = 10 * time.Second

// requeueWaitingForDeps is the backoff while at least one dependency is
// still pre-terminal.
const requeueWaitingForDeps = 10 * time.Second

// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agentictasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agentictasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agentictasks/finalizers,verbs=update
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=fleetnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=modelprofiles,verbs=get;list;watch

// Reconcile drives a single AgenticTask toward Scheduled.
func (r *AgenticTaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var task foremanv1alpha1.AgenticTask
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.V(1).Info("reconciling AgenticTask",
		"kind", task.Spec.Kind,
		"phase", task.Status.Phase,
		"assignedNode", task.Status.AssignedNode,
	)

	// Normalize an empty phase to Pending. We do this on a fresh-from-
	// the-API view, so the next reconcile sees Pending and falls into
	// the scheduling branch.
	if task.Status.Phase == "" {
		return r.setInitialPending(ctx, &task)
	}

	// Terminal phases are done; record the durable audit entry once
	// (best-effort: a failed audit write must not wedge reconciliation),
	// then nothing left to do.
	if task.Status.Phase == foremanv1alpha1.AgenticTaskPhaseSucceeded ||
		task.Status.Phase == foremanv1alpha1.AgenticTaskPhaseFailed {
		if err := audit.RecordTerminal(ctx, r.Client, &task, r.AuditNamespace, log); err != nil {
			log.Error(err, "audit: failed to record terminal run (continuing)")
		}
		// Release the node reservation so the scheduler can dispatch the next
		// task there. Guarded on taskKey, so a node already reserved for a
		// different task is untouched.
		if err := r.clearNodeCurrentTask(ctx, task.Status.AssignedNode, taskKey(&task)); err != nil {
			log.Error(err, "failed to release node reservation on terminal task",
				"node", task.Status.AssignedNode, "task", task.Name)
		}
		return ctrl.Result{}, nil
	}

	// In-flight phases (Scheduled / Running / Verifying) with an AssignedNode
	// need claim-expiry checking: if the node is gone or stale the task must
	// be released back to Pending so the scheduler can re-dispatch it.
	if task.Status.Phase != foremanv1alpha1.AgenticTaskPhasePending &&
		task.Status.AssignedNode != "" {
		return r.checkClaimExpiry(ctx, &task)
	}

	// For non-Pending phases with no assigned node (unexpected but safe to
	// ignore), and for phases not handled above, do nothing.
	if task.Status.Phase != foremanv1alpha1.AgenticTaskPhasePending {
		return ctrl.Result{}, nil
	}

	// Cascade-fail if any dependency has Failed.
	if cascadeMsg, err := r.cascadeFailIfDepFailed(ctx, &task); err != nil {
		return ctrl.Result{}, err
	} else if cascadeMsg != "" {
		return r.failTask(ctx, &task, "UpstreamFailed", cascadeMsg)
	}

	// Wait if any dependency is still pre-terminal.
	allSucceeded, err := r.allDepsSucceeded(ctx, &task)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !allSucceeded {
		return ctrl.Result{RequeueAfter: requeueWaitingForDeps}, nil
	}

	// Resolve the effective RequiredCapability. When spec.agentRef is set
	// it wins: we look up the Agent and use its capability. The task's own
	// spec.requiredCapability is ignored in that path; that is the locked
	// M3 contract. An Agent that does not exist fails the task fast.
	required, requiredModel, jobMode, err := r.effectiveRequiredCapability(ctx, &task)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.failTask(ctx, &task, "AgentNotFound",
				fmt.Sprintf("Agent %q not found in namespace %q", task.Spec.AgentRef.Name, task.Namespace))
		}
		return ctrl.Result{}, err
	}

	// Find a Ready FleetNode that satisfies the effective RequiredCapability
	// and atomically reserve it. The reservation (stamping the node's
	// CurrentTask) is what enforces one-task-per-node and spreads work across
	// the fleet; without it every task funnels onto the first node. See #977.
	nodeName, err := r.reserveFirstFitNode(ctx, &task, required, requiredModel, jobMode)
	if err != nil {
		return ctrl.Result{}, err
	}
	if nodeName == "" {
		log.Info("no free FleetNode matches; will retry", "task", task.Name)
		return ctrl.Result{RequeueAfter: requeueNoFit}, nil
	}

	if err := r.scheduleToNode(ctx, &task, nodeName); err != nil {
		// The node is reserved but the task never reached Scheduled. Release the
		// reservation so the node is not wedged busy with a task it never ran.
		if clearErr := r.clearNodeCurrentTask(ctx, nodeName, taskKey(&task)); clearErr != nil {
			log.Error(clearErr, "failed to release reservation after schedule error",
				"node", nodeName, "task", task.Name)
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// effectiveRequiredCapability returns the capability the scheduler should
// match against. When spec.agentRef is set the Agent's capability wins;
// otherwise the task's own. NotFound on AgentRef is propagated so the
// caller can fail the task with a clear reason.
//
// The trailing bool reports whether the referenced Agent runs in Job mode
// (execution.mode: Job). In Job mode the model is remote and the agent
// loop runs in an ephemeral Job, so the claiming node only needs the
// role/nodeSelector: the scheduler relaxes the model-binding capability
// gates. The no-AgentRef path is never Job mode (false). See #620.
func (r *AgenticTaskReconciler) effectiveRequiredCapability(ctx context.Context, task *foremanv1alpha1.AgenticTask) (foremanv1alpha1.RequiredCapability, string, bool, error) {
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name == "" {
		return task.Spec.RequiredCapability, "", false, nil
	}
	var agent foremanv1alpha1.Agent
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Spec.AgentRef.Name}
	if err := r.Get(ctx, key, &agent); err != nil {
		return foremanv1alpha1.RequiredCapability{}, "", false, err
	}
	jobMode := agent.Spec.Execution != nil && agent.Spec.Execution.Mode == foremanv1alpha1.ExecutionModeJob
	return agent.Spec.RequiredCapability, agentModelIdentity(&agent), jobMode, nil
}

// agentModelIdentity returns the model name used to test installedModels
// membership for RequiresModelInstalled scheduling. Prefers the explicit
// spec.model; falls back to the InferenceService reference name (which,
// in single-model fleets, matches the advertised installedModels entry).
func agentModelIdentity(agent *foremanv1alpha1.Agent) string {
	if agent.Spec.Model != "" {
		return agent.Spec.Model
	}
	return agent.Spec.InferenceServiceRef.Name
}

// setInitialPending writes phase=Pending the first time we see the task.
// The status patch triggers a fresh reconcile via the controller's
// For(AgenticTask) watch, so we do not need an explicit requeue (avoiding
// the deprecated Result.Requeue boolean).
func (r *AgenticTaskReconciler) setInitialPending(ctx context.Context, task *foremanv1alpha1.AgenticTask) (ctrl.Result, error) {
	patch := client.MergeFrom(task.DeepCopy())
	task.Status.Phase = foremanv1alpha1.AgenticTaskPhasePending
	if err := r.Status().Patch(ctx, task, patch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// cascadeFailIfDepFailed returns a non-empty message if any dependency
// is terminal-without-success; the caller fails the task with that
// message.
//
// "Terminal without success" means either:
//   - Phase=Failed (executor errored out), OR
//   - Phase=Succeeded but verdict in {INCOMPLETE, NO-GO, GATE-FAIL,
//     GATE-ERROR}. The dep is done, but it did not produce a usable
//     artifact. A downstream verify task should not run against a
//     branch the coder never pushed; a downstream review task should
//     not run against a coder verdict that already declined.
//
// Previously this gated on Phase=Failed only, which leaked INCOMPLETE
// coder tasks through to their downstream verifiers and made the
// downstream task fail GATE-FAIL on a clone-of-nonexistent-branch
// (the wrong reason). Fixes defilantech/LLMKube#541.
func (r *AgenticTaskReconciler) cascadeFailIfDepFailed(ctx context.Context, task *foremanv1alpha1.AgenticTask) (string, error) {
	for _, depName := range task.Spec.DependsOn {
		var dep foremanv1alpha1.AgenticTask
		if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: depName}, &dep); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", err
		}
		if dep.Status.Phase == foremanv1alpha1.AgenticTaskPhaseFailed {
			return fmt.Sprintf("dependency %q failed; cascade-failing", depName), nil
		}
		// Phase=Succeeded but verdict not on-target = terminal without
		// usable output. Cascade-fail so dependents don't run against
		// a nonexistent artifact.
		if dep.Status.Phase == foremanv1alpha1.AgenticTaskPhaseSucceeded && !dep.SucceededOnTarget() {
			return fmt.Sprintf("dependency %q ended with verdict=%s (not on-target); cascade-failing",
				depName, dep.Status.Verdict), nil
		}
	}
	return "", nil
}

// allDepsSucceeded returns true only when every dependency exists in
// the same namespace AND is on-target (Phase=Succeeded AND verdict in
// {GO, GATE-PASS}).
//
// Previously gated on Phase=Succeeded alone, allowing INCOMPLETE /
// GATE-FAIL deps to unblock dependents. Fixes defilantech/LLMKube#541.
func (r *AgenticTaskReconciler) allDepsSucceeded(ctx context.Context, task *foremanv1alpha1.AgenticTask) (bool, error) {
	for _, depName := range task.Spec.DependsOn {
		var dep foremanv1alpha1.AgenticTask
		if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: depName}, &dep); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil // wait for the dep to appear
			}
			return false, err
		}
		if !dep.SucceededOnTarget() {
			return false, nil
		}
	}
	return true, nil
}

// reserveFirstFitNode picks the alphabetically-first Ready FleetNode whose
// advertised capability satisfies the effective RequiredCapability and that is
// not already running another task, and atomically reserves it by stamping the
// node's Status.CurrentTask. It returns the reserved node's name, or "" when no
// eligible node is currently free.
//
// Reservation is what makes one-task-per-node spread work across the fleet. The
// node scan reads FleetNodes from a cache that lags writes, so two back-to-back
// scheduling reconciles can both observe the same node as free. The reservation
// is an optimistic-lock status patch, so at most one wins; the loser (Conflict,
// or a node already reserved for a live task) falls through to the next
// candidate. Without this, every task funnels onto one node. See #977.
func (r *AgenticTaskReconciler) reserveFirstFitNode(ctx context.Context, task *foremanv1alpha1.AgenticTask, required foremanv1alpha1.RequiredCapability, requiredModel string, jobMode bool) (string, error) {
	var nodes foremanv1alpha1.FleetNodeList
	if err := r.List(ctx, &nodes); err != nil {
		return "", err
	}
	sort.Slice(nodes.Items, func(i, j int) bool {
		return nodes.Items[i].Name < nodes.Items[j].Name
	})
	now := time.Now()
	key := taskKey(task)
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if !nodeSchedulable(n, now) {
			continue
		}
		if !capabilitySatisfies(required, requiredModel, n, jobMode) {
			continue
		}
		reserved, err := r.reserveNode(ctx, n, key)
		if err != nil {
			return "", err
		}
		if reserved {
			return n.Name, nil
		}
		// Node was busy with a live task, or another reconcile reserved it
		// first; try the next candidate.
	}
	return "", nil
}

// reserveNode stamps node.Status.CurrentTask with taskKey via an optimistic-
// lock status patch, claiming the node for a single task. It returns false
// (try the next node) when the node is already running another live task or
// when a concurrent writer won the node first (Conflict).
//
// A CurrentTask that points at a task which no longer exists or has reached a
// terminal phase is stale: the node is treated as free and re-reserved. That
// self-heals a reservation leaked by a task deleted mid-flight, so a missed
// clear cannot wedge a node busy forever.
func (r *AgenticTaskReconciler) reserveNode(ctx context.Context, node *foremanv1alpha1.FleetNode, taskKey string) (bool, error) {
	if cur := node.Status.CurrentTask; cur != "" && cur != taskKey {
		live, err := r.taskIsLive(ctx, cur)
		if err != nil {
			return false, err
		}
		if live {
			return false, nil // genuinely busy
		}
	}
	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	node.Status.CurrentTask = taskKey
	if err := r.Status().Patch(ctx, node, patch); err != nil {
		if apierrors.IsConflict(err) {
			return false, nil // another reconcile reserved it first
		}
		return false, err
	}
	return true, nil
}

// taskIsLive reports whether the namespaced-name key refers to an AgenticTask
// that still exists and has not reached a terminal phase. A missing or terminal
// task is not live, so a node whose CurrentTask points at it may be reclaimed.
func (r *AgenticTaskReconciler) taskIsLive(ctx context.Context, key string) (bool, error) {
	ns, name, ok := splitNamespacedName(key)
	if !ok {
		return false, nil // unparseable key: treat as not live so the node frees
	}
	var t foremanv1alpha1.AgenticTask
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &t); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return t.Status.Phase != foremanv1alpha1.AgenticTaskPhaseSucceeded &&
		t.Status.Phase != foremanv1alpha1.AgenticTaskPhaseFailed, nil
}

// clearNodeCurrentTask releases the named FleetNode's reservation, but only if
// it still points at taskKey, so a node already re-reserved for a different
// task is left untouched. A missing node (or empty name) is a no-op.
func (r *AgenticTaskReconciler) clearNodeCurrentTask(ctx context.Context, nodeName, taskKey string) error {
	if nodeName == "" {
		return nil
	}
	var node foremanv1alpha1.FleetNode
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return client.IgnoreNotFound(err)
	}
	if node.Status.CurrentTask != taskKey {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Status.CurrentTask = ""
	return r.Status().Patch(ctx, &node, patch)
}

// taskKey is the "namespace/name" string stored in FleetNode.Status.CurrentTask.
func taskKey(task *foremanv1alpha1.AgenticTask) string {
	return types.NamespacedName{Namespace: task.Namespace, Name: task.Name}.String()
}

// splitNamespacedName parses the "namespace/name" form written by taskKey.
func splitNamespacedName(key string) (namespace, name string, ok bool) {
	i := strings.IndexByte(key, '/')
	if i < 0 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}

// nodeSchedulable reports whether the scheduler may dispatch to a node. It
// must read Phase=Ready AND have a fresh heartbeat: the FleetNodeReconciler
// flips a dead node to NotReady, but that is level-triggered and may lag a
// heartbeat. Re-checking staleness here (defense-in-depth) prevents
// dispatching a task into a node whose agent has gone dark but whose phase
// has not yet been reconciled. See defilantech/LLMKube#627.
func nodeSchedulable(n *foremanv1alpha1.FleetNode, now time.Time) bool {
	if n.Status.Phase != foremanv1alpha1.FleetNodePhaseReady {
		return false
	}
	return !n.HeartbeatStale(now)
}

// capabilitySatisfies returns true when the node's advertised capability
// meets every requirement the task declares. Unset requirements are
// unconstrained; an "any" accelerator matches everything.
//
// When jobMode is true the agent loop runs in an ephemeral Job and the
// model is a remote (HTTP) dependency, so the claiming node does not host
// the model: the accelerator, RequiresModelInstalled, minRAMGB, and
// minContextTokens gates (all of which bind a node to a locally-resident
// model) are skipped. nodeSelector and roles still apply, since they
// constrain where the Job's claiming node may live. See #620.
func capabilitySatisfies(req foremanv1alpha1.RequiredCapability, requiredModel string, n *foremanv1alpha1.FleetNode, jobMode bool) bool {
	cap := n.Status.Capability

	if !jobMode {
		if req.Accelerator != "" && req.Accelerator != foremanv1alpha1.AgenticTaskAccelerator("any") {
			if string(cap.Accelerator) != string(req.Accelerator) {
				return false
			}
		}
		if req.RequiresModelInstalled {
			// Warm-driver path: the Agent's model must already be resident on
			// the node, and the minRAMGB gate is bypassed (the loaded model
			// has already paid the RAM cost; the loop adds ~0). An empty
			// requiredModel is a misconfiguration we cannot confirm, so it
			// fails the match rather than silently bypassing the gate.
			// See defilantech/LLMKube#579.
			if requiredModel == "" || !slices.Contains(cap.InstalledModels, requiredModel) {
				return false
			}
		} else if req.MinRAMGB > 0 && req.MinRAMGB > cap.AvailableRAMGB {
			return false
		}
		if req.MinContextTokens > 0 && req.MinContextTokens > cap.MaxContextTokens {
			return false
		}
	}
	for k, v := range req.NodeSelector {
		if n.Labels[k] != v {
			return false
		}
	}
	if len(req.Roles) > 0 {
		have := make(map[string]struct{}, len(n.Spec.Roles))
		for _, r := range n.Spec.Roles {
			have[r] = struct{}{}
		}
		for _, want := range req.Roles {
			if _, ok := have[want]; !ok {
				return false
			}
		}
	}
	return true
}

// scheduleToNode patches the task to phase=Scheduled with the chosen
// FleetNode set on status.assignedNode. The FleetAgent on that node
// picks it up via its watcher.
func (r *AgenticTaskReconciler) scheduleToNode(ctx context.Context, task *foremanv1alpha1.AgenticTask, nodeName string) error {
	patch := client.MergeFrom(task.DeepCopy())
	now := metav1.Now()
	task.Status.Phase = foremanv1alpha1.AgenticTaskPhaseScheduled
	task.Status.AssignedNode = nodeName
	setCondition(&task.Status.Conditions, metav1.Condition{
		Type:               "Scheduled",
		Status:             metav1.ConditionTrue,
		Reason:             "FleetNodeAssigned",
		Message:            fmt.Sprintf("scheduled to FleetNode %q", nodeName),
		LastTransitionTime: now,
	})
	return r.Status().Patch(ctx, task, patch)
}

// failTask cascade-fails a Pending task before it reaches an agent.
func (r *AgenticTaskReconciler) failTask(ctx context.Context, task *foremanv1alpha1.AgenticTask, reason, message string) (ctrl.Result, error) {
	patch := client.MergeFrom(task.DeepCopy())
	now := metav1.Now()
	task.Status.Phase = foremanv1alpha1.AgenticTaskPhaseFailed
	task.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictIncomplete
	task.Status.FinishedAt = &now
	setCondition(&task.Status.Conditions, metav1.Condition{
		Type:               "Failed",
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
	return ctrl.Result{}, r.Status().Patch(ctx, task, patch)
}

// checkClaimExpiry inspects the FleetNode named by task.status.assignedNode.
// If the node is absent or its heartbeat is stale the task is either
// released back to Pending (3-strike ladder: < claimExpiryLimit prior
// expiries) or terminal-failed (>= claimExpiryLimit prior expiries).
// If the node is fresh the task is left untouched and a requeue is scheduled
// at FleetNodeHeartbeatTimeout/2 so staleness is caught promptly without
// relying solely on events.
//
// Counter ordering: the metadata annotation (counter) is updated BEFORE the
// status patch so that a crash between the two errs toward counting more
// expiries rather than fewer, bounding poison loops conservatively.
func (r *AgenticTaskReconciler) checkClaimExpiry(ctx context.Context, task *foremanv1alpha1.AgenticTask) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var node foremanv1alpha1.FleetNode
	nodeNotFound := false
	lastHeartbeatMsg := ""

	if err := r.Get(ctx, types.NamespacedName{Name: task.Status.AssignedNode}, &node); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get FleetNode %s: %w", task.Status.AssignedNode, err)
		}
		nodeNotFound = true
		lastHeartbeatMsg = "FleetNode not found"
	}

	// Node is present and heartbeat is fresh: nothing to do yet.
	if !nodeNotFound && !node.HeartbeatStale(time.Now()) {
		return ctrl.Result{RequeueAfter: foremanv1alpha1.FleetNodeHeartbeatTimeout / 2}, nil
	}

	if !nodeNotFound {
		// Build the last-heartbeat context for the condition message.
		if node.Status.LastHeartbeatTime != nil {
			lastHeartbeatMsg = fmt.Sprintf("last heartbeat %s", node.Status.LastHeartbeatTime.Format(time.RFC3339))
		} else {
			lastHeartbeatMsg = "no heartbeat recorded"
		}
	}

	// Read the prior expiry count from the annotation.
	prior := 0
	if raw, ok := task.Annotations[claimExpiriesAnnotation]; ok {
		if n, err := strconv.Atoi(raw); err == nil {
			prior = n
		}
	}

	nodeName := task.Status.AssignedNode

	if prior >= claimExpiryLimit {
		// 3-strike limit reached: terminal-fail.
		log.Info("claim expiry limit reached; terminal-failing task",
			"task", task.Name, "node", nodeName, "priorExpiries", prior)
		return r.terminalFailExpired(ctx, task, nodeName, prior)
	}

	// Release: increment counter first (crash-safe ordering), then release.
	log.Info("claim expired; releasing task to Pending",
		"task", task.Name, "node", nodeName, "priorExpiries", prior, "heartbeat", lastHeartbeatMsg)
	if err := r.incrementExpiryCounter(ctx, task, prior); err != nil {
		return ctrl.Result{}, err
	}
	return r.releaseExpiredClaim(ctx, task, nodeName, lastHeartbeatMsg)
}

// incrementExpiryCounter writes max(freshValue, snapshotValue)+1 into the
// claim-expiries annotation, where freshValue is read from the live object.
// Using the live value closes an informer-lag window: if a prior expiry
// landed between the reconcile snapshot and this write, the snapshot's
// counter would be stale and we would double-count the expiry on the next
// reconcile (or under-count after a crash). Taking the max ensures we
// never regress the counter regardless of which view is ahead.
//
// This is a metadata (non-status) Update, distinct from the status patch
// that follows. It must happen first so a crash between the two errs toward
// counting more expiries.
func (r *AgenticTaskReconciler) incrementExpiryCounter(ctx context.Context, task *foremanv1alpha1.AgenticTask, snapshotPrior int) error {
	// Re-fetch to get the current resourceVersion for the metadata patch.
	var current foremanv1alpha1.AgenticTask
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, &current); err != nil {
		return fmt.Errorf("re-fetch for expiry counter: %w", err)
	}
	// Read the freshly-fetched counter; fall back to 0 if absent/invalid.
	freshPrior := 0
	if raw, ok := current.Annotations[claimExpiriesAnnotation]; ok {
		if n, err := strconv.Atoi(raw); err == nil {
			freshPrior = n
		}
	}
	// Advance from whichever view is higher to avoid regressing the counter.
	base := freshPrior
	if snapshotPrior > base {
		base = snapshotPrior
	}
	patch := client.MergeFrom(current.DeepCopy())
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[claimExpiriesAnnotation] = strconv.Itoa(base + 1)
	return r.Patch(ctx, &current, patch)
}

// releaseExpiredClaim resets the task to Pending, clears the claim fields,
// and records a ClaimExpired condition.
//
// Guard: after the re-fetch the function bails out without patching if the
// live object has already moved to a terminal phase (Succeeded/Failed) or if
// its AssignedNode no longer matches the node we judged stale. Either
// condition means a concurrent terminal patch landed in the window between
// checkClaimExpiry's staleness decision and this write; yanking the task back
// to Pending in that case would undo legitimate agent progress.
func (r *AgenticTaskReconciler) releaseExpiredClaim(ctx context.Context, task *foremanv1alpha1.AgenticTask, nodeName, heartbeatMsg string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var current foremanv1alpha1.AgenticTask
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, &current); err != nil {
		return ctrl.Result{}, fmt.Errorf("re-fetch for claim release: %w", err)
	}
	if current.Status.Phase == foremanv1alpha1.AgenticTaskPhaseSucceeded ||
		current.Status.Phase == foremanv1alpha1.AgenticTaskPhaseFailed ||
		current.Status.AssignedNode != nodeName {
		log.Info("claim expiry superseded by concurrent state change; skipping release",
			"task", current.Name, "node", nodeName,
			"livePhase", current.Status.Phase, "liveAssignedNode", current.Status.AssignedNode)
		return ctrl.Result{}, nil
	}
	patch := client.MergeFrom(current.DeepCopy())
	now := metav1.Now()
	current.Status.Phase = foremanv1alpha1.AgenticTaskPhasePending
	current.Status.AssignedNode = ""
	current.Status.ClaimedAt = nil
	current.Status.StartedAt = nil
	setCondition(&current.Status.Conditions, metav1.Condition{
		Type:               "ClaimExpired",
		Status:             metav1.ConditionTrue,
		Reason:             "ClaimExpired",
		Message:            fmt.Sprintf("released from node %q: %s", nodeName, heartbeatMsg),
		LastTransitionTime: now,
	})
	if err := r.Status().Patch(ctx, &current, patch); err != nil {
		return ctrl.Result{}, err
	}
	// Free the node so the released task (or another) can be dispatched there.
	if err := r.clearNodeCurrentTask(ctx, nodeName, taskKey(&current)); err != nil {
		log.Error(err, "failed to release node reservation on claim expiry",
			"node", nodeName, "task", current.Name)
	}
	return ctrl.Result{}, nil
}

// terminalFailExpired marks a task Failed after it has exhausted the
// 3-strike expiry ladder.
//
// Guard: same as releaseExpiredClaim. If the live object is already terminal
// or has been reassigned away from the stale node, a concurrent patch already
// resolved the task; bail out without patching to avoid overwriting it.
func (r *AgenticTaskReconciler) terminalFailExpired(ctx context.Context, task *foremanv1alpha1.AgenticTask, nodeName string, priorExpiries int) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var current foremanv1alpha1.AgenticTask
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, &current); err != nil {
		return ctrl.Result{}, fmt.Errorf("re-fetch for expiry terminal-fail: %w", err)
	}
	if current.Status.Phase == foremanv1alpha1.AgenticTaskPhaseSucceeded ||
		current.Status.Phase == foremanv1alpha1.AgenticTaskPhaseFailed ||
		current.Status.AssignedNode != nodeName {
		log.Info("claim expiry superseded by concurrent state change; skipping terminal-fail",
			"task", current.Name, "node", nodeName,
			"livePhase", current.Status.Phase, "liveAssignedNode", current.Status.AssignedNode)
		return ctrl.Result{}, nil
	}
	patch := client.MergeFrom(current.DeepCopy())
	now := metav1.Now()
	current.Status.Phase = foremanv1alpha1.AgenticTaskPhaseFailed
	current.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictIncomplete
	current.Status.FailureReason = foremanv1alpha1.FailureInfrastructureError
	current.Status.FinishedAt = &now
	setCondition(&current.Status.Conditions, metav1.Condition{
		Type:   "Failed",
		Status: metav1.ConditionTrue,
		Reason: "ClaimExpiryLimitReached",
		Message: fmt.Sprintf("task on node %q expired %d time(s); terminal-failing to prevent poison loop",
			nodeName, priorExpiries+1),
		LastTransitionTime: now,
	})
	if err := r.Status().Patch(ctx, &current, patch); err != nil {
		return ctrl.Result{}, err
	}
	// The task is terminal-failed on this node; release the reservation.
	if err := r.clearNodeCurrentTask(ctx, nodeName, taskKey(&current)); err != nil {
		log.Error(err, "failed to release node reservation on expiry terminal-fail",
			"node", nodeName, "task", current.Name)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler. We also watch FleetNode so a
// node going Ready (or freeing up CurrentTask) re-enqueues every Pending
// task immediately rather than waiting for the requeue-after timer.
func (r *AgenticTaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&foremanv1alpha1.AgenticTask{}).
		Watches(&foremanv1alpha1.FleetNode{}, handler.EnqueueRequestsFromMapFunc(r.fleetNodeEnqueues)).
		Named("agentictask").
		Complete(r)
}

// fleetNodeEnqueues re-enqueues AgenticTasks when a FleetNode event arrives:
//   - Every Pending task (scheduling: a newly-Ready node may satisfy a waiting
//     task immediately rather than waiting for the requeue-after timer).
//   - Every in-flight task (Scheduled/Running/Verifying) whose AssignedNode
//     matches the changed node (claim expiry: a node going stale or deleted
//     must trigger the expiry check on its assigned task promptly rather than
//     waiting for the 45s backstop requeue).
//
// The workqueue dedupes so the worst-case cost is one reconcile per task per
// FleetNode event, which is acceptable at v0.1 task volumes.
func (r *AgenticTaskReconciler) fleetNodeEnqueues(ctx context.Context, obj client.Object) []ctrl.Request {
	var list foremanv1alpha1.AgenticTaskList
	if err := r.List(ctx, &list); err != nil {
		logf.FromContext(ctx).Error(err, "fleetnode-trigger list failed")
		return nil
	}
	changedNodeName := obj.GetName()
	requests := make([]ctrl.Request, 0, len(list.Items))
	seen := make(map[types.NamespacedName]struct{}, len(list.Items))
	for i := range list.Items {
		t := &list.Items[i]
		key := types.NamespacedName{Namespace: t.Namespace, Name: t.Name}
		switch t.Status.Phase {
		case foremanv1alpha1.AgenticTaskPhasePending:
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				requests = append(requests, ctrl.Request{NamespacedName: key})
			}
		case foremanv1alpha1.AgenticTaskPhaseScheduled,
			foremanv1alpha1.AgenticTaskPhaseRunning,
			foremanv1alpha1.AgenticTaskPhaseVerifying:
			if t.Status.AssignedNode == changedNodeName {
				if _, ok := seen[key]; !ok {
					seen[key] = struct{}{}
					requests = append(requests, ctrl.Request{NamespacedName: key})
				}
			}
		}
	}
	return requests
}
