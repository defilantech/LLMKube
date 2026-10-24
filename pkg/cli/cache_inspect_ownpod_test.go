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
	"io"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// unreachableRESTConfig is a valid-shaped config with no server behind it:
// an exec against it fails cleanly instead of panicking on a nil REST client.
var unreachableRESTConfig = &rest.Config{Host: "https://127.0.0.1:1"}

// cacheListScheme is the fake controller-runtime scheme these tests build
// their clients from.
func cacheListScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

// shelllessCachePod is the shape of the chart's controller Deployment in the
// default shared cache mode: distroless, no shell, the cache claim mounted at
// /models. A cache list that borrows this pod cannot run `du` in it (#2140).
func shelllessCachePod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "llmkube-controller", Namespace: "test-ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "manager",
				Image: "ghcr.io/defilantech/llmkube-controller:0.10.5",
				VolumeMounts: []corev1.VolumeMount{
					{Name: "model-cache", MountPath: "/models"},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "model-cache",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: modelCachePVCName},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// cacheClaim is the discovered shared cache claim.
func cacheClaim() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelCachePVCName,
			Namespace: "test-ns",
			Labels:    map[string]string{modelCacheLabel: modelCacheLabelValue},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
}

type execCall struct {
	pod       string
	container string
	command   string
}

// stubCacheExec replaces the exec and wait seams, recording every exec.
func stubCacheExec(t *testing.T, answer execFunc, calls *[]execCall) {
	t.Helper()
	origExec, origWait, origSleep := purgeExec, purgeWait, purgeSleep
	t.Cleanup(func() { purgeExec, purgeWait, purgeSleep = origExec, origWait, origSleep })
	purgeSleep = func(time.Duration) {}
	purgeWait = func(_ context.Context, _ kubernetes.Interface, _, _ string, _ time.Duration) error {
		return nil
	}
	purgeExec = func(_ context.Context, _ *rest.Config, _ kubernetes.Interface,
		_, pod, container string, command []string) (string, error) {
		*calls = append(*calls, execCall{
			pod:       pod,
			container: container,
			command:   strings.Join(command, " "),
		})
		return answer(command)
	}
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what
// it printed.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	defer func() { os.Stderr = orig }()
	fn()
	_ = w.Close()
	os.Stderr = orig
	return <-done
}

// podNames lists the pods in namespace, so a test can assert what was created
// and what was left behind.
func podNames(t *testing.T, clientset *fakeclientset.Clientset, namespace string) []string {
	t.Helper()
	pods, err := clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	names := make([]string, 0, len(pods.Items))
	for _, pod := range pods.Items {
		names = append(names, pod.Name)
	}
	return names
}

// execTarget recovers which pod and container an exec was aimed at. Base's
// inspector path calls execInPod directly, bypassing the seam, and that panics
// on a config with no REST client; recovering records the target all the
// same, which is the fact under test.
func execTarget(fn func()) (pod string, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
		}
	}()
	fn()
	return "", false
}

func TestInspectPVCCacheUsesOwnPodWhenAPodMountsTheClaim(t *testing.T) {
	// The controller-shaped pod the chart's shared mode creates: it mounts
	// the claim and has no shell, so an exec there cannot run `du`. Base
	// borrows this pod; the fix creates its own.
	borrowable := shelllessCachePod()
	k8sClient := fake.NewClientBuilder().
		WithScheme(cacheListScheme()).
		WithObjects(cacheClaim(), borrowable).
		Build()
	// Base lists pods from this client to pick one to borrow; pin that the
	// fixture is the pod it would find.
	listed := &corev1.PodList{}
	if err := k8sClient.List(context.Background(), listed); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(listed.Items) != 1 || listed.Items[0].Name != borrowable.Name {
		t.Fatalf("fixture pods = %d, want only %q", len(listed.Items), borrowable.Name)
	}
	clientset := fakeclientset.NewClientset()

	var calls []execCall
	stubCacheExec(t, func([]string) (string, error) {
		return "4294967296\t/models/a1b2c3d4e5f60718/\n", nil
	}, &calls)

	_, panicked := execTarget(func() {
		_, _ = inspectSinglePVC(context.Background(), &rest.Config{}, k8sClient, clientset, "test-ns",
			PVCInfo{Name: modelCachePVCName})
	})

	if panicked {
		t.Errorf("exec went somewhere other than the exec seam (the borrowed pod %q)",
			borrowable.Name)
	}
	if len(calls) != 1 {
		t.Fatalf("exec calls through the seam = %d, want 1 (the controller pod holds no shell)", len(calls))
	}
	if want := inspectorPodName(modelCachePVCName); calls[0].pod != want {
		t.Errorf("exec ran in pod %q, want the inspector pod %q", calls[0].pod, want)
	}
	if calls[0].container != "inspector" {
		t.Errorf("exec ran in container %q, want %q", calls[0].container, "inspector")
	}

	for _, name := range podNames(t, clientset, "test-ns") {
		if strings.HasPrefix(name, "llmkube-cache-inspector") {
			t.Errorf("inspector pod %q was left behind", name)
		}
	}
}

func TestInspectPVCCacheFailsWhenNoPVCIsReadable(t *testing.T) {
	k8sClient := fake.NewClientBuilder().
		WithScheme(cacheListScheme()).
		WithObjects(cacheClaim()).
		Build()
	clientset := fakeclientset.NewClientset()
	clientset.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pods is forbidden: User cannot create resource pods")
	})

	entries, err := inspectPVCCache(context.Background(), unreachableRESTConfig, k8sClient, "test-ns")
	if err == nil {
		t.Fatalf("inspectPVCCache = %+v, nil; want an error: no cache claim was read", entries)
	}
	if !strings.Contains(err.Error(), modelCachePVCName) {
		t.Errorf("error %q does not name the claim it could not read", err)
	}
}

// pinnedTo lists the nodes a pod's required node affinity accepts.
func pinnedTo(pod *corev1.Pod) []string {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil ||
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return nil
	}
	var nodes []string
	for _, term := range pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == "kubernetes.io/hostname" {
				nodes = append(nodes, expr.Values...)
			}
		}
	}
	return nodes
}

// A strict RWO claim a running pod holds is attached on that pod's node, so
// the inspector has to land there or stall on Multi-Attach. A claim nothing
// holds must not be pinned: the pod is its first consumer.
func TestInspectPVCCachePinsInspectorToTheClaimHolderNode(t *testing.T) {
	withNode := func(name string, phase corev1.PodPhase, node string) *corev1.Pod {
		pod := shelllessCachePod()
		pod.Name = name
		pod.Spec.NodeName = node
		pod.Status.Phase = phase
		return pod
	}

	pendingTainted := withNode("pending-gpu", corev1.PodPending, "node-e")
	pendingTainted.Spec.Tolerations = []corev1.Toleration{{
		Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
	}}

	tainted := withNode("gpu-serving", corev1.PodRunning, "node-d")
	tainted.Spec.Tolerations = []corev1.Toleration{{
		Key: "nvidia.com/gpu", Operator: corev1.TolerationOpEqual, Value: "present",
		Effect: corev1.TaintEffectNoSchedule,
	}}

	tests := []struct {
		name        string
		pods        []*corev1.Pod
		want        []string
		wantTolerat bool
	}{{
		name: "a running holder on node-b",
		pods: []*corev1.Pod{withNode("serving", corev1.PodRunning, "node-b")},
		want: []string{"node-b"},
	}, {
		name:        "a holder on a tainted node carries its toleration",
		pods:        []*corev1.Pod{tainted},
		want:        []string{"node-d"},
		wantTolerat: true,
	}, {
		name: "nothing holds the claim",
		pods: nil,
		want: nil,
	}, {
		name: "a holder still pulling its image holds the claim on its node",
		pods: []*corev1.Pod{withNode("pending", corev1.PodPending, "node-c")},
		want: []string{"node-c"},
	}, {
		name:        "a pending holder on a tainted node is pinned and tolerated",
		pods:        []*corev1.Pod{pendingTainted},
		want:        []string{"node-e"},
		wantTolerat: true,
	}, {
		name: "a holder still shutting down keeps the claim on its node",
		pods: []*corev1.Pod{func() *corev1.Pod {
			p := withNode("graceful", corev1.PodRunning, "node-c")
			now := metav1.Now()
			p.DeletionTimestamp = &now
			p.Finalizers = []string{"test/hold"}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "manager", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}
			return p
		}()},
		want: []string{"node-c"},
	}, {
		name: "a holder being deleted with nothing running is no holder",
		pods: []*corev1.Pod{func() *corev1.Pod {
			p := withNode("terminating", corev1.PodRunning, "node-c")
			now := metav1.Now()
			p.DeletionTimestamp = &now
			// The fake client refuses a deletion timestamp with no finalizer.
			p.Finalizers = []string{"test/hold"}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "manager", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}},
			}}
			return p
		}()},
		want: nil,
	}, {
		name: "a finished pod holds nothing",
		pods: []*corev1.Pod{withNode("finished", corev1.PodSucceeded, "node-c")},
		want: nil,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{cacheClaim()}
			for _, pod := range tt.pods {
				objs = append(objs, pod)
			}
			k8sClient := fake.NewClientBuilder().WithScheme(cacheListScheme()).WithObjects(objs...).Build()

			origExec, origWait, origSleep := purgeExec, purgeWait, purgeSleep
			t.Cleanup(func() { purgeExec, purgeWait, purgeSleep = origExec, origWait, origSleep })
			purgeSleep = func(time.Duration) {}
			purgeWait = func(_ context.Context, _ kubernetes.Interface, _, _ string, _ time.Duration) error {
				return nil
			}
			purgeExec = func(context.Context, *rest.Config, kubernetes.Interface,
				string, string, string, []string) (string, error) {
				return "", nil
			}

			// Keep the pod: recording the delete would leave nothing to read.
			origDelete := deleteCacheAccessPod
			t.Cleanup(func() { deleteCacheAccessPod = origDelete })
			deleteCacheAccessPod = func(context.Context, kubernetes.Interface, string, string) {}

			clientset := fakeclientset.NewClientset()
			if _, err := inspectSinglePVC(context.Background(), &rest.Config{}, k8sClient, clientset,
				"test-ns", PVCInfo{Name: modelCachePVCName}); err != nil {
				t.Fatalf("inspectSinglePVC: %v", err)
			}

			pod, err := clientset.CoreV1().Pods("test-ns").Get(context.Background(),
				inspectorPodName(modelCachePVCName), metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get the inspector pod the run created: %v", err)
			}
			if got := pinnedTo(pod); len(got) != len(tt.want) || (len(got) == 1 && got[0] != tt.want[0]) {
				t.Errorf("inspector pinned to %v, want %v", got, tt.want)
			}
			if tt.wantTolerat {
				if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "nvidia.com/gpu" {
					t.Errorf("inspector tolerations = %v, want the holder's nvidia.com/gpu one",
						pod.Spec.Tolerations)
				}
			}
			// A pod another run holds is refused by this stamp; unstamped it
			// would be deleted instead.
			if stamp := pod.Labels[cachePurgeRunLabel]; stamp == "" {
				t.Error("inspector pod carries no run stamp, so a concurrent run would take it over")
			}
		})
	}
}
