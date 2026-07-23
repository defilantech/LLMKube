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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	federationv1alpha1 "github.com/defilantech/llmkube/api/federation/v1alpha1"
)

// FederatedClusterReconciler owns ONLY status.phase, derived from the edge-written
// lastHeartbeatTime. It never writes to edge clusters.
type FederatedClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// phaseForHeartbeat derives status.phase from staleness of the last edge
// heartbeat relative to a multiple of the expected interval. A nil heartbeat
// (never reported) is Unreachable.
func phaseForHeartbeat(last *metav1.Time, intervalSeconds int32, now time.Time) string {
	if last == nil {
		return federationv1alpha1.FederatedClusterUnreachable
	}
	if intervalSeconds <= 0 {
		intervalSeconds = 30
	}
	age := now.Sub(last.Time)
	iv := time.Duration(intervalSeconds) * time.Second
	switch {
	case age <= 3*iv:
		return federationv1alpha1.FederatedClusterConnected
	case age <= 10*iv:
		return federationv1alpha1.FederatedClusterStale
	default:
		return federationv1alpha1.FederatedClusterUnreachable
	}
}

// +kubebuilder:rbac:groups=federation.llmkube.dev,resources=federatedclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=federation.llmkube.dev,resources=federatedclusters/status,verbs=get;update;patch

func (r *FederatedClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	fc := &federationv1alpha1.FederatedCluster{}
	if err := r.Get(ctx, req.NamespacedName, fc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	want := phaseForHeartbeat(fc.Status.LastHeartbeatTime, fc.Spec.HeartbeatIntervalSeconds, time.Now())
	if fc.Status.Phase != want {
		fc.Status.Phase = want
		if err := r.Status().Update(ctx, fc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Requeue so phase decays even without an edge write. Requeue at the
	// interval so a missed heartbeat is reflected within one interval.
	iv := fc.Spec.HeartbeatIntervalSeconds
	if iv <= 0 {
		iv = 30
	}
	return ctrl.Result{RequeueAfter: time.Duration(iv) * time.Second}, nil
}

func (r *FederatedClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&federationv1alpha1.FederatedCluster{}).
		Named("federatedcluster").
		Complete(r)
}
