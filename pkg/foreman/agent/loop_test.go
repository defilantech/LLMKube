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

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// scriptedOAIServer returns canned chat-completions responses in
// sequence. Helpful for driving the loop through multi-turn flows
// without standing up a real model.
func scriptedOAIServer(t *testing.T, bodies []string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(calls.Add(1) - 1)
		if i >= len(bodies) {
			t.Errorf("scriptedOAIServer: %d-th call exceeds script (%d)", i+1, len(bodies))
			http.Error(w, "script exhausted", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bodies[i]))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// fakeRegistry records every Dispatch call and returns canned ToolResult
// or errors keyed by tool name. Unknown tools return an error so we
// also exercise the loop's error-as-tool-message recovery path.
type fakeRegistry struct {
	schemas    []oai.Tool
	results    map[string]*ToolResult
	dispatched []string
}

func (r *fakeRegistry) Schemas() []oai.Tool { return r.schemas }

func (r *fakeRegistry) Dispatch(_ context.Context, name string, _ json.RawMessage) (*ToolResult, error) {
	r.dispatched = append(r.dispatched, name)
	res, ok := r.results[name]
	if !ok {
		return nil, fmt.Errorf("fakeRegistry: unknown tool %q", name)
	}
	return res, nil
}

const toolCallReadFile = `{
  "id": "t1",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-rf",
        "type": "function",
        "function": {"name": "read_file", "arguments": "{\"path\":\"README.md\"}"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

const toolCallSubmitGo = `{
  "id": "t2",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-sr",
        "type": "function",
        "function": {"name": "submit_result", "arguments": "{\"verdict\":\"GO\"}"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

const toolCallUnknownTool = `{
  "id": "t3",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-unk",
        "type": "function",
        "function": {"name": "noooope", "arguments": "{}"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

const assistantNoCalls = `{
  "id": "t4",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "I'm done."
    },
    "finish_reason": "stop"
  }]
}`

// newTestLoop builds a Loop pointed at srv with the given registry. We
// bypass retry by setting maxRetries=0 (the OAI client surface is
// tested separately).
func newTestLoop(srv *httptest.Server, reg ToolRegistry) *Loop {
	client := oai.New(srv.URL+"/v1", time.Second, 0)
	return NewLoop(client, reg, nil)
}

func TestLoop_HappyPath_TerminalSubmitResult(t *testing.T) {
	srv, calls := scriptedOAIServer(t, []string{toolCallReadFile, toolCallSubmitGo})
	reg := &fakeRegistry{
		results: map[string]*ToolResult{
			"read_file":     {Output: map[string]any{"content": "# README\n"}},
			"submit_result": {Terminal: true, Verdict: "GO", Summary: "ok"},
		},
	}
	loop := newTestLoop(srv, reg)
	res, err := loop.Run(context.Background(), LoopConfig{
		Model: "test", SystemPrompt: "be brief", UserPrompt: "do the thing", MaxTurns: 5,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Terminal == nil || res.Terminal.Verdict != "GO" {
		t.Errorf("expected terminal with GO, got %+v", res.Terminal)
	}
	if res.Turns != 2 {
		t.Errorf("turns: want 2 got %d", res.Turns)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("OAI calls: want 2 got %d", got)
	}
	// Transcript should contain: system, user, assistant#1, tool result#1,
	// assistant#2, tool result#2 = 6 messages.
	if len(res.Transcript) != 6 {
		t.Errorf("transcript len: want 6 got %d", len(res.Transcript))
	}
	if res.Transcript[0].Role != oai.RoleSystem || res.Transcript[1].Role != oai.RoleUser {
		t.Errorf("transcript prefix wrong: %s, %s", res.Transcript[0].Role, res.Transcript[1].Role)
	}
}

func TestLoop_MaxTurnsExhausted(t *testing.T) {
	// Three calls all return read_file. With MaxTurns=3 the loop should
	// hit the limit and return ErrMaxTurnsExhausted, transcript intact.
	srv, _ := scriptedOAIServer(t, []string{toolCallReadFile, toolCallReadFile, toolCallReadFile})
	reg := &fakeRegistry{
		results: map[string]*ToolResult{
			"read_file": {Output: map[string]any{"content": "# README\n"}},
		},
	}
	loop := newTestLoop(srv, reg)
	res, err := loop.Run(context.Background(), LoopConfig{Model: "test", MaxTurns: 3})
	if !errors.Is(err, ErrMaxTurnsExhausted) {
		t.Fatalf("expected ErrMaxTurnsExhausted, got %v", err)
	}
	if res.Turns != 3 {
		t.Errorf("turns: want 3 got %d", res.Turns)
	}
	if res.Terminal != nil {
		t.Errorf("did not expect terminal: %+v", res.Terminal)
	}
}

func TestLoop_UnknownToolBecomesToolErrorMessage(t *testing.T) {
	srv, _ := scriptedOAIServer(t, []string{toolCallUnknownTool, toolCallSubmitGo})
	reg := &fakeRegistry{
		results: map[string]*ToolResult{
			"submit_result": {Terminal: true, Verdict: "GO"},
		},
	}
	loop := newTestLoop(srv, reg)
	res, err := loop.Run(context.Background(), LoopConfig{Model: "test", MaxTurns: 5})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Turn 1's tool message must contain the error string for the model
	// to recover from.
	var sawErr bool
	for _, m := range res.Transcript {
		if m.Role == oai.RoleTool && m.Name == "noooope" {
			if want := `"error"`; len(m.Content) > 0 && contains(m.Content, want) {
				sawErr = true
			}
		}
	}
	if !sawErr {
		t.Errorf("expected a tool message with error content; transcript=%v", res.Transcript)
	}
	if res.Terminal == nil {
		t.Errorf("expected terminal from turn 2")
	}
}

func TestLoop_AssistantNoToolCallsIsError(t *testing.T) {
	srv, _ := scriptedOAIServer(t, []string{assistantNoCalls})
	reg := &fakeRegistry{}
	loop := newTestLoop(srv, reg)
	_, err := loop.Run(context.Background(), LoopConfig{Model: "test", MaxTurns: 3})
	if !errors.Is(err, ErrAssistantNoToolCalls) {
		t.Errorf("expected ErrAssistantNoToolCalls, got %v", err)
	}
}

func TestLoop_TranscriptPreservedOnError(t *testing.T) {
	// Server fails after a successful first turn; the loop must surface
	// the error but keep the transcript built so far so the executor
	// can persist what happened.
	srv, _ := scriptedOAIServer(t, []string{toolCallReadFile})
	reg := &fakeRegistry{
		results: map[string]*ToolResult{
			"read_file": {Output: map[string]any{"content": "ok"}},
		},
	}
	// Wrap the script in a handler that fails on the second request.
	mux := http.NewServeMux()
	var n atomic.Int64
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(toolCallReadFile))
			return
		}
		http.Error(w, "kaboom", http.StatusInternalServerError)
	})
	wrap := httptest.NewServer(mux)
	defer wrap.Close()
	_ = srv // keep the helper warm; this test uses a custom server

	client := oai.New(wrap.URL+"/v1", time.Second, 0)
	loop := NewLoop(client, reg, nil)
	res, err := loop.Run(context.Background(), LoopConfig{Model: "test", MaxTurns: 5})
	if err == nil {
		t.Fatalf("expected error on second turn")
	}
	if len(res.Transcript) < 4 {
		t.Errorf("transcript should include system+user+assistant1+tool1; got %d msgs", len(res.Transcript))
	}
	if res.Terminal != nil {
		t.Errorf("did not expect terminal: %+v", res.Terminal)
	}
}

// contains is a tiny strings.Contains avoiding the extra import for one use.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
