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

// Whitebox tests for the v0.3 #558 observation-masking helpers in
// loop.go. These pin the masking behavior in isolation; loop_test.go
// covers end-to-end loop behavior with a fake OAI server.

import (
	"strings"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// transcriptFixture builds a transcript of: system + user + (assistant +
// 1 tool result) * nToolTurns. Each tool message carries `toolBytes`
// bytes of content so the test can assert on masking decisions.
func transcriptFixture(nToolTurns, toolBytes int) []oai.Message {
	tx := []oai.Message{
		{Role: oai.RoleSystem, Content: "you are a coder"},
		{Role: oai.RoleUser, Content: "fix issue 510"},
	}
	body := strings.Repeat("x", toolBytes)
	for i := 0; i < nToolTurns; i++ {
		callID := "call_" + string(rune('a'+i))
		tx = append(tx,
			oai.Message{
				Role: oai.RoleAssistant,
				ToolCalls: []oai.ToolCall{
					{
						ID:       callID,
						Type:     "function",
						Function: oai.ToolCallFunction{Name: "bash", Arguments: `{}`},
					},
				},
			},
			oai.Message{
				Role:       oai.RoleTool,
				ToolCallID: callID,
				Name:       "bash",
				Content:    body,
			},
		)
	}
	return tx
}

// TestMaskTranscript_ObsWindowZeroDisablesMasking pins back-compat: if
// the caller hasn't set ObservationWindowTurns, the wire payload is
// the transcript verbatim (v0.1 behavior).
func TestMaskTranscript_ObsWindowZeroDisablesMasking(t *testing.T) {
	tx := transcriptFixture(5, 1000)
	wire := maskTranscriptForWire(tx, 0, 0)
	if len(wire) != len(tx) {
		t.Fatalf("wire length: want %d got %d", len(tx), len(wire))
	}
	for i := range tx {
		if wire[i].Content != tx[i].Content {
			t.Errorf("message %d: content modified despite obsWindow=0", i)
		}
	}
}

// TestMaskTranscript_KeepsObsWindowFull is the core invariant: the
// last obsWindow tool messages stay verbatim regardless of how big
// they are or how old the transcript is.
func TestMaskTranscript_KeepsObsWindowFull(t *testing.T) {
	tx := transcriptFixture(7, 2000) // 7 assistant+tool pairs
	wire := maskTranscriptForWire(tx, 3, 0)

	if len(wire) != len(tx) {
		t.Fatalf("wire length: want %d got %d", len(tx), len(wire))
	}

	// Walk tool messages newest-first; the first 3 we hit stay full.
	keptFull := 0
	maskedOlder := 0
	for i := len(wire) - 1; i >= 0; i-- {
		if wire[i].Role != oai.RoleTool {
			continue
		}
		if keptFull < 3 {
			if wire[i].Content != tx[i].Content {
				t.Errorf("tool message at idx %d: in obs window but content modified", i)
			}
			keptFull++
		} else {
			if wire[i].Content == tx[i].Content {
				t.Errorf("tool message at idx %d: outside obs window but content unchanged", i)
			}
			if !strings.Contains(wire[i].Content, "truncated") {
				t.Errorf("tool message at idx %d: masked content missing 'truncated' marker: %q", i, wire[i].Content)
			}
			maskedOlder++
		}
	}
	if keptFull != 3 {
		t.Errorf("expected exactly 3 tool messages kept full, got %d", keptFull)
	}
	if maskedOlder != 4 {
		t.Errorf("expected 4 older tool messages masked, got %d", maskedOlder)
	}
}

// TestMaskTranscript_PreservesNonToolMessages pins the rule: system,
// user, and assistant messages are NEVER masked, even when their
// content is large. Masking those would lose the model's understanding
// of what it's been asked to do or what it has already decided.
func TestMaskTranscript_PreservesNonToolMessages(t *testing.T) {
	tx := []oai.Message{
		{Role: oai.RoleSystem, Content: strings.Repeat("S", 10000)},
		{Role: oai.RoleUser, Content: strings.Repeat("U", 10000)},
		{Role: oai.RoleAssistant, Content: strings.Repeat("A", 10000)},
		{Role: oai.RoleTool, Name: "bash", ToolCallID: "c1", Content: strings.Repeat("T", 10000)},
	}
	// obsWindow=0 disables masking entirely; the tiny budget would
	// otherwise mask the tool message but the disable flag wins.
	wire := maskTranscriptForWire(tx, 0, 100)
	for i, m := range tx {
		if wire[i].Content != m.Content {
			t.Errorf("idx %d (role=%s): masked when obsWindow=0 disabled masking", i, m.Role)
		}
	}
}

// TestMaskTranscript_FewerToolsThanWindowKeepsAllFull pins the edge
// case: when there are fewer tool messages than the observation
// window, all of them stay full (no masking happens).
func TestMaskTranscript_FewerToolsThanWindowKeepsAllFull(t *testing.T) {
	tx := transcriptFixture(2, 500) // 2 tool turns, window=5
	wire := maskTranscriptForWire(tx, 5, 0)
	for i := range tx {
		if wire[i].Content != tx[i].Content {
			t.Errorf("idx %d (role=%s): content modified despite tool count < window", i, tx[i].Role)
		}
	}
}

// TestMaskTranscript_PreservesToolCallIDAndName confirms that masked
// tool messages still carry their Role/ToolCallID/Name so the OAI
// server can thread the tool_call_id back to its assistant
// invocation. A masked tool message that loses these fields would
// cause a 400 on strict-schema servers (e.g. Devstral after the #556
// fix lands).
func TestMaskTranscript_PreservesToolCallIDAndName(t *testing.T) {
	tx := transcriptFixture(5, 2000)
	wire := maskTranscriptForWire(tx, 1, 0) // window=1: 4 of 5 tool msgs masked

	for i, m := range wire {
		if m.Role != oai.RoleTool {
			continue
		}
		orig := tx[i]
		if m.Role != orig.Role {
			t.Errorf("idx %d: Role lost (want %q got %q)", i, orig.Role, m.Role)
		}
		if m.ToolCallID != orig.ToolCallID {
			t.Errorf("idx %d: ToolCallID lost (want %q got %q)", i, orig.ToolCallID, m.ToolCallID)
		}
		if m.Name != orig.Name {
			t.Errorf("idx %d: Name lost (want %q got %q)", i, orig.Name, m.Name)
		}
	}
}

// TestMaskTranscript_DoesNotMutateInput pins the contract: the
// caller's transcript slice is not modified. The persisted ConfigMap
// must see the full unmasked history; masking is wire-only.
func TestMaskTranscript_DoesNotMutateInput(t *testing.T) {
	tx := transcriptFixture(4, 1000)
	original := make([]oai.Message, len(tx))
	copy(original, tx)
	// Snapshot Content slices too (the assertion below is by value, but
	// a future bug that mutates m.Content via index would fail this).
	originalContents := make([]string, len(tx))
	for i := range tx {
		originalContents[i] = tx[i].Content
	}

	_ = maskTranscriptForWire(tx, 1, 100)

	for i := range tx {
		if tx[i].Content != originalContents[i] {
			t.Errorf("idx %d: caller's transcript[i].Content was mutated", i)
		}
	}
}

// TestMaskedToolMessage_ContainsByteCount lets the model see roughly
// how much was truncated, so it can decide whether to re-run the
// tool. Pinning this so we don't silently regress the masked header.
func TestMaskedToolMessage_ContainsByteCount(t *testing.T) {
	body := strings.Repeat("x", 12345)
	m := oai.Message{Role: oai.RoleTool, Name: "bash", ToolCallID: "c1", Content: body}
	masked := maskedToolMessage(m)
	if !strings.Contains(masked.Content, "12345") {
		t.Errorf("masked content missing byte count: %q", masked.Content)
	}
	if !strings.Contains(masked.Content, "bash") {
		t.Errorf("masked content missing tool name: %q", masked.Content)
	}
}

// TestApproxTokens_MonotonicWithInput sanity-checks the token estimator:
// more content yields a higher count. We don't pin specific numbers
// (the heuristic can be tuned), only the monotonicity invariant the
// budget bound relies on.
func TestApproxTokens_MonotonicWithInput(t *testing.T) {
	small := []oai.Message{
		{Role: oai.RoleUser, Content: "short"},
	}
	large := []oai.Message{
		{Role: oai.RoleUser, Content: strings.Repeat("x", 4000)},
	}
	if approxTokens(small) >= approxTokens(large) {
		t.Errorf("approxTokens not monotonic: small=%d large=%d", approxTokens(small), approxTokens(large))
	}
	if approxTokens(nil) != 0 {
		t.Errorf("approxTokens(nil) should be 0, got %d", approxTokens(nil))
	}
}
