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

package agent

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// claimedRunningTask builds an AgenticTask in phase=Running that is owned by
// the given node with the given claimedAt timestamp. It is used to seed both
// the fake client (the live object the controller sees) and the in-memory
// copy the watcher holds after calling claim().
func claimedRunningTask(name, node string, claimedAt metav1.Time) *foremanv1alpha1.AgenticTask {
	return &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: foremanv1alpha1.AgenticTaskStatus{
			Phase:        foremanv1alpha1.AgenticTaskPhaseRunning,
			AssignedNode: node,
			ClaimedAt:    &claimedAt,
		},
	}
}

// inMemoryTask builds an in-memory copy (the watcher's `t`) with the given
// ownership. This simulates the task pointer that claim() leaves behind after
// mutating t.Status.ClaimedAt and succeeding, but before the executor returns.
func inMemoryTask(name, node string, claimedAt metav1.Time) *foremanv1alpha1.AgenticTask {
	return claimedRunningTask(name, node, claimedAt)
}

// TestPatchTerminal_StandsDownWhenTaskReleased verifies that if the
// controller expired the claim (#669) and reset the task to Pending while the
// executor was running, patchTerminal returns nil WITHOUT writing any terminal
// status — the live object must remain Pending. Fixes #668.
func TestPatchTerminal_StandsDownWhenTaskReleased(t *testing.T) {
	// Use second-precision timestamp: metav1.Time JSON-encodes to second
	// precision, so the fake client's JSON round-trip truncates sub-seconds.
	// A time.Unix value with zero nanoseconds survives the round-trip intact,
	// enabling reliable Equal comparisons.
	t1 := metav1.NewTime(time.Unix(time.Now().Add(-2*time.Minute).Unix(), 0))

	// Live object: controller reset it to Pending, cleared claim.
	released := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "code-668a", Namespace: "default"},
		Status: foremanv1alpha1.AgenticTaskStatus{
			Phase:        foremanv1alpha1.AgenticTaskPhasePending,
			AssignedNode: "",
			ClaimedAt:    nil,
		},
	}
	c := newRecoveryClient(t, released)
	w := &AgenticTaskWatcher{Client: c, NodeName: "node-a", Namespace: "default"}

	// In-memory copy: what the watcher held after claim() returned.
	tInMemory := inMemoryTask("code-668a", "node-a", t1)

	res := NewResult("issue-fix", foremanv1alpha1.AgenticTaskVerdictGo, "done", time.Second)

	err := w.patchTerminal(context.Background(), tInMemory, res, nil)
	if err != nil {
		t.Fatalf("patchTerminal returned error: %v", err)
	}

	// The live object must not have been touched — it must still be Pending.
	got := getTask(t, c, "code-668a")
	if got.Status.Phase != foremanv1alpha1.AgenticTaskPhasePending {
		t.Fatalf("phase = %q, want Pending (stand-down should prevent terminal patch)", got.Status.Phase)
	}
	if got.Status.Verdict != "" {
		t.Fatalf("verdict = %q, want empty on stand-down", got.Status.Verdict)
	}
	if got.Status.FinishedAt != nil {
		t.Fatalf("finishedAt = %v, want nil on stand-down", got.Status.FinishedAt)
	}
}

// TestPatchTerminal_StandsDownWhenTaskReClaimedElsewhere verifies that if
// another node re-claimed the task (different AssignedNode and ClaimedAt)
// while this executor was running, patchTerminal returns nil WITHOUT writing
// terminal status over the new owner's Running state. Fixes #668.
func TestPatchTerminal_StandsDownWhenTaskReClaimedElsewhere(t *testing.T) {
	// Use second-precision timestamps for reliable fake-client round-trip equality.
	t1 := metav1.NewTime(time.Unix(time.Now().Add(-2*time.Minute).Unix(), 0))
	t2 := metav1.NewTime(time.Unix(time.Now().Add(-30*time.Second).Unix(), 0))

	// Live object: re-claimed by a different node at a later timestamp.
	reClaimed := claimedRunningTask("code-668b", "node-b", t2)
	c := newRecoveryClient(t, reClaimed)
	w := &AgenticTaskWatcher{Client: c, NodeName: "node-a", Namespace: "default"}

	// In-memory copy: the original claim by node-a at t1.
	tInMemory := inMemoryTask("code-668b", "node-a", t1)

	res := NewResult("issue-fix", foremanv1alpha1.AgenticTaskVerdictGo, "done", time.Second)

	err := w.patchTerminal(context.Background(), tInMemory, res, nil)
	if err != nil {
		t.Fatalf("patchTerminal returned error: %v", err)
	}

	// The live object must not have been patched — node-b's Running state intact.
	got := getTask(t, c, "code-668b")
	if got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseRunning {
		t.Fatalf("phase = %q, want Running (node-b still owns it)", got.Status.Phase)
	}
	if got.Status.AssignedNode != "node-b" {
		t.Fatalf("assignedNode = %q, want node-b", got.Status.AssignedNode)
	}
	if got.Status.Verdict != "" {
		t.Fatalf("verdict = %q, want empty on stand-down", got.Status.Verdict)
	}
}

// TestPatchTerminal_NormalPathStillLands is the regression guard: when the
// live object is still owned by this agent (same node, same ClaimedAt), the
// terminal patch must land exactly as before. Fixes #668 must not regress the
// happy path.
func TestPatchTerminal_NormalPathStillLands(t *testing.T) {
	// Use second-precision timestamp for reliable fake-client round-trip equality.
	t1 := metav1.NewTime(time.Unix(time.Now().Add(-90*time.Second).Unix(), 0))

	// Live object: still owned by node-a with the same claimedAt.
	live := claimedRunningTask("code-668c", "node-a", t1)
	c := newRecoveryClient(t, live)
	w := &AgenticTaskWatcher{Client: c, NodeName: "node-a", Namespace: "default"}

	// In-memory copy: same node, same timestamp.
	tInMemory := inMemoryTask("code-668c", "node-a", t1)

	res := NewResult("issue-fix", foremanv1alpha1.AgenticTaskVerdictGo, "fixed it", time.Second)

	if err := w.patchTerminal(context.Background(), tInMemory, res, nil); err != nil {
		t.Fatalf("patchTerminal returned error: %v", err)
	}

	got := getTask(t, c, "code-668c")
	if got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", got.Status.Phase)
	}
	if got.Status.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("verdict = %q, want GO", got.Status.Verdict)
	}
	if got.Status.FinishedAt == nil {
		t.Fatal("finishedAt is nil, want set")
	}
}
