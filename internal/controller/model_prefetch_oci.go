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
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/ocipublish"
)

const (
	ociPublisherContainer = "model-publisher"
	annotationOCIFiles    = "inference.llmkube.dev/oci-files"
)

func (r *ModelReconciler) removeOCIPrefetch(ctx context.Context, model *inferencev1alpha1.Model) error {
	readCtx, cancel := context.WithTimeout(ctx, r.prefetchJobReadTimeout())
	defer cancel()
	job, err := ownedPrefetchJob(readCtx, r.Client, model, prefetchJobNameFor(model.Name, "-prefetch-oci"))
	if err != nil {
		return err
	}
	if job != nil {
		if err := deletePrefetchJob(ctx, r.Client, job); err != nil {
			return err
		}
	}
	model.Status.PrefetchOCI = nil
	return nil
}

// OCI publication has no claim to verify or serialize. Keep its result outside
// status.prefetch, which service controllers interpret as PVC staging records.
func (r *ModelReconciler) reconcileOCIPrefetch(ctx context.Context, model *inferencev1alpha1.Model) (ctrl.Result, error) {
	spec := model.Spec.PrefetchOCI
	fail := func(reason, message string) (ctrl.Result, error) {
		if err := r.removeOCIPrefetch(ctx, model); err != nil {
			return ctrl.Result{}, err
		}
		model.Status.Phase = PhaseFailed
		model.Status.PrefetchOCI = &inferencev1alpha1.OCIPrefetchStatus{Phase: inferencev1alpha1.PrefetchPhaseFailed, Message: message, ObservedGeneration: model.Generation}
		setCondition(model, ConditionAvailable, metav1.ConditionFalse, reason, message)
		setCondition(model, ConditionProgressing, metav1.ConditionFalse, reason, message)
		return ctrl.Result{}, nil
	}
	if !model.Spec.Prefetch || model.Spec.ModelCache != nil || !prefetchableSource(model) {
		return fail("InvalidOCIPrefetch", "prefetchOCI requires prefetch: true, an HTTP(S)/HF source, and no Model modelCache configuration")
	}
	if _, err := ocipublish.Destination(spec.Image); err != nil {
		return fail("InvalidOCIPrefetch", err.Error())
	}
	if r.OCIPublisherImage == "" {
		return fail("InvalidOCIPrefetch", "operator --oci-publisher-image must name an image containing /manager publish-model")
	}
	plan, listErr, planErr := r.prefetchStagingPlan(ctx, model)
	if listErr != nil {
		result, err := fail("RepoListingFailed", listErr.Error())
		result.RequeueAfter = prefetchClaimWait
		return result, err
	}
	if planErr != nil {
		return fail("InvalidFileSet", planErr.Error())
	}
	jobModel := prefetchJobModel(model, plan)
	fingerprintInput, err := json.Marshal(struct {
		Source      string
		Files       string
		Destination *inferencev1alpha1.OCIPrefetchSpec
		Primary     string
	}{model.Spec.Source, prefetchFingerprint(jobModel, plan), spec, canonicalModelBasename(model)})
	if err != nil {
		return ctrl.Result{}, err
	}
	sum := sha256.Sum256(fingerprintInput)
	fingerprint := hex.EncodeToString(sum[:])
	entry := model.Status.PrefetchOCI
	if entry == nil || entry.Fingerprint != fingerprint {
		entry = &inferencev1alpha1.OCIPrefetchStatus{Phase: inferencev1alpha1.PrefetchPhasePending, Fingerprint: fingerprint}
		model.Status.PrefetchOCI = entry
	}
	entry.ObservedGeneration = model.Generation
	if entry.Phase == inferencev1alpha1.PrefetchPhaseSucceeded {
		r.completeOCIPrefetch(model, entry)
		return ctrl.Result{}, nil
	}
	entry.JobName = prefetchJobNameFor(model.Name, "-prefetch-oci")
	readCtx, cancel := context.WithTimeout(ctx, r.prefetchJobReadTimeout())
	defer cancel()
	job, err := ownedPrefetchJob(readCtx, r.Client, model, entry.JobName)
	if err != nil {
		return ctrl.Result{}, err
	}
	model.Status.Phase = PhaseDownloading
	setCondition(model, ConditionAvailable, metav1.ConditionFalse, ReasonPrefetchRunning, "Publishing model to OCI registry")
	setCondition(model, ConditionProgressing, metav1.ConditionTrue, ReasonPrefetchRunning, "Publishing model to OCI registry")
	if job != nil && (job.Annotations[annotationPrefetchFingerprint] != fingerprint || !job.DeletionTimestamp.IsZero()) {
		if job.DeletionTimestamp.IsZero() {
			if err := deletePrefetchJob(ctx, r.Client, job); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: prefetchRetryInterval}, nil
	}
	if job == nil {
		var files []string
		job, files, err = r.buildOCIPrefetchJob(ctx, jobModel, fingerprint)
		if err != nil {
			return fail("InvalidOCIPrefetch", err.Error())
		}
		entry.Files = files
		if err := controllerutil.SetControllerReference(model, job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := client.IgnoreAlreadyExists(r.Create(ctx, job)); err != nil {
			return ctrl.Result{}, err
		}
		entry.Phase = inferencev1alpha1.PrefetchPhaseRunning
		entry.Message = ""
		return ctrl.Result{RequeueAfter: prefetchRetryInterval}, nil
	}
	// Recover the file contract from the Job, not a possibly changed metadata
	// basename, if status persistence failed immediately after creation.
	if err := json.Unmarshal([]byte(job.Annotations[annotationOCIFiles]), &entry.Files); err != nil {
		return fail("InvalidOCIPrefetchResult", "OCI prefetch Job has no valid file manifest")
	}
	if jobFailed(job) {
		entry.Phase = inferencev1alpha1.PrefetchPhaseFailed
		entry.Message = "OCI prefetch Job failed; inspect its pod logs and delete the Job to retry"
		model.Status.Phase = PhaseFailed
		setCondition(model, ConditionAvailable, metav1.ConditionFalse, ReasonPrefetchFailed, entry.Message)
		setCondition(model, ConditionProgressing, metav1.ConditionFalse, ReasonPrefetchFailed, entry.Message)
		return ctrl.Result{}, nil
	}
	if jobSucceeded(job) {
		digest, err := r.ociPublishedImage(ctx, job, spec.Image)
		if err != nil {
			return ctrl.Result{}, err
		}
		if digest != "" {
			entry.Image, entry.Phase, entry.Message = digest, inferencev1alpha1.PrefetchPhaseSucceeded, ""
			r.completeOCIPrefetch(model, entry)
			return ctrl.Result{}, nil
		}
		entry.Message = "Completed OCI prefetch Job has no valid publisher digest; inspect its pods and delete the Job to retry"
		entry.Phase = inferencev1alpha1.PrefetchPhaseFailed
		model.Status.Phase = PhaseFailed
		setCondition(model, ConditionAvailable, metav1.ConditionFalse, "InvalidOCIPrefetchResult", entry.Message)
		setCondition(model, ConditionProgressing, metav1.ConditionFalse, "InvalidOCIPrefetchResult", entry.Message)
	} else {
		entry.Phase = inferencev1alpha1.PrefetchPhaseRunning
		reason, message := r.prefetchJobBlocked(ctx, job)
		if reason != "" {
			entry.Message = message
		}
	}
	return ctrl.Result{RequeueAfter: prefetchRetryInterval}, nil
}

func (r *ModelReconciler) completeOCIPrefetch(model *inferencev1alpha1.Model, entry *inferencev1alpha1.OCIPrefetchStatus) {
	model.Status.Phase = PhaseReady
	seedPrefetchCacheKey(model)
	if hasMultiFileStaging(model) {
		model.Status.StagedFiles = append([]string(nil), entry.Files...)
	} else {
		model.Status.StagedFiles = nil
	}
	setCondition(model, ConditionAvailable, metav1.ConditionTrue, ReasonPrefetchComplete, "Model published to "+entry.Image)
	setCondition(model, ConditionProgressing, metav1.ConditionFalse, ReasonPrefetchComplete, "OCI prefetch completed")
	setCondition(model, ConditionDegraded, metav1.ConditionFalse, ReasonPrefetchComplete, "OCI prefetch completed")
}

func (r *ModelReconciler) ociPublishedImage(ctx context.Context, job *batchv1.Job, destination string) (string, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name}); err != nil {
		return "", err
	}
	tag, err := ocipublish.Destination(destination)
	if err != nil {
		return "", err
	}
	for _, pod := range pods.Items {
		if !metav1.IsControlledBy(&pod, job) {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			ended := status.State.Terminated
			if status.Name != ociPublisherContainer || ended == nil || ended.ExitCode != 0 {
				continue
			}
			result := strings.TrimSpace(ended.Message)
			if !strings.HasPrefix(result, "oci://") {
				continue
			}
			digest, err := name.NewDigest(strings.TrimPrefix(result, "oci://"), name.StrictValidation)
			if err == nil && digest.Context().Name() == tag.Context().Name() {
				return result, nil
			}
		}
	}
	return "", nil
}

func (r *ModelReconciler) buildOCIPrefetchJob(ctx context.Context, model *inferencev1alpha1.Model, fingerprint string) (*batchv1.Job, []string, error) {
	image := r.InitContainerImage
	if image == "" {
		image = defaultPrefetchImage
	}
	storage := buildEmptyDirStorageConfig(model, nil, effectiveModelCache{}, model.Namespace, r.CACertConfigMap, image, hfEndpointFromSecret(ctx, r.Client, model))
	plan, err := modelStagingPlan(model)
	if err != nil {
		return nil, nil, err
	}
	var files []ocipublish.File
	if plan == nil {
		files = []ocipublish.File{{Path: strings.TrimPrefix(storage.modelPath, "/models/"), Name: canonicalModelBasename(model)}}
	} else {
		for _, file := range plan.Files {
			files = append(files, ocipublish.File{Path: path.Join(strings.TrimPrefix(storage.stagedDir, "/models/"), file), Name: file})
		}
	}
	names := make([]string, 0, len(files))
	for _, file := range files {
		names = append(names, file.Name)
	}
	encoded, err := json.Marshal(files)
	if err != nil {
		return nil, nil, err
	}
	encodedNames, err := json.Marshal(names)
	if err != nil {
		return nil, nil, err
	}
	uid := resolveInitContainerUser(r.InitContainerUser)
	for i := range storage.initContainers {
		storage.initContainers[i].SecurityContext = restrictedPrefetchSecurityContext(uid)
	}
	applyInitContainerDiagnostics(storage.initContainers)
	publisher := corev1.Container{
		Name: ociPublisherContainer, Image: r.OCIPublisherImage,
		Command:                  []string{"/manager", "publish-model"},
		Args:                     []string{"--image=" + model.Spec.PrefetchOCI.Image, "--files=" + string(encoded)},
		VolumeMounts:             []corev1.VolumeMount{{Name: "model-storage", MountPath: DefaultModelCachePath, ReadOnly: true}},
		SecurityContext:          restrictedPrefetchSecurityContext(uid),
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
	if ref := model.Spec.PrefetchOCI.PushSecretRef; ref != nil {
		if ref.Name == "" {
			return nil, nil, fmt.Errorf("pushSecretRef.name must not be empty")
		}
		storage.volumes = append(storage.volumes, corev1.Volume{Name: "registry-auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: ref.Name, Items: []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
		}}})
		publisher.VolumeMounts = append(publisher.VolumeMounts, corev1.VolumeMount{Name: "registry-auth", MountPath: "/registry", ReadOnly: true})
		publisher.Env = append(publisher.Env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: "/registry"})
	}
	if r.CACertConfigMap != "" {
		publisher.VolumeMounts = append(publisher.VolumeMounts, corev1.VolumeMount{Name: "custom-ca-cert", MountPath: "/custom-certs", ReadOnly: true})
		publisher.Args = append(publisher.Args, "--ca-directory=/custom-certs")
	}
	backoff, ttl := int32(2), int32(86400)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: prefetchJobNameFor(model.Name, "-prefetch-oci"), Namespace: model.Namespace,
			Annotations: map[string]string{annotationPrefetchFingerprint: fingerprint, annotationOCIFiles: string(encodedNames)},
		},
		Spec: batchv1.JobSpec{BackoffLimit: &backoff, TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: boolPtr(false),
				SecurityContext: &corev1.PodSecurityContext{FSGroup: &uid, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				NodeSelector:    model.Spec.PrefetchNodeSelector, Tolerations: model.Spec.PrefetchTolerations, Affinity: model.Spec.PrefetchAffinity,
				InitContainers: storage.initContainers, Containers: []corev1.Container{publisher}, Volumes: storage.volumes,
			}},
		},
	}, names, nil
}
