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
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	federationv1alpha1 "github.com/defilantech/llmkube/api/federation/v1alpha1"
	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// defaultFederationEdgeInterval is the push cadence used when
// FederationEdgeReconciler.Interval is left at its zero value.
const defaultFederationEdgeInterval = 30 * time.Second

// Federation role values for cmd/main.go's --federation-role flag. Hub (the
// default) runs the datacenter-side FederatedClusterReconciler, which owns
// status.phase for every registered edge. Edge runs FederationEdgeReconciler
// instead: it never runs both roles in the same process, since only one
// side of a given FederatedCluster object should ever be reconciled by a
// given operator instance.
const (
	FederationRoleHub  = "hub"
	FederationRoleEdge = "edge"
)

// gpuResourceNames are the extended resources summed across node
// Capacity/Allocatable for FederatedCluster GPU accounting. Reuses the
// vendor resource-name variables declared in gpu_resources.go (used there
// for per-InferenceService scheduling) rather than redeclaring them, so a
// new vendor added there is picked up here for free.
var gpuResourceNames = []corev1.ResourceName{
	nvidiaGPUResourceName,
	amdGPUResourceName,
	intelGPUResourceNameI915,
	intelGPUResourceNameXE,
	vulkanDRIResourceName,
}

// FederationEdgeReconciler is a manager.Runnable (not a reconcile.Reconciler:
// there is no local CR driving this side of federation) that periodically
// observes local inference state and pushes it to the datacenter's
// FederatedCluster status subresource. It is spoke-initiated and outbound
// only:
//
//   - LocalClient reads Models, InferenceServices, and Nodes on THIS
//     (edge) cluster.
//   - DatacenterClient writes ONLY the status subresource of the
//     ClusterName FederatedCluster object on the datacenter cluster, using
//     a separate, RBAC-scoped client built from
//     --federation-datacenter-kubeconfig (a scoped token, distinct from
//     the local manager client). Nothing else is ever written on the
//     datacenter.
//
// Two-writer discipline: buildStatusSummary never populates Phase, and
// tick's status patch leaves fc.Status.Phase exactly as read from the
// datacenter, so the computed merge patch carries no "phase" key at all.
// Phase stays the datacenter FederatedClusterReconciler's exclusively (see
// federatedcluster_controller.go).
type FederationEdgeReconciler struct {
	// LocalClient reads Models/InferenceServices/Nodes on the edge cluster.
	LocalClient client.Client
	// DatacenterClient writes status on the datacenter cluster's
	// FederatedCluster/ClusterName object. Built from a separate,
	// RBAC-scoped kubeconfig (--federation-datacenter-kubeconfig).
	DatacenterClient client.Client
	// ClusterName is this edge's FederatedCluster object name on the
	// datacenter (--federation-cluster-name).
	ClusterName string
	// Version is recorded as status.observedVersion on every push.
	Version string
	// Interval is the push cadence; defaultFederationEdgeInterval when unset.
	Interval time.Duration
	// Log is used for the log-and-retry-next-tick failure path; discarded
	// when unset.
	Log logr.Logger
}

// NeedLeaderElection makes the edge push leader-election-aware: when the
// operator runs with --leader-elect=true, only the elected replica pushes,
// avoiding redundant writes to the datacenter from every replica. A
// standalone deployment (or leader election disabled) pushes regardless,
// same as pkg/foreman/audit.Reaper.
func (r *FederationEdgeReconciler) NeedLeaderElection() bool { return true }

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=models,verbs=get;list;watch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=inferenceservices,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Start runs the periodic observe-and-push cycle until ctx is cancelled,
// then returns nil. It ticks once immediately (so a freshly started edge
// reports in without waiting a full interval) and then on Interval. Every
// failure mode inside tick is logged and swallowed, never propagated as a
// returned error, so a single bad tick (local list error, datacenter
// unreachable, FederatedCluster not yet registered) never crashes the
// operator; the next tick simply tries again.
func (r *FederationEdgeReconciler) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = defaultFederationEdgeInterval
	}
	log := r.Log
	if log.IsZero() {
		log = logr.Discard()
	}

	r.tick(ctx, log)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.tick(ctx, log)
		}
	}
}

// tick performs one observe-and-push cycle: list local state, build the
// summary, then patch it onto the datacenter FederatedCluster status. Every
// error is logged and swallowed here so the caller (Start's loop) never
// sees an error to propagate.
func (r *FederationEdgeReconciler) tick(ctx context.Context, log logr.Logger) {
	var models inferencev1alpha1.ModelList
	if err := r.LocalClient.List(ctx, &models); err != nil {
		log.Error(err, "federation edge: list local Models, retrying next tick")
		return
	}
	var isvcs inferencev1alpha1.InferenceServiceList
	if err := r.LocalClient.List(ctx, &isvcs); err != nil {
		log.Error(err, "federation edge: list local InferenceServices, retrying next tick")
		return
	}
	var nodes corev1.NodeList
	if err := r.LocalClient.List(ctx, &nodes); err != nil {
		log.Error(err, "federation edge: list local Nodes, retrying next tick")
		return
	}

	summary := buildStatusSummary(models.Items, isvcs.Items, nodes.Items, r.Version)

	fc := &federationv1alpha1.FederatedCluster{}
	if err := r.DatacenterClient.Get(ctx, types.NamespacedName{Name: r.ClusterName}, fc); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("federation edge: FederatedCluster not registered on datacenter yet, retrying next tick",
				"clusterName", r.ClusterName)
			return
		}
		log.Error(err, "federation edge: get FederatedCluster on datacenter (unreachable?), retrying next tick",
			"clusterName", r.ClusterName)
		return
	}

	// Two-writer discipline: patch is diffed against this DeepCopy. Every
	// edge-owned field below is assigned; fc.Status.Phase is left exactly
	// as read, so it never appears in the computed merge patch and the
	// datacenter's phase can never be clobbered by this push.
	patch := client.MergeFrom(fc.DeepCopy())
	now := metav1.Now()
	fc.Status.LastHeartbeatTime = &now
	fc.Status.ObservedVersion = summary.ObservedVersion
	fc.Status.Capacity = summary.Capacity
	fc.Status.Inference = summary.Inference

	if err := r.DatacenterClient.Status().Patch(ctx, fc, patch); err != nil {
		log.Error(err, "federation edge: patch FederatedCluster status on datacenter (unreachable?), retrying next tick",
			"clusterName", r.ClusterName)
		return
	}

	log.V(1).Info("federation edge: pushed status to datacenter",
		"clusterName", r.ClusterName,
		"servicesReady", summary.Inference.ServicesReady,
		"servicesTotal", summary.Inference.ServicesTotal,
		"nodes", summary.Capacity.Nodes)
}

// buildStatusSummary is a pure function deriving the edge-owned
// FederatedClusterStatus fields (ObservedVersion, Capacity, Inference) from
// locally observed state. Phase and LastHeartbeatTime are intentionally left
// zero: Phase is datacenter-owned (federatedcluster_controller.go) and
// LastHeartbeatTime is stamped by the caller (tick) at push time, so this
// function stays deterministic and easy to table-test.
func buildStatusSummary(
	models []inferencev1alpha1.Model,
	isvcs []inferencev1alpha1.InferenceService,
	nodes []corev1.Node,
	version string,
) federationv1alpha1.FederatedClusterStatus {
	inference := federationv1alpha1.ClusterInferenceSummary{
		Models:        int32(len(models)), //nolint:gosec // G115: fleet size is far below int32 range
		ServicesTotal: int32(len(isvcs)),  //nolint:gosec // G115: fleet size is far below int32 range
	}
	for i := range isvcs {
		switch isvcs[i].Status.Phase {
		case PhaseReady:
			inference.ServicesReady++
		case PhaseFailed:
			inference.ServicesFailed++
		}
	}

	capacity := federationv1alpha1.ClusterCapacity{
		Nodes: int32(len(nodes)), //nolint:gosec // G115: fleet size is far below int32 range
	}
	for i := range nodes {
		capacity.GPUsTotal += sumGPUQuantity(nodes[i].Status.Capacity)
		capacity.GPUsAllocatable += sumGPUQuantity(nodes[i].Status.Allocatable)
	}

	return federationv1alpha1.FederatedClusterStatus{
		ObservedVersion: version,
		Capacity:        &capacity,
		Inference:       &inference,
	}
}

// sumGPUQuantity sums every known GPU extended resource (gpuResourceNames)
// present in a node's ResourceList (either Status.Capacity or
// Status.Allocatable). GPU counts are always whole units, so Value() (not
// MilliValue) is the correct accessor.
func sumGPUQuantity(rl corev1.ResourceList) int32 {
	var total int64
	for _, name := range gpuResourceNames {
		if q, ok := rl[name]; ok {
			total += q.Value()
		}
	}
	return int32(total)
}
