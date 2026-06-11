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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrTruncatedToolCallArguments wraps a parser failure on
// ToolCall.Function.Arguments. The loop sees this and retries because
// llama.cpp issue #22072 returns HTTP 200 with truncated argument JSON
// roughly 13% of the time on Apple Silicon Metal builds; the request is
// otherwise valid and the same prompt usually succeeds on a retry.
var ErrTruncatedToolCallArguments = errors.New("oai: truncated tool_call.arguments JSON")

// ErrNoChoices means the server returned a 2xx with no Choices in the
// response. Non-retryable: usually points at a misconfigured upstream
// (wrong model name, empty body) rather than a transient hiccup.
var ErrNoChoices = errors.New("oai: response contained no choices")

// Client is a minimal OpenAI-compatible chat-completions client.
//
// It is deliberately not the official OpenAI SDK: Foreman needs exactly
// one HTTP shape, we want llama.cpp-specific retry semantics, and the
// SDK pulls a large transitive cone we would rather not adopt for v0.1.
//
// Wire protocol is SSE (text/event-stream) only -- see ChatRequest's
// docstring for why. Aggregation happens transparently inside Chat;
// callers see the same ChatResponse shape regardless.
type Client struct {
	baseURL    string
	httpClient *http.Client
	maxRetries int
	// authHeader, when non-empty, is sent verbatim on every request as
	// the Authorization header (typically "Bearer <token>"). Empty
	// dials without auth (the v0.1 local-llama-server path).
	authHeader string
	// backoffs are the inter-attempt delays. Each is wrapped with +/- 20%
	// jitter so a fleet of agents does not retry in lockstep.
	backoffs []time.Duration
}

// Option configures the Client.
type Option func(*Client)

// WithHTTPClient overrides the default http.Client (mainly for tests).
// Tests that drive a chunked SSE response from an httptest.Server pass
// their own client here so the SSE parsing path is exercised end to end.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.httpClient = c }
}

// WithBackoffs replaces the default 50ms / 250ms / 1s schedule. Useful
// in tests to keep the suite fast.
func WithBackoffs(b []time.Duration) Option {
	return func(cl *Client) {
		cl.backoffs = append([]time.Duration(nil), b...)
	}
}

// WithAuthHeader sets the Authorization header value sent on every
// request. v0.2 uses this for the cloud-proxy provider (e.g.
// "Bearer sk-..." for a LiteLLM gateway). Empty disables the header,
// matching the v0.1 local-llama-server path which expects no auth.
func WithAuthHeader(value string) Option {
	return func(cl *Client) { cl.authHeader = value }
}

// New builds a client. baseURL must include the /v1 suffix
// ("http://localhost:8080/v1"). requestTimeout bounds how long we wait
// for the upstream to start sending response headers; it does not
// bound the total streaming duration (long completions on slow models
// would otherwise spuriously time out). The caller's context.Context
// is the overall guard. maxRetries bounds how many additional attempts
// we make on ErrTruncatedToolCallArguments; pass 0 to disable retries
// entirely.
func New(baseURL string, requestTimeout time.Duration, maxRetries int, opts ...Option) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = requestTimeout
	c := &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Transport: transport,
			// No Client.Timeout: the streaming body can legitimately
			// take longer than the header wait (a thinking model
			// emitting a 50K-token reasoning trace at 30 tok/s = 30
			// min). ctx.Done is the overall guard.
		},
		maxRetries: maxRetries,
		backoffs: []time.Duration{
			50 * time.Millisecond,
			250 * time.Millisecond,
			time.Second,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Chat sends a single chat-completions request and returns the
// aggregated streamed response. On ErrTruncatedToolCallArguments the
// call is retried up to MaxRetries times with bounded exponential
// backoff plus jitter. Any other error (HTTP non-2xx, transport
// failure, SSE parse failure) is returned to the caller immediately
// without retry.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.sleepBackoff(ctx, attempt-1); err != nil {
				return nil, err
			}
		}
		resp, err := c.doOnce(ctx, req)
		if err == nil {
			return resp, nil
		}
		// Never retry once the parent context is done: a loop-wide budget
		// (#532) or external cancellation must propagate so the caller can
		// exit gracefully instead of spinning retries against a model that
		// will not answer in time. Checked before classification because a
		// context deadline also satisfies net.Error.Timeout().
		if ctx.Err() != nil {
			return nil, err
		}
		// Retry a truncated tool-call stream (a streaming hiccup) and a
		// transient per-request header timeout (slow prompt-eval on a warm
		// long context, #532). Any other error is returned as-is.
		if errors.Is(err, ErrTruncatedToolCallArguments) || isRetryableTimeout(err) {
			lastErr = err
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("after %d retries: %w", c.maxRetries, lastErr)
}

// isRetryableTimeout reports whether err is a network timeout (e.g. the
// transport's ResponseHeaderTimeout firing while awaiting the first
// token). Callers must rule out a done parent context first, since
// context.DeadlineExceeded also satisfies net.Error.Timeout().
func isRetryableTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// sleepBackoff blocks for backoffs[i] +/- 20% jitter, honoring ctx
// cancellation. i is clamped to the last backoff if the schedule is
// shorter than the retry count.
func (c *Client) sleepBackoff(ctx context.Context, i int) error {
	if len(c.backoffs) == 0 {
		return nil
	}
	if i >= len(c.backoffs) {
		i = len(c.backoffs) - 1
	}
	d := c.backoffs[i]
	// math/rand/v2 is intentional here: this is jitter to de-synchronize a
	// fleet of agents retrying at the same time, not a security primitive.
	// crypto/rand would be a wasteful overcorrection and add an unbounded
	// failure mode (entropy exhaustion under load).
	jitter := time.Duration(float64(d) * 0.4 * (rand.Float64() - 0.5)) //nolint:gosec // G404: jitter, not security
	d += jitter
	if d < 0 {
		d = 0
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// doOnce performs a single streaming round trip with no retry. The wire
// protocol is SSE (text/event-stream): the server emits a sequence of
// `data: {json}` lines that this method aggregates into a single
// ChatResponse. Returns the aggregated response on 2xx + clean
// arguments, or one of:
//
//   - ErrTruncatedToolCallArguments :: 2xx but tool_call.arguments not JSON
//   - ErrNoChoices                  :: 2xx but no choices arrived in the stream
//   - a wrapped status error        :: non-2xx, body included in the message
//   - a wrapped transport error     :: dial / TLS / read failure
func (c *Client) doOnce(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	// Force streaming on the wire regardless of caller setting.
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.authHeader != "" {
		httpReq.Header.Set("Authorization", c.authHeader)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("dispatch request: %w", err)
	}
	defer func() {
		// Drain so the TCP connection can be reused, but bound the
		// drain so a server that holds the connection open after
		// finish_reason without sending [DONE] (observed live with
		// llama.cpp + Carnice on tool-call responses) cannot stall
		// the caller. Cap at 64 KiB and 100ms; on hit, we close
		// without reuse and the next call opens a fresh conn.
		drained := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(100 * time.Millisecond):
		}
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("oai: status %d: %s", resp.StatusCode, string(b))
	}

	parsed, err := readSSEStream(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(parsed.Choices) == 0 {
		return nil, ErrNoChoices
	}
	if err := validateToolCallArguments(parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// readSSEStream reads the SSE event stream and aggregates the streamed
// chunks into a single ChatResponse. The OpenAI streaming format is:
//
//	data: {"id":"...","choices":[{"index":0,"delta":{...}, ...}]}
//	data: {"id":"...","choices":[{"index":0,"delta":{...}, ...}]}
//	...
//	data: [DONE]
//
// Each delta carries an incremental piece of the assistant message:
// initial chunk has the role, subsequent chunks accumulate content and
// tool-call argument fragments. The aggregator keys tool calls by their
// Index across deltas and concatenates Function.Arguments into the
// final string.
func readSSEStream(body io.Reader) (*ChatResponse, error) {
	scanner := bufio.NewScanner(body)
	// SSE lines can be arbitrarily long (a single chunk may carry a
	// large content burst). Set a generous max-token size so the
	// scanner does not fail mid-stream on a 1 MiB JSON line.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	agg := &chatAggregator{
		choices: map[int]*Choice{},
	}

	for scanner.Scan() {
		line := scanner.Text()
		// SSE protocol: blank lines separate events; non-data: lines
		// (e.g. `: keep-alive` comments, `event:` field) are ignored
		// here -- we only care about the data: payload.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var chunk ChatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return nil, fmt.Errorf("decode SSE chunk: %w (line=%q)", err, payload)
		}
		agg.absorb(chunk)

		// Some llama.cpp builds + response shapes (notably thinking-trace
		// models emitting tool calls) finish the model output but never
		// emit `data: [DONE]\n\n` -- they just stop sending and keep the
		// HTTP/1.1 keep-alive connection open. Reading would block forever
		// waiting on bytes the server will not send.
		//
		// Use finish_reason as the authoritative termination signal: when
		// every choice we have observed has a non-empty finish_reason, the
		// model has declared it done. Break here; [DONE] is now optional.
		if agg.allChoicesFinished() {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read SSE stream: %w", err)
	}

	return agg.build(), nil
}

// chatAggregator collapses a sequence of SSE chunks into a single
// ChatResponse with one Message per choice index. It tracks per-choice
// accumulators so multiple parallel choices (some servers stream more
// than one) are kept independent.
type chatAggregator struct {
	id      string
	choices map[int]*Choice // keyed by Choice.Index for stable ordering on build()
}

// absorb folds one streamed ChatChunk into the aggregator state.
func (a *chatAggregator) absorb(chunk ChatChunk) {
	if a.id == "" && chunk.ID != "" {
		a.id = chunk.ID
	}
	for _, cd := range chunk.Choices {
		ch, ok := a.choices[cd.Index]
		if !ok {
			ch = &Choice{Index: cd.Index}
			a.choices[cd.Index] = ch
		}
		// Role arrives on the first delta; later deltas may repeat it
		// or omit it. First non-empty wins.
		if ch.Message.Role == "" && cd.Delta.Role != "" {
			ch.Message.Role = cd.Delta.Role
		}
		ch.Message.Content += cd.Delta.Content
		ch.Message.ReasoningContent += cd.Delta.ReasoningContent

		// Tool call fragments arrive keyed by their own per-message
		// Index (distinct from the choice index). Aggregate into the
		// Choice's Message.ToolCalls slice, allocating new entries on
		// demand and concatenating Function.Arguments piece by piece.
		for _, tcd := range cd.Delta.ToolCalls {
			for len(ch.Message.ToolCalls) <= tcd.Index {
				ch.Message.ToolCalls = append(ch.Message.ToolCalls, ToolCall{})
			}
			tc := &ch.Message.ToolCalls[tcd.Index]
			if tcd.ID != "" {
				tc.ID = tcd.ID
			}
			if tcd.Type != "" {
				tc.Type = tcd.Type
			}
			if tcd.Function.Name != "" {
				tc.Function.Name = tcd.Function.Name
			}
			tc.Function.Arguments += tcd.Function.Arguments
		}

		if cd.FinishReason != "" {
			ch.FinishReason = cd.FinishReason
		}
	}
}

// allChoicesFinished returns true when the aggregator has seen at least
// one choice and every observed choice has a non-empty FinishReason.
// Callers use this to detect end-of-stream when the server omits the
// SSE [DONE] terminator -- which llama.cpp does for some response
// shapes (tool calls emitted by thinking-trace models).
func (a *chatAggregator) allChoicesFinished() bool {
	if len(a.choices) == 0 {
		return false
	}
	for _, ch := range a.choices {
		if ch.FinishReason == "" {
			return false
		}
	}
	return true
}

// build returns the aggregated ChatResponse with choices ordered by
// their Index so iteration is deterministic regardless of map order.
func (a *chatAggregator) build() *ChatResponse {
	if len(a.choices) == 0 {
		return &ChatResponse{ID: a.id}
	}
	// Order by index. Most responses have one choice; a small slice
	// makes the ordered range obvious without a sort.Slice.
	maxIdx := -1
	for k := range a.choices {
		if k > maxIdx {
			maxIdx = k
		}
	}
	out := &ChatResponse{
		ID:      a.id,
		Choices: make([]Choice, 0, maxIdx+1),
	}
	for i := 0; i <= maxIdx; i++ {
		if ch, ok := a.choices[i]; ok {
			out.Choices = append(out.Choices, *ch)
		}
	}
	return out
}

// validateToolCallArguments returns ErrTruncatedToolCallArguments if any
// tool_call in any choice has an Arguments string that is not valid
// JSON. An empty Arguments string is treated as the literal "{}" by the
// caller (some models emit "" for parameter-less tools) and is not
// flagged here.
func validateToolCallArguments(resp *ChatResponse) error {
	for _, ch := range resp.Choices {
		for _, tc := range ch.Message.ToolCalls {
			if tc.Function.Arguments == "" {
				continue
			}
			var probe any
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &probe); err != nil {
				return fmt.Errorf("%w (tool_call %s): %w", ErrTruncatedToolCallArguments, tc.ID, err)
			}
		}
	}
	return nil
}
