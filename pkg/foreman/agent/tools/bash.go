/*
Copyright 2025.

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

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// Defaults are conservative enough for a hostile model and generous
// enough for typical "go build ./..." use during a coder run.
//
// The output cap is per-stream (stdout and stderr each capped
// independently), so a worst-case turn surfaces ~32 KiB total. Each
// tool result stays in the OAI message history for every subsequent
// turn's prompt-eval, so per-stream caps in the tens of KiB compound
// quickly across 15+ turns. 16 KiB per stream is enough to surface
// the tail of a `go test ./...` run or a `make lint` failure, which
// is what the model actually needs to debug.
const (
	defaultBashTimeout   = 30 * time.Second
	defaultBashOutputCap = 16 * 1024 // 16 KiB per stream

	// defaultBashWaitDelay caps the time Cmd.Wait blocks waiting for
	// io.Copy goroutines on stdout/stderr to see EOF after the
	// process itself exits. Without this, an LLM-issued bash command
	// that backgrounds a grandchild (e.g. `find / -name foo &`,
	// `docker run -d`, anything that detaches) leaves the grandchild
	// holding the inherited stdout/stderr pipes, and Cmd.Wait blocks
	// indefinitely in awaitGoroutines. Empirically observed in the
	// 2026-05-25 v5 batch where `find / -maxdepth 5 -name AGENTS.md`
	// pinned a coder run for 36+ minutes (defilantech/LLMKube#539).
	//
	// 5 s is generous: with the process group killed by Cancel below,
	// kernel-level pipe drainage usually completes in <100 ms.
	defaultBashWaitDelay = 5 * time.Second
)

// BashTool runs a shell command in the workspace under "sh -c" with a
// scrubbed environment, a bounded timeout, and capped output. cwd is
// always the workspace; PATH, HOME, and GIT_* are preserved so common
// build / git tools work.
type BashTool struct {
	Workspace      string
	Timeout        time.Duration
	MaxOutputBytes int
	// EnvAllowlist optionally replaces the default allowlist. Empty
	// uses defaultBashEnvAllowlist.
	EnvAllowlist []string
}

type bashArgs struct {
	Command string `json:"command"`
}

// Name returns the tool name as advertised to the model.
func (t *BashTool) Name() string { return "bash" }

// Schema returns the OAI schema advertisement.
func (t *BashTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name: "bash",
		Description: "Run a shell command in the workspace under sh -c. " +
			"Bounded timeout; combined stdout+stderr returned with the exit code.",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "command": {"type": "string", "description": "The command to run via sh -c."}
},
"required": ["command"]
}`),
	}
}

// Execute runs the command and returns its stdout, stderr, exit code,
// and a timed_out flag. Non-zero exit codes are not errors; the tool
// surfaces them as data so the model can inspect them.
func (t *BashTool) Execute(ctx context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	var a bashArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("bash: bad args: %w", err)
	}
	if a.Command == "" {
		return nil, fmt.Errorf("bash: command is required")
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = defaultBashTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// G204 (subprocess launched with variable): the bash tool is the
	// point of admitting variable input. Containment lives at the
	// workspace boundary and the env allowlist below.
	cmd := exec.CommandContext(runCtx, "sh", "-c", a.Command) //nolint:gosec // G204: bash by design
	cmd.Dir = t.Workspace
	cmd.Env = t.scrubbedEnv()

	// Run the shell in its own process group so we can kill the entire
	// subtree (grandchildren too) when the deadline fires. Setpgid on
	// Unix; harmless no-op on platforms that ignore it. Without this,
	// a backgrounded grandchild (`sleep 60 &`, `docker run -d`, etc.)
	// outlives the shell and keeps the inherited stdout/stderr pipes
	// open, blocking Cmd.Wait forever. See defaultBashWaitDelay for
	// the empirical case that motivated the fix.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Cancel runs when runCtx fires (i.e. the BashTool deadline). The
	// os/exec default SIGKILLs the leader; we SIGKILL the whole process
	// group so grandchildren die with the shell. Required for WaitDelay
	// below to be useful: if the grandchildren stay alive, the pipes
	// stay open and io.Copy never sees EOF regardless of WaitDelay.
	cmd.Cancel = func() error {
		return killProcessGroup(cmd.Process)
	}

	// WaitDelay caps the time Cmd.Wait will block on stdio drainage
	// after the process exits (or is killed by Cancel). The pipes
	// should close almost immediately once the process group is dead;
	// the cap is paranoid insurance against edge cases (e.g. a child
	// that detaches into its own session via setsid). After WaitDelay
	// elapses, the kernel-level pipe handles are force-closed and the
	// io.Copy goroutines unblock with an io.ErrClosedPipe.
	cmd.WaitDelay = defaultBashWaitDelay

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)

	exitCode := 0
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		exitCode = 0
	case errors.As(err, &exitErr):
		exitCode = exitErr.ExitCode()
	case timedOut:
		// Process killed by the deadline; record -1 so the model can
		// distinguish "ran and exited cleanly" from "we killed it".
		exitCode = -1
	default:
		// Some other failure (sh not found, etc.). Propagate.
		return nil, fmt.Errorf("bash: %w", err)
	}

	cap := t.MaxOutputBytes
	if cap <= 0 {
		cap = defaultBashOutputCap
	}
	return &agent.ToolResult{
		Output: map[string]any{
			"command":   a.Command,
			"exit_code": exitCode,
			"stdout":    truncateBytes(stdout.Bytes(), cap),
			"stderr":    truncateBytes(stderr.Bytes(), cap),
			"timed_out": timedOut,
		},
	}, nil
}

// defaultBashEnvAllowlist names the env vars carried into the sandboxed
// shell. PATH + HOME let common tools resolve; LANG/LC_ALL/TZ keep
// locale-sensitive output deterministic enough for diffing; GIT_* and
// GITHUB_TOKEN are required for the Phase D repo helpers when they
// shell out to git from inside a bash tool call.
var defaultBashEnvAllowlist = []string{
	"PATH",
	"HOME",
	"USER",
	"LANG",
	"LC_ALL",
	"TZ",
	"GIT_ASKPASS",
	"GIT_AUTHOR_NAME",
	"GIT_AUTHOR_EMAIL",
	"GIT_COMMITTER_NAME",
	"GIT_COMMITTER_EMAIL",
	"GITHUB_TOKEN",
}

// scrubbedEnv returns the env vars in the allowlist that are actually
// set on the process. Anything else (notably ANTHROPIC_API_KEY, AWS
// credentials, etc.) does not reach the sandboxed shell.
func (t *BashTool) scrubbedEnv() []string {
	allow := t.EnvAllowlist
	if len(allow) == 0 {
		allow = defaultBashEnvAllowlist
	}
	allowSet := make(map[string]struct{}, len(allow))
	for _, k := range allow {
		allowSet[k] = struct{}{}
	}
	var out []string
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		if _, ok := allowSet[kv[:eq]]; ok {
			out = append(out, kv)
		}
	}
	return out
}

// killProcessGroup sends SIGKILL to the entire process group leader-ed
// by p. Required because Cmd.Wait blocks on `awaitGoroutines` until
// io.Copy sees EOF on the inherited stdout/stderr pipes, and those
// pipes do not close until every descendant holding them exits. A
// backgrounded grandchild (sleep, docker daemon, kubectl proxy, find
// /, etc.) inherits the shell's pipes; killing only the shell leaves
// the pipes open. Negative pid in syscall.Kill addresses the process
// group, which (combined with Setpgid: true on the shell) catches the
// whole subtree.
//
// nil p means the process never started; nothing to kill.
//
// Any error is ignored at the call site -- this runs from Cmd.Cancel
// on context expiry, where the goal is best-effort cleanup. If
// signaling the group fails (ESRCH because the leader already exited
// cleanly, EPERM in some sandboxed containers), the WaitDelay timeout
// will still force pipe closure as a fallback.
func killProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	// Negative pid in syscall.Kill targets the process group with that
	// pgid. Combined with SysProcAttr.Setpgid: true on the leader,
	// pgid == leader's pid.
	return syscall.Kill(-p.Pid, syscall.SIGKILL)
}

// truncateBytes returns b as a string, capped at maxBytes with a
// truncation marker if it overflows.
func truncateBytes(b []byte, maxBytes int) string {
	const suffix = "\n... [output truncated]"
	if len(b) <= maxBytes {
		return string(b)
	}
	if maxBytes <= len(suffix) {
		return suffix
	}
	head := b[:maxBytes-len(suffix)]
	return string(head) + suffix
}
