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
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// processLogTailLines is how many trailing lines of a child's log a startup
// failure carries. llama.cpp prints the fatal cause last, and a bounded tail
// keeps the error readable in events and agent logs.
const processLogTailLines = 20

// processLogTailBytes caps how much of the log tailLines reads, so a child
// that logged megabytes before dying does not get slurped whole.
const processLogTailBytes = 64 << 10

// errChildExited reports that the child exited before it became healthy.
var errChildExited = errors.New("process exited before becoming healthy")

// KilledAfterGraceError reports that a child ignored SIGTERM for the whole
// grace period and was SIGKILLed. The engine is stopped: a teardown that
// ends in this error completed, so the eviction path must not treat it as a
// stop failure (#2091).
type KilledAfterGraceError struct{ PID int }

func (e *KilledAfterGraceError) Error() string {
	return fmt.Sprintf("process %d did not exit gracefully, killed", e.PID)
}

// errProcessAlreadyGone reports that a stop found no live process at the
// PID: the signal hit ESRCH. The engine is stopped. Only a failed signal sets
// it, because only that proves the PID is dead.
var errProcessAlreadyGone = errors.New("process already gone")

// stopGracePeriod is how long a child may ignore SIGTERM before it is
// SIGKILLed. A var so tests can shorten it; mirrors helpProbeTimeout.
var stopGracePeriod = 10 * time.Second

// stopPollInterval is how often a stop checks that a PID it cannot wait on,
// or the rest of a process group, has exited. A var so tests can pin the
// number of polls.
var stopPollInterval = 50 * time.Millisecond

// signalFunc sends sig to pid (negative for a process group); a var so tests
// can script the kernel's answers.
type signalFunc func(pid int, sig syscall.Signal) error

var killFn signalFunc = syscall.Kill

// getpgidFn returns the process group pid is in; a var so tests can script
// the kernel's answer.
var getpgidFn = syscall.Getpgid

// stopEngineGroup stops an engine the agent did not spawn in this process (an
// adopted engine, #2079) by signalling its process group. Nothing is signalled
// until the PID is proven to be the recorded engine: it must lead its own group
// (pgid == pid), be alive, and still have the recorded start token. It never
// escalates on doubt: EPERM, a lookup error or a changed start token returns an
// error instead of SIGKILL. Only a proven-gone process or a changed token wraps
// errProcessAlreadyGone; a failed lookup is a plain error so the caller keeps
// the record of an engine that may still run. A record with no group of its
// own (pgid <= 0 or pgid != pid) is refused without a signal.
func stopEngineGroup(pid, pgid int, token string, inspect processInspector) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to stop invalid PID %d", pid)
	}
	if pgid != pid {
		return fmt.Errorf("refusing to signal process group %d: not led by engine PID %d", pgid, pid)
	}
	if err := verifyEngineIdentity(pid, token, inspect); err != nil {
		return err
	}
	// The record says the engine leads pgid; the kernel must agree, or the
	// group signal below would reach another group, or none, and an ESRCH from
	// it would pass for an engine already gone.
	if err := verifyEngineGroup(pid, pgid); err != nil {
		return err
	}
	if err := killFn(-pgid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("process group %d: %w", pgid, errProcessAlreadyGone)
		}
		return fmt.Errorf("failed to send SIGTERM to process group %d: %w", pgid, err)
	}
	deadline := time.Now().Add(stopGracePeriod)
	for time.Now().Before(deadline) {
		err := killFn(pid, 0)
		if errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("process %d is no longer ours (EPERM); not escalating", pid)
		}
		if errors.Is(err, syscall.ESRCH) {
			return drainProcessGroup(pid, pgid, deadline, killFn)
		}
		sleepUntil(deadline, stopPollInterval)
	}
	// The leader may have exited since the last poll: then only its group is
	// left, and a group with members cannot have had its ID reused. A living
	// leader needs the identity re-check before SIGKILL.
	if err := verifyEngineIdentity(pid, token, inspect); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return drainProcessGroup(pid, pgid, deadline, killFn)
		}
		return err
	}
	_ = killFn(-pgid, syscall.SIGKILL)
	return &KilledAfterGraceError{PID: pid}
}

// verifyEngineGroup proves the living, identified engine pid is in process
// group pgid. A PID gone since the identity check wraps errProcessAlreadyGone;
// any other failure or a different group is a plain error, so the caller keeps
// the record of an engine that may still run.
func verifyEngineGroup(pid, pgid int) error {
	got, err := getpgidFn(pid)
	if errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("process %d: %w (%w)", pid, errProcessAlreadyGone, err)
	}
	if err != nil {
		return fmt.Errorf("cannot read the process group of process %d: %w", pid, err)
	}
	if got != pgid {
		return fmt.Errorf("process %d is in process group %d, not its recorded group %d; not signalling", pid, got, pgid)
	}
	return nil
}

// stopLegacyEngine stops the engine of a version-1 record, written by an agent
// from before engines led their own process group (0.10.5 to 0.10.11). Such an
// engine ran in the agent's group, so it is signalled as a single PID, and
// only after its start token proves it is the recorded process. Like
// stopEngineGroup it never escalates on doubt. It returns nil once its signal
// stopped the engine, and an error wrapping errProcessAlreadyGone when the PID
// was dead or held another process before any signal.
func stopLegacyEngine(pid int, token string, inspect processInspector) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to stop invalid PID %d", pid)
	}
	if err := verifyEngineIdentity(pid, token, inspect); err != nil {
		return err
	}
	if err := killFn(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("process %d: %w", pid, errProcessAlreadyGone)
		}
		return fmt.Errorf("failed to send SIGTERM to process %d: %w", pid, err)
	}
	deadline := time.Now().Add(stopGracePeriod)
	for time.Now().Before(deadline) {
		err := killFn(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil // it exited on SIGTERM
		}
		if errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("process %d is no longer ours (EPERM); not escalating", pid)
		}
		sleepUntil(deadline, stopPollInterval)
	}
	if err := verifyEngineIdentity(pid, token, inspect); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	_ = killFn(pid, syscall.SIGKILL)
	return &KilledAfterGraceError{PID: pid}
}

// drainProcessGroup finishes a group stop whose leader is gone. A member may
// outlive the leader and ignore SIGTERM, and with its own group it would
// outlive the agent too, so the group is polled until deadline and any member
// still in it is SIGKILLed. ESRCH means the group is empty; EPERM is macOS's
// answer for a group holding only zombies. It probes at least once, so a
// caller at its deadline still learns whether members remain.
func drainProcessGroup(pid, pgid int, deadline time.Time, kill signalFunc) error {
	for {
		err := kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		sleepUntil(deadline, stopPollInterval)
	}
	_ = kill(-pgid, syscall.SIGKILL)
	return &KilledAfterGraceError{PID: pid}
}

// sleepUntil sleeps for poll, or less when deadline comes first.
func sleepUntil(deadline time.Time, poll time.Duration) {
	time.Sleep(max(min(poll, time.Until(deadline)), 0))
}

// verifyEngineIdentity proves pid is alive with the recorded start token. A
// dead PID or a changed token wraps errProcessAlreadyGone (a dead PID also
// wraps syscall.ESRCH, so a caller can tell it from a reused one); a failed
// lookup or EPERM is a plain error so no caller treats the engine as stopped.
func verifyEngineIdentity(pid int, token string, inspect processInspector) error {
	if err := killFn(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("process %d: %w (%w)", pid, errProcessAlreadyGone, err)
		}
		return fmt.Errorf("cannot confirm process %d is ours: %w", pid, err)
	}
	now, err := inspect.StartToken(pid)
	if err != nil {
		return fmt.Errorf("cannot read the start time of process %d: %w", pid, err)
	}
	if now != token {
		return fmt.Errorf("process %d is not the recorded engine (start token changed): %w", pid, errProcessAlreadyGone)
	}
	return nil
}

// childExit is the single reaper for a spawned child: one goroutine calls
// cmd.Wait, records the result, and closes done. Everything else that needs
// to know whether the child is gone (the health wait, StopProcess) selects on
// done instead of waiting again, since a second wait on a reaped PID fails.
type childExit struct {
	done  chan struct{}
	err   error // valid once done is closed
	group bool  // the child leads its own process group, so stops signal the group
}

func watchChild(cmd *exec.Cmd) *childExit {
	c := &childExit{done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	return c
}

// status describes how the child ended, for error messages. Only call it
// after done is closed.
func (c *childExit) status() string {
	if c.err == nil {
		return "exit status 0"
	}
	return c.err.Error()
}

// tailLines returns the last n lines of the file at path, or "" when it
// cannot be read. It is diagnostic only, so read errors are swallowed.
func tailLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := max(info.Size()-processLogTailBytes, 0)
	data, err := io.ReadAll(io.NewSectionReader(f, offset, info.Size()-offset))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// withLogTail appends the tail of the child's log to a startup error, so the
// reason a child failed travels with the error instead of only living on disk.
func withLogTail(err error, logPath string) error {
	return &engineLogError{err: err, logPath: logPath, tail: tailLines(logPath, processLogTailLines)}
}

// engineLogError is a startup error with the tail of the engine's log
// attached. Its text, which goes to the agent log, is the error followed by
// the tail; the parts stay separate so an Event can say what failed and where
// the log is without carrying the tail (summarizeForEvent).
type engineLogError struct {
	err     error
	logPath string
	tail    string
}

func (e *engineLogError) Error() string {
	if e.tail == "" {
		return fmt.Sprintf("%v (log: %s)", e.err, e.logPath)
	}
	return fmt.Sprintf("%v; last lines of %s:\n%s", e.err, e.logPath, e.tail)
}

func (e *engineLogError) Unwrap() error { return e.err }

// eventSummaryMaxBytes bounds the error summary an Event or the scheduling
// status carries.
const eventSummaryMaxBytes = 200

// summarizeForEvent returns what an Event (and the scheduling status) may say
// about err: its first line, without an attached engine log tail, cut to
// eventSummaryMaxBytes, plus the engine log's path when one is attached.
// Never the log's content: Events are readable by anyone who can read the
// namespace and are commonly exported to central logging, and an engine log
// can carry request text. The full error, tail included, goes to the agent
// log only.
func summarizeForEvent(err error) (summary, logPath string) {
	text := err.Error()
	var le *engineLogError
	if errors.As(err, &le) {
		text = strings.Replace(text, le.Error(), le.err.Error(), 1)
		logPath = le.logPath
	}
	summary, _, _ = strings.Cut(text, "\n")
	if len(summary) > eventSummaryMaxBytes {
		summary = strings.ToValidUTF8(summary[:eventSummaryMaxBytes], "") + "..."
	}
	return summary, logPath
}

// engineLogHint tells an Event's reader where the engine's logs are on the
// Mac, since the Event does not carry them: logPath for the run that failed,
// logPath.1 for the run before it. Empty when no log is known.
func engineLogHint(logPath string) string {
	if logPath == "" {
		return ""
	}
	return fmt.Sprintf(". Engine log on the Mac: %s (the run before it: %s.1); its last lines are in the agent log",
		logPath, logPath)
}

// childTracker maps the PID of every child an executor spawned to its reaper,
// so StopProcess can learn the child is gone without a second wait. A PID
// absent here is signalled and polled until it exits, because it is not our
// child and cannot be waited on. Every spawn path tracks its child, so nothing
// reaches that path today; it is for engines adopted across an agent restart
// (#2079). The zero value is ready to use.
type childTracker struct {
	childMu  sync.Mutex
	children map[int]*childExit
}

func (t *childTracker) trackChild(cmd *exec.Cmd) *childExit {
	exit := watchChild(cmd)
	exit.group = cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid
	t.childMu.Lock()
	if t.children == nil {
		t.children = map[int]*childExit{}
	}
	t.children[cmd.Process.Pid] = exit
	t.childMu.Unlock()
	return exit
}

// afterSpawn runs cfg.OnSpawned for a child that just started. When the hook
// fails, the child is stopped and the error returned, so the caller fails the
// start without leaving an unrecorded engine behind. A child that had already
// exited reports that instead, with its log tail, so a crashed engine is not
// blamed on the record. The executable is recorded both resolved and as
// executed, so adoption still recognizes the engine after a Homebrew upgrade
// repoints the bin/ symlink it was started through.
func (t *childTracker) afterSpawn(
	cfg ExecutorConfig, cmd *exec.Cmd, exit *childExit, port int, modelPath, logPath string,
) error {
	if cfg.OnSpawned == nil {
		return nil
	}
	executable := cmd.Path
	if resolved, err := filepath.EvalSymlinks(cmd.Path); err == nil {
		executable = resolved
	}
	sp := SpawnedProcess{
		PID: cmd.Process.Pid, Port: port, Executable: executable, LaunchPath: cmd.Path, ModelPath: modelPath,
		LogPath: logPath, BindHost: bindHost(cfg),
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid {
		sp.PGID = sp.PID
	}
	err := cfg.OnSpawned(sp)
	if err == nil {
		return nil
	}
	exited := false
	if exit != nil {
		select {
		case <-exit.done:
			exited = true
		default:
		}
	}
	_ = t.stopChild(sp.PID)
	if exited {
		return withLogTail(fmt.Errorf("engine %w (%s)", errChildExited, exit.status()), logPath)
	}
	return fmt.Errorf("record spawned process: %w", err)
}

func (t *childTracker) trackedChild(pid int) *childExit {
	t.childMu.Lock()
	defer t.childMu.Unlock()
	return t.children[pid]
}

func (t *childTracker) untrackChild(pid int) {
	t.childMu.Lock()
	delete(t.children, pid)
	t.childMu.Unlock()
}

// stopChild sends SIGTERM with a 10s grace period before SIGKILL. A tracked
// child is stopped through its reaper; an untracked PID is signalled and
// polled until it is gone.
func (t *childTracker) stopChild(pid int) error {
	if pid <= 0 {
		// kill(2) treats 0 and negative values as process groups, never a single engine.
		return fmt.Errorf("refusing to stop invalid PID %d", pid)
	}
	if exit := t.trackedChild(pid); exit != nil {
		return t.stopTrackedChild(pid, exit)
	}

	// Untracked: a PID this process did not spawn (an engine adopted across an
	// agent restart, #2079). It is not our child, so it cannot be waited on:
	// os.Process.Wait returns ECHILD at once. Signal, then poll for it to go.
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("process %d: %w", pid, errProcessAlreadyGone)
		}
		return fmt.Errorf("failed to send SIGTERM to process %d: %w", pid, err)
	}
	deadline := time.Now().Add(stopGracePeriod)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(stopPollInterval)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return &KilledAfterGraceError{PID: pid}
}

// stopTrackedChild stops a child this executor spawned. Its reaper owns the
// wait, so this only signals and watches done. A child that leads its own
// process group is stopped by group, so helpers it forked go with it: once the
// leader exits, the rest of the group gets what is left of the grace period
// before it is SIGKILLed, since a member that ignores SIGTERM would otherwise
// outlive the agent unrecorded.
func (t *childTracker) stopTrackedChild(pid int, exit *childExit) error {
	defer t.untrackChild(pid)

	select {
	case <-exit.done:
		// Exited and reaped already; signalling now could hit a reused PID.
		return nil
	default:
	}

	target := pid
	if exit.group {
		target = -pid
	}
	if err := syscall.Kill(target, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("process %d: %w", pid, errProcessAlreadyGone)
		}
		return fmt.Errorf("failed to send SIGTERM to process %d: %w", pid, err)
	}

	deadline := time.Now().Add(stopGracePeriod)
	select {
	case <-time.After(stopGracePeriod):
		_ = syscall.Kill(target, syscall.SIGKILL)
		return &KilledAfterGraceError{PID: pid}
	case <-exit.done:
	}
	if !exit.group {
		return nil
	}
	return drainProcessGroup(pid, pid, deadline, syscall.Kill)
}

// waitForChildHealthy polls http://127.0.0.1:<port>/health until it returns
// 200, the timeout fires, or exited closes. The last returns errChildExited at
// once: a child that died on startup will never answer, and polling it for the
// full timeout would stall every other InferenceService the agent is waiting
// to reconcile.
func waitForChildHealthy(port int, timeout time.Duration, exited <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for health check")
		case <-exited:
			return errChildExited
		case <-ticker.C:
			resp, err := http.Get(healthURL)
			if err == nil && resp.StatusCode == http.StatusOK {
				_ = resp.Body.Close()
				return nil
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
		}
	}
}
