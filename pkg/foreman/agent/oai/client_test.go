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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statuses[i])
		_, _ = w.Write([]byte(bodies[i]))
	})
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
