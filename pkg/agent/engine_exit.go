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
	"errors"
	"fmt"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// EventReasonEngineExited is the Warning Event reason for an engine process
// that exited while the agent was serving it (#2405).
const EventReasonEngineExited = "EngineExited"

// errAdoptedEngineExited is the exit status of an adopted engine: it is not a
// child of this agent process, so the kernel does not hand its real exit
// status to the agent.
var errAdoptedEngineExited = errors.New("exit status unknown: the engine was adopted from a previous agent process")

// watchEngineExit acts on the exit of proc, just stored under key, as soon as
// its reaper sees it, instead of leaving a dead engine's endpoint up until
// the next health probe and its restart until the probe threshold. That
// threshold exists so one slow probe of a live engine does not reload a large
// model; an engine that is gone gains nothing from it. An engine with no
// exit to watch (a model of the shared oMLX or Ollama daemon, or an adopted
// engine where watchAdoptedExits could not set one up) is left to the health
// probe.
func (a *MetalAgent) watchEngineExit(ctx context.Context, key string, proc *ManagedProcess) {
	if proc.exit == nil {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-proc.exit.done:
		}
		a.handleEngineExit(ctx, key, proc)
	}()
}

// handleEngineExit handles the exit of proc if the agent still tracks it
// under key: an engine the agent stopped itself (a delete, a scale to zero, a
// spec change, an eviction or a health restart) is removed from a.processes
// before its stop, so its exit is ignored here. It withdraws the endpoint at
// once, logs the exit status with the engine log tail, and stops tracking the
// engine and schedules its restart on the service's restart backoff
// (stopForRestart), reporting the exit status and where the log is in a
// Warning Event, which does not carry the log's content (see summarizeForEvent).
// The tail is read first: the restart rotates the log. A start in flight for key holds the
// restart off; it is tried again every restartGuardRetry while proc is still
// tracked, withdrawing the endpoint again first, since the start that held
// the key may have registered it.
func (a *MetalAgent) handleEngineExit(ctx context.Context, key string, proc *ManagedProcess) {
	status := proc.exit.status()
	var tail string
	if proc.logPath != "" {
		tail = tailLines(proc.logPath, processLogTailLines)
	}
	if !a.markExited(key, proc) {
		return
	}
	a.logger.Warnw("engine process exited",
		"namespace", proc.Namespace, "name", proc.Name, "pid", proc.PID, "status", status,
		"log", proc.logPath, "logTail", tail)
	processHealthy.WithLabelValues(proc.Name, proc.Namespace).Set(0)
	a.withdrawEndpoint(ctx, proc.Name, proc.Namespace)

	for ctx.Err() == nil {
		if restartIn, ok := a.stopForRestart(ctx, key, proc, "exitStatus", status); ok {
			a.emitInferenceEvent(ctx, proc, corev1.EventTypeWarning, EventReasonEngineExited, "%s",
				fmt.Sprintf("Engine process %d exited (%s); restarting in %s", proc.PID, status, restartIn)+
					crashedLogHint(proc.logPath))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(restartGuardRetry):
		}
		if !a.markExited(key, proc) {
			return
		}
		// The start that held the key may have registered the endpoint
		// Ready since the withdrawal above.
		a.withdrawEndpoint(ctx, proc.Name, proc.Namespace)
	}
}

// crashedLogHint tells an EngineExited Event's reader where the crashed
// run's log is on the Mac; the Event does not carry its content. Empty when
// the engine keeps no log the agent knows of.
func crashedLogHint(logPath string) string {
	if logPath == "" {
		return ""
	}
	return fmt.Sprintf(". The crashed run's log on the Mac: %s, moved to %s.1 when the engine restarts; "+
		"its last lines are in the agent log", logPath, logPath)
}

// markExited marks proc unhealthy if it is still tracked under key, and
// reports whether it is. A probe that answered for the port in the meantime
// cannot make a dead engine healthy again.
func (a *MetalAgent) markExited(key string, proc *ManagedProcess) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.processes[key] != proc {
		return false
	}
	proc.Healthy = false
	return true
}

// watchAdoptedExits sets up an exit watch (watchProcessExit) for each engine
// adopted at startup, so the exit of an engine this agent did not spawn is
// handled like a child's (watchEngineExit). Start calls it once the registry
// is in its mode, so an exit handled at once withdraws the right endpoint.
// Where no watch can be set up (not on macOS, or the kernel refuses one), the
// health probe stays the detector.
func (a *MetalAgent) watchAdoptedExits(ctx context.Context) {
	a.mu.RLock()
	adopted := map[string]*ManagedProcess{}
	for key, p := range a.processes {
		if p.Adopted && p.PID > 0 && p.exit == nil {
			adopted[key] = p
		}
	}
	a.mu.RUnlock()
	for key, p := range adopted {
		exit, err := a.watchAdoptedExit(ctx, p)
		if err != nil {
			a.logger.Debugw("no exit watch for an adopted engine; the health probe detects its exit",
				"key", key, "pid", p.PID, "error", err)
			continue
		}
		a.mu.Lock()
		tracked := a.processes[key] == p
		if tracked {
			p.exit = exit
		}
		a.mu.Unlock()
		if tracked {
			a.watchEngineExit(ctx, key, p)
		}
	}
}

// watchAdoptedExit watches p's PID for its exit and then proves the PID is
// still the recorded engine: the watch is on whatever process holds the PID
// when it is set, and a PID is reused only after its process is gone, so an
// identity that holds after the watch is set means the watch is on the
// engine. An engine already gone at either step is reported as exited.
func (a *MetalAgent) watchAdoptedExit(ctx context.Context, p *ManagedProcess) (*childExit, error) {
	exit, err := watchProcessExit(ctx, p.PID)
	if errors.Is(err, syscall.ESRCH) {
		return exitedEngine(), nil
	}
	if err != nil {
		return nil, err
	}
	if err := verifyEngineIdentity(p.PID, p.StartToken, a.inspect); err != nil {
		if errors.Is(err, errProcessAlreadyGone) {
			return exitedEngine(), nil
		}
		return nil, err
	}
	return exit, nil
}

// exitedEngine is the childExit of an adopted engine found already gone.
func exitedEngine() *childExit {
	c := &childExit{done: make(chan struct{}), err: errAdoptedEngineExited}
	close(c.done)
	return c
}
