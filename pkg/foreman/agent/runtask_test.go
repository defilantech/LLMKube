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

package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

// TestRunTask_HappyPathPushesAndEmitsResult exercises the single-task
// runner end to end: it loads the AgenticTask + Agent from a fake
// client, resolves the stub OAI endpoint, clones a temp bare repo,
// runs the loop to a GO verdict, commits + pushes the branch, and emits
// the structured result JSON plus a final sentinel line on the
// configured writer (the contract the coder Job poller reads, mirroring
// the gate Job's GATE PASS / GATE FAIL).
func TestRunTask_HappyPathPushesAndEmitsResult(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{submitGoBody})

	agent, task := taskAndAgent("runtask-happy")
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(agent, task).
		Build()

	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			"submit_result": {
				Terminal: true, Verdict: "GO", Summary: "fixed",
				CommitMessage: "fix: trivial change\n",
			},
		},
		touch: func(name string, ws string) {
			if name == "submit_result" {
				_ = os.WriteFile(filepath.Join(ws, "fix.txt"), []byte("foreman touched this\n"), 0o644)
			}
		},
	}

	var out bytes.Buffer
	cfg := foremanagent.RunTaskConfig{
		Client:       c,
		Task:         types.NamespacedName{Namespace: task.Namespace, Name: task.Name},
		WorkspaceDir: filepath.Join(root, "ws"),
		GitRemoteURL: bare,
		// task carries payload.repo "defilantech/LLMKube" (see taskAndAgent),
		// which would otherwise resolve to the real github.com upstream: a
		// live network fetch this test does not need. Point the base-branch
		// fetch at the SAME local bare fixture used as the clone/push
		// remote, so origin/<base> and the fetched upstream base share
		// identical history (required since #1075: the claim-evidence gate
		// anchors evidence provenance to their merge-base, which needs a
		// real common ancestor to resolve to anything meaningful).
		UpstreamURLForRepo:       func(string) string { return bare },
		InferenceBaseURLOverride: oaiSrv.URL + "/v1",
		CommitAuthor:             repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		CommitCommitter:          repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		RegistryFactory: func(
			_ context.Context, ws string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			reg.workspace = ws
			return reg, nil
		},
		AuthFactory: fakeAuth(t),
		Stdout:      &out,
	}

	got, err := foremanagent.RunTask(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	// Structured return.
	if got.Verdict != string(foremanv1alpha1.AgenticTaskVerdictGo) {
		t.Errorf("verdict: want GO got %q", got.Verdict)
	}
	if got.Branch != "foreman/issue-9999" {
		t.Errorf("branch: want foreman/issue-9999 got %q", got.Branch)
	}
	if got.CommitSHA == "" {
		t.Errorf("commitSHA: want non-empty, got empty")
	}
	if got.Result == nil || got.Result.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("Result envelope missing or wrong verdict: %+v", got.Result)
	}

	// Branch landed on the remote.
	br, brErr := exec.Command("git", "-C", bare, "branch", "--list", "foreman/issue-9999").CombinedOutput()
	if brErr != nil {
		t.Fatalf("post-push branch list: %v: %s", brErr, br)
	}
	if !strings.Contains(string(br), "foreman/issue-9999") {
		t.Errorf("branch not on remote: %s", br)
	}

	// Emitted stream carries the JSON result + a final sentinel line the
	// Job poller can match on (mirror of the gate Job's GATE PASS line).
	emitted := out.String()
	if !strings.Contains(emitted, foremanagent.RunTaskResultPrefix) {
		t.Errorf("emitted stream missing result prefix %q:\n%s", foremanagent.RunTaskResultPrefix, emitted)
	}
	if !strings.Contains(emitted, foremanagent.RunTaskSentinelGo) {
		t.Errorf("emitted stream missing GO sentinel %q:\n%s", foremanagent.RunTaskSentinelGo, emitted)
	}

	// The result prefix line must parse back into a RunTaskResult.
	line := resultLine(t, emitted, foremanagent.RunTaskResultPrefix)
	var parsed foremanagent.RunTaskResult
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		t.Fatalf("emitted result line is not valid RunTaskResult JSON: %v\nline=%q", err, line)
	}
	if parsed.Verdict != string(foremanv1alpha1.AgenticTaskVerdictGo) {
		t.Errorf("emitted JSON verdict: want GO got %q", parsed.Verdict)
	}
	if parsed.CommitSHA != got.CommitSHA {
		t.Errorf("emitted JSON commitSHA %q != returned %q", parsed.CommitSHA, got.CommitSHA)
	}
}

// TestRunTask_TaskNotFound returns an error so the Job exits non-zero
// when the named task does not exist in the namespace.
func TestRunTask_TaskNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	cfg := foremanagent.RunTaskConfig{
		Client: c,
		Task:   types.NamespacedName{Namespace: "default", Name: "ghost"},
		RegistryFactory: func(
			_ context.Context, _ string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			return &fakeRegistry{}, nil
		},
	}
	if _, err := foremanagent.RunTask(context.Background(), cfg); err == nil {
		t.Fatal("RunTask: want error for missing task, got nil")
	}
}

// resultLine extracts the JSON payload following the given prefix from a
// multi-line emitted stream. Fails the test if the prefix is absent.
func resultLine(t *testing.T, emitted, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(emitted, "\n") {
		if strings.HasPrefix(l, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(l, prefix))
		}
	}
	t.Fatalf("no line with prefix %q in:\n%s", prefix, emitted)
	return ""
}
