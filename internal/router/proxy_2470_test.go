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
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// postRaw posts body to path through handler and returns the response.
func postRaw(t *testing.T, handler http.Handler, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestProxy2470_ForwardsOtherV1Path is #2470: a POST to a /v1 path the router
// does not name (vLLM's /v1/decisions here) goes to the backend the request's
// model routes to, on the same path, with the body unchanged.
func TestProxy2470_ForwardsOtherV1Path(t *testing.T) {
	h := newProxyHarness(t)
	const body = `{"model":"d3","decision":{"options":["a","b"]}}`

	rec := postRaw(t, h.handler, "/v1/decisions", body, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := h.localBack.LastPath(); got != "/v1/decisions" {
		t.Errorf("backend path = %q, want /v1/decisions", got)
	}
	if got := h.localBack.LastBody(); got != body {
		t.Errorf("backend body = %q, want it unchanged: %q", got, body)
	}
}

// TestProxy2470_OtherV1PathOnlyReachesLocalBackends: a cloud-tier backend
// carries the router's provider key, and an unknown path there (files,
// batches, fine-tuning) would spend it outside any budget. The fallback route
// skips cloud backends: a rule that lists one first falls through to its
// local backend, and a rule with no local backend answers 404.
func TestProxy2470_OtherV1PathOnlyReachesLocalBackends(t *testing.T) {
	h := newProxyHarness(t)
	complex := map[string]string{"x-llmkube-task-complexity": "complex"}

	rec := postRaw(t, h.handler, "/v1/decisions", `{"model":"d3"}`, complex)
	if rec.Code != http.StatusOK || h.cloudBack.calls.Load() != 0 || h.localBack.calls.Load() != 1 {
		t.Fatalf("cloud-then-local rule: status %d, cloud calls %d, local calls %d; want the local backend only",
			rec.Code, h.cloudBack.calls.Load(), h.localBack.calls.Load())
	}

	cloud := newFakeBackend(t)
	cfg := &Config{
		Backends:     []Backend{{Name: "cloud-only", Tier: "cloud", Address: cloud.URL(), Provider: "openai", Model: "gpt"}},
		DefaultRoute: "cloud-only",
		Policy:       Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	mux := http.NewServeMux()
	NewProxy(cfg, slog.Default()).Mount(mux)

	rec = postRaw(t, mux, "/v1/files", `{"model":"gpt"}`, nil)
	if rec.Code != http.StatusNotFound || cloud.calls.Load() != 0 {
		t.Fatalf("cloud-only route: status %d (body %q), cloud calls %d; want 404 and no call",
			rec.Code, rec.Body.String(), cloud.calls.Load())
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || !strings.Contains(e.Error.Message, "local-tier") {
		t.Errorf("404 body = %q, want an OpenAI-shaped error naming the local-tier rule", rec.Body.String())
	}

	// The explicit paths still reach the cloud backend.
	rec = postRaw(t, mux, "/v1/chat/completions", `{"model":"gpt"}`, nil)
	if rec.Code != http.StatusOK || cloud.calls.Load() != 1 {
		t.Fatalf("chat completions: status %d, cloud calls %d; want the cloud backend", rec.Code, cloud.calls.Load())
	}
}

// TestProxy2470_OtherV1PathIsNotMirroredToCloud: a shadow rule mirrors a
// fallback-path request to its local backends only.
func TestProxy2470_OtherV1PathIsNotMirroredToCloud(t *testing.T) {
	h := newShadowHarness(t,
		Backend{Name: "primary"},
		Backend{Name: "cloud-candidate", Tier: "cloud", Provider: "openai", Model: "cand"},
		Backend{Name: "local-candidate", InferenceService: "local-cand"},
	)
	rec := postRaw(t, h.handler, "/v1/decisions", `{"model":"m"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	waitFor(t, "the local copy to finish", func() bool {
		return h.shadowCount("local-candidate", shadowOutcomeOK) == 1
	})
	// The copies start together; give a wrongly started cloud copy time to land.
	time.Sleep(50 * time.Millisecond)
	if got := h.ups["cloud-candidate"].calls.Load(); got != 0 {
		t.Errorf("cloud candidate got %d mirrored requests, want 0", got)
	}
}

// TestProxy2470_OtherV1PathChargesInputOutputTokens: vLLM's decisions
// response and the OpenAI Responses API report usage as input_tokens and
// output_tokens, which a budget must charge.
func TestProxy2470_OtherV1PathChargesInputOutputTokens(t *testing.T) {
	proxy, mux, back := budgetTestProxy(t, []Budget{
		{Name: "router-cap", Scope: BudgetScopeRouter, Window: time.Hour, MaxTokens: 1_000_000},
	})
	resp := `{"decision":"a","usage":{"input_tokens":70,"output_tokens":30,"total_tokens":100}}`
	back.body.Store(&resp)

	r := budgetPostPath(t, mux, "/v1/decisions", `{"model":"d3"}`, nil)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.StatusCode)
	}
	if snap := proxy.budgets.Snapshot(); len(snap) != 1 || snap[0].UsedTokens != 100 {
		t.Fatalf("snapshot = %+v, want one entry with UsedTokens 100", snap)
	}
}

// TestProxy2470_ResponsesStreamIsChargedAndNotRewritten covers a budgeted
// stream on /v1/responses: the router must not add stream_options, which the
// Responses API does not define, and must charge the usage its
// response.completed event carries.
func TestProxy2470_ResponsesStreamIsChargedAndNotRewritten(t *testing.T) {
	proxy, mux, back := budgetTestProxy(t, []Budget{
		{Name: "router-cap", Scope: BudgetScopeRouter, Window: time.Hour, MaxTokens: 1_000_000},
	})
	back.stream.Store(true)
	script := []string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20}}}`,
	}
	back.sseScript.Store(&script)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	const body = `{"model":"m","input":"hi","stream":true}`
	resp, err := http.Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "response.completed") {
		t.Fatalf("status %d body %q, want the stream through", resp.StatusCode, out)
	}
	if got := back.LastBody(); got != body {
		t.Errorf("backend body = %q, want it unchanged (no stream_options)", got)
	}
	waitFor(t, "the stream to be charged", func() bool {
		snap := proxy.budgets.Snapshot()
		return len(snap) == 1 && snap[0].UsedTokens == 20
	})
}

// TestProxy2470_OtherV1PathKeepsEncodedCharacters: the path goes upstream as
// the client escaped it, so an encoded '?' stays part of the path and never
// becomes a query string on the backend.
func TestProxy2470_OtherV1PathKeepsEncodedCharacters(t *testing.T) {
	h := newProxyHarness(t)
	rec := postRaw(t, h.handler, "/v1/decisions%3Fadmin=1", `{"model":"d3"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := h.localBack.LastPath(); got != "/v1/decisions?admin=1" {
		t.Errorf("backend decoded path = %q, want /v1/decisions?admin=1 (the '?' still in the path)", got)
	}
}

// TestProxy2470_GetOnOtherV1PathIs404: the fallback route is POST only.
func TestProxy2470_GetOnOtherV1PathIs404(t *testing.T) {
	h := newProxyHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions", nil)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 404 or 405", rec.Code)
	}
	if h.localBack.calls.Load() != 0 {
		t.Errorf("backend got %d calls, want 0", h.localBack.calls.Load())
	}
}

// TestProxy2470_OtherV1PathBadJSONIsAnOpenAIError: a body that does not parse
// is a 400 in the OpenAI error shape, as on the explicit OpenAI paths.
func TestProxy2470_OtherV1PathBadJSONIsAnOpenAIError(t *testing.T) {
	h := newProxyHarness(t)
	rec := postRaw(t, h.handler, "/v1/decisions", `{not json`, nil)
	var e struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %q, want a 400 OpenAI error", rec.Code, rec.Body.String())
	}
}

// TestProxy2470_LocalFailureIsTheCauseWhenCloudIsSkipped: when the local
// backend fails and the next one is skipped as cloud-tier, the client gets
// the local failure (502), not a 404 saying the path has no local backend.
func TestProxy2470_LocalFailureIsTheCauseWhenCloudIsSkipped(t *testing.T) {
	local := newFakeBackend(t)
	local.status.Store(http.StatusInternalServerError)
	cloud := newFakeBackend(t)
	cfg := &Config{
		Backends: []Backend{
			{Name: "local", Tier: "local", Address: local.URL()},
			{Name: "cloud", Tier: "cloud", Address: cloud.URL(), Provider: "openai", Model: "gpt"},
		},
		Rules:        []Rule{{Name: "all", Route: RuleRoute{Backends: []string{"local", "cloud"}}}},
		DefaultRoute: "local",
		Policy:       Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	mux := http.NewServeMux()
	NewProxy(cfg, slog.Default()).Mount(mux)

	rec := postRaw(t, mux, "/v1/decisions", `{"model":"d3"}`, nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "local returned 500") || cloud.calls.Load() != 0 {
		t.Fatalf("status %d body %q, cloud calls %d; want 502 naming the local failure and no cloud call",
			rec.Code, rec.Body.String(), cloud.calls.Load())
	}
}
