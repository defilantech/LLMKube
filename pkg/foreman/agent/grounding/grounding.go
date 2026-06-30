// Package grounding holds deterministic, model-free checks that verify a
// coder's diff is grounded in reality: that every LLMKube-owned symbol it
// references actually exists (reference-grounding) and that every LLMKube
// annotation key it introduces has a reader (inert-symbol). The detectors are
// pure functions of a workspace path and an injected command runner so they
// unit-test without shelling out. Wiring lives in coder_gate.go (Plan 1) and
// the executor/controller (Plan 2).
package grounding

import (
	"context"
	"fmt"
)

// CommandRunner runs one command in dir with extra KEY=VALUE env appended,
// returning combined stdout+stderr and the exec error. It mirrors the
// commandRunner in coder_gate.go so the gate can pass its production runner
// straight through.
type CommandRunner func(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error)

// Finding is one grounding defect. It mirrors the reviewer.Finding shape
// (severity/area/file/line/message) so Plan 2 can surface gate findings and
// reviewer findings as one schema on the task status.
type Finding struct {
	Severity string // "blocker" | "major" | "minor"
	Area     string // "doc-consistency" | "wired-up"
	File     string
	Line     int
	Message  string
}

// String renders a finding as "file:line [severity/area] message".
func (f Finding) String() string {
	return fmt.Sprintf("%s:%d [%s/%s] %s", f.File, f.Line, f.Severity, f.Area, f.Message)
}
