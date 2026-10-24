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

package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

// sizedBody returns a valid chat body of exactly n bytes.
func sizedBody(n int) []byte {
	head, tail := `{"model":"qwen","messages":[{"role":"user","content":"`, `"}]}`
	return []byte(head + strings.Repeat("a", n-len(head)-len(tail)) + tail)
}

// count_tokens tokenizes the whole conversation but runs no prefill, so it
// must add no prefill tokens, create or resize no session binding, and count
// as no placement.
func TestDispatchCountTokensSkipsPrefillAndSession(t *testing.T) {
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: {}\n\n")
	}))
	defer srv.Close()
	cfg, b := loadAwareBackend(srv.URL)
	b.Balancing.SessionHeaders = []string{"x-session-affinity"}
	d := NewDispatcher(cfg, WithRouterLabel("count-tokens-router"))
	d.SetEndpoints("qwen", []string{srv.URL})
	h := http.Header{}
	h.Set("X-Session-Affinity", "agent-1")
	placements := func() float64 {
		return testutil.ToFloat64(prommetrics.RouterPlacementsTotal.WithLabelValues("count-tokens-router", "qwen", "least_loaded", "")) +
			testutil.ToFloat64(prommetrics.RouterPlacementsTotal.WithLabelValues("count-tokens-router", "qwen", "sticky", ""))
	}
	// send dispatches one request and returns the prefill tokens held for it
	// after headers, before the first byte.
	send := func(path string, body []byte) int64 {
		t.Helper()
		resp, err := d.Dispatch(context.Background(), b, http.MethodPost, path, h, body)
		if err != nil {
			t.Fatal(err)
		}
		held := prefill(d, srv.URL)
		gate <- struct{}{}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return held
	}
	sessions := func() int { return d.sessionsFor("qwen", b.Balancing).Len() }

	if got := send("/v1/messages/count_tokens", sizedBody(4000)); got != 0 {
		t.Errorf("count_tokens on a new session held %d prefill tokens, want 0", got)
	}
	if n := sessions(); n != 0 {
		t.Errorf("count_tokens created %d session bindings, want 0", n)
	}
	if p := placements(); p != 0 {
		t.Errorf("count_tokens counted %v placements, want 0", p)
	}
	if got := send("/v1/messages", sizedBody(4000)); got != 1000 {
		t.Fatalf("first turn held %d, want 1000", got)
	}
	if got := send("/v1/messages/count_tokens", sizedBody(8000)); got != 0 {
		t.Errorf("count_tokens on a bound session held %d prefill tokens, want 0", got)
	}
	// The next turn grew by 4,000 bytes over the last real turn. Had
	// count_tokens recorded its 8,000 bytes, this would estimate 0.
	if got := send("/v1/messages", sizedBody(8000)); got != 1000 {
		t.Errorf("turn after count_tokens held %d, want 1000 (growth over the previous turn)", got)
	}
}
