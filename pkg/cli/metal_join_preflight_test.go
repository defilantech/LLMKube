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
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// addrInUse is the error net.Listen returns for a taken port.
var addrInUse = &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}

// requireJoinStringUnused fails if the join string was redeemed: join
// deletes its Secret only after minting the agent's token.
func requireJoinStringUnused(t *testing.T, c client.Client, ns string) {
	t.Helper()
	key := types.NamespacedName{Namespace: ns, Name: "llmkube-metal-" + joinTestNode + "-join"}
	if err := c.Get(context.Background(), key, &corev1.Secret{}); err != nil {
		t.Errorf("the join string was used up: get join Secret: %v", err)
	}
}

func requireNothingWritten(t *testing.T, mac *fakeMac) {
	t.Helper()
	if entries, _ := os.ReadDir(mac.home); len(entries) != 0 {
		t.Errorf("a refused join wrote %v", entries)
	}
	for _, c := range mac.calls {
		if !strings.HasPrefix(c, "print ") {
			t.Errorf("a refused join ran launchctl %s", c)
		}
	}
}

// TestMetalJoinRefusesRootBeforeDecodingTheString: sudo llmkube metal join
// would redeem the string and then fail at launchctl bootstrap gui/0. join
// refuses before it even reads the string.
func TestMetalJoinRefusesRootBeforeDecodingTheString(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	joinString, _ := issueJoinString(t, ns)
	for _, s := range []string{"not-a-join-string", joinString} {
		mac := newFakeMac(t)
		mac.uid = 0
		_, _, err := runMetal(t, "join", s, "--wait", "0")
		if err == nil || !strings.Contains(err.Error(), "not as root or with sudo") {
			t.Fatalf("join as root with %.20q: err = %v, want the root refusal", s, err)
		}
		requireNothingWritten(t, mac)
		if len(mac.calls) != 0 {
			t.Errorf("join as root ran launchctl %q", mac.calls)
		}
	}
	requireJoinStringUnused(t, c, ns)
}

// TestMetalJoinRefusesWithoutAGUISession: over SSH with nobody logged in at
// the Mac there is no gui/<uid> domain to bootstrap the LaunchAgent into.
// join says so before it uses the string.
func TestMetalJoinRefusesWithoutAGUISession(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	mac.noGUISession = true
	joinString, _ := issueJoinString(t, ns)
	_, _, err := runMetal(t, "join", joinString, "--wait", "0")
	if err == nil || !strings.Contains(err.Error(), "no GUI login session") ||
		!strings.Contains(err.Error(), "has not been used") || !strings.Contains(err.Error(), "Could not find domain") {
		t.Fatalf("join without a GUI session: err = %v, want a refusal that says why and that the string is unused", err)
	}
	requireNothingWritten(t, mac)
	if want := []string{"print gui/501"}; !reflect.DeepEqual(mac.calls, want) {
		t.Errorf("launchctl calls %q, want only %q", mac.calls, want)
	}
	requireJoinStringUnused(t, c, ns)
}

// TestMetalJoinRefusesABusyPort: a taken health or ingress port makes the
// agent exit at startup and launchd restart it every 30 seconds; a taken
// client port leaves it without its client proxy. join names the port, the
// process holding it, and how to pick another, before it uses the string.
func TestMetalJoinRefusesABusyPort(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	joinString, _ := issueJoinString(t, ns)
	for _, tc := range []struct {
		addr    string
		port    int
		args    []string
		holders []portHolder
		want    []string
	}{
		{"127.0.0.1:9090", 9090, nil, []portHolder{{pid: 812, command: "prometheus"}},
			[]string{"port 9090 (127.0.0.1:9090)", "health and metrics server (--port)", "prometheus (pid 812)",
				"exit at startup", "--agent-arg port=<free port>"}},
		{":9443", 9443, nil, []portHolder{{pid: 77, command: "kubectl"}, {pid: 78, command: "kubectl"}},
			[]string{"port 9443 (:9443)", "--ingress-port", "kubectl (pid 77), kubectl (pid 78)",
				"--agent-arg ingress-port=<free port>"}},
		{"127.0.0.1:9999", 9999, nil, nil,
			[]string{"client proxy", "lsof cannot see by what", "client-port=0 turns it off"}},
		{"127.0.0.1:9191", 9191, []string{"--agent-arg", "port=9191"}, nil, []string{"port 9191", "--port"}},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			mac := newFakeMac(t)
			mac.busy = map[string]error{tc.addr: addrInUse}
			mac.holders = map[int][]portHolder{tc.port: tc.holders}
			_, _, err := runMetal(t, append([]string{"join", joinString, "--wait", "0"}, tc.args...)...)
			if err == nil {
				t.Fatal("join with a busy port succeeded")
			}
			for _, want := range append(tc.want, "has not been used") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("busy %s: error does not say %q: %v", tc.addr, want, err)
				}
			}
			requireNothingWritten(t, mac)
		})
	}
	requireJoinStringUnused(t, c, ns)
}

// TestMetalJoinPortCheckBindsThePortForReal: the check is a real bind, at
// the agent's address, of a port the test holds (an ephemeral one: the Mac
// running the tests may run an agent on the defaults).
func TestMetalJoinPortCheckBindsThePortForReal(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	port := held.Addr().(*net.TCPAddr).Port
	mac.realPorts = map[int]bool{port: true}
	joinString, _ := issueJoinString(t, ns)
	_, _, err = runMetal(t, "join", joinString, "--wait", "0", "--agent-arg", "port="+strconv.Itoa(port))
	if want := fmt.Sprintf("port %d (127.0.0.1:%d), which the agent's health and metrics server (--port) binds, "+
		"is in use", port, port); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("join with --port %d held: err = %v, want %q", port, err, want)
	}
	requireNothingWritten(t, mac)
	requireJoinStringUnused(t, c, ns)
}

// TestCheckMetalAgentPortsNamesTheHolder runs the real bind and the real lsof
// against an ephemeral port this test holds, so lsof names this process; and
// a port it finds free is free again afterwards, for the agent to bind.
func TestCheckMetalAgentPortsNamesTheHolder(t *testing.T) {
	if _, err := os.Stat("/usr/sbin/lsof"); err != nil {
		t.Skip("no /usr/sbin/lsof")
	}
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	port := held.Addr().(*net.TCPAddr).Port
	host := metalHost{
		uid:         501,
		listen:      net.Listen,
		portHolders: lsofPortHolders,
		launchctl: func(context.Context, ...string) (string, error) {
			return "", errors.New("Could not find service")
		},
	}
	busy := []metalAgentPort{{flag: "port", what: "health and metrics server", fatal: true,
		addr: "127.0.0.1:" + strconv.Itoa(port), port: port}}
	err = checkMetalAgentPorts(context.Background(), host, busy, io.Discard)
	if want := fmt.Sprintf("(pid %d)", os.Getpid()); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("port held by this test: err = %v, want lsof to name it %s", err, want)
	}
	_ = held.Close()

	if err := checkMetalAgentPorts(context.Background(), host, busy, io.Discard); err != nil {
		t.Fatalf("a free port: %v", err)
	}
	again, err := net.Listen("tcp", busy[0].addr)
	if err != nil {
		t.Fatalf("the check left %s bound: %v", busy[0].addr, err)
	}
	_ = again.Close()
}

func TestParseLsofHolders(t *testing.T) {
	// lsof -nP -iTCP:9443 -sTCP:LISTEN -Fpc, as macOS prints it.
	out := "p94367\ncllmkube-metal-agent\nf7\np812\ncprometheus\nf9\nf10\n"
	want := []portHolder{{pid: 94367, command: "llmkube-metal-agent"}, {pid: 812, command: "prometheus"}}
	if got := parseLsofHolders([]byte(out)); !reflect.DeepEqual(got, want) {
		t.Errorf("parseLsofHolders = %v, want %v", got, want)
	}
	if got := parseLsofHolders(nil); len(got) != 0 {
		t.Errorf("no output: %v, want none", got)
	}
}

// TestMetalRejoinPassesPortsHeldByTheAgentItReplaces: on a re-join the
// running agent holds its own ports, and join stops it before the new one
// starts, so those ports pass. The same ports held by another process, even
// another llmkube-metal-agent, do not.
func TestMetalRejoinPassesPortsHeldByTheAgentItReplaces(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	joinString, _ := issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0"); err != nil {
		t.Fatalf("first join: %v\n%s", err, stderr)
	}
	const agentPID = 4242
	mac.pid = agentPID
	mac.busy = map[string]error{"127.0.0.1:9090": addrInUse, ":9443": addrInUse, "127.0.0.1:9999": addrInUse}
	running := []portHolder{{pid: agentPID, command: metalAgentBinary}}
	mac.holders = map[int][]portHolder{9090: running, 9443: running, 9999: running}

	joinString, _ = issueJoinString(t, ns)
	_, stderr, err := runMetal(t, "join", joinString, "--wait", "0")
	if err != nil {
		t.Fatalf("re-join over the running agent: %v\n%s", err, stderr)
	}
	if want := "port 9443 is held by the agent this join replaces (pid 4242)"; !strings.Contains(stderr, want) {
		t.Errorf("re-join does not say why a held port passed: %q", stderr)
	}

	// An agent someone started by hand: join would not stop it.
	mac.holders[9443] = []portHolder{{pid: 999, command: metalAgentBinary}}
	joinString, _ = issueJoinString(t, ns)
	_, _, err = runMetal(t, "join", joinString, "--wait", "0")
	if want := "in use by " + metalAgentBinary + " (pid 999)"; err == nil || !strings.Contains(err.Error(), want) ||
		strings.Contains(err.Error(), "launchctl bootout") {
		t.Errorf("port held by another agent process: err = %v, want it to say %q and no bootout hint", err, want)
	}
	requireJoinStringUnused(t, c, ns)

	// lsof cannot tell, and the agent join replaces is running: say how to
	// stop that one, in case it holds the port.
	mac.holders[9443] = nil
	joinString, _ = issueJoinString(t, ns)
	_, _, err = runMetal(t, "join", joinString, "--wait", "0")
	if want := "(pid 4242), stop it first: launchctl bootout gui/501/" + metalAgentLabel; err == nil ||
		!strings.Contains(err.Error(), want) {
		t.Errorf("port held by a process lsof cannot see: err = %v, want %q", err, want)
	}
	requireJoinStringUnused(t, c, ns)
}
