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
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
)

// purgeTestClient is a claim reader that finds no claim, so the purge path
// reads no access mode and proceeds as before #2155.
func purgeTestClient() client.Client {
	return fake.NewClientBuilder().WithScheme(cacheListScheme()).Build()
}

// nilRESTConfig is never dereferenced: the tests replace purgeExec and
// purgeWait, so no SPDY executor or API poll is ever built.
var nilRESTConfig = &rest.Config{}

// fakePurgeExec replaces purgeExec and purgeWait for the duration of a test.
// Every exec'd command is appended to *cmds in order.
func fakePurgeExec(t *testing.T, result execFunc, cmds *[][]string) {
	t.Helper()
	origExec, origWait, origSleep := purgeExec, purgeWait, purgeSleep
	t.Cleanup(func() { purgeExec, purgeWait, purgeSleep = origExec, origWait, origSleep })
	purgeSleep = func(time.Duration) {}
	purgeExec = func(_ context.Context, _ *rest.Config, _ kubernetes.Interface,
		_, _, _ string, command []string) (string, error) {
		*cmds = append(*cmds, command)
		return result(command)
	}
	purgeWait = func(_ context.Context, _ kubernetes.Interface, _, _ string, _ time.Duration) error {
		return nil
	}
}

// execFunc answers one exec'd command.
type execFunc func(command []string) (string, error)

func TestValidateCacheKey(t *testing.T) {
	valid := []string{
		"a3b8c9d4e5f67890",
		"deadbeef01234567",
		"0123456789abcdef",
		"a",
		strings.Repeat("a", 63),
	}
	for _, key := range valid {
		if err := validateCacheKey(key); err != nil {
			t.Errorf("validateCacheKey(%q) = %v, want nil", key, err)
		}
	}

	invalid := []string{
		"",
		".",
		"..",
		"../etc",
		"a/b",
		"/models/abc",
		"-rf",
		".hidden",
		"a b",
		strings.Repeat("a", 64),
	}
	for _, key := range invalid {
		if err := validateCacheKey(key); err == nil {
			t.Errorf("validateCacheKey(%q) = nil, want an error", key)
		}
	}
}

func TestPurgePodName(t *testing.T) {
	a := purgePodName("llmkube-model-cache")
	b := purgePodName("isvc-a-model-cache")
	if a == b {
		t.Fatalf("purge pod names collided: both %q", a)
	}
	for _, name := range []string{a, b} {
		if !strings.HasPrefix(name, cachePurgePodPrefix) {
			t.Errorf("purge pod name %q missing prefix %q", name, cachePurgePodPrefix)
		}
		if len(name) > 63 {
			t.Errorf("purge pod name %q exceeds the 63-char DNS-1123 limit", name)
		}
	}
}

func TestNewPurgePod(t *testing.T) {
	pvcName := "my-isvc-model-cache"
	clientset := fakeclientset.NewClientset()

	podName, err := createPurgePodForPVC(context.Background(), clientset, "test-ns", pvcName, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pod, err := clientset.CoreV1().Pods("test-ns").Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get created pod: %v", err)
	}
	if pod.Labels[managedByLabel] != managedByValue {
		t.Errorf("managed-by label = %q, want %q", pod.Labels[managedByLabel], managedByValue)
	}
	if pod.Labels[modelCacheLabel] != cachePurgeComponent {
		t.Errorf("component label = %q, want %q", pod.Labels[modelCacheLabel], cachePurgeComponent)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy = %q, want %q", pod.Spec.RestartPolicy, corev1.RestartPolicyNever)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("container count = %d, want 1", len(pod.Spec.Containers))
	}

	container := pod.Spec.Containers[0]
	if container.Name != "purge" {
		t.Errorf("container name = %q, want %q", container.Name, "purge")
	}
	if container.Image != "busybox:1.37.0" {
		t.Errorf("image = %q, want %q", container.Image, "busybox:1.37.0")
	}
	if len(container.VolumeMounts) != 1 {
		t.Fatalf("volume mount count = %d, want 1", len(container.VolumeMounts))
	}
	if container.VolumeMounts[0].MountPath != defaultModelMountPath {
		t.Errorf("mount path = %q, want %q", container.VolumeMounts[0].MountPath, defaultModelMountPath)
	}
	if container.VolumeMounts[0].ReadOnly {
		t.Error("purge mount must be read-write")
	}
	if len(pod.Spec.Volumes) != 1 {
		t.Fatalf("volume count = %d, want 1", len(pod.Spec.Volumes))
	}
	vol := pod.Spec.Volumes[0]
	if vol.PersistentVolumeClaim == nil {
		t.Fatal("volume PVC source is nil")
	}
	if vol.PersistentVolumeClaim.ClaimName != pvcName {
		t.Errorf("PVC claim = %q, want %q", vol.PersistentVolumeClaim.ClaimName, pvcName)
	}
	if vol.PersistentVolumeClaim.ReadOnly {
		t.Error("PVC volume source must be read-write")
	}
}

func TestEnsurePurgePod_ReplacesStale(t *testing.T) {
	pvcName := "llmkube-model-cache"
	podName := purgePodName(pvcName)
	stale := newCacheAccessPodForRun(podName, pvcName, cachePurgeComponent, "purge", false, "")
	stale.Namespace = "default"
	// A pod with no stamp (no run label) is the interrupted-run case, which
	// purgePodAbandoned reads as abandoned (#2140 refactor: behavior preserved).
	stale.CreationTimestamp = metav1.NewTime(time.Now().Add(-11 * time.Minute))
	clientset := fakeclientset.NewClientset(stale)

	// The delete is asynchronous on a real API server: the pod keeps its name
	// until the kubelet acks it. This fake scripts exactly that, keeping the
	// pod readable for two polls after the delete.
	var (
		deleting    bool
		lingering   int
		seenGone    bool
		earlyCreate int
	)
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if deleting && !seenGone {
			earlyCreate++
			return true, nil, apierrors.NewAlreadyExists(
				corev1.SchemeGroupVersion.WithResource("pods").GroupResource(), podName)
		}
		return false, nil, nil
	})
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.GetAction).GetName() != podName {
			return false, nil, nil
		}
		if !deleting {
			return true, stale.DeepCopy(), nil
		}
		lingering++
		if lingering <= 2 {
			pod := stale.DeepCopy()
			pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			return true, pod, nil
		}
		seenGone = true
		return true, nil, apierrors.NewNotFound(
			corev1.SchemeGroupVersion.WithResource("pods").GroupResource(), podName)
	})

	origDelete, origSleep := deleteCacheAccessPod, purgeSleep
	t.Cleanup(func() { deleteCacheAccessPod, purgeSleep = origDelete, origSleep })
	deleteCacheAccessPod = func(ctx context.Context, c kubernetes.Interface, ns, name string) {
		deleting = true
		origDelete(ctx, c, ns, name)
	}
	purgeSleep = func(time.Duration) {}

	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) { return "", nil }, &cmds)

	got, err := ensurePurgePodReleasing(context.Background(), purgeTestClient(), clientset, "default",
		purgeTarget{Name: pvcName}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != podName {
		t.Errorf("pod name = %q, want %q", got, podName)
	}
	if earlyCreate > 0 {
		t.Errorf("recreated the purge pod %d time(s) before the stale one was gone", earlyCreate)
	}
}

func TestEnsurePurgePod_RefusesForeignPod(t *testing.T) {
	pvcName := "llmkube-model-cache"
	foreign := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      purgePodName(pvcName),
			Namespace: "default",
			Labels:    map[string]string{"app": "someone-else"},
		},
	}
	clientset := fakeclientset.NewClientset(foreign)

	// No pod can start in the fake, so the wait is stubbed to whatever the
	// guard lets through: without the guard this test must fail on the
	// refusal, not hang.
	origWait, origSleep := purgeWait, purgeSleep
	t.Cleanup(func() { purgeWait, purgeSleep = origWait, origSleep })
	purgeSleep = func(time.Duration) {}
	purgeWait = func(_ context.Context, _ kubernetes.Interface, _, _ string, _ time.Duration) error {
		return nil
	}

	_, err := ensurePurgePodReleasing(context.Background(), purgeTestClient(), clientset, "default",
		purgeTarget{Name: pvcName}, nil)
	if err == nil {
		t.Fatal("expected an error for a foreign pod at the purge pod name")
	}
	if !strings.Contains(err.Error(), "refusing to delete") {
		t.Errorf("error = %q, want it to refuse the foreign pod", err)
	}
	if _, getErr := clientset.CoreV1().Pods("default").Get(
		context.Background(), purgePodName(pvcName), metav1.GetOptions{}); getErr != nil {
		t.Errorf("foreign pod was deleted: %v", getErr)
	}
}

func TestDeleteCachePaths_RemovesValidatedPaths(t *testing.T) {
	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) {
		if strings.HasPrefix(command[2], "for p in") {
			return "removed /models/aaa\nabsent /models/bbb\n", nil
		}
		return "", nil
	}, &cmds)
	clientset := fakeclientset.NewClientset()

	removed, absent, err := deleteCachePaths(context.Background(), nilRESTConfig, clientset,
		"default", "pod", []string{"/models/aaa", "/models/bbb"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(removed) != 1 || removed[0] != "/models/aaa" {
		t.Errorf("removed = %v, want [/models/aaa]", removed)
	}
	if len(absent) != 1 || absent[0] != "/models/bbb" {
		t.Errorf("absent = %v, want [/models/bbb]", absent)
	}
	if len(cmds) != 2 {
		t.Fatalf("exec count = %d, want 2 (remove then verify)", len(cmds))
	}
	if !strings.Contains(cmds[0][2], "'/models/aaa' '/models/bbb'") {
		t.Errorf("remove command = %q, want both validated paths", cmds[0][2])
	}
	if strings.Contains(cmds[0][2], "*") {
		t.Errorf("remove command = %q, want no glob", cmds[0][2])
	}
}

func TestDeleteCachePaths_StillPresent(t *testing.T) {
	fakePurgeExec(t, func(command []string) (string, error) {
		if strings.Contains(command[2], "echo \"still $p\"") {
			return "still /models/aaa\n", nil
		}
		return "removed /models/aaa\n", nil
	}, &[][]string{})
	clientset := fakeclientset.NewClientset()

	_, _, err := deleteCachePaths(context.Background(), nilRESTConfig, clientset,
		"default", "pod", []string{"/models/aaa"})
	if err == nil {
		t.Fatal("expected an error when the entry survives the rm")
	}
	if !strings.Contains(err.Error(), "/models/aaa") {
		t.Errorf("error = %q, want it to name the surviving path", err)
	}
}

func TestDeleteCachePaths_ExecError(t *testing.T) {
	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) {
		return "", fmt.Errorf("exec failed: container not running")
	}, &cmds)
	clientset := fakeclientset.NewClientset()

	_, _, err := deleteCachePaths(context.Background(), nilRESTConfig, clientset,
		"default", "pod", []string{"/models/aaa"})
	if err == nil {
		t.Fatal("expected the exec error to be returned, not swallowed")
	}
}

func TestPurgeCacheKeys_CreatesAndCleansUp(t *testing.T) {
	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) {
		if len(command) < 3 || command[1] != "-c" {
			return "", nil
		}
		if strings.Contains(command[2], "ls -1") {
			return "", nil
		}
		return "removed /models/deadbeef01234567\n", nil
	}, &cmds)
	clientset := fakeclientset.NewClientset()

	outcome, err := purgeCacheKeys(context.Background(), nilRESTConfig, purgeTestClient(), clientset,
		"default", purgeTarget{Name: "llmkube-model-cache"}, []string{"deadbeef01234567"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(outcome.Removed) != 1 || outcome.Removed[0] != "/models/deadbeef01234567" {
		t.Errorf("removed = %v, want [/models/deadbeef01234567]", outcome.Removed)
	}
	if _, err := clientset.CoreV1().Pods("default").Get(context.Background(),
		purgePodName("llmkube-model-cache"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("purge pod was not deleted: err = %v", err)
	}
}

func TestPurgeCacheKeys_RejectsTraversal(t *testing.T) {
	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) {
		return "", nil
	}, &cmds)
	clientset := fakeclientset.NewClientset()

	_, err := purgeCacheKeys(context.Background(), nilRESTConfig, purgeTestClient(), clientset,
		"default", purgeTarget{Name: "llmkube-model-cache"}, []string{"../etc/passwd"})
	if err == nil {
		t.Fatal("expected an error for a key that is not one path segment")
	}
	pods, _ := clientset.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("a pod was created for an unvalidated key: %d pods", len(pods.Items))
	}
	if len(cmds) != 0 {
		t.Errorf("exec ran for an unvalidated key: %v", cmds)
	}
}

func TestPurgeCacheKeys_Absent(t *testing.T) {
	fakePurgeExec(t, func(command []string) (string, error) {
		return "absent /models/deadbeef01234567\n", nil
	}, &[][]string{})
	clientset := fakeclientset.NewClientset()

	outcome, err := purgeCacheKeys(context.Background(), nilRESTConfig, purgeTestClient(), clientset,
		"default", purgeTarget{Name: "llmkube-model-cache"}, []string{"deadbeef01234567"})
	if err != nil {
		t.Fatalf("an absent entry must not be an error: %v", err)
	}
	if len(outcome.Removed) != 0 {
		t.Errorf("removed = %v, want none", outcome.Removed)
	}
	if len(outcome.Absent) != 1 {
		t.Errorf("absent = %v, want one path", outcome.Absent)
	}
}

// purgeScripts records every rm command run through purgeListed.
func capturePurgeScripts(t *testing.T) *[]string {
	t.Helper()
	var rmCmds []string
	fakePurgeExec(t, func(command []string) (string, error) {
		if len(command) < 3 || command[1] != "-c" {
			return "", nil
		}
		if strings.Contains(command[2], "ls -1 /models") {
			return "deadbeef01234567\n0123456789abcdef\nkeep-me.txt\nit's here\n", nil
		}
		rmCmds = append(rmCmds, command[2])
		return "", nil
	}, &[][]string{})
	return &rmCmds
}

func lastRMCmd(t *testing.T, cmds []string) string {
	t.Helper()
	if len(cmds) == 0 {
		t.Fatal("no rm command was run")
	}
	return cmds[len(cmds)-1]
}

func TestPurgeOrphanedEntries(t *testing.T) {
	cmds := capturePurgeScripts(t)
	clientset := fakeclientset.NewClientset()

	outcome, err := purgeOrphanedEntries(context.Background(), nilRESTConfig, purgeTestClient(), clientset,
		"default", purgeTarget{Name: "llmkube-model-cache"}, map[string]bool{"0123456789abcdef": true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cmd := lastRMCmd(t, *cmds)
	if !strings.Contains(cmd, "'/models/deadbeef01234567'") {
		t.Errorf("orphaned entry was not removed: %q", cmd)
	}
	if strings.Contains(cmd, "'/models/0123456789abcdef'") {
		t.Errorf("active entry was removed: %q", cmd)
	}
	if strings.Join(outcome.Skipped, ",") != "keep-me.txt,it's here" {
		t.Errorf("skipped = %v, want [keep-me.txt it's here]", outcome.Skipped)
	}
}

func TestPurgeAllEntries_OperatorClaim(t *testing.T) {
	cmds := capturePurgeScripts(t)
	clientset := fakeclientset.NewClientset()

	if _, err := purgeAllEntries(context.Background(), nilRESTConfig, purgeTestClient(), clientset,
		"default", purgeTarget{Name: "llmkube-model-cache"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cmd := lastRMCmd(t, *cmds)
	for _, want := range []string{"'/models/deadbeef01234567'", "'/models/0123456789abcdef'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("operator claim: %s missing from %q", want, cmd)
		}
	}
	if strings.Contains(cmd, "keep-me.txt") || strings.Contains(cmd, "it's") {
		t.Errorf("operator claim: a non-key name reached the shell: %q", cmd)
	}
}

func TestPurgeAllEntries_UserClaim(t *testing.T) {
	cmds := capturePurgeScripts(t)
	clientset := fakeclientset.NewClientset()

	outcome, err := purgeAllEntries(context.Background(), nilRESTConfig, purgeTestClient(), clientset,
		"default", purgeTarget{Name: "user-claim", UserOwned: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cmd := lastRMCmd(t, *cmds)
	if !strings.Contains(cmd, "'/models/deadbeef01234567'") ||
		!strings.Contains(cmd, "'/models/0123456789abcdef'") {
		t.Errorf("user claim: cache keys not removed: %q", cmd)
	}
	if strings.Contains(cmd, "keep-me.txt") {
		t.Errorf("user claim: non-key entry was removed: %q", cmd)
	}
	if strings.Join(outcome.Skipped, ",") != "keep-me.txt,it's here" {
		t.Errorf("skipped = %v, want [keep-me.txt it's here]", outcome.Skipped)
	}
}

func newCLIScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = inferencev1alpha1.AddToScheme(s)
	return s
}

func TestCachePVCandidates(t *testing.T) {
	shared := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      "llmkube-model-cache",
		Namespace: "default",
	}}
	perIsvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      "labeled-model-cache",
		Namespace: "default",
		Labels:    map[string]string{modelCacheLabel: modelCacheLabelValue},
	}}
	userClaim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      "my-cache-claim",
		Namespace: "default",
	}}
	otherClaim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      "other-cache-claim",
		Namespace: "default",
	}}

	named := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "model-a", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:   "model-a",
			ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "my-cache-claim"},
		},
	}
	refs := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-b", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:   "model-a",
			ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "my-cache-claim"},
		},
	}
	unrelated := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-c", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:   "model-c",
			ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "other-cache-claim"},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(newCLIScheme()).
		WithObjects(shared, perIsvc, userClaim, otherClaim, named, refs, unrelated).
		Build()

	got, _, err := cachePVCandidates(context.Background(), k8sClient, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"labeled-model-cache", "llmkube-model-cache", "my-cache-claim", "other-cache-claim"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("candidates = %v, want %v (the discovered caches plus every claimName override)", got, want)
	}

	all, _, err := cachePVCandidates(context.Background(), k8sClient, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(all, ",") != strings.Join(want, ",") {
		t.Errorf("candidates = %v, want the discovered caches plus the override", all)
	}
}

// TestResolvePurgeTargets_InheritsOnlyTheReferencedModelsClaim drives the
// real listing: a service that shares a model's name but references another
// Model must not bring that other Model's claim into the purge, while a
// service that references the model and has no block of its own does inherit
// the model's claim.
func TestResolvePurgeTargets_InheritsOnlyTheReferencedModelsClaim(t *testing.T) {
	const ns = "default"
	pvc := func(name string, labeled bool) *corev1.PersistentVolumeClaim {
		p := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		if labeled {
			p.Labels = map[string]string{modelCacheLabel: modelCacheLabelValue}
		}
		return p
	}
	model := func(name, claim string, staged ...string) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       inferencev1alpha1.ModelSpec{Source: "hf://org/" + name},
		}
		if claim != "" {
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: claim}
		}
		for _, c := range staged {
			m.Status.Prefetch = append(m.Status.Prefetch,
				inferencev1alpha1.PrefetchStatus{ClaimName: c, Phase: inferencev1alpha1.PrefetchPhaseSucceeded})
		}
		return m
	}
	svc := func(name, modelRef string) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: modelRef},
		}
	}
	k8sClient := fake.NewClientBuilder().WithScheme(newCLIScheme()).WithObjects(
		pvc(modelCachePVCName, true), pvc("other-claim", false), pvc("qwen-claim", false),
		model("other", "other-claim"), model("qwen", "qwen-claim", modelCachePVCName),
		svc("qwen", "other"), svc("svc-a", "qwen"),
	).Build()
	ctx := context.Background()

	targets, svcs, err := resolvePurgeTargets(ctx, k8sClient, ns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	names := func(targets []purgeTarget) []string {
		out := make([]string, 0, len(targets))
		for _, tgt := range targets {
			out = append(out, tgt.Name)
		}
		return out
	}
	qwenClaim, qwenStaged := modelCacheClaims(ctx, k8sClient, ns, "qwen")
	if !slices.Equal(qwenStaged, []string{modelCachePVCName}) {
		t.Errorf("modelCacheClaims staged = %v, want the status.prefetch claim %s", qwenStaged, modelCachePVCName)
	}
	forQwen := names(targetedClaims(targets, svcs, "qwen", qwenClaim, qwenStaged))
	if !slices.Contains(forQwen, modelCachePVCName) {
		t.Errorf("clearing qwen targets %v, want %s too: status.prefetch records a staging there", forQwen, modelCachePVCName)
	}
	if slices.Contains(forQwen, "other-claim") {
		t.Errorf("clearing qwen targets %v: other-claim belongs to Model other, "+
			"which service qwen references but model qwen does not own", forQwen)
	}
	if !slices.Contains(forQwen, "qwen-claim") {
		t.Errorf("clearing qwen targets %v, want qwen-claim: "+
			"svc-a references qwen with no block and inherits its claim", forQwen)
	}
	otherClaim, otherStaged := modelCacheClaims(ctx, k8sClient, ns, "other")
	forOther := names(targetedClaims(targets, svcs, "other", otherClaim, otherStaged))
	if !slices.Contains(forOther, "other-claim") || slices.Contains(forOther, "qwen-claim") {
		t.Errorf("clearing other targets %v, want other-claim and not qwen-claim", forOther)
	}
}

func TestSeedCacheEntries(t *testing.T) {
	metal := inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "metal-model", Namespace: "ns"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:   "https://example.com/a.gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}
	first := inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "ns"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://org/repo",
			Files:  []string{"model.gguf", "model-00001-of-00002.gguf"},
		},
		Status: inferencev1alpha1.ModelStatus{Size: "7.2 GiB"},
	}
	second := *first.DeepCopy()
	second.Name = "m2"

	entries := map[string]*CacheEntry{}
	seedCacheEntries([]inferencev1alpha1.Model{metal, first, second}, false, entries)

	key := cachekey.Compute("hf://org/repo")
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (metal model skipped, shared key merged)", len(entries))
	}
	entry := entries[key]
	if entry == nil {
		t.Fatalf("missing entry for derived key %q", key)
	}
	if entry.Status != statusActive {
		t.Errorf("status = %q, want %q", entry.Status, statusActive)
	}
	if strings.Join(entry.ModelNames, ",") != "m1,m2" {
		t.Errorf("model names = %v, want [m1 m2]", entry.ModelNames)
	}
	if entry.SizeHuman != "7.2 GiB" {
		t.Errorf("size = %q, want %q", entry.SizeHuman, "7.2 GiB")
	}

	qualified := map[string]*CacheEntry{}
	seedCacheEntries([]inferencev1alpha1.Model{first}, true, qualified)
	if names := qualified[key].ModelNames; len(names) != 1 || names[0] != "ns/m1" {
		t.Errorf("all-namespaces names = %v, want [ns/m1]", names)
	}
}

// TestPurgeKey_MultiFileModel guards the key `cache clear --model` resolves:
// an hf:// repo model with spec.files leaves Status.CacheKey empty, so reading
// that field alone would refuse a model that is demonstrably on the cache.
func TestPurgeKey_MultiFileModel(t *testing.T) {
	model := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://org/repo",
			Files:  []string{"model.gguf", "model-00001-of-00002.gguf"},
		},
	}
	key, err := purgeKey(model, "repo-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := cachekey.Compute("hf://org/repo"); key != want {
		t.Errorf("purgeKey = %q, want the derived key %q", key, want)
	}

	metal := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source:   "https://example.com/a.gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}
	if _, err := purgeKey(metal, "metal-model"); err == nil {
		t.Error("expected an error for a model with no cache key")
	}
}

func TestTargetedClaims(t *testing.T) {
	shared := purgeTarget{Name: "llmkube-model-cache"}
	overrideClaim := purgeTarget{Name: "isvc-cache", UserOwned: true}
	ownPerService := purgeTarget{Name: "chat-api-model-cache"}
	unrelatedPerService := purgeTarget{Name: "other-isvc-model-cache"}

	svc := func(name, modelRef, claim string) inferencev1alpha1.InferenceService {
		s := inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: modelRef},
		}
		if claim != "" {
			s.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: claim}
		}
		return s
	}

	modelClaim := purgeTarget{Name: "model-cache", UserOwned: true}
	svcWithBlock := func(name, modelRef string) inferencev1alpha1.InferenceService {
		s := svc(name, modelRef, "")
		s.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{Prefetch: true}
		return s
	}

	tests := []struct {
		name       string
		all        []purgeTarget
		svcs       []inferencev1alpha1.InferenceService
		model      string
		modelClaim string
		staged     []string
		want       []string
	}{
		{
			// The controller's sticky target: the Model staged into the shared
			// cache before the service and its claim appeared, and keeps
			// staging there, so the files are on both.
			name:   "the claims status.prefetch records are targets beside the service's",
			all:    []purgeTarget{shared, overrideClaim, unrelatedPerService},
			svcs:   []inferencev1alpha1.InferenceService{svc("qwen", "qwen", "isvc-cache")},
			model:  "qwen",
			staged: []string{"llmkube-model-cache"},
			want:   []string{"llmkube-model-cache", "isvc-cache"},
		},
		{
			// #2136: the Model names the claim and the service inherits it;
			// the service's own spec says nothing.
			name:       "a Model's claimName is the target with or without a service",
			all:        []purgeTarget{shared, modelClaim, unrelatedPerService},
			svcs:       []inferencev1alpha1.InferenceService{svc("qwen", "qwen", "")},
			model:      "qwen",
			modelClaim: "model-cache",
			want:       []string{"model-cache"},
		},
		{
			// A block of its own, even one naming no claim, mounts the
			// default claim, as the controller resolves it.
			name:       "a service with its own modelCache block does not inherit the Model's claim",
			all:        []purgeTarget{shared, modelClaim},
			svcs:       []inferencev1alpha1.InferenceService{svcWithBlock("qwen", "qwen")},
			model:      "qwen",
			modelClaim: "model-cache",
			want:       []string{"llmkube-model-cache", "model-cache"},
		},
		{
			name:       "a service named after the model but referencing another does not inherit",
			all:        []purgeTarget{shared, modelClaim},
			svcs:       []inferencev1alpha1.InferenceService{svc("qwen", "other-model", "")},
			model:      "qwen",
			modelClaim: "model-cache",
			want:       []string{"llmkube-model-cache", "model-cache"},
		},
		{
			name:       "a prefetch-era Model with no service targets its own claim, not the shared cache",
			all:        []purgeTarget{shared, modelClaim},
			model:      "qwen",
			modelClaim: "model-cache",
			want:       []string{"model-cache"},
		},
		{
			name:  "claimName override targets the override only",
			all:   []purgeTarget{shared, overrideClaim, unrelatedPerService},
			svcs:  []inferencev1alpha1.InferenceService{svc("my-model", "my-model", "isvc-cache")},
			model: "my-model",
			want:  []string{"isvc-cache"},
		},
		{
			name: "unrelated per-service claims stay untouched",
			all:  []purgeTarget{shared, overrideClaim, unrelatedPerService},
			svcs: []inferencev1alpha1.InferenceService{
				svc("my-model", "my-model", "isvc-cache"),
				svc("other-isvc", "other-model", ""),
			},
			model: "my-model",
			want:  []string{"isvc-cache"},
		},
		{
			// Every matching service sits on its own claim, so the model never
			// reached the shared cache; a pod there would only stall on RWO
			// attach while a shared-mode service holds it.
			name:  "shared claim is skipped when every matching service is per-service",
			all:   []purgeTarget{shared, ownPerService},
			svcs:  []inferencev1alpha1.InferenceService{svc("chat-api", "llama", "")},
			model: "llama",
			want:  []string{"chat-api-model-cache"},
		},
		{
			// The default install: shared mode, so no per-service claim exists
			// and the serving service's pods mount llmkube-model-cache.
			name:  "shared mode serves from the shared claim",
			all:   []purgeTarget{shared},
			svcs:  []inferencev1alpha1.InferenceService{svc("chat-api", "llama", "")},
			model: "llama",
			want:  []string{"llmkube-model-cache"},
		},
		{
			// One matching service brings an override, the other has no
			// per-service claim and so serves from the shared cache.
			name: "a matching service without a claimName and no per-service claim uses the shared claim",
			all:  []purgeTarget{shared, overrideClaim},
			svcs: []inferencev1alpha1.InferenceService{
				svc("chat-api", "llama", "isvc-cache"),
				svc("chat-worker", "llama", ""),
			},
			model: "llama",
			want:  []string{"llmkube-model-cache", "isvc-cache"},
		},
		{
			// No InferenceService to consult (prefetch-era Model), so the
			// shared cache is the only possible home.
			name:  "no matching service falls back to the shared claim",
			all:   []purgeTarget{shared, unrelatedPerService},
			svcs:  []inferencev1alpha1.InferenceService{svc("other-isvc", "other-model", "")},
			model: "my-model",
			want:  []string{"llmkube-model-cache"},
		},
		{
			// The per-service claim is named after the InferenceService, not
			// the Model (controller controller modelCachePVCName:
			// "<isvc>-model-cache").
			name:  "per-service claim is named after the service that serves the model",
			all:   []purgeTarget{ownPerService, unrelatedPerService},
			svcs:  []inferencev1alpha1.InferenceService{svc("chat-api", "llama", "")},
			model: "llama",
			want:  []string{"chat-api-model-cache"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := targetedClaims(tt.all, tt.svcs, tt.model, tt.modelClaim, tt.staged)
			var names []string
			for _, target := range got {
				names = append(names, target.Name)
			}
			if strings.Join(names, ",") != strings.Join(tt.want, ",") {
				t.Errorf("targetedClaims = %v, want %v", names, tt.want)
			}
		})
	}
}

// TestDiscoveredClaims pins the namespace-wide scope: `cache clear` and
// `--orphaned` act on the claims `cache list` discovers, so a spec.modelCache
// .claimName, which may be an RWO volume a serving pod holds, is never
// swept by them.
func TestDiscoveredClaims(t *testing.T) {
	all := []purgeTarget{
		{Name: "llmkube-model-cache"},
		{Name: "isvc-model-cache"},
		{Name: "user-claim", UserOwned: true},
	}
	got := discoveredClaims(all)
	names := make([]string, 0, len(got))
	for _, target := range got {
		names = append(names, target.Name)
	}
	if strings.Join(names, ",") != "llmkube-model-cache,isvc-model-cache" {
		t.Errorf("discoveredClaims = %v, want the operator-managed claims only", names)
	}
}

// TestReplaceStalePurgePod_LiveRunLeftAlone pins the ownership rule for a
// purge pod another invocation is using: two `cache clear` runs in one
// namespace share the deterministic pod name, so a stamped pod must only be
// replaced once it is old enough to be a leftover, never while it is live.
func TestReplaceStalePurgePod_LiveRunLeftAlone(t *testing.T) {
	pvcName := "llmkube-model-cache"
	podName := purgePodName(pvcName)
	live := newCacheAccessPodForRun(podName, pvcName, cachePurgeComponent, "purge", false, "other-run")
	live.Namespace = "default"
	live.CreationTimestamp = metav1.NewTime(time.Now())
	clientset := fakeclientset.NewClientset(live)

	origDelete, origSleep := deleteCacheAccessPod, purgeSleep
	t.Cleanup(func() { deleteCacheAccessPod, purgeSleep = origDelete, origSleep })
	var deletes int
	deleteCacheAccessPod = func(ctx context.Context, c kubernetes.Interface, ns, name string) {
		deletes++
		origDelete(ctx, c, ns, name)
	}
	purgeSleep = func(time.Duration) {}

	err := replaceStalePurgePod(context.Background(), clientset, "default", pvcName, podName, "this-run")
	if err == nil {
		t.Fatal("expected an error rather than replacing a live run's pod")
	}
	if !strings.Contains(err.Error(), "another llmkube cache purge run") {
		t.Errorf("error = %q, want it to name the concurrent run", err)
	}
	if deletes != 0 {
		t.Errorf("a live run's purge pod was deleted %d time(s)", deletes)
	}

	// The same pod is replaced once it has outlived any plausible purge.
	pod, _ := clientset.CoreV1().Pods("default").Get(context.Background(), podName, metav1.GetOptions{})
	if pod == nil {
		t.Fatal("live pod vanished")
	}
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	_, _ = clientset.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{})
	if err := replaceStalePurgePod(context.Background(), clientset, "default", pvcName, podName, "this-run"); err != nil {
		t.Errorf("an abandoned stamped pod must be replaced: %v", err)
	}
}

// TestReplaceStalePurgePod_FinishedPodReplaced pins the other abandoned case:
// a fresh pod whose sleep has already exited (Succeeded or Failed) holds
// nothing, so it is replaced instead of blocking the next run for ten minutes.
func TestReplaceStalePurgePod_FinishedPodReplaced(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed} {
		t.Run(string(phase), func(t *testing.T) {
			pvcName := "llmkube-model-cache"
			podName := purgePodName(pvcName)
			finished := newCacheAccessPodForRun(podName, pvcName, cachePurgeComponent, "purge", false, "other-run")
			finished.Namespace = "default"
			finished.CreationTimestamp = metav1.NewTime(time.Now())
			finished.Status.Phase = phase
			clientset := fakeclientset.NewClientset(finished)

			origSleep := purgeSleep
			t.Cleanup(func() { purgeSleep = origSleep })
			purgeSleep = func(time.Duration) {}

			err := replaceStalePurgePod(context.Background(), clientset, "default", pvcName, podName, "this-run")
			if err != nil {
				t.Fatalf("a finished stamped pod must be replaced: %v", err)
			}
		})
	}
}
