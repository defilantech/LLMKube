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

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/tools"
)

// Register appends MCP tools (discovered from servers) to base and returns
// the combined tool set plus a closer for the opened MCP sessions. When
// enabled is false or servers is empty, it returns base unchanged and a
// no-op closer. The per-call record hook is built internally from log
// (V(1) debug visibility); the authoritative audit is the run transcript,
// where every MCP call already appears as a normal mcp/<server>/<tool> call.
func Register(
	ctx context.Context, log logr.Logger, base []tools.Tool, servers []ServerConfig, opts Options, enabled bool,
) (all []tools.Tool, closer func() error) {
	if !enabled || len(servers) == 0 {
		return base, func() error { return nil }
	}

	record := func(r mcpCallRecord) {
		log.V(1).Info("mcp tool call", "server", r.Server, "tool", r.Tool,
			"resultBytes", r.ResultBytes, "truncated", r.Truncated,
			"latencyMs", r.LatencyMs, "callError", r.Error, "isError", r.IsError)
	}

	mcpTools, c, _ := Connect(ctx, log, servers, opts, record) // Connect never returns a server-failure error
	if c == nil {
		// Connect only returns a nil closer alongside a non-nil err (a
		// programmer error, e.g. a nil ctx); guard here too so a caller
		// that (like Connect's own doc warns) ignores that error can
		// still call closer() unconditionally without a nil-pointer
		// panic.
		c = func() error { return nil }
	}
	return append(append([]tools.Tool{}, base...), mcpTools...), c
}

// BuildServers maps a CRD MCPConfig into ServerConfigs + Options, resolving
// each header's secret via resolve. A server whose header resolution fails
// is logged and SKIPPED (best-effort; MCP never fails the registry build).
func BuildServers(
	cfg *v1alpha1.MCPConfig, resolve func(ref *corev1.SecretKeySelector) (string, error), log logr.Logger,
) (servers []ServerConfig, opts Options) {
	if cfg == nil {
		return nil, Options{}
	}

	opts = Options{
		CallTimeout:    cfg.CallTimeout.Duration,
		MaxResultBytes: cfg.MaxResultBytes,
	}

	for _, s := range cfg.Servers {
		headers := make(map[string]string, len(s.Headers))
		skip := false
		for _, h := range s.Headers {
			if h.ValueFrom == nil {
				continue
			}
			v, err := resolve(h.ValueFrom)
			if err != nil {
				log.Info("mcp: header secret resolution failed; skipping server",
					"server", s.Name, "header", h.Name, "err", err.Error())
				skip = true
				break
			}
			headers[h.Name] = v
		}
		if skip {
			continue
		}

		servers = append(servers, ServerConfig{
			Name:         s.Name,
			URL:          s.URL,
			Headers:      headers,
			AllowedTools: s.AllowedTools,
		})
	}

	return servers, opts
}
