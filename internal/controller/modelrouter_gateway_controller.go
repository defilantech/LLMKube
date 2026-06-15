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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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

	// slice 2d-core auth fail-loud reason.
	modelRouterGatewayReasonInvalidAuth = "InvalidAuth"

	// slice 2d.2 authorization (per-team model allowlist) fail-loud reasons.
	// AuthorizationRequiresJWT is the structural prerequisite (you cannot
	// authorize on an unverified claim); InvalidAuthorization covers a malformed
	// allowlist (empty or duplicate team). Both generate NOTHING and set
	// GatewayReady False, consistent with the 2a/2b/2d-core honest boundary.
	modelRouterGatewayReasonAuthzRequiresJWT = "AuthorizationRequiresJWT"
	modelRouterGatewayReasonInvalidAuthz     = "InvalidAuthorization"

	// slice 2e-core sensitive-route fail-loud reason. A rule whose
	// dataClassification intersects the sensitive set MUST be failClosed and route
	// only to local-tier backends; otherwise the whole ModelRouter generates
	// NOTHING and GatewayReady goes False. This makes "sensitive data never
	// egresses to a cloud tier" structural rather than advisory.
	modelRouterGatewayReasonUnsafeSensitiveRoute = "UnsafeSensitiveRoute"

	// slice 2e-core classification defaults.
	classificationModeHeaderOnly   = "header-only"
	defaultClassificationHeaderKey = "x-llmkube-classification"

	// slice 2c audit-log fail-loud reason. policy.auditLog is a Proxy-mode-only
	// field (it names the router-proxy container and a file path); in Gateway mode
	// it has no per-router meaning, so it is refused loudly rather than silently
	// ignored. Consistent with the 2a/2b/2d honest boundary: GatewayReady goes
	// False and the router generates NOTHING.
	modelRouterGatewayReasonUnsupportedAuditLog = "UnsupportedAuditLogInGatewayMode"
)

// defaultSensitiveClassifications mirrors the CRD default for
// policy.classification.sensitiveClassifications. It is the sensitive set used
// by the fail-closed guard when the operator did not customize the list (or when
// policy.classification is unset entirely).
var defaultSensitiveClassifications = []string{"pii", "phi"}

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
// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=securitypolicies,verbs=get;list;watch;create;update;patch;delete

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

	// Fail-loud on a half-configured auth block. A SecurityPolicy missing a
	// required JWT field would either fail open or reject everything; we generate
	// NOTHING and surface why, same ordering as the match check (before any
	// CreateOrUpdate).
	if msg := invalidAuthMessage(mr); msg != "" {
		return ctrl.Result{}, r.setGatewayNotReady(ctx, mr, modelRouterGatewayReasonInvalidAuth, msg)
	}

	// Fail-loud on a malformed authorization (per-team model allowlist) config:
	// allowlists without JWT, or an empty/duplicate team. Same ordering as the
	// auth check (before any CreateOrUpdate); a non-empty allowlist makes the
	// SecurityPolicy default-Deny, so a malformed one must never be emitted (it
	// would silently lock everyone out).
	if reason, msg := invalidAuthorizationMessage(mr); reason != "" {
		return ctrl.Result{}, r.setGatewayNotReady(ctx, mr, reason, msg)
	}

	// Fail-loud on a per-router auditLog directive: policy.auditLog is a Proxy-mode
	// field with no per-route Gateway equivalent (Envoy Gateway access logging is
	// configured on the gateway-scoped EnvoyProxy, not per route). Silently ignoring
	// an audit directive a user believes is active is a compliance footgun, so we
	// generate NOTHING and point the operator at the gateway-level access-log config.
	// Same ordering as the auth checks (before any CreateOrUpdate).
	if msg := unsupportedAuditLogMessage(mr); msg != "" {
		return ctrl.Result{}, r.setGatewayNotReady(ctx, mr, modelRouterGatewayReasonUnsupportedAuditLog, msg)
	}

	// Fail-loud on a sensitive-classification rule that is not fail-closed and
	// local-tier only: a rule that DECLARES a pii/phi dataClassification but lacks
	// fail-closed, or routes to a cloud-tier backend, cannot be compiled. Placed
	// AFTER the unsupported-match check (so a rule with an inexpressible match is
	// reported first) and BEFORE generation (so a rejected rule yields no partial
	// resources).
	//
	// SCOPE: this guard is per-declaring-rule, not a global PII-egress invariant.
	// It only inspects rules whose Match declares a sensitive class; a model-only,
	// catch-all, or defaultRoute rule that carries pii-headed traffic to some other
	// backend is NOT inspected here. The global "PII never reaches a cloud tier"
	// property additionally relies on Gateway mode having NO cloud/external backends
	// at all today (resolveBackends hard-errors on External, and CRD validation
	// rejects tier=cloud on InferenceServiceRef backends), so no cloud egress path
	// is currently expressible. When Gateway mode gains cloud/external backends, the
	// non-declaring egress paths (defaultRoute, model-only rules) need their own
	// handling; the deferred in-proxy classifier (2e-detector) is where real
	// content-based enforcement lands.
	if msg := unsafeSensitiveRouteMessage(mr); msg != "" {
		return ctrl.Result{}, r.setGatewayNotReady(ctx, mr, modelRouterGatewayReasonUnsafeSensitiveRoute, msg)
	}

	ejected, err := r.reconcileGatewayResources(ctx, mr)
	if err != nil {
		_ = r.setGatewayNotReady(ctx, mr, modelRouterGatewayReasonReconcile, err.Error())
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.setGatewayReady(ctx, mr, ejected)
}

// reconcileGatewayResources resolves the router's backends to cluster FQDNs,
// compiles the backends + rules into the gateway resource set, and
// creates-or-updates each one owner-referenced to the ModelRouter.
func (r *ModelRouterGatewayReconciler) reconcileGatewayResources(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) ([]string, error) {
	backends, err := r.resolveBackends(ctx, mr)
	if err != nil {
		return nil, err
	}

	rules, err := compileRouterRules(mr)
	if err != nil {
		return nil, err
	}

	budgets := routerBudgets(mr)

	// Order matters for clean upserts: Backends and AIServiceBackends before the
	// route that references them, then the BTP that targets the route. Every
	// backend (healthy or not) still gets a Backend + AIServiceBackend so an
	// ejected backend exists to be probed and re-added on recovery; only the
	// route's backendRefs are filtered below.
	desired := make([]*unstructured.Unstructured, 0, len(backends)*2+3)
	for _, b := range backends {
		desired = append(desired, newRouterBackend(mr, b), newRouterAIServiceBackend(mr, b))
	}

	// When auth.jwt is configured, apply the SecurityPolicy BEFORE the route, so
	// the JWT filter is in place the moment the generated HTTPRoute appears,
	// minimizing any window where the route could serve unauthenticated. A router
	// with no auth produces no SecurityPolicy. (targetRef may reference the route
	// before it exists; Envoy Gateway attaches it once the route appears.)
	if jwt := routerJWT(mr); jwt != nil {
		desired = append(desired, newRouterSecurityPolicy(mr, jwt, routerAllowlists(mr)))
	}

	// Slice 4b: drop unhealthy backends from the route's rule backendRefs so Envoy
	// fails over to a healthy backend, the instant the health signal changes. The
	// Backend/AIServiceBackend objects above are generated for ALL backends; only
	// the route is filtered, and a rule is never emptied (see ejectUnhealthyBackends).
	rules, ejected := ejectUnhealthyBackends(rules, backends)

	desired = append(desired,
		newRouterAIGatewayRoute(mr, mr.Spec.GatewayRef, rules, budgets),
		newRouterBackendTrafficPolicy(mr, budgets),
	)

	for _, obj := range desired {
		if err := r.applyResource(ctx, mr, obj); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return ejected, nil
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
			// A backend is healthy iff its InferenceService has at least one ready
			// replica. This is the single aggregate both prior layers drive to 0:
			// Slice 3's metal withdrawal flips the endpoint Ready:false, #663's
			// heartbeat-staleness expiry zeroes it, and pod backends get it from pod
			// readiness. An unhealthy backend is ejected from the route's
			// backendRefs (slice 4b) while its Backend/AIServiceBackend stay in
			// place for re-add on recovery.
			Healthy: isvc.Status.ReadyReplicas > 0,
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
	ejected []string,
) error {
	patch := client.MergeFrom(mr.DeepCopy())
	authEnabled := routerJWT(mr) != nil
	mr.Status.Gateway = &inferencev1alpha1.GatewayStatus{
		RouteReady:  true,
		Endpoint:    gatewayEndpointAddress(mr.Spec.GatewayRef),
		AuthEnabled: authEnabled,
	}
	message := gatewayReadyMessage(mr)
	if authEnabled {
		message += "; JWT authentication enforced"
		// Allowlists imply JWT (invalidAuthorizationMessage rejects allowlists
		// without it), so this only appends on an already-authenticated router.
		if n := len(routerAllowlists(mr)); n > 0 {
			message += fmt.Sprintf("; %d team model allowlist(s) enforced", n)
		}
	}
	// Slice 4b: surface route degradation so an operator sees the route is
	// serving on its healthy backends while some are ejected.
	if len(ejected) > 0 {
		message += fmt.Sprintf("; ejected %d unhealthy backend(s): %s", len(ejected), strings.Join(ejected, ", "))
	}
	apimeta.SetStatusCondition(&mr.Status.Conditions, metav1.Condition{
		Type:    ModelRouterGatewayConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  modelRouterGatewayReasonExposed,
		Message: message,
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
		Watches(
			&inferencev1alpha1.InferenceService{},
			handler.EnqueueRequestsFromMapFunc(r.modelRoutersForInferenceService),
		).
		Named(modelRouterGatewayControllerName).
		Complete(r)
}

// modelRoutersForInferenceService maps a changed InferenceService to the
// dataPlane: Gateway ModelRouters that reference it, so a backend's readiness
// flip re-reconciles the route (slice 4b ejection/restore) within a reconcile
// rather than at the active-probe interval. Reconcile is idempotent
// (CreateOrUpdate no-ops on an unchanged route), so firing on unrelated
// InferenceService updates is harmless. Proxy-mode routers are skipped: their
// route is owned by ModelRouterReconciler, not this reconciler. Mirrors
// ModelRouterReconciler.findModelRoutersForInferenceService.
func (r *ModelRouterGatewayReconciler) modelRoutersForInferenceService(ctx context.Context, obj client.Object) []reconcile.Request {
	isvc, ok := obj.(*inferencev1alpha1.InferenceService)
	if !ok {
		return nil
	}

	routerList := &inferencev1alpha1.ModelRouterList{}
	if err := r.List(ctx, routerList, client.InNamespace(isvc.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for i := range routerList.Items {
		mr := &routerList.Items[i]
		if mr.Spec.DataPlane != inferencev1alpha1.ModelRouterDataPlaneGateway {
			continue
		}
		if routerReferencesInferenceService(mr, isvc.Name) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
			})
		}
	}
	return requests
}

// compileRouterRules turns the ModelRouter's spec.rules (plus a trailing
// catch-all for defaultRoute) into resolved routerRuleResources. It assumes
// matches were already vetted by unsupportedMatchMessage. Strategy decides
// whether backendRefs carry priority (primary-fallback) or weight (weighted).
func compileRouterRules(mr *inferencev1alpha1.ModelRouter) ([]routerRuleResource, error) {
	weights := backendWeights(mr)
	// header-only is the only mode that reaches compilation with a
	// dataClassification match; detector/hybrid are rejected earlier by
	// unsupportedMatchMessage. Gate on it defensively so we never emit a
	// classification header match in a mode whose semantics differ.
	headerOnly := classificationMode(mr) == classificationModeHeaderOnly
	classHeaderKey := classificationHeaderKey(mr)

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
			if headerOnly && len(rule.Match.DataClassification) > 0 {
				resolved.DataClassifications = rule.Match.DataClassification
				resolved.ClassificationHeaderKey = classHeaderKey
			}
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

// classificationMode returns the router's classification mode, defaulted to
// header-only when policy.classification (or the policy itself) is unset. It
// decides whether a dataClassification match is expressible (header-only) or
// fail-loud (detector/hybrid; the in-proxy classifier is not built yet).
func classificationMode(mr *inferencev1alpha1.ModelRouter) string {
	if mr.Spec.Policy == nil || mr.Spec.Policy.Classification == nil || mr.Spec.Policy.Classification.Mode == "" {
		return classificationModeHeaderOnly
	}
	return mr.Spec.Policy.Classification.Mode
}

// classificationHeaderKey returns the request header carrying the data
// classification, defaulted to defaultClassificationHeaderKey when unset. A
// header-only dataClassification match compiles to an Exact match on this header.
func classificationHeaderKey(mr *inferencev1alpha1.ModelRouter) string {
	if mr.Spec.Policy == nil || mr.Spec.Policy.Classification == nil || mr.Spec.Policy.Classification.HeaderKey == "" {
		return defaultClassificationHeaderKey
	}
	return mr.Spec.Policy.Classification.HeaderKey
}

// sensitiveClassifications returns the classification values that trigger the
// fail-closed sensitive-route guard, defaulted to the CRD default when unset.
// The default applies even when policy.classification is nil: a rule that
// declares a pii/phi dataClassification is sensitive regardless of whether the
// operator customized the list.
func sensitiveClassifications(mr *inferencev1alpha1.ModelRouter) []string {
	if mr.Spec.Policy != nil && mr.Spec.Policy.Classification != nil &&
		len(mr.Spec.Policy.Classification.SensitiveClassifications) > 0 {
		return mr.Spec.Policy.Classification.SensitiveClassifications
	}
	return defaultSensitiveClassifications
}

// unsafeSensitiveRouteMessage returns a non-empty message for the first rule
// whose dataClassification intersects the sensitive set (Policy.Classification.
// SensitiveClassifications, defaulted to ["pii","phi"]) but is not safely
// constrained: it must have Route.FailClosed == true AND route only to local-tier
// backends. Empty means every sensitive-DECLARING rule is safe (or no rule
// declares a sensitive class). Like the other honest-boundary checks it runs
// BEFORE generation so a rejected rule yields no partial resources (proposal 5.2
// fail-closed sensitive guard).
//
// This is per-declaring-rule, NOT a global PII-egress invariant: a rule that does
// not declare a sensitive dataClassification (a model-only rule, a catch-all, or
// the defaultRoute) is not inspected, even if pii-headed traffic would match it.
// That is sound today only because Gateway mode cannot express a cloud/external
// backend at all (see the reconcile-site comment); when it can, the non-declaring
// egress paths need separate handling.
func unsafeSensitiveRouteMessage(mr *inferencev1alpha1.ModelRouter) string {
	sensitive := make(map[string]struct{}, len(defaultSensitiveClassifications))
	for _, c := range sensitiveClassifications(mr) {
		sensitive[c] = struct{}{}
	}

	tiers := backendTiers(mr)

	for _, rule := range mr.Spec.Rules {
		if rule.Match == nil {
			continue
		}
		hit := firstSensitiveClassification(rule.Match.DataClassification, sensitive)
		if hit == "" {
			continue
		}
		if !rule.FailClosed {
			return fmt.Sprintf("rule %q matches sensitive classification %q but is not failClosed", rule.Name, hit)
		}
		for _, name := range rule.Route.Backends {
			if tiers[name] != backendTierLocal {
				return fmt.Sprintf("rule %q matches sensitive classification %q but routes to non-local backend %q",
					rule.Name, hit, name)
			}
		}
	}
	return ""
}

// firstSensitiveClassification returns the first declared classification that is
// in the sensitive set (iterating the rule's declared order for a deterministic
// message), or "" when the rule declares none.
func firstSensitiveClassification(declared []string, sensitive map[string]struct{}) string {
	for _, c := range declared {
		if _, ok := sensitive[c]; ok {
			return c
		}
	}
	return ""
}

// backendTiers maps each backend name to its tier, defaulting an unset tier to
// "local" per the RouterBackend.Tier CRD default. A backend name not present in
// the map (e.g. a typo in Route.Backends) resolves to "" and is treated as
// non-local by the sensitive guard, which is the safe direction.
func backendTiers(mr *inferencev1alpha1.ModelRouter) map[string]string {
	tiers := make(map[string]string, len(mr.Spec.Backends))
	for _, b := range mr.Spec.Backends {
		tier := b.Tier
		if tier == "" {
			tier = backendTierLocal
		}
		tiers[b.Name] = tier
	}
	return tiers
}

// routerJWT returns the configured JWT auth block, or nil when the router
// declares no policy.auth.jwt.
func routerJWT(mr *inferencev1alpha1.ModelRouter) *inferencev1alpha1.JWTAuthSpec {
	if mr.Spec.Policy == nil || mr.Spec.Policy.Auth == nil {
		return nil
	}
	return mr.Spec.Policy.Auth.JWT
}

// routerAllowlists returns the configured per-team model allowlists (nil-safe
// through policy.auth). nil/empty means no authorization is enforced; a non-empty
// slice flips the generated SecurityPolicy to default-Deny. Mirrors routerJWT.
func routerAllowlists(mr *inferencev1alpha1.ModelRouter) []inferencev1alpha1.TeamModelAllowlist {
	if mr.Spec.Policy == nil || mr.Spec.Policy.Auth == nil {
		return nil
	}
	return mr.Spec.Policy.Auth.Allowlists
}

// unsupportedAuditLogMessage returns a non-empty message when policy.auditLog is
// set on a router in dataPlane: Gateway mode, or "" when it is absent. Any
// auditLog block present means the user asked for per-router audit, which is a
// Proxy-mode-only feature (it names the router-proxy container and a file path).
// Envoy Gateway access logging is configured on the gateway-scoped EnvoyProxy
// (telemetry.accessLog), not per route, and the operator does not own that
// external gateway infra, so a per-router auditLog has no Gateway equivalent. Like
// invalidAuthMessage it runs BEFORE generation so a rejected auditLog yields no
// partial resources; refusing loudly avoids silently dropping an audit directive
// (a compliance footgun). The field stays fully valid in Proxy mode.
func unsupportedAuditLogMessage(mr *inferencev1alpha1.ModelRouter) string {
	if mr.Spec.Policy == nil || mr.Spec.Policy.AuditLog == nil {
		return ""
	}
	return "policy.auditLog is a Proxy-mode field with no per-router equivalent in dataPlane: Gateway; " +
		"Envoy Gateway access logging is configured on the gateway-scoped EnvoyProxy (telemetry.accessLog), " +
		"not per route. Enable the chart's gateway.auditLog values to ship the audit EnvoyProxy and reference " +
		"it from your GatewayClass/Gateway parametersRef, or remove policy.auditLog"
}

// invalidAuthMessage returns a non-empty message when policy.auth.jwt is set but
// missing a required field (issuer, jwksURI, or teamClaim). Empty means auth is
// absent or fully configured. CRD validation should also reject these, but the
// reconciler defends fail-loud so a partial SecurityPolicy is never emitted. The
// provider name is structurally required too; an empty one is reported here for
// completeness.
func invalidAuthMessage(mr *inferencev1alpha1.ModelRouter) string {
	jwt := routerJWT(mr)
	if jwt == nil {
		return ""
	}
	var missing []string
	if jwt.Provider == "" {
		missing = append(missing, "provider")
	}
	if jwt.Issuer == "" {
		missing = append(missing, "issuer")
	}
	if jwt.JWKSURI == "" {
		missing = append(missing, "jwksURI")
	}
	if jwt.TeamClaim == "" {
		missing = append(missing, "teamClaim")
	}
	if len(missing) == 0 {
		return ""
	}
	sort.Strings(missing)
	return fmt.Sprintf("policy.auth.jwt is set but missing required field(s): %s", strings.Join(missing, ", "))
}

// invalidAuthorizationMessage returns a fail-loud (reason, message) for a
// malformed authorization (per-team model allowlist) configuration, or ("", "")
// when allowlists are absent or well-formed. Like the budget and auth guards it
// runs BEFORE generation so a rejected allowlist yields no SecurityPolicy (whose
// default-Deny could otherwise silently lock everyone out). The rejected cases
// (proposal 5.2 authorization honest boundary):
//   - allowlists set without policy.auth.jwt: authorization matches on a verified
//     JWT claim, so it cannot stand without authentication
//     (AuthorizationRequiresJWT).
//   - an entry with a semantically-empty Team (empty or whitespace-only): a
//     default-Deny policy with a malformed principal would not allow the intended
//     team (InvalidAuthorization). CRD MinLength=1 only catches the truly-empty
//     case; " " slips through, so the reconciler trims before checking.
//   - an entry whose Team has leading/trailing whitespace: it can never match the
//     exact JWT claim, so it is a silent-deny footgun (InvalidAuthorization).
//   - a duplicate Team across entries: ambiguous intent (InvalidAuthorization).
//     Detected on the raw Team string; distinct teams that merely sanitize to the
//     same rule-name fragment are NOT duplicates (allowlistRuleName disambiguates
//     them with an index suffix).
//
// CRD validation also rejects an empty Team (Required+MinLength), but the
// reconciler defends fail-loud so a partial authorization is never emitted.
func invalidAuthorizationMessage(mr *inferencev1alpha1.ModelRouter) (reason, message string) {
	allowlists := routerAllowlists(mr)
	if len(allowlists) == 0 {
		return "", ""
	}

	if routerJWT(mr) == nil {
		return modelRouterGatewayReasonAuthzRequiresJWT,
			"policy.auth.allowlists is set but policy.auth.jwt is not; authorization matches on a verified JWT " +
				"claim and cannot be enforced without authentication. Configure policy.auth.jwt, or remove the allowlists"
	}

	seen := make(map[string]struct{}, len(allowlists))
	for i, entry := range allowlists {
		if strings.TrimSpace(entry.Team) == "" {
			return modelRouterGatewayReasonInvalidAuthz,
				fmt.Sprintf("policy.auth.allowlists[%d] has an empty team; every allowlist entry must name the "+
					"verified team it grants access to", i)
		}
		if entry.Team != strings.TrimSpace(entry.Team) {
			return modelRouterGatewayReasonInvalidAuthz,
				fmt.Sprintf("policy.auth.allowlists team %q has leading or trailing whitespace and would never match "+
					"the exact JWT claim; trim the team value", entry.Team)
		}
		if _, dup := seen[entry.Team]; dup {
			return modelRouterGatewayReasonInvalidAuthz,
				fmt.Sprintf("policy.auth.allowlists has duplicate team %q; declare each team at most once "+
					"(list all its models in a single entry)", entry.Team)
		}
		seen[entry.Team] = struct{}{}
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
	mode := classificationMode(mr)
	for _, rule := range mr.Spec.Rules {
		if rule.Match != nil {
			if unsupported := unsupportedMatchFields(rule.Match, mode); len(unsupported) > 0 {
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
// RuleMatch, in a stable order. The classification mode decides whether
// dataClassification is expressible: in header-only mode it compiles to a header
// match on the classification header key (slice 2e-core), so it is not listed;
// in detector/hybrid mode the in-proxy classifier is not built yet, so it stays
// fail-loud with a message pointing at header-only mode.
func unsupportedMatchFields(m *inferencev1alpha1.RuleMatch, classificationMode string) []string {
	var fields []string
	if len(m.DataClassification) > 0 && classificationMode != classificationModeHeaderOnly {
		fields = append(fields, "dataClassification (detector/hybrid classifier not implemented; use header-only mode)")
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
