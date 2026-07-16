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
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
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
		{
			// #573: issue-fix with a Workload owner-ref produces a
			// workload-prefixed branch so reruns on the same issue do
			// not collide.
			name: "issue-fix with workload owner produces workload-prefixed branch",
			task: &foremanv1alpha1.AgenticTask{
				ObjectMeta: metav1.ObjectMeta{
					Name: "code-510",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: foremanv1alpha1.GroupVersion.String(),
							Kind:       "Workload",
							Name:       "v03-validation-batch-rerun-6",
						},
					},
				},
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind:    foremanv1alpha1.AgenticTaskKindIssueFix,
					Payload: foremanv1alpha1.AgenticTaskPayload{Issue: 510},
				},
			},
			want: "foreman/v03-validation-batch-rerun-6/issue-510",
		},
		{
			// Backward-compat: hand-applied issue-fix task without a
			// Workload owner still gets the legacy foreman/issue-N path.
			name: "issue-fix without workload owner uses legacy form",
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
			// A non-Workload owner-ref must not be mistaken for a
			// Workload (cluster-roles can chain-own resources via
			// arbitrary kinds; we only want our own kind).
			name: "non-workload owner-ref does not affect branch name",
			task: &foremanv1alpha1.AgenticTask{
				ObjectMeta: metav1.ObjectMeta{
					Name: "code-507",
					OwnerReferences: []metav1.OwnerReference{
						{APIVersion: "v1", Kind: "ConfigMap", Name: "irrelevant"},
					},
				},
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind:    foremanv1alpha1.AgenticTaskKindIssueFix,
					Payload: foremanv1alpha1.AgenticTaskPayload{Issue: 507},
				},
			},
			want: "foreman/issue-507",
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
		// Bite check defaults on for the verify gate (#787/#799): the gate
		// must reject self-confirming tests without anyone remembering to
		// opt in. A gate you have to enable is a gate that is usually off.
		if got["biteCheck"] != true {
			t.Errorf("biteCheck: want true (default-on for verify gate) got %v", got["biteCheck"])
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

	t.Run("nil GateProfile resolves to golang image", func(t *testing.T) {
		raw := buildDeterministicArgs(task, "foreman/issue-510", "")
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode args: %v", err)
		}
		if got["image"] != "golang:1.26" {
			t.Errorf("image: want golang:1.26 (nil GateProfile default) got %v", got["image"])
		}
		if _, ok := got["generic"]; ok {
			t.Errorf("nil GateProfile must keep the Go gate path (no generic key); got %v", got["generic"])
		}
	})

	t.Run("python GateProfile resolves to python image", func(t *testing.T) {
		pyTask := &foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{Name: "gate-py", Namespace: "default"},
			Spec: foremanv1alpha1.AgenticTaskSpec{
				Kind: foremanv1alpha1.AgenticTaskKindVerify,
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:  "defilantech/LLMKube",
					Issue: 839,
				},
				GateProfile: &foremanv1alpha1.GateProfile{
					Language: foremanv1alpha1.GateLanguagePython,
				},
			},
		}
		raw := buildDeterministicArgs(pyTask, "foreman/issue-839", "")
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode args: %v", err)
		}
		if got["image"] != "python:3.13" {
			t.Errorf("image: want python:3.13 got %v", got["image"])
		}
		if got["generic"] != true {
			t.Errorf("python GateProfile must set generic=true; got %v", got["generic"])
		}
		cmds, ok := got["commands"].([]any)
		if !ok || len(cmds) == 0 {
			t.Fatalf("python GateProfile must set non-empty commands; got %v", got["commands"])
		}
		joined := ""
		for _, c := range cmds {
			joined += c.(string) + "\n"
		}
		if !strings.Contains(joined, "pytest") || !strings.Contains(joined, "ruff") {
			t.Errorf("commands should be the python preset (ruff/pytest); got %v", cmds)
		}
	})
}

// resolveSchemeForTests builds a runtime scheme with the API types the
// resolveInferenceBaseURL tests touch. discovery/v1 covers EndpointSlice,
// inferencev1alpha1 covers InferenceService, foreman covers the
// Agent CR field types referenced incidentally. corev1 is registered for
// the incidental object references the executor builds.
func resolveSchemeForTests(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1: %v", err)
	}
	if err := discoveryv1.AddToScheme(s); err != nil {
		t.Fatalf("discoveryv1: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("inferencev1alpha1: %v", err)
	}
	if err := foremanv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("foreman: %v", err)
	}
	return s
}

// TestResolveInferenceBaseURL pins the precedence rules among the
// three resolution modes (full override, host override + EndpointSlice,
// status.endpoint default) and the error shapes the caller sees when
// any prerequisite is missing. Regression for #540: the static
// override locked the port at install time, so every metal-agent
// respawn broke every subsequent task; the host-override path re-reads
// the live port from the EndpointSlice on each call.
func TestResolveInferenceBaseURL(t *testing.T) {
	// Helpers that build the canned cluster objects each case may want
	// the fake client seeded with.
	mkAgent := func(isvcName string) *foremanv1alpha1.Agent {
		return &foremanv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "default"},
			Spec: foremanv1alpha1.AgentSpec{
				InferenceServiceRef: foremanv1alpha1Local(isvcName),
			},
		}
	}
	mkISvc := func(name, endpoint string) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Status:     inferencev1alpha1.InferenceServiceStatus{Endpoint: endpoint},
		}
	}
	// mkEndpoints builds the EndpointSlice the metal-agent registers, labeled
	// with the kubernetes.io/service-name the consumer lists by. name is the
	// sanitized (hyphenated) service name. When withAddress is false the slice
	// has a port but no ready endpoint, modelling a respawn gap.
	mkEndpoints := func(name string, port int32, withAddress bool) *discoveryv1.EndpointSlice {
		slice := &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels:    map[string]string{"kubernetes.io/service-name": name},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Ports:       []discoveryv1.EndpointPort{{Port: ptr.To(port)}},
		}
		if withAddress {
			slice.Endpoints = []discoveryv1.Endpoint{{
				Addresses:  []string{"10.42.0.5"},
				Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			}}
		}
		return slice
	}

	cases := []struct {
		name        string
		executor    NativeAgentLoopExecutor
		seedObjects []any
		want        string
		wantErrFrag string
	}{
		{
			name: "full override wins over everything else",
			executor: NativeAgentLoopExecutor{
				InferenceBaseURLOverride:     "http://stub:7777/v1/",
				InferenceBaseURLHostOverride: "127.0.0.1", // ignored when full override set
			},
			want: "http://stub:7777/v1",
		},
		{
			name:     "default: status.endpoint cluster-DNS form, chat suffix stripped",
			executor: NativeAgentLoopExecutor{},
			seedObjects: []any{
				mkISvc("test-svc", "http://test-svc.default.svc.cluster.local:80/v1/chat/completions"),
			},
			want: "http://test-svc.default.svc.cluster.local:80/v1",
		},
		{
			name: "host override rewrites host + uses live port from Endpoints",
			executor: NativeAgentLoopExecutor{
				InferenceBaseURLHostOverride: "127.0.0.1",
			},
			seedObjects: []any{
				mkISvc("test-svc", "http://test-svc.default.svc.cluster.local:80/v1/chat/completions"),
				mkEndpoints("test-svc", 60177, true),
			},
			want: "http://127.0.0.1:60177/v1",
		},
		{
			name: "host override: live port flows through after a respawn (different port)",
			executor: NativeAgentLoopExecutor{
				InferenceBaseURLHostOverride: "127.0.0.1",
			},
			seedObjects: []any{
				mkISvc("test-svc", "http://test-svc.default.svc.cluster.local:80/v1/chat/completions"),
				mkEndpoints("test-svc", 49931, true), // metal-agent rolled it to a new port
			},
			want: "http://127.0.0.1:49931/v1",
		},
		{
			name: "host override: dotted InferenceService name maps to hyphenated service-name label",
			executor: NativeAgentLoopExecutor{
				InferenceBaseURLHostOverride: "127.0.0.1",
			},
			seedObjects: []any{
				// Agent references the dotted name; the operator
				// sanitizes dots to hyphens for the service-name label
				// the metal-agent stamps on the EndpointSlice.
				func() *foremanv1alpha1.Agent { return mkAgent("inf.svc.dotted") }(),
				mkISvc("inf.svc.dotted", "http://inf-svc-dotted.default.svc.cluster.local:80/v1/chat/completions"),
				mkEndpoints("inf-svc-dotted", 60177, true),
			},
			want: "http://127.0.0.1:60177/v1",
		},
		{
			name: "host override: missing Endpoints surfaces a clear error (not connect-refused later)",
			executor: NativeAgentLoopExecutor{
				InferenceBaseURLHostOverride: "127.0.0.1",
			},
			seedObjects: []any{
				mkISvc("test-svc", "http://test-svc.default.svc.cluster.local:80/v1/chat/completions"),
				// no EndpointSlice object: an empty list resolves to no ready port
			},
			wantErrFrag: "no ready endpoint with a port",
		},
		{
			name: "host override: EndpointSlice exists but has no ready endpoint",
			executor: NativeAgentLoopExecutor{
				InferenceBaseURLHostOverride: "127.0.0.1",
			},
			seedObjects: []any{
				mkISvc("test-svc", "http://test-svc.default.svc.cluster.local:80/v1/chat/completions"),
				mkEndpoints("test-svc", 60177, false), // port present, no ready endpoint
			},
			wantErrFrag: "no ready endpoint with a port",
		},
		{
			name:        "default: InferenceService not found",
			executor:    NativeAgentLoopExecutor{},
			seedObjects: nil,
			wantErrFrag: "get InferenceService",
		},
		{
			name:     "default: status.endpoint empty (operator has not populated it yet)",
			executor: NativeAgentLoopExecutor{},
			seedObjects: []any{
				mkISvc("test-svc", ""),
			},
			wantErrFrag: "empty status.endpoint",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(resolveSchemeForTests(t))
			// Build the agent for this case: the dotted-name case
			// supplies its own; everything else uses the standard
			// "test-svc" agent.
			var agent *foremanv1alpha1.Agent
			for _, obj := range tc.seedObjects {
				switch v := obj.(type) {
				case *foremanv1alpha1.Agent:
					agent = v
					b = b.WithObjects(v)
				case *inferencev1alpha1.InferenceService:
					b = b.WithObjects(v)
				case *discoveryv1.EndpointSlice:
					b = b.WithObjects(v)
				default:
					t.Fatalf("unhandled seed object type %T", obj)
				}
			}
			if agent == nil {
				agent = mkAgent("test-svc")
			}
			e := tc.executor
			e.Client = b.Build()

			got, err := e.resolveInferenceBaseURL(context.Background(), "default", agent)
			if tc.wantErrFrag != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (result=%q)", tc.wantErrFrag, got)
				}
				if !strings.Contains(err.Error(), tc.wantErrFrag) {
					t.Errorf("error fragment: want %q, got %v", tc.wantErrFrag, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveInferenceBaseURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("URL: want %q got %q", tc.want, got)
			}
		})
	}
}

// foremanv1alpha1Local is a tiny test-side helper to avoid importing
// corev1 directly in the test fixture builder. The Agent CR uses
// corev1.LocalObjectReference for InferenceServiceRef, which the
// production code already depends on; this function names the import
// boundary so the test reads cleanly.
func foremanv1alpha1Local(name string) corev1.LocalObjectReference {
	return corev1.LocalObjectReference{Name: name}
}

// TestFailResult_EmitsStructuredFailureReason pins the v0.3 #559
// invariant: the failResult helper writes the typed reason to BOTH
// Result.FailureReason (the structured field; what the watcher writes
// to Status.FailureReason for downstream consumers) AND
// Result.Extra["reason"] (the back-compat mirror; v0.1 observers).
//
// The whitebox path keeps the surface small; the executor's many
// failResult call sites pass typed constants, and this test pins
// that the conversion to the wire shape is correct.
func TestFailResult_EmitsStructuredFailureReason(t *testing.T) {
	e := &NativeAgentLoopExecutor{}
	cases := []struct {
		name   string
		reason foremanv1alpha1.AgenticTaskFailureReason
	}{
		{"AgentNotFound", foremanv1alpha1.FailureAgentNotFound},
		{"InferenceServiceUnavailable", foremanv1alpha1.FailureInferenceServiceUnavailable},
		{"AuthUnavailable", foremanv1alpha1.FailureAuthUnavailable},
		{"GitRemoteNotConfigured", foremanv1alpha1.FailureGitRemoteNotConfigured},
		{"CloneFailed", foremanv1alpha1.FailureCloneFailed},
		{"InfrastructureError", foremanv1alpha1.FailureInfrastructureError},
		{"ToolFailed", foremanv1alpha1.FailureToolFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := e.failResult(time.Time{}, tc.reason, "some message")
			if r.FailureReason != tc.reason {
				t.Errorf("Result.FailureReason: want %q got %q", tc.reason, r.FailureReason)
			}
			if got := r.Extra["reason"]; got != string(tc.reason) {
				t.Errorf("Result.Extra[reason]: want %q got %v", string(tc.reason), got)
			}
			// Verdict on a failResult is always INCOMPLETE; the watcher
			// patches Phase=Failed in this path.
			if r.Verdict != foremanv1alpha1.AgenticTaskVerdictIncomplete {
				t.Errorf("Verdict: want INCOMPLETE got %q", r.Verdict)
			}
		})
	}
}

// TestIncompleteResult_EmitsStructuredFailureReason mirrors the
// failResult test for the in-loop incomplete path. The incompleteResult
// helper is called when the loop exited cleanly but without a terminal
// (MaxTurns, AssistantNoToolCalls, ctx Timeout). Each path must emit
// the right structured reason.
func TestIncompleteResult_EmitsStructuredFailureReason(t *testing.T) {
	e := &NativeAgentLoopExecutor{}
	cases := []foremanv1alpha1.AgenticTaskFailureReason{
		foremanv1alpha1.FailureMaxTurnsExhausted,
		foremanv1alpha1.FailureModelMisunderstood,
		foremanv1alpha1.FailureTimeout,
		foremanv1alpha1.FailureInfrastructureError,
	}
	for _, reason := range cases {
		t.Run(string(reason), func(t *testing.T) {
			r := e.incompleteResult(
				time.Time{},
				corev1.ObjectReference{Name: "transcript"},
				&LoopResult{Turns: 7},
				reason,
				"msg",
			)
			if r.FailureReason != reason {
				t.Errorf("Result.FailureReason: want %q got %q", reason, r.FailureReason)
			}
			if got := r.Extra["reason"]; got != string(reason) {
				t.Errorf("Result.Extra[reason]: want %q got %v", string(reason), got)
			}
			// turnCount + outcome carry across as before.
			if got := r.Extra["turnCount"]; got != 7 {
				t.Errorf("Extra[turnCount]: want 7 got %v", got)
			}
			if got := r.Extra["outcome"]; got != "LOOP-INCOMPLETE" {
				t.Errorf("Extra[outcome]: want LOOP-INCOMPLETE got %v", got)
			}
		})
	}
}

// TestResolveProviderEndpoint covers the v0.2 cloud-proxy resolution
// path: providerConfig must carry baseURL + model, the optional
// APIKeySecretRef must reference a real Secret, and missing fields
// must surface as clean executor errors rather than 401s from the
// upstream proxy mid-loop.
func TestResolveProviderEndpoint(t *testing.T) {
	mkAgent := func(name string, provider foremanv1alpha1.AgentProvider, cfg *foremanv1alpha1.ProviderConfig) *foremanv1alpha1.Agent {
		return &foremanv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: foremanv1alpha1.AgentSpec{
				Role:           foremanv1alpha1.AgentRoleReviewer,
				Provider:       provider,
				ProviderConfig: cfg,
				Model:          "human-readable-name",
			},
		}
	}
	mkSecret := func(name, key, value string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Data:       map[string][]byte{key: []byte(value)},
		}
	}

	cases := []struct {
		name        string
		agent       *foremanv1alpha1.Agent
		seedObjects []runtime.Object
		wantBase    string
		wantModel   string
		wantAuth    string
		wantErrFrag string
	}{
		{
			name: "cloud-proxy without auth: baseURL + model resolve, authHeader empty",
			agent: mkAgent("cloud-no-auth", foremanv1alpha1.AgentProviderCloudProxy, &foremanv1alpha1.ProviderConfig{
				BaseURL: "http://foundation-router.lan:4000/v1/",
				Model:   "claude-sonnet-4-6",
			}),
			wantBase:  "http://foundation-router.lan:4000/v1",
			wantModel: "claude-sonnet-4-6",
			wantAuth:  "",
		},
		{
			name: "cloud-proxy with Secret: authHeader = 'Bearer <token>'",
			agent: mkAgent("cloud-auth", foremanv1alpha1.AgentProviderCloudProxy, &foremanv1alpha1.ProviderConfig{
				BaseURL: "http://foundation-router.lan:4000/v1",
				Model:   "anthropic/claude-sonnet-4-6",
				APIKeySecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "litellm-master-key"},
					Key:                  "token",
				},
			}),
			seedObjects: []runtime.Object{mkSecret("litellm-master-key", "token", "sk-1234-test\n")},
			wantBase:    "http://foundation-router.lan:4000/v1",
			wantModel:   "anthropic/claude-sonnet-4-6",
			wantAuth:    "Bearer sk-1234-test", // TrimSpace removes the newline
		},
		{
			name:        "cloud-proxy missing providerConfig",
			agent:       mkAgent("cloud-no-cfg", foremanv1alpha1.AgentProviderCloudProxy, nil),
			wantErrFrag: "providerConfig is required",
		},
		{
			name: "cloud-proxy missing baseURL",
			agent: mkAgent("cloud-no-base", foremanv1alpha1.AgentProviderCloudProxy, &foremanv1alpha1.ProviderConfig{
				Model: "claude-sonnet-4-6",
			}),
			wantErrFrag: "baseURL is required",
		},
		{
			name: "cloud-proxy missing model",
			agent: mkAgent("cloud-no-model", foremanv1alpha1.AgentProviderCloudProxy, &foremanv1alpha1.ProviderConfig{
				BaseURL: "http://foundation-router.lan:4000/v1",
			}),
			wantErrFrag: "model is required",
		},
		{
			name: "cloud-proxy: APIKeySecretRef points at nonexistent Secret",
			agent: mkAgent("cloud-missing-secret", foremanv1alpha1.AgentProviderCloudProxy, &foremanv1alpha1.ProviderConfig{
				BaseURL: "http://foundation-router.lan:4000/v1",
				Model:   "claude-sonnet-4-6",
				APIKeySecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "nope"},
					Key:                  "token",
				},
			}),
			wantErrFrag: "get Secret",
		},
		{
			name: "cloud-proxy: APIKeySecretRef key not present in Secret",
			agent: mkAgent("cloud-bad-key", foremanv1alpha1.AgentProviderCloudProxy, &foremanv1alpha1.ProviderConfig{
				BaseURL: "http://foundation-router.lan:4000/v1",
				Model:   "claude-sonnet-4-6",
				APIKeySecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "litellm-master-key"},
					Key:                  "missing-key",
				},
			}),
			seedObjects: []runtime.Object{mkSecret("litellm-master-key", "token", "sk-1234")},
			wantErrFrag: "no value for key",
		},
		{
			name:        "unknown provider value surfaces a clean error (not silently treated as local)",
			agent:       mkAgent("weird-provider", foremanv1alpha1.AgentProvider("rot13"), nil),
			wantErrFrag: "unknown agent.spec.provider",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(resolveSchemeForTests(t))
			objs := []client.Object{tc.agent}
			b = b.WithObjects(tc.agent)
			for _, obj := range tc.seedObjects {
				if co, ok := obj.(client.Object); ok {
					b = b.WithObjects(co)
					objs = append(objs, co)
				}
			}
			e := &NativeAgentLoopExecutor{Client: b.Build()}

			ep, err := e.resolveProviderEndpoint(context.Background(), "default", tc.agent)
			if tc.wantErrFrag != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (endpoint=%+v)", tc.wantErrFrag, ep)
				}
				if !strings.Contains(err.Error(), tc.wantErrFrag) {
					t.Errorf("error fragment: want %q, got %v", tc.wantErrFrag, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveProviderEndpoint: %v", err)
			}
			if ep.baseURL != tc.wantBase {
				t.Errorf("baseURL: want %q got %q", tc.wantBase, ep.baseURL)
			}
			if ep.modelName != tc.wantModel {
				t.Errorf("modelName: want %q got %q", tc.wantModel, ep.modelName)
			}
			if ep.authHeader != tc.wantAuth {
				t.Errorf("authHeader: want %q got %q", tc.wantAuth, ep.authHeader)
			}
		})
	}
}

// TestIsDeterministicAgent pins the rules for the model-free branch:
// only local + empty InferenceServiceRef qualifies. A cloud-proxy
// Agent always runs the LLM loop, even with an empty
// InferenceServiceRef.
func TestIsDeterministicAgent(t *testing.T) {
	cases := []struct {
		name  string
		agent *foremanv1alpha1.Agent
		want  bool
	}{
		{
			name: "local + empty InferenceServiceRef -> deterministic (gate Agent shape)",
			agent: &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{
				Provider: foremanv1alpha1.AgentProviderLocal,
			}},
			want: true,
		},
		{
			name:  "provider unset + empty InferenceServiceRef -> deterministic (v0.1 shape)",
			agent: &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{}},
			want:  true,
		},
		{
			name: "local + InferenceServiceRef set -> LLM",
			agent: &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{
				InferenceServiceRef: corev1.LocalObjectReference{Name: "svc"},
			}},
			want: false,
		},
		{
			name: "cloud-proxy with empty InferenceServiceRef -> LLM (NOT deterministic)",
			agent: &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{
				Provider: foremanv1alpha1.AgentProviderCloudProxy,
			}},
			want: false,
		},
		{
			name: "cloud-proxy with InferenceServiceRef set (defensive) -> LLM",
			agent: &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{
				Provider:            foremanv1alpha1.AgentProviderCloudProxy,
				InferenceServiceRef: corev1.LocalObjectReference{Name: "ignored"},
			}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDeterministicAgent(tc.agent); got != tc.want {
				t.Errorf("want %v got %v", tc.want, got)
			}
		})
	}
}

// TestMCPEnabledForTask pins the default-allow gate the executor passes
// to RegistryFactory: MCP tool access is permitted unless the task
// explicitly opts out via Spec.MCPEnabled=false (the benchmark control
// run case propagated from Workload.Spec.MCPEnabled). A nil task or a
// nil MCPEnabled pointer must NOT disable MCP.
func TestMCPEnabledForTask(t *testing.T) {
	cases := []struct {
		name string
		task *foremanv1alpha1.AgenticTask
		want bool
	}{
		{
			name: "nil task -> enabled",
			task: nil,
			want: true,
		},
		{
			name: "nil MCPEnabled (default) -> enabled",
			task: &foremanv1alpha1.AgenticTask{
				Spec: foremanv1alpha1.AgenticTaskSpec{MCPEnabled: nil},
			},
			want: true,
		},
		{
			name: "MCPEnabled=true -> enabled",
			task: &foremanv1alpha1.AgenticTask{
				Spec: foremanv1alpha1.AgenticTaskSpec{MCPEnabled: ptr.To(true)},
			},
			want: true,
		},
		{
			name: "MCPEnabled=false -> disabled (benchmark control run)",
			task: &foremanv1alpha1.AgenticTask{
				Spec: foremanv1alpha1.AgenticTaskSpec{MCPEnabled: ptr.To(false)},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mcpEnabledForTask(tc.task); got != tc.want {
				t.Errorf("want %v got %v", tc.want, got)
			}
		})
	}
}

// TestProgressConfigFromAgent_ReviewerOverridesEditFree exercises the
// role-aware override in progressConfigFromAgent: reviewer-role agents
// always get EditFreeTurnsLimit=0 (signal disabled), regardless of
// whether the Agent CR set a per-Agent stuckLoopDetection block or
// left it nil. The other signals are unchanged.
//
// Empirical motivation: the rerun-7 batch (2026-05-27) had the qwen
// reviewer correctly investigate a diff for 16 turns and get force-
// terminated by EditFreeStreak even though it was making progress;
// reviewers are read-only by design (their tool whitelist excludes
// write_file / str_replace) so the edit-free signal would fire on
// every well-behaved reviewer run that takes more than the limit to
// finish investigating.
func TestProgressConfigFromAgent_ReviewerOverridesEditFree(t *testing.T) {
	cases := []struct {
		name           string
		agent          *foremanv1alpha1.Agent
		wantEditFree   int
		wantRepeatedTC int
	}{
		{
			name: "coder with default config keeps EditFreeTurnsLimit",
			agent: &foremanv1alpha1.Agent{
				Spec: foremanv1alpha1.AgentSpec{Role: foremanv1alpha1.AgentRoleCoder},
			},
			wantEditFree:   DefaultProgressConfig.EditFreeTurnsLimit,
			wantRepeatedTC: DefaultProgressConfig.RepeatedToolThreshold,
		},
		{
			name: "reviewer with default config gets EditFreeTurnsLimit=0",
			agent: &foremanv1alpha1.Agent{
				Spec: foremanv1alpha1.AgentSpec{Role: foremanv1alpha1.AgentRoleReviewer},
			},
			wantEditFree:   0,
			wantRepeatedTC: DefaultProgressConfig.RepeatedToolThreshold,
		},
		{
			name: "reviewer with explicit per-Agent EditFreeTurnsLimit STILL gets 0",
			agent: &foremanv1alpha1.Agent{
				Spec: foremanv1alpha1.AgentSpec{
					Role: foremanv1alpha1.AgentRoleReviewer,
					StuckLoopDetection: &foremanv1alpha1.StuckLoopDetectionSpec{
						EditFreeTurnsLimit:    25,
						RepeatedToolThreshold: 7,
					},
				},
			},
			wantEditFree:   0, // role override wins
			wantRepeatedTC: 7, // other signals respect the per-Agent override
		},
		{
			name: "coder with explicit per-Agent config preserves all signals",
			agent: &foremanv1alpha1.Agent{
				Spec: foremanv1alpha1.AgentSpec{
					Role: foremanv1alpha1.AgentRoleCoder,
					StuckLoopDetection: &foremanv1alpha1.StuckLoopDetectionSpec{
						EditFreeTurnsLimit:    12,
						RepeatedToolThreshold: 4,
					},
				},
			},
			wantEditFree:   12,
			wantRepeatedTC: 4,
		},
		{
			name: "verifier (deterministic agent) keeps DefaultProgressConfig",
			agent: &foremanv1alpha1.Agent{
				Spec: foremanv1alpha1.AgentSpec{Role: foremanv1alpha1.AgentRoleVerifier},
			},
			wantEditFree:   DefaultProgressConfig.EditFreeTurnsLimit,
			wantRepeatedTC: DefaultProgressConfig.RepeatedToolThreshold,
		},
		{
			name:           "nil agent yields DefaultProgressConfig with no overrides",
			agent:          nil,
			wantEditFree:   DefaultProgressConfig.EditFreeTurnsLimit,
			wantRepeatedTC: DefaultProgressConfig.RepeatedToolThreshold,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := progressConfigFromAgent(tc.agent)
			if got.EditFreeTurnsLimit != tc.wantEditFree {
				t.Errorf("EditFreeTurnsLimit: want %d got %d", tc.wantEditFree, got.EditFreeTurnsLimit)
			}
			if got.RepeatedToolThreshold != tc.wantRepeatedTC {
				t.Errorf("RepeatedToolThreshold: want %d got %d", tc.wantRepeatedTC, got.RepeatedToolThreshold)
			}
		})
	}
}

// TestBuildUserPrompt_ReviewerCaseProducesNonEmptyContent guards
// against the rerun-7 review-510-1 failure: with empty
// Payload.Prompt on AgenticTaskKindReview, the user message used to
// fall to the default branch and emit "". Qwen tolerated empty
// content; Devstral 24B rejected HTTP 400 with "All non-assistant
// messages must contain 'content'". This test ensures the reviewer
// case writes a non-empty, payload-surfacing message regardless of
// whether Payload.Prompt is set.
func TestBuildUserPrompt_ReviewerCaseProducesNonEmptyContent(t *testing.T) {
	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindReview,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  510,
				Branch: "foreman/v04-validation-batch-rerun-7/issue-510",
			},
		},
	}
	got := buildUserPrompt(task)
	if got == "" {
		t.Fatal("reviewer user prompt must not be empty (Devstral 400 fix)")
	}
	for _, want := range []string{
		"reviewing the branch",
		"defilantech/LLMKube",
		"510",
		"foreman/v04-validation-batch-rerun-7/issue-510",
		"Step 1 of your system prompt",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reviewer prompt missing %q in:\n%s", want, got)
		}
	}
}

// TestFileListsEqual_TypeShapes covers the two ways the model's
// claim shows up in extra: as []string (the executor's own writes)
// or as []any (the standard shape after a json.Unmarshal pass over
// submit_result.extra). Equality is order-insensitive because git's
// diff order is deterministic but not semantically meaningful.
// Closes part of #582.
func TestFileListsEqual_TypeShapes(t *testing.T) {
	gt := []string{"a.go", "b.go", "c.go"}

	cases := []struct {
		name string
		prev any
		want bool
	}{
		{"any-slice-same-order", []any{"a.go", "b.go", "c.go"}, true},
		{"any-slice-different-order", []any{"c.go", "a.go", "b.go"}, true},
		{"any-slice-missing-one", []any{"a.go", "b.go"}, false},
		{"any-slice-extra-one", []any{"a.go", "b.go", "c.go", "d.go"}, false},
		{"string-slice-same", []string{"b.go", "a.go", "c.go"}, true},
		{"string-slice-different", []string{"x.go", "y.go", "z.go"}, false},
		{"nil-claim", nil, false},
		{"wrong-type", "a.go,b.go,c.go", false},
		{"any-with-non-string", []any{"a.go", 42, "c.go"}, false},
		{"both-empty-vs-non-empty-gt", []any{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fileListsEqual(tc.prev, gt)
			if got != tc.want {
				t.Errorf("fileListsEqual: want %v got %v", tc.want, got)
			}
		})
	}

	if !fileListsEqual([]any{}, []string{}) {
		t.Error("two empty lists should be equal")
	}
}

// TestReconcileReviewerFilesTouched_OverwritesAndPreservesClaim is the
// headline test for #582: a model that confabulates filesTouched gets
// its claim preserved under filesTouchedClaimed while the actual
// reported filesTouched gets rewritten to the diff's ground truth.
func TestReconcileReviewerFilesTouched_OverwritesAndPreservesClaim(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ws := setupTestRepoWithFeatureBranch(t)

	confabulatedClaim := []any{
		"internal/controller/fake.go",
		"internal/controller/also-fake.go",
		"internal/controller/never-existed.go",
	}
	extra := map[string]any{
		"reviewOutcome": "REQUEST-CHANGES",
		"issueAsk":      "Some asks here",
		"filesTouched":  confabulatedClaim,
		"findings":      []any{},
	}

	reconcileReviewerFilesTouched(context.Background(), logr.Discard(), ws, "main", extra)

	got, ok := extra["filesTouched"].([]string)
	if !ok {
		t.Fatalf("filesTouched should be []string after reconcile, got %T (%v)",
			extra["filesTouched"], extra["filesTouched"])
	}
	wantSet := map[string]bool{"a.go": true, "c.go": true}
	if len(got) != len(wantSet) {
		t.Errorf("filesTouched: want 2 files got %d (%v)", len(got), got)
	}
	for _, p := range got {
		if !wantSet[p] {
			t.Errorf("filesTouched contains unexpected %q (b.go was unchanged)", p)
		}
	}

	claim, ok := extra["filesTouchedClaimed"].([]any)
	if !ok {
		t.Fatalf("filesTouchedClaimed should be []any (preserved); got %T",
			extra["filesTouchedClaimed"])
	}
	if !reflect.DeepEqual(claim, confabulatedClaim) {
		t.Errorf("filesTouchedClaimed mutated: want %v got %v", confabulatedClaim, claim)
	}

	if extra["reviewOutcome"] != "REQUEST-CHANGES" {
		t.Error("reviewOutcome should be untouched by reconciliation")
	}
	if extra["issueAsk"] != "Some asks here" {
		t.Error("issueAsk should be untouched by reconciliation")
	}
}

func TestReconcileReviewerFilesTouched_HonestClaimNoClaimedFieldAdded(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ws := setupTestRepoWithFeatureBranch(t)

	honestClaim := []any{"a.go", "c.go"}
	extra := map[string]any{"filesTouched": honestClaim}
	reconcileReviewerFilesTouched(context.Background(), logr.Discard(), ws, "main", extra)

	if _, present := extra["filesTouchedClaimed"]; present {
		t.Errorf("filesTouchedClaimed should NOT be set when claim matched; got %v",
			extra["filesTouchedClaimed"])
	}
	got, ok := extra["filesTouched"].([]string)
	if !ok {
		t.Fatalf("filesTouched should be canonicalized to []string; got %T",
			extra["filesTouched"])
	}
	if len(got) != 2 {
		t.Errorf("filesTouched: want 2 entries got %v", got)
	}
}

func TestReconcileReviewerFilesTouched_NilExtraIsNoOp(t *testing.T) {
	reconcileReviewerFilesTouched(context.Background(), logr.Discard(), "/tmp", "main", nil)
	reconcileReviewerFilesTouched(context.Background(), logr.Discard(), "", "main", map[string]any{"x": 1})
}

func TestReconcileReviewerFilesTouched_GitFailurePreservesClaim(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ws := t.TempDir()
	if out, err := exec.Command("git", "-C", ws, "init", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	originalClaim := []any{"some-file.go"}
	extra := map[string]any{"filesTouched": originalClaim}
	reconcileReviewerFilesTouched(context.Background(), logr.Discard(), ws, "main", extra)

	got, ok := extra["filesTouched"].([]any)
	if !ok {
		t.Fatalf("filesTouched should keep model's original type on git failure; got %T",
			extra["filesTouched"])
	}
	if !reflect.DeepEqual(got, originalClaim) {
		t.Errorf("filesTouched mutated on git failure: want %v got %v", originalClaim, got)
	}
	if _, claimed := extra["filesTouchedClaimed"]; claimed {
		t.Errorf("filesTouchedClaimed should not be set when reconciliation could not run")
	}
}

func setupTestRepoWithFeatureBranch(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	ctx := context.Background()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdirall %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	write("a.go", "package a\n")
	write("b.go", "package b\n")
	run("add", ".")
	run("commit", "-m", "initial")

	run("checkout", "-b", "feature")
	write("a.go", "package a\n// edit\n")
	write("c.go", "package c\n")
	run("add", ".")
	run("commit", "-m", "feature work")

	return ws
}

// ---- issueAsk reconciliation (#582 follow-on; second confabulation fix) ----

func TestFirstBodyParagraph_SkipsHeadersTakesFirstProse(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"empty", "", 200, ""},
		{"plain-prose", "Fix the thing.", 200, "Fix the thing."},
		{
			"strips-leading-h2",
			"## Bug Description\n\nThe metal-agent picks the wrong IP.",
			200,
			"The metal-agent picks the wrong IP.",
		},
		{
			"strips-leading-h1",
			"# Feature\n\nAdd a make lint-all target nudge to AGENTS.md.",
			200,
			"Add a make lint-all target nudge to AGENTS.md.",
		},
		{
			"joins-wrapped-lines-of-first-paragraph",
			"## Bug Description\n\nLine one of the bug\nstill same paragraph\n\nLine of paragraph two",
			200,
			"Line one of the bug still same paragraph",
		},
		{
			"truncates-at-maxchars",
			"short header\n\n" + strings.Repeat("x", 300),
			50,
			"short header",
		},
		{
			"only-headers-no-prose",
			"## Bug Description\n## Steps\n## Expected",
			200,
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := firstBodyParagraph(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestExtractFetchIssueBody_HappyAndMissing(t *testing.T) {
	// Synthesize a fetch_issue tool-call + tool-result pair in a
	// realistic transcript shape.
	mkMsgs := func(toolID, content string) []oai.Message {
		return []oai.Message{
			{Role: oai.RoleUser, Content: "you are reviewing the branch ..."},
			{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{{
				ID:   toolID,
				Type: "function",
				Function: oai.ToolCallFunction{
					Name:      "fetch_issue",
					Arguments: `{"repo":"defilantech/LLMKube","number":510}`,
				},
			}}},
			{Role: oai.RoleTool, ToolCallID: toolID, Content: content},
		}
	}

	body := extractFetchIssueBody(mkMsgs("tc-1",
		`{"number":510,"title":"docs","body":"The real issue body here.","state":"open"}`))
	if body != "The real issue body here." {
		t.Errorf("happy path body: got %q", body)
	}

	if body := extractFetchIssueBody(nil); body != "" {
		t.Errorf("nil msgs: want empty got %q", body)
	}

	if body := extractFetchIssueBody([]oai.Message{
		{Role: oai.RoleUser, Content: "no tool calls here"},
	}); body != "" {
		t.Errorf("no fetch_issue call: want empty got %q", body)
	}

	if body := extractFetchIssueBody(mkMsgs("tc-2", `not-valid-json`)); body != "" {
		t.Errorf("malformed JSON tool content: want empty got %q", body)
	}

	if body := extractFetchIssueBody(mkMsgs("tc-3",
		`{"number":510,"title":"docs","state":"open"}`)); body != "" {
		t.Errorf("missing body field: want empty got %q", body)
	}

	// Multiple fetch_issue calls: last successful body wins.
	msgs := []oai.Message{
		{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{{ID: "tc-a", Type: "function",
			Function: oai.ToolCallFunction{Name: "fetch_issue", Arguments: `{"number":1}`}}}},
		{Role: oai.RoleTool, ToolCallID: "tc-a",
			Content: `{"body":"first body"}`},
		{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{{ID: "tc-b", Type: "function",
			Function: oai.ToolCallFunction{Name: "fetch_issue", Arguments: `{"number":2}`}}}},
		{Role: oai.RoleTool, ToolCallID: "tc-b",
			Content: `{"body":"second body"}`},
	}
	if got := extractFetchIssueBody(msgs); got != "second body" {
		t.Errorf("last-wins: want %q got %q", "second body", got)
	}
}

func TestReconcileReviewerIssueAsk_VerbatimQuotePreserved(t *testing.T) {
	body := "## Feature Description\n\nAdd a `make lint-all` nudge to AGENTS.md per #508 follow-up."
	msgs := []oai.Message{
		{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{{
			ID: "tc-1", Type: "function",
			Function: oai.ToolCallFunction{
				Name: "fetch_issue", Arguments: `{"repo":"o/r","number":510}`,
			},
		}}},
		{Role: oai.RoleTool, ToolCallID: "tc-1",
			Content: `{"body":"` + strings.ReplaceAll(body, "`", "\\u0060") + `"}`},
	}
	// Pre-serialize the body via json.Marshal so escaping is correct.
	content, _ := json.Marshal(map[string]string{"body": body})
	msgs[1].Content = string(content)

	extra := map[string]any{
		"issueAsk": "Add a `make lint-all` nudge to AGENTS.md per #508 follow-up.",
	}
	reconcileReviewerIssueAsk(logr.Discard(), msgs, extra)

	if extra["issueAsk"] != "Add a `make lint-all` nudge to AGENTS.md per #508 follow-up." {
		t.Errorf("verbatim claim should be preserved unchanged; got %v", extra["issueAsk"])
	}
	if v, _ := extra["issueAskVerified"].(bool); !v {
		t.Errorf("verbatim claim should set issueAskVerified=true; got %v", extra["issueAskVerified"])
	}
	if _, claimed := extra["issueAskClaimed"]; claimed {
		t.Errorf("issueAskClaimed should not be set for honest quote")
	}
}

func TestReconcileReviewerIssueAsk_ConfabulationRewritten(t *testing.T) {
	body := "## Bug Description\n\nmetal-agent picks the wrong IP on multi-NIC macOS hosts."
	content, _ := json.Marshal(map[string]string{"body": body})
	msgs := []oai.Message{
		{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{{
			ID: "tc-1", Type: "function",
			Function: oai.ToolCallFunction{Name: "fetch_issue"},
		}}},
		{Role: oai.RoleTool, ToolCallID: "tc-1", Content: string(content)},
	}

	confab := "Add a reconciler that cleans up orphaned Service+Endpoints objects when agent restarts."
	extra := map[string]any{"issueAsk": confab}
	reconcileReviewerIssueAsk(logr.Discard(), msgs, extra)

	if extra["issueAsk"] != "metal-agent picks the wrong IP on multi-NIC macOS hosts." {
		t.Errorf("confabulated claim should be rewritten with body excerpt; got %v",
			extra["issueAsk"])
	}
	if v, _ := extra["issueAskVerified"].(bool); v {
		t.Errorf("rewritten issueAsk should mark issueAskVerified=false; got true")
	}
	if extra["issueAskClaimed"] != confab {
		t.Errorf("issueAskClaimed should preserve original confabulation; got %v",
			extra["issueAskClaimed"])
	}
}

func TestReconcileReviewerIssueAsk_NoFetchInTranscript(t *testing.T) {
	extra := map[string]any{"issueAsk": "some claim"}
	reconcileReviewerIssueAsk(logr.Discard(), []oai.Message{
		{Role: oai.RoleUser, Content: "no fetch_issue here"},
	}, extra)
	if extra["issueAsk"] != "some claim" {
		t.Errorf("with no fetch_issue body in transcript, claim should be preserved unchanged; got %v", extra["issueAsk"])
	}
	if _, verified := extra["issueAskVerified"]; verified {
		t.Errorf("issueAskVerified should NOT be set when reconciliation could not run")
	}
}

func TestReconcileReviewerIssueAsk_EmptyClaimFilledFromBody(t *testing.T) {
	body := "## Feature\n\nDocument the new --lint-all flag."
	content, _ := json.Marshal(map[string]string{"body": body})
	msgs := []oai.Message{
		{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{{
			ID: "tc-1", Type: "function",
			Function: oai.ToolCallFunction{Name: "fetch_issue"},
		}}},
		{Role: oai.RoleTool, ToolCallID: "tc-1", Content: string(content)},
	}
	extra := map[string]any{} // model omitted issueAsk entirely
	reconcileReviewerIssueAsk(logr.Discard(), msgs, extra)
	if extra["issueAsk"] != "Document the new --lint-all flag." {
		t.Errorf("empty claim should be filled from body excerpt; got %v", extra["issueAsk"])
	}
	if v, _ := extra["issueAskVerified"].(bool); v {
		t.Errorf("body-derived issueAsk should mark issueAskVerified=false")
	}
	if _, claimed := extra["issueAskClaimed"]; claimed {
		t.Errorf("issueAskClaimed should not be set when model had no claim to archive")
	}
}

func TestReconcileReviewerIssueAsk_NilExtraIsNoOp(t *testing.T) {
	reconcileReviewerIssueAsk(logr.Discard(), nil, nil) // must not panic
}

func TestReconcileReviewerIssueAsk_ParaphraseVerified(t *testing.T) {
	body := "## Feature Description\n\nUpdate the toolchain image to v2 in the agent-builder Dockerfile so golangci-lint and controller-gen are available at build time."
	content, _ := json.Marshal(map[string]string{"body": body})
	msgs := []oai.Message{
		{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{{
			ID: "tc-1", Type: "function",
			Function: oai.ToolCallFunction{Name: "fetch_issue"},
		}}},
		{Role: oai.RoleTool, ToolCallID: "tc-1", Content: string(content)},
	}
	// Faithful paraphrase: mentions the key nouns (toolchain, image, v2, agent-builder, golangci-lint, controller-gen).
	claim := "Update the toolchain image to v2 in the agent-builder so golangci-lint and controller-gen are available."
	extra := map[string]any{"issueAsk": claim}
	reconcileReviewerIssueAsk(logr.Discard(), msgs, extra)
	if v, _ := extra["issueAskVerified"].(bool); !v {
		t.Errorf("faithful paraphrase should be verified via semantic coverage; got %v", extra["issueAskVerified"])
	}
	if extra["issueAsk"] != claim {
		t.Errorf("paraphrase should be preserved unchanged; got %v", extra["issueAsk"])
	}
	if extra["issueAskMethod"] != "semantic" {
		t.Errorf("semantic verification should set issueAskMethod=semantic; got %v", extra["issueAskMethod"])
	}
}

func TestReconcileReviewerIssueAsk_HallucinationRewritten(t *testing.T) {
	body := "## Feature Description\n\nUpdate the toolchain image to v2 in the agent-builder Dockerfile so golangci-lint and controller-gen are available at build time."
	content, _ := json.Marshal(map[string]string{"body": body})
	msgs := []oai.Message{
		{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{{
			ID: "tc-1", Type: "function",
			Function: oai.ToolCallFunction{Name: "fetch_issue"},
		}}},
		{Role: oai.RoleTool, ToolCallID: "tc-1", Content: string(content)},
	}
	// Confabulation: mentions unrelated nouns (cache, list, CLI).
	claim := "enhance `llmkube cache list` to show cached model digests."
	extra := map[string]any{"issueAsk": claim}
	reconcileReviewerIssueAsk(logr.Discard(), msgs, extra)
	if v, _ := extra["issueAskVerified"].(bool); v {
		t.Errorf("hallucinated claim should not be verified; got %v", extra["issueAskVerified"])
	}
	if extra["issueAsk"] == claim {
		t.Errorf("hallucinated claim should be rewritten; got %v", extra["issueAsk"])
	}
	if extra["issueAskClaimed"] != claim {
		t.Errorf("issueAskClaimed should preserve original claim; got %v", extra["issueAskClaimed"])
	}
}

func TestExtractIssueKeywords_Basic(t *testing.T) {
	body := "## Feature Description\n\nUpdate the toolchain image to v2 in the agent-builder Dockerfile so golangci-lint and controller-gen are available at build time."
	kw := extractIssueKeywords(body)
	if len(kw) == 0 {
		t.Fatalf("expected non-empty keywords")
	}
	// Should include salient nouns.
	found := map[string]bool{}
	for _, k := range kw {
		found[k] = true
	}
	for _, want := range []string{"toolchain", "image", "agent-builder", "golangci-lint", "controller-gen"} {
		if !found[want] {
			t.Errorf("expected keyword %q in %v", want, kw)
		}
	}
}

func TestExtractIssueKeywords_StopsFiltered(t *testing.T) {
	body := "the a an the feature is implemented with the new tool"
	kw := extractIssueKeywords(body)
	for _, k := range kw {
		if k == "the" || k == "a" || k == "an" || k == "is" || k == "with" {
			t.Errorf("stop word %q should be filtered; got %v", k, kw)
		}
	}
}

func TestIssueAskSemanticallyCovers_ShortClaim(t *testing.T) {
	// Short claim with enough keyword coverage should pass.
	body := "## Feature\n\nUpdate the toolchain image to v2 in the agent-builder Dockerfile so golangci-lint and controller-gen are available at build time."
	if !issueAskSemanticallyCovers("update toolchain image agent-builder golangci-lint controller-gen", body) {
		t.Errorf("claim covering enough keywords should pass")
	}
	if issueAskSemanticallyCovers("xyz abc def ghi jkl", body) {
		t.Errorf("irrelevant claim should fail")
	}
}

func TestEnforceReviewerIssueAsk_VerifiedGoStands(t *testing.T) {
	extra := map[string]any{"issueAskVerified": true}
	got := enforceReviewerIssueAsk(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictGo, false, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("verified GO should stand; got %v", got)
	}
	if _, demoted := extra["verdictDemoted"]; demoted {
		t.Errorf("verified review should not be annotated as demoted")
	}
}

func TestEnforceReviewerIssueAsk_UnverifiedGoDemotedToNoGo(t *testing.T) {
	extra := map[string]any{"issueAskVerified": false}
	got := enforceReviewerIssueAsk(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictGo, true, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Errorf("unverified GO must demote to NO-GO; got %v", got)
	}
	if v, _ := extra["verdictDemoted"].(bool); !v {
		t.Errorf("demotion must set verdictDemoted=true; got %v", extra["verdictDemoted"])
	}
	if extra["verdictClaimed"] != string(foremanv1alpha1.AgenticTaskVerdictGo) {
		t.Errorf("verdictClaimed should archive the original GO; got %v", extra["verdictClaimed"])
	}
	if reason, _ := extra["demotionReason"].(string); reason == "" {
		t.Errorf("demotionReason must explain the demotion")
	}
}

func TestEnforceReviewerIssueAsk_UnverifiedNoGoKeptButMarked(t *testing.T) {
	extra := map[string]any{"issueAskVerified": false}
	got := enforceReviewerIssueAsk(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictNoGo, true, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Errorf("unverified NO-GO should stay NO-GO; got %v", got)
	}
	if v, _ := extra["verdictDemoted"].(bool); !v {
		t.Errorf("unverified NO-GO must still be marked verdictDemoted=true so the escalation reviewer knows the base verdict is untrusted")
	}
	if extra["verdictClaimed"] != string(foremanv1alpha1.AgenticTaskVerdictNoGo) {
		t.Errorf("verdictClaimed should archive the original NO-GO; got %v", extra["verdictClaimed"])
	}
}

func TestEnforceReviewerIssueAsk_AbsentFieldIsObserveOnly(t *testing.T) {
	// issueAskVerified absent means the harness had no fetch_issue body
	// to verify against (a harness-side gap, not model dishonesty);
	// enforcement must not fire.
	extra := map[string]any{"issueAsk": "some claim"}
	got := enforceReviewerIssueAsk(logr.Discard(), extra, foremanv1alpha1.AgenticTaskVerdictGo, false, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("absent issueAskVerified must not demote; got %v", got)
	}
	if _, demoted := extra["verdictDemoted"]; demoted {
		t.Errorf("absent issueAskVerified should not be annotated as demoted")
	}
}

func TestEnforceReviewerIssueAsk_NilExtraIsNoOp(t *testing.T) {
	got := enforceReviewerIssueAsk(logr.Discard(), nil, foremanv1alpha1.AgenticTaskVerdictGo, false, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("nil extra must pass the verdict through; got %v", got)
	}
}

// TestEnforceReviewerIssueAsk_UnverifiedGoScopeVouchKeepsGo covers #744:
// an honest reviewer that paraphrases the issue ask (issueAskVerified==false)
// but whose diff touches a file named in the issue (scope vouch) must keep
// GO instead of being demoted to NO-GO.
func TestEnforceReviewerIssueAsk_UnverifiedGoScopeVouchKeepsGo(t *testing.T) {
	extra := map[string]any{"issueAskVerified": false}
	got := enforceReviewerIssueAsk(logr.Discard(), extra,
		foremanv1alpha1.AgenticTaskVerdictGo, false, []string{"internal/controller/model_controller.go"})
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("unverified GO with scope vouch must keep GO; got %v", got)
	}
	if v, _ := extra["verdictDemoted"].(bool); !v {
		t.Errorf("demotion flag must still be set for observability; got %v", extra["verdictDemoted"])
	}
	if extra["verdictClaimed"] != string(foremanv1alpha1.AgenticTaskVerdictGo) {
		t.Errorf("verdictClaimed should archive the original GO; got %v", extra["verdictClaimed"])
	}
	if v, _ := extra["scopeVouched"].(bool); !v {
		t.Errorf("scopeVouched should be true when scope rail confirms in-scope")
	}
	if reason, _ := extra["demotionReason"].(string); reason == "" {
		t.Errorf("demotionReason must explain the scope-vouch outcome")
	}
}

// TestEnforceReviewerIssueAsk_UnverifiedGoNoScopeVouchDemotes covers the
// case where the model paraphrases the issue ask AND scope-overlap detects
// drift (or has no refs to vouch). This must still demote to NO-GO.
func TestEnforceReviewerIssueAsk_UnverifiedGoNoScopeVouchDemotes(t *testing.T) {
	extra := map[string]any{"issueAskVerified": false}
	got := enforceReviewerIssueAsk(logr.Discard(), extra,
		foremanv1alpha1.AgenticTaskVerdictGo, true, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Errorf("unverified GO with scope drift must demote to NO-GO; got %v", got)
	}
	if v, _ := extra["verdictDemoted"].(bool); !v {
		t.Errorf("demotion must set verdictDemoted=true; got %v", extra["verdictDemoted"])
	}
	if extra["verdictClaimed"] != string(foremanv1alpha1.AgenticTaskVerdictGo) {
		t.Errorf("verdictClaimed should archive the original GO; got %v", extra["verdictClaimed"])
	}
	if reason, _ := extra["demotionReason"].(string); reason == "" {
		t.Errorf("demotionReason must explain the demotion")
	}
}

// TestEnforceReviewerIssueAsk_DocOnlyScopeMatchDemotes covers the
// misospace/miso-chat#687 shape: the issue named a config file
// (package.json) AND a doc (SECURITY_REVIEW.md), the diff touched only the
// doc, and issueAsk verification failed. Scope-overlap "matched" the doc, so
// the vouch used to keep the GO. A documentation-only scope match is too weak
// to rescue an unverifiable GO — demote to NO-GO so it escalates for
// confirmation.
func TestEnforceReviewerIssueAsk_DocOnlyScopeMatchDemotes(t *testing.T) {
	extra := map[string]any{"issueAskVerified": false}
	got := enforceReviewerIssueAsk(logr.Discard(), extra,
		foremanv1alpha1.AgenticTaskVerdictGo, false, []string{"SECURITY_REVIEW.md"})
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Errorf("doc-only scope match must not vouch; want NO-GO, got %v", got)
	}
	if _, vouched := extra["scopeVouched"]; vouched {
		t.Errorf("doc-only scope match must not set scopeVouched")
	}
	if v, _ := extra["verdictDemoted"].(bool); !v {
		t.Errorf("demotion must set verdictDemoted=true; got %v", extra["verdictDemoted"])
	}
}

// TestEnforceReviewerIssueAsk_MixedScopeMatchVouches verifies the non-doc
// requirement is "any", not "all": a matched set carrying at least one
// non-doc file still vouches even alongside a matched doc.
func TestEnforceReviewerIssueAsk_MixedScopeMatchVouches(t *testing.T) {
	extra := map[string]any{"issueAskVerified": false}
	got := enforceReviewerIssueAsk(logr.Discard(), extra,
		foremanv1alpha1.AgenticTaskVerdictGo, false, []string{"README.md", "pkg/foo.go"})
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("a non-doc match should vouch even alongside docs; want GO, got %v", got)
	}
	if v, _ := extra["scopeVouched"].(bool); !v {
		t.Errorf("scopeVouched should be true when a non-doc file matched")
	}
}

// TestEnforceReviewerIssueAsk_UnverifiedGoNoRefsNoVouchDemotes covers the
// case where the issue has no concrete file refs (scope observe-only) and
// the model paraphrased the ask. Without scope vouch, must demote.
func TestEnforceReviewerIssueAsk_UnverifiedGoNoRefsNoVouchDemotes(t *testing.T) {
	extra := map[string]any{"issueAskVerified": false}
	got := enforceReviewerIssueAsk(logr.Discard(), extra,
		foremanv1alpha1.AgenticTaskVerdictGo, false, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Errorf("unverified GO with no scope refs must demote to NO-GO; got %v", got)
	}
	if v, _ := extra["verdictDemoted"].(bool); !v {
		t.Errorf("demotion must set verdictDemoted=true; got %v", extra["verdictDemoted"])
	}
}

// TestNormalizeModelVerdict_ErrorMapsToIncompleteWithModelReportedError pins
// the #649 fix: the submit_result tool contract allows verdict="ERROR" (model
// reports it cannot complete the task: a reviewer's could-not-review, a
// coder's unrecoverable-error), but the CRD does not store ERROR as a
// verdict. normalizeModelVerdict must convert it to INCOMPLETE with
// FailureModelReportedError. The watcher backstop (issue #649 follow-up
// commit) covers any remaining out-of-enum strings that reach the terminal
// patch; normalizeModelVerdict handles only the ERROR contract case.
//
// All other verdicts (GO, NO-GO, GATE-PASS, GATE-FAIL, GATE-ERROR,
// INCOMPLETE) must pass through unchanged with an empty failure reason.
func TestNormalizeModelVerdict_ErrorMapsToIncompleteWithModelReportedError(t *testing.T) {
	cases := []struct {
		raw         string
		wantVerdict foremanv1alpha1.AgenticTaskVerdict
		wantReason  foremanv1alpha1.AgenticTaskFailureReason
	}{
		{
			raw:         "ERROR",
			wantVerdict: foremanv1alpha1.AgenticTaskVerdictIncomplete,
			wantReason:  foremanv1alpha1.FailureModelReportedError,
		},
		{
			raw:         "GO",
			wantVerdict: foremanv1alpha1.AgenticTaskVerdictGo,
			wantReason:  "",
		},
		{
			raw:         "NO-GO",
			wantVerdict: foremanv1alpha1.AgenticTaskVerdictNoGo,
			wantReason:  "",
		},
		{
			raw:         "INCOMPLETE",
			wantVerdict: foremanv1alpha1.AgenticTaskVerdictIncomplete,
			wantReason:  "",
		},
		{
			raw:         "GATE-PASS",
			wantVerdict: foremanv1alpha1.AgenticTaskVerdictGatePass,
			wantReason:  "",
		},
		{
			raw:         "GATE-FAIL",
			wantVerdict: foremanv1alpha1.AgenticTaskVerdictGateFail,
			wantReason:  "",
		},
		{
			raw:         "GATE-ERROR",
			wantVerdict: foremanv1alpha1.AgenticTaskVerdictGateError,
			wantReason:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			gotVerdict, gotReason := normalizeModelVerdict(tc.raw)
			if gotVerdict != tc.wantVerdict {
				t.Errorf("verdict: want %q got %q", tc.wantVerdict, gotVerdict)
			}
			if gotReason != tc.wantReason {
				t.Errorf("reason: want %q got %q", tc.wantReason, gotReason)
			}
		})
	}
}

// TestCoderJobResultToResult_EmbeddedFailureReasonPreserved pins the
// Job-mode supervisor fix: when an in-pod run-task produces an INCOMPLETE
// verdict with FailureReason=ModelReportedError (model called submit_result
// with ERROR), the supervisor must lift the embedded reason rather than
// overwrite it with the generic FailureMaxTurnsExhausted.
func TestCoderJobResultToResult_EmbeddedFailureReasonPreserved(t *testing.T) {
	start := time.Now().Add(-time.Second)
	cjr := CoderJobResult{
		Verdict:       string(foremanv1alpha1.AgenticTaskVerdictIncomplete),
		Summary:       "model reported it could not complete the task",
		FailureReason: string(foremanv1alpha1.FailureModelReportedError),
	}
	r := coderJobResultToResult("issue-fix", start, cjr)
	if r.Verdict != foremanv1alpha1.AgenticTaskVerdictIncomplete {
		t.Errorf("verdict: want INCOMPLETE got %s", r.Verdict)
	}
	if r.FailureReason != foremanv1alpha1.FailureModelReportedError {
		t.Errorf("failureReason: want ModelReportedError got %s", r.FailureReason)
	}
}

// TestCoderJobResultToResult_NoEmbeddedReasonFallsBackToMaxTurns confirms
// that an INCOMPLETE result with no embedded FailureReason still defaults
// to FailureMaxTurnsExhausted (the pre-fix behavior for the common
// max-turns case).
func TestCoderJobResultToResult_NoEmbeddedReasonFallsBackToMaxTurns(t *testing.T) {
	start := time.Now().Add(-time.Second)
	cjr := CoderJobResult{
		Verdict: string(foremanv1alpha1.AgenticTaskVerdictIncomplete),
		Summary: "hit max turns without a verdict",
	}
	r := coderJobResultToResult("issue-fix", start, cjr)
	if r.Verdict != foremanv1alpha1.AgenticTaskVerdictIncomplete {
		t.Errorf("verdict: want INCOMPLETE got %s", r.Verdict)
	}
	if r.FailureReason != foremanv1alpha1.FailureMaxTurnsExhausted {
		t.Errorf("failureReason: want MaxTurnsExhausted got %s", r.FailureReason)
	}
}

// ---- workspace resilience (#654) ----

// TestRemoveAllResilient_HappyPath verifies that removeAllResilient
// removes a normal (all-writable) tree successfully.
func TestRemoveAllResilient_HappyPath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	target := filepath.Join(root, "sub")
	if err := removeAllResilient(target); err != nil {
		t.Fatalf("removeAllResilient on normal tree: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target should not exist after removal; stat err: %v", err)
	}
}

// TestRemoveAllResilient_ReadOnlyTree verifies that removeAllResilient
// succeeds when the workspace contains a read-only file (0444) inside
// a read-only directory (0555) — the shape left behind by envtest-
// fetched etcd binaries (issue #654). A bare os.RemoveAll fails with
// "permission denied" on this fixture; removeAllResilient must not.
//
// Fixture layout:
//
//	<target>/
//	  child/         (0555: read-only dir — blocks unlink of its children)
//	    data.bin     (0444: read-only file)
//
// On Linux and macOS the kernel checks the *parent directory* write
// permission before allowing unlink; making the dir 0555 is what
// causes permission denied, not merely the file mode.
//
// Two separate temp-rooted fixtures are used:
//  1. A "canary" fixture proves that bare os.RemoveAll actually fails on
//     this platform/user combination; the test is skipped (not failed) if
//     the OS bypasses permission checks (e.g. running as root in CI).
//  2. A fresh "target" fixture is passed to removeAllResilient so the
//     helper does its own chmod-and-retry without relying on any pre-
//     applied chmod from the canary phase.
func TestRemoveAllResilient_ReadOnlyTree(t *testing.T) {
	buildFixture := func(root string) (target, child string) {
		target = filepath.Join(root, "workspace")
		child = filepath.Join(target, "child")
		if err := os.MkdirAll(child, 0o755); err != nil {
			t.Fatalf("mkdir child: %v", err)
		}
		if err := os.WriteFile(filepath.Join(child, "data.bin"), []byte("binary"), 0o444); err != nil {
			t.Fatalf("write data.bin: %v", err)
		}
		if err := os.Chmod(child, 0o555); err != nil { //nolint:gosec // intentional: building read-only fixture to test resilient removal
			t.Fatalf("chmod child: %v", err)
		}
		return target, child
	}

	// Phase 1: confirm the fixture actually breaks bare os.RemoveAll.
	// Use a dedicated temp root so a failed partial RemoveAll does not
	// corrupt the fixture used in phase 2.
	canaryRoot := t.TempDir()
	canaryTarget, canaryChild := buildFixture(canaryRoot)
	_ = canaryChild
	if err := os.RemoveAll(canaryTarget); err == nil {
		// Running as root (some CI environments) bypasses permission
		// checks. Skip rather than false-pass.
		t.Skip("os.RemoveAll succeeded on read-only fixture (running as root?); skipping")
	}
	// Repair the canary so t.TempDir's own cleanup can remove it.
	_ = os.Chmod(canaryChild, 0o755) //nolint:gosec // intentional: restoring writable perms so test cleanup succeeds

	// Phase 2: build a fresh fixture and test removeAllResilient.
	targetRoot := t.TempDir()
	target, _ := buildFixture(targetRoot)

	if err := removeAllResilient(target); err != nil {
		t.Fatalf("removeAllResilient failed on read-only tree: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target should not exist after resilient removal; stat err: %v", err)
	}
}

// TestRemoveAllResilient_NonExistent verifies that a non-existent path
// is treated as a no-op (mirrors os.RemoveAll behaviour).
func TestRemoveAllResilient_NonExistent(t *testing.T) {
	if err := removeAllResilient(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatalf("removeAllResilient on non-existent path should be a no-op: %v", err)
	}
}

// TestRemoveAllResilient_SymlinkToExternal is a security regression test for
// the case where the workspace contains a symlink pointing to a file OUTSIDE
// the workspace. os.Chmod follows symlinks, so a naive chmod of every entry
// would silently rewrite the target's permissions.
//
// Fixture layout:
//
//	<workspaceRoot>/
//	  workspace/
//	    readonly/        (0555: blocks RemoveAll without repair)
//	      link -> <externalRoot>/victim.txt
//
//	<externalRoot>/
//	  victim.txt         (0755: mode that must not change)
//
// Assertions:
//  1. The workspace tree is fully removed.
//  2. The external victim still exists with its original mode (0755).
func TestRemoveAllResilient_SymlinkToExternal(t *testing.T) {
	// Build external target in its own temp root.
	externalRoot := t.TempDir()
	victim := filepath.Join(externalRoot, "victim.txt")
	if err := os.WriteFile(victim, []byte("external"), 0o755); err != nil { //nolint:gosec // intentional: specific mode for assertion
		t.Fatalf("create victim: %v", err)
	}

	// Build the workspace.
	workspaceRoot := t.TempDir()
	workspace := filepath.Join(workspaceRoot, "workspace")
	readonly := filepath.Join(workspace, "readonly")
	if err := os.MkdirAll(readonly, 0o755); err != nil {
		t.Fatalf("mkdir readonly: %v", err)
	}
	link := filepath.Join(readonly, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Make the directory read-only so RemoveAll needs the chmod-and-retry path.
	if err := os.Chmod(readonly, 0o555); err != nil { //nolint:gosec // intentional: building read-only fixture to test resilient removal
		t.Fatalf("chmod readonly: %v", err)
	}

	// Phase 1: confirm the fixture actually breaks bare os.RemoveAll.
	// If it doesn't (e.g. running as root), skip rather than give a false pass.
	canaryRoot := t.TempDir()
	canaryWorkspace := filepath.Join(canaryRoot, "workspace")
	canaryReadonly := filepath.Join(canaryWorkspace, "readonly")
	if err := os.MkdirAll(canaryReadonly, 0o755); err != nil {
		t.Fatalf("mkdir canary readonly: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(canaryReadonly, "link")); err != nil {
		t.Fatalf("canary symlink: %v", err)
	}
	if err := os.Chmod(canaryReadonly, 0o555); err != nil { //nolint:gosec // intentional: building read-only canary fixture
		t.Fatalf("chmod canary readonly: %v", err)
	}
	if removeErr := os.RemoveAll(canaryWorkspace); removeErr == nil {
		// Running as root bypasses permission checks; repair canary and skip.
		_ = os.Chmod(canaryReadonly, 0o755) //nolint:gosec // intentional: restoring writable perms so test cleanup succeeds
		t.Skip("os.RemoveAll succeeded on read-only fixture (running as root?); skipping")
	}
	_ = os.Chmod(canaryReadonly, 0o755) //nolint:gosec // intentional: restoring writable perms so test cleanup succeeds

	// Phase 2: run removeAllResilient on the real workspace fixture.
	if err := removeAllResilient(workspace); err != nil {
		t.Fatalf("removeAllResilient failed: %v", err)
	}

	// Assertion (a): workspace fully removed.
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Errorf("workspace should not exist after removal; stat err: %v", err)
	}

	// Assertion (b): external victim still exists with its original mode.
	info, err := os.Stat(victim)
	if err != nil {
		t.Fatalf("external victim should still exist: %v", err)
	}
	gotMode := info.Mode().Perm()
	const wantMode = os.FileMode(0o755)
	if gotMode != wantMode {
		t.Errorf("external victim mode changed: got %04o, want %04o", gotMode, wantMode)
	}
}

func TestGateAdvisories_LandInResultExtra(t *testing.T) {
	acc := &[]advisory{}
	*acc = append(*acc, advisory{Check: "grounding-breadth", Detail: "cites dcgm_gpu_utilization (unknown)"})
	extra := map[string]any{"branch": "b", "commitSHA": "s"}
	attachGateAdvisories(extra, acc)
	got, ok := extra["gateAdvisories"].([]advisory)
	if !ok || len(got) != 1 || got[0].Check != "grounding-breadth" {
		t.Fatalf("want gateAdvisories with grounding-breadth, got %#v", extra["gateAdvisories"])
	}
}

func TestAttachGateAdvisories_OmitsWhenEmpty(t *testing.T) {
	extra := map[string]any{"branch": "b"}
	attachGateAdvisories(extra, &[]advisory{})
	if _, present := extra["gateAdvisories"]; present {
		t.Fatal("empty advisories should not add the key")
	}
}

func TestRenderGateAdvisories_RendersWhenPresent(t *testing.T) {
	advs := []foremanv1alpha1.GateAdvisory{
		{Check: "grounding-breadth", Detail: "cites dcgm_gpu_utilization (unknown)"},
		{Check: "scope-overlap", Detail: "diff touches files not mentioned in the issue"},
	}
	got := renderGateAdvisories(advs)
	if got == "" {
		t.Fatal("want non-empty output for non-empty advisories")
	}
	for _, want := range []string{"grounding-breadth", "scope-overlap", "dcgm_gpu_utilization"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered advisories missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderGateAdvisories_OmittedWhenEmpty(t *testing.T) {
	if got := renderGateAdvisories(nil); got != "" {
		t.Errorf("nil advisories: want empty string, got %q", got)
	}
	if got := renderGateAdvisories([]foremanv1alpha1.GateAdvisory{}); got != "" {
		t.Errorf("empty advisories: want empty string, got %q", got)
	}
}

// TestBuildUserPrompt_IssueFix_RendersPromptPrefix verifies that a
// PromptPrefix set on an issue-fix payload is rendered before the fetched
// issue body, so an escalation-tier diagnosis hint coexists with the issue
// context rather than replacing it.
func TestBuildUserPrompt_IssueFix_RendersPromptPrefix(t *testing.T) {
	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:         "defilantech/LLMKube",
				Issue:        944,
				Prompt:       "The issue body.",
				PromptPrefix: "A previous smaller-model attempt could not resolve this.",
			},
		},
	}
	got := buildUserPrompt(task)
	pi := strings.Index(got, "previous smaller-model attempt")
	bi := strings.Index(got, "The issue body.")
	if pi < 0 {
		t.Fatalf("prompt prefix missing from: %q", got)
	}
	if bi >= 0 && pi > bi {
		t.Errorf("prompt prefix must precede the issue body; got prefix@%d body@%d", pi, bi)
	}
}

// TestBuildUserPrompt_ReviewerAppendsAdvisories verifies that gate advisories
// wired into the payload by the reconciler are included in the reviewer's
// first user message so the model is prompted to confirm or dismiss each one.
func TestBuildUserPrompt_ReviewerAppendsAdvisories(t *testing.T) {
	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindReview,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  510,
				Branch: "foreman/wl/issue-510",
				GateAdvisories: []foremanv1alpha1.GateAdvisory{
					{Check: "grounding-breadth", Detail: "cites dcgm_gpu_utilization (unknown)"},
					{Check: "scope-overlap", Detail: "diff touches api/foreman/v1alpha1/types.go not in the issue"},
				},
			},
		},
	}
	got := buildUserPrompt(task)
	for _, want := range []string{
		"reviewing the branch",
		"Gate advisories to verify",
		"grounding-breadth",
		"dcgm_gpu_utilization",
		"scope-overlap",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reviewer prompt with advisories missing %q in:\n%s", want, got)
		}
	}
}

// TestBuildUserPrompt_ReviewerOmitsAdvisoryBlockWhenNone verifies that the
// advisory block is silently absent when the payload carries no advisories,
// so the prompt stays clean for tasks where the coder gate found nothing.
func TestBuildUserPrompt_ReviewerOmitsAdvisoryBlockWhenNone(t *testing.T) {
	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindReview,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  510,
				Branch: "foreman/wl/issue-510",
			},
		},
	}
	got := buildUserPrompt(task)
	if strings.Contains(got, "Gate advisories") {
		t.Errorf("reviewer prompt without advisories must not contain advisory block; got:\n%s", got)
	}
	// Must still produce a non-empty, useful prompt.
	if !strings.Contains(got, "reviewing the branch") {
		t.Errorf("reviewer prompt missing base content; got:\n%s", got)
	}
}

// stubIssueFetcher is a minimal whitebox Fetcher: it returns the
// canned issue and counts calls. The blackbox fakeIssueFetcher in
// executor_native_test.go drives the end-to-end path; this one pins
// fetchIssueBodyIfNeeded's prompt-composition logic in isolation.
type stubIssueFetcher struct {
	issue *githubissue.Issue
	calls int
}

func (s *stubIssueFetcher) Fetch(context.Context, string, string, int, string) (*githubissue.Issue, error) {
	s.calls++
	return s.issue, nil
}

// TestFetchIssueBodyIfNeeded_AppendsIssueOnRevisionTask covers the
// revision path (#951): on a task with reviseFromBranch set, the
// review-feedback prompt must not suppress the issue fetch — the
// original ask is APPENDED under an "## Original issue" heading so the
// retry sees both the feedback and what it is actually fixing.
func TestFetchIssueBodyIfNeeded_AppendsIssueOnRevisionTask(t *testing.T) {
	fetcher := &stubIssueFetcher{issue: &githubissue.Issue{
		Number: 641,
		Title:  "Widget breaks on input X",
		Body:   "The widget panics when given X. Acceptance: no panic.",
		State:  "open",
		Labels: []string{"bug"},
	}}
	const feedback = "The reviewer rejected your previous attempt at issue #641 (verdict NO-GO)."
	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:             "defilantech/LLMKube",
				Issue:            641,
				Branch:           "foreman/wl/issue-641",
				ReviseFromBranch: "foreman/wl/issue-641",
				Prompt:           feedback,
			},
		},
	}

	fetchIssueBodyIfNeeded(context.Background(), fetcher, task, nil, logr.Discard())

	if fetcher.calls != 1 {
		t.Fatalf("fetcher calls = %d, want 1", fetcher.calls)
	}
	got := task.Spec.Payload.Prompt
	if !strings.HasPrefix(got, feedback) {
		t.Errorf("existing prompt must stay first; got:\n%s", got)
	}
	for _, want := range []string{
		"## Original issue (#641): Widget breaks on input X",
		"State: open",
		"Labels: bug",
		"The widget panics when given X. Acceptance: no panic.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("appended prompt missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "# Issue #641:") {
		t.Errorf("append path must use the Original-issue heading, not the empty-prompt H1; got:\n%s", got)
	}
}

// TestFetchIssueBodyIfNeeded_NoIssueNumberIsNoOp pins the Issue==0
// contract: without an issue number there is nothing to fetch, and an
// existing prompt must pass through untouched.
func TestFetchIssueBodyIfNeeded_NoIssueNumberIsNoOp(t *testing.T) {
	fetcher := &stubIssueFetcher{issue: &githubissue.Issue{Number: 1, Title: "unused"}}
	const prompt = "hand-authored task prompt with no issue reference"
	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Prompt: prompt,
			},
		},
	}

	fetchIssueBodyIfNeeded(context.Background(), fetcher, task, nil, logr.Discard())

	if fetcher.calls != 0 {
		t.Errorf("fetcher must not be called when Issue is 0; calls = %d", fetcher.calls)
	}
	if task.Spec.Payload.Prompt != prompt {
		t.Errorf("prompt must be untouched; got %q", task.Spec.Payload.Prompt)
	}
}

// TestFetchIssueBodyIfNeeded_ComposedPromptWithoutRevisionUntouched
// pins the pre-#951 contract for NON-revision tasks: a composed prompt
// (bridge- or hand-authored) owns the task context, so the fetch is
// suppressed even though an issue number is present.
func TestFetchIssueBodyIfNeeded_ComposedPromptWithoutRevisionUntouched(t *testing.T) {
	fetcher := &stubIssueFetcher{issue: &githubissue.Issue{Number: 641, Title: "unused"}}
	const prompt = "bridge-composed prompt that already embeds the issue text"
	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  641,
				Prompt: prompt,
			},
		},
	}

	fetchIssueBodyIfNeeded(context.Background(), fetcher, task, nil, logr.Discard())

	if fetcher.calls != 0 {
		t.Errorf("fetcher must not be called for a composed prompt without reviseFromBranch; calls = %d", fetcher.calls)
	}
	if task.Spec.Payload.Prompt != prompt {
		t.Errorf("prompt must be untouched; got %q", task.Spec.Payload.Prompt)
	}
}

// ---- setupTaskBranch (#951 revise-from-branch restore) ----

// gitIn runs git in dir with a hermetic identity, failing the test on
// error and returning trimmed stdout.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// seededRemote creates a bare remote with one commit on main and
// returns its path plus the main tip SHA.
func seededRemote(t *testing.T, dir string) (bare, mainSHA string) {
	t.Helper()
	bare = filepath.Join(dir, "origin.git")
	gitIn(t, "", "init", "--bare", "-b", "main", bare)
	seed := filepath.Join(dir, "seed")
	gitIn(t, "", "clone", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	gitIn(t, seed, "add", "README.md")
	gitIn(t, seed, "commit", "-m", "seed")
	gitIn(t, seed, "push", "origin", "main")
	return bare, gitIn(t, seed, "rev-parse", "HEAD")
}

// TestReviewerDiffBase_ResolvesUpstreamNotStaleForkMain guards #1005: the
// reviewer clones the fork, whose local `main` lags the upstream base the
// coder branch was cut from. Diffing against local `main` sweeps in the whole
// intervening upstream delta; reviewerDiffBase must instead resolve the fresh
// upstream base so the diff isolates the coder's change.
func TestReviewerDiffBase_ResolvesUpstreamNotStaleForkMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	bare, _ := seededRemote(t, dir) // upstream main at A

	// Fork clone taken BEFORE upstream advances: local "main" stays at A.
	fork := filepath.Join(dir, "fork")
	gitIn(t, "", "clone", bare, fork)

	// Upstream advances by one unrelated commit AFTER the fork was cloned.
	adv := filepath.Join(dir, "adv")
	gitIn(t, "", "clone", bare, adv)
	if err := os.WriteFile(filepath.Join(adv, "UPSTREAM.md"), []byte("intervening upstream delta\n"), 0o644); err != nil {
		t.Fatalf("write UPSTREAM.md: %v", err)
	}
	gitIn(t, adv, "add", "UPSTREAM.md")
	gitIn(t, adv, "commit", "-m", "intervening upstream commit")
	gitIn(t, adv, "push", "origin", "main")

	// Coder branch: cut from the CURRENT upstream tip, plus a one-file fix.
	gitIn(t, fork, "fetch", bare, "main")
	gitIn(t, fork, "checkout", "-b", "fix/issue-1005", "FETCH_HEAD")
	fixDir := filepath.Join(fork, "pkg", "agent")
	if err := os.MkdirAll(fixDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixDir, "fix.go"), []byte("// fix\npackage agent\n"), 0o644); err != nil {
		t.Fatalf("write fix.go: %v", err)
	}
	gitIn(t, fork, "add", "-A")
	gitIn(t, fork, "commit", "-m", "fix: the coder change under review")

	// Pre-condition: against the STALE local main, the diff wrongly includes
	// the upstream delta (the bug this fix removes).
	if stale := gitIn(t, fork, "diff", "--name-only", "main...HEAD"); !strings.Contains(stale, "UPSTREAM.md") {
		t.Fatalf("pre-condition wrong: local-main diff should include the upstream delta, got %q", stale)
	}

	// reviewerDiffBase resolves the fresh upstream base instead of local main.
	e := &NativeAgentLoopExecutor{UpstreamURLForRepo: func(string) string { return bare }}
	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind:    foremanv1alpha1.AgenticTaskKindReview,
			Payload: foremanv1alpha1.AgenticTaskPayload{Repo: "defilantech/LLMKube", BaseBranch: "main"},
		},
	}
	base := e.reviewerDiffBase(context.Background(), logr.Discard(), task, fork)
	if base == "main" {
		t.Fatal("reviewerDiffBase fell back to local main; expected the resolved upstream base SHA")
	}

	// Diffing against the resolved base isolates the coder's change.
	got := gitIn(t, fork, "diff", "--name-only", base+"...HEAD")
	if !strings.Contains(got, "pkg/agent/fix.go") {
		t.Errorf("diff should include the coder fix, got %q", got)
	}
	if strings.Contains(got, "UPSTREAM.md") {
		t.Errorf("BUG #1005: reviewer diff base still sweeps in the upstream delta, got %q", got)
	}
}

func revisionTask(branch string) *foremanv1alpha1.AgenticTask {
	return &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:             "defilantech/LLMKube",
				Issue:            641,
				Branch:           branch,
				ReviseFromBranch: branch,
				// The in-review revision path restores the prior attempt and
				// rebases it onto the current base (#1029). Under the default
				// reset strategy the restore is skipped entirely.
				BranchStrategy: foremanv1alpha1.BranchStrategyRebase,
			},
		},
	}
}

// TestSetupTaskBranch_RestoresPriorAttempt drives the executor-owned
// restore (#951): with reviseFromBranch set and the ref present on the
// push remote, the working branch starts at the prior attempt's tip
// (files present), NOT at a fresh fetch of the base branch.
func TestSetupTaskBranch_RestoresPriorAttempt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	bare, _ := seededRemote(t, dir)
	const branch = "foreman/wl/issue-641"

	// Prior attempt pushed to the remote.
	prior := filepath.Join(dir, "prior")
	gitIn(t, "", "clone", bare, prior)
	gitIn(t, prior, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(prior, "fix.txt"), []byte("attempt 1\n"), 0o644); err != nil {
		t.Fatalf("write fix.txt: %v", err)
	}
	gitIn(t, prior, "add", "fix.txt")
	gitIn(t, prior, "commit", "-m", "attempt 1")
	gitIn(t, prior, "push", "origin", branch)
	priorSHA := gitIn(t, prior, "rev-parse", "HEAD")

	// Fresh executor-style workspace clone.
	ws := filepath.Join(dir, "ws")
	gitIn(t, "", "clone", bare, ws)

	err := setupTaskBranch(context.Background(), revisionTask(branch), ws, branch, "main",
		func(string) string { return bare }, nil, logr.Discard())
	if err != nil {
		t.Fatalf("setupTaskBranch: %v", err)
	}
	if got := gitIn(t, ws, "rev-parse", "HEAD"); got != priorSHA {
		t.Errorf("HEAD = %s, want prior attempt tip %s (restore must not rebuild from base)", got, priorSHA)
	}
	if _, err := os.Stat(filepath.Join(ws, "fix.txt")); err != nil {
		t.Errorf("prior attempt's file must be present: %v", err)
	}
	if got := gitIn(t, ws, "branch", "--show-current"); got != branch {
		t.Errorf("current branch = %q, want %q", got, branch)
	}
}

// TestSetupTaskBranch_MissingRefFallsBackToBase pins the degradation
// contract: when the reviseFromBranch ref is gone from the remote the
// task still runs — branched from the upstream base — instead of
// failing.
func TestSetupTaskBranch_MissingRefFallsBackToBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	bare, mainSHA := seededRemote(t, dir)
	const branch = "foreman/wl/issue-641"

	ws := filepath.Join(dir, "ws")
	gitIn(t, "", "clone", bare, ws)

	err := setupTaskBranch(context.Background(), revisionTask(branch), ws, branch, "main",
		func(string) string { return bare }, nil, logr.Discard())
	if err != nil {
		t.Fatalf("setupTaskBranch must fall back, not fail: %v", err)
	}
	if got := gitIn(t, ws, "rev-parse", "HEAD"); got != mainSHA {
		t.Errorf("HEAD = %s, want base tip %s (fallback branches from base)", got, mainSHA)
	}
	if got := gitIn(t, ws, "branch", "--show-current"); got != branch {
		t.Errorf("current branch = %q, want %q", got, branch)
	}
}

// TestSetupTaskBranch_ResetSkipsPriorAttempt pins the #1029 contract: under the
// default "reset" strategy a task carrying reviseFromBranch is NOT restored —
// the branch is cut fresh from the current base — so a retry or repair
// re-dispatch cannot carry a stale prior branch that reverts merged work.
func TestSetupTaskBranch_ResetSkipsPriorAttempt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	bare, mainSHA := seededRemote(t, dir)
	const branch = "foreman/wl/issue-641"

	// A prior attempt exists on the remote — it WOULD be restored under rebase.
	prior := filepath.Join(dir, "prior")
	gitIn(t, "", "clone", bare, prior)
	gitIn(t, prior, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(prior, "fix.txt"), []byte("attempt 1\n"), 0o644); err != nil {
		t.Fatalf("write fix.txt: %v", err)
	}
	gitIn(t, prior, "add", "fix.txt")
	gitIn(t, prior, "commit", "-m", "attempt 1")
	gitIn(t, prior, "push", "origin", branch)

	ws := filepath.Join(dir, "ws")
	gitIn(t, "", "clone", bare, ws)

	// reset strategy despite reviseFromBranch being set.
	task := revisionTask(branch)
	task.Spec.Payload.BranchStrategy = foremanv1alpha1.BranchStrategyReset

	err := setupTaskBranch(context.Background(), task, ws, branch, "main",
		func(string) string { return bare }, nil, logr.Discard())
	if err != nil {
		t.Fatalf("setupTaskBranch: %v", err)
	}
	if got := gitIn(t, ws, "rev-parse", "HEAD"); got != mainSHA {
		t.Errorf("HEAD = %s, want base tip %s (reset cuts fresh, ignores prior attempt)", got, mainSHA)
	}
	if _, err := os.Stat(filepath.Join(ws, "fix.txt")); err == nil {
		t.Error("prior attempt's fix.txt must NOT be present under reset strategy")
	}
	if got := gitIn(t, ws, "branch", "--show-current"); got != branch {
		t.Errorf("current branch = %q, want %q", got, branch)
	}
}

// TestSetupTaskBranch_ReviseFromBranchDefaultsToRebase pins #1047: when
// reviseFromBranch is set but branchStrategy is left empty (the common
// caller mistake that produced the silent no-op), the effective strategy
// must be rebase so the prior attempt is restored instead of silently
// discarded. An explicit reset still wins.
func TestSetupTaskBranch_ReviseFromBranchDefaultsToRebase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	bare, _ := seededRemote(t, dir)
	const branch = "foreman/wl/issue-641"

	// Prior attempt pushed to the remote.
	prior := filepath.Join(dir, "prior")
	gitIn(t, "", "clone", bare, prior)
	gitIn(t, prior, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(prior, "fix.txt"), []byte("attempt 1\n"), 0o644); err != nil {
		t.Fatalf("write fix.txt: %v", err)
	}
	gitIn(t, prior, "add", "fix.txt")
	gitIn(t, prior, "commit", "-m", "attempt 1")
	gitIn(t, prior, "push", "origin", branch)
	priorSHA := gitIn(t, prior, "rev-parse", "HEAD")

	ws := filepath.Join(dir, "ws")
	gitIn(t, "", "clone", bare, ws)

	// reviseFromBranch set, branchStrategy left empty — the #1047 footgun.
	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:             "defilantech/LLMKube",
				Branch:           branch,
				ReviseFromBranch: branch,
				// BranchStrategy intentionally left empty — this is the bug we're
				// guarding against.
			},
		},
	}

	err := setupTaskBranch(context.Background(), task, ws, branch, "main",
		func(string) string { return bare }, nil, logr.Discard())
	if err != nil {
		t.Fatalf("setupTaskBranch: %v", err)
	}
	if got := gitIn(t, ws, "rev-parse", "HEAD"); got != priorSHA {
		t.Errorf("HEAD = %s, want prior attempt tip %s (empty branchStrategy + reviseFromBranch must default to rebase)", got, priorSHA)
	}
	if _, err := os.Stat(filepath.Join(ws, "fix.txt")); err != nil {
		t.Errorf("prior attempt's file must be present: %v", err)
	}
}

// TestRecoverSelfCommitsOrNoChange_StaleForkBaseIsNotCountedAsCommitsAhead
// reproduces the #982/#813 scenario where the fork clone's local "main"
// lags the upstream tip the task branch was actually cut from. If the
// helper recovered against a possibly-stale local literal ref instead
// of a freshly-resolved upstream SHA, it would over-count and the soft
// reset would re-stage the intervening upstream commits, polluting the
// recovered commit with upstream delta.
//
// Setup mirrors the production workflow: a bare upstream, a fork clone
// whose local "main" lags by one commit (created before the upstream
// advance), and a task branch cut FROM THE UPSTREAM TIP at the fresh
// FETCH_HEAD (mirroring CreateBranchFromUpstream — the #813 fix). The
// helper must see exactly 1 commit ahead (the model's self-commit),
// not 2 (self-commit + the lagging upstream delta). The exact regression
// a previous iteration of this code introduced.
func TestRecoverSelfCommitsOrNoChange_StaleForkBaseIsNotCountedAsCommitsAhead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	bare, _ := seededRemote(t, dir) // upstream tip at SHA-A

	// Fork clone taken BEFORE upstream advances. Local "main" stays at SHA-A.
	fork := filepath.Join(dir, "fork")
	gitIn(t, "", "clone", bare, fork)

	// Upstream advances by one commit AFTER the fork was cloned.
	advancer := filepath.Join(dir, "adv")
	gitIn(t, "", "clone", bare, advancer)
	if err := os.WriteFile(filepath.Join(advancer, "UPSTREAM.md"), []byte("intervening upstream delta\n"), 0o644); err != nil {
		t.Fatalf("write UPSTREAM.md: %v", err)
	}
	gitIn(t, advancer, "add", "UPSTREAM.md")
	gitIn(t, advancer, "commit", "-m", "intervening upstream commit")
	gitIn(t, advancer, "push", "origin", "main")
	upstreamTip := gitIn(t, advancer, "rev-parse", "HEAD")
	forkLag := gitIn(t, fork, "rev-parse", "main")
	if forkLag == upstreamTip {
		t.Fatalf("test setup failed: fork's local main (%s) should lag upstream tip (%s)", forkLag, upstreamTip)
	}

	// Mirrors CreateBranchFromUpstream: fetch upstream tip into FETCH_HEAD
	// and cut the task branch there so the branch point is upstreamTip,
	// not the lagging forkLag.
	gitIn(t, fork, "fetch", bare, "main")
	gitIn(t, fork, "checkout", "-b", "fix/issue-982", "FETCH_HEAD")
	if got := gitIn(t, fork, "rev-parse", "HEAD"); got != upstreamTip {
		t.Fatalf("branch not at upstream tip: branch=%s upstream=%s", got, upstreamTip)
	}

	// Model self-commits. ONE commit ahead of upstream tip, not two.
	fixDir := filepath.Join(fork, "pkg", "agent")
	if err := os.MkdirAll(fixDir, 0o755); err != nil {
		t.Fatalf("mkdir fixDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixDir, "fix.go"), []byte("// fix\npackage agent\n\nfunc Fix() {}\n"), 0o644); err != nil {
		t.Fatalf("write fix.go: %v", err)
	}
	gitIn(t, fork, "add", "-A")
	gitIn(t, fork, "commit", "-m", "fix: self-committed model work")

	// Sanity: against local "main" (the OLD base), commits ahead = 2
	// (self-commit + UPSTREAM.md delta). Against the resolved upstream
	// tip, it must be exactly 1.
	if got := gitIn(t, fork, "rev-list", "--count", "main..HEAD"); got != "2" {
		t.Fatalf("pre-condition wrong: against local main, expected 2 commits ahead (the regression case), got %s", got)
	}

	// The recovery must use the resolved upstream tip, not local "main".
	e := &NativeAgentLoopExecutor{}
	if e.recoverSelfCommitsOrNoChange(context.Background(), logr.Discard(), false, fork, bare, "main") {
		t.Fatal("recoverSelfCommitsOrNoChange returned true (no-change); self-commit should have been recovered")
	}

	// Post-recovery invariants:
	//  - HEAD is now at the upstream tip (soft reset succeeded).
	//  - Working tree contains the SELF-COMMIT's diff.
	//  - Diff does NOT contain UPSTREAM.md (the source of the prior bug).
	if got := gitIn(t, fork, "rev-parse", "HEAD"); got != upstreamTip {
		t.Errorf("HEAD = %s, want upstream tip %s (soft reset must move to upstream base, not fork main)", got, upstreamTip)
	}
	staged := gitIn(t, fork, "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "pkg/agent/fix.go") {
		t.Errorf("staged changes should include model fix, got: %q", staged)
	}
	if strings.Contains(staged, "UPSTREAM.md") {
		t.Errorf("BUG (regression): soft reset re-staged UPSTREAM.md — base resolution fell back to local main. staged: %q", staged)
	}
}

// TestRecoverSelfCommitsOrNoChange_GenuinelyNothingToRecover covers the
// NO-CHANGES branch: the model said GO but never edited anything AND
// did not self-commit. The helper must return true (no-change) without
// making any mutations and without logging errors — a true outcome,
// not a silent degrade.
func TestRecoverSelfCommitsOrNoChange_GenuinelyNothingToRecover(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	bare, mainSHA := seededRemote(t, dir)

	ws := filepath.Join(dir, "ws")
	gitIn(t, "", "clone", bare, ws)
	gitIn(t, ws, "checkout", "-b", "feat/empty")

	e := &NativeAgentLoopExecutor{}
	log := logr.Discard()
	if !e.recoverSelfCommitsOrNoChange(context.Background(), log, false, ws, bare, "main") {
		t.Fatal("expected no-change recovery to return true for a clean branch with no commits ahead")
	}
	// HEAD must still be at the upstream tip — no mutation occurred.
	if got := gitIn(t, ws, "rev-parse", "HEAD"); got != mainSHA {
		t.Errorf("HEAD = %s, want %s (no-change path must not move HEAD)", got, mainSHA)
	}
}

// ---- makeCoderGateVerifier: generic-gate + claim-evidence merge (#1075) ----

// TestMakeCoderGateVerifier_GenericGate_ClaimEvidenceMergedIntoFeedback
// covers finding 4: the non-Go (generic-gate) merge path in
// makeCoderGateVerifier had zero tests. This exercises it end to end with a
// fake execCommandRunner (the generic-gate path is not injectable via a
// commandRunner parameter, so the package-level var is swapped for the
// duration of the test, mirroring the existing execCommandRunner-swap seam
// in coder_grounding_gate_test.go): a Python GateProfile whose resolved
// format/lint/build/test commands all pass (RunGenericGate alone would
// report a clean GO), but the terminal's docs diff adds an unsourced
// benchmark claim with no declared evidence. The merged result must reject
// the GO and carry the claim-evidence marker alongside the synthesized
// gate-failed header.
func TestMakeCoderGateVerifier_GenericGate_ClaimEvidenceMergedIntoFeedback(t *testing.T) {
	orig := execCommandRunner
	t.Cleanup(func() { execCommandRunner = orig })
	execCommandRunner = func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "git" && len(args) > 0 {
			switch args[0] {
			case "merge-base":
				return fakeForkPointSHA, nil
			case "diff":
				return unsourcedBenchmarkPatch, nil
			}
		}
		// Every `sh -c <format/lint/build/test command>` call from
		// RunGenericGate, plus `git add`/`git show`, succeeds silently.
		return "", nil
	}

	profile := &foremanv1alpha1.GateProfile{Language: foremanv1alpha1.GateLanguagePython}
	verifier := makeCoderGateVerifier("/ws", "", fakeEvidenceBaseSHA, logr.Discard(), profile, nil)

	terminal := &ToolResult{Verdict: "GO", Extra: map[string]any{}}
	accept, feedback, err := verifier(context.Background(), terminal, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if accept {
		t.Fatalf("expected the merged generic gate to reject a GO with an unsourced claim; feedback:\n%s", feedback)
	}
	if strings.Count(feedback, "The verification gate failed") != 1 {
		t.Errorf("want the gate-failed header exactly once (synthesized since RunGenericGate itself "+
			"passed clean); got:\n%s", feedback)
	}
	if !strings.Contains(feedback, "## claim-evidence") {
		t.Errorf("feedback should carry the claim-evidence section header; got:\n%s", feedback)
	}
	if !strings.Contains(feedback, "[claim-evidence]") {
		t.Errorf("feedback should carry the claim-evidence marker; got:\n%s", feedback)
	}
}

// TestMakeCoderGateVerifier_GenericGate_CleanPassesWithNoClaims is the
// counterpart: the same Python profile and passing commands, but no docs
// diff at all, so the merged claim-evidence check has nothing to flag and
// the GO stands.
func TestMakeCoderGateVerifier_GenericGate_CleanPassesWithNoClaims(t *testing.T) {
	orig := execCommandRunner
	t.Cleanup(func() { execCommandRunner = orig })
	execCommandRunner = func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "git" && len(args) > 0 && args[0] == "merge-base" {
			return fakeForkPointSHA, nil
		}
		return "", nil // sh -c commands pass; empty git diff, no claims
	}

	profile := &foremanv1alpha1.GateProfile{Language: foremanv1alpha1.GateLanguagePython}
	verifier := makeCoderGateVerifier("/ws", "", fakeEvidenceBaseSHA, logr.Discard(), profile, nil)

	terminal := &ToolResult{Verdict: "GO", Extra: map[string]any{}}
	accept, feedback, err := verifier(context.Background(), terminal, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !accept {
		t.Fatalf("expected a clean GO with no claims to stand; feedback:\n%s", feedback)
	}
	if feedback != "" {
		t.Errorf("expected empty feedback on accept, got: %q", feedback)
	}
}
