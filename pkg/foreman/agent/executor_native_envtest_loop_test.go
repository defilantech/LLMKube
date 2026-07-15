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

	"sigs.k8s.io/controller-runtime/pkg/client"
	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

// The post-push envtest gate feedback loop (#768) exercises runLLMPath's
// commit -> push -> gate retry restructure end-to-end.
//
// Two harness facts shape these tests:
//
//  1. The terminal verdict the loop records comes from the ToolResult the
//     tool registry returns for submit_result (loop.dispatchToolCalls), not
//     from the scripted OAI body's tool-call arguments. The shared
//     fakeRegistry (executor_native_test.go) is keyed by tool name and so
//     returns one fixed verdict for every submit_result call; it cannot
//     express "GO on attempt 0, NO-GO on the retry". This file therefore
//     uses a small sequenced registry (seqEnvtestRegistry) whose verdict is
//     driven per submit_result call, which the no-go-on-retry case needs.
//
//  2. changedEnvtestPackages reads `git status -z`, which reports untracked
//     files, so writing a new .go file under internal/controller/ before the
//     commit makes envtestTouched true and arms the post-push gate.

// seqEnvtestRegistry implements foremanagent.ToolRegistry, returning the
// next scripted verdict for each submit_result call (repeating the final
// entry after the script runs out, mirroring scriptedOAI's exhaustion
// rule) and touching a controller file so the change is envtest-backed.
type seqEnvtestRegistry struct {
	verdicts  []string
	calls     int
	workspace string
}

func (r *seqEnvtestRegistry) Schemas() []oai.Tool { return nil }

func (r *seqEnvtestRegistry) Dispatch(
	_ context.Context, name string, _ json.RawMessage,
) (*foremanagent.ToolResult, error) {
	if name != "submit_result" {
		return nil, fmt.Errorf("seqEnvtestRegistry: unexpected tool %q", name)
	}
	touchController(r.workspace)
	i := r.calls
	if i >= len(r.verdicts) {
		i = len(r.verdicts) - 1
	}
	r.calls++
	return &foremanagent.ToolResult{
		Terminal:      true,
		Verdict:       r.verdicts[i],
		Summary:       "fix",
		CommitMessage: "fix: envtest change\n",
	}, nil
}

// scriptedEnvtestRunner returns results[i] for gate call i, repeating the
// final entry after the script runs out.
type scriptedEnvtestRunner struct {
	results []envtestGateResult
	calls   int
}

type envtestGateResult struct {
	pass, ran bool
	feedback  string
}

func (f *scriptedEnvtestRunner) Run(
	_ context.Context, _, _, _, _, _ string,
) (pass bool, ran bool, feedback string) {
	i := f.calls
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	f.calls++
	r := f.results[i]
	return r.pass, r.ran, r.feedback
}

// touchController writes a file under internal/controller/ and stages it so
// changedEnvtestPackages reports an envtest-backed change. Staging is load
// bearing: `git status -z` collapses an entirely-untracked new directory to
// a single "internal/" entry (no .go suffix), which changedEnvtestPackages
// skips; `git add` surfaces the file as an individual staged ".go" path.
func touchController(ws string) {
	dir := filepath.Join(ws, "internal", "controller")
	_ = os.MkdirAll(dir, 0o755)
	rel := filepath.Join("internal", "controller", "zz_envtest_touch.go")
	_ = os.WriteFile(filepath.Join(ws, rel),
		[]byte("package controller\n\n// touched by the envtest-loop test\n"), 0o644)
	_ = exec.Command("git", "-C", ws, "add", rel).Run()
}

// envtestLoopExecutor assembles the same *NativeAgentLoopExecutor literal
// TestNativeExecutor_HappyPathPushesBranch builds, plus the EnvtestJobRunner
// under test. reg's workspace is bound inside RegistryFactory so its touch
// writes into the per-task clone.
func envtestLoopExecutor(
	t *testing.T, root, bare, oaiURL string, c client.Client,
	runner foremanagent.EnvtestJobRunner, reg *seqEnvtestRegistry,
) *foremanagent.NativeAgentLoopExecutor {
	t.Helper()
	return &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             bare,
		UpstreamURLForRepo:       func(string) string { return bare },
		InferenceBaseURLOverride: oaiURL + "/v1",
		CommitAuthor:             repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		CommitCommitter:          repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		RegistryFactory: func(
			_ context.Context, ws string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			reg.workspace = ws
			return reg, nil
		},
		AuthFactory:      fakeAuth(t),
		EnvtestJobRunner: runner,
	}
}

// The gate fails on attempt 0; with the default bound (1) the executor
// re-runs the coder against the same workspace and re-gates, which passes,
// so the task settles GO after exactly two gate calls.
func TestNativeExecutor_EnvtestLoop_ConvergesAfterOneRetry(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{submitGoBody}) // GO on every turn
	agent, task := taskAndAgent("envtest-converge")

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(agent, task).Build()

	reg := &seqEnvtestRegistry{verdicts: []string{"GO"}}
	runner := &scriptedEnvtestRunner{results: []envtestGateResult{
		{pass: false, ran: true, feedback: "controller_test.go:1 boom"}, // attempt 0 fails
		{pass: true, ran: true}, // retry passes
	}}

	e := envtestLoopExecutor(t, root, bare, oaiSrv.URL, c, runner, reg)
	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("verdict: want GO got %s (result=%+v)", res.Verdict, res)
	}
	if runner.calls != 2 {
		t.Fatalf("gate calls: want 2 got %d", runner.calls)
	}
}

// With the bound set to 0, the first gate failure is terminal: the executor
// falls back to the pre-#768 ENVTEST-GATE-FAILED / INCOMPLETE outcome and
// never re-runs the coder.
func TestNativeExecutor_EnvtestLoop_IncompleteAfterCapExhausted(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{submitGoBody})
	agent, task := taskAndAgent("envtest-cap")
	zero := int32(0)
	agent.Spec.MaxEnvtestIterations = &zero // no retries: fail on first gate failure

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(agent, task).Build()

	reg := &seqEnvtestRegistry{verdicts: []string{"GO"}}
	runner := &scriptedEnvtestRunner{results: []envtestGateResult{
		{pass: false, ran: true, feedback: "still broken"},
	}}

	e := envtestLoopExecutor(t, root, bare, oaiSrv.URL, c, runner, reg)
	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictIncomplete {
		t.Fatalf("verdict: want INCOMPLETE got %s", res.Verdict)
	}
	if runner.calls != 1 {
		t.Fatalf("gate calls with cap 0: want 1 got %d", runner.calls)
	}
	if got, _ := res.Extra["outcome"].(string); got != "ENVTEST-GATE-FAILED" {
		t.Fatalf("outcome: want ENVTEST-GATE-FAILED got %q", got)
	}
}

// The retry coder returns NO-GO: the executor surfaces that terminal rather
// than pushing work the coder did not stand behind.
func TestNativeExecutor_EnvtestLoop_NoGoOnRetrySurfaces(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{submitGoBody}) // OAI only needs to call submit_result each turn
	agent, task := taskAndAgent("envtest-nogo")

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(agent, task).Build()

	// GO on attempt 0 (edits committed + pushed), NO-GO on the retry.
	reg := &seqEnvtestRegistry{verdicts: []string{"GO", "NO-GO"}}
	runner := &scriptedEnvtestRunner{results: []envtestGateResult{
		{pass: false, ran: true, feedback: "boom"},
	}}

	e := envtestLoopExecutor(t, root, bare, oaiSrv.URL, c, runner, reg)
	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("verdict: want NO-GO got %s", res.Verdict)
	}
}
