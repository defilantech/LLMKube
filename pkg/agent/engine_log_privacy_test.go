/*
Copyright 2026.

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
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Events are readable by anyone who can read the namespace and are commonly
// exported to central logging, while an engine log can carry request text
// (an engine run with verbose logging logs prompts). So no Event may carry
// engine log content; the agent's own log keeps the tail.

// promptSentinel stands for request text in an engine log.
const promptSentinel = "PROMPT-SENTINEL"

// sentinelEngineLog writes, as the engine log of default/svc in dir, the
// output of an engine that logged a request before it failed, and returns
// the log's path.
func sentinelEngineLog(t *testing.T, dir string) string {
	t.Helper()
	return writeEngineLog(t, dir,
		`srv  log_server_r: request: {"prompt": "`+promptSentinel+` tell me the launch codes"}`+"\n"+
			"llama_model_load: error loading model: unknown model architecture: 'qwen9'\n")
}

// observeAgentLog replaces a's logger with one that records every entry.
func observeAgentLog(a *MetalAgent) *observer.ObservedLogs {
	core, logs := observer.New(zapcore.DebugLevel)
	a.logger = zap.New(core).Sugar()
	return logs
}

// agentLogHas reports whether any recorded agent log entry carries s, in its
// message or its fields.
func agentLogHas(logs *observer.ObservedLogs, s string) bool {
	for _, e := range logs.All() {
		if strings.Contains(e.Message, s) || strings.Contains(fmt.Sprint(e.ContextMap()), s) {
			return true
		}
	}
	return false
}

// TestEvents_NeverCarryEngineLogContent drives each Event that reports an
// engine failure (StartFailed, RestartFailed, EngineExited) with an engine log
// whose tail carries request text, and requires that the text appears in no
// Event, that each Event names the log on the Mac instead, and that the
// agent's own log keeps the tail.
func TestEvents_NeverCarryEngineLogContent(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		// run triggers the Event and returns the agent's recorded log, the
		// events and the engine log path.
		run func(t *testing.T) (*observer.ObservedLogs, []string, string)
	}{
		{"StartFailed", wantStartFailedReason, func(t *testing.T) (*observer.ObservedLogs, []string, string) {
			a, ex, rec, _ := startFailureFixture(t)
			logs := observeAgentLog(a)
			logPath := sentinelEngineLog(t, a.config.ModelStorePath)
			ex.startErr = engineCrashedOnLoad(logPath)
			_ = a.ensureProcess(context.Background(), freshISVC(t, a))
			return logs, drainEvents(rec), logPath
		}},
		{"RestartFailed", EventReasonRestartFailed, func(t *testing.T) (*observer.ObservedLogs, []string, string) {
			f := newRestartFixture(t, "svc")
			logs := observeAgentLog(f.agent)
			logPath := sentinelEngineLog(t, t.TempDir())
			f.exec.startErr = engineCrashedOnLoad(logPath)
			f.probe(DefaultHealthFailureThreshold)
			f.agent.runDueRestarts(context.Background())
			return logs, drainEvents(f.rec), logPath
		}},
		{"EngineExited", wantEngineExitedReason, func(t *testing.T) (*observer.ObservedLogs, []string, string) {
			f := newRestartFixture(t, "svc")
			logs := observeAgentLog(f.agent)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			proc := f.tracked()
			proc.exit = &childExit{done: make(chan struct{})}
			proc.logPath = sentinelEngineLog(t, t.TempDir())
			f.agent.watchEngineExit(ctx, f.key, proc)
			close(proc.exit.done)
			var events []string
			eventually(func() bool {
				events = append(events, drainEvents(f.rec)...)
				return len(eventsWithReason(events, wantEngineExitedReason)) > 0
			})
			return logs, events, proc.logPath
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs, events, logPath := tc.run(t)
			reported := eventsWithReason(events, tc.reason)
			if len(reported) == 0 {
				t.Fatalf("events = %v, want a %s Event", events, tc.reason)
			}
			for _, e := range events {
				if strings.Contains(e, promptSentinel) {
					t.Errorf("an Event carries engine log content: %q", e)
				}
			}
			if !strings.Contains(reported[0], logPath) {
				t.Errorf("%s Event %q does not name the engine log %s", tc.reason, reported[0], logPath)
			}
			if !agentLogHas(logs, promptSentinel) {
				t.Errorf("the agent log does not keep the engine log tail")
			}
		})
	}
}
