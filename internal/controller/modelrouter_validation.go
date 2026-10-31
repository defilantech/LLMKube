/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

// Sane bounds for rule and backend timeouts. The lower bound rejects
// values too short to be meaningful (even local llama-server can't
// produce first-token in 100ms cold-start); the upper bound rejects
// values that are more likely a unit typo (eg "30s" vs "30m") than a
// legitimate request.
const (
	minRouterTimeout = 100 * time.Millisecond
	maxRouterTimeout = 30 * time.Minute
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
//   - A weighted rule names at least one backend that can take traffic
//     (positive weight, every required capability).
//   - A shadow rule names at least two backends.
//   - Rules matching sensitive classifications (pii/phi by default, or
//     whatever policy.classification.sensitiveClassifications says) must
//     have failClosed=true AND route only to local-tier backends. This is
//     the regulated-data gate that prevents sensitive data from egressing.
//   - Budget specs are well-formed (rule scope references a real rule;
//     at least one of maxTokens or maxUSD is set; a maxUSD budget requires
//     every backend to declare pricing).
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
	errs = append(errs, validateAuditLog(spec)...)

	return errs
}

// validateAuditLog checks the client attribution settings in
// spec.policy.auditLog (#2283). The CIDRs are parsed here, not by a CEL
// rule, because CEL's isCIDR() needs Kubernetes 1.31; the proxy's
// Config.Validate repeats the checks for a config written by hand.
func validateAuditLog(spec *inferencev1alpha1.ModelRouterSpec) []ModelRouterValidationError {
	if spec.Policy == nil || spec.Policy.AuditLog == nil {
		return nil
	}
	a := spec.Policy.AuditLog
	var errs []ModelRouterValidationError
	if _, err := router.ParseTrustedProxies(a.TrustedProxyCIDRs); err != nil {
		errs = append(errs, ModelRouterValidationError{
			Field:   "spec.policy.auditLog.trustedProxyCIDRs",
			Message: err.Error(),
		})
	}
	if len(a.TrustedProxyCIDRs) > 0 && !a.LogClientAddress {
		errs = append(errs, ModelRouterValidationError{
			Field:   "spec.policy.auditLog.trustedProxyCIDRs",
			Message: "has no effect unless logClientAddress is true",
		})
	}
	if router.IsCredentialHeader(a.CallerHeader) {
		errs = append(errs, ModelRouterValidationError{
			Field:   "spec.policy.auditLog.callerHeader",
			Message: fmt.Sprintf("%q carries a credential; logging it would write the secret into every audit entry", a.CallerHeader),
		})
	}
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
		if b.InferenceServiceRef != nil && b.Tier == backendTierCloud {
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

		errs = append(errs, validateBackendTimeout(b, path)...)
	}
	return nameSet, byName, errs
}

// validateBackendTimeout enforces the same sane bounds as rule.timeout
// (see validateRuleTimeout). Backend-level timeouts override the proxy
// default but are themselves overridden by rule-level timeouts at
// dispatch time.
func validateBackendTimeout(
	b *inferencev1alpha1.RouterBackend,
	path string,
) []ModelRouterValidationError {
	if b.Timeout == nil {
		return nil
	}
	d := b.Timeout.Duration
	if d < minRouterTimeout {
		return []ModelRouterValidationError{{
			Field:   path + ".timeout",
			Message: fmt.Sprintf("must be >= %s; got %s", minRouterTimeout, d),
		}}
	}
	if d > maxRouterTimeout {
		return []ModelRouterValidationError{{
			Field:   path + ".timeout",
			Message: fmt.Sprintf("must be <= %s; got %s (probable unit typo)", maxRouterTimeout, d),
		}}
	}
	return nil
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
		errs = append(errs, validateRuleWeights(rule, path, byName)...)
		errs = append(errs, validateRuleShadow(rule, path)...)
		errs = append(errs, validateRuleSensitiveData(rule, path, sensitiveSet, byName)...)
		errs = append(errs, validateRuleTimeout(rule, path)...)
	}
	return ruleNames, errs
}

// validateRuleTimeout enforces sane bounds on rule.timeout. Too short
// (<100ms) is almost certainly a config error: even local llama-server
// can't first-token in under that. Too long (>30m) is a smell for any
// LLM request and is more likely a unit typo (eg "30s" intended as
// "30m") than a legitimate ask.
func validateRuleTimeout(
	rule *inferencev1alpha1.RouterRule,
	path string,
) []ModelRouterValidationError {
	if rule.Timeout == nil {
		return nil
	}
	d := rule.Timeout.Duration
	if d < minRouterTimeout {
		return []ModelRouterValidationError{{
			Field:   path + ".timeout",
			Message: fmt.Sprintf("must be >= %s; got %s", minRouterTimeout, d),
		}}
	}
	if d > maxRouterTimeout {
		return []ModelRouterValidationError{{
			Field:   path + ".timeout",
			Message: fmt.Sprintf("must be <= %s; got %s (probable unit typo)", maxRouterTimeout, d),
		}}
	}
	return nil
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

// validateRuleWeights rejects a weighted rule none of whose backends can take
// traffic: every backend it names has weight 0 or lacks a capability the rule
// requires. Both data planes would serve such a rule nothing (a zero weight
// carries no traffic on either), so it fails here instead of at request time.
// Undefined backend names are validateRuleRoute's to report.
func validateRuleWeights(
	rule *inferencev1alpha1.RouterRule,
	path string,
	byName map[string]*inferencev1alpha1.RouterBackend,
) []ModelRouterValidationError {
	if rule.Route.Strategy != weightedStrategy {
		return nil
	}
	var required []string
	if rule.Match != nil {
		required = rule.Match.RequiredCapabilities
	}
	for _, name := range rule.Route.Backends {
		b := byName[name]
		if b == nil || (b.Weight != nil && *b.Weight == 0) {
			continue
		}
		if hasEveryCapability(b.Capabilities, required) {
			return nil
		}
	}
	return []ModelRouterValidationError{{
		Field: path + ".route.backends",
		Message: "strategy weighted needs at least one backend with a positive weight " +
			"(unset means 1) that advertises every required capability",
	}}
}

// shadowStrategy is the RuleRoute strategy that serves from the first backend
// and mirrors each request to the others.
const shadowStrategy = "shadow"

// validateRuleShadow rejects a shadow rule with fewer than two backends: the
// first serves the client, and a rule with nothing to mirror to is a
// primary-fallback rule under another name. Whether a mirror target is a
// ModelPool member needs the pools, so the compile checks that (the
// router-proxy's Config.Validate).
func validateRuleShadow(rule *inferencev1alpha1.RouterRule, path string) []ModelRouterValidationError {
	// An empty list is validateRuleRoute's to report.
	if rule.Route.Strategy != shadowStrategy || len(rule.Route.Backends) != 1 {
		return nil
	}
	return []ModelRouterValidationError{{
		Field:   path + ".route.backends",
		Message: "strategy shadow needs at least two backends: the first serves, the rest receive copies",
	}}
}

// hasEveryCapability reports whether have lists every entry of want.
func hasEveryCapability(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
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
		if b.Tier == backendTierCloud {
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
// must reference a real rule, every budget must set at least one of
// maxTokens / maxUSD, and a maxUSD budget requires every backend to declare
// pricing. A maxUSD budget is unenforceable unless every backend it can route
// to declares pricing, so fail loud here rather than admit a budget that
// charges 0 USD forever.
func validateBudgets(
	spec *inferencev1alpha1.ModelRouterSpec,
	ruleNames map[string]bool,
) []ModelRouterValidationError {
	if spec.Policy == nil {
		return nil
	}
	var errs []ModelRouterValidationError
	needsPricing := false
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
		if strings.TrimSpace(b.MaxUSD) != "" {
			needsPricing = true
		}
	}
	if needsPricing {
		for i, b := range spec.Backends {
			if !pricesTokens(b.CostPerMillionTokens) {
				errs = append(errs, ModelRouterValidationError{
					Field: fmt.Sprintf("spec.backends[%d].costPerMillionTokens", i),
					Message: "required when any budget sets maxUSD; without it that budget " +
						"charges 0 USD and never blocks",
				})
			}
		}
	}
	return errs
}

// pricesTokens reports whether a backend's declared pricing can charge a
// nonzero USD amount. A nil cost, or one whose prompt and completion rates
// both parse to zero, prices every request at zero and cannot feed a maxUSD
// budget.
func pricesTokens(c *inferencev1alpha1.TokenCost) bool {
	if c == nil {
		return false
	}
	return positiveUSD(c.PromptUSD) || positiveUSD(c.CompletionUSD)
}

// positiveUSD reports whether a validated decimal string parses to a value
// greater than zero.
func positiveUSD(s string) bool {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return err == nil && f > 0
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
