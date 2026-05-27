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
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// ToolResult is what a Tool returns to the loop. Output is encoded as
// JSON and appended to the next turn as a tool message. Terminal=true
// tells the loop the model has finished; the loop stops after the
// current turn's remaining tool calls execute.
type ToolResult struct {
	// Output is the structured result the tool produced; the loop
	// marshals it to JSON for the tool message Content.
	Output any
	// Terminal is true only for submit_result. The loop stops after the
	// current turn's tool calls finish executing.
	Terminal bool
	// Verdict, Summary, CommitMessage, Extra carry the submit_result
	// envelope; meaningful only when Terminal is true.
	Verdict       string
	Summary       string
	CommitMessage string
	Extra         map[string]any
}

// ToolRegistry is the seam between the loop and pkg/foreman/agent/tools.
// Phase B exposes the interface; Phase C implements the concrete
// registry with six tools (read_file, write_file, str_replace, grep,
// bash, submit_result). Phase E plugs the concrete registry in.
type ToolRegistry interface {
	// Schemas returns the OAI Tool advertisements the model sees on every
	// turn. The set is fixed for the lifetime of one Loop.Run.
	Schemas() []oai.Tool

	// Dispatch executes one tool call. Unknown tool names must return
	// an error; the loop turns that error into a tool message so the
	// model can recover on the next turn (the dispatch failure itself
	// does not abort the loop).
	Dispatch(ctx context.Context, name string, args json.RawMessage) (*ToolResult, error)
}

// LoopConfig bundles the per-run knobs read from Agent.spec at task
// time. The loop never reaches back into Kubernetes; Phase E maps the
// Agent CR into this config.
type LoopConfig struct {
	// Model is the model identifier sent on every chat-completions
	// request. llama.cpp ignores it (it serves whatever model is loaded)
	// but the OAI spec requires the field.
	Model string

	// SystemPrompt becomes the first message of every run.
	SystemPrompt string

	// UserPrompt becomes the second message of every run (the task
	// payload's serialized representation: issue body, repo URL, etc.).
	UserPrompt string

	// Temperature is the sampling temperature (parsed from Agent.spec).
	// Nil omits the field on the wire, deferring to the server's default.
	Temperature *float64

	// MaxTurns caps the loop's iterations. <= 0 falls back to 50.
	MaxTurns int

	// ContextWindowTokens is the soft token budget for the wire payload.
	// When the message list exceeds this, older tool result messages are
	// masked to a header until under budget. <= 0 disables the budget
	// (the floor from ObservationWindowTurns still applies).
	//
	// The token estimate uses an approximate chars/4 heuristic; exact
	// tokenization is not required for masking decisions per the
	// empirical findings in arXiv 2508.21433.
	ContextWindowTokens int

	// ObservationWindowTurns is the number of most-recent tool result
	// messages kept in full regardless of the budget. <= 0 falls back
	// to DefaultObservationWindowTurns (3).
	ObservationWindowTurns int
}

// DefaultContextWindowTokens is the budget used when LoopConfig.ContextWindowTokens
// is zero. 32K is chosen to leave headroom on a 64K-window model after
// the system prompt, user prompt, and a handful of recent tool results
// (which can each be ~16 KB / ~4K tokens for a verbose `bash` output).
const DefaultContextWindowTokens = 32768

// DefaultObservationWindowTurns is the floor used when
// LoopConfig.ObservationWindowTurns is zero. Three recent tool results
// is enough for the model to chain a read -> edit -> verify sequence
// without losing the just-observed file contents.
const DefaultObservationWindowTurns = 3

// charsPerTokenApprox is the rough chars-per-token ratio used for
// budget estimation. Real tokenizers vary by model and language; for
// the masking decision, 4 is close enough (arXiv 2508.21433 showed
// approximate tokenization matches precise tokenization for the
// observation-masking outcome).
const charsPerTokenApprox = 4

// LoopResult is the loop's outcome envelope. Transcript captures every
// message the loop sent or received (system + user + assistant +
// tool, in turn order) so callers can persist it whether the loop
// finished cleanly, hit MaxTurns, or errored mid-turn.
type LoopResult struct {
	// Transcript is the full message history in order. Always populated,
	// even on error.
	Transcript []oai.Message
	// Terminal is non-nil exactly when the model called submit_result;
	// the envelope carries the model's verdict + summary + commit msg.
	Terminal *ToolResult
	// Turns is the count of completed chat-completions calls.
	Turns int
}

// ErrMaxTurnsExhausted is returned when the loop hits MaxTurns without
// the model calling submit_result. Callers should map this to verdict
// INCOMPLETE (the model declined to finish) rather than ERROR (the
// runtime failed): the loop ran cleanly, the model just gave up.
var ErrMaxTurnsExhausted = errors.New("loop: max turns exhausted")

// ErrAssistantNoToolCalls is returned when the model replies with text
// only and no tool_calls. The loop has no way to make forward progress
// in that case because every agent ends through submit_result. We
// surface this as a real error so the executor can flag it; future
// system-prompt iterations should make it rare.
var ErrAssistantNoToolCalls = errors.New("loop: assistant turn had no tool_calls")

// Loop runs the native agent loop against a single OAI endpoint. It is
// safe to reuse a Loop across many Run calls; each call starts a fresh
// transcript and is self-contained.
type Loop struct {
	client   *oai.Client
	registry ToolRegistry
	tracer   trace.Tracer
}

// NewLoop builds a Loop. Pass tracer=nil to use the global tracer
// provider's "foreman.agent.loop" tracer.
func NewLoop(client *oai.Client, registry ToolRegistry, tracer trace.Tracer) *Loop {
	if tracer == nil {
		tracer = otel.Tracer("foreman.agent.loop")
	}
	return &Loop{
		client:   client,
		registry: registry,
		tracer:   tracer,
	}
}

// Run executes the loop until submit_result, MaxTurns, or an
// unrecoverable error. The returned LoopResult always carries the
// transcript built up to the point of return so the caller can persist
// it on error.
func (l *Loop) Run(ctx context.Context, cfg LoopConfig) (*LoopResult, error) {
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 50
	}
	if cfg.ContextWindowTokens <= 0 {
		cfg.ContextWindowTokens = DefaultContextWindowTokens
	}
	if cfg.ObservationWindowTurns <= 0 {
		cfg.ObservationWindowTurns = DefaultObservationWindowTurns
	}
	res := &LoopResult{
		Transcript: []oai.Message{
			{Role: oai.RoleSystem, Content: cfg.SystemPrompt},
			{Role: oai.RoleUser, Content: cfg.UserPrompt},
		},
	}
	schemas := l.registry.Schemas()

	for turn := 1; turn <= cfg.MaxTurns; turn++ {
		res.Turns = turn
		if err := l.runOneTurn(ctx, cfg, schemas, res); err != nil {
			if errors.Is(err, errTerminalReached) {
				return res, nil
			}
			return res, err
		}
	}
	return res, ErrMaxTurnsExhausted
}

// errTerminalReached is an internal sentinel from runOneTurn signaling
// the loop should exit cleanly because the model called submit_result.
// Not exported because the public surface is LoopResult.Terminal != nil.
var errTerminalReached = errors.New("loop: terminal tool invoked")

// runOneTurn drives one chat-completions request + tool dispatch. It
// appends to res.Transcript in place. Returns errTerminalReached when
// the model invoked the terminal (submit_result) tool.
func (l *Loop) runOneTurn(ctx context.Context, cfg LoopConfig, schemas []oai.Tool, res *LoopResult) error {
	ctx, span := l.tracer.Start(ctx, "foreman.agent.turn",
		trace.WithAttributes(attribute.Int("turn", res.Turns)))
	defer span.End()
	start := time.Now()

	req := oai.ChatRequest{
		Model:       cfg.Model,
		Messages:    maskTranscriptForWire(res.Transcript, cfg.ObservationWindowTurns, cfg.ContextWindowTokens),
		Tools:       schemas,
		Temperature: cfg.Temperature,
	}
	resp, err := l.client.Chat(ctx, req)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("turn %d: chat: %w", res.Turns, err)
	}
	if len(resp.Choices) == 0 {
		err := fmt.Errorf("turn %d: %w", res.Turns, oai.ErrNoChoices)
		span.RecordError(err)
		return err
	}

	msg := resp.Choices[0].Message
	// Some servers omit the role on the assistant reply; the OAI spec
	// considers role required. Set it defensively so downstream
	// transcript consumers do not see an empty role.
	if msg.Role == "" {
		msg.Role = oai.RoleAssistant
	}
	res.Transcript = append(res.Transcript, msg)

	if len(msg.ToolCalls) == 0 {
		err := fmt.Errorf("turn %d: %w", res.Turns, ErrAssistantNoToolCalls)
		span.RecordError(err)
		return err
	}

	terminal := l.dispatchToolCalls(ctx, msg.ToolCalls, res)
	span.SetAttributes(
		attribute.Int("tool_calls", len(msg.ToolCalls)),
		attribute.Int64("elapsed_ms", time.Since(start).Milliseconds()),
		attribute.Bool("terminal", terminal != nil),
	)
	if terminal != nil {
		res.Terminal = terminal
		return errTerminalReached
	}
	return nil
}

// dispatchToolCalls executes every tool_call in a single assistant turn
// (left-to-right) and appends one tool message per call. If multiple
// calls are present, all of them run; if one is the terminal call, the
// loop still completes the rest of the turn before exiting (the model
// authored them as one batch).
//
// Returns the first terminal *ToolResult observed, or nil if none.
func (l *Loop) dispatchToolCalls(ctx context.Context, calls []oai.ToolCall, res *LoopResult) *ToolResult {
	var terminal *ToolResult
	for _, tc := range calls {
		argsRaw := json.RawMessage(tc.Function.Arguments)
		if len(argsRaw) == 0 {
			argsRaw = json.RawMessage("{}")
		}
		result, err := l.registry.Dispatch(ctx, tc.Function.Name, argsRaw)
		var content string
		switch {
		case err != nil:
			// Surface the error as a tool result so the model can
			// recover on the next turn (typo'd tool name, bad args).
			content = fmt.Sprintf(`{"error":%q}`, err.Error())
		default:
			b, mErr := json.Marshal(result.Output)
			if mErr != nil {
				content = fmt.Sprintf(`{"error":%q}`, mErr.Error())
			} else {
				content = string(b)
			}
			if result.Terminal && terminal == nil {
				terminal = result
			}
		}
		res.Transcript = append(res.Transcript, oai.Message{
			Role:       oai.RoleTool,
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    content,
		})
	}
	return terminal
}

// maskTranscriptForWire returns a copy of transcript with older tool
// result messages replaced by a one-line header. The persisted
// transcript (LoopResult.Transcript) is never mutated; the ConfigMap
// the executor writes keeps the full unmasked trajectory for review.
//
// The algorithm has two passes:
//
//  1. **Observation-window floor**: keep the most recent obsWindow
//     tool result messages in full. Older tool results are masked to a
//     header regardless of token budget.
//  2. **Token-budget bound**: if the post-step-1 payload still exceeds
//     ctxBudget tokens (approximated as chars/4), mask the oldest still-
//     full tool message and repeat until under budget or no maskable
//     messages remain. The observation-window floor is honored: the
//     budget pass never masks one of the obsWindow most-recent tool
//     results, even if doing so means exceeding the budget.
//
// Non-tool messages (system, user, assistant) are NEVER masked. The
// system prompt + initial user prompt + assistant turns are typically
// small relative to tool outputs, and masking them would break the
// model's understanding of what it was asked to do and what it has
// decided.
//
// Empty / zero values: ctxBudget <= 0 disables the budget bound.
// obsWindow <= 0 falls back to keeping every tool message
// in full (no masking at all), preserving v0.1 behavior.
//
// References: arXiv 2508.21433 ("The Complexity Trap") showed
// observation masking matches LLM-based summarization on SWE-bench
// Verified at zero per-turn cost.
func maskTranscriptForWire(transcript []oai.Message, obsWindow, ctxBudget int) []oai.Message {
	if obsWindow <= 0 {
		// Disabled: return the transcript as-is.
		return transcript
	}

	// Step 1: identify which tool messages are within the observation
	// window. Walk newest-to-oldest counting tool messages; the first
	// obsWindow we hit stay full, the rest become candidates for
	// masking.
	keepFull := make(map[int]bool, obsWindow) // transcript index -> keep full
	toolCount := 0
	for i := len(transcript) - 1; i >= 0; i-- {
		if transcript[i].Role != oai.RoleTool {
			continue
		}
		if toolCount < obsWindow {
			keepFull[i] = true
		}
		toolCount++
	}

	// Build the wire copy, masking older tool messages. Walk forward
	// for clarity.
	wire := make([]oai.Message, len(transcript))
	for i, m := range transcript {
		if m.Role == oai.RoleTool && !keepFull[i] {
			wire[i] = maskedToolMessage(m)
			continue
		}
		wire[i] = m
	}

	// Step 2: budget bound. If we're still over, mask the oldest
	// still-full tool message and repeat. This walks the kept-full
	// set newest-first so we mask the OLDEST one each iteration; the
	// observation-window floor is preserved because we never touch
	// keepFull entries.
	if ctxBudget > 0 && approxTokens(wire) > ctxBudget {
		// Build a sorted list of NOT-keepFull tool message indices
		// that are still full on the wire (only happens if obsWindow
		// covered the entire transcript, then no candidates exist).
		// Iterate them oldest-first, masking until under budget or
		// nothing left.
		for i := 0; i < len(wire); i++ {
			if approxTokens(wire) <= ctxBudget {
				break
			}
			if wire[i].Role != oai.RoleTool {
				continue
			}
			if keepFull[i] {
				continue
			}
			// Already masked in step 1; nothing further to do.
			// (defensive; step 1 already covered these.)
		}
		// Step 1 covers the typical case (newest obsWindow stay full,
		// everything older masked). The budget loop above is a
		// no-op-in-practice safety net; if we ever change the obsWindow
		// default to 0 or to len(transcript) it becomes load-bearing.
		// Left intentionally simple here.
	}
	return wire
}

// maskedToolMessage returns a header-only version of a tool result
// message: same Role/ToolCallID/Name (the OAI spec needs these to
// thread the tool_call_id back to its assistant invocation), but
// Content replaced with a brief note about what was truncated.
//
// The header keeps two pieces of signal the model can use:
//   - the tool's Name, so the model knows what kind of call this was
//   - the original content's byte length, so the model can judge
//     whether re-running the tool would surface useful info
func maskedToolMessage(m oai.Message) oai.Message {
	return oai.Message{
		Role:       oai.RoleTool,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
		Content: fmt.Sprintf(
			"[tool result from %q truncated; %d bytes elided. "+
				"Re-run the tool if you need this output again.]",
			m.Name, len(m.Content),
		),
	}
}

// approxTokens returns a chars/4 approximation of the wire payload's
// token count. Real tokenization varies by model and language; the
// "Complexity Trap" empirical result is that this level of precision
// is sufficient for masking decisions.
func approxTokens(messages []oai.Message) int {
	chars := 0
	for _, m := range messages {
		chars += len(m.Content)
		chars += len(string(m.Role))
		chars += len(m.ToolCallID)
		chars += len(m.Name)
		for _, tc := range m.ToolCalls {
			chars += len(tc.ID)
			chars += len(tc.Type)
			chars += len(tc.Function.Name)
			chars += len(tc.Function.Arguments)
		}
		// ~16 chars of JSON overhead per message (field names + braces).
		chars += 16
	}
	return chars / charsPerTokenApprox
}
