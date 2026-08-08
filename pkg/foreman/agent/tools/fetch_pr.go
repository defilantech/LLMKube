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

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubprfetch"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// FetchPullRequestTool is the coder's read-only path to a GitHub pull
// request. It wraps the same githubprfetch.Client the executor uses and
// exposes it as a first-class tool so the coder can read the PR it is
// asked to fix — including reviews, review comments, and check runs —
// without needing a separately-authed `gh` CLI on its FleetNode.
//
// Why this exists (issue #1434): the v0.4 PR-fix path mandated that the
// coder reason from a pre-chewed digest assembled by the orchestrator.
// The coder could not see other reviews, inline review comments with
// file/line anchors, or check-run conclusions beyond a pre-trimmed log
// excerpt. The fix is to stop passing a digest and instead expose one
// bounded GitHub surface (this tool) using the same token the
// foreman-agent already loads at startup.
//
// Security shape vs the GH_TOKEN-env alternative:
//
//   - Read-only by construction: exactly one GET per Execute (PR details,
//     reviews, review comments, check runs). The model has no path to
//     gh pr create / gh repo delete / generic authenticated REST writes
//     through this tool.
//   - The token lives behind a TokenSource closure; the struct stores
//     no credentials between calls.
//   - Containerised foreman-agents do not need `gh` installed in their
//     image; pure Go HTTP suffices.
//   - Token rotation surface is one file (~/.config/foreman/github-token)
//     instead of also a per-user `gh` config or per-process env var
//     inherited by every bash subprocess.
type FetchPullRequestTool struct {
	// Fetcher is the GitHub PR client. Required. Production wires
	// githubprfetch.NewClient(); tests pass a fake or an httptest-backed
	// Client.
	Fetcher githubprfetch.Fetcher
	// Token resolves the GitHub PAT at Execute time. Required.
	Token TokenSource
}

type fetchPullRequestArgs struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// Name returns the tool name as advertised to the model.
func (t *FetchPullRequestTool) Name() string { return "fetch_pull_request" }

// Schema returns the OAI schema advertisement.
func (t *FetchPullRequestTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name: "fetch_pull_request",
		Description: "Read a GitHub pull request's title, body, state, merged, " +
			"head_ref, base_ref, reviews, review_comments, and check_runs. " +
			"Use this as the scope anchor before fixing a PR: a verbatim " +
			"sentence from a review comment is the right citation for " +
			"whether the diff addresses what was asked. Returns title, body " +
			"(truncated to ~16 KiB if needed), state, merged, head_ref, " +
			"base_ref, reviews, review_comments, check_runs.",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "repo":   {"type": "string", "description": "owner/name, e.g. \"defilantech/LLMKube\"."},
  "number": {"type": "integer", "minimum": 1, "description": "PR number."}
},
"required": ["repo", "number"]
}`),
	}
}

// Execute reads the GitHub pull request at args.repo / args.number. All
// failure modes are surfaced via the returned error, which the loop
// emits as the tool-result content the model sees on its next turn.
//
//   - bad args (missing repo, non-positive number, malformed repo
//     string): wrapped fmt.Errorf naming the field.
//   - token resolution failure: wrapped error citing the env var /
//     file the operator is expected to populate; this points at the
//     foreman-agent host, not the model.
//   - 404: "PR not found" so the model can choose to ERROR out rather
//     than guess.
//   - 401 / 403: "unauthorized" with a hint that the token is wrong
//     or out of scope.
//   - 5xx / network: generic transient failure; the model can retry
//     once or call submit_result with verdict=ERROR.
func (t *FetchPullRequestTool) Execute(ctx context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	if t.Fetcher == nil {
		return nil, fmt.Errorf("fetch_pull_request: tool not configured (Fetcher is nil)")
	}
	if t.Token == nil {
		return nil, fmt.Errorf("fetch_pull_request: tool not configured (Token resolver is nil)")
	}
	var a fetchPullRequestArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("fetch_pull_request: bad args: %w", err)
	}
	if a.Repo == "" {
		return nil, fmt.Errorf("fetch_pull_request: repo is required (owner/name)")
	}
	if a.Number <= 0 {
		return nil, fmt.Errorf("fetch_pull_request: number must be a positive integer (got %d)", a.Number)
	}
	owner, name, err := githubprfetch.ParseRepo(a.Repo)
	if err != nil {
		return nil, fmt.Errorf("fetch_pull_request: %w", err)
	}
	token, err := t.Token()
	if err != nil {
		return nil, fmt.Errorf(
			"fetch_pull_request: no GitHub token "+
				"(set GITHUB_TOKEN env or populate "+
				"~/.config/foreman/github-token on the FleetNode): %w", err)
	}
	pr, err := t.Fetcher.Fetch(ctx, owner, name, a.Number, token)
	if err != nil {
		var herr *githubprfetch.HTTPError
		if errors.As(err, &herr) {
			switch {
			case herr.IsNotFound():
				return nil, fmt.Errorf("fetch_pull_request: PR %s#%d not found", a.Repo, a.Number)
			case herr.IsUnauthorized():
				return nil, fmt.Errorf(
					"fetch_pull_request: unauthorized for %s#%d; "+
						"foreman-agent's GitHub token may be missing the repo scope, "+
						"or the PR is in a private repo not visible to it",
					a.Repo, a.Number)
			}
		}
		return nil, fmt.Errorf("fetch_pull_request: %w", err)
	}
	return &agent.ToolResult{
		Output: map[string]any{
			"repo":            a.Repo,
			"number":          pr.Number,
			"title":           pr.Title,
			"body":            pr.Body,
			"state":           pr.State,
			"merged":          pr.Merged,
			"head_ref":        pr.HeadRef,
			"base_ref":        pr.BaseRef,
			"reviews":         pr.Reviews,
			"review_comments": pr.ReviewComments,
			"check_runs":      pr.CheckRuns,
		},
	}, nil
}
