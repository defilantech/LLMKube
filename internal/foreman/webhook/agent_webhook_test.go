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

package webhook

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	executoragent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/tools/catalog"
)

// validAgent builds a baseline LLM-driven Agent that passes validation;
// each test mutates one field to exercise a single branch.
func validLLMAgent() *foremanv1alpha1.Agent {
	return &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:                foremanv1alpha1.AgentRoleCoder,
			InferenceServiceRef: corev1.LocalObjectReference{Name: "qwen"},
			SystemPrompt:        "You are a coder.",
			Tools:               []string{"read_file", "write_file", "submit_result"},
		},
	}
}

// validDeterministicAgent builds a baseline gate-shaped Agent (no
// InferenceServiceRef, no SystemPrompt, one non-terminal tool).
func validDeterministicAgent() *foremanv1alpha1.Agent {
	return &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "gate", Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:  foremanv1alpha1.AgentRoleVerifier,
			Tools: []string{"run_gate_job", "submit_result"},
		},
	}
}

func TestAgentValidator_Create(t *testing.T) {
	v := &AgentValidator{}
	ctx := context.Background()

	tests := []struct {
		name      string
		mutate    func(*foremanv1alpha1.Agent)
		wantError bool
	}{
		{
			name:      "valid LLM-driven agent accepted",
			mutate:    func(*foremanv1alpha1.Agent) {},
			wantError: false,
		},
		{
			name: "valid deterministic agent accepted",
			mutate: func(a *foremanv1alpha1.Agent) {
				*a = *validDeterministicAgent()
			},
			wantError: false,
		},
		{
			name: "LLM agent with empty systemPrompt rejected",
			mutate: func(a *foremanv1alpha1.Agent) {
				a.Spec.SystemPrompt = ""
			},
			wantError: true,
		},
		{
			name: "LLM agent with whitespace-only systemPrompt rejected",
			mutate: func(a *foremanv1alpha1.Agent) {
				a.Spec.SystemPrompt = "   \n\t "
			},
			wantError: true,
		},
		{
			name: "deterministic agent with systemPrompt rejected",
			mutate: func(a *foremanv1alpha1.Agent) {
				*a = *validDeterministicAgent()
				a.Spec.SystemPrompt = "should not be here"
			},
			wantError: true,
		},
		{
			name: "deterministic agent with only submit_result rejected (no usable tool)",
			mutate: func(a *foremanv1alpha1.Agent) {
				*a = *validDeterministicAgent()
				a.Spec.Tools = []string{"submit_result"}
			},
			wantError: true,
		},
		{
			name: "unknown tool name rejected",
			mutate: func(a *foremanv1alpha1.Agent) {
				a.Spec.Tools = []string{"read_file", "typo_tool", "submit_result"}
			},
			wantError: true,
		},
		{
			name: "cloud-proxy agent is never deterministic; empty isvc requires systemPrompt path NOT taken",
			mutate: func(a *foremanv1alpha1.Agent) {
				// Provider cloud-proxy with empty InferenceServiceRef is
				// LLM-driven (isDeterministicAgent == false), so a
				// non-empty systemPrompt is REQUIRED, not forbidden.
				a.Spec.Provider = foremanv1alpha1.AgentProviderCloudProxy
				a.Spec.InferenceServiceRef = corev1.LocalObjectReference{}
				a.Spec.SystemPrompt = "You are a cloud reviewer."
			},
			wantError: false,
		},
		{
			name: "cloud-proxy agent with empty systemPrompt rejected",
			mutate: func(a *foremanv1alpha1.Agent) {
				a.Spec.Provider = foremanv1alpha1.AgentProviderCloudProxy
				a.Spec.InferenceServiceRef = corev1.LocalObjectReference{}
				a.Spec.SystemPrompt = ""
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := validLLMAgent()
			tt.mutate(agent)
			_, err := v.ValidateCreate(ctx, agent)
			if tt.wantError && err == nil {
				t.Fatalf("expected validation error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tt.wantError && err != nil && !apierrors.IsInvalid(err) {
				t.Fatalf("expected an Invalid error, got %T: %v", err, err)
			}
		})
	}
}

// TestAgentValidator_Update reuses the create branches when the spec
// changes: a spec-changing update applies the same spec invariants.
func TestAgentValidator_Update(t *testing.T) {
	v := &AgentValidator{}
	ctx := context.Background()

	// A spec-changing update to an invalid spec is rejected. oldObj here
	// is valid, so this is a genuine spec change into an invalid state.
	bad := validLLMAgent()
	bad.Spec.SystemPrompt = ""
	if _, err := v.ValidateUpdate(ctx, validLLMAgent(), bad); err == nil {
		t.Fatalf("expected spec-changing update of an LLM agent into empty systemPrompt to fail")
	}

	good := validLLMAgent()
	good.Spec.SystemPrompt = "You are still a coder, now improved."
	if _, err := v.ValidateUpdate(ctx, validLLMAgent(), good); err != nil {
		t.Fatalf("expected spec-changing update to a valid spec to pass, got %v", err)
	}
}

// TestAgentValidator_Update_Grandfathering proves Fix 4: a status/metadata
// patch that leaves the spec untouched is accepted even when the spec is
// already invalid, so a grandfathered bad Agent cannot be wedged. A patch
// that actually changes the (invalid) spec is still rejected.
func TestAgentValidator_Update_Grandfathering(t *testing.T) {
	v := &AgentValidator{}
	ctx := context.Background()

	// An Agent that would fail create-time validation (LLM-driven but no
	// systemPrompt) is presumed to predate the webhook.
	invalid := validLLMAgent()
	invalid.Spec.SystemPrompt = ""
	if _, err := v.ValidateCreate(ctx, invalid.DeepCopy()); err == nil {
		t.Fatalf("test setup: expected the invalid agent to fail create validation")
	}

	// Status/metadata-only patch: spec unchanged -> accepted.
	statusPatched := invalid.DeepCopy()
	statusPatched.Labels = map[string]string{"patched": "true"}
	statusPatched.Status.ObservedGeneration = 7
	if _, err := v.ValidateUpdate(ctx, invalid.DeepCopy(), statusPatched); err != nil {
		t.Fatalf("expected status-only update of a spec-invalid agent to be accepted (grandfathering), got %v", err)
	}

	// Spec-changing patch into a still-invalid state -> rejected.
	specPatched := invalid.DeepCopy()
	specPatched.Spec.Tools = []string{"read_file", "totally_made_up_tool", "submit_result"}
	if _, err := v.ValidateUpdate(ctx, invalid.DeepCopy(), specPatched); err == nil {
		t.Fatalf("expected a spec-changing update of an invalid agent to be re-validated and rejected")
	}
}

// TestDeterministicPredicateMatchesExecutor is the real drift guard. The
// webhook keeps a PRIVATE copy of the deterministic predicate (it must not
// import pkg/foreman/agent into the operator binary). This test, which is
// allowed to import the executor package, asserts that the webhook's
// private copies produce identical results to the exported executor
// helpers (executoragent.IsDeterministicAgent /
// executoragent.FirstDeterministicTool) across a shared table.
//
// Because the executor's own isDeterministicAgent / pickDeterministicTool
// delegate to those exported helpers, editing the executor's real behavior
// changes the exported helpers and this test fails until the webhook's
// private copy is brought back in line. That is the guarantee the previous
// version of this test did not provide.
func TestDeterministicPredicateMatchesExecutor(t *testing.T) {
	t.Run("IsDeterministicAgent", func(t *testing.T) {
		specs := []foremanv1alpha1.AgentSpec{
			// LLM-driven: inferenceServiceRef set, default provider.
			{InferenceServiceRef: corev1.LocalObjectReference{Name: "qwen"}},
			// Deterministic: empty provider, empty isvc.
			{},
			// Deterministic: explicit local provider, empty isvc.
			{Provider: foremanv1alpha1.AgentProviderLocal},
			// LLM-driven: local provider but isvc set.
			{Provider: foremanv1alpha1.AgentProviderLocal, InferenceServiceRef: corev1.LocalObjectReference{Name: "qwen"}},
			// LLM-driven: cloud-proxy is never deterministic, even with empty isvc.
			{Provider: foremanv1alpha1.AgentProviderCloudProxy},
			{Provider: foremanv1alpha1.AgentProviderCloudProxy, InferenceServiceRef: corev1.LocalObjectReference{Name: "remote"}},
		}
		for _, spec := range specs {
			a := &foremanv1alpha1.Agent{Spec: spec}
			want := executoragent.IsDeterministicAgent(spec)
			if got := isDeterministicAgent(a); got != want {
				t.Errorf("isDeterministicAgent(%+v) = %v; executor IsDeterministicAgent = %v (webhook private copy drifted from executor)", spec, got, want)
			}
		}
	})

	t.Run("FirstDeterministicTool vs hasUsableDeterministicTool", func(t *testing.T) {
		toolSets := [][]string{
			{"run_gate_job", "submit_result"},
			{"submit_result"},
			{""},
			nil,
			{"read_file"},
			{"submit_result", "run_gate_job"},
			{"", "submit_result", "write_file"},
		}
		for _, ts := range toolSets {
			// The webhook's hasUsableDeterministicTool is true exactly
			// when the executor would find a tool to dispatch, i.e. when
			// FirstDeterministicTool returns a non-empty name.
			wantUsable := executoragent.FirstDeterministicTool(ts) != ""
			if got := hasUsableDeterministicTool(ts); got != wantUsable {
				t.Errorf("hasUsableDeterministicTool(%v) = %v; executor FirstDeterministicTool non-empty = %v (webhook private copy drifted from executor)", ts, got, wantUsable)
			}
		}
	})
}

// TestCanonicalToolNamesCoversWhitelist guards that every name the
// validator accepts is a real registered tool (and that the canonical set
// is non-empty so a registry refactor that empties it would fail loud).
func TestCanonicalToolNamesCoversWhitelist(t *testing.T) {
	names := catalog.CanonicalToolNames()
	if len(names) == 0 {
		t.Fatalf("CanonicalToolNames returned empty; webhook would reject every tool")
	}
	wantSubset := []string{"read_file", "write_file", "submit_result", "run_gate_job"}
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	for _, w := range wantSubset {
		if !have[w] {
			t.Errorf("canonical tool set missing expected tool %q (have %v)", w, names)
		}
	}
}
