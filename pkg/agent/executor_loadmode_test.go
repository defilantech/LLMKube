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
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// newBuildHelp is an excerpt of `llama-server --help` from Homebrew llama.cpp
// 0.5.0 (build 11146, commit 7fe450e19), the first build that dropped --mlock.
// It still mentions "mlock" in the mode descriptions, so detection must key on
// the --load-mode option and never on the absence of the word "mlock".
const newBuildHelp = `-dt,   --defrag-thold N                 KV cache defragmentation threshold (DEPRECATED)
                                        (env: LLAMA_ARG_DEFRAG_THOLD)
-lm,   --load-mode MODE                 model loading mode (default: auto)
                                        - auto: mmap, unless a device does not support it
                                        - none: no special loading mode
                                        - mmap: memory-map model (if mmap disabled, slower load but may reduce
                                        pageouts if not using mlock)
                                        - mlock: force system to keep model in RAM rather than swapping or
                                        compressing
                                        - mmap+mlock: mmap + force system to keep model in RAM rather than
                                        swapping or compressing
                                        - dio: use DirectIO if available
`

// oldBuildHelp is the equivalent excerpt from a pre-0.5.0 build, which has
// --mlock and no --load-mode. Long lines are split only to satisfy lll; the
// joined bytes are the real help text.
const oldBuildHelp = "      --mlock                          force system to keep model in RAM " +
	"rather than swapping or compressing\n" +
	"                                       (env: LLAMA_ARG_MLOCK)\n" +
	"      --no-mmap                        do not memory-map model " +
	"(slower load but may reduce pageouts if not using mlock)\n"

func countToken(args []string, token string) int {
	n := 0
	for _, a := range args {
		if a == token {
			n++
		}
	}
	return n
}

func TestBuildLlamaServerArgs_MlockSpelling(t *testing.T) {
	tests := []struct {
		name          string
		loadMode      bool
		wantLoadMode  string
		wantMlockFlag int
	}{
		{"old build keeps --mlock", false, "", 1},
		{"new build emits --load-mode mmap+mlock", true, "mmap+mlock", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := buildLlamaServerArgs("/models/m.gguf", 8080, ExecutorConfig{
				ContextSize:       4096,
				Mlock:             true,
				LoadModeSupported: tc.loadMode,
			})
			if got := countToken(args, "--mlock"); got != tc.wantMlockFlag {
				t.Errorf("--mlock count = %d, want %d; args: %v", got, tc.wantMlockFlag, args)
			}
			if got := flagValue(args, "--load-mode"); got != tc.wantLoadMode {
				t.Errorf("--load-mode = %q, want %q; args: %v", got, tc.wantLoadMode, args)
			}
		})
	}
}

func TestBuildLlamaServerArgs_MlockOffEmitsNeither(t *testing.T) {
	for _, loadMode := range []bool{false, true} {
		args := buildLlamaServerArgs("/models/m.gguf", 8080, ExecutorConfig{
			ContextSize:       4096,
			LoadModeSupported: loadMode,
		})
		if hasFlag(args, "--mlock") || hasFlag(args, "--load-mode") {
			t.Errorf("loadMode=%v: Mlock=false must emit no load flag; args: %v", loadMode, args)
		}
	}
}

// TestBuildLlamaServerArgs_LoadFlagNotDuplicated pins the extraArgs guard: a
// user who already chose a load mode (in either spelling, on either build) must
// not get a second, operator-added flag. The user's tokens are appended last,
// so the check counts the operator-owned tokens that precede them.
func TestBuildLlamaServerArgs_LoadFlagNotDuplicated(t *testing.T) {
	extras := map[string][]string{
		"--mlock":          {"--mlock"},
		"--load-mode sep":  {"--load-mode", "mmap"},
		"--load-mode=":     {"--load-mode=mlock"},
		"-lm sep":          {"-lm", "mmap"},
		"-lm=":             {"-lm=mmap"},
		"--mlock=inline":   {"--mlock=true"},
		"unrelated extras": nil,
	}
	for name, extra := range extras {
		for _, loadMode := range []bool{false, true} {
			args := buildLlamaServerArgs("/models/m.gguf", 8080, ExecutorConfig{
				ContextSize:       4096,
				Mlock:             true,
				LoadModeSupported: loadMode,
				ExtraArgs:         extra,
			})
			operator := args[:len(args)-len(extra)]
			added := countToken(operator, "--mlock") + countToken(operator, "--load-mode")
			want := 0
			if extra == nil {
				want = 1
			}
			if added != want {
				t.Errorf("%s (loadMode=%v): operator added %d load flags, want %d; args: %v",
					name, loadMode, added, want, args)
			}
		}
	}
}

func TestHelpSupportsLoadMode(t *testing.T) {
	if !helpSupportsLoadMode(newBuildHelp) {
		t.Error("new-build help (has --load-mode) must report supported")
	}
	if helpSupportsLoadMode(oldBuildHelp) {
		t.Error("old-build help (only --mlock) must report unsupported")
	}
	if helpSupportsLoadMode("") {
		t.Error("empty help must report unsupported")
	}
}

func newProbedExecutor(t *testing.T, help string, err error, calls *int) *MetalExecutor {
	t.Helper()
	e := NewMetalExecutor("/fake/llama-server", t.TempDir(), newNopLogger())
	e.helpProbe = func(_ context.Context, bin string, _ []string) (string, error) {
		*calls++
		if bin != "/fake/llama-server" {
			t.Errorf("probe bin = %q, want the executor's llama-server", bin)
		}
		return help, err
	}
	return e
}

// TestLlamaServerArgs_ProbesOnceAndCaches runs the StartProcess arg path twice
// and requires one probe: the binary is fixed for the executor's life, so
// re-running --help on every spawn is waste.
func TestLlamaServerArgs_ProbesOnceAndCaches(t *testing.T) {
	calls := 0
	e := newProbedExecutor(t, newBuildHelp, nil, &calls)
	cfg := ExecutorConfig{ContextSize: 4096, Mlock: true}

	for i := range 2 {
		args := e.llamaServerArgs(context.Background(), "/models/m.gguf", 8080, cfg)
		if got := flagValue(args, "--load-mode"); got != "mmap+mlock" {
			t.Fatalf("call %d: --load-mode = %q, want mmap+mlock; args: %v", i, got, args)
		}
		if hasFlag(args, "--mlock") {
			t.Fatalf("call %d: --mlock must not be emitted on a --load-mode build; args: %v", i, args)
		}
	}
	if calls != 1 {
		t.Errorf("help probe called %d times, want 1", calls)
	}
}

func TestLlamaServerArgs_OldBuildKeepsMlock(t *testing.T) {
	calls := 0
	e := newProbedExecutor(t, oldBuildHelp, nil, &calls)
	args := e.llamaServerArgs(context.Background(), "/models/m.gguf", 8080,
		ExecutorConfig{ContextSize: 4096, Mlock: true})
	if !hasFlag(args, "--mlock") || hasFlag(args, "--load-mode") {
		t.Errorf("old build must get --mlock only; args: %v", args)
	}
}

// TestLlamaServerArgs_ProbeFailureFallsBackToMlock covers a probe that errors
// or times out: the executor keeps the historical --mlock rather than guessing
// a flag the binary may not know, and does not cache the failure, so a later
// spawn gets another chance to detect a --load-mode build.
func TestLlamaServerArgs_ProbeFailureFallsBackToMlock(t *testing.T) {
	calls := 0
	e := newProbedExecutor(t, "", errors.New("signal: killed"), &calls)
	cfg := ExecutorConfig{ContextSize: 4096, Mlock: true}

	for range 2 {
		args := e.llamaServerArgs(context.Background(), "/models/m.gguf", 8080, cfg)
		if !hasFlag(args, "--mlock") || hasFlag(args, "--load-mode") {
			t.Fatalf("probe failure must fall back to --mlock; args: %v", args)
		}
	}
	if calls != 2 {
		t.Errorf("failed probe calls = %d, want 2 (a failure must not be cached)", calls)
	}

	// Once the binary answers, the result is cached from then on.
	e.helpProbe = func(context.Context, string, []string) (string, error) {
		calls++
		return newBuildHelp, nil
	}
	for range 2 {
		args := e.llamaServerArgs(context.Background(), "/models/m.gguf", 8080, cfg)
		if !slices.Contains(args, "mmap+mlock") {
			t.Fatalf("recovered probe must switch to --load-mode; args: %v", args)
		}
	}
	if calls != 3 {
		t.Errorf("total probe calls = %d, want 3", calls)
	}
}

func TestNewMetalExecutor_DefaultHelpProbe(t *testing.T) {
	e := NewMetalExecutor("/fake/llama-server", t.TempDir(), newNopLogger())
	if e.helpProbe == nil {
		t.Fatal("helpProbe must default to the exec-based probe")
	}
}

// writeScript drops an executable shell script into t.TempDir().
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

// TestExecHelpProbe drives the real exec probe against fake binaries that
// print each build's help, so the seam's default is covered end to end.
func TestExecHelpProbe(t *testing.T) {
	for name, tc := range map[string]struct {
		help string
		want bool
	}{
		"new build": {newBuildHelp, true},
		"old build": {oldBuildHelp, false},
	} {
		t.Run(name, func(t *testing.T) {
			bin := writeScript(t, "cat <<'HELP'\n"+tc.help+"HELP\n")
			out, err := execHelpProbe(context.Background(), bin, buildEngineEnv(os.Environ(), nil))
			if err != nil {
				t.Fatalf("execHelpProbe: %v", err)
			}
			if got := helpSupportsLoadMode(out); got != tc.want {
				t.Errorf("helpSupportsLoadMode = %v, want %v; output: %q", got, tc.want, out)
			}
		})
	}
}

func TestExecHelpProbe_Timeout(t *testing.T) {
	orig := helpProbeTimeout
	helpProbeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { helpProbeTimeout = orig })

	bin := writeScript(t, "exec sleep 30\n")
	start := time.Now()
	if _, err := execHelpProbe(context.Background(), bin, buildEngineEnv(os.Environ(), nil)); err == nil {
		t.Fatal("a hung --help must return an error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("probe took %s; the timeout did not bound it", elapsed)
	}
}
