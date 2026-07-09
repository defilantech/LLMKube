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
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
)

// fakeCaller is a test double for the caller interface. It lets
// tool_test.go drive mcpTool.Execute without a real Session or network
// connection.
type fakeCaller struct {
	text    string
	isError bool
	err     error
}

func (f *fakeCaller) callTool(_ context.Context, _ string, _ json.RawMessage) (string, bool, error) {
	return f.text, f.isError, f.err
}

// TestNewTools_Wiring exercises the wiring newTools does per discovered
// tool: namespaced Name() and a Schema() built from the toolDesc. It does
// not call Execute -- newTools is typed to take a *Session, and a nil
// Session would panic on a real call, so Execute behavior is covered
// separately via the newTool helper and a fakeCaller.
func TestNewTools_Wiring(t *testing.T) {
	toolList := newTools(nil, ServerConfig{Name: "fake"}, []toolDesc{
		{Name: "echo", Description: "echoes back", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
	}, Options{}, nil)

	if len(toolList) != 1 {
		t.Fatalf("newTools returned %d tools, want 1", len(toolList))
	}
	tl := toolList[0]
	if got, want := tl.Name(), "mcp/fake/echo"; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}

	schema := tl.Schema()
	if schema.Name != "mcp/fake/echo" {
		t.Fatalf("Schema().Name = %q, want %q", schema.Name, "mcp/fake/echo")
	}
	if schema.Description != "echoes back" {
		t.Fatalf("Schema().Description = %q, want %q", schema.Description, "echoes back")
	}
	if string(schema.Parameters) != `{"type":"object","properties":{}}` {
		t.Fatalf("Schema().Parameters = %s, want passthrough of InputSchema", schema.Parameters)
	}
}

// TestNewTools_SchemaDefaultsWhenInputSchemaEmpty verifies the safe
// default so the model always sees a valid parameters schema even when
// a remote server advertises a tool with no input schema.
func TestNewTools_SchemaDefaultsWhenInputSchemaEmpty(t *testing.T) {
	toolList := newTools(nil, ServerConfig{Name: "fake"}, []toolDesc{
		{Name: "echo", Description: "echoes back"},
	}, Options{}, nil)

	schema := toolList[0].Schema()
	if string(schema.Parameters) != `{"type":"object"}` {
		t.Fatalf("Schema().Parameters = %s, want default {\"type\":\"object\"}", schema.Parameters)
	}
}

func TestMcpTool_Execute_ReturnsEchoedText(t *testing.T) {
	c := &fakeCaller{text: "hi"}
	mt := newTool(c, "fake", "echo", Options{}, nil)

	res, err := mt.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if res.Terminal {
		t.Fatalf("Execute result Terminal = true, want false")
	}
	if res.Output != "hi" {
		t.Fatalf("Execute Output = %v, want %q", res.Output, "hi")
	}
}

func TestMcpTool_Execute_Truncates(t *testing.T) {
	long := strings.Repeat("a", 100)
	c := &fakeCaller{text: long}

	var records []mcpCallRecord
	mt := newTool(c, "fake", "echo", Options{MaxResultBytes: 10}, func(r mcpCallRecord) {
		records = append(records, r)
	})

	res, err := mt.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	out, ok := res.Output.(string)
	if !ok {
		t.Fatalf("Output is %T, want string", res.Output)
	}
	if !strings.HasPrefix(out, strings.Repeat("a", 10)) {
		t.Fatalf("Output = %q, want it to start with 10 a's", out)
	}
	if !strings.HasSuffix(out, "\n…[truncated]") {
		t.Fatalf("Output = %q, want truncation marker suffix", out)
	}

	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	r := records[0]
	if !r.Truncated {
		t.Fatalf("record.Truncated = false, want true")
	}
	if r.ResultBytes != len(long) {
		t.Fatalf("record.ResultBytes = %d, want original length %d", r.ResultBytes, len(long))
	}
	if r.Server != "fake" || r.Tool != "echo" {
		t.Fatalf("record.Server/Tool = %q/%q, want fake/echo", r.Server, r.Tool)
	}
	if r.Error != "" {
		t.Fatalf("record.Error = %q, want empty", r.Error)
	}
}

// TestMcpTool_Execute_TruncatesOnRuneBoundary guards the multibyte-safe
// cut: MaxResultBytes lands mid-rune, so a naive byte slice would split a
// multibyte UTF-8 character and produce invalid UTF-8 in the tool output
// the model reads.
func TestMcpTool_Execute_TruncatesOnRuneBoundary(t *testing.T) {
	// Each "é" is 2 bytes (U+00E9). A cap of 11 lands mid-rune at byte 11
	// (5 full runes = 10 bytes, then 1 byte into the 6th).
	text := strings.Repeat("é", 20)
	c := &fakeCaller{text: text}
	mt := newTool(c, "fake", "echo", Options{MaxResultBytes: 11}, nil)

	res, err := mt.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	out := res.Output.(string)
	if !strings.HasSuffix(out, "\n…[truncated]") {
		t.Fatalf("Output = %q, want truncation marker suffix", out)
	}
	body := strings.TrimSuffix(out, "\n…[truncated]")
	if !strings.HasPrefix(text, body) {
		t.Fatalf("truncated body %q is not a prefix of the original text", body)
	}
	if len(body) > 11 {
		t.Fatalf("truncated body is %d bytes, want <= 11", len(body))
	}

	// Assert that the truncated body contains only valid UTF-8 sequences.
	if !utf8.ValidString(body) {
		t.Fatalf("truncated body contains invalid UTF-8: %q", body)
	}

	// The body should be the largest whole-rune prefix that fits in MaxResultBytes.
	// Each "é" is 2 bytes, so 11 / 2 = 5 whole runes.
	expectedBody := strings.Repeat("é", 5)
	if body != expectedBody {
		t.Fatalf("truncated body = %q, want %q (exact whole-rune prefix)", body, expectedBody)
	}
}

func TestMcpTool_Execute_NoTruncationUnderCap(t *testing.T) {
	c := &fakeCaller{text: "short"}
	var records []mcpCallRecord
	mt := newTool(c, "fake", "echo", Options{MaxResultBytes: 100}, func(r mcpCallRecord) {
		records = append(records, r)
	})

	res, err := mt.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if res.Output != "short" {
		t.Fatalf("Output = %v, want %q (no truncation marker)", res.Output, "short")
	}
	if records[0].Truncated {
		t.Fatalf("record.Truncated = true, want false")
	}
	if records[0].ResultBytes != len("short") {
		t.Fatalf("record.ResultBytes = %d, want %d", records[0].ResultBytes, len("short"))
	}
}

func TestMcpTool_Execute_SoftErrorOnCallToolFailure(t *testing.T) {
	c := &fakeCaller{err: errors.New("boom")}

	var records []mcpCallRecord
	mt := newTool(c, "fake", "echo", Options{}, func(r mcpCallRecord) {
		records = append(records, r)
	})

	res, err := mt.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute returned Go error %v, want nil (soft error)", err)
	}
	out, ok := res.Output.(string)
	if !ok || !strings.Contains(out, "MCP error: boom") {
		t.Fatalf("Output = %v, want it to contain 'MCP error: boom'", res.Output)
	}

	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].Error != "boom" {
		t.Fatalf("record.Error = %q, want %q", records[0].Error, "boom")
	}
	if records[0].Server != "fake" || records[0].Tool != "echo" {
		t.Fatalf("record.Server/Tool = %q/%q, want fake/echo", records[0].Server, records[0].Tool)
	}
}

// blockingCaller is a caller double that never returns on its own: it
// blocks until the ctx Execute hands it is cancelled, then reports the
// ctx error, mimicking a slow/black-hole MCP server. It exists to prove
// Execute bounds callTool with Options.CallTimeout rather than the raw
// run ctx (which in production may stay live for the whole agent run).
type blockingCaller struct{}

func (blockingCaller) callTool(ctx context.Context, _ string, _ json.RawMessage) (string, bool, error) {
	<-ctx.Done()
	return "", false, ctx.Err()
}

// TestMcpTool_Execute_CallTimeout guards the finding that
// Options.CallTimeout (defaulted to 30s, and the CRD's documented safety
// knob) was threaded onto every mcpTool but never applied: Execute called
// t.caller.callTool with the raw run ctx, so a stalled MCP server could
// block a tool call for as long as the whole run allowed. With a 20ms
// CallTimeout and a caller that only unblocks when its ctx is cancelled,
// Execute must return well before a real hang would ever be noticed, and
// must still fold the timeout into the soft-error path like any other
// callTool failure (nil Go error, "MCP error: ..." Output, recorded
// mcpCallRecord.Error).
func TestMcpTool_Execute_CallTimeout(t *testing.T) {
	var records []mcpCallRecord
	mt := newTool(blockingCaller{}, "fake", "echo", Options{CallTimeout: 20 * time.Millisecond}, func(r mcpCallRecord) {
		records = append(records, r)
	})

	type execOutcome struct {
		res *agent.ToolResult
		err error
	}
	done := make(chan execOutcome, 1)
	go func() {
		res, err := mt.Execute(context.Background(), json.RawMessage(`{}`))
		done <- execOutcome{res, err}
	}()

	var outcome execOutcome
	select {
	case outcome = <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Execute did not return within 500ms of a 20ms CallTimeout; timeout is not being applied to callTool")
	}

	if outcome.err != nil {
		t.Fatalf("Execute returned Go error %v, want nil (timeout is a soft error)", outcome.err)
	}
	out, ok := outcome.res.Output.(string)
	if !ok || !strings.Contains(out, "MCP error") {
		t.Fatalf("Output = %v, want it to contain %q", outcome.res.Output, "MCP error")
	}

	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].Error == "" {
		t.Fatalf("record.Error = %q, want non-empty (timeout should be recorded)", records[0].Error)
	}
}

// TestMcpTool_Execute_ToolLevelErrorSetsIsErrorAndPrefixesOutput pins the
// fix for the discarded isError flag: before the fix, callTool's isError
// bool was thrown away (`text, _, err := ...`), so a tool-level failure
// (isError=true, nil Go error -- e.g. the remote tool ran but reported its
// own failure) recorded as an ordinary success with no way for the model
// or the transcript reader to tell it apart from a good result. Execute
// must now (a) set mcpCallRecord.IsError, and (b) prefix the model-visible
// Output with the toolErrorMarker so the model can see the call failed.
func TestMcpTool_Execute_ToolLevelErrorSetsIsErrorAndPrefixesOutput(t *testing.T) {
	c := &fakeCaller{text: "no docs found for that symbol", isError: true}

	var records []mcpCallRecord
	mt := newTool(c, "fake", "get-docs", Options{}, func(r mcpCallRecord) {
		records = append(records, r)
	})

	res, err := mt.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute returned Go error %v, want nil (tool-level error is a soft error)", err)
	}
	out, ok := res.Output.(string)
	if !ok {
		t.Fatalf("Output is %T, want string", res.Output)
	}
	if !strings.HasPrefix(out, "[mcp tool error] ") {
		t.Fatalf("Output = %q, want it prefixed with the tool-error marker", out)
	}
	if !strings.Contains(out, "no docs found for that symbol") {
		t.Fatalf("Output = %q, want it to still contain the original tool text", out)
	}

	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if !records[0].IsError {
		t.Fatalf("record.IsError = false, want true")
	}
	if records[0].Error != "" {
		t.Fatalf("record.Error = %q, want empty (isError is not a Go/transport error)", records[0].Error)
	}
}

// TestMcpTool_Execute_SuccessLeavesIsErrorFalse is the success-path
// counterpart: IsError must default to false and the Output must be
// unprefixed when the remote tool reports success.
func TestMcpTool_Execute_SuccessLeavesIsErrorFalse(t *testing.T) {
	c := &fakeCaller{text: "hi", isError: false}

	var records []mcpCallRecord
	mt := newTool(c, "fake", "echo", Options{}, func(r mcpCallRecord) {
		records = append(records, r)
	})

	res, err := mt.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if res.Output != "hi" {
		t.Fatalf("Output = %v, want unprefixed %q", res.Output, "hi")
	}

	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].IsError {
		t.Fatalf("record.IsError = true, want false")
	}
}

func TestMcpTool_Execute_NilRecordIsNoop(t *testing.T) {
	mt := newTool(&fakeCaller{text: "ok"}, "fake", "echo", Options{}, nil)

	if _, err := mt.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
}

// TestTruncateUTF8_RuneBoundaries covers the case Jory flagged on #1014: when
// the byte cap lands exactly on the last byte of a complete multibyte rune,
// that rune fits and must be kept, not walked back past. The earlier
// implementation dropped it (and then a second byte via DecodeLastRune),
// silently losing data whenever an MCP result truncated on a rune boundary.
func TestTruncateUTF8_RuneBoundaries(t *testing.T) {
	tests := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"cap on 2-byte boundary keeps both runes", "ééé", 4, "éé"},
		{"cap on 2-byte boundary keeps one rune", "ééé", 2, "é"},
		{"cap on 4-byte boundary keeps the rune", "𐍈𐍈", 4, "𐍈"},
		{"cap splits a 2-byte rune drops the partial", "ééé", 3, "é"},
		{"cap splits a 4-byte rune drops the partial", "é𐍈", 3, "é"},
		{"cap smaller than first rune yields empty", "é", 1, ""},
		{"ascii cut mid-string", "hello", 3, "hel"},
		{"max >= len returns whole string", "éé", 99, "éé"},
		{"max <= 0 returns whole string", "éé", 0, "éé"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateUTF8(tc.s, tc.max)
			if got != tc.want {
				t.Fatalf("truncateUTF8(%q, %d) = %q (%d bytes), want %q (%d bytes)",
					tc.s, tc.max, got, len(got), tc.want, len(tc.want))
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncateUTF8(%q, %d) = %q is not valid UTF-8", tc.s, tc.max, got)
			}
			if len(got) > tc.max && tc.max > 0 {
				t.Fatalf("truncateUTF8(%q, %d) = %q exceeds cap (%d > %d)",
					tc.s, tc.max, got, len(got), tc.max)
			}
		})
	}
}
