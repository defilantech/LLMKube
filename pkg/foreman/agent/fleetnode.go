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

// Package agent is the Foreman worker library: it lets a node register
// itself as a FleetNode, heartbeat its capability, and (in later
// milestones) watch + execute AgenticTasks dispatched to it. The
// cmd/foreman-agent binary is a thin wrapper around this package.
package agent

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// DefaultHeartbeatInterval is the v0.1 cadence. The scheduler treats a
// FleetNode as stale (NotReady) when LastHeartbeatTime is older than
// roughly 3x this interval, so 30s gives a ~90s detection window.
const DefaultHeartbeatInterval = 30 * time.Second

// CapabilityProvider returns the current capability profile of this node.
// Implementations must be cheap (called on every heartbeat) and must not
// panic; on transient failure, return a zero or last-known value.
type CapabilityProvider interface {
	Capability() foremanv1alpha1.FleetNodeCapability
}

// UpdateApplier applies a binary update given a (version, url, sha256) triple.
// The canonical implementation wraps *selfupdate.Updater.MaybeApply; tests
// may inject a fake via UpdateApplierFunc.
//
// Returns (restarting, err). restarting=true means the symlink was flipped
// and the caller should drain then exit so the supervisor can relaunch.
type UpdateApplier interface {
	Apply(version, url, sha256 string) (restarting bool, err error)
}

// UpdateApplierFunc is a convenience adapter so a plain function satisfies
// UpdateApplier without a named struct.
type UpdateApplierFunc func(version, url, sha256 string) (bool, error)

// Apply implements UpdateApplier.
func (f UpdateApplierFunc) Apply(version, url, sha256 string) (bool, error) {
	return f(version, url, sha256)
}

// Registrar owns the FleetNode CR for this host: upserts it on startup,
// patches its status every heartbeat, and patches phase=Draining on
// clean shutdown so the scheduler stops routing tasks to us promptly.
type Registrar struct {
	Client   client.Client
	NodeName string
	Spec     foremanv1alpha1.FleetNodeSpec
	Provider CapabilityProvider
	Interval time.Duration // zero defaults to DefaultHeartbeatInterval

	// Version is the agent binary's version string (e.g. "v0.8.4").
	// Set from the build-time ldflags var and stamped on FleetNode.status
	// every heartbeat so the cluster can observe which version is running.
	// Empty string is accepted (older agents omit this field).
	Version string

	// Kind identifies the agent type. Conventionally "foreman-agent".
	// Stamped on FleetNode.status.agentKind every heartbeat.
	Kind string

	// OS is the operating system this agent is running on (runtime.GOOS).
	// Stamped on FleetNode.status.os every heartbeat so the
	// AgentReleaseReconciler can select the correct platform artifact.
	OS string

	// Arch is the CPU architecture this agent is running on (runtime.GOARCH).
	// Stamped on FleetNode.status.arch every heartbeat.
	Arch string

	// Updater handles self-update when an UpdateRequest appears on the
	// FleetNode status. When nil, update requests are logged and ignored
	// (the pre-PR4 behaviour). The caller is responsible for gating the
	// Updater on RunningUnderManagedRoot before setting it; if the binary
	// is not running from the managed install root the caller should leave
	// Updater nil so dev/test builds are unaffected.
	Updater UpdateApplier
}

// Upsert creates the FleetNode if missing, otherwise updates its Spec so
// flag changes between agent restarts take effect immediately.
func (r *Registrar) Upsert(ctx context.Context) error {
	log := logf.FromContext(ctx)
	key := types.NamespacedName{Name: r.NodeName}

	var existing foremanv1alpha1.FleetNode
	err := r.Client.Get(ctx, key, &existing)
	switch {
	case apierrors.IsNotFound(err):
		node := &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: r.NodeName},
			Spec:       r.Spec,
		}
		if err := r.Client.Create(ctx, node); err != nil {
			return fmt.Errorf("create FleetNode %q: %w", r.NodeName, err)
		}
		log.Info("created FleetNode", "name", r.NodeName)
		return nil
	case err != nil:
		return fmt.Errorf("get FleetNode %q: %w", r.NodeName, err)
	}

	// Update spec only if it actually changed; avoids touch noise on
	// every restart with identical flags.
	if specEqual(existing.Spec, r.Spec) {
		log.Info("FleetNode spec unchanged", "name", r.NodeName)
		return nil
	}
	existing.Spec = r.Spec
	if err := r.Client.Update(ctx, &existing); err != nil {
		return fmt.Errorf("update FleetNode %q spec: %w", r.NodeName, err)
	}
	log.Info("updated FleetNode spec", "name", r.NodeName)
	return nil
}

// PatchHeartbeat patches the FleetNode's status with a fresh heartbeat
// time, the current phase, and the latest capability snapshot. Uses a
// merge patch so concurrent edits to other status fields by future
// reconcilers (M2+) do not conflict.
//
// Multiple writers own different fields on FleetNode.status:
//   - Agent: phase, lastHeartbeatTime, capability, agentVersion, agentKind, os, arch.
//   - FleetNodeReconciler: staleness phase + Ready condition.
//   - AgentReleaseReconciler: updateRequest.
//
// The field-scoped MergeFrom patch ensures each writer only touches its
// own fields without clobbering concurrent writes from others.
//
// The returned *FleetNodeUpdateRequest is the value observed on the node
// at Get time (before the status patch). Run uses this to trigger
// self-update after a successful Ready heartbeat, ensuring the operator
// always sees the node as Ready before the flip rather than getting a
// stale version in a post-flip Ready heartbeat.
func (r *Registrar) PatchHeartbeat(ctx context.Context, phase foremanv1alpha1.FleetNodePhase) (*foremanv1alpha1.FleetNodeUpdateRequest, error) { //nolint:lll
	log := logf.FromContext(ctx)
	var node foremanv1alpha1.FleetNode
	if err := r.Client.Get(ctx, types.NamespacedName{Name: r.NodeName}, &node); err != nil {
		return nil, fmt.Errorf("get FleetNode for heartbeat: %w", err)
	}

	// Capture the update request BEFORE patching so we return it to the
	// caller regardless of whether the patch succeeds. The operator owns
	// this field; the agent never touches it.
	updateReq := node.Status.UpdateRequest
	if updateReq != nil {
		log.Info("update requested",
			"target", updateReq.TargetVersion,
			"url", updateReq.URL,
		)
	}

	patch := client.MergeFrom(node.DeepCopy())
	now := metav1.Now()
	node.Status.Phase = phase
	node.Status.LastHeartbeatTime = &now
	node.Status.Capability = r.Provider.Capability()
	if r.Version != "" {
		node.Status.AgentVersion = r.Version
	}
	if r.Kind != "" {
		node.Status.AgentKind = foremanv1alpha1.FleetNodeAgentKind(r.Kind)
	}
	if r.OS != "" {
		node.Status.OS = r.OS
	}
	if r.Arch != "" {
		node.Status.Arch = r.Arch
	}
	if err := r.Client.Status().Patch(ctx, &node, patch); err != nil {
		return nil, fmt.Errorf("patch FleetNode status: %w", err)
	}
	return updateReq, nil
}

// ErrSelfUpdateRestart is returned from Run when a self-update was applied
// and the process should exit so the supervisor can relaunch onto the new
// binary. The errgroup in main.go propagates this return value, which
// triggers cancellation of the sibling watcher goroutine (clean shutdown).
var ErrSelfUpdateRestart = fmt.Errorf("self-update applied: exiting for supervisor restart")

// Run blocks, heartbeating every Interval until ctx is cancelled. On
// cancellation it makes a best-effort drain patch (phase=Draining) so
// the scheduler stops dispatching to us before the process exits.
//
// Self-update flow (when r.Updater is set and the binary runs from the
// managed install root):
//  1. After each successful Ready heartbeat, check the observed UpdateRequest.
//  2. If a new version is available, call r.Updater.Apply. On success:
//     a. Emit a final Draining heartbeat (stops scheduler routing).
//     b. Return ErrSelfUpdateRestart — the errgroup cancels the watcher.
//  3. The process exits; launchd/systemd restarts it onto the new symlink.
//
// CRITICAL: between the symlink flip and the final return, no further
// Ready heartbeat is sent. This ensures the rollout health gate never sees
// the old version in a Ready state after the flip.
func (r *Registrar) Run(ctx context.Context) error {
	log := logf.FromContext(ctx)
	if _, err := r.PatchHeartbeat(ctx, foremanv1alpha1.FleetNodePhaseReady); err != nil {
		return fmt.Errorf("initial heartbeat: %w", err)
	}
	log.Info("FleetNode Ready", "name", r.NodeName)

	interval := r.Interval
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Best-effort drain. A short fresh timeout keeps us from
			// hanging on a dead apiserver during shutdown.
			drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := r.PatchHeartbeat(drainCtx, foremanv1alpha1.FleetNodePhaseDraining)
			cancel()
			if err != nil {
				log.Error(err, "drain heartbeat failed")
			} else {
				log.Info("FleetNode Draining", "name", r.NodeName)
			}
			return nil
		case <-ticker.C:
			updateReq, err := r.PatchHeartbeat(ctx, foremanv1alpha1.FleetNodePhaseReady)
			if err != nil {
				// Don't return on transient errors; the next tick can
				// recover. A persistent failure is visible via stale
				// LastHeartbeatTime, which is exactly the staleness
				// signal the scheduler uses anyway.
				log.Error(err, "heartbeat patch failed; will retry")
				continue
			}

			// Check for a pending self-update after the heartbeat so the
			// operator always observes at least one Ready beat with the
			// current version before we flip.
			if restarting := r.maybeApplySelfUpdate(ctx, updateReq); restarting {
				// Symlink flipped. Drain before exiting so the scheduler
				// stops routing tasks to us. A short timeout prevents
				// blocking indefinitely on a slow apiserver.
				drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, derr := r.PatchHeartbeat(drainCtx, foremanv1alpha1.FleetNodePhaseDraining)
				cancel()
				if derr != nil {
					log.Error(derr, "self-update drain heartbeat failed; exiting anyway")
				} else {
					log.Info("FleetNode Draining for self-update restart", "name", r.NodeName)
				}
				return ErrSelfUpdateRestart
			}
		}
	}
}

// maybeApplySelfUpdate checks the observed UpdateRequest and calls the
// Updater if appropriate. Returns true when the binary was flipped and
// the process should restart.
//
// Self-update is skipped when:
//   - r.Updater is nil (PR3 / pre-PR4 deployments)
//   - req is nil (no update request from the operator)
//   - the binary is not running from the managed install root (dev builds)
func (r *Registrar) maybeApplySelfUpdate(ctx context.Context, req *foremanv1alpha1.FleetNodeUpdateRequest) bool {
	if r.Updater == nil || req == nil {
		return false
	}
	log := logf.FromContext(ctx)

	restarting, err := r.Updater.Apply(req.TargetVersion, req.URL, req.SHA256)
	if err != nil {
		log.Error(err, "self-update failed; will retry on next heartbeat",
			"target", req.TargetVersion)
		return false
	}
	return restarting
}

func specEqual(a, b foremanv1alpha1.FleetNodeSpec) bool {
	if a.NodeName != b.NodeName || a.TailscaleAddr != b.TailscaleAddr {
		return false
	}
	if len(a.Roles) != len(b.Roles) {
		return false
	}
	for i := range a.Roles {
		if a.Roles[i] != b.Roles[i] {
			return false
		}
	}
	return true
}
