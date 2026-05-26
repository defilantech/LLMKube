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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

// gitOrSkip mirrors the helper in the repo subpackage: skip when git is
// not on PATH so the suite stays healthy on minimal containers.
func gitOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
}

// initBareWithSeed creates a bare git repo and seeds it with a single
// commit on `main` so subsequent clone+commit+push round-trips work.
func initBareWithSeed(t *testing.T, root string) string {
	t.Helper()
	bare := filepath.Join(root, "origin.git")
	// -b main pins HEAD so hosts whose init.defaultBranch is `master`
	// (most Ubuntu CI runners) do not end up with a bare whose HEAD
	// references a ref the seed never creates.
	out, err := exec.Command("git", "init", "--bare", "-b", "main", bare).CombinedOutput()
	if err != nil {
		t.Fatalf("git init bare: %v: %s", err, out)
	}
	seed := filepath.Join(root, "seed")
	if out, err := exec.Command("git", "clone", bare, seed).CombinedOutput(); err != nil {
		t.Fatalf("git clone seed: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	for _, args := range [][]string{
		{"git", "-c", "user.email=seed@x", "-c", "user.name=seed", "add", "README.md"},
		{"git", "-c", "user.email=seed@x", "-c", "user.name=seed", "commit", "-m", "seed"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = seed
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	cur, _ := exec.Command("git", "-C", seed, "branch", "--show-current").Output()
	if strings.TrimSpace(string(cur)) != "main" {
		cmd := exec.Command("git", "-C", seed, "branch", "-M", strings.TrimSpace(string(cur)), "main")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("rename main: %v: %s", err, out)
		}
	}
	if out, err := exec.Command("git", "-C", seed, "push", "origin", "main").CombinedOutput(); err != nil {
		t.Fatalf("seed push: %v: %s", err, out)
	}
	return bare
}

// scriptedOAI returns canned chat-completions bodies in order. After
// the script runs out, every subsequent request returns the final body
// (lets the loop hit MaxTurns on stuck-tools tests). Fixtures stay in
// the readable ChatResponse JSON form; the helper converts them to the
// SSE wire format the streaming client expects.
func scriptedOAI(t *testing.T, bodies []string) *httptest.Server {
	t.Helper()
	var i atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		k := int(i.Add(1) - 1)
		if k >= len(bodies) {
			k = len(bodies) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatJSONBodyToSSE(t, bodies[k])))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// chatJSONBodyToSSE wraps a readable ChatResponse JSON fixture into the
// SSE event stream the streaming client reads. Local to this test file
// to keep tests isolated from the oai package's internals.
func chatJSONBodyToSSE(t *testing.T, body string) string {
	t.Helper()
	var parsed oai.ChatResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("chatJSONBodyToSSE: fixture is not a ChatResponse JSON: %v\nbody=%q", err, body)
	}
	var sb strings.Builder
	for _, ch := range parsed.Choices {
		chunk := oai.ChatChunk{
			ID:     parsed.ID,
			Object: "chat.completion.chunk",
			Choices: []oai.ChoiceDelta{
				{
					Index: ch.Index,
					Delta: oai.MessageDelta{
						Role:    ch.Message.Role,
						Content: ch.Message.Content,
					},
					FinishReason: ch.FinishReason,
				},
			},
		}
		for j, tc := range ch.Message.ToolCalls {
			chunk.Choices[0].Delta.ToolCalls = append(
				chunk.Choices[0].Delta.ToolCalls,
				oai.ToolCallDelta{
					Index:    j,
					ID:       tc.ID,
					Type:     tc.Type,
					Function: oai.ToolCallFunctionDelta{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
				},
			)
		}
		out, _ := json.Marshal(chunk)
		sb.WriteString("data: ")
		sb.Write(out)
		sb.WriteString("\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

// fakeRegistry implements foremanagent.ToolRegistry for the executor
// tests. It returns canned ToolResult values keyed by name.
type fakeRegistry struct {
	results map[string]*foremanagent.ToolResult
	// touch is invoked once per dispatch so a test can assert which
	// tool was called and write a file to the workspace if it wants
	// to drive the commit path.
	touch func(name string, workspace string)
	// workspace is captured at construction so touch knows where to
	// write changes for tests that want the commit path to succeed.
	workspace string
	// lastName + lastArgs record the most recent Dispatch call so a
	// test can assert that the executor passed the expected args (e.g.
	// branch + cloneURL) into the tool layer.
	lastName string
	lastArgs json.RawMessage
}

func (r *fakeRegistry) Schemas() []oai.Tool { return nil }

func (r *fakeRegistry) Dispatch(
	_ context.Context, name string, args json.RawMessage,
) (*foremanagent.ToolResult, error) {
	if r.touch != nil {
		r.touch(name, r.workspace)
	}
	r.lastName = name
	r.lastArgs = args
	res, ok := r.results[name]
	if !ok {
		return nil, errors.New("fake registry: unknown tool " + name)
	}
	return res, nil
}

// fakeAuth returns an *Auth that uses an explicit token. Real auth
// reads from env/file; tests pin it so we do not depend on the host's
// environment.
func fakeAuth(t *testing.T) func() (*repo.Auth, error) {
	t.Helper()
	return func() (*repo.Auth, error) {
		return repo.NewAuth("ghp_test_token_unused_for_file_remote")
	}
}

// newScheme returns the fake-client scheme with foreman + inference + corev1 types.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1: %v", err)
	}
	if err := foremanv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("foreman: %v", err)
	}
	return s
}

// taskAndAgent returns a matched pair: an Agent CR and an AgenticTask
// referencing it via spec.agentRef.
func taskAndAgent(name string) (*foremanv1alpha1.Agent, *foremanv1alpha1.AgenticTask) {
	a := &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder-" + name, Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:                foremanv1alpha1.AgentRoleCoder,
			Model:               "test-model",
			InferenceServiceRef: corev1.LocalObjectReference{Name: "test-svc"},
			SystemPrompt:        "you are a test coder",
			Tools:               []string{"read_file", "submit_result"},
			MaxTurns:            5,
		},
	}
	t := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID("test-uid-" + name),
		},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  9999,
				Prompt: "test issue",
			},
			AgentRef: &corev1.LocalObjectReference{Name: a.Name},
		},
	}
	return a, t
}

// --- AgentRef NotFound ----------------------------------------------------

func TestNativeExecutor_AgentNotFound(t *testing.T) {
	_, task := taskAndAgent("no-agent")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(task).Build()

	e := &foremanagent.NativeAgentLoopExecutor{
		Client:       c,
		GitRemoteURL: "file:///nope",
		RegistryFactory: func(string, *foremanv1alpha1.Agent) (foremanagent.ToolRegistry, error) {
			return &fakeRegistry{}, nil
		},
		AuthFactory: fakeAuth(t),
	}
	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil || res.Verdict != foremanv1alpha1.AgenticTaskVerdictIncomplete {
		t.Fatalf("expected INCOMPLETE result, got %+v", res)
	}
	if got := res.Extra["reason"]; got != "AgentNotFound" {
		t.Errorf("reason: want AgentNotFound got %v", got)
	}
}

// --- No AgentRef on task --------------------------------------------------

func TestNativeExecutor_NoAgentRefIsHardError(t *testing.T) {
	_, task := taskAndAgent("no-ref")
	task.Spec.AgentRef = nil
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(task).Build()

	e := &foremanagent.NativeAgentLoopExecutor{
		Client: c,
		RegistryFactory: func(string, *foremanv1alpha1.Agent) (foremanagent.ToolRegistry, error) {
			return &fakeRegistry{}, nil
		},
	}
	if _, err := e.Execute(context.Background(), task); !errors.Is(err, foremanagent.ErrNoAgentRef) {
		t.Errorf("expected ErrNoAgentRef, got %v", err)
	}
}

// --- Happy path: clone, loop, submit_result GO, commit, push -------------

const submitGoBody = `{
  "id": "t1",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-1",
        "type": "function",
        "function": {"name": "submit_result", "arguments": "{\"verdict\":\"GO\",\"summary\":\"fixed\"}"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

func TestNativeExecutor_HappyPathPushesBranch(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{submitGoBody})

	agent, task := taskAndAgent("happy")
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(agent, task).
		Build()

	// fakeRegistry's touch creates a file in the workspace so Commit
	// actually has something to add (otherwise we'd hit
	// ErrNothingToCommit and the test would assert the wrong path).
	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			"submit_result": {
				Terminal: true, Verdict: "GO", Summary: "fixed",
				CommitMessage: "fix: trivial change\n\nSigned-off-by trailer added by `git commit -s`.\n",
			},
		},
		touch: func(name string, ws string) {
			if name == "submit_result" {
				_ = os.WriteFile(filepath.Join(ws, "fix.txt"), []byte("foreman touched this\n"), 0o644)
			}
		},
	}

	e := &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             bare,
		InferenceBaseURLOverride: oaiSrv.URL + "/v1",
		CommitAuthor:             repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		CommitCommitter:          repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		RegistryFactory: func(ws string, _ *foremanv1alpha1.Agent) (foremanagent.ToolRegistry, error) {
			reg.workspace = ws
			return reg, nil
		},
		AuthFactory: fakeAuth(t),
	}

	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("verdict: want GO got %s; result=%+v", res.Verdict, res)
	}
	if got, want := res.Extra["branch"], "foreman/issue-9999"; got != want {
		t.Errorf("branch: want %q got %v", want, got)
	}
	if got := res.Extra["commitSHA"]; got == nil || got == "" {
		t.Errorf("commitSHA missing in Extra: %+v", res.Extra)
	}

	// Branch should be present on the bare remote.
	out, err := exec.Command("git", "-C", bare, "branch", "--list", "foreman/issue-9999").CombinedOutput()
	if err != nil {
		t.Fatalf("post-push branch list: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "foreman/issue-9999") {
		t.Errorf("branch not on remote: %s", out)
	}

	// Transcript ConfigMap should exist with the owner-ref pointing at
	// the task.
	var cm corev1.ConfigMap
	key := types.NamespacedName{
		Namespace: task.Namespace,
		Name:      "foreman-transcript-" + task.Name,
	}
	if err := c.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("get transcript cm: %v", err)
	}
	if _, ok := cm.Data["transcript.json"]; !ok {
		t.Errorf("transcript.json key missing from cm data")
	}
	if got := cm.Labels["foreman.llmkube.dev/transcript-of"]; got != task.Name {
		t.Errorf("transcript label: want %q got %q", task.Name, got)
	}
	if len(cm.OwnerReferences) == 0 || cm.OwnerReferences[0].UID != task.UID {
		t.Errorf("owner ref not set on transcript cm: %+v", cm.OwnerReferences)
	}
}

// --- Loop returns no-change GO -> reported as NO-GO/NO-CHANGES -----------

func TestNativeExecutor_ModelEmitsGoButNoChanges(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{submitGoBody})

	agent, task := taskAndAgent("nochanges")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()

	// No touch this time: registry succeeds, but no files change, so
	// Commit returns ErrNothingToCommit and we expect NO-CHANGES path.
	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			"submit_result": {Terminal: true, Verdict: "GO", Summary: "nothing"},
		},
	}

	e := &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             bare,
		InferenceBaseURLOverride: oaiSrv.URL + "/v1",
		CommitAuthor:             repo.Identity{Name: "Bot", Email: "b@x"},
		CommitCommitter:          repo.Identity{Name: "Bot", Email: "b@x"},
		RegistryFactory: func(ws string, _ *foremanv1alpha1.Agent) (foremanagent.ToolRegistry, error) {
			reg.workspace = ws
			return reg, nil
		},
		AuthFactory: fakeAuth(t),
	}
	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Errorf("verdict: want NO-GO got %s", res.Verdict)
	}
	if got := res.Extra["outcome"]; got != "NO-CHANGES" {
		t.Errorf("outcome: want NO-CHANGES got %v", got)
	}
}

// --- Loop returns NO-GO -> no commit, NO-GO surfaces with MODEL-DECIDED --

const submitNoGoBody = `{
  "id": "t1",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-1",
        "type": "function",
        "function": {"name": "submit_result", "arguments": "{\"verdict\":\"NO-GO\",\"summary\":\"already fixed\"}"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

func TestNativeExecutor_ModelEmitsNoGo(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{submitNoGoBody})

	agent, task := taskAndAgent("nogo")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()

	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			"submit_result": {
				Terminal: true, Verdict: "NO-GO", Summary: "already fixed upstream",
			},
		},
	}
	e := &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             bare,
		InferenceBaseURLOverride: oaiSrv.URL + "/v1",
		CommitAuthor:             repo.Identity{Name: "B", Email: "b@x"},
		CommitCommitter:          repo.Identity{Name: "B", Email: "b@x"},
		RegistryFactory: func(_ string, _ *foremanv1alpha1.Agent) (foremanagent.ToolRegistry, error) {
			return reg, nil
		},
		AuthFactory: fakeAuth(t),
	}
	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Errorf("verdict: want NO-GO got %s", res.Verdict)
	}
	if got := res.Extra["outcome"]; got != "MODEL-DECIDED" {
		t.Errorf("outcome: want MODEL-DECIDED got %v", got)
	}

	// No branch should have landed on the remote.
	out, _ := exec.Command("git", "-C", bare, "branch", "--list", "foreman/issue-9999").CombinedOutput()
	if strings.Contains(string(out), "foreman/issue-9999") {
		t.Errorf("NO-GO path should not push; bare repo has branch: %s", out)
	}

	// Transcript still written.
	cmKey := types.NamespacedName{Namespace: task.Namespace, Name: "foreman-transcript-" + task.Name}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), cmKey, &cm); err != nil {
		t.Fatalf("transcript should exist on NO-GO path: %v", err)
	}
	if apierrors.IsNotFound(err) {
		t.Errorf("transcript not found")
	}
}

// --- Deterministic Agent path (M4): no LLM, single tool dispatch ----------

func TestNativeExecutor_DeterministicGateAgent(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)

	// Gate Agent: no inferenceServiceRef, no systemPrompt, tools list
	// names the deterministic worker tool first. The executor should
	// dispatch that tool directly without spinning up the OAI loop.
	agent := &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder-gate", Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:               foremanv1alpha1.AgentRoleVerifier,
			Tools:              []string{"run_gate_job"},
			RequiredCapability: foremanv1alpha1.RequiredCapability{Roles: []string{"verifier"}},
		},
	}
	task := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gate", Namespace: "default", UID: types.UID("test-uid-gate"),
		},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindVerify,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  9999,
				Branch: "foreman/issue-9999",
			},
			AgentRef: &corev1.LocalObjectReference{Name: agent.Name},
		},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()

	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			// Synthesizes the M4 gate tool's expected envelope.
			"run_gate_job": {
				Terminal: true,
				Verdict:  "GATE-PASS",
				Summary:  "all checks green",
				Output:   map[string]any{"jobName": "foreman-gate-fake-001"},
			},
		},
	}

	dispatched := 0
	e := &foremanagent.NativeAgentLoopExecutor{
		Client:          c,
		WorkspaceRoot:   filepath.Join(root, "ws"),
		GitRemoteURL:    bare,
		CommitAuthor:    repo.Identity{Name: "Bot", Email: "b@x"},
		CommitCommitter: repo.Identity{Name: "Bot", Email: "b@x"},
		RegistryFactory: func(_ string, _ *foremanv1alpha1.Agent) (foremanagent.ToolRegistry, error) {
			dispatched++
			return reg, nil
		},
		AuthFactory: fakeAuth(t),
	}

	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdict("GATE-PASS") {
		t.Errorf("verdict: want GATE-PASS got %s", res.Verdict)
	}
	if got := res.Extra["deterministic"]; got != true {
		t.Errorf("Extra.deterministic: want true got %v", got)
	}
	if got := res.Extra["dispatchedTool"]; got != "run_gate_job" {
		t.Errorf("Extra.dispatchedTool: want run_gate_job got %v", got)
	}
	if dispatched != 1 {
		t.Errorf("RegistryFactory should be called once; got %d", dispatched)
	}

	// No transcript ConfigMap on the deterministic path -- there are
	// no model turns to preserve. Assert it is NOT present.
	var cm corev1.ConfigMap
	cmKey := types.NamespacedName{Namespace: task.Namespace, Name: "foreman-transcript-" + task.Name}
	getErr := c.Get(context.Background(), cmKey, &cm)
	if getErr == nil {
		t.Errorf("transcript ConfigMap should NOT exist on deterministic runs, but it does")
	} else if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected NotFound for transcript cm; got %v", getErr)
	}

	// Args the registry actually saw must carry the payload's branch
	// (not a task-name-derived one) and the executor's GitRemoteURL as
	// cloneURL. Both are required for the gate Job to clone the right
	// branch from the right remote in v0.1. Regression coverage for
	// #528.
	var got map[string]any
	if err := json.Unmarshal(reg.lastArgs, &got); err != nil {
		t.Fatalf("decode dispatched args: %v", err)
	}
	if got["branch"] != "foreman/issue-9999" {
		t.Errorf("dispatched branch: want foreman/issue-9999 got %v", got["branch"])
	}
	if got["cloneURL"] != bare {
		t.Errorf("dispatched cloneURL: want %q got %v", bare, got["cloneURL"])
	}
	if got["repo"] != "defilantech/LLMKube" {
		t.Errorf("dispatched repo: want defilantech/LLMKube got %v", got["repo"])
	}
}
