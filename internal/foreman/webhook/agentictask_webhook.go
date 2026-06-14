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

package webhook

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-foreman-llmkube-dev-v1alpha1-agentictask,mutating=false,failurePolicy=fail,sideEffects=None,groups=foreman.llmkube.dev,resources=agentictasks,verbs=create,versions=v1alpha1,name=vagentictask.foreman.llmkube.dev,admissionReviewVersions=v1

// AgenticTaskValidator validates AgenticTask CRs at admission. Its single
// invariant is referential integrity: a task that names an Agent via
// spec.agentRef must resolve to an Agent that exists in the task's
// namespace, because the executor reads exactly that
// (NativeAgentLoopExecutor.Execute does a same-namespace Get and fails
// AgentNotFound otherwise).
//
// Tasks that set NO agentRef (capability-only / requiredCapability.roles
// tasks, including the kind/stub-mode tasks the e2e suite creates) are
// always accepted: the agentRef field is optional and the M2 capability
// path is a first-class, valid shape.
type AgenticTaskValidator struct {
	// Client reads the referenced Agent. Required: a validator wired
	// without a client cannot resolve agentRef and would reject every
	// agent-referencing task.
	Client client.Client
}

var _ admission.Validator[*foremanv1alpha1.AgenticTask] = &AgenticTaskValidator{}

// SetupAgenticTaskWebhookWithManager registers the AgenticTask validating
// webhook with a reader backed by the manager's cache + client.
func SetupAgenticTaskWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &foremanv1alpha1.AgenticTask{}).
		WithValidator(&AgenticTaskValidator{Client: mgr.GetClient()}).
		Complete()
}

// ValidateCreate validates an AgenticTask on creation. Update + delete are
// no-ops: agentRef is effectively immutable in practice (the scheduler
// drives the lifecycle), and re-checking on every status patch would add
// an apiserver round-trip per reconcile for no safety gain.
func (v *AgenticTaskValidator) ValidateCreate(ctx context.Context, task *foremanv1alpha1.AgenticTask) (admission.Warnings, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("validating AgenticTask create", "name", task.Name, "namespace", task.Namespace)

	// No agentRef: capability-only task. Always valid; do not touch the
	// apiserver. This is the path the kind/stub e2e tasks (#521) take.
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name == "" {
		return nil, nil
	}

	var agent foremanv1alpha1.Agent
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Spec.AgentRef.Name}
	if err := v.Client.Get(ctx, key, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			fld := field.NewPath("spec", "agentRef", "name")
			return nil, apierrors.NewInvalid(
				foremanv1alpha1.GroupVersion.WithKind("AgenticTask").GroupKind(),
				task.Name,
				field.ErrorList{field.Invalid(fld, task.Spec.AgentRef.Name,
					fmt.Sprintf("Agent %q not found in namespace %q", key.Name, key.Namespace))},
			)
		}
		// A real apiserver error (not NotFound) is a system fault, not a
		// validation failure; bubble it so admission retries rather than
		// permanently rejecting a possibly-valid task.
		return nil, fmt.Errorf("resolve agentRef %s: %w", key, err)
	}
	return nil, nil
}

// ValidateUpdate is a no-op. See ValidateCreate for why.
func (v *AgenticTaskValidator) ValidateUpdate(_ context.Context, _, _ *foremanv1alpha1.AgenticTask) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete is a no-op: deleting a task is always allowed.
func (v *AgenticTaskValidator) ValidateDelete(_ context.Context, _ *foremanv1alpha1.AgenticTask) (admission.Warnings, error) {
	return nil, nil
}
