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
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// withRandInt64N replaces the random source weighted rules draw from, so a
// test can make the draws deterministic. It lives in a test file so the
// dead-code guard does not see a production symbol only tests use.
func withRandInt64N(fn randInt64N) ProxyOption {
	return func(p *Proxy) { p.randN = fn }
}

// seededRand returns a deterministic, concurrency-safe randInt64N.
func seededRand(seed uint64) randInt64N {
	var mu sync.Mutex
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) //nolint:gosec // G404: deterministic test source
	return func(n int64) int64 {
		mu.Lock()
		defer mu.Unlock()
		return r.Int64N(n)
	}
}

// lastDraw always draws the top of the range, which picks the last remaining
// candidate in declared order. A weighted rule then tries its backends in
// reverse declared order, the opposite of primary-fallback, so a test can
// tell the two strategies apart without statistics.
func lastDraw(n int64) int64 { return n - 1 }

func weight(w int32) *int32 { return &w }

// weightedHarness serves one rule over the named backends, each a fake
// upstream.
type weightedHarness struct {
	proxy   *Proxy
	handler http.Handler
	backs   map[string]*fakeBackend
}

type weightedSpec struct {
	name   string
	weight *int32
	caps   []string
}

func newWeightedHarness(t *testing.T, rnd randInt64N, required []string, specs ...weightedSpec) *weightedHarness {
	t.Helper()
	return newRuleHarness(t, strategyWeighted, rnd, required, specs...)
}

// newRuleHarness serves one rule with the given strategy over the named
// backends, each a fake upstream.
func newRuleHarness(t *testing.T, strategy string, rnd randInt64N, required []string, specs ...weightedSpec) *weightedHarness {
	t.Helper()
	h := &weightedHarness{backs: map[string]*fakeBackend{}}
	cfg := &Config{
		Policy: Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		fb := newFakeBackend(t)
		h.backs[s.name] = fb
		cfg.Backends = append(cfg.Backends, Backend{
			Name: s.name, Tier: "local", Address: fb.URL(), Weight: s.weight, Capabilities: s.caps,
		})
		names = append(names, s.name)
	}
	cfg.Rules = []Rule{{
		Name:  "split",
		Match: RuleMatch{Models: []string{"m"}, RequiredCapabilities: required},
		Route: RuleRoute{Backends: names, Strategy: strategy},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("weighted harness config: %v", err)
	}
	h.proxy = NewProxy(cfg, slog.Default(), withRandInt64N(rnd))
	mux := http.NewServeMux()
	h.proxy.Mount(mux)
	h.handler = mux
	return h
}

func (h *weightedHarness) post(t *testing.T) int {
	t.Helper()
	ph := &proxyHarness{handler: h.handler}
	resp := ph.post(t, map[string]any{"model": "m"}, nil)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func (h *weightedHarness) calls(name string) int64 { return h.backs[name].calls.Load() }

// TestWeightedOrderDrawsEveryCandidateOnce pins the failover contract: the
// order is a permutation of the candidates, so a failover never retries a
// backend it already tried and never skips one that could serve.
func TestWeightedOrderDrawsEveryCandidateOnce(t *testing.T) {
	cands := []weightedCandidate{{"a", 1}, {"b", 5}, {"c", 2}, {"d", 1}}
	rnd := seededRand(7)
	for i := range 200 {
		got := weightedOrder(cands, rnd)
		sorted := slices.Sorted(slices.Values(got))
		if !slices.Equal(sorted, []string{"a", "b", "c", "d"}) {
			t.Fatalf("draw %d: order %v is not a permutation of a,b,c,d", i, got)
		}
	}
	if !slices.Equal(cands, []weightedCandidate{{"a", 1}, {"b", 5}, {"c", 2}, {"d", 1}}) {
		t.Errorf("weightedOrder mutated its input: %v", cands)
	}
}

// TestWeightedOrderLastDrawReversesDeclaredOrder documents the deterministic
// source the proxy tests below rely on.
func TestWeightedOrderLastDrawReversesDeclaredOrder(t *testing.T) {
	got := weightedOrder([]weightedCandidate{{"a", 1}, {"b", 3}, {"c", 2}}, lastDraw)
	if want := []string{"c", "b", "a"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestWeightedOrderFirstPickIsProportional is the one statistical test: with a
// fixed seed it is deterministic, and the tolerance is several standard
// deviations wide.
func TestWeightedOrderFirstPickIsProportional(t *testing.T) {
	cands := []weightedCandidate{{"a", 1}, {"b", 3}}
	rnd := seededRand(42)
	const draws = 4000
	counts := map[string]int{}
	for range draws {
		counts[weightedOrder(cands, rnd)[0]]++
	}
	// Expect 1000 / 3000; one standard deviation is about 27.
	if a := counts["a"]; a < 850 || a > 1150 {
		t.Errorf("a was drawn first %d of %d times, want about 1000 (weight 1 of 4); counts=%v", a, draws, counts)
	}
}

// TestProxyWeightedRuleSplitsTraffic is the issue's reproduction: two backends
// at equal weight must both take traffic, where primary-fallback sent every
// request to the first.
func TestProxyWeightedRuleSplitsTraffic(t *testing.T) {
	h := newWeightedHarness(t, seededRand(1), nil,
		weightedSpec{name: "mac-m5"}, weightedSpec{name: "nvidia-gb10", weight: weight(3)})
	const n = 400
	for range n {
		if code := h.post(t); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	}
	mac, gb10 := h.calls("mac-m5"), h.calls("nvidia-gb10")
	if mac+gb10 != n {
		t.Fatalf("calls mac-m5=%d nvidia-gb10=%d, want %d in total (one attempt per request)", mac, gb10, n)
	}
	// Weights 1:3 (mac-m5 has no weight, so the default of 1): expect 100
	// and 300; one standard deviation is about 9.
	if mac < 60 || mac > 140 {
		t.Errorf("mac-m5 served %d of %d, want about 100 (weight 1 of 4); nvidia-gb10 served %d", mac, n, gb10)
	}
}

// TestProxyWeightedRuleDrawsPerRequest pins that the draw, not the declared
// order, picks the backend: the source that always takes the last candidate
// sends every request to the backend declared last.
func TestProxyWeightedRuleDrawsPerRequest(t *testing.T) {
	h := newWeightedHarness(t, lastDraw, nil, weightedSpec{name: "a"}, weightedSpec{name: "b"})
	for range 3 {
		if code := h.post(t); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	}
	if a, b := h.calls("a"), h.calls("b"); a != 0 || b != 3 {
		t.Errorf("calls a=%d b=%d, want a=0 b=3 (the draw picks b, the declared order would pick a)", a, b)
	}
}

// TestProxyWeightedRuleFailsOverAwayFromFailingPick: the drawn backend
// answers 500, so the request must move to another backend of the rule and
// succeed there, and the failing backend is not tried a second time.
func TestProxyWeightedRuleFailsOverAwayFromFailingPick(t *testing.T) {
	h := newWeightedHarness(t, lastDraw, nil,
		weightedSpec{name: "a"}, weightedSpec{name: "b"}, weightedSpec{name: "c"})
	h.backs["c"].status.Store(http.StatusInternalServerError)

	if code := h.post(t); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from a failover backend", code)
	}
	if c := h.calls("c"); c != 1 {
		t.Errorf("failing pick c called %d times, want exactly 1 (never retried)", c)
	}
	if b := h.calls("b"); b != 1 {
		t.Errorf("failover b called %d times, want 1", b)
	}
	if a := h.calls("a"); a != 0 {
		t.Errorf("a called %d times, want 0 (b served)", a)
	}
}

// TestProxyWeightedRuleAllFailReturns502 checks the exhausted case tries each
// backend exactly once and reports the outage like primary-fallback does.
func TestProxyWeightedRuleAllFailReturns502(t *testing.T) {
	h := newWeightedHarness(t, seededRand(3), nil, weightedSpec{name: "a"}, weightedSpec{name: "b"})
	h.backs["a"].status.Store(http.StatusInternalServerError)
	h.backs["b"].status.Store(http.StatusServiceUnavailable)
	if code := h.post(t); code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", code)
	}
	if a, b := h.calls("a"), h.calls("b"); a != 1 || b != 1 {
		t.Errorf("calls a=%d b=%d, want 1 each", a, b)
	}
}

// TestProxyWeightedRuleSkipsQuarantinedBackend: a quarantined backend takes no
// traffic while its window lasts, even when the draw lands on it.
func TestProxyWeightedRuleSkipsQuarantinedBackend(t *testing.T) {
	h := newWeightedHarness(t, lastDraw, nil, weightedSpec{name: "a"}, weightedSpec{name: "b"})
	h.proxy.Dispatcher().MarkUnhealthy("b")
	for range 5 {
		if code := h.post(t); code != http.StatusOK {
			t.Fatalf("status = %d, want 200 from a", code)
		}
	}
	if a, b := h.calls("a"), h.calls("b"); a != 5 || b != 0 {
		t.Errorf("calls a=%d b=%d, want a=5 b=0 (b is quarantined)", a, b)
	}
}

// TestProxyWeightedRuleZeroWeightIsDrained: a zero-weight backend takes no
// traffic, and stays out even when the rule's only other backend fails. This
// matches the Gateway data plane, where a zero-weight backendRef is dropped.
func TestProxyWeightedRuleZeroWeightIsDrained(t *testing.T) {
	h := newWeightedHarness(t, seededRand(5), nil,
		weightedSpec{name: "drained", weight: weight(0)}, weightedSpec{name: "live"})
	for range 50 {
		if code := h.post(t); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	}
	if d, l := h.calls("drained"), h.calls("live"); d != 0 || l != 50 {
		t.Fatalf("calls drained=%d live=%d, want 0 and 50", d, l)
	}

	h.backs["live"].status.Store(http.StatusInternalServerError)
	if code := h.post(t); code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502: the drained backend must not stand in for a failed one", code)
	}
	if d := h.calls("drained"); d != 0 {
		t.Errorf("drained called %d times after the live backend failed, want 0", d)
	}
}

// TestProxyWeightedRuleHonorsRequiredCapabilities: the rule's required
// capabilities filter its candidates, so a backend without them takes none of
// the rule's traffic however the draw falls.
func TestProxyWeightedRuleHonorsRequiredCapabilities(t *testing.T) {
	h := newWeightedHarness(t, lastDraw, []string{"vision"},
		weightedSpec{name: "sees", caps: []string{"vision"}}, weightedSpec{name: "blind"})
	for range 3 {
		if code := h.post(t); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	}
	if s, b := h.calls("sees"), h.calls("blind"); s != 3 || b != 0 {
		t.Errorf("calls sees=%d blind=%d, want 3 and 0", s, b)
	}
}

// TestBackendWeightWireShape pins the wire contract with the controller: an
// absent weight is the default share, and an explicit 0 survives decoding.
func TestBackendWeightWireShape(t *testing.T) {
	var bs []Backend
	if err := json.Unmarshal([]byte(`[{"name":"absent"},{"name":"zero","weight":0},{"name":"five","weight":5}]`), &bs); err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{1, 0, 5} {
		if got := bs[i].effectiveWeight(); got != want {
			t.Errorf("%s: effective weight %d, want %d", bs[i].Name, got, want)
		}
	}
	out, err := json.Marshal(bs[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"weight":0`) {
		t.Errorf("explicit zero weight dropped on marshal: %s", out)
	}
}

func TestConfigValidateWeightedRules(t *testing.T) {
	pooled := func(name, member string) Backend {
		return Backend{Name: name, Tier: "local", Address: "http://" + name,
			Pool: &BackendPool{Name: "slot", Namespace: "ns", Member: member}}
	}
	cases := []struct {
		name     string
		backends []Backend
		route    []string
		strategy string
		required []string
		wantErr  string
	}{
		{name: "default weights", route: []string{"local-qwen", "cloud-opus"}, strategy: strategyWeighted},
		{name: "one drained one live", route: []string{"local-qwen", "cloud-opus"}, strategy: strategyWeighted,
			backends: []Backend{{Name: "local-qwen", Weight: weight(0)}}},
		{name: "every weight zero", route: []string{"local-qwen", "cloud-opus"}, strategy: strategyWeighted,
			backends: []Backend{{Name: "local-qwen", Weight: weight(0)}, {Name: "cloud-opus", Weight: weight(0)}},
			wantErr:  "positive weight"},
		{name: "only capable backend drained", route: []string{"local-qwen", "cloud-opus"}, strategy: strategyWeighted,
			required: []string{"vision"},
			backends: []Backend{{Name: "local-qwen", Weight: weight(0), Capabilities: []string{"vision"}}},
			wantErr:  "positive weight"},
		{name: "zero weight ignored by primary-fallback", route: []string{"local-qwen"},
			backends: []Backend{{Name: "local-qwen", Weight: weight(0)}}},
		{name: "negative weight", route: []string{"local-qwen"},
			backends: []Backend{{Name: "local-qwen", Weight: weight(-1)}}, wantErr: "must not be negative"},
		{name: "unknown strategy", route: []string{"local-qwen"}, strategy: "round-robin", wantErr: "route.strategy"},
		{name: "two members of one pool", route: []string{"p1", "p2"}, strategy: strategyWeighted,
			backends: []Backend{pooled("p1", "qwen"), pooled("p2", "gemma")}, wantErr: "share one GPU slot"},
		{name: "two members of one pool under primary-fallback", route: []string{"p1", "p2"},
			backends: []Backend{pooled("p1", "qwen"), pooled("p2", "gemma")}},
		{name: "same pool member twice", route: []string{"p1", "p2"}, strategy: strategyWeighted,
			backends: []Backend{pooled("p1", "qwen"), pooled("p2", "qwen")}},
		{name: "pooled and unpooled", route: []string{"p1", "local-qwen"}, strategy: strategyWeighted,
			backends: []Backend{pooled("p1", "qwen")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			for _, b := range tc.backends {
				if existing := cfg.BackendByName(b.Name); existing != nil {
					existing.Weight = b.Weight
					existing.Capabilities = b.Capabilities
					continue
				}
				cfg.Backends = append(cfg.Backends, b)
			}
			cfg.Rules = append(cfg.Rules, Rule{
				Name:  "under-test",
				Match: RuleMatch{RequiredCapabilities: tc.required},
				Route: RuleRoute{Backends: tc.route, Strategy: tc.strategy},
			})
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
