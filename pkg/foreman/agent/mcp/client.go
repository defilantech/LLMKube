// Package mcp provides Foreman's agents with a thin wrapper over the
// official Model Context Protocol Go SDK, giving live documentation
// lookups via remote MCP servers.
//
// client.go is the ONLY file in this codebase that imports the MCP SDK
// (github.com/modelcontextprotocol/go-sdk/mcp). Everything else in this
// package, and everything built on top of it, depends only on the
// exported surface here: dial, Session.listTools, Session.callTool, and
// Session.Close.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// dialTimeout bounds the initial MCP handshake (transport connect plus
// the initialize/initialized exchange) so a server that accepts a TCP
// connection but never completes the handshake cannot hang registry
// build indefinitely. It is deliberately not derived from
// Options.CallTimeout and not applied anywhere past Connect: the go-sdk
// (github.com/modelcontextprotocol/go-sdk@v1.6.1/mcp) hands the
// connect-time ctx only to the transport connect and the initialize
// calls, and internally wraps it in a notDone{} context (see
// internal/jsonrpc2/conn.go's NewConnection) specifically so the
// connection's background read loop is immune to that ctx's
// cancellation. Every later ClientSession.ListTools/CallTool call takes
// its own fresh ctx argument (see Session.listTools/callTool below), so
// bounding only the handshake here cannot cut short a later long-running
// tool call.
const dialTimeout = 15 * time.Second

// Session is a live connection to a single remote MCP server.
type Session struct {
	cs   *sdkmcp.ClientSession
	name string
}

// toolDesc describes a tool discovered on a remote MCP server.
type toolDesc struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// headerRoundTripper injects a fixed set of headers into every outgoing
// request before delegating to base. It exists so ServerConfig.Headers
// (populated with secret-injected auth headers by later callers) reaches
// the wire; the SDK's StreamableClientTransport has no first-class
// per-request header option, only HTTPClient.
type headerRoundTripper struct {
	headers map[string]string
	base    http.RoundTripper
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}
	if len(h.headers) == 0 {
		return base.RoundTrip(req)
	}
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return base.RoundTrip(req)
}

// dial connects to the MCP server described by cfg over the streamable
// HTTP transport and returns a live Session. It never panics: all
// failures are returned as errors.
func dial(ctx context.Context, cfg ServerConfig) (*Session, error) {
	httpClient := &http.Client{}
	if len(cfg.Headers) > 0 {
		httpClient.Transport = &headerRoundTripper{headers: cfg.Headers}
	}

	transport := &sdkmcp.StreamableClientTransport{
		Endpoint:   cfg.URL,
		HTTPClient: httpClient,
	}

	client := sdkmcp.NewClient(&sdkmcp.Implementation{
		Name:    "llmkube-foreman",
		Version: "0.1.0",
	}, nil)

	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	cs, err := client.Connect(dctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: dial %q (%s): %w", cfg.Name, cfg.URL, err)
	}
	return &Session{cs: cs, name: cfg.Name}, nil
}

// listTools returns the tools the remote server currently advertises.
//
// It does not paginate: it returns whatever the server hands back on the
// first page. Pagination support can be added if a real server needs it.
func (s *Session) listTools(ctx context.Context) ([]toolDesc, error) {
	res, err := s.cs.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: list tools on %q: %w", s.name, err)
	}
	if res == nil {
		return nil, nil
	}

	tools := make([]toolDesc, 0, len(res.Tools))
	for _, t := range res.Tools {
		if t == nil {
			continue
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("mcp: marshal input schema for tool %q on %q: %w", t.Name, s.name, err)
		}
		tools = append(tools, toolDesc{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return tools, nil
}

// callTool invokes the named tool with the given JSON arguments and
// returns the concatenated text content of the result along with the
// server's isError flag.
func (s *Session) callTool(ctx context.Context, name string, args json.RawMessage) (
	text string, isError bool, err error,
) {
	// Pass args through as raw JSON rather than unmarshaling into an `any`:
	// encoding/json decodes all JSON numbers as float64, which silently
	// loses precision on integers outside float64's 53-bit safe range.
	// json.RawMessage implements MarshalJSON, so the SDK marshals it back
	// out byte-for-byte.
	params := &sdkmcp.CallToolParams{Name: name}
	if len(args) > 0 {
		params.Arguments = args
	}

	res, err := s.cs.CallTool(ctx, params)
	if err != nil {
		return "", false, fmt.Errorf("mcp: call tool %q on %q: %w", name, s.name, err)
	}
	if res == nil {
		return "", false, nil
	}

	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError, nil
}

// Close terminates the session and its underlying transport connection.
func (s *Session) Close() error {
	if s == nil || s.cs == nil {
		return nil
	}
	return s.cs.Close()
}
