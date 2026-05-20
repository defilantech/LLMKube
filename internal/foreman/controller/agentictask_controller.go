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
)

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

	// The scheduler only acts on Pending. Every other phase is the
	// FleetAgent's domain.
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
	required, err := r.effectiveRequiredCapability(ctx, &task)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.failTask(ctx, &task, "AgentNotFound",
				fmt.Sprintf("Agent %q not found in namespace %q", task.Spec.AgentRef.Name, task.Namespace))
		}
		return ctrl.Result{}, err
	}

	// Find a FleetNode that satisfies the effective RequiredCapability.
	nodeName, err := r.firstFitNode(ctx, required)
	if err != nil {
		return ctrl.Result{}, err
	}
	if nodeName == "" {
		log.Info("no FleetNode matches; will retry", "task", task.Name)
		return ctrl.Result{RequeueAfter: requeueNoFit}, nil
	}

	return r.scheduleToNode(ctx, &task, nodeName)
}

// effectiveRequiredCapability returns the capability the scheduler should
// match against. When spec.agentRef is set the Agent's capability wins;
// otherwise the task's own. NotFound on AgentRef is propagated so the
// caller can fail the task with a clear reason.
func (r *AgenticTaskReconciler) effectiveRequiredCapability(ctx context.Context, task *foremanv1alpha1.AgenticTask) (foremanv1alpha1.RequiredCapability, error) {
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name == "" {
		return task.Spec.RequiredCapability, nil
	}
	var agent foremanv1alpha1.Agent
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Spec.AgentRef.Name}
	if err := r.Get(ctx, key, &agent); err != nil {
		return foremanv1alpha1.RequiredCapability{}, err
	}
	return agent.Spec.RequiredCapability, nil
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

// cascadeFailIfDepFailed returns a non-empty message if any dependency is
// already Failed; the caller fails the task with that message.
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
	}
	return "", nil
}

// allDepsSucceeded returns true only when every dependency exists in the
// same namespace AND is in phase=Succeeded.
func (r *AgenticTaskReconciler) allDepsSucceeded(ctx context.Context, task *foremanv1alpha1.AgenticTask) (bool, error) {
	for _, depName := range task.Spec.DependsOn {
		var dep foremanv1alpha1.AgenticTask
		if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: depName}, &dep); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil // wait for the dep to appear
			}
			return false, err
		}
		if dep.Status.Phase != foremanv1alpha1.AgenticTaskPhaseSucceeded {
			return false, nil
		}
	}
	return true, nil
}

// firstFitNode picks the alphabetically-first Ready FleetNode whose
// advertised capability satisfies the effective RequiredCapability and
// that is not already running another task.
func (r *AgenticTaskReconciler) firstFitNode(ctx context.Context, required foremanv1alpha1.RequiredCapability) (string, error) {
	var nodes foremanv1alpha1.FleetNodeList
	if err := r.List(ctx, &nodes); err != nil {
		return "", err
	}
	sort.Slice(nodes.Items, func(i, j int) bool {
		return nodes.Items[i].Name < nodes.Items[j].Name
	})
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Status.Phase != foremanv1alpha1.FleetNodePhaseReady {
			continue
		}
		if n.Status.CurrentTask != "" {
			continue // v0.1: one task per node
		}
		if !capabilitySatisfies(required, n) {
			continue
		}
		return n.Name, nil
	}
	return "", nil
}

// capabilitySatisfies returns true when the node's advertised capability
// meets every requirement the task declares. Unset requirements are
// unconstrained; an "any" accelerator matches everything.
func capabilitySatisfies(req foremanv1alpha1.RequiredCapability, n *foremanv1alpha1.FleetNode) bool {
	cap := n.Status.Capability

	if req.Accelerator != "" && req.Accelerator != foremanv1alpha1.AgenticTaskAccelerator("any") {
		if string(cap.Accelerator) != string(req.Accelerator) {
			return false
		}
	}
	if req.MinRAMGB > 0 && req.MinRAMGB > cap.AvailableRAMGB {
		return false
	}
	if req.MinContextTokens > 0 && req.MinContextTokens > cap.MaxContextTokens {
		return false
	}
	for k, v := range req.NodeSelector {
		if n.Labels[k] != v {
			return false
		}
	}
	return true
}

// scheduleToNode patches the task to phase=Scheduled with the chosen
// FleetNode set on status.assignedNode. The FleetAgent on that node
// picks it up via its watcher.
func (r *AgenticTaskReconciler) scheduleToNode(ctx context.Context, task *foremanv1alpha1.AgenticTask, nodeName string) (ctrl.Result, error) {
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
	if err := r.Status().Patch(ctx, task, patch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
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

// setCondition upserts a condition by type. Local to the controller
// because the watcher in pkg/foreman/agent has its own copy; promote to
// an internal helpers package if a third writer appears.
func setCondition(conds *[]metav1.Condition, c metav1.Condition) {
	for i, existing := range *conds {
		if existing.Type == c.Type {
			(*conds)[i] = c
			return
		}
	}
	*conds = append(*conds, c)
}

// SetupWithManager wires the reconciler. We also watch FleetNode so a
// node going Ready (or freeing up CurrentTask) re-enqueues every Pending
// task immediately rather than waiting for the requeue-after timer.
func (r *AgenticTaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&foremanv1alpha1.AgenticTask{}).
		Watches(&foremanv1alpha1.FleetNode{}, handler.EnqueueRequestsFromMapFunc(r.fleetNodeEnqueuesPending)).
		Named("agentictask").
		Complete(r)
}

// fleetNodeEnqueuesPending re-enqueues every Pending AgenticTask when a
// FleetNode event arrives. Cheap because the controller-runtime
// workqueue dedupes; expensive in the limit only when there are many
// Pending tasks, which is exactly when faster dispatch matters.
func (r *AgenticTaskReconciler) fleetNodeEnqueuesPending(ctx context.Context, _ client.Object) []ctrl.Request {
	var list foremanv1alpha1.AgenticTaskList
	if err := r.List(ctx, &list); err != nil {
		logf.FromContext(ctx).Error(err, "fleetnode-trigger list failed")
		return nil
	}
	requests := make([]ctrl.Request, 0, len(list.Items))
	for i := range list.Items {
		t := &list.Items[i]
		if t.Status.Phase != foremanv1alpha1.AgenticTaskPhasePending {
			continue
		}
		requests = append(requests, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: t.Namespace, Name: t.Name},
		})
	}
	return requests
}
