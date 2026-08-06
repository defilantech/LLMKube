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
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// CUDA driver/runtime mismatch diagnosis. A serving image bundles a CUDA
// userspace; the node's kernel driver supports CUDA up to some version. When
// the image's CUDA major is newer than the driver's, the engine dies at init
// — after a GPU node has been provisioned and a multi-GB image pulled — with
// the real error buried in the crashed container's logs. This file turns that
// termination message into a named diagnosis on the InferenceService, the
// same move getPodSchedulingInfo makes for Insufficient-GPU pod conditions.

const (
	// ConditionDriverCompatible reports whether the node's GPU driver
	// satisfied the CUDA userspace bundled in the runtime image, as observed
	// from actual container terminations. Diagnostic only: it never feeds
	// phase or the Available condition. Absent until a mismatch is first
	// diagnosed; True once the runtime container subsequently starts (proof
	// CUDA init succeeded).
	ConditionDriverCompatible = "DriverCompatible"

	// ReasonCUDADriverInsufficient is set when DriverCompatible=False because
	// the runtime container terminated with a CUDA driver/runtime version
	// incompatibility. Also the reason of the Warning event emitted on that
	// transition, so one string serves kubectl field-selectors and condition
	// queries alike.
	ReasonCUDADriverInsufficient = "CUDADriverInsufficient"

	// ReasonRuntimeStarted is set when DriverCompatible flips back to True: a
	// previously diagnosed service now has a ready runtime container, so CUDA
	// initialization against the node driver succeeded.
	ReasonRuntimeStarted = "RuntimeStarted"
)

// cudaDriverMismatchSignatures are substrings CUDA-based runtimes print when
// the node's kernel driver is older than the CUDA userspace in the image:
// PyTorch's wording ("The NVIDIA driver on your system is too old (found
// version 12040)"), the CUDA runtime's canonical error string (also what
// llama.cpp's ggml_cuda_init surfaces via cudaGetErrorString), and both
// error-enum spellings (runtime API camelCase, driver API underscores).
// Matching is case-insensitive; entries must be lowercase. Deliberately
// absent: "Driver/library version mismatch" (NVML), which is node-internal
// driver/userspace skew after an in-place driver change, not an
// image-vs-driver incompatibility — diagnosing it with this message would
// misdirect the fix.
var cudaDriverMismatchSignatures = []string{
	"driver on your system is too old",
	"cuda driver version is insufficient",
	"cudaerrorinsufficientdriver",
	"cuda_error_insufficient_driver",
}

// asciiLower lowercases ASCII letters only, preserving byte length and
// offsets. strings.ToLower is unusable here: Unicode simple case mapping can
// change byte length (e.g. U+023A lowercases from 2 to 3 bytes), so an index
// found in its output does not address the input — and the input is arbitrary
// container output, which must never be able to panic the controller. The
// signatures are pure ASCII, so ASCII folding is sufficient.
func asciiLower(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// sanitizeMessageLine trims the line, replaces control characters with
// spaces, and caps its length on a rune boundary, so the condition message
// and the (quoted, length-limited) event note stay valid and bounded no
// matter what the container printed.
func sanitizeMessageLine(line string) string {
	line = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.TrimSpace(line))
	const maxLine = 200
	if len(line) <= maxLine {
		return line
	}
	cut := maxLine
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + "..."
}

// matchCudaDriverMismatch reports whether msg contains a known CUDA
// driver-too-old signature, returning the (sanitized, length-capped) line
// that matched for inclusion in the condition message.
func matchCudaDriverMismatch(msg string) (string, bool) {
	if msg == "" {
		return "", false
	}
	lower := asciiLower(msg)
	for _, sig := range cudaDriverMismatchSignatures {
		idx := strings.Index(lower, sig)
		if idx < 0 {
			continue
		}
		// Report the full line containing the signature so the condition
		// carries the runtime's own words (e.g. the found-version number).
		start := strings.LastIndexByte(msg[:idx], '\n') + 1
		end := strings.IndexByte(msg[idx:], '\n')
		if end < 0 {
			end = len(msg)
		} else {
			end += idx
		}
		return sanitizeMessageLine(msg[start:end]), true
	}
	return "", false
}

// findCudaDriverCrash scans the service's pods for a runtime container that
// terminated with a CUDA driver/runtime incompatibility. Only the container
// named containerName is inspected: init containers (model download, cache
// prep) and sidecars never run the engine, and a signature match there would
// be a different problem. Both the current terminated state (fresh crash,
// before backoff) and the last terminated state (CrashLoopBackOff) are
// checked. Returns the node the pod ran on and the matched message line.
func findCudaDriverCrash(pods []corev1.Pod, containerName string) (nodeName, matchedLine string, found bool) {
	for i := range pods {
		pod := &pods[i]
		for j := range pod.Status.ContainerStatuses {
			cs := &pod.Status.ContainerStatuses[j]
			if cs.Name != containerName {
				continue
			}
			for _, term := range []*corev1.ContainerStateTerminated{
				cs.State.Terminated,
				cs.LastTerminationState.Terminated,
			} {
				if term == nil || term.ExitCode == 0 {
					continue
				}
				if line, ok := matchCudaDriverMismatch(term.Message); ok {
					return pod.Spec.NodeName, line, true
				}
			}
		}
	}
	return "", "", false
}

// runtimeContainerReady reports whether any pod's runtime container is Ready.
// Used as the recovery gate: a ready runtime container means CUDA init
// succeeded against the node driver, which is the only observation that
// justifies flipping DriverCompatible back to True.
func runtimeContainerReady(pods []corev1.Pod, containerName string) bool {
	for i := range pods {
		for _, cs := range pods[i].Status.ContainerStatuses {
			if cs.Name == containerName && cs.Ready {
				return true
			}
		}
	}
	return false
}

// gfdCudaOffer extracts the node's supported CUDA version and driver version
// from gpu-feature-discovery labels, preferring the current label names and
// falling back to the deprecated pre-GFD-0.15 forms. Empty strings when the
// node carries no GFD labels (clusters without gpu-operator/GFD).
func gfdCudaOffer(labels map[string]string) (cudaMajor, cudaMinor, driverVersion string) {
	cudaMajor = labels["nvidia.com/cuda.runtime-version.major"]
	cudaMinor = labels["nvidia.com/cuda.runtime-version.minor"]
	if cudaMajor == "" {
		cudaMajor = labels["nvidia.com/cuda.runtime.major"]
		cudaMinor = labels["nvidia.com/cuda.runtime.minor"]
	}
	driverVersion = labels["nvidia.com/cuda.driver-version.full"]
	if driverVersion == "" {
		if major := labels["nvidia.com/cuda.driver.major"]; major != "" {
			driverVersion = major
			if minor := labels["nvidia.com/cuda.driver.minor"]; minor != "" {
				driverVersion += "." + minor
				if rev := labels["nvidia.com/cuda.driver.rev"]; rev != "" {
					driverVersion += "." + rev
				}
			}
		}
	}
	return cudaMajor, cudaMinor, driverVersion
}

// reconcileDriverCompatCondition maintains the DriverCompatible condition
// from observed pod terminations. Never touches phase, Available, or
// scheduling status; failures to list/get only log (a diagnosis must never
// fail a reconcile).
func (r *InferenceServiceReconciler) reconcileDriverCompatCondition(ctx context.Context, isvc *inferencev1alpha1.InferenceService) {
	log := logf.FromContext(ctx)

	podList := &corev1.PodList{}
	labels := client.MatchingLabels{
		"app":                           isvc.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}
	if err := r.List(ctx, podList, client.InNamespace(isvc.Namespace), labels); err != nil {
		log.Error(err, "Failed to list pods for driver-compat diagnosis")
		return
	}

	containerName := resolveBackend(isvc).ContainerName()
	now := metav1.NewTime(time.Now())

	// A Ready runtime container is checked before crash evidence: CUDA init
	// succeeding now outranks a stale signature that lingers in a pod's
	// LastTerminationState for its whole lifetime (e.g. a container that
	// crashed before the node's driver daemonset finished, then started in
	// place) or in an old pod still draining during a rollout. Without this
	// ordering the diagnosis would stick at False forever on a serving pod.
	ready := runtimeContainerReady(podList.Items, containerName)
	nodeName, matchedLine, found := findCudaDriverCrash(podList.Items, containerName)

	if ready || !found {
		// Recovery follows the transition-only convention (cf.
		// reconcileVLLMSpecCondition): flip to True only when a prior
		// diagnosis exists AND the runtime container has demonstrably
		// started. Pods merely gone, or failing some other way, keep the
		// last diagnosis in place rather than asserting compatibility we
		// have not observed.
		existing := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDriverCompatible)
		if existing == nil || existing.Status != metav1.ConditionFalse {
			return
		}
		if !ready {
			return
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type:               ConditionDriverCompatible,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             ReasonRuntimeStarted,
			Message:            "Runtime container started; CUDA initialization succeeded against the node driver",
		})
		return
	}

	message := r.buildDriverMismatchMessage(ctx, nodeName, matchedLine)
	transitioned := !meta.IsStatusConditionFalse(isvc.Status.Conditions, ConditionDriverCompatible)
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               ConditionDriverCompatible,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: isvc.Generation,
		LastTransitionTime: now,
		Reason:             ReasonCUDADriverInsufficient,
		Message:            message,
	})
	// Warning event on transition only, so a crash loop emits one event per
	// episode instead of one per restart.
	if transitioned && r.Recorder != nil {
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, ReasonCUDADriverInsufficient, "Reconcile", "%s", message)
	}
}

// buildDriverMismatchMessage assembles the condition/event message from the
// matched termination line plus, when readable, the node's GFD-reported
// driver and CUDA support. One targeted node Get, no fleet listing: the
// diagnosis names the node the crash actually happened on. Guidance stays at
// the constraint level (which CUDA major to build for) and never recommends a
// specific image version.
func (r *InferenceServiceReconciler) buildDriverMismatchMessage(ctx context.Context, nodeName, matchedLine string) string {
	base := fmt.Sprintf("Runtime container terminated with a CUDA driver/runtime incompatibility: %q. "+
		"The image's CUDA userspace requires a newer GPU driver than the node provides.", matchedLine)

	if nodeName == "" {
		return base + " Use an image whose CUDA major matches the node's driver support, or schedule onto nodes with a newer driver."
	}

	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		// The node may already be gone (autoscaler scale-down); the error
		// text alone still names the failure class.
		return base + fmt.Sprintf(" Pod ran on node %q (no longer readable). "+
			"Use an image whose CUDA major matches that node pool's driver support, or schedule onto nodes with a newer driver.", nodeName)
	}

	cudaMajor, cudaMinor, driverVersion := gfdCudaOffer(node.Labels)
	if cudaMajor == "" {
		return base + fmt.Sprintf(" Pod ran on node %q, which carries no GPU feature labels to report its supported CUDA version. "+
			"Use an image whose CUDA major matches the node's driver support, or schedule onto nodes with a newer driver.", nodeName)
	}

	supported := cudaMajor
	if cudaMinor != "" {
		supported += "." + cudaMinor
	}
	detail := fmt.Sprintf(" Node %q supports CUDA <= %s", nodeName, supported)
	if driverVersion != "" {
		detail += fmt.Sprintf(" (driver %s)", driverVersion)
	}
	return base + detail + fmt.Sprintf(". Use an image built for CUDA %s.x, or schedule onto nodes with a newer driver.", cudaMajor)
}
