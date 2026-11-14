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

package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The checks here run before join redeems the join string, which is single
// use: each one stands for a failure that would otherwise show only once the
// string is spent, as an agent launchd cannot start or keeps restarting.

// checkMetalAgentGUISession refuses when this user has no GUI login session.
// The agent is a LaunchAgent in the gui/<uid> domain, which exists only while
// the user is logged in at the Mac (or through Screen Sharing); over SSH with
// nobody logged in, launchctl bootstrap fails after the string is used up.
func checkMetalAgentGUISession(ctx context.Context, host metalHost) error {
	domain := "gui/" + strconv.Itoa(host.uid)
	if _, err := host.launchctl(ctx, "print", domain); err != nil {
		return fmt.Errorf("this user has no GUI login session (%w). The Metal agent is a LaunchAgent, which "+
			"launchd runs only for a user logged in at the Mac (or through Screen Sharing), so join would use up "+
			"the join string and then fail to start it. Log in on the Mac (on a dedicated Mac, turn on automatic "+
			"login) and run join again; the join string has not been used", err)
	}
	return nil
}

// portHolder is a process listening on a TCP port, as lsof reports it.
type portHolder struct {
	pid     int
	command string
}

func (h portHolder) String() string { return fmt.Sprintf("%s (pid %d)", h.command, h.pid) }

const lsofTimeout = 5 * time.Second

// lsofPortHolders lists the processes listening on TCP port with lsof. It is
// best effort: run as this user, lsof cannot see other users' sockets, and
// any failure means "cannot tell".
func lsofPortHolders(ctx context.Context, port int) []portHolder {
	return lsofPortHoldersWith(ctx, "/usr/sbin/lsof", port)
}

// lsofPortHoldersWith is lsofPortHolders with the lsof binary lsof.
func lsofPortHoldersWith(ctx context.Context, lsof string, port int) []portHolder {
	ctx, cancel := context.WithTimeout(ctx, lsofTimeout)
	defer cancel()
	out, _ := exec.CommandContext(ctx, lsof, "-nP", "-iTCP:"+strconv.Itoa(port),
		"-sTCP:LISTEN", "-Fpc").Output()
	return parseLsofHolders(out)
}

// parseLsofHolders reads lsof -F pc: a "p<pid>" line starts each process,
// "c<command>" names it, and other fields (lsof adds "f<fd>") are ignored.
func parseLsofHolders(out []byte) []portHolder {
	var holders []portHolder
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "p"):
			if pid, err := strconv.Atoi(line[1:]); err == nil {
				holders = append(holders, portHolder{pid: pid})
			}
		case strings.HasPrefix(line, "c") && len(holders) > 0:
			holders[len(holders)-1].command = line[1:]
		}
	}
	return holders
}

// metalAgentJobPID is the pid of the com.llmkube.metal-agent job join is
// about to boot out, from launchctl print's top-level "pid = N" line, or 0
// when it is not loaded or not running. On Linux it is the systemd unit's
// main pid.
func metalAgentJobPID(ctx context.Context, host metalHost) int {
	if host.goos == goosLinux {
		return systemdAgentMainPID(ctx, host)
	}
	out, err := host.launchctl(ctx, "print", "gui/"+strconv.Itoa(host.uid)+"/"+metalAgentLabel)
	if err != nil {
		return 0
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "\tpid = "); ok {
			if pid, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return pid
			}
		}
	}
	return 0
}

// checkMetalAgentPorts binds each port the new agent will bind, at the same
// address, and refuses when one is taken, naming the process when lsof can.
// A port held by the agent this join replaces passes: join stops it before
// the new one starts.
func checkMetalAgentPorts(ctx context.Context, host metalHost, ports []metalAgentPort, log io.Writer) error {
	replaced := -1
	for _, p := range ports {
		ln, err := host.listen("tcp", p.addr)
		if err == nil {
			_ = ln.Close()
			continue
		}
		holders := host.portHolders(ctx, p.port)
		if replaced < 0 {
			replaced = metalAgentJobPID(ctx, host)
		}
		if replaced > 0 && heldBy(holders, replaced) {
			_, _ = fmt.Fprintf(log, "port %d is held by the agent this join replaces (pid %d), which join stops first\n",
				p.port, replaced)
			continue
		}
		return metalAgentPortError(host, p, err, holders, replaced)
	}
	return nil
}

func heldBy(holders []portHolder, pid int) bool {
	for _, h := range holders {
		if h.pid == pid {
			return true
		}
	}
	return false
}

func metalAgentPortError(host metalHost, p metalAgentPort, err error, holders []portHolder, replaced int) error {
	state := "in use (lsof cannot see by what: another user's process or the system's)"
	if len(holders) > 0 {
		names := make([]string, 0, len(holders))
		for _, h := range holders {
			names = append(names, h.String())
		}
		state = "in use by " + strings.Join(names, ", ")
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		state = fmt.Sprintf("not available (%v)", err)
	}
	supervisor, stop := "launchd", fmt.Sprintf("launchctl bootout gui/%d/%s", host.uid, metalAgentLabel)
	if host.goos == goosLinux {
		supervisor, stop = "systemd", "systemctl --user stop "+systemdAgentUnit
	}
	effect := "the agent would exit at startup, and " + supervisor + " would restart it every 30 seconds"
	if !p.fatal {
		effect = "the agent would run without it (" + p.flag + "=0 turns it off)"
	}
	msg := fmt.Sprintf("port %d (%s), which the agent's %s (--%s) binds, is %s; %s. Stop that process, or give "+
		"the agent another port with --agent-arg %s=<free port>", p.port, p.addr, p.what, p.flag, state, effect, p.flag)
	if replaced > 0 && len(holders) == 0 {
		// lsof could not tell; it may still be the agent join replaces.
		msg += fmt.Sprintf(". If it is the agent join replaces (pid %d), stop it first: %s", replaced, stop)
	}
	return errors.New(msg + ". The join string has not been used")
}
