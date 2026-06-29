package agent

import (
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// TestProgressMonitor_896_PostEditStreakNudgesNotTerminates is the #896
// regression: a coder that has ALREADY made a successful edit must not be
// force-terminated by EditFreeStreak during post-edit verification. Instead the
// edit-free streak re-arming after a real edit should NUDGE (recoverable: the
// loop enters the forcing phase that drops bash and pushes the model toward
// submit_result), not terminate empty-handed with no branch.
//
// It replays the per-turn tool trace captured on the #856 in-cluster run at the
// ornith profile's editFreeTurnsLimit=4:
//
//	read_file   x4   (pre-edit exploration -> first EditFreeStreak nudge)
//	str_replace x2   (real edits -- streak AND escalation state must reset)
//	bash        x4   (verification: build/vet/test/diff -- edit-free turns)
//
// Before the fix the run force-terminated at the 4th verification turn because
// nudgedEditFree survived the edits. After the fix the edits reset the
// escalation state, so the post-edit streak nudges (recoverable) instead.
func TestProgressMonitor_896_PostEditStreakNudgesNotTerminates(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{EditFreeTurnsLimit: 4})
	tr := []oai.Message{}

	turn := 0
	obs := func(name, args string) ProgressDecision {
		turn++
		return mon.Observe(turn, []oai.ToolCall{makeCall("c", name, args)}, tr)
	}

	// Turns 1-4: pre-edit reads. The 4th trips the limit and nudges.
	var d ProgressDecision
	for i := 0; i < 4; i++ {
		d = obs("read_file", `{"path":"pkg/agent/agent.go"}`)
	}
	if d.Action != ProgressNudge || d.Signal != signalEditFreeStreak {
		t.Fatalf("turn 4: expected pre-edit EditFreeStreak nudge, got action=%v signal=%q",
			d.Action, d.Signal)
	}

	// Turns 5-6: the real edits. These must reset BOTH the streak and the
	// escalation state, so the run is no longer "poisoned" by the early nudge.
	obs("str_replace", `{"path":"pkg/agent/agent.go","old":"a","new":"b"}`)
	obs("str_replace", `{"path":"pkg/agent/agent.go","old":"c","new":"d"}`)

	// Turns 7-10: post-edit verification. None mutate the workspace (and we
	// avoid `git add`, whose "dd " substring is a separate false-positive). The
	// streak climbs 1..4; at the limit it must NUDGE (recoverable), not
	// force-terminate, because the edits cleared nudgedEditFree.
	verify := []string{
		`{"command":"go build ./..."}`,
		`{"command":"go vet ./..."}`,
		`{"command":"go test ./pkg/foreman/agent/ -run TestX"}`,
		`{"command":"git diff --stat HEAD"}`,
	}
	for _, cmd := range verify {
		d = obs("bash", cmd)
		t.Logf("turn %d (bash verify): action=%v signal=%q streak=%d",
			turn, d.Action, d.Signal, mon.editFreeStreak)
	}

	if d.Action == ProgressForceTerminate {
		t.Fatalf("#896: coder force-terminated during post-edit verification at turn %d "+
			"after 2 real edits (branch would never push). Want a recoverable nudge.", turn)
	}
	if d.Action != ProgressNudge || d.Signal != signalEditFreeStreak {
		t.Fatalf("turn %d: expected recoverable EditFreeStreak nudge post-edit, got "+
			"action=%v signal=%q", turn, d.Action, d.Signal)
	}
	t.Logf("#896 fixed: post-edit streak nudges (recoverable) at turn %d instead of "+
		"terminating empty-handed.", turn)
}

// TestProgressMonitor_896_EditClearsNudgeEscalation is the focused unit for the
// fix: an edit that resets the streak must also clear nudgedEditFree, so a
// later streak that re-hits the limit nudges again rather than escalating
// straight to force-terminate off the stale flag.
func TestProgressMonitor_896_EditClearsNudgeEscalation(t *testing.T) {
	mon := NewLoopProgressMonitor(ProgressConfig{EditFreeTurnsLimit: 2})
	tr := []oai.Message{}
	turn := 0
	obs := func(name, args string) ProgressDecision {
		turn++
		return mon.Observe(turn, []oai.ToolCall{makeCall("c", name, args)}, tr)
	}

	// Two edit-free reads trip the limit -> first nudge (sets nudgedEditFree).
	obs("read_file", `{"path":"f"}`)
	if d := obs("read_file", `{"path":"f"}`); d.Action != ProgressNudge {
		t.Fatalf("turn 2: want nudge, got action=%v", d.Action)
	}
	if !mon.nudgedEditFree {
		t.Fatal("nudgedEditFree should be set after the first edit-free nudge")
	}

	// A real edit lands: streak AND the escalation flag must clear.
	obs("str_replace", `{"path":"f","old":"a","new":"b"}`)
	if mon.nudgedEditFree {
		t.Fatal("#896: nudgedEditFree must reset when an edit resets the streak")
	}

	// Two more edit-free turns re-hit the limit. Because the flag was cleared,
	// this is a fresh NUDGE (recoverable), not a force-terminate.
	obs("read_file", `{"path":"f"}`)
	d := obs("read_file", `{"path":"f"}`)
	if d.Action != ProgressNudge || d.Signal != signalEditFreeStreak {
		t.Fatalf("post-edit streak: want recoverable nudge, got action=%v signal=%q",
			d.Action, d.Signal)
	}
}
