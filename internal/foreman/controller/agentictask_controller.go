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

// AgenticTaskReconciler is the scheduler: it watches AgenticTask objects,
// matches Pending tasks to Ready FleetNodes by capability, writes the
// assignment back onto the task, and (later) chains a verify child task
// when an issue-fix succeeds.
//
// v0.1 / M0: this is a stub. It reads the task and logs the phase; no
// scheduling, no node matching. The real logic lands in M2.
type AgenticTaskReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agentictasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agentictasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agentictasks/finalizers,verbs=update
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=fleetnodes,verbs=get;list;watch

// Reconcile is the entry point for AgenticTask events.
func (r *AgenticTaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var task foremanv1alpha1.AgenticTask
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("reconciling AgenticTask",
		"kind", task.Spec.Kind,
		"phase", task.Status.Phase,
		"assignedNode", task.Status.AssignedNode,
	)

	// M0 stub: no-op. Scheduling logic lands in M2.
	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler into the controller-runtime manager.
func (r *AgenticTaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&foremanv1alpha1.AgenticTask{}).
		Named("agentictask").
		Complete(r)
}
