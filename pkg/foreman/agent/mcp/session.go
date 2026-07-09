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
	"errors"
	"fmt"

	"github.com/go-logr/logr"

	"github.com/defilantech/llmkube/pkg/foreman/agent/tools"
)

// Connect is the single entry point the wiring layer calls to turn a list
// of MCP server configs into ready-to-register Foreman tools.
//
// MCP must never fail a Foreman run: a server that fails to dial or list
// its tools is logged at warning and skipped, contributing zero tools,
// and Connect keeps going to the next server. The returned error is
// non-nil only for a programmer error (a nil ctx); a server-level failure
// is never surfaced as err.
func Connect(
	ctx context.Context, log logr.Logger, servers []ServerConfig, opts Options, record func(mcpCallRecord),
) (aggregated []tools.Tool, closer func() error, err error) {
	if ctx == nil {
		return nil, nil, fmt.Errorf("mcp: Connect: nil ctx")
	}

	sessions := make([]*Session, 0, len(servers))
	closer = func() error {
		var errs []error
		for _, s := range sessions {
			if cerr := s.Close(); cerr != nil {
				errs = append(errs, cerr)
			}
		}
		return errors.Join(errs...)
	}

	for _, server := range servers {
		s, dialErr := dial(ctx, server)
		if dialErr != nil {
			log.Info("mcp: dial failed; skipping server", "server", server.Name, "err", dialErr.Error())
			continue
		}

		discovered, listErr := s.listTools(ctx)
		if listErr != nil {
			log.Info("mcp: list tools failed; skipping server", "server", server.Name, "err", listErr.Error())
			if cerr := s.Close(); cerr != nil {
				log.Info("mcp: close failed after list error", "server", server.Name, "err", cerr.Error())
			}
			continue
		}

		sessions = append(sessions, s)

		filtered := make([]toolDesc, 0, len(discovered))
		for _, d := range discovered {
			if allowed(d.Name, server.AllowedTools) {
				filtered = append(filtered, d)
			}
		}

		aggregated = append(aggregated, newTools(s, server, filtered, opts, record)...)
	}

	return aggregated, closer, nil
}
