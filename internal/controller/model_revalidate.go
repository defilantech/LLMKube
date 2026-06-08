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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// sourceFingerprint is what the controller records to detect upstream changes.
// For http/https sources it carries the ETag and Content-Length from a HEAD
// request; for local sources it carries the file size and mtime. A zero value
// means the probe could not determine a fingerprint.
type sourceFingerprint struct {
	etag          string
	contentLength int64
	mtime         time.Time
}

// revalidateOutcome reports the result of a revalidation pass.
type revalidateOutcome int

const (
	// revalidateSkipped means the cadence gate was not yet open; nothing changed.
	revalidateSkipped revalidateOutcome = iota
	// revalidateIndeterminate means the probe failed (network error, 5xx,
	// air-gapped); the cached file is kept and no drift is flagged.
	revalidateIndeterminate
	// revalidateBaseline means a fingerprint was recorded for the first time;
	// no drift is flagged (an operator upgrade must not trigger a re-pull).
	revalidateBaseline
	// revalidateInSync means the upstream matches the recorded fingerprint.
	revalidateInSync
	// revalidateDrifted means the upstream bytes differ from the cached copy.
	revalidateDrifted
)

func (r *ModelReconciler) revalidateInterval() time.Duration {
	if r.RevalidateInterval > 0 {
		return r.RevalidateInterval
	}
	return DefaultRevalidateInterval
}

// shouldRevalidate reports whether the cadence gate is open for this Model.
func (r *ModelReconciler) shouldRevalidate(model *inferencev1alpha1.Model, now time.Time) bool {
	if model.Status.LastRevalidated == nil {
		return true
	}
	return now.Sub(model.Status.LastRevalidated.Time) >= r.revalidateInterval()
}

// revalidateSource checks whether the upstream source has changed relative to
// the fingerprint stored in status. It is cadence-gated: it does nothing until
// the revalidation interval has elapsed since LastRevalidated.
//
// Drift detection runs regardless of RefreshPolicy so drift is always visible
// via the SourceDrifted condition. The caller decides whether to act on a
// returned revalidateDrifted outcome (only OnChange re-downloads).
//
// Robustness: a failed probe (network error, upstream 5xx, unreachable, or an
// air-gapped local path) returns revalidateIndeterminate. The caller keeps
// serving the cached file, does not set SourceDrifted, and does not surface an
// error to the phase.
func (r *ModelReconciler) revalidateSource(
	ctx context.Context, model *inferencev1alpha1.Model,
) revalidateOutcome {
	logger := log.FromContext(ctx)
	now := time.Now()

	if !r.shouldRevalidate(model, now) {
		return revalidateSkipped
	}

	fp, ok := r.probeSource(ctx, model.Spec.Source)
	if !ok {
		// Cannot determine upstream state. Keep serving the cache, do not flag
		// drift. Do not advance LastRevalidated: a transient failure should not
		// suppress the next attempt for a full interval.
		logger.Info("Source revalidation indeterminate; keeping cached file", "source", model.Spec.Source)
		return revalidateIndeterminate
	}

	revalidatedAt := metav1.NewTime(now)
	model.Status.LastRevalidated = &revalidatedAt

	// Backward-compat: no baseline recorded yet (existing cached models, or a
	// model that reached Ready before this field existed). Record the baseline
	// and do not force a re-download.
	if model.Status.SourceETag == "" && model.Status.SourceContentLength == 0 {
		model.Status.SourceETag = fp.etag
		model.Status.SourceContentLength = fp.contentLength
		logger.Info("Recorded source fingerprint baseline",
			"source", model.Spec.Source, "etag", fp.etag, "contentLength", fp.contentLength)
		return revalidateBaseline
	}

	if fingerprintMatches(model.Status, fp) {
		return revalidateInSync
	}

	logger.Info("Detected upstream source drift",
		"source", model.Spec.Source,
		"cachedETag", model.Status.SourceETag, "upstreamETag", fp.etag,
		"cachedContentLength", model.Status.SourceContentLength, "upstreamContentLength", fp.contentLength)
	return revalidateDrifted
}

// fingerprintMatches reports whether the probed fingerprint matches the one
// stored in status. ETag is authoritative when both sides have one; otherwise
// fall back to Content-Length. A change in either signals drift.
func fingerprintMatches(status inferencev1alpha1.ModelStatus, fp sourceFingerprint) bool {
	if status.SourceETag != "" && fp.etag != "" {
		return status.SourceETag == fp.etag
	}
	return status.SourceContentLength == fp.contentLength
}

// probeSource fetches an upstream fingerprint. It returns ok=false when the
// fingerprint cannot be determined (the caller treats this as "keep the
// cache"). HTTP sources are probed with HEAD; local sources are stat'd.
func (r *ModelReconciler) probeSource(ctx context.Context, source string) (sourceFingerprint, bool) {
	switch {
	case isRemoteHTTPSource(source):
		return r.probeHTTPSource(ctx, source)
	case isLocalSource(source):
		return probeLocalSource(source)
	default:
		// PVC and HF-repo sources have no controller-observable fingerprint.
		return sourceFingerprint{}, false
	}
}

func (r *ModelReconciler) probeHTTPSource(ctx context.Context, source string) (sourceFingerprint, bool) {
	logger := log.FromContext(ctx)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, source, nil)
	if err != nil {
		logger.Info("Failed to build HEAD request for revalidation (non-fatal)", "source", source, "error", err.Error())
		return sourceFingerprint{}, false
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Info("HEAD request failed for revalidation (non-fatal)", "source", source, "error", err.Error())
		return sourceFingerprint{}, false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		logger.Info("HEAD request returned non-200 for revalidation (non-fatal)", "source", source, "status", resp.Status)
		return sourceFingerprint{}, false
	}

	fp := sourceFingerprint{
		etag:          resp.Header.Get("ETag"),
		contentLength: resp.ContentLength,
	}
	if fp.contentLength < 0 {
		// Server did not advertise a length; fall back to the Content-Length
		// header if it is parseable.
		if v := resp.Header.Get("Content-Length"); v != "" {
			if n, parseErr := strconv.ParseInt(v, 10, 64); parseErr == nil {
				fp.contentLength = n
			} else {
				fp.contentLength = 0
			}
		} else {
			fp.contentLength = 0
		}
	}

	// A response with neither ETag nor a usable length tells us nothing.
	if fp.etag == "" && fp.contentLength == 0 {
		return sourceFingerprint{}, false
	}
	return fp, true
}

// recordSourceFingerprint probes the upstream source and stores its fingerprint
// in status (best-effort). Called after a successful (re-)download so the next
// revalidation has a current baseline. A failed probe leaves the previous
// values untouched and is non-fatal.
func (r *ModelReconciler) recordSourceFingerprint(ctx context.Context, model *inferencev1alpha1.Model) {
	fp, ok := r.probeSource(ctx, model.Spec.Source)
	if !ok {
		return
	}
	model.Status.SourceETag = fp.etag
	model.Status.SourceContentLength = fp.contentLength
	revalidatedAt := metav1.Now()
	model.Status.LastRevalidated = &revalidatedAt
}

// removeCachedFiles deletes any cached *.gguf files in modelDir so a re-download
// can overwrite them. The migrateModelFilename helper refuses to clobber an
// existing canonical file, so a stale copy must be removed first.
func (r *ModelReconciler) removeCachedFiles(ctx context.Context, modelDir string) error {
	logger := log.FromContext(ctx)
	matches, err := filepath.Glob(filepath.Join(modelDir, "*"+modelFileExt))
	if err != nil {
		return fmt.Errorf("glob cached files in %s: %w", modelDir, err)
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale cached file %s: %w", m, err)
		}
		logger.Info("Removed stale cached model file for re-download", "path", m)
	}
	return nil
}

func probeLocalSource(source string) (sourceFingerprint, bool) {
	info, err := os.Stat(getLocalPath(source))
	if err != nil {
		return sourceFingerprint{}, false
	}
	return sourceFingerprint{
		contentLength: info.Size(),
		mtime:         info.ModTime(),
	}, true
}

// setSourceDrifted sets or clears the SourceDrifted condition in place. It is a
// no-op when the condition is already in the desired state, so it does not
// churn the condition's LastTransitionTime on every reconcile.
func (r *ModelReconciler) setSourceDrifted(model *inferencev1alpha1.Model, drifted bool) {
	want := metav1.ConditionFalse
	reason := "InSync"
	message := "Cached model matches the upstream source"
	if drifted {
		want = metav1.ConditionTrue
		reason = "UpstreamChanged"
		message = fmt.Sprintf("Upstream source %q differs from the cached copy", model.Spec.Source)
	}

	condition := metav1.Condition{
		Type:               ConditionSourceDrifted,
		Status:             want,
		ObservedGeneration: model.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
	for i, c := range model.Status.Conditions {
		if c.Type == ConditionSourceDrifted {
			if c.Status == want {
				return
			}
			model.Status.Conditions[i] = condition
			return
		}
	}
	model.Status.Conditions = append(model.Status.Conditions, condition)
}

// handleRevalidation runs a cadence-gated drift check against the upstream
// source for an already-Ready Model and persists any resulting status changes
// (LastRevalidated, recorded fingerprint, SourceDrifted condition) in a single
// write. It reports whether the caller should act on detected drift by
// re-downloading: that is true only when the policy is OnChange AND drift was
// detected. The returned requeueAfter schedules the next revalidation so drift
// is caught without an external trigger.
func (r *ModelReconciler) handleRevalidation(
	ctx context.Context, model *inferencev1alpha1.Model,
) (reDownload bool, requeueAfter time.Duration, err error) {
	logger := log.FromContext(ctx)

	outcome := r.revalidateSource(ctx, model)
	requeueAfter = r.revalidateInterval()

	switch outcome {
	case revalidateSkipped, revalidateIndeterminate:
		// Nothing to persist; leave any existing condition as-is.
		return false, requeueAfter, nil

	case revalidateBaseline, revalidateInSync:
		// In sync (or freshly baselined): ensure the drift condition is clear
		// and persist the updated LastRevalidated / baseline fields.
		r.setSourceDrifted(model, false)
		if statusErr := r.Status().Update(ctx, model); statusErr != nil {
			return false, requeueAfter, statusErr
		}
		return false, requeueAfter, nil

	case revalidateDrifted:
		r.setSourceDrifted(model, true)
		if statusErr := r.Status().Update(ctx, model); statusErr != nil {
			return false, requeueAfter, statusErr
		}
		if model.Spec.RefreshPolicy == RefreshPolicyOnChange {
			logger.Info("RefreshPolicy=OnChange and source drifted; will re-download", "source", model.Spec.Source)
			return true, requeueAfter, nil
		}
		logger.Info("Source drifted but RefreshPolicy is not OnChange; keeping cached file",
			"source", model.Spec.Source, "refreshPolicy", model.Spec.RefreshPolicy)
		return false, requeueAfter, nil

	default:
		return false, requeueAfter, nil
	}
}
