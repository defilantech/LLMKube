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

// AgentReconciler is the M3 stub for Agent CR lifecycle. It exists so
// the operator's scheme knows about the type and the binary registers a
// controller for it (events still flow); the M3 reconciler does not
// write status because v0.1 keeps exactly two condition writers across
// the system (the scheduler in agentictask_controller.go and the
// AgenticTaskWatcher in pkg/foreman/agent). Promoting validation results
// to a Ready / Validated condition is a v0.2 task.
//
// The reconciler logs Agent create/update events so a Helm install or a
// kubectl apply leaves a breadcrumb; that is the entire M3 contract.
type AgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agents/finalizers,verbs=update

// Reconcile is the entry point for Agent events. M3 stub: log and exit.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var agent foremanv1alpha1.Agent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.V(1).Info("reconciling Agent",
		"role", agent.Spec.Role,
		"model", agent.Spec.Model,
		"inferenceService", agent.Spec.InferenceServiceRef.Name,
		"toolCount", len(agent.Spec.Tools),
	)

	// M3 stub: no status mutation. The scheduler reads Agent.spec
	// directly when an AgenticTask references it; status fields are
	// reserved for v0.2.
	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler into the controller-runtime
// manager. We do not watch dependent resources in M3 because the stub
// has nothing to compute from them.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&foremanv1alpha1.Agent{}).
		Named("agent").
		Complete(r)
}
