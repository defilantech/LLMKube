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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/defilantech/llmkube/pkg/foreman/agent/repomap"
)

// ripwireBackendEnv selects the coder repo-map backend. Unset (or anything
// other than "ripwire") keeps repomap, so default behavior is unchanged. A
// missing or failing ripwire binary falls through to repomap and never fails
// the task.
const ripwireBackendEnv = "FOREMAN_REPOMAP_BACKEND" //nolint:gosec // env var name, not a credential

// ripwireBinEnv overrides the ripwire binary path; unset resolves "ripwire"
// from PATH.
const ripwireBinEnv = "FOREMAN_RIPWIRE_BIN" //nolint:gosec // env var name, not a credential

// ripwireTimeout bounds one ripwire invocation. A coder that cannot get a
// repo-map this turn is better served by repomap than by a hung prefix build.
const ripwireTimeout = 60 * time.Second

// ripwireBackendEnabled reports whether the ripwire repo-map backend was
// requested. Off by default: a bare "ripwire" is the opt-in.
func ripwireBackendEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(ripwireBackendEnv)), "ripwire")
}

// RipwireBin resolves the ripwire binary: the explicit FOREMAN_RIPWIRE_BIN
// override, else "ripwire" resolved from PATH. Exported so the agent tool
// resolves the same binary the advisory backend uses.
func RipwireBin() string {
	if b := strings.TrimSpace(os.Getenv(ripwireBinEnv)); b != "" {
		return b
	}
	return "ripwire"
}

// repoMapSummary returns the coder's repo-map prefix, selecting the backend.
// It is advisory-only and must never be wired into scopeRelevantFiles
// (coder_gate.go), which feeds the GO/NO-GO scope-overlap demotion.
//
// When the ripwire backend is enabled and succeeds, its summary is returned.
// On any ripwire failure (missing binary, non-zero exit, unparseable
// output) the function logs and falls through to repomap, exactly as a
// repomap build failure today leaves the prompt unchanged. An empty string
// means no summary; the caller prepends nothing.
func repoMapSummary(ctx context.Context, workspace, issueText string, log logr.Logger) string {
	if ripwireBackendEnabled() {
		summary, err := ripwireSummary(ctx, workspace, issueText, repomap.DefaultTokenBudget)
		switch {
		case err != nil:
			log.Info("ripwire repo-map failed; falling back to repomap", "err", err.Error())
		case summary != "":
			return summary
		}
	}
	summary, err := repomap.Build(ctx, workspace, issueText, repomap.Options{})
	switch {
	case err != nil:
		log.Info("repomap build failed; continuing without summary", "err", err.Error())
	case summary != "":
		return summary
	}
	return ""
}

// ripwirePayload is the subset of ripwire's --json bundle the renderer reads.
// Field names mirror ripwire's wire keys one to one.
type ripwirePayload struct {
	Sigs []struct {
		Path string `json:"p"`
		Name string `json:"n"`
		Sig  string `json:"sig"`
		Doc  string `json:"doc"`
	} `json:"sigs"`
}

// ripwireSummary runs ripwire's task lens over workspace and renders its
// ranked symbols into repomap's file-level markdown shape, so the coder
// prompt prefix stays one contract. tokenBudget caps the run.
func ripwireSummary(ctx context.Context, workspace, issueText string, tokenBudget int) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		return "", nil
	}
	args := []string{workspace, "--for=" + issueText, "--json"}
	if tokenBudget > 0 {
		args = append(args, fmt.Sprintf("--token-budget=%d", tokenBudget))
	}

	cctx, cancel := context.WithTimeout(ctx, ripwireTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, RipwireBin(), args...).Output()
	if err != nil {
		return "", fmt.Errorf("ripwire %q: %w", RipwireBin(), err)
	}
	return renderRipwire(out)
}

// renderRipwire turns ripwire's JSON bundle into the markdown prefix shape
// the coder already reads from repomap: a "## Repository overview" header
// then one "### path" section per file, signatures as bullets in rank order.
func renderRipwire(raw []byte) (string, error) {
	var payload ripwirePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("parse ripwire json: %w", err)
	}
	if len(payload.Sigs) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("## Repository overview\n\n")
	b.WriteString("Ranked by the ripwire call graph. Re-read any file with the " +
		"`read_file` tool when you need more context than this summary provides.\n\n")

	var current string
	for _, s := range payload.Sigs {
		if s.Path != current {
			b.WriteString("### ")
			b.WriteString(s.Path)
			b.WriteString("\n")
			current = s.Path
		}
		b.WriteString("- ")
		if s.Sig != "" {
			b.WriteString(s.Sig)
		} else {
			b.WriteString(s.Name)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}
