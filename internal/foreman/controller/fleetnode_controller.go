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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// FleetNodeReconciler watches FleetNode objects and marks them NotReady when
// their heartbeat goes stale, returning them to Ready when the heartbeat
// resumes. The scheduler reads status.phase (and independently re-checks
// heartbeat freshness) to decide eligibility.
type FleetNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=fleetnodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=fleetnodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=fleetnodes/finalizers,verbs=update

// Reconcile is the entry point for FleetNode events.
func (r *FleetNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var node foremanv1alpha1.FleetNode
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.V(1).Info("reconciling FleetNode",
		"nodeName", node.Spec.NodeName,
		"phase", node.Status.Phase,
		"currentTask", node.Status.CurrentTask,
	)

	// A node the agent is intentionally draining is the agent's domain;
	// the scheduler already treats Draining as ineligible. Don't fight it.
	if node.Status.Phase == foremanv1alpha1.FleetNodePhaseDraining {
		return ctrl.Result{RequeueAfter: foremanv1alpha1.FleetNodeHeartbeatTimeout}, nil
	}

	now := time.Now()
	stale := node.HeartbeatStale(now)

	desiredPhase := foremanv1alpha1.FleetNodePhaseReady
	cond := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "HeartbeatFresh",
		Message:            "FleetAgent heartbeat is current",
		LastTransitionTime: metav1.NewTime(now),
	}
	if stale {
		desiredPhase = foremanv1alpha1.FleetNodePhaseNotReady
		cond.Status = metav1.ConditionFalse
		cond.Reason = "HeartbeatStale"
		cond.Message = fmt.Sprintf("no heartbeat within %s", foremanv1alpha1.FleetNodeHeartbeatTimeout)
	}

	if node.Status.Phase != desiredPhase || !hasCondition(node.Status.Conditions, cond) {
		patch := client.MergeFrom(node.DeepCopy())
		node.Status.Phase = desiredPhase
		setCondition(&node.Status.Conditions, cond)
		if err := r.Status().Patch(ctx, &node, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Requeue so a node that goes stale between heartbeats is detected
	// without waiting for an external event.
	return ctrl.Result{RequeueAfter: foremanv1alpha1.FleetNodeHeartbeatTimeout}, nil
}

// hasCondition reports whether conds already holds a condition matching c
// on the fields the reconciler manages (type, status, reason). Lets us skip
// a no-op status patch when nothing meaningful changed, avoiding a churn of
// LastTransitionTime-only updates.
func hasCondition(conds []metav1.Condition, c metav1.Condition) bool {
	for i := range conds {
		if conds[i].Type == c.Type {
			return conds[i].Status == c.Status && conds[i].Reason == c.Reason
		}
	}
	return false
}

// SetupWithManager wires the reconciler into the controller-runtime manager.
func (r *FleetNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&foremanv1alpha1.FleetNode{}).
		Named("fleetnode").
		Complete(r)
}
