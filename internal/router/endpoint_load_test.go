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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/defilantech/llmkube/internal/router/picker"
)

func singleEndpointBackend(url string) (*Config, *Backend) {
	cfg := &Config{Backends: []Backend{{
		Name: "qwen", Tier: "local", Address: url,
		EndpointSource: &EndpointSource{Namespace: "ns", Service: "qwen", Port: 8000},
	}}}
	return cfg, &cfg.Backends[0]
}

func inflight(d *Dispatcher, ep string) int64 { return d.loadFor("qwen", ep).inflight.Load() }

func TestDispatchReleasesInFlight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	cfg, b := singleEndpointBackend(srv.URL)
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})

	resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/v1/chat/completions", http.Header{}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := inflight(d, srv.URL); got != 1 {
		t.Errorf("in-flight while the body is open = %d, want 1", got)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = resp.Body.Close() // a second Close must not release twice
	if got := inflight(d, srv.URL); got != 0 {
		t.Errorf("in-flight after Close = %d, want 0", got)
	}
}

func TestDispatchReleasesInFlightOnFailureAndCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // a stream the client abandons
	}))
	defer srv.Close()
	cfg, b := singleEndpointBackend(srv.URL)
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})

	ctx, cancel := context.WithCancel(context.Background())
	resp, err := d.Dispatch(ctx, b, http.MethodPost, "/v1/chat/completions", http.Header{}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = resp.Body.Close() // closed without reading to the end
	if got := inflight(d, srv.URL); got != 0 {
		t.Errorf("in-flight after an abandoned stream = %d, want 0", got)
	}

	dead := "http://127.0.0.1:1"
	d.SetEndpoints("qwen", []string{dead})
	if resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/x", http.Header{}, []byte(`{}`)); err == nil {
		_ = resp.Body.Close()
		t.Fatal("dispatch to a closed port succeeded")
	}
	if got := inflight(d, dead); got != 0 {
		t.Errorf("in-flight after a failed request = %d, want 0", got)
	}
}

func TestRecordSampleStampsInFlightAndClearsOnLeave(t *testing.T) {
	cfg, _ := singleEndpointBackend("http://unused")
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{"http://10.0.0.1:8000", "http://10.0.0.2:8000"})
	d.loadFor("qwen", "http://10.0.0.1:8000").inflight.Store(3)
	d.RecordSample("qwen", "http://10.0.0.1:8000", picker.Sample{Running: 2, At: time.Now()})
	s := d.loadFor("qwen", "http://10.0.0.1:8000").sample.Load()
	if s == nil || s.InFlightAt != 3 || s.Running != 2 {
		t.Fatalf("stored sample = %+v, want Running 2 and InFlightAt 3", s)
	}
	if got := d.EndpointsOf("qwen"); len(got) != 2 {
		t.Fatalf("EndpointsOf = %v, want 2 endpoints", got)
	}
	d.SetEndpoints("qwen", []string{"http://10.0.0.2:8000"})
	if s := d.loadFor("qwen", "http://10.0.0.1:8000").sample.Load(); s != nil {
		t.Errorf("a departed endpoint kept its sample: %+v", s)
	}
}

func loadAwareBackend(url string) (*Config, *Backend) {
	cfg, b := singleEndpointBackend(url)
	b.Balancing = &Balancing{Mode: BalancingLoadAware}
	return cfg, b
}

func prefill(d *Dispatcher, ep string) int64 { return d.loadFor("qwen", ep).prefill.Load() }

// promptBody returns a valid chat body of exactly 4,000 bytes, which
// estimates to 1,000 prompt tokens.
func promptBody() []byte {
	head, tail := `{"model":"qwen","messages":[{"role":"user","content":"`, `"}]}`
	return []byte(head + strings.Repeat("a", 4000-len(head)-len(tail)) + tail)
}

func TestDispatchHoldsPrefillUntilFirstByte(t *testing.T) {
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // headers go out before prefill, as vLLM's do
		<-unblock
		_, _ = io.WriteString(w, "data: {}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	var once sync.Once
	finish := func() { once.Do(func() { close(unblock) }) }
	defer finish() // a failed assertion must not leave the handler blocked
	cfg, b := loadAwareBackend(srv.URL)
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})

	resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/v1/chat/completions", http.Header{}, promptBody())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := prefill(d, srv.URL); got != 1000 {
		t.Fatalf("prefill after headers, before the first byte = %d, want 1000", got)
	}
	finish()
	if n, err := resp.Body.Read(make([]byte, 64)); n == 0 {
		t.Fatalf("first read returned no bytes: %v", err)
	}
	if got := prefill(d, srv.URL); got != 0 {
		t.Errorf("prefill after the first byte = %d, want 0", got)
	}
}

func TestDispatchReleasesPrefillOnCloseWithoutRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	cfg, b := loadAwareBackend(srv.URL)
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})

	resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/v1/chat/completions", http.Header{}, promptBody())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	_ = resp.Body.Close()
	if got := prefill(d, srv.URL); got != 0 {
		t.Errorf("prefill after Close = %d, want 0", got)
	}
}

func TestDispatchReleasesPrefillOnFailures(t *testing.T) {
	body := promptBody()

	dead := "http://127.0.0.1:1"
	cfg, b := loadAwareBackend(dead)
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{dead})
	if resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/x", http.Header{}, body); err == nil {
		_ = resp.Body.Close()
		t.Fatal("dispatch to a closed port succeeded")
	}
	if got := prefill(d, dead); got != 0 {
		t.Errorf("prefill after a connect error = %d, want 0", got)
	}

	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer errSrv.Close()
	cfg, b = loadAwareBackend(errSrv.URL)
	d = NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{errSrv.URL})
	resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/x", http.Header{}, body)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if got := prefill(d, errSrv.URL); got != 0 {
		t.Errorf("prefill after a 5xx = %d, want 0", got)
	}

	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the client leave
		<-r.Context().Done()               // never sends headers
	}))
	defer hang.Close()
	cfg, b = loadAwareBackend(hang.URL)
	d = NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{hang.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if resp, err := d.Dispatch(ctx, b, http.MethodPost, "/x", http.Header{}, body); err == nil {
		_ = resp.Body.Close()
		t.Fatal("dispatch past its deadline succeeded")
	}
	if got := prefill(d, hang.URL); got != 0 {
		t.Errorf("prefill after a cancel before headers = %d, want 0", got)
	}
}

func TestDispatchRoundRobinCountsNoPrefill(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	cfg, b := singleEndpointBackend(srv.URL)
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})
	resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/v1/chat/completions", http.Header{}, promptBody())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := prefill(d, srv.URL); got != 0 {
		t.Errorf("round-robin prefill = %d, want 0", got)
	}
	if got := d.ExpirePrefill(time.Now().Add(24 * time.Hour)); got != 0 {
		t.Errorf("round-robin ledger entries = %d, want 0", got)
	}
}

func TestDispatchEmptyBodyCountsNoPrefill(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	cfg, b := loadAwareBackend(srv.URL)
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})
	resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/v1/chat/completions", http.Header{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := prefill(d, srv.URL); got != 0 {
		t.Errorf("empty-body prefill = %d, want 0", got)
	}
	if got := d.ExpirePrefill(time.Now().Add(24 * time.Hour)); got != 0 {
		t.Errorf("empty-body ledger entries = %d, want 0", got)
	}
}

func TestPrefillReleasesAfterEndpointLeaves(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	cfg, b := loadAwareBackend(srv.URL)
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})
	resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/v1/chat/completions", http.Header{}, promptBody())
	if err != nil {
		t.Fatal(err)
	}
	held := d.loadFor("qwen", srv.URL)
	d.SetEndpoints("qwen", nil)
	_ = resp.Body.Close()
	if got := held.prefill.Load(); got != 0 {
		t.Errorf("prefill on the departed endpoint's record = %d, want 0", got)
	}
	if got := prefill(d, srv.URL); got < 0 {
		t.Errorf("prefill went negative: %d", got)
	}
}

func TestPrefillJanitorExpiresOnlyOldEntries(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cfg, _ := loadAwareBackend("http://unused")
	d := NewDispatcher(cfg, WithNowFunc(func() time.Time { return now }))
	releaseOld := d.trackPrefill("qwen", "ep", 1000)
	start := now
	now = now.Add(10 * time.Minute)
	releaseNew := d.trackPrefill("qwen", "ep", 2000)
	defer releaseNew()

	if got := d.ExpirePrefill(start.Add(d.prefillTTL() + time.Second)); got != 1 {
		t.Fatalf("expired = %d, want 1 (only the old entry)", got)
	}
	if got := d.loadFor("qwen", "ep").prefill.Load(); got != 2000 {
		t.Fatalf("prefill after expiry = %d, want 2000", got)
	}
	releaseOld() // the real release after the janitor's must not subtract again
	if got := d.loadFor("qwen", "ep").prefill.Load(); got != 2000 {
		t.Errorf("prefill after a late release = %d, want 2000", got)
	}
}

func TestPrefillJanitorTTLWithIdleGuardOff(t *testing.T) {
	cfg, _ := loadAwareBackend("http://unused")
	d := NewDispatcher(cfg, WithStreamIdleTimeout(0))
	if got, want := d.prefillTTL(), defaultResponseHeaderTimeout+defaultStreamIdleTimeout; got != want {
		t.Errorf("prefillTTL with the idle guard off = %s, want %s", got, want)
	}
}

func TestPrefillConcurrentDispatchEndsAtZero(t *testing.T) {
	cfg, _ := loadAwareBackend("http://unused")
	d := NewDispatcher(cfg)
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			release := d.trackPrefill("qwen", "ep", 1000)
			release()
			if i%2 == 0 {
				release()
			}
		}(i)
	}
	wg.Wait()
	if got := d.loadFor("qwen", "ep").prefill.Load(); got != 0 {
		t.Errorf("prefill after 1000 concurrent dispatches = %d, want 0", got)
	}
	if got := d.ExpirePrefill(time.Now().Add(24 * time.Hour)); got != 0 {
		t.Errorf("ledger entries left = %d, want 0", got)
	}
}

func TestTrackPrefillCountsEstimatedTokens(t *testing.T) {
	cfg, _ := loadAwareBackend("http://unused")
	d := NewDispatcher(cfg, WithRouterLabel("r-count"))
	c := prommetrics.RouterPrefillTokensEstimatedTotal.WithLabelValues("r-count", "qwen", "ep")
	before := testutil.ToFloat64(c)
	d.trackPrefill("qwen", "ep", 1000)()
	d.trackPrefill("qwen", "ep", 2000)()
	d.trackPrefill("qwen", "ep", 0)()
	if got := testutil.ToFloat64(c) - before; got != 3000 {
		t.Errorf("estimated-tokens counter rose by %v, want 3000", got)
	}
}

// A LoadAware backend without a metrics profile has no scraper, so the
// departure path must drop the estimated-tokens series itself.
func TestSetEndpointsDropsDepartedPrefillCounterSeries(t *testing.T) {
	cfg, _ := loadAwareBackend("http://unused")
	d := NewDispatcher(cfg, WithRouterLabel("r-leave"))
	d.SetEndpoints("qwen", []string{"http://10.0.0.1:8000", "http://10.0.0.2:8000"})
	d.trackPrefill("qwen", "http://10.0.0.1:8000", 1000)()
	d.trackPrefill("qwen", "http://10.0.0.2:8000", 1000)()
	d.SetEndpoints("qwen", []string{"http://10.0.0.2:8000"})
	if prommetrics.RouterPrefillTokensEstimatedTotal.DeleteLabelValues("r-leave", "qwen", "http://10.0.0.1:8000") {
		t.Error("estimated-tokens series of the departed endpoint survived")
	}
	if !prommetrics.RouterPrefillTokensEstimatedTotal.DeleteLabelValues("r-leave", "qwen", "http://10.0.0.2:8000") {
		t.Error("estimated-tokens series of the kept endpoint is missing")
	}
}
