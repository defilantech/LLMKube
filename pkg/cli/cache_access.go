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

package cli

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// defaultCacheAccessImage is the image both cache-access pods run when
	// nothing sets one.
	defaultCacheAccessImage = "busybox:1.37.0"
	// cacheImageEnv sets the cache-access pod image for every cache command.
	cacheImageEnv = "LLMKUBE_CACHE_IMAGE"
)

// cacheAccessImage is the image the --cache-image flag selected for this run;
// empty when the flag did not set one.
var cacheAccessImage string

// cacheAccessImageName resolves the cache-access pod image: the flag, then
// LLMKUBE_CACHE_IMAGE, then the busybox default. Both the inspector and the
// purge pod read it here, so an air-gapped mirror reaches all three commands.
func cacheAccessImageName() string {
	if cacheAccessImage != "" {
		return cacheAccessImage
	}
	if image := os.Getenv(cacheImageEnv); image != "" {
		return image
	}
	return defaultCacheAccessImage
}

// withCacheImage runs fn with image as the cache-access pod image, restoring
// the previous setting afterwards.
func withCacheImage(image string, fn func() error) error {
	previous := cacheAccessImage
	cacheAccessImage = image
	defer func() { cacheAccessImage = previous }()
	return fn()
}

// readCacheClaim reads a cache claim. A claim that cannot be read (including a
// forbidden one) yields nil, and the access mode counts as unknown: an
// unpinned strict claim stalls on Multi-Attach, so the pre-#2155 pin is the
// safer of the two.
func readCacheClaim(
	ctx context.Context, k8sClient client.Client, namespace, pvcName string,
) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: namespace}, pvc); err != nil {
		return nil
	}
	return pvc
}

// claimShareable reports whether claim is attached wherever it is needed. Any
// ReadWriteMany or ReadOnlyMany mode makes it shareable, so no pod's node
// holds it, mirroring singleWriterClaimHolder in internal/controller.
func claimShareable(pvc *corev1.PersistentVolumeClaim) bool {
	if pvc == nil {
		return false
	}
	for _, mode := range pvc.Spec.AccessModes {
		switch mode {
		case corev1.ReadWriteMany, corev1.ReadOnlyMany:
			return true
		}
	}
	return false
}

// claimSinglePod reports whether claim allows exactly one pod, so a second
// mount never succeeds while another pod holds it.
func claimSinglePod(pvc *corev1.PersistentVolumeClaim) bool {
	if pvc == nil {
		return false
	}
	for _, mode := range pvc.Spec.AccessModes {
		if mode == corev1.ReadWriteOncePod {
			return true
		}
	}
	return false
}

// secondMountBlocker returns the error to fail with when another pod holds a
// ReadWriteOncePod claim and will keep holding it, or nil when the run may
// create its pod; singlePod is the caller's answer for the claim
// (purgeTarget.SinglePod on the purge path, the live read on the inspector
// path). releasing reports a holder whose own delete is already underway, such
// as the serving pod a `delete --purge-cache` is tearing down.
//
// A pod that is being deleted releases the claim on its own, as does the pod
// this run replaces at ownPod, so waiting for either is what base did and what
// works. A pod that is merely running never releases it, so waiting only runs
// out the start timeout.
func secondMountBlocker(holder *corev1.Pod, pvcName, ownPod string,
	kind cacheAccessPodKind, runID string, singlePod bool, releasing func(*corev1.Pod) bool,
) error {
	if holder == nil || !singlePod {
		return nil
	}
	if holder.DeletionTimestamp != nil {
		return nil
	}
	if releasing != nil && releasing(holder) {
		return nil
	}
	if holder.Name == ownPod {
		return nil
	}
	// An abandoned cache pod of this kind is replaced by the run that needs the
	// claim, at a name this run would delete if that name is its own.
	if cacheAccessPodOurs(holder, kind) && !cacheAccessPodInUse(holder, runID) {
		return nil
	}
	if isCacheAccessPod(holder) && !cacheAccessPodInUse(holder, runID) {
		// A leftover of the other kind sits at a name no run deletes, so no
		// run will free the claim: say so instead of waiting out the start
		// timeout on a pod that cannot mount.
		return fmt.Errorf("claim %s is ReadWriteOncePod and pod %q holds it; "+
			"only one pod can mount a ReadWriteOncePod claim, so the %s pod cannot start. "+
			"That pod is a leftover llmkube cache pod, not a serving pod, so nothing will "+
			"remove it; delete it and retry, or use a ReadWriteOnce or ReadWriteMany claim",
			pvcName, holder.Name, kind.noun)
	}
	return fmt.Errorf("claim %s is ReadWriteOncePod and pod %q holds it; "+
		"only one pod can mount a ReadWriteOncePod claim, so the %s pod cannot start. "+
		"Wait for that pod to stop, or use a ReadWriteOnce or ReadWriteMany claim",
		pvcName, holder.Name, kind.noun)
}

// isCacheAccessPod reports whether pod is one of the short-lived pods llmkube
// creates to reach a cache claim, of either kind.
func isCacheAccessPod(pod *corev1.Pod) bool {
	return pod.Labels[managedByLabel] == managedByValue &&
		(pod.Labels[modelCacheLabel] == inspectorPodKind.component ||
			pod.Labels[modelCacheLabel] == purgePodKind.component)
}

// cacheAccessPodOurs reports whether pod is a cache-access pod of this kind,
// the only kind of pod llmkube deletes to free a claim.
func cacheAccessPodOurs(pod *corev1.Pod, kind cacheAccessPodKind) bool {
	return pod.Labels[managedByLabel] == managedByValue && pod.Labels[modelCacheLabel] == kind.component
}

// cacheAccessPodInUse reports whether a cache-access pod is stamped by a run
// that is not this one and has not outlived it.
func cacheAccessPodInUse(pod *corev1.Pod, runID string) bool {
	stamp := pod.Labels[cachePurgeRunLabel]
	return stamp != "" && stamp != runID && !purgePodAbandoned(pod)
}
