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
	"net/http"
	"testing"
)

// TestProxyPrimaryFallbackSkipsBackendWithoutRequiredCapability is the #2328
// reproduction: the text-only backend is listed first, but the rule requires
// vision, so the request must go to the backend that has it.
func TestProxyPrimaryFallbackSkipsBackendWithoutRequiredCapability(t *testing.T) {
	h := newRuleHarness(t, strategyPrimaryFallback, nil, []string{"vision"},
		weightedSpec{name: "text-only"},
		weightedSpec{name: "with-vision", caps: []string{"vision"}})
	for range 3 {
		if code := h.post(t); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	}
	if txt, vis := h.calls("text-only"), h.calls("with-vision"); txt != 0 || vis != 3 {
		t.Errorf("calls text-only=%d with-vision=%d, want 0 and 3", txt, vis)
	}
}

// TestProxyPrimaryFallbackFallsBackAmongCapableBackends: fallback still works,
// but only across the backends that have the capability; the incapable one
// is not a fallback either.
func TestProxyPrimaryFallbackFallsBackAmongCapableBackends(t *testing.T) {
	h := newRuleHarness(t, strategyPrimaryFallback, nil, []string{"vision"},
		weightedSpec{name: "vision-a", caps: []string{"vision", "tools"}},
		weightedSpec{name: "text-only"},
		weightedSpec{name: "vision-b", caps: []string{"vision"}})
	h.backs["vision-a"].status.Store(http.StatusInternalServerError)

	if code := h.post(t); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from vision-b", code)
	}
	if a, txt, b := h.calls("vision-a"), h.calls("text-only"), h.calls("vision-b"); a != 1 || txt != 0 || b != 1 {
		t.Errorf("calls vision-a=%d text-only=%d vision-b=%d, want 1, 0, 1", a, txt, b)
	}

	// With every capable backend down, the request fails rather than
	// landing on the one that cannot serve it.
	h.backs["vision-b"].status.Store(http.StatusInternalServerError)
	h.proxy.Dispatcher().MarkHealthy("vision-a")
	h.proxy.Dispatcher().MarkHealthy("vision-b")
	if code := h.post(t); code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 with every capable backend failing", code)
	}
	if txt := h.calls("text-only"); txt != 0 {
		t.Errorf("text-only called %d times, want 0", txt)
	}
}

// TestProxyRuleWithoutRequiredCapabilitiesUsesDeclaredOrder: a rule that
// requires nothing keeps today's behavior, first backend first, whatever the
// backends advertise.
func TestProxyRuleWithoutRequiredCapabilitiesUsesDeclaredOrder(t *testing.T) {
	h := newRuleHarness(t, strategyPrimaryFallback, nil, nil,
		weightedSpec{name: "plain"},
		weightedSpec{name: "fancy", caps: []string{"vision"}})
	if code := h.post(t); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if p, f := h.calls("plain"), h.calls("fancy"); p != 1 || f != 0 {
		t.Errorf("calls plain=%d fancy=%d, want 1 and 0", p, f)
	}
	h.backs["plain"].status.Store(http.StatusInternalServerError)
	if code := h.post(t); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the fallback", code)
	}
	if f := h.calls("fancy"); f != 1 {
		t.Errorf("fancy called %d times, want 1 as the fallback", f)
	}
}

// TestCapableBackendsKeepsOrderAndDuplicates pins the filter itself: declared
// order is kept, and with no required capability the input is returned as is.
func TestCapableBackendsKeepsOrderAndDuplicates(t *testing.T) {
	cfg := &Config{Backends: []Backend{
		{Name: "a", Capabilities: []string{"vision"}},
		{Name: "b"},
		{Name: "c", Capabilities: []string{"vision", "tools"}},
	}}
	m := NewMatcher(cfg)
	names := []string{"c", "b", "a", "missing", "c"}

	rule := &Rule{Match: RuleMatch{RequiredCapabilities: []string{"vision"}}}
	got := capableBackends(rule, names, m.BackendByName)
	want := []string{"c", "a", "c"}
	if len(got) != len(want) {
		t.Fatalf("capableBackends = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("capableBackends = %v, want %v", got, want)
		}
	}

	if got := capableBackends(&Rule{}, names, m.BackendByName); &got[0] != &names[0] || len(got) != len(names) {
		t.Errorf("rule without required capabilities: got %v, want the input slice unchanged", got)
	}
}
