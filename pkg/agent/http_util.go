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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Shared HTTP + polling helpers for the daemon-style executors
// (Ollama, oMLX) that otherwise duplicate the same marshal / request /
// drain / close boilerplate at every call site.

// drainAndClose drains up to 64 KiB of the response body so the
// underlying TCP connection can be reused by the client, then closes
// the body. Safe to defer even if resp is nil.
func drainAndClose(resp *http.Response) {
	if resp == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// postJSON marshals payload as JSON and POSTs it to url using client.
// The caller owns the response and must drainAndClose it. Content-Type
// is set to application/json. Pass a custom client when a longer
// timeout than the executor's default httpClient is needed.
func postJSON(
	ctx context.Context,
	client *http.Client,
	url string,
	payload interface{},
) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}

// pollUntil calls check at interval until it returns (true, nil), ctx
// is canceled, or timeout elapses. Transient errors from check are
// swallowed so a single failed poll does not abort the wait — the
// check closure is expected to log/handle its own errors and simply
// return (false, nil) when it wants polling to continue.
func pollUntil(
	ctx context.Context,
	interval, timeout time.Duration,
	check func(context.Context) (bool, error),
) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout after %s", timeout)
		case <-ticker.C:
			ok, err := check(ctx)
			if err == nil && ok {
				return nil
			}
		}
	}
}
