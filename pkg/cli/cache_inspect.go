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

	// Non-nil even when no per-PVC entries are found: a successful inspection
	// of an empty (e.g. not-yet-downloaded) cache must still tell runCacheList
	// "PVC inspection ran" so it renders the STATUS column. Returning nil here
	// would suppress STATUS and regress the listing to the pre-inspection
	// format (#767).
	allEntries := []PVCCacheEntry{}
	for _, pvcInfo := range pvcInfos {
		entries, err := inspectSinglePVC(ctx, cfg, k8sClient, clientset, namespace, pvcInfo)
		if err != nil {
			// One PVC failing to inspect (e.g. no running pod mounts it and an
			// inspector pod cannot start) must not blank the entire listing;
			// skip it and surface the caches we can read.
			fmt.Fprintf(os.Stderr, "warning: skipping cache PVC %s: %v\n", pvcInfo.Name, err)
			continue
		}
		allEntries = append(allEntries, entries...)
	}
	return allEntries, nil
}

// inspectSinglePVC inspects the contents of one model cache PVC.
func inspectSinglePVC(
	ctx context.Context, cfg *rest.Config, k8sClient client.Client, clientset kubernetes.Interface,
	namespace string, pvcInfo PVCInfo,
) ([]PVCCacheEntry, error) {
	pod, containerName, err := findPodWithPVC(ctx, k8sClient, namespace, pvcInfo.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to find pod with cache PVC %s: %w", pvcInfo.Name, err)
	}

	createdPod := false
	if pod == nil {
		podName, err := createInspectorPodForPVC(ctx, clientset, namespace, pvcInfo.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to create inspector pod for PVC %s: %w", pvcInfo.Name, err)
		}
		defer deleteInspectorPod(context.Background(), clientset, namespace, podName)
		createdPod = true

		// 120s accommodates slower storage classes (Longhorn, OpenShift CSI,
		// remote-attach disks) where PVC binding plus volume attach can exceed
		// the original 60s. Fast local-path setups still resolve in seconds; the
		// only effect of the higher ceiling is a longer wait when the inspector
		// genuinely cannot start, which is acceptable for a one-off CLI command.
		if err := waitForPodRunning(ctx, clientset, namespace, podName, 120*time.Second); err != nil {
			return nil, fmt.Errorf("inspector pod failed to start: %w", err)
		}

		pod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace}}
		containerName = "inspector"
	}

	mountPath := defaultModelMountPath
	if !createdPod {
		mountPath = findMountPathForPVC(pod, containerName, pvcInfo.Name)
	}

	output, err := execInPod(ctx, cfg, clientset, namespace, pod.Name, containerName,
		[]string{"sh", "-c", fmt.Sprintf("du -sb %s/*/ 2>/dev/null || true", mountPath)})
	if err != nil {
		return nil, fmt.Errorf("failed to exec in pod: %w", err)
	}

	return parseDuOutput(output, pvcInfo.InferenceService), nil
}

func findPodWithPVC(
	ctx context.Context, k8sClient client.Client, namespace, pvcName string,
) (*corev1.Pod, string, error) {
	podList := &corev1.PodList{}
	if err := k8sClient.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return nil, "", fmt.Errorf("failed to list pods: %w", err)
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == pvcName {
				containerName := findContainerWithVolume(pod, vol.Name)
				if containerName != "" {
					return pod, containerName, nil
				}
			}
		}
	}

	return nil, "", nil
}

func findContainerWithVolume(pod *corev1.Pod, volumeName string) string {
	for _, c := range pod.Spec.Containers {
		for _, vm := range c.VolumeMounts {
			if vm.Name == volumeName {
				return c.Name
			}
		}
	}
	for _, c := range pod.Spec.InitContainers {
		for _, vm := range c.VolumeMounts {
			if vm.Name == volumeName {
				return c.Name
			}
		}
	}
	return ""
}

func findMountPathForPVC(pod *corev1.Pod, containerName, pvcName string) string {
	for _, c := range pod.Spec.Containers {
		if c.Name == containerName {
			for _, vm := range c.VolumeMounts {
				for _, vol := range pod.Spec.Volumes {
					pvc := vol.PersistentVolumeClaim
					if vol.Name == vm.Name && pvc != nil && pvc.ClaimName == pvcName {
						return vm.MountPath
					}
				}
			}
		}
	}
	return defaultModelMountPath
}

// inspectorPodName returns a per-PVC inspector pod name. Each cache list run
// may inspect several PVCs in sequence; a single shared pod name would collide
// (AlreadyExists) with the previous inspector while it is still terminating,
// causing every PVC after the first to be skipped. Deriving the name from the
// PVC keeps it unique per PVC and DNS-1123 safe (computeCacheKey is a 16-char
// hex digest).
func inspectorPodName(pvcName string) string {
	return "llmkube-cache-inspector-" + computeCacheKey(pvcName)
}

func createInspectorPodForPVC(
	ctx context.Context, clientset kubernetes.Interface, namespace, pvcName string,
) (string, error) {
	podName := inspectorPodName(pvcName)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "llmkube-cli",
				"app.kubernetes.io/component":  "cache-inspector",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:    "inspector",
					Image:   "busybox:1.37.0",
					Command: []string{"sleep", "300"},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "model-cache",
							MountPath: defaultModelMountPath,
							ReadOnly:  true,
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
							ReadOnly:  true,
						},
					},
				},
			},
		},
	}

	_, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return podName, nil
}

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

func deleteInspectorPod(ctx context.Context, clientset kubernetes.Interface, namespace, name string) {
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
