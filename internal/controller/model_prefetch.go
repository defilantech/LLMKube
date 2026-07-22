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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Model prefetch (#904): a Model with spec.prefetch=true and a remote source
// gets its artifact pulled into the namespace's SHARED model cache PVC ahead
// of any InferenceService, via an owner-ref'd Job whose pod reuses the exact
// init-container download stack the serving path uses
// (buildModelStorageConfig with a nil InferenceService): same cache-prep
// container, same downloader image and ETag handling, same multi-file
// staging plan, same SourceSecretRef env. The first serving pod then starts
// from a cache hit instead of a cold download.
//
// The prefetch deliberately targets the shared cache
// (ModelCacheModeShared): perService cache PVCs are created per
// InferenceService at serve time and cannot be pre-populated for a service
// that does not exist yet. The field's GoDoc documents that limitation and
// the pvc:// staging alternative.

// defaultPrefetchImage mirrors the --init-container-image flag default for
// the no-op completion container when the reconciler is constructed without
// the field (tests); production wiring always sets InitContainerImage.
const defaultPrefetchImage = "docker.io/curlimages/curl:8.18.0"

// prefetchJobName returns the deterministic Job name for a Model's prefetch.
func prefetchJobName(model *inferencev1alpha1.Model) string {
	return model.Name + "-prefetch"
}

// seedPrefetchCacheKey populates Status.CacheKey (in memory) when empty so
// the storage builder targets the shared cache PVC rather than an emptyDir.
func seedPrefetchCacheKey(model *inferencev1alpha1.Model) {
	if model.Status.CacheKey == "" {
		model.Status.CacheKey = computeCacheKey(model.Spec.Source)
	}
}

// prefetchEligible reports whether this Model should take the prefetch path:
// the field is set and the source is a remote artifact the downloader stack
// can fetch (http/https/hf). Local paths and pvc:// sources have nothing to
// prefetch.
func prefetchEligible(model *inferencev1alpha1.Model) bool {
	if model == nil || !model.Spec.Prefetch {
		return false
	}
	return isRemoteHTTPSource(normalizeHFSource(model.Spec.Source))
}

// reconcilePrefetch drives the prefetch state machine. handled=true means
// the prefetch path owns this reconcile and Reconcile should return its
// result; handled=false means fall through to the normal source dispatch
// (not eligible, or the prefetch already completed and the model is Ready).
func (r *ModelReconciler) reconcilePrefetch(ctx context.Context, model *inferencev1alpha1.Model) (bool, ctrl.Result, error) {
	if !prefetchEligible(model) {
		return false, ctrl.Result{}, nil
	}

	// Completed earlier: the cache is warm and status is terminal. Fall
	// through so the ordinary remote-source reconcile keeps owning Ready
	// (revalidation, accelerator checks) without re-running the Job.
	if model.Status.Phase == PhaseReady && model.Status.CacheKey != "" {
		return false, ctrl.Result{}, nil
	}

	logger := logf.FromContext(ctx)

	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: prefetchJobName(model), Namespace: model.Namespace}, job)
	switch {
	case apierrors.IsNotFound(err):
		result, startErr := r.startPrefetch(ctx, model)
		return true, result, startErr
	case err != nil:
		return true, ctrl.Result{}, fmt.Errorf("checking prefetch job: %w", err)
	}

	switch {
	case jobSucceeded(job):
		return true, ctrl.Result{}, r.completePrefetch(ctx, model)
	case jobFailed(job):
		logger.Info("Prefetch job failed", "job", job.Name)
		model.Status.Phase = PhaseFailed
		return true, ctrl.Result{}, r.updateStatus(ctx, model, ConditionProgressing, metav1.ConditionFalse,
			"PrefetchFailed", fmt.Sprintf("prefetch job %q failed; see its pod logs", job.Name))
	default:
		// Still running: reflect progress and poll. The Job's completion
		// does not generate a Model event, so a modest requeue keeps the
		// status honest without watching Jobs from this controller.
		if model.Status.Phase != PhaseDownloading {
			model.Status.Phase = PhaseDownloading
			seedPrefetchCacheKey(model)
			if err := r.updateStatus(ctx, model, ConditionProgressing, metav1.ConditionTrue,
				"PrefetchRunning", "Prefetch job downloading model into the shared cache"); err != nil {
				return true, ctrl.Result{}, err
			}
		}
		return true, ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
}

// startPrefetch ensures the shared cache PVC exists and creates the
// owner-ref'd prefetch Job.
func (r *ModelReconciler) startPrefetch(ctx context.Context, model *inferencev1alpha1.Model) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// Seed the cache key BEFORE building the Job: effectiveModelCacheKey
	// reads Status.CacheKey for single-file models, and an empty key makes
	// buildModelStorageConfig fall back to an emptyDir — the prefetch would
	// download and then discard the artifact. Same derivation as the remote
	// branch of reconcileBySourceType, so the serving pod later computes the
	// identical key and takes the cache-hit path.
	seedPrefetchCacheKey(model)

	if err := ensureSharedModelCachePVC(ctx, r.Client, model.Namespace,
		r.ModelCacheSize, r.ModelCacheClass, r.ModelCacheAccessMode); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring shared model cache PVC: %w", err)
	}

	job, err := r.buildPrefetchJob(model)
	if err != nil {
		model.Status.Phase = PhaseFailed
		return ctrl.Result{}, r.updateStatus(ctx, model, ConditionProgressing, metav1.ConditionFalse,
			"PrefetchInvalid", err.Error())
	}
	if err := controllerutil.SetControllerReference(model, job, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("owner-ref prefetch job: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Raced with a concurrent reconcile; the poll branch takes over.
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("creating prefetch job: %w", err)
	}

	logger.Info("Created prefetch job", "job", job.Name)
	model.Status.Phase = PhaseDownloading
	if err := r.updateStatus(ctx, model, ConditionProgressing, metav1.ConditionTrue,
		"PrefetchStarted", "Started prefetch job downloading model into the shared cache"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// completePrefetch marks the Model Ready with its cache key so the first
// InferenceService takes the cache-hit path.
func (r *ModelReconciler) completePrefetch(ctx context.Context, model *inferencev1alpha1.Model) error {
	model.Status.Phase = PhaseReady
	seedPrefetchCacheKey(model)
	model.Status.AcceleratorReady = r.checkAcceleratorAvailability(ctx, model)
	now := metav1.Now()
	model.Status.LastUpdated = &now
	return r.updateStatus(ctx, model, "Available", metav1.ConditionTrue,
		"ModelPrefetched", "Model prefetched into the shared cache")
}

// buildPrefetchJob assembles the download Job. The pod reuses the serving
// path's init containers and volumes verbatim (nil InferenceService: the
// only isvc-derived input is an optional fsGroup override) and adds a no-op
// completion container, since a Job pod must have at least one non-init
// container.
func (r *ModelReconciler) buildPrefetchJob(model *inferencev1alpha1.Model) (*batchv1.Job, error) {
	if effectiveModelCacheKey(model) == "" {
		return nil, fmt.Errorf("prefetch: model has no cache key; refusing to build a Job that would download into an emptyDir")
	}
	storage := buildModelStorageConfig(model, nil, model.Namespace, true, ModelCacheModeShared,
		r.CACertConfigMap, r.InitContainerImage, r.DefaultFSGroup, r.AllowedHostPathRoots)
	if len(storage.initContainers) == 0 {
		return nil, fmt.Errorf("prefetch: source %q produced no downloader containers", model.Spec.Source)
	}

	backoff := int32(2)
	ttl := int32(24 * 60 * 60) // keep a day for log triage, then self-clean

	var podSecurity *corev1.PodSecurityContext
	if r.DefaultFSGroup > 0 {
		fs := r.DefaultFSGroup
		podSecurity = &corev1.PodSecurityContext{FSGroup: &fs}
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
		},
		Spec: batchv1.JobSpec{
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
					InitContainers:  storage.initContainers,
					Containers: []corev1.Container{{
						Name:    "prefetch-done",
						Image:   image,
						Command: []string{"sh", "-c", "echo prefetch complete"},
					}},
					Volumes: storage.volumes,
				},
			},
		},
	}, nil
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
