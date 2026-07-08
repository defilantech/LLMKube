package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSession_ListAndCall(t *testing.T) {
	srv := startFakeMCP(t) // from fake_server_test.go; returns URL
	s, err := dial(context.Background(), ServerConfig{Name: "fake", URL: srv})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = s.Close() }()

	tools, err := s.listTools(context.Background())
	if err != nil || len(tools) != 2 {
		t.Fatalf("listTools = %v, %v", tools, err)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}
	if !names["echo"] || !names["boom"] {
		t.Fatalf("listTools names = %v, want echo and boom", names)
	}
	text, isErr, err := s.callTool(context.Background(), "echo", json.RawMessage(`{"msg":"hi"}`))
	if err != nil || isErr || text != "hi" {
		t.Fatalf("callTool = %q, %v, %v", text, isErr, err)
	}
}

// TestSession_CallTool_IsError verifies that a tool-level error result from
// the remote server surfaces as isError=true with a nil Go error, not as a
// Go error. Later callers rely on this distinction to tell "the tool ran
// and reported failure" apart from "the call itself failed" (transport,
// protocol, or unknown-tool errors).
func TestSession_CallTool_IsError(t *testing.T) {
	srv := startFakeMCP(t)
	s, err := dial(context.Background(), ServerConfig{Name: "fake", URL: srv})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = s.Close() }()

	text, isErr, err := s.callTool(context.Background(), "boom", nil)
	if err != nil {
		t.Fatalf("callTool returned Go error: %v", err)
	}
	if !isErr {
		t.Fatalf("callTool isError = false, want true")
	}
	if !strings.Contains(text, "kaboom") {
		t.Fatalf("callTool text = %q, want it to contain %q", text, "kaboom")
	}
}
