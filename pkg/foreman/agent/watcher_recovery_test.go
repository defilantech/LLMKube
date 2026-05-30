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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func newRecoveryClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&foremanv1alpha1.AgenticTask{}).
		Build()
}

func runningTask(name, node string) *foremanv1alpha1.AgenticTask {
	now := metav1.Now()
	return &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: foremanv1alpha1.AgenticTaskStatus{
			Phase:        foremanv1alpha1.AgenticTaskPhaseRunning,
			AssignedNode: node,
			ClaimedAt:    &now,
			StartedAt:    &now,
		},
	}
}

func getTask(t *testing.T, c client.Client, name string) foremanv1alpha1.AgenticTask {
	t.Helper()
	var got foremanv1alpha1.AgenticTask
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return got
}

// A task left Running by a dead PID on this node must be reset to Pending
// with assignedNode/claimedAt/startedAt cleared and a recovery condition,
// so the scheduler re-dispatches it. Regression for defilantech/LLMKube#542.
func TestRecoverOrphanedTasks_ResetsRunningTaskOnThisNode(t *testing.T) {
	c := newRecoveryClient(t, runningTask("code-531", "m5max-coder"))
	w := &AgenticTaskWatcher{Client: c, NodeName: "m5max-coder", Namespace: "default"}

	if err := w.recoverOrphanedTasks(context.Background(), "default"); err != nil {
		t.Fatalf("recoverOrphanedTasks: %v", err)
	}

	got := getTask(t, c, "code-531")
	if got.Status.Phase != foremanv1alpha1.AgenticTaskPhasePending {
		t.Fatalf("phase = %q, want Pending", got.Status.Phase)
	}
	if got.Status.AssignedNode != "" {
		t.Fatalf("assignedNode = %q, want cleared", got.Status.AssignedNode)
	}
	if got.Status.ClaimedAt != nil || got.Status.StartedAt != nil {
		t.Fatalf("claimedAt=%v startedAt=%v, want both cleared", got.Status.ClaimedAt, got.Status.StartedAt)
	}
	found := false
	for _, cond := range got.Status.Conditions {
		if cond.Reason == "AgentRestartRecovery" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an AgentRestartRecovery condition, got %+v", got.Status.Conditions)
	}
}

// A Running task assigned to a different node is not this agent's to
// recover; leave it untouched.
func TestRecoverOrphanedTasks_IgnoresOtherNode(t *testing.T) {
	c := newRecoveryClient(t, runningTask("other-node-task", "some-other-node"))
	w := &AgenticTaskWatcher{Client: c, NodeName: "m5max-coder", Namespace: "default"}

	if err := w.recoverOrphanedTasks(context.Background(), "default"); err != nil {
		t.Fatalf("recoverOrphanedTasks: %v", err)
	}

	got := getTask(t, c, "other-node-task")
	if got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseRunning {
		t.Fatalf("phase = %q, want Running (untouched)", got.Status.Phase)
	}
}

// Tasks on this node that are not Running (e.g. Scheduled) must not be
// disturbed: only orphaned in-flight work is recovered.
func TestRecoverOrphanedTasks_IgnoresNonRunningPhase(t *testing.T) {
	scheduled := runningTask("scheduled-task", "m5max-coder")
	scheduled.Status.Phase = foremanv1alpha1.AgenticTaskPhaseScheduled
	c := newRecoveryClient(t, scheduled)
	w := &AgenticTaskWatcher{Client: c, NodeName: "m5max-coder", Namespace: "default"}

	if err := w.recoverOrphanedTasks(context.Background(), "default"); err != nil {
		t.Fatalf("recoverOrphanedTasks: %v", err)
	}

	got := getTask(t, c, "scheduled-task")
	if got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseScheduled {
		t.Fatalf("phase = %q, want Scheduled (untouched)", got.Status.Phase)
	}
}
