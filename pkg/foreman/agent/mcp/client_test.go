package mcp

import (
	"context"
	"encoding/json"
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
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("listTools = %v, %v", tools, err)
	}
	text, isErr, err := s.callTool(context.Background(), "echo", json.RawMessage(`{"msg":"hi"}`))
	if err != nil || isErr || text != "hi" {
		t.Fatalf("callTool = %q, %v, %v", text, isErr, err)
	}
}
