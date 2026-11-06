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

// procNetTable is one /proc/net/tcp-format file. absentIsEmpty marks a table
// the kernel omits when the protocol is compiled out or disabled at boot
// (tcp6 with ipv6.disable=1): a missing file then means no sockets, not an
// unreadable table.
type procNetTable struct {
	path          string
	absentIsEmpty bool
}

// listenerChecker reports which required TCP ports have no LISTEN socket in
// the pod's network namespace. It fails open: when a table cannot be read and
// a port was not found in the tables that could, the port is not reported
// missing, because a /proc problem is not a reason to take a replica out.
type listenerChecker struct {
	ports  []uint16
	tables []procNetTable
	logger *slog.Logger

	mu sync.Mutex
	// unreadable holds the tables whose last read failed, so a failure is
	// logged when it starts and when it clears, not on every probe.
	unreadable map[string]bool
	// lastMissing is the missing set the previous check reported, for the
	// same transition-only logging.
	lastMissing string
}

func newListenerChecker(ports []uint16, tcpPath, tcp6Path string, logger *slog.Logger) *listenerChecker {
	sorted := slices.Clone(ports)
	slices.Sort(sorted)
	return &listenerChecker{
		ports: slices.Compact(sorted),
		tables: []procNetTable{
			{path: tcpPath},
			{path: tcp6Path, absentIsEmpty: true},
		},
		logger:     logger,
		unreadable: map[string]bool{},
	}
}

// missing returns the required ports with no LISTEN socket, in ascending
// order, or nil when every port listens or a table could not be read.
func (c *listenerChecker) missing() []uint16 {
	want := make(map[uint16]bool, len(c.ports))
	for _, p := range c.ports {
		want[p] = true
	}
	found := make(map[uint16]bool, len(c.ports))

	c.mu.Lock()
	defer c.mu.Unlock()

	anyUnreadable := false
	for _, t := range c.tables {
		err := scanListeners(t.path, want, found)
		if err != nil && t.absentIsEmpty && errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
		switch {
		case err != nil:
			anyUnreadable = true
			if !c.unreadable[t.path] {
				c.unreadable[t.path] = true
				c.logger.Warn("cannot read TCP socket table; required listeners are not enforced until it is readable",
					"path", t.path, "error", err)
			}
		case c.unreadable[t.path]:
			delete(c.unreadable, t.path)
			c.logger.Info("TCP socket table readable again", "path", t.path)
		}
	}

	var missing []uint16
	for _, p := range c.ports {
		if !found[p] {
			missing = append(missing, p)
		}
	}
	if anyUnreadable {
		missing = nil
	}

	if key := fmt.Sprint(missing); key != c.lastMissing {
		c.lastMissing = key
		if len(missing) > 0 {
			c.logger.Warn("required listeners missing; reporting not ready", "ports", missing)
		} else {
			c.logger.Info("required listeners present; reporting ready", "ports", c.ports)
		}
	}
	return missing
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
