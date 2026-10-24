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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8sscheme "k8s.io/client-go/kubernetes/scheme"
)

const (
	modelCachePVCName     = "llmkube-model-cache"
	defaultModelMountPath = "/models"
	modelCacheLabel       = "app.kubernetes.io/component"
	modelCacheLabelValue  = "model-cache"
)

// PVCInfo holds metadata about a discovered model cache PVC.
type PVCInfo struct {
	Name             string
	InferenceService string // empty for the shared cache
}

type PVCCacheEntry struct {
	CacheKey         string
	SizeBytes        int64
	InferenceService string // empty for the shared cache
}

// discoverCachePVCs lists all model cache PVCs in the given namespace by
// looking for PVCs with the label app.kubernetes.io/component=model-cache.
// For each PVC it determines the owning InferenceService (empty for the
// shared cache).
func discoverCachePVCs(ctx context.Context, k8sClient client.Client, namespace string) ([]PVCInfo, error) {
	pvcList := &corev1.PersistentVolumeClaimList{}
	if err := k8sClient.List(ctx, pvcList, client.InNamespace(namespace), client.MatchingLabels{
		modelCacheLabel: modelCacheLabelValue,
	}); err != nil {
		return nil, fmt.Errorf("failed to list model cache PVCs: %w", err)
	}

	var infos []PVCInfo
	seen := map[string]bool{}
	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		// Pending PVCs are NOT skipped. The cluster default storage class is
		// commonly WaitForFirstConsumer (kind local-path, microk8s hostpath,
		// most topology-aware CSI), under which the cache PVC stays Pending
		// until its first consumer is scheduled. The transient inspector pod
		// inspectSinglePVC creates IS that first consumer: creating it binds
		// the volume on the inspector's node, then it is read. Skipping Pending
		// here would mean such a PVC is never inspected, so pvcInspected stays
		// false and `cache list` drops the STATUS column entirely (#767). A
		// genuinely unschedulable PVC is bounded by the inspector pod's start
		// timeout and handled by the per-PVC skip in inspectPVCCache.
		isvcName := ""
		for _, ref := range pvc.OwnerReferences {
			if ref.Kind == "InferenceService" && ref.Controller != nil && *ref.Controller {
				isvcName = ref.Name
				break
			}
		}
		infos = append(infos, PVCInfo{
			Name:             pvc.Name,
			InferenceService: isvcName,
		})
		seen[pvc.Name] = true
	}

	// Always include the well-known shared cache PVC by name, even if it was
	// not label-discovered above: the shared cache can predate the
	// app.kubernetes.io/component=model-cache label (the InferenceService
	// reconciler only sets it on create and does not backfill an existing
	// PVC), and the original cache-list behavior always inspected it. The
	// shared cache has no owning InferenceService.
	if !seen[modelCachePVCName] {
		shared := &corev1.PersistentVolumeClaim{}
		err := k8sClient.Get(ctx, client.ObjectKey{Name: modelCachePVCName, Namespace: namespace}, shared)
		switch {
		case err == nil:
			// Included regardless of phase; a Pending WaitForFirstConsumer
			// shared cache binds when the inspector pod mounts it (see the
			// loop comment above).
			infos = append(infos, PVCInfo{Name: modelCachePVCName, InferenceService: ""})
		case !apierrors.IsNotFound(err):
			return nil, fmt.Errorf("failed to get shared cache PVC: %w", err)
		}
	}

	return infos, nil
}

// inspectPVCCache reads every discovered cache claim.
func inspectPVCCache(
	ctx context.Context, cfg *rest.Config, k8sClient client.Client, namespace string,
) ([]PVCCacheEntry, error) {
	pvcInfos, err := discoverCachePVCs(ctx, k8sClient, namespace)
	if err != nil {
		return nil, err
	}
	if len(pvcInfos) == 0 {
		return nil, nil
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}
	return inspectPVCInfos(ctx, cfg, k8sClient, clientset, namespace, pvcInfos)
}

// inspectPVCInfos reads each discovered claim and applies the per-claim
// policy: skip a claim that cannot be read, and fail only when every claim
// failed.
func inspectPVCInfos(
	ctx context.Context, cfg *rest.Config, k8sClient client.Client,
	clientset kubernetes.Interface, namespace string, pvcInfos []PVCInfo,
) ([]PVCCacheEntry, error) {

	// Non-nil even when no per-PVC entries are found: a successful inspection
	// of an empty (e.g. not-yet-downloaded) cache must still tell runCacheList
	// "PVC inspection ran" so it renders the STATUS column. Returning nil here
	// would suppress STATUS and regress the listing to the pre-inspection
	// format (#767).
	allEntries := []PVCCacheEntry{}
	var failures []error
	for _, pvcInfo := range pvcInfos {
		entries, err := inspectSinglePVC(ctx, cfg, k8sClient, clientset, namespace, pvcInfo)
		if err != nil {
			// One PVC failing to inspect (a volume that cannot attach, an
			// inspector pod that cannot start) must not blank the entire
			// listing; skip it and surface the caches we can read.
			failures = append(failures, fmt.Errorf("%s: %w", pvcInfo.Name, err))
			fmt.Fprintf(os.Stderr, "warning: skipping cache PVC %s: %v\n", pvcInfo.Name, err)
			continue
		}
		allEntries = append(allEntries, entries...)
	}

	// Zero readable claims is an error: `cache list --orphaned` must never
	// answer "none" from a cache it did not read.
	if len(failures) == len(pvcInfos) {
		return nil, fmt.Errorf("cannot read any model cache claim in namespace %s: %w",
			namespace, errors.Join(failures...))
	}
	return allEntries, nil
}

// inspectSinglePVC inspects the contents of one model cache PVC.
//
// Own pod, never a borrowed one: in the default shared cache mode the shared
// claim is mounted by the distroless controller, which has no shell, and a
// serving container mounts it read-only (#2140).
func inspectSinglePVC(
	ctx context.Context, cfg *rest.Config, k8sClient client.Client, clientset kubernetes.Interface,
	namespace string, pvcInfo PVCInfo,
) ([]PVCCacheEntry, error) {
	// A claim read that fails (including a forbidden one) leaves the access
	// mode unknown, which keeps the pre-#2155 pin: a wrongly unpinned strict
	// claim stalls on Multi-Attach.
	claim := readCacheClaim(ctx, k8sClient, namespace, pvcInfo.Name)
	pods := &corev1.PodList{}
	if err := k8sClient.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	holder := claimHolder(pods.Items, pvcInfo.Name)
	runID := purgeRunID()
	if err := secondMountBlocker(holder, pvcInfo.Name, inspectorPodName(pvcInfo.Name),
		inspectorPodKind, runID, claimSinglePod(claim), nil); err != nil {
		return nil, err
	}
	// A shareable claim is attached wherever it is needed, so pinning the
	// inspector adds only a way to fail; it still takes the tolerations of a
	// pod that mounts the claim, or a cluster whose nodes are all tainted for
	// that pod has nowhere to run it.
	node, tolerations := "", []corev1.Toleration(nil)
	if holder != nil {
		// Copy so a later mutation of the live object cannot reach the pod.
		tolerations = append(tolerations, holder.Spec.Tolerations...)
		if !claimShareable(claim) {
			node = holder.Spec.NodeName
		}
	}

	podName, err := ensureCacheAccessPod(ctx, clientset, namespace, pvcInfo.Name,
		inspectorPodName(pvcInfo.Name), inspectorPodKind, runID,
		func(runID string) (string, error) {
			return createInspectorPodForRun(ctx, clientset, namespace, pvcInfo.Name, runID, node, tolerations)
		})
	if err != nil {
		return nil, err
	}
	defer deleteCacheAccessPod(context.Background(), clientset, namespace, podName)
	return readCacheEntries(ctx, cfg, clientset, namespace, podName, pvcInfo.InferenceService)
}

// createInspectorPodForRun creates the read-only pod that reads a cache claim,
// stamped with runID so a concurrent run's pod is refused rather than taken
// over. node is the node the claim is attached on, and tolerations what
// scheduling there needs; empty node means the pod schedules anywhere.
func createInspectorPodForRun(
	ctx context.Context, clientset kubernetes.Interface, namespace, pvcName, runID string,
	node string, tolerations []corev1.Toleration,
) (string, error) {
	return createCacheAccessPod(ctx, clientset, namespace, pvcName, inspectorPodName(pvcName),
		inspectorPodKind, runID, node, tolerations)
}

// readCacheEntries runs the size listing in the inspector pod and parses it.
func readCacheEntries(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, podName, isvcName string,
) ([]PVCCacheEntry, error) {
	output, err := purgeExec(ctx, cfg, clientset, namespace, podName, inspectorPodKind.container,
		[]string{"sh", "-c", fmt.Sprintf("du -sb %s/*/ 2>/dev/null || true", defaultModelMountPath)})
	if err != nil {
		return nil, fmt.Errorf("failed to exec in pod: %w", err)
	}
	return parseDuOutput(output, isvcName), nil
}

// claimHolder returns the pod a strict RWO claim is attached for from pods, or
// nil when nothing holds it: the volume is attached for any assigned pod that
// has not terminated, including one still pulling its image (the attach/detach
// controller's rule, not phase Running). The inspector has to land on that
// pod's node, under the same tolerations; with no holder the inspector binds
// the claim itself (WaitForFirstConsumer). With several holders the first is
// taken: they share the claim, so any of their nodes has it attached.
func claimHolder(pods []corev1.Pod, pvcName string) *corev1.Pod {
	for i := range pods {
		pod := &pods[i]
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			continue
		}
		if pod.Spec.NodeName == "" || holderTerminated(pod) {
			continue
		}
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == pvcName {
				return pod
			}
		}
	}
	return nil
}

// holderTerminated reports whether a pod being deleted has no container left
// running, the condition the attach/detach controller reads as terminated
// (kubernetes pkg/volume/util.IsPodTerminated). A pod still inside its
// termination grace period keeps the volume attached.
func holderTerminated(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp == nil {
		return false
	}
	return noContainerRunning(pod.Status.InitContainerStatuses) &&
		noContainerRunning(pod.Status.ContainerStatuses) &&
		noContainerRunning(pod.Status.EphemeralContainerStatuses)
}

// noContainerRunning reports whether every status is terminated or waiting,
// or there are none.
func noContainerRunning(statuses []corev1.ContainerStatus) bool {
	for _, status := range statuses {
		if status.State.Terminated == nil && status.State.Waiting == nil {
			return false
		}
	}
	return true
}

// newCacheAccessPodForRun builds the pod and, when runID is set, stamps it
// with the invocation that created it, so a concurrent llmkube run's pod is
// never mistaken for a stale one.
func newCacheAccessPodForRun(
	podName, pvcName, component, containerName string, readOnly bool, runID string,
) *corev1.Pod {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "llmkube-cli",
		"app.kubernetes.io/component":  component,
	}
	if runID != "" {
		labels[cachePurgeRunLabel] = runID
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   podName,
			Labels: labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  containerName,
					Image: cacheAccessImageName(),
					// A mirror tagged :latest would otherwise be pulled on
					// every run, on a cluster that may not reach it at all.
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{"sleep", "300"},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "model-cache",
							MountPath: defaultModelMountPath,
							ReadOnly:  readOnly,
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "model-cache",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
							ReadOnly:  readOnly,
						},
					},
				},
			},
		},
	}
	return pod
}

// newCacheAccessPodOnNode builds the pod pinned to node, with the tolerations
// that node needs, when one is given: the claim is attached there, so
// scheduling anywhere else is the Multi-Attach error the pod would stall on.
func newCacheAccessPodOnNode(
	podName, pvcName, component, containerName string, readOnly bool, runID, node string,
	tolerations []corev1.Toleration,
) *corev1.Pod {
	pod := newCacheAccessPodForRun(podName, pvcName, component, containerName, readOnly, runID)
	// Unpinned pods still take the tolerations; they just have no node to pin to.
	pod.Spec.Tolerations = tolerations
	if node != "" {
		pod.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "kubernetes.io/hostname",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{node},
						}},
					}},
				},
			},
		}
	}
	return pod
}

// waitForPodRunning polls until the pod reports Running.
//
// 120s accommodates slower storage classes (Longhorn, OpenShift CSI,
// remote-attach disks) where PVC binding plus volume attach can exceed the
// original 60s. Fast local-path setups still resolve in seconds; the only
// effect of the higher ceiling is a longer wait when the pod genuinely cannot
// start, which is acceptable for a one-off CLI command.
func waitForPodRunning(
	ctx context.Context, clientset kubernetes.Interface,
	namespace, name string, timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return nil
		case corev1.PodFailed, corev1.PodSucceeded:
			return fmt.Errorf("pod %s entered phase %s", name, pod.Status.Phase)
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("timed out waiting for pod %s to be running", name)
}

// deleteCacheAccessPod removes a short-lived cache pod. It is a variable so
// tests can observe (and script) the asynchronous delete.
var deleteCacheAccessPod = func(ctx context.Context, clientset kubernetes.Interface, namespace, name string) {
	gracePeriod := int64(0)
	_ = clientset.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	})
}

func execInPod(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, podName, containerName string, command []string,
) (string, error) {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, k8sscheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return "", fmt.Errorf("exec failed: %w (stderr: %s)", err, stderr.String())
	}

	return stdout.String(), nil
}

func parseDuOutput(output string, isvcName string) []PVCCacheEntry {
	lines := strings.Split(output, "\n")
	entries := make([]PVCCacheEntry, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}

		sizeBytes, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			continue
		}

		path := strings.TrimSpace(parts[1])
		path = strings.TrimSuffix(path, "/")
		cacheKey := filepath.Base(path)
		if cacheKey == "" || cacheKey == "." || cacheKey == "/" {
			continue
		}

		entries = append(entries, PVCCacheEntry{
			CacheKey:         cacheKey,
			SizeBytes:        sizeBytes,
			InferenceService: isvcName,
		})
	}
	return entries
}
