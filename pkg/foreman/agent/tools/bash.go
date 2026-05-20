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
	"time"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// Defaults are conservative enough for a hostile model and generous
// enough for typical "go build ./..." use during a coder run.
const (
	defaultBashTimeout   = 30 * time.Second
	defaultBashOutputCap = 64 * 1024 // 64 KiB combined per stream
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
