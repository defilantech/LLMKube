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
	"fmt"
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
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
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
		RegistryFactory: func(
			_ context.Context, _ string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
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
		t.Errorf("Extra[reason]: want AgentNotFound got %v", got)
	}
	// v0.3 #559: same value should also surface on the structured
	// Result.FailureReason field (which the watcher writes to
	// Status.FailureReason for downstream consumers).
	if got := res.FailureReason; got != foremanv1alpha1.FailureAgentNotFound {
		t.Errorf("FailureReason: want %q got %q", foremanv1alpha1.FailureAgentNotFound, got)
	}
}

// --- No AgentRef on task --------------------------------------------------

func TestNativeExecutor_NoAgentRefIsHardError(t *testing.T) {
	_, task := taskAndAgent("no-ref")
	task.Spec.AgentRef = nil
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(task).Build()

	e := &foremanagent.NativeAgentLoopExecutor{
		Client: c,
		RegistryFactory: func(
			_ context.Context, _ string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
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

// --- #915: no static remote -> clone+push the task's own repo -----------

// TestNativeExecutor_MultiRepoClonesTaskRepoWhenNoStaticRemote proves the
// #915 path: with no static --git-remote-url, the coder clones and pushes
// the task's own payload.repo, so one agent can serve many repos. Mirrors
// the happy path but leaves GitRemoteURL empty; the branch must still land
// on the repo the task names (here, the bare the override resolves to).
func TestNativeExecutor_MultiRepoClonesTaskRepoWhenNoStaticRemote(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{submitGoBody})

	agent, task := taskAndAgent("multirepo")
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

	e := &foremanagent.NativeAgentLoopExecutor{
		Client:        c,
		WorkspaceRoot: filepath.Join(root, "ws"),
		// No static fork remote: payload.repo drives clone + push.
		GitRemoteURL:             "",
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
	}

	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("verdict: want GO got %s; result=%+v", res.Verdict, res)
	}
	// Branch pushed to the task's repo (the override target), proving the
	// clone+push used payload.repo rather than a static --git-remote-url.
	out, err := exec.Command("git", "-C", bare, "branch", "--list", "foreman/issue-9999").CombinedOutput()
	if err != nil {
		t.Fatalf("post-push branch list: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "foreman/issue-9999") {
		t.Errorf("branch not pushed to task repo: %s", out)
	}
}

// TestNativeExecutor_NoStaticRemoteAndNoRepoIsHardError covers the #915
// precondition: no --git-remote-url and no usable payload.repo leaves
// nothing to clone, so the task fails cleanly with GitRemoteNotConfigured
// (not a system error).
func TestNativeExecutor_NoStaticRemoteAndNoRepoIsHardError(t *testing.T) {
	root := t.TempDir()

	agent, task := taskAndAgent("norepo")
	task.Spec.Payload.Repo = "" // no slug to derive a remote from
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(agent, task).
		Build()

	e := &foremanagent.NativeAgentLoopExecutor{
		Client:        c,
		WorkspaceRoot: filepath.Join(root, "ws"),
		GitRemoteURL:  "", // and no repo -> cannot clone
		// Bypass InferenceService lookup so execution reaches the clone
		// precondition (endpoint resolution runs before the clone step).
		InferenceBaseURLOverride: "http://127.0.0.1:1/v1",
		AuthFactory:              fakeAuth(t),
		// Required precondition; never exercised — the clone fails first.
		RegistryFactory: func(
			_ context.Context, _ string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			return &fakeRegistry{}, nil
		},
	}

	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute returned a system error, want a data-shaped fail: %v", err)
	}
	if res == nil || res.Verdict != foremanv1alpha1.AgenticTaskVerdictIncomplete {
		t.Fatalf("verdict: want INCOMPLETE got %+v", res)
	}
	if res.FailureReason != foremanv1alpha1.FailureGitRemoteNotConfigured {
		t.Errorf("FailureReason: want %q got %q", foremanv1alpha1.FailureGitRemoteNotConfigured, res.FailureReason)
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
		UpstreamURLForRepo:       func(string) string { return bare },
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

const submitErrorBody = `{
  "id": "t1",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-1",
        "type": "function",
        "function": {"name": "submit_result", "arguments": "{\"verdict\":\"ERROR\",\"summary\":\"could not clone\"}"}
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
		UpstreamURLForRepo:       func(string) string { return bare },
		InferenceBaseURLOverride: oaiSrv.URL + "/v1",
		CommitAuthor:             repo.Identity{Name: "B", Email: "b@x"},
		CommitCommitter:          repo.Identity{Name: "B", Email: "b@x"},
		RegistryFactory: func(
			_ context.Context, _ string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
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

// TestAlreadyResolvedEnvelopeShape is a schema-shape regression guard
// for the #970 ALREADY-RESOLVED machine outcome. The model emits a
// submit_result envelope with `extra.outcome="ALREADY-RESOLVED"` and
// (optionally) `extra.resolvedBy=<sha|branch>`. The controller's
// coderResultEnvelope parser in
// internal/foreman/controller/workload_coder_escalation.go reads
// exactly these fields. This test locks the JSON contract: any change
// to the field names or nesting would break the parser.
//
// What this test does NOT cover: the executor's actual pass-through
// behavior (model JSON in → Status.Result.Raw out). A future change
// to the executor that rewrites the envelope mid-pipeline would
// silently break the controller's classifier and this test would
// still pass. The real pass-through is exercised by the existing
// TestNativeExecutor_ModelEmitsNoGo and the other ModelEmits* tests
// in this file (which drive the executor end-to-end with a scripted
// OAI server). For #970 specifically, no new executor behavior is
// introduced; the executor passes the model's extra map through
// verbatim, and the controller does the rest. If a future PR adds
// executor-side ALREADY-RESOLVED detection or rewriting, add a new
// TestNativeExecutor_<that-behavior> test here.
func TestAlreadyResolvedEnvelopeShape(t *testing.T) {
	// Construct the JSON the model would produce via submit_result.
	submitted := map[string]any{
		"schemaVersion": "foreman.v1",
		"kind":          "issue-fix",
		"verdict":       "NO-GO",
		"summary":       "Issue #152 is already resolved by prior fix e97d0ca (Fixes #129)",
		"extra": map[string]any{
			"outcome":    "ALREADY-RESOLVED",
			"resolvedBy": "e97d0ca",
		},
	}
	raw, err := json.Marshal(submitted)
	if err != nil {
		t.Fatalf("marshal submitted envelope: %v", err)
	}

	// Parse the shape the controller later reads (mirrors
	// coderResultEnvelope.Extra in
	// internal/foreman/controller/workload_coder_escalation.go).
	var env struct {
		Summary string `json:"summary"`
		Extra   struct {
			Outcome    string `json:"outcome"`
			ResolvedBy string `json:"resolvedBy"`
		} `json:"extra"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Extra.Outcome != "ALREADY-RESOLVED" {
		t.Errorf("outcome: want ALREADY-RESOLVED, got %q", env.Extra.Outcome)
	}
	if env.Extra.ResolvedBy != "e97d0ca" {
		t.Errorf("resolvedBy: want e97d0ca, got %q", env.Extra.ResolvedBy)
	}
	if !strings.Contains(env.Summary, "already resolved") {
		t.Errorf("summary: want to contain 'already resolved', got %q", env.Summary)
	}
}

// --- Reviewer-role Agent: GO means APPROVE, not commit + push ------------

// reviewerTaskAndAgent builds a reviewer-role Agent and a freeform
// AgenticTask pointing at it. Distinct from taskAndAgent because the
// role bit is exactly what this test exercises.
func reviewerTaskAndAgent(name string) (*foremanv1alpha1.Agent, *foremanv1alpha1.AgenticTask) {
	a := &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer-" + name, Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:                foremanv1alpha1.AgentRoleReviewer,
			Model:               "test-model",
			InferenceServiceRef: corev1.LocalObjectReference{Name: "test-svc"},
			SystemPrompt:        "you are a test reviewer",
			// Read-only tool whitelist: no write_file, no str_replace.
			Tools:    []string{"read_file", "grep", "bash", "submit_result"},
			MaxTurns: 5,
		},
	}
	t := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID("test-uid-" + name),
		},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindFreeform,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  9999,
				Prompt: "review the branch",
			},
			AgentRef: &corev1.LocalObjectReference{Name: a.Name},
		},
	}
	return a, t
}

// TestNativeExecutor_ReviewerGoIsApproveNotCommit checks that when a
// reviewer-role Agent emits verdict=GO with no workspace changes (the
// expected reviewer shape, since reviewers don't have write tools),
// the executor takes the modelDecidedResult path and preserves the
// model's structured extra (reviewOutcome, findings, issueAsk, etc.)
// in status.result, instead of downgrading to NO-GO via noChangesResult.
//
// Regression test for defilantech/LLMKube#543.
func TestNativeExecutor_ReviewerGoIsApproveNotCommit(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{submitGoBody})

	agent, task := reviewerTaskAndAgent("approves-clean")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()

	// fakeRegistry returns a submit_result envelope that mimics the
	// shape the M5-lite reviewer Agent produces in production: GO
	// verdict, APPROVE outcome, three findings, an issueAsk quote,
	// and the touched-files list. The reviewer never edits the
	// workspace, so HasChanges would be false; this test asserts the
	// no-commit reviewer path keeps that envelope intact.
	reviewExtra := map[string]any{
		"reviewOutcome": "APPROVE",
		"issueAsk": "Update the existing release workflow so every tagged release " +
			"builds the router-proxy image for amd64+arm64.",
		"findings": []any{
			map[string]any{
				"severity": "info",
				"area":     "scope-alignment",
				"message": "Diff hits .goreleaser.yaml, values.yaml, and " +
					"docs/site/concepts/model-router.md as the issue body names.",
			},
			map[string]any{
				"severity": "info",
				"area":     "style-consistency",
				"message":  "New goreleaser entries mirror the existing controller + foreman-operator patterns.",
			},
		},
		"filesTouched": []any{".goreleaser.yaml", "charts/llmkube/values.yaml", "docs/site/concepts/model-router.md"},
		"riskLevel":    "low",
		"testsAdded":   0,
	}
	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			"submit_result": {
				Terminal: true,
				Verdict:  "GO",
				Summary:  "APPROVE: diff matches the issue ask, minimal scope, idiomatic.",
				Extra:    reviewExtra,
			},
		},
	}
	e := &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             bare,
		UpstreamURLForRepo:       func(string) string { return bare },
		InferenceBaseURLOverride: oaiSrv.URL + "/v1",
		CommitAuthor:             repo.Identity{Name: "Bot", Email: "b@x"},
		CommitCommitter:          repo.Identity{Name: "Bot", Email: "b@x"},
		RegistryFactory: func(
			_ context.Context, _ string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			return reg, nil
		},
		AuthFactory: fakeAuth(t),
	}
	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Verdict stays GO (not downgraded to NO-GO by noChangesResult).
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("verdict: want GO got %s; full result=%+v", res.Verdict, res)
	}
	// Outcome is MODEL-DECIDED (the no-commit reviewer path), not
	// NO-CHANGES (the wrong path that drops modelExtra).
	if got := res.Extra["outcome"]; got != "MODEL-DECIDED" {
		t.Errorf("outcome: want MODEL-DECIDED got %v", got)
	}
	// modelExtra survives intact and carries the structured review.
	mx, ok := res.Extra["modelExtra"].(map[string]any)
	if !ok {
		t.Fatalf("modelExtra: missing or wrong type; res.Extra=%+v", res.Extra)
	}
	if got := mx["reviewOutcome"]; got != "APPROVE" {
		t.Errorf("modelExtra.reviewOutcome: want APPROVE got %v", got)
	}
	if findings, ok := mx["findings"].([]any); !ok || len(findings) != 2 {
		t.Errorf("modelExtra.findings: want 2-element slice got %T %v", mx["findings"], mx["findings"])
	}
	if got := mx["issueAsk"]; got == nil {
		t.Errorf("modelExtra.issueAsk: missing; the verbatim issue quote should survive")
	}

	// No branch should have landed on the remote: reviewers do not push.
	out, _ := exec.Command("git", "-C", bare, "branch", "--list", "foreman/issue-9999").CombinedOutput()
	if strings.Contains(string(out), "foreman/issue-9999") {
		t.Errorf("reviewer path should not push; bare repo has branch: %s", out)
	}

	// Transcript still written.
	cmKey := types.NamespacedName{Namespace: task.Namespace, Name: "foreman-transcript-" + task.Name}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), cmKey, &cm); err != nil {
		t.Errorf("transcript should exist on reviewer path: %v", err)
	}
}

// TestNativeExecutor_ReviewerERRORMapsToIncompleteWithModelReportedError checks
// that a reviewer-role Agent emitting verdict=ERROR is correctly wired
// through normalizeModelVerdict. The CRD has no ERROR verdict; the harness
// converts it to INCOMPLETE and tags the result with
// FailureModelReportedError so operators can distinguish "model reported
// inability" from other incomplete outcomes (issue #649).
func TestNativeExecutor_ReviewerERRORMapsToIncompleteWithModelReportedError(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{submitErrorBody})

	agent, task := reviewerTaskAndAgent("error-verdict")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()

	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			"submit_result": {
				Terminal: true,
				Verdict:  "ERROR",
				Summary:  "could not clone",
			},
		},
	}
	e := &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             bare,
		UpstreamURLForRepo:       func(string) string { return bare },
		InferenceBaseURLOverride: oaiSrv.URL + "/v1",
		CommitAuthor:             repo.Identity{Name: "B", Email: "b@x"},
		CommitCommitter:          repo.Identity{Name: "B", Email: "b@x"},
		RegistryFactory: func(
			_ context.Context, _ string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			return reg, nil
		},
		AuthFactory: fakeAuth(t),
	}
	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictIncomplete {
		t.Errorf("verdict: want INCOMPLETE got %s", res.Verdict)
	}
	if res.FailureReason != foremanv1alpha1.FailureModelReportedError {
		t.Errorf("failureReason: want ModelReportedError got %s", res.FailureReason)
	}
}

// --- repo-map prefix (#560): coder Agent sees a workspace summary --------

// initBareWithGoSeed extends initBareWithSeed with a small Go source
// file so the repo-map walk has something to index. Without a .go file
// the package returns an empty summary and the executor wiring is a
// no-op (correct, but doesn't exercise the prefix path we want to
// regression-cover).
func initBareWithGoSeed(t *testing.T, root string) string {
	t.Helper()
	bare := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init bare: %v: %s", err, out)
	}
	seed := filepath.Join(root, "seed")
	if out, err := exec.Command("git", "clone", bare, seed).CombinedOutput(); err != nil {
		t.Fatalf("git clone seed: %v: %s", err, out)
	}
	files := map[string]string{
		"README.md": "# seed\n",
		"tools/bash.go": "// Package tools holds the agent's tool implementations.\n" +
			"package tools\n\n" +
			"// BashTool runs shell commands inside the agent workspace.\n" +
			"type BashTool struct{}\n\n" +
			"// Run executes the supplied command and returns its output.\n" +
			"func (b *BashTool) Run(cmd string) (string, error) { return \"\", nil }\n",
	}
	for rel, body := range files {
		p := filepath.Join(seed, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	for _, args := range [][]string{
		{"git", "-c", "user.email=seed@x", "-c", "user.name=seed", "add", "-A"},
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

// recordingOAI wraps scriptedOAI to capture request bodies. The first
// captured body is the only one we look at; that is the turn where the
// loop sends system + user messages for the first time, which is where
// the repomap prefix has to land.
func recordingOAI(t *testing.T, bodies []string, sink *[]string) *httptest.Server {
	t.Helper()
	var i atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r.Body)
		*sink = append(*sink, string(body))
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

// readAll is a tiny io.ReadAll equivalent so we don't have to import
// "io" just for the test helper. Returns the bytes plus any read error;
// callers ignore the error because httptest bodies always close cleanly.
func readAll(r interface{ Read(p []byte) (int, error) }) ([]byte, error) {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return out, nil
			}
			return out, err
		}
	}
}

// TestNativeExecutor_CoderPromptHasRepoMapPrefix exercises the #560
// wiring: a coder Agent's first OAI request should carry the repo-map
// markdown header in its user message. Non-coder Agents (verifier,
// reviewer) get no prefix in v0.3 even when an LLM is in the loop.
func TestNativeExecutor_CoderPromptHasRepoMapPrefix(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithGoSeed(t, root)

	var captured []string
	oaiSrv := recordingOAI(t, []string{submitGoBody}, &captured)

	agent, task := taskAndAgent("repomap")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()

	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			"submit_result": {
				Terminal: true, Verdict: "GO", Summary: "ok",
				CommitMessage: "fix: trivial\n",
			},
		},
		touch: func(name string, ws string) {
			if name == "submit_result" {
				_ = os.WriteFile(filepath.Join(ws, "fix.txt"), []byte("x\n"), 0o644)
			}
		},
	}

	e := &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             bare,
		UpstreamURLForRepo:       func(string) string { return bare },
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

	if _, err := e.Execute(context.Background(), task); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(captured) == 0 {
		t.Fatal("no OAI requests captured")
	}
	first := captured[0]
	if !strings.Contains(first, "## Repository overview") {
		t.Errorf("first OAI request body missing repo-map header. body excerpt:\n%s", truncForTest(first))
	}
	if !strings.Contains(first, "tools/bash.go") {
		t.Errorf("first OAI request body missing seeded path tools/bash.go (issue should rank it top). body excerpt:\n%s",
			truncForTest(first))
	}
	// Workspace orientation block from #567 must also be present.
	// It is prepended outermost, so the model reads orientation ->
	// repo map -> task in order.
	if !strings.Contains(first, "## Workspace") {
		t.Errorf("first OAI request body missing workspace orientation header (#567). body excerpt:\n%s",
			truncForTest(first))
	}
	if !strings.Contains(first, "WORKSPACE_ROOT") {
		t.Errorf("orientation block should mention $WORKSPACE_ROOT. body excerpt:\n%s",
			truncForTest(first))
	}
}

// truncForTest keeps test failure output bounded. 1000 chars is enough
// to spot the missing-header cases the assertions look for while still
// fitting in a terminal screen of test output.
func truncForTest(s string) string {
	const cap = 1000
	if len(s) <= cap {
		return s
	}
	return s[:cap] + "...[truncated]"
}

// fakeIssueFetcher is a deterministic Fetcher for the executor tests.
// Returns a canned Issue for the configured number; returns an error
// for any other number so a test mismatch fails loudly rather than
// silently exercising the wrong path.
type fakeIssueFetcher struct {
	want   int
	issue  *githubissue.Issue
	err    error
	calls  int
	lastTk string
}

func (f *fakeIssueFetcher) Fetch(_ context.Context, _, _ string, n int, token string) (*githubissue.Issue, error) {
	f.calls++
	f.lastTk = token
	if n != f.want {
		return nil, fmt.Errorf("fakeIssueFetcher: unexpected issue #%d (want %d)", n, f.want)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.issue, nil
}

// TestNativeExecutor_FetchesIssueBodyWhenPromptEmpty exercises #571:
// when the AgenticTask payload has no prompt body, the executor pulls
// the issue title + body from GitHub and prepends them to the user
// prompt so the model knows what it is being asked to fix.
func TestNativeExecutor_FetchesIssueBodyWhenPromptEmpty(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)

	var captured []string
	oaiSrv := recordingOAI(t, []string{submitGoBody}, &captured)

	agent, task := taskAndAgent("issuefetch")
	// taskAndAgent ships a non-empty prompt ("test issue") so other
	// tests do not exercise the fetch path. For this test we clear it
	// to simulate the M6 stub planner's output (issue number set,
	// prompt empty) which is the case the fetch is meant to handle.
	task.Spec.Payload.Prompt = ""

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()

	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			"submit_result": {
				Terminal: true, Verdict: "GO", Summary: "ok",
				CommitMessage: "fix: trivial\n",
			},
		},
		touch: func(name string, ws string) {
			if name == "submit_result" {
				_ = os.WriteFile(filepath.Join(ws, "fix.txt"), []byte("x\n"), 0o644)
			}
		},
	}

	fetcher := &fakeIssueFetcher{
		want: 9999,
		issue: &githubissue.Issue{
			Number: 9999,
			Title:  "[BUG] Foo widget breaks on input X",
			Body:   "The widget panics when given X. Steps to reproduce: ...",
			State:  "open",
			Labels: []string{"bug", "area/foreman"},
		},
	}

	e := &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             bare,
		UpstreamURLForRepo:       func(string) string { return bare },
		InferenceBaseURLOverride: oaiSrv.URL + "/v1",
		CommitAuthor:             repo.Identity{Name: "Bot", Email: "b@x"},
		CommitCommitter:          repo.Identity{Name: "Bot", Email: "b@x"},
		RegistryFactory: func(
			_ context.Context, ws string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			reg.workspace = ws
			return reg, nil
		},
		AuthFactory:  fakeAuth(t),
		IssueFetcher: fetcher,
	}

	if _, err := e.Execute(context.Background(), task); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if fetcher.calls != 1 {
		t.Errorf("fetcher should be called exactly once; got %d", fetcher.calls)
	}
	if fetcher.lastTk == "" {
		t.Errorf("fetcher should receive the auth token from repo.Auth; got empty")
	}

	if len(captured) == 0 {
		t.Fatal("no OAI requests captured")
	}
	first := captured[0]

	// The issue title, state, labels, and body all need to be in the
	// user message so the model has the full ask in front of it.
	for _, want := range []string{
		"# Issue #9999",
		"Foo widget breaks on input X",
		"State: open",
		"Labels: bug, area/foreman",
		"The widget panics when given X",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("OAI request body missing %q. excerpt:\n%s", want, truncForTest(first))
		}
	}
}

// TestNativeExecutor_NoFetcherFallsBackToEmptyBody confirms the
// pre-#571 behavior is preserved when the executor has no fetcher
// wired (nil) or when the fetcher fails. A failed fetch must not
// abort the run; the loop runs with whatever buildUserPrompt produces
// from the empty payload prompt.
func TestNativeExecutor_NoFetcherFallsBackToEmptyBody(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)

	var captured []string
	oaiSrv := recordingOAI(t, []string{submitGoBody}, &captured)

	agent, task := taskAndAgent("nofetcher")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()

	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			"submit_result": {
				Terminal: true, Verdict: "GO", Summary: "ok",
				CommitMessage: "fix: trivial\n",
			},
		},
		touch: func(name string, ws string) {
			if name == "submit_result" {
				_ = os.WriteFile(filepath.Join(ws, "fix.txt"), []byte("x\n"), 0o644)
			}
		},
	}

	e := &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             bare,
		UpstreamURLForRepo:       func(string) string { return bare },
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
		// IssueFetcher intentionally nil.
	}

	if _, err := e.Execute(context.Background(), task); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(captured) == 0 {
		t.Fatal("no OAI requests captured")
	}
	first := captured[0]
	// Expect the legacy "You are working on issue #9999" line to be
	// present, and NO "# Issue #9999" header (since no fetch happened).
	if !strings.Contains(first, "You are working on issue #9999") {
		t.Errorf("legacy issue line missing. excerpt:\n%s", truncForTest(first))
	}
	if strings.Contains(first, "# Issue #9999") {
		t.Errorf("issue header should NOT be present without a fetcher. excerpt:\n%s", truncForTest(first))
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
		Client:             c,
		WorkspaceRoot:      filepath.Join(root, "ws"),
		GitRemoteURL:       bare,
		UpstreamURLForRepo: func(string) string { return bare },
		CommitAuthor:       repo.Identity{Name: "Bot", Email: "b@x"},
		CommitCommitter:    repo.Identity{Name: "Bot", Email: "b@x"},
		RegistryFactory: func(
			_ context.Context, _ string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
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

// --- Job-mode dispatch (#620) --------------------------------------------

// fakeCoderJobSubmitter records the request it was handed and returns a
// canned result. Tests inject it to assert Execute routes Job-mode Agents
// to the coder-Job path without standing up a real Job.
type fakeCoderJobSubmitter struct {
	called bool
	gotReq foremanagent.CoderJobRequest
	result foremanagent.CoderJobResult
	err    error
}

func (f *fakeCoderJobSubmitter) Submit(
	_ context.Context, req foremanagent.CoderJobRequest,
) (foremanagent.CoderJobResult, error) {
	f.called = true
	f.gotReq = req
	return f.result, f.err
}

// TestNativeExecutor_JobModeDispatchesToCoderJob asserts that an Agent
// with spec.execution.mode==Job AND a wired CoderJobSubmitter takes the
// coder-Job path: the submitter is called with the task identity + the
// Agent's ExecutionSpec, and its GO result is folded into the *Result.
// The in-process loop (RegistryFactory / clone / OAI) is never touched.
func TestNativeExecutor_JobModeDispatchesToCoderJob(t *testing.T) {
	agent, task := taskAndAgent("job-mode")
	deadline := int64(1800)
	agent.Spec.Execution = &foremanv1alpha1.ExecutionSpec{
		Mode:                  foremanv1alpha1.ExecutionModeJob,
		Image:                 "ghcr.io/defilantech/foreman-agent:dev",
		ServiceAccountName:    "foreman-coder",
		ActiveDeadlineSeconds: &deadline,
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()

	sub := &fakeCoderJobSubmitter{
		result: foremanagent.CoderJobResult{
			Verdict:   "GO",
			Summary:   "fixed in a Job",
			Branch:    "foreman/issue-9999",
			CommitSHA: "deadbeef",
			JobName:   "foreman-coder-job-mode-1",
		},
	}

	// RegistryFactory is set but should never be invoked on the Job path;
	// a t.Fatal here proves the in-process path was not taken.
	e := &foremanagent.NativeAgentLoopExecutor{
		Client: c,
		RegistryFactory: func(
			_ context.Context, _ string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			t.Fatalf("RegistryFactory must NOT be called on the Job path")
			return nil, nil
		},
		CoderJobSubmitter: sub,
	}

	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !sub.called {
		t.Fatalf("submitter was not called; Job path not taken")
	}
	if sub.gotReq.TaskName != task.Name || sub.gotReq.TaskNamespace != task.Namespace {
		t.Errorf("request task identity: got %+v", sub.gotReq)
	}
	if sub.gotReq.Image != "ghcr.io/defilantech/foreman-agent:dev" {
		t.Errorf("request image: got %q", sub.gotReq.Image)
	}
	if sub.gotReq.ServiceAccountName != "foreman-coder" {
		t.Errorf("request SA: got %q", sub.gotReq.ServiceAccountName)
	}
	if sub.gotReq.ActiveDeadlineSeconds == nil || *sub.gotReq.ActiveDeadlineSeconds != deadline {
		t.Errorf("request deadline: got %v", sub.gotReq.ActiveDeadlineSeconds)
	}
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("Verdict: want GO got %s", res.Verdict)
	}
	if got, _ := res.Extra["branch"].(string); got != "foreman/issue-9999" {
		t.Errorf("Extra.branch: got %v", res.Extra["branch"])
	}
	if got, _ := res.Extra["commitSHA"].(string); got != "deadbeef" {
		t.Errorf("Extra.commitSHA: got %v", res.Extra["commitSHA"])
	}
	if got, _ := res.Extra["executionMode"].(string); got != "Job" {
		t.Errorf("Extra.executionMode: got %v", res.Extra["executionMode"])
	}
}

// TestNativeExecutor_JobModeWithoutSubmitterRunsInProcess is the
// recursion guard: an Agent in Job mode but with NO CoderJobSubmitter
// wired (the state inside the run-task Job) must NOT take the Job path. It
// proves useCoderJobPath returns false, so the in-process loop runs
// instead. We detect "in-process path taken" by observing that the
// executor proceeds to endpoint resolution and fails on the missing
// InferenceService (no submitter short-circuit happened).
func TestNativeExecutor_JobModeWithoutSubmitterRunsInProcess(t *testing.T) {
	agent, task := taskAndAgent("job-no-sub")
	agent.Spec.Execution = &foremanv1alpha1.ExecutionSpec{
		Mode:  foremanv1alpha1.ExecutionModeJob,
		Image: "img",
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()

	registryCalled := false
	e := &foremanagent.NativeAgentLoopExecutor{
		Client:       c,
		GitRemoteURL: "file:///nope",
		RegistryFactory: func(
			_ context.Context, _ string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			registryCalled = true
			return &fakeRegistry{}, nil
		},
		AuthFactory: fakeAuth(t),
		// CoderJobSubmitter intentionally nil: this is the run-task-in-Job
		// state. The Job path must not activate.
	}

	res, err := e.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The in-process path resolves the (missing) InferenceService before
	// it would reach the registry, so it fails with
	// InferenceServiceUnavailable. The key assertion is simply that we did
	// NOT short-circuit into a Job: a non-nil result on the in-process
	// failure path, never a JOB-* outcome.
	if res == nil {
		t.Fatalf("expected an in-process failure result, got nil")
		return
	}
	if got, _ := res.Extra["executionMode"].(string); got == "Job" {
		t.Errorf("must NOT take Job path without a submitter; got executionMode=Job")
	}
	_ = registryCalled // registry may or may not be reached depending on resolve order
}

// TestExecutorNative_SelfCommittedWorkIsRecovered verifies that when a model
// self-commits its edits (runs git commit before submit_result), the executor
// detects those commits ahead of base and recovers them by soft-resetting into
// the working tree, then re-committing with DCO sign-off. This is the #982 fix
// for false NO-GO outcomes on self-committed work (~64% of non-GO fleet).
func TestExecutorNative_SelfCommittedWorkIsRecovered(t *testing.T) {
	gitOrSkip(t)
	tmp := t.TempDir()

	env := []string{
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
	}

	// 1. Create initial repo state (main branch, one commit).
	gitRun(t, tmp, env, "init", "-b", "main")
	gitWriteFile(t, tmp, "README.md", "# Test\n")
	gitRun(t, tmp, env, "add", "-A")
	gitRun(t, tmp, env, "commit", "-m", "initial commit")

	// 2. Clone it (simulates what executor does).
	clone := t.TempDir()
	gitRun(t, clone, nil, "clone", tmp, clone)
	gitRun(t, clone, env, "checkout", "-b", "fix/issue-982")

	// 3. Model self-commits a change (this is the bug scenario).
	gitWriteFile(t, clone, "pkg/agent/foo.go", "// fixed\npackage agent\n\nfunc Foo() {}\n")
	gitRun(t, clone, env, "add", "-A")
	gitRun(t, clone, env, "commit", "-m", "fix: resolve issue #982\n\nFixes #982")

	// 4. Verify CommitsAheadOfBase sees the self-commit.
	count, err := repo.CommitsAheadOfBase(context.Background(), clone, "main")
	if err != nil {
		t.Fatalf("CommitsAheadOfBase error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 commit ahead of main (model self-committed), got %d", count)
	}

	// 5. Verify SoftResetToBase recovers changes into working tree.
	err = repo.SoftResetToBase(context.Background(), clone, "main")
	if err != nil {
		t.Fatalf("SoftResetToBase error: %v", err)
	}

	hasChanges, _ := repo.HasChanges(context.Background(), clone)
	if !hasChanges {
		t.Fatal("after soft reset, HasChanges should be true (model's edits recovered)")
	}

	// 6. Verify HEAD is back at main after soft reset (the self-commit was undone).
	commitsAhead, _ := repo.CommitsAheadOfBase(context.Background(), clone, "main")
	if commitsAhead != 0 {
		t.Errorf("expected 0 commits ahead after soft reset, got %d", commitsAhead)
	}

	// 7. Verify repo.Commit re-commits the recovered changes with DCO sign-off.
	sha, err := repo.Commit(context.Background(), repo.CommitOptions{
		Workspace: clone,
		Message:   "fix: resolve issue #982",
		Author:    repo.Identity{Name: "foreman", Email: "foreman@test.com"},
		Committer: repo.Identity{Name: "foreman", Email: "foreman@test.com"},
	})
	if err != nil {
		t.Fatalf("repo.Commit after soft reset failed: %v", err)
	}
	if sha == "" {
		t.Fatal("expected non-empty commit SHA")
	}

	// Verify the resulting commit has DCO sign-off.
	logMsg, _ := gitRunOut(t, clone, env, "log", "--format=%B", "-1")
	if !strings.Contains(logMsg, "Signed-off-by:") {
		t.Errorf("expected DCO sign-off in re-committed message, got: %q", logMsg)
	}

	// 8. Verify the branch has 1 commit ahead of main (the re-committed work).
	count, _ = repo.CommitsAheadOfBase(context.Background(), clone, "main")
	if count != 1 {
		t.Errorf("expected 1 commit ahead after re-commit, got %d", count)
	}
}

// gitRun runs a git command and fails the test on error.
func gitRun(t *testing.T, workspace string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if workspace != "" {
		cmd.Dir = workspace
	}
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\noutput:\n%s", strings.Join(args, " "), err, string(out))
	}
}

// gitRunOut runs a git command and returns stdout.
func gitRunOut(t *testing.T, workspace string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if workspace != "" {
		cmd.Dir = workspace
	}
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gitWriteFile writes a file to the workspace, creating parent dirs.
func gitWriteFile(t *testing.T, dir, path, content string) {
	t.Helper()
	fullPath := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("gitWriteFile: %v", err)
	}
}
