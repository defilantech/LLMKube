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
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// This file is the ModelRouter dataPlane: Gateway compiler (proposal 5.2 slice
// 2a). It reuses the GVK/constant helpers and resource shapes defined for the
// InferenceService gateway path in gateway_resources.go, but compiles a
// ModelRouter's backends + rules into:
//
//   - one Backend + AIServiceBackend per InferenceServiceRef backend,
//   - one multi-rule AIGatewayRoute (a rule per spec.rules entry, matching on
//     model name + headers, routing to the rule's backends with priority for
//     failover or weight for weighted),
//   - one BackendTrafficPolicy (retry + passive outlier healthCheck) targeting
//     the generated HTTPRoute, which shares the AIGatewayRoute's name.
//
// Shapes mirror the validated spike manifests
// (experiments/ai-gateway-spike/manifests/): 02/04-backend-*.yaml,
// 03-route.yaml (multi-rule + priority backendRefs), and 05-fallback-policy.yaml
// (the retry/outlier BTP). The rateLimit/budget stanza of 05 is deliberately
// OMITTED here; it lands in slice 2b on this same BTP (footgun 7.1: retry and
// rate-limit must share one BTP or the newer one silently no-ops).
//
// As in slice 1 we build *unstructured.Unstructured (no envoyproxy/* module
// dependency) and own everything via the ModelRouter owner ref.

const (
	// btpKind is the gateway.envoyproxy.io BackendTrafficPolicy kind. The retry
	// + passive healthCheck policy that makes priority failover actually retry
	// onto the secondary backend.
	btpKind       = "BackendTrafficPolicy"
	btpResource   = "backendtrafficpolicies"
	httpRouteKind = "HTTPRoute"
)

// btpGVK is the GVK of the generated BackendTrafficPolicy. Same group/version as
// Backend (gateway.envoyproxy.io/v1alpha1).
func btpGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: gatewayBackendGroup, Version: gatewayBackendVersion, Kind: btpKind}
}

// routerBackendResource is one resolved ModelRouter backend ready to compile:
// the RouterBackend name (used as the Backend/AIServiceBackend name and
// referenced by route backendRefs) plus the cluster FQDN + port the Backend
// targets. The reconciler resolves FQDN/port from the referenced
// InferenceService before calling the builders so the builders stay pure.
type routerBackendResource struct {
	// Name is the RouterBackend.Name; it names the generated Backend and
	// AIServiceBackend and is what rule backendRefs reference.
	Name string
	// FQDN is the cluster-internal hostname the Backend resolves
	// (<svc>.<ns>.svc.cluster.local).
	FQDN string
	// Port is the Service port the Backend targets.
	Port int64
}

// routerRuleResource is one resolved ModelRouter rule ready to compile: the
// model-name match values + header matches, and the ordered backend refs with
// their priority (failover) or weight (weighted).
type routerRuleResource struct {
	// Models are the OpenAI model-name match values (RuleMatch.Models). Each
	// compiles to its own match entry (matches are ORed within a rule).
	Models []string
	// Headers are exact header matches (RuleMatch.Headers), ANDed into every
	// model match (and into a header-only match when Models is empty).
	Headers map[string]string
	// BackendRefs are the ordered destinations. For primary-fallback each ref
	// carries an ascending Priority; for weighted each carries a Weight.
	BackendRefs []routerBackendRef
}

// routerBackendRef is one backendRef in a compiled rule.
type routerBackendRef struct {
	// Name references a generated AIServiceBackend (a routerBackendResource
	// Name).
	Name string
	// Priority is the failover order (0 = primary) for the primary-fallback
	// strategy. nil when the strategy is weighted.
	Priority *int64
	// Weight is the traffic share for the weighted strategy. nil when the
	// strategy is primary-fallback.
	Weight *int64
}

// modelRouterGatewayResourceName is the shared, DNS-sanitized name for the
// AIGatewayRoute and the BackendTrafficPolicy of a ModelRouter. Per-backend
// Backend/AIServiceBackend resources are named after the RouterBackend instead
// (so rule backendRefs can reference them), see newRouterBackend.
func modelRouterGatewayResourceName(mr *inferencev1alpha1.ModelRouter) string {
	return sanitizeDNSName(mr.Name)
}

// newRouterBackend builds the gateway.envoyproxy.io Backend for one resolved
// ModelRouter backend, pointing at the referenced InferenceService's Service.
// Lives in the ModelRouter namespace. Mirrors spike 02/04-backend-*.yaml.
func newRouterBackend(mr *inferencev1alpha1.ModelRouter, b routerBackendResource) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(backendGVK())
	u.SetName(sanitizeDNSName(b.Name))
	u.SetNamespace(mr.Namespace)
	u.Object["spec"] = map[string]interface{}{
		"endpoints": []interface{}{
			map[string]interface{}{
				"fqdn": map[string]interface{}{
					"hostname": b.FQDN,
					"port":     b.Port,
				},
			},
		},
	}
	return u
}

// newRouterAIServiceBackend builds the aigateway.envoyproxy.io AIServiceBackend
// wrapping one backend's Backend with the OpenAI schema. Lives in the
// ModelRouter namespace. Mirrors spike 02/04-backend-*.yaml.
func newRouterAIServiceBackend(mr *inferencev1alpha1.ModelRouter, b routerBackendResource) *unstructured.Unstructured {
	name := sanitizeDNSName(b.Name)
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(aiServiceBackendGVK())
	u.SetName(name)
	u.SetNamespace(mr.Namespace)
	u.Object["spec"] = map[string]interface{}{
		"schema": map[string]interface{}{
			"name": aiGatewayBackendSchemaName,
		},
		"backendRef": map[string]interface{}{
			"name":  name,
			"kind":  gatewayBackendKind,
			"group": gatewayBackendGroup,
		},
	}
	return u
}

// newRouterAIGatewayRoute builds the one-per-ModelRouter multi-rule
// AIGatewayRoute attached to the referenced Gateway. Each resolved rule becomes
// one spec.rules entry whose matches OR over the rule's model names (each ANDed
// with the rule's header matches) and whose backendRefs carry priority
// (failover) or weight (weighted). Mirrors spike 03-route.yaml.
func newRouterAIGatewayRoute(
	mr *inferencev1alpha1.ModelRouter,
	gatewayRef *inferencev1alpha1.GatewayReference,
	rules []routerRuleResource,
) *unstructured.Unstructured {
	parentRef := map[string]interface{}{
		"name":  gatewayRef.Name,
		"kind":  aiGatewayRouteParentRefKind,
		"group": gatewayBackendRefGroupAPI,
	}
	if gatewayRef.Namespace != "" {
		parentRef["namespace"] = gatewayRef.Namespace
	}

	compiledRules := make([]interface{}, 0, len(rules))
	for _, rule := range rules {
		compiledRules = append(compiledRules, compileRouteRule(rule))
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(aiGatewayRouteGVK())
	u.SetName(modelRouterGatewayResourceName(mr))
	u.SetNamespace(mr.Namespace)
	u.Object["spec"] = map[string]interface{}{
		"parentRefs": []interface{}{parentRef},
		"rules":      compiledRules,
	}
	return u
}

// compileRouteRule builds one AIGatewayRoute rule: the matches block (a match
// per model name, ANDed with header matches; a header-only match when the rule
// declares no models, e.g. a catch-all) and the backendRefs block.
func compileRouteRule(rule routerRuleResource) map[string]interface{} {
	return map[string]interface{}{
		"matches":     compileRuleMatches(rule),
		"backendRefs": compileRuleBackendRefs(rule.BackendRefs),
	}
}

// compileRuleMatches turns a rule's model names + headers into AIGatewayRoute
// match entries. The gateway data plane copies the request body "model" field
// into the x-ai-eg-model header, so a model match is an Exact header match on
// that header. Multiple models become multiple match entries (ORed). Declared
// headers are ANDed into each match entry. A rule with no models compiles to a
// single header-only match (the catch-all / defaultRoute case).
func compileRuleMatches(rule routerRuleResource) []interface{} {
	headerMatches := sortedHeaderMatches(rule.Headers)

	if len(rule.Models) == 0 {
		// Header-only (or fully unconditional catch-all) match.
		match := map[string]interface{}{}
		if len(headerMatches) > 0 {
			match["headers"] = headerMatches
		}
		return []interface{}{match}
	}

	matches := make([]interface{}, 0, len(rule.Models))
	for _, model := range rule.Models {
		headers := make([]interface{}, 0, 1+len(headerMatches))
		headers = append(headers, modelHeaderMatch(model))
		headers = append(headers, headerMatches...)
		matches = append(matches, map[string]interface{}{"headers": headers})
	}
	return matches
}

// modelHeaderMatch is the Exact x-ai-eg-model header match for one model name.
func modelHeaderMatch(model string) map[string]interface{} {
	return map[string]interface{}{
		"type":  "Exact",
		"name":  aiGatewayModelHeader,
		"value": model,
	}
}

// sortedHeaderMatches turns the rule's exact-header map into a deterministic
// slice of Exact header matches (sorted by name so the generated route is
// stable across reconciles and CreateOrUpdate does not churn).
func sortedHeaderMatches(headers map[string]string) []interface{} {
	if len(headers) == 0 {
		return nil
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]interface{}, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]interface{}{
			"type":  "Exact",
			"name":  name,
			"value": headers[name],
		})
	}
	return out
}

// compileRuleBackendRefs turns the resolved backend refs into AIGatewayRoute
// backendRefs, attaching priority (failover) or weight (weighted) when set.
func compileRuleBackendRefs(refs []routerBackendRef) []interface{} {
	out := make([]interface{}, 0, len(refs))
	for _, ref := range refs {
		backendRef := map[string]interface{}{
			"name": sanitizeDNSName(ref.Name),
		}
		if ref.Priority != nil {
			backendRef["priority"] = *ref.Priority
		}
		if ref.Weight != nil {
			backendRef["weight"] = *ref.Weight
		}
		out = append(out, backendRef)
	}
	return out
}

// newRouterBackendTrafficPolicy builds the retry + passive-outlier
// BackendTrafficPolicy that makes priority failover actually retry onto the
// secondary backend. It targets the generated HTTPRoute (which shares the
// AIGatewayRoute's name) by name. Mirrors spike 05-fallback-policy.yaml MINUS
// the rateLimit/budget stanza (slice 2b adds that to THIS policy; two BTPs on
// one target silently conflict, footgun 7.1).
func newRouterBackendTrafficPolicy(mr *inferencev1alpha1.ModelRouter) *unstructured.Unstructured {
	name := modelRouterGatewayResourceName(mr)
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(btpGVK())
	u.SetName(name)
	u.SetNamespace(mr.Namespace)
	u.Object["spec"] = map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{
				"group": gatewayBackendRefGroupAPI,
				"kind":  httpRouteKind,
				"name":  name,
			},
		},
		"retry": map[string]interface{}{
			"numAttemptsPerPriority": int64(1),
			"numRetries":             int64(5),
			"perRetry": map[string]interface{}{
				"backOff": map[string]interface{}{
					"baseInterval": "100ms",
					"maxInterval":  "10s",
				},
				"timeout": "60s",
			},
			"retryOn": map[string]interface{}{
				"httpStatusCodes": []interface{}{int64(500), int64(503)},
				"triggers":        []interface{}{"connect-failure", "retriable-status-codes"},
			},
		},
		"healthCheck": map[string]interface{}{
			"passive": map[string]interface{}{
				"baseEjectionTime":     "30s",
				"consecutive5XxErrors": int64(3),
				"interval":             "5s",
				"maxEjectionPercent":   int64(100),
			},
		},
	}
	return u
}

// modelRouterGatewayGVKs are the GVKs the ModelRouter gateway path needs the
// cluster to have registered before it generates anything: slice 1's three plus
// the BackendTrafficPolicy.
func modelRouterGatewayGVKs() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		backendGVK(),
		aiServiceBackendGVK(),
		aiGatewayRouteGVK(),
		btpGVK(),
	}
}

// gatewayEndpointAddress is the human-facing endpoint string surfaced on
// status.gateway.endpoint: the OpenAI base path on the referenced Gateway. We do
// not resolve the Gateway's external address (that is gateway-owned config the
// operator does not read); this records WHICH gateway fronts the router.
func gatewayEndpointAddress(gatewayRef *inferencev1alpha1.GatewayReference) string {
	ns := gatewayRef.Namespace
	if ns == "" {
		return fmt.Sprintf("gateway %q", gatewayRef.Name)
	}
	return fmt.Sprintf("gateway %s/%s", ns, gatewayRef.Name)
}
