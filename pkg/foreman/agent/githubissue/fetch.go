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

// Package githubissue fetches a single GitHub issue's title + body so the
// Foreman executor can include it in the user prompt sent to a coder
// Agent. The model needs to know WHAT it is fixing; reading the issue
// body is the obvious way to learn that, and the harness reading it
// for the model keeps the model's tool budget free for the actual fix.
//
// Trade-offs deliberately taken in v0.3.x:
//
//   - REST, not GraphQL: one endpoint, one JSON shape, no scopes to
//     guess. The body cap below already addresses the "rich content"
//     argument for GraphQL.
//   - Title + body + labels only. Comments often clarify the ask, but
//     they explode payload size unpredictably and the model can pull
//     them via `bash` if it really needs them (it almost never does).
//   - Best-effort. A failed fetch logs and the executor keeps the
//     existing empty-body behavior; the loop runs with whatever
//     buildUserPrompt produces from the (empty) payload prompt.
package githubissue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBodyCap bounds the issue body size we paste into the user
// prompt. 16 KiB is enough for the actual feature/bug description on
// the LLMKube tracker (median issue body ~1-2 KB, p99 ~8 KB). Larger
// issues get truncated with a marker so the model knows there is more.
const DefaultBodyCap = 16 * 1024

// DefaultTimeout caps a single fetch. The GitHub API is fast; if the
// network is slow we'd rather skip the fetch than hold up the loop.
const DefaultTimeout = 10 * time.Second

// Issue is the minimum subset of the GitHub issue payload the executor
// needs. State is included so a model handed a closed issue can decide
// to NO-GO immediately rather than re-implement a fix that already
// shipped.
type Issue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	State  string   `json:"state"`
	Labels []string `json:"labels,omitempty"`
}

// Fetcher is the seam between the executor and the GitHub API. The
// executor takes a Fetcher in via dependency injection so unit tests
// can substitute an httptest server. Production builds wire a Client
// backed by net/http.
type Fetcher interface {
	Fetch(ctx context.Context, owner, repo string, number int, token string) (*Issue, error)
}

// Client is the production Fetcher. BaseURL is overridable so tests
// can point at an httptest server; production leaves it empty and the
// client uses https://api.github.com.
type Client struct {
	HTTPClient *http.Client
	BaseURL    string // empty defaults to https://api.github.com
	BodyCap    int    // 0 defaults to DefaultBodyCap
}

// NewClient constructs a Client with sensible defaults: 10s timeout,
// GitHub's public API base URL, and the standard body cap.
func NewClient() *Client {
	return &Client{
		HTTPClient: &http.Client{Timeout: DefaultTimeout},
	}
}

// Fetch calls GET /repos/{owner}/{repo}/issues/{number}. Token is the
// GitHub PAT (or fine-grained token) used in the Authorization header;
// the empty string sends an unauthenticated request, which works for
// public repos at the lower rate-limit tier (60/hr/IP).
func (c *Client) Fetch(ctx context.Context, owner, repo string, number int, token string) (*Issue, error) {
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("githubissue: owner+repo required")
	}
	if number <= 0 {
		return nil, fmt.Errorf("githubissue: issue number must be positive")
	}

	base := c.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d", base, owner, repo, number)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("githubissue: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubissue: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("githubissue: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Surface the status code so the executor's log line tells you
		// whether the issue exists, the token is wrong, or the rate
		// limit kicked in. Don't expose the body verbatim in the error:
		// it's free-form HTML on auth failures and clutters logs.
		return nil, &HTTPError{StatusCode: resp.StatusCode, URL: url}
	}

	// Use a raw struct because the GitHub labels field is `[{name,...}]`
	// not `[]string` (so we can't unmarshal directly into Issue).
	var raw struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		State  string `json:"state"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("githubissue: decode: %w", err)
	}

	out := &Issue{
		Number: raw.Number,
		Title:  raw.Title,
		Body:   truncateBody(raw.Body, c.BodyCap),
		State:  raw.State,
	}
	for _, l := range raw.Labels {
		if l.Name != "" {
			out.Labels = append(out.Labels, l.Name)
		}
	}
	return out, nil
}

// HTTPError is returned when the GitHub API returns a non-200 status.
// Callers that want to distinguish "issue not found" from "token
// invalid" from "rate limited" can errors.As() into this and read
// StatusCode.
type HTTPError struct {
	StatusCode int
	URL        string
}

// Error implements error.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("githubissue: %s returned HTTP %d", e.URL, e.StatusCode)
}

// IsNotFound is true when the API returned 404; the executor uses this
// to log a single helpful line rather than a generic "fetch failed."
func (e *HTTPError) IsNotFound() bool { return e.StatusCode == http.StatusNotFound }

// IsUnauthorized is true when the API returned 401 or 403; usually
// means the token is wrong or the issue is in a private repo the
// token does not see.
func (e *HTTPError) IsUnauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// truncateBody bounds the body size. The marker is parseable by both
// humans and models: a clear note that more text exists and the
// model can pull the full issue via the GitHub UI / `gh` if needed.
func truncateBody(body string, cap int) string {
	if cap <= 0 {
		cap = DefaultBodyCap
	}
	if len(body) <= cap {
		return body
	}
	const marker = "\n\n... [issue body truncated; full text on GitHub]"
	keep := cap - len(marker)
	if keep < 0 {
		keep = 0
	}
	return body[:keep] + marker
}

// ParseRepo splits an "owner/repo" string into its parts. Returns an
// error if the input is malformed; the executor logs the error and
// skips the fetch (best-effort).
func ParseRepo(s string) (owner, repo string, err error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("githubissue: repo must be owner/repo")
	}
	return parts[0], parts[1], nil
}
