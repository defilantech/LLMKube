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
	"encoding/base64"
	"time"

	"go.uber.org/zap"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

const (
	// metalNodeRenewInterval is how often the agent renews its Lease.
	metalNodeRenewInterval = 15 * time.Second
	// metalNodeLeaseDurationSeconds is the duration the agent writes; the
	// controller counts the agent connected for three of them.
	metalNodeLeaseDurationSeconds = int32(45)
	// metalNodeWarnInterval rate-limits the reporter's warnings, so a missing
	// Lease or RBAC does not log every 15 seconds.
	metalNodeWarnInterval = time.Minute
	// metalNodeBindRecheck is how often the agent re-reads its MetalNode's
	// UID once its ingress certificate names it, so a MetalNode deleted and
	// re-created under the same name is picked up without a restart.
	metalNodeBindRecheck = 5 * time.Minute
)

// metalNodeReporter is the agent's only presence on its MetalNode: it renews
// the Lease the admin pre-created (llmkube metal token create) and carries
// the agent's report in an annotation on it. It only ever gets and updates
// that one Lease, the two verbs the per-host Role grants on it; it never
// creates it and never writes the MetalNode. It reads the MetalNode (the
// Role's get) for its UID, which the ingress certificate must name.
type metalNodeReporter struct {
	client    client.Client
	namespace string
	node      string
	lease     string
	report    func(ctx context.Context) metalnode.Report
	logger    *zap.SugaredLogger
	now       func() time.Time
	// timeout bounds one renewal (the address lookup and the API calls);
	// zero means metalNodeRenewInterval. A hung call must not stall the
	// heartbeat for good.
	timeout time.Duration
	// bind makes the ingress certificate name the MetalNode with the given
	// UID and reports whether it re-issued it; nil without an ingress.
	bind func(uid string) (bool, error)
	// follow, when set, gets the MetalNode on every renewal (nil when it
	// does not exist) with the report about to go out, so the agent can
	// register the address the cluster verified. The MetalNode is then read
	// on every renewal; without follow (--host-ip, legacy mode) it is read
	// only for the ingress binding, until it binds and then every
	// metalNodeBindRecheck.
	follow func(mn *inferencev1alpha1.MetalNode, report metalnode.Report)

	lastWarn     time.Time
	lastBindWarn time.Time
	boundUID     types.UID
	boundAt      time.Time
}

// run renews at once and then every metalNodeRenewInterval until ctx ends.
func (r *metalNodeReporter) run(ctx context.Context) {
	ticker := time.NewTicker(metalNodeRenewInterval)
	defer ticker.Stop()
	for {
		r.renew(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// renew performs one heartbeat. Failures are logged (rate-limited) and
// retried on the next tick; the agent keeps serving either way.
func (r *metalNodeReporter) renew(ctx context.Context) {
	timeout := r.timeout
	if timeout <= 0 {
		timeout = metalNodeRenewInterval
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	key := types.NamespacedName{Namespace: r.namespace, Name: r.lease}
	lease := &coordinationv1.Lease{}
	if err := r.client.Get(ctx, key, lease); err != nil {
		if apierrors.IsNotFound(err) {
			r.warn("Lease "+key.String()+" not found; run llmkube metal token create", "metalNode", r.node)
			return
		}
		r.warn("cannot read the MetalNode Lease", "lease", key.String(), "error", err)
		return
	}

	mn, read, readErr := r.readMetalNode(ctx)
	if read {
		r.bindIngress(mn, readErr)
	}
	report := r.report(ctx)
	raw, err := metalnode.Encode(report)
	if err == nil {
		// The controller rejects a report it cannot decode; catching that
		// here names the cause in the agent's own log.
		_, err = metalnode.Decode(raw)
	}
	if err != nil {
		r.warn("not renewing the MetalNode Lease: the report is invalid", "lease", key.String(), "error", err)
		return
	}
	// A failed read says nothing about the MetalNode, so the registered
	// address stays as it is; a MetalNode that is gone has nothing verified.
	if r.follow != nil && read && (readErr == nil || apierrors.IsNotFound(readErr)) {
		r.follow(mn, report)
	}

	holder := r.node
	duration := metalNodeLeaseDurationSeconds
	renewed := metav1.NewMicroTime(r.now())
	lease.Spec.HolderIdentity = &holder
	lease.Spec.LeaseDurationSeconds = &duration
	lease.Spec.RenewTime = &renewed
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	lease.Annotations[metalnode.ReportAnnotation] = raw
	if err := r.client.Update(ctx, lease); err != nil {
		if apierrors.IsConflict(err) {
			r.logger.Debugw("MetalNode Lease changed underneath; renewing on the next tick", "lease", key.String())
			return
		}
		r.warn("cannot renew the MetalNode Lease", "lease", key.String(), "error", err)
		return
	}
	r.lastWarn = time.Time{}
}

// bindDue reports whether the ingress certificate's binding is due a check:
// on every renewal until the certificate names the MetalNode, then every
// metalNodeBindRecheck.
func (r *metalNodeReporter) bindDue() bool {
	return r.bind != nil && (r.boundUID == "" || r.now().Sub(r.boundAt) >= metalNodeBindRecheck)
}

// readMetalNode gets the MetalNode when this renewal has a use for it: the
// ingress binding is due, or follow is set (every renewal). read is false
// when no read was due. A failure is logged (rate-limited) and returned.
func (r *metalNodeReporter) readMetalNode(ctx context.Context) (mn *inferencev1alpha1.MetalNode, read bool, err error) {
	bindDue := r.bindDue()
	if !bindDue && r.follow == nil {
		return nil, false, nil
	}
	mn = &inferencev1alpha1.MetalNode{}
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: r.node}, mn); err != nil {
		switch {
		case bindDue:
			r.warnBind("cannot read the MetalNode to bind the ingress certificate to it; the dial-back fails until it can",
				"metalNode", r.node, "error", err)
		case !apierrors.IsNotFound(err):
			// A MetalNode that is gone is follow's to log, as the switch
			// away from its address.
			r.warnBind("cannot read the MetalNode; the agent's endpoints keep their address until it can",
				"metalNode", r.node, "error", err)
		}
		return nil, true, err
	}
	return mn, true, nil
}

// bindIngress makes the ingress certificate name this MetalNode's UID before
// the report carries it: the controller's dial-back accepts only a server
// whose certificate names it, so no other server can pass for this host. It
// binds when bindDue, or when a read made for follow shows a UID other than
// the bound one (a MetalNode re-created under the same name). A failure is
// logged and the report goes out anyway; the controller then says the
// certificate does not name the MetalNode.
func (r *metalNodeReporter) bindIngress(mn *inferencev1alpha1.MetalNode, readErr error) {
	if r.bind == nil || (!r.bindDue() && (readErr != nil || mn.UID == r.boundUID)) {
		return
	}
	if readErr != nil {
		// readMetalNode logged it.
		return
	}
	reissued, err := r.bind(string(mn.UID))
	if err != nil {
		r.warnBind("cannot bind the ingress certificate to the MetalNode; the dial-back fails until it can",
			"metalNode", r.node, "error", err)
		return
	}
	if reissued {
		r.logger.Infow("re-issued the ingress certificate (same key and pin) to name this MetalNode",
			"metalNode", r.node, "uid", mn.UID, "hostname", metalnode.IngressHostname(string(mn.UID)))
	}
	r.boundUID, r.boundAt, r.lastBindWarn = mn.UID, r.now(), time.Time{}
}

func (r *metalNodeReporter) warnBind(msg string, kv ...any) {
	now := r.now()
	if !r.lastBindWarn.IsZero() && now.Sub(r.lastBindWarn) < metalNodeWarnInterval {
		return
	}
	r.lastBindWarn = now
	r.logger.Warnw(msg, kv...)
}

func (r *metalNodeReporter) warn(msg string, kv ...any) {
	now := r.now()
	if !r.lastWarn.IsZero() && now.Sub(r.lastWarn) < metalNodeWarnInterval {
		return
	}
	r.lastWarn = now
	r.logger.Warnw(msg, kv...)
}

// startMetalNodeReporter starts the Lease reporter when --metal-node is set
// and reports whether it did. With no MetalNode configured the agent makes
// no Lease call at all, exactly as before the feature existed.
func (a *MetalAgent) startMetalNodeReporter(ctx context.Context) bool {
	r := a.newMetalNodeReporter()
	if r == nil {
		return false
	}
	a.logger.Infow("reporting to MetalNode through its Lease",
		"metalNode", a.config.MetalNode, "lease", a.config.Namespace+"/"+r.lease, "interval", metalNodeRenewInterval,
		"followVerifiedHostIP", r.follow != nil)
	go r.run(ctx)
	return true
}

// newMetalNodeReporter is the reporter startMetalNodeReporter runs; nil
// without --metal-node.
func (a *MetalAgent) newMetalNodeReporter() *metalNodeReporter {
	if a.config.MetalNode == "" {
		return nil
	}
	lease := a.config.MetalNodeLease
	if lease == "" {
		lease = a.config.MetalNode
	}
	r := &metalNodeReporter{
		client:    a.config.K8sClient,
		namespace: a.config.Namespace,
		node:      a.config.MetalNode,
		lease:     lease,
		report:    a.metalNodeReport,
		logger:    a.logger.With("subsystem", "metal-node"),
		now:       time.Now,
	}
	if a.ingressIdentity != nil {
		r.bind = a.ingressIdentity.BindMetalNode
	}
	if a.followsMetalNodeHostIP() {
		r.follow = a.followMetalNodeHostIP
	}
	return r
}

// metalNodeReport is what the agent knows about itself right now. The
// candidate addresses are recomputed every time, so a network change shows
// up within one renewal.
func (a *MetalAgent) metalNodeReport(ctx context.Context) metalnode.Report {
	capacity := a.config.HostCapacity
	report := metalnode.Report{
		Version:      a.config.Version,
		Chip:         capacity.Chip,
		MemoryBytes:  capacity.MemoryBytes,
		GPUCores:     capacity.GPUCores,
		MetalVersion: capacity.MetalVersion,
	}
	if p := a.config.IngressPort; a.ingressPin != "" && a.ingressListening.Load() && p > 0 && p <= 65535 {
		report.IngressSPKI = a.ingressPin
		report.IngressPort = int32(p)
		if a.ingressIdentity != nil {
			report.IngressCert = base64.StdEncoding.EncodeToString(a.ingressIdentity.Certificate().Certificate[0])
		}
	}
	env := newCandidateGatherer().gather(ctx, a.config.HostIP, a.config.APIServerHost)
	report.Candidates = buildMetalNodeCandidates(env)
	return report
}
