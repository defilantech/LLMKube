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
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/defilantech/llmkube/pkg/datadir"
	"github.com/defilantech/llmkube/pkg/metaljoin"
)

// fakeLinux stands in for the Linux host join and leave run on: a temporary
// home, a llmkube-metal-agent on PATH, and a systemctl --user that records
// its calls and, like systemd, starts only a unit whose file is on disk.
type fakeLinux struct {
	home, agentBin, llamaServer string
	xdgDataHome, xdgConfigHome  string
	calls                       []string
	failOn                      string
	// onStart runs when enable --now starts the agent.
	onStart func()
	active  bool
	// startedUnit is the unit file as systemd read it at enable --now.
	startedUnit string
	// stuck: the agent keeps running through stop and disable --now.
	stuck bool
	// noUserManager: systemctl --user cannot reach the user's instance.
	noUserManager bool
	lingering     bool
	uid           int
	pid           int
	onAgent       func(args []string) error
	busy          map[string]error
	holders       map[int][]portHolder
}

func newFakeLinux(t *testing.T) *fakeLinux {
	t.Helper()
	m := &fakeLinux{home: t.TempDir(), uid: 1000}
	m.agentBin = filepath.Join(t.TempDir(), "bin", metalAgentBinary)
	if err := os.MkdirAll(filepath.Dir(m.agentBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.agentBin, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // G306: an executable
		t.Fatal(err)
	}
	m.llamaServer = filepath.Join(filepath.Dir(m.agentBin), "llama-server")
	if err := os.WriteFile(m.llamaServer, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // G306: an executable
		t.Fatal(err)
	}
	prev := newMetalHost
	newMetalHost = func() (metalHost, error) { return m.host(t), nil }
	prevNode := joinTestNode
	joinTestNode = "host-a"
	t.Cleanup(func() { newMetalHost, joinTestNode = prev, prevNode })
	return m
}

func (m *fakeLinux) host(t *testing.T) metalHost {
	return metalHost{
		goos:          goosLinux,
		home:          m.home,
		uid:           m.uid,
		username:      "agent",
		xdgDataHome:   m.xdgDataHome,
		xdgConfigHome: m.xdgConfigHome,
		lingering:     func() bool { return m.lingering },
		launchctl: func(_ context.Context, args ...string) (string, error) {
			t.Errorf("launchctl %q on a Linux host", args)
			return "", errors.New("no launchctl on Linux")
		},
		systemctl: m.systemctl,
		lookPath: func(name string) (string, error) {
			if name == metalAgentBinary {
				return m.agentBin, nil
			}
			return "", exec.ErrNotFound
		},
		llamaServerPaths: []string{m.llamaServer},
		executable:       func() (string, error) { return filepath.Join(m.home, "llmkube"), nil },
		runAgent: func(_ context.Context, bin string, args []string, _, _ io.Writer) error {
			call := "agent " + bin + " " + strings.Join(args, " ")
			if m.active {
				call += " (while the agent runs)"
			}
			m.calls = append(m.calls, call)
			if m.onAgent != nil {
				return m.onAgent(args)
			}
			return nil
		},
		listen: func(_, address string) (net.Listener, error) {
			if err, ok := m.busy[address]; ok {
				return nil, err
			}
			return fakeListener{}, nil
		},
		portHolders: func(_ context.Context, port int) []portHolder { return m.holders[port] },
	}
}

func (m *fakeLinux) paths() metalAgentPaths {
	return newSystemdAgentPaths(metalHost{home: m.home, xdgDataHome: m.xdgDataHome, xdgConfigHome: m.xdgConfigHome})
}

func (m *fakeLinux) systemctl(_ context.Context, args ...string) (string, error) {
	call := strings.Join(args, " ")
	m.calls = append(m.calls, call)
	if m.noUserManager {
		return "", errors.New("systemctl --user " + call + ": exit status 1: Failed to connect to bus: No medium found")
	}
	if m.failOn != "" && strings.HasPrefix(call, m.failOn) {
		return "", errors.New("systemctl --user " + call + " failed")
	}
	switch call {
	case "stop " + systemdAgentUnit:
		m.active = m.stuck
	case "enable --now " + systemdAgentUnit:
		raw, err := os.ReadFile(m.paths().Unit)
		if err != nil {
			return "", errors.New("Failed to enable unit: Unit file " + systemdAgentUnit + " does not exist.")
		}
		m.startedUnit, m.active = string(raw), true
		if m.onStart != nil {
			m.onStart()
		}
	case "disable --now " + systemdAgentUnit:
		m.active = m.stuck
	case "show --property=MainPID --value " + systemdAgentUnit:
		if m.active {
			return strconv.Itoa(m.pid) + "\n", nil
		}
		return "0\n", nil
	case "show --property=ActiveState --value " + systemdAgentUnit:
		if m.active {
			return "active\n", nil
		}
		return "inactive\n", nil
	}
	return "", nil
}

// agentStartsOnEnable makes the agent join starts renew its Lease and get
// verified.
func (m *fakeLinux) agentStartsOnEnable(t *testing.T, c client.Client, ns string) {
	m.onStart = func() { markVerified(t, c, ns, time.Now()) }
}

func (m *fakeLinux) systemctlCalls() []string {
	var out []string
	for _, c := range m.calls {
		if !strings.HasPrefix(c, "agent ") {
			out = append(out, c)
		}
	}
	return out
}

func requireLinuxNothingWritten(t *testing.T, m *fakeLinux) {
	t.Helper()
	if entries, _ := os.ReadDir(m.home); len(entries) != 0 {
		t.Errorf("a refused join wrote %v", entries)
	}
	for _, c := range m.systemctlCalls() {
		if c != "show-environment" {
			t.Errorf("a refused join ran systemctl --user %s", c)
		}
	}
}

// unitExecStart is the command line systemd would run for the unit join
// started.
func unitExecStart(t *testing.T, unit string) []string {
	t.Helper()
	args, err := systemdUnitExecStart([]byte(unit))
	if err != nil {
		t.Fatalf("read ExecStart from the unit: %v\n%s", err, unit)
	}
	return args
}

// TestSystemdJoinSetsUpTheAgent runs token create and join end to end on a
// Linux host: the credentials land under ~/.local/share/llmkube/metal-agent,
// the join Secret is gone, and systemd starts the unit join wrote under
// ~/.config/systemd/user with the agent's arguments, after a daemon-reload.
func TestSystemdJoinSetsUpTheAgent(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	host := newFakeLinux(t)
	joinString, p := issueJoinString(t, ns)
	host.agentStartsOnEnable(t, c, ns)

	stdout, stderr, err := runMetal(t, "join", joinString, "--wait", "30s")
	if err != nil {
		t.Fatalf("join: %v\nstderr: %s", err, stderr)
	}
	paths := host.paths()
	if want := filepath.Join(host.home, ".local", "share", "llmkube", "metal-agent"); paths.Dir != want {
		t.Fatalf("state directory %s, want %s", paths.Dir, want)
	}
	if want := filepath.Join(host.home, ".config", "systemd", "user", systemdAgentUnit); paths.Unit != want {
		t.Fatalf("unit %s, want %s", paths.Unit, want)
	}
	for path, want := range map[string]os.FileMode{paths.Dir: 0o700, paths.Token: 0o600, paths.Kubeconfig: 0o600,
		paths.Unit: 0o644} {
		if got := fileMode(t, path); got != want {
			t.Errorf("%s mode %o, want %o", path, got, want)
		}
	}
	assertAgentKubeconfig(t, paths, p, ns)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "llmkube-metal-host-a-join"},
		&corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("join Secret after join: %v, want NotFound", err)
	}

	want := []string{host.agentBin, "--kubeconfig", paths.Kubeconfig, "--namespace", ns, "--metal-node", "host-a",
		"--renew-service-account-token", "--log-level", "info", "--llama-server", host.llamaServer}
	if got := unitExecStart(t, host.startedUnit); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("agent arguments %q, want %q", got, want)
	}
	for _, line := range []string{metalJoinUnitMarker, "Restart=on-failure", "KillMode=process",
		"WantedBy=default.target"} {
		if !strings.Contains(host.startedUnit, line+"\n") {
			t.Errorf("the unit has no %q line:\n%s", line, host.startedUnit)
		}
	}
	if strings.Contains(host.startedUnit, "Environment=") {
		t.Errorf("without XDG_DATA_HOME the unit sets an environment:\n%s", host.startedUnit)
	}
	wantCalls := []string{"show-environment", "daemon-reload", "stop " + systemdAgentUnit,
		"enable --now " + systemdAgentUnit}
	if got := host.systemctlCalls(); strings.Join(got, "; ") != strings.Join(wantCalls, "; ") {
		t.Errorf("systemctl --user calls %q, want %q", got, wantCalls)
	}
	if !strings.Contains(stdout, "Host host-a joined: the cluster reaches it at 100.64.0.2:9443 (log: "+
		systemdAgentLogs+")") {
		t.Errorf("join does not say where the cluster reaches the host and where its log is: %q", stdout)
	}
	if !strings.Contains(stderr, "sudo loginctl enable-linger agent") {
		t.Errorf("join without lingering does not say how to keep the agent running after logout: %q", stderr)
	}
	if !strings.Contains(stderr, "verify it can reach this host") {
		t.Errorf("join on Linux calls the host a Mac: %q", stderr)
	}
}

// TestSystemdJoinFollowsXDGDirs: with XDG_DATA_HOME and XDG_CONFIG_HOME set,
// join writes there, and the unit passes XDG_DATA_HOME on so the agent's
// default state directory is the one holding its credentials.
func TestSystemdJoinFollowsXDGDirs(t *testing.T) {
	_, _, ns := metalEnvtest(t)
	host := newFakeLinux(t)
	host.lingering = true
	host.xdgDataHome = filepath.Join(host.home, "data 100%")
	host.xdgConfigHome = filepath.Join(host.home, "conf")
	joinString, _ := issueJoinString(t, ns)
	_, stderr, err := runMetal(t, "join", joinString, "--wait", "0")
	if err != nil {
		t.Fatalf("join: %v\n%s", err, stderr)
	}
	paths := host.paths()
	for _, f := range []string{filepath.Join(host.xdgDataHome, "llmkube", "metal-agent", "kubeconfig"),
		filepath.Join(host.xdgConfigHome, "systemd", "user", systemdAgentUnit)} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("join did not write %s: %v", f, err)
		}
	}
	if want := `Environment="XDG_DATA_HOME=` + strings.ReplaceAll(host.xdgDataHome, "%", "%%") + `"`; !strings.Contains(
		host.startedUnit, want+"\n") {
		t.Errorf("the unit does not set %s:\n%s", want, host.startedUnit)
	}
	if got := unitExecStart(t, host.startedUnit); got[2] != paths.Kubeconfig {
		t.Errorf("agent --kubeconfig %q, want %s", got[2], paths.Kubeconfig)
	}
	if strings.Contains(stderr, "enable-linger") {
		t.Errorf("join suggests lingering where it is on: %q", stderr)
	}
}

// TestSystemdJoinRefusesBeforeAnyChange: no reachable user instance, root,
// or no llama-server for the default runtime stop join before it writes a
// file, starts anything, or redeems the token.
func TestSystemdJoinRefusesBeforeAnyChange(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	joinString, _ := issueJoinString(t, ns)
	for _, tc := range []struct {
		name  string
		setup func(*fakeLinux)
		args  []string
		want  []string
	}{
		{"no systemd user instance", func(m *fakeLinux) { m.noUserManager = true }, nil,
			[]string{"systemd instance does not answer", "loginctl enable-linger agent", "has not been used"}},
		{"root", func(m *fakeLinux) { m.uid = 0 }, nil, []string{"not as root or with sudo"}},
		{"no llama-server for the default runtime", func(m *fakeLinux) { _ = os.Remove(m.llamaServer) }, nil,
			[]string{"llama-server not found", "runtime=ollama", "distribution's llama.cpp package"}},
		{"no llama-server for --runtime llama-server", func(m *fakeLinux) { _ = os.Remove(m.llamaServer) },
			[]string{"--agent-arg", "runtime=llama-server"}, []string{"llama-server not found"}},
		{"a --llama-server that is not there", nil, []string{"--llama-server", "/nowhere/llama-server",
			"--agent-arg", "runtime=ollama"}, []string{"not an executable file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newFakeLinux(t)
			if tc.setup != nil {
				tc.setup(host)
			}
			_, _, err := runMetal(t, append([]string{"join", joinString, "--wait", "0"}, tc.args...)...)
			for _, want := range tc.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("join: err = %v, want a refusal mentioning %q", err, want)
				}
			}
			requireLinuxNothingWritten(t, host)
		})
	}
	requireJoinStringUnused(t, c, ns)
}

// TestSystemdJoinWithoutLlamaServerForAnotherRuntime: the agent starts
// without llama-server when its runtime is another one, so join does too,
// and does not name a llama-server the agent would not find.
func TestSystemdJoinWithoutLlamaServerForAnotherRuntime(t *testing.T) {
	_, _, ns := metalEnvtest(t)
	host := newFakeLinux(t)
	_ = os.Remove(host.llamaServer)
	joinString, _ := issueJoinString(t, ns)
	_, stderr, err := runMetal(t, "join", joinString, "--wait", "0", "--agent-arg", "runtime=ollama",
		"--agent-arg", "ollama-port=8000")
	if err != nil {
		t.Fatalf("join --agent-arg runtime=ollama without llama-server: %v\n%s", err, stderr)
	}
	got := unitExecStart(t, host.startedUnit)
	for _, a := range got {
		if strings.HasPrefix(a, "--llama-server") {
			t.Errorf("agent arguments %q name llama-server, which join did not find", got)
		}
	}
	if tail := strings.Join(got[len(got)-2:], " "); tail != "--runtime=ollama --ollama-port=8000" {
		t.Errorf("agent arguments end %q, want the given runtime and port", tail)
	}
	if !strings.Contains(stderr, "runs --runtime ollama without llama.cpp") {
		t.Errorf("join does not say the agent runs without llama.cpp: %q", stderr)
	}

	// A re-join keeps the runtime, so it still needs no llama-server.
	joinString, _ = issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0"); err != nil {
		t.Fatalf("re-join keeping runtime=ollama: %v\n%s", err, stderr)
	}
	if got := unitExecStart(t, host.startedUnit); !strings.Contains(strings.Join(got, " "), "--runtime=ollama") {
		t.Errorf("re-join dropped the runtime: %q", got)
	}
}

// TestSystemdRejoinStopsThePreviousAgentFirst: a re-join keeps the previous
// unit's arguments, passes the ports the agent it replaces holds, and stops
// that agent before it starts the new one.
func TestSystemdRejoinStopsThePreviousAgentFirst(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	host := newFakeLinux(t)
	joinString, _ := issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0", "--agent-arg", "port=9091"); err != nil {
		t.Fatalf("first join: %v\n%s", err, stderr)
	}
	const agentPID = 4242
	host.pid, host.calls = agentPID, nil
	host.busy = map[string]error{"127.0.0.1:9091": addrInUse, ":9443": addrInUse}
	running := []portHolder{{pid: agentPID, command: metalAgentBinary}}
	host.holders = map[int][]portHolder{9091: running, 9443: running}

	joinString, _ = issueJoinString(t, ns)
	_, stderr, err := runMetal(t, "join", joinString, "--wait", "0")
	if err != nil {
		t.Fatalf("re-join over the running agent: %v\n%s", err, stderr)
	}
	for _, want := range []string{"keeping agent arguments from the previous join: --port=9091",
		"port 9443 is held by the agent this join replaces (pid 4242)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("re-join does not say %q: %q", want, stderr)
		}
	}
	calls := strings.Join(host.systemctlCalls(), "; ")
	if !strings.HasSuffix(calls, "daemon-reload; stop "+systemdAgentUnit+"; enable --now "+systemdAgentUnit) {
		t.Errorf("systemctl --user calls %q, want the old agent stopped before the new one starts", calls)
	}

	// lsof cannot tell, and the agent join replaces is running.
	host.holders[9443] = nil
	host.active = true
	joinString, _ = issueJoinString(t, ns)
	_, _, err = runMetal(t, "join", joinString, "--wait", "0")
	if want := "(pid 4242), stop it first: systemctl --user stop " + systemdAgentUnit; err == nil ||
		!strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "systemd would restart it") {
		t.Errorf("port held by a process lsof cannot see: err = %v, want %q", err, want)
	}
	requireJoinStringUnused(t, c, ns)
}

// TestSystemdJoinKeepsAUnitItDidNotWrite: a host whose agent runs from a unit
// written by hand keeps it unless --force, which keeps a copy outside the
// unit directories for leave to point back to.
func TestSystemdJoinKeepsAUnitItDidNotWrite(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	host := newFakeLinux(t)
	paths := host.paths()
	const own = "[Service]\nExecStart=/opt/agent --namespace edge\n"
	if err := os.MkdirAll(filepath.Dir(paths.Unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Unit, []byte(own), 0o644); err != nil { //nolint:gosec // G306: a unit
		t.Fatal(err)
	}
	joinString, _ := issueJoinString(t, ns)
	_, _, err := runMetal(t, "join", joinString, "--wait", "0")
	if err == nil || !strings.Contains(err.Error(), "did not install") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("join over another unit: err = %v, want a refusal naming --force", err)
	}
	if got, _ := os.ReadFile(paths.Unit); string(got) != own || len(host.calls) != 0 {
		t.Errorf("a refused join left the unit %q and ran systemctl %q", got, host.calls)
	}
	requireJoinStringUnused(t, c, ns)

	_, stderr, err := runMetal(t, "join", joinString, "--wait", "0", "--force")
	if err != nil {
		t.Fatalf("join --force: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "join does not keep the arguments of the systemd unit it replaces") {
		t.Errorf("join --force does not say the replaced unit's arguments are not kept: %q", stderr)
	}
	if kept, err := os.ReadFile(paths.UnitBackup); err != nil || string(kept) != own {
		t.Errorf("backup %s = %q, %v; want the replaced unit", paths.UnitBackup, kept, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(paths.Unit)); len(entries) != 1 {
		t.Errorf("the unit directory holds %d files, want only join's unit", len(entries))
	}
	stdout, _, err := runMetal(t, "leave")
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if !strings.Contains(stdout, "cp \""+paths.UnitBackup+"\" \""+paths.Unit+"\" && "+systemdStartHint) {
		t.Errorf("leave does not say how to restore the replaced unit: %q", stdout)
	}
}

// TestSystemdLeaveUndoesJoin: leave stops and disables the unit, runs the
// agent's stop-engines once it has stopped and before the credentials go,
// removes the unit and the credentials, reloads systemd, and keeps the rest
// of the state directory.
func TestSystemdLeaveUndoesJoin(t *testing.T) {
	_, _, ns := metalEnvtest(t)
	host := newFakeLinux(t)
	script := []byte("#!/bin/sh\n# usage: llmkube-metal-agent " + metaljoin.StopEnginesCommand + " [flags]\n")
	if err := os.WriteFile(host.agentBin, script, 0o755); err != nil { //nolint:gosec // G306: an executable
		t.Fatal(err)
	}
	joinString, _ := issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0"); err != nil {
		t.Fatalf("join: %v\n%s", err, stderr)
	}
	paths := host.paths()
	identity := filepath.Join(paths.Dir, "ingress", "ingress-key.pem")
	if err := os.MkdirAll(filepath.Dir(identity), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	host.calls = nil
	credentialsPresent := false
	host.onAgent = func([]string) error {
		_, errKC := os.Stat(paths.Kubeconfig)
		credentialsPresent = errKC == nil
		return nil
	}

	stdout, _, err := runMetal(t, "leave")
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	for _, f := range []string{paths.Unit, paths.Kubeconfig, paths.Token} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after leave: %v, want removed", f, err)
		}
	}
	requireExist(t, "leave", identity)
	wantCalls := []string{"show-environment", "disable --now " + systemdAgentUnit,
		"show --property=ActiveState --value " + systemdAgentUnit,
		"agent " + host.agentBin + " " + strings.Join(metaljoin.StopEnginesArgs(
			[]string{"--kubeconfig", paths.Kubeconfig, "--namespace", ns}, paths.Kubeconfig), " "),
		"daemon-reload"}
	if strings.Join(host.calls, "; ") != strings.Join(wantCalls, "; ") {
		t.Errorf("leave calls %q, want %q", host.calls, wantCalls)
	}
	if !credentialsPresent {
		t.Error("stop-engines ran after leave removed the kubeconfig it withdraws endpoints with")
	}
	if !strings.Contains(stdout, "left the cluster on this host") ||
		!strings.Contains(stdout, "llmkube metal remove host-a -n "+ns) {
		t.Errorf("leave does not say how to revoke the host: %q", stdout)
	}

	stdout, _, err = runMetal(t, "leave")
	if err != nil || !strings.Contains(stdout, "this host has not joined") {
		t.Errorf("second leave: %q, %v; want nothing to remove", stdout, err)
	}
}

// TestSystemdLeaveRemovesNothingWhileTheAgentRuns: an agent still running
// after disable --now could start an engine after stop-engines, and one
// leave cannot reach systemd to stop may be running, so leave stops there.
func TestSystemdLeaveRemovesNothingWhileTheAgentRuns(t *testing.T) {
	_, _, ns := metalEnvtest(t)
	host := newFakeLinux(t)
	joinString, _ := issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0"); err != nil {
		t.Fatalf("join: %v\n%s", err, stderr)
	}
	paths := host.paths()
	for _, tc := range []struct {
		name  string
		setup func()
		want  string
	}{
		{"still active", func() { host.stuck = true }, "the agent is not stopped"},
		{"no systemd user instance", func() { host.stuck, host.noUserManager = false, true }, "removed nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			host.calls = nil
			_, _, err := runMetal(t, "leave")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("leave: err = %v, want %q", err, tc.want)
			}
			requireExist(t, "a refused leave", paths.Unit, paths.Kubeconfig, paths.Token)
			if runs := agentRuns(host.calls); len(runs) != 0 {
				t.Errorf("a refused leave ran %q", runs)
			}
		})
	}
}

// TestSystemdLeaveLeavesAUnitJoinDidNotWrite: leave neither stops nor
// removes a unit written another way.
func TestSystemdLeaveLeavesAUnitJoinDidNotWrite(t *testing.T) {
	host := newFakeLinux(t)
	paths := host.paths()
	const own = "[Service]\nExecStart=/opt/agent\n"
	if err := os.MkdirAll(filepath.Dir(paths.Unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Unit, []byte(own), 0o644); err != nil { //nolint:gosec // G306: a unit
		t.Fatal(err)
	}
	stdout, _, err := runMetal(t, "leave")
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if got, _ := os.ReadFile(paths.Unit); string(got) != own || len(host.calls) != 0 {
		t.Errorf("leave left the unit %q and ran systemctl %q; want both untouched", got, host.calls)
	}
	if !strings.Contains(stdout, "did not install it") {
		t.Errorf("leave does not say why it left the agent: %q", stdout)
	}
}

// TestSystemdJoinSaysHowToStartAfterSystemctlFails: the string is used up
// by then, so the error must say how to start the agent without a new one.
func TestSystemdJoinSaysHowToStartAfterSystemctlFails(t *testing.T) {
	_, _, ns := metalEnvtest(t)
	for _, step := range []string{"daemon-reload", "enable"} {
		t.Run(step, func(t *testing.T) {
			host := newFakeLinux(t)
			host.failOn = step
			joinString, _ := issueJoinString(t, ns)
			_, _, err := runMetal(t, "join", joinString, "--wait", "0")
			if err == nil || !strings.Contains(err.Error(), "start it with: "+systemdStartHint) {
				t.Errorf("join with a failing %s: err = %v, want it to say %s", step, err, systemdStartHint)
			}
			if _, err := os.Stat(host.paths().Unit); err != nil {
				t.Errorf("the unit to start by hand is gone: %v", err)
			}
		})
	}
}

func TestSystemdUnitQuotingRoundTrips(t *testing.T) {
	args := []string{"/home/u/R&D <bin>/" + metalAgentBinary, "--model-store=/home/u/models 100%",
		`--allowlist=a"b`, `--x=back\slash`, "--y=$HOME and $$", "--z=it's", "--w=line\nbreak\ttab\x01"}
	unit := renderSystemdAgentUnit(args, "")
	got, err := systemdUnitExecStart(unit)
	if err != nil {
		t.Fatalf("read back: %v\n%s", err, unit)
	}
	if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
		t.Errorf("round trip %q, want %q\n%s", got, args, unit)
	}
	for _, escaped := range []string{"100%%", "$$HOME and $$$$", `a\"b`, `back\\slash`, `line\nbreak`} {
		if !strings.Contains(string(unit), escaped) {
			t.Errorf("the unit does not escape %q as systemd needs:\n%s", escaped, unit)
		}
	}
	if path, err := exec.LookPath("systemd-analyze"); err == nil {
		dir := t.TempDir()
		bin := filepath.Join(dir, metalAgentBinary)
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // G306: an executable
			t.Fatal(err)
		}
		file := filepath.Join(dir, systemdAgentUnit)
		if err := os.WriteFile(file, renderSystemdAgentUnit(append([]string{bin}, args[1:]...), ""), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(path, "verify", "--user", file).CombinedOutput(); err != nil &&
			strings.Contains(string(out), systemdAgentUnit) {
			t.Errorf("systemd-analyze verify: %v: %s", err, out)
		}
	}
}

func TestSystemdUnitExecStartReadsHandEdits(t *testing.T) {
	for _, tc := range []struct {
		name, unit string
		want       []string
		err        string
	}{
		{"unquoted words over continuation lines with a comment",
			"[Service]\nExecStart=/opt/agent \\\n# a comment\n  --namespace edge \\\n  'single quoted'\n",
			[]string{"/opt/agent", "--namespace", "edge", "single quoted"}, ""},
		{"the last ExecStart in [Service] wins, prefixes stripped",
			"[Unit]\nExecStart=/not/this\n[Service]\nExecStart=/first\nExecStart=-/opt/agent --a=b\n[Install]\n",
			[]string{"/opt/agent", "--a=b"}, ""},
		{"escapes", `[Service]` + "\n" + `ExecStart=/opt/agent "--x=\x41\101\u00e9\s"` + "\n",
			[]string{"/opt/agent", "--x=AAé "}, ""},
		{"an empty ExecStart resets it", "[Service]\nExecStart=/opt/agent\nExecStart=\n", nil, "no ExecStart"},
		{"a specifier", "[Service]\nExecStart=%h/bin/agent\n", nil, "specifier"},
		{"an environment variable", "[Service]\nExecStart=/opt/agent --home=$HOME\n", nil, "environment variable"},
		{"an unterminated quote", "[Service]\nExecStart=/opt/agent \"--a\n", nil, "unterminated"},
		{"an unknown escape", "[Service]\nExecStart=/opt/agent \\q\n", nil, "unknown escape"},
		{"a hex escape with a bad digit", "[Service]\nExecStart=/opt/agent \\xzz\n", nil, "invalid escape"},
		{"an octal escape past a byte", "[Service]\nExecStart=/opt/agent \\777\n", nil, "out of range"},
		{"a short escape", "[Service]\nExecStart=/opt/agent \\u00\n", nil, "short escape"},
		{"argv[0] apart from the binary", "[Service]\nExecStart=@/opt/agent agent\n", nil, "argv[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := systemdUnitExecStart([]byte(tc.unit))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Errorf("err = %v (args %q), want %q", err, got, tc.err)
				}
				return
			}
			if err != nil || strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("= %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestSystemdAgentPathsMatchTheAgent: join's state directory is the agent's
// --state-dir default (datadir.Dir), where it looks for the
// ingress identity and engine records next to the kubeconfig.
func TestSystemdAgentPathsMatchTheAgent(t *testing.T) {
	for _, xdg := range []string{"", "/srv/data", "relative/ignored"} {
		got := newSystemdAgentPaths(metalHost{home: "/home/u", xdgDataHome: xdg}).Dir
		want := filepath.Join("/home/u", ".local", "share", "llmkube", "metal-agent")
		if filepath.IsAbs(xdg) {
			want = filepath.Join(xdg, "llmkube", "metal-agent")
		}
		if got != want {
			t.Errorf("XDG_DATA_HOME=%q: state directory %s, want %s", xdg, got, want)
		}
		if goruntime.GOOS != "darwin" {
			t.Setenv("XDG_DATA_HOME", xdg)
			if agentDir := filepath.Join(datadir.Dir("/home/u"), "metal-agent"); got != agentDir {
				t.Errorf("XDG_DATA_HOME=%q: join writes %s, the agent reads %s", xdg, got, agentDir)
			}
		}
	}
	if got := newSystemdAgentPaths(metalHost{home: "/home/u", xdgConfigHome: "/etc/u"}).Unit; got !=
		"/etc/u/systemd/user/"+systemdAgentUnit {
		t.Errorf("unit with XDG_CONFIG_HOME: %s", got)
	}
}

// TestLinuxLlamaServerPathsMatchTheAgent: join looks for llama-server where
// the agent does (cmd/metal-agent defaultLlamaServerPaths).
func TestLinuxLlamaServerPathsMatchTheAgent(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "cmd", "metal-agent", "main.go"),
		nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var agentPaths []string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "defaultLlamaServerPaths" || len(spec.Values) != 1 {
			return true
		}
		lit, ok := spec.Values[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, e := range lit.Elts {
			if s, ok := e.(*ast.BasicLit); ok {
				v, err := strconv.Unquote(s.Value)
				if err != nil {
					t.Fatal(err)
				}
				agentPaths = append(agentPaths, v)
			}
		}
		return false
	})
	if len(agentPaths) == 0 || strings.Join(agentPaths, " ") != strings.Join(linuxLlamaServerPaths, " ") {
		t.Errorf("join searches %q on Linux, the agent %q", linuxLlamaServerPaths, agentPaths)
	}
}
