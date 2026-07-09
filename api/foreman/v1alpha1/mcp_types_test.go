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
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// TestAgentMCPDeepCopy builds an Agent with a populated MCP block (two
// servers, one carrying a header sourced from a Secret plus an
// AllowedTools whitelist) and asserts DeepCopy produces a value-equal,
// non-aliased clone. This is the regression net for "added a new
// pointer/slice field to MCPConfig and forgot to regen DeepCopy."
func TestAgentMCPDeepCopy(t *testing.T) {
	orig := &Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "default"},
		Spec: AgentSpec{
			Role:                AgentRoleCoder,
			InferenceServiceRef: corev1.LocalObjectReference{Name: "svc"},
			SystemPrompt:        "you are a coder",
			Tools:               []string{"read_file", "submit_result"},
			MCP: &MCPConfig{
				Enabled:        true,
				CallTimeout:    metav1.Duration{Duration: 45 * time.Second},
				MaxResultBytes: 65536,
				Servers: []MCPServer{
					{
						Name:      "search",
						Transport: "http",
						URL:       "http://mcp-search.default.svc:8080",
						Headers: []MCPHeader{
							{
								Name: "Authorization",
								ValueFrom: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "search-token"},
									Key:                  "token",
								},
							},
						},
						AllowedTools: []string{"web_search", "fetch_url"},
					},
					{
						Name:      "fs",
						Transport: "http",
						URL:       "http://mcp-fs.default.svc:8080",
					},
				},
			},
		},
	}

	cp := orig.DeepCopy()

	if !reflect.DeepEqual(orig, cp) {
		t.Fatalf("DeepCopy is not value-equal to original:\norig=%+v\ncp=%+v", orig, cp)
	}

	// Mutating the copy's nested slices/pointers must never touch the
	// original.
	cp.Spec.MCP.Servers[0].AllowedTools[0] = "mutated"
	if orig.Spec.MCP.Servers[0].AllowedTools[0] != "web_search" {
		t.Errorf("AllowedTools slice was shared; orig[0]=%q", orig.Spec.MCP.Servers[0].AllowedTools[0])
	}

	cp.Spec.MCP.Servers[0].Headers[0].ValueFrom.Key = "mutated"
	if orig.Spec.MCP.Servers[0].Headers[0].ValueFrom.Key != "token" {
		t.Errorf("Header ValueFrom pointer was aliased; orig.Key=%q", orig.Spec.MCP.Servers[0].Headers[0].ValueFrom.Key)
	}

	cp.Spec.MCP.Servers[0].Name = "mutated"
	if orig.Spec.MCP.Servers[0].Name != "search" {
		t.Errorf("Servers slice was shared; orig[0].Name=%q", orig.Spec.MCP.Servers[0].Name)
	}

	cp.Spec.MCP.Enabled = false
	if !orig.Spec.MCP.Enabled {
		t.Errorf("MCPConfig pointer was aliased; orig.Enabled=%v", orig.Spec.MCP.Enabled)
	}
}

// TestAgentMCPDeepCopyNil ensures a nil AgentSpec.MCP round-trips through
// DeepCopy without panicking or inventing a value, matching the "MCP
// disabled by default" contract.
func TestAgentMCPDeepCopyNil(t *testing.T) {
	orig := &Agent{
		Spec: AgentSpec{
			Role:                AgentRoleReviewer,
			InferenceServiceRef: corev1.LocalObjectReference{Name: "svc"},
			Tools:               []string{"submit_result"},
		},
	}
	cp := orig.DeepCopy()
	if cp.Spec.MCP != nil {
		t.Errorf("DeepCopy invented an MCP block: %+v", cp.Spec.MCP)
	}
}

// TestWorkloadSpecMCPEnabledDeepCopy round-trips the benchmark opt-out
// pointer through DeepCopy without aliasing.
func TestWorkloadSpecMCPEnabledDeepCopy(t *testing.T) {
	orig := &Workload{
		ObjectMeta: metav1.ObjectMeta{Name: "batch", Namespace: "default"},
		Spec: WorkloadSpec{
			Intent:     "fix bugs",
			MCPEnabled: ptr.To(false),
		},
	}
	cp := orig.DeepCopy()

	if !reflect.DeepEqual(orig, cp) {
		t.Fatalf("DeepCopy is not value-equal to original:\norig=%+v\ncp=%+v", orig, cp)
	}

	*cp.Spec.MCPEnabled = true
	if orig.Spec.MCPEnabled == nil || *orig.Spec.MCPEnabled != false {
		t.Errorf("MCPEnabled pointer was aliased; orig=%v", orig.Spec.MCPEnabled)
	}
}

// TestAgenticTaskSpecMCPEnabledDeepCopy round-trips the reconciler-
// propagated effective opt-out through DeepCopy without aliasing.
func TestAgenticTaskSpecMCPEnabledDeepCopy(t *testing.T) {
	orig := &AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "code-1", Namespace: "default"},
		Spec: AgenticTaskSpec{
			Kind:       AgenticTaskKindIssueFix,
			Payload:    AgenticTaskPayload{Repo: "x/y", Issue: 1},
			MCPEnabled: ptr.To(true),
		},
	}
	cp := orig.DeepCopy()

	if !reflect.DeepEqual(orig, cp) {
		t.Fatalf("DeepCopy is not value-equal to original:\norig=%+v\ncp=%+v", orig, cp)
	}

	*cp.Spec.MCPEnabled = false
	if orig.Spec.MCPEnabled == nil || *orig.Spec.MCPEnabled != true {
		t.Errorf("MCPEnabled pointer was aliased; orig=%v", orig.Spec.MCPEnabled)
	}

	// Nil MCPEnabled on a separate task must round-trip without panic.
	orig2 := &AgenticTask{
		Spec: AgenticTaskSpec{
			Kind:    AgenticTaskKindFreeform,
			Payload: AgenticTaskPayload{Prompt: "hi"},
		},
	}
	cp2 := orig2.DeepCopy()
	if cp2.Spec.MCPEnabled != nil {
		t.Errorf("DeepCopy invented an MCPEnabled from nil: %v", cp2.Spec.MCPEnabled)
	}
}
