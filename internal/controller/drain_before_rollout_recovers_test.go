/*
Copyright 2026.

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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	rolloutTestImage   = "ghcr.io/ggml-org/llama.cpp:server"
	rolloutTestImageV2 = "ghcr.io/ggml-org/llama.cpp:server-v2"
)

// rolloutFixture is one InferenceService mid-decision: a Deployment created by
// the first reconcile, plus the old-generation pods the test places on top of
// it. Reconcile is called with a real Service URL probe unless the test sets
// slotsURL, so the default idle path (EndpointSlices) is the one under test.
type rolloutFixture struct {
	c      client.Client
	key    types.NamespacedName
	isvc   *inferencev1alpha1.InferenceService
	r      *InferenceServiceReconciler
	slots  *httptest.Server
	dep    *appsv1.Deployment
	ctx    context.Context
	prefix string
}

func newRolloutFixture(t *testing.T, name, runtimeName string) *rolloutFixture {
	t.Helper()
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	model := readyModelFor(t, c, name+"-model")
	ns := testNS

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: model.Name,
			Replicas: ptrInt32(1),
			Image:    rolloutTestImage,
			Runtime:  runtimeName,
			RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
				WaitForIdle:        true,
				IdleTimeoutSeconds: 900,
			},
		},
	}
	if err := c.Create(ctx, isvc); err != nil {
		t.Fatalf("create isvc: %v", err)
	}
	key := client.ObjectKeyFromObject(isvc)
	f := &rolloutFixture{
		c:      c,
		key:    key,
		isvc:   isvc,
		ctx:    ctx,
		prefix: name,
		r: &InferenceServiceReconciler{
			Client:             c,
			Scheme:             c.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		},
	}
	if _, err := f.r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, key, dep); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		t.Fatalf("fixture: deployment has no containers")
	}
	f.dep = dep
	t.Cleanup(func() {
		if f.slots != nil {
			f.slots.Close()
		}
	})
	return f
}

// servingContainerName reads the name the controller actually gave the
// serving container, rather than assuming the runtime's default.
func (f *rolloutFixture) servingContainerName(t *testing.T) string {
	t.Helper()
	return f.dep.Spec.Template.Spec.Containers[0].Name
}

// useIdleStub points the idle probe at a local server that answers /slots with
// the given body and status, the way a running replica would.
func (f *rolloutFixture) useIdleStub(t *testing.T, status int, body string) {
	t.Helper()
	f.slots = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slots" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	f.r.RolloutIdleBaseURL = f.slots.URL
}

// pod places an old-generation, not-Ready pod whose serving container is in
// the given state.
func (f *rolloutFixture) pod(t *testing.T, suffix string, state corev1.ContainerState) *corev1.Pod {
	t.Helper()
	container := f.servingContainerName(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      f.prefix + "-" + suffix,
			Namespace: f.key.Namespace,
			Labels: map[string]string{
				"app":                           f.key.Name,
				"inference.llmkube.dev/service": f.key.Name,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: container, Image: rolloutTestImage}},
		},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		t.Fatalf("create pod %s: %v", suffix, err)
	}
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  container,
			State: state,
		}},
	}
	if err := f.c.Status().Update(f.ctx, pod); err != nil {
		t.Fatalf("update pod %s status: %v", suffix, err)
	}
	return pod
}

func crashLoopState() corev1.ContainerState {
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
}

func containerCreatingState() corev1.ContainerState {
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
}

func runningState() corev1.ContainerState {
	return corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
}

// reconcileAfterEdit changes the image and runs one reconcile, with a short
// deadline so a base-tree deferral cannot sit on the 900s idle budget.
func (f *rolloutFixture) reconcileAfterEdit(t *testing.T) (reconcile.Result, error, context.CancelFunc) {
	t.Helper()
	bumpISVCImage(t, f.c, f.key, rolloutTestImageV2)
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	t.Cleanup(cancel)
	result, err := f.r.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
	return result, err, cancel
}

func (f *rolloutFixture) deploymentImage(t *testing.T) string {
	t.Helper()
	dep := &appsv1.Deployment{}
	if err := f.c.Get(f.ctx, f.key, dep); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	// The serving container is the one this fixture created; find it by name
	// so an injected sidecar cannot shift the index.
	return dep.Spec.Template.Spec.Containers[0].Image
}

func (f *rolloutFixture) rolloutCondition(t *testing.T) *metav1.Condition {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{}
	if err := f.c.Get(f.ctx, f.key, isvc); err != nil {
		t.Fatalf("get isvc: %v", err)
	}
	return meta.FindStatusCondition(isvc.Status.Conditions, ConditionRolloutDeferred)
}

// --- reproductions: these fail on base and pass once the decision reads
// container state. ---

func TestRolloutProceedsWhenServingContainerIsCrashLooping(t *testing.T) {
	f := newRolloutFixture(t, "rollout-crashloop", "")
	f.pod(t, "old", crashLoopState())

	if _, err, _ := f.reconcileAfterEdit(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := f.deploymentImage(t); got != rolloutTestImageV2 {
		t.Fatalf("deployment image is %q, want %q: the rollout was held for a pod whose serving container is not running", got, rolloutTestImageV2)
	}
	if cond := f.rolloutCondition(t); cond != nil && cond.Status == metav1.ConditionTrue {
		t.Fatalf("RolloutDeferred=True (%s) for a pod whose serving container is not running: %s", cond.Reason, cond.Message)
	}
}

func TestRolloutProceedsWhenPodIsContainerCreating(t *testing.T) {
	f := newRolloutFixture(t, "rollout-container-creating", "")
	f.pod(t, "old", containerCreatingState())

	if _, err, _ := f.reconcileAfterEdit(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := f.deploymentImage(t); got != rolloutTestImageV2 {
		t.Fatalf("deployment image is %q, want %q: the rollout was held for a pod that is still creating", got, rolloutTestImageV2)
	}
}

func TestRolloutProceedsForUnsupportedRuntimeWhenContainerNotRunning(t *testing.T) {
	f := newRolloutFixture(t, "rollout-unsupported-crashloop", "personaplex")
	f.pod(t, "old", crashLoopState())

	if _, err, _ := f.reconcileAfterEdit(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := f.deploymentImage(t); got != rolloutTestImageV2 {
		t.Fatalf("deployment image is %q, want %q: a runtime without idle detection held the rollout for a container that is not running", got, rolloutTestImageV2)
	}
}

// One running replica answers idle, the other is not running: the idle answer
// is the only evidence there is to weigh, so the rollout goes.
func TestRolloutProceedsWhenOnePodRunningAndIdle(t *testing.T) {
	f := newRolloutFixture(t, "rollout-mixed-idle", "")
	f.useIdleStub(t, http.StatusOK, `[{"id":0,"is_processing":false}]`)
	f.pod(t, "running", runningState())
	f.pod(t, "crashloop", crashLoopState())

	if _, err, _ := f.reconcileAfterEdit(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := f.deploymentImage(t); got != rolloutTestImageV2 {
		t.Fatalf("deployment image is %q, want %q: the idle running replica was not enough to roll past a pod that is not running", got, rolloutTestImageV2)
	}
}

// --- parity: these give the same result on base and on the branch. ---

func TestRolloutStillDefersWhenRunningPodAnswersBusy(t *testing.T) {
	f := newRolloutFixture(t, "rollout-running-busy", "")
	f.useIdleStub(t, http.StatusOK, `[{"id":0,"is_processing":true}]`)
	f.pod(t, "old", runningState())

	if _, err, _ := f.reconcileAfterEdit(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := f.deploymentImage(t); got != rolloutTestImage {
		t.Fatalf("deployment image is %q, want %q: a busy running replica must hold the rollout", got, rolloutTestImage)
	}
	cond := f.rolloutCondition(t)
	// The rollout is held either way. The reason differs: base reports
	// PodsCrashLooping for a not-Ready pod before the probe answer matters,
	// the branch reports PodsBusy because the pod is running and answered.
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("RolloutDeferred = %+v, want True", cond)
	}
}

func TestRolloutStillDefersWhenRunningPodProbeFails(t *testing.T) {
	f := newRolloutFixture(t, "rollout-running-probe-error", "")
	f.useIdleStub(t, http.StatusInternalServerError, "boom")
	f.pod(t, "old", runningState())
	if _, err, _ := f.reconcileAfterEdit(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := f.deploymentImage(t); got != rolloutTestImage {
		t.Fatalf("deployment image is %q, want %q: an unanswered probe on a running replica must hold the rollout", got, rolloutTestImage)
	}

	cond := f.rolloutCondition(t)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("RolloutDeferred = %+v, want True", cond)
	}
}

func TestRolloutStillDefersWhenRunningPodUnsupported(t *testing.T) {
	f := newRolloutFixture(t, "rollout-running-unsupported", "generic")
	f.pod(t, "old", runningState())

	if _, err, _ := f.reconcileAfterEdit(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := f.deploymentImage(t); got != rolloutTestImage {
		t.Fatalf("deployment image is %q, want %q: a runtime without idle detection must hold a running replica", got, rolloutTestImage)
	}
	cond := f.rolloutCondition(t)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonIdleCheckUnsupported {
		t.Fatalf("RolloutDeferred = %+v, want True/IdleCheckUnsupported", cond)
	}
}

func TestRolloutStillDefersWhenRunningReplicaBusy(t *testing.T) {
	f := newRolloutFixture(t, "rollout-mixed-busy", "")
	f.useIdleStub(t, http.StatusOK, `[{"id":0,"is_processing":true}]`)
	f.pod(t, "running", runningState())
	f.pod(t, "crashloop", crashLoopState())

	if _, err, _ := f.reconcileAfterEdit(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := f.deploymentImage(t); got != rolloutTestImage {
		t.Fatalf("deployment image is %q, want %q: a busy running replica must hold the rollout", got, rolloutTestImage)
	}
}

func TestRolloutProceedsWithNoOldPods(t *testing.T) {
	f := newRolloutFixture(t, "rollout-no-pods", "")

	if _, err, _ := f.reconcileAfterEdit(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := f.deploymentImage(t); got != rolloutTestImageV2 {
		t.Fatalf("deployment image is %q, want %q: with no old pods there is nothing to drain", got, rolloutTestImageV2)
	}
	if cond := f.rolloutCondition(t); cond != nil && cond.Status == metav1.ConditionTrue {
		t.Fatalf("RolloutDeferred=True for a service with no old pods: %s", cond.Reason)
	}
}

// A lone terminating pod, with a probe that answers idle, does not hold the
// rollout: base rolls, and the branch rolls too.
func TestRolloutProceedsWithOnlyATerminatingPod(t *testing.T) {
	f := newRolloutFixture(t, "rollout-terminating", "")
	f.useIdleStub(t, http.StatusOK, `[{"id":0,"is_processing":false}]`)
	pod := f.pod(t, "terminating", runningState())
	if err := f.c.Delete(f.ctx, pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}

	if _, err, _ := f.reconcileAfterEdit(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := f.deploymentImage(t); got != rolloutTestImageV2 {
		t.Fatalf("deployment image is %q, want %q: an idle cluster with a terminating pod should roll", got, rolloutTestImageV2)
	}
}
