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

// pendingTaskClaimedAt is a fixed second-precision timestamp used by
// pendingTask so that the fake-client JSON round-trip (which truncates
// sub-seconds) preserves the value and the ownership guard can compare it
// reliably against the in-memory copy.
var pendingTaskClaimedAt = metav1.NewTime(time.Unix(1_700_000_000, 0))

// pendingTask returns an AgenticTask in phase=Running claimed by "coder".
// The AssignedNode and ClaimedAt fields satisfy the ownership guard added in
// #668: tests that call patchTerminal directly must present a live object
// that matches the node name and claim timestamp on the in-memory copy.
func pendingTask(name string) *foremanv1alpha1.AgenticTask {
	ts := pendingTaskClaimedAt
	return &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: foremanv1alpha1.AgenticTaskStatus{
			Phase:        foremanv1alpha1.AgenticTaskPhaseRunning,
			AssignedNode: "coder",
			ClaimedAt:    &ts,
		},
	}
}

// patchTerminal must lift the pushed branch + head commit out of the GO
// Result envelope and onto AgenticTask.status.branch / .status.commitSHA so
// downstream consumers (verify/gate auto-spawn) have something to key off.
// Both the in-process executor (goResult) and the Job-mode executor
// (coderJobResultToResult) populate Result.Extra["branch"]/["commitSHA"] on a
// GO, so a single lift covers both paths.
//
// Regression for defilantech/LLMKube#624: Job-mode coder pushed the branch but
// status.branch / status.commitSHA came back empty.
func TestPatchTerminal_LiftsBranchAndCommitOnGo(t *testing.T) {
	c := newRecoveryClient(t, pendingTask("code-624"))
	w := &AgenticTaskWatcher{Client: c, NodeName: "coder", Namespace: "default"}

	res := NewResult("issue-fix", foremanv1alpha1.AgenticTaskVerdictGo,
		"fixed the bug", time.Second)
	res.Extra = map[string]any{
		"outcome":   "",
		"branch":    "foreman/issue-624",
		"commitSHA": "abc1234def5678",
	}

	if err := w.patchTerminal(context.Background(), pendingTask("code-624"), res, nil); err != nil {
		t.Fatalf("patchTerminal: %v", err)
	}

	got := getTask(t, c, "code-624")
	if got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", got.Status.Phase)
	}
	if got.Status.Branch != "foreman/issue-624" {
		t.Fatalf("status.branch = %q, want %q", got.Status.Branch, "foreman/issue-624")
	}
	if got.Status.CommitSHA != "abc1234def5678" {
		t.Fatalf("status.commitSHA = %q, want %q", got.Status.CommitSHA, "abc1234def5678")
	}
}

// A non-GO terminal outcome carries only an intendedBranch (no push happened),
// so status.commitSHA stays empty. status.branch reflects the branch the run
// targeted so an operator can still locate the intended work.
func TestPatchTerminal_NoGoLeavesCommitEmpty(t *testing.T) {
	c := newRecoveryClient(t, pendingTask("code-nogo"))
	w := &AgenticTaskWatcher{Client: c, NodeName: "coder", Namespace: "default"}

	res := NewResult("issue-fix", foremanv1alpha1.AgenticTaskVerdictNoGo,
		"model declined", time.Second)
	res.Extra = map[string]any{
		"outcome":        "MODEL-NO-GO",
		"intendedBranch": "foreman/issue-nogo",
	}

	if err := w.patchTerminal(context.Background(), pendingTask("code-nogo"), res, nil); err != nil {
		t.Fatalf("patchTerminal: %v", err)
	}

	got := getTask(t, c, "code-nogo")
	if got.Status.Branch != "foreman/issue-nogo" {
		t.Fatalf("status.branch = %q, want %q", got.Status.Branch, "foreman/issue-nogo")
	}
	if got.Status.CommitSHA != "" {
		t.Fatalf("status.commitSHA = %q, want empty on NO-GO", got.Status.CommitSHA)
	}
}
