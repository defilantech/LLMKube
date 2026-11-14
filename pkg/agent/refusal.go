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

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/agent/policy"
)

// Event reasons for starts the agent refuses before (or instead of) serving.
// These equal the api/v1alpha1.Reason* constants of the same name: that
// package is the single source of truth MetalAgentRefusalReasons draws from,
// which internal/controller/scheduling.go's agentRefusalReasons set is built
// from in turn, so the agent and the controller can never drift onto two
// different lists of refusal reasons again. That drift once left a refusal
// the controller did not recognize: it cleared the status every poll and the
// agent refused again every poll, in a loop.
const (
	EventReasonEndpointNameConflict   = inferencev1alpha1.ReasonEndpointNameConflict
	EventReasonModelSourceNotAllowed  = inferencev1alpha1.ReasonModelSourceNotAllowed
	EventReasonExtraArgsRejected      = inferencev1alpha1.ReasonExtraArgsRejected
	EventReasonServiceNameTooLong     = inferencev1alpha1.ReasonServiceNameTooLong
	EventReasonModelDigestMismatch    = inferencev1alpha1.ReasonModelDigestMismatch
	EventReasonModelDownloadTooLarge  = inferencev1alpha1.ReasonModelDownloadTooLarge
	EventReasonModelDownloadNoSpace   = inferencev1alpha1.ReasonModelDownloadNoSpace
	EventReasonSourceSecretNotAllowed = inferencev1alpha1.ReasonSourceSecretNotAllowed
	EventReasonRuntimeNotAvailable    = inferencev1alpha1.ReasonRuntimeNotAvailable
)

// runtimeNotAvailableMessage is the refusal message for an InferenceService
// whose resolved runtime has no registered executor.
func runtimeNotAvailableMessage(runtime string) string {
	if runtime == runtimeLlamaServer || runtime == runtimeLlamaCPP {
		return fmt.Sprintf("no executor registered for runtime %q: this agent has no llama.cpp executor because "+
			"llama-server was not found when it started; install llama.cpp or pass "+
			"--llama-server=/path/to/llama-server and restart the agent, or set spec.runtime to an engine "+
			"this agent serves", runtime)
	}
	return fmt.Sprintf("no executor registered for runtime %q: this agent was started without that engine; "+
		"install it, pass its binary flag or select it with --runtime, and restart the agent, or set "+
		"spec.runtime to an engine this agent serves", runtime)
}

// refuseStart records why the agent will not serve an InferenceService: it
// writes the agent-owned scheduling status fields (the same surface memory
// admission uses, cleared by the next admitted start), emits a Warning Event
// unless this repeats the refusal already recorded for the service, and
// schedules a re-check (publishRefusal), then returns an error so
// ensureProcess withdraws any endpoint. It must not be called while holding
// a.mu (emitInferenceEvent does I/O).
func (a *MetalAgent) refuseStart(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	reason, message string,
) error {
	a.logger.Warnw("refusing to serve inference service",
		"namespace", isvc.Namespace, "name", isvc.Name, "reason", reason, "message", message)
	prev := statusBeforeRefusal(isvc)
	isvc.Status.SchedulingStatus = reason
	isvc.Status.SchedulingMessage = message
	if a.publishRefusal(ctx, isvc, prev) {
		a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: isvc.Namespace, Name: isvc.Name},
			corev1.EventTypeWarning, reason, "%s", message)
	}
	return fmt.Errorf("%s: %s", reason, message)
}

// handleStartProcessError turns a StartProcess failure into the error
// reconcileProcess returns. A *ModelDigestMismatchError is remembered in
// a.digestMismatches (so checkModelDigestMemo can refuse the next reconcile
// without downloading again) and refused through refuseStart, the same way
// an endpoint-name conflict or a bad model path is refused, so it is visible
// as a status field and a Warning Event instead of only a log line. A
// *ModelDownloadTooLargeError (a download over --max-download-size) is
// remembered and refused the same way, as ModelDownloadTooLarge: without the
// memo every reconcile would download up to the cap again. A
// *ModelCacheEntryNotRegularError (a symlink in a downloaded source's cache
// slot that appeared after checkModelCacheSlot ran) is refused the same way
// as ModelSourceNotAllowed. A *SourceSecretNotAllowedError (a sourceSecretRef
// Secret that lost its marker or was deleted after checkSourceSecret ran) is
// refused as SourceSecretNotAllowed. A transient failure to read the
// sourceSecretRef Secret is retried on the next poll. Every other StartProcess
// failure (an engine that exits while loading, a download that fails on a
// full disk or a bad status, a health-check timeout, ...) is not a refusal: it
// is reported as StartFailed and retried on the restart backoff (failStart,
// #2404). Split out of reconcileProcess, rather than inlined as an extra
// branch there, to keep its cyclomatic complexity from crossing the linter's
// threshold.
func (a *MetalAgent) handleStartProcessError(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, startErr error,
) error {
	var digestErr *ModelDigestMismatchError
	if errors.As(startErr, &digestErr) {
		a.digestMismatches.record(
			types.NamespacedName{Namespace: model.Namespace, Name: model.Name},
			model.Spec.Source, model.Spec.SHA256, digestErr,
		)
		return a.refuseStart(ctx, isvc, EventReasonModelDigestMismatch, digestErr.Error())
	}
	var tooLargeErr *ModelDownloadTooLargeError
	if errors.As(startErr, &tooLargeErr) {
		a.digestMismatches.record(
			types.NamespacedName{Namespace: model.Namespace, Name: model.Name},
			model.Spec.Source, model.Spec.SHA256, tooLargeErr,
		)
		return a.refuseStart(ctx, isvc, EventReasonModelDownloadTooLarge, tooLargeErr.Error())
	}
	// Not memoized: freeing disk space writes nothing to the cluster, so the
	// refusal re-check (publishRefusal) is what retries it.
	var noSpaceErr *ModelDownloadNoSpaceError
	if errors.As(startErr, &noSpaceErr) {
		return a.refuseStart(ctx, isvc, EventReasonModelDownloadNoSpace, noSpaceErr.Error())
	}
	var slotErr *ModelCacheEntryNotRegularError
	if errors.As(startErr, &slotErr) {
		return a.refuseStart(ctx, isvc, EventReasonModelSourceNotAllowed, slotErr.Error())
	}
	var secretErr *SourceSecretNotAllowedError
	if errors.As(startErr, &secretErr) {
		return a.refuseStart(ctx, isvc, EventReasonSourceSecretNotAllowed, secretErr.Error())
	}
	// A download that could not read its sourceSecretRef has not fetched
	// anything yet, so retrying the start on the next poll costs one more
	// read. Any other failure is retried on the restart backoff instead: a
	// retry may download or load the model again.
	var readErr *sourceSecretReadError
	if errors.As(startErr, &readErr) {
		a.requeue(types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}.String())
		return fmt.Errorf("failed to start process: %w", startErr)
	}
	eventReason := EventReasonStartFailed
	if errors.Is(startErr, errEngineRecordWrite) {
		eventReason = EventReasonEngineRecordFailed
	}
	a.failStart(ctx, isvc, startErr, eventReason)
	return fmt.Errorf("failed to start process: %w", startErr)
}

// checkModelPreflight runs the Model-derived pre-flight refusal checks that
// must all happen before memory admission (checkModelPaths, then
// checkSourceSecret, then checkModelCacheSlot, then checkModelDigestMemo),
// returning the first refusal. Combined into one call from reconcileProcess, rather than
// sequential `if err != nil { return err }` blocks there, purely to keep that
// function's cyclomatic complexity under the linter's threshold; see
// handleStartProcessError's doc comment for the same reasoning applied to the
// StartProcess error path.
func (a *MetalAgent) checkModelPreflight(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model,
	runtime, pagedSSDCacheDir string,
) error {
	if err := a.checkModelPaths(ctx, isvc, model, pagedSSDCacheDir); err != nil {
		return err
	}
	if err := a.checkSourceSecret(ctx, isvc, model); err != nil {
		return err
	}
	if err := a.checkModelCacheSlot(ctx, isvc, model, runtime); err != nil {
		return err
	}
	return a.checkModelDigestMemo(ctx, isvc, model)
}

// checkModelCacheSlot refuses, before memory admission, a llama-server start
// whose downloaded source's cache slot is not a regular file (a symlink
// above all). ensureModel refuses the same slot inside StartProcess, but
// that runs after admission, whose success path clears SchedulingStatus, so
// relying on it alone would flap the status on every reconcile. Only the
// llama-server executor caches downloads in that slot, and a local source
// is loaded in place, so both are left alone.
func (a *MetalAgent) checkModelCacheSlot(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, runtime string,
) error {
	if runtime != runtimeLlamaServer && runtime != runtimeLlamaCPP {
		return nil
	}
	if isLocalModelSource(model.Spec.Source) {
		return nil
	}
	slot := modelCacheSlot(a.config.ModelStorePath, model.Name, model.Spec.Source)
	if err := checkModelCacheSlot(slot); err != nil {
		return a.refuseStart(ctx, isvc, EventReasonModelSourceNotAllowed, err.Error())
	}
	return nil
}

// checkModelDigestMemo refuses a Model this agent already found to have a
// mismatching SHA256, or a download over --max-download-size, for its CURRENT
// spec.source and spec.sha256, without letting StartProcess download it
// again: the memo (a.digestMismatches) was populated by
// handleStartProcessError on a prior reconcile and self-invalidates the
// moment source or sha256 changes (digestMismatchMemo.check), so an edited
// Model always gets a fresh download attempt. A no-op when the Model has no
// recorded refusal. A Model without spec.sha256 is checked too, since an
// over-cap download can be recorded for one (a digest mismatch cannot).
func (a *MetalAgent) checkModelDigestMemo(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model,
) error {
	modelKey := types.NamespacedName{Namespace: model.Namespace, Name: model.Name}
	memoErr := a.digestMismatches.check(modelKey, model.Spec.Source, model.Spec.SHA256)
	var tooLargeErr *ModelDownloadTooLargeError
	switch {
	case memoErr == nil:
		return nil
	case errors.As(memoErr, &tooLargeErr):
		return a.refuseStart(ctx, isvc, EventReasonModelDownloadTooLarge, memoErr.Error())
	default:
		return a.refuseStart(ctx, isvc, EventReasonModelDigestMismatch, memoErr.Error())
	}
}

// checkModelPaths refuses a Model the controller marked Failed and any local
// model source or pagedSSDCacheDir that resolves outside the allowed roots.
// Remote sources are downloaded into the model store, which is always inside
// a root, so they are not checked here.
func (a *MetalAgent) checkModelPaths(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	pagedSSDCacheDir string,
) error {
	if model.Status.Phase == inferencev1alpha1.PhaseFailed {
		return a.refuseStart(ctx, isvc, EventReasonModelSourceNotAllowed,
			fmt.Sprintf("Model %s is Failed (%s); the agent does not serve a Model the controller rejected",
				model.Name, modelFailureReason(model)))
	}
	// A ".." path segment is refused regardless of scheme. mlx-server,
	// vllm-swift and tensorfold's resolveModelPath all filepath.Join a
	// non-absolute ModelSource onto the model store, including one WITH a
	// scheme (only an absolute local path is left as-is), so e.g.
	// "https://../../../../Users/victim/x" or "hf://../../../x" walks the
	// joined path back out of the store. The CRD's Source pattern allows any
	// character after a scheme, and the controller marks a remote-looking
	// source Ready, so neither the pattern nor the Failed check above catches
	// this; check the literal segments here, before the scheme-aware branches
	// below (which still handle a symlink escape with no ".." at all, but only
	// for local and scheme-less sources; a remote-scheme source is not
	// path-checked at all beyond this ".." scan).
	for _, seg := range strings.Split(model.Spec.Source, "/") {
		if seg == ".." {
			return a.refuseStart(ctx, isvc, EventReasonModelSourceNotAllowed,
				fmt.Sprintf("model source %q contains a %q path segment; the agent refuses it because "+
					"some runtimes join sources onto the model store", model.Spec.Source, ".."))
		}
	}
	workDir := a.config.ModelStorePath
	if isLocalModelSource(model.Spec.Source) {
		if err := a.roots.CheckPath("model path", localSourcePath(model.Spec.Source), workDir, a.home); err != nil {
			return a.refuseStart(ctx, isvc, EventReasonModelSourceNotAllowed, err.Error())
		}
	} else if !strings.Contains(model.Spec.Source, "://") {
		// A scheme-less source with no leading "/" is neither a URL
		// (http(s)/hf/s3/pvc/oci/file) nor an absolute local path: it is a
		// relative value such as an owner/repo id, which every executor's
		// resolveModelPath joins onto the model store. filepath.Join cleans
		// ".." out of the joined path, so an unchecked value like
		// "a/../../../../etc/passwd" would resolve outside the store. Run it
		// through the same root check as a local source; ResolvePath joins it
		// onto workDir, refuses any ".." component, and resolves symlinks, so
		// a plain repo id that does not exist yet still resolves under the
		// store and passes.
		if err := a.roots.CheckPath("model path", model.Spec.Source, workDir, a.home); err != nil {
			return a.refuseStart(ctx, isvc, EventReasonModelSourceNotAllowed, err.Error())
		}
	}
	if pagedSSDCacheDir != "" {
		if err := a.roots.CheckPath("pagedSSDCacheDir", pagedSSDCacheDir, workDir, a.home); err != nil {
			return a.refuseStart(ctx, isvc, EventReasonModelSourceNotAllowed, err.Error())
		}
	}
	return nil
}

// checkExtraArgs applies the extraArgs policy for runtime and refuses the
// start on a violation. It is a no-op when args is empty. Called from
// reconcileProcess right after checkModelPaths, before checkMemoryAdmission,
// for the same reason checkModelPaths runs there: memory admission's success
// path clears SchedulingStatus, so a refusal check placed after it would flap
// the status on every watch poll of an otherwise-admittable spec.
func (a *MetalAgent) checkExtraArgs(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	runtime string,
	args []string,
) error {
	if len(args) == 0 {
		return nil
	}
	err := policy.CheckExtraArgs(policy.ArgsInput{
		Runtime:     runtime,
		Args:        args,
		Roots:       a.roots,
		WorkDir:     a.config.ModelStorePath,
		Home:        a.home,
		AllowUnsafe: a.config.AllowUnsafeExtraArgs,
	})
	if err != nil {
		return a.refuseStart(ctx, isvc, EventReasonExtraArgsRejected, err.Error())
	}
	return nil
}

// modelFailureReason returns the reason of the Model's Degraded condition, or
// "no reason recorded". The literal "Degraded" matches the controller's
// ConditionDegraded (internal/controller/model_controller.go), which
// pkg/agent cannot import.
func modelFailureReason(model *inferencev1alpha1.Model) string {
	for _, c := range model.Status.Conditions {
		if c.Type == "Degraded" && c.Status == metav1.ConditionTrue && c.Reason != "" {
			return c.Reason
		}
	}
	return "no reason recorded"
}
