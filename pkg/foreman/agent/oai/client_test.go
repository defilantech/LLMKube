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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedHandler returns canned responses in order. After the script
// is exhausted, subsequent calls return the final entry (so a test that
// expects a retry to succeed on the second try just needs two entries).
//
// On 2xx responses the body fixture (a complete ChatResponse JSON, kept
// in that shape for readability) is converted on the wire into SSE
// events so the client's streaming parser is exercised end to end. The
// fixtures stay in the readable / authoritative form; serialization to
// SSE is a single helper call right before WriteHeader.
func scriptedHandler(t *testing.T, attempts *atomic.Int64, statuses []int, bodies []string) http.Handler {
	t.Helper()
	if len(statuses) != len(bodies) {
		t.Fatalf("scriptedHandler: %d statuses vs %d bodies", len(statuses), len(bodies))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(attempts.Add(1) - 1)
		if i >= len(statuses) {
			i = len(statuses) - 1
		}
		if got, want := r.Header.Get("Content-Type"), "application/json"; got != want {
			t.Errorf("Content-Type: want %q got %q", want, got)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		var probe ChatRequest
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Errorf("request body is not a ChatRequest: %v", err)
		}
		if !probe.Stream {
			t.Errorf("client must set Stream=true on the wire")
		}
		status := statuses[i]
		body := bodies[i]
		if status >= 200 && status < 300 {
			body = chatResponseToSSE(t, body)
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

// chatResponseToSSE serializes a complete ChatResponse JSON fixture as
// the SSE event stream a real OpenAI-compatible server would emit:
// one `data:` line per choice's delta, then a terminal `data: [DONE]`.
// One chunk per choice is enough for the parser tests; the multi-chunk
// aggregation path is covered by TestReadSSEStream_Aggregation.
func chatResponseToSSE(t *testing.T, body string) string {
	t.Helper()
	var parsed ChatResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("chatResponseToSSE: fixture is not a ChatResponse JSON: %v", err)
	}
	var sb strings.Builder
	for _, ch := range parsed.Choices {
		chunk := ChatChunk{
			ID:     parsed.ID,
			Object: "chat.completion.chunk",
			Choices: []ChoiceDelta{
				{
					Index: ch.Index,
					Delta: MessageDelta{
						Role:    ch.Message.Role,
						Content: ch.Message.Content,
					},
					FinishReason: ch.FinishReason,
				},
			},
		}
		// Tool calls collapse into one fragment per call: the model
		// would have streamed Function.Arguments in pieces; here we
		// emit the whole argument string in a single delta. The
		// aggregator handles either shape identically.
		for _, tc := range ch.Message.ToolCalls {
			chunk.Choices[0].Delta.ToolCalls = append(
				chunk.Choices[0].Delta.ToolCalls,
				ToolCallDelta{
					Index:    len(chunk.Choices[0].Delta.ToolCalls),
					ID:       tc.ID,
					Type:     tc.Type,
					Function: ToolCallFunctionDelta{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
				},
			)
		}
		out, err := json.Marshal(chunk)
		if err != nil {
			t.Fatalf("chatResponseToSSE: marshal chunk: %v", err)
		}
		sb.WriteString("data: ")
		sb.Write(out)
		sb.WriteString("\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

const okBodyOneToolCall = `{
  "id": "test",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-1",
        "type": "function",
        "function": {"name": "read_file", "arguments": "{\"path\":\"README.md\"}"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

const truncatedToolCallBody = `{
  "id": "test",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-1",
        "type": "function",
        "function": {"name": "read_file", "arguments": "{\"path\":\"READM"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

const okBodyEmptyArgs = `{
  "id": "test",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-1",
        "type": "function",
        "function": {"name": "list", "arguments": ""}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

const okBodyNoChoices = `{"id": "test", "choices": []}`

// noJitterBackoffs is a tiny, deterministic backoff schedule for tests.
// We still go through Client.sleepBackoff to exercise the cancellation
// path; the durations are short enough not to slow the suite down.
var noJitterBackoffs = []time.Duration{
	time.Millisecond,
	time.Millisecond,
	time.Millisecond,
}

func TestClient_Chat_HappyPath(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(scriptedHandler(t, &attempts,
		[]int{http.StatusOK}, []string{okBodyOneToolCall}))
	defer srv.Close()

	c := New(srv.URL+"/v1", time.Second, 0, WithBackoffs(noJitterBackoffs))
	resp, err := c.Chat(context.Background(), ChatRequest{Model: "test"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if attempts.Load() != 1 {
		t.Errorf("attempts: want 1 got %d", attempts.Load())
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestClient_Chat_RetriesTruncatedThenSucceeds(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(scriptedHandler(t, &attempts,
		[]int{http.StatusOK, http.StatusOK},
		[]string{truncatedToolCallBody, okBodyOneToolCall},
	))
	defer srv.Close()

	c := New(srv.URL+"/v1", time.Second, 3, WithBackoffs(noJitterBackoffs))
	resp, err := c.Chat(context.Background(), ChatRequest{Model: "test"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts: want 2 got %d", got)
	}
	if resp.Choices[0].Message.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestClient_Chat_ExhaustsRetries(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(scriptedHandler(t, &attempts,
		[]int{http.StatusOK}, []string{truncatedToolCallBody}))
	defer srv.Close()

	c := New(srv.URL+"/v1", time.Second, 2, WithBackoffs(noJitterBackoffs))
	_, err := c.Chat(context.Background(), ChatRequest{Model: "test"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrTruncatedToolCallArguments) {
		t.Errorf("error chain missing ErrTruncatedToolCallArguments: %v", err)
	}
	// 1 initial + 2 retries
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts: want 3 got %d", got)
	}
}

func TestClient_Chat_HTTPNon2xxNoRetry(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(scriptedHandler(t, &attempts,
		[]int{http.StatusInternalServerError}, []string{`{"error":"boom"}`}))
	defer srv.Close()

	c := New(srv.URL+"/v1", time.Second, 3, WithBackoffs(noJitterBackoffs))
	_, err := c.Chat(context.Background(), ChatRequest{Model: "test"})
	if err == nil {
		t.Fatal("expected error on 5xx")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("error did not mention status: %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("non-2xx must not retry; attempts=%d", got)
	}
}

func TestClient_Chat_EmptyArgumentsNotFlagged(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(scriptedHandler(t, &attempts,
		[]int{http.StatusOK}, []string{okBodyEmptyArgs}))
	defer srv.Close()

	c := New(srv.URL+"/v1", time.Second, 3, WithBackoffs(noJitterBackoffs))
	resp, err := c.Chat(context.Background(), ChatRequest{Model: "test"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("empty args should not retry; attempts=%d", got)
	}
	if resp.Choices[0].Message.ToolCalls[0].Function.Arguments != "" {
		t.Errorf("expected pass-through empty arguments")
	}
}

func TestClient_Chat_NoChoicesError(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(scriptedHandler(t, &attempts,
		[]int{http.StatusOK}, []string{okBodyNoChoices}))
	defer srv.Close()

	c := New(srv.URL+"/v1", time.Second, 2, WithBackoffs(noJitterBackoffs))
	_, err := c.Chat(context.Background(), ChatRequest{Model: "test"})
	if err == nil {
		t.Fatal("expected error on empty choices")
	}
	if !errors.Is(err, ErrNoChoices) {
		t.Errorf("expected ErrNoChoices; got %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("no-choices must not retry; attempts=%d", got)
	}
}

func TestClient_Chat_ContextCancelDuringBackoff(t *testing.T) {
	var attempts atomic.Int64
	// Always returns a retryable body so the second attempt requires a
	// backoff sleep that we cancel before it completes.
	srv := httptest.NewServer(scriptedHandler(t, &attempts,
		[]int{http.StatusOK}, []string{truncatedToolCallBody}))
	defer srv.Close()

	c := New(srv.URL+"/v1", time.Second, 5, WithBackoffs([]time.Duration{
		time.Hour, // long enough that ctx cancellation lands first
	}))
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the first attempt has returned and we are in the
	// first backoff sleep.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := c.Chat(ctx, ChatRequest{Model: "test"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// TestClient_Chat_TerminatesOnFinishReasonWithoutDONE pins the
// finish_reason-as-end-of-stream behavior. Discovered live 2026-05-24:
// llama.cpp builds emitting tool calls from a thinking-trace model
// (Carnice APEX-MTP-I) send the model's finish_reason chunk but then
// keep the HTTP/1.1 keep-alive connection open without sending the
// terminal `data: [DONE]\n\n` marker. The client must treat
// finish_reason set on every observed choice as authoritative
// end-of-stream; otherwise the scanner blocks indefinitely on bytes
// the server will not send.
func TestClient_Chat_TerminatesOnFinishReasonWithoutDONE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// One delta carrying a finished tool call. NO trailing
		// `data: [DONE]` marker -- the server just stops sending.
		// The handler returns here, the response body completes
		// naturally, but the test specifically exercises the
		// finish_reason-based early exit by also writing a long
		// hang-up window: we sleep AFTER the chunk so the scanner
		// would block if finish_reason were not consulted.
		flusher, _ := w.(http.Flusher)
		chunk := ChatChunk{
			ID:     "no-done",
			Object: "chat.completion.chunk",
			Choices: []ChoiceDelta{
				{
					Index: 0,
					Delta: MessageDelta{
						Role: RoleAssistant,
						ToolCalls: []ToolCallDelta{
							{
								Index:    0,
								ID:       "tc-1",
								Type:     "function",
								Function: ToolCallFunctionDelta{Name: "noop", Arguments: "{}"},
							},
						},
					},
					FinishReason: "tool_calls",
				},
			},
		}
		out, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(out)
		_, _ = w.Write([]byte("\n\n"))
		flusher.Flush()
		// Hold the connection open without sending more data. If the
		// client did not honor finish_reason, the scanner would still
		// be blocked reading when the server-side handler finally
		// returns (which closes the body and lets the scanner see EOF).
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", time.Second, 0, WithBackoffs(noJitterBackoffs))
	start := time.Now()
	resp, err := c.Chat(context.Background(), ChatRequest{Model: "test"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// Must return well before the server's 200ms hold finishes. The
	// caller-side drain budget is 100ms (see doOnce's defer), so we
	// allow up to that plus a small scheduler slack. The point of
	// the test is that we do NOT block for the full 200ms server hold.
	if elapsed > 180*time.Millisecond {
		t.Errorf("Chat blocked %v waiting for [DONE]; finish_reason should have terminated sooner", elapsed)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices: want 1 got %d", len(resp.Choices))
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason: got %q", resp.Choices[0].FinishReason)
	}
	tcs := resp.Choices[0].Message.ToolCalls
	if len(tcs) != 1 || tcs[0].Function.Name != "noop" {
		t.Errorf("tool call shape: %+v", tcs)
	}
}

// TestClient_Chat_AggregatesMultiChunkArguments covers the streaming
// aggregation path the real server uses: tool-call arguments arrive in
// fragments, often character-by-character or token-by-token. The
// aggregator must concatenate them into the final argument string.
func TestClient_Chat_AggregatesMultiChunkArguments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// First chunk: role + tool call id/name + empty args.
		c1 := ChatChunk{
			ID: "agg", Object: "chat.completion.chunk",
			Choices: []ChoiceDelta{{
				Index: 0,
				Delta: MessageDelta{
					Role: RoleAssistant,
					ToolCalls: []ToolCallDelta{{
						Index:    0,
						ID:       "tc-x",
						Type:     "function",
						Function: ToolCallFunctionDelta{Name: "read_file", Arguments: `{"pa`},
					}},
				},
			}},
		}
		// Subsequent chunks stream arg fragments. No id/type/name (already set).
		c2 := ChatChunk{
			ID: "agg", Object: "chat.completion.chunk",
			Choices: []ChoiceDelta{{
				Index: 0,
				Delta: MessageDelta{
					ToolCalls: []ToolCallDelta{{
						Index:    0,
						Function: ToolCallFunctionDelta{Arguments: `th":"R`},
					}},
				},
			}},
		}
		c3 := ChatChunk{
			ID: "agg", Object: "chat.completion.chunk",
			Choices: []ChoiceDelta{{
				Index: 0,
				Delta: MessageDelta{
					ToolCalls: []ToolCallDelta{{
						Index:    0,
						Function: ToolCallFunctionDelta{Arguments: `EADME.md"}`},
					}},
				},
				FinishReason: "tool_calls",
			}},
		}
		for _, c := range []ChatChunk{c1, c2, c3} {
			out, _ := json.Marshal(c)
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(out)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", time.Second, 0, WithBackoffs(noJitterBackoffs))
	resp, err := c.Chat(context.Background(), ChatRequest{Model: "test"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	tcs := resp.Choices[0].Message.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("tool_calls: want 1 got %d", len(tcs))
	}
	want := `{"path":"README.md"}`
	if tcs[0].Function.Arguments != want {
		t.Errorf("aggregated arguments: got %q want %q", tcs[0].Function.Arguments, want)
	}
	if tcs[0].ID != "tc-x" || tcs[0].Function.Name != "read_file" {
		t.Errorf("metadata lost in aggregation: %+v", tcs[0])
	}
}
