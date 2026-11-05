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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Model-transfer diagnosis. The sibling of the CUDA driver/runtime diagnosis in
// driver_compat.go, aimed at the containers that actually fail on a fresh
// deploy: the init containers that fetch the weights.
//
// The engine crashing is diagnosed there and deliberately scans only the
// runtime container. This scans only the init containers, for the same reason
// in reverse: a curl failure in the engine would be a different problem, and
// one failure must never collect two contradictory diagnoses.
//
// Without this, a transfer failure surfaces as a Pod wedged in Init with a
// non-zero exit code and nothing naming the cause. The operator already knows
// the cause; it is in the termination message, which every generated init
// container now reports (#1437).

const (
	// ConditionModelTransferHealthy reports whether the init containers that
	// stage model weights completed. Diagnostic only: it never feeds phase or
	// the Available condition. Absent until a failure is first diagnosed;
	// True once a subsequent pod gets past its init containers.
	ConditionModelTransferHealthy = "ModelTransferHealthy"

	// ReasonModelSourceUnauthorized: the source rejected our credentials
	// (401/403). Usually a missing or wrong sourceSecretRef, or a signature
	// the object store did not accept.
	ReasonModelSourceUnauthorized = "ModelSourceUnauthorized"

	// ReasonModelFileDigestMismatch: a multi-file staging run hashed a staged
	// file against spec.fileSha256 and rejected it. Distinct from
	// ReasonModelDigestMismatch so the remedy names the right field.
	ReasonModelFileDigestMismatch = "ModelFileDigestMismatch"

	// ReasonModelPVCDigestMismatch: a pre-staged pvc:// artifact failed its
	// spec.sha256 check at pod start. There is no rejection marker to clear.
	ReasonModelPVCDigestMismatch = "ModelPVCDigestMismatch"

	// ReasonModelSourceNotFound: the source returned 404. Usually a wrong
	// bucket, key, or file name in spec.source.
	ReasonModelSourceNotFound = "ModelSourceNotFound"

	// ReasonModelSourceUntrusted: TLS verification failed. Typically a private
	// endpoint signed by an internal CA that the downloader does not trust,
	// which is what --ca-cert-configmap exists to fix.
	ReasonModelSourceUntrusted = "ModelSourceUntrusted"

	// ReasonModelSourceUnreachable: DNS or connection failure reaching the
	// source. A wrong endpoint, a NetworkPolicy, or an egress-blocked cluster.
	ReasonModelSourceUnreachable = "ModelSourceUnreachable"

	// ReasonModelTransferOutOfSpace: the write ran out of room. Node
	// ephemeral storage, or an emptyDir sizeLimit smaller than the model.
	ReasonModelTransferOutOfSpace = "ModelTransferOutOfSpace"

	// ReasonModelCacheNotWritable: the cache volume refused a write the
	// downloader made (mkdir of the model's cache directory, or the download
	// itself). On a cache-backed pod the usual cause is that the prep was
	// turned off for storage that does not apply fsGroup, or a cache root the
	// downloader's uid does not own.
	ReasonModelCacheNotWritable = "ModelCacheNotWritable"

	// ReasonStagedFilesNotCurrent: a pod was built before the Model controller
	// recorded the glob expansion in status.stagedFiles, so the downloader
	// refused to stage a pattern it cannot expand. The cause is the Model's
	// status, not spec.files.
	ReasonStagedFilesNotCurrent = "StagedFilesNotCurrent"

	// ReasonModelCacheLockUnsupported: staging needs a cross-node lock that
	// the cache filesystem cannot provide. Warm cached files remain usable.
	ReasonModelCacheLockUnsupported = "ModelCacheLockUnsupported"

	// ReasonModelTransferSucceeded is set when ModelTransferHealthy flips back
	// to True: a pod has since got past its init containers.
	ReasonModelTransferSucceeded = "ModelTransferSucceeded"
)

// modelTransferSignature pairs a diagnosis with the substrings that justify it.
// Matching is case-insensitive, so entries must be lowercase.
//
// remedyFrom, when set, replaces remedy: it reads the remedy out of the
// container's own message, for a failure whose generated script already
// printed the remedy that applies to its source.
type modelTransferSignature struct {
	reason     string
	remedy     string
	remedyFrom func(msg string) string
	matches    []string
}

// modelTransferFailure is a diagnosed staging failure: the node it ran on, the
// reason, the line that justified it, and the remedy for the condition.
type modelTransferFailure struct {
	node   string
	reason string
	line   string
	remedy string
}

// modelTransferSignatures are matched in order, so the more specific entries
// come first. Every entry is a failure observed in practice; a signature that
// cannot be tied to a real message does not belong here, because a confident
// wrong label is worse than the bare exit code the user already has.
var modelTransferSignatures = []modelTransferSignature{
	{
		// The downloader printed the remedy for its own source kind; the
		// condition repeats that one, so the two cannot disagree (#2251).
		reason:     ReasonModelCacheLockUnsupported,
		remedyFrom: cacheLockRemedyIn,
		matches:    []string{"modelcachelockunsupported"},
	},
	{
		// A pod rendered to consume a prefetched cache cannot reach a staged
		// file (#2450). The token is unique to the model-verify container.
		reason: ReasonPrefetchedFilesUnreadable,
		remedy: "the pod's user cannot read a file the prefetch staged (the message names its uid, its groups and the blocking path's owner and mode); " +
			"the prefetch Job writes the cache in the operator's --default-fsgroup, which the operator adds to a prefetched pod's groups, " +
			"so a path it cannot read was written under another group (files staged before --default-fsgroup changed, or by something else): make it readable by the pod's groups",
		matches: []string{"prefetchedfilesunreadable"},
	},
	{
		// A pod rendered to consume a prefetched cache found a staged file
		// absent. The token is unique to the model-verify container.
		reason: ReasonPrefetchedFilesMissing,
		remedy: "a file the Model's prefetch should have staged is missing from the cache claim the pod mounts; " +
			"check the Model's status.prefetch entry for that claim and its Job's pod logs, and delete the Job to stage again",
		matches: []string{"prefetchedfilesmissing"},
	},
	{
		// A pod built before the Model recorded its glob expansion. The token
		// is unique to that message and lowercase, so it cannot shadow the
		// digest or curl entries below.
		reason: ReasonStagedFilesNotCurrent,
		remedy: "the Model has not recorded its spec.files glob expansion for the pod's generation; " +
			"check the Model's status (a failed repository listing is the usual cause), and once it is Ready the pod is rebuilt",
		matches: []string{"stagedfilesnotcurrent"},
	},
	{
		// A multi-file staging failure. Every per-file failure prints
		// "spec.fileSha256 mismatch", so this must be matched before the
		// single-file entry below, whose message it also contains. Match the
		// failure phrase, not the bare field name: success and keep lines
		// (stamp hit, kept pinned copy) name spec.fileSha256 too, and must not
		// turn a later shard's unrelated failure into a digest mismatch.
		reason: ReasonModelFileDigestMismatch,
		remedy: "spec.fileSha256 does not match the bytes at spec.source for at least one staged file; " +
			"correct the digest (or the source), then delete the cached <file>.<sha256>.sha256-rejected marker to retry",
		matches: []string{"spec.filesha256 mismatch"},
	},
	{
		// A pre-staged pvc:// artifact that fails spec.sha256 at pod start.
		// There is no marker on the read-only mount, so the remedy must not
		// tell the user to delete one.
		reason: ReasonModelPVCDigestMismatch,
		remedy: "spec.sha256 does not match the bytes on the mounted pvc:// volume; " +
			"correct the digest or re-stage the file",
		matches: []string{"for the pre-staged"},
	},
	{
		// The downloader's own spec.sha256 gate (#1965). Its message names
		// the expected and computed digests, and the marker it left behind,
		// so the operator can see the mismatch instead of only a pod wedged
		// in Init.
		reason: inferencev1alpha1.ReasonModelDigestMismatch,
		remedy: "spec.sha256 does not match the bytes at spec.source; correct the digest " +
			"(or the source), then delete the cached <file>.<sha256>.sha256-rejected marker to retry",
		matches: []string{"sha256 mismatch", "rejected against spec.sha256"},
	},
	{
		reason: ReasonModelSourceUntrusted,
		remedy: "the endpoint's CA is not trusted by the downloader; " +
			"publish it via the operator's --ca-cert-configmap (the ConfigMap must exist " +
			"in the workload's namespace, not the operator's)",
		// Both curl spellings: 8.x prints "SSL certificate OpenSSL verify
		// result: ...", older builds print "SSL certificate problem: ...".
		// Verified against curl 8.18 (the pinned init image) hitting a private
		// endpoint with no CA bundle.
		matches: []string{
			"ssl certificate problem",
			"ssl certificate openssl verify result",
			"unable to get local issuer certificate",
			"self signed certificate",
			"certificate verify failed",
		},
	},
	{
		reason: ReasonModelSourceUnauthorized,
		remedy: "check spec.sourceSecretRef and the credentials it names",
		matches: []string{
			"returned error: 401",
			"returned error: 403",
			"403 forbidden",
			"401 unauthorized",
			"signaturedoesnotmatch",
			"invalidaccesskeyid",
		},
	},
	{
		reason:  ReasonModelSourceNotFound,
		remedy:  "check spec.source: the bucket, key or file name does not exist at the endpoint",
		matches: []string{"returned error: 404", "404 not found", "nosuchkey", "nosuchbucket"},
	},
	{
		reason: ReasonModelTransferOutOfSpace,
		remedy: "raise spec.resources.ephemeralStorage (which also sizes the emptyDir) " +
			"or give the model cache a larger volume",
		matches: []string{"no space left on device", "disk quota exceeded"},
	},
	{
		reason: ReasonModelSourceUnreachable,
		remedy: "check the endpoint address, cluster DNS, and any NetworkPolicy or egress restriction",
		matches: []string{
			"could not resolve host",
			"failed to connect to",
			"connection refused",
			"connection timed out",
		},
	},
	{
		// Captured from curl 8.18.0 (the pinned init image) with a cache root
		// the downloader cannot write: "mkdir: can't create directory
		// '/models/<key>': Permission denied". A write failure inside curl
		// prints only "curl: (23) ... on write", which is also a full disk, so
		// that spelling is deliberately not matched on its own.
		//
		// It comes last so a more specific cause wins when a message carries
		// both. It only fires for a container that writes the cache
		// (writesModelCache): the prefetched serving pod mounts the same claim
		// read-only and this remedy does not apply to it.
		//
		// The diagnosis cannot tell from the pod whether the operator disabled
		// fsGroup (an OpenShift SCC injects one into the pod either way), so
		// the remedy names both causes: the prep switched off, and, with the
		// prep on and fsGroup off, an --init-container-user that is not the
		// init image's uid (#2204).
		reason: ReasonModelCacheNotWritable,
		remedy: "the downloader cannot write to the model cache. The cache-prep init container fixes ownership, " +
			"and it is off when the operator runs with --model-cache-prep=never (chart value modelCache.prep): " +
			"that setting is only right for storage that applies the pod fsGroup itself (a CSI driver with " +
			"fsGroupPolicy File). Either turn the prep back on (modelCache.prep: always) or chown the cache root " +
			"to the downloader's uid, for example with `llmkube cache prep`. If the prep is already on and the " +
			"operator default fsGroup is disabled (--default-fsgroup=0, the OpenShift preset), the prep chowns the " +
			"root to --init-container-user (chart value controllerManager.initContainer.user, default 101) unless " +
			"spec.podSecurityContext.runAsUser is set: set that flag to the uid --init-container-image runs as",
		matches: []string{"permission denied"},
	},
}

// matchModelTransferFailure classifies an init container's termination message.
// Returns the reason, the (sanitized, length-capped) line that justified it,
// and whether anything matched at all.
//
// Reuses driver_compat.go's asciiLower and sanitizeMessageLine: the input is
// arbitrary container output, so offsets must stay byte-stable and the result
// must be bounded no matter what was printed.
func matchModelTransferFailure(msg string) (modelTransferFailure, bool) {
	if msg == "" {
		return modelTransferFailure{}, false
	}
	lower := asciiLower(msg)
	for _, sig := range modelTransferSignatures {
		for _, m := range sig.matches {
			idx := strings.Index(lower, m)
			if idx < 0 {
				continue
			}
			// Report the whole line the match sits on, not the fragment: the
			// surrounding text is what makes the diagnosis actionable (which
			// URL, which host, which file).
			start := strings.LastIndexByte(msg[:idx], '\n') + 1
			end := strings.IndexByte(msg[idx:], '\n')
			if end < 0 {
				end = len(msg)
			} else {
				end += idx
			}
			remedy := sig.remedy
			if sig.remedyFrom != nil {
				remedy = sig.remedyFrom(msg)
			}
			return modelTransferFailure{reason: sig.reason, line: sanitizeMessageLine(msg[start:end]), remedy: remedy}, true
		}
	}
	return modelTransferFailure{}, false
}

// isGeneratedInitContainer reports whether a container name is one the operator
// generates for model staging. Scoped by name rather than by position so an
// init container injected by a mesh or a policy controller is never diagnosed
// as our transfer failing.
func isGeneratedInitContainer(name string) bool {
	return name == "model-downloader" || name == "model-cache-prep" || name == "model-verify"
}

// findModelTransferFailure scans pods for a generated init container that
// terminated non-zero with a recognisable cause. Both the current terminated
// state (fresh failure, before backoff) and the last terminated state
// (CrashLoopBackOff) are checked, mirroring findCudaDriverCrash.
func findModelTransferFailure(pods []corev1.Pod) (modelTransferFailure, bool) {
	for i := range pods {
		pod := &pods[i]
		// One pod-level answer, reused per container: does anything here
		// mount a model cache at all.
		cacheBacked := podMountsModelCache(pod)
		for j := range pod.Status.InitContainerStatuses {
			cs := &pod.Status.InitContainerStatuses[j]
			if !isGeneratedInitContainer(cs.Name) {
				continue
			}
			// A container whose CURRENT termination is success has staged its
			// weights. LastTerminationState still holds any earlier failed attempt
			// for the life of the pod, so consulting it here would pin the
			// condition False forever after a retry that succeeded (#1536).
			if cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0 {
				continue
			}
			for _, term := range []*corev1.ContainerStateTerminated{
				cs.State.Terminated,
				cs.LastTerminationState.Terminated,
			} {
				if term == nil || term.ExitCode == 0 {
					continue
				}
				if f, ok := matchModelTransferFailure(term.Message); ok {
					// The cache remedy needs both: a container that writes
					// the cache (model-verify only reads it) and a pod that
					// mounts one (an emptyDir store has no cache-prep to
					// turn on).
					if f.reason == ReasonModelCacheNotWritable && !(writesModelCache(cs.Name) && cacheBacked) {
						continue
					}
					f.node = pod.Spec.NodeName
					return f, true
				}
			}
		}
	}
	return modelTransferFailure{}, false
}

// podMountsModelCache reports whether the pod mounts the model-cache PVC, the
// only mount a cache-prep init container ever prepared.
func podMountsModelCache(pod *corev1.Pod) bool {
	return cacheClaimName(&pod.Spec) != ""
}

// writesModelCache reports whether a generated init container writes the model
// cache, so a cache-write failure can only come from it. model-verify is the
// exception among the containers that mount the cache: it runs on the
// read-only prefetched mount and never creates anything. The draft-prefixed
// names are absent because isGeneratedInitContainer filters them out before
// this is consulted.
func writesModelCache(name string) bool {
	return name == "model-cache-prep" || name == "model-downloader"
}

// initContainersCompleted reports whether any pod has got past every generated
// init container. The recovery gate: weights staged successfully somewhere is
// the only observation that justifies clearing the diagnosis.
func initContainersCompleted(pods []corev1.Pod) bool {
	for i := range pods {
		seen, ok := false, true
		for _, cs := range pods[i].Status.InitContainerStatuses {
			if !isGeneratedInitContainer(cs.Name) {
				continue
			}
			seen = true
			if cs.State.Terminated == nil || cs.State.Terminated.ExitCode != 0 {
				ok = false
			}
		}
		if seen && ok {
			return true
		}
	}
	return false
}

// reconcileModelTransferCondition diagnoses model-staging failures onto the
// InferenceService. Mirrors reconcileDriverCompatCondition, including its
// transition-only recovery: pods merely disappearing never asserts health we
// have not observed.
func (r *InferenceServiceReconciler) reconcileModelTransferCondition(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService,
) {
	log := logf.FromContext(ctx)

	podList := &corev1.PodList{}
	labels := client.MatchingLabels{
		"app":                           isvc.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}
	if err := r.List(ctx, podList, client.InNamespace(isvc.Namespace), labels); err != nil {
		log.Error(err, "Failed to list pods for model-transfer diagnosis")
		return
	}

	now := metav1.NewTime(time.Now())
	failure, found := findModelTransferFailure(podList.Items)

	if !found {
		existing := meta.FindStatusCondition(isvc.Status.Conditions, ConditionModelTransferHealthy)
		if existing == nil || existing.Status != metav1.ConditionFalse {
			return
		}
		if !initContainersCompleted(podList.Items) {
			return
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type:               ConditionModelTransferHealthy,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             ReasonModelTransferSucceeded,
			Message:            "Model weights staged successfully",
		})
		return
	}

	reason := failure.reason
	message := fmt.Sprintf("Model transfer failed: %s", failure.line)
	if failure.node != "" {
		message += fmt.Sprintf(" (node %s)", failure.node)
	}
	if failure.remedy != "" {
		message += ". " + failure.remedy
	}

	existing := meta.FindStatusCondition(isvc.Status.Conditions, ConditionModelTransferHealthy)
	changed := existing == nil || existing.Status != metav1.ConditionFalse || existing.Reason != reason
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               ConditionModelTransferHealthy,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: isvc.Generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})
	if changed && r.Recorder != nil {
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, reason, "Reconcile", "%s", message)
	}
}
