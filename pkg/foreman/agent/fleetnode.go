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

// Registrar owns the FleetNode CR for this host: upserts it on startup,
// patches its status every heartbeat, and patches phase=Draining on
// clean shutdown so the scheduler stops routing tasks to us promptly.
type Registrar struct {
	Client   client.Client
	NodeName string
	Spec     foremanv1alpha1.FleetNodeSpec
	Provider CapabilityProvider
	Interval time.Duration // zero defaults to DefaultHeartbeatInterval
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
func (r *Registrar) PatchHeartbeat(ctx context.Context, phase foremanv1alpha1.FleetNodePhase) error {
	var node foremanv1alpha1.FleetNode
	if err := r.Client.Get(ctx, types.NamespacedName{Name: r.NodeName}, &node); err != nil {
		return fmt.Errorf("get FleetNode for heartbeat: %w", err)
	}
	patch := client.MergeFrom(node.DeepCopy())
	now := metav1.Now()
	node.Status.Phase = phase
	node.Status.LastHeartbeatTime = &now
	node.Status.Capability = r.Provider.Capability()
	if err := r.Client.Status().Patch(ctx, &node, patch); err != nil {
		return fmt.Errorf("patch FleetNode status: %w", err)
	}
	return nil
}

// Run blocks, heartbeating every Interval until ctx is cancelled. On
// cancellation it makes a best-effort drain patch (phase=Draining) so
// the scheduler stops dispatching to us before the process exits.
func (r *Registrar) Run(ctx context.Context) error {
	log := logf.FromContext(ctx)
	if err := r.PatchHeartbeat(ctx, foremanv1alpha1.FleetNodePhaseReady); err != nil {
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
			err := r.PatchHeartbeat(drainCtx, foremanv1alpha1.FleetNodePhaseDraining)
			cancel()
			if err != nil {
				log.Error(err, "drain heartbeat failed")
			} else {
				log.Info("FleetNode Draining", "name", r.NodeName)
			}
			return nil
		case <-ticker.C:
			if err := r.PatchHeartbeat(ctx, foremanv1alpha1.FleetNodePhaseReady); err != nil {
				// Don't return on transient errors; the next tick can
				// recover. A persistent failure is visible via stale
				// LastHeartbeatTime, which is exactly the staleness
				// signal the scheduler uses anyway.
				log.Error(err, "heartbeat patch failed; will retry")
			}
		}
	}
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
