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

// Whitebox tests for the unexported helpers in executor_native.go.
// The blackbox tests in executor_native_test.go drive end-to-end
// behavior through the public Executor; this file pins the helper
// semantics individually so a regression surfaces with a precise
// failure rather than as a cascading executor flake.

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// TestBranchNameForTask covers the precedence rule between explicit
// payload.branch (set on verify tasks per the v0.1 hand-off, and as an
// escape hatch on any task) and the issue-fix / task-name derivation.
// Regression for #528 part 1.
func TestBranchNameForTask(t *testing.T) {
	cases := []struct {
		name string
		task *foremanv1alpha1.AgenticTask
		want string
	}{
		{
			name: "payload.branch wins over issue-fix derivation",
			task: &foremanv1alpha1.AgenticTask{
				ObjectMeta: metav1.ObjectMeta{Name: "code-510"},
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
					Payload: foremanv1alpha1.AgenticTaskPayload{
						Issue:  510,
						Branch: "release-1.2-cherry-pick-of-510",
					},
				},
			},
			want: "release-1.2-cherry-pick-of-510",
		},
		{
			name: "payload.branch on verify (the gate hand-off shape)",
			task: &foremanv1alpha1.AgenticTask{
				ObjectMeta: metav1.ObjectMeta{Name: "gate-510"},
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind: foremanv1alpha1.AgenticTaskKindVerify,
					Payload: foremanv1alpha1.AgenticTaskPayload{
						Issue:  510,
						Branch: "foreman/issue-510",
					},
				},
			},
			want: "foreman/issue-510",
		},
		{
			name: "issue-fix without payload.branch falls back to issue derivation",
			task: &foremanv1alpha1.AgenticTask{
				ObjectMeta: metav1.ObjectMeta{Name: "code-503"},
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind:    foremanv1alpha1.AgenticTaskKindIssueFix,
					Payload: foremanv1alpha1.AgenticTaskPayload{Issue: 503},
				},
			},
			want: "foreman/issue-503",
		},
		{
			name: "non-issue-fix without payload.branch falls back to task name",
			task: &foremanv1alpha1.AgenticTask{
				ObjectMeta: metav1.ObjectMeta{Name: "verify-only"},
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind: foremanv1alpha1.AgenticTaskKindVerify,
				},
			},
			want: "foreman/verify-only",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := branchNameForTask(tc.task); got != tc.want {
				t.Errorf("want %q got %q", tc.want, got)
			}
		})
	}
}

// TestBuildDeterministicArgs pins the JSON shape buildDeterministicArgs
// produces, including the cloneURL passthrough the v0.1 gate path
// needs (#528 part 2). The tool layer asserts on these fields; this
// test catches drift between the executor's argument synthesis and
// run_gate_job's runGateJobArgs decoding.
func TestBuildDeterministicArgs(t *testing.T) {
	task := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "gate-510", Namespace: "default"},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindVerify,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  510,
				Branch: "foreman/issue-510",
			},
		},
	}

	t.Run("cloneURL set", func(t *testing.T) {
		raw := buildDeterministicArgs(task, "foreman/issue-510", "https://github.com/Defilan/LLMKube.git")
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode args: %v", err)
		}
		if got["branch"] != "foreman/issue-510" {
			t.Errorf("branch: want foreman/issue-510 got %v", got["branch"])
		}
		if got["repo"] != "defilantech/LLMKube" {
			t.Errorf("repo: want defilantech/LLMKube got %v", got["repo"])
		}
		if got["cloneURL"] != "https://github.com/Defilan/LLMKube.git" {
			t.Errorf("cloneURL: want fork URL got %v", got["cloneURL"])
		}
		ref, ok := got["taskRef"].(map[string]any)
		if !ok {
			t.Fatalf("taskRef missing or wrong shape: %v", got["taskRef"])
		}
		if ref["namespace"] != "default" || ref["name"] != "gate-510" {
			t.Errorf("taskRef: want default/gate-510 got %v/%v", ref["namespace"], ref["name"])
		}
	})

	t.Run("cloneURL empty preserves M4 default", func(t *testing.T) {
		raw := buildDeterministicArgs(task, "foreman/issue-510", "")
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode args: %v", err)
		}
		if got["cloneURL"] != "" {
			t.Errorf("cloneURL: want empty (so tool falls back to CloneURLBase+Repo) got %v", got["cloneURL"])
		}
	})
}
