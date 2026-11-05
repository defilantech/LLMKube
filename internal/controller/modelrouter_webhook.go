/*
Copyright 2026.

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
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-inference-llmkube-dev-v1alpha1-modelrouter,mutating=false,failurePolicy=fail,sideEffects=None,groups=inference.llmkube.dev,resources=modelrouters,verbs=create;update,versions=v1alpha1,name=vmodelrouter.inference.llmkube.dev,admissionReviewVersions=v1

// ModelRouterValidator validates ModelRouter CRs at admission. It promotes the
// honest-boundary checks the gateway reconciler accumulates (unsupported match,
// unsupported budget, invalid auth, invalid authorization, unsupported auditLog,
// unsafe sensitive route) from a post-reconcile GatewayReady=False status to an
// apply-time rejection, so a misconfigured router fails loud at `kubectl apply`
// with the same message instead of applying cleanly and silently doing nothing.
//
// It is a deliberate mirror of the reconciler enforcement: the validator calls
// the SAME pure (mr.Spec-only) check functions the gateway controller calls, so
// the webhook and the reconciler can never diverge. The reconciler keeps all its
// status checks (defense in depth, and the webhook can be disabled).
//
// Boundary: pure checks only. Checks that need a cluster lookup (a backend
// referencing a missing InferenceService; a backend that resolves to a non-local
// tier) live in resolveBackends and stay reconciler-only: a webhook cannot
// reliably do cross-object reads under all apiserver conditions. The webhook
// catches config-level mistakes; the reconciler catches live-cluster ones.
type ModelRouterValidator struct{}

var _ admission.Validator[*inferencev1alpha1.ModelRouter] = &ModelRouterValidator{}

// SetupModelRouterWebhookWithManager registers the ModelRouter validating webhook.
func SetupModelRouterWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &inferencev1alpha1.ModelRouter{}).
		WithValidator(&ModelRouterValidator{}).
		Complete()
}

// ValidateCreate validates a ModelRouter on creation.
func (v *ModelRouterValidator) ValidateCreate(ctx context.Context, mr *inferencev1alpha1.ModelRouter) (admission.Warnings, error) {
	logf.FromContext(ctx).V(1).Info("validating ModelRouter create", "name", mr.Name, "namespace", mr.Namespace)
	return balancingWarnings(mr), v.validate(mr)
}

// ValidateUpdate validates a ModelRouter on update. We grandfather updates that
// do not touch the spec: a pre-existing ModelRouter with an already-invalid spec
// must not be rejected on an unrelated status or metadata patch, since that
// would wedge any controller that patches it (and the gateway reconciler patches
// status every reconcile). Spec invariants are only re-checked when the spec
// actually changes; none of the checks are transition-scoped, so when the spec
// changes we run the full create-time validation against the new spec.
func (v *ModelRouterValidator) ValidateUpdate(ctx context.Context, oldMR, mr *inferencev1alpha1.ModelRouter) (admission.Warnings, error) {
	log := logf.FromContext(ctx).V(1)
	if oldMR != nil && reflect.DeepEqual(oldMR.Spec, mr.Spec) {
		log.Info("skipping ModelRouter update validation; spec unchanged", "name", mr.Name, "namespace", mr.Namespace)
		return nil, nil
	}
	log.Info("validating ModelRouter update", "name", mr.Name, "namespace", mr.Namespace)
	return balancingWarnings(mr), v.validate(mr)
}

// ValidateDelete is a no-op: deleting a ModelRouter is always allowed (owner-ref
// GC removes the generated resources).
func (v *ModelRouterValidator) ValidateDelete(_ context.Context, _ *inferencev1alpha1.ModelRouter) (admission.Warnings, error) {
	return nil, nil
}

// validate runs every ModelRouter invariant and aggregates the failures into a
// single apierrors.Invalid so `kubectl apply` reports all problems at once
// rather than one-per-retry. It always runs the static spec validation
// (validateModelRouter); the gateway-mode honest-boundary checks run ONLY when
// spec.dataPlane is Gateway, because they describe limits of the gateway data
// plane that do not apply to a Proxy-mode router.
func (v *ModelRouterValidator) validate(mr *inferencev1alpha1.ModelRouter) error {
	specPath := field.NewPath("spec")

	var errs field.ErrorList

	// Static spec validation (pure; same function the reconciler uses to set
	// the Validated condition). Each ModelRouterValidationError carries a
	// dotted JSONPath-style Field that we surface as the field.Error path.
	for _, e := range validateModelRouter(mr) {
		errs = append(errs, field.Invalid(field.NewPath(e.Field), nil, e.Message))
	}

	// Balancing rules apply to every data plane. They live here and not in
	// validateModelRouter, which also drives the reconciler's Validated
	// condition: an existing RoundRobin router with sessionAffinity must keep
	// serving, so the rule is enforced at admission only.
	errs = append(errs, balancingViolations(mr, specPath)...)

	// Gateway-mode honest-boundary checks. These mirror the reconciler's
	// fail-loud guards in ModelRouterGatewayReconciler.Reconcile, in the same
	// order, calling the SAME pure check functions. A Proxy-mode router is not
	// subject to them (the gateway data plane's limits do not apply).
	if mr.Spec.DataPlane == inferencev1alpha1.ModelRouterDataPlaneGateway {
		errs = append(errs, gatewayModeViolations(mr, specPath)...)
	}

	// AgentGateway-mode honest-boundary checks. These mirror the reconciler's
	// fail-loud guards in ModelRouterAgentGatewayReconciler.Reconcile, calling
	// the SAME agentgateway-flavored pure check functions. A Proxy- or
	// Gateway-mode router is not subject to them (the agentgateway data plane's
	// limits do not apply).
	if mr.Spec.DataPlane == inferencev1alpha1.ModelRouterDataPlaneAgentGateway {
		errs = append(errs, agentGatewayModeViolations(mr, specPath)...)
	}

	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		inferencev1alpha1.GroupVersion.WithKind("ModelRouter").GroupKind(),
		mr.Name, errs)
}

// gatewayModeViolations runs the gateway-mode honest-boundary checks and returns
// a field.Error per non-empty violation. All checks are pure (mr.Spec only) and
// already exist in this package; they are called directly so the webhook and the
// gateway reconciler can never diverge. ALL violations are collected (not just
// the first) so the user sees every problem in one apply.
func gatewayModeViolations(mr *inferencev1alpha1.ModelRouter, specPath *field.Path) field.ErrorList {
	var errs field.ErrorList

	if msg := unsupportedMatchMessage(mr); msg != "" {
		errs = append(errs, field.Invalid(specPath.Child("rules"), nil, msg))
	}
	if _, msg := unsupportedBudgetMessage(mr); msg != "" {
		errs = append(errs, field.Invalid(specPath.Child("policy", "budgets"), nil, msg))
	}
	if msg := invalidAuthMessage(mr); msg != "" {
		errs = append(errs, field.Invalid(specPath.Child("policy", "auth", "jwt"), nil, msg))
	}
	if _, msg := invalidAuthorizationMessage(mr); msg != "" {
		errs = append(errs, field.Invalid(specPath.Child("policy", "auth", "allowlists"), nil, msg))
	}
	if msg := unsupportedAuditLogMessage(mr); msg != "" {
		errs = append(errs, field.Invalid(specPath.Child("policy", "auditLog"), nil, msg))
	}
	if msg := unsafeSensitiveRouteMessage(mr); msg != "" {
		errs = append(errs, field.Invalid(specPath.Child("rules"), nil, msg))
	}

	return errs
}

// agentGatewayModeViolations runs the agentgateway-mode honest-boundary checks
// and returns a field.Error per non-empty violation. All checks are pure
// (mr.Spec only) and already exist in this package; they are called directly so
// the webhook and the agentgateway reconciler can never diverge. ALL violations
// are collected (not just the first) so the user sees every problem in one
// apply. A Proxy- or Gateway-mode router is not subject to these.
func agentGatewayModeViolations(mr *inferencev1alpha1.ModelRouter, specPath *field.Path) field.ErrorList {
	var errs field.ErrorList

	if mr.Spec.AgentGatewayRef == nil {
		errs = append(errs, field.Required(specPath.Child("agentGatewayRef"),
			"dataPlane is AgentGateway but spec.agentGatewayRef is unset; cannot attach a pool or route"))
	}
	if _, reason, msg := agentGatewayResolvePoolPickers(mr); reason != "" {
		errs = append(errs, field.Invalid(specPath.Child("backends"), nil, msg))
	}
	if msg := agentGatewayModelRoutingMessage(mr); msg != "" {
		errs = append(errs, field.Invalid(specPath, nil, msg))
	}
	if msg := agentGatewayUnsupportedMatchMessage(mr); msg != "" {
		errs = append(errs, field.Invalid(specPath.Child("rules"), nil, msg))
	}
	if reason, msg := agentGatewayUnsupportedBudgetMessage(mr); reason != "" {
		errs = append(errs, field.Invalid(specPath.Child("policy", "budgets"), nil, msg))
	}
	if msg := agentGatewayUnsupportedAuthMessage(mr); msg != "" {
		errs = append(errs, field.Invalid(specPath.Child("policy", "auth"), nil, msg))
	}
	if msg := agentGatewayUnsupportedAuditLogMessage(mr); msg != "" {
		errs = append(errs, field.Invalid(specPath.Child("policy", "auditLog"), nil, msg))
	}
	if msg := unsafeSensitiveRouteMessage(mr); msg != "" {
		errs = append(errs, field.Invalid(specPath.Child("rules"), nil, msg))
	}

	return errs
}

// balancingViolations rejects a backend that sets balancing.sessionAffinity or
// balancing.prefixAffinity without balancing.mode: LoadAware. An empty mode
// counts as not LoadAware. Pure (mr.Spec only).
func balancingViolations(mr *inferencev1alpha1.ModelRouter, specPath *field.Path) field.ErrorList {
	var errs field.ErrorList
	for i, b := range mr.Spec.Backends {
		if b.Balancing == nil || b.Balancing.Mode == inferencev1alpha1.BalancingModeLoadAware {
			continue
		}
		path := specPath.Child("backends").Index(i).Child("balancing")
		if b.Balancing.SessionAffinity != nil {
			errs = append(errs, field.Invalid(path.Child("sessionAffinity"),
				nil, "sessionAffinity requires balancing.mode: LoadAware"))
		}
		if b.Balancing.PrefixAffinity != nil {
			errs = append(errs, field.Invalid(path.Child("prefixAffinity"),
				nil, "prefixAffinity requires balancing.mode: LoadAware"))
		}
	}
	return errs
}

// balancingWarnings returns one admission warning per backend that still sets
// the deprecated sessionAffinity.maxLoadPercent. Pure (mr.Spec only).
func balancingWarnings(mr *inferencev1alpha1.ModelRouter) admission.Warnings {
	var warnings admission.Warnings
	for i, b := range mr.Spec.Backends {
		if b.Balancing == nil || b.Balancing.SessionAffinity == nil || b.Balancing.SessionAffinity.MaxLoadPercent == 0 {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"spec.backends[%d].balancing.sessionAffinity.maxLoadPercent is deprecated and has no effect; "+
				"if you never set it, it was defaulted by an older LLMKube and can be removed with a JSON patch", i))
	}
	return warnings
}
