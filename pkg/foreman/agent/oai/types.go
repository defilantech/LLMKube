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

// Package oai is a minimal OpenAI-compatible chat-completions client.
//
// It targets the subset of the v1 chat-completions API that llama.cpp's
// OAI-compatible /v1/chat/completions endpoint implements. We do not use
// the official SDK because Foreman needs exactly one HTTP shape, we want
// llama.cpp-specific retry semantics (#22072 truncated tool_call args),
// and the SDK pulls a large transitive cone we would rather avoid for
// v0.1.
package oai

import "encoding/json"

// Role names the chat-completions message role. We use the OpenAI v1
// values verbatim; llama.cpp's OAI surface accepts the same set.
type Role string

const (
	// RoleSystem is the system message that sets the agent's persona +
	// tool-calling conventions. Emitted once at loop start.
	RoleSystem Role = "system"
	// RoleUser is the prompt the loop builds from the AgenticTask payload.
	RoleUser Role = "user"
	// RoleAssistant is the model's reply. Content may be empty when the
	// reply consists solely of ToolCalls.
	RoleAssistant Role = "assistant"
	// RoleTool carries a tool execution result back to the model. Each
	// tool message names the tool_call_id it responds to.
	RoleTool Role = "tool"
)

// Message is a single chat-completions message. Field semantics by role:
//
//	system, user   : Content carries text. Other fields empty.
//	assistant      : Content optional (often empty when ToolCalls are
//	                 present); ToolCalls drives the next turn's work.
//	tool           : Content carries the tool result (typically a JSON
//	                 string). ToolCallID names the assistant call this
//	                 result responds to; Name is the tool's function name.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is what the model emits when it wants a tool to run. The
// Arguments field is a JSON-encoded object that the tool itself parses;
// the OAI spec wraps it in a string rather than nesting it as JSON so
// the wire format stays a flat string regardless of arg shape.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the name+arguments envelope inside a ToolCall.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool advertises a callable function to the model.
type Tool struct {
	Type     string        `json:"type"` // always "function"
	Function ToolSchemaDef `json:"function"`
}

// ToolSchemaDef is the function-shaped advertisement: name, description,
// and the JSONSchema for the arguments object. Tools provide their own
// ToolSchemaDef via the registry; the loop assembles the []Tool slice
// for each chat-completions request.
type ToolSchemaDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ChatRequest is the request body for POST /v1/chat/completions. v0.1
// is non-streaming; the loop pulls a complete response per turn.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	Stream      bool      `json:"stream"`
}

// ChatResponse is the relevant subset of the response body. We do not
// stream so there is only one choice in v0.1; we do not track usage.
type ChatResponse struct {
	ID      string   `json:"id"`
	Choices []Choice `json:"choices"`
}

// Choice is one of the alternatives in ChatResponse.Choices.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}
