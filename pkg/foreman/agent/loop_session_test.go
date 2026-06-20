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

func TestCompactTranscript_DegenerateKeepsHeadAndLastGroup(t *testing.T) {
	tx := transcriptFixture(4, 8000)          // each tool ~2000 tokens
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
