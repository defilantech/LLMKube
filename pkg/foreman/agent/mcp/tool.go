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

package mcp

import (
	"context"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
	"github.com/defilantech/llmkube/pkg/foreman/agent/tools"
)

// truncationMarker is appended to a tool result when callTool's text is
// cut down to opts.MaxResultBytes. The ellipsis makes it visually
// distinct from real tool output when a model scans the transcript.
const truncationMarker = "\n…[truncated]"

// mcpCallRecord is a single observation of an MCP tool call, handed to
// the record callback newTools/newTool are given. It exists so callers
// (Task 4's server manager) can log or trace every call without mcpTool
// itself taking a logging or tracing dependency.
type mcpCallRecord struct {
	Server      string
	Tool        string // the bare MCP tool name, not the namespaced one
	Args        json.RawMessage
	ResultBytes int
	Truncated   bool
	LatencyMs   int64
	Error       string // "" on success; the callTool error string otherwise
}

// caller is the subset of *Session that mcpTool depends on. Extracting
// it lets tests inject a fake in-process double instead of dialing a
// real MCP server.
type caller interface {
	callTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error)
}

// mcpTool adapts one tool discovered on a remote MCP server to the
// Foreman tools.Tool interface, so it flows through the native loop's
// dispatch, whitelist, and transcript path exactly like a built-in tool.
type mcpTool struct {
	caller      caller
	server      string
	toolName    string // bare MCP tool name, e.g. "echo"
	description string
	inputSchema json.RawMessage
	opts        Options
	record      func(mcpCallRecord)
}

// newTool builds a single mcpTool around c. It is the seam tests use to
// construct an mcpTool around a fake caller without a real Session;
// newTools (the production entry point) calls it once per discovered
// tool and then fills in description/inputSchema from the toolDesc.
func newTool(c caller, server, toolName string, opts Options, record func(mcpCallRecord)) *mcpTool {
	return &mcpTool{
		caller:   c,
		server:   server,
		toolName: toolName,
		opts:     opts.withDefaults(),
		record:   record,
	}
}

// newTools adapts every tool discovered on server's Session into a
// tools.Tool, namespaced as "mcp/<server.Name>/<tool name>" so tools
// from different MCP servers (or a same-named native tool) never
// collide in the registry. record is invoked once per Execute call;
// pass nil if the caller does not need call observations.
func newTools(
	s *Session, server ServerConfig, discovered []toolDesc, opts Options, record func(mcpCallRecord),
) []tools.Tool {
	out := make([]tools.Tool, 0, len(discovered))
	for _, d := range discovered {
		t := newTool(s, server.Name, d.Name, opts, record)
		t.description = d.Description
		t.inputSchema = d.InputSchema
		out = append(out, t)
	}
	return out
}

// Name returns the namespaced tool name as advertised to the model and
// used as the registry key.
func (t *mcpTool) Name() string {
	return "mcp/" + t.server + "/" + t.toolName
}

// Schema returns the OAI schema advertisement, built from the remote
// server's tool description. Falls back to a minimal object schema when
// the server advertised no input schema, so the model always sees valid
// JSONSchema for the arguments.
func (t *mcpTool) Schema() oai.ToolSchemaDef {
	params := t.inputSchema
	if len(params) == 0 {
		params = json.RawMessage(`{"type":"object"}`)
	}
	return oai.ToolSchemaDef{
		Name:        t.Name(),
		Description: t.description,
		Parameters:  params,
	}
}

// Execute calls the remote tool via t.caller and returns its text as a
// soft result. MCP must never fail a Foreman run: both a callTool Go
// error and a tool-level error result (isError=true, but nil Go error)
// flow back as Output text rather than as a Go error, which the loop
// would otherwise surface as an infra failure.
//
// The call is bounded by Options.CallTimeout rather than the raw run
// ctx: without this, a slow or black-hole MCP server would stall a
// single tool call for as long as the whole run allows, instead of the
// configured per-call timeout.
func (t *mcpTool) Execute(ctx context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	start := time.Now()
	to := t.opts.withDefaults().CallTimeout
	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	text, _, err := t.caller.callTool(cctx, t.toolName, args)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		t.recordCall(args, mcpCallRecord{
			LatencyMs: latency,
			Error:     err.Error(),
		})
		return &agent.ToolResult{Output: "MCP error: " + err.Error()}, nil
	}

	original := len(text)
	truncated := false
	if original > t.opts.MaxResultBytes {
		text = truncateUTF8(text, t.opts.MaxResultBytes) + truncationMarker
		truncated = true
	}

	t.recordCall(args, mcpCallRecord{
		ResultBytes: original,
		Truncated:   truncated,
		LatencyMs:   latency,
	})
	return &agent.ToolResult{Output: text}, nil
}

// recordCall fills in the fields shared by every record call site
// (server, tool, args) and invokes t.record if the caller supplied one.
func (t *mcpTool) recordCall(args json.RawMessage, r mcpCallRecord) {
	if t.record == nil {
		return
	}
	r.Server = t.server
	r.Tool = t.toolName
	r.Args = args
	t.record(r)
}

// truncateUTF8 cuts s to at most max bytes without splitting a
// multibyte rune. It trims from the end of a byte-cap slice back to the
// last full rune boundary rather than decoding the whole string, since
// MCP results are typically small enough that this scan is cheap either
// way.
func truncateUTF8(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.RuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	// cut now ends either on a complete rune or (if the very start of a
	// multibyte rune was itself the byte we trimmed to) an incomplete
	// leading byte; utf8.RuneStart only confirms the byte *begins* a
	// rune, so verify the full rune actually fits before keeping it.
	if len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r == utf8.RuneError && size == 1 {
			cut = cut[:len(cut)-1]
		}
	}
	return cut
}
