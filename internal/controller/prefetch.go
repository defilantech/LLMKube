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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Prefetch helpers both controllers use. model_prefetch.go stages: it is
// the only code that creates, replaces or deletes a prefetch Job and the
// only writer of Model.status.prefetch. inferenceservice_prefetch.go
// consumes: it reads the entry for its claim, holds the workload, and asks
// for a re-stage by deleting the Job. What they must agree on lives here,
// so neither side can decide it differently.

// ReasonPrefetchedFilesMissing: a serving pod built to consume a prefetched
// cache found a staged file absent from the claim. Classified by the
// init-container diagnosis and read by both controllers.
const ReasonPrefetchedFilesMissing = "PrefetchedFilesMissing"

// prefetchApplicability classifies a service's prefetch request against its
// Model and the operator. Both controllers use it, so the Model side never
// stages for a request the service side cannot consume.
type prefetchApplicability int

const (
	// prefetchApplies: the pods mount a cache claim the Job can fill.
	prefetchApplies prefetchApplicability = iota
	// prefetchNotApplicable: nothing downloads at serve time, so there is
	// nothing to prefetch and nothing to hold for.
	prefetchNotApplicable
	// prefetchUnsupported: the pods would download at serve time and no
	// prefetch can cover it. The request is refused, not ignored.
	prefetchUnsupported
	// prefetchPending: the request can be honoured once the Model's own
	// reconcile has caught up (its cache key is not derived yet).
	prefetchPending
)

// classifyPrefetchRequest is the one place that decides whether a prefetch
// request can be honoured.
func classifyPrefetchRequest(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelCachePath string) (prefetchApplicability, string) {
	memberNode, memberClaim := multiNodeMemberClaim(isvc)
	draftRef, draftField := draftWeightsRefFor(isvc)
	switch {
	case isMetalModel(model):
		return prefetchNotApplicable, "a Metal service mounts no cache claim; the host metal-agent fetches the model itself"
	case isPVCSource(model.Spec.Source) || isOCISource(model.Spec.Source):
		return prefetchNotApplicable, fmt.Sprintf("source %q is pre-staged and mounted without a download", model.Spec.Source)
	case isLocalSource(model.Spec.Source):
		return prefetchNotApplicable, fmt.Sprintf("source %q is a local path the operator copies; nothing is downloaded on the serving node", model.Spec.Source)
	case modelCachePath == "":
		return prefetchUnsupported, "model caching is disabled on the operator (--model-cache-path unset, chart modelCache.enabled=false), so the pods would download into an emptyDir on the serving node"
	case !operatorStagesModel(isvc, resolveBackend(isvc)):
		return prefetchUnsupported, fmt.Sprintf("runtime %q fetches the model itself on the serving node (skipModelInit, or a runtime that stages only with spec.stageModel); a prefetched claim cannot be put in front of it", resolveServingRuntime(isvc))
	case draftRef != "":
		return prefetchUnsupported, fmt.Sprintf("%s %q is not prefetched and its downloader would run on the serving node", draftField, draftRef)
	case memberClaim != "":
		return prefetchUnsupported, fmt.Sprintf("multiNode member on node %q mounts its own cache claim %q, which no prefetch stages; the prefetch covers the service-level claim only", memberNode, memberClaim)
	case !prefetchableSource(model):
		return prefetchUnsupported, fmt.Sprintf("source %q is not one the downloader stages ahead of time, so the pods would download it on the serving node", model.Spec.Source)
	case effectiveModelCacheKey(model) == "":
		return prefetchPending, fmt.Sprintf("Model %q has no cache key yet; it is derived when the Model reconciles", model.Name)
	}
	return prefetchApplies, ""
}

// multiNodeMemberClaim returns the first member-level claim override and its
// node, or "". memberPodFromTemplate rewrites that member's cache volume to
// it, so a prefetch into the service-level claim never fills what the pod
// mounts.
func multiNodeMemberClaim(isvc *inferencev1alpha1.InferenceService) (node, claim string) {
	if isvc == nil || isvc.Spec.MultiNode == nil {
		return "", ""
	}
	for _, m := range isvc.Spec.MultiNode.Members {
		if m.ModelCache != nil && m.ModelCache.ClaimName != "" {
			return m.Node, m.ModelCache.ClaimName
		}
	}
	return "", ""
}

// resolveServingRuntime names the runtime for messages.
func resolveServingRuntime(isvc *inferencev1alpha1.InferenceService) string {
	if isvc.Spec.Runtime == "" {
		return "llamacpp"
	}
	return isvc.Spec.Runtime
}

// modelWantsPrefetch reports whether the Model asks to be staged into its own
// cache claim: spec.prefetch, or its alias spec.modelCache.prefetch.
func modelWantsPrefetch(model *inferencev1alpha1.Model) bool {
	if model == nil || model.Spec.PrefetchOCI != nil {
		return false
	}
	return model.Spec.Prefetch || (model.Spec.ModelCache != nil && model.Spec.ModelCache.Prefetch)
}

// modelCacheClaimName returns spec.modelCache.claimName, or "".
func modelCacheClaimName(model *inferencev1alpha1.Model) string {
	if model == nil || model.Spec.ModelCache == nil {
		return ""
	}
	return model.Spec.ModelCache.ClaimName
}

// prefetchableSource reports whether the Model's source is a remote artifact
// the downloader stack can stage ahead of time (http/https/hf, or a bare
// repo ID with spec.files). Local paths, pvc://, oci:// and s3:// sources
// are not prefetched.
func prefetchableSource(model *inferencev1alpha1.Model) bool {
	if model == nil {
		return false
	}
	// A source is prefetchable when it normalizes to a fetchable http(s) URL:
	// hf:// and huggingface.co repo URLs become resolve URLs, direct file URLs
	// stay as-is, while bare org/name (runtime-resolved), local paths, pvc://
	// and s3:// do not. Checking the normalized scheme directly preserves the
	// original semantics without depending on isRemoteHTTPSource's repo-vs-file
	// classification.
	normalized := normalizeHFSource(model.Spec.Source)
	if hasSchemeFold(normalized, "https://") || hasSchemeFold(normalized, "http://") {
		return true
	}
	// A bare repo ID with spec.files is staged by the operator (stagingRepoBase).
	return hasMultiFileStaging(model) && isHFRepoSource(model.Spec.Source)
}

// findPrefetchStatus returns the entry for claim, or nil.
func findPrefetchStatus(model *inferencev1alpha1.Model, claim string) *inferencev1alpha1.PrefetchStatus {
	for i := range model.Status.Prefetch {
		if model.Status.Prefetch[i].ClaimName == claim {
			return &model.Status.Prefetch[i]
		}
	}
	return nil
}

// ownedPrefetchJob returns the Job at name when it exists and model controls
// it, else nil. The name is deterministic, so a user's Job can sit at it; a
// Job this Model does not own is never adopted, replaced or deleted.
func ownedPrefetchJob(ctx context.Context, c client.Reader, model *inferencev1alpha1.Model, name string) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: model.Namespace}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if !metav1.IsControlledBy(job, model) {
		return nil, nil
	}
	return job, nil
}

// deletePrefetchJob removes a prefetch Job and its pod. The API server's
// default for Jobs is to orphan the pod, so propagation is set; a Job that
// is already gone is not an error.
func deletePrefetchJob(ctx context.Context, c client.Client, job *batchv1.Job) error {
	propagation := metav1.DeletePropagationBackground
	if prefetchJobIsStager(job) || job.Annotations[annotationOCIFiles] != "" {
		// OCI publishers must also finish terminating before a replacement
		// can push to the same tag.
		// These stagers may bypass flock on FUSE. Keep the Job's name
		// occupied until its pods are gone, including on spec replacement.
		propagation = metav1.DeletePropagationForeground
	}
	if err := c.Delete(ctx, job, client.PropagationPolicy(propagation)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// prefetchJobIsStager reports whether a prefetch Job is a stager, one whose
// downloader may write a FUSE cache without flock. It reads the switch
// buildPrefetchJob sets in the pod template, not spec.podReplacementPolicy:
// the API server drops that field when the JobPodReplacementPolicy feature
// gate is off, and a stager read back without it must still be deleted in
// the foreground (#2252).
func prefetchJobIsStager(job *batchv1.Job) bool {
	for _, c := range job.Spec.Template.Spec.InitContainers {
		for _, env := range c.Env {
			if env.Name == "LLMKUBE_PREFETCH_STAGER" && env.Value == "true" {
				return true
			}
		}
	}
	return false
}

// serviceReportsPrefetchedFilesMissing reports whether a service's pods found
// the staged files gone from the claim they mount.
func serviceReportsPrefetchedFilesMissing(isvc *inferencev1alpha1.InferenceService) bool {
	c := meta.FindStatusCondition(isvc.Status.Conditions, ConditionModelTransferHealthy)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == ReasonPrefetchedFilesMissing
}

// unprefetchedConsumerClaim returns the claim a service's pods mount when
// that service relies on the Model's own prefetch rather than asking for its
// own, and would download at serve time if the claim were not staged: the
// Model asks for prefetch, the service does not, and the pods mount a cache
// claim (not Metal, not an ephemeral or disabled cache, not a pre-staged
// source). ok is false otherwise.
func unprefetchedConsumerClaim(cache effectiveModelCache, model *inferencev1alpha1.Model, cacheMode, cachePath string) (string, bool) {
	if cache.prefetchRequested() || !modelWantsPrefetch(model) || isMetalModel(model) ||
		!modelNeedsCachePVC(model, cache, cachePath) {
		return "", false
	}
	return cache.pvcName(cacheMode), true
}

// jobSucceeded / jobFailed read the Job's terminal conditions.
func jobSucceeded(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
