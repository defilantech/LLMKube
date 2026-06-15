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
	"regexp"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// nonDNSChar matches any run of characters that are not DNS label-safe
// ([a-z0-9-]); allowlistRuleName collapses each run to a single '-' when
// building a human-readable rule-name fragment from a team value.
var nonDNSChar = regexp.MustCompile(`[^a-z0-9-]+`)

// This file is the ModelRouter dataPlane: Gateway compiler (proposal 5.2 slice
// 2a). It reuses the GVK/constant helpers and resource shapes defined for the
// InferenceService gateway path in gateway_resources.go, but compiles a
// ModelRouter's backends + rules into:
//
//   - one Backend + AIServiceBackend per InferenceServiceRef backend,
//   - one multi-rule AIGatewayRoute (a rule per spec.rules entry, matching on
//     model name + headers, routing to the rule's backends with priority for
//     failover or weight for weighted),
//   - one BackendTrafficPolicy (retry + passive outlier + active HTTP
//     healthCheck) targeting the generated HTTPRoute, which shares the
//     AIGatewayRoute's name.
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

	// aiGatewayMetadataNamespace is the dynamic-metadata namespace the Envoy AI
	// Gateway extproc writes per-request usage under. See aiGatewayTotalTokenKey
	// for the total-token key within it.
	aiGatewayMetadataNamespace = "io.envoy.ai_gateway"

	// defaultTeamHeaderKey is the team header shared by two slices: a team-scope
	// budget (2b) keys its rateLimit bucket on it, and JWT auth (2d) lands the
	// verified team claim on it, so the gateway derives team identity from the
	// token instead of a client header. Matches the CRD default on
	// BudgetSpec.HeaderKey and JWTAuthSpec.HeaderKey.
	defaultTeamHeaderKey = "x-llmkube-team"

	// Budget scopes compiled in slice 2b. "rule" is fail-loud (deferred).
	budgetScopeRouter = "router"
	budgetScopeTeam   = "team"
	budgetScopeRule   = "rule"

	// securityPolicyKind is the gateway.envoyproxy.io SecurityPolicy kind. Slice
	// 2d-core compiles policy.auth.jwt into one of these to validate inbound JWTs
	// and map a verified claim onto a trusted header.
	securityPolicyKind     = "SecurityPolicy"
	securityPolicyResource = "securitypolicies"

	// metadataNameField is the "name" key used throughout the unstructured
	// object builders in this file (metadata.name and the many nested
	// backendRef/targetRef name fields). Extracted to a const because slice 2d
	// pushed the package-wide occurrence count past the goconst threshold.
	metadataNameField = "name"

	// Active HTTP health-check tunables compiled onto the BTP in slice 4a (#662).
	// Hardcoded sensible defaults for v1, mirroring how the passive healthCheck
	// block is hardcoded; a follow-up can promote them to the ModelRouter API.
	// The short interval favors fast detection of an abruptly-killed Metal backend
	// behind a ClusterIP (which passive outlier detection never ejects, since a
	// blackholed connection returns no 5xx). The path is the llama.cpp /
	// llama-server health endpoint, served by the llama.cpp/vLLM/TGI pod runtimes
	// and the Metal llama/mlx runtime this slice targets.
	//
	// CAVEAT: this hardcodes GET /health for EVERY gateway-mode backend. Runtimes
	// that do not serve HTTP /health (the TCP-probed PersonaPlex/Generic runtimes
	// today, and external/cloud backends once dataPlane: Gateway supports them)
	// would fail this probe and be ejected even while healthy. Gateway mode
	// currently rejects external backends outright (resolveBackends), and the
	// demo/target runtimes all serve /health, so this is safe to ship; the
	// per-runtime configurable health path is the deferred follow-up that lifts
	// the assumption (see proposal 5.3, slice 4a "Deferred").
	activeHealthCheckInterval           = "2s"
	activeHealthCheckTimeout            = "1s"
	activeHealthCheckPath               = "/health"
	activeHealthCheckUnhealthyThreshold = int64(2)
	activeHealthCheckHealthyThreshold   = int64(1)
)

// aiGatewayTotalTokenKey is the dynamic-metadata key (within
// aiGatewayMetadataNamespace) under which the gateway records a request's total
// token count. The route's llmRequestCosts emits this key at response
// completion, and the BTP rateLimit charges it. Mirrors the spike manifests
// 03-route.yaml (llmRequestCosts) and 05-fallback-policy.yaml (rateLimit
// cost.response.metadata). It is a function (not a const) so the metadata-key
// literal is not bound to a "token"-named package-level identifier, which the
// gosec G101 hardcoded-credential heuristic flags as a false positive.
func aiGatewayTotalTokenKey() string {
	return "llm_total_token"
}

// btpGVK is the GVK of the generated BackendTrafficPolicy. Same group/version as
// Backend (gateway.envoyproxy.io/v1alpha1).
func btpGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: gatewayBackendGroup, Version: gatewayBackendVersion, Kind: btpKind}
}

// securityPolicyGVK is the GVK of the generated SecurityPolicy. Same
// group/version as Backend (gateway.envoyproxy.io/v1alpha1).
func securityPolicyGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: gatewayBackendGroup, Version: gatewayBackendVersion, Kind: securityPolicyKind}
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
			metadataNameField: aiGatewayBackendSchemaName,
		},
		"backendRef": map[string]interface{}{
			metadataNameField: name,
			"kind":            gatewayBackendKind,
			"group":           gatewayBackendGroup,
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
	budgets []inferencev1alpha1.BudgetSpec,
) *unstructured.Unstructured {
	parentRef := map[string]interface{}{
		metadataNameField: gatewayRef.Name,
		"kind":            aiGatewayRouteParentRefKind,
		"group":           gatewayBackendRefGroupAPI,
	}
	if gatewayRef.Namespace != "" {
		parentRef["namespace"] = gatewayRef.Namespace
	}

	compiledRules := make([]interface{}, 0, len(rules))
	for _, rule := range rules {
		compiledRules = append(compiledRules, compileRouteRule(rule))
	}

	spec := map[string]interface{}{
		"parentRefs": []interface{}{parentRef},
		"rules":      compiledRules,
	}

	// slice 2b: when budgets exist, charge the rate limit in tokens by emitting
	// the TotalToken cost metadata. Without budgets we omit the key entirely so a
	// 2a-only ModelRouter's route is byte-identical to #693's output.
	if len(budgets) > 0 {
		spec["llmRequestCosts"] = routerLLMRequestCosts()
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(aiGatewayRouteGVK())
	u.SetName(modelRouterGatewayResourceName(mr))
	u.SetNamespace(mr.Namespace)
	u.Object["spec"] = spec
	return u
}

// routerLLMRequestCosts is the llmRequestCosts block added to the AIGatewayRoute
// when budgets are present. It emits the total-token count into dynamic metadata
// (io.envoy.ai_gateway/llm_total_token) at response completion, which is the key
// the BTP rateLimit charges. Mirrors spike 03-route.yaml; slice 2b only needs
// the TotalToken entry (the input/output splits there feed the audit access logs
// of slice 2c, which is not yet compiled).
func routerLLMRequestCosts() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"metadataKey": aiGatewayTotalTokenKey(),
			"type":        "TotalToken",
		},
	}
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
		"type":            "Exact",
		metadataNameField: aiGatewayModelHeader,
		"value":           model,
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
			"type":            "Exact",
			metadataNameField: name,
			"value":           headers[name],
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
			metadataNameField: sanitizeDNSName(ref.Name),
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

// newRouterBackendTrafficPolicy builds the retry + passive-outlier + active
// HTTP health-check BackendTrafficPolicy that makes priority failover actually
// retry onto the secondary backend and ejects dead backends fast. It targets the
// generated HTTPRoute (which shares the AIGatewayRoute's name) by name. Mirrors
// spike 05-fallback-policy.yaml MINUS the rateLimit/budget stanza (slice 2b adds
// that to THIS policy; two BTPs on one target silently conflict, footgun 7.1).
// The active HTTP check (slice 4a, #662) probes each backend cluster's /health
// endpoint directly at the cluster level, bypassing the route filters, and is
// independent of the long-generation request timeout (it hits /health, not the
// generation path), so it ejects an abruptly-killed Metal backend behind a
// ClusterIP without waiting on the per-attempt request timeout.
func newRouterBackendTrafficPolicy(mr *inferencev1alpha1.ModelRouter, budgets []inferencev1alpha1.BudgetSpec) *unstructured.Unstructured {
	name := modelRouterGatewayResourceName(mr)
	spec := map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{
				"group":           gatewayBackendRefGroupAPI,
				"kind":            httpRouteKind,
				metadataNameField: name,
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
			// Active HTTP probe (slice 4a, #662): Envoy hits each backend cluster's
			// /health directly, bypassing the route filters, so a blackholed Metal
			// backend (abrupt Mac death behind a ClusterIP) is ejected at probe speed
			// rather than stalling for the per-attempt request timeout. Independent of
			// the long-generation timeout since it probes /health, not the generation
			// path.
			"active": map[string]interface{}{
				"type":               "HTTP",
				"interval":           activeHealthCheckInterval,
				"timeout":            activeHealthCheckTimeout,
				"unhealthyThreshold": activeHealthCheckUnhealthyThreshold,
				"healthyThreshold":   activeHealthCheckHealthyThreshold,
				"http": map[string]interface{}{
					"path":             activeHealthCheckPath,
					"method":           "GET",
					"expectedStatuses": []interface{}{int64(200)},
				},
			},
		},
	}

	// slice 2b: budgets compile to a Global rateLimit stanza on THIS SAME BTP.
	// Two BTPs targeting one route silently conflict (oldest wins, footgun 7.1),
	// so the budget rateLimit must share the policy with retry/healthCheck. When
	// there are no budgets we omit the key entirely so a 2a-only ModelRouter's
	// BTP is byte-identical to #693's output.
	if len(budgets) > 0 {
		spec["rateLimit"] = compileBudgetRateLimit(budgets)
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(btpGVK())
	u.SetName(name)
	u.SetNamespace(mr.Namespace)
	u.Object["spec"] = spec
	return u
}

// compileBudgetRateLimit turns the router's budgets into a Global rateLimit
// stanza: one rule per budget. router-scope budgets become a global descriptor
// (no clientSelector); team-scope budgets become a descriptor keyed (Distinct)
// on the budget's HeaderKey so each header value gets an independent bucket. The
// limit is charged in tokens via the route's llmRequestCosts (TotalToken), with
// the request path check-only (cost.request.number = 0) and usage debited at
// response completion (cost.response.metadata). Mirrors spike
// 05-fallback-policy.yaml. Callers guarantee (via unsupportedBudgetMessage)
// every budget here is router or team scope with MaxTokens set; rule scope and
// MaxUSD fail loud before generation.
func compileBudgetRateLimit(budgets []inferencev1alpha1.BudgetSpec) map[string]interface{} {
	rules := make([]interface{}, 0, len(budgets))
	for _, b := range budgets {
		rules = append(rules, compileBudgetRule(b))
	}
	return map[string]interface{}{
		"type": "Global",
		"global": map[string]interface{}{
			"rules": rules,
		},
	}
}

// compileBudgetRule compiles one budget into a rateLimit rule. Envoy expresses a
// limit as "N per ONE unit" (Second/Minute/Hour/Day), so MaxTokens (the cap over
// WindowSeconds) is scaled to the chosen unit to keep the enforced average rate
// faithful for arbitrary windows. team scope adds a Distinct header clientSelector.
func compileBudgetRule(b inferencev1alpha1.BudgetSpec) map[string]interface{} {
	unit := budgetWindowUnit(b.WindowSeconds)
	// requests = MaxTokens * unitSeconds / WindowSeconds, floored, min 1. Using a
	// raw MaxTokens with a sub-window unit would under-enforce (e.g. 5M tokens
	// over 90s emitted as "5M per Second" is ~90x too loose).
	requests := *b.MaxTokens * budgetUnitSeconds(unit) / int64(b.WindowSeconds)
	if requests < 1 {
		requests = 1
	}
	rule := map[string]interface{}{
		"limit": map[string]interface{}{
			"requests": requests,
			"unit":     unit,
		},
		// Token-denominated: request path is check-only; usage is charged from
		// the response's total-token dynamic metadata at completion.
		"cost": map[string]interface{}{
			"request": map[string]interface{}{
				"from":   "Number",
				"number": int64(0),
			},
			"response": map[string]interface{}{
				"from": "Metadata",
				"metadata": map[string]interface{}{
					"namespace": aiGatewayMetadataNamespace,
					"key":       aiGatewayTotalTokenKey(),
				},
			},
		},
	}

	// team scope keys an independent bucket per header value. Without auth (slice
	// 2d) this header is client-settable, so a team budget is only tamper-proof
	// once 2d derives HeaderKey from a verified JWT claim. We compile it now
	// because the rateLimit-by-header mechanism is identity-agnostic and useful
	// immediately; the status note warns operators of the auth pairing.
	if b.Scope == budgetScopeTeam {
		rule["clientSelectors"] = []interface{}{
			map[string]interface{}{
				"headers": []interface{}{
					map[string]interface{}{
						metadataNameField: teamHeaderKey(b),
						"type":            "Distinct",
					},
				},
			},
		}
	}
	return rule
}

// teamHeaderKey is the request header a team budget keys on, defaulting to
// x-llmkube-team when HeaderKey is unset (matches the CRD default).
func teamHeaderKey(b inferencev1alpha1.BudgetSpec) string {
	if b.HeaderKey != "" {
		return b.HeaderKey
	}
	return defaultTeamHeaderKey
}

// routerJWTHeaderKey is where the verified team claim lands: the explicit
// HeaderKey, else the x-llmkube-team default (matching team-scoped budgets).
func routerJWTHeaderKey(jwt *inferencev1alpha1.JWTAuthSpec) string {
	if jwt.HeaderKey != "" {
		return jwt.HeaderKey
	}
	return defaultTeamHeaderKey
}

// budgetWindowUnit picks the Envoy Gateway rateLimit unit the limit is
// denominated in: the LARGEST unit whose duration is <= WindowSeconds, so the
// scaled request count (see compileBudgetRule) stays a sensible integer rather
// than rounding toward zero. Envoy supports only a single unit
// (Second/Minute/Hour/Day), not "per N units"; compileBudgetRule scales
// MaxTokens to this unit so any window enforces the faithful average rate. Exact
// windows (60 -> Minute, 3600 -> Hour, 86400 -> Day) scale to requests ==
// MaxTokens. The CRD default is 3600 (Hour).
func budgetWindowUnit(windowSeconds int32) string {
	switch {
	case windowSeconds >= 86400:
		return "Day"
	case windowSeconds >= 3600:
		return "Hour"
	case windowSeconds >= 60:
		return "Minute"
	default:
		return "Second"
	}
}

// budgetUnitSeconds is the duration in seconds of a budgetWindowUnit value.
func budgetUnitSeconds(unit string) int64 {
	switch unit {
	case "Day":
		return 86400
	case "Hour":
		return 3600
	case "Minute":
		return 60
	default:
		return 1
	}
}

// newRouterSecurityPolicy builds the gateway.envoyproxy.io SecurityPolicy that
// validates inbound JWTs and maps the verified team claim onto a trusted header.
// It targets the generated HTTPRoute (which shares the AIGatewayRoute's name),
// like the BTP. The JWT filter runs before the AI extproc, so an invalid token
// is a 401 before any model dispatch. Mirrors the jwt stanza of the spike's
// 07-security-policy.yaml; the authorization stanza of that manifest is slice
// 2d.2 and deliberately NOT compiled here.
func newRouterSecurityPolicy(
	mr *inferencev1alpha1.ModelRouter,
	jwt *inferencev1alpha1.JWTAuthSpec,
	allowlists []inferencev1alpha1.TeamModelAllowlist,
) *unstructured.Unstructured {
	name := modelRouterGatewayResourceName(mr)
	spec := map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{
				"group":           gatewayBackendRefGroupAPI,
				"kind":            httpRouteKind,
				metadataNameField: name,
			},
		},
		"jwt": map[string]interface{}{
			"providers": []interface{}{
				map[string]interface{}{
					metadataNameField: jwt.Provider,
					"issuer":          jwt.Issuer,
					"remoteJWKS": map[string]interface{}{
						"uri": jwt.JWKSURI,
					},
					"claimToHeaders": []interface{}{
						map[string]interface{}{
							"claim":  jwt.TeamClaim,
							"header": routerJWTHeaderKey(jwt),
						},
					},
				},
			},
		},
	}

	// slice 2d.2: per-team model allowlists compile to the authorization block of
	// THIS SAME SecurityPolicy (authZ on top of authN). When there are no
	// allowlists we omit the key entirely so an authN-only router's SecurityPolicy
	// is byte-identical to slice 2d-core's output (adding the field must not
	// retroactively flip an existing router to default-Deny).
	if len(allowlists) > 0 {
		spec["authorization"] = compileRouterAuthorization(jwt, allowlists)
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(securityPolicyGVK())
	u.SetName(name)
	u.SetNamespace(mr.Namespace)
	u.Object["spec"] = spec
	return u
}

// compileRouterAuthorization turns the router's per-team model allowlists into
// the SecurityPolicy authorization block: defaultAction Deny plus one Allow rule
// per entry. A non-empty allowlist is the user opting into default-deny access
// control, so any verified team not named here is rejected (403). Each rule's
// principal matches the verified teamClaim value via principal.jwt.claims (using
// the SAME jwt.Provider the authentication block declares, as Envoy Gateway
// requires for claim authorization), and, when the entry lists models, restricts
// to those models via a principal.headers match on the resolved x-ai-eg-model
// header. Mirrors the authorization stanza of the spike's 07-security-policy.yaml.
func compileRouterAuthorization(
	jwt *inferencev1alpha1.JWTAuthSpec,
	allowlists []inferencev1alpha1.TeamModelAllowlist,
) map[string]interface{} {
	rules := make([]interface{}, 0, len(allowlists))
	for i, entry := range allowlists {
		rules = append(rules, compileAllowlistRule(jwt, entry, i))
	}
	return map[string]interface{}{
		// Deny is the secure default once authorization is configured: a verified
		// team with no matching Allow rule is rejected rather than served.
		"defaultAction": "Deny",
		"rules":         rules,
	}
}

// compileAllowlistRule compiles one allowlist entry into an Allow authorization
// rule. The principal always carries a jwt-claim match on the teamClaim; when the
// entry lists models it additionally carries an x-ai-eg-model header match (ANDed
// with the claim, since multiple principal types all must match). The entry's
// index i is folded into the rule name: the index suffix makes the name unique
// regardless of team content, so two distinct teams that sanitize to the same
// fragment (e.g. "team.b" and "team-b") still get distinct rule names.
func compileAllowlistRule(jwt *inferencev1alpha1.JWTAuthSpec, entry inferencev1alpha1.TeamModelAllowlist, i int) map[string]interface{} {
	principal := map[string]interface{}{
		"jwt": map[string]interface{}{
			"provider": jwt.Provider,
			"claims": []interface{}{
				map[string]interface{}{
					metadataNameField: jwt.TeamClaim,
					"values":          stringSlice(entry.Team),
				},
			},
		},
	}

	// Empty models == identity-only allow (team may reach all models): no header
	// restriction. Non-empty models restrict to those resolved model names. The AI
	// Gateway extproc sets the x-ai-eg-model header to the resolved model before
	// RBAC runs (validated in the spike), so it is the value to authorize on, the
	// same value spec.rules[].match.models route on.
	if len(entry.Models) > 0 {
		principal["headers"] = []interface{}{
			map[string]interface{}{
				metadataNameField: aiGatewayModelHeader,
				"values":          toInterfaceSlice(entry.Models),
			},
		}
	}

	return map[string]interface{}{
		metadataNameField: allowlistRuleName(entry.Team, i),
		"action":          "Allow",
		"principal":       principal,
	}
}

// allowlistRuleName builds a unique, DNS-safe name for the Allow rule of one
// allowlist entry. The index suffix guarantees uniqueness even when two
// distinct team values sanitize to the same fragment (e.g. "team.b" and
// "team-b"); the fragment is for human readability only.
func allowlistRuleName(team string, i int) string {
	frag := strings.ToLower(team)
	frag = nonDNSChar.ReplaceAllString(frag, "-") // any char not [a-z0-9-] -> '-'
	frag = strings.Trim(frag, "-")
	if frag == "" {
		frag = "team"
	}
	return fmt.Sprintf("allow-%s-%d", frag, i)
}

// stringSlice wraps a single string into the []interface{} the unstructured
// claim "values" array expects.
func stringSlice(s string) []interface{} {
	return []interface{}{s}
}

// toInterfaceSlice copies a []string into a []interface{} (the type the
// unstructured object requires for string arrays).
func toInterfaceSlice(in []string) []interface{} {
	out := make([]interface{}, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// modelRouterGatewayGVKs are the GVKs the ModelRouter gateway path needs the
// cluster to have registered before it generates anything: slice 1's three, the
// BackendTrafficPolicy (2a), and the SecurityPolicy (2d-core).
func modelRouterGatewayGVKs() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		backendGVK(),
		aiServiceBackendGVK(),
		aiGatewayRouteGVK(),
		btpGVK(),
		securityPolicyGVK(),
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
