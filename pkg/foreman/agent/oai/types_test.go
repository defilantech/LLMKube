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

package oai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMessageMarshalJSON_NonAssistantAlwaysIncludesContent pins the
// #556 fix: non-assistant messages (system / user / tool) MUST carry a
// `content` field on the wire even when the value is empty, otherwise
// strict-schema backends (Devstral / Mistral / DeepSeek / OpenAI API)
// reject with HTTP 400. Carnice and other lenient llama.cpp backends
// tolerate the omission, which is why this never surfaced until live
// against Devstral.
func TestMessageMarshalJSON_NonAssistantAlwaysIncludesContent(t *testing.T) {
	cases := []struct {
		name        string
		msg         Message
		wantHasKey  bool   // wire payload must contain `"content":`
		wantContent string // expected value of content key (only checked when wantHasKey)
	}{
		{
			name:        "system role with non-empty content emits content",
			msg:         Message{Role: RoleSystem, Content: "you are a coder"},
			wantHasKey:  true,
			wantContent: "you are a coder",
		},
		{
			name:        "system role with empty content STILL emits content (#556)",
			msg:         Message{Role: RoleSystem, Content: ""},
			wantHasKey:  true,
			wantContent: "",
		},
		{
			name:        "user role with empty content STILL emits content (#556)",
			msg:         Message{Role: RoleUser, Content: ""},
			wantHasKey:  true,
			wantContent: "",
		},
		{
			name: "tool role with empty content STILL emits content (the original Devstral repro)",
			msg: Message{
				Role:       RoleTool,
				Content:    "",
				ToolCallID: "call_abc123",
			},
			wantHasKey:  true,
			wantContent: "",
		},
		{
			name: "tool role with non-empty content emits the result verbatim",
			msg: Message{
				Role:       RoleTool,
				Content:    `{"exit_code":0,"stdout":"ok"}`,
				ToolCallID: "call_abc123",
			},
			wantHasKey:  true,
			wantContent: `{"exit_code":0,"stdout":"ok"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if tc.wantHasKey && !strings.Contains(string(raw), `"content":`) {
				t.Errorf("payload missing required content key: %s", raw)
			}
			// Round-trip parse to confirm the content value is exactly
			// what we expected (catches any escape-encoding regressions).
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("round-trip decode: %v", err)
			}
			if got, ok := decoded["content"]; !ok {
				t.Errorf("decoded map missing content key: %v", decoded)
			} else if got != tc.wantContent {
				t.Errorf("content: want %q got %v", tc.wantContent, got)
			}
		})
	}
}

// TestMessageMarshalJSON_AssistantOmitsContentWhenEmpty pins the
// counter-case: assistant messages with no text content (only
// tool_calls) MUST NOT emit `"content":""`. Some servers reject the
// combination of empty content + tool_calls; the OpenAI spec allows
// content to be absent on assistant turns. Keep the existing
// omitempty semantics for this role.
func TestMessageMarshalJSON_AssistantOmitsContentWhenEmpty(t *testing.T) {
	cases := []struct {
		name       string
		msg        Message
		wantHasKey bool
	}{
		{
			name: "assistant with empty content + tool_calls omits content",
			msg: Message{
				Role: RoleAssistant,
				ToolCalls: []ToolCall{
					{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "read_file", Arguments: `{"path":"foo.go"}`}},
				},
			},
			wantHasKey: false,
		},
		{
			name: "assistant with non-empty content emits content",
			msg: Message{
				Role:    RoleAssistant,
				Content: "thinking through this...",
			},
			wantHasKey: true,
		},
		{
			name:       "assistant with empty content AND no tool_calls still omits content (edge case)",
			msg:        Message{Role: RoleAssistant},
			wantHasKey: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			hasKey := strings.Contains(string(raw), `"content":`)
			if hasKey != tc.wantHasKey {
				t.Errorf("content key presence: want %v got %v (payload=%s)", tc.wantHasKey, hasKey, raw)
			}
		})
	}
}

// TestChatRequestMarshalJSON_MaxTokens pins the per-turn generation cap
// wire contract: max_tokens must appear in the marshaled body when set
// (> 0) so the server bounds a reasoning model's decision-turn <think>,
// and must be omitted when zero so the request defers to the server's own
// default (max_model_len - prompt) rather than pinning it to 0 (which
// would generate nothing).
func TestChatRequestMarshalJSON_MaxTokens(t *testing.T) {
	cases := []struct {
		name       string
		maxTokens  int
		wantHasKey bool
	}{
		{name: "positive max_tokens is emitted", maxTokens: 8192, wantHasKey: true},
		{name: "zero max_tokens is omitted", maxTokens: 0, wantHasKey: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(ChatRequest{
				Model:     "test",
				Messages:  []Message{{Role: RoleUser, Content: "go"}},
				MaxTokens: tc.maxTokens,
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			hasKey := strings.Contains(string(raw), `"max_tokens":`)
			if hasKey != tc.wantHasKey {
				t.Errorf("max_tokens present=%v want=%v in %s", hasKey, tc.wantHasKey, raw)
			}
			if tc.wantHasKey && !strings.Contains(string(raw), `"max_tokens":8192`) {
				t.Errorf("expected max_tokens=8192 in %s", raw)
			}
		})
	}
}

// TestMessageMarshalJSON_RoleAlwaysEmitted is a belt-and-suspenders
// guard: regardless of which marshal branch a message goes through,
// the role field must always be on the wire (it's the primary
// dispatch key for the server). A bug in either branch that dropped
// the role would surface here.
func TestMessageMarshalJSON_RoleAlwaysEmitted(t *testing.T) {
	for _, role := range []Role{RoleSystem, RoleUser, RoleAssistant, RoleTool} {
		raw, err := json.Marshal(Message{Role: role})
		if err != nil {
			t.Fatalf("marshal %q: %v", role, err)
		}
		want := `"role":"` + string(role) + `"`
		if !strings.Contains(string(raw), want) {
			t.Errorf("role %q missing from payload %s", role, raw)
		}
	}
}
