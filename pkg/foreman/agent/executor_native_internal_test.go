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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
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

// resolveSchemeForTests builds a runtime scheme with the API types the
// resolveInferenceBaseURL tests touch. corev1 covers Endpoints,
// inferencev1alpha1 covers InferenceService, foreman covers the
// Agent CR field types referenced incidentally.
func resolveSchemeForTests(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1: %v", err)
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
// three resolution modes (full override, host override + Endpoints,
// status.endpoint default) and the error shapes the caller sees when
// any prerequisite is missing. Regression for #540: the static
// override locked the port at install time, so every metal-agent
// respawn broke every subsequent task; the host-override path re-reads
// the live port from Endpoints on each call.
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
	//nolint:staticcheck // SA1019: matches production code path.
	mkEndpoints := func(name string, port int32, withAddress bool) *corev1.Endpoints {
		eps := &corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Subsets:    []corev1.EndpointSubset{{Ports: []corev1.EndpointPort{{Port: port}}}},
		}
		if withAddress {
			eps.Subsets[0].Addresses = []corev1.EndpointAddress{{IP: "10.42.0.5"}}
		}
		return eps
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
			name: "host override: dotted InferenceService name maps to hyphenated Endpoints",
			executor: NativeAgentLoopExecutor{
				InferenceBaseURLHostOverride: "127.0.0.1",
			},
			seedObjects: []any{
				// Agent references the dotted name; the operator
				// sanitizes dots to hyphens when naming the Endpoints
				// object the metal-agent registers.
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
				// no Endpoints object
			},
			wantErrFrag: "get Endpoints",
		},
		{
			name: "host override: Endpoints exists but has no ready address",
			executor: NativeAgentLoopExecutor{
				InferenceBaseURLHostOverride: "127.0.0.1",
			},
			seedObjects: []any{
				mkISvc("test-svc", "http://test-svc.default.svc.cluster.local:80/v1/chat/completions"),
				mkEndpoints("test-svc", 60177, false), // port present, no addresses
			},
			wantErrFrag: "no ready address with a port",
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
				//nolint:staticcheck // SA1019: mirrors the production
				// resolveInferenceBaseURL path; producer + consumer
				// migrate from v1 Endpoints to discoveryv1 EndpointSlice
				// together.
				case *corev1.Endpoints:
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
