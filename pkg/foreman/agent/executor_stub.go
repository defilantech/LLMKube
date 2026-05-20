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
	"fmt"
	"time"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// DefaultStubSleep is the synthetic duration the stub executor blocks
// for; long enough to prove the dispatch loop end-to-end (Pending ->
// Scheduled -> Running -> Succeeded transitions are visible), short
// enough not to clog the demo.
const DefaultStubSleep = 10 * time.Second

// StubExecutor is the M2 placeholder: it sleeps for SleepDuration, then
// returns a synthetic GO-verdict Result. Exists so the scheduler ->
// watcher -> executor -> status-patch loop can be validated end-to-end
// before the real native agent loop lands in M3.
type StubExecutor struct {
	// SleepDuration controls how long Execute blocks. Zero defaults to
	// DefaultStubSleep.
	SleepDuration time.Duration
}

// NewStubExecutor returns a StubExecutor with default sleep duration.
func NewStubExecutor() *StubExecutor {
	return &StubExecutor{SleepDuration: DefaultStubSleep}
}

// Kind identifies this executor in Result.Kind and in logs.
func (s *StubExecutor) Kind() string { return "stub" }

// Execute sleeps for SleepDuration (or default) and returns a Result that
// labels the task as a stub run. Cancellation via ctx returns
// immediately with ctx.Err().
func (s *StubExecutor) Execute(ctx context.Context, task *foremanv1alpha1.AgenticTask) (*Result, error) {
	dur := s.SleepDuration
	if dur <= 0 {
		dur = DefaultStubSleep
	}
	start := time.Now()
	select {
	case <-time.After(dur):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	elapsed := time.Since(start)
	res := NewResult(
		s.Kind(),
		foremanv1alpha1.AgenticTaskVerdictGo,
		fmt.Sprintf("stub executor slept %s on task %s/%s", elapsed.Round(time.Millisecond), task.Namespace, task.Name),
		elapsed,
	)
	res.Extra = map[string]any{
		"taskKind":  string(task.Spec.Kind),
		"agentName": task.Spec.Payload.Agent,
		"modelRef":  task.Spec.ModelRef,
	}
	return res, nil
}
