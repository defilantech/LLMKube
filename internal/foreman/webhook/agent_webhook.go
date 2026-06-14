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

// Package webhook holds the foreman-operator's admission webhooks. Each
// kind gets a controller-runtime CustomValidator that surfaces an
// invariant violation at `kubectl apply` time instead of letting the CR
// reach the executor and fail at dispatch.
//
// The validators are deliberately a mirror of the runtime enforcement in
// pkg/foreman/agent: the webhook must never reject a CR the executor would
// happily run, nor accept one that the executor would fail on. Where the
// two could drift, the runtime code is the source of truth and the
// validator is documented against the exact predicate it copies.
package webhook

import (
	"context"
	"reflect"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/tools/catalog"
)

// +kubebuilder:webhook:path=/validate-foreman-llmkube-dev-v1alpha1-agent,mutating=false,failurePolicy=fail,sideEffects=None,groups=foreman.llmkube.dev,resources=agents,verbs=create;update,versions=v1alpha1,name=vagent.foreman.llmkube.dev,admissionReviewVersions=v1

// AgentValidator validates Agent CRs at admission. It enforces the same
// invariants the NativeAgentLoopExecutor relies on at dispatch time, so a
// misconfigured Agent fails loud at apply instead of producing a confusing
// INCOMPLETE task later.
type AgentValidator struct{}

var _ admission.Validator[*foremanv1alpha1.Agent] = &AgentValidator{}

// SetupAgentWebhookWithManager registers the Agent validating webhook.
func SetupAgentWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &foremanv1alpha1.Agent{}).
		WithValidator(&AgentValidator{}).
		Complete()
}

// ValidateCreate validates an Agent on creation.
func (v *AgentValidator) ValidateCreate(ctx context.Context, agent *foremanv1alpha1.Agent) (admission.Warnings, error) {
	logf.FromContext(ctx).V(1).Info("validating Agent create", "name", agent.Name, "namespace", agent.Namespace)
	return nil, v.validate(agent)
}

// ValidateUpdate validates an Agent on update. We grandfather updates that
// do not touch the spec: a pre-existing Agent with an already-invalid spec
// must not be rejected on an unrelated status or metadata patch, since that
// would wedge any controller that patches it. Spec invariants are only
// re-checked when the spec actually changes. (None of the checks are
// transition-scoped, so when the spec changes we run the full create-time
// validation against the new spec.)
func (v *AgentValidator) ValidateUpdate(ctx context.Context, oldAgent, agent *foremanv1alpha1.Agent) (admission.Warnings, error) {
	log := logf.FromContext(ctx).V(1)
	if oldAgent != nil && reflect.DeepEqual(oldAgent.Spec, agent.Spec) {
		log.Info("skipping Agent update validation; spec unchanged", "name", agent.Name, "namespace", agent.Namespace)
		return nil, nil
	}
	log.Info("validating Agent update", "name", agent.Name, "namespace", agent.Namespace)
	return nil, v.validate(agent)
}

// ValidateDelete is a no-op: deleting an Agent is always allowed (any
// dangling AgenticTask reference is handled at execution time, not here).
func (v *AgentValidator) ValidateDelete(_ context.Context, _ *foremanv1alpha1.Agent) (admission.Warnings, error) {
	return nil, nil
}

// validate runs every Agent invariant and aggregates the failures into a
// single apierrors.Invalid so `kubectl apply` reports all problems at once
// rather than one-per-retry.
func (v *AgentValidator) validate(agent *foremanv1alpha1.Agent) error {
	specPath := field.NewPath("spec")

	errs := make(field.ErrorList, 0, len(agent.Spec.Tools)+2)
	errs = append(errs, validateAgentToolNames(agent, specPath.Child("tools"))...)
	errs = append(errs, validateAgentPromptShape(agent, specPath)...)

	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		foremanv1alpha1.GroupVersion.WithKind("Agent").GroupKind(),
		agent.Name, errs)
}

// validateAgentToolNames rejects any spec.tools entry that is not a
// registered tool name. The valid set is sourced from the dependency-free
// leaf catalog.CanonicalToolNames so it tracks the registry the
// foreman-agent builds (makeRegistryFactory) WITHOUT pulling the
// executor's dependency tree into the operator binary; the
// pkg/foreman/agent/tools drift test asserts the leaf list stays in sync
// with the real constructors. This matches the runtime Filter() check
// that fails an Agent whose whitelist names an unknown tool.
func validateAgentToolNames(agent *foremanv1alpha1.Agent, toolsPath *field.Path) field.ErrorList {
	var errs field.ErrorList
	canonical := catalog.CanonicalToolNames()
	valid := make(map[string]bool, len(canonical))
	for _, name := range canonical {
		valid[name] = true
	}
	for i, name := range agent.Spec.Tools {
		if !valid[name] {
			errs = append(errs, field.NotSupported(
				toolsPath.Index(i), name, canonical))
		}
	}
	return errs
}

// validateAgentPromptShape enforces the system-prompt invariant the
// executor relies on, split by whether the Agent is deterministic.
//
// The deterministic predicate is copied verbatim from
// pkg/foreman/agent.isDeterministicAgent: an Agent runs the model-free
// path when its provider is "" or "local" AND its InferenceServiceRef.Name
// is empty. (A cloud-proxy Agent is never deterministic.)
//
//   - Deterministic Agent: there is no LLM, so a non-empty SystemPrompt is
//     dead config the executor silently ignores; we reject it so the
//     mistake surfaces. The executor also requires at least one usable
//     (non-terminal) tool, mirrored below.
//   - LLM-driven Agent: SystemPrompt must be non-empty (the loop sends it
//     as the system message every turn; an empty prompt is almost always
//     an authoring mistake).
func validateAgentPromptShape(agent *foremanv1alpha1.Agent, specPath *field.Path) field.ErrorList {
	var errs field.ErrorList
	if isDeterministicAgent(agent) {
		if strings.TrimSpace(agent.Spec.SystemPrompt) != "" {
			errs = append(errs, field.Invalid(
				specPath.Child("systemPrompt"), agent.Spec.SystemPrompt,
				"deterministic Agent (empty inferenceServiceRef) runs no model; systemPrompt must be empty"))
		}
		// The deterministic executor dispatches the first non-terminal
		// tool (pickDeterministicTool): a tool that is non-empty and not
		// submit_result. An Agent with only submit_result (or only empty
		// strings) would fail at dispatch with "no non-terminal tool";
		// reject it here instead.
		if !hasUsableDeterministicTool(agent.Spec.Tools) {
			errs = append(errs, field.Invalid(
				specPath.Child("tools"), agent.Spec.Tools,
				"deterministic Agent (empty inferenceServiceRef) needs at least one non-terminal tool "+
					"(a tool other than submit_result, e.g. run_gate_job) to dispatch"))
		}
		return errs
	}

	// LLM-driven Agent.
	if strings.TrimSpace(agent.Spec.SystemPrompt) == "" {
		errs = append(errs, field.Required(
			specPath.Child("systemPrompt"),
			"LLM-driven Agent (inferenceServiceRef set) requires a non-empty systemPrompt"))
	}
	return errs
}

// isDeterministicAgent mirrors pkg/foreman/agent.isDeterministicAgent.
// Kept as a private copy rather than importing the agent package because
// the predicate is two field reads and importing pkg/foreman/agent into
// the webhook would pull the executor's full dependency tree (git, OAI
// client, repomap) into the operator binary for no benefit. The two
// implementations are asserted equivalent by the validator unit tests.
func isDeterministicAgent(agent *foremanv1alpha1.Agent) bool {
	if agent.Spec.Provider != "" && agent.Spec.Provider != foremanv1alpha1.AgentProviderLocal {
		return false
	}
	return agent.Spec.InferenceServiceRef.Name == ""
}

// hasUsableDeterministicTool mirrors pkg/foreman/agent.pickDeterministicTool:
// it returns true when at least one tool is non-empty and not the terminal
// submit_result tool, i.e. there is something the deterministic executor
// can actually dispatch.
func hasUsableDeterministicTool(toolNames []string) bool {
	for _, t := range toolNames {
		if t == "" || t == "submit_result" {
			continue
		}
		return true
	}
	return false
}
