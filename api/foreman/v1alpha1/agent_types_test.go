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

package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestAgentDeepCopySharesNoState verifies the generated DeepCopy honors
// the rule that mutating the copy must never touch the original. The
// fields most likely to share backing arrays (Tools, Conditions, the
// embedded RequiredCapability with its NodeSelector map) are exercised
// explicitly.
func TestAgentDeepCopySharesNoState(t *testing.T) {
	temp := "0.4"
	orig := &Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "default"},
		Spec: AgentSpec{
			Role:                AgentRoleCoder,
			Model:               "carnice-q35",
			InferenceServiceRef: corev1.LocalObjectReference{Name: "svc"},
			SystemPrompt:        "you are a coder",
			Temperature:         &temp,
			MaxTurns:            50,
			MaxRetries:          3,
			Tools:               []string{"read_file", "submit_result"},
			RequiredCapability: RequiredCapability{
				Accelerator:  "metal",
				MinRAMGB:     96,
				NodeSelector: map[string]string{"role": "coder"},
			},
		},
		Status: AgentStatus{
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
		},
	}
	cp := orig.DeepCopy()

	// Mutating the copy's slice / map / pointer fields must not touch the
	// original. This is the regression net for "added a new field and
	// forgot to regen DeepCopy".
	cp.Spec.Tools[0] = "mutated"
	if orig.Spec.Tools[0] != "read_file" {
		t.Errorf("Tools slice was shared; orig[0]=%q after copy mutation", orig.Spec.Tools[0])
	}

	cp.Spec.RequiredCapability.NodeSelector["role"] = "mutated"
	if orig.Spec.RequiredCapability.NodeSelector["role"] != "coder" {
		t.Errorf("NodeSelector map was shared; orig role=%q", orig.Spec.RequiredCapability.NodeSelector["role"])
	}

	newTemp := "0.9"
	cp.Spec.Temperature = &newTemp
	if *orig.Spec.Temperature != "0.4" {
		t.Errorf("Temperature pointer was aliased; orig=%q", *orig.Spec.Temperature)
	}

	cp.Status.Conditions[0].Status = metav1.ConditionFalse
	if orig.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Errorf("Conditions slice was shared; orig=%q", orig.Status.Conditions[0].Status)
	}
}

// TestAgentDeepCopyHandlesNilTemperature ensures Temperature=nil round-
// trips through DeepCopy without panicking. The earliest place a nil
// pointer crash would surface is the scheduler reading a freshly applied
// Agent CR.
func TestAgentDeepCopyHandlesNilTemperature(t *testing.T) {
	orig := &Agent{
		Spec: AgentSpec{
			Role:                AgentRoleReviewer,
			InferenceServiceRef: corev1.LocalObjectReference{Name: "svc"},
			SystemPrompt:        "you are a reviewer",
			Tools:               []string{"submit_result"},
		},
	}
	cp := orig.DeepCopy()
	if cp.Spec.Temperature != nil {
		t.Errorf("DeepCopy filled in Temperature unexpectedly: %v", *cp.Spec.Temperature)
	}
}

// TestAgenticTaskAgentRefDeepCopy is the analogue for the new AgentRef
// field on AgenticTaskSpec. A pointer that aliases between original and
// copy would let a controller mutating one silently corrupt the other.
func TestAgenticTaskAgentRefDeepCopy(t *testing.T) {
	orig := &AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-1", Namespace: "default"},
		Spec: AgenticTaskSpec{
			Kind:     AgenticTaskKindIssueFix,
			Payload:  AgenticTaskPayload{Repo: "x/y", Issue: 1},
			AgentRef: &corev1.LocalObjectReference{Name: "coder"},
		},
	}
	cp := orig.DeepCopy()

	cp.Spec.AgentRef.Name = "mutated"
	if orig.Spec.AgentRef.Name != "coder" {
		t.Errorf("AgentRef pointer was aliased; orig=%q", orig.Spec.AgentRef.Name)
	}

	// Nil AgentRef on a separate task must round-trip without panic.
	orig2 := &AgenticTask{
		Spec: AgenticTaskSpec{
			Kind:    AgenticTaskKindFreeform,
			Payload: AgenticTaskPayload{Prompt: "hi"},
		},
	}
	cp2 := orig2.DeepCopy()
	if cp2.Spec.AgentRef != nil {
		t.Errorf("DeepCopy invented an AgentRef from nil: %+v", cp2.Spec.AgentRef)
	}
}

// TestAgentDeterministicShape exercises the M4 contract for deterministic
// Agents: InferenceServiceRef + SystemPrompt may both be empty, and a
// DeepCopy of that shape must round-trip cleanly without inventing
// fields. The gate Agent (M4) is the canonical caller.
func TestAgentDeterministicShape(t *testing.T) {
	orig := &Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "gate", Namespace: "default"},
		Spec: AgentSpec{
			Role:               AgentRoleVerifier,
			Tools:              []string{"run_gate_job"},
			RequiredCapability: RequiredCapability{Roles: []string{"verifier"}},
			// InferenceServiceRef + SystemPrompt deliberately omitted.
		},
	}
	cp := orig.DeepCopy()
	if cp.Spec.InferenceServiceRef.Name != "" {
		t.Errorf("DeepCopy invented an InferenceServiceRef: %q", cp.Spec.InferenceServiceRef.Name)
	}
	if cp.Spec.SystemPrompt != "" {
		t.Errorf("DeepCopy invented a SystemPrompt: %q", cp.Spec.SystemPrompt)
	}
	// Mutating Tools / Roles on the copy must not touch the original.
	cp.Spec.Tools[0] = "mutated"
	if orig.Spec.Tools[0] != "run_gate_job" {
		t.Errorf("Tools slice was shared")
	}
	cp.Spec.RequiredCapability.Roles[0] = "mutated"
	if orig.Spec.RequiredCapability.Roles[0] != "verifier" {
		t.Errorf("RequiredCapability.Roles slice was shared")
	}
}

// TestAgentRoleConstantsMatchEnum guards against a future contributor
// renaming a constant without updating the kubebuilder enum tag. The
// CRD schema validation is the API server's job; this test only catches
// the source-level drift.
func TestAgentRoleConstantsMatchEnum(t *testing.T) {
	want := map[AgentRole]struct{}{
		AgentRoleCoder:    {},
		AgentRoleVerifier: {},
		AgentRoleReviewer: {},
		AgentRolePlanner:  {},
	}
	got := map[AgentRole]struct{}{
		"coder":    {},
		"verifier": {},
		"reviewer": {},
		"planner":  {},
	}
	if len(want) != len(got) {
		t.Fatalf("constant count drift: want=%d got=%d", len(want), len(got))
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("constant %q not in expected set", k)
		}
	}
}
