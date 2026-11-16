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

	"github.com/prometheus/client_golang/prometheus/testutil"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

const responsesBody = `{"model":"any","input":"hi"}`

// Only POST /v1/responses is mounted: the retrieve and cancel routes answer
// 404 from the mux without reaching a backend.
func TestProxyMountsResponses(t *testing.T) {
	_, mux, back := budgetTestProxy(t, nil)
	r := budgetPostPath(t, mux, "/v1/responses", responsesBody, nil)
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/responses = %d, want 200", r.StatusCode)
	}
	if got := back.LastPath(); got != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses", got)
	}
	for _, path := range []string{"/v1/responses/resp_1", "/v1/responses/resp_1/cancel"} {
		t.Run(path, func(t *testing.T) {
			before := back.calls.Load()
			r := budgetPostPath(t, mux, path, responsesBody, nil)
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
			if r.StatusCode != http.StatusNotFound {
				t.Errorf("%s = %d, want 404 (only POST /v1/responses is routed)", path, r.StatusCode)
			}
			if got := back.calls.Load(); got != before {
				t.Errorf("%s reached a backend: calls %d -> %d", path, before, got)
			}
		})
	}
}

// The Responses body reaches the backend exactly as sent, previous_response_id
// included: the route translates nothing.
func TestProxyResponsesForwardsBodyVerbatim(t *testing.T) {
	_, mux, back := budgetTestProxy(t, nil)
	sent := `{"model":"any","input":"hi","previous_response_id":"resp_abc","store":false,"max_output_tokens":16}`
	r := budgetPostPath(t, mux, "/v1/responses", sent, nil)
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.StatusCode)
	}
	if got := back.LastBody(); got != sent {
		t.Errorf("upstream body = %s, want it verbatim (%s)", got, sent)
	}
}

// The same rule set routes the new path on the request's model field.
func TestProxyResponsesRoutesByModel(t *testing.T) {
	h := newProxyHarness(t)
	h.cfg.Rules = append([]Rule{{
		Name:  "big-to-cloud",
		Match: RuleMatch{Models: []string{"big-*"}},
		Route: RuleRoute{Backends: []string{"cloud-opus"}},
	}}, h.cfg.Rules...)

	resp := h.postPath(t, "/v1/responses", map[string]any{"model": "big-1", "input": "hi"})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := h.cloudBack.calls.Load(); got != 1 {
		t.Errorf("cloud calls = %d, want 1 for a rule matching the model", got)
	}
	if got := h.localBack.calls.Load(); got != 0 {
		t.Errorf("local calls = %d, want 0", got)
	}
	if got := h.cloudBack.LastPath(); got != "/v1/responses" {
		t.Errorf("cloud upstream path = %q, want /v1/responses", got)
	}
}

// The same fallback chain serves the new path: a 5xx from the first backend
// fails over to the next.
func TestProxyResponsesFailsOver(t *testing.T) {
	h := newProxyHarness(t)
	h.localBack.status.Store(http.StatusInternalServerError)
	h.cfg.Rules = append([]Rule{{
		Name:  "all",
		Route: RuleRoute{Backends: []string{"local-qwen", "cloud-opus"}},
	}}, h.cfg.Rules...)

	resp := h.postPath(t, "/v1/responses", map[string]any{"model": "any", "input": "hi"})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fallback to cloud)", resp.StatusCode)
	}
	if got := h.cloudBack.calls.Load(); got != 1 {
		t.Errorf("cloud calls = %d, want 1 after fallback", got)
	}
	if got := h.cloudBack.LastPath(); got != "/v1/responses" {
		t.Errorf("cloud upstream path = %q, want /v1/responses", got)
	}
}

// A Responses body is charged from usage.input_tokens + usage.output_tokens;
// the *_details breakdowns are already part of those totals and are not added.
func TestProxyResponsesBudgetCharged(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "with a total",
			body: `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":60,"input_tokens_details":{"cached_tokens":20},"output_tokens":40,"output_tokens_details":{"reasoning_tokens":10},"total_tokens":100}}`,
		},
		{
			// Real bodies carry total_tokens, but a body without one must
			// still charge from the two totals rather than go uncharged.
			name: "without a total",
			body: `{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":60,"output_tokens":40}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy, mux, back := budgetTestProxy(t, []Budget{
				{Name: "router-cap", Scope: BudgetScopeRouter, Window: time.Hour, MaxTokens: 1000000},
			})
			back.body.Store(&tc.body)

			r := budgetPostPath(t, mux, "/v1/responses", responsesBody, nil)
			_ = r.Body.Close()
			if r.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", r.StatusCode)
			}

			snap := proxy.budgets.Snapshot()
			if len(snap) != 1 || snap[0].UsedTokens != 100 {
				t.Fatalf("snapshot = %+v, want one entry with UsedTokens 100 (60 input + 40 output, not the details)", snap)
			}
		})
	}
}

// A budgeted Responses stream is charged from its response.completed event,
// and its body is never given OpenAI's stream_options, which the API lacks.
func TestProxyResponsesStreamingBudgetCharged(t *testing.T) {
	proxy, mux, back := budgetTestProxy(t, []Budget{
		{Name: "router-cap", Scope: BudgetScopeRouter, Window: time.Hour, MaxTokens: 1000000},
	})
	back.stream.Store(true)
	script := []string{
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":60,\"input_tokens_details\":{\"cached_tokens\":20},\"output_tokens\":40,\"output_tokens_details\":{\"reasoning_tokens\":10},\"total_tokens\":100}}}",
	}
	back.sseScript.Store(&script)

	before := testutil.ToFloat64(prommetrics.RouterBudgetUnchargedTotal.WithLabelValues("default", "no_usage"))

	resp := budgetStreamPostPath(t, mux, "/v1/responses", `{"model":"any","input":"hi","stream":true}`)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := back.LastBody(); strings.Contains(got, "stream_options") {
		t.Errorf("upstream body = %s, want no stream_options on a Responses request", got)
	}

	snap := proxy.budgets.Snapshot()
	if len(snap) != 1 || snap[0].UsedTokens != 100 {
		t.Fatalf("snapshot = %+v, want one entry with UsedTokens 100 (60 input + 40 output from response.completed)", snap)
	}
	after := testutil.ToFloat64(prommetrics.RouterBudgetUnchargedTotal.WithLabelValues("default", "no_usage"))
	if after != before {
		t.Errorf("no_usage counter = %v, want unchanged at %v", after, before)
	}
}

// Errors the router writes on the new route keep the OpenAI envelope the other
// OpenAI routes use.
func TestProxyResponsesRouteErrorShape(t *testing.T) {
	cfg := &Config{
		Backends:     []Backend{{Name: "local", Tier: "local", Address: "http://127.0.0.1:1"}},
		DefaultRoute: "local",
		Policy:       Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewProxy(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Mount(mux)
	front := httptest.NewServer(mux)
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1/responses", "application/json", strings.NewReader(responsesBody))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	errObj, _ := got["error"].(map[string]any)
	if errObj["code"] != float64(http.StatusBadGateway) || errObj["message"] == nil {
		t.Errorf("body = %v, want {\"error\":{\"code\":502,\"message\":...}}", got)
	}
	if got["type"] != nil {
		t.Errorf("body = %v, want no Anthropic top-level type", got)
	}
}

// The fail-closed gate runs on the new path: a pii request whose only backend
// is unhealthy is refused before dispatch.
func TestProxyResponsesFailClosed(t *testing.T) {
	h := newProxyHarness(t)
	h.proxy.disp.MarkUnhealthy("local-qwen")

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(responsesBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-llmkube-classification", "pii")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 fail-closed runtime", rec.Code)
	}
	if got := h.localBack.calls.Load(); got != 0 {
		t.Errorf("local calls = %d, want 0 for the refused request", got)
	}
	if got := h.cloudBack.calls.Load(); got != 0 {
		t.Errorf("cloud calls = %d, want 0 for a pii request", got)
	}
}

// A backend that does not serve the Responses API answers 404: the client gets
// that 404 as is, and the backend stays healthy for its other routes.
func TestProxyResponsesBackend404PassesThrough(t *testing.T) {
	proxy, mux, back := budgetTestProxy(t, nil)
	back.status.Store(http.StatusNotFound)
	notFound := `{"detail":"Not Found"}`
	back.body.Store(&notFound)
	r := budgetPostPath(t, mux, "/v1/responses", responsesBody, nil)
	got, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusNotFound || string(got) != notFound {
		t.Fatalf("got %d %s, want the backend's own 404", r.StatusCode, got)
	}
	if !proxy.disp.IsHealthy("local-qwen") {
		t.Error("a 404 from the backend marked it unhealthy")
	}
}

// The new path runs a prefill, so it is a normal placement, not a
// count-tokens probe.
func TestResponsesIsNotATokenCountPath(t *testing.T) {
	if isTokenCountPath("/v1/responses") {
		t.Error("isTokenCountPath(/v1/responses) = true, want false")
	}
}
