//go:build darwin

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
)

// processExitPoll is how often a process exit watch checks that its context
// is still live. The exit itself wakes the watch at once.
const processExitPoll = time.Second

// watchProcessExit reports the exit of pid, a process this agent did not
// spawn and so cannot wait on (an engine adopted from a previous agent
// process, #2079): the returned childExit's done closes when it exits. It
// uses a kqueue EVFILT_PROC filter for NOTE_EXIT, which the kernel delivers
// for any process the agent's user may signal. A non-child's exit status is
// not available, so err only says that it exited. A pid that is already gone
// returns an error wrapping syscall.ESRCH. The watch ends when ctx ends,
// without closing done.
func watchProcessExit(ctx context.Context, pid int) (*childExit, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("refusing to watch invalid PID %d", pid)
	}
	kq, err := syscall.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("kqueue: %w", err)
	}
	change := syscall.Kevent_t{
		Ident:  uint64(pid),
		Filter: syscall.EVFILT_PROC,
		Flags:  syscall.EV_ADD | syscall.EV_ONESHOT,
		Fflags: syscall.NOTE_EXIT,
	}
	if _, err := syscall.Kevent(kq, []syscall.Kevent_t{change}, nil, nil); err != nil {
		_ = syscall.Close(kq)
		return nil, fmt.Errorf("watch process %d for its exit: %w", pid, err)
	}
	c := &childExit{done: make(chan struct{})}
	go func() {
		defer func() { _ = syscall.Close(kq) }()
		events := make([]syscall.Kevent_t, 1)
		timeout := syscall.NsecToTimespec(processExitPoll.Nanoseconds())
		for ctx.Err() == nil {
			n, err := syscall.Kevent(kq, nil, events, &timeout)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil {
				// The watch is lost; the health probe still notices the exit.
				return
			}
			if n > 0 && events[0].Fflags&syscall.NOTE_EXIT != 0 {
				c.err = errAdoptedEngineExited
				close(c.done)
				return
			}
		}
	}()
	return c, nil
}
