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
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

const (
	// defaultRipwireToolTimeout bounds one ripwire invocation.
	defaultRipwireToolTimeout = 60 * time.Second
	// defaultRipwireMaxBytes caps returned output so a large call graph does
	// not dominate the transcript budget.
	defaultRipwireMaxBytes = 32 * 1024
	// ripwireBinEnv overrides the ripwire binary path; unset resolves
	// "ripwire" from PATH. Mirrors the repo-map backend's resolver
	// (pkg/foreman/agent/ripwire.go) so the tool and the backend find the same
	// binary, but the tool deliberately does not depend on that package's
	// helper: the tool must build and run whether or not the backend is
	// wired.
	ripwireBinEnv = "FOREMAN_RIPWIRE_BIN"
)

// ripwireBin resolves the ripwire binary for the tool.
func ripwireBin() string {
	if b := strings.TrimSpace(os.Getenv(ripwireBinEnv)); b != "" {
		return b
	}
	return "ripwire"
}

// ripwireVerbFlag maps the model's verb choice to the one ripwire flag it may
// select. The model picks the verb; it never supplies a flag, and the query is
// appended to the fixed prefix as a single argv element, so a query like
// "--apply" cannot become a second flag.
var ripwireVerbFlag = map[string]string{
	"for":     "--for=",
	"callers": "--callers=",
	"impact":  "--impact=",
	"tests":   "--affected=",
}

// RipwireTool queries the ripwire call graph over the workspace. Read-only:
// only the fixed verb set above is reachable, never an edit verb.
type RipwireTool struct {
	Workspace string
	// Timeout bounds one invocation. 0 falls back to
	// defaultRipwireToolTimeout.
	Timeout time.Duration
	// MaxBytes caps returned output. 0 falls back to defaultRipwireMaxBytes.
	MaxBytes int
}

type ripwireArgs struct {
	Verb  string `json:"verb"`
	Query string `json:"query"`
}

// Name returns the tool name as advertised to the model.
func (t *RipwireTool) Name() string { return "ripwire" }

// Schema returns the OAI schema advertisement. The verb is an enum so the
// model cannot invent a flag; the query is opaque text.
func (t *RipwireTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name: "ripwire",
		Description: "Query the ripwire call graph over the workspace. Verbs: " +
			"for (rank files for a task), callers (who calls a symbol), " +
			"impact (the blast radius of a symbol), tests (tests that reach a file or symbol).",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "verb":  {"type": "string", "enum": ["for","callers","impact","tests"], "description": "Which ripwire query to run."},
  "query": {"type": "string", "description": "The task text when verb=for; otherwise the symbol or file path."}
},
"required": ["verb", "query"]
}`),
	}
}

// Execute runs one ripwire query. Model-supplied args reach ripwire only as:
// a verb validated against the fixed map, and a query appended to that verb's
// fixed flag prefix as one argv element. No shell, a hard timeout, and an
// output cap.
func (t *RipwireTool) Execute(ctx context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	var a ripwireArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("ripwire: bad args: %w", err)
	}
	prefix, ok := ripwireVerbFlag[a.Verb]
	if !ok {
		return nil, fmt.Errorf("ripwire: unknown verb %q", a.Verb)
	}
	if strings.TrimSpace(a.Query) == "" {
		return nil, fmt.Errorf("ripwire: query is required")
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = defaultRipwireToolTimeout
	}
	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultRipwireMaxBytes
	}

	argv := []string{t.Workspace, prefix + a.Query, "--json"}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, ripwireBin(), argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil && stdout.Len() == 0 {
		// A non-zero exit with output on stdout is a legitimate result
		// (ripwire's finding verbs exit non-zero); only an empty result with
		// an error is a tool failure.
		return nil, fmt.Errorf("ripwire %s: %w: %s", a.Verb, err, strings.TrimSpace(stderr.String()))
	}

	out, truncated := capRipwireBytes(stdout.Bytes(), maxBytes)
	return &agent.ToolResult{
		Output: map[string]any{
			"verb":      a.Verb,
			"result":    out,
			"truncated": truncated,
		},
	}, nil
}

// capRipwireBytes clips b to at most maxBytes bytes on a rune boundary and
// reports whether it clipped, so a model reading a cut result knows it was
// cut rather than reasoning from a partial call graph as if it were whole.
func capRipwireBytes(b []byte, maxBytes int) (string, bool) {
	if len(b) <= maxBytes {
		return string(b), false
	}
	cut := b[:maxBytes]
	for len(cut) > 0 && !utf8.Valid(cut) {
		cut = cut[:len(cut)-1]
	}
	return string(cut) + "\n...(truncated)...", true
}
