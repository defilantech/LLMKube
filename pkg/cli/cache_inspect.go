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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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
)

type PVCCacheEntry struct {
	CacheKey  string
	SizeBytes int64
}

func inspectPVCCache(
	ctx context.Context, cfg *rest.Config, k8sClient client.Client, namespace string,
) ([]PVCCacheEntry, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	err := k8sClient.Get(ctx, client.ObjectKey{Name: modelCachePVCName, Namespace: namespace}, pvc)
	if err != nil {
		return nil, nil
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	pod, containerName, err := findPodWithCachePVC(ctx, k8sClient, namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to find pod with cache PVC: %w", err)
	}

	createdPod := false
	if pod == nil {
		podName, err := createInspectorPod(ctx, clientset, namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to create inspector pod: %w", err)
		}
		defer deleteInspectorPod(context.Background(), clientset, namespace, podName)
		createdPod = true

		if err := waitForPodRunning(ctx, clientset, namespace, podName, 60*time.Second); err != nil {
			return nil, fmt.Errorf("inspector pod failed to start: %w", err)
		}

		pod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace}}
		containerName = "inspector"
	}

	mountPath := defaultModelMountPath
	if !createdPod {
		mountPath = findMountPath(pod, containerName)
	}

	output, err := execInPod(ctx, cfg, clientset, namespace, pod.Name, containerName,
		[]string{"sh", "-c", fmt.Sprintf("du -sb %s/*/ 2>/dev/null || true", mountPath)})
	if err != nil {
		return nil, fmt.Errorf("failed to exec in pod: %w", err)
	}

	return parseDuOutput(output), nil
}

func findPodWithCachePVC(ctx context.Context, k8sClient client.Client, namespace string) (*corev1.Pod, string, error) {
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
			if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == modelCachePVCName {
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

func findMountPath(pod *corev1.Pod, containerName string) string {
	for _, c := range pod.Spec.Containers {
		if c.Name == containerName {
			for _, vm := range c.VolumeMounts {
				for _, vol := range pod.Spec.Volumes {
					pvc := vol.PersistentVolumeClaim
					if vol.Name == vm.Name && pvc != nil && pvc.ClaimName == modelCachePVCName {
						return vm.MountPath
					}
				}
			}
		}
	}
	return defaultModelMountPath
}

func createInspectorPod(ctx context.Context, clientset kubernetes.Interface, namespace string) (string, error) {
	podName := "llmkube-cache-inspector"
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
							ClaimName: modelCachePVCName,
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

func parseDuOutput(output string) []PVCCacheEntry {
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
			CacheKey:  cacheKey,
			SizeBytes: sizeBytes,
		})
	}
	return entries
}
