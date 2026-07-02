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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// scriptedOAIServer returns canned chat-completions responses in
// sequence. Helpful for driving the loop through multi-turn flows
// without standing up a real model. The canned bodies are kept in the
// readable ChatResponse JSON form; this helper converts each to the
// SSE wire format the streaming client expects.
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
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatJSONToSSE(t, bodies[i])))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// chatJSONToSSE wraps a readable ChatResponse JSON fixture into the
// SSE event stream the new streaming client reads. One chunk per
// choice, then `data: [DONE]`. Tool calls collapse into a single
// fragment per call.
func chatJSONToSSE(t *testing.T, body string) string {
	t.Helper()
	var parsed oai.ChatResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("chatJSONToSSE: fixture is not a ChatResponse JSON: %v\nbody=%q", err, body)
	}
	var sb strings.Builder
	for _, ch := range parsed.Choices {
		chunk := oai.ChatChunk{
			ID:     parsed.ID,
			Object: "chat.completion.chunk",
			Choices: []oai.ChoiceDelta{
				{
					Index: ch.Index,
					Delta: oai.MessageDelta{
						Role:             ch.Message.Role,
						Content:          ch.Message.Content,
						ReasoningContent: ch.Message.ReasoningContent,
					},
					FinishReason: ch.FinishReason,
				},
			},
		}
		for j, tc := range ch.Message.ToolCalls {
			chunk.Choices[0].Delta.ToolCalls = append(
				chunk.Choices[0].Delta.ToolCalls,
				oai.ToolCallDelta{
					Index:    j,
					ID:       tc.ID,
					Type:     tc.Type,
					Function: oai.ToolCallFunctionDelta{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
				},
			)
		}
		out, err := json.Marshal(chunk)
		if err != nil {
			t.Fatalf("chatJSONToSSE: marshal chunk: %v", err)
		}
		sb.WriteString("data: ")
		sb.Write(out)
		sb.WriteString("\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
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

// blockingOAIServer never sends a response; it holds each request open
// until the client's context is cancelled. Simulates a turn whose
// prompt-eval is so slow that the loop's wall-clock budget fires before
// any headers arrive (the #532 failure shape).
func blockingOAIServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the request open until the client gives up, but bound the
		// block so srv.Close() in t.Cleanup never hangs if the client-side
		// cancellation is slow to propagate to the server context.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLoop_LoopBudgetExhausted_GracefulTerminal(t *testing.T) {
	// A turn that never returns before the loop-wide budget fires must
	// end the loop gracefully: an INCOMPLETE terminal with the transcript
	// preserved, NOT a hard error that the executor maps to a retry-less
	// ExecutorError that kills the whole AgenticTask. Regression for #532.
	srv := blockingOAIServer(t)
	// Generous per-request timeout so the loop budget, not the per-turn
	// header timeout, is the thing that fires.
	client := oai.New(srv.URL+"/v1", 10*time.Second, 0)
	reg := &fakeRegistry{results: map[string]*ToolResult{}}
	loop := NewLoop(client, reg, nil)

	res, err := loop.Run(context.Background(), LoopConfig{
		Model:        "test",
		SystemPrompt: "sys",
		UserPrompt:   "go",
		MaxTurns:     50,
		LoopBudget:   80 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("expected graceful nil error on budget exhaustion, got %v", err)
	}
	if res.Terminal == nil || res.Terminal.Verdict != "INCOMPLETE" {
		t.Fatalf("expected INCOMPLETE terminal, got %+v", res.Terminal)
	}
	if got := res.Terminal.Extra["outcome"]; got != LoopBudgetOutcome {
		t.Errorf("outcome: want %q got %v", LoopBudgetOutcome, got)
	}
	if len(res.Transcript) < 2 || res.Transcript[0].Role != oai.RoleSystem {
		t.Errorf("transcript not preserved: %+v", res.Transcript)
	}
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

// TestLoop_VerifyTerminal_VetoThenAccept covers the coder gate feedback
// loop (#749): the gate vetoes the first GO with feedback, the loop
// injects that feedback as a user message and continues, and the gate
// accepts the next GO so the terminal stands.
func TestLoop_VerifyTerminal_VetoThenAccept(t *testing.T) {
	srv, calls := scriptedOAIServer(t, []string{toolCallSubmitGo, toolCallSubmitGo})
	reg := &fakeRegistry{
		results: map[string]*ToolResult{
			"submit_result": {Terminal: true, Verdict: "GO", Summary: "ok"},
		},
	}
	loop := newTestLoop(srv, reg)
	const feedback = "gate: gofmt reported internal/foo.go; fix it and resubmit"
	verifyCalls := 0
	res, err := loop.Run(context.Background(), LoopConfig{
		Model: "test", UserPrompt: "go", MaxTurns: 5, MaxVerifyRetries: 2,
		VerifyTerminal: func(_ context.Context, _ *ToolResult, _ []oai.Message) (bool, string, error) {
			verifyCalls++
			if verifyCalls == 1 {
				return false, feedback, nil
			}
			return true, "", nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Terminal == nil || res.Terminal.Verdict != "GO" {
		t.Fatalf("want GO terminal after the gate accepts, got %+v", res.Terminal)
	}
	if verifyCalls != 2 {
		t.Errorf("verify calls: want 2 got %d", verifyCalls)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("OAI calls: want 2 (veto consumed a turn) got %d", got)
	}
	found := false
	for _, m := range res.Transcript {
		if m.Role == oai.RoleUser && m.Content == feedback {
			found = true
		}
	}
	if !found {
		t.Errorf("gate feedback was not injected as a user message")
	}
}

// TestLoop_VerifyTerminal_BudgetExhaustedDowngrades covers the case where
// the gate never passes: after MaxVerifyRetries fix attempts the terminal
// is downgraded to INCOMPLETE so a branch that still fails fmt/vet/build/
// lint cannot land as a clean GO.
func TestLoop_VerifyTerminal_BudgetExhaustedDowngrades(t *testing.T) {
	srv, _ := scriptedOAIServer(t, []string{toolCallSubmitGo, toolCallSubmitGo, toolCallSubmitGo})
	reg := &fakeRegistry{
		results: map[string]*ToolResult{
			"submit_result": {Terminal: true, Verdict: "GO", Summary: "ok"},
		},
	}
	loop := newTestLoop(srv, reg)
	res, err := loop.Run(context.Background(), LoopConfig{
		Model: "test", UserPrompt: "go", MaxTurns: 5, MaxVerifyRetries: 2,
		VerifyTerminal: func(_ context.Context, _ *ToolResult, _ []oai.Message) (bool, string, error) {
			return false, "gate: still failing", nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Terminal == nil || res.Terminal.Verdict != "INCOMPLETE" {
		t.Fatalf("want INCOMPLETE downgrade, got %+v", res.Terminal)
	}
	if res.Terminal.Extra["outcome"] != CoderGateFailedOutcome {
		t.Errorf("want outcome %s, got %v", CoderGateFailedOutcome, res.Terminal.Extra["outcome"])
	}
	if res.Terminal.Extra["gateAttempts"] != 2 {
		t.Errorf("want gateAttempts 2, got %v", res.Terminal.Extra["gateAttempts"])
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

const toolCallWriteFile = `{
  "id": "tw",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-wf",
        "type": "function",
        "function": {"name": "write_file", "arguments": "{\"path\":\"x.go\",\"content\":\"package x\"}"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

// sixToolSchemas returns the full production tool advertisement set so a
// test can assert which subset the loop advertises on a given turn.
func sixToolSchemas() []oai.Tool {
	names := []string{"read_file", "write_file", "str_replace", "grep", "bash", "submit_result"}
	out := make([]oai.Tool, 0, len(names))
	for _, n := range names {
		out = append(out, oai.Tool{Type: "function", Function: oai.ToolSchemaDef{Name: n}})
	}
	return out
}

// recordingScriptedServer behaves like scriptedOAIServer but also records,
// per request, the set of tool names the loop advertised in that request's
// `tools` array. advertised[i] is the sorted-ish slice for the i-th call.
func recordingScriptedServer(t *testing.T, bodies []string) (*httptest.Server, *[][]string) {
	t.Helper()
	var calls atomic.Int64
	advertised := make([][]string, len(bodies))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(calls.Add(1) - 1)
		if i >= len(bodies) {
			t.Errorf("recordingScriptedServer: %d-th call exceeds script (%d)", i+1, len(bodies))
			http.Error(w, "script exhausted", http.StatusInternalServerError)
			return
		}
		var req oai.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("recordingScriptedServer: decode request: %v", err)
		}
		names := make([]string, 0, len(req.Tools))
		for _, tl := range req.Tools {
			names = append(names, tl.Function.Name)
		}
		advertised[i] = names
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatJSONToSSE(t, bodies[i])))
	}))
	t.Cleanup(srv.Close)
	return srv, &advertised
}

func hasTool(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// toolCallStrReplaceBad is a str_replace whose registry result is an error
// (e.g. "old_string found 0 times") -- a failed edit attempt.
const toolCallStrReplaceBad = `{
  "id": "tsr",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-sr-bad",
        "type": "function",
        "function": {
          "name": "str_replace",
          "arguments": "{\"path\":\"x.go\",\"old_string\":\"nope\",\"new_string\":\"y\"}"
        }
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

func TestLoop_EditFreeStreak_ForcingPhaseDropsExplorationKeepsRead(t *testing.T) {
	// When EditFreeStreak nudges, the forcing phase drops grep+bash but
	// KEEPS read_file (so the model can fetch exact text to fix an edit).
	// The phase lifts only when an edit actually lands. Script: two reads
	// trip EditFreeTurnsLimit=2; turn 3 (restricted) the model reads to get
	// the text; turn 4 it writes successfully (lifting the phase); turn 5
	// the full tool set is restored and it submits.
	srv, advertised := recordingScriptedServer(t, []string{
		toolCallReadFile, toolCallReadFile, toolCallReadFile, toolCallWriteFile, toolCallSubmitGo,
	})
	reg := &fakeRegistry{
		schemas: sixToolSchemas(),
		results: map[string]*ToolResult{
			"read_file":     {Output: map[string]any{"content": "x"}},
			"write_file":    {Output: map[string]any{"ok": true}},
			"submit_result": {Terminal: true, Verdict: "GO", Summary: "done"},
		},
	}
	loop := newTestLoop(srv, reg)
	res, err := loop.Run(context.Background(), LoopConfig{
		Model: "test", MaxTurns: 10,
		Progress: ProgressConfig{EditFreeTurnsLimit: 2},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Terminal == nil || res.Terminal.Verdict != "GO" {
		t.Fatalf("expected GO terminal, got %+v", res.Terminal)
	}
	adv := *advertised
	if len(adv) < 5 {
		t.Fatalf("expected 5 recorded requests, got %d", len(adv))
	}
	// Turns 1 and 2: full tool set.
	for _, turn := range []int{0, 1} {
		if len(adv[turn]) != 6 {
			t.Errorf("turn %d: expected full 6-tool set, got %v", turn+1, adv[turn])
		}
	}
	// Turns 3 and 4 (forcing phase): grep+bash gone, read_file kept.
	for _, turn := range []int{2, 3} {
		r := adv[turn]
		if hasTool(r, "grep") || hasTool(r, "bash") {
			t.Errorf("turn %d: expected grep/bash removed, got %v", turn+1, r)
		}
		if !hasTool(r, "read_file") || !hasTool(r, "write_file") ||
			!hasTool(r, "str_replace") || !hasTool(r, "submit_result") {
			t.Errorf("turn %d: expected read_file + edit tools present, got %v", turn+1, r)
		}
	}
	// Turn 5: phase lifted after the successful write; full set restored.
	if len(adv[4]) != 6 {
		t.Errorf("turn 5: expected full tool set restored, got %v", adv[4])
	}
}

func TestLoop_EditFreeStreak_FailedEditDoesNotLiftPhase(t *testing.T) {
	// A str_replace that ERRORS ("old_string found 0 times") must not count
	// as progress: the phase stays armed and the model gets another
	// restricted turn to recover, instead of escaping back to exploration.
	// Script: two reads trip the limit; then three failed str_replace
	// attempts exhaust the 3-turn restricted budget -> force-terminate.
	srv, _ := recordingScriptedServer(t, []string{
		toolCallReadFile, toolCallReadFile,
		toolCallStrReplaceBad, toolCallStrReplaceBad, toolCallStrReplaceBad,
	})
	reg := &fakeRegistry{
		schemas: sixToolSchemas(),
		results: map[string]*ToolResult{
			"read_file": {Output: map[string]any{"content": "x"}},
			// str_replace absent from results -> Dispatch returns an error,
			// modeling a failed edit (wrong old_string).
		},
	}
	loop := newTestLoop(srv, reg)
	res, err := loop.Run(context.Background(), LoopConfig{
		Model: "test", MaxTurns: 10,
		Progress: ProgressConfig{EditFreeTurnsLimit: 2},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Terminal == nil {
		t.Fatalf("expected force-terminate envelope, got nil")
	}
	if got := res.Terminal.Extra["signal"]; got != "EditFreeStreak" {
		t.Errorf("signal: want EditFreeStreak got %v", got)
	}
	if got := res.Terminal.Extra["outcome"]; got != StuckLoopOutcome {
		t.Errorf("outcome: want %q got %v", StuckLoopOutcome, got)
	}
}

func TestLoop_EditFreeStreak_TerminatesAfterRestrictedBudget(t *testing.T) {
	// A model that keeps reading inside the forcing phase (never editing)
	// is force-terminated once the restricted-turn budget is spent, not on
	// the first restricted turn. With EditFreeTurnsLimit=2 the nudge fires
	// after turn 2; turns 3,4,5 are restricted reads; the run terminates at
	// turn 5 (the 3rd restricted turn).
	srv, _ := recordingScriptedServer(t, []string{
		toolCallReadFile, toolCallReadFile, toolCallReadFile, toolCallReadFile, toolCallReadFile,
	})
	reg := &fakeRegistry{
		schemas: sixToolSchemas(),
		results: map[string]*ToolResult{
			"read_file": {Output: map[string]any{"content": "x"}},
		},
	}
	loop := newTestLoop(srv, reg)
	res, err := loop.Run(context.Background(), LoopConfig{
		Model: "test", MaxTurns: 10,
		Progress: ProgressConfig{EditFreeTurnsLimit: 2},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Terminal == nil || res.Terminal.Extra["signal"] != "EditFreeStreak" {
		t.Fatalf("expected EditFreeStreak force-terminate, got %+v", res.Terminal)
	}
	if res.Turns != 5 {
		t.Errorf("expected termination at turn 5 (nudge@2 + 3 restricted turns), got %d", res.Turns)
	}
}

func TestLoop_FinalTurnsForceSubmit(t *testing.T) {
	// In the last forceSubmitFinalTurns (3) turns before MaxTurns, the loop
	// advertises submit_result ONLY, so a non-concluding agent (e.g. a reviewer
	// with EditFreeStreak disabled) is forced to produce a terminal verdict
	// instead of exhausting MaxTurns with no result. MaxTurns=8 -> final window
	// is turns 6,7,8. The model reads 5 turns, then submits on turn 6 (when only
	// submit_result is available).
	srv, advertised := recordingScriptedServer(t, []string{
		toolCallReadFile, toolCallReadFile, toolCallReadFile,
		toolCallReadFile, toolCallReadFile, toolCallSubmitGo,
	})
	reg := &fakeRegistry{
		schemas: sixToolSchemas(),
		results: map[string]*ToolResult{
			"read_file":     {Output: map[string]any{"content": "x"}},
			"submit_result": {Terminal: true, Verdict: "GO", Summary: "done"},
		},
	}
	loop := newTestLoop(srv, reg)
	res, err := loop.Run(context.Background(), LoopConfig{Model: "test", MaxTurns: 8})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Terminal == nil || res.Terminal.Verdict != "GO" {
		t.Fatalf("expected GO terminal, got %+v", res.Terminal)
	}
	adv := *advertised
	if len(adv) < 6 {
		t.Fatalf("expected >=6 recorded requests, got %d", len(adv))
	}
	for _, turn := range []int{0, 1, 2, 3, 4} {
		if len(adv[turn]) != 6 {
			t.Errorf("turn %d: expected full 6-tool set, got %v", turn+1, adv[turn])
		}
	}
	if len(adv[5]) != 1 || adv[5][0] != "submit_result" {
		t.Errorf("turn 6: expected [submit_result] only, got %v", adv[5])
	}
	var sawNudge bool
	for _, m := range res.Transcript {
		if m.Role == oai.RoleUser && strings.Contains(m.Content, "ONLY tool available") {
			sawNudge = true
		}
	}
	if !sawNudge {
		t.Errorf("expected ForceSubmitMessage in transcript")
	}
}

func TestLoop_AssistantNoToolCalls_RetriesThenErrors(t *testing.T) {
	// A model that replies with prose and no tool_calls cannot make
	// forward progress, but rather than failing the whole task on the
	// first such turn the loop appends a forceful corrective and gives the
	// model a bounded number of chances to recover. Three consecutive
	// no-tool-call turns (initial + 2 retries) exhausts the budget and
	// surfaces ErrAssistantNoToolCalls.
	srv, calls := scriptedOAIServer(t, []string{assistantNoCalls, assistantNoCalls, assistantNoCalls})
	reg := &fakeRegistry{}
	loop := newTestLoop(srv, reg)
	res, err := loop.Run(context.Background(), LoopConfig{
		Model: "test", MaxTurns: 10, MaxNoToolCallRetries: 2,
	})
	if !errors.Is(err, ErrAssistantNoToolCalls) {
		t.Fatalf("expected ErrAssistantNoToolCalls after retries, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("OAI calls: want 3 (initial + 2 retries) got %d", got)
	}
	// The corrective nudges must be persisted so the trajectory shows the
	// harness tried to recover before giving up.
	var nudges int
	for _, m := range res.Transcript {
		if m.Role == oai.RoleUser && m.Content == NoToolCallNudgeMessage() {
			nudges++
		}
	}
	if nudges != 2 {
		t.Errorf("expected 2 corrective nudges in transcript, got %d", nudges)
	}
}

func TestLoop_AssistantNoToolCalls_RecoversAfterNudge(t *testing.T) {
	// The model narrates with no tool call on turn 1, then (after the
	// corrective nudge) calls submit_result on turn 2. The loop must
	// recover and finish with the model's terminal rather than fail. This
	// is the common local-model failure: the model states its conclusion
	// as prose instead of calling submit_result.
	srv, calls := scriptedOAIServer(t, []string{assistantNoCalls, toolCallSubmitGo})
	reg := &fakeRegistry{
		results: map[string]*ToolResult{
			"submit_result": {Terminal: true, Verdict: "GO", Summary: "done"},
		},
	}
	loop := newTestLoop(srv, reg)
	res, err := loop.Run(context.Background(), LoopConfig{
		Model: "test", MaxTurns: 10, MaxNoToolCallRetries: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Terminal == nil || res.Terminal.Verdict != "GO" {
		t.Errorf("expected GO terminal after recovery, got %+v", res.Terminal)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("OAI calls: want 2 got %d", got)
	}
	var sawNudge bool
	for _, m := range res.Transcript {
		if m.Role == oai.RoleUser && m.Content == NoToolCallNudgeMessage() {
			sawNudge = true
		}
	}
	if !sawNudge {
		t.Errorf("expected corrective nudge in transcript before recovery")
	}
}

func TestLoop_AssistantNoToolCalls_StreakResetsOnToolCall(t *testing.T) {
	// A single mid-run narration followed by normal tool-calling must not
	// accumulate toward the retry budget: the streak resets after any
	// successful tool turn. Sequence: narrate, recover (read_file), narrate
	// again, recover (submit_result). With a budget of 1 this would error
	// if the streak did not reset; it must instead finish cleanly.
	srv, _ := scriptedOAIServer(t, []string{
		assistantNoCalls, toolCallReadFile, assistantNoCalls, toolCallSubmitGo,
	})
	reg := &fakeRegistry{
		results: map[string]*ToolResult{
			"read_file":     {Output: map[string]any{"content": "# README\n"}},
			"submit_result": {Terminal: true, Verdict: "GO", Summary: "done"},
		},
	}
	loop := newTestLoop(srv, reg)
	res, err := loop.Run(context.Background(), LoopConfig{
		Model: "test", MaxTurns: 10, MaxNoToolCallRetries: 1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Terminal == nil || res.Terminal.Verdict != "GO" {
		t.Errorf("expected GO terminal, got %+v", res.Terminal)
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
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(chatJSONToSSE(t, toolCallReadFile)))
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

// TestLoop_StuckLoopDetectorForceTerminates exercises the #544 wire-in:
// the model rapid-fires the same read_file call over and over. The
// progress monitor nudges on the threshold-th call, then force-
// terminates on the next still-stuck turn. Total turns should be
// threshold + 1 (nudge) + 1 (escalation) at most, well below MaxTurns.
func TestLoop_StuckLoopDetectorForceTerminates(t *testing.T) {
	// 10 identical read_file responses; threshold=3 means we'll nudge
	// at turn 3 and force-terminate at turn 4. MaxTurns=10 gives the
	// detector room to fire well before the cap so the test is
	// unambiguous about which path terminated the loop.
	script := make([]string, 10)
	for i := range script {
		script[i] = toolCallReadFile
	}
	srv, _ := scriptedOAIServer(t, script)
	reg := &fakeRegistry{
		results: map[string]*ToolResult{
			"read_file": {Output: map[string]any{"content": "# README\n"}},
		},
	}
	loop := newTestLoop(srv, reg)
	res, err := loop.Run(context.Background(), LoopConfig{
		Model:    "test",
		MaxTurns: 10,
		Progress: ProgressConfig{RepeatedToolThreshold: 3},
	})
	// The loop returns nil error on a clean force-terminate: it's a
	// structural outcome (the model is stuck, harness intervened), not
	// an infrastructure failure. Callers distinguish this from a model-
	// emitted terminal by inspecting Terminal.Extra["outcome"].
	if err != nil {
		t.Fatalf("expected nil error on clean force-terminate; got %v", err)
	}
	if res.Terminal == nil {
		t.Fatal("expected synthesized Terminal envelope")
	}
	if res.Terminal.Verdict != "INCOMPLETE" {
		t.Errorf("synthesized verdict: want INCOMPLETE; got %q", res.Terminal.Verdict)
	}
	if got := res.Terminal.Extra["outcome"]; got != StuckLoopOutcome {
		t.Errorf("Extra.outcome: want %q; got %v", StuckLoopOutcome, got)
	}
	if got := res.Terminal.Extra["signal"]; got != "RepeatedToolCall" {
		t.Errorf("Extra.signal: want RepeatedToolCall; got %v", got)
	}
	// Turns should be 4 (3 to nudge, +1 to escalate); definitely < 10.
	if res.Turns > 5 {
		t.Errorf("detector should fire within 5 turns; got %d", res.Turns)
	}
	// Transcript must include the nudge message (synthetic user role)
	// between the nudge turn's tool result and the next assistant turn.
	var sawNudge bool
	for _, m := range res.Transcript {
		if m.Role == oai.RoleUser && strings.Contains(m.Content, "PROGRESS MONITOR") {
			sawNudge = true
			break
		}
	}
	if !sawNudge {
		t.Errorf("nudge message should appear in transcript")
	}
}

// TestLoop_ProgressMonitorDisabledByDefault confirms that a LoopConfig
// with a zero-value Progress field disables detection entirely. Lets
// callers opt out by omitting the field rather than having to spell
// out negative thresholds.
func TestLoop_ProgressMonitorDisabledByDefault(t *testing.T) {
	// Same setup as TestLoop_StuckLoopDetectorForceTerminates but
	// without Progress set. The loop should now hit MaxTurns rather
	// than force-terminate.
	script := make([]string, 5)
	for i := range script {
		script[i] = toolCallReadFile
	}
	srv, _ := scriptedOAIServer(t, script)
	reg := &fakeRegistry{
		results: map[string]*ToolResult{
			"read_file": {Output: map[string]any{"content": "# README\n"}},
		},
	}
	loop := newTestLoop(srv, reg)
	_, err := loop.Run(context.Background(), LoopConfig{Model: "test", MaxTurns: 5})
	if !errors.Is(err, ErrMaxTurnsExhausted) {
		t.Fatalf("expected ErrMaxTurnsExhausted (detector disabled by default); got %v", err)
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

const assistantReasoningOnly = `{
  "id": "t5",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "reasoning_content": "Let me think about which file to read next."
    },
    "finish_reason": "stop"
  }]
}`

func TestLoop_ReasoningOnlyTurn_ContinuesAndRecovers(t *testing.T) {
	// A hybrid-thinking model that spends a turn reasoning without a
	// tool call must get a continuation nudge, not the prose corrective,
	// and the run must recover when the next turn acts (#650).
	srv, calls := scriptedOAIServer(t, []string{assistantReasoningOnly, toolCallSubmitGo})
	reg := &fakeRegistry{results: map[string]*ToolResult{
		"submit_result": {Terminal: true, Verdict: "GO"},
	}}
	loop := newTestLoop(srv, reg)

	res, err := loop.Run(context.Background(), LoopConfig{
		Model: "test", SystemPrompt: "sys", UserPrompt: "go", MaxTurns: 5,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Terminal == nil || res.Terminal.Verdict != "GO" {
		t.Fatalf("expected GO terminal after recovery, got %+v", res.Terminal)
	}
	if calls.Load() != 2 {
		t.Errorf("calls: want 2 got %d", calls.Load())
	}
	var sawNudge, sawReasoning bool
	for _, m := range res.Transcript {
		if m.Role == oai.RoleUser && m.Content == ReasoningOnlyNudgeMessage() {
			sawNudge = true
		}
		if m.Role == oai.RoleAssistant && m.ReasoningContent != "" {
			sawReasoning = true
		}
	}
	if !sawNudge {
		t.Errorf("transcript should contain the reasoning continuation nudge")
	}
	if !sawReasoning {
		t.Errorf("transcript should preserve the model's reasoning_content")
	}
}

func TestLoop_ReasoningOnlyExhausted(t *testing.T) {
	srv, _ := scriptedOAIServer(t, []string{assistantReasoningOnly, assistantReasoningOnly})
	reg := &fakeRegistry{results: map[string]*ToolResult{}}
	loop := newTestLoop(srv, reg)

	_, err := loop.Run(context.Background(), LoopConfig{
		Model: "test", SystemPrompt: "sys", UserPrompt: "go",
		MaxTurns: 10, MaxReasoningOnlyRetries: 1,
	})
	if !errors.Is(err, ErrAssistantReasoningOnly) {
		t.Fatalf("want ErrAssistantReasoningOnly after budget exhaustion, got %v", err)
	}
}

func TestStripReasoningForWire(t *testing.T) {
	transcript := []oai.Message{
		{Role: oai.RoleSystem, Content: "sys"},
		{Role: oai.RoleAssistant, ReasoningContent: "thinking", Content: "answer"},
		{Role: oai.RoleUser, Content: "next"},
	}
	wire := stripReasoningForWire(transcript)
	if wire[1].ReasoningContent != "" {
		t.Errorf("wire copy must strip reasoning_content, got %q", wire[1].ReasoningContent)
	}
	if wire[1].Content != "answer" {
		t.Errorf("wire copy must keep content, got %q", wire[1].Content)
	}
	if transcript[1].ReasoningContent != "thinking" {
		t.Errorf("original transcript must be untouched, got %q", transcript[1].ReasoningContent)
	}
}

// emptyAssistantReply is a chat-completions body where the model returns
// an assistant message with neither content nor tool_calls — the shape
// that triggers #935: the loop appends it to history, the next turn's
// request is rejected with 400 "Assistant message must contain either
// 'content' or 'tool_calls'", and the task cascade-fails.
const emptyAssistantReply = `{"choices":[{"index":0,"message":{"role":"assistant"},"finish_reason":"stop"}]}`

// TestLoop_EmptyAssistantReply_SubstitutesPlaceholder covers #935: when
// the model returns an assistant message with no content and no
// tool_calls, the loop must not append it verbatim to the transcript.
// Instead it substitutes a placeholder so the next turn's request stays
// valid against strict backends (llama.cpp, Devstral, OpenAI). The loop
// should then treat the turn as a no-tool-call and apply its existing
// corrective-nudge path.
func TestLoop_EmptyAssistantReply_SubstitutesPlaceholder(t *testing.T) {
	// Turn 1: empty assistant reply. Turn 2: a real submit_result so the
	// loop can finish cleanly (proving the empty reply did not poison
	// history).
	srv, calls := scriptedOAIServer(t, []string{emptyAssistantReply, toolCallSubmitGo})
	reg := &fakeRegistry{
		results: map[string]*ToolResult{
			"submit_result": {Terminal: true, Verdict: "GO", Summary: "ok"},
		},
	}
	loop := newTestLoop(srv, reg)
	res, err := loop.Run(context.Background(), LoopConfig{
		Model: "test", SystemPrompt: "sys", UserPrompt: "go", MaxTurns: 5,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Terminal == nil || res.Terminal.Verdict != "GO" {
		t.Errorf("expected GO terminal after recovery, got %+v", res.Terminal)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("OAI calls: want 2 (empty reply + submit) got %d", got)
	}
	// The empty assistant message must have been replaced with a
	// placeholder, not left empty.
	var sawPlaceholder bool
	for _, m := range res.Transcript {
		if m.Role == oai.RoleAssistant && m.Content != "" {
			if strings.Contains(m.Content, "empty response from model") {
				sawPlaceholder = true
			}
		}
	}
	if !sawPlaceholder {
		t.Errorf(
			"expected placeholder content on the assistant message "+
				"that was originally empty; transcript=%+v", res.Transcript)
	}
}

// TestRunOneTurn_EmptyAssistantReply_SubstitutesPlaceholder exercises
// runOneTurn directly (the gate requires a test referencing the changed
// function by name) and asserts the same #935 behavior as the
// end-to-end TestLoop_EmptyAssistantReply_SubstitutesPlaceholder.
func TestRunOneTurn_EmptyAssistantReply_SubstitutesPlaceholder(t *testing.T) {
	srv, _ := scriptedOAIServer(t, []string{emptyAssistantReply})
	loop := newTestLoop(srv, &fakeRegistry{results: map[string]*ToolResult{}})
	res := &LoopResult{
		Transcript: []oai.Message{
			{Role: oai.RoleSystem, Content: "sys"},
			{Role: oai.RoleUser, Content: "go"},
		},
	}
	_, err := loop.runOneTurn(context.Background(), LoopConfig{Model: "test"}, sixToolSchemas(), res)
	// The placeholder makes the message non-empty, so runOneTurn treats
	// it as a no-tool-call turn and returns ErrAssistantNoToolCalls.
	if !errors.Is(err, ErrAssistantNoToolCalls) {
		t.Fatalf("runOneTurn: expected ErrAssistantNoToolCalls, got %v", err)
	}
	// The assistant message appended by runOneTurn must carry the
	// placeholder, not be empty (the #935 guard).
	last := res.Transcript[len(res.Transcript)-1]
	if last.Role != oai.RoleAssistant {
		t.Fatalf("last message role: want assistant got %q", last.Role)
	}
	if strings.TrimSpace(last.Content) == "" {
		t.Errorf("runOneTurn must substitute placeholder for empty assistant reply; got empty content")
	}
	if !strings.Contains(last.Content, "empty response from model") {
		t.Errorf("placeholder content not found in: %q", last.Content)
	}
}

// TestLoop_StrippedReasoningDoesNotProduceEmptyAssistant covers the
// sibling case: stripReasoningForWire must not leave an empty assistant
// message on the wire.
func TestLoop_StrippedReasoningDoesNotProduceEmptyAssistant(t *testing.T) {
	// Stripping reasoning from a reasoning-only assistant turn must not
	// leave an empty assistant message on the wire: llama-server rejects
	// the request with 400 "Assistant message must contain either
	// 'content' or 'tool_calls'!", poisoning every subsequent turn of
	// the conversation.
	transcript := []oai.Message{
		{Role: oai.RoleAssistant, ReasoningContent: "thinking only, no action"},
		{Role: oai.RoleUser, Content: ReasoningOnlyNudgeMessage()},
	}
	wire := stripReasoningForWire(transcript)
	if wire[0].ReasoningContent != "" {
		t.Errorf("reasoning must be stripped, got %q", wire[0].ReasoningContent)
	}
	if wire[0].Content == "" && len(wire[0].ToolCalls) == 0 {
		t.Errorf("wire assistant message must carry content or tool_calls after stripping; got empty message")
	}
	if transcript[0].ReasoningContent == "" || transcript[0].Content != "" {
		t.Errorf("original transcript must be untouched: %+v", transcript[0])
	}
}
