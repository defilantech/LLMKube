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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

// rebase1839Git runs git -C dir, failing the test on error.
func rebase1839Git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// rebase1839Seed builds a fork+upstream pair where a prior-attempt branch on
// the fork edits conflict.go, and the upstream base then edits the SAME line
// differently — so restoring the prior and rebasing it onto the current base
// conflicts (#1839). Returns the mapped production URLs and the prior branch.
func rebase1839Seed(t *testing.T, root string) (forkURL, upstreamURL, priorBranch string) {
	t.Helper()
	const (
		forkRemote     = "https://github.com/Defilan/LLMKube.git"
		upstreamRemote = "https://github.com/defilantech/LLMKube.git"
	)
	initBare := func(p string) {
		if out, err := exec.Command("git", "init", "--bare", "-b", "main", p).CombinedOutput(); err != nil {
			t.Fatalf("git init bare %s: %v: %s", p, err, out)
		}
	}
	clone := func(src, dst string) {
		if out, err := exec.Command("git", "clone", src, dst).CombinedOutput(); err != nil {
			t.Fatalf("git clone %s: %v: %s", src, err, out)
		}
	}
	write := func(dir, body string) {
		if err := os.WriteFile(filepath.Join(dir, "conflict.go"), []byte(body), 0o644); err != nil {
			t.Fatalf("write conflict.go: %v", err)
		}
	}
	commitAll := func(dir, msg string) {
		rebase1839Git(t, dir, "-c", "user.email=u@x", "-c", "user.name=u", "add", "-A")
		rebase1839Git(t, dir, "-c", "user.email=u@x", "-c", "user.name=u", "commit", "-m", msg)
	}

	upstream := filepath.Join(root, "upstream.git")
	fork := filepath.Join(root, "fork.git")
	initBare(upstream)
	initBare(fork)

	// Seed both remotes from one commit.
	seed := filepath.Join(root, "seed")
	clone(upstream, seed)
	write(seed, "package p\n\nconst v = 1\n")
	commitAll(seed, "seed")
	rebase1839Git(t, seed, "push", "origin", "main")
	rebase1839Git(t, seed, "push", fork, "main")

	// Prior attempt on the fork: edit the line, push a branch.
	priorBranch = "foreman/prior-attempt"
	prior := filepath.Join(root, "prior")
	clone(fork, prior)
	rebase1839Git(t, prior, "checkout", "-b", priorBranch)
	write(prior, "package p\n\nconst v = 2 // prior attempt\n")
	commitAll(prior, "prior attempt")
	rebase1839Git(t, prior, "push", "origin", priorBranch)

	// Upstream advances with a CONFLICTING edit to the same line.
	up2 := filepath.Join(root, "up2")
	clone(upstream, up2)
	write(up2, "package p\n\nconst v = 3 // upstream since the prior attempt\n")
	commitAll(up2, "upstream advance")
	rebase1839Git(t, up2, "push", "origin", "main")

	// Map the production URLs onto the local bares via url.insteadOf under a
	// test-scoped HOME (the executor threads HOME through runGit).
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	cfg := "[url \"" + fork + "\"]\n\tinsteadOf = " + forkRemote + "\n" +
		"[url \"" + upstream + "\"]\n\tinsteadOf = " + upstreamRemote + "\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write .gitconfig: %v", err)
	}
	t.Setenv("HOME", home)
	return forkRemote, upstreamRemote, priorBranch
}

// rebase1839Registry returns a coder GO on submit_result WITHOUT resolving the
// rebase. With abort=true it first runs `git rebase --abort` — the model
// "resolving" by returning the branch to its stale pre-rebase tip.
type rebase1839Registry struct {
	workspace string
	abort     bool
}

func (r *rebase1839Registry) Schemas() []oai.Tool { return nil }

func (r *rebase1839Registry) Dispatch(
	_ context.Context, name string, _ json.RawMessage,
) (*foremanagent.ToolResult, error) {
	if name != "submit_result" {
		return nil, fmt.Errorf("rebase1839Registry: unexpected tool %q", name)
	}
	if r.abort && r.workspace != "" {
		_ = exec.Command("git", "-C", r.workspace, "rebase", "--abort").Run()
	}
	return &foremanagent.ToolResult{
		Terminal: true, Verdict: "GO", Summary: "claims done", CommitMessage: "fix: x\n",
	}, nil
}

// TestNativeExecutor_RebaseConflictUnresolvedDowngradesGO drives the fix
// end-to-end: setupTaskBranch leaves the workspace mid-rebase (#1839), the
// coder submits GO without resolving, and runLLMPath's guard downgrades the GO
// to INCOMPLETE/RebaseConflictUnresolved rather than committing a
// merged-work-reverting tree. Covers BOTH the still-mid-rebase case and the
// aborted-to-stale-tip case (which a clean-tree check alone would miss).
func TestNativeExecutor_RebaseConflictUnresolvedDowngradesGO(t *testing.T) {
	gitOrSkip(t)
	for _, tc := range []struct {
		name  string
		abort bool
	}{
		{"still mid-rebase", false},
		{"aborted to stale tip", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fork, upstream, priorBranch := rebase1839Seed(t, root)
			oaiSrv := scriptedOAI(t, []string{submitGoBody})

			agent, task := taskAndAgent("rebase-1839")
			task.Spec.Payload.ReviseFromBranch = priorBranch
			task.Spec.Payload.BranchStrategy = foremanv1alpha1.BranchStrategyRebase

			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()
			reg := &rebase1839Registry{abort: tc.abort}
			e := &foremanagent.NativeAgentLoopExecutor{
				Client:                   c,
				WorkspaceRoot:            filepath.Join(root, "ws"),
				GitRemoteURL:             fork,
				UpstreamURLForRepo:       func(string) string { return upstream },
				InferenceBaseURLOverride: oaiSrv.URL + "/v1",
				CommitAuthor:             repo.Identity{Name: "Bot", Email: "b@x"},
				CommitCommitter:          repo.Identity{Name: "Bot", Email: "b@x"},
				RegistryFactory: func(
					_ context.Context, ws string, _ *foremanv1alpha1.Agent, _ bool,
				) (foremanagent.ToolRegistry, error) {
					reg.workspace = ws
					return reg, nil
				},
				AuthFactory: fakeAuth(t),
			}

			res, err := execWithAgent(t, e, task)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Verdict != foremanv1alpha1.AgenticTaskVerdictIncomplete {
				t.Fatalf("verdict: want INCOMPLETE (unresolved rebase must not land a GO), got %s; result=%+v",
					res.Verdict, res)
			}
			if res.FailureReason != foremanv1alpha1.FailureRebaseConflictUnresolved {
				t.Errorf("FailureReason: want %q got %q",
					foremanv1alpha1.FailureRebaseConflictUnresolved, res.FailureReason)
			}
		})
	}
}
