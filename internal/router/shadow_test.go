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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

// shadowUpstream is a fake backend that records what each request carried and
// can answer an error status, hang until the request is cancelled, or stream.
type shadowUpstream struct {
	srv      *httptest.Server
	calls    atomic.Int64
	inflight atomic.Int64
	status   atomic.Int64
	hang     atomic.Bool
	stream   atomic.Bool
	usage    atomic.Bool
	reply    string

	mu        sync.Mutex
	headers   []http.Header
	bodies    []string
	cancelled atomic.Int64
}

func newShadowUpstream(t *testing.T, reply string) *shadowUpstream {
	t.Helper()
	u := &shadowUpstream{reply: reply}
	u.status.Store(http.StatusOK)
	release := make(chan struct{})
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		u.inflight.Add(1)
		defer u.inflight.Add(-1)
		raw, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.headers = append(u.headers, r.Header.Clone())
		u.bodies = append(u.bodies, string(raw))
		u.mu.Unlock()
		if u.hang.Load() {
			select {
			case <-r.Context().Done():
				u.cancelled.Add(1)
			case <-release:
			}
			return
		}
		if u.stream.Load() {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(int(u.status.Load()))
			for _, c := range []string{"a", "b", "c"} {
				_, _ = fmt.Fprintf(w, "data: {\"delta\":%q}\n\n", c)
				w.(http.Flusher).Flush()
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(u.status.Load()))
		if u.usage.Load() {
			_, _ = io.WriteString(w, `{"from":"`+u.reply+`","usage":{"prompt_tokens":10,"completion_tokens":5}}`)
			return
		}
		_, _ = io.WriteString(w, `{"from":"`+u.reply+`"}`)
	}))
	// Unblock a hung handler before the server waits for it to return.
	t.Cleanup(func() { close(release); u.srv.Close() })
	return u
}

// first returns the headers and body of the first request u received.
func (u *shadowUpstream) first() (http.Header, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.headers[0], u.bodies[0]
}

type shadowHarness struct {
	proxy   *Proxy
	handler http.Handler
	rule    string
	ups     map[string]*shadowUpstream
}

// shadowHarnessSeq makes each harness's rule name unique, so its metric series
// start at zero even under -count.
var shadowHarnessSeq atomic.Int64

// newShadowHarness serves one shadow rule, named after the test and unique
// per harness so its metric series are its own, over the given backends in
// order.
func newShadowHarness(t *testing.T, backends ...Backend) *shadowHarness {
	t.Helper()
	return newShadowHarnessWith(t, nil, backends...)
}

// newShadowHarnessWith is newShadowHarness with a hook to adjust the config
// before it is validated.
func newShadowHarnessWith(t *testing.T, mutate func(*Config), backends ...Backend) *shadowHarness {
	t.Helper()
	rule := fmt.Sprintf("%s-%d", strings.ReplaceAll(t.Name(), "/", "_"), shadowHarnessSeq.Add(1))
	h := &shadowHarness{rule: rule, ups: map[string]*shadowUpstream{}}
	cfg := &Config{Policy: Policy{Classification: ClassificationPolicy{Mode: "header-only"}}}
	names := make([]string, 0, len(backends))
	for _, b := range backends {
		u := newShadowUpstream(t, b.Name)
		h.ups[b.Name] = u
		b.Address = u.srv.URL
		if b.Tier == "" {
			b.Tier = "local"
		}
		cfg.Backends = append(cfg.Backends, b)
		names = append(names, b.Name)
	}
	cfg.Rules = []Rule{{
		Name:  h.rule,
		Match: RuleMatch{Models: []string{"m"}},
		Route: RuleRoute{Backends: names, Strategy: strategyShadow},
	}}
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("shadow harness config: %v", err)
	}
	h.proxy = NewProxy(cfg, slog.Default())
	t.Cleanup(h.proxy.Close)
	mux := http.NewServeMux()
	h.proxy.Mount(mux)
	h.handler = mux
	return h
}

func (h *shadowHarness) post(t *testing.T, payload map[string]any, headers map[string]string) (int, string) {
	t.Helper()
	ph := &proxyHarness{handler: h.handler}
	resp := ph.post(t, payload, headers)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (h *shadowHarness) shadowCount(backend, outcome string) float64 {
	return testutil.ToFloat64(prommetrics.RouterShadowRequestsTotal.WithLabelValues("default", h.rule, backend, outcome))
}

// TestProxyShadowMirrorsToEveryOtherBackend: the first backend answers the
// client, and each other backend gets one copy carrying the shadow header,
// the backend's own credentials in place of the client's, and its own model.
func TestProxyShadowMirrorsToEveryOtherBackend(t *testing.T) {
	t.Setenv("SHADOW_TEST_KEY", "candidate-secret")
	h := newShadowHarness(t,
		Backend{Name: "primary"},
		Backend{Name: "candidate", Tier: "cloud", Provider: "openai", Model: "cand-model", CredentialsEnv: "SHADOW_TEST_KEY"},
		Backend{Name: "local-candidate", InferenceService: "local-cand"},
	)
	code, body := h.post(t, map[string]any{"model": "m"}, map[string]string{
		"Authorization": "Bearer client-secret",
		"X-Team":        "eval",
	})
	if code != http.StatusOK || !strings.Contains(body, `"from":"primary"`) {
		t.Fatalf("client got %d %s, want 200 from primary", code, body)
	}
	waitFor(t, "both copies to finish", func() bool {
		return h.shadowCount("candidate", shadowOutcomeOK) == 1 && h.shadowCount("local-candidate", shadowOutcomeOK) == 1
	})

	if got := h.ups["primary"].calls.Load(); got != 1 {
		t.Errorf("primary calls = %d, want 1", got)
	}
	if hdr, _ := h.ups["primary"].first(); hdr.Get(ShadowHeader) != "" {
		t.Errorf("the client's request carried %s=%q, want no shadow header", ShadowHeader, hdr.Get(ShadowHeader))
	}
	for name, wantModel := range map[string]string{"candidate": "cand-model", "local-candidate": "local-cand"} {
		u := h.ups[name]
		if got := u.calls.Load(); got != 1 {
			t.Errorf("%s calls = %d, want 1 copy", name, got)
			continue
		}
		hdr, raw := u.first()
		if hdr.Get(ShadowHeader) != "true" {
			t.Errorf("%s: %s = %q, want true", name, ShadowHeader, hdr.Get(ShadowHeader))
		}
		if hdr.Get("X-Team") != "eval" {
			t.Errorf("%s: X-Team = %q, want the client's header forwarded", name, hdr.Get("X-Team"))
		}
		var m struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal([]byte(raw), &m)
		if m.Model != wantModel {
			t.Errorf("%s: model %q, want %q", name, m.Model, wantModel)
		}
	}
	if got, _ := h.ups["candidate"].first(); got.Get("Authorization") != "Bearer candidate-secret" {
		t.Errorf("candidate Authorization = %q, want the backend's own key", got.Get("Authorization"))
	}
	if got, _ := h.ups["local-candidate"].first(); got.Get("Authorization") != "" {
		t.Errorf("local-candidate Authorization = %q, want the client's credential stripped", got.Get("Authorization"))
	}
}

// TestProxyShadowClientUnaffectedByFailingOrHangingShadows: a copy that errors
// or never answers does not delay or change the client's response.
func TestProxyShadowClientUnaffectedByFailingOrHangingShadows(t *testing.T) {
	h := newShadowHarness(t, Backend{Name: "primary"}, Backend{Name: "broken"}, Backend{Name: "stuck"})
	h.ups["broken"].status.Store(http.StatusInternalServerError)
	h.ups["stuck"].hang.Store(true)

	start := time.Now()
	code, body := h.post(t, map[string]any{"model": "m"}, nil)
	if code != http.StatusOK || !strings.Contains(body, `"from":"primary"`) {
		t.Fatalf("client got %d %s, want 200 from primary", code, body)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("client waited %s with a hung shadow, want no wait", elapsed)
	}
	waitFor(t, "the hung copy to arrive", func() bool { return h.ups["stuck"].inflight.Load() == 1 })
	waitFor(t, "the failing copy to be counted", func() bool { return h.shadowCount("broken", shadowOutcomeError) == 1 })
}

// TestProxyShadowFailuresNeverQuarantine: a shadow backend that answers 500
// stays healthy, so other rules keep serving from it and it keeps receiving
// copies.
func TestProxyShadowFailuresNeverQuarantine(t *testing.T) {
	h := newShadowHarness(t, Backend{Name: "primary"}, Backend{Name: "candidate"})
	h.ups["candidate"].status.Store(http.StatusInternalServerError)
	for i := 1; i <= 3; i++ {
		if code, _ := h.post(t, map[string]any{"model": "m"}, nil); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		waitFor(t, "the failing copy to be counted", func() bool {
			return h.shadowCount("candidate", shadowOutcomeError) == float64(i)
		})
		if !h.proxy.Dispatcher().IsHealthy("candidate") {
			t.Fatalf("after %d failed copies the candidate is quarantined, want it healthy", i)
		}
	}
	if got := h.ups["candidate"].calls.Load(); got != 3 {
		t.Errorf("candidate calls = %d, want 3 (one copy per request)", got)
	}
}

// TestProxyShadowDropsCopiesOverTheCap: with maxShadowInFlight copies stuck on
// a backend, further copies to it are dropped and counted, and the client
// still gets its answer.
func TestProxyShadowDropsCopiesOverTheCap(t *testing.T) {
	h := newShadowHarness(t, Backend{Name: "primary"}, Backend{Name: "slow"})
	h.ups["slow"].hang.Store(true)
	const extra = 2
	for range maxShadowInFlight + extra {
		if code, _ := h.post(t, map[string]any{"model": "m"}, nil); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	}
	waitFor(t, "the capped copies to arrive", func() bool { return h.ups["slow"].inflight.Load() == maxShadowInFlight })
	if got := h.shadowCount("slow", shadowOutcomeDropped); got != extra {
		t.Errorf("dropped copies = %v, want %d", got, extra)
	}
	if got := h.ups["slow"].calls.Load(); got != maxShadowInFlight {
		t.Errorf("slow calls = %d, want %d (the cap)", got, maxShadowInFlight)
	}
}

// TestProxyShadowPrimaryFallback: when the first backend fails, the client is
// served by the next one exactly as under primary-fallback. The backend that
// served gets no extra copy, the one that failed is not mirrored to again, and
// the backends after the server still get theirs.
func TestProxyShadowPrimaryFallback(t *testing.T) {
	h := newShadowHarness(t, Backend{Name: "first"}, Backend{Name: "second"}, Backend{Name: "third"})
	h.ups["first"].status.Store(http.StatusInternalServerError)

	code, body := h.post(t, map[string]any{"model": "m"}, nil)
	if code != http.StatusOK || !strings.Contains(body, `"from":"second"`) {
		t.Fatalf("client got %d %s, want 200 from second", code, body)
	}
	waitFor(t, "the copy to third", func() bool { return h.shadowCount("third", shadowOutcomeOK) == 1 })
	for name, want := range map[string]int64{"first": 1, "second": 1, "third": 1} {
		if got := h.ups[name].calls.Load(); got != want {
			t.Errorf("%s calls = %d, want %d", name, got, want)
		}
	}
	if hdr, _ := h.ups["second"].first(); hdr.Get(ShadowHeader) != "" {
		t.Errorf("second served the client but got the shadow header")
	}
}

// TestProxyShadowStreamsAreMirroredAndDrained: a streaming request is copied
// as is and the shadow's stream is read to the end.
func TestProxyShadowStreamsAreMirroredAndDrained(t *testing.T) {
	h := newShadowHarness(t, Backend{Name: "primary"}, Backend{Name: "candidate"})
	h.ups["primary"].stream.Store(true)
	h.ups["candidate"].stream.Store(true)

	code, body := h.post(t, map[string]any{"model": "m", "stream": true}, nil)
	if code != http.StatusOK || !strings.Contains(body, "[DONE]") {
		t.Fatalf("client got %d %q, want the primary's stream", code, body)
	}
	waitFor(t, "the streamed copy to drain", func() bool { return h.shadowCount("candidate", shadowOutcomeOK) == 1 })
	if _, raw := h.ups["candidate"].first(); !strings.Contains(raw, `"stream":true`) {
		t.Errorf("candidate body %s, want the streaming request as sent", raw)
	}
}

// TestProxyShadowCopiesAreNotCharged: a budget charges the tokens the serving
// backend reported, and nothing for the copies, which report usage too.
func TestProxyShadowCopiesAreNotCharged(t *testing.T) {
	h := newShadowHarnessWith(t, func(c *Config) {
		c.Policy.Budgets = []Budget{{Name: "cap", Scope: BudgetScopeRouter, Window: time.Hour, MaxTokens: 1000}}
	}, Backend{Name: "primary"}, Backend{Name: "candidate"})
	h.ups["primary"].usage.Store(true)
	h.ups["candidate"].usage.Store(true)

	if code, _ := h.post(t, map[string]any{"model": "m"}, nil); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	waitFor(t, "the copy to finish", func() bool { return h.shadowCount("candidate", shadowOutcomeOK) == 1 })
	found := false
	for _, u := range h.proxy.budgets.Snapshot() {
		if u.Name != "cap" {
			continue
		}
		found = true
		if u.UsedTokens != 15 {
			t.Errorf("budget used %d tokens, want 15 (the primary's usage only)", u.UsedTokens)
		}
	}
	if !found {
		t.Fatal("budget cap missing from the snapshot")
	}
}

// TestProxyShadowSkipsQuarantinedTarget: a target quarantined by client
// traffic is not eligible, so it gets no copy while the window lasts.
func TestProxyShadowSkipsQuarantinedTarget(t *testing.T) {
	h := newShadowHarness(t, Backend{Name: "primary"}, Backend{Name: "candidate"})
	h.proxy.Dispatcher().MarkUnhealthy("candidate")
	if code, _ := h.post(t, map[string]any{"model": "m"}, nil); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	time.Sleep(50 * time.Millisecond)
	if got := h.ups["candidate"].calls.Load(); got != 0 {
		t.Errorf("quarantined candidate calls = %d, want 0", got)
	}
}

// TestProxyCloseCancelsShadowCopies: Close cancels a copy still in flight,
// waits for its goroutine, and copies a handler starts afterwards are dropped,
// so no shadow work outlives the proxy.
func TestProxyCloseCancelsShadowCopies(t *testing.T) {
	h := newShadowHarness(t, Backend{Name: "primary"}, Backend{Name: "stuck"})
	h.ups["stuck"].hang.Store(true)
	if code, _ := h.post(t, map[string]any{"model": "m"}, nil); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	waitFor(t, "the hung copy to arrive", func() bool { return h.ups["stuck"].inflight.Load() == 1 })

	done := make(chan struct{})
	go func() { h.proxy.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return with a hung shadow copy in flight")
	}
	if got := h.proxy.shadows.slots("stuck").Load(); got != 0 {
		t.Errorf("shadow slots held after Close = %d, want 0 (the goroutine returned)", got)
	}
	waitFor(t, "the upstream to see the copy cancelled", func() bool { return h.ups["stuck"].cancelled.Load() == 1 })
	if got := h.shadowCount("stuck", shadowOutcomeError); got != 1 {
		t.Errorf("cancelled copy counted %v times as an error, want 1", got)
	}

	if code, _ := h.post(t, map[string]any{"model": "m"}, nil); code != http.StatusOK {
		t.Fatalf("status after Close = %d, want 200", code)
	}
	if got := h.shadowCount("stuck", shadowOutcomeDropped); got != 1 {
		t.Errorf("copies after Close counted dropped %v times, want 1", got)
	}
	if got := h.ups["stuck"].calls.Load(); got != 1 {
		t.Errorf("stuck calls = %d, want 1 (no copy after Close)", got)
	}
}

func TestConfigValidateShadowRules(t *testing.T) {
	pooled := func(name string) Backend {
		return Backend{Name: name, Tier: "local", Address: "http://" + name,
			Pool: &BackendPool{Name: "slot", Namespace: "ns", Member: name}}
	}
	cases := []struct {
		name     string
		backends []Backend
		route    []string
		wantErr  string
	}{
		{name: "two backends", route: []string{"local-qwen", "cloud-opus"}},
		{name: "one backend", route: []string{"local-qwen"}, wantErr: "at least two backends"},
		{name: "pooled target", route: []string{"local-qwen", "p1"}, backends: []Backend{pooled("p1")},
			wantErr: `mirror to backend "p1", a member of ModelPool ns/slot`},
		{name: "pooled primary", route: []string{"p1", "local-qwen"}, backends: []Backend{pooled("p1")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Backends = append(cfg.Backends, tc.backends...)
			cfg.Rules = append(cfg.Rules, Rule{Name: "mirror", Route: RuleRoute{Backends: tc.route, Strategy: strategyShadow}})
			err := cfg.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate: %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("Validate: %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
