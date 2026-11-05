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
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/defilantech/llmkube/internal/router/picker"
)

const testBlockBytes = 256

// prefixDispatcher is balancedDispatcher with prefix affinity on.
func prefixDispatcher(t *testing.T, headers []string, n int) (*Dispatcher, *Backend, []string) {
	t.Helper()
	d, b, eps := balancedDispatcher(headers, n)
	b.Balancing.PrefixBlockBytes = testBlockBytes
	return d, b, eps
}

// chat builds an OpenAI chat body from alternating user and assistant turns
// behind a system prompt.
func chat(t *testing.T, system string, turns ...string) []byte {
	t.Helper()
	msgs := make([]map[string]string, 0, 1+len(turns))
	msgs = append(msgs, map[string]string{"role": "system", "content": system})
	for i, c := range turns {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]string{"role": role, "content": c})
	}
	b, err := json.Marshal(map[string]any{"model": "qwen", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func equalLoad(d *Dispatcher, eps []string) {
	now := d.nowFn()
	for _, ep := range eps {
		d.RecordSample("qwen", ep, picker.Sample{Running: 1, At: now})
	}
}

var longSystem = "You are a careful coding agent. " + strings.Repeat("Follow the repository conventions exactly. ", 60)

func TestPlacePrefixFollowUpTurnReturnsToItsPod(t *testing.T) {
	d, b, eps := prefixDispatcher(t, nil, 3)
	equalLoad(d, eps)
	turn1 := chat(t, longSystem, "Fix the failing test in pkg/agent.")
	first, _, _, _ := d.place(b, http.Header{}, turn1)
	for i := 0; i < 5; i++ {
		turn := chat(t, longSystem, "Fix the failing test in pkg/agent.", "Done: it was a nil map.", strings.Repeat("Now do the same for pkg/cli. ", i+1))
		ep, res, _, tokens := d.place(b, http.Header{}, turn)
		if ep != first || res != picker.PrefixMatch {
			t.Fatalf("turn %d: place = %s/%s, want %s prefix_match", i+2, ep, res, first)
		}
		if est := picker.EstimateTokens(int64(len(turn))); tokens <= 0 || tokens >= est {
			t.Errorf("turn %d: uncached tokens = %d, want between 0 and the whole prompt (%d)", i+2, tokens, est)
		}
	}
}

func TestPlacePrefixSharedSystemPromptAcrossClients(t *testing.T) {
	d, b, eps := prefixDispatcher(t, nil, 4)
	equalLoad(d, eps)
	first, _, _, _ := d.place(b, http.Header{}, chat(t, longSystem, "user one asks about routing"))
	for i := 0; i < 6; i++ {
		ep, res, _, _ := d.place(b, http.Header{}, chat(t, longSystem, strings.Repeat("another user, another question ", i+1)))
		if ep != first || res != picker.PrefixMatch {
			t.Errorf("client %d: place = %s/%s, want the pod holding the shared system prompt (%s)", i+2, ep, res, first)
		}
	}
}

func TestPlacePrefixBusyPodStillLoses(t *testing.T) {
	d, b, eps := prefixDispatcher(t, nil, 2)
	equalLoad(d, eps)
	body := chat(t, longSystem, "first question")
	cached, _, _, _ := d.place(b, http.Header{}, body)
	other := eps[0]
	if other == cached {
		other = eps[1]
	}
	now := d.nowFn()
	d.RecordSample("qwen", cached, picker.Sample{Running: 4, At: now})
	d.RecordSample("qwen", other, picker.Sample{Running: 1, At: now})
	if ep, res, _, _ := d.place(b, http.Header{}, chat(t, longSystem, "first question", "answer", "follow-up")); ep != other || res != picker.LeastLoaded {
		t.Errorf("place = %s/%s, want the idle pod %s least_loaded: three queued requests outweigh a cached system prompt", ep, res, other)
	}
}

// With no match anywhere, a prompt goes to its first block's ring home, so two
// router replicas that never share state send the same prefix to the same pod.
func TestPlacePrefixUnmatchedGoesToTheSameHomeOnEveryReplica(t *testing.T) {
	body := chat(t, longSystem, "a brand new conversation")
	var homes []string
	for replica := 0; replica < 2; replica++ {
		d, b, eps := prefixDispatcher(t, nil, 5)
		equalLoad(d, eps)
		// Advance the round-robin cursor differently per replica: without the
		// ring home, the tie would rotate to different pods.
		for i := 0; i < replica*3; i++ {
			d.endpointCursor("qwen").Add(1)
		}
		ep, res, _, _ := d.place(b, http.Header{}, body)
		if res != picker.LeastLoaded {
			t.Fatalf("replica %d: result %s, want least_loaded for an unmatched prompt", replica, res)
		}
		homes = append(homes, ep)
	}
	if homes[0] != homes[1] {
		t.Errorf("replicas placed the same unmatched prompt on %s and %s, want one ring home", homes[0], homes[1])
	}
}

func TestPlaceWithoutPrefixAffinityIsUnchanged(t *testing.T) {
	d, b, eps := balancedDispatcher(nil, 3)
	equalLoad(d, eps)
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		ep, res, _, tokens := d.place(b, http.Header{}, chat(t, longSystem, "same question"))
		if res != picker.LeastLoaded || tokens != picker.EstimateTokens(int64(len(chat(t, longSystem, "same question")))) {
			t.Errorf("place %d = %s/%d, want least_loaded and the whole prompt uncached", i, res, tokens)
		}
		seen[ep] = true
	}
	if len(seen) != 3 {
		t.Errorf("equal loads rotated over %d pods, want all 3 (prefix affinity off must not pin a prompt)", len(seen))
	}
}

func TestPlacePrefixForgetsAPodThatLeft(t *testing.T) {
	d, b, eps := prefixDispatcher(t, nil, 2)
	equalLoad(d, eps)
	body := chat(t, longSystem, "question")
	first, _, _, _ := d.place(b, http.Header{}, body)
	var rest []string
	for _, ep := range eps {
		if ep != first {
			rest = append(rest, ep)
		}
	}
	d.SetEndpoints("qwen", rest)
	d.place(b, http.Header{}, body)
	d.SetEndpoints("qwen", eps) // a new pod at the old address holds nothing
	equalLoad(d, eps)
	if got := d.prefixFor("qwen", b.Balancing).Len(first); got != 0 {
		t.Errorf("the departed pod still holds %d blocks after it left the endpoint set", got)
	}
}

func TestPlacePrefixSessionUsesTheMeasuredUncachedTokens(t *testing.T) {
	d, b, eps := prefixDispatcher(t, []string{"x-session-affinity"}, 2)
	equalLoad(d, eps)
	h := http.Header{}
	h.Set("X-Session-Affinity", "s-1")
	d.place(b, h, chat(t, longSystem, "q1"))
	// The session's previous turn was smaller, but the prompt was edited at the
	// front: growth says almost nothing is new, the index says most of it is.
	edited := chat(t, "A different system prompt. "+longSystem, "q1", "a1", "q2")
	_, res, _, tokens := d.place(b, h, edited)
	if res != picker.Sticky {
		t.Fatalf("result %s, want sticky", res)
	}
	if est := picker.EstimateTokens(int64(len(edited))); tokens != est {
		t.Errorf("uncached tokens = %d, want the whole prompt (%d): nothing of the edited prompt is cached", tokens, est)
	}
}

// A new session is placed like a headerless request, so a pod holding its
// prompt's prefix wins and the placement says so.
func TestPlacePrefixNewSessionGoesToThePodHoldingItsPrefix(t *testing.T) {
	d, b, eps := prefixDispatcher(t, []string{"x-session-affinity"}, 3)
	equalLoad(d, eps)
	h := http.Header{}
	h.Set("X-Session-Affinity", "s-1")
	held, _, _, _ := d.place(b, h, chat(t, longSystem, "first session"))
	h.Set("X-Session-Affinity", "s-2")
	ep, res, _, _ := d.place(b, h, chat(t, longSystem, "second session, same system prompt"))
	if ep != held || res != picker.PrefixMatch {
		t.Errorf("new session placed %s/%s, want %s prefix_match", ep, res, held)
	}
}

func TestPlacePrefixCountsPromptAndCachedTokens(t *testing.T) {
	d, b, eps := prefixDispatcher(t, nil, 2)
	d.routerName = "prefix-metrics"
	equalLoad(d, eps)
	prompt := prommetrics.RouterPrefixPromptTokensTotal.WithLabelValues("prefix-metrics", "qwen")
	cached := prommetrics.RouterPrefixCachedTokensTotal.WithLabelValues("prefix-metrics", "qwen")
	p0, c0 := testutil.ToFloat64(prompt), testutil.ToFloat64(cached)
	turn1 := chat(t, longSystem, "q1")
	d.place(b, http.Header{}, turn1)
	if got := testutil.ToFloat64(cached) - c0; got != 0 {
		t.Errorf("first turn counted %v cached tokens, want 0", got)
	}
	turn2 := chat(t, longSystem, "q1", "a1", "q2")
	d.place(b, http.Header{}, turn2)
	wantPrompt := float64(picker.EstimateTokens(int64(len(turn1))) + picker.EstimateTokens(int64(len(turn2))))
	if got := testutil.ToFloat64(prompt) - p0; got != wantPrompt {
		t.Errorf("prompt tokens counted %v, want %v", got, wantPrompt)
	}
	if got := testutil.ToFloat64(cached) - c0; got <= 0 {
		t.Errorf("the follow-up turn counted %v cached tokens, want > 0", got)
	}
}

func TestPromptStream(t *testing.T) {
	spaced := []byte(`{"model":"m","messages":[ {"role": "system", "content": "sys"}, {"role":"user","content":"hi"} ],"temperature":0.2}`)
	compact := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"model":"other","temperature":1}`)
	if a, b := promptStream(spaced), promptStream(compact); string(a) != string(b) || len(a) == 0 {
		t.Errorf("whitespace, model and sampling fields changed the stream:\n%q\n%q", a, b)
	}
	turn2 := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`)
	if s1, s2 := promptStream(compact), promptStream(turn2); !strings.HasPrefix(string(s2), string(s1)) {
		t.Errorf("a follow-up turn's stream does not extend the first turn's:\n%q\n%q", s1, s2)
	}
	tools := []byte(`{"tools":[{"type":"function","function":{"name":"read"}}],"messages":[{"role":"user","content":"hi"}]}`)
	if s := string(promptStream(tools)); !strings.HasPrefix(s, "T") || !strings.Contains(s, `"name":"read"`) {
		t.Errorf("tools are not first in the stream: %q", s)
	}
	anthropic := []byte(`{"system":"be brief","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if s := string(promptStream(anthropic)); !strings.HasPrefix(s, "S") || !strings.Contains(s, `"text":"hi"`) {
		t.Errorf("an Anthropic system prompt is not first in the stream: %q", s)
	}
	completion := []byte(`{"model":"m","prompt":"Once upon a time"}`)
	if s := string(promptStream(completion)); !strings.Contains(s, "Once upon a time") {
		t.Errorf("a completion prompt is missing from the stream: %q", s)
	}
	for _, body := range [][]byte{nil, []byte(`{"input":"embed me"}`), []byte(`not json`)} {
		if s := promptStream(body); s != nil {
			t.Errorf("promptStream(%q) = %q, want nil", body, s)
		}
	}
}
