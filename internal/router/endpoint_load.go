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
	"io"
	"sync"
	"sync/atomic"
	"time"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/defilantech/llmkube/internal/router/picker"
)

// endpointLoad is what placement reads for one endpoint of one backend.
type endpointLoad struct {
	inflight atomic.Int64
	sample   atomic.Pointer[picker.Sample]
	// prefill is the estimated uncached prompt tokens this replica has sent
	// to the endpoint and the endpoint has not started answering.
	prefill atomic.Int64
}

// prefillEntry is one unreleased prefill count, for the janitor.
type prefillEntry struct {
	dispatchedAt time.Time
	release      func()
}

// loadFor returns the load record for a backend's endpoint, creating it.
func (d *Dispatcher) loadFor(backend, endpoint string) *endpointLoad {
	key := healthKey(backend, endpoint)
	if v, ok := d.loads.Load(key); ok {
		return v.(*endpointLoad)
	}
	actual, _ := d.loads.LoadOrStore(key, &endpointLoad{})
	return actual.(*endpointLoad)
}

// RecordSample stores an engine reading for a backend's endpoint, stamped
// with this replica's in-flight count at the moment it is stored.
func (d *Dispatcher) RecordSample(backend, endpoint string, s picker.Sample) {
	l := d.loadFor(backend, endpoint)
	s.InFlightAt = l.inflight.Load()
	l.sample.Store(&s)
}

// EndpointsOf returns the live endpoint set of a backend (nil when none).
func (d *Dispatcher) EndpointsOf(backend string) []string {
	if v, ok := d.endpoints.Load(backend); ok {
		return v.([]string)
	}
	return nil
}

// track counts a request against an endpoint and returns the release
// function, which is safe to call more than once.
func (d *Dispatcher) track(backend, endpoint string) func() {
	if endpoint == "" {
		return func() {}
	}
	l := d.loadFor(backend, endpoint)
	l.inflight.Add(1)
	var once sync.Once
	return func() { once.Do(func() { l.inflight.Add(-1) }) }
}

// releaseOnCloseBody releases an in-flight count when the body is closed.
type releaseOnCloseBody struct {
	io.ReadCloser
	release func()
}

func (b *releaseOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

// trackPrefill counts a request's estimated uncached prompt tokens against an
// endpoint and returns the release function, which is safe to call more than
// once. It counts nothing for an empty endpoint or zero tokens.
func (d *Dispatcher) trackPrefill(backend, endpoint string, tokens int64) func() {
	if endpoint == "" || tokens <= 0 {
		return func() {}
	}
	l := d.loadFor(backend, endpoint)
	l.prefill.Add(tokens)
	prommetrics.RouterPrefillTokensEstimatedTotal.WithLabelValues(d.routerName, backend, endpoint).Add(float64(tokens))

	d.prefillMu.Lock()
	if d.prefillLedger == nil {
		d.prefillLedger = make(map[uint64]prefillEntry)
	}
	d.prefillSeq++
	id := d.prefillSeq
	var once sync.Once
	release := func() {
		once.Do(func() {
			l.prefill.Add(-tokens)
			d.prefillMu.Lock()
			delete(d.prefillLedger, id)
			d.prefillMu.Unlock()
		})
	}
	d.prefillLedger[id] = prefillEntry{dispatchedAt: d.nowFn(), release: release}
	d.prefillMu.Unlock()
	return release
}

// prefillTTL is how long a prefill count may stay unreleased before the
// janitor releases it: past it the idle guard has already ended the body.
func (d *Dispatcher) prefillTTL() time.Duration {
	idle := d.streamIdleTimeout
	if idle <= 0 {
		idle = defaultStreamIdleTimeout
	}
	return d.responseHeaderTimeout + idle
}

// ExpirePrefill releases every prefill count older than prefillTTL and
// returns how many it released.
func (d *Dispatcher) ExpirePrefill(now time.Time) int {
	ttl := d.prefillTTL()
	var expired []func()
	d.prefillMu.Lock()
	for _, e := range d.prefillLedger {
		if now.Sub(e.dispatchedAt) > ttl {
			expired = append(expired, e.release)
		}
	}
	d.prefillMu.Unlock()
	for _, release := range expired {
		release()
	}
	return len(expired)
}

// RunPrefillJanitor expires stale prefill counts every interval until ctx is
// done.
func (d *Dispatcher) RunPrefillJanitor(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.ExpirePrefill(d.nowFn())
		}
	}
}

// firstByteBody releases a prefill count when the endpoint sends its first
// response byte, which for a streaming engine means prefill is done, or when
// the body is closed first.
type firstByteBody struct {
	io.ReadCloser
	release func()
}

func (b *firstByteBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.release()
	}
	return n, err
}

func (b *firstByteBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}
