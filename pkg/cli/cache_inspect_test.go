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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestParseDuOutput(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []PVCCacheEntry
	}{
		{
			name:  "valid output with multiple entries",
			input: "4831838208\t/models/abc123def4567890/\n1073741824\t/models/fedcba0987654321/\n",
			expected: []PVCCacheEntry{
				{CacheKey: "abc123def4567890", SizeBytes: 4831838208},
				{CacheKey: "fedcba0987654321", SizeBytes: 1073741824},
			},
		},
		{
			name:     "empty output",
			input:    "",
			expected: nil,
		},
		{
			name:     "whitespace only",
			input:    "   \n  \n",
			expected: nil,
		},
		{
			name:  "malformed lines are skipped",
			input: "not-a-number\t/models/abc123/\n4096\t/models/valid123/\nbadline\n",
			expected: []PVCCacheEntry{
				{CacheKey: "valid123", SizeBytes: 4096},
			},
		},
		{
			name:  "trailing slashes stripped",
			input: "1024\t/models/cachekey123/\n",
			expected: []PVCCacheEntry{
				{CacheKey: "cachekey123", SizeBytes: 1024},
			},
		},
		{
			name:  "no trailing slash",
			input: "2048\t/models/cachekey456\n",
			expected: []PVCCacheEntry{
				{CacheKey: "cachekey456", SizeBytes: 2048},
			},
		},
		{
			name:  "single entry no newline",
			input: "512\t/models/mykey",
			expected: []PVCCacheEntry{
				{CacheKey: "mykey", SizeBytes: 512},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := parseDuOutput(tt.input)

			if len(entries) != len(tt.expected) {
				t.Fatalf("got %d entries, want %d", len(entries), len(tt.expected))
			}

			for i, got := range entries {
				want := tt.expected[i]
				if got.CacheKey != want.CacheKey {
					t.Errorf("entry[%d].CacheKey = %q, want %q", i, got.CacheKey, want.CacheKey)
				}
				if got.SizeBytes != want.SizeBytes {
					t.Errorf("entry[%d].SizeBytes = %d, want %d", i, got.SizeBytes, want.SizeBytes)
				}
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		name     string
		input    int64
		expected string
	}{
		{"zero bytes", 0, "0 B"},
		{"sub-KiB", 512, "512 B"},
		{"exactly 1 KiB", 1024, "1.0 KiB"},
		{"KiB range", 1536, "1.5 KiB"},
		{"MiB range", 5242880, "5.0 MiB"},
		{"GiB range", 4831838208, "4.5 GiB"},
		{"TiB range", 1099511627776, "1.0 TiB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatBytes(tt.input)
			if got != tt.expected {
				t.Errorf("formatBytes(%d) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestNewCacheListCommandOrphanedFlag(t *testing.T) {
	cmd := newCacheListCommand()

	f := cmd.Flags().Lookup("orphaned")
	if f == nil {
		t.Fatal("Missing --orphaned flag")
	}
	if f.DefValue != "false" {
		t.Errorf("--orphaned default = %q, want %q", f.DefValue, "false")
	}
}

func TestFindContainerWithVolume(t *testing.T) {
	tests := []struct {
		name       string
		pod        *corev1.Pod
		volumeName string
		want       string
	}{
		{
			name: "container has matching volume mount",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "server",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "model-cache", MountPath: "/models"},
							},
						},
					},
				},
			},
			volumeName: "model-cache",
			want:       "server",
		},
		{
			name: "no container has the volume",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "server",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "other-vol", MountPath: "/data"},
							},
						},
					},
				},
			},
			volumeName: "model-cache",
			want:       "",
		},
		{
			name: "init container has matching volume mount",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:         "server",
							VolumeMounts: []corev1.VolumeMount{},
						},
					},
					InitContainers: []corev1.Container{
						{
							Name: "downloader",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "model-cache", MountPath: "/models"},
							},
						},
					},
				},
			},
			volumeName: "model-cache",
			want:       "downloader",
		},
		{
			name: "regular container preferred over init container",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "server",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "model-cache", MountPath: "/models"},
							},
						},
					},
					InitContainers: []corev1.Container{
						{
							Name: "downloader",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "model-cache", MountPath: "/models"},
							},
						},
					},
				},
			},
			volumeName: "model-cache",
			want:       "server",
		},
		{
			name: "multiple containers only second matches",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "sidecar",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "logs", MountPath: "/var/log"},
							},
						},
						{
							Name: "llama-server",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "model-cache", MountPath: "/models"},
							},
						},
					},
				},
			},
			volumeName: "model-cache",
			want:       "llama-server",
		},
		{
			name: "empty pod spec",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{},
			},
			volumeName: "model-cache",
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findContainerWithVolume(tt.pod, tt.volumeName)
			if got != tt.want {
				t.Errorf("findContainerWithVolume() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFindMountPath(t *testing.T) {
	tests := []struct {
		name          string
		pod           *corev1.Pod
		containerName string
		want          string
	}{
		{
			name: "finds mount path for cache PVC",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "server",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "model-cache", MountPath: "/data/models"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "model-cache",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: modelCachePVCName,
								},
							},
						},
					},
				},
			},
			containerName: "server",
			want:          "/data/models",
		},
		{
			name: "default fallback when container not found",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "server",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "model-cache", MountPath: "/data/models"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "model-cache",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: modelCachePVCName,
								},
							},
						},
					},
				},
			},
			containerName: "nonexistent",
			want:          "/models",
		},
		{
			name: "default fallback when volume is not a PVC",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "server",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "config", MountPath: "/etc/config"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: "my-config"},
								},
							},
						},
					},
				},
			},
			containerName: "server",
			want:          "/models",
		},
		{
			name: "default fallback when PVC claim name does not match",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "server",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "other-pvc", MountPath: "/data"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "other-pvc",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "some-other-pvc",
								},
							},
						},
					},
				},
			},
			containerName: "server",
			want:          "/models",
		},
		{
			name: "container has multiple volumes but only one is cache PVC",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "server",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "config", MountPath: "/etc/config"},
								{Name: "cache-vol", MountPath: "/cache"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"},
								},
							},
						},
						{
							Name: "cache-vol",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: modelCachePVCName,
								},
							},
						},
					},
				},
			},
			containerName: "server",
			want:          "/cache",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findMountPath(tt.pod, tt.containerName)
			if got != tt.want {
				t.Errorf("findMountPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func newCoreScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

func TestFindPodWithCachePVC_RunningPodWithPVC(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-server", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "server",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "cache-vol", MountPath: "/models"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "cache-vol",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: modelCachePVCName,
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(newCoreScheme()).
		WithObjects(pod).
		Build()

	foundPod, containerName, err := findPodWithCachePVC(context.Background(), k8sClient, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundPod == nil {
		t.Fatal("expected a pod, got nil")
	}
	if foundPod.Name != "llm-server" {
		t.Errorf("pod name = %q, want %q", foundPod.Name, "llm-server")
	}
	if containerName != "server" {
		t.Errorf("container = %q, want %q", containerName, "server")
	}
}

func TestFindPodWithCachePVC_SkipsNonRunningPods(t *testing.T) {
	pendingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "server",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "cache-vol", MountPath: "/models"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "cache-vol",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: modelCachePVCName,
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}

	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "server",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "cache-vol", MountPath: "/models"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "cache-vol",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: modelCachePVCName,
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(newCoreScheme()).
		WithObjects(pendingPod, failedPod).
		Build()

	foundPod, containerName, err := findPodWithCachePVC(context.Background(), k8sClient, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundPod != nil {
		t.Errorf("expected nil pod, got %q", foundPod.Name)
	}
	if containerName != "" {
		t.Errorf("expected empty container name, got %q", containerName)
	}
}

func TestFindPodWithCachePVC_NoPods(t *testing.T) {
	k8sClient := fake.NewClientBuilder().
		WithScheme(newCoreScheme()).
		Build()

	foundPod, containerName, err := findPodWithCachePVC(context.Background(), k8sClient, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundPod != nil {
		t.Error("expected nil pod")
	}
	if containerName != "" {
		t.Errorf("expected empty container name, got %q", containerName)
	}
}

func TestFindPodWithCachePVC_RunningPodWithoutCachePVC(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "other-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "data", MountPath: "/data"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "some-other-pvc",
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(newCoreScheme()).
		WithObjects(pod).
		Build()

	foundPod, containerName, err := findPodWithCachePVC(context.Background(), k8sClient, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundPod != nil {
		t.Errorf("expected nil pod, got %q", foundPod.Name)
	}
	if containerName != "" {
		t.Errorf("expected empty container name, got %q", containerName)
	}
}

func TestFindPodWithCachePVC_PVCMountedButNoContainerMount(t *testing.T) {
	// Volume references the cache PVC but no container actually mounts it
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "misconfigured", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:         "server",
					VolumeMounts: []corev1.VolumeMount{},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "cache-vol",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: modelCachePVCName,
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(newCoreScheme()).
		WithObjects(pod).
		Build()

	foundPod, containerName, err := findPodWithCachePVC(context.Background(), k8sClient, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundPod != nil {
		t.Errorf("expected nil pod (no container mount), got %q", foundPod.Name)
	}
	if containerName != "" {
		t.Errorf("expected empty container, got %q", containerName)
	}
}

func TestFindPodWithCachePVC_NamespaceFiltering(t *testing.T) {
	podInDefault := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-default", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "server",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "cache-vol", MountPath: "/models"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "cache-vol",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: modelCachePVCName,
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	podInOther := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-other", Namespace: "other"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "server",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "cache-vol", MountPath: "/models"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "cache-vol",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: modelCachePVCName,
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(newCoreScheme()).
		WithObjects(podInDefault, podInOther).
		Build()

	foundPod, _, err := findPodWithCachePVC(context.Background(), k8sClient, "other")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundPod == nil {
		t.Fatal("expected pod in 'other' namespace, got nil")
	}
	if foundPod.Name != "pod-other" {
		t.Errorf("pod name = %q, want %q", foundPod.Name, "pod-other")
	}
}

func TestCreateInspectorPod(t *testing.T) {
	clientset := fakeclientset.NewClientset()
	ctx := context.Background()

	podName, err := createInspectorPod(ctx, clientset, "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if podName != "llmkube-cache-inspector" {
		t.Errorf("pod name = %q, want %q", podName, "llmkube-cache-inspector")
	}

	pod, err := clientset.CoreV1().Pods("test-ns").Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get created pod: %v", err)
	}

	if pod.Labels["app.kubernetes.io/managed-by"] != "llmkube-cli" {
		t.Errorf("managed-by label = %q, want %q", pod.Labels["app.kubernetes.io/managed-by"], "llmkube-cli")
	}
	if pod.Labels["app.kubernetes.io/component"] != "cache-inspector" {
		t.Errorf("component label = %q, want %q", pod.Labels["app.kubernetes.io/component"], "cache-inspector")
	}

	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy = %q, want %q", pod.Spec.RestartPolicy, corev1.RestartPolicyNever)
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("container count = %d, want 1", len(pod.Spec.Containers))
	}

	container := pod.Spec.Containers[0]
	if container.Name != "inspector" {
		t.Errorf("container name = %q, want %q", container.Name, "inspector")
	}
	if container.Image != "busybox:1.37.0" {
		t.Errorf("image = %q, want %q", container.Image, "busybox:1.37.0")
	}

	if len(container.VolumeMounts) != 1 {
		t.Fatalf("volume mount count = %d, want 1", len(container.VolumeMounts))
	}
	vm := container.VolumeMounts[0]
	if vm.MountPath != "/models" {
		t.Errorf("mount path = %q, want %q", vm.MountPath, "/models")
	}
	if !vm.ReadOnly {
		t.Error("volume mount should be read-only")
	}

	if len(pod.Spec.Volumes) != 1 {
		t.Fatalf("volume count = %d, want 1", len(pod.Spec.Volumes))
	}
	vol := pod.Spec.Volumes[0]
	if vol.PersistentVolumeClaim == nil {
		t.Fatal("volume PVC source is nil")
	}
	if vol.PersistentVolumeClaim.ClaimName != modelCachePVCName {
		t.Errorf("PVC claim = %q, want %q", vol.PersistentVolumeClaim.ClaimName, modelCachePVCName)
	}
	if !vol.PersistentVolumeClaim.ReadOnly {
		t.Error("PVC volume source should be read-only")
	}
}

func TestCreateInspectorPod_AlreadyExists(t *testing.T) {
	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llmkube-cache-inspector",
			Namespace: "default",
		},
	}
	clientset := fakeclientset.NewClientset(existingPod)

	_, err := createInspectorPod(context.Background(), clientset, "default")
	if err == nil {
		t.Fatal("expected error when pod already exists")
	}
}

func TestWaitForPodRunning_AlreadyRunning(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	clientset := fakeclientset.NewClientset(pod)

	err := waitForPodRunning(context.Background(), clientset, "default", "test-pod", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForPodRunning_PodFailed(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}
	clientset := fakeclientset.NewClientset(pod)

	err := waitForPodRunning(context.Background(), clientset, "default", "test-pod", 5*time.Second)
	if err == nil {
		t.Fatal("expected error for failed pod")
	}
	if got := err.Error(); got != "pod test-pod entered phase Failed" {
		t.Errorf("error = %q, want %q", got, "pod test-pod entered phase Failed")
	}
}

func TestWaitForPodRunning_PodSucceeded(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	clientset := fakeclientset.NewClientset(pod)

	err := waitForPodRunning(context.Background(), clientset, "default", "test-pod", 5*time.Second)
	if err == nil {
		t.Fatal("expected error for succeeded pod")
	}
	if got := err.Error(); got != "pod test-pod entered phase Succeeded" {
		t.Errorf("error = %q, want %q", got, "pod test-pod entered phase Succeeded")
	}
}

func TestWaitForPodRunning_Timeout(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	clientset := fakeclientset.NewClientset(pod)

	// Use a very short timeout so the test runs fast
	err := waitForPodRunning(context.Background(), clientset, "default", "test-pod", 1*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if got := err.Error(); got != "timed out waiting for pod test-pod to be running" {
		t.Errorf("error = %q, want timeout message", got)
	}
}

func TestWaitForPodRunning_PodNotFound(t *testing.T) {
	clientset := fakeclientset.NewClientset()

	err := waitForPodRunning(context.Background(), clientset, "default", "nonexistent", 5*time.Second)
	if err == nil {
		t.Fatal("expected error for nonexistent pod")
	}
}

func TestWaitForPodRunning_ContextCancelled(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	clientset := fakeclientset.NewClientset(pod)

	// The fake clientset does not propagate context cancellation, so a
	// cancelled context still results in the timeout path. Use a short
	// timeout to keep the test fast.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForPodRunning(ctx, clientset, "default", "test-pod", 1*time.Millisecond)
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
}

func TestDeleteInspectorPod(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "llmkube-cache-inspector", Namespace: "default"},
	}
	clientset := fakeclientset.NewClientset(pod)

	deleteInspectorPod(context.Background(), clientset, "default", "llmkube-cache-inspector")

	_, err := clientset.CoreV1().Pods("default").Get(context.Background(), "llmkube-cache-inspector", metav1.GetOptions{})
	if err == nil {
		t.Error("pod should have been deleted")
	}
}

func TestDeleteInspectorPod_NonexistentPod(t *testing.T) {
	clientset := fakeclientset.NewClientset()

	// Should not panic when deleting a pod that doesn't exist
	deleteInspectorPod(context.Background(), clientset, "default", "nonexistent")
}

func TestInspectPVCCache_NoPVC(t *testing.T) {
	// When the PVC doesn't exist, inspectPVCCache should return nil, nil
	k8sClient := fake.NewClientBuilder().
		WithScheme(newCoreScheme()).
		Build()

	entries, err := inspectPVCCache(context.Background(), nil, k8sClient, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries when PVC doesn't exist, got %v", entries)
	}
}

func TestMergePVCEntriesWithModels(t *testing.T) {
	tests := []struct {
		name          string
		cacheEntries  map[string]*CacheEntry
		pvcEntries    []PVCCacheEntry
		wantActive    int
		wantOrphaned  int
		wantSizeBytes map[string]int64
	}{
		{
			name: "PVC entry matches existing model entry",
			cacheEntries: map[string]*CacheEntry{
				"abc123": {
					CacheKey:   "abc123",
					Source:     "https://example.com/model.gguf",
					ModelNames: []string{"my-model"},
					Status:     statusActive,
				},
			},
			pvcEntries: []PVCCacheEntry{
				{CacheKey: "abc123", SizeBytes: 4831838208},
			},
			wantActive:    1,
			wantOrphaned:  0,
			wantSizeBytes: map[string]int64{"abc123": 4831838208},
		},
		{
			name:         "PVC entry with no matching model becomes orphaned",
			cacheEntries: map[string]*CacheEntry{},
			pvcEntries: []PVCCacheEntry{
				{CacheKey: "orphan123", SizeBytes: 1024},
			},
			wantActive:    0,
			wantOrphaned:  1,
			wantSizeBytes: map[string]int64{"orphan123": 1024},
		},
		{
			name: "mix of matched and orphaned entries",
			cacheEntries: map[string]*CacheEntry{
				"active1": {
					CacheKey:   "active1",
					Source:     "https://example.com/a.gguf",
					ModelNames: []string{"model-a"},
					Status:     statusActive,
				},
			},
			pvcEntries: []PVCCacheEntry{
				{CacheKey: "active1", SizeBytes: 2048},
				{CacheKey: "orphan1", SizeBytes: 4096},
			},
			wantActive:   1,
			wantOrphaned: 1,
			wantSizeBytes: map[string]int64{
				"active1": 2048,
				"orphan1": 4096,
			},
		},
		{
			name: "model entry exists but no PVC entry retains zero size",
			cacheEntries: map[string]*CacheEntry{
				"nodisc": {
					CacheKey:   "nodisc",
					Source:     "https://example.com/b.gguf",
					ModelNames: []string{"model-b"},
					Status:     statusActive,
				},
			},
			pvcEntries:    []PVCCacheEntry{},
			wantActive:    1,
			wantOrphaned:  0,
			wantSizeBytes: map[string]int64{"nodisc": 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Apply the same merge logic that runCacheList uses
			for _, pe := range tt.pvcEntries {
				if entry, exists := tt.cacheEntries[pe.CacheKey]; exists {
					entry.Size = pe.SizeBytes
					entry.SizeHuman = formatBytes(pe.SizeBytes)
				} else {
					tt.cacheEntries[pe.CacheKey] = &CacheEntry{
						CacheKey:  pe.CacheKey,
						Size:      pe.SizeBytes,
						SizeHuman: formatBytes(pe.SizeBytes),
						Status:    statusOrphaned,
					}
				}
			}

			var activeCount, orphanedCount int
			for _, entry := range tt.cacheEntries {
				if entry.Status == statusOrphaned {
					orphanedCount++
				} else {
					activeCount++
				}
			}

			if activeCount != tt.wantActive {
				t.Errorf("active count = %d, want %d", activeCount, tt.wantActive)
			}
			if orphanedCount != tt.wantOrphaned {
				t.Errorf("orphaned count = %d, want %d", orphanedCount, tt.wantOrphaned)
			}

			for key, wantSize := range tt.wantSizeBytes {
				entry, exists := tt.cacheEntries[key]
				if !exists {
					t.Errorf("missing entry for key %q", key)
					continue
				}
				if entry.Size != wantSize {
					t.Errorf("entry[%q].Size = %d, want %d", key, entry.Size, wantSize)
				}
			}
		})
	}
}

func TestMergeOrphanedOnlyFilter(t *testing.T) {
	cacheEntries := map[string]*CacheEntry{
		"active1": {
			CacheKey: "active1",
			Status:   statusActive,
		},
		"orphan1": {
			CacheKey: "orphan1",
			Status:   statusOrphaned,
		},
		"active2": {
			CacheKey: "active2",
			Status:   statusActive,
		},
		"orphan2": {
			CacheKey: "orphan2",
			Status:   statusOrphaned,
		},
	}

	// Apply the same filter logic from runCacheList
	for key, entry := range cacheEntries {
		if entry.Status != statusOrphaned {
			delete(cacheEntries, key)
		}
	}

	if len(cacheEntries) != 2 {
		t.Fatalf("expected 2 orphaned entries, got %d", len(cacheEntries))
	}
	for key, entry := range cacheEntries {
		if entry.Status != statusOrphaned {
			t.Errorf("entry %q should be orphaned, got %q", key, entry.Status)
		}
	}
	if _, ok := cacheEntries["orphan1"]; !ok {
		t.Error("missing expected orphan1 entry")
	}
	if _, ok := cacheEntries["orphan2"]; !ok {
		t.Error("missing expected orphan2 entry")
	}
}

func TestMergeOrphanedEntryHasNoModelNames(t *testing.T) {
	cacheEntries := map[string]*CacheEntry{}
	pvcEntries := []PVCCacheEntry{
		{CacheKey: "orphan-key", SizeBytes: 9999},
	}

	for _, pe := range pvcEntries {
		if entry, exists := cacheEntries[pe.CacheKey]; exists {
			entry.Size = pe.SizeBytes
		} else {
			cacheEntries[pe.CacheKey] = &CacheEntry{
				CacheKey:  pe.CacheKey,
				Size:      pe.SizeBytes,
				SizeHuman: formatBytes(pe.SizeBytes),
				Status:    statusOrphaned,
			}
		}
	}

	entry := cacheEntries["orphan-key"]
	if entry == nil {
		t.Fatal("expected orphan entry to exist")
	}
	if entry.Source != "" {
		t.Errorf("orphaned entry source = %q, want empty", entry.Source)
	}
	if len(entry.ModelNames) != 0 {
		t.Errorf("orphaned entry model names = %v, want empty", entry.ModelNames)
	}
	if entry.SizeHuman != "9.8 KiB" {
		t.Errorf("orphaned entry size human = %q, want %q", entry.SizeHuman, "9.8 KiB")
	}
}
