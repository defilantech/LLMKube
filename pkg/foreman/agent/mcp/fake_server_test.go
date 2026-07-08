package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// echoArgs is the input schema for the fake server's "echo" tool.
type echoArgs struct {
	Msg string `json:"msg"`
}

// startFakeMCP starts an in-process MCP server, speaking the real
// streamable-HTTP protocol, with an "echo" tool that returns its "msg"
// argument as text content, and a "boom" tool that always returns a
// tool-level error result. It returns the server's base URL and registers
// cleanup via t.Cleanup.
func startFakeMCP(t *testing.T) string {
	t.Helper()

	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake-mcp", Version: "0.0.1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "echo",
		Description: "echoes the msg argument back to the caller",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, args echoArgs) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: args.Msg}},
		}, nil, nil
	})
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "boom",
		Description: "always returns a tool-level error result",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{
			IsError: true,
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "kaboom"}},
		}, nil, nil
	})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return server
	}, nil)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer.URL
}
