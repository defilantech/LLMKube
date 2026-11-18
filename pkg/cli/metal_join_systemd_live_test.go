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
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// TestSystemdAgentUnitLive installs, starts and removes the agent's unit
// against this user's real systemd instance. It writes the real unit path, so
// it runs only with LLMKUBE_TEST_SYSTEMD_USER=1, on Linux, with an instance
// that answers and no unit of a real join in the way. The shell's
// XDG_CONFIG_HOME and XDG_DATA_HOME point elsewhere than the instance's: the
// unit must still be where systemd loads it, and the agent must get the data
// home join used.
func TestSystemdAgentUnitLive(t *testing.T) {
	if os.Getenv("LLMKUBE_TEST_SYSTEMD_USER") != "1" {
		t.Skip("set LLMKUBE_TEST_SYSTEMD_USER=1 to run against this user's systemd instance")
	}
	if goruntime.GOOS != goosLinux {
		t.Skipf("systemd user units are Linux only, not %s", goruntime.GOOS)
	}
	shellConfig, dataHome := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", shellConfig)
	t.Setenv("XDG_DATA_HOME", dataHome)
	host, err := newMetalHost()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	env, err := checkSystemdUserManager(ctx, host)
	if err != nil {
		t.Fatalf("LLMKUBE_TEST_SYSTEMD_USER=1 but no systemd user instance answers: %v", err)
	}
	unitDir, err := systemdUnitDir(ctx, host, env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(unitDir, shellConfig) {
		t.Fatalf("unit directory %s follows the shell's XDG_CONFIG_HOME, which systemd does not read", unitDir)
	}
	for _, addr := range []string{"127.0.0.1:0", ":0"} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if !host.pidListens(os.Getpid(), port) {
			t.Errorf("/proc does not show this process listening on %s (port %d)", addr, port)
		}
		_ = ln.Close()
	}
	paths := newSystemdAgentPaths(host, unitDir)
	if _, err := os.Stat(paths.Unit); err == nil {
		t.Skipf("%s exists; not touching a real join", paths.Unit)
	}

	dir := t.TempDir()
	stopped, ran := filepath.Join(dir, "stop-engines-args"), filepath.Join(dir, "agent-args")
	bin := filepath.Join(dir, metalAgentBinary)
	script := "#!/bin/sh\nif [ \"$1\" = stop-engines ]; then printf '%s\\n' \"$@\" > '" + stopped + "'; exit 0; fi\n" +
		"printf '%s\\n' \"$XDG_DATA_HOME\" \"$@\" > '" + ran + "'\nexec sleep 600\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // G306: an executable
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = host.systemctl(context.Background(), "disable", "--now", systemdAgentUnit)
		_ = os.Remove(paths.Unit)
		_, _ = host.systemctl(context.Background(), "daemon-reload")
	})

	activeState := func() string {
		out, _ := host.systemctl(ctx, "show", "--property=ActiveState", "--value", systemdAgentUnit)
		return strings.TrimSpace(out)
	}
	namespace := `live 100% $HOME "q" back\slash`
	args := []string{bin, "--kubeconfig", paths.Kubeconfig, "--namespace", namespace}
	if _, err := installSystemdAgent(ctx, host, paths, agentServiceState{}, args, io.Discard); err != nil {
		t.Fatalf("install: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for activeState() != "active" || systemdAgentMainPID(ctx, host) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the unit is %q with main pid %d, want active and running", activeState(),
				systemdAgentMainPID(ctx, host))
		}
		time.Sleep(200 * time.Millisecond)
	}
	for {
		got, err := os.ReadFile(ran)
		if want := strings.Join(append([]string{dataHome}, args[1:]...), "\n") + "\n"; err == nil &&
			string(got) == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("systemd started the agent with %q (%v), want XDG_DATA_HOME %s and %q", got, err, dataHome,
				args[1:])
		}
		time.Sleep(200 * time.Millisecond)
	}

	// leave from a shell with another data home still finds the agent's.
	host.xdgDataHome = ""
	var out, errOut bytes.Buffer
	if err := runSystemdLeave(ctx, host, &out, &errOut); err != nil {
		t.Fatalf("leave: %v\n%s%s", err, out.String(), errOut.String())
	}
	if _, err := os.Stat(paths.Unit); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s after leave: %v, want removed", paths.Unit, err)
	}
	if state := activeState(); state == "active" {
		t.Errorf("the unit is %s after leave", state)
	}
	got, err := os.ReadFile(stopped)
	if want := strings.Join([]string{"stop-engines", "--kubeconfig", paths.Kubeconfig, "--namespace", namespace,
		"--state-dir", paths.Dir}, "\n") + "\n"; err != nil || string(got) != want {
		t.Errorf("stop-engines ran with %q (%v), want %q", got, err, want)
	}
}
