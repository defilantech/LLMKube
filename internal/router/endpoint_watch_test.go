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
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

func watchedSlice(name, svc string, ready bool, addrs ...string) *discoveryv1.EndpointSlice {
	s := slice(&ready, addrs...)
	s.ObjectMeta = metav1.ObjectMeta{Name: name, Namespace: "ns",
		Labels: map[string]string{discoveryv1.LabelServiceName: svc}}
	s.AddressType = discoveryv1.AddressTypeIPv4
	return s
}

func twoBackendsOneService() *Config {
	src := &EndpointSource{Namespace: "ns", Service: "qwen", Port: 8080}
	return &Config{Backends: []Backend{
		{Name: "a", Tier: "local", Address: "http://qwen.ns.svc.cluster.local:8080", EndpointSource: src},
		{Name: "b", Tier: "local", Address: "http://qwen.ns.svc.cluster.local:8080", EndpointSource: src},
		{Name: "plain", Tier: "local", Address: "http://other.ns.svc.cluster.local:8080"},
	}}
}

func waitForSet(t *testing.T, d *Dispatcher, backend string, want []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got []string
		if v, ok := d.endpoints.Load(backend); ok {
			got = v.([]string)
		}
		if reflect.DeepEqual(got, want) || (len(got) == 0 && len(want) == 0) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	v, _ := d.endpoints.Load(backend)
	t.Fatalf("backend %s endpoints = %v, want %v", backend, v, want)
}

func TestEndpointWatcherFollowsSlices(t *testing.T) {
	cfg := twoBackendsOneService()
	d := NewDispatcher(cfg)
	cs := fake.NewClientset(watchedSlice("qwen-1", "qwen", true, "10.0.0.1"))
	w := NewEndpointWatcher(cs, cfg, d, "r", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !w.HasSources() {
		t.Fatal("HasSources = false for a config with EndpointSource backends")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)

	// Both backends on the Service get the set; the plain backend gets nothing.
	waitForSet(t, d, "a", []string{"http://10.0.0.1:8080"})
	waitForSet(t, d, "b", []string{"http://10.0.0.1:8080"})
	if _, ok := d.endpoints.Load("plain"); ok {
		t.Error("a backend without an EndpointSource received endpoints")
	}

	// A second slice adds a pod; an unready pod is excluded.
	if _, err := cs.DiscoveryV1().EndpointSlices("ns").Create(ctx,
		watchedSlice("qwen-2", "qwen", true, "10.0.0.2"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitForSet(t, d, "a", []string{"http://10.0.0.1:8080", "http://10.0.0.2:8080"})
	if _, err := cs.DiscoveryV1().EndpointSlices("ns").Update(ctx,
		watchedSlice("qwen-2", "qwen", false, "10.0.0.2"), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitForSet(t, d, "a", []string{"http://10.0.0.1:8080"})

	// Deleting every slice empties the set (Address fallback).
	for _, n := range []string{"qwen-1", "qwen-2"} {
		if err := cs.DiscoveryV1().EndpointSlices("ns").Delete(ctx, n, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	waitForSet(t, d, "b", nil)

	// The gauge flips after WaitForCacheSync returns, which polls; allow for it.
	g := prommetrics.RouterEndpointSourceReady.WithLabelValues("r", "a")
	deadline := time.Now().Add(5 * time.Second)
	for testutil.ToFloat64(g) != 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := testutil.ToFloat64(g); got != 1 {
		t.Errorf("endpoint_source_ready = %v, want 1 once synced", got)
	}
}

func TestEndpointWatcherHandlesTombstone(t *testing.T) {
	cfg := twoBackendsOneService()
	d := NewDispatcher(cfg)
	w := NewEndpointWatcher(fake.NewClientset(), cfg, d, "r", slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.SetEndpoints("a", []string{"http://10.0.0.9:8080"})
	// No informer running: the lister is empty, so a recompute empties the set.
	w.onEvent(cacheTombstone(watchedSlice("qwen-1", "qwen", true, "10.0.0.9")))
	waitForSet(t, d, "a", nil)
}

func TestEndpointWatcherForbiddenKeepsAddress(t *testing.T) {
	cfg := twoBackendsOneService()
	d := NewDispatcher(cfg)
	cs := fake.NewClientset()
	cs.PrependReactor("list", "endpointslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "discovery.k8s.io", Resource: "endpointslices"}, "", fmt.Errorf("no RBAC"))
	})
	w := NewEndpointWatcher(cs, cfg, d, "r-forbidden", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	w.Start(ctx) // returns when ctx ends; must not panic or exit

	if got := d.pickEndpoint(&cfg.Backends[0]); got != "" {
		t.Errorf("pick = %q, want empty so dispatch uses Address", got)
	}
	if got := testutil.ToFloat64(prommetrics.RouterEndpointSourceReady.WithLabelValues("r-forbidden", "a")); got != 0 {
		t.Errorf("endpoint_source_ready = %v, want 0 while the watch cannot sync", got)
	}
}

func cacheTombstone(obj *discoveryv1.EndpointSlice) interface{} {
	return cache.DeletedFinalStateUnknown{Key: obj.Namespace + "/" + obj.Name, Obj: obj}
}

func TestEndpointWatcherTwoNamespaces(t *testing.T) {
	cfg := &Config{Backends: []Backend{
		{Name: "a", Tier: "local", Address: "http://x", EndpointSource: &EndpointSource{Namespace: "ns-a", Service: "qwen", Port: 8080}},
		{Name: "b", Tier: "local", Address: "http://y", EndpointSource: &EndpointSource{Namespace: "ns-b", Service: "qwen", Port: 9090}},
	}}
	d := NewDispatcher(cfg)
	sa := watchedSlice("qwen-a", "qwen", true, "10.0.1.1")
	sa.Namespace = "ns-a"
	sb := watchedSlice("qwen-b", "qwen", true, "10.0.2.2")
	sb.Namespace = "ns-b"
	w := NewEndpointWatcher(fake.NewClientset(sa, sb), cfg, d, "r-two", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)
	waitForSet(t, d, "a", []string{"http://10.0.1.1:8080"})
	waitForSet(t, d, "b", []string{"http://10.0.2.2:9090"})
}

func TestEndpointWatcherIgnoresUnwatchedService(t *testing.T) {
	cfg := twoBackendsOneService()
	d := NewDispatcher(cfg)
	cs := fake.NewClientset(watchedSlice("qwen-1", "qwen", true, "10.0.0.1"))
	w := NewEndpointWatcher(cs, cfg, d, "r-unwatched", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)
	waitForSet(t, d, "a", []string{"http://10.0.0.1:8080"})

	if _, err := cs.DiscoveryV1().EndpointSlices("ns").Create(ctx,
		watchedSlice("other-1", "other", true, "10.9.9.9"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// A later event for the watched Service proves the other slice was processed first.
	if _, err := cs.DiscoveryV1().EndpointSlices("ns").Create(ctx,
		watchedSlice("qwen-2", "qwen", true, "10.0.0.2"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitForSet(t, d, "a", []string{"http://10.0.0.1:8080", "http://10.0.0.2:8080"})
	waitForSet(t, d, "b", []string{"http://10.0.0.1:8080", "http://10.0.0.2:8080"})
	if _, ok := d.endpoints.Load("plain"); ok {
		t.Error("plain backend received endpoints")
	}
}

func TestEndpointWatcherLabelsEndpointsWithPodAndNode(t *testing.T) {
	cfg := twoBackendsOneService()
	d := NewDispatcher(cfg, WithRouterLabel("r-watch-target"))
	s := watchedSlice("qwen-1", "qwen", true, "10.0.0.1")
	node := "gpu-1"
	s.Endpoints[0].TargetRef = &corev1.ObjectReference{Kind: "Pod", Namespace: "ns", Name: "qwen-abc"}
	s.Endpoints[0].NodeName = &node
	w := NewEndpointWatcher(fake.NewClientset(s), cfg, d, "r-watch-target", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)
	waitForSet(t, d, "a", []string{"http://10.0.0.1:8080"})

	d.trackPrefill("a", "http://10.0.0.1:8080", 10)()
	c := prommetrics.RouterPrefillTokensEstimatedTotal.WithLabelValues("r-watch-target", "a", "http://10.0.0.1:8080", "qwen-abc", "gpu-1")
	if got := testutil.ToFloat64(c); got != 10 {
		t.Errorf("prefill counter under the slice's pod and node = %v, want 10", got)
	}
}
