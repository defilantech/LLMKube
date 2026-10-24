package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestProcessInspector_StartTokenStableForSameProcess(t *testing.T) {
	in := newProcessInspector()
	a, err := in.StartToken(os.Getpid())
	if err != nil {
		t.Fatalf("StartToken(self): %v", err)
	}
	b, err := in.StartToken(os.Getpid())
	if err != nil {
		t.Fatalf("StartToken(self) again: %v", err)
	}
	if a == "" || a != b {
		t.Fatalf("tokens %q and %q, want one non-empty stable token", a, b)
	}
}

func TestProcessInspector_StartTokenDiffersForAnotherProcess(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	in := newProcessInspector()
	self, err := in.StartToken(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	child, err := in.StartToken(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("StartToken(child): %v", err)
	}
	if child == "" || child == self {
		t.Fatalf("child token %q, self %q: want a different non-empty token", child, self)
	}
}

func TestProcessInspector_StartTokenErrorsForDeadPID(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := newProcessInspector().StartToken(cmd.Process.Pid); err == nil {
		t.Fatal("StartToken on a reaped PID returned no error")
	}
}

func TestProcessInspector_ExecutableOfSelf(t *testing.T) {
	want, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, _ = filepath.EvalSymlinks(want)
	got, err := newProcessInspector().Executables(os.Getpid())
	if err != nil || !executableMatches(got, want) {
		t.Fatalf("Executables(self) = %q, %v; want it to include %q", got, err, want)
	}
}

// A pip-installed engine is a #! script: the record holds the script path,
// which must match although the kernel runs the interpreter.
func TestProcessInspector_ScriptEngineMatchesItsScript(t *testing.T) {
	script := filepath.Join(t.TempDir(), "mlx-server")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile :; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script, "--model", "m")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	want, _ := filepath.EvalSymlinks(script)
	got, err := newProcessInspector().Executables(cmd.Process.Pid)
	if err != nil || !executableMatches(got, want) {
		t.Fatalf("Executables(script) = %q, %v; want it to include %q", got, err, want)
	}
	if executableMatches(got, "/usr/local/bin/llama-server") {
		t.Fatal("an unrelated binary matched")
	}
}

func TestExecutableMatches_EmptyRecordNeverMatches(t *testing.T) {
	if executableMatches([]string{""}, "") || executableMatches(nil, "") {
		t.Fatal("an empty recorded path matched")
	}
}
