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
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	modelRouterAgentGatewayControllerName = "modelrouter-agentgateway"

	// ModelRouterAgentGatewayConditionReady is the ModelRouter status condition
	// type the agentgateway reconciler owns. True once the InferencePool /
	// HTTPRoute / model-extraction AgentgatewayPolicy reconciled against the
	// referenced agentgateway Gateway; False (with a reason) when the required
	// CRDs are absent, when a rule uses an unsupported match, or when
	// reconciliation fails.
	ModelRouterAgentGatewayConditionReady = "AgentGatewayReady"

	// ModelRouter agentgateway condition reasons.
	modelRouterAgentGatewayReasonExposed     = "Exposed"
	modelRouterAgentGatewayReasonCRDsMissing = "InferenceExtensionCRDsNotInstalled"
	modelRouterAgentGatewayReasonReconcile   = "ReconcileFailed"
	modelRouterAgentGatewayReasonUnsupported = "UnsupportedMatchInAgentGatewayMode"
	modelRouterAgentGatewayReasonNoRef       = "AgentGatewayRefMissing"
	// modelRouterAgentGatewayReasonPolicyConflict marks a Gateway that already
	// carries a PreRouting transformation policy LLMKube does not own.
	// agentgateway merges same-specificity policies oldest-wins per whole field
	// and never combines list entries, so a foreign policy either silently
	// disables the generated model extraction or is silently disabled by it,
	// and no condition reports the dropped field.
	modelRouterAgentGatewayReasonPolicyConflict = "ConflictingPreRoutingPolicy"
	// modelRouterAgentGatewayReasonMultiPool marks a router where two
	// InferencePools would share one Endpoint Picker (or two InferenceServices
	// compile to one pool name): a GIE EPP binds to exactly one pool, so the
	// compiled shape would make endpoint decisions for the wrong pool.
	modelRouterAgentGatewayReasonMultiPool = "MultipleInferencePoolsUnsupported"
	// modelRouterAgentGatewayReasonConflictingPicker marks backends on the same
	// InferenceService that name different Endpoint Pickers: one pool has one
	// picker.
	modelRouterAgentGatewayReasonConflictingPicker = "ConflictingEndpointPicker"
	// modelRouterAgentGatewayReasonPickerRequired marks an InferencePool that
	// resolves no Endpoint Picker: no backend override and no router default.
	modelRouterAgentGatewayReasonPickerRequired = "EndpointPickerRequired"
	// modelRouterAgentGatewayReasonGatewayNotFound marks an agentGatewayRef
	// whose Gateway does not exist (or whose kind is not registered). The
	// router previously went Ready against a missing Gateway; nothing it
	// generates would serve traffic.
	modelRouterAgentGatewayReasonGatewayNotFound = "GatewayNotFound"
	// modelRouterAgentGatewayReasonNotAllowed marks a Gateway whose listeners
	// do not admit the ModelRouter's namespace via allowedRoutes. This is the
	// Gateway API consent mechanism: writing a Gateway-wide PreRouting policy
	// into another namespace without it would let any router operator inject
	// body parsing into every request on a Gateway they do not own.
	modelRouterAgentGatewayReasonNotAllowed = "GatewayDoesNotAllowRouterNamespace"
	// modelRouterAgentGatewayReasonResourceNameTaken marks a router whose
	// generated name is held by an object LLMKube did not create: the
	// extraction policy, an InferencePool named after the InferenceService, or
	// the HTTPRoute named after the router. Adopting such an object would
	// overwrite the owner's spec and, via the router-UID label or an owner
	// reference, delete it when this router goes away.
	modelRouterAgentGatewayReasonResourceNameTaken = "GeneratedResourceNameTaken"

	// modelRouterAgentGatewayFinalizer guards the cross-namespace extraction
	// policy. For a Gateway in another namespace the policy lives in the
	// Gateway's namespace, where an owner reference to the ModelRouter is
	// impossible, so the finalizer is what deletes it when the router is
	// deleted, moves to a different Gateway, or leaves AgentGateway mode.
	modelRouterAgentGatewayFinalizer = "agentgateway.inference.llmkube.dev/finalizer"

	// agentGatewayRouterUIDLabel marks every object the operator generated for
	// a specific ModelRouter (value: the router's UID): the extraction policy,
	// the per-backend InferencePools, and the HTTPRoute. The conflict detector
	// uses it to tell the router's own policy from a foreign one in the same
	// namespace, cleanup uses it to find every generated object a router left
	// behind, the leaving-AgentGateway sweep matches pools and routes on it,
	// and the apply refuses a policy name another router's object holds.
	agentGatewayRouterUIDLabel = "inference.llmkube.dev/agentgateway-router-uid"

	// agentGatewayConflictRequeueAfter is how often this reconciler rechecks
	// state that no watch covers: a Ready router (and a conflicted one) looks
	// for foreign PreRouting transformations created after it went Ready, and
	// every refusal whose truth depends on cluster state outside the router's
	// spec - the Gateway existing (GatewayNotFound), its allowedRoutes consent
	// (GatewayDoesNotAllowRouterNamespace), and a generated name being free
	// (GeneratedResourceNameTaken) - requeues on the same cadence so removing
	// the blocker takes effect without an unrelated event. Spec-determined
	// refusals (unsupported match, auth, budget, auditLog, sensitive-route,
	// multi-pool) stay event-driven. A watch on AgentgatewayPolicies was
	// rejected because registering it fails manager startup when the CRD is
	// absent - the same hazard the Owns() comment in SetupWithManager
	// documents.
	agentGatewayConflictRequeueAfter = 2 * time.Minute

	// The honest-boundary reasons mirror the Gateway plane exactly: a
	// dataPlane: AgentGateway ModelRouter must enforce the SAME limits the
	// Gateway plane does, so the same spec that is rejected in Gateway mode is
	// rejected here (with an agentgateway-flavored message) rather than silently
	// compiling to a route that drops the field.
	modelRouterAgentGatewayReasonUnsupportedBudgetField = "UnsupportedBudgetField"
	modelRouterAgentGatewayReasonUnsupportedBudgetScope = "UnsupportedBudgetScope"
	modelRouterAgentGatewayReasonInvalidBudget          = "InvalidBudget"
	modelRouterAgentGatewayReasonInvalidAuth            = "InvalidAuth"
	modelRouterAgentGatewayReasonAuthzRequiresJWT       = "AuthorizationRequiresJWT"
	modelRouterAgentGatewayReasonInvalidAuthz           = "InvalidAuthorization"
	modelRouterAgentGatewayReasonUnsupportedAuditLog    = "UnsupportedAuditLogInAgentGatewayMode"
	modelRouterAgentGatewayReasonUnsafeSensitiveRoute   = "UnsafeSensitiveRoute"
)

// ModelRouterAgentGatewayReconciler compiles a ModelRouter in dataPlane:
// AgentGateway mode onto an agentgateway data plane via the Gateway API
// Inference Extension: one InferencePool per referenced InferenceService (with
// the operator's Endpoint Picker), one model-extraction AgentgatewayPolicy
// (agentgateway.dev) that copies the OpenAI "model" field into a header before
// route selection, and a multi-rule HTTPRoute that matches on that header and
// routes to the pools directly. It is a sibling
// of ModelRouterGatewayReconciler (Envoy AI Gateway) and ModelRouterReconciler
// (Proxy), selected by spec.dataPlane, so the agentgateway integration stays
// cleanly optional and feature-flaggable, and a cluster without the required
// CRDs runs the rest of the operator unaffected.
type ModelRouterAgentGatewayReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder surfaces reconcile failures as Kubernetes Events. A failed
	// reconcile does not retract the previously compiled InferencePool /
	// HTTPRoute / AgentgatewayPolicy, so the data plane keeps serving its last-good
	// config: requests still succeed while silently using stale routing. A
	// status condition alone is not enough, because nobody watches conditions
	// during a routing change (#1395).
	Recorder events.EventRecorder

	// detector is the shared CRD-presence gate, lazily initialized on first
	// reconcile and reused thereafter. It requires the Inference Extension's
	// InferencePool kind and agentgateway.dev's AgentgatewayPolicy kind.
	detectorOnce sync.Once
	detector     *crdDetector

	// modelDetector gates routing: Model on the AgentgatewayModel CRD alone.
	modelDetectorOnce sync.Once
	modelDetector     *crdDetector
}

// +kubebuilder:rbac:groups=inference.networking.k8s.io,resources=inferencepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentgateway.dev,resources=agentgatewaypolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentgateway.dev,resources=agentgatewaymodels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile compiles the agentgateway resources for a ModelRouter in dataPlane:
// AgentGateway mode, or no-ops cleanly when the router is in Proxy or Gateway
// mode or when the Inference Extension CRDs are not installed.
//
// The guard chain (ref, CRDs, Gateway consent, conflicts, unsupported fields)
// is one linear refusal sequence; splitting it would scatter the refusals that
// must all fire before anything is generated, so it sits just over the
// gocyclo threshold now that the consent gate is part of it.
//
//nolint:gocyclo
func (r *ModelRouterAgentGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName(modelRouterAgentGatewayControllerName)

	mr := &inferencev1alpha1.ModelRouter{}
	if err := r.Get(ctx, req.NamespacedName, mr); err != nil {
		if apierrors.IsNotFound(err) {
			// Owner-ref GC removes the generated resources when the ModelRouter
			// is deleted; nothing to do here.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Cleanup path. The sweep must run with or without the finalizer: the
	// finalizer is only ever added for cross-namespace policies, so a
	// same-namespace router leaving AgentGateway mode would otherwise keep its
	// Gateway-wide body-extraction policy, its pools, and its route until the
	// router itself is deleted. It runs when the router is deleted, when it no
	// longer compiles in AgentGateway mode (different data plane or ref unset),
	// and - for a finalizer-bearing router - when the Gateway moved back into
	// the router's namespace. Runs before the dataPlane early-return so a
	// router switching to Proxy still cleans up.
	deleting := !mr.DeletionTimestamp.IsZero()
	leaving := mr.Spec.DataPlane != inferencev1alpha1.ModelRouterDataPlaneAgentGateway || mr.Spec.AgentGatewayRef == nil
	hasFinalizer := controllerutil.ContainsFinalizer(mr, modelRouterAgentGatewayFinalizer)
	// The ref nil-check short-circuits before agentGatewayPolicyNamespace
	// dereferences it.
	sameNSPolicy := mr.Spec.AgentGatewayRef != nil && agentGatewayPolicyNamespace(mr) == mr.Namespace
	// Evidence the router ever used AgentGateway mode: the finalizer, a
	// status.agentGateway block (set exactly when generation succeeded), or a
	// TRUE AgentGatewayReady condition. A FALSE condition is not evidence:
	// refusals set it without generating anything, and counting it would make
	// every refusal reconcile re-run the four Lists below - the cost this
	// gate exists to remove.
	readyCond := apimeta.FindStatusCondition(mr.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	usedAgentGateway := hasFinalizer || mr.Status.AgentGateway != nil ||
		(readyCond != nil && readyCond.Status == metav1.ConditionTrue)
	if usedAgentGateway && (deleting || leaving || (hasFinalizer && sameNSPolicy)) {
		if err := r.cleanupExtractionPolicies(ctx, mr, ""); err != nil {
			// A status patch is futile on a terminating object; the event
			// is the only visibility a stuck deletion has.
			if r.Recorder != nil {
				r.Recorder.Eventf(mr, nil, corev1.EventTypeWarning, modelRouterAgentGatewayReasonReconcile, "Reconcile",
					"deletion blocked: cleaning up the cross-namespace model-extraction AgentgatewayPolicy failed: %v", err)
			}
			return ctrl.Result{}, err
		}
		if deleting || leaving {
			// Pools, routes and models are owner-referenced, but owner-ref GC
			// only fires when the router object goes away: a router that
			// leaves AgentGateway mode must stop attaching to the Gateway now.
			if err := r.deleteGeneratedObjects(ctx, mr, nil,
				inferencePoolGVK(), gatewayHTTPRouteGVK(), agentGatewayModelGVK()); err != nil {
				return ctrl.Result{}, err
			}
		}
		if hasFinalizer {
			controllerutil.RemoveFinalizer(mr, modelRouterAgentGatewayFinalizer)
			if err := r.Update(ctx, mr); err != nil {
				return ctrl.Result{}, err
			}
		}
		if leaving && !deleting {
			// The router left AgentGateway mode for good: drop the status so
			// later reconciles skip this sweep (no evidence left). Fall
			// through afterwards: a removed ref must still report
			// AgentGatewayRefMissing, and that is now convergent because the
			// refusal no longer allocates status.agentGateway - the cleared
			// evidence stays cleared, the refusal's condition write is a
			// no-op from the second reconcile on, and the sweep runs exactly
			// once. A never-used router (no evidence) skips the sweep and
			// reports the refusal directly.
			patch := client.MergeFrom(mr.DeepCopy())
			mr.Status.AgentGateway = nil
			apimeta.RemoveStatusCondition(&mr.Status.Conditions, ModelRouterAgentGatewayConditionReady)
			if err := r.Status().Patch(ctx, mr, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
		if !leaving {
			// Deleting, or the Gateway moved into the router's namespace: done
			// here. Regeneration, when applicable, happens on the reconcile the
			// finalizer removal triggers.
			return ctrl.Result{}, nil
		}
		// Leaving AgentGateway mode: continue below (the dataPlane early-return
		// ends Proxy/Gateway routers here; a ref-unset AgentGateway router
		// reaches the AgentGatewayRefMissing refusal).
	}

	// Only the AgentGateway data plane is this reconciler's concern. Proxy mode
	// (the default) and Gateway mode are owned by other reconcilers; we do not
	// touch their status.
	if mr.Spec.DataPlane != inferencev1alpha1.ModelRouterDataPlaneAgentGateway {
		return ctrl.Result{}, nil
	}

	// agentGatewayRef is required in AgentGateway mode. Fail loud rather than
	// guessing a Gateway or Endpoint Picker.
	if mr.Spec.AgentGatewayRef == nil {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonNoRef,
			"dataPlane is AgentGateway but spec.agentGatewayRef is unset; cannot attach a pool or route")
	}

	if msg := backendSelectorUnsupportedMessage(mr); msg != "" {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonUnsupported, msg)
	}

	// A keyed backend would 401 through agentgateway: fail loud before any
	// generation, like the unsupported-match check.
	if names, err := keyedBackends(ctx, r.Client, mr.Namespace, mr.Spec.Backends); err != nil {
		return ctrl.Result{}, err
	} else if len(names) > 0 {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, ModelRouterReasonBackendAPIKeyUnsupported, keyedBackendsMessage(names))
	}

	// CRD-presence gate. When the required CRDs are absent we never create
	// resources and never requeue in a hot loop; we set a clear condition
	// naming the missing kinds so the user sees why nothing happened (and which
	// kind is meant, rather than having to read the detector; #1725). A
	// transient discovery error (not a missing kind) is returned so we requeue
	// rather than disabling.
	modelRouting := agentGatewayModelRouting(mr)
	present, missing, err := r.requiredCRDsMissing(log, modelRouting)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !present {
		names := make([]string, 0, len(missing))
		for _, gvk := range missing {
			names = append(names, fmt.Sprintf("%s (%s/%s)", gvk.Kind, gvk.Group, gvk.Version))
		}
		if modelRouting {
			return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonModelCRDMissing,
				"agentGatewayRef.routing: Model needs agentgateway v1.6.0 or later, and its AgentgatewayModel CRD is not "+
					"installed; agentgateway exposure is disabled; missing: "+strings.Join(names, ", "))
		}
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonCRDsMissing,
			"Gateway API Inference Extension and agentgateway CRDs are not installed; agentgateway exposure is disabled; missing: "+
				strings.Join(names, ", "))
	}

	// The consent check and the conflict check both need the referenced
	// Gateway; fetch it once. A missing Gateway is a refusal (nothing this
	// reconciler generates would serve traffic), not a requeue loop.
	gateway, err := r.getReferencedGateway(ctx, mr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if gateway == nil {
		if err := r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonGatewayNotFound,
			fmt.Sprintf("agentGatewayRef targets Gateway %s/%s, which does not exist; agentgateway exposure is disabled",
				agentGatewayPolicyNamespace(mr), mr.Spec.AgentGatewayRef.Name)); err != nil {
			return ctrl.Result{}, err
		}
		// Consent cannot be verified, so withdraw whatever a previously
		// admitted reconcile left in the Gateway's namespace. Nothing watches
		// Gateways, so the requeue is the only recovery for the normal GitOps
		// order (router applied before its Gateway) - without it the router
		// stays refused until an unrelated event.
		if err := r.withdrawAgentGatewayPolicy(ctx, mr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: agentGatewayConflictRequeueAfter}, nil
	}

	// Fail loud when no listener's allowedRoutes admits the ModelRouter's
	// namespace. allowedRoutes is Gateway API's consent mechanism for route
	// attachment; the AgentgatewayPolicy has no equivalent, and without this
	// check any router in any namespace could make the operator write a
	// Gateway-wide PreRouting transformation into a namespace it does not own
	// (a body parse plus a header write on every request through that Gateway).
	// Unset allowedRoutes defaults to Same, so absence never admits a
	// cross-namespace router.
	if msg, err := r.agentGatewayAllowedRoutesViolation(ctx, gateway, mr); err != nil {
		return ctrl.Result{}, err
	} else if msg != "" {
		if err := r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonNotAllowed, msg); err != nil {
			return ctrl.Result{}, err
		}
		// Consent is gone: the HTTPRoute would be refused, so the Gateway-wide
		// body parse this router left in the Gateway's namespace must go too.
		// (A conflicting foreign policy is different: there the router's own
		// policy is kept, documented oldest-wins.) Nothing watches Gateways,
		// so the requeue is the only recovery when the Gateway owner restores
		// consent.
		if err := r.withdrawAgentGatewayPolicy(ctx, mr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: agentGatewayConflictRequeueAfter}, nil
	}

	// routing: Model writes no extraction policy, so these checks don't apply.
	if !modelRouting {
		// Fail loud when the AgentgatewayPolicy this router would generate already
		// exists and LLMKube did not create it. Runs BEFORE the conflict check so
		// a foreign policy that both holds our name and conflicts reports the more
		// specific name-taken reason, and before any generation so a name-taken
		// router yields no partial resources. The pools and the route are checked
		// the same way inside reconcileAgentGatewayResources, once their names are
		// known from the resolved backends.
		if err := r.checkGeneratedNameAvailable(ctx, mr, newAgentGatewayModelExtractionPolicy(mr, mr.Spec.AgentGatewayRef)); err != nil {
			var taken *agentGatewayNameTakenError
			if errors.As(err, &taken) {
				// Whether the name is free depends on objects outside the router's
				// spec; requeue so deleting or renaming the foreign object takes
				// effect without an unrelated event.
				if serr := r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonResourceNameTaken, err.Error()); serr != nil {
					return ctrl.Result{}, serr
				}
				return ctrl.Result{RequeueAfter: agentGatewayConflictRequeueAfter}, nil
			}
			return ctrl.Result{}, err
		}

		// Fail loud when the target Gateway already carries a PreRouting
		// transformation policy this router does not own. agentgateway merges
		// equal-specificity policies oldest-wins per whole field and never
		// combines list entries (and no condition reports the dropped field), so a
		// second transformation on the same Gateway either silently disables the
		// generated model extraction or is silently disabled by it. Refuse before
		// any generation so a conflicted router yields no partial resources.
		conflicts, err := r.agentGatewayPreRoutingConflicts(ctx, mr, gateway.GetLabels())
		if err != nil {
			return ctrl.Result{}, err
		}
		if len(conflicts) > 0 {
			if err := r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonPolicyConflict,
				fmt.Sprintf("Gateway %s/%s already carries PreRouting transformation policy(ies) that LLMKube does not own: %s. "+
					"agentgateway merges same-specificity policies oldest-wins per field without combining list entries, "+
					"so the generated model extraction could be silently dropped, or silently shadow the existing policy. "+
					"Remove or scope the other policy", agentGatewayPolicyNamespace(mr), mr.Spec.AgentGatewayRef.Name,
					strings.Join(conflicts, ", "))); err != nil {
				return ctrl.Result{}, err
			}
			// Recheck on the conflict cadence so the condition clears by itself
			// when the foreign policy is deleted.
			return ctrl.Result{RequeueAfter: agentGatewayConflictRequeueAfter}, nil
		}
	}

	// Fail-loud on matches the agentgateway data plane cannot express. We
	// generate NOTHING for such a router rather than silently dropping a rule the
	// user expects. The agentgateway plane matches on the OpenAI "model" field
	// (via the extracted agentGatewayHTTPModelHeader) and on headers (via
	// HTTPRoute header matches); everything the Gateway plane rejects is
	// rejected here too. routing: Model has its own boundary, checked first.
	if msg := agentGatewayModelRoutingMessage(mr); msg != "" {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonUnsupportedModelRouting, msg)
	}
	if msg := agentGatewayUnsupportedMatchMessage(mr); msg != "" {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonUnsupported, msg)
	}

	// Fail-loud on any policy.auth configuration. The Gateway plane compiles
	// policy.auth.jwt into a SecurityPolicy (JWT validation) and
	// policy.auth.allowlists into its authorization block; the agentgateway
	// Inference-Extension path compiles neither (it fronts an InferencePool via
	// HTTPRoute, not a SecurityPolicy). Accepting a well-formed auth block and
	// silently enforcing nothing would be the silent-reduction failure this plane
	// must avoid (#1478 ask 4), so auth is rejected loudly rather than validated.
	// Runs BEFORE generation so no partial resources are emitted.
	if msg := agentGatewayUnsupportedAuthMessage(mr); msg != "" {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonInvalidAuth, msg)
	}

	// Fail-loud on budgets the agentgateway data plane cannot honor. The
	// agentgateway plane (like Envoy) rate-limits on tokens, and there is no
	// single honest token conversion across heterogeneous backend costs; rule
	// scope and a budget with no cap are equally unsupported. Same ordering as
	// the match check: runs BEFORE any CreateOrUpdate so a rejected budget yields
	// no partial generation.
	if reason, msg := agentGatewayUnsupportedBudgetMessage(mr); reason != "" {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, reason, msg)
	}

	// Fail-loud on a per-router auditLog directive: policy.auditLog is a Proxy-mode
	// field with no per-route agentgateway equivalent. Silently ignoring an audit
	// directive a user believes is active is a compliance footgun, so we generate
	// NOTHING and point the operator at the agentgateway-level access-log config.
	// Same ordering as the auth checks (before any CreateOrUpdate).
	if msg := agentGatewayUnsupportedAuditLogMessage(mr); msg != "" {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonUnsupportedAuditLog, msg)
	}

	// Fail-loud on a sensitive-classification rule that is not fail-closed and
	// local-tier only: a rule that DECLARES a pii/phi dataClassification but lacks
	// fail-closed, or routes to a cloud-tier backend, cannot be compiled. Placed
	// AFTER the unsupported-match check (so a rule with an inexpressible match is
	// reported first) and BEFORE generation (so a rejected rule yields no partial
	// resources). The same scoping caveat as the Gateway plane applies: this is a
	// per-declaring-rule guard, and the global "PII never egresses" property also
	// relies on agentgateway mode having no cloud/external backends today.
	if msg := unsafeSensitiveRouteMessage(mr); msg != "" {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonUnsafeSensitiveRoute, msg)
	}

	// Fail loud unless every InferencePool resolves exactly one Endpoint
	// Picker and no picker serves two pools: a GIE EPP binds to exactly one
	// pool. Same pure check the webhook runs (modelrouter_webhook.go), so
	// admission and reconcile cannot diverge.
	if _, reason, msg := agentGatewayResolvePoolPickers(mr); reason != "" {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, reason, msg)
	}

	var ejected []string
	if modelRouting {
		err = r.reconcileAgentGatewayModels(ctx, mr)
	} else {
		ejected, err = r.reconcileAgentGatewayResources(ctx, mr)
	}
	if err != nil {
		var taken *agentGatewayNameTakenError
		if errors.As(err, &taken) {
			// Same reasoning as the guard-chain name check: the pre-check and
			// this write raced, and the foreign object is still there; requeue
			// so its removal takes effect.
			if serr := r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonResourceNameTaken, err.Error()); serr != nil {
				return ctrl.Result{}, serr
			}
			return ctrl.Result{RequeueAfter: agentGatewayConflictRequeueAfter}, nil
		}
		// No requeue: the InferenceService watch re-reconciles on a fix.
		var refusal *agentGatewayRefusalError
		if errors.As(err, &refusal) {
			return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, refusal.reason, refusal.message)
		}
		_ = r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonReconcile, err.Error())
		return ctrl.Result{}, err
	}

	if err := r.setAgentGatewayReady(ctx, mr, ejected); err != nil {
		return ctrl.Result{}, err
	}
	// Keep rechecking so a foreign PreRouting policy created after this
	// reconcile is detected (and a cleared conflict is noticed) without
	// waiting for an unrelated event.
	return ctrl.Result{RequeueAfter: agentGatewayConflictRequeueAfter}, nil
}

// reconcileAgentGatewayResources resolves the router's backends to their
// InferenceService, compiles each into an InferencePool, generates the
// model-extraction AgentgatewayPolicy, and compiles the rules into a multi-rule
// HTTPRoute, creating-or-updating each one. Pools and route live in the
// ModelRouter's namespace and carry a controller owner reference; the
// extraction policy lives in the Gateway's namespace and carries one only when
// that is the same namespace.
func (r *ModelRouterAgentGatewayReconciler) reconcileAgentGatewayResources(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) ([]string, error) {
	backends, err := r.resolveAgentGatewayBackends(ctx, mr)
	if err != nil {
		return nil, err
	}

	rules, err := compileAgentGatewayRules(mr)
	if err != nil {
		return nil, err
	}

	// InferencePools for ALL backends (healthy or not) go up first, so the route
	// that references them has its targets in place. An ejected backend still
	// gets a pool so it can be re-added on recovery; only the route's
	// backendRefs are filtered below.
	desired := make([]*unstructured.Unstructured, 0, len(backends)+2)
	for _, b := range backends {
		desired = append(desired, newAgentGatewayInferencePool(mr, b))
	}

	// Drop unhealthy backends from the route's backendRefs so agentgateway fails
	// over to a healthy backend the instant the health signal changes. The
	// InferencePools above are generated for ALL backends; only the route is
	// filtered, and a rule is never emptied (see agentGatewayEjectUnhealthy).
	rules, ejected := agentGatewayEjectUnhealthy(rules, backends)
	desired = append(desired, newAgentGatewayModelExtractionPolicy(mr, mr.Spec.AgentGatewayRef))
	desired = append(desired, newAgentGatewayHTTPRoute(mr, mr.Spec.AgentGatewayRef, rules, backends))

	// Before ANY write (finalizer, prune, apply): refuse when any generated
	// name is held by an object LLMKube did not create. The policy was
	// already checked in the guard chain (its check must precede the conflict
	// check); re-checking it here is one redundant Get that keeps this a
	// single code path over every generated kind.
	for _, obj := range desired {
		if err := r.checkGeneratedNameAvailable(ctx, mr, obj); err != nil {
			return nil, err
		}
	}

	// A cross-namespace policy has no owner reference to fall back on, so the
	// finalizer must exist BEFORE the policy is created: a crash between policy
	// creation and finalizer addition would strand the policy.
	policyNS := agentGatewayPolicyNamespace(mr)
	if policyNS != mr.Namespace && !controllerutil.ContainsFinalizer(mr, modelRouterAgentGatewayFinalizer) {
		controllerutil.AddFinalizer(mr, modelRouterAgentGatewayFinalizer)
		if err := r.Update(ctx, mr); err != nil {
			return nil, fmt.Errorf("add %s finalizer: %w", modelRouterAgentGatewayFinalizer, err)
		}
	}
	// Prune a policy left in a previous Gateway's namespace when
	// agentGatewayRef.namespace changed (the finalizer stays: the current
	// policy is still cross-namespace). When no policy is cross-namespace this
	// deletes nothing.
	if err := r.cleanupExtractionPolicies(ctx, mr, policyNS); err != nil {
		return nil, err
	}

	for _, obj := range desired {
		if err := r.applyAgentGatewayResource(ctx, mr, obj); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	// Remove what routing: Model left behind.
	if err := r.deleteGeneratedObjects(ctx, mr, nil, agentGatewayModelGVK()); err != nil {
		return nil, err
	}
	return ejected, nil
}

// resolveAgentGatewayBackends turns every InferenceServiceRef backend into a
// agentGatewayBackendResource (name + cluster FQDN + port + health). External
// backends are a hard error: agentgateway routes to an InferencePool, and an
// InferencePool selects from model-server pods, so an off-cluster endpoint has
// no pool to attach to. A backend referencing a missing InferenceService is
// also an error (we cannot point a pool at a Service that does not exist).
func (r *ModelRouterAgentGatewayReconciler) resolveAgentGatewayBackends(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) ([]agentGatewayBackendResource, error) {
	// The reconcile guard already refused any router whose pickers do not
	// resolve, so every pool has an entry here.
	pickers, _, _ := agentGatewayResolvePoolPickers(mr)
	resolved := make([]agentGatewayBackendResource, 0, len(mr.Spec.Backends))
	for _, b := range mr.Spec.Backends {
		isvc, err := r.backendInferenceService(ctx, mr, b)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, agentGatewayBackendResource{
			Name: b.Name,
			// The InferencePool selects pods by label; the pool name is the
			// InferenceService name so one pool serves one model's pods.
			PoolName: sanitizeDNSName(isvc.Name),
			FQDN:     fmt.Sprintf("%s.%s.svc.cluster.local", sanitizeDNSName(isvc.Name), isvc.Namespace),
			Port:     gatewayServicePort(isvc),
			Picker:   pickers[sanitizeDNSName(isvc.Name)],
			// A backend is healthy iff its InferenceService has at least one ready
			// replica. An unhealthy backend is ejected from the route's
			// backendRefs while its InferencePool stays in place for re-add on
			// recovery.
			Healthy: isvc.Status.ReadyReplicas > 0,
		})
	}
	return resolved, nil
}

// backendInferenceService fetches a backend's InferenceService. External
// backends and missing InferenceServices are errors.
func (r *ModelRouterAgentGatewayReconciler) backendInferenceService(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	b inferencev1alpha1.RouterBackend,
) (*inferencev1alpha1.InferenceService, error) {
	if b.External != nil {
		return nil, fmt.Errorf("backend %q is External; dataPlane: AgentGateway routes to an InferencePool "+
			"that selects model-server pods, so off-cluster backends cannot be attached", b.Name)
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
	return isvc, nil
}

// applyAgentGatewayResource creates-or-updates desired. Objects in the
// ModelRouter's namespace (pools, route, same-namespace extraction policy) are
// controller-referenced to the ModelRouter so owner-ref GC removes them. The
// cross-namespace extraction policy cannot carry that reference (owner refs
// cannot cross namespaces), so it is finalizer-managed instead, and its apply
// refuses a name another router's policy already holds: the ns-name
// concatenation behind the deterministic cross-namespace name is not
// injective, and adopting a foreign policy would silently repoint its
// targetRef.
func (r *ModelRouterAgentGatewayReconciler) applyAgentGatewayResource(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	desired *unstructured.Unstructured,
) error {
	desiredSpec, _, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil {
		return err
	}
	crossNamespace := desired.GetNamespace() != mr.Namespace

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(desired.GroupVersionKind())
	live.SetName(desired.GetName())
	live.SetNamespace(desired.GetNamespace())

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, live, func() error {
		// Belt against the race between the generated-name pre-check and this
		// write: an object that appeared in between, or one whose label holds
		// another router's UID, must never be adopted or overwritten, for any
		// generated kind and in either namespace. ResourceVersion is empty
		// only when CreateOrUpdate is about to Create.
		if live.GetResourceVersion() != "" && live.GetLabels()[agentGatewayRouterUIDLabel] != string(mr.UID) {
			return &agentGatewayNameTakenError{
				kind:  desired.GetKind(),
				ns:    live.GetNamespace(),
				name:  live.GetName(),
				owner: live.GetLabels()[agentGatewayRouterUIDLabel],
			}
		}
		// The UID label marks every generated object as this router's, for the
		// conflict detector and the leaving-AgentGateway sweep; re-assert it on
		// every update so drift cannot strip it.
		labels := live.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[agentGatewayRouterUIDLabel] = string(mr.UID)
		live.SetLabels(labels)
		live.Object["spec"] = desiredSpec
		if crossNamespace {
			return nil
		}
		return setControllerReferenceUnblocked(mr, live, r.Scheme)
	})
	return err
}

// requiredCRDsMissing reports whether the CRDs this slice needs are registered,
// and, when they are not, exactly which GVKs are missing (so the disabled
// condition can name them). Delegates to the shared crdDetector.
func (r *ModelRouterAgentGatewayReconciler) requiredCRDsMissing(log logr.Logger, modelRouting bool) (bool, []schema.GroupVersionKind, error) {
	if modelRouting {
		r.modelDetectorOnce.Do(func() {
			r.modelDetector = newCRDDetector("agentgateway-model", []schema.GroupVersionKind{agentGatewayModelGVK()})
		})
		return r.modelDetector.PresentMissing(r.Client, log)
	}
	r.detectorOnce.Do(func() {
		r.detector = newCRDDetector("agentgateway", modelRouterAgentGatewayGVKs())
	})
	return r.detector.PresentMissing(r.Client, log)
}

// agentGatewayPolicyNamespace is the namespace the model-extraction
// AgentgatewayPolicy lives in: the Gateway's namespace (agentGatewayRef
// .namespace defaults to the ModelRouter's). Nil-safe: the cleanup path calls
// it before the ref-nil guard.
func agentGatewayPolicyNamespace(mr *inferencev1alpha1.ModelRouter) string {
	if ref := mr.Spec.AgentGatewayRef; ref != nil && ref.Namespace != "" {
		return ref.Namespace
	}
	return mr.Namespace
}

// getReferencedGateway fetches the agentGatewayRef'd Gateway from the policy
// namespace. A missing Gateway (or an unregistered Gateway kind, which is how
// a cluster without the Gateway API CRDs presents) returns (nil, nil): the
// caller refuses with GatewayNotFound rather than requeueing, since nothing
// this reconciler generates would serve traffic against a Gateway that is not
// there.
func (r *ModelRouterAgentGatewayReconciler) getReferencedGateway(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) (*unstructured.Unstructured, error) {
	gateway := &unstructured.Unstructured{}
	gateway.SetGroupVersionKind(schema.GroupVersionKind{Group: gatewayBackendRefGroupAPI, Version: "v1", Kind: "Gateway"})
	ns, name := agentGatewayPolicyNamespace(mr), mr.Spec.AgentGatewayRef.Name
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, gateway)
	if err == nil {
		return gateway, nil
	}
	if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
		return nil, nil
	}
	return nil, fmt.Errorf("get Gateway %s/%s: %w", ns, name, err)
}

// agentGatewayAllowedRoutesViolation returns a non-empty message when no
// listener of the referenced Gateway admits the ModelRouter's namespace via
// allowedRoutes, or an error for transient lookup failures. allowedRoutes is
// Gateway API's consent mechanism for route attachment: per listener,
// namespaces.from is Same by default (an unset allowedRoutes admits only the
// Gateway's own namespace, so absence never admits a cross-namespace router),
// All admits every namespace, and Selector matches the router NAMESPACE's
// labels. Selector matching is fail-closed: an invalid selector admits
// nothing. A listener must also be able to carry an HTTPRoute at all: the
// protocol must be HTTP or HTTPS, and a non-empty allowedRoutes.kinds list
// must contain HTTPRoute (an unset list defaults by protocol). The HTTPRoute
// the operator generates would be refused by the Gateway controller under
// exactly these rules, and the Gateway-wide extraction policy must not be
// written without the same consent.
func (r *ModelRouterAgentGatewayReconciler) agentGatewayAllowedRoutesViolation(
	ctx context.Context,
	gateway *unstructured.Unstructured,
	mr *inferencev1alpha1.ModelRouter,
) (string, error) {
	policyNS := agentGatewayPolicyNamespace(mr)
	sectionName := mr.Spec.AgentGatewayRef.SectionName
	listeners, _, _ := unstructured.NestedSlice(gateway.Object, "spec", "listeners")

	nsLabels := map[string]string{}
	nsFetched := false
	fetchNamespaceLabels := func() (map[string]string, error) {
		if nsFetched {
			return nsLabels, nil
		}
		ns := &corev1.Namespace{}
		if err := r.Get(ctx, types.NamespacedName{Name: mr.Namespace}, ns); err != nil {
			return nil, fmt.Errorf("get namespace %q for allowedRoutes selector matching: %w", mr.Namespace, err)
		}
		nsLabels, nsFetched = ns.GetLabels(), true
		return nsLabels, nil
	}

	summary := make([]string, 0, len(listeners))
	describeListener := func(from, proto string, kinds []interface{}) string {
		entry := "from=" + from + ",proto=" + proto
		if len(kinds) > 0 {
			names := make([]string, 0, len(kinds))
			for _, k := range kinds {
				if km, ok := k.(map[string]interface{}); ok {
					g, _, _ := unstructured.NestedString(km, "group")
					n, _, _ := unstructured.NestedString(km, "kind")
					if g == "" {
						g = gatewayBackendRefGroupAPI
					}
					names = append(names, g+"/"+n)
				}
			}
			entry += ",kinds=" + strings.Join(names, "|")
		}
		return entry
	}
	for i, l := range listeners {
		listener, ok := l.(map[string]interface{})
		if !ok {
			summary = append(summary, fmt.Sprintf("listener[%d] unreadable", i))
			continue
		}
		from, _, _ := unstructured.NestedString(listener, "allowedRoutes", "namespaces", "from")
		if from == "" {
			from = "Same" // Gateway API default for an unset allowedRoutes
		}
		proto, _, _ := unstructured.NestedString(listener, "protocol")
		kinds, _, _ := unstructured.NestedSlice(listener, "allowedRoutes", "kinds")
		summary = append(summary, describeListener(from, proto, kinds))

		// A sectionName route attaches to that listener only.
		if sectionName != "" {
			if name, _, _ := unstructured.NestedString(listener, metadataNameField); name != sectionName {
				continue
			}
		}

		// An HTTPRoute attaches only to HTTP/HTTPS listeners, and only to
		// kinds the listener lists: an unset kinds list defaults by protocol
		// (HTTP/HTTPS with no kinds admits HTTPRoute), while a non-empty list
		// must contain HTTPRoute - group is optional and defaults to the
		// Gateway API group. Unknown protocols and kind lists without
		// HTTPRoute fail closed.
		switch proto {
		case "HTTP", "HTTPS":
		default:
			continue
		}
		if len(kinds) > 0 {
			admits := false
			for _, k := range kinds {
				km, ok := k.(map[string]interface{})
				if !ok {
					continue
				}
				kind, _, _ := unstructured.NestedString(km, "kind")
				group, _, _ := unstructured.NestedString(km, "group")
				if kind == "HTTPRoute" && (group == "" || group == gatewayBackendRefGroupAPI) {
					admits = true
					break
				}
			}
			if !admits {
				continue
			}
		}
		switch from {
		case "All":
			return "", nil
		case "Same":
			if policyNS == mr.Namespace {
				return "", nil
			}
		case "Selector":
			selectorMap, _, _ := unstructured.NestedMap(listener, "allowedRoutes", "namespaces", "selector")
			sel, err := labelSelectorFromUnstructured(selectorMap)
			if err != nil {
				// Fail closed: an unreadable selector admits nothing.
				continue
			}
			selector, err := metav1.LabelSelectorAsSelector(sel)
			if err != nil {
				// Fail closed: an invalid selector admits nothing.
				continue
			}
			nsLabels, err := fetchNamespaceLabels()
			if err != nil {
				return "", err
			}
			if selector.Matches(labels.Set(nsLabels)) {
				return "", nil
			}
		}
	}
	listener := "no listener"
	if sectionName != "" {
		listener = fmt.Sprintf("no listener named %q", sectionName)
	}
	msg := fmt.Sprintf("Gateway %s/%s has %s that admits namespace %q for an HTTPRoute (listeners: %s); "+
		"the generated HTTPRoute would be refused (NotAllowedByListeners)",
		policyNS, mr.Spec.AgentGatewayRef.Name, listener, mr.Namespace, strings.Join(summary, ", "))
	if agentGatewayModelRouting(mr) {
		return msg, nil
	}
	return msg + ", and the model-extraction AgentgatewayPolicy must not be written without that consent", nil
}

// labelSelectorFromUnstructured converts an unstructured LabelSelector
// (matchLabels + matchExpressions) into a metav1.LabelSelector.
func labelSelectorFromUnstructured(m map[string]interface{}) (*metav1.LabelSelector, error) {
	if m == nil {
		return &metav1.LabelSelector{}, nil
	}
	sel := &metav1.LabelSelector{}
	if matchLabels, ok := m["matchLabels"].(map[string]interface{}); ok {
		sel.MatchLabels = make(map[string]string, len(matchLabels))
		for k, v := range matchLabels {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("matchLabels[%q] is not a string", k)
			}
			sel.MatchLabels[k] = s
		}
	}
	if exprs, ok := m["matchExpressions"].([]interface{}); ok {
		sel.MatchExpressions = make([]metav1.LabelSelectorRequirement, 0, len(exprs))
		for _, e := range exprs {
			em, ok := e.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("matchExpressions entry is not an object")
			}
			key, _ := em["key"].(string)
			op, _ := em["operator"].(string)
			req := metav1.LabelSelectorRequirement{Key: key, Operator: metav1.LabelSelectorOperator(op)}
			if vals, ok := em["values"].([]interface{}); ok {
				req.Values = make([]string, 0, len(vals))
				for _, v := range vals {
					s, ok := v.(string)
					if !ok {
						return nil, fmt.Errorf("matchExpressions value is not a string")
					}
					req.Values = append(req.Values, s)
				}
			}
			sel.MatchExpressions = append(sel.MatchExpressions, req)
		}
	}
	return sel, nil
}

// agentGatewayNameTakenError reports a generated object whose name is held by
// an object LLMKube did not create. Reconcile converts it into the named
// GeneratedResourceNameTaken refusal instead of a reconcile error.
type agentGatewayNameTakenError struct {
	kind, ns, name, owner string
}

func (e *agentGatewayNameTakenError) Error() string {
	if e.owner != "" {
		return fmt.Sprintf("%s %s/%s is already managed by another ModelRouter (uid %s); "+
			"refusing to adopt or overwrite it; rename one of the routers", e.kind, e.ns, e.name, e.owner)
	}
	return fmt.Sprintf("%s %s/%s already exists and is not managed by this ModelRouter; refusing to adopt or overwrite it "+
		"(LLMKube did not create it, or its %s ownership label was stripped - a stripped label means LLMKube has stopped "+
		"managing it); delete or rename the object, or restore the label", e.kind, e.ns, e.name, agentGatewayRouterUIDLabel)
}

// checkGeneratedNameAvailable Gets one desired object before any write and
// refuses unless it is absent or already carries this router's UID label. The
// generated names are derived from the router (or its InferenceServices), so a
// pre-existing object can hold one: adopting it would overwrite the owner's
// spec with the generated spec and stamp the router-UID label on it, after
// which deleting this router deletes the owner's object. NoMatch tolerates the
// CRD vanishing between the gate and here; generation would fail on its own
// either way.
func (r *ModelRouterAgentGatewayReconciler) checkGeneratedNameAvailable(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	desired *unstructured.Unstructured,
) error {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(desired.GroupVersionKind())
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.GetNamespace(), Name: desired.GetName()}, live)
	if err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("check %s %s/%s: %w", desired.GetKind(), desired.GetNamespace(), desired.GetName(), err)
	}
	if live.GetLabels()[agentGatewayRouterUIDLabel] == string(mr.UID) {
		return nil
	}
	return &agentGatewayNameTakenError{
		kind:  desired.GetKind(),
		ns:    desired.GetNamespace(),
		name:  desired.GetName(),
		owner: live.GetLabels()[agentGatewayRouterUIDLabel],
	}
}

// agentGatewayPreRoutingConflicts lists the AgentgatewayPolicies in the
// Gateway's namespace that target the referenced Gateway with a PreRouting
// transformation LLMKube does not own (no router-UID label), returning their
// names. agentgateway merges same-specificity policies oldest-wins per whole
// field and never combines list entries, and every affected policy still
// reports Accepted and Attached with no condition about the dropped field, so
// any such policy silently disables the generated model extraction or is
// silently disabled by it. Policies without a transformation (extAuth, CORS,
// ...) do not touch the transformation field and do not conflict. The
// referenced Gateway's labels are passed in by the caller (fetched once for
// the consent check); they back the targetSelectors matching.
func (r *ModelRouterAgentGatewayReconciler) agentGatewayPreRoutingConflicts(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	gatewayLabels map[string]string,
) ([]string, error) {
	policyNS := agentGatewayPolicyNamespace(mr)

	policies := &unstructured.UnstructuredList{}
	policies.SetGroupVersionKind(schema.GroupVersionKind{
		Group: agentGatewayPolicyGroup, Version: agentGatewayPolicyVersion, Kind: agentGatewayPolicyKind + "List",
	})
	if err := r.List(ctx, policies, client.InNamespace(policyNS)); err != nil {
		if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			// The CRD disappeared between the gate and here (NoMatch straight
			// away, a cached-mapping 404 right after deletion); nothing to
			// conflict with, and nothing could be generated either way.
			return nil, nil
		}
		return nil, fmt.Errorf("list AgentgatewayPolicies in %q: %w", policyNS, err)
	}

	var conflicts []string
	for i := range policies.Items {
		p := &policies.Items[i]
		if label := p.GetLabels()[agentGatewayRouterUIDLabel]; label != "" {
			// Any LLMKube-generated policy is skipped, including another
			// router's: the extraction content is identical for every router
			// (same header, same expression), so under oldest-wins the
			// surviving policy behaves the same. Only foreign policies
			// conflict.
			continue
		}
		if !agentGatewayPolicyTargetsGateway(p, mr.Spec.AgentGatewayRef.Name, gatewayLabels) {
			continue
		}
		phase, _, _ := unstructured.NestedString(p.Object, "spec", "traffic", "phase")
		if phase != "PreRouting" {
			continue
		}
		transformation, _, _ := unstructured.NestedMap(p.Object, "spec", "traffic", "transformation")
		if len(transformation) == 0 {
			continue
		}
		conflicts = append(conflicts, p.GetName())
	}
	sort.Strings(conflicts)
	return conflicts, nil
}

// agentGatewayPolicyTargetsGateway reports whether an AgentgatewayPolicy
// targets the named Gateway, via targetRefs or via targetSelectors matched
// against the Gateway's own labels.
func agentGatewayPolicyTargetsGateway(p *unstructured.Unstructured, gatewayName string, gatewayLabels map[string]string) bool {
	isGatewayRef := func(ref map[string]interface{}) bool {
		kind, _, _ := unstructured.NestedString(ref, "kind")
		group, _, _ := unstructured.NestedString(ref, "group")
		name, _, _ := unstructured.NestedString(ref, "name")
		return kind == "Gateway" && (group == "" || group == gatewayBackendRefGroupAPI) && name == gatewayName
	}
	refs, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs")
	for _, r := range refs {
		if ref, ok := r.(map[string]interface{}); ok && isGatewayRef(ref) {
			return true
		}
	}
	selectors, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetSelectors")
	for _, s := range selectors {
		sel, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		kind, _, _ := unstructured.NestedString(sel, "kind")
		group, _, _ := unstructured.NestedString(sel, "group")
		if kind != "Gateway" || (group != "" && group != gatewayBackendRefGroupAPI) {
			continue
		}
		matchLabels, _, _ := unstructured.NestedStringMap(sel, "matchLabels")
		matches := true
		for k, v := range matchLabels {
			if gatewayLabels[k] != v {
				matches = false
				break
			}
		}
		// An empty selector (if the CRD permitted one) selects every Gateway in
		// the namespace, so it matches ours too.
		if matches {
			return true
		}
	}
	return false
}

// cleanupExtractionPolicies deletes every AgentgatewayPolicy this router
// generated outside keepNamespace. Owner references cannot cross namespaces,
// so the UID label is the only handle the router has on policies it left in a
// previous Gateway's namespace (agentGatewayRef.namespace changed) or anywhere
// at all (router deleted, dataPlane changed). When the AgentgatewayPolicy CRD
// is gone the API server removed the CRs with it.
func (r *ModelRouterAgentGatewayReconciler) cleanupExtractionPolicies(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	keepNamespace string,
) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: agentGatewayPolicyGroup, Version: agentGatewayPolicyVersion, Kind: agentGatewayPolicyKind + "List",
	})
	if err := r.List(ctx, list, client.MatchingLabels{agentGatewayRouterUIDLabel: string(mr.UID)}); err != nil {
		if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			// The CRD is gone. NoMatch is what discovery reports directly;
			// after the CRD is deleted while the operator runs, the CACHED
			// REST mapping keeps resolving and the LIST returns 404 - which
			// wedged a router in Terminating live until an operator restart
			// (round-2 review). Either way the API server removed the CRs
			// with the CRD, so there is nothing to clean.
			return nil
		}
		return fmt.Errorf("list AgentgatewayPolicies: %w", err)
	}
	for i := range list.Items {
		p := &list.Items[i]
		if p.GetNamespace() == keepNamespace {
			continue
		}
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale AgentgatewayPolicy %s/%s: %w", p.GetNamespace(), p.GetName(), err)
		}
	}
	return nil
}

// deleteGeneratedObjects deletes the objects of each given kind this router
// generated, matched by the router-UID label in the router's namespace, except
// the names in keep. It runs when the router leaves AgentGateway mode
// (dataPlane change or ref removal) and on deletion: owner-ref GC only fires
// when the router object goes away, but a router that leaves the mode must
// stop attaching to the Gateway immediately. Each routing mode also runs it to
// clear the other mode's kinds. The sweep is label-based, not owner-ref-based,
// because the Gateway plane also owner-references objects in the router's
// namespace; the UID label is what makes it mode-precise. A missing CRD
// (NoMatch, or the cached-mapping 404 after CRD removal) means the API server
// removed the CRs with it, so there is nothing to clean.
func (r *ModelRouterAgentGatewayReconciler) deleteGeneratedObjects(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	keep map[string]bool,
	gvks ...schema.GroupVersionKind,
) error {
	for _, gvk := range gvks {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)
		if err := r.List(ctx, list, client.InNamespace(mr.Namespace),
			client.MatchingLabels{agentGatewayRouterUIDLabel: string(mr.UID)}); err != nil {
			if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("list %s generated by router %s/%s: %w", gvk.Kind, mr.Namespace, mr.Name, err)
		}
		for i := range list.Items {
			obj := &list.Items[i]
			if keep[obj.GetName()] {
				continue
			}
			if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete %s %s/%s: %w", gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
			}
		}
	}
	return nil
}

// withdrawAgentGatewayPolicy deletes every AgentgatewayPolicy labeled with the
// router's UID and, when the sweep succeeded, drops the finalizer (nothing
// cross-namespace remains to guard). It runs when the consent check refuses a
// previously Ready router: the Gateway owner revoked allowedRoutes or the
// Gateway vanished, and a route that cannot attach must not leave a
// Gateway-wide body parse behind. The sweep is label-based, so a
// same-namespace policy a same-namespace router can no longer justify goes
// too. Re-running is safe: a failed sweep returns the error and retries on the
// next reconcile.
func (r *ModelRouterAgentGatewayReconciler) withdrawAgentGatewayPolicy(ctx context.Context, mr *inferencev1alpha1.ModelRouter) error {
	if err := r.cleanupExtractionPolicies(ctx, mr, ""); err != nil {
		return err
	}
	if controllerutil.ContainsFinalizer(mr, modelRouterAgentGatewayFinalizer) {
		controllerutil.RemoveFinalizer(mr, modelRouterAgentGatewayFinalizer)
		return r.Update(ctx, mr)
	}
	return nil
}

// setAgentGatewayReady writes the success status: AgentGatewayReady=True plus
// status.agentGateway with the resolved endpoint.
func (r *ModelRouterAgentGatewayReconciler) setAgentGatewayReady(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	ejected []string,
) error {
	patch := client.MergeFrom(mr.DeepCopy())
	mr.Status.AgentGateway = &inferencev1alpha1.AgentGatewayStatus{
		PoolReady: !agentGatewayModelRouting(mr),
		Endpoint:  agentGatewayEndpointAddress(mr.Spec.AgentGatewayRef),
	}
	message := agentGatewayReadyMessage(mr)
	if len(ejected) > 0 {
		message += fmt.Sprintf("; ejected %d unhealthy backend(s): %s", len(ejected), strings.Join(ejected, ", "))
	}
	apimeta.SetStatusCondition(&mr.Status.Conditions, metav1.Condition{
		Type:    ModelRouterAgentGatewayConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  modelRouterAgentGatewayReasonExposed,
		Message: message,
	})
	return r.Status().Patch(ctx, mr, patch)
}

// agentGatewayReadyMessage builds the success condition message: the rule count
// (with routing: Model, the model count) and the resolved endpoint.
func agentGatewayReadyMessage(mr *inferencev1alpha1.ModelRouter) string {
	if agentGatewayModelRouting(mr) {
		return fmt.Sprintf("compiled %d model(s) onto agentgateway %s", len(mr.Spec.Backends),
			agentGatewayEndpointAddress(mr.Spec.AgentGatewayRef))
	}
	return fmt.Sprintf("compiled %d rule(s) onto agentgateway %s", len(mr.Spec.Rules),
		agentGatewayEndpointAddress(mr.Spec.AgentGatewayRef))
}

// setAgentGatewayNotReady writes a False AgentGatewayReady condition and clears
// any stale PoolReady so status reflects reality. Used on every disabled /
// unsupported / failure path (the success path is setAgentGatewayReady).
func (r *ModelRouterAgentGatewayReconciler) setAgentGatewayNotReady(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	reason, message string,
) error {
	patch := client.MergeFrom(mr.DeepCopy())
	// Do not allocate the block here: status.agentGateway is the sweep gate's
	// evidence that generation succeeded at least once, and a refusal must not
	// fabricate it (a ref-unset router would otherwise alternate between
	// "refuse and allocate" and "sweep and clear" forever). Only tone down a
	// block a successful generation wrote.
	if mr.Status.AgentGateway != nil {
		mr.Status.AgentGateway.PoolReady = false
	}
	apimeta.SetStatusCondition(&mr.Status.Conditions, metav1.Condition{
		Type:    ModelRouterAgentGatewayConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
	// Say plainly that the previously compiled pools and routes are still live.
	// The dangerous case is not the failure itself but that traffic keeps flowing
	// against a config the operator believes they just replaced (#1400 pattern).
	if r.Recorder != nil {
		r.Recorder.Eventf(mr, nil, corev1.EventTypeWarning, reason, "Reconcile",
			"%s; agentgateway continues serving the last successfully compiled pools and routes, "+
				"so this router's traffic does NOT reflect the current spec", message)
	}
	return r.Status().Patch(ctx, mr, patch)
}

// SetupWithManager wires the ModelRouter agentgateway reconciler to watch
// ModelRouters.
//
// As with the Gateway plane we intentionally do not Owns() the generated
// resources: the operator may run where the Inference Extension CRDs are
// absent, and an Owns watch on an unregistered kind fails manager startup. The
// ModelRouter primary watch plus CreateOrUpdate's drift correction is
// sufficient.
func (r *ModelRouterAgentGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.ModelRouter{}).
		Watches(
			&inferencev1alpha1.InferenceService{},
			handler.EnqueueRequestsFromMapFunc(r.modelRoutersForInferenceService),
		).
		Named(modelRouterAgentGatewayControllerName).
		Complete(r)
}

// modelRoutersForInferenceService maps a changed InferenceService to the
// dataPlane: AgentGateway ModelRouters that reference it, so a backend's
// readiness flip re-reconciles the route (ejection/restore) within a reconcile
// rather than at the active-probe interval. Proxy- and Gateway-mode routers are
// skipped. Mirrors the Gateway plane's modelRoutersForInferenceService.
func (r *ModelRouterAgentGatewayReconciler) modelRoutersForInferenceService(ctx context.Context, obj client.Object) []reconcile.Request {
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
		if mr.Spec.DataPlane != inferencev1alpha1.ModelRouterDataPlaneAgentGateway {
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

// agentGatewayBackendResource is one resolved ModelRouter backend ready to
// compile onto the agentgateway data plane: the RouterBackend name, the
// InferencePool name (the referenced InferenceService), the cluster FQDN + port,
// and health.
type agentGatewayBackendResource struct {
	// Name is the RouterBackend.Name; it is what route backendRefs resolve
	// (through the poolFor map) to the generated InferencePool.
	Name string
	// PoolName is the InferencePool name (the sanitized InferenceService name).
	PoolName string
	// FQDN is the cluster-internal hostname the pool's selector resolves.
	FQDN string
	// Port is the Service port the pool targets.
	Port int64
	// Picker is the Endpoint Picker the backend's InferencePool references,
	// resolved by agentGatewayResolvePoolPickers.
	Picker agentGatewayPicker
	// Healthy reports whether the referenced InferenceService currently has at
	// least one ready replica. An unhealthy backend is dropped from the route's
	// backendRefs while its InferencePool stays in place.
	Healthy bool
}

// compileAgentGatewayRules turns the ModelRouter's spec.rules into resolved
// agentGatewayRuleResources (model-name match + backend refs), plus a trailing
// catch-all for defaultRoute. Matches were already vetted by
// agentGatewayUnsupportedMatchMessage.
// Backend health is applied separately by agentGatewayEjectUnhealthy, so the
// resolved backend set is deliberately not a parameter here: rule refs use
// RouterBackend.Name, which is identical in spec and resolved form.
// Rule and defaultRoute refs must name DECLARED backends: refs resolve to
// InferencePools by backend name, so an undeclared name has no pool and would
// silently vanish at render time. The webhook rejects such a spec, but the
// reconciler must not rely on the webhook being installed.
func compileAgentGatewayRules(mr *inferencev1alpha1.ModelRouter) ([]agentGatewayRuleResource, error) {
	declared := make(map[string]bool, len(mr.Spec.Backends))
	for _, b := range mr.Spec.Backends {
		declared[b.Name] = true
	}
	rules := make([]agentGatewayRuleResource, 0, len(mr.Spec.Rules)+len(mr.Spec.Backends)+1)
	for _, rule := range mr.Spec.Rules {
		for _, name := range rule.Route.Backends {
			if !declared[name] {
				return nil, fmt.Errorf("rule %q references backend %q, which is not declared in spec.backends", rule.Name, name)
			}
		}
		refs, err := compileAgentGatewayBackendRefs(rule.Name, rule.Route)
		if err != nil {
			return nil, err
		}
		resolved := agentGatewayRuleResource{BackendRefs: refs}
		if rule.Match != nil {
			resolved.Models = rule.Match.Models
			resolved.Headers = rule.Match.Headers
		}
		rules = append(rules, resolved)
	}

	// BackendNameMatch compiles to one model-match rule per backend (model ==
	// backend published id: DisplayName or Name) inserted ahead of the
	// defaultRoute catch-all. First-match ordering mirrors the proxy.
	if mr.Spec.DefaultRouteStrategy == inferencev1alpha1.DefaultRouteStrategyBackendNameMatch {
		for _, b := range mr.Spec.Backends {
			rules = append(rules, agentGatewayRuleResource{
				Models:      []string{agentGatewayServedName(b)},
				BackendRefs: []agentGatewayBackendRef{{Name: b.Name}},
			})
		}
	}

	// defaultRoute compiles to a trailing catch-all rule (no model/header match)
	// routing to the named backend.
	if mr.Spec.DefaultRoute != "" {
		if !declared[mr.Spec.DefaultRoute] {
			return nil, fmt.Errorf("defaultRoute references backend %q, which is not declared in spec.backends", mr.Spec.DefaultRoute)
		}
		rules = append(rules, agentGatewayRuleResource{
			BackendRefs: []agentGatewayBackendRef{{Name: mr.Spec.DefaultRoute}},
		})
	}

	return rules, nil
}

// compileAgentGatewayBackendRefs turns a rule's route.backends into ordered
// backendRefs. Each ref names a RouterBackend; the route renderer resolves
// names to the generated InferencePools (the GIE v1 routing path).
// Priority/weight are not expressed on the Inference Extension backendRef
// (agentgateway selects the endpoint via the EPP), so a single ref per backend
// is emitted.
func compileAgentGatewayBackendRefs(ruleName string, route inferencev1alpha1.RuleRoute) ([]agentGatewayBackendRef, error) {
	refs := make([]agentGatewayBackendRef, 0, len(route.Backends))
	if route.Strategy != "" && route.Strategy != "primary-fallback" {
		return nil, fmt.Errorf("rule %q uses strategy %q, which has no agentgateway equivalent; use primary-fallback", ruleName, route.Strategy)
	}
	for _, name := range route.Backends {
		refs = append(refs, agentGatewayBackendRef{Name: name})
	}
	return refs, nil
}

// agentGatewayRuleResource is one resolved agentgateway rule: the model-name
// match values + header matches, and the ordered backend refs.
type agentGatewayRuleResource struct {
	// Models are the OpenAI model-name match values (RuleMatch.Models). Each
	// compiles to its own HTTPRoute match on agentGatewayHTTPModelHeader.
	Models []string
	// Headers are exact header matches (RuleMatch.Headers), ANDed into every
	// model match.
	Headers map[string]string
	// BackendRefs are the ordered destinations, each naming a RouterBackend
	// that resolves to a generated InferencePool.
	BackendRefs []agentGatewayBackendRef
}

// agentGatewayBackendRef is one backendRef in a compiled rule, naming a
// RouterBackend. The route renderer resolves it to that backend's generated
// InferencePool.
type agentGatewayBackendRef struct {
	// Name is the RouterBackend.Name.
	Name string
}

// agentGatewayEjectUnhealthy drops unhealthy backends from each rule's
// backendRefs, returning the list of dropped names. The InferencePool and
// model-extraction AgentgatewayPolicy objects are generated for ALL backends
// regardless; only the route is filtered.
func agentGatewayEjectUnhealthy(rules []agentGatewayRuleResource, backends []agentGatewayBackendResource) ([]agentGatewayRuleResource, []string) {
	healthy := make(map[string]bool, len(backends))
	for _, b := range backends {
		healthy[b.Name] = b.Healthy
	}
	// Preallocated: at most one ejection message per rule.
	ejected := make([]string, 0, len(rules))
	out := make([]agentGatewayRuleResource, 0, len(rules))
	for _, rule := range rules {
		var kept []agentGatewayBackendRef
		var dropped []agentGatewayBackendRef
		for _, ref := range rule.BackendRefs {
			if healthy[ref.Name] {
				kept = append(kept, ref)
			} else {
				dropped = append(dropped, ref)
			}
		}
		if len(kept) == 0 {
			// Never hand agentgateway an empty backendRef list (it would 503 every
			// matched request); keep the original refs so the rule still resolves
			// once the backend recovers, while still reporting the drop.
			kept = rule.BackendRefs
		}
		for _, ref := range dropped {
			ejected = append(ejected, ref.Name)
		}
		rule.BackendRefs = kept
		out = append(out, rule)
	}
	return out, ejected
}

// agentGatewayEndpointAddress is the human-facing endpoint string surfaced on
// status.agentGateway.endpoint: which agentgateway Gateway fronts the router.
// We do not resolve the Gateway's external address (that is gateway-owned
// config the operator does not read).
func agentGatewayEndpointAddress(ref *inferencev1alpha1.AgentGatewayReference) string {
	ns := ref.Namespace
	if ns == "" {
		return fmt.Sprintf("gateway %q", ref.Name)
	}
	return fmt.Sprintf("gateway %s/%s", ns, ref.Name)
}

// modelRouterAgentGatewayGVKs are the GVKs the ModelRouter agentgateway path
// needs the cluster to have registered before it generates anything: the
// Inference Extension's InferencePool and agentgateway.dev's
// AgentgatewayPolicy.
func modelRouterAgentGatewayGVKs() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		inferencePoolGVK(),
		agentGatewayPolicyGVK(),
	}
}

// agentGatewayUnsupportedMatchMessage returns a non-empty message naming the
// first rule whose match uses a condition the agentgateway data plane cannot
// express, or a strategy with no agentgateway equivalent. Empty means every
// rule compiles. Only model-name (Models) and Headers matches, with the
// primary-fallback strategy, compile. This is the SAME honest boundary the
// Gateway plane enforces, flavored for agentgateway.
func agentGatewayUnsupportedMatchMessage(mr *inferencev1alpha1.ModelRouter) string {
	mode := classificationMode(mr)
	for _, rule := range mr.Spec.Rules {
		if rule.Match != nil {
			if unsupported := agentGatewayUnsupportedMatchFields(rule.Match, mode); len(unsupported) > 0 {
				return fmt.Sprintf("rule %q uses %s, which the agentgateway data plane cannot match; only model name and headers are supported in dataPlane: AgentGateway",
					rule.Name, strings.Join(unsupported, ", "))
			}
		}
		if s := rule.Route.Strategy; s != "" && s != "primary-fallback" {
			return fmt.Sprintf("rule %q uses strategy %q, which has no agentgateway equivalent; use primary-fallback in dataPlane: AgentGateway",
				rule.Name, s)
		}
	}
	return ""
}

// agentGatewayUnsupportedMatchFields lists the agentgateway-inexpressible match
// fields set on a RuleMatch, in a stable order. The classification mode decides
// whether dataClassification is expressible: in header-only mode it compiles to
// a header match (an HTTPRoute header match), so it is not listed; in
// detector/hybrid mode the in-proxy classifier is not built yet, so it stays
// fail-loud.
func agentGatewayUnsupportedMatchFields(m *inferencev1alpha1.RuleMatch, classificationMode string) []string {
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

// agentGatewayUnsupportedBudgetMessage returns a fail-loud (reason, message)
// for the first budget the agentgateway data plane cannot honor, or ("", "")
// when every budget compiles. It reuses the Gateway-plane
// unsupportedBudgetMessage for detection (same reasons, same ordering, runs
// BEFORE generation so a rejected budget yields no partial resources) but
// rephrases the message for the agentgateway plane. The honest boundary is
// identical: agentgateway rate-limits on tokens and there is no single honest
// token conversion across heterogeneous backend costs (MaxUSD), per-rule
// clientSelectors are not yet compiled (scope=rule), and a budget with no cap is
// malformed (InvalidBudget).
func agentGatewayUnsupportedBudgetMessage(mr *inferencev1alpha1.ModelRouter) (reason, message string) {
	gwReason, _ := unsupportedBudgetMessage(mr)
	if gwReason == "" {
		return "", ""
	}
	switch gwReason {
	case modelRouterGatewayReasonUnsupportedBudgetField:
		return modelRouterAgentGatewayReasonUnsupportedBudgetField,
			"budget sets maxUSD; dollar budgets are not yet supported in dataPlane: AgentGateway " +
				"(agentgateway rate-limits on tokens, and there is no single honest token conversion across " +
				"heterogeneous backend costs). Use maxTokens, or track maxUSD via the Proxy data plane"
	case modelRouterGatewayReasonUnsupportedBudgetScope:
		return modelRouterAgentGatewayReasonUnsupportedBudgetScope,
			"budget uses scope rule, which is not yet supported in dataPlane: AgentGateway; " +
				"use scope router (total cap) or team (per-tenant cap)"
	case modelRouterGatewayReasonInvalidBudget:
		return modelRouterAgentGatewayReasonInvalidBudget,
			"budget sets neither maxTokens nor maxUSD; at least one cap is required"
	default:
		return gwReason, "budget not supported in dataPlane: AgentGateway"
	}
}

// agentGatewayUnsupportedAuthMessage returns a non-empty message when any
// policy.auth configuration is set on a dataPlane: AgentGateway router, or ""
// when auth is absent. The Gateway plane compiles policy.auth.jwt into a
// SecurityPolicy (JWT validation) and policy.auth.allowlists into its
// authorization block; the agentgateway Inference-Extension path compiles
// neither (it fronts an InferencePool via HTTPRoute, not a SecurityPolicy).
// Accepting a well-formed auth block and silently enforcing nothing would be the
// silent-reduction failure this plane must avoid (#1478 ask 4), so auth is
// rejected loudly rather than validated. The field stays fully valid in Proxy
// mode and on the Envoy Gateway plane. Runs BEFORE generation so a rejected auth
// block yields no partial resources.
func agentGatewayUnsupportedAuthMessage(mr *inferencev1alpha1.ModelRouter) string {
	jwt := routerJWT(mr)
	allowlists := routerAllowlists(mr)
	if jwt == nil && len(allowlists) == 0 {
		return ""
	}
	return "policy.auth is not supported in dataPlane: AgentGateway; the agentgateway Inference-Extension path " +
		"fronts an InferencePool via HTTPRoute and compiles no SecurityPolicy, so JWT validation and per-team " +
		"authorization would be silently unenforced. Configure auth on the agentgateway data plane, or use " +
		"dataPlane: Gateway (Envoy AI Gateway) or Proxy"
}

// agentGatewayUnsupportedAuditLogMessage returns a non-empty message when
// policy.auditLog is set on a dataPlane: AgentGateway router, or "" when it is
// absent. Any auditLog block present means the user asked for per-router audit,
// which is a Proxy-mode-only feature (it names the router-proxy container and a
// file path). agentgateway access logging is configured on the gateway-scoped
// layer, not per route, and the operator does not own that external gateway
// infra, so a per-router auditLog has no agentgateway equivalent. Like the
// Gateway plane it runs BEFORE generation so a rejected auditLog yields no
// partial resources; refusing loudly avoids silently dropping an audit directive
// (a compliance footgun). The field stays fully valid in Proxy mode.
func agentGatewayUnsupportedAuditLogMessage(mr *inferencev1alpha1.ModelRouter) string {
	if mr.Spec.Policy == nil || mr.Spec.Policy.AuditLog == nil {
		return ""
	}
	return "policy.auditLog is a Proxy-mode field with no per-route equivalent in dataPlane: AgentGateway; " +
		"agentgateway access logging is configured on the gateway-scoped layer, not per route. Configure the " +
		"agentgateway access-log destination, or remove policy.auditLog"
}
