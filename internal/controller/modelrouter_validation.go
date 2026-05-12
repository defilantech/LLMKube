/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"fmt"
	"strings"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// ModelRouterValidationError describes a single static-validation failure on
// a ModelRouter spec. The Field is a dotted JSONPath-style locator so users
// can match it to their manifest; Message is a human-readable explanation.
type ModelRouterValidationError struct {
	Field   string
	Message string
}

func (e ModelRouterValidationError) String() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// validateModelRouter performs static validation of a ModelRouter spec.
// It checks invariants that the kubebuilder OpenAPI schema cannot express:
//
//   - Each backend must declare exactly one of inferenceServiceRef or external.
//   - Backend tier must be consistent with its kind (local for
//     inferenceServiceRef, cloud-or-omitted for external).
//   - Backend names must be unique.
//   - DefaultRoute, if set, names an existing backend.
//   - Every rule's route.backends entries reference existing backends.
//   - Rules matching sensitive classifications (pii/phi by default, or
//     whatever policy.classification.sensitiveClassifications says) must
//     have failClosed=true AND route only to local-tier backends. This is
//     the regulated-data gate that prevents sensitive data from egressing.
//   - Budget specs are well-formed (rule scope references a real rule;
//     at least one of maxTokens or maxUSD is set).
//
// The function is pure: no K8s API access. The caller transforms the
// returned errors into a Validated condition on the ModelRouter status.
func validateModelRouter(mr *inferencev1alpha1.ModelRouter) []ModelRouterValidationError {
	spec := &mr.Spec
	var errs []ModelRouterValidationError

	nameSet, backendsByName, backendErrs := validateBackends(spec)
	errs = append(errs, backendErrs...)

	if spec.DefaultRoute != "" && !nameSet[spec.DefaultRoute] {
		errs = append(errs, ModelRouterValidationError{
			Field:   "spec.defaultRoute",
			Message: fmt.Sprintf("references undefined backend %q", spec.DefaultRoute),
		})
	}

	ruleNames, ruleErrs := validateRules(spec, nameSet, backendsByName)
	errs = append(errs, ruleErrs...)
	errs = append(errs, validateBudgets(spec, ruleNames)...)

	return errs
}

// validateBackends checks each RouterBackend in spec.backends and returns
// the resolved name set, a name-to-backend lookup map, and any errors.
func validateBackends(spec *inferencev1alpha1.ModelRouterSpec) (
	map[string]bool,
	map[string]*inferencev1alpha1.RouterBackend,
	[]ModelRouterValidationError,
) {
	nameSet := make(map[string]bool, len(spec.Backends))
	byName := make(map[string]*inferencev1alpha1.RouterBackend, len(spec.Backends))
	var errs []ModelRouterValidationError

	for i := range spec.Backends {
		b := &spec.Backends[i]
		path := fmt.Sprintf("spec.backends[%d]", i)
		errs = append(errs, validateBackendKindExclusivity(b, path)...)

		// Tier consistency: an InferenceServiceRef backend is by definition
		// local. Allowing cloud tier here would let users bypass the
		// fail-closed gate by mislabelling a local backend.
		if b.InferenceServiceRef != nil && b.Tier == "cloud" {
			errs = append(errs, ModelRouterValidationError{
				Field:   path + ".tier",
				Message: "tier=cloud is invalid for a backend with inferenceServiceRef (use tier=local)",
			})
		}

		if b.Name == "" {
			errs = append(errs, ModelRouterValidationError{
				Field:   path + ".name",
				Message: "name is required",
			})
		} else if nameSet[b.Name] {
			errs = append(errs, ModelRouterValidationError{
				Field:   path + ".name",
				Message: fmt.Sprintf("duplicate backend name %q", b.Name),
			})
		}
		nameSet[b.Name] = true
		byName[b.Name] = b
	}
	return nameSet, byName, errs
}

// validateBackendKindExclusivity enforces exactly-one-of(inferenceServiceRef,
// external) on a single backend.
func validateBackendKindExclusivity(b *inferencev1alpha1.RouterBackend, path string) []ModelRouterValidationError {
	hasLocal := b.InferenceServiceRef != nil
	hasExt := b.External != nil
	switch {
	case hasLocal && hasExt:
		return []ModelRouterValidationError{{
			Field:   path,
			Message: "exactly one of inferenceServiceRef or external must be set, not both",
		}}
	case !hasLocal && !hasExt:
		return []ModelRouterValidationError{{
			Field:   path,
			Message: "exactly one of inferenceServiceRef or external must be set",
		}}
	}
	return nil
}

// validateRules checks each RouterRule and returns the set of rule names
// (for budget cross-references) plus any errors.
func validateRules(
	spec *inferencev1alpha1.ModelRouterSpec,
	nameSet map[string]bool,
	byName map[string]*inferencev1alpha1.RouterBackend,
) (map[string]bool, []ModelRouterValidationError) {
	sensitiveSet := sensitiveClassificationSet(spec.Policy)
	ruleNames := make(map[string]bool, len(spec.Rules))
	errs := make([]ModelRouterValidationError, 0, len(spec.Rules))

	for i := range spec.Rules {
		rule := &spec.Rules[i]
		path := fmt.Sprintf("spec.rules[%d]", i)

		if rule.Name != "" {
			ruleNames[rule.Name] = true
		}
		errs = append(errs, validateRuleRoute(rule, path, nameSet)...)
		errs = append(errs, validateRuleSensitiveData(rule, path, sensitiveSet, byName)...)
	}
	return ruleNames, errs
}

// validateRuleRoute checks that rule.route.backends references real backends.
func validateRuleRoute(
	rule *inferencev1alpha1.RouterRule,
	path string,
	nameSet map[string]bool,
) []ModelRouterValidationError {
	var errs []ModelRouterValidationError
	if len(rule.Route.Backends) == 0 {
		errs = append(errs, ModelRouterValidationError{
			Field:   path + ".route.backends",
			Message: "must reference at least one backend",
		})
	}
	for j, name := range rule.Route.Backends {
		if !nameSet[name] {
			errs = append(errs, ModelRouterValidationError{
				Field:   fmt.Sprintf("%s.route.backends[%d]", path, j),
				Message: fmt.Sprintf("references undefined backend %q", name),
			})
		}
	}
	return errs
}

// validateRuleSensitiveData enforces the fail-closed gate: rules matching
// sensitive classifications must have failClosed=true and may only reference
// local-tier backends.
func validateRuleSensitiveData(
	rule *inferencev1alpha1.RouterRule,
	path string,
	sensitiveSet map[string]bool,
	byName map[string]*inferencev1alpha1.RouterBackend,
) []ModelRouterValidationError {
	if rule.Match == nil {
		return nil
	}

	var matched []string
	for _, cls := range rule.Match.DataClassification {
		if sensitiveSet[cls] {
			matched = append(matched, cls)
		}
	}
	if len(matched) == 0 {
		return nil
	}

	var errs []ModelRouterValidationError
	if !rule.FailClosed {
		errs = append(errs, ModelRouterValidationError{
			Field: path + ".failClosed",
			Message: fmt.Sprintf(
				"rule matches sensitive classifications %v and must set failClosed=true",
				matched),
		})
	}
	for _, name := range rule.Route.Backends {
		b, ok := byName[name]
		if !ok {
			// Already reported in validateRuleRoute.
			continue
		}
		if b.Tier == "cloud" {
			errs = append(errs, ModelRouterValidationError{
				Field: path + ".route.backends",
				Message: fmt.Sprintf(
					"sensitive-data rule cannot route to cloud-tier backend %q (matched classifications: %v)",
					name, matched),
			})
		}
	}
	return errs
}

// validateBudgets covers the BudgetSpec invariants: rule-scoped budgets
// must reference a real rule, and every budget must set at least one of
// maxTokens / maxUSD.
func validateBudgets(
	spec *inferencev1alpha1.ModelRouterSpec,
	ruleNames map[string]bool,
) []ModelRouterValidationError {
	if spec.Policy == nil {
		return nil
	}
	var errs []ModelRouterValidationError
	for i, b := range spec.Policy.Budgets {
		path := fmt.Sprintf("spec.policy.budgets[%d]", i)
		if b.Scope == "rule" {
			switch {
			case b.RuleName == "":
				errs = append(errs, ModelRouterValidationError{
					Field:   path + ".ruleName",
					Message: "ruleName is required when scope=rule",
				})
			case !ruleNames[b.RuleName]:
				errs = append(errs, ModelRouterValidationError{
					Field:   path + ".ruleName",
					Message: fmt.Sprintf("references undefined rule %q", b.RuleName),
				})
			}
		}
		if b.MaxTokens == nil && strings.TrimSpace(b.MaxUSD) == "" {
			errs = append(errs, ModelRouterValidationError{
				Field:   path,
				Message: "must set at least one of maxTokens or maxUSD",
			})
		}
	}
	return errs
}

// sensitiveClassificationSet returns the set of classification values that
// trigger fail-closed validation. When the policy explicitly lists
// SensitiveClassifications, those are used as-is; otherwise the default of
// {"pii", "phi"} applies. Documented in the ModelRouter concept page so
// operators can predict the gate's behavior.
func sensitiveClassificationSet(policy *inferencev1alpha1.RouterPolicy) map[string]bool {
	out := make(map[string]bool, 2)
	if policy != nil && policy.Classification != nil && len(policy.Classification.SensitiveClassifications) > 0 {
		for _, s := range policy.Classification.SensitiveClassifications {
			out[s] = true
		}
		return out
	}
	out["pii"] = true
	out["phi"] = true
	return out
}

// formatValidationErrors joins validation errors into a single status
// message. Joined by "; " so each entry stays scannable in `kubectl describe`.
func formatValidationErrors(errs []ModelRouterValidationError) string {
	if len(errs) == 0 {
		return ""
	}
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.String()
	}
	return strings.Join(parts, "; ")
}
