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
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// makeCall is a test helper for one tool_call with deterministic
// fields. Tests vary name/args to control duplicate detection.
func makeCall(id, name, args string) oai.ToolCall {
	return oai.ToolCall{
		ID:       id,
		Type:     "function",
		Function: oai.ToolCallFunction{Name: name, Arguments: args},
	}
}

// TestProgressMonitor_DefaultsAreActive locks in the documented
// defaults so a future ProgressConfig refactor cannot quietly disable
// them without updating this test.
func TestProgressMonitor_DefaultsAreActive(t *testing.T) {
	if DefaultProgressConfig.RepeatedToolThreshold <= 0 {
		t.Errorf("RepeatedToolThreshold should be > 0; got %d", DefaultProgressConfig.RepeatedToolThreshold)
	}
	if DefaultProgressConfig.EditFreeTurnsLimit <= 0 {
		t.Errorf("EditFreeTurnsLimit should be > 0; got %d", DefaultProgressConfig.EditFreeTurnsLimit)
	}
	if DefaultProgressConfig.ContextSoftCap <= 0 || DefaultProgressConfig.ContextHardCap <= 0 {
		t.Errorf("context caps should be > 0; got soft=%d hard=%d",
			DefaultProgressConfig.ContextSoftCap, DefaultProgressConfig.ContextHardCap)
	}
	if DefaultProgressConfig.ContextSoftCap >= DefaultProgressConfig.ContextHardCap {
		t.Errorf("soft cap must be < hard cap; got soft=%d hard=%d",
			DefaultProgressConfig.ContextSoftCap, DefaultProgressConfig.ContextHardCap)
	}
}

// TestProgressMonitor_RepeatedToolNudgesThenTerminates exercises the
// canonical stuck-loop case: same tool, same args, 5 times. First five
// calls produce Continue/Nudge depending on the threshold; the next
// call escalates to ForceTerminate.
func TestProgressMonitor_RepeatedToolNudgesThenTerminates(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{
		RepeatedToolThreshold: 5,
	})
	args := `{"command":"git log | grep 449"}`
	transcript := []oai.Message{{Role: oai.RoleSystem, Content: "sys"}}

	// Turns 1-4: under threshold, expect Continue.
	for turn := 1; turn <= 4; turn++ {
		d := mon.Observe(turn, []oai.ToolCall{makeCall("t", "bash", args)}, transcript)
		if d.Action != ProgressContinue {
			t.Fatalf("turn %d: expected Continue; got %+v", turn, d)
		}
	}

	// Turn 5: 5th identical call -> hits threshold -> Nudge.
	d := mon.Observe(5, []oai.ToolCall{makeCall("t", "bash", args)}, transcript)
	if d.Action != ProgressNudge {
		t.Fatalf("turn 5: expected Nudge; got %+v", d)
	}
	if d.Signal != "RepeatedToolCall" {
		t.Errorf("turn 5: want signal RepeatedToolCall; got %q", d.Signal)
	}
	if !strings.Contains(d.Detail, "bash") {
		t.Errorf("turn 5 detail should mention tool name; got %q", d.Detail)
	}

	// Turn 6: same call again -> already nudged -> ForceTerminate.
	d = mon.Observe(6, []oai.ToolCall{makeCall("t", "bash", args)}, transcript)
	if d.Action != ProgressForceTerminate {
		t.Fatalf("turn 6: expected ForceTerminate; got %+v", d)
	}
	if d.Signal != "RepeatedToolCall" {
		t.Errorf("turn 6: want signal RepeatedToolCall; got %q", d.Signal)
	}
}

// TestProgressMonitor_RecoveryAfterNudge confirms the model gets one
// chance: if the nudge is heeded (different call on the next turn),
// the monitor does NOT escalate to ForceTerminate.
//
// Note: the RepeatedToolCall nudge flag stays set for the remainder
// of the run (we don't re-arm it after a recovery); the model has
// already shown the pattern once and we want any future re-emergence
// of the same pattern to escalate immediately rather than starting
// the nudge ladder over.
func TestProgressMonitor_RecoveryAfterNudge(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{
		RepeatedToolThreshold: 3,
	})
	transcript := []oai.Message{}
	stuckArgs := `{"command":"git status"}`

	// Build up 3 identical calls to trigger Nudge.
	_ = mon.Observe(1, []oai.ToolCall{makeCall("a", "bash", stuckArgs)}, transcript)
	_ = mon.Observe(2, []oai.ToolCall{makeCall("b", "bash", stuckArgs)}, transcript)
	d := mon.Observe(3, []oai.ToolCall{makeCall("c", "bash", stuckArgs)}, transcript)
	if d.Action != ProgressNudge {
		t.Fatalf("expected Nudge at turn 3; got %+v", d)
	}

	// Turn 4: model heeds nudge, calls a different tool with different
	// args. Should be Continue.
	d = mon.Observe(4, []oai.ToolCall{makeCall("d", "read_file", `{"path":"AGENTS.md"}`)}, transcript)
	if d.Action != ProgressContinue {
		t.Fatalf("turn 4: expected Continue after recovery; got %+v", d)
	}
}

// TestProgressMonitor_EditFreeTurnsTriggers verifies the edit-free
// streak signal. A coder Agent that reads/grep's for N turns without
// any edit-producing tool calls should be nudged, then force-
// terminated.
func TestProgressMonitor_EditFreeTurnsTriggers(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{
		EditFreeTurnsLimit: 5,
	})
	transcript := []oai.Message{}

	// 4 read-only turns: under limit.
	for turn := 1; turn <= 4; turn++ {
		d := mon.Observe(turn, []oai.ToolCall{makeCall("x", "read_file", `{"path":"foo"}`)}, transcript)
		if d.Action != ProgressContinue {
			t.Fatalf("turn %d: expected Continue; got %+v", turn, d)
		}
	}
	// Turn 5: 5 consecutive read-only turns -> hits limit -> Nudge.
	d := mon.Observe(5, []oai.ToolCall{makeCall("x", "read_file", `{"path":"foo"}`)}, transcript)
	if d.Action != ProgressNudge {
		t.Fatalf("turn 5: expected Nudge; got %+v", d)
	}
	if d.Signal != "EditFreeStreak" {
		t.Errorf("want signal EditFreeStreak; got %q", d.Signal)
	}
	// Turn 6: still no edit -> ForceTerminate.
	d = mon.Observe(6, []oai.ToolCall{makeCall("x", "grep", `{"pattern":"foo"}`)}, transcript)
	if d.Action != ProgressForceTerminate {
		t.Fatalf("turn 6: expected ForceTerminate; got %+v", d)
	}
}

// TestProgressMonitor_GroundingReadsExtendEditFreeLimit pins #1066: reading
// DISTINCT files to ground the next edit (the anti-confab rule, #1062) raises
// the effective edit-free limit, so grounding is not force-terminated like an
// open-ended search loop. At the base limit (turn 3) the guard must not fire
// because the distinct reads bought tolerance; the nudge comes later, once a
// no-new-file streak catches up to the extended limit.
func TestProgressMonitor_GroundingReadsExtendEditFreeLimit(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{EditFreeTurnsLimit: 3})
	tr := []oai.Message{}

	for i, f := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		d := mon.Observe(i+1, []oai.ToolCall{makeCall("t", "read_file", fmt.Sprintf(`{"path":%q}`, f))}, tr)
		if d.Action != ProgressContinue {
			t.Fatalf("turn %d (distinct read %s): grounding should extend the limit, got %+v", i+1, f, d)
		}
	}

	// No new files now: the effective limit stops growing and the streak
	// catches up, so it nudges -- but later than the base limit of 3.
	nudged := 0
	for turn := 6; turn <= 14; turn++ {
		d := mon.Observe(turn, []oai.ToolCall{makeCall("t", "grep", `{"pattern":"x"}`)}, tr)
		if d.Action == ProgressNudge && d.Signal == signalEditFreeStreak {
			nudged = turn
			break
		}
	}
	if nudged == 0 {
		t.Fatal("expected an EditFreeStreak nudge once the streak caught up to the extended limit")
	}
	if nudged <= 3 {
		t.Fatalf("nudge at turn %d; grounding should have pushed it past the base limit of 3", nudged)
	}
}

// TestProgressMonitor_GroundingBonusIsCapped pins #1066: the tolerance earned
// by distinct reads is bounded, so a pure-read loop still terminates rather
// than reading forever. Reading one distinct file per turn, the nudge lands at
// base + groundingReadBonusCap.
func TestProgressMonitor_GroundingBonusIsCapped(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{EditFreeTurnsLimit: 3})
	tr := []oai.Message{}
	nudged := 0
	for turn := 1; turn <= 30; turn++ {
		d := mon.Observe(turn, []oai.ToolCall{makeCall("t", "read_file", fmt.Sprintf(`{"path":"f%d.go"}`, turn))}, tr)
		if d.Action == ProgressNudge {
			nudged = turn
			break
		}
	}
	if want := 3 + groundingReadBonusCap; nudged != want {
		t.Fatalf("nudge at turn %d; want %d (base 3 + cap %d)", nudged, want, groundingReadBonusCap)
	}
}

// TestProgressMonitor_EditResetsGroundingBonus pins #1066: an edit clears the
// grounded-file set along with the streak, so a prior run of grounding reads
// does not carry tolerance into a later, unrelated stuck phase.
func TestProgressMonitor_EditResetsGroundingBonus(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{EditFreeTurnsLimit: 3})
	tr := []oai.Message{}

	for i, f := range []string{"a.go", "b.go", "c.go", "d.go"} {
		mon.Observe(i+1, []oai.ToolCall{makeCall("t", "read_file", fmt.Sprintf(`{"path":%q}`, f))}, tr)
	}
	// An edit resets the streak AND the grounded-file set.
	mon.Observe(5, []oai.ToolCall{makeCall("t", "write_file", `{"path":"a.go","content":"x"}`)}, tr)

	// A grep loop with no new grounding now trips at the BASE limit: streak
	// restarts at turn 6, so the nudge is at turn 8 (3 consecutive no-edit
	// turns), with no carried-over bonus.
	nudged := 0
	for turn := 6; turn <= 14; turn++ {
		d := mon.Observe(turn, []oai.ToolCall{makeCall("t", "grep", `{"pattern":"x"}`)}, tr)
		if d.Action == ProgressNudge {
			nudged = turn
			break
		}
	}
	if nudged != 8 {
		t.Fatalf("nudge at turn %d; want 8 (base limit 3 from the post-edit restart, no carried bonus)", nudged)
	}
}

// TestProgressMonitor_EditResetsStreak confirms a write_file (or
// str_replace, or submit_result) call resets the edit-free counter.
func TestProgressMonitor_EditResetsStreak(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{
		EditFreeTurnsLimit: 3,
	})
	transcript := []oai.Message{}

	// 2 reads, then an edit, then 2 more reads: streak should NOT
	// trip even though total read-turns = 4 (above the limit).
	_ = mon.Observe(1, []oai.ToolCall{makeCall("a", "read_file", `{}`)}, transcript)
	_ = mon.Observe(2, []oai.ToolCall{makeCall("b", "read_file", `{}`)}, transcript)
	d := mon.Observe(3, []oai.ToolCall{makeCall("c", "write_file", `{"path":"x","content":"y"}`)}, transcript)
	if d.Action != ProgressContinue {
		t.Fatalf("turn 3 (write_file): expected Continue; got %+v", d)
	}
	d = mon.Observe(4, []oai.ToolCall{makeCall("d", "read_file", `{}`)}, transcript)
	if d.Action != ProgressContinue {
		t.Fatalf("turn 4: expected Continue (streak reset); got %+v", d)
	}
}

// TestProgressMonitor_BashFileWriteResetsStreak verifies that creating
// or editing a workspace file through the bash tool (cat heredoc, sed
// -i, etc.) counts as an edit. Models commonly write files via the
// shell instead of the dedicated write_file/str_replace tools; without
// this the EditFreeStreak detector force-terminates a run that is
// actually making changes (observed with Gemma 4 on issue #522).
func TestProgressMonitor_BashFileWriteResetsStreak(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{EditFreeTurnsLimit: 3})
	transcript := []oai.Message{}

	// 2 reads, then a bash heredoc that writes a new file, then 2 reads:
	// the streak must reset on the bash write so it does NOT trip.
	_ = mon.Observe(1, []oai.ToolCall{makeCall("a", "read_file", `{}`)}, transcript)
	_ = mon.Observe(2, []oai.ToolCall{makeCall("b", "read_file", `{}`)}, transcript)
	heredoc := `{"command":"cat <<EOF > charts/foreman/templates/network-policy.yaml\nkind: NetworkPolicy\nEOF"}`
	d := mon.Observe(3, []oai.ToolCall{makeCall("c", "bash", heredoc)}, transcript)
	if d.Action != ProgressContinue {
		t.Fatalf("turn 3 (bash heredoc write): expected Continue; got %+v", d)
	}
	d = mon.Observe(4, []oai.ToolCall{makeCall("d", "read_file", `{}`)}, transcript)
	if d.Action != ProgressContinue {
		t.Fatalf("turn 4: expected Continue (streak reset by bash write); got %+v", d)
	}
	sedCmd := `{"command":"sed -i 's/a/b/' charts/foreman/values.yaml"}`
	d = mon.Observe(5, []oai.ToolCall{makeCall("e", "bash", sedCmd)}, transcript)
	if d.Action != ProgressContinue {
		t.Fatalf("turn 5 (sed -i): expected Continue; got %+v", d)
	}
}

// TestProgressMonitor_ReadOnlyBashStillTripsStreak verifies that
// read-only bash commands (ls, helm lint, cat, redirect to /dev/null)
// do NOT reset the streak, so a model that only inspects via the shell
// is still caught.
func TestProgressMonitor_ReadOnlyBashStillTripsStreak(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{EditFreeTurnsLimit: 3})
	transcript := []oai.Message{}
	cmds := []string{
		`{"command":"ls -R charts/"}`,
		`{"command":"helm lint charts/foreman"}`,
		`{"command":"go test ./... > /dev/null 2>&1"}`,
	}
	var d ProgressDecision
	for i, c := range cmds {
		d = mon.Observe(i+1, []oai.ToolCall{makeCall("x", "bash", c)}, transcript)
	}
	if d.Signal != "EditFreeStreak" {
		t.Fatalf("read-only bash should trip EditFreeStreak; got %+v", d)
	}
}

func TestBashLikelyMutatesWorkspace(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"cat <<EOF > foo.yaml\nx\nEOF", true},
		{"echo hi >> notes.txt", true},
		{"sed -i 's/a/b/' f", true},
		{"tee f.yaml", true},
		{"mv a b", true},
		{"cp a b", true},
		{"patch -p1 < x.diff", true},
		{"ls -R charts/", false},
		{"helm lint charts/foreman", false},
		{"cat foo.yaml", false},
		{"grep -r foo .", false},
		{"go test ./... > /dev/null 2>&1", false},
		{"echo done 2>&1", false},
		// #896 secondary: tokens must match at a word boundary, not as a
		// substring inside another word. "git add " contains "dd " (the dd
		// command token); a coder runs `git add` constantly, and a false reset
		// of the edit-free streak here masks genuine stuck loops.
		{"git add -A", false},
		{"git add -A && git commit -m fix", false},
		{"go build ./cmd/...", false}, // "cmd" must not trip "dd "/"cp "/etc.
		// True positives at a real word boundary must still match.
		{"dd if=/dev/zero of=out.bin", true},
		{"rm -rf x && tee y.txt", true},
	}
	for _, c := range cases {
		if got := bashLikelyMutatesWorkspace(c.cmd); got != c.want {
			t.Errorf("bashLikelyMutatesWorkspace(%q)=%v want %v", c.cmd, got, c.want)
		}
	}
}

// TestBashLikelyMutatesWorkspace_GitApplyAdded verifies that git apply commands
// are now detected as workspace mutations by bashLikelyMutatesWorkspace. This
// closes a gap identified in #982 where models editing via `git apply` would
// never reset the EditFreeStreak counter, causing force-terminate mid-edit.
func TestBashLikelyMutatesWorkspace_GitApplyAdded(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		expected bool
	}{
		// New: git apply patterns that were previously undetected.
		{"git apply patch", "git apply path/to/patch.diff", true},
		{"git apply --check then apply", "git apply --check && git apply path.patch", true},

		// Existing patterns (regression: must still work).
		{"sed -i in-place edit", "sed -i 's/foo/bar/g' file.txt", true},
		{"mv rename file", "mv old.go new.go", true},
		{"cp copy template", "cp templates/main.go pkg/agent/main.go", true},
		{"patch command", "patch -p1 < fix.patch", true},

		// Non-mutating: verification commands must NOT trigger.
		{"go build", "go build ./...", false},
		{"go vet package", "go vet ./pkg/agent/", false},
		{"git diff stat", "git diff --stat HEAD", false},
		{"echo to stdout (no redirect)", "echo hello world", false},

		// Edge cases handled by redirect check.
		{"cat heredoc with redirect", "cat <<EOF > file.go\npackage agent\nEOF", true},
		{"echo append to file", "echo 'new line' >> config.yaml", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := bashLikelyMutatesWorkspace(tt.command)
			if result != tt.expected {
				t.Errorf("bashLikelyMutatesWorkspace(%q) = %v, want %v",
					tt.command, result, tt.expected)
			}
		})
	}
}

// TestProgressMonitor_ContextHardCapImmediate verifies the hard cap
// force-terminates without a nudge stage.
func TestProgressMonitor_ContextHardCapImmediate(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{
		ContextSoftCap: 100,
		ContextHardCap: 200,
	})
	// Build a transcript that crosses the hard cap. Each char ~ 0.25
	// tokens via the chars/4 approximation; aim for >800 chars in one
	// content blob to be safely over 200 tokens.
	big := strings.Repeat("x", 1000)
	transcript := []oai.Message{
		{Role: oai.RoleSystem, Content: big},
	}
	d := mon.Observe(1, nil, transcript)
	if d.Action != ProgressForceTerminate {
		t.Fatalf("expected ForceTerminate on hard cap; got %+v", d)
	}
	if d.Signal != "ContextHardCap" {
		t.Errorf("want signal ContextHardCap; got %q", d.Signal)
	}
}

// TestProgressMonitor_ContextSoftCapNudgesThenEscalates verifies the
// soft cap nudges, then escalates on next still-over turn.
func TestProgressMonitor_ContextSoftCapNudgesThenEscalates(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{
		ContextSoftCap: 100,
		ContextHardCap: 100000, // far away, won't trip
	})
	// chars/4 approximation: ~500 chars puts us at ~125 tokens
	// (over soft cap of 100, under hard cap of 100000).
	mid := strings.Repeat("y", 500)
	transcript := []oai.Message{{Role: oai.RoleSystem, Content: mid}}

	d := mon.Observe(1, nil, transcript)
	if d.Action != ProgressNudge {
		t.Fatalf("turn 1: expected Nudge for soft cap; got %+v", d)
	}
	if d.Signal != "ContextSoftCap" {
		t.Errorf("want signal ContextSoftCap; got %q", d.Signal)
	}
	// Turn 2: still over soft cap -> escalate.
	d = mon.Observe(2, nil, transcript)
	if d.Action != ProgressForceTerminate {
		t.Fatalf("turn 2: expected ForceTerminate after soft-cap nudge; got %+v", d)
	}
}

// TestProgressMonitor_InvalidCapsDisableContextSignal confirms a
// misconfigured cap pair (soft >= hard) silently disables the
// context signal rather than producing inconsistent decisions.
func TestProgressMonitor_InvalidCapsDisableContextSignal(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{
		ContextSoftCap: 200,
		ContextHardCap: 100, // invalid: soft >= hard
	})
	huge := strings.Repeat("z", 100000)
	transcript := []oai.Message{{Role: oai.RoleSystem, Content: huge}}

	d := mon.Observe(1, nil, transcript)
	if d.Action != ProgressContinue {
		t.Errorf("invalid caps should disable context signal; got %+v", d)
	}
}

// TestProgressMonitor_AllDisabledIsNoop confirms that a zeroed config
// (every threshold = 0) returns Continue for any input.
func TestProgressMonitor_AllDisabledIsNoop(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{})
	transcript := []oai.Message{{Role: oai.RoleSystem, Content: strings.Repeat("a", 1000000)}}
	for turn := 1; turn <= 100; turn++ {
		d := mon.Observe(turn, []oai.ToolCall{makeCall("t", "bash", `{"command":"x"}`)}, transcript)
		if d.Action != ProgressContinue {
			t.Fatalf("turn %d: expected Continue with disabled config; got %+v", turn, d)
		}
	}
}

// TestProgressMonitor_OutOfOrderTurnIgnored proves the defensive
// guard against double-Observe calls; the second call with the same
// turn returns Continue without disturbing state.
func TestProgressMonitor_OutOfOrderTurnIgnored(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{RepeatedToolThreshold: 2})
	calls := []oai.ToolCall{makeCall("x", "bash", `{"command":"a"}`)}
	transcript := []oai.Message{}

	_ = mon.Observe(1, calls, transcript)
	// Re-call turn 1: should be a no-op.
	d := mon.Observe(1, calls, transcript)
	if d.Action != ProgressContinue {
		t.Errorf("repeated turn observation should be a noop; got %+v", d)
	}
	// Now turn 2: cumulative count is 2, threshold=2 -> Nudge.
	d = mon.Observe(2, calls, transcript)
	if d.Action != ProgressNudge {
		t.Errorf("turn 2: expected Nudge; got %+v", d)
	}
}

// TestNudgeMessage verifies the directive contains the key elements
// the model needs to either change course or call submit_result.
func TestNudgeMessage(t *testing.T) {
	d := ProgressDecision{
		Action: ProgressNudge,
		Signal: "RepeatedToolCall",
		Detail: "bash called 5 times with identical arguments",
	}
	msg := NudgeMessage(d)
	for _, want := range []string{"PROGRESS MONITOR", "submit_result", "force-terminated", d.Detail} {
		if !strings.Contains(msg, want) {
			t.Errorf("nudge message missing %q; got:\n%s", want, msg)
		}
	}
}

// TestForceTerminateEnvelope verifies the synthetic terminal envelope
// has the right shape: verdict INCOMPLETE, populated Extra including
// outcome STUCK-LOOP-DETECTED.
func TestForceTerminateEnvelope(t *testing.T) {
	d := ProgressDecision{
		Action: ProgressForceTerminate,
		Signal: "RepeatedToolCall",
		Detail: "bash called 5 times with identical arguments",
	}
	env := ForceTerminateEnvelope(d, 7)
	if !env.Terminal {
		t.Errorf("envelope should be Terminal")
	}
	if env.Verdict != "INCOMPLETE" {
		t.Errorf("verdict: want INCOMPLETE; got %q", env.Verdict)
	}
	if env.Extra["outcome"] != "STUCK-LOOP-DETECTED" {
		t.Errorf("Extra.outcome: want STUCK-LOOP-DETECTED; got %v", env.Extra["outcome"])
	}
	if env.Extra["signal"] != "RepeatedToolCall" {
		t.Errorf("Extra.signal: want RepeatedToolCall; got %v", env.Extra["signal"])
	}
	if env.Extra["terminateTurn"] != 7 {
		t.Errorf("Extra.terminateTurn: want 7; got %v", env.Extra["terminateTurn"])
	}
}

func TestFilterForcedEditSchemas_RestrictReads(t *testing.T) {
	mk := func(names ...string) []oai.Tool {
		out := make([]oai.Tool, 0, len(names))
		for _, n := range names {
			out = append(out, oai.Tool{Function: oai.ToolSchemaDef{Name: n}})
		}
		return out
	}
	names := func(ts []oai.Tool) []string {
		out := make([]string, 0, len(ts))
		for _, tt := range ts {
			out = append(out, tt.Function.Name)
		}
		return out
	}
	schemas := mk("read_file", "grep", "bash", "str_replace", "submit_result")

	got := names(filterForcedEditSchemas(schemas, false))
	want := []string{"read_file", "str_replace", "submit_result"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restrictReads=false: got %v want %v", got, want)
	}

	got = names(filterForcedEditSchemas(schemas, true))
	want = []string{"str_replace", "submit_result"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restrictReads=true: got %v want %v", got, want)
	}
}
