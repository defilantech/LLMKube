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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
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
type Client struct {
	baseURL    string
	httpClient *http.Client
	maxRetries int
	// backoffs are the inter-attempt delays. Each is wrapped with +/- 20%
	// jitter so a fleet of agents does not retry in lockstep.
	backoffs []time.Duration
}

// Option configures the Client.
type Option func(*Client)

// WithHTTPClient overrides the default http.Client (mainly for tests).
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

// New builds a client. baseURL must include the /v1 suffix
// ("http://localhost:8080/v1"). requestTimeout bounds a single HTTP
// request. maxRetries bounds how many additional attempts we make on
// ErrTruncatedToolCallArguments; pass 0 to disable retries entirely.
func New(baseURL string, requestTimeout time.Duration, maxRetries int, opts ...Option) *Client {
	c := &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: requestTimeout},
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

// Chat sends a single chat-completions request and returns the response.
// On ErrTruncatedToolCallArguments the call is retried up to MaxRetries
// times with bounded exponential backoff plus jitter. Any other error
// (HTTP non-2xx, transport failure, JSON decode failure) is returned to
// the caller immediately without retry.
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
		if !errors.Is(err, ErrTruncatedToolCallArguments) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("after %d retries: %w", c.maxRetries, lastErr)
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

// doOnce performs a single round trip with no retry. Returns the parsed
// response on 2xx + clean arguments, or one of:
//
//   - ErrTruncatedToolCallArguments :: 2xx but tool_call.arguments not JSON
//   - ErrNoChoices                  :: 2xx but len(Choices) == 0
//   - a wrapped status error        :: non-2xx, body included in the message
//   - a wrapped transport error     :: dial / TLS / read failure
func (c *Client) doOnce(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
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
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("dispatch request: %w", err)
	}
	defer func() {
		// Drain and close so the TCP connection can be reused, capped
		// at 64 KB to keep a chatty server from stalling shutdown.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("oai: status %d: %s", resp.StatusCode, string(b))
	}

	var parsed ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, ErrNoChoices
	}
	if err := validateToolCallArguments(&parsed); err != nil {
		return nil, err
	}
	return &parsed, nil
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
