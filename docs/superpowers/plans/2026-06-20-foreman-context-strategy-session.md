# Foreman `contextStrategy` session mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a per-Agent `contextStrategy: window|session` field so large-context models can keep a stable, cache-friendly append-only prompt prefix instead of forced observation masking.

**Architecture:** A new pure function `compactTranscriptForWire` implements the session strategy (passthrough under budget; drop oldest middle turn-groups, pinning head + newest group, when over budget). A thin `selectWireTranscript` branches on `LoopConfig.ContextStrategy` at the single existing wire-build callsite (`loop.go:744`). The window path (`maskTranscriptForWire`) is untouched. A new CRD field is threaded through the executor. Spec: `docs/superpowers/specs/2026-06-20-foreman-context-strategy-session-design.md`. Tracks issue #756.

**Tech Stack:** Go 1.25, Kubebuilder/controller-runtime CRDs, `oai.Message` chat types, whitebox `go test` (no envtest needed for these unit tests).

---

## Background the engineer needs

- **The chokepoint.** Every turn builds its request at `pkg/foreman/agent/loop.go:741-747`. The message list is `stripReasoningForWire(maskTranscriptForWire(res.Transcript, cfg.ObservationWindowTurns, cfg.ContextWindowTokens))`. We will replace the inner `maskTranscriptForWire(...)` call with `selectWireTranscript(cfg, res.Transcript)` and leave `stripReasoningForWire(...)` wrapping it.
- **`oai.Message`** (`pkg/foreman/agent/oai/types.go:55`): fields `Role Role`, `Content string`, `ToolCalls []ToolCall`, `ToolCallID string`, `Name string`, `ReasoningContent string`. Roles: `oai.RoleSystem`, `oai.RoleUser`, `oai.RoleAssistant`, `oai.RoleTool`.
- **Transcript shape**: `[system]`, `[user task]`, then repeating `[assistant (with ToolCalls)]`, `[tool result]`. An assistant message and the tool results that answer it must travel together or the OAI server rejects an orphaned `tool_call_id`.
- **Existing helper** `approxTokens(messages []oai.Message) int` (`loop.go:973`) is a chars/4 estimate. Reuse it; do not write a new one.
- **Test fixture** `transcriptFixture(nToolTurns, toolBytes int) []oai.Message` already exists in `loop_masking_test.go:33` and builds exactly the shape above (system + user + n*(assistant+tool), each tool carrying `toolBytes` bytes). Reuse it.
- **Running unit tests without envtest:** the `agent` package has both pure and envtest-backed tests. Run only the new ones with a `-run` filter, e.g. `go test ./pkg/foreman/agent/ -run TestCompact -v`. Do NOT run the whole package (it would pull envtest).

---

## File structure

- `pkg/foreman/agent/loop.go` — add `turnGroup` type, `compactTranscriptForWire`, `assembleSessionWire`, `selectWireTranscript`, strategy constants, `LoopConfig.ContextStrategy`; change the callsite.
- `pkg/foreman/agent/loop_session_test.go` — **new** whitebox test file for the session helpers.
- `api/foreman/v1alpha1/agent_types.go` — add `ContextStrategy` field.
- `pkg/foreman/agent/executor_native.go` — map `agent.Spec.ContextStrategy` into `LoopConfig`.
- `config/crd/bases/foreman.llmkube.dev_agents.yaml` + `charts/foreman/templates/crds/...agents.yaml` — regenerated, not hand-edited.
- `docs/site/foreman/model-compatibility.md` — operator note (caps tuning + tradeoff).

---

### Task 1: `compactTranscriptForWire` passthrough (cache-stability invariant)

**Files:**
- Create: `pkg/foreman/agent/loop_session_test.go`
- Modify: `pkg/foreman/agent/loop.go` (add new function near `maskTranscriptForWire`, ~line 945)

- [ ] **Step 1: Write the failing test**

Create `pkg/foreman/agent/loop_session_test.go`:

```go
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

// Whitebox tests for the "session" context strategy helpers
// (compactTranscriptForWire / selectWireTranscript) in loop.go. These
// pin the cache-stability and compaction behavior in isolation.

import (
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// assertNoOrphanedToolMessages fails if any RoleTool message in wire has
// a ToolCallID without a preceding assistant ToolCall of the same ID.
func assertNoOrphanedToolMessages(t *testing.T, wire []oai.Message) {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range wire {
		if m.Role == oai.RoleAssistant {
			for _, tc := range m.ToolCalls {
				seen[tc.ID] = true
			}
		}
		if m.Role == oai.RoleTool && !seen[m.ToolCallID] {
			t.Errorf("orphaned tool message: tool_call_id %q has no preceding assistant call", m.ToolCallID)
		}
	}
}

func TestCompactTranscript_UnderBudgetReturnsIdentical(t *testing.T) {
	tx := transcriptFixture(5, 200) // small, well under budget
	wire := compactTranscriptForWire(tx, 100000)
	if len(wire) != len(tx) {
		t.Fatalf("wire length: want %d got %d", len(tx), len(wire))
	}
	for i := range tx {
		if wire[i].Content != tx[i].Content || wire[i].Role != tx[i].Role {
			t.Errorf("message %d changed under budget (cache-stability violated)", i)
		}
	}
}

func TestCompactTranscript_ZeroBudgetReturnsIdentical(t *testing.T) {
	tx := transcriptFixture(5, 200)
	wire := compactTranscriptForWire(tx, 0)
	if len(wire) != len(tx) {
		t.Fatalf("wire length: want %d got %d", len(tx), len(wire))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/foreman/agent/ -run TestCompactTranscript -v`
Expected: FAIL — `undefined: compactTranscriptForWire`.

- [ ] **Step 3: Write minimal implementation**

In `pkg/foreman/agent/loop.go`, after `maskedToolMessage` (~line 972), add:

```go
// compactTranscriptForWire implements the "session" context strategy: a
// stable, append-only prefix that lets a caching runtime reuse the prompt
// prefix across turns. Under budget the transcript is returned unchanged
// (cache-stable). Over budget, the oldest whole turn-groups in the middle
// are dropped, pinning the head (leading system messages + the first user
// task message) and the most recent turn-group, so the agent never loses
// its instructions, its task, or its latest work. Dropping is in
// turn-group units so a tool_call_id is never orphaned. See issue #756.
func compactTranscriptForWire(transcript []oai.Message, ctxBudget int) []oai.Message {
	// Compaction is added in a later task; for now always passthrough.
	return transcript
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/foreman/agent/ -run TestCompactTranscript -v`
Expected: PASS (both passthrough tests).

- [ ] **Step 5: Commit**

```bash
git add pkg/foreman/agent/loop.go pkg/foreman/agent/loop_session_test.go
git commit -s -m "feat(foreman): compactTranscriptForWire passthrough for session strategy (#756)"
```

---

### Task 2: `compactTranscriptForWire` compaction (drop oldest middle turn-groups)

**Files:**
- Modify: `pkg/foreman/agent/loop.go` (replace the placeholder return; add `assembleSessionWire`)
- Modify: `pkg/foreman/agent/loop_session_test.go` (add compaction test)

- [ ] **Step 1: Write the failing test**

Append to `pkg/foreman/agent/loop_session_test.go`:

```go
func TestCompactTranscript_OverBudgetDropsOldestMiddle(t *testing.T) {
	// 8 turn-groups, each tool ~4000 bytes (~1000 tokens). Head is tiny.
	tx := transcriptFixture(8, 4000)
	budget := 3500 // tokens: keeps head + a few newest groups
	wire := compactTranscriptForWire(tx, budget)

	// Something was dropped.
	if len(wire) >= len(tx) {
		t.Fatalf("expected compaction to drop messages: in=%d out=%d", len(tx), len(wire))
	}
	// Head preserved: system then the original task message.
	if wire[0].Role != oai.RoleSystem {
		t.Fatalf("head[0] not system: %s", wire[0].Role)
	}
	if wire[1].Role != oai.RoleUser || wire[1].Content != "fix issue 510" {
		t.Fatalf("original task message not pinned: %+v", wire[1])
	}
	// Most recent turn-group preserved: last two messages of tx survive
	// as the last two of wire.
	if wire[len(wire)-1].Content != tx[len(tx)-1].Content {
		t.Errorf("most recent tool result not kept")
	}
	if wire[len(wire)-2].Role != oai.RoleAssistant {
		t.Errorf("most recent assistant turn not kept")
	}
	// Under budget after compaction.
	if approxTokens(wire) > budget {
		t.Errorf("still over budget after compaction: %d > %d", approxTokens(wire), budget)
	}
	// No orphaned tool_call_id.
	assertNoOrphanedToolMessages(t, wire)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/foreman/agent/ -run TestCompactTranscript_OverBudget -v`
Expected: FAIL — placeholder returns the full transcript, so `len(wire) >= len(tx)` fails.

- [ ] **Step 3: Write minimal implementation**

In `pkg/foreman/agent/loop.go`, add the `turnGroup` type just above `compactTranscriptForWire`:

```go
// turnGroup is a half-open index range [start,end) into a transcript: a
// leading assistant/user message plus the tool results that answer it.
type turnGroup struct{ start, end int }
```

Then replace the entire `compactTranscriptForWire` function from Task 1 (the
passthrough stub) with the full implementation below, and add
`assembleSessionWire` directly after it (keep the existing godoc comment above
the function):

```go
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
		return transcript // nothing droppable; degenerate guard handled by caller budget
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/foreman/agent/ -run TestCompactTranscript -v`
Expected: PASS (passthrough + compaction).

- [ ] **Step 5: Commit**

```bash
git add pkg/foreman/agent/loop.go pkg/foreman/agent/loop_session_test.go
git commit -s -m "feat(foreman): session compaction drops oldest middle turn-groups (#756)"
```

---

### Task 3: degenerate case (head + newest group over budget)

**Files:**
- Modify: `pkg/foreman/agent/loop_session_test.go` (add degenerate test)

No code change expected — Task 2's final `return assembleSessionWire(..., len(groups)-1)` already handles it. This task pins that behavior so it cannot regress.

- [ ] **Step 1: Write the test**

Append to `pkg/foreman/agent/loop_session_test.go`:

```go
func TestCompactTranscript_DegenerateKeepsHeadAndLastGroup(t *testing.T) {
	tx := transcriptFixture(4, 8000) // each tool ~2000 tokens
	wire := compactTranscriptForWire(tx, 100) // absurdly small budget

	if wire[0].Role != oai.RoleSystem || wire[1].Role != oai.RoleUser {
		t.Fatalf("head not preserved: %+v", wire[:2])
	}
	// Last turn-group (assistant + tool) preserved as the final two messages.
	if wire[len(wire)-1].Role != oai.RoleTool || wire[len(wire)-2].Role != oai.RoleAssistant {
		t.Fatalf("last turn-group not preserved: tail=%+v", wire[len(wire)-2:])
	}
	assertNoOrphanedToolMessages(t, wire)
}
```

- [ ] **Step 2: Run test to verify it passes**

Run: `go test ./pkg/foreman/agent/ -run TestCompactTranscript_Degenerate -v`
Expected: PASS (no implementation change needed; if it fails, the bug is in Task 2's degenerate return).

- [ ] **Step 3: Commit**

```bash
git add pkg/foreman/agent/loop_session_test.go
git commit -s -m "test(foreman): pin session compaction degenerate case (#756)"
```

---

### Task 4: `selectWireTranscript` routing + strategy constants + callsite

**Files:**
- Modify: `pkg/foreman/agent/loop.go` (constants, `LoopConfig.ContextStrategy`, `selectWireTranscript`, callsite at `:744`)
- Modify: `pkg/foreman/agent/loop_session_test.go` (routing tests)

- [ ] **Step 1: Write the failing test**

Append to `pkg/foreman/agent/loop_session_test.go`:

```go
func TestSelectWireTranscript_SessionDoesNotMask(t *testing.T) {
	tx := transcriptFixture(6, 4000)
	cfg := LoopConfig{ContextStrategy: ContextStrategySession, ContextWindowTokens: 1000000}
	wire := selectWireTranscript(cfg, tx)
	for i := range tx {
		if wire[i].Content != tx[i].Content {
			t.Errorf("session strategy masked content at %d", i)
		}
	}
}

func TestSelectWireTranscript_WindowMasks(t *testing.T) {
	tx := transcriptFixture(6, 4000)
	cfg := LoopConfig{ContextStrategy: ContextStrategyWindow, ObservationWindowTurns: 1}
	wire := selectWireTranscript(cfg, tx)
	changed := false
	for i := range tx {
		if tx[i].Role == oai.RoleTool && wire[i].Content != tx[i].Content {
			changed = true
		}
	}
	if !changed {
		t.Error("window strategy did not mask any tool messages")
	}
}

func TestSelectWireTranscript_EmptyDefaultsToWindow(t *testing.T) {
	tx := transcriptFixture(6, 4000)
	got := selectWireTranscript(LoopConfig{ObservationWindowTurns: 1}, tx)
	want := maskTranscriptForWire(tx, 1, 0)
	if len(got) != len(want) {
		t.Fatalf("empty strategy did not route to window: len got=%d want=%d", len(got), len(want))
	}
	for i := range got {
		if got[i].Content != want[i].Content {
			t.Errorf("message %d differs from window output", i)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/foreman/agent/ -run TestSelectWireTranscript -v`
Expected: FAIL — `undefined: ContextStrategySession` / `selectWireTranscript` / `LoopConfig.ContextStrategy`.

- [ ] **Step 3: Write minimal implementation**

In `pkg/foreman/agent/loop.go`, add the constants near the other loop defaults (just below `DefaultObservationWindowTurns`, ~line 181):

```go
const (
	// ContextStrategyWindow applies observation masking bounded by
	// ObservationWindowTurns (the default strategy).
	ContextStrategyWindow = "window"
	// ContextStrategySession keeps a stable, append-only prefix and
	// compacts only at the ContextWindowTokens ceiling. See issue #756.
	ContextStrategySession = "session"
)
```

Add the field to `LoopConfig` (after `ObservationWindowTurns`, ~line 105):

```go
	// ContextStrategy selects the wire-payload builder. "" or "window"
	// applies observation masking (ObservationWindowTurns); "session"
	// keeps an append-only prefix and compacts at ContextWindowTokens.
	// See selectWireTranscript and issue #756.
	ContextStrategy string
```

Add `selectWireTranscript` next to `compactTranscriptForWire`:

```go
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
```

Change the callsite at `loop.go:743-744` from:

```go
		Messages: stripReasoningForWire(
			maskTranscriptForWire(res.Transcript, cfg.ObservationWindowTurns, cfg.ContextWindowTokens)),
```

to:

```go
		Messages: stripReasoningForWire(
			selectWireTranscript(cfg, res.Transcript)),
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/foreman/agent/ -run 'TestSelectWireTranscript|TestCompactTranscript|TestMaskTranscript' -v`
Expected: PASS (new routing tests + existing masking tests, proving no regression).

- [ ] **Step 5: Verify build**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add pkg/foreman/agent/loop.go pkg/foreman/agent/loop_session_test.go
git commit -s -m "feat(foreman): selectWireTranscript routes window vs session strategy (#756)"
```

---

### Task 5: CRD field + executor mapping + regenerate manifests

**Files:**
- Modify: `api/foreman/v1alpha1/agent_types.go` (add `ContextStrategy`)
- Modify: `pkg/foreman/agent/executor_native.go` (map into `LoopConfig`)
- Regenerated: `config/crd/bases/foreman.llmkube.dev_agents.yaml`, `charts/foreman/templates/crds/foreman.llmkube.dev_agents.yaml`

- [ ] **Step 1: Add the CRD field**

In `api/foreman/v1alpha1/agent_types.go`, immediately after the `ObservationWindowTurns` field (~line 257), add:

```go
	// ContextStrategy selects how the loop builds each request's message
	// list. "window" (default) applies observation masking bounded by
	// ObservationWindowTurns: older tool results are masked, keeping the
	// payload small for small-context models but rewriting the prefix each
	// turn (which defeats prompt caching). "session" keeps a stable,
	// append-only prefix so a caching runtime reuses it across turns, and
	// compacts (drops the oldest middle turns, pinning the system prompt,
	// the task, and the most recent turn) only when the payload approaches
	// ContextWindowTokens. Use "session" for large-context models on
	// caching runtimes; set ContextHardCap >= the server context size so a
	// healthy deep session is not aborted early. See issue #756.
	// +kubebuilder:validation:Enum=window;session
	// +kubebuilder:default=window
	// +optional
	ContextStrategy string `json:"contextStrategy,omitempty"`
```

- [ ] **Step 2: Map it in the executor**

In `pkg/foreman/agent/executor_native.go`, in the `cfg := LoopConfig{...}` literal (~line 375), add after the `ObservationWindowTurns:` line:

```go
		ContextStrategy:        agent.Spec.ContextStrategy,
```

- [ ] **Step 3: Regenerate CRDs (deepcopy + manifests + foreman chart)**

Run: `make manifests generate foreman-chart-crds`
Expected: `config/crd/bases/foreman.llmkube.dev_agents.yaml` and `charts/foreman/templates/crds/foreman.llmkube.dev_agents.yaml` now contain a `contextStrategy` property with `enum: [window, session]` and `default: window`.

- [ ] **Step 4: Verify the generated CRD and build**

Run: `grep -A3 'contextStrategy' config/crd/bases/foreman.llmkube.dev_agents.yaml`
Expected: shows the enum + default.

Run: `git status --porcelain`
Expected: only the four files above changed (types, executor, two CRDs); no other drift. If other files changed, the generated output is stale elsewhere — investigate before committing.

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 5: Commit**

```bash
git add api/foreman/v1alpha1/agent_types.go pkg/foreman/agent/executor_native.go config/crd/bases/foreman.llmkube.dev_agents.yaml charts/foreman/templates/crds/foreman.llmkube.dev_agents.yaml
git commit -s -m "feat(foreman): add Agent.spec.contextStrategy field (window|session) (#756)"
```

---

### Task 6: Operator documentation

**Files:**
- Modify: `docs/site/foreman/model-compatibility.md`

- [ ] **Step 1: Add a context-strategy section**

Append to `docs/site/foreman/model-compatibility.md`:

```markdown
## Context strategy: window vs session

Foreman builds each turn's request from the running transcript using one of
two strategies, set per Agent via `spec.contextStrategy`.

**`window` (default).** Observation masking: tool results older than
`observationWindowTurns` are masked to a header, bounding the payload. Correct
for small-context models. Because masking rewrites the older part of the
prompt every turn, it defeats prompt caching on runtimes that support it.

**`session`.** A stable, append-only prefix. Nothing is masked, so a caching
runtime (for example llama.cpp's prompt cache) reuses the prefix and only
prefills the new tokens each turn. When the payload approaches
`contextWindowTokens`, Foreman compacts by dropping the oldest middle turns,
always keeping the system prompt, the original task, and the most recent turn.

Use `session` for large-context models on caching runtimes. Two settings
matter:

- Set `stuckLoopDetection.contextHardCap` at or above the server's context
  size (`n_ctx`) and `contextSoftCap` proportionally, so a healthy deep
  session is not aborted before it reaches the ceiling.
- Tradeoff: `session` trades an occasional cold re-prefill (one per compaction
  event, rare when the ceiling is high) for cheap, cache-hit steady-state
  turns. `window` makes the opposite trade.
```

- [ ] **Step 2: Commit**

```bash
git add docs/site/foreman/model-compatibility.md
git commit -s -m "docs(foreman): document window vs session context strategy (#756)"
```

---

### Task 7: Final verification

- [ ] **Step 1: Run the full new test set**

Run: `go test ./pkg/foreman/agent/ -run 'TestCompactTranscript|TestSelectWireTranscript|TestMaskTranscript' -v`
Expected: all PASS.

- [ ] **Step 2: Build + lint (cross-arch, per AGENTS.md)**

Run: `go build ./... && go vet ./pkg/foreman/... && GOOS=linux ./bin/golangci-lint run ./pkg/foreman/... ./api/foreman/...`
Expected: clean. (Install golangci-lint via `make golangci-lint` if `./bin/golangci-lint` is missing.)

- [ ] **Step 3: Confirm CRD sync**

Run: `make foreman-chart-crds && git status --porcelain`
Expected: no changes (the chart CRD already matches `config/crd/bases`). A diff means Task 5's regen was incomplete.
