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
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
	"github.com/defilantech/llmkube/pkg/foreman/agent/worktracker"
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

// TestMapLoopError_MaxTurns_SubmissionCount pins the #1713 fix: a max-turns
// exhaustion reports the actual terminal state. When the model submitted
// and the gate rejected it (SubmissionsRejected > 0), the summary says so
// and says how many — it must NOT claim the model never called
// submit_result. When no submission was rejected, the original "never
// concluded" wording is kept.
func TestMapLoopError_MaxTurns_SubmissionCount(t *testing.T) {
	e := &NativeAgentLoopExecutor{}
	tref := corev1.ObjectReference{Name: "transcript"}

	// No rejection: the model never concluded.
	r, err := e.mapLoopError(time.Time{}, tref, &LoopResult{Turns: 5}, ErrMaxTurnsExhausted)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r.Summary != "model did not call submit_result within max_turns" {
		t.Errorf("summary: want the never-concluded wording, got %q", r.Summary)
	}

	// Rejections: the model concluded repeatedly and was refused.
	r, err = e.mapLoopError(time.Time{}, tref,
		&LoopResult{Turns: 5, SubmissionsRejected: 3}, ErrMaxTurnsExhausted)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(r.Summary, "3 time(s)") {
		t.Errorf("summary: want it to report 3 rejected submissions, got %q", r.Summary)
	}
	if !strings.Contains(r.Summary, "verification gate") {
		t.Errorf("summary: want it to name the gate, got %q", r.Summary)
	}
	if strings.Contains(r.Summary, "did not call submit_result") {
		t.Errorf("summary: must not claim the model never submitted, got %q", r.Summary)
	}
}

// TestMapLoopError_MaxTurns_SpinGuard pins the #1628 spin
// classification: a max-turns run whose per-turn average is strictly below
// spinGuardMaxAvgTurn ran faster than a model can generate, so the
// failure reason is LoopSpinning and the summary reports the rate. Real
// exhaustion (1662s of work), runs below the min-turns floor, and runs at
// exactly the threshold (strict less-than fails) all stay on the
// ordinary MaxTurnsExhausted reason.
func TestMapLoopError_MaxTurns_SpinGuard(t *testing.T) {
	e := &NativeAgentLoopExecutor{}
	tref := corev1.ObjectReference{Name: "transcript"}

	cases := []struct {
		name       string
		lr         *LoopResult
		wantReason foremanv1alpha1.AgenticTaskFailureReason
		wantFrags  []string
	}{
		{
			// #1628's observed spin: 160 turns in 65s (0.41s/turn),
			// far faster than any model can produce a turn.
			name:       "spin",
			lr:         &LoopResult{Turns: 160, TurnDuration: 65 * time.Second, TurnsWithCompletion: 160, TurnsWithToolCall: 150},
			wantReason: foremanv1alpha1.FailureLoopSpinning,
			wantFrags:  []string{"0.41s/turn", "spinning backend or shim"},
		},
		{
			// A spin that also had gate-rejected submissions keeps the
			// #1713 rejection detail in the summary.
			name:       "spin with rejected submissions",
			lr:         &LoopResult{Turns: 160, TurnDuration: 65 * time.Second, TurnsWithCompletion: 160, TurnsWithToolCall: 150, SubmissionsRejected: 2},
			wantReason: foremanv1alpha1.FailureLoopSpinning,
			wantFrags:  []string{"0.41s/turn", "2 time(s)", "verification gate"},
		},
		{
			// 1662s over 160 turns is slow real work, not a spin.
			name:       "real exhaustion",
			lr:         &LoopResult{Turns: 160, TurnDuration: 1662 * time.Second, TurnsWithCompletion: 160, TurnsWithToolCall: 150},
			wantReason: foremanv1alpha1.FailureMaxTurnsExhausted,
		},
		{
			// Below the min-turns floor: a fast tiny run keeps the
			// ordinary reason even at ~0.2ms/turn.
			name:       "below min-turns floor",
			lr:         &LoopResult{Turns: 5, TurnDuration: time.Millisecond},
			wantReason: foremanv1alpha1.FailureMaxTurnsExhausted,
		},
		{
			// Average exactly at the threshold: the strict less-than
			// fails, so 10 turns in 10s is not a spin.
			name:       "boundary exactly one second per turn",
			lr:         &LoopResult{Turns: 10, TurnDuration: 10 * time.Second},
			wantReason: foremanv1alpha1.FailureMaxTurnsExhausted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := e.mapLoopError(time.Time{}, tref, tc.lr, ErrMaxTurnsExhausted)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if r.FailureReason != tc.wantReason {
				t.Errorf("FailureReason: want %q got %q", tc.wantReason, r.FailureReason)
			}
			for _, frag := range tc.wantFrags {
				if !strings.Contains(r.Summary, frag) {
					t.Errorf("summary: want it to contain %q, got %q", frag, r.Summary)
				}
			}
			if got := r.Extra["turnsWithCompletion"]; got != tc.lr.TurnsWithCompletion {
				t.Errorf("Extra[turnsWithCompletion]: want %d got %v", tc.lr.TurnsWithCompletion, got)
			}
			if got := r.Extra["turnsWithToolCall"]; got != tc.lr.TurnsWithToolCall {
				t.Errorf("Extra[turnsWithToolCall]: want %d got %v", tc.lr.TurnsWithToolCall, got)
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

// TestResolveAnthropicEndpoint covers the v0.3 anthropic resolution
// path (#1627): providerConfig must carry baseURL + model, and the
// optional APIKeySecretRef value lands RAW in the endpoint (the
// Messages API sends it as x-api-key, never Authorization: Bearer) —
// the inverse of the cloud-proxy contract, which is the whole point of
// having a separate resolver.
func TestResolveAnthropicEndpoint(t *testing.T) {
	mkAgent := func(name string, cfg *foremanv1alpha1.ProviderConfig) *foremanv1alpha1.Agent {
		return &foremanv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: foremanv1alpha1.AgentSpec{
				Role:           foremanv1alpha1.AgentRoleReviewer,
				Provider:       foremanv1alpha1.AgentProviderAnthropic,
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
		wantKey     string
		wantErrFrag string
	}{
		{
			name: "anthropic without auth: baseURL + model resolve, both auth fields empty",
			agent: mkAgent("anth-no-auth", &foremanv1alpha1.ProviderConfig{
				BaseURL: "https://api.anthropic.com/v1/",
				Model:   "claude-sonnet-4-6",
			}),
			wantBase:  "https://api.anthropic.com/v1",
			wantModel: "claude-sonnet-4-6",
		},
		{
			name: "anthropic with Secret: apiKey is the raw value, authHeader stays empty",
			agent: mkAgent("anth-auth", &foremanv1alpha1.ProviderConfig{
				BaseURL: "https://api.anthropic.com/v1",
				Model:   "claude-sonnet-4-6",
				APIKeySecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "anthropic-api-key"},
					Key:                  "key",
				},
			}),
			seedObjects: []runtime.Object{mkSecret("anthropic-api-key", "key", "sk-ant-test\n")},
			wantBase:    "https://api.anthropic.com/v1",
			wantModel:   "claude-sonnet-4-6",
			wantAuth:    "",            // no Bearer header on the Messages wire
			wantKey:     "sk-ant-test", // raw, TrimSpace'd
		},
		{
			name:        "anthropic missing providerConfig",
			agent:       mkAgent("anth-no-cfg", nil),
			wantErrFrag: "providerConfig is required for provider=anthropic",
		},
		{
			name: "anthropic missing baseURL",
			agent: mkAgent("anth-no-base", &foremanv1alpha1.ProviderConfig{
				Model: "claude-sonnet-4-6",
			}),
			wantErrFrag: "baseURL is required for provider=anthropic",
		},
		{
			name: "anthropic missing model",
			agent: mkAgent("anth-no-model", &foremanv1alpha1.ProviderConfig{
				BaseURL: "https://api.anthropic.com/v1",
			}),
			wantErrFrag: "model is required for provider=anthropic",
		},
		{
			name: "anthropic: APIKeySecretRef points at nonexistent Secret",
			agent: mkAgent("anth-missing-secret", &foremanv1alpha1.ProviderConfig{
				BaseURL: "https://api.anthropic.com/v1",
				Model:   "claude-sonnet-4-6",
				APIKeySecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "nope"},
					Key:                  "key",
				},
			}),
			wantErrFrag: "get Secret",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(resolveSchemeForTests(t))
			b = b.WithObjects(tc.agent)
			for _, obj := range tc.seedObjects {
				if co, ok := obj.(client.Object); ok {
					b = b.WithObjects(co)
				}
			}
			e := &NativeAgentLoopExecutor{Client: b.Build()}

			ep, err := e.resolveAnthropicEndpoint(context.Background(), "default", tc.agent)
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
				t.Fatalf("resolveAnthropicEndpoint: %v", err)
			}
			if ep.baseURL != tc.wantBase {
				t.Errorf("baseURL: want %q got %q", tc.wantBase, ep.baseURL)
			}
			if ep.modelName != tc.wantModel {
				t.Errorf("modelName: want %q got %q", tc.wantModel, ep.modelName)
			}
			if ep.authHeader != tc.wantAuth {
				t.Errorf("authHeader: want %q got %q (anthropic never sends Bearer)", tc.wantAuth, ep.authHeader)
			}
			if ep.apiKey != tc.wantKey {
				t.Errorf("apiKey: want %q got %q", tc.wantKey, ep.apiKey)
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
	if extra["verdictDemotedBy"] != railIssueAsk {
		t.Errorf("demotion must name this rail in verdictDemotedBy=%q; got %v",
			railIssueAsk, extra["verdictDemotedBy"])
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
	// verdictDemotedBy travels with the flag on every path that sets it, or
	// a consumer finding verdictDemoted cannot say which rail set it. On THIS
	// path the rail marked the verdict without rewriting it; verdictClaimed
	// is what records that, and the controller's inertDemotion keys off it so
	// this genuine rejection still opens a fix iteration (#1636).
	if extra["verdictDemotedBy"] != railIssueAsk {
		t.Errorf("marker must name this rail in verdictDemotedBy=%q; got %v",
			railIssueAsk, extra["verdictDemotedBy"])
	}
	if extra["verdictClaimed"] != string(foremanv1alpha1.AgenticTaskVerdictNoGo) {
		t.Errorf("verdictClaimed should archive the original NO-GO; got %v", extra["verdictClaimed"])
	}
}

// TestIssueAskDemotionLandsUnderModelExtra is the cross-package shape
// contract for #1636/#1641. It drives the REAL rail and the REAL envelope
// builder (enforceReviewerIssueAsk stamps LoopResult.Terminal.Extra, and
// modelDecidedResult wraps it into the Result the watcher stores in
// AgenticTask.status.result), then asserts the JSON path the
// controller's inertDemotion (internal/foreman/controller/
// workload_iteration.go, not imported here) reads.
//
// The bug this pins: modelDecidedResult nests the rails' WHOLE map one
// level down under "modelExtra", and promoteTerminalOutcome lifts only
// outcome / unverified / resolvedBy to the top level. The suppression
// predicate shipped reading extra.verdictDemoted, a key nothing ever
// writes, so it returned false on every production result while its own
// hand-written fixture (extra.verdictDemoted as a sibling of modelExtra)
// made it look correct. Any future change to the nesting fails here.
func TestIssueAskDemotionLandsUnderModelExtra(t *testing.T) {
	// What the reviewer's submit_result carried, before the rails ran: an
	// approval the harness could not tie back to the fetched issue body.
	terminalExtra := map[string]any{"issueAskVerified": false}
	verdict := enforceReviewerIssueAsk(logr.Discard(), terminalExtra,
		foremanv1alpha1.AgenticTaskVerdictGo, true, nil)
	if verdict != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("precondition: rail must demote GO to NO-GO; got %v", verdict)
	}

	lr := &LoopResult{
		Terminal: &ToolResult{
			Terminal: true,
			Verdict:  string(foremanv1alpha1.AgenticTaskVerdictGo),
			Summary:  "APPROVE: the change is minimal and well covered",
			Extra:    terminalExtra,
		},
		Turns: 7,
	}
	e := &NativeAgentLoopExecutor{}
	res := e.modelDecidedResult(context.Background(), time.Now(), corev1.ObjectReference{Name: "t"}, lr, verdict, "", "")
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}

	// Mirrors inertDemotion's envelope, plus the top-level keys it must NOT
	// find, so a regression that flattens the nesting is caught here too.
	var envelope struct {
		Extra struct {
			VerdictDemoted   bool           `json:"verdictDemoted"`
			VerdictDemotedBy string         `json:"verdictDemotedBy"`
			ModelExtra       map[string]any `json:"modelExtra"`
		} `json:"extra"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if envelope.Extra.ModelExtra["verdictDemotedBy"] != railIssueAsk {
		t.Errorf("extra.modelExtra.verdictDemotedBy = %v, want %q (the path the controller reads)",
			envelope.Extra.ModelExtra["verdictDemotedBy"], railIssueAsk)
	}
	if envelope.Extra.ModelExtra["verdictClaimed"] != string(foremanv1alpha1.AgenticTaskVerdictGo) {
		t.Errorf("extra.modelExtra.verdictClaimed = %v, want GO",
			envelope.Extra.ModelExtra["verdictClaimed"])
	}
	if v, _ := envelope.Extra.ModelExtra["verdictDemoted"].(bool); !v {
		t.Errorf("extra.modelExtra.verdictDemoted = %v, want true",
			envelope.Extra.ModelExtra["verdictDemoted"])
	}
	if reason, _ := envelope.Extra.ModelExtra["demotionReason"].(string); reason == "" {
		t.Error("extra.modelExtra.demotionReason is empty; the controller's suppression condition would name no cause")
	}
	if envelope.Extra.VerdictDemoted || envelope.Extra.VerdictDemotedBy != "" {
		t.Errorf("the demotion markers must NOT appear at the TOP level of extra "+
			"(promoteTerminalOutcome lifts only outcome/unverified/resolvedBy); "+
			"got verdictDemoted=%v verdictDemotedBy=%q",
			envelope.Extra.VerdictDemoted, envelope.Extra.VerdictDemotedBy)
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

// TestEnforceReviewerUnverifiedSummary covers #1454: a reviewer GO whose
// terminal summary states in plain language that verification could not be
// performed must demote to NO-GO, and a normal GO must stand untouched. The
// first row is verbatim from the windowstead#321 incident that motivated
// the rail; the variant rows pin the full phrase set the enforcer's
// docstring promises, including both apostrophe forms of the contractions.
func TestEnforceReviewerUnverifiedSummary(t *testing.T) {
	cases := []struct {
		name    string
		summary string
		verdict foremanv1alpha1.AgenticTaskVerdict
		want    foremanv1alpha1.AgenticTaskVerdict
	}{
		{
			name:    "windowstead reproduction: cannot verify",
			summary: "Tests fail due to missing godot runtime in environment; cannot verify goal reward or progression logic.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictNoGo,
		},
		{
			name:    "could not verify",
			summary: "The change looks correct but I could not verify it without the service running.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictNoGo,
		},
		{
			name:    "unable to verify",
			summary: "Reviewed statically; unable to verify runtime behavior in this environment.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictNoGo,
		},
		{
			name:    "contraction can't, straight apostrophe",
			summary: "Reviewed statically; can't verify the migration path without a database.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictNoGo,
		},
		{
			name:    "contraction can't, curly apostrophe",
			summary: "Reviewed statically; can’t verify the migration path without a database.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictNoGo,
		},
		{
			name:    "contraction couldn't, straight apostrophe",
			summary: "The harness is missing, so we couldn't verify the fix in this environment.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictNoGo,
		},
		{
			name:    "contraction couldn't, curly apostrophe",
			summary: "The harness is missing, so we couldn’t verify the fix in this environment.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictNoGo,
		},
		{
			name:    "spaced can not",
			summary: "No runtime is available here, so I can not verify the behavior.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictNoGo,
		},
		{
			name:    "was not able to",
			summary: "I was not able to verify the fix before approving; the harness was unavailable.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictNoGo,
		},
		{
			name:    "case-insensitive",
			summary: "APPROVE. Could NOT VERIFY the migration path; no database available.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictNoGo,
		},
		{
			name:    "whitespace-tolerant across wrapped lines",
			summary: "The harness is missing, so we cannot\nverify the fix here.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictNoGo,
		},
		{
			name:    "normal GO stays GO",
			summary: "APPROVE: ran go test ./... and all specs pass; the change is minimal and well covered.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictGo,
		},
		{
			name:    "affirmative verification language is not a match",
			summary: "The new guard verifies the token before use; tests confirm it rejects expired tokens.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictGo,
		},
		{
			// The tradeoff the enforcer's docstring documents: phrase
			// anchoring matches whatever subject the phrase carries, so an
			// honest GO describing what a third party cannot do is
			// demoted. The false positive is accepted, because the
			// subjectless incident summaries this rail exists for (#1454)
			// are the common case.
			name:    "object-subject honest GO is the accepted false positive",
			summary: "The new signature check is correct: a client without the key cannot verify a forged signature, and the tests prove it.",
			verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			want:    foremanv1alpha1.AgenticTaskVerdictNoGo,
		},
		{
			name:    "non-GO with the phrase passes through unmarked",
			summary: "REJECT: cannot verify anything in this broken environment and the diff is wrong.",
			verdict: foremanv1alpha1.AgenticTaskVerdictNoGo,
			want:    foremanv1alpha1.AgenticTaskVerdictNoGo,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra := map[string]any{}
			got := enforceReviewerUnverifiedSummary(logr.Discard(), extra, tc.summary, tc.verdict)
			if got != tc.want {
				t.Fatalf("verdict = %v, want %v", got, tc.want)
			}
			demoted := tc.verdict == foremanv1alpha1.AgenticTaskVerdictGo &&
				tc.want == foremanv1alpha1.AgenticTaskVerdictNoGo
			v, marked := extra["verdictDemoted"].(bool)
			if !demoted {
				if marked {
					t.Errorf("non-demoting path must not annotate extra; got verdictDemoted=%v", v)
				}
				return
			}
			if !v {
				t.Errorf("demotion must set verdictDemoted=true; got %v", v)
			}
			if extra["verdictDemotedBy"] != railUnverifiedSummary {
				t.Errorf("verdictDemotedBy = %v, want %q", extra["verdictDemotedBy"], railUnverifiedSummary)
			}
			if extra["verdictClaimed"] != string(foremanv1alpha1.AgenticTaskVerdictGo) {
				t.Errorf("verdictClaimed should archive the original GO; got %v", extra["verdictClaimed"])
			}
			if reason, _ := extra["demotionReason"].(string); reason == "" {
				t.Error("demotionReason must explain the demotion")
			}
			if phrase, _ := extra["unverifiedSummaryPhrase"].(string); phrase == "" {
				t.Error("unverifiedSummaryPhrase must record the matched admission")
			}
		})
	}
}

func TestEnforceReviewerUnverifiedSummary_NilExtraIsNoOp(t *testing.T) {
	// Without an extra map the demotion cannot be recorded or grounded in
	// the audit trail, so the rails pass the verdict through with only a
	// log line — the enforceReviewerIssueAsk convention.
	got := enforceReviewerUnverifiedSummary(logr.Discard(), nil,
		"cannot verify anything", foremanv1alpha1.AgenticTaskVerdictGo)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("nil extra must pass the verdict through; got %v", got)
	}
}

func TestEnforceReviewerUnverifiedSummary_PreservesFirstWriterVerdictClaimed(t *testing.T) {
	// The issueAsk scope-vouch path marks a still-GO verdict with
	// verdictClaimed; this rail must not overwrite it (#1678 first-writer-
	// wins), while its own rail name still records who made the rewrite.
	extra := map[string]any{
		"verdictClaimed": string(foremanv1alpha1.AgenticTaskVerdictGo),
	}
	got := enforceReviewerUnverifiedSummary(logr.Discard(), extra,
		"Tests could not verify the fix; no runtime available.",
		foremanv1alpha1.AgenticTaskVerdictGo)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("matching GO must demote to NO-GO; got %v", got)
	}
	if extra["verdictClaimed"] != string(foremanv1alpha1.AgenticTaskVerdictGo) {
		t.Errorf("verdictClaimed must keep the first writer's archive; got %v", extra["verdictClaimed"])
	}
	if extra["verdictDemotedBy"] != railUnverifiedSummary {
		t.Errorf("verdictDemotedBy = %v, want %q", extra["verdictDemotedBy"], railUnverifiedSummary)
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

// TestBuildUserPrompt_IssueFix_ContainsDateLine verifies that the issue-fix
// prompt includes the current date so the model can anchor time-sensitive
// research queries (#1202).
func TestBuildUserPrompt_IssueFix_ContainsDateLine(t *testing.T) {
	fixed := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	orig := nowFunc
	nowFunc = func() time.Time { return fixed }
	defer func() { nowFunc = orig }()

	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:  "defilantech/LLMKube",
				Issue: 1202,
			},
		},
	}
	got := buildUserPrompt(task)
	if !strings.Contains(got, "Today's date is 2026-07-22") {
		t.Fatalf("date line missing from prompt:\n%s", got)
	}
	if !strings.Contains(got, "authoritative over your internal sense of time") {
		t.Fatalf("date authority clause missing from prompt:\n%s", got)
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

// TestBuildUserPrompt_ReviewerZeroIssueOmitsIssue verifies that a review task
// whose payload carries no issue key (int32 zero value) does not anchor the
// reviewer on a non-existent "issue #0": the branch under review is the anchor
// instead (#1761), matching the #1530 guards on the two sibling call sites.
func TestBuildUserPrompt_ReviewerZeroIssueOmitsIssue(t *testing.T) {
	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindReview,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Branch: "foreman/wl/no-issue-review",
			},
		},
	}
	got := buildUserPrompt(task)
	if strings.Contains(got, "issue #0") {
		t.Errorf("reviewer prompt with no issue in the payload must not reference issue #0; got:\n%s", got)
	}
	if strings.Contains(got, "- issue:") {
		t.Errorf("zero-issue reviewer prompt must omit the issue line entirely; got:\n%s", got)
	}
	if !strings.Contains(got, "reviewing the branch foreman/wl/no-issue-review of defilantech/LLMKube") {
		t.Errorf("zero-issue reviewer prompt must name the branch under review; got:\n%s", got)
	}
}

// TestBuildUserPrompt_ReviewerPositiveIssueKeepsReferences verifies the guard
// leaves the issue-anchored form untouched when the payload carries an issue.
func TestBuildUserPrompt_ReviewerPositiveIssueKeepsReferences(t *testing.T) {
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
	for _, want := range []string{
		"reviewing the branch the coder produced for issue #510 of defilantech/LLMKube",
		"- issue: 510",
		"- branch: foreman/wl/issue-510",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reviewer prompt with an issue missing %q in:\n%s", want, got)
		}
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

	fetchIssueBodyIfNeeded(context.Background(), &worktracker.GitHubWorkItems{Fetcher: fetcher}, task, logr.Discard())

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

	fetchIssueBodyIfNeeded(context.Background(), &worktracker.GitHubWorkItems{Fetcher: fetcher}, task, logr.Discard())

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

	fetchIssueBodyIfNeeded(context.Background(), &worktracker.GitHubWorkItems{Fetcher: fetcher}, task, logr.Discard())

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

// TestSetupTaskBranch_MissingRefFailsLoud pins the #1364 contract: when the
// reviseFromBranch ref is gone from the remote the task FAILS instead of
// rebuilding from base. The old fallback, combined with allowOverwrite (which
// revisions and pr-fixes always carry), force-pushed a fresh base-cut over the
// prior attempt — silently destroying reviewed work. A named prior attempt
// that cannot be restored is an error to surface (pruned ref, or a race with
// a concurrent fix attempt), never a fallback.
func TestSetupTaskBranch_MissingRefFailsLoud(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	bare, _ := seededRemote(t, dir)
	const branch = "foreman/wl/issue-641"

	ws := filepath.Join(dir, "ws")
	gitIn(t, "", "clone", bare, ws)

	err := setupTaskBranch(context.Background(), revisionTask(branch), ws, branch, "main",
		func(string) string { return bare }, nil, logr.Discard())
	if err == nil {
		t.Fatal("missing reviseFromBranch ref must fail the task, not fall back to base")
	}
	if !strings.Contains(err.Error(), "reviseFromBranch") ||
		!strings.Contains(err.Error(), "refusing") {
		t.Errorf("error must explain the refusal, got: %v", err)
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

// TestMaxTokensPerTurnForAgent pins the per-turn generation-cap resolution:
// an Agent that leaves spec.maxOutputTokens unset (0) gets the package
// default; a set value flows through verbatim. This is the knob that bounds
// a reasoning model's decision-turn <think> so a truncated turn recovers via
// a continuation instead of hanging the task.
func TestMaxTokensPerTurnForAgent(t *testing.T) {
	cases := []struct {
		name string
		spec int32
		want int
	}{
		{name: "unset uses the package default", spec: 0, want: defaultMaxTokensPerTurn},
		{name: "set flows through as the CR value", spec: 2048, want: 2048},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := &foremanv1alpha1.Agent{
				Spec: foremanv1alpha1.AgentSpec{MaxOutputTokens: tc.spec},
			}
			if got := maxTokensPerTurnForAgent(agent); got != tc.want {
				t.Errorf("maxTokensPerTurnForAgent=%d want %d", got, tc.want)
			}
		})
	}
}

// reviewTask builds a review-shaped task: payload.Branch names the branch the
// coder already pushed, and BaseBranch is the base the change is measured
// against. No ReviseFromBranch, because a reviewer is not restoring a prior
// attempt; it is reading one that already exists on the remote.
func reviewTask(branch string) *foremanv1alpha1.AgenticTask {
	return &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindReview,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:       "defilantech/LLMKube",
				Issue:      641,
				Branch:     branch,
				BaseBranch: "main",
			},
		},
	}
}

// A reviewer must end up on the coder's commit, not on the base branch.
//
// setupTaskBranch's base-cut path runs `checkout -B <branch> FETCH_HEAD` after
// fetching the BASE. For an issue-fix that is exactly right: cut a fresh branch
// from base. For a review it is destructive, because -B force-moves the branch
// ref off the coder's pushed commit and onto base. The reviewer then holds a
// working tree identical to base, its diff against base is empty, and it
// reasons about code the coder never wrote.
//
// Observed live: a coder produced a correct two-line fix and reported GO, and
// the reviewer returned GO describing the base commit's contents ("adds
// particles, fireflies, weather") rather than the fix. Both stages green, the
// review meaningless.
func TestSetupTaskBranch_ReviewStartsAtCoderCommitNotBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	bare, mainSHA := seededRemote(t, dir)
	const branch = "foreman/wl/issue-641"

	// The coder's branch, already pushed.
	coder := filepath.Join(dir, "coder")
	gitIn(t, "", "clone", bare, coder)
	gitIn(t, coder, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(coder, "fix.txt"), []byte("the fix\n"), 0o644); err != nil {
		t.Fatalf("write fix.txt: %v", err)
	}
	gitIn(t, coder, "add", "fix.txt")
	gitIn(t, coder, "commit", "-m", "fix: the change under review")
	gitIn(t, coder, "push", "origin", branch)
	coderSHA := gitIn(t, coder, "rev-parse", "HEAD")

	// The reviewer's fresh workspace clone.
	ws := filepath.Join(dir, "review-ws")
	gitIn(t, "", "clone", bare, ws)

	if err := setupTaskBranch(context.Background(), reviewTask(branch), ws, branch, "main",
		func(string) string { return bare }, nil, logr.Discard()); err != nil {
		t.Fatalf("setupTaskBranch: %v", err)
	}

	if got := gitIn(t, ws, "rev-parse", "HEAD"); got != coderSHA {
		t.Errorf("HEAD = %s, want the coder's commit %s (base is %s).\n"+
			"A reviewer reset to base reviews an empty diff and approves work it never saw.",
			got, coderSHA, mainSHA)
	}
	if _, err := os.Stat(filepath.Join(ws, "fix.txt")); err != nil {
		t.Errorf("the change under review must be present in the working tree: %v", err)
	}
}

// The issue-fix base-cut must keep working: a coder branch is cut fresh from
// the current base so a retry cannot carry stale work. Guarding the review case
// must not weaken this.
func TestSetupTaskBranch_IssueFixStillCutsFromBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	bare, mainSHA := seededRemote(t, dir)
	const branch = "foreman/wl/issue-900"

	// A stale branch of the same name exists on the remote. An issue-fix must
	// ignore it and start from base.
	stale := filepath.Join(dir, "stale")
	gitIn(t, "", "clone", bare, stale)
	gitIn(t, stale, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(stale, "stale.txt"), []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write stale.txt: %v", err)
	}
	gitIn(t, stale, "add", "stale.txt")
	gitIn(t, stale, "commit", "-m", "stale work")
	gitIn(t, stale, "push", "origin", branch)

	ws := filepath.Join(dir, "coder-ws")
	gitIn(t, "", "clone", bare, ws)

	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo: "defilantech/LLMKube", Issue: 900, BaseBranch: "main",
			},
		},
	}
	if err := setupTaskBranch(context.Background(), task, ws, branch, "main",
		func(string) string { return bare }, nil, logr.Discard()); err != nil {
		t.Fatalf("setupTaskBranch: %v", err)
	}
	if got := gitIn(t, ws, "rev-parse", "HEAD"); got != mainSHA {
		t.Errorf("HEAD = %s, want base %s: an issue-fix must cut fresh from base", got, mainSHA)
	}
	if _, err := os.Stat(filepath.Join(ws, "stale.txt")); err == nil {
		t.Error("stale remote branch content must not be carried into a fresh issue-fix")
	}
}

// A review whose branch is missing from the push remote must fail, not quietly
// review the base.
//
// This is the fail-closed half of the fix. The dangerous outcome is not an
// error, it is a green GO on an empty diff: falling back to a base-cut produces
// a working tree that looks perfectly valid and a review that means nothing.
// The three ways to get here (the coder never pushed, the ref was pruned, or
// the push remote is not the one the coder wrote to, as in #1464) are all
// conditions an operator needs to see.
func TestSetupTaskBranch_ReviewMissingBranchFailsLoud(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	bare, _ := seededRemote(t, dir)
	const branch = "foreman/wl/issue-641"

	ws := filepath.Join(dir, "review-ws")
	gitIn(t, "", "clone", bare, ws)

	err := setupTaskBranch(context.Background(), reviewTask(branch), ws, branch, "main",
		func(string) string { return bare }, nil, logr.Discard())
	if err == nil {
		t.Fatal("a review with no branch on the remote must fail; falling back to base " +
			"yields a GO on an empty diff, which is worse than an error")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error must explain the refusal, got: %v", err)
	}
}

// A verifier must run its gate against the coder's commit when that branch
// exists, for the same reason a reviewer must read it.
//
// The Workload sets Payload.Branch to the coder's branch and makes the verify
// step depend on the coder, so this path has the identical shape to review. A
// verifier reset to base runs the gate on code the coder never wrote and
// reports a pass, which is worse than a review rubber-stamp: the gate is the
// stage that is supposed to be mechanical and trustworthy.
func TestSetupTaskBranch_VerifyStartsAtCoderCommitNotBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	bare, _ := seededRemote(t, dir)
	const branch = "foreman/wl/issue-641"

	coder := filepath.Join(dir, "coder")
	gitIn(t, "", "clone", bare, coder)
	gitIn(t, coder, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(coder, "fix.txt"), []byte("the fix\n"), 0o644); err != nil {
		t.Fatalf("write fix.txt: %v", err)
	}
	gitIn(t, coder, "add", "fix.txt")
	gitIn(t, coder, "commit", "-m", "fix: the change under verification")
	gitIn(t, coder, "push", "origin", branch)
	coderSHA := gitIn(t, coder, "rev-parse", "HEAD")

	ws := filepath.Join(dir, "verify-ws")
	gitIn(t, "", "clone", bare, ws)

	task := reviewTask(branch)
	task.Spec.Kind = foremanv1alpha1.AgenticTaskKindVerify

	if err := setupTaskBranch(context.Background(), task, ws, branch, "main",
		func(string) string { return bare }, nil, logr.Discard()); err != nil {
		t.Fatalf("setupTaskBranch: %v", err)
	}
	if got := gitIn(t, ws, "rev-parse", "HEAD"); got != coderSHA {
		t.Errorf("HEAD = %s, want the coder's commit %s: a gate run against base proves nothing",
			got, coderSHA)
	}
}

// #1625: an issue-fix task with an empty payload.repo used to fall through
// to the cloned fork's HEAD, silently basing on a fork tip that drifts from
// upstream. The task must now fail with a configuration error and create
// NO branch: the fix is the payload (set payload.repo), not a guess at a
// base, and the claim-evidence gate's anchor assumption (#813) must hold.
func TestSetupTaskBranch_IssueFixEmptyRepoRefusesStaleForkHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	seededRemote(t, dir) // upstream main at A
	const branch = "foreman/wl/issue-641"

	// A STALE fork clone taken before upstream advanced: its local "main"
	// is the pre-advance tip — exactly what the old fallback would base on.
	ws := filepath.Join(dir, "fork-ws")
	gitIn(t, "", "clone", filepath.Join(dir, "origin.git"), ws)
	forkSHA := gitIn(t, ws, "rev-parse", "HEAD")

	// Advance upstream AFTER the fork was cloned, so the fork is stale.
	adv := filepath.Join(dir, "adv")
	gitIn(t, "", "clone", filepath.Join(dir, "origin.git"), adv)
	if err := os.WriteFile(filepath.Join(adv, "UPSTREAM.md"), []byte("intervening upstream delta\n"), 0o644); err != nil {
		t.Fatalf("write UPSTREAM.md: %v", err)
	}
	gitIn(t, adv, "add", "UPSTREAM.md")
	gitIn(t, adv, "commit", "-m", "intervening upstream commit")
	gitIn(t, adv, "push", "origin", "main")
	upstreamSHA := gitIn(t, adv, "rev-parse", "HEAD")
	if forkSHA == upstreamSHA {
		t.Fatal("test setup wrong: the fork tip must predate the upstream advance")
	}

	// Empty Repo: the #1625 hazard. The default resolver yields "" for it.
	task := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "code-641"},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind:    foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{Issue: 641},
		},
	}
	err := setupTaskBranch(context.Background(), task, ws, branch, "main",
		upstreamURLForRepo, nil, logr.Discard())
	if err == nil {
		t.Fatal("an issue-fix with an empty payload.repo must fail, not fall back to the fork HEAD")
	}
	for _, want := range []string{"payload.repo", "set payload.repo", "current upstream tip"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name the remedy (%q), got: %v", want, err)
		}
	}
	// No branch was created: the workspace is still on its original branch,
	// and the refused branch does not exist locally.
	if got := gitIn(t, ws, "branch", "--show-current"); got != "main" {
		t.Errorf("current branch = %q, want the fork's original branch (no branch may be cut)", got)
	}
	// show-ref --verify --quiet exits non-zero when the ref is ABSENT, which
	// is the desired outcome here; gitIn would fatal on that exit code, so
	// probe with exec directly. A zero exit means the branch was created —
	// the bug we are guarding against.
	probe := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	probe.Dir = ws
	if probe.Run() == nil {
		t.Errorf("branch %q unexpectedly exists after refusal", branch)
	}
}

// #1625: a freeform task with an empty payload.repo keeps the fork-HEAD
// fallback — freeform tasks carry no repo slug by design (the
// ResolveCloneURL contract), so nothing has drifted by construction.
func TestSetupTaskBranch_FreeformEmptyRepoKeepsForkHeadFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	_, mainSHA := seededRemote(t, dir)
	const branch = "foreman/freeform"

	ws := filepath.Join(dir, "ws")
	gitIn(t, "", "clone", filepath.Join(dir, "origin.git"), ws)

	task := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "freeform-1"},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind:    foremanv1alpha1.AgenticTaskKindFreeform,
			Payload: foremanv1alpha1.AgenticTaskPayload{Prompt: "just do a thing"},
		},
	}
	if err := setupTaskBranch(context.Background(), task, ws, branch, "main",
		upstreamURLForRepo, nil, logr.Discard()); err != nil {
		t.Fatalf("freeform with an empty payload.repo must keep the fork-HEAD fallback: %v", err)
	}
	if got := gitIn(t, ws, "branch", "--show-current"); got != branch {
		t.Errorf("current branch = %q, want %q", got, branch)
	}
	if got := gitIn(t, ws, "rev-parse", "HEAD"); got != mainSHA {
		t.Errorf("HEAD = %s, want the fork HEAD %s", got, mainSHA)
	}
}

// #1625: an issue-fix task with a valid payload.repo still takes the
// CreateBranchFromUpstream path, basing on the CURRENT upstream tip even
// though the fork clone is stale.
func TestSetupTaskBranch_IssueFixValidRepoBasesOnUpstreamTip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	_, forkSHA := seededRemote(t, dir)
	const branch = "foreman/wl/issue-641"

	ws := filepath.Join(dir, "fork-ws")
	gitIn(t, "", "clone", filepath.Join(dir, "origin.git"), ws)

	// Upstream advances AFTER the fork was cloned, so the fork is stale.
	adv := filepath.Join(dir, "adv")
	gitIn(t, "", "clone", filepath.Join(dir, "origin.git"), adv)
	if err := os.WriteFile(filepath.Join(adv, "UPSTREAM.md"), []byte("intervening upstream delta\n"), 0o644); err != nil {
		t.Fatalf("write UPSTREAM.md: %v", err)
	}
	gitIn(t, adv, "add", "UPSTREAM.md")
	gitIn(t, adv, "commit", "-m", "intervening upstream commit")
	gitIn(t, adv, "push", "origin", "main")
	upstreamSHA := gitIn(t, adv, "rev-parse", "HEAD")
	if upstreamSHA == forkSHA {
		t.Fatal("test setup wrong: upstream tip must differ from the fork tip")
	}

	// A valid slug resolves an upstream URL; the branch must land on the
	// upstream tip, not the stale fork HEAD.
	task := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "code-641"},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind:    foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{Repo: "defilantech/LLMKube", Issue: 641},
		},
	}
	if err := setupTaskBranch(context.Background(), task, ws, branch, "main",
		func(string) string { return filepath.Join(dir, "origin.git") }, nil, logr.Discard()); err != nil {
		t.Fatalf("setupTaskBranch with a valid slug: %v", err)
	}
	if got := gitIn(t, ws, "rev-parse", "HEAD"); got != upstreamSHA {
		t.Errorf("HEAD = %s, want the upstream tip %s (not the stale fork tip %s)", got, upstreamSHA, forkSHA)
	}
	if got := gitIn(t, ws, "branch", "--show-current"); got != branch {
		t.Errorf("current branch = %q, want %q", got, branch)
	}
}

// isForkOf contract for #1464: a fork shares the repo NAME (owner may
// differ); a differing repo name is an unrelated repository, not a fork.
func TestIsForkOf(t *testing.T) {
	cases := []struct {
		name      string
		remoteURL string
		taskRepo  string
		want      bool
	}{
		{name: "fork of same project", remoteURL: "https://github.com/Defilan/LLMKube.git", taskRepo: "defilantech/LLMKube", want: true},
		{name: "same repo", remoteURL: "https://github.com/defilantech/LLMKube.git", taskRepo: "defilantech/LLMKube", want: true},
		{name: "case-insensitive name", remoteURL: "https://github.com/Defilan/llmkube.git", taskRepo: "defilantech/LLMKube", want: true},
		{name: "scp-like fork", remoteURL: "git@github.com:Defilan/LLMKube.git", taskRepo: "defilantech/LLMKube", want: true},
		{name: "different project is not a fork", remoteURL: "https://github.com/Defilan/LLMKube.git", taskRepo: "Defilan/greenhouse-demo", want: false},
		{name: "local path is not a fork", remoteURL: "/tmp/origin.git", taskRepo: "a/b", want: false},
		{name: "file:// is not a fork", remoteURL: "file:///tmp/origin.git", taskRepo: "a/b", want: false},
		{name: "empty remote", remoteURL: "", taskRepo: "a/b", want: false},
		{name: "empty task repo", remoteURL: "https://github.com/o/r.git", taskRepo: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isForkOf(tc.remoteURL, tc.taskRepo); got != tc.want {
				t.Errorf("isForkOf(%q, %q) = %v, want %v", tc.remoteURL, tc.taskRepo, got, tc.want)
			}
		})
	}
}

// pushedRemoteMismatch reads the workspace's origin after a push and flags a
// branch that landed in a repository other than the task's payload.repo.
func TestPushedRemoteMismatch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "origin.git")
	gitIn(t, "", "init", "--bare", bare)
	ws := filepath.Join(dir, "ws")
	gitIn(t, "", "clone", bare, ws)

	// Local bare path is not owner/repo shaped -> no mismatch flagged.
	if msg := pushedRemoteMismatch(context.Background(), ws, "defilantech/LLMKube"); msg != "" {
		t.Errorf("local bare remote must not be flagged, got: %s", msg)
	}
	// Empty task repo -> no mismatch flagged.
	if msg := pushedRemoteMismatch(context.Background(), ws, ""); msg != "" {
		t.Errorf("empty task repo must not be flagged, got: %s", msg)
	}

	// Positive case (#1464): the pushed remote genuinely disagrees with
	// payload.repo. Point origin at a differently-named repository and assert
	// the mismatch is flagged. If pushedRemoteMismatch unconditionally returned
	// "" this would fail, which is exactly the regression that would
	// reintroduce #1464 (a push to an unrelated repo reported as GO).
	if err := repo.SetRemote(context.Background(), ws, "origin",
		"https://github.com/Defilan/greenhouse-demo.git"); err != nil {
		t.Fatalf("SetRemote origin to a differently-named repo: %v", err)
	}
	if msg := pushedRemoteMismatch(context.Background(), ws, "defilantech/LLMKube"); msg == "" {
		t.Fatalf("remote greenhouse-demo vs task repo defilantech/LLMKube must be " +
			"flagged as a mismatch; got an empty message")
	}

	// Control: a fork (same repo name, different owner) is NOT a mismatch —
	// the legitimate fork deployment must stay off this guard.
	if err := repo.SetRemote(context.Background(), ws, "origin",
		"https://github.com/Defilan/LLMKube.git"); err != nil {
		t.Fatalf("SetRemote origin to a fork of the task repo: %v", err)
	}
	if msg := pushedRemoteMismatch(context.Background(), ws, "defilantech/LLMKube"); msg != "" {
		t.Errorf("a fork of the task repo must not be flagged, got: %s", msg)
	}
}

// TestOpenPRsAsDraft covers the #1706 resolution, including the two cases that
// decide whether an upgrade is silent: a nil Agent and an unset field must both
// stay true, so 0.9.22 behaviour is preserved for anyone who has not opted in.
func TestOpenPRsAsDraft(t *testing.T) {
	boolp := func(b bool) *bool { return &b }
	cases := []struct {
		name  string
		agent *foremanv1alpha1.Agent
		want  bool
	}{
		{
			name:  "nil agent defaults to draft",
			agent: nil,
			want:  true,
		},
		{
			name:  "unset field defaults to draft (preserves #1703)",
			agent: &foremanv1alpha1.Agent{},
			want:  true,
		},
		{
			name: "explicit true opens a draft",
			agent: &foremanv1alpha1.Agent{
				Spec: foremanv1alpha1.AgentSpec{OpenPullRequestsAsDraft: boolp(true)},
			},
			want: true,
		},
		{
			// The reported case: an unattended loop whose reviewer workflow
			// skips drafts needs ready-for-review PRs.
			name: "explicit false opens ready for review",
			agent: &foremanv1alpha1.Agent{
				Spec: foremanv1alpha1.AgentSpec{OpenPullRequestsAsDraft: boolp(false)},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := openPRsAsDraft(tc.agent); got != tc.want {
				t.Fatalf("openPRsAsDraft() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- ALREADY-RESOLVED resolvedBy validation (#1692) ----

// setupResolvedByFixtureRepo builds a real git repo used by
// TestPromoteTerminalOutcome_AlreadyResolvedResolvedByValidation:
//
//	main:  baseSHA -> mainTipSHA (HEAD)
//	side:  sideSHA (cut from baseSHA, not an ancestor of main)
//
// baseBranch is "main". mainTipSHA and baseSHA are ancestors of main;
// sideSHA resolves but is not an ancestor; "deadbeef..." is a well-formed
// SHA that does not resolve.
func setupResolvedByFixtureRepo(t *testing.T) (ws string, baseSHA, mainTipSHA, sideSHA string) {
	t.Helper()
	ws = t.TempDir()
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = ws
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
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(ws, rel)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	write("a.go", "package a\n")
	run("add", ".")
	run("commit", "-m", "base")
	baseSHA = run("rev-parse", "HEAD")

	write("b.go", "package b\n")
	run("add", ".")
	run("commit", "-m", "main tip")
	mainTipSHA = run("rev-parse", "HEAD")

	run("checkout", "-b", "side")
	write("c.go", "package c\n")
	run("add", ".")
	run("commit", "-m", "side work")
	sideSHA = run("rev-parse", "HEAD")
	return ws, baseSHA, mainTipSHA, sideSHA
}

// TestPromoteTerminalOutcome_AlreadyResolvedResolvedByValidation pins the
// #1692 guard: an ALREADY-RESOLVED terminal is only allowed to keep its
// verdict when extra.resolvedBy names a commit that resolves AND is an
// ancestor of the task's base branch. Everything else is downgraded to
// NEEDS-VERIFICATION (the shared needsVerificationOutcome constant) so the
// work is still gated rather than skipped. The claim is preserved under
// resolvedByClaimed for the audit record; it is never silently dropped.
//
// Mutation this test kills: delete the ancestor check (or the whole
// resolvedBy validation) and the free-text case below stops being
// downgraded -- the exact failure #1692 describes.
func TestPromoteTerminalOutcome_AlreadyResolvedResolvedByValidation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ws, baseSHA, mainTipSHA, sideSHA := setupResolvedByFixtureRepo(t)

	// The two live strings from the #1692 report. The false claim's branch
	// name + prose is used verbatim as the free-text case; the true one
	// (88c85cce, a real ancestor commit) cannot be reproduced by SHA in a
	// fresh fixture, so its semantic -- a valid ancestor -- is covered by
	// the fixture's mainTipSHA / baseSHA cases instead.
	const liveFalseClaim = "issue-1676-prefetch-claimname (working tree clean; full controller suite passes with envtest 1.31.0 and 1.32.x)"

	wellFormedUnknownSHA := strings.Repeat("0", 40)

	cases := []struct {
		name          string
		resolvedBy    string
		wantOutcome   string
		wantKeepClaim bool // whether resolvedBy is promoted verbatim
		wantClaimed   string
	}{
		{
			name:          "valid ancestor SHA stays ALREADY-RESOLVED",
			resolvedBy:    mainTipSHA,
			wantOutcome:   alreadyResolvedOutcome,
			wantKeepClaim: true,
		},
		{
			name:          "earlier ancestor SHA stays ALREADY-RESOLVED",
			resolvedBy:    baseSHA,
			wantOutcome:   alreadyResolvedOutcome,
			wantKeepClaim: true,
		},
		{
			name:        "well-formed SHA that is not an ancestor is downgraded",
			resolvedBy:  sideSHA,
			wantOutcome: needsVerificationOutcome,
			wantClaimed: sideSHA,
		},
		{
			name:        "SHA that does not resolve is downgraded",
			resolvedBy:  wellFormedUnknownSHA,
			wantOutcome: needsVerificationOutcome,
			wantClaimed: wellFormedUnknownSHA,
		},
		{
			name:        "free text (live false claim) is downgraded",
			resolvedBy:  liveFalseClaim,
			wantOutcome: needsVerificationOutcome,
			wantClaimed: liveFalseClaim,
		},
		{
			name:        "empty resolvedBy is downgraded and drops nothing",
			resolvedBy:  "",
			wantOutcome: needsVerificationOutcome,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			terminalExtra := map[string]any{
				"outcome":    alreadyResolvedOutcome,
				"resolvedBy": tc.resolvedBy,
			}
			extra := map[string]any{}
			promoteTerminalOutcome(context.Background(), extra, terminalExtra,
				ws, execCommandRunner, "main")

			if got, _ := extra["outcome"].(string); got != tc.wantOutcome {
				t.Fatalf("extra.outcome = %q, want %q", got, tc.wantOutcome)
			}

			gotResolvedBy, hasResolvedBy := extra["resolvedBy"].(string)
			if tc.wantKeepClaim {
				if !hasResolvedBy || gotResolvedBy != tc.resolvedBy {
					t.Errorf("resolvedBy should be promoted verbatim; got %q (present=%v)",
						gotResolvedBy, hasResolvedBy)
				}
				if _, claimed := extra["resolvedByClaimed"]; claimed {
					t.Errorf("resolvedByClaimed should NOT be set on the happy path")
				}
			} else {
				if hasResolvedBy {
					t.Errorf("resolvedBy should not be promoted after downgrade; got %q", gotResolvedBy)
				}
				if gotClaimed, _ := extra["resolvedByClaimed"].(string); gotClaimed != tc.wantClaimed {
					t.Errorf("resolvedByClaimed = %q, want %q", gotClaimed, tc.wantClaimed)
				}
			}
		})
	}
}
