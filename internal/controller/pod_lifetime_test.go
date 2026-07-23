package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestReconcilePodLifetimeRequeuesUnexpiredPod(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	start := now.Add(-5 * time.Second)
	r, isvc, pod := lifetimeReconciler(t, start, 30*time.Second)

	requeue, err := r.reconcilePodLifetime(context.Background(), isvc, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := 25 * time.Second; requeue != want {
		t.Fatalf("requeue = %s, want %s", requeue, want)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Fatalf("unexpired pod was deleted: %v", err)
	}
}

func TestReconcilePodLifetimeDeletesOldestExpiredPod(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	r, isvc, first := lifetimeReconcilerWith(t, now.Add(-2*time.Minute), time.Minute, 2, nil)
	second := first.DeepCopy()
	second.Name = "second"
	second.UID = "pod-second"
	second.ResourceVersion = ""
	second.Status.StartTime = &metav1.Time{Time: now.Add(-90 * time.Second)}
	if err := r.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingClient{Client: r.Client}
	r.Client = recorder
	if _, err := r.reconcilePodLifetime(context.Background(), isvc, false, now); err != nil {
		t.Fatal(err)
	}
	if len(recorder.evictions) != 1 {
		t.Fatalf("eviction calls = %d", len(recorder.evictions))
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(second), &corev1.Pod{}); err != nil {
		t.Fatalf("second pod was deleted too: %v", err)
	}
}

func TestReconcilePodLifetimeIgnoresTerminalPods(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	r, isvc, active := lifetimeReconciler(t, now.Add(-2*time.Minute), time.Minute)
	terminal := active.DeepCopy()
	terminal.Name = "succeeded"
	terminal.UID = "pod-succeeded"
	terminal.ResourceVersion = ""
	terminal.Status.Phase = corev1.PodSucceeded
	terminal.Status.StartTime = nil
	if err := r.Create(context.Background(), terminal); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingClient{Client: r.Client}
	r.Client = recorder
	if _, err := r.reconcilePodLifetime(context.Background(), isvc, false, now); err != nil {
		t.Fatal(err)
	}
	if len(recorder.evictions) != 1 {
		t.Fatalf("eviction calls = %d, want 1", len(recorder.evictions))
	}
}

func TestReconcilePodLifetimeNoopWhenUnsetOrMetal(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	r, isvc, _ := lifetimeReconciler(t, now.Add(-2*time.Minute), time.Minute)
	isvc.Spec.MaxPodLifetimeSeconds = nil
	if got, err := r.reconcilePodLifetime(context.Background(), isvc, false, now); err != nil || got != 0 {
		t.Fatalf("unset lifetime returned %s, %v", got, err)
	}
	seconds := int64(60)
	isvc.Spec.MaxPodLifetimeSeconds = &seconds
	if got, err := r.reconcilePodLifetime(context.Background(), isvc, true, now); err != nil || got != 0 {
		t.Fatalf("metal lifetime returned %s, %v", got, err)
	}
}

func TestReconcilePodLifetimeBlocksUnstablePodsAndDeployment(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	for name, mutate := range map[string]func(*appsv1.Deployment, *corev1.Pod){
		"terminating": func(_ *appsv1.Deployment, p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{Time: now} },
		"unready":     func(_ *appsv1.Deployment, p *corev1.Pod) { p.Status.Conditions[0].Status = corev1.ConditionFalse },
		"no start":    func(_ *appsv1.Deployment, p *corev1.Pod) { p.Status.StartTime = nil },
		"unobserved":  func(d *appsv1.Deployment, _ *corev1.Pod) { d.Status.ObservedGeneration = d.Generation - 1 },
		"replicas":    func(d *appsv1.Deployment, _ *corev1.Pod) { d.Status.Replicas = 0 },
		"updated":     func(d *appsv1.Deployment, _ *corev1.Pod) { d.Status.UpdatedReplicas = 0 },
		"ready":       func(d *appsv1.Deployment, _ *corev1.Pod) { d.Status.ReadyReplicas = 0 },
		"available":   func(d *appsv1.Deployment, _ *corev1.Pod) { d.Status.AvailableReplicas = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			var podMutator func(*corev1.Pod)
			if name == "terminating" {
				podMutator = func(p *corev1.Pod) {
					p.DeletionTimestamp = &metav1.Time{Time: now}
					p.Finalizers = []string{"test/finalizer"}
				}
			}
			r, isvc, pod := lifetimeReconcilerWith(t, now.Add(-2*time.Minute), time.Minute, 1, podMutator)
			deployment := &appsv1.Deployment{}
			if err := r.Get(context.Background(), types.NamespacedName{Name: "svc", Namespace: "ns"}, deployment); err != nil {
				t.Fatal(err)
			}
			mutate(deployment, pod)
			if err := r.Status().Update(context.Background(), deployment); err != nil {
				t.Fatal(err)
			}
			if err := r.Status().Update(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			if _, err := r.reconcilePodLifetime(context.Background(), isvc, false, now); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
				t.Fatalf("pod was deleted: %v", err)
			}
		})
	}
}

// A pod the Deployment does not own but which shares its selector makes the
// active count disagree with Status.Replicas. Recycling must hold rather than
// evict something the operator cannot account for.
func TestReconcilePodLifetimeHoldsOnForeignPod(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	r, isvc, pod := lifetimeReconciler(t, now.Add(-2*time.Minute), time.Minute)
	foreign := pod.DeepCopy()
	foreign.Name = "foreign"
	foreign.UID = "foreign-uid"
	foreign.ResourceVersion = ""
	foreign.OwnerReferences[0].UID = "other-rs"
	if err := r.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingClient{Client: r.Client}
	r.Client = recorder
	if _, err := r.reconcilePodLifetime(context.Background(), isvc, false, now); err != nil {
		t.Fatal(err)
	}
	if len(recorder.evictions) != 0 {
		t.Fatalf("eviction calls = %d, want 0", len(recorder.evictions))
	}
}

// The pod starts 2 minutes ago with a 1 minute lifetime, so by now it has been
// overdue for a minute: a 30s idle timeout is spent, a 300s one is not.
func TestReconcilePodLifetimeWaitsForIdle(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		busy        bool
		idleTimeout *int64
		wantEvicted bool
	}{
		"idle backend recycles":                {busy: false, wantEvicted: true},
		"busy backend waits indefinitely":      {busy: true, wantEvicted: false},
		"busy backend waits within budget":     {busy: true, idleTimeout: ptr.To(int64(300)), wantEvicted: false},
		"busy backend recycles once overdue":   {busy: true, idleTimeout: ptr.To(int64(30)), wantEvicted: true},
		"zero timeout recycles without asking": {busy: true, idleTimeout: ptr.To(int64(0)), wantEvicted: true},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/slots" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `[{"id":0,"is_processing":%t}]`, tc.busy)
			}))
			defer server.Close()

			r, isvc, _ := lifetimeReconciler(t, now.Add(-2*time.Minute), time.Minute)
			isvc.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{WaitForIdle: true}
			isvc.Spec.MaxPodLifetimeIdleTimeoutSeconds = tc.idleTimeout
			r.RolloutIdleBaseURL = server.URL
			recorder := &recordingClient{Client: r.Client}
			r.Client = recorder

			requeue, err := r.reconcilePodLifetime(context.Background(), isvc, false, now)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.wantEvicted {
				want = 1
			}
			if len(recorder.evictions) != want {
				t.Fatalf("eviction calls = %d, want %d", len(recorder.evictions), want)
			}
			if !tc.wantEvicted && requeue != inferencev1alpha1.DefaultIdleCheckInterval {
				t.Fatalf("requeue = %s, want %s", requeue, inferencev1alpha1.DefaultIdleCheckInterval)
			}
		})
	}
}

func TestEarliestPositive(t *testing.T) {
	if got := earliestPositive(0, 5*time.Second, 2*time.Second, -time.Second); got != 2*time.Second {
		t.Fatalf("got %s", got)
	}
	if got := earliestPositive(0, -time.Second); got != 0 {
		t.Fatalf("got %s", got)
	}
}

// recordingClient captures the evictions the controller submits and can fail
// them, so tests can assert on the request the apiserver would have seen.
type recordingClient struct {
	client.Client
	evictions []*policyv1.Eviction
	evictErr  error
}

func (c *recordingClient) SubResource(name string) client.SubResourceClient {
	return &recordingSubResource{SubResourceClient: c.Client.SubResource(name), parent: c}
}

type recordingSubResource struct {
	client.SubResourceClient
	parent *recordingClient
}

func (s *recordingSubResource) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	if eviction, ok := subResource.(*policyv1.Eviction); ok {
		s.parent.evictions = append(s.parent.evictions, eviction)
		if s.parent.evictErr != nil {
			return s.parent.evictErr
		}
	}
	return s.SubResourceClient.Create(ctx, obj, subResource, opts...)
}

func TestReconcilePodLifetimeDeleteOptions(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	r, isvc, _ := lifetimeReconciler(t, now.Add(-2*time.Minute), time.Minute)
	recorder := &recordingClient{Client: r.Client}
	r.Client = recorder
	if _, err := r.reconcilePodLifetime(context.Background(), isvc, false, now); err != nil {
		t.Fatal(err)
	}
	if len(recorder.evictions) != 1 {
		t.Fatalf("eviction calls = %d", len(recorder.evictions))
	}
	got := recorder.evictions[0].DeleteOptions
	if got == nil || got.GracePeriodSeconds != nil {
		t.Fatalf("delete options did not preserve graceful termination: %#v", got)
	}
	if got.Preconditions == nil || got.Preconditions.UID == nil || *got.Preconditions.UID != "pod-first" {
		t.Fatalf("missing UID precondition: %#v", got.Preconditions)
	}
}

func TestReconcilePodLifetimeNotFoundDeleteIsSuccess(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	r, isvc, _ := lifetimeReconciler(t, now.Add(-2*time.Minute), time.Minute)
	recorder := &recordingClient{Client: r.Client, evictErr: apierrors.NewNotFound(corev1.Resource("pods"), "first")}
	r.Client = recorder
	if _, err := r.reconcilePodLifetime(context.Background(), isvc, false, now); err != nil {
		t.Fatal(err)
	}
}

// Found on a live cluster: swapping the operator image without the chart that
// owns its ClusterRole leaves pods/eviction ungranted. That must back off, not
// fail the reconcile and retry hot forever.
func TestReconcilePodLifetimeForbiddenBacksOff(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	r, isvc, _ := lifetimeReconciler(t, now.Add(-2*time.Minute), time.Minute)
	forbidden := apierrors.NewForbidden(corev1.Resource("pods"), "first",
		fmt.Errorf(`User "system:serviceaccount:ai:llmkube-controller-manager" cannot create resource "pods/eviction"`))
	r.Client = &recordingClient{Client: r.Client, evictErr: forbidden}

	got, err := r.reconcilePodLifetime(context.Background(), isvc, false, now)
	if err != nil {
		t.Fatalf("forbidden eviction failed the reconcile: %v", err)
	}
	if got != podLifetimeRetry {
		t.Fatalf("requeue = %s, want %s", got, podLifetimeRetry)
	}
}

func TestReconcilePodLifetimePDBRetry(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	r, isvc, _ := lifetimeReconciler(t, now.Add(-2*time.Minute), time.Minute)
	recorder := &recordingClient{Client: r.Client, evictErr: apierrors.NewTooManyRequests("pdb", 1)}
	r.Client = recorder
	got, err := r.reconcilePodLifetime(context.Background(), isvc, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if got != podLifetimeRetry {
		t.Fatalf("requeue = %s, want %s", got, podLifetimeRetry)
	}
}

func TestBoundedSecondsClampsOverflow(t *testing.T) {
	if got := boundedSeconds(maxPodLifetimeSeconds); got != time.Duration(maxPodLifetimeSeconds)*time.Second {
		t.Fatalf("duration = %s", got)
	}
	if got := boundedSeconds(maxPodLifetimeSeconds + 1); got != time.Duration(maxPodLifetimeSeconds)*time.Second {
		t.Fatalf("overflow duration = %s", got)
	}
}

func lifetimeReconciler(t *testing.T, start time.Time, lifetime time.Duration) (*InferenceServiceReconciler, *inferencev1alpha1.InferenceService, *corev1.Pod) {
	return lifetimeReconcilerWith(t, start, lifetime, 1, nil)
}

func lifetimeReconcilerWith(t *testing.T, start time.Time, lifetime time.Duration, replicaCount int32, podMutator func(*corev1.Pod)) (*InferenceServiceReconciler, *inferencev1alpha1.InferenceService, *corev1.Pod) {
	t.Helper()
	const (
		isvcUID = types.UID("isvc-uid")
		depUID  = types.UID("deployment-uid")
		rsUID   = types.UID("rs-uid")
	)
	controller := true
	replicas := replicaCount
	seconds := int64(lifetime / time.Second)
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: isvcUID}, Spec: inferencev1alpha1.InferenceServiceSpec{MaxPodLifetimeSeconds: &seconds}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: depUID, Generation: 2, OwnerReferences: []metav1.OwnerReference{{UID: isvcUID, Controller: &controller}}}, Spec: appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "svc"}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: replicaCount, UpdatedReplicas: replicaCount, ReadyReplicas: replicaCount, AvailableReplicas: replicaCount}}
	ready := corev1.ConditionTrue
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "ns", UID: "pod-first", Labels: map[string]string{"app": "svc"}, OwnerReferences: []metav1.OwnerReference{{UID: rsUID, Controller: &controller}}}, Status: corev1.PodStatus{StartTime: &metav1.Time{Time: start}, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}}}
	if podMutator != nil {
		podMutator(pod)
	}
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := policyv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &InferenceServiceReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(deployment, pod).WithObjects(isvc, deployment, pod).Build()}, isvc, pod
}
