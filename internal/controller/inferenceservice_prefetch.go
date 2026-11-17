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
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// InferenceService side of prefetch. A service with spec.modelCache.prefetch
// asks for its Model to be staged into the cache claim its pods mount before
// any of them runs. This side never stages: model_prefetch.go owns the Jobs
// and is the only writer of Model.status.prefetch. The methods here read the
// entry for the service's claim, hold the workload until it is current,
// render pods that only consume the cache (buildPrefetchedStorageConfig in
// model_storage.go), and ask for a re-stage by deleting the Model's Job. The
// decisions both sides must agree on are in prefetch.go.

const (
	// ConditionModelPrefetched reports whether the weights this service's
	// pods mount were staged by a prefetch for the Model's current spec.
	// Written only when spec.modelCache.prefetch is set.
	ConditionModelPrefetched = "ModelPrefetched"

	// ReasonWaitingForPrefetch: the prefetch into this service's claim has not
	// succeeded yet; the workload is held.
	ReasonWaitingForPrefetch = "WaitingForPrefetch"
	// ReasonWaitingForModel: the Model has not derived what the prefetch
	// needs (its cache key) yet; the workload is held as Pending, not failed.
	ReasonWaitingForModel = "WaitingForModel"
	// ReasonPrefetchNotApplicable: the service asked, but nothing downloads
	// at serve time (Metal, a pre-staged source); the workload proceeds as if
	// the field were unset. Caching disabled on the operator is
	// PrefetchUnsupported: the pods would download into an emptyDir.
	ReasonPrefetchNotApplicable = "PrefetchNotApplicable"
	// ReasonPrefetchTargetMismatch is the event a service gets when its Model
	// was prefetched, but not into the claim this service mounts.
	ReasonPrefetchTargetMismatch = "PrefetchTargetMismatch"
	// ReasonPrefetchRestaged is the event a service records when it deletes
	// the Model's prefetch Job so the claim its pods found empty is filled
	// again.
	ReasonPrefetchRestaged = "PrefetchRestaged"
	// ReasonPrefetchUnsupported: the service asked, its pods would download
	// at serve time, and prefetch cannot stand in front of that download
	// (caching disabled, a runtime the operator does not stage, a draft
	// model, a source the downloader does not stage). The workload is held
	// rather than rolled out with a downloader: prefetch: true means "serve
	// only from a prefetched cache".
	ReasonPrefetchUnsupported = "PrefetchUnsupported"
)

// prefetchGate is what a service's modelCache.prefetch resolves to on this
// reconcile.
type prefetchGate struct {
	// requested: spec.modelCache.prefetch is set.
	requested bool
	// applies: and there is a cache claim to stage into.
	applies bool
	claim   string
	// satisfied: the Model's entry for claim is Succeeded and current.
	satisfied bool
	// failed: the Model's entry for claim is Failed.
	failed bool
	// unsupported: the request cannot be honoured and the workload is held.
	unsupported bool
	// entry is the Model's record for claim, when there is one.
	entry   *inferencev1alpha1.PrefetchStatus
	reason  string
	message string
}

// prefetchGate reads the Model's prefetch entry for the claim this service's
// pods mount.
func (r *InferenceServiceReconciler) prefetchGate(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, cache effectiveModelCache) prefetchGate {
	g := prefetchGate{}
	if !cache.prefetchRequested() {
		return g
	}
	g.requested = true
	switch kind, why := classifyPrefetchRequest(isvc, model, r.ModelCachePath); kind {
	case prefetchNotApplicable:
		g.reason, g.message = ReasonPrefetchNotApplicable, why
		return g
	case prefetchUnsupported:
		g.applies, g.unsupported = true, true
		g.reason, g.message = ReasonPrefetchUnsupported, why+"; the workload is held until the spec changes"
		return g
	case prefetchPending:
		g.applies = true
		g.reason, g.message = ReasonWaitingForModel, why
		return g
	}
	g.applies = true
	g.claim = cache.pvcName(r.ModelCacheMode)
	entry := findPrefetchStatus(model, g.claim)
	g.entry = entry
	switch {
	case entry == nil:
		g.reason = ReasonWaitingForPrefetch
		g.message = fmt.Sprintf("no prefetch of Model %q into cache claim %q recorded yet; the Model controller starts one", model.Name, g.claim)
	case entry.Phase == inferencev1alpha1.PrefetchPhaseFailed:
		g.failed = true
		g.reason = ReasonPrefetchFailed
		g.message = fmt.Sprintf("prefetch of Model %q into cache claim %q failed: %s", model.Name, g.claim, entry.Message)
	case entry.Phase != inferencev1alpha1.PrefetchPhaseSucceeded:
		g.reason = ReasonWaitingForPrefetch
		g.message = fmt.Sprintf("prefetch of Model %q into cache claim %q is %s (job %q): %s", model.Name, g.claim, entry.Phase, entry.JobName, entry.Message)
	case entry.ObservedGeneration != model.Generation || entry.CacheKey != effectiveModelCacheKey(model):
		g.reason = ReasonWaitingForPrefetch
		g.message = fmt.Sprintf("prefetch of Model %q into cache claim %q was confirmed for generation %d; waiting for generation %d", model.Name, g.claim, entry.ObservedGeneration, model.Generation)
	default:
		// The pods stage the recorded expansion of a glob; a Model whose
		// expansion is not current for its generation is not ready to be
		// consumed even though its entry is.
		if _, err := modelStagingPlan(model); err != nil {
			g.reason = ReasonWaitingForPrefetch
			g.message = fmt.Sprintf("Model %q has not recorded its staged file set for generation %d yet", model.Name, model.Generation)
			return g
		}
		g.satisfied = true
		g.reason = ReasonPrefetchComplete
		g.message = fmt.Sprintf("Model %q is prefetched into cache claim %q (job %q); pods mount it read-only at /models", model.Name, g.claim, entry.JobName)
	}
	return g
}

// setModelPrefetchedCondition records the gate on the service. The condition
// exists only while the field is set; it is removed, not flipped, when the
// user drops the request.
func setModelPrefetchedCondition(isvc *inferencev1alpha1.InferenceService, g prefetchGate) {
	if !g.requested {
		meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionModelPrefetched)
		return
	}
	status := metav1.ConditionFalse
	if g.satisfied {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               ConditionModelPrefetched,
		Status:             status,
		ObservedGeneration: isvc.Generation,
		Reason:             g.reason,
		Message:            g.message,
	})
}

// holdForPrefetch keeps the workload as it is while a requested prefetch is
// pending or failed. held=true means the caller returns what it is handed:
// the live Deployment and its counts when one exists (so a rollout waits on
// the new weights while the old template keeps serving), or a status write
// and no workload when nothing serves yet. A failed prefetch holds the same
// way and shows on the service; it never rolls pods onto a cache that was
// not filled. A workload scaled to zero is not held: there is nothing to
// protect and the scale-down must land.
//
// Two gaps while a hold is on. A replica change other than to zero waits
// with the template. A multiNode group is returned with its last counts and
// not reconciled, so a member lost to an eviction or a drain is recreated
// only once the prefetch finishes; the Deployment path heals through its
// ReplicaSet meanwhile.
func (r *InferenceServiceReconciler) holdForPrefetch(ctx context.Context, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, cache effectiveModelCache, desiredReplicas int32, modelReady bool) (held bool, dep *appsv1.Deployment, counts replicaCounts, result *ctrl.Result, err error) {
	g := r.prefetchGate(isvc, model, cache)
	setModelPrefetchedCondition(isvc, g)
	if !g.applies || g.satisfied || desiredReplicas == 0 {
		// The hold's diagnosis is this controller's to clear: the Ready path
		// clears scheduling fields only once pods serve, and a first rollout
		// that was held has none yet.
		switch isvc.Status.SchedulingStatus {
		case ReasonWaitingForPrefetch, ReasonWaitingForModel, ReasonPrefetchFailed:
			isvc.Status.SchedulingStatus = ""
			isvc.Status.SchedulingMessage = ""
			isvc.Status.WaitingFor = ""
		}
		return false, nil, replicaCounts{}, nil, nil
	}
	log := logf.FromContext(ctx)

	if isvc.Spec.MultiNode != nil {
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(isvc.Namespace), client.MatchingLabels{LabelMultiNodeGroup: isvc.Name}); err != nil {
			return true, nil, replicaCounts{}, nil, err
		}
		if len(pods.Items) > 0 {
			log.Info("Holding the multiNode group on its current members while the prefetch runs", "claim", g.claim, "reason", g.reason)
			return true, nil, replicaCounts{Ready: isvc.Status.ReadyReplicas, Observed: isvc.Status.Replicas}, nil, nil
		}
	} else {
		existing := &appsv1.Deployment{}
		err := r.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace}, existing)
		switch {
		case err == nil && ensureDeploymentOwned(isvc, existing) == nil:
			log.Info("Holding the Deployment on its current template while the prefetch runs", "claim", g.claim, "reason", g.reason)
			return true, existing, replicaCounts{Ready: existing.Status.ReadyReplicas, Observed: existing.Status.Replicas}, nil, nil
		case err != nil && !apierrors.IsNotFound(err):
			return true, nil, replicaCounts{}, nil, err
		}
	}

	// Nothing serves yet: no pods until the weights are in place, and none
	// at all for a request that cannot be honoured.
	phase := "Pending"
	if g.failed || g.unsupported {
		phase = PhaseFailed
	}
	log.Info("Not creating the workload until the prefetch completes", "claim", g.claim, "reason", g.reason, "phase", phase)
	res, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, phase, modelReady, 0, isvc.Status.Replicas, desiredReplicas, "",
		g.message, &SchedulingInfo{Status: g.reason, Message: g.message})
	return true, nil, replicaCounts{}, &res, updateErr
}

// restagePrefetchOnMissingFiles reacts to serving pods that found the staged
// files gone from a claim the Model's status still records as filled (a
// cache clear, a claim recreated under the same name). The service deletes
// the Model's prefetch Job for its claim; the Model controller, which owns
// the Job, recreates it as a re-verification, and the failing pods pass
// model-verify on their next init retry with no rollout. Once per episode:
// the condition must have transitioned after the entry last succeeded, so a
// claim the serving node cannot reach at all is re-staged once and then
// stays reported, not re-staged in a loop.
func (r *InferenceServiceReconciler) restagePrefetchOnMissingFiles(ctx context.Context, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, cache effectiveModelCache) {
	g := r.prefetchGate(isvc, model, cache)
	if !g.applies || !g.satisfied {
		return
	}
	if !serviceReportsPrefetchedFilesMissing(isvc) {
		return
	}
	cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionModelTransferHealthy)
	entry := findPrefetchStatus(model, g.claim)
	if entry == nil || entry.JobName == "" {
		return
	}
	if entry.LastTransitionTime != nil && !cond.LastTransitionTime.After(entry.LastTransitionTime.Time) {
		return
	}
	log := logf.FromContext(ctx)
	// Gone already, or not this Model's: the Model controller re-verifies
	// a missing Job on its own, and a foreign Job is nobody's to delete.
	job, err := ownedPrefetchJob(ctx, r.Client, model, entry.JobName)
	if err != nil {
		log.Error(err, "Failed to read the prefetch job to re-stage", "job", entry.JobName)
		return
	}
	if job == nil {
		return
	}
	// A staging already in flight is the remedy, not a stale Job; and one
	// that finished after the failure was observed has already refilled the
	// claim. Either way deleting it would only start the download over.
	if !jobSucceeded(job) && !jobFailed(job) {
		return
	}
	if job.Status.CompletionTime != nil && !cond.LastTransitionTime.After(job.Status.CompletionTime.Time) {
		return
	}
	if err := deletePrefetchJob(ctx, r.Client, job); err != nil {
		log.Error(err, "Failed to delete the prefetch job to re-stage", "job", job.Name)
		return
	}
	log.Info("Serving pods found the prefetched files missing; deleted the prefetch job so the Model stages the claim again",
		"job", job.Name, "claim", g.claim, "model", model.Name)
	if r.Recorder != nil {
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, ReasonPrefetchRestaged, "Reconcile",
			"pods report PrefetchedFilesMissing on claim %q; deleted prefetch job %q so Model %q stages it again", g.claim, job.Name, model.Name)
	}
}

// warnPrefetchTargetMismatch tells a service that did not ask for its own
// prefetch when its Model was prefetched, but into another claim than the
// one this service mounts: its pods download at serve time, which is what
// the Model's spec.prefetch was meant to avoid.
func (r *InferenceServiceReconciler) warnPrefetchTargetMismatch(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, cache effectiveModelCache) {
	if r.Recorder == nil {
		return
	}
	claim, ok := unprefetchedConsumerClaim(cache, model, r.ModelCacheMode, r.ModelCachePath)
	if !ok {
		return
	}
	var staged []string
	for _, e := range model.Status.Prefetch {
		if e.ClaimName == claim {
			return
		}
		if e.Phase == inferencev1alpha1.PrefetchPhaseSucceeded {
			staged = append(staged, e.ClaimName)
		}
	}
	if len(staged) == 0 {
		return
	}
	r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, ReasonPrefetchTargetMismatch, "Reconcile",
		"Model %q was prefetched into %s, not into this service's cache claim %q; the pods download at serve time. "+
			"Set spec.modelCache.prefetch to stage into %q, or point spec.modelCache.claimName at a prefetched claim",
		model.Name, strings.Join(staged, ", "), claim, claim)
}
