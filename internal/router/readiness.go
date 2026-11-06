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

package router

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The kernel's TCP socket tables for the reading process's network namespace,
// which is the pod's, so a listener another process opened in the pod (a mesh
// data plane, for example) shows up here.
const (
	defaultProcNetTCP  = "/proc/net/tcp"
	defaultProcNetTCP6 = "/proc/net/tcp6"
)

// tcpStateListen is TCP_LISTEN in the st column of /proc/net/tcp{,6}.
const tcpStateListen = "0A"

// listenerCheckTTL bounds how often /readyz rescans the socket tables. The
// endpoint is reachable by any pod that can reach the proxy, so callers must
// not be able to drive a scan per request.
const listenerCheckTTL = time.Second

// procNetTable is one /proc/net/tcp-format file. absentIsEmpty marks a table
// the kernel omits when the protocol is compiled out or disabled at boot
// (tcp6 with ipv6.disable=1): a missing file then means no sockets, not an
// unreadable table.
type procNetTable struct {
	path          string
	absentIsEmpty bool
}

// listenerState is what a check concluded, for transition-only logging.
type listenerState int

const (
	listenersUnchecked listenerState = iota
	listenersPresent
	listenersMissing
	// listenersNotEnforced: a table could not be read and a port was not found
	// in the tables that could, so the check reports ready without having seen
	// the port.
	listenersNotEnforced
)

// listenerCheck is the outcome of one scan of the socket tables.
type listenerCheck struct {
	state listenerState
	// notFound holds the required ports no readable table listed.
	notFound []uint16
	// unreadable maps each table that failed to read to its error.
	unreadable map[string]error
}

// listenerChecker reports which required TCP ports have no LISTEN socket in
// the pod's network namespace. It fails open: when a table cannot be read and
// a port was not found in the tables that could, the port is not reported
// missing, because a /proc problem is not a reason to take a replica out.
type listenerChecker struct {
	ports  []uint16
	want   map[uint16]bool
	tables []procNetTable
	logger *slog.Logger
	ttl    time.Duration
	now    func() time.Time
	scan   func(path string, want, found map[uint16]bool) error

	mu sync.Mutex
	// result and checkedAt cache the last check for ttl.
	result    []uint16
	checkedAt time.Time
	checked   bool
	// inflight is closed when the running check finishes, so concurrent
	// callers wait for it instead of scanning again.
	inflight chan struct{}
	// unreadable and last hold what was last logged, so a state is logged
	// when it starts, not on every check.
	unreadable  map[string]bool
	last        listenerState
	lastMissing string
}

func newListenerChecker(ports []uint16, tcpPath, tcp6Path string, logger *slog.Logger) *listenerChecker {
	sorted := slices.Clone(ports)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	want := make(map[uint16]bool, len(sorted))
	for _, p := range sorted {
		want[p] = true
	}
	return &listenerChecker{
		ports: sorted,
		want:  want,
		tables: []procNetTable{
			{path: tcpPath},
			{path: tcp6Path, absentIsEmpty: true},
		},
		logger:     logger,
		ttl:        listenerCheckTTL,
		now:        time.Now,
		scan:       scanListeners,
		unreadable: map[string]bool{},
	}
}

// missing returns the required ports with no LISTEN socket, in ascending
// order, or nil when every port listens or a table could not be read. A result
// younger than ttl is reused, and callers that arrive while a check runs share
// its result. The returned slice must not be modified.
func (c *listenerChecker) missing() []uint16 {
	c.mu.Lock()
	if c.checked && c.now().Sub(c.checkedAt) < c.ttl {
		defer c.mu.Unlock()
		return c.result
	}
	if ch := c.inflight; ch != nil {
		c.mu.Unlock()
		<-ch
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.result
	}
	ch := make(chan struct{})
	c.inflight = ch
	c.mu.Unlock()

	res := c.check()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.record(res)
	c.inflight = nil
	close(ch)
	return c.result
}

// check scans every table. It takes no lock: want is read-only after
// construction.
func (c *listenerChecker) check() listenerCheck {
	found := make(map[uint16]bool, len(c.ports))
	res := listenerCheck{unreadable: map[string]error{}}
	for _, t := range c.tables {
		err := c.scan(t.path, c.want, found)
		if err != nil && t.absentIsEmpty && errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
		if err != nil {
			res.unreadable[t.path] = err
		}
	}
	for _, p := range c.ports {
		if !found[p] {
			res.notFound = append(res.notFound, p)
		}
	}
	switch {
	case len(res.notFound) == 0:
		res.state = listenersPresent
	case len(res.unreadable) > 0:
		res.state = listenersNotEnforced
	default:
		res.state = listenersMissing
	}
	return res
}

// record caches res and logs what changed since the last check. Callers hold
// c.mu.
func (c *listenerChecker) record(res listenerCheck) {
	c.checked = true
	c.checkedAt = c.now()
	c.result = nil
	if res.state == listenersMissing {
		c.result = res.notFound
	}

	for _, t := range c.tables {
		err, failed := res.unreadable[t.path]
		switch {
		case failed && !c.unreadable[t.path]:
			c.unreadable[t.path] = true
			c.logger.Warn("cannot read TCP socket table", "path", t.path, "error", err)
		case !failed && c.unreadable[t.path]:
			delete(c.unreadable, t.path)
			c.logger.Info("TCP socket table readable again", "path", t.path)
		}
	}

	missingKey := fmt.Sprint(res.notFound)
	if res.state == c.last && (res.state != listenersMissing || missingKey == c.lastMissing) {
		return
	}
	c.last = res.state
	c.lastMissing = missingKey
	switch res.state {
	case listenersPresent:
		c.logger.Info("required listeners present; reporting ready", "ports", c.ports)
	case listenersMissing:
		c.logger.Warn("required listeners missing; reporting not ready", "ports", res.notFound)
	case listenersNotEnforced:
		unreadable := make([]string, 0, len(res.unreadable))
		for path := range res.unreadable {
			unreadable = append(unreadable, path)
		}
		slices.Sort(unreadable)
		c.logger.Warn("required listeners not enforced; reporting ready",
			"ports", res.notFound, "unreadable", unreadable)
	}
}

// scanListeners marks in found every port in want that has a LISTEN entry in
// the /proc/net/tcp-format table at path. A table whose header does not name
// local_address and st in the columns read here is an error, so a format the
// parser does not understand fails open instead of reporting every port
// missing.
func scanListeners(path string, want, found map[uint16]bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return err
		}
		return fmt.Errorf("%s: empty table", path)
	}
	header := strings.Fields(sc.Text())
	if len(header) < 4 || header[1] != "local_address" || header[3] != "st" {
		return fmt.Errorf("%s: unexpected header %q", path, sc.Text())
	}
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[3] != tcpStateListen {
			continue
		}
		local := fields[1]
		i := strings.LastIndexByte(local, ':')
		if i < 0 {
			continue
		}
		port, err := strconv.ParseUint(local[i+1:], 16, 16)
		if err != nil {
			continue
		}
		if want[uint16(port)] {
			found[uint16(port)] = true
		}
	}
	return sc.Err()
}
