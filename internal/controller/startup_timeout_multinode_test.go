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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// buildRingPods renders the multi-node fixture with spec.startupTimeout set
// (nil leaves it unset) and returns head, worker and the desired set.
func buildRingPods(t *testing.T, timeout *metav1.Duration) (head, worker *corev1.Pod, desired []*corev1.Pod) {
	t.Helper()
	isvc, model := multiNodeFixture()
	isvc.Spec.StartupTimeout = timeout
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	pods, _, err := r.memberPodsFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	if len(pods) != 2 {
		t.Fatalf("got %d pods, want 2", len(pods))
	}
	return pods[0], pods[1], pods
}

// TestConstructMemberPodsStartupTimeout pins spec.startupTimeout on the head's
// HTTP startup probe and on the worker's rendezvous startup probe: a worker
// killed at the old 30m while the head is allowed 90m would restart, and a
// worker restart recreates the whole group.
func TestConstructMemberPodsStartupTimeout(t *testing.T) {
	d90 := &metav1.Duration{Duration: 90 * time.Minute}

	t.Run("defaults keep the head's 30m budget on both", func(t *testing.T) {
		head, worker, _ := buildRingPods(t, nil)
		if got := head.Spec.Containers[0].StartupProbe.FailureThreshold; got != 180 {
			t.Errorf("head startup failureThreshold = %d, want 180", got)
		}
		if got := worker.Spec.Containers[0].StartupProbe.FailureThreshold; got != 180 {
			t.Errorf("worker startup failureThreshold = %d, want 180", got)
		}
	})

	t.Run("90m raises the head and the worker", func(t *testing.T) {
		head, worker, _ := buildRingPods(t, d90)
		hc, wc := head.Spec.Containers[0], worker.Spec.Containers[0]
		if hc.StartupProbe.FailureThreshold != 540 {
			t.Errorf("head startup failureThreshold = %d, want 540", hc.StartupProbe.FailureThreshold)
		}
		if wc.StartupProbe == nil || wc.StartupProbe.FailureThreshold != 540 {
			t.Fatalf("worker startup probe = %+v, want a failureThreshold of 540", wc.StartupProbe)
		}
		if wc.StartupProbe.PeriodSeconds != 10 {
			t.Errorf("worker startup period = %d, want 10", wc.StartupProbe.PeriodSeconds)
		}
		// The worker keeps the rendezvous dial, not the head's HTTP handler.
		if wc.StartupProbe.Exec == nil || wc.StartupProbe.HTTPGet != nil {
			t.Errorf("worker startup probe must stay the rendezvous exec: %+v", wc.StartupProbe.ProbeHandler)
		}
		// The rendezvous liveness budget is not a startup budget.
		if wc.LivenessProbe.PeriodSeconds != 30 || wc.LivenessProbe.FailureThreshold != 10 {
			t.Errorf("worker liveness budget changed: %+v", wc.LivenessProbe)
		}
		if wc.ReadinessProbe != nil {
			t.Errorf("worker must keep no readiness probe: %+v", wc.ReadinessProbe)
		}
	})

	// The override runs on a 30s period, so copying the head's failure count
	// instead of its window would give the worker 180 x 10s (30m), not the
	// head's 180 x 30s (90m).
	t.Run("probeOverrides.startup reaches the worker as a window", func(t *testing.T) {
		head, worker := ringWithOverride(t, &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/true"}}},
			PeriodSeconds:    30,
			FailureThreshold: 180,
		})
		hw, ww := startupWindow(head.Spec.Containers[0].StartupProbe), startupWindow(worker.Spec.Containers[0].StartupProbe)
		if ww != hw {
			t.Errorf("worker startup window = %v, want the head's %v", ww, hw)
		}
		if got := worker.Spec.Containers[0].StartupProbe.FailureThreshold; got != 540 {
			t.Errorf("worker startup failureThreshold = %d, want 540 at its 10s period", got)
		}
	})
}

// ringWithOverride renders the multi-node fixture with spec.probeOverrides.startup
// set and returns the head and worker pods.
func ringWithOverride(t *testing.T, startup *corev1.Probe) (head, worker *corev1.Pod) {
	t.Helper()
	isvc, model := multiNodeFixture()
	isvc.Spec.ProbeOverrides = &inferencev1alpha1.ProbeOverrides{Startup: startup}
	r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
	pods, _, err := r.memberPodsFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, "", "")
	if err != nil {
		t.Fatalf("constructMemberPods: %v", err)
	}
	return pods[0], pods[1]
}

// TestGroupNeedsRecreateStartupBudget pins that the group deadlines read the
// head's desired startup probe rather than the 30m constant: a head allowed
// 90m must not be recreated at 45m, and a restart after staging must follow
// the same budget.
func TestGroupNeedsRecreateStartupBudget(t *testing.T) {
	const hash = "abc"
	d90 := &metav1.Duration{Duration: 90 * time.Minute}
	head, worker, desired := buildRingPods(t, d90)
	now := time.Now()

	running := func(src *corev1.Pod, name string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Annotations: map[string]string{AnnotationMultiNodeGroupHash: hash},
		}}
		p.Spec = *src.Spec.DeepCopy()
		p.Status.Phase = corev1.PodRunning
		return p
	}
	// runningSince is a head whose runtime container started ago.
	runningSince := func(src *corev1.Pod, name string, ago time.Duration) *corev1.Pod {
		p := running(src, name)
		p.CreationTimestamp = metav1.NewTime(now.Add(-ago - 10*time.Minute))
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  src.Spec.Containers[0].Name,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-ago))}},
		}}
		return p
	}

	t.Run("a 90m head is not recreated at 45m", func(t *testing.T) {
		obs := []memberObservation{{rank: 0, desired: desired[0], existing: runningSince(head, head.Name, 45*time.Minute)}}
		if reason, _, _ := groupNeedsRecreate(obs, hash, now, time.Time{}, time.Time{}); reason != "" {
			t.Errorf("reason = %q, want none inside the 90m budget", reason)
		}
		obs[0].existing = runningSince(head, head.Name, 91*time.Minute)
		if reason, _, _ := groupNeedsRecreate(obs, hash, now, time.Time{}, time.Time{}); reason != multiNodeReasonHeadNotReady {
			t.Errorf("reason = %q, want %s past the 90m budget", reason, multiNodeReasonHeadNotReady)
		}
	})

	t.Run("a long staging init does not consume the head's budget", func(t *testing.T) {
		// Created 100m ago with a 70m download, so the runtime has been up
		// 30m: the probe window has barely started and must not recreate.
		h := runningSince(head, head.Name, 30*time.Minute)
		h.CreationTimestamp = metav1.NewTime(now.Add(-100 * time.Minute))
		obs := []memberObservation{{rank: 0, desired: desired[0], existing: h}}
		if reason, _, _ := groupNeedsRecreate(obs, hash, now, time.Time{}, time.Time{}); reason != "" {
			t.Errorf("reason = %q, want none while the runtime is inside its window", reason)
		}
	})

	t.Run("a default head is recreated at 45m", func(t *testing.T) {
		plainHead, _, plainDesired := buildRingPods(t, nil)
		obs := []memberObservation{{rank: 0, desired: plainDesired[0], existing: runningSince(plainHead, plainHead.Name, 45*time.Minute)}}
		if reason, _, _ := groupNeedsRecreate(obs, hash, now, time.Time{}, time.Time{}); reason != multiNodeReasonHeadNotReady {
			t.Errorf("reason = %q, want %s past the 30m budget", reason, multiNodeReasonHeadNotReady)
		}
	})

	t.Run("an override below 30m does not lower the floor", func(t *testing.T) {
		// A fast-fail override (10 failures at 10s) must not shrink the group
		// deadlines below the built-in 30m.
		shortHead, shortWorker := ringWithOverride(t, &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/true"}}},
			PeriodSeconds:    10,
			FailureThreshold: 10,
		})
		obs := []memberObservation{{rank: 0, desired: shortHead, existing: runningSince(shortHead, shortHead.Name, 20*time.Minute)}}
		if reason, _, _ := groupNeedsRecreate(obs, hash, now, time.Time{}, time.Time{}); reason != "" {
			t.Errorf("reason = %q, want none: a short probe must not shorten the group budget", reason)
		}
		if w := startupWindow(shortWorker.Spec.Containers[0].StartupProbe); w < multiNodeStartupBudget {
			t.Errorf("worker startup window = %v, want at least the built-in %v", w, multiNodeStartupBudget)
		}
	})

	t.Run("the post-staging mask follows the budget", func(t *testing.T) {
		w := running(worker, worker.Name)
		w.Status.ContainerStatuses = []corev1.ContainerStatus{{
			RestartCount: 1,
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   1,
				FinishedAt: metav1.NewTime(now.Add(-10 * time.Minute)),
			}},
		}}
		obs := []memberObservation{
			{rank: 0, desired: desired[0], existing: running(head, head.Name)},
			{rank: 1, desired: desired[1], existing: w},
		}
		stagedAt := now.Add(-45 * time.Minute) // inside 90m, past the old 30m
		if reason, _, _ := groupNeedsRecreate(obs, hash, now, time.Time{}, stagedAt); reason != "" {
			t.Errorf("reason = %q, want the restart masked inside the 90m budget", reason)
		}

		plainHead, plainWorker, plainDesired := buildRingPods(t, nil)
		obs = []memberObservation{
			{rank: 0, desired: plainDesired[0], existing: running(plainHead, plainHead.Name)},
			{rank: 1, desired: plainDesired[1], existing: running(plainWorker, worker.Name)},
		}
		obs[1].existing.Status.ContainerStatuses = w.Status.ContainerStatuses
		if reason, _, _ := groupNeedsRecreate(obs, hash, now, time.Time{}, stagedAt); reason != multiNodeReasonMemberRestarted {
			t.Errorf("reason = %q, want %s past the 30m budget", reason, multiNodeReasonMemberRestarted)
		}
	})
}
