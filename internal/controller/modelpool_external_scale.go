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
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// externalScaleQuietPeriod is how long ExternallyScaled stays True after the
// last outside scale-up. It is longer than common GitOps sync intervals (Flux
// and Argo CD default to minutes), so a tool restoring replicas on every sync
// keeps the condition True and raises one event per quiet period, not one per
// sync.
const externalScaleQuietPeriod = 30 * time.Minute

const (
	// eventReasonExternalScale is the Warning event raised on the pool.
	eventReasonExternalScale = "ExternalScale"
	// reasonExternalFieldManager is the ExternallyScaled=True reason.
	reasonExternalFieldManager = "ExternalFieldManager"
	// reasonExternalScaleQuiet is the ExternallyScaled=False reason.
	reasonExternalScaleQuiet = "QuietPeriodElapsed"
)

// llmkubeReplicaManagers are the field managers whose scale-ups are LLMKube's
// own. Before #2302 both writers patched without a field owner, so the API
// server named them after the binary in their user agent: the operator image
// runs /manager and the router-proxy image runs router-proxy. A member whose
// replicas one of those builds wrote keeps that name until the next write, so
// it is recognized too; otherwise an upgrade landing mid-swap would report the
// old operator as an outside tool.
var llmkubeReplicaManagers = map[string]bool{
	inferencev1alpha1.ModelPoolFieldManager:   true,
	inferencev1alpha1.RouterProxyFieldManager: true,
	"manager":      true,
	"router-proxy": true,
}

// externalScaleEvent is a scale-up to announce with a Warning event once the
// pass's status write has landed.
type externalScaleEvent struct {
	member *inferencev1alpha1.InferenceService
	note   string
}

// observeExternalScale reports a non-resident member scaled up by a field
// manager outside LLMKube (#2302). The pool treats any such scale-up as a
// request for the slot, which is what makes `kubectl scale` an activation and
// what makes a GitOps tool restoring spec.replicas swap the slot on every sync.
// Inside the cluster both look like a router activation, so the only witness
// is the member's managedFields. This never blocks the activation: it only
// records the scale-up in status (ExternalScale and the ExternallyScaled
// condition) and returns the event to raise, nil when the same write was
// already reported or the manager was already named inside the quiet period.
// The duration is when to come back to clear the condition, zero when there is
// nothing to clear.
//
// It must run after the status baseline is captured, so updateStatus persists
// what it records.
func observeExternalScale(
	pool *inferencev1alpha1.ModelPool,
	members map[string]*inferencev1alpha1.InferenceService,
	now time.Time,
) (*externalScaleEvent, time.Duration) {
	prev := pool.Status.ExternalScale
	active := apimeta.IsStatusConditionTrue(pool.Status.Conditions, inferencev1alpha1.ConditionModelPoolExternallyScaled)

	if seen, isvc := externallyScaledMember(pool, members); seen != nil && !sameExternalScale(prev, seen) {
		seen.ObservedAt = metav1.NewTime(now)
		pool.Status.ExternalScale = seen
		msg := externalScaleMessage(seen, replicasOf(isvc))
		apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               inferencev1alpha1.ConditionModelPoolExternallyScaled,
			Status:             metav1.ConditionTrue,
			Reason:             reasonExternalFieldManager,
			Message:            msg,
			ObservedGeneration: pool.Generation,
		})
		// One event per manager per quiet period: a GitOps tool alternating
		// between two members names a different member each sync.
		if active && prev != nil && prev.Manager == seen.Manager {
			return nil, externalScaleQuietPeriod
		}
		return &externalScaleEvent{member: isvc, note: msg}, externalScaleQuietPeriod
	}

	if !active {
		return nil, 0
	}
	if prev != nil {
		if remaining := externalScaleQuietPeriod - now.Sub(prev.ObservedAt.Time); remaining > 0 {
			return nil, remaining
		}
	}
	apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:   inferencev1alpha1.ConditionModelPoolExternallyScaled,
		Status: metav1.ConditionFalse,
		Reason: reasonExternalScaleQuiet,
		Message: fmt.Sprintf("No member scaled up outside LLMKube in the last %s",
			externalScaleQuietPeriod),
		ObservedGeneration: pool.Generation,
	})
	return nil, 0
}

// externallyScaledMember returns the first member, in declaration order, that
// asks for the slot and whose spec.replicas is owned by a field manager outside
// LLMKube. "Asks for the slot" is resolveSlotOwner's fresh activation: scaled
// up and not the resident, or the resident scaled back up after the pool
// drained it. A cold pool is skipped: members created at replicas 1 before any
// member became resident are how a pool is set up, not a tool fighting it.
func externallyScaledMember(
	pool *inferencev1alpha1.ModelPool,
	members map[string]*inferencev1alpha1.InferenceService,
) (*inferencev1alpha1.ModelPoolExternalScale, *inferencev1alpha1.InferenceService) {
	resident := pool.Status.ResidentMember
	if resident == "" {
		return nil, nil
	}
	for _, name := range memberNames(pool) {
		isvc, ok := members[name]
		if !ok || replicasOf(isvc) < 1 || (name == resident && !drainedByPool(isvc)) {
			continue
		}
		if manager := externalReplicasManager(isvc); manager != "" {
			return &inferencev1alpha1.ModelPoolExternalScale{
				Member:           name,
				Manager:          manager,
				MemberGeneration: isvc.Generation,
			}, isvc
		}
	}
	return nil, nil
}

// externalReplicasManager returns the field manager outside LLMKube that owns
// the member's spec.replicas, or "" when only LLMKube's managers own it or no
// manager does (a defaulted value). An Update (merge patch, scale subresource)
// that changes a field takes it from every other manager, so the owners are
// the writers of the current value. Several managers own it only when an Apply
// set the value another manager already held; that tool still manages the
// field, so it is the one named. A scale subresource entry carries no time, so
// a timed entry wins a tie between outside managers.
func externalReplicasManager(isvc *inferencev1alpha1.InferenceService) string {
	var manager string
	var latest *metav1.Time
	for _, entry := range isvc.ManagedFields {
		if llmkubeReplicaManagers[entry.Manager] || !ownsSpecReplicas(entry) {
			continue
		}
		if manager == "" || (entry.Time != nil && (latest == nil || latest.Before(entry.Time))) {
			manager = entry.Manager
			latest = entry.Time
		}
	}
	return manager
}

// ownsSpecReplicas reports whether a managedFields entry owns spec.replicas.
// The entry's FieldsV1 is the API server's own encoding, so an entry that does
// not decode owns nothing this check can name.
func ownsSpecReplicas(entry metav1.ManagedFieldsEntry) bool {
	if entry.FieldsV1 == nil {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(entry.FieldsV1.GetRawBytes(), &fields); err != nil {
		return false
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(fields["f:spec"], &spec); err != nil {
		return false
	}
	_, ok := spec["f:replicas"]
	return ok
}

// sameExternalScale reports whether seen is the write prev already recorded.
func sameExternalScale(prev, seen *inferencev1alpha1.ModelPoolExternalScale) bool {
	return prev != nil && prev.Member == seen.Member && prev.Manager == seen.Manager &&
		prev.MemberGeneration == seen.MemberGeneration
}

func externalScaleMessage(seen *inferencev1alpha1.ModelPoolExternalScale, replicas int32) string {
	return fmt.Sprintf("Member %q was scaled to %d by field manager %q, not by LLMKube. The pool treats "+
		"this as a request for the slot; a GitOps tool that manages member replicas swaps the slot on "+
		"every sync (see docs/operations/gpu-sharing.md, GitOps)", seen.Member, replicas, seen.Manager)
}

// soonerRequeue returns the earlier of two requeue delays, where zero means
// none.
func soonerRequeue(current, next time.Duration) time.Duration {
	if next > 0 && (current == 0 || next < current) {
		return next
	}
	return current
}

// reportExternalScale raises the Warning event for a scale-up
// observeExternalScale recorded. It runs after the status write, so a pass
// whose write fails is retried with the scale-up still unrecorded and the event
// is raised once, by the pass that records it.
func (r *ModelPoolReconciler) reportExternalScale(pool *inferencev1alpha1.ModelPool, ev *externalScaleEvent) {
	if ev == nil || r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(pool, ev.member, corev1.EventTypeWarning, eventReasonExternalScale, "Reconcile", "%s", ev.note)
}
