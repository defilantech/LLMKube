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
	"time"

	"github.com/defilantech/llmkube/internal/router/picker"
)

func balancedConfig(b *Balancing, m *MetricsProfile) *Config {
	return &Config{Backends: []Backend{{
		Name: "qwen", Tier: "local", Address: "http://qwen.ns.svc.cluster.local:8000",
		EndpointSource: &EndpointSource{Namespace: "ns", Service: "qwen", Port: 8000},
		Balancing:      b, Metrics: m,
	}}}
}

func TestValidateBalancing(t *testing.T) {
	vllm := &MetricsProfile{Path: "/metrics", Running: "vllm:num_requests_running", Waiting: "vllm:num_requests_waiting", KVUsage: "vllm:kv_cache_usage_perc"}
	for _, ok := range []*Config{
		balancedConfig(&Balancing{Mode: BalancingLoadAware, SessionHeaders: []string{"x-session-affinity"}, SessionIdleTimeout: time.Hour}, vllm),
		balancedConfig(&Balancing{SessionIdleTimeout: time.Minute}, nil),
		balancedConfig(&Balancing{SessionIdleTimeout: 24 * time.Hour}, nil),
		balancedConfig(&Balancing{SessionIdleTimeout: 0}, nil),
		balancedConfig(&Balancing{Mode: BalancingRoundRobin}, nil),
		balancedConfig(&Balancing{}, nil),
		balancedConfig(&Balancing{SessionHeaders: []string{"X-Session_ID.v2", "!#$%&'*+-.^_`|~"}}, nil),
	} {
		if err := ok.Validate(); err != nil {
			t.Errorf("valid config rejected: %v", err)
		}
	}
	for name, bad := range map[string]*Config{
		"unknown mode":            balancedConfig(&Balancing{Mode: "Fastest"}, nil),
		"idle timeout 30s":        balancedConfig(&Balancing{SessionIdleTimeout: 30 * time.Second}, nil),
		"idle timeout 25h":        balancedConfig(&Balancing{SessionIdleTimeout: 25 * time.Hour}, nil),
		"idle timeout negative":   balancedConfig(&Balancing{SessionIdleTimeout: -time.Minute}, nil),
		"empty header name":       balancedConfig(&Balancing{SessionHeaders: []string{""}}, nil),
		"padded header name":      balancedConfig(&Balancing{SessionHeaders: []string{" x-foo "}}, nil),
		"header name with space":  balancedConfig(&Balancing{SessionHeaders: []string{"x session"}}, nil),
		"header name with colon":  balancedConfig(&Balancing{SessionHeaders: []string{"x-session:"}}, nil),
		"header name non-ASCII":   balancedConfig(&Balancing{SessionHeaders: []string{"x-sessión"}}, nil),
		"header name too long":    balancedConfig(&Balancing{SessionHeaders: []string{strings.Repeat("x", 257)}}, nil),
		"profile without path":    balancedConfig(&Balancing{Mode: BalancingLoadAware}, &MetricsProfile{Running: "a", Waiting: "b"}),
		"profile without waiting": balancedConfig(&Balancing{Mode: BalancingLoadAware}, &MetricsProfile{Path: "/metrics", Running: "a"}),
	} {
		err := bad.Validate()
		if err == nil || !(strings.Contains(err.Error(), "balancing") || strings.Contains(err.Error(), "metrics")) {
			t.Errorf("%s: err = %v, want a balancing or metrics error", name, err)
		}
	}
}

// TestDecodesV0107BalancingConfig: a 0.10.7 controller still writes
// maxLoadPercent. The proxy must decode it, validate it and place a session
// request, ignoring the field.
func TestDecodesV0107BalancingConfig(t *testing.T) {
	var bal Balancing
	raw := `{"mode":"LoadAware","sessionHeaders":["x-session-affinity"],"maxLoadPercent":125}`
	if err := json.Unmarshal([]byte(raw), &bal); err != nil {
		t.Fatalf("decode: %v", err)
	}
	vllm := &MetricsProfile{Path: "/metrics", Running: "vllm:num_requests_running", Waiting: "vllm:num_requests_waiting"}
	cfg := balancedConfig(&bal, vllm)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("0.10.7 config rejected: %v", err)
	}
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{"http://10.0.1.1:8000", "http://10.0.1.2:8000"})
	h := http.Header{}
	h.Set("X-Session-Affinity", "agent-1")
	ep, res, _, _ := d.place(&d.cfg.Backends[0], h, 0)
	if ep == "" || res != picker.LeastLoaded {
		t.Errorf("place = %q/%s, want a pod, least_loaded for a first request", ep, res)
	}
	if ep2, res2, _, _ := d.place(&d.cfg.Backends[0], h, 0); ep2 != ep || res2 != picker.Sticky {
		t.Errorf("second place = %q/%s, want %q sticky", ep2, res2, ep)
	}
}
