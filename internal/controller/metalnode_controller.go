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
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

const (
	// metalNodeResyncInterval requeues every MetalNode so a Lease that
	// stopped being renewed turns stale without any event.
	metalNodeResyncInterval = 60 * time.Second
	// metalNodeRecheckInterval re-verifies an unchanged, reachable Mac.
	metalNodeRecheckInterval = 10 * time.Minute
	// metalNodeVerifierTimeout gives up on a verifier pod that has not
	// finished. activeDeadlineSeconds only runs once the pod has started, so
	// an unschedulable verifier would otherwise wait forever.
	metalNodeVerifierTimeout = 2 * time.Minute
	// metalNodeVerifierPollInterval requeues while a verifier runs, as a
	// backstop for the pod watch.
	metalNodeVerifierPollInterval = 5 * time.Second
	// metalNodeHeartbeatWindowUnset is how long a Lease with no
	// leaseDurationSeconds counts as fresh.
	metalNodeHeartbeatWindowUnset = 2 * time.Minute
	// metalNodeMaxLeaseDuration caps the agent-written leaseDurationSeconds,
	// so a Mac cannot claim a heartbeat that stays fresh for hours.
	metalNodeMaxLeaseDuration = 5 * time.Minute
	// metalNodeMinStartSpacing is the least time between two verifier starts
	// for one MetalNode, whatever changed.
	metalNodeMinStartSpacing = 30 * time.Second
	// metalNodeMaxClockSkew is how far in the future a renewTime may be
	// before it counts as stale rather than fresh.
	metalNodeMaxClockSkew = 30 * time.Second
	// metalNodeHeartbeatResolution is how far status.lastHeartbeatTime may
	// lag the Lease before it is advanced.
	metalNodeHeartbeatResolution = 60 * time.Second
	// metalNodeRenewGap is a renewal gap that means the agent was away (it
	// renews every 15 s), so the update reconciles at once.
	metalNodeRenewGap = 45 * time.Second
	// metalNodeMaxResultAge is how long a True Reachable stands without a
	// newer check finishing. An unchanged Mac is re-checked after
	// metalNodeRecheckInterval and that check ends, one way or the other,
	// within metalNodeVerifierTimeout; the resync interval is slack for
	// when the reconcile that reads it runs. Only a check that keeps being
	// cut short (inputs that keep changing, a verifier pod that never goes
	// away) reaches the bound, and then the result is too old to stand for
	// the address.
	metalNodeMaxResultAge = metalNodeRecheckInterval + metalNodeVerifierTimeout + metalNodeResyncInterval
)

// metalNodeLeaseUpdateMatters reports whether a Lease update should
// reconcile its MetalNode. An on-schedule renewal that changes nothing but
// renewTime does not: the 60 s resync turns a stopped agent stale, and the
// dial-back depends only on the report. A changed report, label, holder or
// duration does, and so does the first renewal or one after a gap (the agent
// came back), so AgentConnected turns True promptly.
func metalNodeLeaseUpdateMatters(old, updated *coordinationv1.Lease) bool {
	if old == nil || updated == nil {
		return true
	}
	if !apiequality.Semantic.DeepEqual(old.Labels, updated.Labels) ||
		old.Annotations[metalnode.ReportAnnotation] != updated.Annotations[metalnode.ReportAnnotation] ||
		!apiequality.Semantic.DeepEqual(old.Spec.HolderIdentity, updated.Spec.HolderIdentity) ||
		!apiequality.Semantic.DeepEqual(old.Spec.LeaseDurationSeconds, updated.Spec.LeaseDurationSeconds) {
		return true
	}
	o, n := old.Spec.RenewTime, updated.Spec.RenewTime
	if o == nil || n == nil {
		return o != n
	}
	return n.Sub(o.Time) > metalNodeRenewGap
}

// MetalNodeReconciler fills MetalNode status from the agent's Lease and
// verifies the Mac is reachable with a dial-back verifier pod. The agent
// never writes the MetalNode; this controller is the only writer of its
// status. It does nothing until a MetalNode exists.
type MetalNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads the MetalNode and the agent's Lease. The MetalNode is
	// read uncached because status.verification.podUID is the only record of
	// which verifier pod the controller started: that pod's own create event
	// reconciles again, and a cached MetalNode from before the status write
	// would treat the pod as a stranger and delete it. The Lease informer only
	// holds Leases labeled inference.llmkube.dev/metal-node, so reading
	// through it would miss an unlabeled Lease; an uncached read always finds
	// it.
	APIReader client.Reader
	// InitContainerImage is the operator's curl image (--init-container-image),
	// which the verifier pod runs.
	InitContainerImage string
	// AllowedCIDRs are the networks the verifier may dial
	// (--metal-node-allowed-cidrs).
	AllowedCIDRs []netip.Prefix
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=metalnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=metalnodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=metalnodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;delete

func (r *MetalNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	mn := &inferencev1alpha1.MetalNode{}
	if err := r.APIReader.Get(ctx, req.NamespacedName, mn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := r.now()
	orig := mn.DeepCopy()
	mn.Status.ObservedGeneration = mn.Generation

	report, connected, err := r.observeLease(ctx, mn, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	// A dial-back error (a Forbidden pod create, a failed list) must not
	// cost the MetalNode its observed status: store what is known first,
	// then return the error so the request is retried.
	requeue, toDelete, dialErr := r.reconcileDialBack(ctx, mn, report, connected, now,
		orig.Status.IngressSPKI, orig.Status.IngressPort)
	requeue = min(metalNodeResyncInterval, requeue)
	if meta.FindStatusCondition(mn.Status.Conditions, inferencev1alpha1.MetalNodeConditionReachable) == nil {
		r.setCondition(mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionUnknown,
			inferencev1alpha1.MetalNodeReasonNotVerified, "no dial-back has completed yet")
	}

	if !apiequality.Semantic.DeepEqual(orig.Status, mn.Status) {
		if err := r.Status().Patch(ctx, mn, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, fmt.Errorf("update MetalNode %s status: %w", req.NamespacedName, err)
		}
	}
	// Finished verifier pods go only after their result is stored: a lost
	// status write then re-reads the same pod instead of losing the result.
	for i := range toDelete {
		if err := r.Delete(ctx, &toDelete[i], client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("delete verifier pod %s: %w", toDelete[i].Name, err)
		}
	}
	if dialErr != nil {
		return ctrl.Result{}, dialErr
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *MetalNodeReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *MetalNodeReconciler) setCondition(mn *inferencev1alpha1.MetalNode, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&mn.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: mn.Generation,
	})
}

// observeLease reads the agent's Lease, sets AgentConnected, advances
// lastHeartbeatTime (throttled) and copies a valid report's facts into
// status. Without a valid report (no Lease, or a report that does not
// decode) the facts are cleared: nothing current backs them, and they carry
// no time of their own. A stale but valid report keeps them as the last
// known facts, dated by lastHeartbeatTime. It returns the report when it is
// valid (stale or not) and whether the heartbeat is fresh. Messages never
// quote the agent's values.
func (r *MetalNodeReconciler) observeLease(ctx context.Context, mn *inferencev1alpha1.MetalNode, now time.Time) (*metalnode.Report, bool, error) {
	wasConnected := meta.IsStatusConditionTrue(mn.Status.Conditions, inferencev1alpha1.MetalNodeConditionAgentConnected)
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: mn.Namespace, Name: mn.Spec.LeaseName}
	if err := r.APIReader.Get(ctx, key, lease); err != nil {
		if apierrors.IsNotFound(err) {
			copyReportFacts(&mn.Status, metalnode.Report{})
			r.setCondition(mn, inferencev1alpha1.MetalNodeConditionAgentConnected, metav1.ConditionFalse,
				inferencev1alpha1.MetalNodeReasonLeaseNotFound,
				fmt.Sprintf("Lease %s not found; create it with llmkube metal token create", key))
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get Lease %s: %w", key, err)
	}

	obs := evaluateLease(lease, key, now)
	if obs.report != nil {
		copyReportFacts(&mn.Status, *obs.report)
	} else {
		copyReportFacts(&mn.Status, metalnode.Report{})
	}
	r.setCondition(mn, inferencev1alpha1.MetalNodeConditionAgentConnected, obs.status, obs.reason, obs.message)
	connected := obs.status == metav1.ConditionTrue
	if rt := lease.Spec.RenewTime; rt != nil && !rt.Time.After(now.Add(metalNodeMaxClockSkew)) {
		advanceHeartbeat(&mn.Status, rt.Time, connected != wasConnected)
	}
	return obs.report, connected, nil
}

// leaseObservation is what one read of the agent's Lease says.
type leaseObservation struct {
	status  metav1.ConditionStatus
	reason  string
	message string
	// report is the decoded report when it is valid, stale or not.
	report *metalnode.Report
}

func evaluateLease(lease *coordinationv1.Lease, key types.NamespacedName, now time.Time) leaseObservation {
	raw, ok := lease.Annotations[metalnode.ReportAnnotation]
	if !ok {
		return leaseObservation{status: metav1.ConditionFalse, reason: inferencev1alpha1.MetalNodeReasonReportInvalid,
			message: fmt.Sprintf("Lease %s carries no %s annotation", key, metalnode.ReportAnnotation)}
	}
	report, err := metalnode.Decode(raw)
	if err == nil {
		err = checkIngressClaims(report)
	}
	if err != nil {
		return leaseObservation{status: metav1.ConditionFalse, reason: inferencev1alpha1.MetalNodeReasonReportInvalid,
			message: fmt.Sprintf("the report on Lease %s is invalid: %v", key, err)}
	}
	if fresh, msg := heartbeatFresh(lease, now); !fresh {
		return leaseObservation{status: metav1.ConditionFalse, reason: inferencev1alpha1.MetalNodeReasonHeartbeatStale,
			message: msg, report: &report}
	}
	return leaseObservation{status: metav1.ConditionTrue, reason: inferencev1alpha1.MetalNodeReasonHeartbeatFresh,
		message: fmt.Sprintf("the agent is renewing Lease %s", key), report: &report}
}

// advanceHeartbeat moves lastHeartbeatTime the way the kubelet's node status
// does since node Leases (KEP-589): when AgentConnected changes, or when the
// stored time is a minute behind. Each Mac renews every 15 seconds, and a
// status write per renewal would add one MetalNode write per Mac per renewal
// for no reader's benefit; the Lease holds the exact time.
func advanceHeartbeat(st *inferencev1alpha1.MetalNodeStatus, renewed time.Time, flipped bool) {
	stored := st.LastHeartbeatTime
	if stored == nil || flipped || renewed.Before(stored.Time) || renewed.Sub(stored.Time) >= metalNodeHeartbeatResolution {
		st.LastHeartbeatTime = &metav1.Time{Time: renewed}
	}
}

// checkIngressClaims applies the relay's own pin rule, so the dial-back
// never pins a key the relay would refuse. The port floor is enforced by
// metalnode.Decode. An ingress certificate must be a certificate for the
// pinned key, signed by that key: the agent's key put the MetalNode's name
// in it, which a report pairing another server's key with the name cannot
// show. Which MetalNode it names is the dial-back's to check.
func checkIngressClaims(report metalnode.Report) error {
	if report.IngressSPKI == "" {
		if report.IngressCert != "" {
			return fmt.Errorf("ingressCert is set but ingressSPKI is not")
		}
		return nil
	}
	if !validIngressPin(report.IngressSPKI) {
		return fmt.Errorf("ingressSPKI is not the base64 encoding of a SHA-256 digest")
	}
	if report.IngressPort == 0 {
		return fmt.Errorf("ingressSPKI is set but ingressPort is not")
	}
	if report.IngressCert == "" {
		return nil
	}
	cert, err := parseIngressCert(report.IngressCert)
	if err != nil {
		return fmt.Errorf("ingressCert is not a DER certificate")
	}
	if sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo); base64.StdEncoding.EncodeToString(sum[:]) != report.IngressSPKI {
		return fmt.Errorf("ingressCert is not for the key ingressSPKI pins")
	}
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		return fmt.Errorf("ingressCert is not signed by its own key")
	}
	return nil
}

// heartbeatFresh reports whether the Lease was renewed within three lease
// durations (2 minutes when the duration is unset). A renewTime more than
// metalNodeMaxClockSkew in the future is not fresh: it would otherwise never
// go stale.
func heartbeatFresh(lease *coordinationv1.Lease, now time.Time) (bool, string) {
	if lease.Spec.RenewTime == nil {
		return false, fmt.Sprintf("Lease %s has never been renewed; is the agent running with --metal-node?", lease.Name)
	}
	renewed := lease.Spec.RenewTime.Time
	if renewed.After(now.Add(metalNodeMaxClockSkew)) {
		return false, fmt.Sprintf("Lease %s has a renewTime more than %s in the future; check the Mac's clock",
			lease.Name, metalNodeMaxClockSkew)
	}
	window := metalNodeHeartbeatWindowUnset
	if d := lease.Spec.LeaseDurationSeconds; d != nil && *d > 0 {
		window = 3 * min(time.Duration(*d)*time.Second, metalNodeMaxLeaseDuration)
	}
	if now.Sub(renewed) > window {
		return false, fmt.Sprintf("Lease %s was last renewed at %s, more than %s ago",
			lease.Name, renewed.UTC().Format(time.RFC3339), window)
	}
	return true, ""
}

func copyReportFacts(st *inferencev1alpha1.MetalNodeStatus, report metalnode.Report) {
	st.AgentVersion = report.Version
	st.IngressPort = report.IngressPort
	st.IngressSPKI = report.IngressSPKI
	capacity := &inferencev1alpha1.MetalNodeCapacity{
		MemoryBytes:  report.MemoryBytes,
		Chip:         report.Chip,
		GPUCores:     report.GPUCores,
		MetalVersion: report.MetalVersion,
	}
	if report.MemoryBytes > 0 {
		capacity.Memory = resource.NewQuantity(report.MemoryBytes, resource.BinarySI).String()
	}
	if *capacity == (inferencev1alpha1.MetalNodeCapacity{}) {
		capacity = nil
	}
	st.Capacity = capacity
}

// reconcileDialBack keeps the candidate list current, harvests this
// MetalNode's verifier pods, and starts a new verifier when one is due. It
// returns how soon to look again, the pods to delete once status is stored,
// and an error that must be returned after the status write.
//
// The Mac controls its report, so nothing in it may make the controller run
// more pods: every verifier pod this MetalNode owns counts as busy until it
// is gone (terminating ones included), and starts are at least
// metalNodeMinStartSpacing apart whatever changed.
func (r *MetalNodeReconciler) reconcileDialBack(ctx context.Context, mn *inferencev1alpha1.MetalNode, report *metalnode.Report,
	connected bool, now time.Time, prevPin string, prevPort int32) (time.Duration, []corev1.Pod, error) {
	st := &mn.Status
	hostIP, hostIPValid := specHostIP(mn)
	var hash string
	if report != nil {
		desired := metalNodeDesiredCandidates(hostIP, *report, r.AllowedCIDRs)
		hash = metalNodeInputHash(report.IngressSPKI, report.IngressCert, report.IngressPort,
			dialableAddresses(desired), mn.Spec.Verifier)
		keep := st.Verification != nil && st.Verification.InputHash == hash
		st.Candidates = mergeCandidates(st.Candidates, desired, keep)
		if !keep && verifiedResultStale(st, report, prevPin, prevPort) {
			r.setReachable(mn, metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonVerificationPending,
				"the agent's pin, port or addresses changed since the last check; a new check is due")
		}
	}

	pods, err := r.ownedVerifiers(ctx, mn)
	if err != nil {
		return 0, nil, err
	}
	if verifiedResultExpired(mn, now) {
		msg := fmt.Sprintf("the last check that reached this Mac finished more than %s ago and none has finished since; a new check is due",
			metalNodeMaxResultAge)
		if blocking := terminatingVerifier(pods); blocking != nil {
			msg += fmt.Sprintf("; verifier pod %s has been terminating since %s and no check starts until it is gone",
				blocking.Name, blocking.DeletionTimestamp.UTC().Format(time.RFC3339))
		}
		r.setReachable(mn, metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonVerificationPending, msg)
	}
	switch {
	case !hostIPValid:
		r.setReachable(mn, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonInvalidSpec,
			"spec.hostIP is not an IP address; nothing is dialed until it is fixed")
		return metalNodeResyncInterval, stopVerifiers(pods), nil
	case report == nil && agentConnectedReason(mn) == inferencev1alpha1.MetalNodeReasonReportInvalid:
		// The agent may well be renewing its Lease; what stops the check is
		// the report, so Reachable points there rather than at the network.
		r.setReachable(mn, metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonReportInvalid,
			"the agent's report on its Lease is invalid (see AgentConnected); reachability is not checked until it reports a valid one")
		return metalNodeResyncInterval, stopVerifiers(pods), nil
	case report == nil || !connected:
		r.setReachable(mn, metav1.ConditionUnknown, inferencev1alpha1.MetalNodeReasonAgentNotConnected,
			"the agent is not connected (see AgentConnected); reachability is not checked until it is")
		return metalNodeResyncInterval, stopVerifiers(pods), nil
	}

	busy, toDelete := r.harvestVerifiers(mn, pods, hash, now)
	addrs := dialableAddresses(st.Candidates)
	switch {
	case report.IngressSPKI == "":
		r.setReachable(mn, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonPinMissing,
			"the agent reports no ingress SPKI pin (is it running with --legacy-direct-endpoints?); nothing to verify against")
		return metalNodeResyncInterval, toDelete, nil
	case report.IngressCert == "":
		r.setReachable(mn, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonAgentUpgradeRequired,
			"the agent reports an ingress pin but no ingress certificate, so the dial-back cannot tell its ingress "+
				"from another server at the same address; upgrade the metal-agent to this controller's version")
		return metalNodeResyncInterval, toDelete, nil
	case !ingressCertNames(report.IngressCert, mn.UID):
		r.setReachable(mn, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonCertificateUnbound,
			fmt.Sprintf("the agent's ingress certificate does not name this MetalNode (%s); the agent re-issues it "+
				"once it reads its MetalNode, so see the agent log for why it cannot", metalnode.IngressHostname(string(mn.UID))))
		return metalNodeResyncInterval, toDelete, nil
	case len(addrs) == 0:
		r.setReachable(mn, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonNoCandidates,
			fmt.Sprintf("no candidate address is inside the allowed CIDRs (%d reported); set spec.hostIP or widen --metal-node-allowed-cidrs",
				len(st.Candidates)))
		return metalNodeResyncInterval, toDelete, nil
	case busy:
		return metalNodeVerifierPollInterval, toDelete, nil
	}
	if wait := dialBackWait(mn, hash, now); wait > 0 {
		return wait, toDelete, nil
	}

	prev := st.Verification.DeepCopy()
	v := verification(mn)
	started := metav1.NewTime(now)
	v.LastStartTime = &started
	v.PodUID = ""
	pod, err := buildMetalNodeVerifierPod(mn, r.verifierImage(), report.IngressSPKI, report.IngressCert,
		report.IngressPort, addrs, hash)
	if err != nil {
		return 0, toDelete, err
	}
	if err := controllerutil.SetControllerReference(mn, pod, r.Scheme); err != nil {
		return 0, toDelete, fmt.Errorf("own verifier pod: %w", err)
	}
	if err := r.Create(ctx, pod); apierrors.IsAlreadyExists(err) {
		return r.verifierExists(ctx, mn, prev, pod.Name, hash, now, toDelete)
	} else if err != nil {
		r.recordVerifierFailure(mn, hash, now,
			"the controller could not create the verifier pod; see the controller log")
		return metalNodeVerifierPollInterval, toDelete, fmt.Errorf("create verifier pod %s: %w", pod.Name, err)
	}
	v.PodUID = string(pod.UID)
	log.FromContext(ctx).V(1).Info("started MetalNode dial-back", "pod", pod.Name, "candidates", len(addrs))
	return metalNodeVerifierPollInterval, toDelete, nil
}

// verifierExists handles a verifier create that found the pod's name taken.
// The name follows the inputs, so a verifier this MetalNode owns is already
// checking them and the pod list has not caught up: that is a running check,
// and status.verification stays as it was (it may track that very pod). A pod
// of that name this MetalNode does not own blocks every check for these
// inputs, which Reachable says.
func (r *MetalNodeReconciler) verifierExists(ctx context.Context, mn *inferencev1alpha1.MetalNode,
	prev *inferencev1alpha1.MetalNodeVerification, name, hash string, now time.Time,
	toDelete []corev1.Pod) (time.Duration, []corev1.Pod, error) {
	existing := &corev1.Pod{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: mn.Namespace, Name: name}, existing); err != nil {
		mn.Status.Verification = prev
		return metalNodeVerifierPollInterval, toDelete, fmt.Errorf("get existing verifier pod %s: %w", name, err)
	}
	if owner := metav1.GetControllerOf(existing); owner == nil || owner.UID != mn.UID {
		r.recordVerifierFailure(mn, hash, now, fmt.Sprintf(
			"pod %s, the verifier's name for these inputs, exists and is not owned by this MetalNode; delete it", name))
		return metalNodeVerifierPollInterval, toDelete, nil
	}
	mn.Status.Verification = prev
	return metalNodeVerifierPollInterval, toDelete, nil
}

// verifiedResultStale reports whether a True Reachable no longer describes
// the current inputs: the pin or port changed since the status was last
// written (the result proved the old key or port), or status.hostIP is no
// longer among the addresses being dialed. A result for inputs that only
// grew (a new address beside the verified one) stands until the next check
// lands.
func verifiedResultStale(st *inferencev1alpha1.MetalNodeStatus, report *metalnode.Report, prevPin string, prevPort int32) bool {
	if st.HostIP == "" {
		return false
	}
	if report.IngressSPKI != prevPin || report.IngressPort != prevPort {
		return true
	}
	return !slices.Contains(dialableAddresses(st.Candidates), st.HostIP)
}

// verifiedResultExpired reports whether a True Reachable is older than
// metalNodeMaxResultAge. A result carried across changed inputs (only grown,
// see verifiedResultStale) stands until a check for the new inputs lands,
// and a verifier whose inputs change again is deleted without a result: a
// Mac that keeps changing its report would otherwise keep an old success,
// and the hostIP it names, for good.
func verifiedResultExpired(mn *inferencev1alpha1.MetalNode, now time.Time) bool {
	if !meta.IsStatusConditionTrue(mn.Status.Conditions, inferencev1alpha1.MetalNodeConditionReachable) {
		return false
	}
	v := mn.Status.Verification
	return v == nil || v.LastCheckTime == nil || now.Sub(v.LastCheckTime.Time) > metalNodeMaxResultAge
}

// agentConnectedReason is the reason observeLease gave AgentConnected.
func agentConnectedReason(mn *inferencev1alpha1.MetalNode) string {
	if c := meta.FindStatusCondition(mn.Status.Conditions, inferencev1alpha1.MetalNodeConditionAgentConnected); c != nil {
		return c.Reason
	}
	return ""
}

// specHostIP returns spec.hostIP in canonical form and whether it is usable
// (empty, or an IP address without a zone).
func specHostIP(mn *inferencev1alpha1.MetalNode) (string, bool) {
	if mn.Spec.HostIP == "" {
		return "", true
	}
	addr, err := netip.ParseAddr(mn.Spec.HostIP)
	if err != nil || addr.Zone() != "" {
		return "", false
	}
	return addr.Unmap().String(), true
}

func (r *MetalNodeReconciler) verifierImage() string {
	if r.InitContainerImage != "" {
		return r.InitContainerImage
	}
	return defaultPrefetchImage
}

// mergeCandidates is the desired list, carrying over the last result for an
// address that is still dialed when the inputs that decide the probe are
// unchanged (keep). A reworded source keeps its result; new inputs start
// Pending.
func mergeCandidates(current, desired []inferencev1alpha1.MetalNodeCandidate, keep bool) []inferencev1alpha1.MetalNodeCandidate {
	if !keep {
		return desired
	}
	prev := map[string]inferencev1alpha1.MetalNodeCandidate{}
	for _, c := range current {
		if c.Result != inferencev1alpha1.CandidateNotAllowed {
			prev[c.Address] = c
		}
	}
	out := make([]inferencev1alpha1.MetalNodeCandidate, len(desired))
	for i, c := range desired {
		if p, ok := prev[c.Address]; ok && c.Result != inferencev1alpha1.CandidateNotAllowed {
			c.Result, c.CheckedAt = p.Result, p.CheckedAt
		}
		out[i] = c
	}
	return out
}

// dialBackWait is how long until the next check is due: at once for new
// inputs, after the backoff while Reachable is not True, and after
// metalNodeRecheckInterval otherwise; and never sooner than
// metalNodeMinStartSpacing after the last start.
func dialBackWait(mn *inferencev1alpha1.MetalNode, hash string, now time.Time) time.Duration {
	v := mn.Status.Verification
	if v == nil {
		return 0
	}
	var wait time.Duration
	if v.LastCheckTime != nil && v.InputHash == hash {
		interval := metalNodeRecheckInterval
		if c := meta.FindStatusCondition(mn.Status.Conditions, inferencev1alpha1.MetalNodeConditionReachable); c != nil && c.Status != metav1.ConditionTrue {
			interval = metalNodeDialBackBackoff(v.ConsecutiveFailures)
		}
		wait = interval - now.Sub(v.LastCheckTime.Time)
	}
	if v.LastStartTime != nil {
		wait = max(wait, metalNodeMinStartSpacing-now.Sub(v.LastStartTime.Time))
	}
	if v.LastCheckTime != nil {
		// lastCheckTime is stored to the second; a verifier started in that
		// second could finish, clamped to its start, no later than it, and
		// unread would take its result for one already read.
		wait = max(wait, v.LastCheckTime.Truncate(time.Second).Add(time.Second).Sub(now))
	}
	return max(0, wait)
}

// ownedVerifiers lists the verifier pods this MetalNode controls.
func (r *MetalNodeReconciler) ownedVerifiers(ctx context.Context, mn *inferencev1alpha1.MetalNode) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(mn.Namespace),
		client.MatchingLabels{"app.kubernetes.io/component": metalNodeVerifierComponent}); err != nil {
		return nil, fmt.Errorf("list verifier pods: %w", err)
	}
	var out []corev1.Pod
	for _, pod := range pods.Items {
		if owner := metav1.GetControllerOf(&pod); owner != nil && owner.UID == mn.UID {
			out = append(out, pod)
		}
	}
	return out, nil
}

// terminatingVerifier is a verifier pod being deleted, which keeps any new
// check from starting until it is gone, or nil.
func terminatingVerifier(pods []corev1.Pod) *corev1.Pod {
	for i := range pods {
		if pods[i].DeletionTimestamp != nil {
			return &pods[i]
		}
	}
	return nil
}

// stopVerifiers is every verifier pod not already being deleted.
func stopVerifiers(pods []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range pods {
		if p.DeletionTimestamp == nil {
			out = append(out, p)
		}
	}
	return out
}

// harvestVerifiers looks at this MetalNode's verifier pods. Only the pod the
// controller last created (status.verification.podUID), for the current
// inputs, can report a result, and only once. Any other pod, a finished
// pod, or one stuck past metalNodeVerifierTimeout is returned for deletion.
// busy is true while any verifier pod exists, terminating ones included.
func (r *MetalNodeReconciler) harvestVerifiers(mn *inferencev1alpha1.MetalNode, pods []corev1.Pod, hash string,
	now time.Time) (busy bool, toDelete []corev1.Pod) {
	for _, pod := range pods {
		busy = true
		if pod.DeletionTimestamp != nil {
			continue
		}
		v := mn.Status.Verification
		tracked := v != nil && v.PodUID != "" && v.PodUID == string(pod.UID)
		current := tracked && pod.Annotations[annotationMetalNodeVerifyHash] == hash
		term := verifierTerminated(&pod)
		switch {
		case term != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed:
			finished := now
			if term != nil && !term.FinishedAt.IsZero() {
				finished = term.FinishedAt.Time
			}
			finished = clampFinished(v, finished, now)
			if current && unread(mn, finished) {
				r.applyVerifierResult(mn, &pod, term, hash, finished)
			}
			toDelete = append(toDelete, pod)
		case !current:
			toDelete = append(toDelete, pod)
		case now.Sub(pod.CreationTimestamp.Time) > metalNodeVerifierTimeout:
			r.recordVerifierFailure(mn, hash, now, fmt.Sprintf(
				"verifier pod %s did not finish within %s; check that spec.verifier lets it schedule and pull %s",
				pod.Name, metalNodeVerifierTimeout, pod.Spec.Containers[0].Image))
			toDelete = append(toDelete, pod)
		}
	}
	return busy, toDelete
}

// clampFinished bounds a verifier's finish time, which the kubelet stamps on
// its node's clock, to when the controller started it (lastStartTime) and
// now, both on the controller's clock. lastCheckTime is compared with the
// controller's clock (the recheck, the backoff, the result's age) and with
// failures it records at its own now, so an unclamped node clock minutes
// behind would expire every fresh result at once, and one ahead would delay
// rechecks or hide later results.
func clampFinished(v *inferencev1alpha1.MetalNodeVerification, finished, now time.Time) time.Time {
	if v != nil && v.LastStartTime != nil && finished.Before(v.LastStartTime.Time) {
		finished = v.LastStartTime.Time
	}
	return minTime(finished, now)
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// unread reports whether a verifier that finished at finished (clamped) has
// not been recorded yet: lastCheckTime is the finish time of the last result
// read, or when the last failure was recorded. dialBackWait starts no
// verifier in the same second as lastCheckTime, so a newer verifier's clamped
// finish always lands in a later second.
func unread(mn *inferencev1alpha1.MetalNode, finished time.Time) bool {
	v := mn.Status.Verification
	return v == nil || v.LastCheckTime == nil || finished.Truncate(time.Second).After(v.LastCheckTime.Time)
}

func verifierTerminated(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == metalNodeVerifierContainer && cs.State.Terminated != nil {
			return cs.State.Terminated
		}
	}
	return nil
}

// verification returns status.verification, creating it if needed.
func verification(mn *inferencev1alpha1.MetalNode) *inferencev1alpha1.MetalNodeVerification {
	if mn.Status.Verification == nil {
		mn.Status.Verification = &inferencev1alpha1.MetalNodeVerification{}
	}
	return mn.Status.Verification
}

func (r *MetalNodeReconciler) applyVerifierResult(mn *inferencev1alpha1.MetalNode, pod *corev1.Pod,
	term *corev1.ContainerStateTerminated, hash string, finished time.Time) {
	if term == nil || term.Message == "" {
		reason := pod.Status.Reason
		if reason == "" {
			reason = string(pod.Status.Phase)
		}
		r.recordVerifierFailure(mn, hash, finished, fmt.Sprintf("verifier pod %s ended (%s) without reporting results", pod.Name, reason))
		return
	}
	st := &mn.Status
	results, err := parseVerifierMessage(term.Message, dialableAddresses(st.Candidates))
	if err != nil {
		r.recordVerifierFailure(mn, hash, finished, fmt.Sprintf("verifier pod %s: %v", pod.Name, err))
		return
	}
	checkedAt := metav1.NewTime(finished)
	st.HostIP = ""
	counts := map[inferencev1alpha1.MetalNodeCandidateResult]int{}
	for i := range st.Candidates {
		c := &st.Candidates[i]
		res, ok := results[c.Address]
		if !ok {
			continue
		}
		c.Result, c.CheckedAt = res, &checkedAt
		counts[res]++
		if res == inferencev1alpha1.CandidateReachable && st.HostIP == "" {
			st.HostIP = c.Address
		}
	}
	v := verification(mn)
	v.InputHash, v.LastCheckTime = hash, &checkedAt
	if st.HostIP != "" {
		v.ConsecutiveFailures = 0
		r.setReachable(mn, metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonDialBackSucceeded,
			fmt.Sprintf("a verifier pod completed TLS with %s:%d against the agent's SPKI pin and its certificate for this MetalNode",
				st.HostIP, st.IngressPort))
		return
	}
	v.ConsecutiveFailures++
	r.setReachable(mn, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonNoneReachable,
		"no candidate completed TLS against the agent's SPKI pin and its certificate for this MetalNode: "+summarizeResults(counts))
}

func (r *MetalNodeReconciler) recordVerifierFailure(mn *inferencev1alpha1.MetalNode, hash string, at time.Time, msg string) {
	checkedAt := metav1.NewTime(at)
	v := verification(mn)
	v.InputHash, v.LastCheckTime = hash, &checkedAt
	v.ConsecutiveFailures++
	r.setReachable(mn, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonVerifierFailed, msg)
}

// setReachable sets the Reachable condition and keeps the invariant that
// status.hostIP is set only while Reachable is True.
func (r *MetalNodeReconciler) setReachable(mn *inferencev1alpha1.MetalNode, status metav1.ConditionStatus, reason, msg string) {
	if status != metav1.ConditionTrue {
		mn.Status.HostIP = ""
	}
	r.setCondition(mn, inferencev1alpha1.MetalNodeConditionReachable, status, reason, msg)
}

// summarizeResults renders counts as "2 Timeout, 1 Refused", in a fixed order
// so an unchanged result does not rewrite the condition message.
func summarizeResults(counts map[inferencev1alpha1.MetalNodeCandidateResult]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[inferencev1alpha1.MetalNodeCandidateResult(k)], k))
	}
	return strings.Join(parts, ", ")
}

// metalNodesForLease maps a Lease event to the MetalNodes in its namespace
// whose spec.leaseName names it.
func (r *MetalNodeReconciler) metalNodesForLease(ctx context.Context, lease *coordinationv1.Lease) []reconcile.Request {
	list := &inferencev1alpha1.MetalNodeList{}
	if err := r.List(ctx, list, client.InNamespace(lease.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "list MetalNodes for Lease", "lease", client.ObjectKeyFromObject(lease))
		return nil
	}
	var out []reconcile.Request
	for _, mn := range list.Items {
		if mn.Spec.LeaseName == lease.Name {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&mn)})
		}
	}
	return out
}

// SetupWithManager watches MetalNodes, their verifier pods, and the agents'
// Leases. Leases come from a cache of their own, scoped to Leases that carry
// the inference.llmkube.dev/metal-node label: the manager's cache scopes
// Leases to the watch canary's one Lease (cache ByObject is per type), and
// an unscoped informer would hold every node heartbeat in the cluster.
func (r *MetalNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	hasLabel, err := labels.NewRequirement(inferencev1alpha1.LabelMetalNode, selection.Exists, nil)
	if err != nil {
		return err
	}
	leaseCache, err := cache.New(mgr.GetConfig(), cache.Options{
		Scheme: mgr.GetScheme(),
		Mapper: mgr.GetRESTMapper(),
		ByObject: map[client.Object]cache.ByObject{
			&coordinationv1.Lease{}: {Label: labels.NewSelector().Add(*hasLabel)},
		},
	})
	if err != nil {
		return fmt.Errorf("build MetalNode Lease cache: %w", err)
	}
	if err := mgr.Add(leaseCache); err != nil {
		return fmt.Errorf("add MetalNode Lease cache: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		// The controller writes status on every heartbeat; reacting to its
		// own writes would double every reconcile.
		For(&inferencev1alpha1.MetalNode{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Pod{}).
		WatchesRawSource(source.Kind(leaseCache, &coordinationv1.Lease{},
			handler.TypedEnqueueRequestsFromMapFunc(r.metalNodesForLease),
			predicate.TypedFuncs[*coordinationv1.Lease]{
				UpdateFunc: func(e event.TypedUpdateEvent[*coordinationv1.Lease]) bool {
					return metalNodeLeaseUpdateMatters(e.ObjectOld, e.ObjectNew)
				},
			})).
		Named("metalnode").
		Complete(r)
}
