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

// Package githubprfetch fetches a single GitHub pull request's details,
// reviews, review comments, and check runs so a coder Agent can read the
// PR it is asked to fix. The model needs to know WHAT it is fixing; reading
// the PR body and review feedback is the obvious way to learn that, and the
// harness reading it for the model keeps the model's tool budget free for
// the actual fix.
//
// Trade-offs deliberately taken:
//
//   - REST, not GraphQL: one endpoint per call, one JSON shape, no scopes
//     to guess. Body caps address the "rich content" argument for GraphQL.
//   - Title + body + state + reviews + review_comments + check_runs. The
//     model can pull more via `bash` if it really needs it (it almost
//     never does).
//   - Best-effort. A failed fetch logs and the executor keeps the existing
//     empty-body behavior; the loop runs with whatever buildUserPrompt
//     produces from the (empty) payload prompt.
package githubprfetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/defilantech/llmkube/pkg/foreman/agent/codehost"
)

// DefaultBodyCap bounds the PR body size we paste into the user prompt.
// 16 KiB is enough for the actual feature/bug description on the LLMKube
// tracker (median PR body ~1-2 KB, p99 ~8 KB). Larger PRs get truncated
// with a marker so the model knows there is more.
const DefaultBodyCap = 16 * 1024

// DefaultTimeout caps a single fetch. The GitHub API is fast; if the
// network is slow we'd rather skip the fetch than hold up the loop.
const DefaultTimeout = 10 * time.Second

// PullRequest is the minimum subset of the GitHub PR payload the executor
// needs. State is included so a model handed a closed PR can decide to
// NO-GO immediately rather than re-implement a fix that already shipped.
type PullRequest struct {
	Number         int             `json:"number"`
	Title          string          `json:"title"`
	Body           string          `json:"body"`
	State          string          `json:"state"`
	Merged         bool            `json:"merged"`
	HeadRef        string          `json:"head_ref"`
	BaseRef        string          `json:"base_ref"`
	Reviews        []Review        `json:"reviews,omitempty"`
	ReviewComments []ReviewComment `json:"review_comments,omitempty"`
	CheckRuns      []CheckRun      `json:"check_runs,omitempty"`
}

// Review is a single PR review.
type Review struct {
	Author    string `json:"author"`
	State     string `json:"state"`
	Body      string `json:"body"`
	Submitted string `json:"submitted_at"`
}

// ReviewComment is an inline review comment on a specific file/line.
type ReviewComment struct {
	Author       string `json:"author"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"original_line"`
	Body         string `json:"body"`
}

// CheckRun is a single check run result.
type CheckRun struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"details_url"`
}

// Fetcher is the seam between the executor and the GitHub API. The
// executor takes a Fetcher in via dependency injection so unit tests
// can substitute an httptest server. Production builds wire a Client
// backed by net/http.
type Fetcher interface {
	Fetch(ctx context.Context, owner, repo string, number int, token string) (*PullRequest, error)
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

// Fetch calls GET /repos/{owner}/{repo}/pulls/{number} and related
// endpoints to gather reviews, review comments, and check runs. Token
// is the GitHub PAT (or fine-grained token) used in the Authorization
// header; the empty string sends an unauthenticated request, which
// works for public repos at the lower rate-limit tier (60/hr/IP).
func (c *Client) Fetch(ctx context.Context, owner, repo string, number int, token string) (*PullRequest, error) {
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("githubprfetch: owner+repo required")
	}
	if number <= 0 {
		return nil, fmt.Errorf("githubprfetch: PR number must be positive")
	}

	base := c.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}

	// Fetch PR details.
	pr, err := c.fetchPR(ctx, base, owner, repo, number, token)
	if err != nil {
		return nil, err
	}

	// Fetch reviews (best-effort; a failure here does not abort the whole fetch).
	pr.Reviews, _ = c.fetchReviews(ctx, base, owner, repo, number, token)

	// Fetch review comments (best-effort).
	pr.ReviewComments, _ = c.fetchReviewComments(ctx, base, owner, repo, number, token)

	// Fetch check runs (best-effort).
	pr.CheckRuns, _ = c.fetchCheckRuns(ctx, base, owner, repo, number, token)

	return pr, nil
}

func (c *Client) fetchPR(
	ctx context.Context, base, owner, repo string, number int, token string,
) (*PullRequest, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", base, owner, repo, number)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("githubprfetch: build request: %w", err)
	}
	c.headers(req, token)

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubprfetch: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("githubprfetch: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{StatusCode: resp.StatusCode, URL: url}
	}

	var raw struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		State   string `json:"state"`
		Merged  bool   `json:"merged"`
		HeadRef string `json:"head_ref"`
		BaseRef string `json:"base_ref"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("githubprfetch: decode: %w", err)
	}

	return &PullRequest{
		Number:  raw.Number,
		Title:   raw.Title,
		Body:    truncateBody(raw.Body, c.BodyCap),
		State:   raw.State,
		Merged:  raw.Merged,
		HeadRef: raw.HeadRef,
		BaseRef: raw.BaseRef,
	}, nil
}

func (c *Client) fetchReviews(
	ctx context.Context, base, owner, repo string, number int, token string,
) ([]Review, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews", base, owner, repo, number)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("githubprfetch: build reviews request: %w", err)
	}
	c.headers(req, token)

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubprfetch: reviews http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{StatusCode: resp.StatusCode, URL: url}
	}

	var raw []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		State     string `json:"state"`
		Body      string `json:"body"`
		Submitted string `json:"submitted_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("githubprfetch: decode reviews: %w", err)
	}

	out := make([]Review, 0, len(raw))
	for _, r := range raw {
		out = append(out, Review{
			Author:    r.User.Login,
			State:     r.State,
			Body:      truncateBody(r.Body, c.BodyCap),
			Submitted: r.Submitted,
		})
	}
	return out, nil
}

func (c *Client) fetchReviewComments(
	ctx context.Context, base, owner, repo string, number int, token string,
) ([]ReviewComment, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/comments", base, owner, repo, number)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("githubprfetch: build review comments request: %w", err)
	}
	c.headers(req, token)

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubprfetch: review comments http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{StatusCode: resp.StatusCode, URL: url}
	}

	var raw []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Path         string `json:"path"`
		Line         int    `json:"line"`
		OriginalLine int    `json:"original_line"`
		Body         string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("githubprfetch: decode review comments: %w", err)
	}

	out := make([]ReviewComment, 0, len(raw))
	for _, r := range raw {
		out = append(out, ReviewComment{
			Author:       r.User.Login,
			Path:         r.Path,
			Line:         r.Line,
			OriginalLine: r.OriginalLine,
			Body:         r.Body,
		})
	}
	return out, nil
}

func (c *Client) fetchCheckRuns(
	ctx context.Context, base, owner, repo string, number int, token string,
) ([]CheckRun, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/check-runs", base, owner, repo, number)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("githubprfetch: build check runs request: %w", err)
	}
	c.headers(req, token)

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubprfetch: check runs http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{StatusCode: resp.StatusCode, URL: url}
	}

	var raw struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			DetailsURL string `json:"details_url"`
		} `json:"check_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("githubprfetch: decode check runs: %w", err)
	}

	out := make([]CheckRun, 0, len(raw.CheckRuns))
	for _, r := range raw.CheckRuns {
		out = append(out, CheckRun{
			Name:       r.Name,
			Conclusion: r.Conclusion,
			DetailsURL: r.DetailsURL,
		})
	}
	return out, nil
}

func (c *Client) headers(req *http.Request, token string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// HTTPError is returned when the GitHub API returns a non-200 status.
// Callers that want to distinguish "PR not found" from "token
// invalid" from "rate limited" can errors.As() into this and read
// StatusCode.
type HTTPError struct {
	StatusCode int
	URL        string
}

// Error implements error.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("githubprfetch: %s returned HTTP %d", e.URL, e.StatusCode)
}

// IsNotFound is true when the API returned 404; the executor uses this
// to log a single helpful line rather than a generic "fetch failed."
func (e *HTTPError) IsNotFound() bool { return e.StatusCode == http.StatusNotFound }

// IsUnauthorized is true when the API returned 401 or 403; usually
// means the token is wrong or the PR is in a private repo the
// token does not see.
func (e *HTTPError) IsUnauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// truncateBody bounds the body size. The marker is parseable by both
// humans and models: a clear note that more text exists and the
// model can pull the full PR via the GitHub UI / `gh` if needed.
func truncateBody(body string, cap int) string {
	if cap <= 0 {
		cap = DefaultBodyCap
	}
	if len(body) <= cap {
		return body
	}
	const marker = "\n\n... [PR body truncated; full text on GitHub]"
	keep := cap - len(marker)
	if keep < 0 {
		keep = 0
	}
	return body[:keep] + marker
}

// ParseRepo splits a repo slug into its owner (namespace) and repo
// (name) parts, splitting on the last slash so multi-segment slugs like
// "group/subgroup/project" are accepted: owner="group/subgroup",
// repo="project". For GitHub this means the namespace is passed as-is
// to the API path; GitHub will reject slugs that are not valid owner/repo
// pairs at request time. Returns an error if the input is malformed
// (no slash, empty segments, or a ".." traversal segment); the executor
// logs the error and skips the fetch (best-effort).
func ParseRepo(s string) (owner, repo string, err error) {
	// Delegate to the shared codehost validator so a repo slug is validated
	// and split by the SAME rule the clone and change-request paths use,
	// rather than a divergent local reimplementation.
	namespace, name, ok := codehost.SplitRepoSlug(s)
	if !ok {
		return "", "", errors.New("githubprfetch: invalid repo slug (want owner/name or " +
			"group/.../name with git-safe segments; no whitespace, empty, absolute, or '..' segments)")
	}
	return namespace, name, nil
}
