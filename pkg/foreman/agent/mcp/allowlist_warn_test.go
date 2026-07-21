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

	"github.com/go-logr/logr/funcr"
)

// TestUnmatchedAllowlistEntries pins the drift-detection helper: an explicit
// allowlist entry that matches no discovered tool is reported, so a stale or
// mistyped tool name (the context7 get-library-docs vs query-docs bug, #1183)
// is surfaced instead of silently dropping the tool.
func TestUnmatchedAllowlistEntries(t *testing.T) {
	tests := []struct {
		name       string
		allow      []string
		discovered []string
		want       []string
	}{
		{"all match", []string{"echo", "boom"}, []string{"echo", "boom"}, nil},
		{"one stale", []string{"echo", "ghost"}, []string{"echo", "boom"}, []string{"ghost"}},
		{"two stale", []string{"a", "b"}, []string{"echo"}, []string{"a", "b"}},
		{"empty allowlist means allow-all", nil, []string{"echo"}, nil},
		{"wildcard means allow-all", []string{"*"}, []string{"echo"}, nil},
		{"wildcard suppresses unmatched", []string{"*", "ghost"}, []string{"echo"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := unmatchedAllowlistEntries(tc.allow, tc.discovered)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("unmatchedAllowlistEntries(%v, %v) = %v, want %v", tc.allow, tc.discovered, got, tc.want)
			}
		})
	}
}

// TestConnect_WarnsOnUnmatchedAllowlist pins the end-to-end behavior: when a
// server's allowlist names a tool the server does not expose, Connect logs a
// warning naming the unmatched entry, while still registering the tools that
// do match.
func TestConnect_WarnsOnUnmatchedAllowlist(t *testing.T) {
	base := startFakeMCP(t) // exposes "echo" and "boom"
	var logs []string
	log := funcr.New(func(prefix, args string) { logs = append(logs, args) }, funcr.Options{})

	servers := []ServerConfig{{Name: "fake", URL: base, AllowedTools: []string{"echo", "ghost"}}}
	toolz, closer, err := Connect(context.Background(), log, servers, Options{}, func(mcpCallRecord) {})
	if closer != nil {
		defer func() { _ = closer() }()
	}
	if err != nil {
		t.Fatalf("Connect err = %v", err)
	}
	// "echo" still registers; "ghost" does not exist.
	if len(toolz) != 1 || toolz[0].Name() != "mcp/fake/echo" {
		names := make([]string, 0, len(toolz))
		for _, tl := range toolz {
			names = append(names, tl.Name())
		}
		t.Fatalf("want only mcp/fake/echo registered, got %v", names)
	}
	// A warning must name the unmatched entry "ghost".
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "ghost") || !strings.Contains(strings.ToLower(joined), "allowlist") {
		t.Errorf("expected a warning naming the unmatched allowlist entry 'ghost'; logs:\n%s", joined)
	}
}
