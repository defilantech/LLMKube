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

	// ReasoningContent is the thinking trace hybrid-reasoning models
	// emit alongside (or instead of) content when the server's
	// reasoning-format separates it out (llama.cpp default). Preserved
	// in transcripts for archaeology; stripped from the wire by the
	// loop before the next request, mirroring how chat templates drop
	// think blocks from history (#650).
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// MarshalJSON serializes a Message such that non-assistant roles always
// carry a content field (even when the content is the empty string),
// while assistant messages keep the existing omitempty semantics so
// {"role":"assistant","tool_calls":[...]} stays valid (no awkward
// "content":"" alongside tool_calls).
//
// The OpenAI v1 chat-completions spec requires content on system,
// user, and tool roles. llama.cpp's OAI surface accepts the field
// either missing or empty; that's why this never bit Foreman against
// Carnice or other Qwen-family backends. Stricter implementations
// (Devstral / Mistral / DeepSeek and the OpenAI API itself) reject
// HTTP 400 with `"All non-assistant messages must contain 'content'"`
// when the field is absent. Fixes #556.
func (m Message) MarshalJSON() ([]byte, error) {
	if m.Role == RoleAssistant {
		// Assistant: content may legitimately be omitted when the
		// message is purely tool_calls. Fall back to the default
		// omitempty behavior by serializing through a type alias
		// (the alias drops MarshalJSON so json.Marshal uses the
		// struct tags directly).
		type assistantMessage Message
		return json.Marshal(assistantMessage(m))
	}
	// system / user / tool: always emit content, even when empty.
	out := struct {
		Role       Role       `json:"role"`
		Content    string     `json:"content"`
		ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
		ToolCallID string     `json:"tool_call_id,omitempty"`
		Name       string     `json:"name,omitempty"`
	}{
		Role:       m.Role,
		Content:    m.Content,
		ToolCalls:  m.ToolCalls,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	}
	return json.Marshal(out)
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

// ChatRequest is the request body for POST /v1/chat/completions.
//
// The client always sets Stream=true on the wire: thinking-trace models
// like Carnice generate enormous reasoning blocks before producing the
// actual tool call, and llama.cpp buffers the entire completion before
// sending response headers in non-streaming mode -- which makes
// http.Client.Timeout fire on long completions even though the model
// is doing real work. Streaming sends headers immediately and lets the
// caller's context.Context govern the overall lifetime.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	// Stream is set by the client just before the wire send; callers
	// do not need to populate it.
	Stream bool `json:"stream"`
}

// ChatResponse is the relevant subset of the response body. The Client
// always reads streamed chunks off the wire and aggregates them into
// this shape so the rest of the loop sees a single complete response.
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

// ChatChunk is a single Server-Sent Events frame on the chat-completions
// stream. The OpenAI SSE format wraps an unwrapped JSON object per
// `data:` line; this struct is what the line decodes into.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Choices []ChoiceDelta `json:"choices"`
}

// ChoiceDelta is the streamed-piece counterpart to Choice. The Delta
// field carries incremental content / tool-call fragments; FinishReason
// is empty (omitted) until the model's final chunk for this choice.
type ChoiceDelta struct {
	Index        int          `json:"index"`
	Delta        MessageDelta `json:"delta"`
	FinishReason string       `json:"finish_reason"`
}

// MessageDelta is the streamed-piece counterpart to Message. Each field
// arrives in pieces across chunks: Role on the first chunk, Content
// accumulating as tokens are generated, and ToolCalls arriving with
// their Function.Arguments streamed in fragments.
type MessageDelta struct {
	Role             Role            `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCallDelta `json:"tool_calls,omitempty"`
}

// ToolCallDelta is the streamed-piece counterpart to ToolCall. The
// Index field identifies which tool call this fragment belongs to
// (the model may emit multiple parallel tool calls; aggregation keys
// by Index). ID + Type + Function.Name arrive on the first fragment;
// Function.Arguments arrives in pieces across subsequent fragments.
type ToolCallDelta struct {
	Index    int                   `json:"index"`
	ID       string                `json:"id,omitempty"`
	Type     string                `json:"type,omitempty"`
	Function ToolCallFunctionDelta `json:"function"`
}

// ToolCallFunctionDelta mirrors ToolCallFunction but with all fields
// optional / streamable.
type ToolCallFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
