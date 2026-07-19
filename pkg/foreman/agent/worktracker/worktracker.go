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

// Package worktracker provides a generic interface for retrieving
// work items (e.g. GitHub issues) by ID, and a GitHub-backed
// implementation that delegates to the githubissue.Fetcher.
package worktracker

import (
	"context"
	"fmt"
	"strconv"

	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
)

// WorkItem represents a single unit of work, such as a GitHub issue.
type WorkItem struct {
	ID     string
	Title  string
	Body   string
	State  string
	URL    string
	Labels []string
}

// WorkItems is the interface for retrieving work items by ID.
type WorkItems interface {
	Get(ctx context.Context, repoSlug, id string) (*WorkItem, error)
}

// GitHubWorkItems implements WorkItems using the GitHub API via
// githubissue.Fetcher. The repoSlug is "owner/repo" and the id is
// the issue number as a string.
type GitHubWorkItems struct {
	Fetcher githubissue.Fetcher
	// Token authenticates the GitHub API calls. Empty means an
	// unauthenticated request (public repos, subject to rate limits),
	// matching the executor's prior best-effort issue fetch.
	Token string
}

// Get fetches a single work item by repo slug and ID. The ID is
// parsed as an integer and passed to the Fetcher. The returned
// githubissue.Issue is mapped to a WorkItem.
func (g *GitHubWorkItems) Get(ctx context.Context, repoSlug, id string) (*WorkItem, error) {
	owner, repo, err := githubissue.ParseRepo(repoSlug)
	if err != nil {
		return nil, fmt.Errorf("worktracker: parse repo slug: %w", err)
	}

	num, err := strconv.Atoi(id)
	if err != nil {
		return nil, fmt.Errorf("worktracker: parse id %q as int: %w", id, err)
	}

	issue, err := g.Fetcher.Fetch(ctx, owner, repo, num, g.Token)
	if err != nil {
		return nil, fmt.Errorf("worktracker: fetch issue %d: %w", num, err)
	}

	return &WorkItem{
		ID:     strconv.Itoa(issue.Number),
		Title:  issue.Title,
		Body:   issue.Body,
		State:  issue.State,
		URL:    "",
		Labels: issue.Labels,
	}, nil
}
