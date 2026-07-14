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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

func gpGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=seed", "GIT_AUTHOR_EMAIL=seed@example.com",
		"GIT_COMMITTER_NAME=seed", "GIT_COMMITTER_EMAIL=seed@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gpSetupWorkspace builds a bare origin seeded with `main` and a workspace clone
// checked out on `branch`; when withChange is true the workspace carries one
// uncommitted file. Returns the workspace path and the bare origin path.
func gpSetupWorkspace(t *testing.T, branch string, withChange bool) (ws, origin string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	gpGit(t, root, "init", "--bare", origin)

	seed := filepath.Join(root, "seed")
	gpGit(t, root, "clone", origin, seed)
	gpGit(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gpGit(t, seed, "add", ".")
	gpGit(t, seed, "commit", "-m", "seed")
	gpGit(t, seed, "push", "-u", "origin", "main")

	ws = filepath.Join(root, "ws")
	gpGit(t, root, "clone", origin, ws)
	gpGit(t, ws, "checkout", "-b", branch)
	if withChange {
		if err := os.WriteFile(filepath.Join(ws, "coder.go"), []byte("package coder\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws, origin
}

func gpOriginHasBranch(t *testing.T, origin, branch string) bool {
	t.Helper()
	out, err := exec.Command("git", "-C", origin, "branch", "--list", branch).CombinedOutput()
	if err != nil {
		t.Fatalf("origin branch --list: %v\n%s", err, out)
	}
	return strings.Contains(string(out), branch)
}

func gpAgent(role foremanv1alpha1.AgentRole) *foremanv1alpha1.Agent {
	return &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{Role: role}}
}

func gpLoopResult(outcome string) *LoopResult {
	return &LoopResult{Terminal: &ToolResult{
		Terminal: true,
		Verdict:  "INCOMPLETE",
		Extra:    map[string]any{outcomeKey: outcome},
	}}
}

func TestMaybePreserveGateFailedBranch(t *testing.T) {
	ident := repo.Identity{Name: "Foreman Bot", Email: "bot@example.com"}
	newExec := func() *NativeAgentLoopExecutor {
		return &NativeAgentLoopExecutor{CommitAuthor: ident, CommitCommitter: ident}
	}
	newTask := func(branch string) *foremanv1alpha1.AgenticTask {
		return &foremanv1alpha1.AgenticTask{
			Spec: foremanv1alpha1.AgenticTaskSpec{
				Payload: foremanv1alpha1.AgenticTaskPayload{Issue: 1109, Branch: branch},
			},
		}
	}

	t.Run("coder CODER-GATE-FAILED with changes preserves the branch", func(t *testing.T) {
		const branch = "foreman/gp/preserve"
		ws, origin := gpSetupWorkspace(t, branch, true)
		r := &Result{Extra: map[string]any{}}
		newExec().maybePreserveGateFailedBranch(context.Background(), logr.Discard(),
			gpAgent(foremanv1alpha1.AgentRoleCoder), newTask(branch), ws, branch, nil,
			gpLoopResult(CoderGateFailedOutcome), r)

		if !gpOriginHasBranch(t, origin, branch) {
			t.Fatalf("expected branch %q pushed to origin", branch)
		}
		if r.Extra["gateFailedBranch"] != branch {
			t.Errorf("gateFailedBranch = %v; want %q", r.Extra["gateFailedBranch"], branch)
		}
		if _, ok := r.Extra["commitSHA"].(string); !ok {
			t.Errorf("commitSHA not recorded (got %v)", r.Extra["commitSHA"])
		}
	})

	t.Run("non-gate INCOMPLETE outcome is not preserved", func(t *testing.T) {
		const branch = "foreman/gp/nongate"
		ws, origin := gpSetupWorkspace(t, branch, true)
		r := &Result{Extra: map[string]any{}}
		newExec().maybePreserveGateFailedBranch(context.Background(), logr.Discard(),
			gpAgent(foremanv1alpha1.AgentRoleCoder), newTask(branch), ws, branch, nil,
			gpLoopResult("STUCK-LOOP-DETECTED"), r)

		if gpOriginHasBranch(t, origin, branch) {
			t.Error("branch pushed for a non-gate outcome; should not preserve")
		}
		if _, ok := r.Extra["gateFailedBranch"]; ok {
			t.Error("gateFailedBranch recorded for a non-gate outcome")
		}
	})

	t.Run("reviewer role is not preserved", func(t *testing.T) {
		const branch = "foreman/gp/reviewer"
		ws, origin := gpSetupWorkspace(t, branch, true)
		r := &Result{Extra: map[string]any{}}
		newExec().maybePreserveGateFailedBranch(context.Background(), logr.Discard(),
			gpAgent(foremanv1alpha1.AgentRoleReviewer), newTask(branch), ws, branch, nil,
			gpLoopResult(CoderGateFailedOutcome), r)

		if gpOriginHasBranch(t, origin, branch) {
			t.Error("branch pushed for a reviewer; reviewers are read-only")
		}
	})

	t.Run("coder gate-failed but no changes is not preserved", func(t *testing.T) {
		const branch = "foreman/gp/nochange"
		ws, origin := gpSetupWorkspace(t, branch, false)
		r := &Result{Extra: map[string]any{}}
		newExec().maybePreserveGateFailedBranch(context.Background(), logr.Discard(),
			gpAgent(foremanv1alpha1.AgentRoleCoder), newTask(branch), ws, branch, nil,
			gpLoopResult(CoderGateFailedOutcome), r)

		if gpOriginHasBranch(t, origin, branch) {
			t.Error("branch pushed with no changes")
		}
		if _, ok := r.Extra["gateFailedBranch"]; ok {
			t.Error("gateFailedBranch recorded with no changes")
		}
	})
}
