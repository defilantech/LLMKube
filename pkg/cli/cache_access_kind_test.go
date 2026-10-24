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
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The two cache-access pods differ by the access they need: the inspector
// reads, the purge writes. A kind that mixed them up would either fail on a
// strict RWO claim or delete entries it was only asked to list.
func TestCacheAccessPodKinds(t *testing.T) {
	if !inspectorPodKind.readOnly {
		t.Error("the inspector pod mounts the claim read-write")
	}
	if purgePodKind.readOnly {
		t.Error("the purge pod mounts the claim read-only")
	}
	if inspectorPodKind.component == purgePodKind.component {
		t.Errorf("both kinds carry the component label %q", inspectorPodKind.component)
	}
	if inspectorPodName("claim-a") == purgePodName("claim-a") {
		t.Error("both kinds name their pod the same")
	}

	clientset := fakeclientset.NewClientset()
	if _, err := createInspectorPodForRun(context.Background(), clientset, "test-ns",
		modelCachePVCName, purgeRunID(), "", nil); err != nil {
		t.Fatalf("create inspector pod: %v", err)
	}
	pod, err := clientset.CoreV1().Pods("test-ns").Get(context.Background(),
		inspectorPodName(modelCachePVCName), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get inspector pod: %v", err)
	}
	if !pod.Spec.Containers[0].VolumeMounts[0].ReadOnly || !pod.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly {
		t.Error("the inspector pod spec mounts the claim read-write")
	}
	if pod.Labels[modelCacheLabel] != "cache-inspector" {
		t.Errorf("inspector component label = %q, want %q", pod.Labels[modelCacheLabel], "cache-inspector")
	}
}

// One unreadable claim must not fail a listing that read another: warn on
// stderr, return the entries that were read, exit 0.
func TestInspectPVCCacheWarnsAndSkipsOneUnreadableClaim(t *testing.T) {
	readable, unreadable := cacheClaim(), cacheClaim()
	readable.Name = "llmkube-model-cache"
	unreadable.Name = "isvc-a-model-cache"

	k8sClient := fake.NewClientBuilder().WithScheme(cacheListScheme()).
		WithObjects(readable, unreadable).Build()
	clientset := fakeclientset.NewClientset()
	// Refuse only the second claim's pod.
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == unreadable.Name {
				return true, nil, errors.New("pods is forbidden")
			}
		}
		return false, nil, nil
	})

	origExec, origWait, origSleep := purgeExec, purgeWait, purgeSleep
	t.Cleanup(func() { purgeExec, purgeWait, purgeSleep = origExec, origWait, origSleep })
	purgeSleep = func(time.Duration) {}
	purgeWait = func(_ context.Context, _ kubernetes.Interface, _, _ string, _ time.Duration) error {
		return nil
	}
	purgeExec = func(context.Context, *rest.Config, kubernetes.Interface,
		string, string, string, []string) (string, error) {
		return "4294967296\t/models/0123456789abcdef/\n", nil
	}

	stderr := captureStderr(t, func() {
		entries, err := inspectPVCInfos(context.Background(), &rest.Config{}, k8sClient, clientset,
			"test-ns", discoveredClaimsFor(t, k8sClient, "test-ns"))
		if err != nil {
			t.Fatalf("one readable claim must not fail the listing: %v", err)
		}
		if len(entries) != 1 || entries[0].CacheKey != "0123456789abcdef" {
			t.Errorf("entries = %+v, want the one key the readable claim holds", entries)
		}
	})
	if !strings.Contains(stderr, "skipping cache PVC "+unreadable.Name) {
		t.Errorf("stderr = %q, want a warning naming %s", stderr, unreadable.Name)
	}
	if strings.Contains(stderr, readable.Name) {
		t.Errorf("stderr = %q, want no warning for the claim that was read", stderr)
	}
}

// discoveredClaimsFor returns the cache claims discoverCachePVCs finds.
func discoveredClaimsFor(t *testing.T, k8sClient client.Client, namespace string) []PVCInfo {
	t.Helper()
	infos, err := discoverCachePVCs(context.Background(), k8sClient, namespace)
	if err != nil {
		t.Fatalf("discover cache PVCs: %v", err)
	}
	return infos
}

// A create failure must say why once: ensureCacheAccessPod already names the
// kind and the claim, so inspectSinglePVC must not wrap it again.
func TestInspectSinglePVCReportsCreateFailureOnce(t *testing.T) {
	k8sClient := fake.NewClientBuilder().WithScheme(cacheListScheme()).
		WithObjects(cacheClaim()).Build()
	clientset := fakeclientset.NewClientset()
	clientset.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pods is forbidden")
	})

	origWait, origSleep := purgeWait, purgeSleep
	t.Cleanup(func() { purgeWait, purgeSleep = origWait, origSleep })
	purgeSleep = func(time.Duration) {}
	purgeWait = func(_ context.Context, _ kubernetes.Interface, _, _ string, _ time.Duration) error {
		return nil
	}

	_, err := inspectSinglePVC(context.Background(), &rest.Config{}, k8sClient, clientset,
		"test-ns", PVCInfo{Name: modelCachePVCName})
	if err == nil {
		t.Fatal("want an error when the inspector pod cannot be created")
	}
	if n := strings.Count(err.Error(), "failed to create inspector pod for PVC"); n != 1 {
		t.Errorf("error names the create failure %d times, want 1: %v", n, err)
	}
}
