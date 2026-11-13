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

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// labelServiceName is the well-known EndpointSlice label that ties a slice to
// its Service. kube-proxy requires it for wiring, and every consumer lists
// slices by it. Kubernetes' built-in EndpointSliceMirroring controller stamps
// the same label on slices it mirrors from a legacy Endpoints object.
const labelServiceName = "kubernetes.io/service-name"

// labelInferenceService ties an agent-written Service or EndpointSlice to the
// InferenceService it serves.
const labelInferenceService = "llmkube.ai/inference-service"

const (
	managedByLabel = inferencev1alpha1.LabelManagedBy
	managedByValue = inferencev1alpha1.ManagedByMetalAgent
)

// EndpointNameConflictError reports that a Service or EndpointSlice with the
// InferenceService's name already exists and is owned neither by this agent
// (managed-by label) nor by the InferenceService itself (controller
// ownerReference). The agent leaves it untouched; the conflict is permanent
// until the InferenceService is renamed or the other object is removed.
type EndpointNameConflictError struct {
	Kind      string // "Service" or "EndpointSlice"
	Namespace string
	Name      string
}

func (e *EndpointNameConflictError) Error() string {
	return fmt.Sprintf("%s %s/%s already exists and is not managed by the metal-agent "+
		"(no %s=%s label); leaving it untouched; rename the InferenceService or remove that object",
		e.Kind, e.Namespace, e.Name, managedByLabel, managedByValue)
}

// ownedByAgent reports whether an object's labels mark it as created by the
// metal-agent.
func ownedByAgent(labels map[string]string) bool {
	return labels[managedByLabel] == managedByValue
}

// ownedFor reports whether the agent may write obj on behalf of the
// InferenceService with UID isvcUID: either the agent created it
// (ownedByAgent), or the InferenceService itself is its controller owner.
// The second case is a Service the controller created while the
// InferenceService ran on a CUDA Model; after a switch to a Metal Model the
// agent takes it over instead of refusing a legitimate migration. Deletion
// does not use this: deleteIfOwned stays label-only.
//
// deployment/macos/metal-agent-write-policy.yaml enforces the same rule at
// admission for the agent's ServiceAccounts (#2081), but for Services only:
// a write the agent makes outside it (an unlabeled object, or an
// EndpointSlice taken over by ownerReference) is denied there.
// write_policy_envtest_test.go drives this registry through that policy.
func ownedFor(obj metav1.Object, isvcUID types.UID) bool {
	if ownedByAgent(obj.GetLabels()) {
		return true
	}
	if isvcUID == "" {
		return false
	}
	ref := metav1.GetControllerOf(obj)
	return ref != nil && ref.UID == isvcUID
}

// ServiceRegistry manages Kubernetes Service and Endpoint resources
// to expose native Metal processes to the cluster
type ServiceRegistry struct {
	client  client.Client
	hostIP  string // explicit host IP; if empty, auto-detect via DNS
	version string // agent binary version stamped on endpoint annotations
	logger  *zap.SugaredLogger
	// retryBackoff bounds RegisterEndpointWithRetry. Overridable in tests.
	retryBackoff wait.Backoff
	// now returns the current time. Defaults to time.Now; overridable in tests
	// to assert deterministic heartbeat annotation values.
	now func() time.Time
	// ingressPort and ingressPin are set by EnableIngress (relay mode). With
	// ingressPort zero the registry runs in legacy mode.
	ingressPort int
	ingressPin  string
	// interfaces lists the host's interface addresses for auto-detect:
	// gatherHostIPCandidates, or a test's own list.
	interfaces func() []hostIPCandidate

	// addrMu guards followed.
	addrMu sync.Mutex
	// followed is the address learned from the agent's MetalNode (see
	// followHostIP), registered while --host-ip is not set. Empty means
	// auto-detect.
	followed string
}

// NewServiceRegistry creates a new service registry.
// If hostIP is non-empty it is used as the endpoint address registered in
// Kubernetes; otherwise the IP is auto-detected via DNS lookups
// (host.minikube.internal / host.docker.internal).
// version is the agent binary's version string (e.g. "v0.8.4") stamped on
// every EndpointSlice as AnnotationAgentVersion; pass empty to omit it.
func NewServiceRegistry(
	k8sClient client.Client,
	hostIP string,
	logger *zap.SugaredLogger,
	version string,
) *ServiceRegistry {
	return &ServiceRegistry{
		client:  k8sClient,
		hostIP:  hostIP,
		version: version,
		logger:  logger,
		retryBackoff: wait.Backoff{
			Duration: 2 * time.Second,
			Factor:   2,
			Steps:    5,
			Cap:      30 * time.Second,
		},
		now:        time.Now,
		interfaces: gatherHostIPCandidates,
	}
}

// EnableIngress switches the registry to relay mode: every endpoint is
// registered as "<isvc>-agent" (Service port 8443 -> the agent's TLS ingress
// port) with the ingress certificate's SPKI pin on the EndpointSlice, instead
// of "<isvc>" pointing at the engine port. The engine port callers pass is then
// ignored for the Service and slice. The agent's own legacy "<isvc>"
// EndpointSlice is removed after each relay write; the "<isvc>" Service is
// never touched in relay mode (the controller owns it for the relay). Call it
// once, before the registry is used. Without it the registry behaves as the
// legacy (pre-relay) writer.
//
// It returns an error, leaving the registry in legacy mode, when port is not
// positive or pin is not the standard base64 encoding of a SHA-256 digest:
// the controller refuses such a pin, so registering it would publish an
// ingress no relay can ever reach.
func (r *ServiceRegistry) EnableIngress(port int, pin string) error {
	if port <= 0 {
		return fmt.Errorf("ingress port %d must be positive", port)
	}
	raw, err := base64.StdEncoding.DecodeString(pin)
	if err != nil || len(raw) != sha256.Size {
		return fmt.Errorf("ingress SPKI pin %q is not the base64-std encoding of a %d-byte SHA-256 digest",
			pin, sha256.Size)
	}
	r.ingressPort = port
	r.ingressPin = pin
	return nil
}

// relayMode reports whether EnableIngress configured an ingress port.
func (r *ServiceRegistry) relayMode() bool {
	return r.ingressPort > 0
}

// serviceNameFor returns the Service and EndpointSlice name the agent writes
// for the InferenceService isvcName: the sanitized name in legacy mode, with
// MetalAgentServiceSuffix appended in relay mode. Every registry lookup keyed
// by InferenceService goes through it.
func (r *ServiceRegistry) serviceNameFor(isvcName string) string {
	name := sanitizeServiceName(isvcName)
	if r.relayMode() {
		return name + inferencev1alpha1.MetalAgentServiceSuffix
	}
	return name
}

// RegisterEndpoint creates/updates a Kubernetes Service and EndpointSlice
// to expose the native process to the cluster, marking the endpoint Ready so
// kube-proxy routes traffic to it.
func (r *ServiceRegistry) RegisterEndpoint(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	port int,
) error {
	return r.upsertEndpoint(ctx, isvc, port, true)
}

// WithdrawEndpoint keeps the Service and EndpointSlice present but flips the
// endpoint's Conditions.Ready to false so kube-proxy stops routing traffic to
// the host. It still refreshes the heartbeat annotation: the agent is alive,
// only the underlying runtime is unhealthy. That combination (fresh heartbeat
// + Ready: false) is the "alive but unhealthy" signal the operator reads,
// distinct from a stale heartbeat (dead agent, the #663 expiry path). Use this
// instead of UnregisterEndpoint, which deletes the Service+EndpointSlice (full
// teardown for delete / scale-to-zero). Recovery is just the next
// RegisterEndpoint flipping Ready back to true (#662).
func (r *ServiceRegistry) WithdrawEndpoint(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	port int,
) error {
	return r.upsertEndpoint(ctx, isvc, port, false)
}

// WithdrawEndpointIfPresent flips an existing agent-managed EndpointSlice for
// isvc to Ready=false without knowing the serving port. It is the withdrawal
// path for an InferenceService this agent owns but is not serving: a start
// that failed (format mismatch, missing executor, memory admission, spawn
// error) or a slice inherited from a previous agent process. Neither has a
// live child whose port could be passed to WithdrawEndpoint, so the port
// already recorded on the slice is reused and the write goes through the same
// upsertEndpoint path. The object is kept, so a later successful start flips
// it back to Ready (#1918).
//
// It never creates anything: with no slice there is nothing for kube-proxy to
// route to. A slice without this agent's managed-by label is left alone.
// Returns whether a withdrawal was written.
func (r *ServiceRegistry) WithdrawEndpointIfPresent(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
) (bool, error) {
	slice := &discoveryv1.EndpointSlice{}
	err := r.client.Get(ctx, types.NamespacedName{
		Namespace: isvc.Namespace,
		Name:      r.serviceNameFor(isvc.Name),
	}, slice)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get endpointslice: %w", err)
	}
	if !ownedByAgent(slice.Labels) {
		return false, nil
	}
	port := 0
	if len(slice.Ports) > 0 && slice.Ports[0].Port != nil {
		port = int(*slice.Ports[0].Port)
	}
	if port <= 0 {
		// An agent-written slice always carries a port; one without it cannot
		// be routed by kube-proxy, so there is nothing to withdraw.
		return false, nil
	}
	if sliceWithdrawnRecently(slice, r.now()) {
		// A failing start is retried on every watch event; skip the rewrite
		// while the slice is already unready and its heartbeat is younger than
		// one heartbeat interval, so retries do not become a write loop.
		return false, nil
	}
	if err := r.upsertEndpoint(ctx, isvc, port, false); err != nil {
		return false, err
	}
	return true, nil
}

// sliceWithdrawnRecently reports whether every endpoint on slice is already
// unready and its heartbeat annotation is younger than one heartbeat interval.
func sliceWithdrawnRecently(slice *discoveryv1.EndpointSlice, now time.Time) bool {
	for _, ep := range slice.Endpoints {
		if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
			return false
		}
	}
	ts, err := time.Parse(time.RFC3339, slice.Annotations[inferencev1alpha1.AnnotationAgentHeartbeat])
	if err != nil {
		return false
	}
	return now.Sub(ts) < inferencev1alpha1.DefaultAgentHeartbeatInterval
}

// WithdrawOwnedEndpoints withdraws (Ready=false) the agent-managed
// EndpointSlice of every InferenceService for which owns reports true. It runs
// once at agent startup: a fresh agent process serves nothing yet, so a slice
// a previous process left Ready points at a child that no longer exists. The
// next successful ensureProcess re-registers it Ready (#1918).
//
// owns is the agent's ownership predicate (the watcher's allowlist + metal
// check, #524), so a multi-host partition never touches a sibling agent's
// slices. Services whose InferenceService is gone are left to
// ReconcileOrphanEndpoints. Per-object errors are logged and skipped.
func (r *ServiceRegistry) WithdrawOwnedEndpoints(
	ctx context.Context,
	namespace string,
	owns func(context.Context, *inferencev1alpha1.InferenceService) bool,
) (int, error) {
	services := &corev1.ServiceList{}
	opts := []client.ListOption{
		client.MatchingLabels{managedByLabel: managedByValue},
	}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := r.client.List(ctx, services, opts...); err != nil {
		return 0, fmt.Errorf("list managed services: %w", err)
	}

	withdrawn := 0
	for i := range services.Items {
		svc := &services.Items[i]
		isvcName := svc.Labels["llmkube.ai/inference-service"]
		if isvcName == "" {
			continue
		}
		isvc := &inferencev1alpha1.InferenceService{}
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: isvcName}, isvc); err != nil {
			if !apierrors.IsNotFound(err) {
				r.logger.Warnw("failed to look up InferenceService for startup withdrawal",
					"namespace", svc.Namespace, "isvc", isvcName, "error", err)
			}
			continue
		}
		if !owns(ctx, isvc) {
			continue
		}
		ok, err := r.WithdrawEndpointIfPresent(ctx, isvc)
		if err != nil {
			r.logger.Warnw("failed to withdraw inherited endpoint at startup",
				"namespace", svc.Namespace, "isvc", isvcName, "error", err)
			continue
		}
		if ok {
			withdrawn++
		}
	}
	return withdrawn, nil
}

// CheckEndpointName reports whether the Service and EndpointSlice the agent
// would register for isvc are free to write: it returns
// *EndpointNameConflictError when either exists and is owned neither by this
// agent nor by isvc (see ownedFor), a wrapped error when a lookup fails, and
// nil otherwise. The agent calls it before starting an engine, so a taken
// name is refused without loading the model.
func (r *ServiceRegistry) CheckEndpointName(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	return r.checkEndpointOwnership(ctx, isvc.Namespace, r.serviceNameFor(isvc.Name), isvc.UID)
}

// checkEndpointOwnership reports a conflict if either the Service or the
// EndpointSlice named (namespace, name) already exists and is not owned for
// the InferenceService with UID isvcUID (ownedFor), checking the Service
// first. Called before either object is
// written: checking only inside each write's own mutate func (the in-write
// guards below) lets a Slice-only conflict leak an owned Service, because the
// Service write already committed by the time the Slice write fails and
// nothing else would ever clean it up. A NotFound Get is not a conflict; any
// other Get error is returned wrapped so the caller can tell it apart from a
// real ownership conflict.
func (r *ServiceRegistry) checkEndpointOwnership(
	ctx context.Context, namespace, name string, isvcUID types.UID,
) error {
	key := types.NamespacedName{Namespace: namespace, Name: name}

	svc := &corev1.Service{}
	switch err := r.client.Get(ctx, key, svc); {
	case err == nil:
		if !ownedFor(svc, isvcUID) {
			return &EndpointNameConflictError{Kind: "Service", Namespace: namespace, Name: name}
		}
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("check existing service: %w", err)
	}

	slice := &discoveryv1.EndpointSlice{}
	switch err := r.client.Get(ctx, key, slice); {
	case err == nil:
		if !ownedFor(slice, isvcUID) {
			return &EndpointNameConflictError{Kind: "EndpointSlice", Namespace: namespace, Name: name}
		}
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("check existing endpointslice: %w", err)
	}

	return nil
}

// upsertEndpoint is the shared Service+EndpointSlice writer behind
// RegisterEndpoint (ready=true) and WithdrawEndpoint (ready=false). The only
// difference between the two is the endpoint's Conditions.Ready value; the
// Service, labels, annotations (including the refreshed heartbeat), and port
// wiring are identical so a withdrawal keeps the address present-but-unready
// rather than tearing it down.
func (r *ServiceRegistry) upsertEndpoint(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	port int,
	ready bool,
) error {
	// Sanitized (DNS-1035) name, with the "-agent" suffix in relay mode.
	serviceName := r.serviceNameFor(isvc.Name)
	// In relay mode the Service and slice front the agent's TLS ingress; the
	// engine port the caller passed is loopback-only and never published.
	portName, servicePort, endpointPort := "http", int32(8080), port
	if r.relayMode() {
		portName, servicePort, endpointPort = "https", inferencev1alpha1.MetalAgentServicePort, r.ingressPort
	}

	// Check both objects for a foreign owner before writing either. Without
	// this, a Service-then-EndpointSlice conflict (Service name free, Slice
	// name taken) would commit the Service write, then fail on the Slice,
	// leaking a Service this agent now owns that nothing ever cleans up (the
	// InferenceService still exists, so ReconcileOrphanEndpoints skips it,
	// and a conflict is not retried).
	if err := r.checkEndpointOwnership(ctx, isvc.Namespace, serviceName, isvc.UID); err != nil {
		return err
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: isvc.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.client, service, func() error {
		// Backstop for an object created between the check above and this
		// write; the common case is already caught by checkEndpointOwnership.
		if service.ResourceVersion != "" && !ownedFor(service, isvc.UID) {
			return &EndpointNameConflictError{Kind: "Service", Namespace: service.Namespace, Name: service.Name}
		}
		service.Labels = map[string]string{
			"app":                          isvc.Name,
			managedByLabel:                 managedByValue,
			"llmkube.ai/inference-service": isvc.Name,
		}
		service.Annotations = map[string]string{
			"llmkube.ai/metal-accelerated": "true",
			"llmkube.ai/native-process":    "true",
		}
		service.Spec.Type = corev1.ServiceTypeClusterIP
		service.Spec.Ports = []corev1.ServicePort{{
			Name:       portName,
			Port:       servicePort,
			TargetPort: intstr.FromInt(endpointPort),
			Protocol:   corev1.ProtocolTCP,
		}}
		// No selector: Endpoints are managed manually.
		service.Spec.Selector = nil
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create/update service: %w", err)
	}

	// Resolved once, so the slice and the log line below name the same
	// address even if the MetalNode's verified address changes meanwhile.
	hostIP := r.resolveHostIP()
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: isvc.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.client, slice, func() error {
		// Backstop for an object created between the check above and this
		// write; the common case is already caught by checkEndpointOwnership.
		if slice.ResourceVersion != "" && !ownedFor(slice, isvc.UID) {
			return &EndpointNameConflictError{Kind: "EndpointSlice", Namespace: slice.Namespace, Name: slice.Name}
		}
		slice.Labels = map[string]string{
			labelServiceName:               serviceName,
			"app":                          isvc.Name,
			managedByLabel:                 managedByValue,
			"llmkube.ai/inference-service": isvc.Name,
		}
		if slice.Annotations == nil {
			slice.Annotations = map[string]string{}
		}
		slice.Annotations[inferencev1alpha1.AnnotationAgentHeartbeat] = r.now().UTC().Format(time.RFC3339)
		if r.version != "" {
			slice.Annotations[inferencev1alpha1.AnnotationAgentVersion] = r.version
		}
		if r.relayMode() {
			slice.Annotations[inferencev1alpha1.AnnotationAgentIngressSPKI] = r.ingressPin
			// The engine's own loopback port, for a foreman-agent using
			// --inference-base-url-host-override on the same host: the
			// "<isvc>" slice it would otherwise read now points at the
			// relay pod once the controller adopts it, not the engine.
			//
			// Only written on a ready registration. WithdrawEndpoint and
			// WithdrawEndpointIfPresent call this with ready=false and a
			// port that is not the live engine port (WithdrawEndpointIfPresent
			// reconstructs it from the slice's own Ports, which in relay mode
			// is the ingress port; a caller of WithdrawEndpoint may not know
			// the engine port either, e.g. a failed start). Writing it here
			// unconditionally would clobber the last known-good engine port
			// with that other value on every withdrawal (ruling 16). Leaving
			// the annotation untouched on a withdrawal preserves whatever
			// value CreateOrUpdate's Get already loaded into slice.Annotations
			// (or leaves it absent if it was never set).
			if ready {
				slice.Annotations[inferencev1alpha1.AnnotationAgentEnginePort] = strconv.Itoa(port)
			}
		}
		// resolveHostIP returns an IPv4 in every routable case, for an
		// address followed from the MetalNode (followHostIP takes IPv4 only)
		// and in the minikube/Docker-Desktop DNS fallback
		// (host.minikube.internal -> 192.168.65.254). The fallback never
		// yields a hostname, so IPv4 is a safe AddressType. If a future
		// host-IP source could return an FQDN, this must branch on the
		// address shape.
		slice.AddressType = discoveryv1.AddressTypeIPv4
		slice.Endpoints = []discoveryv1.Endpoint{{
			Addresses:  []string{hostIP},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(ready)},
			TargetRef: &corev1.ObjectReference{
				Kind: "Pod",
				Name: fmt.Sprintf("%s-metal", isvc.Name),
			},
		}}
		slice.Ports = []discoveryv1.EndpointPort{{
			Name:     ptr.To(portName),
			Port:     ptr.To(int32(endpointPort)), //nolint:gosec // G115: TCP ports fit in int32
			Protocol: ptr.To(corev1.ProtocolTCP),
		}}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create/update endpointslice: %w", err)
	}

	// Best-effort reap of a legacy core/v1 Endpoints object this agent (or a
	// prior version that predates the EndpointSlice migration, #684) may have
	// left behind under the same name. Done after the live slice is written so
	// there is no window where neither the slice nor the legacy object exists.
	r.reapLegacyEndpoints(ctx, isvc.Namespace, serviceName)
	if r.relayMode() {
		r.removeLegacySlice(ctx, isvc.Namespace, isvc.Name)
	}

	msg := "registered endpoint"
	if !ready {
		msg = "withdrew endpoint"
	}
	r.logger.Infow(msg,
		"namespace", isvc.Namespace,
		"name", isvc.Name,
		"service", serviceName,
		"hostIP", hostIP,
		"port", endpointPort,
		"enginePort", port,
	)

	return nil
}

// removeLegacySlice best-effort deletes the agent's own pre-relay "<isvc>"
// EndpointSlice after a relay-mode write. In relay mode that slice would point
// at a loopback-only engine port, so kube-proxy could only blackhole traffic to
// it. Only a slice carrying both the agent's managed-by label and the
// inference-service label for isvcName is deleted: InferenceService "m-agent"
// has the legacy name "m-agent", which is also InferenceService "m"'s relay
// slice, and must not remove it. The "<isvc>" Service is deliberately not
// touched: the controller owns it for the relay. A failure is logged and
// retried on the next write (every heartbeat), never propagated: registration
// itself succeeded.
func (r *ServiceRegistry) removeLegacySlice(ctx context.Context, namespace, isvcName string) {
	name := sanitizeServiceName(isvcName)
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if _, err := r.deleteOwned(ctx, key, &discoveryv1.EndpointSlice{}, isvcName); err != nil {
		r.logger.Warnw("failed to delete legacy EndpointSlice after relay registration; will retry",
			"namespace", namespace, "name", name, "error", err)
	}
}

// reapLegacyEndpoints best-effort deletes a legacy core/v1 Endpoints object
// this agent (or an older version) created before the EndpointSlice migration
// (#684). When an agent is upgraded across that change the old Endpoints object
// is orphaned, and Kubernetes' built-in EndpointSliceMirroring controller keeps
// generating a mirror EndpointSlice from it. On a selector-less Service that
// stale mirror unions with the agent's live slice, blackholing a share of
// traffic to a dead host:port and wedging the InferenceService in Progressing
// (issue #891).
//
// The operation is deliberately:
//   - Idempotent: a NotFound is the normal steady state and is ignored.
//   - Non-fatal: any error (NotFound, forbidden, transient) is logged at most
//     as a warning and never propagated, so it cannot fail registration.
//   - Conservative: only an Endpoints object carrying this agent's own
//     managed-by label is deleted. A user's unrelated Endpoints that happens to
//     share the name is left untouched, because the discriminator is the
//     llmkube.ai/managed-by=metal-agent label that only the agent ever stamps.
func (r *ServiceRegistry) reapLegacyEndpoints(ctx context.Context, namespace, serviceName string) {
	//nolint:staticcheck // SA1019: deliberately operating on the legacy core/v1 Endpoints API to reap it.
	legacy := &corev1.Endpoints{}
	err := r.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: serviceName}, legacy)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			r.logger.Warnw("failed to look up legacy Endpoints for reaping; skipping",
				"namespace", namespace, "name", serviceName, "error", err)
		}
		return
	}

	// Only reap the agent's own legacy artifact. Anything else is left alone.
	if !ownedByAgent(legacy.Labels) {
		r.logger.Debugw("Endpoints exists but is not agent-managed; leaving untouched",
			"namespace", namespace, "name", serviceName,
			"managed-by", legacy.Labels[managedByLabel])
		return
	}

	if err := r.client.Delete(ctx, legacy); err != nil {
		if apierrors.IsNotFound(err) {
			return // raced with another deleter; nothing to do
		}
		r.logger.Warnw("failed to delete legacy Endpoints; mirror slice may persist",
			"namespace", namespace, "name", serviceName, "error", err)
		return
	}
	r.logger.Infow("reaped legacy Endpoints to stop stale mirror EndpointSlice",
		"namespace", namespace, "name", serviceName)
}

// RegisterEndpointWithRetry retries RegisterEndpoint with exponential backoff
// so a brief API-server outage during a process respawn cannot strand stale
// Endpoints (issue #657). All errors are treated as retriable: the agent has
// no path to durable success other than the API server coming back.
func (r *ServiceRegistry) RegisterEndpointWithRetry(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	port int,
) error {
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, r.retryBackoff, func(ctx context.Context) (bool, error) {
		if lastErr = r.RegisterEndpoint(ctx, isvc, port); lastErr != nil {
			var conflict *EndpointNameConflictError
			if errors.As(lastErr, &conflict) {
				return false, lastErr // permanent; stop backing off
			}
			r.logger.Warnw("endpoint registration failed; will retry",
				"namespace", isvc.Namespace, "name", isvc.Name, "port", port, "error", lastErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		var conflict *EndpointNameConflictError
		if errors.As(err, &conflict) {
			return err
		}
		if lastErr == nil {
			lastErr = err // ctx cancelled before the first attempt
		}
		return fmt.Errorf("endpoint registration failed after retries: %w", lastErr)
	}
	return nil
}

// UnregisterEndpoint removes the Service and EndpointSlice for a process,
// deleting only objects this agent owns (deleteIfOwned). A same-named
// Service/EndpointSlice that predates the agent, or that another owner
// created, is left untouched.
func (r *ServiceRegistry) UnregisterEndpoint(ctx context.Context, namespace, name string) error {
	serviceName := r.serviceNameFor(name)
	key := types.NamespacedName{Namespace: namespace, Name: serviceName}

	if _, err := r.deleteIfOwned(ctx, key, &corev1.Service{}); err != nil {
		return fmt.Errorf("failed to delete service: %w", err)
	}
	if _, err := r.deleteIfOwned(ctx, key, &discoveryv1.EndpointSlice{}); err != nil {
		return fmt.Errorf("failed to delete endpointslice: %w", err)
	}
	return nil
}

// deleteIfOwned deletes the object at key only if it carries the metal-agent
// ownership label, reporting whether this call deleted it. Missing objects are
// fine. The delete is preconditioned on the UID and resourceVersion that were
// checked, so an object replaced in between is not removed.
func (r *ServiceRegistry) deleteIfOwned(
	ctx context.Context, key types.NamespacedName, obj client.Object,
) (bool, error) {
	return r.deleteOwned(ctx, key, obj, "")
}

// deleteOwned is deleteIfOwned that, when isvcName is non-empty, also requires
// the object's inference-service label to equal isvcName.
func (r *ServiceRegistry) deleteOwned(
	ctx context.Context, key types.NamespacedName, obj client.Object, isvcName string,
) (bool, error) {
	if err := r.client.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if !ownedByAgent(obj.GetLabels()) {
		r.logger.Warnw("not deleting an object the metal-agent does not own",
			"kind", fmt.Sprintf("%T", obj), "namespace", key.Namespace, "name", key.Name)
		return false, nil
	}
	if isvcName != "" && obj.GetLabels()[labelInferenceService] != isvcName {
		r.logger.Debugw("not deleting an agent object that belongs to another InferenceService",
			"kind", fmt.Sprintf("%T", obj), "namespace", key.Namespace, "name", key.Name,
			"isvc", obj.GetLabels()[labelInferenceService], "want", isvcName)
		return false, nil
	}
	uid, rv := obj.GetUID(), obj.GetResourceVersion()
	err := r.client.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &rv})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ReconcileOrphanEndpoints scans all Service objects labeled as managed by
// this agent and removes any whose corresponding InferenceService no longer
// exists. Intended to be called once at agent startup to clean up state left
// behind when the agent was down at the time an InferenceService was deleted.
//
// Why this is needed: the InferenceServiceWatcher only emits DELETED events
// for resources it observed in its *current* session — its `seen` map is
// reinitialized on each Watch() call. If a user deletes an InferenceService
// between agent restarts, the new agent session has no record of the prior
// resource and never invokes the cleanup path, so the K8s Service+Endpoints
// stay around forever. This reconciler closes that gap by treating the
// agent-managed-by label as the authoritative inventory of "things this
// agent created" and cross-checking each one against the live API.
//
// Each orphan is deleted by the listed Service's own name (and its same-named
// EndpointSlice), not by a name derived from the registry's mode, so the sweep
// removes legacy "<isvc>" leftovers and relay "<isvc>-agent" objects alike. A
// legacy "<isvc>" Service the controller adopted for the relay no longer
// carries the managed-by label, so it is never listed and never touched.
//
// Returns the number of orphaned Services this call actually deleted. Errors
// looking up any individual InferenceService are logged and skipped so one
// transient failure doesn't block cleanup of unrelated orphans.
func (r *ServiceRegistry) ReconcileOrphanEndpoints(ctx context.Context, namespace string) (int, error) {
	services := &corev1.ServiceList{}
	opts := []client.ListOption{
		client.MatchingLabels{managedByLabel: managedByValue},
	}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := r.client.List(ctx, services, opts...); err != nil {
		return 0, fmt.Errorf("list managed services: %w", err)
	}

	cleaned := 0
	for i := range services.Items {
		svc := &services.Items[i]
		isvcName := svc.Labels["llmkube.ai/inference-service"]
		if isvcName == "" {
			// Service is labeled managed-by us but missing the
			// inference-service label — should never happen given how
			// RegisterEndpoint stamps both, but skip rather than
			// guess at an owner.
			r.logger.Warnw(
				"managed service missing inference-service label, skipping reconcile",
				"namespace", svc.Namespace,
				"service", svc.Name,
			)
			continue
		}

		isvc := &inferencev1alpha1.InferenceService{}
		err := r.client.Get(ctx, types.NamespacedName{
			Namespace: svc.Namespace,
			Name:      isvcName,
		}, isvc)
		if err == nil {
			// InferenceService still exists — leave the Service+Endpoints alone.
			continue
		}
		if !apierrors.IsNotFound(err) {
			// Something else went wrong looking up the InferenceService;
			// log and move on. We'd rather leak a Service than delete one
			// whose owner-status we couldn't verify.
			r.logger.Warnw("failed to look up InferenceService for managed Service",
				"namespace", svc.Namespace,
				"service", svc.Name,
				"isvc", isvcName,
				"error", err,
			)
			continue
		}

		r.logger.Infow("cleaning up orphaned managed endpoint",
			"namespace", svc.Namespace,
			"service", svc.Name,
			"isvc", isvcName,
		)
		deleted, err := r.deleteOrphanPair(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name})
		if err != nil {
			r.logger.Warnw("failed to delete orphan endpoint",
				"namespace", svc.Namespace,
				"service", svc.Name,
				"error", err,
			)
			continue
		}
		if deleted {
			cleaned++
		}
	}
	return cleaned, nil
}

// deleteOrphanPair deletes the agent-owned EndpointSlice and Service at key,
// the slice first: the Service is the sweep's inventory, so if the slice
// delete fails the Service stays listed and the next startup retries both.
// It reports whether the Service was deleted by this call.
func (r *ServiceRegistry) deleteOrphanPair(ctx context.Context, key types.NamespacedName) (bool, error) {
	if _, err := r.deleteIfOwned(ctx, key, &discoveryv1.EndpointSlice{}); err != nil {
		return false, fmt.Errorf("delete endpointslice: %w", err)
	}
	deleted, err := r.deleteIfOwned(ctx, key, &corev1.Service{})
	if err != nil {
		return false, fmt.Errorf("delete service: %w", err)
	}
	return deleted, nil
}

// RemoveRelayRegistrations deletes the agent-owned "<isvc>-agent" Service and
// EndpointSlice of every InferenceService that still exists and for which owns
// reports true. It runs at startup in legacy mode: after a relay-to-legacy
// switch those objects would otherwise keep a fresh relay registration
// competing with the direct "<isvc>" one. A Service counts as a relay
// registration only when its name is the sanitized inference-service label
// plus MetalAgentServiceSuffix, so the legacy Service of an InferenceService
// literally named "x-agent" is kept. Pairs whose InferenceService is gone are
// left to ReconcileOrphanEndpoints. Per-object errors are logged and skipped.
// Returns the number of Services deleted.
func (r *ServiceRegistry) RemoveRelayRegistrations(
	ctx context.Context,
	namespace string,
	owns func(context.Context, *inferencev1alpha1.InferenceService) bool,
) (int, error) {
	services := &corev1.ServiceList{}
	opts := []client.ListOption{client.MatchingLabels{managedByLabel: managedByValue}}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := r.client.List(ctx, services, opts...); err != nil {
		return 0, fmt.Errorf("list managed services: %w", err)
	}
	removed := 0
	for i := range services.Items {
		svc := &services.Items[i]
		isvcName := svc.Labels[labelInferenceService]
		if isvcName == "" || svc.Name != sanitizeServiceName(isvcName)+inferencev1alpha1.MetalAgentServiceSuffix {
			continue
		}
		isvc := &inferencev1alpha1.InferenceService{}
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: isvcName}, isvc); err != nil {
			if !apierrors.IsNotFound(err) {
				r.logger.Warnw("failed to look up InferenceService for relay registration cleanup",
					"namespace", svc.Namespace, "isvc", isvcName, "error", err)
			}
			continue
		}
		if !owns(ctx, isvc) {
			continue
		}
		deleted, err := r.deleteOrphanPair(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name})
		if err != nil {
			r.logger.Warnw("failed to delete relay registration in legacy mode",
				"namespace", svc.Namespace, "service", svc.Name, "error", err)
			continue
		}
		if deleted {
			removed++
		}
	}
	return removed, nil
}

// sanitizeServiceName converts a name to be DNS-1035 compliant
// (lowercase alphanumeric characters or '-', must start with alpha, end with alphanumeric)
func sanitizeServiceName(name string) string {
	// Replace dots with dashes
	return strings.ReplaceAll(name, ".", "-")
}

// resolveHostIP returns the IP address that Kubernetes uses to reach this host.
// If an explicit hostIP was provided via --host-ip, that value is returned.
// Otherwise, for an agent that follows its MetalNode, the address it learned
// there (see followHostIP): the one the cluster verified, or before a
// verification the first one the dial-back tries. Otherwise it inspects the
// host's interfaces and applies selectHostIP's preference order (Tailscale >
// primary LAN, excluding bridge/NAT ranges), which a remote cluster or
// tailnet peer can actually route to. Only when no routable interface exists
// does it fall back to the legacy DNS detection for co-located minikube /
// Docker Desktop setups. Fixes defilantech/LLMKube#526.
func (r *ServiceRegistry) resolveHostIP() string {
	if r.hostIP != "" {
		return r.hostIP
	}
	if ip := r.followedHostIP(); ip != "" {
		return ip
	}
	return r.autoDetectHostIP(true)
}

// autoDetectHostIP is resolveHostIP's interface and DNS detection. It logs
// the choice when logChoice is set; followHostIP only names the address a
// switch moves from or to, and does not.
func (r *ServiceRegistry) autoDetectHostIP(logChoice bool) string {
	chosen, ok, rejected := selectHostIP(r.interfaces())
	if ok {
		if logChoice {
			r.logger.Infow("auto-detected host IP",
				"ip", chosen.ip.String(),
				"interface", chosen.iface,
				"rejected", formatRejected(rejected),
			)
		}
		return chosen.ip.String()
	}

	fallback := getHostIP()
	if logChoice {
		r.logger.Warnw("no routable interface for host-IP auto-detect; using co-located DNS fallback",
			"ip", fallback,
			"rejected", formatRejected(rejected),
		)
	}
	return fallback
}

func (r *ServiceRegistry) followedHostIP() string {
	r.addrMu.Lock()
	defer r.addrMu.Unlock()
	return r.followed
}

// onLocalInterface reports whether ip is assigned to one of this host's up,
// non-loopback interfaces.
func (r *ServiceRegistry) onLocalInterface(ip string) bool {
	want := net.ParseIP(ip)
	if want == nil {
		return false
	}
	for _, c := range r.interfaces() {
		if c.ip.Equal(want) {
			return true
		}
	}
	return false
}

// followHostIP sets the address registered while --host-ip is not set, as
// the agent learned it from its MetalNode (MetalAgent.metalNodeHostIP): an
// IPv4 address, or "" to return to auto-detect. It reports whether that
// changes the address the endpoints carry, so the caller re-registers them,
// and logs such a switch once, with why. With --host-ip it does nothing:
// that address is registered verbatim.
func (r *ServiceRegistry) followHostIP(ip, why string) bool {
	if r.hostIP != "" {
		return false
	}
	r.addrMu.Lock()
	prev := r.followed
	r.followed = ip
	r.addrMu.Unlock()
	if prev == ip {
		return false
	}
	from, to := prev, ip
	if from == "" {
		from = r.autoDetectHostIP(false)
	}
	if to == "" {
		to = r.autoDetectHostIP(false)
	}
	if from == to {
		return false
	}
	r.logger.Infow("host IP for the agent's endpoints changed; re-registering them",
		"from", from, "to", to, "reason", why)
	return true
}

// gatherHostIPCandidates enumerates the host's up, non-loopback interfaces
// and returns their unicast addresses as host-IP candidates.
func gatherHostIPCandidates() []hostIPCandidate {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []hostIPCandidate
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			out = append(out, hostIPCandidate{iface: iface.Name, ip: ip})
		}
	}
	return out
}

// formatRejected renders rejected candidates as "iface=ip (reason)" strings
// for a single structured log field.
func formatRejected(rejected []rejectedHostIP) []string {
	out := make([]string, 0, len(rejected))
	for _, r := range rejected {
		out = append(out, fmt.Sprintf("%s=%s (%s)", r.iface, r.ip, r.reason))
	}
	return out
}

// hostIPCandidate is a usable address discovered on a host network interface.
type hostIPCandidate struct {
	iface string
	ip    net.IP
}

// rejectedHostIP records a candidate the selector skipped and why, so the
// chosen-vs-rejected decision is visible in logs without a debug rerun.
type rejectedHostIP struct {
	iface  string
	ip     string
	reason string
}

// selectHostIP applies the host-IP preference policy to a candidate list:
// Tailscale (100.64.0.0/10 CGNAT) first, then any other routable IPv4,
// excluding loopback, link-local, and bridge/NAT ranges (lima/colima
// vmnet, Docker, kind/service nets). Returns ok=false when nothing
// routable is found so the caller can fall back. Pure for unit testing.
func selectHostIP(candidates []hostIPCandidate) (chosen hostIPCandidate, ok bool, rejected []rejectedHostIP) {
	var tailscale, lan *hostIPCandidate
	for i := range candidates {
		c := candidates[i]
		ip4 := c.ip.To4()
		switch {
		case ip4 == nil:
			rejected = append(rejected, rejectedHostIP{c.iface, c.ip.String(), "not IPv4"})
		case c.ip.IsLoopback():
			rejected = append(rejected, rejectedHostIP{c.iface, c.ip.String(), "loopback"})
		case c.ip.IsLinkLocalUnicast():
			rejected = append(rejected, rejectedHostIP{c.iface, c.ip.String(), "link-local"})
		case inAnyNet(ip4, excludedHostNets):
			rejected = append(rejected, rejectedHostIP{c.iface, c.ip.String(), "bridge/NAT range"})
		case tailscaleCGNAT.Contains(ip4):
			if tailscale == nil {
				cc := c
				tailscale = &cc
			}
		default:
			if lan == nil {
				cc := c
				lan = &cc
			}
		}
	}
	switch {
	case tailscale != nil:
		return *tailscale, true, rejected
	case lan != nil:
		return *lan, true, rejected
	default:
		return hostIPCandidate{}, false, rejected
	}
}

// tailscaleCGNAT is the 100.64.0.0/10 carrier-grade NAT range Tailscale
// assigns to tailnet nodes; preferred because a remote cluster joined to
// the same tailnet can always reach it.
var tailscaleCGNAT = mustParseCIDR("100.64.0.0/10")

// excludedHostNets are bridge / NAT / service ranges a remote cluster or
// peer cannot route to: lima/colima/Docker-Desktop vmnet, the Docker
// default bridge, and the kind / Kubernetes service CIDR.
var excludedHostNets = []*net.IPNet{
	mustParseCIDR("192.168.65.0/24"), // lima / colima / Docker Desktop vmnet
	mustParseCIDR("172.17.0.0/16"),   // Docker default bridge
	mustParseCIDR("10.96.0.0/12"),    // kind / Kubernetes service CIDR
}

func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(fmt.Sprintf("invalid CIDR %q: %v", s, err))
	}
	return n
}

func inAnyNet(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// getHostIP returns the auto-detected IP address that Kubernetes can use to
// reach the host machine. For minikube, this is typically
// host.minikube.internal which resolves to 192.168.65.254.
func getHostIP() string {
	// Try to resolve host.minikube.internal (for minikube)
	if ips, err := net.LookupIP("host.minikube.internal"); err == nil && len(ips) > 0 {
		return ips[0].String()
	}

	// Fallback: Try to resolve host.docker.internal (for Docker Desktop)
	if ips, err := net.LookupIP("host.docker.internal"); err == nil && len(ips) > 0 {
		return ips[0].String()
	}

	// Final fallback: Use a common default for minikube
	return "192.168.65.254"
}
