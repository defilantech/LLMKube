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

func scheduledTask(
	name, node string, kind foremanv1alpha1.AgenticTaskKind, age time.Duration,
) *foremanv1alpha1.AgenticTask {
	return &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Spec: foremanv1alpha1.AgenticTaskSpec{Kind: kind},
		Status: foremanv1alpha1.AgenticTaskStatus{
			Phase:        foremanv1alpha1.AgenticTaskPhaseScheduled,
			AssignedNode: node,
		},
	}
}

// TestSortTasksDepthFirst_KindOrder pins the depth-first claim order:
// review before verify before issue-fix before everything else (#936).
// Breadth-first (List-order) claiming starves downstream tasks behind a
// deep issue-fix backlog, so no Workload ever completes.
func TestSortTasksDepthFirst_KindOrder(t *testing.T) {
	tasks := []*foremanv1alpha1.AgenticTask{
		scheduledTask("free", "n", foremanv1alpha1.AgenticTaskKindFreeform, time.Hour),
		scheduledTask("code", "n", foremanv1alpha1.AgenticTaskKindIssueFix, time.Hour),
		scheduledTask("rev", "n", foremanv1alpha1.AgenticTaskKindReview, time.Minute),
		scheduledTask("ver", "n", foremanv1alpha1.AgenticTaskKindVerify, time.Minute),
	}
	sortTasksDepthFirst(tasks)
	got := []string{tasks[0].Name, tasks[1].Name, tasks[2].Name, tasks[3].Name}
	want := []string{"rev", "ver", "code", "free"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("depth-first order: want %v got %v", want, got)
		}
	}
}

// TestSortTasksDepthFirst_OldestFirstWithinKind pins the tiebreak: within
// a kind, the oldest Scheduled task is claimed first so nothing starves.
func TestSortTasksDepthFirst_OldestFirstWithinKind(t *testing.T) {
	tasks := []*foremanv1alpha1.AgenticTask{
		scheduledTask("code-new", "n", foremanv1alpha1.AgenticTaskKindIssueFix, time.Minute),
		scheduledTask("code-old", "n", foremanv1alpha1.AgenticTaskKindIssueFix, time.Hour),
	}
	sortTasksDepthFirst(tasks)
	if tasks[0].Name != "code-old" {
		t.Fatalf("want oldest issue-fix first, got %q", tasks[0].Name)
	}
}

// TestPollOnce_ClaimsReviewBeforeOlderIssueFix is the end-to-end #936
// regression: a review task scheduled AFTER a backlog of issue-fix tasks
// must still be claimed first.
func TestPollOnce_ClaimsReviewBeforeOlderIssueFix(t *testing.T) {
	code := scheduledTask("wl-a-code-1", "node-1", foremanv1alpha1.AgenticTaskKindIssueFix, time.Hour)
	review := scheduledTask("wl-b-review-2-0", "node-1", foremanv1alpha1.AgenticTaskKindReview, time.Minute)
	other := scheduledTask("wl-c-code-3", "other-node", foremanv1alpha1.AgenticTaskKindReview, time.Minute)

	c := newRecoveryClient(t, code, review, other)
	w := &AgenticTaskWatcher{
		Client:   c,
		NodeName: "node-1",
		// A long sleep keeps the claimed task deterministically in
		// phase=Running for the assertions below (a short sleep lets the
		// executor goroutine patch it terminal on a loaded runner). The
		// goroutine is abandoned when the test ends.
		Executor: &StubExecutor{SleepDuration: time.Hour},
	}

	if err := w.pollOnce(context.Background(), "default"); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	// The review task must be the one claimed (Running), the older
	// issue-fix must remain Scheduled.
	if got := getTask(t, c, "wl-b-review-2-0"); got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseRunning {
		t.Errorf("review task phase: want Running got %s", got.Status.Phase)
	}
	if got := getTask(t, c, "wl-a-code-1"); got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseScheduled {
		t.Errorf("issue-fix task phase: want Scheduled (not claimed) got %s", got.Status.Phase)
	}
	// The other node's task must be untouched.
	if got := getTask(t, c, "wl-c-code-3"); got.Status.AssignedNode != "other-node" {
		t.Errorf("other node's task must not be touched")
	}
}
