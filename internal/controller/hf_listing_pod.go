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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/ocipublish"
)

// Expanding a glob in spec.files from a listing pod (#2451).
//
// With --file-listing=pod the controller never contacts the model host. A Job
// in the Model's namespace, labeled and placed like the Model's prefetch Job,
// runs /manager list-repo-files: it lists the tree with listHFRepoFiles,
// resolves spec.files against the listing with ResolveFileSet, and returns
// the files the set resolves to, with their sizes, or the file-set error,
// through its termination message. A NetworkPolicy that lets the prefetch
// pods reach the model host covers the listing too, so the controller needs
// no egress to it.

const (
	fileListingContainer = "file-listing"
	fileListingJobSuffix = "-listing"

	annotationFileListingFingerprint = "inference.llmkube.dev/file-listing-fingerprint"

	// ReasonRepoListingPending: a listing pod is expanding spec.files and has
	// not finished.
	ReasonRepoListingPending = "RepoListingPending"

	repoListingResultPrefix = "llmkube-listing/v1 "
	// The kubelet keeps at most 4096 bytes of a container's termination
	// message, so a result must fit in it whole.
	maxListingResultBytes = 4096
	// Bounds what a result may decompress to, far above any result that fits
	// the termination message honestly.
	maxListingDecodedBytes = 1 << 20

	fileListingDeadlineSeconds = 300
	// A finished listing is reused while its inputs match, and replaced at the
	// cadence a controller listing retries at: a minute after a failure (the
	// RepoListingFailed requeue), five after a file-set error (InvalidFileSet).
	fileListingRetryAfterFailure      = time.Minute
	fileListingRetryAfterFileSetError = 5 * time.Minute
	repoListingPendingRequeue         = 15 * time.Second
)

// Where a glob in spec.files is expanded (--file-listing).
const (
	FileListingController = "controller"
	FileListingPod        = "pod"
)

// FileListingImageFor validates --file-listing and --file-listing-image and
// returns the image the Model reconciler lists from: empty for the controller.
func FileListingImageFor(mode, image string) (string, error) {
	switch mode {
	case FileListingController:
		return "", nil
	case FileListingPod:
		if image == "" {
			return "", errors.New("--file-listing=pod needs --file-listing-image, an image containing /manager list-repo-files (the operator image)")
		}
		return image, nil
	}
	return "", fmt.Errorf("--file-listing must be %q or %q, got %q", FileListingController, FileListingPod, mode)
}

// errRepoListingPending reports a listing pod that has not finished. The
// caller reports RepoListingPending and polls; the Job's own events requeue
// the Model sooner.
var errRepoListingPending = errors.New("spec.files has a glob and the source repository listing has not finished")

// listedFileSetError is the error the listing pod hit resolving spec.files
// against the full listing. The caller reports it as InvalidFileSet, as it
// does its own ResolveFileSet error; it is the same function on the same
// listing, so the same message.
type listedFileSetError struct{ msg string }

func (e *listedFileSetError) Error() string { return e.msg }

// repoListingPending reports errRepoListingPending.
func repoListingPending(err error) bool { return errors.Is(err, errRepoListingPending) }

// listedFileSet returns the listing pod's file-set error in err, or nil.
func listedFileSet(err error) *listedFileSetError {
	var fileSetErr *listedFileSetError
	if errors.As(err, &fileSetErr) {
		return fileSetErr
	}
	return nil
}

// repoListingResult is what a listing pod returns: the listed files spec.files
// resolves to, or the file-set error.
type repoListingResult struct {
	Files        []repoListedFile `json:"files,omitempty"`
	FileSetError string           `json:"fileSetError,omitempty"`
}

type repoListedFile struct {
	Path string `json:"path"`
	// Size is 0 when the listing did not size the file, which hfTotalSize
	// reads as unknown.
	Size int64 `json:"size,omitempty"`
}

func encodeRepoListingResult(res repoListingResult) (string, error) {
	payload, err := json.Marshal(res)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := zw.Write(payload); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return repoListingResultPrefix + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// decodeRepoListingResult reads a listing pod's termination message. The
// paths become cache paths, so each must be repo-relative, as a spec.files
// entry must.
func decodeRepoListingResult(msg string) (repoListingResult, error) {
	encoded, ok := strings.CutPrefix(strings.TrimSpace(msg), repoListingResultPrefix)
	if !ok {
		return repoListingResult{}, errors.New("not a listing result")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return repoListingResult{}, fmt.Errorf("decoding the listing result: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return repoListingResult{}, fmt.Errorf("decompressing the listing result: %w", err)
	}
	payload, err := io.ReadAll(io.LimitReader(zr, maxListingDecodedBytes+1))
	if err != nil {
		return repoListingResult{}, fmt.Errorf("decompressing the listing result: %w", err)
	}
	if len(payload) > maxListingDecodedBytes {
		return repoListingResult{}, fmt.Errorf("the listing result decompresses past %d bytes", maxListingDecodedBytes)
	}
	var res repoListingResult
	if err := json.Unmarshal(payload, &res); err != nil {
		return repoListingResult{}, fmt.Errorf("parsing the listing result: %w", err)
	}
	for _, f := range res.Files {
		if err := validateRepoRelativePath(f.Path); err != nil {
			return repoListingResult{}, fmt.Errorf("the listing result names %w", err)
		}
		if f.Size < 0 {
			return repoListingResult{}, fmt.Errorf("the listing result gives %q a negative size", f.Path)
		}
	}
	return res, nil
}

// RunListRepoFiles is the operator image's list-repo-files subcommand, the
// whole of a listing pod's work. It writes its result to --result, the
// termination log: the encoded listing when the listing succeeds (with a
// file-set error inside it when spec.files does not resolve), or the error
// text when it fails, which it also returns so the container exits non-zero.
func RunListRepoFiles(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("list-repo-files", flag.ContinueOnError)
	treeURL := flags.String("url", "", "Hub tree API URL to list")
	filesJSON := flags.String("files", "", "spec.files as a JSON array")
	mmproj := flags.String("mmproj", "", "spec.mmproj")
	caDir := flags.String("ca-directory", "", "Additional CA certificates")
	result := flags.String("result", "/dev/termination-log", "Result file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	msg, err := listRepoFilesResult(ctx, *treeURL, *filesJSON, *mmproj, *caDir)
	if err != nil {
		msg = truncateUTF8(err.Error(), maxListingResultBytes)
	}
	if writeErr := os.WriteFile(*result, []byte(msg), 0o600); writeErr != nil && err == nil {
		return writeErr
	}
	return err
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func listRepoFilesResult(ctx context.Context, treeURL, filesJSON, mmproj, caDir string) (string, error) {
	var files []string
	if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
		return "", fmt.Errorf("decoding --files: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caDir != "" {
		roots, err := ocipublish.RootsWithCADirectory(caDir)
		if err != nil {
			return "", fmt.Errorf("loading the CA certificates in %s: %w", caDir, err)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	// Trimmed as hfTokenFromSecret trims the Secret value the controller reads.
	token := strings.TrimSpace(os.Getenv("HF_TOKEN"))
	entries, err := listHFRepoFiles(ctx, &http.Client{Transport: transport, Timeout: remoteMetadataTimeout}, treeURL, token)
	if err != nil {
		return "", err
	}
	var res repoListingResult
	if plan, err := ResolveFileSet(files, mmproj, hfFilePaths(entries)); err != nil {
		res.FileSetError = err.Error()
	} else if plan != nil {
		staged := make(map[string]bool, len(plan.Files))
		for _, f := range plan.Files {
			staged[f] = true
		}
		// Every listed file the set stages: ResolveFileSet over these alone
		// expands to the same plan, and hfTotalSize sizes it the same.
		for _, e := range entries {
			if staged[e.Path] {
				res.Files = append(res.Files, repoListedFile{Path: e.Path, Size: int64(e.Size)})
			}
		}
	}
	msg, err := encodeRepoListingResult(res)
	if err != nil {
		return "", err
	}
	if len(msg) > maxListingResultBytes {
		return "", fmt.Errorf("spec.files resolves to %d listed files, %d bytes as a listing result, and a listing pod returns at most %d through its termination message: "+
			"name the files in spec.files instead of a glob, or list from the controller (--file-listing=controller)", len(res.Files), len(msg), maxListingResultBytes)
	}
	return msg, nil
}

// buildFileListingJob builds the Job whose pod lists treeURL for the Model.
// auth reports whether the token may be sent to treeURL
// (isHFAuthSourceForEndpoint); only then does the pod get HF_TOKEN, and only
// that key of the Secret, never its other credentials.
func (r *ModelReconciler) buildFileListingJob(model *inferencev1alpha1.Model, treeURL string, auth bool) (*batchv1.Job, error) {
	files, err := json.Marshal(model.Spec.Files)
	if err != nil {
		return nil, err
	}
	security := restrictedPrefetchSecurityContext(resolveInitContainerUser(r.InitContainerUser))
	security.ReadOnlyRootFilesystem = boolPtr(true)
	container := corev1.Container{
		Name:    fileListingContainer,
		Image:   r.FileListingImage,
		Command: []string{"/manager"},
		Args:    []string{"list-repo-files", "--url=" + treeURL, "--files=" + string(files)},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
		SecurityContext:          security,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
	if model.Spec.Mmproj != "" {
		container.Args = append(container.Args, "--mmproj="+model.Spec.Mmproj)
	}
	if auth && model.Spec.SourceSecretRef != nil && model.Spec.SourceSecretRef.Name != "" {
		container.Env = []corev1.EnvVar{{Name: "HF_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: model.Spec.SourceSecretRef.Name},
			Key:                  "HF_TOKEN",
			// An ungated repository lists without a token, as on the controller.
			Optional: boolPtr(true),
		}}}}
	}
	var volumes []corev1.Volume
	if r.CACertConfigMap != "" {
		volumes = []corev1.Volume{{Name: "custom-ca-cert", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: r.CACertConfigMap}},
		}}}
		container.VolumeMounts = []corev1.VolumeMount{{Name: "custom-ca-cert", MountPath: "/custom-certs", ReadOnly: true}}
		container.Args = append(container.Args, "--ca-directory=/custom-certs")
	}
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: prefetchPodLabels(model)},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtr(false),
			SecurityContext:              &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			NodeSelector:                 model.Spec.PrefetchNodeSelector,
			Tolerations:                  model.Spec.PrefetchTolerations,
			Affinity:                     model.Spec.PrefetchAffinity,
			Containers:                   []corev1.Container{container},
			Volumes:                      volumes,
		},
	}
	// The generation is in the fingerprint so each spec lists afresh, as the
	// controller lists once per generation; the template covers the inputs.
	encoded, err := json.Marshal(struct {
		Generation int64
		Template   corev1.PodTemplateSpec
	}{model.Generation, template})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	backoff, deadline, ttl := int32(0), int64(fileListingDeadlineSeconds), int32(24*60*60)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prefetchJobNameFor(model.Name, fileListingJobSuffix),
			Namespace: model.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "llmkube",
				"app.kubernetes.io/component":  "model-file-listing",
				"app.kubernetes.io/managed-by": "llmkube-controller",
				"inference.llmkube.dev/model":  model.Name,
			},
			Annotations: map[string]string{annotationFileListingFingerprint: hex.EncodeToString(sum[:])[:16]},
		},
		Spec: batchv1.JobSpec{
			// One attempt per Job: the controller retries at its own cadence,
			// and a failure surfaces at once instead of after the retries.
			BackoffLimit: &backoff,
			// Bounds a pod that never starts (an image it cannot pull, a
			// placement no node satisfies), which no retry would end.
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template:                template,
		},
	}, nil
}

// listRepoFilesInPod returns the listing a listing pod made for the Model's
// current inputs, creating its Job when there is none. errRepoListingPending
// means the Job has not finished. A finished Job is reused while its inputs
// match, since a prefetch lists on every pass until it completes.
func (r *ModelReconciler) listRepoFilesInPod(ctx context.Context, model *inferencev1alpha1.Model, treeURL string, auth bool) ([]hfTreeEntry, error) {
	want, err := r.buildFileListingJob(model, treeURL, auth)
	if err != nil {
		return nil, err
	}
	pending := fmt.Errorf("%w: Job %s is listing it", errRepoListingPending, want.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(want), job); apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(model, want, r.Scheme); err != nil {
			return nil, err
		}
		if err := r.Create(ctx, want); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("creating listing Job %s: %w", want.Name, err)
		}
		return nil, pending
	} else if err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(job, model) {
		return nil, fmt.Errorf("a Job named %s exists and is not Model %s's listing Job: rename or delete it", job.Name, model.Name)
	}
	if job.DeletionTimestamp != nil {
		return nil, pending
	}
	if job.Annotations[annotationFileListingFingerprint] != want.Annotations[annotationFileListingFingerprint] {
		return nil, r.replaceFileListingJob(ctx, job, pending)
	}
	finished, failed, at := jobFinishedAt(job)
	if !finished {
		return nil, pending
	}
	if failed {
		if time.Since(at) >= fileListingRetryAfterFailure {
			return nil, r.replaceFileListingJob(ctx, job, pending)
		}
		return nil, fmt.Errorf("listing Job %s failed: %s", job.Name, r.fileListingFailure(ctx, job))
	}
	res, err := r.fileListingResult(ctx, job)
	if err != nil {
		if time.Since(at) >= fileListingRetryAfterFailure {
			return nil, r.replaceFileListingJob(ctx, job, pending)
		}
		return nil, fmt.Errorf("listing Job %s: %w", job.Name, err)
	}
	if res.FileSetError != "" {
		if time.Since(at) >= fileListingRetryAfterFileSetError {
			return nil, r.replaceFileListingJob(ctx, job, pending)
		}
		return nil, &listedFileSetError{msg: res.FileSetError}
	}
	// Never nil: an empty listing is still a listing, which ResolveFileSet
	// tells apart from none (hfFilePaths).
	entries := make([]hfTreeEntry, 0, len(res.Files))
	for _, f := range res.Files {
		entries = append(entries, hfTreeEntry{Type: "file", Path: f.Path, Size: hfTreeSize(f.Size)})
	}
	return entries, nil
}

// replaceFileListingJob deletes a listing Job whose result is stale and
// returns pending: the next pass creates its replacement.
func (r *ModelReconciler) replaceFileListingJob(ctx context.Context, job *batchv1.Job, pending error) error {
	if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting listing Job %s: %w", job.Name, err)
	}
	return pending
}

// jobFinishedAt reports whether the Job reached a terminal condition, which
// one, and when.
func jobFinishedAt(job *batchv1.Job) (finished, failed bool, at time.Time) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, false, c.LastTransitionTime.Time
		case batchv1.JobFailed:
			return true, true, c.LastTransitionTime.Time
		}
	}
	return false, false, time.Time{}
}

func (r *ModelReconciler) fileListingPods(ctx context.Context, job *batchv1.Job) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name}); err != nil {
		return nil, err
	}
	var owned []corev1.Pod
	for i := range pods.Items {
		if metav1.IsControlledBy(&pods.Items[i], job) {
			owned = append(owned, pods.Items[i])
		}
	}
	return owned, nil
}

// fileListingResult reads the result a succeeded listing Job's pod wrote.
func (r *ModelReconciler) fileListingResult(ctx context.Context, job *batchv1.Job) (repoListingResult, error) {
	pods, err := r.fileListingPods(ctx, job)
	if err != nil {
		return repoListingResult{}, err
	}
	for i := range pods {
		for _, s := range pods[i].Status.ContainerStatuses {
			if t := s.State.Terminated; s.Name == fileListingContainer && t != nil && t.ExitCode == 0 {
				return decodeRepoListingResult(t.Message)
			}
		}
	}
	return repoListingResult{}, errors.New("its pod left no listing result")
}

// fileListingFailure says why a listing Job failed: the error its pod wrote,
// or, for a pod that never ran, the Job's reason with what held the pod.
func (r *ModelReconciler) fileListingFailure(ctx context.Context, job *batchv1.Job) string {
	held := ""
	if pods, err := r.fileListingPods(ctx, job); err == nil {
		for i := range pods {
			for _, s := range pods[i].Status.ContainerStatuses {
				if s.Name != fileListingContainer {
					continue
				}
				if t := s.State.Terminated; t != nil && t.ExitCode != 0 {
					if msg := strings.TrimSpace(t.Message); msg != "" {
						return msg
					}
					return fmt.Sprintf("its pod exited %d (%s)", t.ExitCode, t.Reason)
				}
				if w := s.State.Waiting; w != nil && w.Reason != "" {
					held = strings.TrimSpace(w.Reason + " " + w.Message)
				}
			}
			for _, c := range pods[i].Status.Conditions {
				if held == "" && c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
					held = strings.TrimSpace(c.Reason + " " + c.Message)
				}
			}
		}
	}
	reason := "the Job failed"
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			reason = strings.TrimSpace(c.Reason + ": " + c.Message)
		}
	}
	if held != "" {
		return reason + "; its pod: " + held
	}
	return reason
}

// clearRepoListingPending drops the Progressing a pending listing set once the
// listing has an outcome; the caller's status write carries the change.
func clearRepoListingPending(model *inferencev1alpha1.Model) {
	if c := meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing); c != nil && c.Reason == ReasonRepoListingPending {
		meta.RemoveStatusCondition(&model.Status.Conditions, ConditionProgressing)
	}
}
