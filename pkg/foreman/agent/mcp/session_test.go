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
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/defilantech/llmkube/pkg/foreman/agent/tools"
)

// TestConnect_SkipsUnreachableServer exercises the core skip-on-failure
// contract: one server dials and lists fine, the other never comes up
// (nothing listening on the port). Connect must not fail the whole call
// for the bad server -- it logs and moves on, returning only the good
// server's tools.
func TestConnect_SkipsUnreachableServer(t *testing.T) {
	goodURL := startFakeMCP(t)

	servers := []ServerConfig{
		{Name: "good", URL: goodURL},
		{Name: "bad", URL: "http://127.0.0.1:1/nope"},
	}

	toolList, closer, err := Connect(context.Background(), logr.Discard(), servers, Options{}, nil)
	if err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	if closer == nil {
		t.Fatalf("Connect returned nil closer")
	}
	defer func() {
		if cerr := closer(); cerr != nil {
			t.Fatalf("closer() = %v, want nil", cerr)
		}
	}()

	// The fake server exposes both "echo" and "boom" (see fake_server_test.go),
	// so the good server legitimately contributes more than one tool; what
	// matters here is that the good server's echo tool is present exactly
	// once and the unreachable "bad" server contributed nothing at all.
	var echoCount, badCount int
	for _, tl := range toolList {
		switch {
		case tl.Name() == "mcp/good/echo":
			echoCount++
		case strings.HasPrefix(tl.Name(), "mcp/bad/"):
			badCount++
		}
	}
	if echoCount != 1 {
		t.Fatalf("mcp/good/echo appeared %d times, want exactly 1: %+v", echoCount, names(toolList))
	}
	if badCount != 0 {
		t.Fatalf("bad server contributed %d tools, want 0: %+v", badCount, names(toolList))
	}
}

// names returns the Name() of every tool, for readable failure messages.
func names(toolList []tools.Tool) []string {
	out := make([]string, len(toolList))
	for i, tl := range toolList {
		out[i] = tl.Name()
	}
	return out
}

// TestConnect_AllowedToolsFilter verifies the discovered tool set is
// filtered through allowed(desc.Name, server.AllowedTools) before it
// reaches newTools, so a server that advertises "echo" but is only
// permitted "nope" contributes zero tools.
func TestConnect_AllowedToolsFilter(t *testing.T) {
	goodURL := startFakeMCP(t)

	servers := []ServerConfig{
		{Name: "good", URL: goodURL, AllowedTools: []string{"nope"}},
	}

	toolList, closer, err := Connect(context.Background(), logr.Discard(), servers, Options{}, nil)
	if err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	defer func() {
		if cerr := closer(); cerr != nil {
			t.Fatalf("closer() = %v, want nil", cerr)
		}
	}()

	if len(toolList) != 0 {
		t.Fatalf("Connect returned %d tools, want 0: %+v", len(toolList), toolList)
	}
}

// TestConnect_NilContextIsProgrammerError is the one case Connect treats
// as a fatal, returned error rather than a logged-and-skipped server
// failure.
func TestConnect_NilContextIsProgrammerError(t *testing.T) {
	//nolint:staticcheck // intentionally passing nil to exercise the guard
	toolList, closer, err := Connect(nil, logr.Discard(), nil, Options{}, nil)
	if err == nil {
		t.Fatalf("Connect with nil ctx: err = nil, want non-nil")
	}
	if toolList != nil {
		t.Fatalf("Connect with nil ctx: tools = %+v, want nil", toolList)
	}
	if closer != nil {
		t.Fatalf("Connect with nil ctx: closer = non-nil, want nil")
	}
}

// TestConnect_NoServersClosesCleanly guards the zero-sessions case: closer
// must be safe to call and return nil when nothing was ever dialed.
func TestConnect_NoServersClosesCleanly(t *testing.T) {
	toolList, closer, err := Connect(context.Background(), logr.Discard(), nil, Options{}, nil)
	if err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	if len(toolList) != 0 {
		t.Fatalf("Connect returned %d tools, want 0", len(toolList))
	}
	if closer == nil {
		t.Fatalf("Connect returned nil closer")
	}
	if cerr := closer(); cerr != nil {
		t.Fatalf("closer() = %v, want nil", cerr)
	}
}
