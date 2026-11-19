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
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

// syncWarnAfter is how long Start waits for the first sync before logging that
// the watch is not ready (typically missing RBAC); it keeps waiting after that.
const syncWarnAfter = 30 * time.Second

type watchedBackend struct {
	name string
	port int32
}

// EndpointWatcher keeps the dispatcher's endpoint set for every EndpointSource
// backend in step with its Service's EndpointSlices. It runs one informer per
// namespace, filtered to the Services the config names.
type EndpointWatcher struct {
	cs         kubernetes.Interface
	disp       *Dispatcher
	routerName string
	logger     *slog.Logger
	// byService maps "namespace/service" to the backends that use it.
	byService map[string][]watchedBackend
	// services lists the Service names per namespace.
	services map[string][]string
	listers  map[string]discoverylisters.EndpointSliceLister
	// mu serializes recompute so a stale list never overwrites a newer one.
	mu sync.Mutex
}

// NewEndpointWatcher indexes cfg's EndpointSource backends. It starts nothing.
func NewEndpointWatcher(cs kubernetes.Interface, cfg *Config, disp *Dispatcher, routerName string, logger *slog.Logger) *EndpointWatcher {
	w := &EndpointWatcher{
		cs: cs, disp: disp, routerName: routerName, logger: logger,
		byService: map[string][]watchedBackend{},
		services:  map[string][]string{},
		listers:   map[string]discoverylisters.EndpointSliceLister{},
	}
	for _, b := range cfg.Backends {
		s := b.EndpointSource
		if s == nil {
			continue
		}
		key := s.Namespace + "/" + s.Service
		if _, seen := w.byService[key]; !seen {
			w.services[s.Namespace] = append(w.services[s.Namespace], s.Service)
		}
		w.byService[key] = append(w.byService[key], watchedBackend{name: b.Name, port: s.Port})
		prommetrics.RouterEndpointSourceReady.WithLabelValues(routerName, b.Name).Set(0)
	}
	return w
}

// HasSources reports whether any backend needs a watch.
func (w *EndpointWatcher) HasSources() bool { return len(w.byService) > 0 }

// Start runs the informers until ctx ends. It never returns an error: a watch
// that cannot sync leaves dispatch on each backend's Address, and the
// endpoint_source_ready gauge stays 0.
func (w *EndpointWatcher) Start(ctx context.Context) {
	var synced []cache.InformerSynced
	var factories []informers.SharedInformerFactory
	for ns, svcs := range w.services {
		sort.Strings(svcs)
		selector := discoveryv1.LabelServiceName + " in (" + strings.Join(svcs, ",") + ")"
		factory := informers.NewSharedInformerFactoryWithOptions(w.cs, 0,
			informers.WithNamespace(ns),
			informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = selector }))
		inf := factory.Discovery().V1().EndpointSlices()
		w.listers[ns] = inf.Lister()
		if _, err := inf.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    w.onEvent,
			UpdateFunc: func(_, obj interface{}) { w.onEvent(obj) },
			DeleteFunc: w.onEvent,
		}); err != nil {
			w.logger.Error("endpoint watch: add handler", "namespace", ns, "error", err)
			continue
		}
		synced = append(synced, inf.Informer().HasSynced)
		factories = append(factories, factory)
	}
	// Start only after every lister is registered: handlers read w.listers.
	for _, f := range factories {
		f.Start(ctx.Done())
	}

	warn := time.AfterFunc(syncWarnAfter, func() {
		w.logger.Warn("endpoint watch has not synced; dispatching to Service addresses (check the proxy's RBAC for endpointslices)")
	})
	defer warn.Stop()
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return
	}
	warn.Stop()
	for key := range w.byService {
		ns, svc, _ := strings.Cut(key, "/")
		w.recompute(ns, svc)
		for _, b := range w.byService[key] {
			prommetrics.RouterEndpointSourceReady.WithLabelValues(w.routerName, b.name).Set(1)
		}
	}
	<-ctx.Done()
}

// onEvent recomputes the set of the Service an EndpointSlice event belongs to.
func (w *EndpointWatcher) onEvent(obj interface{}) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	s, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return
	}
	w.recompute(s.Namespace, s.Labels[discoveryv1.LabelServiceName])
}

// recompute rebuilds the ready set of one Service from the lister and hands it
// to every backend that uses the Service. Before an informer exists for the
// namespace (or for an unknown Service) it hands out an empty set only to the
// Service's own backends.
func (w *EndpointWatcher) recompute(namespace, service string) {
	backends := w.byService[namespace+"/"+service]
	if len(backends) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var slices []*discoveryv1.EndpointSlice
	if lister, ok := w.listers[namespace]; ok {
		sel := labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: service})
		var err error
		if slices, err = lister.EndpointSlices(namespace).List(sel); err != nil {
			w.logger.Error("endpoint watch: list from cache", "service", service, "error", err)
			return
		}
	}
	for _, b := range backends {
		w.disp.SetReadyEndpoints(b.name, ReadyEndpoints(slices, b.port))
	}
}
