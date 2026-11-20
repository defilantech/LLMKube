/*
Copyright 2026.

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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// errGone is a 404 or 410: a log or artifact past its retention.
var errGone = errors.New("not found or expired")

func isGone(err error) bool { return errors.Is(err, errGone) }

// gitHub is the slice of the REST API the ledger uses. Reads retry on 5xx and
// rate limiting; writes never retry, so a timeout cannot post twice (the
// next run's marker check picks up whatever a failed write left undone).
type gitHub struct {
	api       string
	token     string
	http      *http.Client
	retryWait time.Duration
	reads     atomic.Int64
	writes    atomic.Int64
	// readOnly refuses every write. A dry run sets it, so no code path can
	// change the repository however the ledger logic is edited.
	readOnly bool
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func (g *gitHub) url(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return strings.TrimRight(g.api, "/") + "/" + strings.TrimLeft(path, "/")
}

func (g *gitHub) request(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, g.url(path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "llmkube-flake-ledger")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// net/http drops this header when a log or artifact download redirects
	// to blob storage on another host.
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	return g.http.Do(req)
}

// get returns the body of a GET and its Link header.
func (g *gitHub) get(ctx context.Context, path string) ([]byte, string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(g.retryWait * time.Duration(attempt)):
			}
		}
		g.reads.Add(1)
		resp, err := g.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		switch {
		case err != nil:
			lastErr = err
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			return nil, "", fmt.Errorf("GET %s: %w", path, errGone)
		case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
			lastErr = fmt.Errorf("GET %s: %s: %s", path, resp.Status, snippet(body))
		case resp.StatusCode >= 300:
			return nil, "", fmt.Errorf("GET %s: %s: %s", path, resp.Status, snippet(body))
		default:
			return body, resp.Header.Get("Link"), nil
		}
	}
	return nil, "", lastErr
}

// pages calls page with the body of path and of every rel="next" page after it.
func (g *gitHub) pages(ctx context.Context, path string, page func([]byte) error) error {
	for path != "" {
		body, link, err := g.get(ctx, path)
		if err != nil {
			return err
		}
		if err := page(body); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		path = ""
		if m := nextLink.FindStringSubmatch(link); m != nil {
			path = m[1]
		}
	}
	return nil
}

// post makes one write and decodes the response into v when v is not nil.
func (g *gitHub) post(ctx context.Context, path string, in, v any) error {
	const method = http.MethodPost
	if g.readOnly {
		return fmt.Errorf("%s %s: refused in a dry run", method, path)
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	g.writes.Add(1)
	resp, err := g.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, snippet(out))
	}
	if v != nil {
		return json.Unmarshal(out, v)
	}
	return nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
