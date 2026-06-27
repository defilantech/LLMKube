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
	"strings"
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

	// ContextStrategy selects the wire-payload builder. "" or "window"
	// applies observation masking (ObservationWindowTurns); "session"
	// keeps an append-only prefix and compacts at ContextWindowTokens.
	// See selectWireTranscript and issue #756.
	ContextStrategy string

	// Progress configures the stuck-loop detector (#544). The default
	// value (all zeros) disables detection; callers wanting the
	// debut-quality default should set this to DefaultProgressConfig.
	// The executor maps Agent.spec.stuckLoopDetection here; an unset
	// Agent CR field yields DefaultProgressConfig.
	Progress ProgressConfig

	// LoopBudget is the loop-wide wall-clock ceiling (#532). When > 0 the
	// loop wraps its context with this deadline; if a turn's request is
	// still pending when the budget fires, the loop exits gracefully with
	// an INCOMPLETE terminal (transcript preserved) rather than bubbling a
	// retry-less error that kills the AgenticTask. <= 0 disables it (only
	// MaxTurns and the per-request timeout bound the run). The executor
	// maps Agent.spec.requestTimeoutSeconds here; the per-request header
	// timeout is the separate Agent.spec.requestTurnTimeoutSeconds.
	LoopBudget time.Duration

	// MaxNoToolCallRetries is how many consecutive no-tool-call turns the
	// loop tolerates by appending a forceful corrective ("call a tool; if
	// done, call submit_result") and retrying before giving up with
	// ErrAssistantNoToolCalls. Local models sometimes narrate their
	// conclusion as prose instead of emitting the terminal submit_result
	// call; a single such turn should not fail the whole task. The streak
	// resets after any successful tool-calling turn, so only sustained
	// narration exhausts the budget. <= 0 falls back to
	// DefaultMaxNoToolCallRetries.
	MaxNoToolCallRetries int

	// MaxReasoningOnlyRetries bounds the separate streak for turns where
	// a hybrid-thinking model emitted reasoning_content but no tool call
	// and no content (#650). Such a turn is a model mid-thought, not a
	// model narrating prose, so it gets a gentler continuation nudge and
	// a roomier budget than MaxNoToolCallRetries before the loop gives
	// up with ErrAssistantReasoningOnly. Resets after any tool-calling
	// turn. <= 0 falls back to DefaultMaxReasoningOnlyRetries.
	MaxReasoningOnlyRetries int

	// VerifyTerminal, when set, is the coder gate feedback loop (#749).
	// On a terminal the loop would otherwise accept, the loop calls this
	// hook; if it returns accept=false the loop appends `feedback` as a
	// user message and continues so the agent can fix what the gate
	// found, up to MaxVerifyRetries times. When the budget is exhausted
	// the terminal is downgraded to a CoderGateFailedEnvelope (INCOMPLETE)
	// so a branch that never passes fmt/vet/build/lint cannot land as GO.
	// Nil disables the feature (default), preserving the prior behavior.
	VerifyTerminal TerminalVerifier

	// MaxVerifyRetries bounds the gate fix attempts when VerifyTerminal is
	// set. <= 0 with a non-nil VerifyTerminal means "verify once, never
	// retry": a failing gate immediately downgrades to INCOMPLETE.
	MaxVerifyRetries int

	// RestrictReadsInForcingPhase, when true, drops read_file from the
	// advertised tool set during the EditFreeStreak forcing phase (in
	// addition to grep/bash), forcing a thrash-prone model to edit instead
	// of re-reading. Set from the resolved ModelProfile. Default false.
	RestrictReadsInForcingPhase bool
}

// DefaultContextWindowTokens is the budget used when LoopConfig.ContextWindowTokens
// is zero. 32K is chosen to leave headroom on a 64K-window model after
// the system prompt, user prompt, and a handful of recent tool results
// (which can each be ~16 KB / ~4K tokens for a verbose `bash` output).
const DefaultContextWindowTokens = 32768

// DefaultMaxNoToolCallRetries is the number of corrective retries used
// when LoopConfig.MaxNoToolCallRetries is <= 0. Two is enough to recover
// a model that narrated its conclusion once or twice instead of calling
// submit_result, without letting a model that simply refuses to call
// tools spin to MaxTurns.
const DefaultMaxNoToolCallRetries = 2

// DefaultMaxReasoningOnlyRetries is the budget used when
// LoopConfig.MaxReasoningOnlyRetries is <= 0. Thinking models pause
// mid-reasoning more often than prose models narrate, so the default is
// roomier than DefaultMaxNoToolCallRetries; four consecutive
// reasoning-only turns with no action means the model is circling, not
// converging.
const DefaultMaxReasoningOnlyRetries = 4

// DefaultObservationWindowTurns is the floor used when
// LoopConfig.ObservationWindowTurns is zero. Three recent tool results
// is enough for the model to chain a read -> edit -> verify sequence
// without losing the just-observed file contents.
const DefaultObservationWindowTurns = 3

const (
	// ContextStrategyWindow applies observation masking bounded by
	// ObservationWindowTurns (the default strategy).
	ContextStrategyWindow = "window"
	// ContextStrategySession keeps a stable, append-only prefix and
	// compacts only at the ContextWindowTokens ceiling. See issue #756.
	ContextStrategySession = "session"
)

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

// StuckLoopOutcome is the value the progress monitor sets in
// LoopResult.Terminal.Extra["outcome"] when it force-terminates a run.
// Callers that want to distinguish a model-emitted terminal from a
// detector-synthesized one check this marker (or read the Signal
// field) rather than keying on a sentinel error. The loop returns a
// nil error with a populated Terminal envelope so the executor's
// normal terminal-handling path runs and the synthesized verdict
// surfaces in AgenticTask.status.result.
const StuckLoopOutcome = "STUCK-LOOP-DETECTED"

// LoopBudgetOutcome is the value the loop sets in
// LoopResult.Terminal.Extra["outcome"] when LoopConfig.LoopBudget is
// exhausted mid-turn (#532). Like the stuck-loop case, the loop returns
// a nil error with a populated INCOMPLETE terminal so the executor's
// normal terminal-handling path persists the partial transcript instead
// of recording a retry-less ExecutorError.
const LoopBudgetOutcome = "LOOP-BUDGET-EXHAUSTED"

// CoderGateFailedOutcome is the value the loop sets in
// LoopResult.Terminal.Extra["outcome"] when the coder verification gate
// (#749) never passed within MaxVerifyRetries fix attempts. The terminal
// is downgraded to INCOMPLETE so a branch whose fast checks (fmt / vet /
// build / lint) still fail never lands as a clean GO.
const CoderGateFailedOutcome = "CODER-GATE-FAILED"

// outcomeKey is the Terminal.Extra map key the loop stamps with the
// machine-readable outcome string (LoopBudgetOutcome, CoderGateFailedOutcome,
// StuckLoopOutcome, ...).
const outcomeKey = "outcome"

// LoopBudgetExhaustedEnvelope synthesizes the INCOMPLETE terminal the
// loop returns when its wall-clock budget fires. turn is the turn that
// was in flight when the deadline hit.
func LoopBudgetExhaustedEnvelope(turn int) *ToolResult {
	const summary = "loop wall-clock budget exhausted before the agent finished"
	return &ToolResult{
		Terminal:      true,
		Verdict:       "INCOMPLETE",
		Summary:       summary,
		CommitMessage: "",
		Output: map[string]any{
			"verdict": "INCOMPLETE",
			"summary": summary,
		},
		Extra: map[string]any{
			outcomeKey:      LoopBudgetOutcome,
			"terminateTurn": turn,
		},
	}
}

// CoderGateFailedEnvelope synthesizes the INCOMPLETE terminal the loop
// returns when the coder verification gate (#749) still fails after
// MaxVerifyRetries fix attempts. attempts is how many gate cycles ran;
// lastFeedback is the final gate output, truncated for the status field.
func CoderGateFailedEnvelope(turn, attempts int, lastFeedback string) *ToolResult {
	summary := fmt.Sprintf(
		"verification gate did not pass after %d fix attempt(s); not landing a failing GO",
		attempts)
	return &ToolResult{
		Terminal:      true,
		Verdict:       "INCOMPLETE",
		Summary:       summary,
		CommitMessage: "",
		Output: map[string]any{
			"verdict": "INCOMPLETE",
			"summary": summary,
		},
		Extra: map[string]any{
			outcomeKey:      CoderGateFailedOutcome,
			"terminateTurn": turn,
			"gateAttempts":  attempts,
			"gateOutput":    truncateGateOutput(lastFeedback),
		},
	}
}

// maxGateOutputBytes bounds the gate output stored in the terminal Extra
// (status visibility), mirroring the gate-Job runbook's 32 KiB cap.
const maxGateOutputBytes = 32 * 1024

func truncateGateOutput(s string) string {
	if len(s) <= maxGateOutputBytes {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-maxGateOutputBytes:]
}

// TerminalVerifier inspects a terminal the loop is about to accept and
// reports whether it should stand. Returning accept=false makes the loop
// append feedback as a user message and continue (up to
// LoopConfig.MaxVerifyRetries), so the agent can fix what the gate found
// and re-submit. A non-nil err is treated as "could not verify" and the
// terminal is accepted as-is (the gate is best-effort; the clean-room
// gate Job is the authoritative backstop). The verifier decides which
// terminals it gates: it should accept non-GO verdicts and non-coder
// roles immediately.
type TerminalVerifier func(
	ctx context.Context,
	terminal *ToolResult,
	transcript []oai.Message,
) (accept bool, feedback string, err error)

// ErrAssistantNoToolCalls is returned when the model replies with text
// only and no tool_calls. The loop has no way to make forward progress
// in that case because every agent ends through submit_result. We
// surface this as a real error so the executor can flag it; future
// system-prompt iterations should make it rare.
var ErrAssistantNoToolCalls = errors.New("loop: assistant turn had no tool_calls")

// ErrAssistantReasoningOnly is returned when a hybrid-thinking model
// exhausts MaxReasoningOnlyRetries with turns that carry
// reasoning_content but no tool call and no content (#650). Distinct
// from ErrAssistantNoToolCalls so the failure taxonomy can tell "the
// model narrated prose" from "the model thought itself in circles."
var ErrAssistantReasoningOnly = errors.New("loop: assistant turns carried only reasoning, no tool_calls")

// ReasoningOnlyNudgeMessage is the continuation the loop appends after
// a reasoning-only turn. Unlike NoToolCallNudgeMessage it does not
// scold: the model was mid-thought, so the correction is "act on the
// plan you just reasoned out." Exported so tests can assert on it.
func ReasoningOnlyNudgeMessage() string {
	return "Your previous reply contained only internal reasoning and no " +
		"tool call. Continue from that reasoning and emit the tool call " +
		"for your next action now. If your review or task is complete, " +
		"call submit_result with your verdict."
}

// NoToolCallNudgeMessage is the corrective the loop appends as a user
// message after a turn that returned text but no tool_calls. Every agent
// finishes through a tool (submit_result); a prose-only reply makes no
// forward progress. The message is forceful and unambiguous so a model
// that narrated its conclusion converts that prose into the terminal
// tool call on the retry. Exported so tests and callers can assert on it.
func NoToolCallNudgeMessage() string {
	return "Your previous reply contained no tool call. You cannot finish " +
		"or make progress by writing prose: every action, including " +
		"finishing, happens through a tool call. If your work is complete " +
		"and verified, you MUST call submit_result now with your verdict " +
		"(GO or NO_GO), a summary, and the commit message. Otherwise call " +
		"the appropriate tool to continue. Respond with a tool call, not text."
}

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
	if cfg.MaxNoToolCallRetries <= 0 {
		cfg.MaxNoToolCallRetries = DefaultMaxNoToolCallRetries
	}
	if cfg.MaxReasoningOnlyRetries <= 0 {
		cfg.MaxReasoningOnlyRetries = DefaultMaxReasoningOnlyRetries
	}
	// Loop-wide wall-clock budget (#532). Distinct from the per-request
	// header timeout baked into the OAI client: this bounds the whole
	// run, so one slow long-context turn can't hang past the budget, and
	// the per-request timeout can stay tight without doubling as the
	// loop cap.
	if cfg.LoopBudget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.LoopBudget)
		defer cancel()
	}
	res := &LoopResult{
		Transcript: []oai.Message{
			{Role: oai.RoleSystem, Content: cfg.SystemPrompt},
			{Role: oai.RoleUser, Content: cfg.UserPrompt},
		},
	}
	schemas := l.registry.Schemas()
	restrictedSchemas := filterForcedEditSchemas(schemas, cfg.RestrictReadsInForcingPhase)
	monitor := NewLoopProgressMonitor(cfg.Progress)

	// EditFreeStreak forcing function. A soft text nudge alone has poor
	// recovery rates: a confused model just keeps reading (empirically,
	// #730: a reasoning-off MoE read 16 files, ignored the nudge, and got
	// force-terminated without ever editing). So once the monitor nudges on
	// EditFreeStreak we enter a bounded "forcing phase": for up to
	// maxRestrictedEditTurns turns we drop the exploration tools (grep,
	// bash) from the advertised set -- read_file stays so the model can
	// fetch the exact text to edit -- and the phase ends only when an edit
	// actually lands (a successful write_file/str_replace, NOT a
	// str_replace that errors on a wrong old_string). If no edit lands
	// within the budget, the run is force-terminated as a stuck loop.
	//
	// forceEditOnly is whether we are currently in the forcing phase;
	// restrictedTurnsUsed counts forcing-phase turns spent without a
	// successful edit.
	forceEditOnly := false
	restrictedTurnsUsed := 0
	const maxRestrictedEditTurns = 3

	// Final-turns convergence guard. In the last forceSubmitFinalTurns turns
	// before MaxTurns, an agent that still has not called submit_result is
	// advertised submit_result ONLY (plus a one-time hard nudge), so it
	// produces a terminal verdict instead of silently hitting
	// ErrMaxTurnsExhausted with no result. This is the reviewer loop's
	// convergence mechanism: reviewers have EditFreeStreak disabled (they
	// legitimately read for many turns), so without this a non-converging
	// reviewer just rambles to MaxTurns and yields an empty INCOMPLETE.
	// Coders get it as a backstop behind the EditFreeStreak forcing function.
	const forceSubmitFinalTurns = 3
	forceSubmitAnnounced := false

	// noToolCallStreak counts consecutive turns that returned text without
	// a tool call. It resets to zero after any successful tool-calling
	// turn, so only sustained narration exhausts MaxNoToolCallRetries.
	noToolCallStreak := 0
	// verifyRetries counts coder-gate fix attempts spent so far (#749).
	verifyRetries := 0
	// reasoningOnlyStreak is the sibling counter for reasoning-only
	// turns (#650); separate so a thinking model's pauses don't consume
	// the prose-narration budget and vice versa.
	reasoningOnlyStreak := 0

	for turn := 1; turn <= cfg.MaxTurns; turn++ {
		res.Turns = turn

		// During the forcing phase, advertise the restricted set (no grep,
		// no bash) so the model acts on what it has already read instead of
		// launching a fresh exploration sweep.
		activeSchemas := schemas
		if forceEditOnly {
			activeSchemas = restrictedSchemas
		}

		// Final-turns convergence guard (takes precedence over the forcing
		// phase): in the last forceSubmitFinalTurns turns, advertise
		// submit_result only and append a one-time hard nudge, so the model
		// must conclude rather than exhaust MaxTurns with no verdict.
		if cfg.MaxTurns > forceSubmitFinalTurns && turn > cfg.MaxTurns-forceSubmitFinalTurns {
			activeSchemas = filterSubmitOnlySchemas(schemas)
			if !forceSubmitAnnounced {
				res.Transcript = append(res.Transcript, oai.Message{
					Role:    oai.RoleUser,
					Content: ForceSubmitMessage(cfg.MaxTurns - turn + 1),
				})
				forceSubmitAnnounced = true
			}
		}

		// runOneTurn appends the assistant message + tool messages to
		// res.Transcript. It returns errTerminalReached on submit_result.
		// editSucceeded reports whether a write_file/str_replace landed this
		// turn (gates the forcing phase below).
		editSucceeded, turnErr := l.runOneTurn(ctx, cfg, activeSchemas, res)
		if turnErr != nil {
			if errors.Is(turnErr, errTerminalReached) {
				// Coder gate feedback loop (#749): give the gate a chance to
				// veto a terminal the loop would otherwise accept. A veto
				// with retries left injects feedback and continues the loop.
				if l.applyTerminalGate(ctx, cfg, res, turn, &verifyRetries) {
					continue
				}
				return res, nil
			}
			// Loop-wide budget exhausted: exit gracefully with an
			// INCOMPLETE terminal so the executor persists the partial
			// transcript instead of recording a retry-less ExecutorError
			// (#532). Only the loop's own deadline maps here; an external
			// cancellation (context.Canceled) still propagates as an error.
			if cfg.LoopBudget > 0 && errors.Is(turnErr, context.DeadlineExceeded) {
				res.Terminal = LoopBudgetExhaustedEnvelope(turn)
				return res, nil
			}
			// No-tool-call recovery: a prose-only reply makes no forward
			// progress, but local models sometimes narrate their
			// conclusion instead of calling submit_result. Append a
			// forceful corrective and retry, bounded by
			// MaxNoToolCallRetries, before surfacing the error. The
			// failing assistant turn is already in the transcript, so the
			// model sees its own prose followed by the correction.
			if errors.Is(turnErr, ErrAssistantNoToolCalls) {
				if noToolCallStreak < cfg.MaxNoToolCallRetries {
					noToolCallStreak++
					res.Transcript = append(res.Transcript, oai.Message{
						Role:    oai.RoleUser,
						Content: NoToolCallNudgeMessage(),
					})
					continue
				}
				return res, turnErr
			}
			// Reasoning-only recovery (#650): the model was mid-thought,
			// not narrating. Continue it with a gentler nudge on its own
			// roomier budget; the reasoning itself is already in the
			// transcript for archaeology.
			if errors.Is(turnErr, ErrAssistantReasoningOnly) {
				if reasoningOnlyStreak < cfg.MaxReasoningOnlyRetries {
					reasoningOnlyStreak++
					res.Transcript = append(res.Transcript, oai.Message{
						Role:    oai.RoleUser,
						Content: ReasoningOnlyNudgeMessage(),
					})
					continue
				}
				return res, turnErr
			}
			return res, turnErr
		}

		// A successful tool-calling turn clears both streaks.
		noToolCallStreak = 0
		reasoningOnlyStreak = 0

		// Consult the progress monitor. The most recent assistant
		// message in the transcript has this turn's tool_calls.
		calls := mostRecentAssistantToolCalls(res.Transcript)
		decision := monitor.Observe(turn, calls, res.Transcript)

		// A non-EditFreeStreak force-terminate (e.g. ContextHardCap, the
		// deadliest signal) always wins, even mid-forcing-phase.
		if decision.Action == ProgressForceTerminate && decision.Signal != signalEditFreeStreak {
			res.Terminal = ForceTerminateEnvelope(decision, turn)
			return res, nil
		}

		// Forcing phase: the loop, not the monitor, owns EditFreeStreak
		// termination here so the model gets a bounded number of restricted
		// turns to recover (a failed str_replace often needs one re-read to
		// fix). The monitor's own EditFreeStreak escalation is intercepted
		// below by skipping its decision while we are in the phase.
		if forceEditOnly {
			if editSucceeded {
				// The forced edit landed; leave the phase and fall through
				// to normal handling (the decision is typically Continue
				// because an edit resets the monitor's streak).
				forceEditOnly = false
				restrictedTurnsUsed = 0
			} else {
				// No edit this turn. Spend one restricted turn; terminate
				// when the budget is exhausted, otherwise remind and retry.
				restrictedTurnsUsed++
				if restrictedTurnsUsed >= maxRestrictedEditTurns {
					res.Terminal = ForceTerminateEnvelope(ProgressDecision{
						Action: ProgressForceTerminate,
						Signal: signalEditFreeStreak,
						Detail: fmt.Sprintf(
							"no successful edit in %d restricted turns after the "+
								"edit-free nudge (turn %d)", restrictedTurnsUsed, turn),
					}, turn)
					return res, nil
				}
				res.Transcript = append(res.Transcript, oai.Message{
					Role:    oai.RoleUser,
					Content: ForcedEditReminderMessage(maxRestrictedEditTurns - restrictedTurnsUsed),
				})
				continue
			}
		}

		switch decision.Action {
		case ProgressContinue:
			// nothing to do
		case ProgressNudge:
			// Append a synthetic user message with the nudge text. The
			// next turn's request will include it; the model gets one
			// chance to recover before escalation.
			res.Transcript = append(res.Transcript, oai.Message{
				Role:    oai.RoleUser,
				Content: NudgeMessage(decision),
			})
			// Enter the forcing phase for the EditFreeStreak signal so the
			// nudge is a hard constraint (restricted tools + bounded turns),
			// not just a suggestion the model can ignore.
			if decision.Signal == signalEditFreeStreak {
				forceEditOnly = true
				restrictedTurnsUsed = 0
			}
		case ProgressForceTerminate:
			// Reachable only for EditFreeStreak while NOT in the forcing
			// phase (other signals handled above; an active phase consumes
			// the EditFreeStreak escalation via restrictedTurnsUsed). Treat
			// it as a clean structural outcome: synthesize a submit_result
			// envelope and exit with a NIL error so the executor's terminal
			// path runs and the synthesized verdict + Extra.outcome
			// propagate to AgenticTask status. Callers distinguish this from
			// a model-emitted terminal via Terminal.Extra["outcome"] ==
			// StuckLoopOutcome (see ForceTerminateEnvelope).
			res.Terminal = ForceTerminateEnvelope(decision, turn)
			return res, nil
		}
	}
	return res, ErrMaxTurnsExhausted
}

// stripReasoningForWire returns a wire view of the transcript with
// reasoning_content removed from assistant messages. The transcript
// keeps the reasoning for the persisted ConfigMap; the wire drops it,
// mirroring how chat templates exclude think blocks from history, so
// past reasoning never re-enters the context window or the token
// budget (#650). Copies lazily: if no message carries reasoning, the
// input slice is returned as-is.
func stripReasoningForWire(msgs []oai.Message) []oai.Message {
	needsCopy := false
	for i := range msgs {
		if msgs[i].ReasoningContent != "" {
			needsCopy = true
			break
		}
	}
	if !needsCopy {
		return msgs
	}
	wire := make([]oai.Message, len(msgs))
	copy(wire, msgs)
	for i := range wire {
		if wire[i].ReasoningContent == "" {
			continue
		}
		wire[i].ReasoningContent = ""
		// A reasoning-only turn would become a fully empty assistant
		// message, which llama-server rejects with 400 ("Assistant
		// message must contain either 'content' or 'tool_calls'!"),
		// poisoning every later turn of the conversation. Leave a
		// placeholder so the template stays valid and the adjacent
		// continuation nudge still reads coherently.
		if wire[i].Role == oai.RoleAssistant &&
			strings.TrimSpace(wire[i].Content) == "" && len(wire[i].ToolCalls) == 0 {
			wire[i].Content = "[paused to reason internally; no action taken this turn]"
		}
	}
	return wire
}

// mostRecentAssistantToolCalls walks the transcript backwards and
// returns the tool_calls from the latest assistant message. Empty if
// no assistant message exists. Used by the progress monitor to inspect
// what the model just asked the harness to do.
func mostRecentAssistantToolCalls(transcript []oai.Message) []oai.ToolCall {
	for i := len(transcript) - 1; i >= 0; i-- {
		if transcript[i].Role == oai.RoleAssistant {
			return transcript[i].ToolCalls
		}
	}
	return nil
}

// errTerminalReached is an internal sentinel from runOneTurn signaling
// the loop should exit cleanly because the model called submit_result.
// Not exported because the public surface is LoopResult.Terminal != nil.
var errTerminalReached = errors.New("loop: terminal tool invoked")

// applyTerminalGate runs the coder gate (#749) against a terminal the
// loop would otherwise accept. It returns true when the loop should
// CONTINUE (the gate vetoed the terminal with retries left, so feedback
// was appended and res.Terminal cleared), and false when the terminal
// stands (no gate configured, gate accepted, gate could not run, or the
// retry budget was spent and the terminal was downgraded to a
// CoderGateFailedEnvelope). verifyRetries is incremented on each veto.
func (l *Loop) applyTerminalGate(
	ctx context.Context,
	cfg LoopConfig,
	res *LoopResult,
	turn int,
	verifyRetries *int,
) bool {
	if cfg.VerifyTerminal == nil {
		return false
	}
	accept, feedback, vErr := cfg.VerifyTerminal(ctx, res.Terminal, res.Transcript)
	if vErr != nil || accept {
		// Best-effort gate: a verifier error means "could not verify",
		// so the terminal stands and the authoritative gate Job remains
		// the backstop.
		return false
	}
	if *verifyRetries < cfg.MaxVerifyRetries {
		*verifyRetries++
		res.Transcript = append(res.Transcript, oai.Message{
			Role:    oai.RoleUser,
			Content: feedback,
		})
		res.Terminal = nil
		return true
	}
	// Budget spent and the gate still fails: do not land a failing GO.
	res.Terminal = CoderGateFailedEnvelope(turn, *verifyRetries, feedback)
	return false
}

// runOneTurn drives one chat-completions request + tool dispatch. It
// appends to res.Transcript in place. Returns errTerminalReached when
// the model invoked the terminal (submit_result) tool. editSucceeded
// reports whether a write_file/str_replace landed this turn (used by the
// EditFreeStreak forcing function in Run).
func (l *Loop) runOneTurn(
	ctx context.Context, cfg LoopConfig, schemas []oai.Tool, res *LoopResult,
) (editSucceeded bool, err error) {
	ctx, span := l.tracer.Start(ctx, "foreman.agent.turn",
		trace.WithAttributes(attribute.Int("turn", res.Turns)))
	defer span.End()
	start := time.Now()

	req := oai.ChatRequest{
		Model: cfg.Model,
		Messages: stripReasoningForWire(
			selectWireTranscript(cfg, res.Transcript)),
		Tools:       schemas,
		Temperature: cfg.Temperature,
	}
	resp, err := l.client.Chat(ctx, req)
	if err != nil {
		span.RecordError(err)
		return false, fmt.Errorf("turn %d: chat: %w", res.Turns, err)
	}
	if len(resp.Choices) == 0 {
		err := fmt.Errorf("turn %d: %w", res.Turns, oai.ErrNoChoices)
		span.RecordError(err)
		return false, err
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
		// Distinguish a thinking model mid-reasoning (reasoning_content
		// present, no prose) from a model narrating its conclusion as
		// prose. The two get different correctives and budgets (#650).
		if msg.ReasoningContent != "" && strings.TrimSpace(msg.Content) == "" {
			err := fmt.Errorf("turn %d: %w", res.Turns, ErrAssistantReasoningOnly)
			span.RecordError(err)
			return false, err
		}
		err := fmt.Errorf("turn %d: %w", res.Turns, ErrAssistantNoToolCalls)
		span.RecordError(err)
		return false, err
	}

	terminal, editSucceeded := l.dispatchToolCalls(ctx, msg.ToolCalls, res)
	span.SetAttributes(
		attribute.Int("tool_calls", len(msg.ToolCalls)),
		attribute.Int64("elapsed_ms", time.Since(start).Milliseconds()),
		attribute.Bool("terminal", terminal != nil),
		attribute.Bool("edit_succeeded", editSucceeded),
	)
	if terminal != nil {
		res.Terminal = terminal
		return editSucceeded, errTerminalReached
	}
	return editSucceeded, nil
}

// dispatchToolCalls executes every tool_call in a single assistant turn
// (left-to-right) and appends one tool message per call. If multiple
// calls are present, all of them run; if one is the terminal call, the
// loop still completes the rest of the turn before exiting (the model
// authored them as one batch).
//
// Returns the first terminal *ToolResult observed (or nil) and whether any
// edit-producing tool (write_file, str_replace) dispatched successfully
// this turn. editSucceeded gates the EditFreeStreak forcing function: a
// str_replace that errors with "old_string found 0 times" must NOT count as
// progress, or a model that guesses the wrong text escapes the forcing
// phase without ever landing a fix.
func (l *Loop) dispatchToolCalls(
	ctx context.Context, calls []oai.ToolCall, res *LoopResult,
) (terminal *ToolResult, editSucceeded bool) {
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
			// A successful write_file/str_replace is a real edit. (We
			// reuse editProducingTools but exclude submit_result here: a
			// successful submit short-circuits the loop before the
			// forcing-function check, so its edit status is moot.)
			if tc.Function.Name == "write_file" || tc.Function.Name == "str_replace" {
				editSucceeded = true
			}
		}
		res.Transcript = append(res.Transcript, oai.Message{
			Role:       oai.RoleTool,
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    content,
		})
	}
	return terminal, editSucceeded
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

// turnGroup is a half-open index range [start,end) into a transcript: a
// leading assistant/user message plus the tool results that answer it.
type turnGroup struct{ start, end int }

// compactTranscriptForWire implements the "session" context strategy: a
// stable, append-only prefix that lets a caching runtime reuse the prompt
// prefix across turns. Under budget the transcript is returned unchanged
// (cache-stable). Over budget, the oldest whole turn-groups in the middle
// are dropped, pinning the head (leading system messages + the first user
// task message) and the most recent turn-group, so the agent never loses
// its instructions, its task, or its latest work. Dropping is in
// turn-group units so a tool_call_id is never orphaned. See issue #756.
func compactTranscriptForWire(transcript []oai.Message, ctxBudget int) []oai.Message {
	if ctxBudget <= 0 || approxTokens(transcript) <= ctxBudget {
		return transcript
	}

	// headEnd is the exclusive index of the pinned head: all leading
	// system messages plus the first user message (the task).
	headEnd := 0
	for headEnd < len(transcript) && transcript[headEnd].Role == oai.RoleSystem {
		headEnd++
	}
	if headEnd < len(transcript) && transcript[headEnd].Role == oai.RoleUser {
		headEnd++
	}

	// Partition the tail into turn-groups: each starts at a non-tool
	// message and absorbs the tool messages that follow it.
	var groups []turnGroup
	for i := headEnd; i < len(transcript); {
		start := i
		i++ // leading assistant/user message
		for i < len(transcript) && transcript[i].Role == oai.RoleTool {
			i++
		}
		groups = append(groups, turnGroup{start, i})
	}

	if len(groups) <= 1 {
		return transcript // 0 or 1 groups: nothing to drop (head + at most one group is the floor)
	}

	// Drop oldest groups (after head, before the last group) until under
	// budget or only the last group remains.
	for dropCount := 1; dropCount < len(groups); dropCount++ {
		kept := assembleSessionWire(transcript, headEnd, groups, dropCount)
		if approxTokens(kept) <= ctxBudget {
			return kept
		}
	}
	// Degenerate: only head + last group remain (still possibly over budget).
	return assembleSessionWire(transcript, headEnd, groups, len(groups)-1)
}

// selectWireTranscript routes the transcript through the configured
// context strategy. "session" uses compactTranscriptForWire (stable
// prefix, compact at budget); anything else (including "" and "window")
// uses maskTranscriptForWire (observation masking).
func selectWireTranscript(cfg LoopConfig, transcript []oai.Message) []oai.Message {
	if cfg.ContextStrategy == ContextStrategySession {
		return compactTranscriptForWire(transcript, cfg.ContextWindowTokens)
	}
	return maskTranscriptForWire(transcript, cfg.ObservationWindowTurns, cfg.ContextWindowTokens)
}

// assembleSessionWire builds the wire payload: the pinned head
// (transcript[:headEnd]) followed by every turn-group from index
// dropCount onward (the oldest dropCount groups are omitted).
func assembleSessionWire(transcript []oai.Message, headEnd int, groups []turnGroup, dropCount int) []oai.Message {
	out := make([]oai.Message, 0, len(transcript))
	out = append(out, transcript[:headEnd]...)
	for g := dropCount; g < len(groups); g++ {
		out = append(out, transcript[groups[g].start:groups[g].end]...)
	}
	return out
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
