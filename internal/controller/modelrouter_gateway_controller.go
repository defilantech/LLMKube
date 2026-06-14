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
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	modelRouterGatewayControllerName = "modelrouter-gateway"

	// ModelRouterGatewayConditionReady is the ModelRouter status condition type
	// the gateway reconciler owns. True once the gateway resources are
	// reconciled; False (with a reason) when disabled (aigw CRDs absent), when a
	// rule uses an unsupported match, or when reconciliation fails.
	ModelRouterGatewayConditionReady = "GatewayReady"

	// ModelRouter gateway condition reasons.
	modelRouterGatewayReasonExposed      = "Exposed"
	modelRouterGatewayReasonCRDsMissing  = "GatewayCRDsNotInstalled"
	modelRouterGatewayReasonReconcile    = "ReconcileFailed"
	modelRouterGatewayReasonUnsupported  = "UnsupportedMatchInGatewayMode"
	modelRouterGatewayReasonNoGatewayRef = "GatewayRefMissing"

	// slice 2b budget fail-loud reasons. Consistent with the 2a honest boundary:
	// the whole ModelRouter generates NOTHING and GatewayReady goes False.
	modelRouterGatewayReasonUnsupportedBudgetField = "UnsupportedBudgetField"
	modelRouterGatewayReasonUnsupportedBudgetScope = "UnsupportedBudgetScope"
	modelRouterGatewayReasonInvalidBudget          = "InvalidBudget"
)

// ModelRouterGatewayReconciler compiles a ModelRouter in dataPlane: Gateway mode
// into Envoy AI Gateway resources (a Backend + AIServiceBackend per
// InferenceServiceRef backend, one multi-rule AIGatewayRoute, and a
// retry/failover BackendTrafficPolicy). It is intentionally separate from the
// core ModelRouterReconciler (which owns the Proxy data plane) so the gateway
// integration stays cleanly optional and feature-flaggable, and so a cluster
// without the aigw CRDs runs the rest of the operator unaffected.
type ModelRouterGatewayReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// detector is the shared CRD-presence gate, lazily initialized on first
	// reconcile and reused thereafter. It requires slice 1's three kinds plus
	// the BackendTrafficPolicy this slice adds.
	detectorOnce sync.Once
	detector     *gatewayCRDDetector
}

// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=backendtrafficpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile compiles the gateway resources for a ModelRouter in dataPlane:
// Gateway mode, or no-ops cleanly when the router is in Proxy mode or when the
// aigw CRDs are not installed.
func (r *ModelRouterGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName(modelRouterGatewayControllerName)

	mr := &inferencev1alpha1.ModelRouter{}
	if err := r.Get(ctx, req.NamespacedName, mr); err != nil {
		if apierrors.IsNotFound(err) {
			// Owner-ref GC removes the generated resources when the ModelRouter
			// is deleted; nothing to do here.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Only the Gateway data plane is this reconciler's concern. Proxy mode (the
	// default) is owned by ModelRouterReconciler; we do not touch its status.
	if mr.Spec.DataPlane != inferencev1alpha1.ModelRouterDataPlaneGateway {
		return ctrl.Result{}, nil
	}

	// gatewayRef is required in Gateway mode. Fail loud rather than guessing a
	// Gateway.
	if mr.Spec.GatewayRef == nil {
		return ctrl.Result{}, r.setGatewayNotReady(ctx, mr, modelRouterGatewayReasonNoGatewayRef,
			"dataPlane is Gateway but spec.gatewayRef is unset; cannot attach a route")
	}

	// CRD-presence gate. When the aigw CRDs are absent we never create resources
	// and never requeue in a hot loop; we set a clear condition so the user sees
	// why nothing happened. A transient discovery error (not a missing kind) is
	// returned so we requeue rather than disabling.
	present, err := r.gatewayCRDsPresent(log)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !present {
		return ctrl.Result{}, r.setGatewayNotReady(ctx, mr, modelRouterGatewayReasonCRDsMissing,
			"Envoy AI Gateway CRDs are not installed; gateway exposure is disabled")
	}

	// Fail-loud on matches the gateway data plane cannot express. We generate
	// NOTHING for such a router rather than silently dropping a rule the user
	// expects (proposal 5.2 honest boundary).
	if msg := unsupportedMatchMessage(mr); msg != "" {
		return ctrl.Result{}, r.setGatewayNotReady(ctx, mr, modelRouterGatewayReasonUnsupported, msg)
	}

	// Fail-loud on budgets the gateway data plane cannot honor (dollar budgets,
	// rule scope, or a budget with no cap). Same ordering as the match check:
	// runs BEFORE any CreateOrUpdate so a rejected budget yields no partial
	// generation (proposal 5.2 budget honest boundary).
	if reason, msg := unsupportedBudgetMessage(mr); reason != "" {
		return ctrl.Result{}, r.setGatewayNotReady(ctx, mr, reason, msg)
	}

	if err := r.reconcileGatewayResources(ctx, mr); err != nil {
		_ = r.setGatewayNotReady(ctx, mr, modelRouterGatewayReasonReconcile, err.Error())
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.setGatewayReady(ctx, mr)
}

// reconcileGatewayResources resolves the router's backends to cluster FQDNs,
// compiles the backends + rules into the gateway resource set, and
// creates-or-updates each one owner-referenced to the ModelRouter.
func (r *ModelRouterGatewayReconciler) reconcileGatewayResources(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) error {
	backends, err := r.resolveBackends(ctx, mr)
	if err != nil {
		return err
	}

	rules, err := compileRouterRules(mr)
	if err != nil {
		return err
	}

	budgets := routerBudgets(mr)

	// Order matters for clean upserts: Backends and AIServiceBackends before the
	// route that references them, then the BTP that targets the route.
	desired := make([]*unstructured.Unstructured, 0, len(backends)*2+2)
	for _, b := range backends {
		desired = append(desired, newRouterBackend(mr, b), newRouterAIServiceBackend(mr, b))
	}
	desired = append(desired,
		newRouterAIGatewayRoute(mr, mr.Spec.GatewayRef, rules, budgets),
		newRouterBackendTrafficPolicy(mr, budgets),
	)

	for _, obj := range desired {
		if err := r.applyResource(ctx, mr, obj); err != nil {
			return fmt.Errorf("%s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return nil
}

// resolveBackends turns every InferenceServiceRef backend into a
// routerBackendResource (name + cluster FQDN + port). External backends are
// deferred from slice 2a; encountering one is a hard error so the user is not
// silently missing a backend. A backend referencing a missing InferenceService
// is also an error (we cannot point a Backend at a Service that does not exist).
func (r *ModelRouterGatewayReconciler) resolveBackends(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) ([]routerBackendResource, error) {
	resolved := make([]routerBackendResource, 0, len(mr.Spec.Backends))
	for _, b := range mr.Spec.Backends {
		if b.External != nil {
			return nil, fmt.Errorf("backend %q is External; external backends are not supported in dataPlane: Gateway yet", b.Name)
		}
		if b.InferenceServiceRef == nil {
			return nil, fmt.Errorf("backend %q has no inferenceServiceRef", b.Name)
		}

		isvc := &inferencev1alpha1.InferenceService{}
		key := types.NamespacedName{Name: b.InferenceServiceRef.Name, Namespace: mr.Namespace}
		if err := r.Get(ctx, key, isvc); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("backend %q references InferenceService %q which does not exist in namespace %q",
					b.Name, b.InferenceServiceRef.Name, mr.Namespace)
			}
			return nil, fmt.Errorf("backend %q: looking up InferenceService %q: %w", b.Name, b.InferenceServiceRef.Name, err)
		}

		port := int64(8080)
		if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
			port = int64(isvc.Spec.Endpoint.Port)
		}
		resolved = append(resolved, routerBackendResource{
			Name: b.Name,
			FQDN: fmt.Sprintf("%s.%s.svc.cluster.local", sanitizeDNSName(isvc.Name), isvc.Namespace),
			Port: port,
		})
	}
	return resolved, nil
}

// applyResource owner-references desired to the ModelRouter and
// creates-or-updates it. The desired spec is captured before CreateOrUpdate so
// the mutate function (which sees the live object on update) overwrites spec to
// correct drift while preserving server-managed metadata. Mirrors slice 1's
// applyResource.
func (r *ModelRouterGatewayReconciler) applyResource(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	desired *unstructured.Unstructured,
) error {
	desiredSpec, _, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil {
		return err
	}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(desired.GroupVersionKind())
	live.SetName(desired.GetName())
	live.SetNamespace(desired.GetNamespace())

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, live, func() error {
		live.Object["spec"] = desiredSpec
		return setControllerReferenceUnblocked(mr, live, r.Scheme)
	})
	return err
}

// gatewayCRDsPresent reports whether all gateway CRDs this slice needs are
// registered, delegating to the shared gatewayCRDDetector.
func (r *ModelRouterGatewayReconciler) gatewayCRDsPresent(log logr.Logger) (bool, error) {
	r.detectorOnce.Do(func() {
		r.detector = newGatewayCRDDetector(modelRouterGatewayGVKs())
	})
	return r.detector.Present(r.Client, log)
}

// setGatewayReady writes the success status: GatewayReady=True plus
// status.gateway with the resolved endpoint.
func (r *ModelRouterGatewayReconciler) setGatewayReady(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) error {
	patch := client.MergeFrom(mr.DeepCopy())
	mr.Status.Gateway = &inferencev1alpha1.GatewayStatus{
		RouteReady: true,
		Endpoint:   gatewayEndpointAddress(mr.Spec.GatewayRef),
	}
	apimeta.SetStatusCondition(&mr.Status.Conditions, metav1.Condition{
		Type:    ModelRouterGatewayConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  modelRouterGatewayReasonExposed,
		Message: gatewayReadyMessage(mr),
	})
	return r.Status().Patch(ctx, mr, patch)
}

// gatewayReadyMessage builds the success condition message: the rule count, the
// resolved endpoint, and a budget summary. When any team-scope budget compiled,
// it appends the auth-pairing caveat so an operator is not misled into thinking
// a header-keyed budget is tamper-proof on its own (it becomes so once slice 2d
// derives the header from a verified JWT claim).
func gatewayReadyMessage(mr *inferencev1alpha1.ModelRouter) string {
	msg := fmt.Sprintf("compiled %d rule(s) onto %s", len(mr.Spec.Rules), gatewayEndpointAddress(mr.Spec.GatewayRef))

	budgets := routerBudgets(mr)
	if len(budgets) == 0 {
		return msg
	}

	hasTeam := false
	for _, b := range budgets {
		if b.Scope == budgetScopeTeam {
			hasTeam = true
			break
		}
	}
	msg += fmt.Sprintf("; enforced %d token budget(s)", len(budgets))
	if hasTeam {
		msg += " (team-scoped budgets key on a request header and are tamper-proof only once gateway auth derives that header from a verified identity)"
	}
	return msg
}

// setGatewayNotReady writes a False GatewayReady condition and clears any stale
// RouteReady so status reflects reality. Used on every disabled / unsupported /
// failure path (the success path is setGatewayReady).
func (r *ModelRouterGatewayReconciler) setGatewayNotReady(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	reason, message string,
) error {
	patch := client.MergeFrom(mr.DeepCopy())
	if mr.Status.Gateway == nil {
		mr.Status.Gateway = &inferencev1alpha1.GatewayStatus{}
	}
	mr.Status.Gateway.RouteReady = false
	apimeta.SetStatusCondition(&mr.Status.Conditions, metav1.Condition{
		Type:    ModelRouterGatewayConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
	return r.Status().Patch(ctx, mr, patch)
}

// SetupWithManager wires the ModelRouter gateway reconciler to watch
// ModelRouters.
//
// As in slice 1 we intentionally do not Owns() the generated resources: the
// operator may run where the aigw CRDs are absent, and an Owns watch on an
// unregistered kind fails manager startup. The ModelRouter primary watch plus
// CreateOrUpdate's drift correction is sufficient.
func (r *ModelRouterGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.ModelRouter{}).
		Named(modelRouterGatewayControllerName).
		Complete(r)
}

// compileRouterRules turns the ModelRouter's spec.rules (plus a trailing
// catch-all for defaultRoute) into resolved routerRuleResources. It assumes
// matches were already vetted by unsupportedMatchMessage. Strategy decides
// whether backendRefs carry priority (primary-fallback) or weight (weighted).
func compileRouterRules(mr *inferencev1alpha1.ModelRouter) ([]routerRuleResource, error) {
	weights := backendWeights(mr)

	rules := make([]routerRuleResource, 0, len(mr.Spec.Rules)+1)
	for _, rule := range mr.Spec.Rules {
		refs, err := compileBackendRefs(rule.Name, rule.Route, weights)
		if err != nil {
			return nil, err
		}
		resolved := routerRuleResource{BackendRefs: refs}
		if rule.Match != nil {
			resolved.Models = rule.Match.Models
			resolved.Headers = rule.Match.Headers
		}
		rules = append(rules, resolved)
	}

	// defaultRoute compiles to a trailing catch-all rule (no model/header match)
	// routing to the named backend.
	if mr.Spec.DefaultRoute != "" {
		rules = append(rules, routerRuleResource{
			BackendRefs: []routerBackendRef{{Name: mr.Spec.DefaultRoute}},
		})
	}

	return rules, nil
}

// compileBackendRefs turns a rule's route.backends + strategy into ordered
// backendRefs. primary-fallback assigns ascending priorities (0 = primary);
// weighted assigns each backend's declared Weight (default 1). The shadow
// strategy has no gateway equivalent and is rejected (caught earlier by
// unsupportedMatchMessage, defended again here).
func compileBackendRefs(ruleName string, route inferencev1alpha1.RuleRoute, weights map[string]int64) ([]routerBackendRef, error) {
	refs := make([]routerBackendRef, 0, len(route.Backends))
	switch route.Strategy {
	case "", "primary-fallback":
		for i, name := range route.Backends {
			priority := int64(i)
			refs = append(refs, routerBackendRef{Name: name, Priority: &priority})
		}
	case weightedStrategy:
		for _, name := range route.Backends {
			weight := weights[name]
			refs = append(refs, routerBackendRef{Name: name, Weight: &weight})
		}
	default:
		return nil, fmt.Errorf("rule %q uses strategy %q which has no gateway equivalent", ruleName, route.Strategy)
	}
	return refs, nil
}

// backendWeights maps each backend name to its declared Weight (default 1 when
// unset), used by the weighted strategy.
func backendWeights(mr *inferencev1alpha1.ModelRouter) map[string]int64 {
	weights := make(map[string]int64, len(mr.Spec.Backends))
	for _, b := range mr.Spec.Backends {
		w := int64(1)
		if b.Weight != nil {
			w = int64(*b.Weight)
		}
		weights[b.Name] = w
	}
	return weights
}

const weightedStrategy = "weighted"

// routerBudgets returns the router's compiled budgets (nil-safe). Callers use it
// to decide whether to emit the rateLimit/llmRequestCosts stanzas; an empty
// slice means a 2a-only router whose generated resources stay byte-identical to
// #693's output.
func routerBudgets(mr *inferencev1alpha1.ModelRouter) []inferencev1alpha1.BudgetSpec {
	if mr.Spec.Policy == nil {
		return nil
	}
	return mr.Spec.Policy.Budgets
}

// unsupportedBudgetMessage returns a fail-loud (reason, message) for the first
// budget the gateway data plane cannot honor, or ("", "") when every budget
// compiles. Like unsupportedMatchMessage it runs BEFORE generation so a rejected
// budget yields no partial resources. The rejected cases (proposal 5.2 budget
// boundary):
//   - MaxUSD set: dollar budgets need a token-equivalent conversion that is
//     undecided across heterogeneous backend costs (UnsupportedBudgetField).
//   - Scope=rule: per-rule rateLimit clientSelectors are deferred
//     (UnsupportedBudgetScope).
//   - neither MaxTokens nor MaxUSD set: the CRD requires at least one; we guard
//     anyway so a malformed budget never silently compiles to no limit
//     (InvalidBudget).
//
// team scope is accepted (compiled now, tamper-proof once auth lands in 2d).
func unsupportedBudgetMessage(mr *inferencev1alpha1.ModelRouter) (reason, message string) {
	for _, b := range routerBudgets(mr) {
		if b.MaxUSD != "" {
			return modelRouterGatewayReasonUnsupportedBudgetField,
				fmt.Sprintf("budget %q sets maxUSD; dollar budgets are not yet supported in dataPlane: Gateway "+
					"(the gateway rate-limits on tokens, and there is no single honest token conversion across "+
					"heterogeneous backend costs). Use maxTokens, or track maxUSD via the Proxy data plane", b.Name)
		}
		if b.Scope == budgetScopeRule {
			return modelRouterGatewayReasonUnsupportedBudgetScope,
				fmt.Sprintf("budget %q uses scope %q, which is not yet supported in dataPlane: Gateway; "+
					"use scope router (total cap) or team (per-tenant cap)", b.Name, budgetScopeRule)
		}
		if b.MaxTokens == nil {
			return modelRouterGatewayReasonInvalidBudget,
				fmt.Sprintf("budget %q sets neither maxTokens nor maxUSD; at least one cap is required", b.Name)
		}
	}
	return "", ""
}

// unsupportedMatchMessage returns a non-empty message naming the first rule
// whose match uses a condition the gateway data plane cannot express
// (dataClassification, taskComplexity, requiredCapabilities, latencySLOMs), or
// a strategy with no gateway equivalent (shadow). Empty means every rule
// compiles. Only model-name (Models) and Headers matches, with primary-fallback
// or weighted strategies, compile (proposal 5.2 honest boundary).
func unsupportedMatchMessage(mr *inferencev1alpha1.ModelRouter) string {
	for _, rule := range mr.Spec.Rules {
		if rule.Match != nil {
			if unsupported := unsupportedMatchFields(rule.Match); len(unsupported) > 0 {
				return fmt.Sprintf("rule %q uses %s, which the gateway data plane cannot match; only model name and headers are supported in dataPlane: Gateway",
					rule.Name, strings.Join(unsupported, ", "))
			}
		}
		if s := rule.Route.Strategy; s != "" && s != "primary-fallback" && s != weightedStrategy {
			return fmt.Sprintf("rule %q uses strategy %q, which has no gateway equivalent; use primary-fallback or weighted in dataPlane: Gateway",
				rule.Name, s)
		}
	}
	return ""
}

// unsupportedMatchFields lists the gateway-inexpressible match fields set on a
// RuleMatch, in a stable order.
func unsupportedMatchFields(m *inferencev1alpha1.RuleMatch) []string {
	var fields []string
	if len(m.DataClassification) > 0 {
		fields = append(fields, "dataClassification")
	}
	if m.TaskComplexity != "" {
		fields = append(fields, "taskComplexity")
	}
	if len(m.RequiredCapabilities) > 0 {
		fields = append(fields, "requiredCapabilities")
	}
	if m.LatencySLOMs != nil {
		fields = append(fields, "latencySLOMs")
	}
	if hasGlobModel(m.Models) {
		fields = append(fields, "glob model pattern")
	}
	sort.Strings(fields)
	return fields
}

// hasGlobModel reports whether any model pattern carries glob metacharacters.
// Proxy mode matches these via path.Match; the gateway data plane can only do
// an Exact x-ai-eg-model header match, so a glob would compile to a literal
// that never fires. We fail loud rather than silently route nothing.
func hasGlobModel(models []string) bool {
	for _, mdl := range models {
		if strings.ContainsAny(mdl, "*?[") {
			return true
		}
	}
	return false
}
