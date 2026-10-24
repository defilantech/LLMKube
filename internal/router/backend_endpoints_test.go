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
	"reflect"
	"testing"
	"time"
)

// pickEndpoint places a request that carries no headers, so the tests can
// read the chosen pod directly.
func (d *Dispatcher) pickEndpoint(backend *Backend) string {
	ep, _, _, _ := d.place(backend, nil, 0)
	return ep
}

func endpointSourceConfig() *Config {
	return &Config{Backends: []Backend{{
		Name: "qwen", Tier: "local", Address: "http://qwen.ns.svc.cluster.local:8080",
		EndpointSource: &EndpointSource{Namespace: "ns", Service: "qwen", Port: 8080},
	}}}
}

func TestSetEndpointsDrivesPick(t *testing.T) {
	cfg := endpointSourceConfig()
	d := NewDispatcher(cfg)
	b := &cfg.Backends[0]

	if got := d.pickEndpoint(b); got != "" {
		t.Fatalf("before any set: pick = %q, want empty (Address fallback)", got)
	}

	d.SetEndpoints("qwen", []string{"http://10.0.0.2:8080", "http://10.0.0.1:8080"})
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		seen[d.pickEndpoint(b)] = true
	}
	if !seen["http://10.0.0.1:8080"] || !seen["http://10.0.0.2:8080"] || len(seen) != 2 {
		t.Errorf("picks = %v, want both live endpoints", seen)
	}

	d.SetEndpoints("qwen", nil)
	if got := d.pickEndpoint(b); got != "" {
		t.Errorf("after the set empties: pick = %q, want empty (Address fallback)", got)
	}
	if !d.IsHealthy("qwen") {
		t.Error("an empty set must fall back to the backend-level health, which is healthy")
	}
}

func TestSetEndpointsSortsAndDropsLeftHealth(t *testing.T) {
	d := NewDispatcher(endpointSourceConfig(), WithQuarantineDuration(time.Hour))
	d.SetEndpoints("qwen", []string{"http://10.0.0.2:8080", "http://10.0.0.1:8080"})
	v, _ := d.endpoints.Load("qwen")
	if got := v.([]string); !reflect.DeepEqual(got, []string{"http://10.0.0.1:8080", "http://10.0.0.2:8080"}) {
		t.Errorf("stored set = %v, want it sorted", got)
	}

	// Quarantine one pod, let it leave, then return with the same IP: it must
	// start healthy, not inherit the old quarantine.
	key := healthKey("qwen", "http://10.0.0.2:8080")
	h := d.healthFor(key)
	h.healthy.Store(false)
	h.quarantineUntil.Store(time.Now().Add(time.Hour).UnixNano())
	d.SetEndpoints("qwen", []string{"http://10.0.0.1:8080"})
	if _, ok := d.health.Load(key); ok {
		t.Error("health entry of a departed endpoint was kept")
	}
	d.SetEndpoints("qwen", []string{"http://10.0.0.1:8080", "http://10.0.0.2:8080"})
	if !d.dispatchable(key) {
		t.Error("a returning endpoint inherited the old quarantine")
	}
}

func TestEndpointSourceWinsOverStaticEndpoints(t *testing.T) {
	cfg := endpointSourceConfig()
	cfg.Backends[0].Endpoints = []string{"http://10.9.9.9:8080"}
	d := NewDispatcher(cfg)
	if got := d.pickEndpoint(&cfg.Backends[0]); got != "" {
		t.Errorf("before sync: pick = %q, want empty (Address fallback), the static list must not seed an EndpointSource backend", got)
	}
}
