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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// FleetNodeReconciler watches FleetNode objects and marks them NotReady when
// their heartbeat goes stale. On a phase transition to NotReady it triggers
// re-queue of any AgenticTask whose status.assignedNode points at the
// stale node and whose phase is still Scheduled or Running.
//
// v0.1 / M0: stub. Heartbeat-staleness sweep lands in M1 (alongside the
// FleetAgent that writes the heartbeats in the first place).
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

	log.Info("reconciling FleetNode",
		"nodeName", node.Spec.NodeName,
		"phase", node.Status.Phase,
		"currentTask", node.Status.CurrentTask,
	)

	// M0 stub: no-op. Heartbeat staleness check lands in M1.
	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler into the controller-runtime manager.
func (r *FleetNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&foremanv1alpha1.FleetNode{}).
		Named("fleetnode").
		Complete(r)
}
