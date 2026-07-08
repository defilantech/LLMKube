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

func TestMcpTool_Execute_NilRecordIsNoop(t *testing.T) {
	mt := newTool(&fakeCaller{text: "ok"}, "fake", "echo", Options{}, nil)

	if _, err := mt.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
}
