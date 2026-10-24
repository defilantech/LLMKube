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
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Model prefetch (#904): a remote Model is pulled into a model cache PVC ahead
// of its serving pods by an owner-ref'd Job whose pod reuses the exact
// init-container download stack the serving path uses
// (buildModelStorageConfig with a synthetic InferenceService naming the
// claim): same cache-prep container, same downloader image and ETag handling,
// same multi-file staging plan, same SourceSecretRef env.
//
// Prefetch is tracked per cache claim (#2136). A Model can be read through
// more than one claim, so status.prefetch holds one entry per target, and each
// target has its own Job. Targets come from two places:
//
//   - the Model's own prefetch (spec.prefetch or spec.modelCache.prefetch),
//     staged into spec.modelCache.claimName when set, else the single claim
//     the referencing InferenceServices agree on (#1676), else the namespace's
//     shared cache. This target gates the Model's Ready: the Model reads
//     Downloading until it succeeds.
//   - every referencing InferenceService with modelCache.prefetch, staged
//     into the claim that service's pods mount. These never hold the Model's
//     Ready; the service holds its own rollout on its entry instead.
//
// An entry records the fingerprint of what it staged (cache key, file set,
// pinned digests), so a spec change that alters the files replaces the Job
// and a change that does not merely refreshes the entry's generation. A Job
// whose target is no longer wanted is deleted while it runs; a finished one
// is left for its TTL.
//
// This file is the only writer of status.prefetch and the only creator of
// prefetch Jobs. The InferenceService side (inferenceservice_prefetch.go)
// consumes the entries; what both sides must agree on is in prefetch.go.

// defaultPrefetchImage mirrors the --init-container-image flag default for
// the no-op completion container when the reconciler is constructed without
// the field (tests); production wiring always sets InitContainerImage.
const defaultPrefetchImage = "docker.io/curlimages/curl:8.18.0"

// initContainerUID is the numeric uid under which the default init image runs:
// curlimages/curl:8.18.0 declares the named user curl_user, uid 101, gid 102.
// Under --model-cache-prep=never the prefetch containers set runAsNonRoot with
// this uid explicitly, because the kubelet refuses runAsNonRoot on an image
// whose USER is a name it cannot resolve
// (pkg/kubelet/kuberuntime/security_context_others.go, verifyRunAsNonRoot).
const initContainerUID = 101

// Annotations the prefetch Job carries so a later reconcile can tell whether
// it was built for the current spec and target.
const (
	annotationPrefetchFingerprint = "inference.llmkube.dev/prefetch-fingerprint"
	annotationPrefetchClaim       = "inference.llmkube.dev/prefetch-claim"
	// annotationPrefetchReverify marks a Job that reruns a staging the status
	// already records, because the earlier Job is gone.
	annotationPrefetchReverify = "inference.llmkube.dev/prefetch-reverify"
)

// Model conditions and reasons written by the prefetch path.
const (
	// ConditionPrefetchSkipped is True when spec.prefetch is set on a source
	// the downloader does not stage, so the field is not silently ignored.
	ConditionPrefetchSkipped = "PrefetchSkipped"
	// ConditionPrefetchTargetMismatch is True when a referencing
	// InferenceService mounts a cache claim the Model was not prefetched
	// into: that service downloads at serve time.
	ConditionPrefetchTargetMismatch = "PrefetchTargetMismatch"

	ReasonSourceNotPrefetchable = "SourceNotPrefetchable"
	ReasonPrefetchClaimMismatch = "ClaimMismatch"
	ReasonPrefetchClaimNotFound = "PrefetchClaimNotFound"
	// ReasonPrefetchListingFailed: the InferenceService listing failed and no
	// own target is on record, so the Model holds rather than guess the
	// shared cache.
	ReasonPrefetchListingFailed = "PrefetchListingFailed"
	ReasonPrefetchStarted       = "PrefetchStarted"
	ReasonPrefetchRunning       = "PrefetchRunning"
	ReasonPrefetchFailed        = "PrefetchFailed"
	ReasonPrefetchComplete      = "PrefetchComplete"
	ReasonModelPrefetched       = "ModelPrefetched"
	// ReasonPrefetchUnschedulable: the Job's pod cannot be scheduled, so the
	// download has not started. Placement (or the claim's node) is the fix.
	ReasonPrefetchUnschedulable = "PrefetchUnschedulable"
	// ReasonPrefetchReverifying: the claim was staged earlier and its Job is
	// gone, so a new Job checks the files are still there. The entry stays
	// Succeeded and the Model Ready meanwhile; only a failed check changes
	// them.
	ReasonPrefetchReverifying = "PrefetchReverifying"
	// ReasonPrefetchStalled: the Job's pod was scheduled, none of its
	// containers is running, and it has stayed Pending past
	// prefetchPodStartGrace (an image pull, a volume that cannot attach); the
	// entry names the container that is waiting.
	ReasonPrefetchStalled = "PrefetchStalled"
)

const (
	// prefetchPollInterval is the requeue while a Job runs. Its completion,
	// failure and deletion are events (Owns(Job)); the requeue exists for
	// what the Job object does not show: a pod that cannot be scheduled or
	// has not started.
	prefetchPollInterval = time.Minute
	// prefetchRetryInterval follows a create race or a replaced Job's
	// deletion, both of which resolve within seconds.
	prefetchRetryInterval = 5 * time.Second
	// prefetchClaimWait is the retry while a target claim does not exist yet
	// (a user-owned claim still to be created, or a perService claim the
	// InferenceService controller has not provisioned), and while the
	// InferenceService listing fails.
	prefetchClaimWait = 30 * time.Second
	// prefetchReverifyRetry is the retry while a routine re-verification
	// waits for a single-writer claim to be unmounted.
	prefetchReverifyRetry = 10 * time.Minute
)

// prefetchPodStartGrace is how long a scheduled Job pod may stay Pending
// before the entry names it as stalled. A variable so a test can shrink it.
var prefetchPodStartGrace = 5 * time.Minute

// podInitializingWaitingReason is the kubelet's default waiting state for a
// container it has not reached yet when the pod has init containers
// (pkg/kubelet/kubelet_pods.go, defaultWaitingState): it names a position in
// the init sequence, never a cause that placement can fix.
const podInitializingWaitingReason = "PodInitializing"

// maxPrefetchJobNameLength is the longest Job name whose pods' job-name
// label is still valid.
const maxPrefetchJobNameLength = 63

// prefetchJobName is the Job name of the Model's own prefetch target, and
// the name under which a Job created before per-claim naming is found.
func prefetchJobName(model *inferencev1alpha1.Model) string {
	return prefetchJobNameFor(model.Name, "-prefetch")
}

// prefetchTargetJobName is the Job name for a prefetch into claim: one Job
// per claim, named by the claim's hash so two services sharing a claim share
// the Job.
func prefetchTargetJobName(model *inferencev1alpha1.Model, claim string) string {
	sum := sha256.Sum256([]byte(claim))
	return prefetchJobNameFor(model.Name, "-prefetch-"+hex.EncodeToString(sum[:])[:8])
}

// prefetchJobNameFor appends suffix to the Model name, shortening the name
// part so the Job name fits the pod label the Job controller derives from it.
func prefetchJobNameFor(modelName, suffix string) string {
	if room := maxPrefetchJobNameLength - len(suffix); len(modelName) > room {
		// Names that share their kept prefix must not share a Job.
		sum := sha256.Sum256([]byte(modelName))
		tag := "-" + hex.EncodeToString(sum[:])[:6]
		modelName = strings.TrimRight(modelName[:room-len(tag)], "-.") + tag
	}
	return modelName + suffix
}

// prefetchJobNameInUse reports whether an entry other than claim's already
// drives a Job by name. Two targets must never resolve one Job name, or they
// replace each other's Job without end.
func prefetchJobNameInUse(model *inferencev1alpha1.Model, name, claim string) bool {
	for _, e := range model.Status.Prefetch {
		if e.JobName == name && e.ClaimName != claim {
			return true
		}
	}
	return false
}

// inFlightOwnClaim returns the claim of the Model's own target on record in
// any phase, or "".
func inFlightOwnClaim(model *inferencev1alpha1.Model) string {
	for _, e := range model.Status.Prefetch {
		if e.Own {
			return e.ClaimName
		}
	}
	return ""
}

// stagedClaimFor returns the Model's own target claim already staged for
// fingerprint, or "". A claim a service requested is not an own target: the
// Model's Ready must not rest on a claim that goes away with that service.
func stagedClaimFor(model *inferencev1alpha1.Model, fingerprint string) string {
	for _, e := range model.Status.Prefetch {
		if e.Own && e.Phase == inferencev1alpha1.PrefetchPhaseSucceeded && e.Fingerprint == fingerprint {
			return e.ClaimName
		}
	}
	return ""
}

// seedPrefetchCacheKey sets Status.CacheKey (in memory) to the key the source
// hashes to, so the storage builder targets the cache PVC rather than an
// emptyDir. It also moves the key when the source changed: the fingerprint is
// taken from it, and waiting for the runtime-resolved path to catch up would
// let a stale key pass a changed source off as already staged. Every
// prefetchable source derives its key this way, so an equal key is left alone.
func seedPrefetchCacheKey(model *inferencev1alpha1.Model) {
	if key := computeCacheKey(model.Spec.Source); effectiveModelCacheKey(model) != key {
		model.Status.CacheKey = key
	}
}

// prefetchEligible reports whether this Model's own prefetch runs: the field
// is set and the source is one the downloader stages. A glob in spec.files no
// longer disqualifies it; the prefetch path expands it from the repository
// listing (#2071) before building the Job.
func prefetchEligible(model *inferencev1alpha1.Model) bool {
	return modelWantsPrefetch(model) && prefetchableSource(model)
}

// DefaultPrefetchJobReadTimeout bounds the read of the prefetch Job, so a
// cache read that cannot complete does not hold a shared reconcile worker.
const DefaultPrefetchJobReadTimeout = 30 * time.Second

// prefetchJobReadTimeout returns the configured bound, or the default.
func (r *ModelReconciler) prefetchJobReadTimeout() time.Duration {
	if r.PrefetchJobReadTimeout > 0 {
		return r.PrefetchJobReadTimeout
	}
	return DefaultPrefetchJobReadTimeout
}

// prefetchTarget is one cache claim the Model is staged into.
type prefetchTarget struct {
	claim string
	// own marks the Model's own target, the one that gates Ready.
	own bool
	// requestedBy lists the InferenceServices whose modelCache.prefetch
	// asked for this claim.
	requestedBy []string
}

// prefetchTargets resolves the claims to stage into: the Model's own target
// when it asks for one, plus one per claim the referencing services with
// modelCache.prefetch mount. ownReason explains why the own target fell back
// to the shared cache (empty when it did not). services must already have
// inherited the Model's modelCache (referencingServices does this).
func (r *ModelReconciler) prefetchTargets(model *inferencev1alpha1.Model, services []*inferencev1alpha1.InferenceService, listErr error, stagedClaim string) (targets []prefetchTarget, ownReason string) {
	index := map[string]int{}
	add := func(claim string, own bool, svc string) {
		i, ok := index[claim]
		if !ok {
			index[claim] = len(targets)
			targets = append(targets, prefetchTarget{claim: claim})
			i = len(targets) - 1
		}
		targets[i].own = targets[i].own || own
		if svc != "" {
			targets[i].requestedBy = append(targets[i].requestedBy, svc)
		}
	}
	if modelWantsPrefetch(model) {
		var claim string
		claim, ownReason = resolveOwnPrefetchClaim(model, services, listErr, stagedClaim)
		if claim != "" {
			add(claim, true, "")
		}
	}
	for _, svc := range services {
		if !servicePrefetchRequested(svc) {
			continue
		}
		// The same classification the service applies: a request its pods
		// cannot consume (Metal, caching disabled, a runtime the operator
		// does not stage) must not fill a claim nobody mounts.
		if kind, _ := classifyPrefetchRequest(svc, model, r.ModelCachePath); kind != prefetchApplies {
			continue
		}
		add(modelCachePVCName(svc, r.ModelCacheMode), false, svc.Name)
	}
	return targets, ownReason
}

// resolveOwnPrefetchClaim picks the claim the Model's own prefetch stages
// into, and a reason when the choice is a fallback rather than an explicit
// or agreed target.
//
// spec.modelCache.claimName wins outright: it is the declared target, and
// prefetch never falls back from it. Otherwise a claim already staged for the
// current spec (stagedClaim) stays the target: a service that appears after
// the download completed gets a PrefetchTargetMismatch report, not a second
// download. Failing both, the resolved policy (#1676) applies: exactly one
// distinct modelCache.claimName among the referencing services names the PVC
// the serving pods mount; none, or several that disagree, means the shared
// cache, and the caller surfaces that.
func resolveOwnPrefetchClaim(model *inferencev1alpha1.Model, services []*inferencev1alpha1.InferenceService, listErr error, stagedClaim string) (claim string, reason string) {
	if c := modelCacheClaimName(model); c != "" {
		return c, ""
	}
	if stagedClaim != "" {
		return stagedClaim, ""
	}
	if listErr != nil {
		// Without the listing the agreed claim is unknown. A target already
		// on record stays: falling back to the shared cache here would start
		// a second own Job, and a cache hit there would finish first and pin
		// the own target to the shared cache (#2136 again). With nothing on
		// record the caller holds until the listing works.
		if c := inFlightOwnClaim(model); c != "" {
			return c, ""
		}
		return "", "prefetch cache listing failed and no target is on record: " + listErr.Error()
	}
	var (
		seen     bool
		conflict bool
	)
	for _, svc := range services {
		claimName := userModelCacheClaimName(svc)
		if claimName == "" {
			continue // references the model but no claim override to honour
		}
		if !seen {
			seen = true
			claim = claimName
			continue
		}
		if claimName != claim {
			conflict = true
		}
	}
	switch {
	case seen && !conflict:
		return claim, ""
	case !seen:
		return ModelCachePVCName, "no InferenceService references this Model with a modelCache.claimName override; prefetching into the shared cache"
	default:
		return ModelCachePVCName, "multiple InferenceServices referencing this Model disagree on their modelCache.claimName overrides; prefetching into the shared cache"
	}
}

// referencingServices lists the InferenceServices whose modelRef names the
// Model, each with the Model's modelCache inherited so its claim resolves as
// the InferenceService controller resolves it.
func (r *ModelReconciler) referencingServices(ctx context.Context, model *inferencev1alpha1.Model) ([]*inferencev1alpha1.InferenceService, error) {
	list := &inferencev1alpha1.InferenceServiceList{}
	if err := r.List(ctx, list, client.InNamespace(model.Namespace)); err != nil {
		return nil, err
	}
	var out []*inferencev1alpha1.InferenceService
	for i := range list.Items {
		svc := &list.Items[i]
		if svc.Spec.ModelRef != model.Name {
			continue
		}
		inheritModelCache(svc, model)
		out = append(out, svc)
	}
	return out, nil
}

// prefetchFingerprint identifies what a prefetch stages: the cache directory
// (which encodes the source), the staged file set, and any pinned digests
// (spec.sha256, spec.fileSha256). A change to any of these needs a new
// download; a change to anything else on the Model (placement, tolerations,
// a label) does not.
func prefetchFingerprint(model *inferencev1alpha1.Model, plan *StagingPlan) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\n", effectiveModelCacheKey(model))
	_, _ = fmt.Fprintf(h, "sha256=%s\n", strings.ToLower(model.Spec.SHA256))
	if plan != nil {
		for _, f := range plan.Files {
			_, _ = fmt.Fprintf(h, "%s\n", f)
		}
	}
	keys := make([]string, 0, len(model.Spec.FileSHA256))
	for k := range model.Spec.FileSHA256 {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "%s=%s\n", k, strings.ToLower(string(model.Spec.FileSHA256[k])))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// prefetchStagingPlan resolves the files a prefetch stages. A glob in
// spec.files is expanded from the repository listing when the Model has not
// recorded a current expansion, as reconcileRuntimeResolvedSource does; a
// Model whose status already carries the expansion for this generation is
// not listed again. nil plan means a single-file Model. listErr reports a
// failed listing and planErr an unresolvable file set; the caller fails the
// Model the same way the runtime-resolved path does.
func (r *ModelReconciler) prefetchStagingPlan(ctx context.Context, model *inferencev1alpha1.Model) (plan *StagingPlan, listErr, planErr error) {
	if !hasMultiFileStaging(model) {
		return nil, nil, nil
	}
	if files, err := stagedFileEntries(model); err == nil {
		plan, planErr = ResolveFileSet(files, model.Spec.Mmproj, nil)
		return plan, nil, planErr
	}
	repoFiles, err := r.repoFileListing(ctx, model)
	if err != nil {
		return nil, err, nil
	}
	plan, planErr = ResolveFileSet(model.Spec.Files, model.Spec.Mmproj, hfFilePaths(repoFiles))
	return plan, nil, planErr
}

// prefetchJobModel returns the Model the Job is built from: a copy whose
// spec.files is the expanded plan, so the storage builder stages concrete
// paths without consulting status.stagedFiles (which the serving pods trust
// only once the Model is Ready for its generation).
func prefetchJobModel(model *inferencev1alpha1.Model, plan *StagingPlan) *inferencev1alpha1.Model {
	jobModel := model.DeepCopy()
	seedPrefetchCacheKey(jobModel)
	if plan != nil {
		jobModel.Spec.Files = append([]string(nil), plan.Files...)
	}
	return jobModel
}

// ensurePrefetchStatus returns the entry for claim, appending a Pending one.
func ensurePrefetchStatus(model *inferencev1alpha1.Model, claim string) *inferencev1alpha1.PrefetchStatus {
	if e := findPrefetchStatus(model, claim); e != nil {
		return e
	}
	model.Status.Prefetch = append(model.Status.Prefetch, inferencev1alpha1.PrefetchStatus{
		ClaimName: claim,
		Phase:     inferencev1alpha1.PrefetchPhasePending,
	})
	return &model.Status.Prefetch[len(model.Status.Prefetch)-1]
}

// setPrefetchPhase moves an entry to phase, stamping the transition time only
// when the phase changes so a steady state does not rewrite status.
func setPrefetchPhase(entry *inferencev1alpha1.PrefetchStatus, phase, message string) {
	if entry.Phase != phase {
		now := metav1.Now()
		entry.LastTransitionTime = &now
	}
	entry.Phase = phase
	entry.Message = message
}

// prefetchTargetOutcome is what one target's reconcile asks of the Model: a
// requeue to observe progress, and the Progressing reason the own target's
// state maps to.
type prefetchTargetOutcome struct {
	requeue time.Duration
	reason  string
}

// reconcilePrefetch drives the prefetch state machine. handled=true means
// the prefetch path owns this reconcile and Reconcile should return its
// result; handled=false means fall through to the normal source dispatch,
// with result.RequeueAfter set when a service-requested prefetch is still
// running and needs polling.
func (r *ModelReconciler) reconcilePrefetch(ctx context.Context, model *inferencev1alpha1.Model) (bool, ctrl.Result, error) {
	before := model.Status.DeepCopy()
	handled, result, err := r.reconcilePrefetchState(ctx, model)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	if !equality.Semantic.DeepEqual(before, &model.Status) {
		if err := r.Status().Update(ctx, model); err != nil {
			return true, ctrl.Result{}, err
		}
	}
	return handled, result, nil
}

// reconcilePrefetchState mutates the Model's status in memory; the caller
// persists it once if it changed.
func (r *ModelReconciler) reconcilePrefetchState(ctx context.Context, model *inferencev1alpha1.Model) (bool, ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// A source the downloader does not stage has nothing to prefetch. Say so
	// when asked, rather than leaving spec.prefetch silently inert.
	if !prefetchableSource(model) {
		if modelWantsPrefetch(model) {
			r.setPrefetchSkipped(model)
		} else {
			meta.RemoveStatusCondition(&model.Status.Conditions, ConditionPrefetchSkipped)
		}
		return false, ctrl.Result{}, nil
	}
	meta.RemoveStatusCondition(&model.Status.Conditions, ConditionPrefetchSkipped)
	// The Model's own prefetch, the one that gates its Ready.
	wantsOwn := prefetchEligible(model)

	services, listErr := r.referencingServices(ctx, model)
	if listErr != nil {
		logger.Info("Listing InferenceServices for prefetch targets failed; service-requested targets are unknown this pass", "error", listErr.Error())
	}
	requested := wantsOwn
	for _, svc := range services {
		requested = requested || servicePrefetchRequested(svc)
	}
	if !requested {
		if listErr != nil {
			// Service requests are unknown this pass; their Jobs stay.
			return false, ctrl.Result{RequeueAfter: prefetchClaimWait}, nil
		}
		r.pruneStalePrefetch(ctx, model, nil, "")
		meta.RemoveStatusCondition(&model.Status.Conditions, ConditionPrefetchTargetMismatch)
		return false, ctrl.Result{}, nil
	}

	plan, listingErr, planErr := r.prefetchStagingPlan(ctx, model)
	switch {
	case listingErr != nil:
		// The same failure, reported the same way, as the runtime-resolved
		// path: without the listing neither a Job nor a serving pod can stage
		// the files.
		model.Status.Phase = PhaseFailed
		model.Status.StagedFiles = nil
		setCondition(model, ConditionDegraded, metav1.ConditionTrue, "RepoListingFailed",
			"spec.files has a glob and listing the source repository failed: "+listingErr.Error())
		return true, ctrl.Result{RequeueAfter: time.Minute}, nil
	case planErr != nil:
		model.Status.Phase = PhaseFailed
		setCondition(model, ConditionDegraded, metav1.ConditionTrue, "InvalidFileSet", planErr.Error())
		return true, ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	// The key is part of the fingerprint, so it is settled before the
	// fingerprint is taken; the Ready written below carries it too.
	seedPrefetchCacheKey(model)
	fingerprint := prefetchFingerprint(model, plan)
	jobModel := prefetchJobModel(model, plan)
	targets, ownReason := r.prefetchTargets(model, services, listErr, stagedClaimFor(model, fingerprint))

	var (
		requeue    time.Duration
		ownOutcome prefetchTargetOutcome
		ownClaim   string
	)
	for _, t := range targets {
		entry := ensurePrefetchStatus(model, t.claim)
		entry.CacheKey = effectiveModelCacheKey(model)
		entry.Own = t.own
		outcome, err := r.reconcilePrefetchTarget(ctx, model, jobModel, t, fingerprint, entry, services)
		if err != nil {
			return true, ctrl.Result{}, err
		}
		requeue = earliestPositive(requeue, outcome.requeue)
		if t.own {
			ownOutcome = outcome
			ownClaim = t.claim
		}
	}
	if wantsOwn && ownClaim == "" {
		// No own target this pass: the listing failed with nothing on record.
		model.Status.Phase = "Pending"
		setCondition(model, ConditionProgressing, metav1.ConditionFalse, ReasonPrefetchListingFailed, ownReason)
		return true, ctrl.Result{RequeueAfter: earliestPositive(requeue, prefetchClaimWait)}, nil
	}
	if listErr != nil {
		// The service requests are unknown this pass, so nothing is pruned
		// and no mismatch is judged; the Jobs they drive stay.
		requeue = earliestPositive(requeue, prefetchClaimWait)
	} else {
		r.pruneStalePrefetch(ctx, model, targets, fingerprint)
		r.setPrefetchTargetMismatch(model, services, targets)
	}

	if !wantsOwn {
		return false, ctrl.Result{RequeueAfter: requeue}, nil
	}
	// Looked up after the prune: it compacts the slice in place, and an
	// append for a later target can move it, so a pointer taken in the loop
	// may name another claim's entry by now.
	ownEntry := findPrefetchStatus(model, ownClaim)
	if ownEntry == nil {
		return false, ctrl.Result{RequeueAfter: requeue}, nil
	}
	return r.gateModelOnOwnPrefetch(ctx, model, plan, ownEntry, ownOutcome, ownReason, requeue)
}

// gateModelOnOwnPrefetch maps the Model's own target to the Model's phase: a
// Model that asked to be prefetched is Downloading until its target succeeds,
// Ready after, and Failed or Pending when the Job is.
func (r *ModelReconciler) gateModelOnOwnPrefetch(ctx context.Context, model *inferencev1alpha1.Model, plan *StagingPlan, entry *inferencev1alpha1.PrefetchStatus, outcome prefetchTargetOutcome, ownReason string, requeue time.Duration) (bool, ctrl.Result, error) {
	gen, written := availableObservedGeneration(model)
	readyForSpec := model.Status.Phase == PhaseReady && written && gen == model.Generation
	switch entry.Phase {
	case inferencev1alpha1.PrefetchPhaseSucceeded:
		if readyForSpec {
			// Completed earlier: the cache is warm and status is terminal.
			// A re-verification in flight shows on Progressing with Ready
			// untouched; once it is done, Progressing goes back to False.
			// Either way the ordinary remote-source reconcile keeps owning
			// Ready (revalidation, accelerator checks).
			switch outcome.reason {
			case ReasonPrefetchReverifying, ReasonPrefetchUnschedulable, ReasonPrefetchStalled:
				meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
					Type:               ConditionProgressing,
					Status:             metav1.ConditionTrue,
					ObservedGeneration: model.Generation,
					Reason:             outcome.reason,
					Message:            entry.Message,
				})
			default:
				if c := meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing); c != nil && c.Status == metav1.ConditionTrue {
					setCondition(model, ConditionProgressing, metav1.ConditionFalse, ReasonPrefetchComplete, "Prefetch re-verified the claim")
				}
			}
			return false, ctrl.Result{RequeueAfter: requeue}, nil
		}
		r.completePrefetch(ctx, model, plan)
		return true, ctrl.Result{RequeueAfter: requeue}, nil
	case inferencev1alpha1.PrefetchPhasePending, inferencev1alpha1.PrefetchPhaseRunning:
		if readyForSpec {
			// A re-verification, or a hold that followed one: the files this
			// generation was declared Ready on are not in question until the
			// Job says so, so Ready stands and Progressing carries the state.
			meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
				Type:               ConditionProgressing,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: model.Generation,
				Reason:             outcome.reason,
				Message:            entry.Message,
			})
			return true, ctrl.Result{RequeueAfter: requeue}, nil
		}
	}
	switch entry.Phase {
	case inferencev1alpha1.PrefetchPhaseFailed:
		model.Status.Phase = PhaseFailed
		setCondition(model, ConditionProgressing, metav1.ConditionFalse, ReasonPrefetchFailed, entry.Message)
		return true, ctrl.Result{RequeueAfter: requeue}, nil
	case inferencev1alpha1.PrefetchPhasePending:
		model.Status.Phase = "Pending"
		reason := outcome.reason
		if reason == "" {
			reason = ReasonPrefetchRunning
		}
		setCondition(model, ConditionProgressing, metav1.ConditionFalse, reason, entry.Message)
		return true, ctrl.Result{RequeueAfter: requeue}, nil
	default:
		if model.Status.Phase != PhaseDownloading {
			model.Status.Phase = PhaseDownloading
			message := fmt.Sprintf("Started prefetch job downloading model into %q", entry.ClaimName)
			if ownReason != "" {
				message += " (" + ownReason + ")"
			}
			if outcome.reason == ReasonPrefetchRunning {
				message = "Prefetch job downloading model into the model cache"
			}
			setCondition(model, ConditionProgressing, metav1.ConditionTrue, outcome.reason, message)
		}
		// A Job whose pod cannot be scheduled or has not started is still
		// Downloading, but the condition carries that reason while it lasts,
		// and the running message once it clears. The transition time moves
		// only when the status does.
		if prefetchBlockedReason(outcome.reason) || prefetchBlockedReason(progressingReason(model)) {
			meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
				Type:               ConditionProgressing,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: model.Generation,
				Reason:             outcome.reason,
				Message:            entry.Message,
			})
		}
		return true, ctrl.Result{RequeueAfter: requeue}, nil
	}
}

// prefetchBlockedReason reports whether reason names a Job pod that is not
// progressing (unschedulable, or scheduled and not started).
func prefetchBlockedReason(reason string) bool {
	return reason == ReasonPrefetchUnschedulable || reason == ReasonPrefetchStalled
}

// progressingReason returns the Progressing condition's reason, or "".
func progressingReason(model *inferencev1alpha1.Model) string {
	if c := meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing); c != nil {
		return c.Reason
	}
	return ""
}

// reconcilePrefetchTarget brings one target's Job and status entry in line
// with the desired fingerprint.
func (r *ModelReconciler) reconcilePrefetchTarget(ctx context.Context, model, jobModel *inferencev1alpha1.Model, t prefetchTarget, fingerprint string, entry *inferencev1alpha1.PrefetchStatus, services []*inferencev1alpha1.InferenceService) (prefetchTargetOutcome, error) {
	logger := logf.FromContext(ctx)
	// Staged earlier for this spec. The Job is read anyway: while it exists
	// the entry stands; once it is gone (its TTL, or someone deleted it to
	// retry) the claim is re-verified by running it again. The status cannot
	// see the claim, so the Job's lifetime bounds how long a wiped claim can
	// pass as prefetched.
	staged := entry.Phase == inferencev1alpha1.PrefetchPhaseSucceeded && entry.Fingerprint == fingerprint

	jobName := entry.JobName
	if jobName == "" {
		jobName = prefetchTargetJobName(model, t.claim)
		// The Model's own target keeps the claim-less name unless another
		// entry already drives a Job by it.
		if t.own && !prefetchJobNameInUse(model, prefetchJobName(model), t.claim) {
			jobName = prefetchJobName(model)
		}
	}

	// Bounded read: a cache read that cannot complete must not hold a
	// shared reconcile worker.
	getCtx, cancel := context.WithTimeout(ctx, r.prefetchJobReadTimeout())
	defer cancel()

	job := &batchv1.Job{}
	err := r.Get(getCtx, types.NamespacedName{Name: jobName, Namespace: model.Namespace}, job)
	switch {
	case apierrors.IsNotFound(err):
		// A check Job on a single-writer claim that a pod has attached
		// elsewhere stalls on Multi-Attach, so a routine re-verification
		// waits for the claim to be free. A re-stage a service asked for
		// (its pods report the files missing) runs regardless: those pods
		// fail either way, and the message names what frees the claim.
		if staged && !r.restageRequested(services, t.claim) {
			if pod, mode := r.singleWriterClaimHolder(ctx, model.Namespace, t.claim); pod != "" {
				// The claim holds what this fingerprint describes, so the
				// generation is confirmed without a Job, as it is when the
				// Job still exists.
				entry.Fingerprint = fingerprint
				entry.ObservedGeneration = model.Generation
				setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseSucceeded,
					fmt.Sprintf("re-verification of claim %q waits: the claim is %s and mounted by pod %q, so a check job could only run on that pod's node; it runs once the claim is free", t.claim, mode, pod))
				return prefetchTargetOutcome{requeue: prefetchReverifyRetry, reason: ReasonPrefetchComplete}, nil
			}
		}
		return r.startPrefetchTarget(ctx, model, jobModel, t, fingerprint, entry, jobName, staged)
	case err != nil:
		return prefetchTargetOutcome{}, fmt.Errorf("checking prefetch job: %w", err)
	}

	// The name is the only key, so a Job this Model does not own is a
	// foreign object: neither adopted as the prefetch nor deleted to make
	// room. Report it and leave it alone.
	if !metav1.IsControlledBy(job, model) {
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseFailed,
			fmt.Sprintf("job %q exists but is not owned by this Model; remove or rename it to prefetch into %q", job.Name, t.claim))
		return prefetchTargetOutcome{reason: ReasonPrefetchFailed}, nil
	}

	if job.DeletionTimestamp != nil {
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhasePending,
			fmt.Sprintf("waiting for the replaced prefetch job %q to finish deleting", job.Name))
		return prefetchTargetOutcome{requeue: prefetchRetryInterval, reason: ReasonPrefetchRunning}, nil
	}

	// A Job built for another spec stages the wrong files, and one built for
	// another claim stages them in the wrong place, whether it is still
	// running or already finished: replace it. A Job without the annotations
	// predates them and is adopted as current, so an upgrade never kills a
	// download in flight.
	builtFor, hasFingerprint := job.Annotations[annotationPrefetchFingerprint]
	builtInto, hasClaim := job.Annotations[annotationPrefetchClaim]
	if (hasFingerprint && builtFor != fingerprint) || (hasClaim && builtInto != t.claim) {
		logger.Info("Prefetch job was built for an earlier spec or another claim; replacing it", "job", job.Name, "claim", t.claim, "builtInto", builtInto)
		if err := deletePrefetchJob(ctx, r.Client, job); err != nil {
			return prefetchTargetOutcome{}, fmt.Errorf("deleting stale prefetch job: %w", err)
		}
		entry.JobName = job.Name
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhasePending,
			fmt.Sprintf("replacing prefetch job %q, which was built for an earlier spec", job.Name))
		return prefetchTargetOutcome{requeue: prefetchRetryInterval, reason: ReasonPrefetchRunning}, nil
	}

	entry.JobName = job.Name
	entry.Fingerprint = fingerprint
	entry.ObservedGeneration = model.Generation
	if len(entry.Files) == 0 {
		// Adopted from a controller that recorded no names: consumers verify
		// what the plan says this Job wrote.
		entry.Files, _ = prefetchStagedFileNames(jobModel)
	}
	switch {
	case jobSucceeded(job):
		// A re-verification that passed is a new staging of the same files:
		// move the transition time so a failure a service observed before
		// it (restagePrefetchOnMissingFiles) does not count against it.
		if job.Annotations[annotationPrefetchReverify] == "true" && entry.Phase == inferencev1alpha1.PrefetchPhaseSucceeded && entry.Message != "" {
			now := metav1.Now()
			entry.LastTransitionTime = &now
		}
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseSucceeded, "")
		return prefetchTargetOutcome{reason: ReasonPrefetchComplete}, nil
	case jobFailed(job):
		logger.Info("Prefetch job failed", "job", job.Name, "claim", t.claim)
		message := fmt.Sprintf("prefetch job %q failed; see its pod logs", job.Name)
		if job.Annotations[annotationPrefetchReverify] == "true" {
			message = fmt.Sprintf("re-verification of claim %q by prefetch job %q failed; see its pod logs, then delete the Job to retry", t.claim, job.Name)
		}
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseFailed, message)
		return prefetchTargetOutcome{reason: ReasonPrefetchFailed}, nil
	default:
		// Still running. Completion and failure arrive as Job events; the
		// requeue is for the pod states the Job object does not show.
		reverify := job.Annotations[annotationPrefetchReverify] == "true"
		reason := ReasonPrefetchRunning
		message := prefetchRunningMessage(job.Name, t.claim)
		if reverify {
			reason, message = ReasonPrefetchReverifying, prefetchReverifyMessage(job.Name, t.claim)
		}
		// A Job whose pod cannot be scheduled, or was scheduled and never
		// started (an image pull, a volume that cannot attach), neither
		// completes nor fails: say so rather than report a download that has
		// not begun, and name the retry.
		if blockedReason, blocked := r.prefetchJobBlocked(ctx, job); blocked != "" {
			reason = blockedReason
			message = fmt.Sprintf("prefetch job %q %s. Fix the Model's prefetch placement "+
				"(prefetchNodeSelector, prefetchTolerations, prefetchAffinity) or the claim so the pod can run and reach claim %q, then delete the Job to retry",
				job.Name, blocked, t.claim)
			if entry.Message != message && r.Recorder != nil {
				r.Recorder.Eventf(model, nil, corev1.EventTypeWarning, blockedReason, "Prefetch", "%s", message)
			}
		}
		// A re-verification leaves the entry Succeeded: the files it checks
		// are the ones consumers already serve, so nothing is held for it;
		// only a failed check changes the entry (the Failed branch above).
		phase := inferencev1alpha1.PrefetchPhaseRunning
		if reverify && entry.Phase == inferencev1alpha1.PrefetchPhaseSucceeded {
			phase = inferencev1alpha1.PrefetchPhaseSucceeded
		}
		setPrefetchPhase(entry, phase, message)
		return prefetchTargetOutcome{requeue: prefetchPollInterval, reason: reason}, nil
	}
}

// prefetchSharedCacheClass is the class the shared claim gets when the Job
// creates it: the Model's modelCache.storageClassName, which its services
// inherit, else the operator's, as ensureModelCachePVC resolves it on the
// service side. An existing claim keeps its class.
func prefetchSharedCacheClass(model *inferencev1alpha1.Model, operatorClass string) string {
	if model.Spec.ModelCache != nil && model.Spec.ModelCache.StorageClassName != "" {
		return model.Spec.ModelCache.StorageClassName
	}
	return operatorClass
}

// restageRequested reports whether a service consuming claim says its pods
// found the staged files missing, which is a request to stage again.
func (r *ModelReconciler) restageRequested(services []*inferencev1alpha1.InferenceService, claim string) bool {
	for _, svc := range services {
		if !servicePrefetchRequested(svc) || modelCachePVCName(svc, r.ModelCacheMode) != claim {
			continue
		}
		if serviceReportsPrefetchedFilesMissing(svc) {
			return true
		}
	}
	return false
}

// singleWriterClaimHolder returns a pod that mounts claim, and the claim's
// access mode, when the claim allows no second writer (ReadWriteOnce,
// ReadWriteOncePod); "" when the claim is shareable, unmounted, or unknown.
func (r *ModelReconciler) singleWriterClaimHolder(ctx context.Context, namespace, claim string) (string, string) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: claim, Namespace: namespace}, pvc); err != nil {
		return "", ""
	}
	mode := ""
	for _, m := range pvc.Spec.AccessModes {
		switch m {
		case corev1.ReadWriteMany, corev1.ReadOnlyMany:
			return "", ""
		case corev1.ReadWriteOnce, corev1.ReadWriteOncePod:
			mode = string(m)
		}
	}
	if mode == "" {
		return "", ""
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return "", ""
	}
	// The first holder by name, not by cache order, so the message is the
	// same every pass.
	holder := ""
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim && (holder == "" || pod.Name < holder) {
				holder = pod.Name
			}
		}
	}
	if holder == "" {
		return "", ""
	}
	return holder, mode
}

// prefetchJobBlocked reports a Job pod that is Pending and cannot be
// scheduled (the scheduler's message), or was scheduled with no container
// running and stays Pending past prefetchPodStartGrace (the waiting reason of
// the container that blocks), as a reason and a description, or "" when the
// pods are progressing. A container that is running is the download at work,
// whatever the pod phase says. The pods are read through the cache the
// InferenceService controller's Pod watch already fills.
func (r *ModelReconciler) prefetchJobBlocked(ctx context.Context, job *batchv1.Job) (string, string) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name}); err != nil {
		return "", ""
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		scheduled := false
		for _, c := range pod.Status.Conditions {
			if c.Type != corev1.PodScheduled {
				continue
			}
			if c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
				return ReasonPrefetchUnschedulable, "cannot be scheduled: " + c.Message
			}
			scheduled = c.Status == corev1.ConditionTrue
		}
		if !scheduled || time.Since(pod.CreationTimestamp.Time) < prefetchPodStartGrace {
			continue
		}
		statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		running := false
		for _, cs := range statuses {
			if cs.State.Running != nil {
				running = true
				break
			}
		}
		// The kubelet holds a pod at Pending while any regular init container
		// runs (getPhase counts them as pending), so Pending with a Running
		// container is a download in progress, not one that never started.
		if running {
			continue
		}
		waiting := "no container has started"
		for _, cs := range statuses {
			if cs.State.Waiting == nil || cs.State.Waiting.Reason == podInitializingWaitingReason {
				continue
			}
			waiting = cs.Name + " is waiting: " + cs.State.Waiting.Reason
			if cs.State.Waiting.Message != "" {
				waiting += " (" + cs.State.Waiting.Message + ")"
			}
			break
		}
		// No elapsed time: the message is written to status and sent as an
		// event only when it changes.
		detail := fmt.Sprintf("pod %q has been Pending on node %q since %s; %s",
			pod.Name, pod.Spec.NodeName, pod.CreationTimestamp.UTC().Format(time.RFC3339), waiting)
		if strings.Contains(waiting, "Multi-Attach") {
			detail += ". The claim is attached on another node: suspend the consuming service (spec.suspend) to release it, or use a ReadWriteMany claim"
		}
		return ReasonPrefetchStalled, detail
	}
	return "", ""
}

// startPrefetchTarget makes sure the target claim exists (the shared cache is
// provisioned; any other claim must already exist) and creates the Job.
func (r *ModelReconciler) startPrefetchTarget(ctx context.Context, model, jobModel *inferencev1alpha1.Model, t prefetchTarget, fingerprint string, entry *inferencev1alpha1.PrefetchStatus, jobName string, reverify bool) (prefetchTargetOutcome, error) {
	logger := logf.FromContext(ctx)

	if t.claim == ModelCachePVCName {
		if err := ensureSharedModelCachePVC(ctx, r.Client, model.Namespace,
			r.ModelCacheSize, prefetchSharedCacheClass(model, r.ModelCacheClass), r.ModelCacheAccessMode); err != nil {
			return prefetchTargetOutcome{}, fmt.Errorf("ensuring shared model cache PVC: %w", err)
		}
	} else {
		// A user-owned claim, or a perService claim the InferenceService
		// controller provisions. A Job pod mounting a claim that does not
		// exist sits Pending forever, neither completing nor failing, so hold
		// the Job and name the claim instead.
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, types.NamespacedName{Name: t.claim, Namespace: model.Namespace}, pvc); err != nil {
			if !apierrors.IsNotFound(err) {
				return prefetchTargetOutcome{}, fmt.Errorf("checking prefetch cache claim %q: %w", t.claim, err)
			}
			setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhasePending,
				fmt.Sprintf("cache claim %q does not exist in namespace %q; the prefetch job starts once it does", t.claim, model.Namespace))
			return prefetchTargetOutcome{requeue: prefetchClaimWait, reason: ReasonPrefetchClaimNotFound}, nil
		}
	}

	// The names this Job writes. A re-verification checks the entry's
	// recorded names, which consumers serve: for a single file the Job is
	// pointed at that name whatever the metadata would derive now (the
	// storage builder reads the basename of Status.Path); a plan whose
	// names differ writes new files, which is a fresh staging consumers
	// wait for, not a check of what they already have.
	names, plan := prefetchStagedFileNames(jobModel)
	if reverify && len(entry.Files) > 0 && !slices.Equal(names, entry.Files) {
		if plan == nil && len(entry.Files) == 1 {
			jobModel.Status.Path = entry.Files[0]
			names = append([]string(nil), entry.Files...)
		} else {
			reverify = false
		}
	}

	job, err := r.buildPrefetchJob(ctx, jobModel, t.claim, fingerprint)
	if err != nil {
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseFailed, err.Error())
		return prefetchTargetOutcome{reason: ReasonPrefetchFailed}, nil
	}
	job.Name = jobName
	if reverify {
		job.Annotations[annotationPrefetchReverify] = "true"
	}

	// The Job pod mounts the CA ConfigMap from the Model's namespace like the
	// serving pod does. Without it the pod wedges in ContainerCreating, the
	// Job neither completes nor fails, and the Model reads Downloading
	// forever. Hold the Job and name the cause instead.
	required, caMissing, err := caCertConfigMapMissing(ctx, r.Client, r.CACertConfigMap, model.Namespace, &job.Spec.Template.Spec)
	if err != nil {
		return prefetchTargetOutcome{}, err
	}
	transitioned := setCACertConfigMapCondition(&model.Status.Conditions, model.Generation, r.CACertConfigMap, model.Namespace, required, caMissing)
	if caMissing {
		msg := caCertConfigMapMissingMessage(r.CACertConfigMap, model.Namespace)
		if transitioned && r.Recorder != nil {
			r.Recorder.Eventf(model, nil, corev1.EventTypeWarning, ReasonCACertConfigMapMissing, "Prefetch", "%s", msg)
		}
		logger.Info("CA ConfigMap missing in the Model namespace; not creating the prefetch Job",
			"configMap", r.CACertConfigMap, "namespace", model.Namespace)
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhasePending, msg)
		return prefetchTargetOutcome{requeue: caCertConfigMapMissingRequeue, reason: ReasonCACertConfigMapMissing}, nil
	}

	if err := controllerutil.SetControllerReference(model, job, r.Scheme); err != nil {
		return prefetchTargetOutcome{}, fmt.Errorf("owner-ref prefetch job: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Raced with a concurrent reconcile; the poll branch takes over.
			return prefetchTargetOutcome{requeue: prefetchRetryInterval, reason: ReasonPrefetchRunning}, nil
		}
		if apierrors.IsInvalid(err) {
			// The API server will reject it the same way every time: this
			// target fails, the Model's other work goes on.
			setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseFailed,
				fmt.Sprintf("prefetch job %q was rejected: %v", job.Name, err))
			return prefetchTargetOutcome{reason: ReasonPrefetchFailed}, nil
		}
		return prefetchTargetOutcome{}, fmt.Errorf("creating prefetch job: %w", err)
	}

	logger.Info("Created prefetch job", "job", job.Name, "claim", t.claim, "requestedBy", t.requestedBy, "reverify", reverify)
	entry.JobName = job.Name
	entry.Fingerprint = fingerprint
	entry.ObservedGeneration = model.Generation
	if !reverify {
		entry.Files = names
	}
	if reverify {
		// The entry stays Succeeded: consumers keep serving what they have
		// while the check runs. A failed check moves it to Failed.
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseSucceeded, prefetchReverifyMessage(job.Name, t.claim))
		return prefetchTargetOutcome{requeue: prefetchPollInterval, reason: ReasonPrefetchReverifying}, nil
	}
	setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseRunning, prefetchRunningMessage(job.Name, t.claim))
	return prefetchTargetOutcome{requeue: prefetchPollInterval, reason: ReasonPrefetchStarted}, nil
}

// prefetchReverifyMessage is the entry and condition message while a claim
// the status already records as filled is checked again.
func prefetchReverifyMessage(job, claim string) string {
	return fmt.Sprintf("re-verifying claim %q with prefetch job %q: the earlier job is gone (expired or deleted); the entry stays usable, a cache hit completes in seconds", claim, job)
}

// prefetchRunningMessage is the entry message while a Job stages claim.
func prefetchRunningMessage(job, claim string) string {
	return fmt.Sprintf("prefetch job %q downloading into %q", job, claim)
}

// pruneStalePrefetch drops entries no target wants any more. A Job still
// running for an abandoned target is deleted: the service that asked moved
// to another claim, or was removed. A Succeeded entry whose fingerprint is
// current is kept as the record of where the files are.
func (r *ModelReconciler) pruneStalePrefetch(ctx context.Context, model *inferencev1alpha1.Model, targets []prefetchTarget, fingerprint string) {
	wanted := map[string]bool{}
	for _, t := range targets {
		wanted[t.claim] = true
	}
	kept := model.Status.Prefetch[:0]
	for _, entry := range model.Status.Prefetch {
		switch {
		case wanted[entry.ClaimName]:
			kept = append(kept, entry)
		case entry.Phase == inferencev1alpha1.PrefetchPhaseSucceeded && entry.Fingerprint == fingerprint:
			kept = append(kept, entry)
		default:
			if entry.JobName != "" && entry.Phase != inferencev1alpha1.PrefetchPhaseSucceeded {
				r.deleteOwnedPrefetchJob(ctx, model, entry.JobName, entry.ClaimName)
			}
		}
	}
	if len(kept) == 0 {
		kept = nil
	}
	model.Status.Prefetch = kept
}

// deleteOwnedPrefetchJob deletes the Job of an abandoned target, after
// checking the object at that name is one this Model owns: the name is
// deterministic, so a user's Job could sit at it.
func (r *ModelReconciler) deleteOwnedPrefetchJob(ctx context.Context, model *inferencev1alpha1.Model, name, claim string) {
	logger := logf.FromContext(ctx)
	job, err := ownedPrefetchJob(ctx, r.Client, model, name)
	if err != nil {
		logger.Info("Could not read the prefetch job of an abandoned target; it finishes on its own", "job", name, "error", err.Error())
		return
	}
	if job == nil {
		return
	}
	if err := deletePrefetchJob(ctx, r.Client, job); err != nil {
		logger.Info("Could not delete the prefetch job of an abandoned target; it finishes on its own", "job", name, "error", err.Error())
		return
	}
	logger.Info("Deleted the prefetch job of an abandoned target", "job", name, "claim", claim)
}

// setPrefetchSkipped records that spec.prefetch is set on a source the
// downloader does not stage, and says so once as an event.
func (r *ModelReconciler) setPrefetchSkipped(model *inferencev1alpha1.Model) {
	msg := fmt.Sprintf("spec.prefetch is set but source %q is not a remote artifact the downloader stages; local paths, pvc://, oci:// and s3:// sources are served without a prefetch", model.Spec.Source)
	changed := meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type:               ConditionPrefetchSkipped,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: model.Generation,
		Reason:             ReasonSourceNotPrefetchable,
		Message:            msg,
	})
	if changed && r.Recorder != nil {
		r.Recorder.Eventf(model, nil, corev1.EventTypeWarning, ConditionPrefetchSkipped, "Prefetch", "%s", msg)
	}
}

// setPrefetchTargetMismatch reports the referencing services whose pods mount
// a claim no prefetch of this Model targets. Only a Model that asked to be
// prefetched is held to it: without spec.prefetch, downloading at serve time
// is the documented behaviour.
func (r *ModelReconciler) setPrefetchTargetMismatch(model *inferencev1alpha1.Model, services []*inferencev1alpha1.InferenceService, targets []prefetchTarget) {
	if !modelWantsPrefetch(model) {
		meta.RemoveStatusCondition(&model.Status.Conditions, ConditionPrefetchTargetMismatch)
		return
	}
	staged := map[string]bool{}
	for _, t := range targets {
		staged[t.claim] = true
	}
	var mismatched []string
	for _, svc := range services {
		claim, ok := unprefetchedConsumerClaim(svc, model, r.ModelCacheMode, r.ModelCachePath)
		if !ok {
			continue
		}
		if !staged[claim] {
			mismatched = append(mismatched, fmt.Sprintf("%s (claim %q)", svc.Name, claim))
		}
	}
	if len(mismatched) == 0 {
		meta.RemoveStatusCondition(&model.Status.Conditions, ConditionPrefetchTargetMismatch)
		return
	}
	sort.Strings(mismatched)
	meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type:               ConditionPrefetchTargetMismatch,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: model.Generation,
		Reason:             ReasonPrefetchClaimMismatch,
		Message: "InferenceService " + strings.Join(mismatched, ", ") +
			" mounts a cache this Model was not prefetched into and downloads at serve time;" +
			" set modelCache.prefetch on the service, or modelCache.claimName on the Model",
	})
}

// completePrefetch marks the Model Ready with its cache key and the staged
// file set so a referencing InferenceService takes the cache-hit path.
func (r *ModelReconciler) completePrefetch(ctx context.Context, model *inferencev1alpha1.Model, plan *StagingPlan) {
	model.Status.Phase = PhaseReady
	seedPrefetchCacheKey(model)
	if plan != nil {
		model.Status.StagedFiles = plan.Files
	} else {
		model.Status.StagedFiles = nil
	}
	model.Status.AcceleratorReady = r.checkAcceleratorAvailability(ctx, model)
	now := metav1.Now()
	model.Status.LastUpdated = &now
	setCondition(model, ConditionProgressing, metav1.ConditionFalse, ReasonPrefetchComplete, "Prefetch job completed")
	// A Degraded left by an earlier listing or Job failure is cleared, as
	// clearProgressConditions does; none is added where none was.
	if meta.FindStatusCondition(model.Status.Conditions, ConditionDegraded) != nil {
		setCondition(model, ConditionDegraded, metav1.ConditionFalse, ReasonPrefetchComplete, "Prefetch job completed")
	}
	setCondition(model, ConditionAvailable, metav1.ConditionTrue, ReasonModelPrefetched, "Model prefetched into the model cache")
}

// buildPrefetchJob assembles the download Job for one claim. The pod reuses
// the serving path's init containers and volumes verbatim (a synthetic
// InferenceService whose only field is the claim: the other isvc-derived
// inputs are an optional fsGroup override and the ephemeral budget, both
// unset) and adds a no-op completion container, since a Job pod must have at
// least one non-init container.
//
// Under --model-cache-prep=never the Job carries a Pod Security restricted
// security context instead: the prep is the only root container, so its
// absence is what lets the pod set a pod-level seccompProfile, which no
// serving-side context can be borrowed for. runAsNonRoot is per container.
//
// ctx is needed only to read HF_ENDPOINT from the Model's sourceSecretRef, so
// a prefetch from a mirror carries the bearer token as the serving pod does
// (#1900).
func (r *ModelReconciler) buildPrefetchJob(ctx context.Context, model *inferencev1alpha1.Model, claim, fingerprint string) (*batchv1.Job, error) {
	if effectiveModelCacheKey(model) == "" {
		return nil, fmt.Errorf("prefetch: model has no cache key; refusing to build a Job that would download into an emptyDir")
	}
	var target *inferencev1alpha1.InferenceService
	if claim != "" && claim != ModelCachePVCName {
		target = &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: model.Name, Namespace: model.Namespace},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name, ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: claim}},
		}
	}
	storage := buildModelStorageConfig(model, target, model.Namespace, true, ModelCacheModeShared,
		r.CACertConfigMap, r.InitContainerImage, r.DefaultFSGroup, r.ModelCachePrep, r.AllowedHostPathRoots, hfEndpointFromSecret(ctx, r.Client, model))
	if len(storage.initContainers) == 0 {
		return nil, fmt.Errorf("prefetch: source %q produced no downloader containers", model.Spec.Source)
	}
	var replacementPolicy *batchv1.PodReplacementPolicy
	for i := range storage.initContainers {
		for j := range storage.initContainers[i].Env {
			env := &storage.initContainers[i].Env[j]
			if env.Name == "LLMKUBE_PREFETCH_STAGER" {
				env.Value = "true"
				// Do not overlap a terminating writer with its replacement
				// on storage where the downloader cannot rely on flock.
				policy := batchv1.Failed
				replacementPolicy = &policy
			}
		}
	}

	backoff := int32(2)
	ttl := int32(24 * 60 * 60) // keep a day for log triage, then self-clean

	var podSecurity *corev1.PodSecurityContext
	if r.DefaultFSGroup > 0 {
		fs := r.DefaultFSGroup
		podSecurity = &corev1.PodSecurityContext{FSGroup: &fs, FSGroupChangePolicy: fsGroupChangeOnRootMismatch()}
	}

	initContainers := storage.initContainers
	doneSecurity := (*corev1.SecurityContext)(nil)
	if !cachePrepIncluded(r.ModelCachePrep) {
		// Nothing in this pod runs as root any more, so the pod itself can
		// carry the restricted-level fields the prep forbade.
		if podSecurity == nil {
			podSecurity = &corev1.PodSecurityContext{}
		}
		podSecurity.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
		for i := range initContainers {
			initContainers[i].SecurityContext = restrictedPrefetchSecurityContext()
		}
		doneSecurity = restrictedPrefetchSecurityContext()
	}

	image := r.InitContainerImage
	if image == "" {
		image = defaultPrefetchImage
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prefetchJobName(model),
			Namespace: model.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "llmkube",
				"app.kubernetes.io/component":  "model-prefetch",
				"app.kubernetes.io/managed-by": "llmkube-controller",
				"inference.llmkube.dev/model":  model.Name,
			},
			Annotations: map[string]string{
				annotationPrefetchFingerprint: fingerprint,
				annotationPrefetchClaim:       prefetchTargetName(target),
			},
		},
		Spec: batchv1.JobSpec{
			PodReplacementPolicy:    replacementPolicy,
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/component": "model-prefetch",
						"inference.llmkube.dev/model": model.Name,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:   corev1.RestartPolicyNever,
					SecurityContext: podSecurity,
					// The Model's own tolerations, so the download can reach
					// tainted nodes whose local storage backs the shared
					// cache (#1621).
					Tolerations: model.Spec.PrefetchTolerations,
					// A node-pinned claim (#1676) binds wherever this pod
					// schedules, so placement here decides which node the cache
					// lands on (#1964). Affinity says what a selector cannot:
					// off the GPU nodes, or a preference (#2136).
					NodeSelector:   model.Spec.PrefetchNodeSelector,
					Affinity:       model.Spec.PrefetchAffinity,
					InitContainers: initContainers,
					Containers: []corev1.Container{{
						Name:            "prefetch-done",
						Image:           image,
						Command:         []string{"sh", "-c", "echo prefetch complete"},
						SecurityContext: doneSecurity,
					}},
					Volumes: storage.volumes,
				},
			},
		},
	}, nil
}

// restrictedPrefetchSecurityContext is the container security context every
// container in a prefetch Job carries under --model-cache-prep=never. It is
// applied to the downloaders and to prefetch-done only when the prep is
// absent: the prep runs as uid 0, and a pod-level runAsNonRoot cannot coexist
// with it.
//
// RunAsUser is set explicitly because the default init image declares a named
// user, and the kubelet refuses runAsNonRoot on one (verifyRunAsNonRoot).
func restrictedPrefetchSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             boolPtr(true),
		RunAsUser:                int64Ptr(initContainerUID),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// prefetchTargetName returns the PVC name prefetch will stage into, for status
// messages: the resolved per-service claim, or the shared cache PVC.
func prefetchTargetName(target *inferencev1alpha1.InferenceService) string {
	if claim := userModelCacheClaimName(target); claim != "" {
		return claim
	}
	return ModelCachePVCName
}

// findModelForInferenceService maps an InferenceService to the Model it
// references, so a service created or retargeted after its Model reconciles
// the Model: the prefetch target follows the services that will read it
// (#2136). Registered with a generation predicate, so a service's status
// writes do not fan out into Model reconciles.
func (r *ModelReconciler) findModelForInferenceService(_ context.Context, obj client.Object) []reconcile.Request {
	isvc, ok := obj.(*inferencev1alpha1.InferenceService)
	if !ok || isvc.Spec.ModelRef == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: isvc.Spec.ModelRef, Namespace: isvc.Namespace},
	}}
}
