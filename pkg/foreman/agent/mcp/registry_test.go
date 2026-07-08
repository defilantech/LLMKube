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
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
	"github.com/defilantech/llmkube/pkg/foreman/agent/tools"
)

// fakeNativeTool is a minimal tools.Tool double standing in for one of
// the native tools makeRegistryFactory builds (read_file, bash, etc.).
// Register must never disturb these -- MCP tools are appended after.
type fakeNativeTool struct{ name string }

func (f *fakeNativeTool) Name() string { return f.name }

func (f *fakeNativeTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name:        f.name,
		Description: "fake native tool",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}
}

func (f *fakeNativeTool) Execute(_ context.Context, _ json.RawMessage) (*agent.ToolResult, error) {
	return &agent.ToolResult{Output: "native"}, nil
}

func toolNames(all []tools.Tool) map[string]bool {
	names := make(map[string]bool, len(all))
	for _, t := range all {
		names[t.Name()] = true
	}
	return names
}

// TestRegister_AppendsMCPTools verifies the happy path: a live fake MCP
// server's tools are discovered and appended after the base (native)
// tool set, namespaced as mcp/<server>/<tool>, and the returned closer
// tears the session down without error.
func TestRegister_AppendsMCPTools(t *testing.T) {
	srv := startFakeMCP(t)
	base := []tools.Tool{&fakeNativeTool{name: "native_tool"}}
	servers := []ServerConfig{{Name: "fake", URL: srv}}

	all, closer := Register(context.Background(), logr.Discard(), base, servers, Options{}, true)

	names := toolNames(all)
	if !names["native_tool"] {
		t.Fatalf("Register result %v missing base tool native_tool", names)
	}
	if !names["mcp/fake/echo"] {
		t.Fatalf("Register result %v missing discovered mcp/fake/echo", names)
	}
	if !names["mcp/fake/boom"] {
		t.Fatalf("Register result %v missing discovered mcp/fake/boom", names)
	}
	if len(all) != 3 {
		t.Fatalf("Register result has %d tools, want 3 (1 base + 2 mcp)", len(all))
	}

	if err := closer(); err != nil {
		t.Fatalf("closer() = %v, want nil", err)
	}
}

// TestRegister_DisabledReturnsBase covers both ways Register short-
// circuits to a no-op: enabled=false, and enabled=true with no servers
// configured. Either way the result must be exactly base (no copy
// surprises) and the closer must be a safe no-op.
func TestRegister_DisabledReturnsBase(t *testing.T) {
	base := []tools.Tool{&fakeNativeTool{name: "native_tool"}}

	t.Run("enabled=false", func(t *testing.T) {
		servers := []ServerConfig{{Name: "fake", URL: "http://127.0.0.1:1/unreachable"}}
		all, closer := Register(context.Background(), logr.Discard(), base, servers, Options{}, false)

		if len(all) != 1 || all[0].Name() != "native_tool" {
			t.Fatalf("Register result = %v, want exactly base", toolNames(all))
		}
		if err := closer(); err != nil {
			t.Fatalf("closer() = %v, want nil no-op", err)
		}
	})

	t.Run("empty servers", func(t *testing.T) {
		all, closer := Register(context.Background(), logr.Discard(), base, nil, Options{}, true)

		if len(all) != 1 || all[0].Name() != "native_tool" {
			t.Fatalf("Register result = %v, want exactly base", toolNames(all))
		}
		if err := closer(); err != nil {
			t.Fatalf("closer() = %v, want nil no-op", err)
		}
	})
}

// TestBuildServers_ResolvesAndSkips exercises the CRD -> ServerConfig
// mapping: a server whose header secret resolves is kept with the
// resolved value; a server whose header secret fails to resolve is
// dropped entirely rather than failing the whole build. Options must
// carry the configured CallTimeout/MaxResultBytes through untouched.
func TestBuildServers_ResolvesAndSkips(t *testing.T) {
	cfg := &v1alpha1.MCPConfig{
		Enabled:        true,
		CallTimeout:    metav1.Duration{Duration: 5 * time.Second},
		MaxResultBytes: 4096,
		Servers: []v1alpha1.MCPServer{
			{
				Name:      "good",
				Transport: "http",
				URL:       "http://good.example",
				Headers: []v1alpha1.MCPHeader{
					{
						Name: "Authorization",
						ValueFrom: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "good-secret"},
							Key:                  "token",
						},
					},
				},
				AllowedTools: []string{"echo"},
			},
			{
				Name:      "bad",
				Transport: "http",
				URL:       "http://bad.example",
				Headers: []v1alpha1.MCPHeader{
					{
						Name: "Authorization",
						ValueFrom: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "bad-secret"},
							Key:                  "token",
						},
					},
				},
			},
		},
	}

	resolve := func(ref *corev1.SecretKeySelector) (string, error) {
		if ref.Name == "good-secret" {
			return "resolved-token", nil
		}
		return "", fmt.Errorf("secret %s not found", ref.Name)
	}

	servers, opts := BuildServers(cfg, resolve, logr.Discard())

	if opts.CallTimeout != 5*time.Second {
		t.Fatalf("opts.CallTimeout = %v, want 5s", opts.CallTimeout)
	}
	if opts.MaxResultBytes != 4096 {
		t.Fatalf("opts.MaxResultBytes = %d, want 4096", opts.MaxResultBytes)
	}

	if len(servers) != 1 {
		t.Fatalf("servers = %+v, want exactly 1 (bad server skipped)", servers)
	}
	got := servers[0]
	if got.Name != "good" {
		t.Fatalf("servers[0].Name = %q, want good", got.Name)
	}
	if got.URL != "http://good.example" {
		t.Fatalf("servers[0].URL = %q, want http://good.example", got.URL)
	}
	if got.Headers["Authorization"] != "resolved-token" {
		t.Fatalf("servers[0].Headers[Authorization] = %q, want resolved-token", got.Headers["Authorization"])
	}
	if len(got.AllowedTools) != 1 || got.AllowedTools[0] != "echo" {
		t.Fatalf("servers[0].AllowedTools = %v, want [echo]", got.AllowedTools)
	}
}
