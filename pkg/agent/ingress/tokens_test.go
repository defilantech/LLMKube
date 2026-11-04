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

package ingress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

type fakeSecrets struct {
	mu     sync.Mutex
	tokens map[string]string // namespace -> token; absent = NotFound
	// unmanaged marks namespaces whose Secret lacks the controller's
	// managed-by label; every other Secret carries it, as the controller
	// creates them.
	unmanaged map[string]bool
	calls     map[string]int
	errs      map[string][]error // namespace -> queued errors, returned before tokens
	// maxDeadline records the longest remaining deadline seen on a Get ctx;
	// noDeadline is set if any Get ctx had none.
	maxDeadline time.Duration
	noDeadline  bool
	// hook, when set, runs at the start of every Get outside mu (to block
	// or to advance the fake clock). inFlight/maxInFlight count Gets that
	// are inside the call.
	hook        func()
	inFlight    int
	maxInFlight int
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{
		tokens: map[string]string{}, unmanaged: map[string]bool{},
		calls: map[string]int{}, errs: map[string][]error{},
	}
}

func (f *fakeSecrets) setUnmanaged(ns string, unmanaged bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmanaged[ns] = unmanaged
}

func (f *fakeSecrets) set(ns, tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[ns] = tok
}

func (f *fakeSecrets) count(ns string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[ns]
}

func (f *fakeSecrets) failNext(ns string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[ns] = append(f.errs[ns], err)
}

func (f *fakeSecrets) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func (f *fakeSecrets) setHook(h func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = h
}

func (f *fakeSecrets) Get(ctx context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	f.mu.Lock()
	f.calls[key.Namespace]++
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
	if dl, ok := ctx.Deadline(); ok {
		f.maxDeadline = max(f.maxDeadline, time.Until(dl))
	} else {
		f.noDeadline = true
	}
	// Behave like a real client: a done context fails the call.
	if err := ctx.Err(); err != nil {
		return err
	}
	if q := f.errs[key.Namespace]; len(q) > 0 {
		f.errs[key.Namespace] = q[1:]
		return q[0]
	}
	if key.Name != inferencev1alpha1.MetalRelaySecretName {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
	}
	tok, ok := f.tokens[key.Namespace]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
	}
	s := obj.(*corev1.Secret)
	s.Namespace = key.Namespace
	s.Name = key.Name
	s.Data = map[string][]byte{inferencev1alpha1.MetalRelaySecretKey: []byte(tok)}
	if !f.unmanaged[key.Namespace] {
		s.Labels = map[string]string{inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByController}
	}
	return nil
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const (
	testTTL   = 5 * time.Second
	testGrace = 10 * time.Minute
	tokA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokB      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func newTestStore() (*TokenStore, *fakeSecrets, *fakeClock) {
	f := newFakeSecrets()
	clk := &fakeClock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	return NewTokenStore(f, testTTL, testGrace, clk.now), f, clk
}

func TestTokenStoreBasics(t *testing.T) {
	ctx := context.Background()
	s, f, _ := newTestStore()
	f.set("a", tokA)
	f.set("b", tokB)
	f.set("empty", "")

	cases := []struct {
		name, ns, presented string
		want                bool
	}{
		{"valid", "a", tokA, true},
		{"wrong token", "a", tokB[:63] + "c", false},
		{"empty presented", "a", "", false},
		{"missing secret", "nosecret", tokA, false},
		{"empty token in secret", "empty", "", false},
		{"empty token in secret, nonempty presented", "empty", tokA, false},
		{"cross-namespace token", "b", tokA, false},
		{"prefix of valid token", "a", tokA[:32], false},
		{"empty namespace", "", tokA, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.Valid(ctx, tc.ns, tc.presented); got != tc.want {
				t.Errorf("Valid(%q, ...) = %v, want %v", tc.ns, got, tc.want)
			}
		})
	}
}

func TestTokenStoreRotationGrace(t *testing.T) {
	ctx := context.Background()
	s, f, clk := newTestStore()
	f.set("a", tokA)

	if !s.Valid(ctx, "a", tokA) {
		t.Fatal("initial token should be valid")
	}

	f.set("a", tokB)
	// Within ttl the cached value is still served: new token not yet known.
	if s.Valid(ctx, "a", tokB) {
		t.Error("new token must not be valid before ttl elapses (cached)")
	}
	if !s.Valid(ctx, "a", tokA) {
		t.Error("old token must remain valid within ttl")
	}

	clk.advance(testTTL)
	if !s.Valid(ctx, "a", tokB) {
		t.Error("new token should be valid after refresh")
	}
	if !s.Valid(ctx, "a", tokA) {
		t.Error("old token should be valid within grace")
	}

	clk.advance(testGrace - time.Second)
	if !s.Valid(ctx, "a", tokA) {
		t.Error("old token should still be valid just before grace ends")
	}
	if !s.Valid(ctx, "a", tokB) {
		t.Error("new token should be valid")
	}

	clk.advance(time.Second)
	if s.Valid(ctx, "a", tokA) {
		t.Error("old token must be invalid once grace elapses")
	}
	if !s.Valid(ctx, "a", tokB) {
		t.Error("new token should remain valid")
	}
}

func TestTokenStoreDeletedSecretRevokes(t *testing.T) {
	ctx := context.Background()
	s, f, clk := newTestStore()
	f.set("a", tokA)
	if !s.Valid(ctx, "a", tokA) {
		t.Fatal("initial token should be valid")
	}
	f.mu.Lock()
	delete(f.tokens, "a")
	f.mu.Unlock()
	clk.advance(testTTL)
	if s.Valid(ctx, "a", tokA) {
		t.Error("token must be invalid once its Secret is gone")
	}
}

func TestTokenStoreGetAtMostOncePerTTL(t *testing.T) {
	ctx := context.Background()
	s, f, clk := newTestStore()
	f.set("a", tokA)
	f.set("b", tokB)

	for range 50 {
		s.Valid(ctx, "a", tokA)
		s.Valid(ctx, "a", "wrong")
		s.Valid(ctx, "b", tokB)
		s.Valid(ctx, "nosecret", tokA)
	}
	if got := f.count("a"); got != 1 {
		t.Errorf("Get calls for a = %d, want 1", got)
	}
	if got := f.count("b"); got != 1 {
		t.Errorf("Get calls for b = %d, want 1", got)
	}
	if got := f.count("nosecret"); got != 1 {
		t.Errorf("Get calls for missing-secret namespace = %d, want 1 (negative results cached)", got)
	}

	clk.advance(testTTL - time.Nanosecond)
	s.Valid(ctx, "a", tokA)
	if got := f.count("a"); got != 1 {
		t.Errorf("Get calls for a before ttl = %d, want 1", got)
	}
	clk.advance(time.Nanosecond)
	for range 10 {
		s.Valid(ctx, "a", tokA)
	}
	if got := f.count("a"); got != 2 {
		t.Errorf("Get calls for a after ttl = %d, want 2", got)
	}
}

func TestTokenStoreEmptyPresentedSkipsLookup(t *testing.T) {
	s, f, _ := newTestStore()
	f.set("a", tokA)
	if s.Valid(context.Background(), "a", "") {
		t.Fatal("empty presented must be false")
	}
	if got := f.count("a"); got != 0 {
		t.Errorf("Get calls = %d, want 0 for empty presented token", got)
	}
}

func TestTokenStoreConcurrent(t *testing.T) {
	ctx := context.Background()
	s, f, clk := newTestStore()
	f.set("a", tokA)
	f.set("b", tokB)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				if i == 0 && j%50 == 0 {
					clk.advance(testTTL)
				}
				if !s.Valid(ctx, "a", tokA) || !s.Valid(ctx, "b", tokB) {
					t.Error("valid token rejected under concurrency")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestTokenStoreDeletedSecretRevokesGraceToken(t *testing.T) {
	ctx := context.Background()
	s, f, clk := newTestStore()
	f.set("a", tokA)
	if !s.Valid(ctx, "a", tokA) {
		t.Fatal("initial token should be valid")
	}
	f.set("a", tokB)
	clk.advance(testTTL)
	if !s.Valid(ctx, "a", tokA) || !s.Valid(ctx, "a", tokB) {
		t.Fatal("both tokens should be valid inside the rotation grace window")
	}
	f.mu.Lock()
	delete(f.tokens, "a")
	f.mu.Unlock()
	clk.advance(testTTL)
	if s.Valid(ctx, "a", tokA) {
		t.Error("grace-window token must be revoked once the Secret is gone")
	}
	if s.Valid(ctx, "a", tokB) {
		t.Error("current token must be revoked once the Secret is gone")
	}
}

func (s *TokenStore) entryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func TestTokenStoreRejectsInvalidNamespaces(t *testing.T) {
	ctx := context.Background()
	s, f, _ := newTestStore()
	f.set("a", tokA)

	forms := []string{"UPPER%d", "under_score%d", "dot.ted%d", "sla/sh%d", "-lead%d", "trail%d-", "sp ace%d", "%d\x00nul"}
	for i := range 10000 {
		ns := fmt.Sprintf(forms[i%len(forms)], i)
		if i%len(forms) == 0 && i%16 == 0 {
			ns = strings.Repeat("a", 64) + fmt.Sprint(i) // longer than a DNS-1123 label
		}
		if s.Valid(ctx, ns, tokA) {
			t.Fatalf("Valid(%q) = true for an invalid namespace", ns)
		}
	}
	if got := s.entryCount(); got != 0 {
		t.Errorf("cache entries = %d after 10000 invalid namespaces, want 0", got)
	}
	if got := f.total(); got != 0 {
		t.Errorf("Secret Gets = %d after 10000 invalid namespaces, want 0", got)
	}

	// A valid but unknown namespace is still looked up (the server gates
	// which namespaces reach here).
	if s.Valid(ctx, "unknown-ns", tokA) {
		t.Error("unknown namespace must not validate")
	}
	if got := f.count("unknown-ns"); got != 1 {
		t.Errorf("Gets for valid unknown namespace = %d, want 1", got)
	}
	if !s.Valid(ctx, "a", tokA) {
		t.Error("valid namespace and token rejected")
	}
}

func TestTokenStoreColdTransientErrorRetriesAfterBackoff(t *testing.T) {
	ctx := context.Background()
	s, f, clk := newTestStore()
	f.set("a", tokA)
	f.failNext("a", context.Canceled)

	if s.Valid(ctx, "a", tokA) {
		t.Fatal("no last-known token: a failed cold read must be fail-closed")
	}
	clk.advance(transientRetry - time.Millisecond)
	if s.Valid(ctx, "a", tokA) {
		t.Error("still inside the retry backoff; token not yet known")
	}
	if got := f.count("a"); got != 1 {
		t.Errorf("Gets inside backoff = %d, want 1", got)
	}
	clk.advance(time.Millisecond)
	if !s.Valid(ctx, "a", tokA) {
		t.Error("retry after backoff should have loaded the token")
	}
	if got := f.count("a"); got != 2 {
		t.Errorf("Gets after backoff = %d, want 2 (retry at 1s, not at ttl)", got)
	}
}

// TestTokenStoreOutageKeepsOnlyWhatTheLastReadAllowed pins the rule the agent
// relies on to keep relay traffic flowing through an API server outage
// (#2079): while reads fail, the store answers exactly as its last successful
// read left it. The current token stays valid. A rotated-out token stays valid
// only until the grace deadline set when its rotation was read; the outage
// does not move it. A token rotated in during the outage is refused until a
// read returns it, and a namespace never read stays fail-closed.
func TestTokenStoreOutageKeepsOnlyWhatTheLastReadAllowed(t *testing.T) {
	const tokC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	ctx := context.Background()
	s, f, clk := newTestStore()
	f.set("a", tokA)
	if !s.Valid(ctx, "a", tokA) {
		t.Fatal("initial token should be valid")
	}
	f.set("a", tokB)
	clk.advance(testTTL)
	if !s.Valid(ctx, "a", tokB) {
		t.Fatal("rotated token should be valid once read")
	}
	// tokA's grace ends testGrace from here, the read that saw the rotation.

	// The API goes away, and the controller rotates again meanwhile.
	outage := errors.New("dial tcp: connect: connection refused")
	f.set("a", tokC)
	f.set("b", tokA)
	// Each clock step below passes the 1 s retry, so the first Valid after it
	// re-reads once: queue one failure per step.
	clk.advance(testGrace - time.Second)
	f.failNext("a", outage)
	if !s.Valid(ctx, "a", tokA) {
		t.Error("rotated-out token refused inside its grace window during the outage")
	}
	if !s.Valid(ctx, "a", tokB) {
		t.Error("current token refused during the outage")
	}
	if s.Valid(ctx, "a", tokC) {
		t.Error("a token the store never read was accepted during the outage")
	}
	f.failNext("b", outage)
	if s.Valid(ctx, "b", tokA) {
		t.Error("a namespace never read accepted a token during the outage; it must fail closed")
	}

	clk.advance(time.Second)
	f.failNext("a", outage)
	if s.Valid(ctx, "a", tokA) {
		t.Error("rotated-out token still valid after its grace deadline: the outage extended it")
	}
	if !s.Valid(ctx, "a", tokB) {
		t.Error("current token refused during the outage")
	}
	if got := f.count("a"); got != 4 {
		t.Errorf("Secret reads = %d, want 4 (two before the outage, one failed read per step in it)", got)
	}

	// The API is back: the next read installs tokC, tokB enters its grace.
	clk.advance(transientRetry)
	if !s.Valid(ctx, "a", tokC) {
		t.Error("token rotated in during the outage refused after the API returned")
	}
	if !s.Valid(ctx, "a", tokB) {
		t.Error("previous token refused right after the post-outage rotation, want its grace window")
	}
	if s.Valid(ctx, "a", tokA) {
		t.Error("token rotated out before the outage accepted again")
	}
}

func TestTokenStoreRefreshDetachedFromCallerContext(t *testing.T) {
	s, f, _ := newTestStore()
	f.set("a", tokA)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if !s.Valid(ctx, "a", tokA) {
		t.Error("a cancelled caller ctx must not fail the Secret read")
	}
	if f.noDeadline {
		t.Error("Secret read ran without a deadline")
	}
	if f.maxDeadline <= 0 || f.maxDeadline > refreshTimeout {
		t.Errorf("Secret read deadline = %v, want within (0, %v]", f.maxDeadline, refreshTimeout)
	}
}

func TestTokenStoreTransientErrorKeepsLastKnown(t *testing.T) {
	ctx := context.Background()
	s, f, clk := newTestStore()
	f.set("a", tokA)
	if !s.Valid(ctx, "a", tokA) {
		t.Fatal("initial token should be valid")
	}

	clk.advance(testTTL)
	f.failNext("a", errors.New("apiserver unavailable"))
	f.failNext("a", errors.New("apiserver unavailable"))
	if !s.Valid(ctx, "a", tokA) {
		t.Error("last-known token must survive a transient read error")
	}
	clk.advance(transientRetry)
	if !s.Valid(ctx, "a", tokA) {
		t.Error("last-known token must survive repeated transient read errors")
	}
	if got := f.count("a"); got != 3 {
		t.Errorf("Gets = %d, want 3 (initial + two retries at 1s spacing)", got)
	}

	// The next successful read picks up a rotation as usual.
	f.set("a", tokB)
	clk.advance(transientRetry)
	if !s.Valid(ctx, "a", tokB) {
		t.Error("new token should be valid after recovery")
	}
	if !s.Valid(ctx, "a", tokA) {
		t.Error("old token should be in its grace window after recovery")
	}
}

func TestTokenStoreBackoffMeasuredFromEndOfSlowRead(t *testing.T) {
	ctx := context.Background()
	s, f, clk := newTestStore()
	f.set("slow", tokA)
	start := clk.now()

	var once sync.Once
	f.setHook(func() { once.Do(func() { clk.advance(refreshTimeout) }) })
	f.failNext("slow", context.DeadlineExceeded) // the apiserver hung until the 3s timeout

	if s.Valid(ctx, "slow", tokA) {
		t.Fatal("cold entry with a failed read must be fail-closed")
	}
	if got := s.entry("slow").nextRefresh; !got.Equal(start.Add(refreshTimeout + transientRetry)) {
		t.Errorf("nextRefresh = +%v, want +%v (1s after the read ended)", got.Sub(start), refreshTimeout+transientRetry)
	}

	clk.advance(500 * time.Millisecond) // +3.5s
	s.Valid(ctx, "slow", tokA)
	if got := f.count("slow"); got != 1 {
		t.Errorf("Gets at +3.5s = %d, want 1 (still inside the backoff)", got)
	}
	clk.advance(500 * time.Millisecond) // +4s
	if !s.Valid(ctx, "slow", tokA) {
		t.Error("retry at +4s should load the token")
	}
	if got := f.count("slow"); got != 2 {
		t.Errorf("Gets at +4s = %d, want 2", got)
	}
}

func TestTokenStoreWarmCallersNotBlockedBySlowRefresh(t *testing.T) {
	ctx := context.Background()
	s, f, clk := newTestStore()
	f.set("a", tokA)
	if !s.Valid(ctx, "a", tokA) {
		t.Fatal("initial token should be valid")
	}
	clk.advance(testTTL)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	f.setHook(func() {
		enteredOnce.Do(func() { close(entered) })
		<-release
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	refresher := make(chan bool, 1)
	go func() { refresher <- s.Valid(ctx, "a", tokA) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh Get never started")
	}

	const callers = 50
	var trueCount atomic.Int32
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Valid(ctx, "a", tokA) {
				trueCount.Add(1)
			}
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("warm callers blocked behind the in-flight refresh")
	}
	if got := trueCount.Load(); got != callers {
		t.Errorf("%d/%d warm callers accepted the cached token", got, callers)
	}

	close(release)
	select {
	case ok := <-refresher:
		if !ok {
			t.Error("refreshing caller should accept the token")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refreshing caller never returned")
	}
	f.mu.Lock()
	maxInFlight := f.maxInFlight
	f.mu.Unlock()
	if maxInFlight != 1 {
		t.Errorf("max concurrent Gets = %d, want 1", maxInFlight)
	}
	if got := f.count("a"); got != 2 {
		t.Errorf("Gets = %d, want 2 (initial + one refresh)", got)
	}
}

func TestTokenStoreColdCallersWaitForInFlightRead(t *testing.T) {
	s, f, _ := newTestStore()
	f.set("a", tokA)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	f.setHook(func() {
		enteredOnce.Do(func() { close(entered) })
		<-release
	})

	first := make(chan bool, 1)
	go func() { first <- s.Valid(context.Background(), "a", tokA) }()
	<-entered

	// A cold waiter whose ctx ends gives up with false instead of hanging.
	cctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if s.Valid(cctx, "a", tokA) {
		t.Error("cold waiter with an expired ctx must return false")
	}

	second := make(chan bool, 1)
	go func() { second <- s.Valid(context.Background(), "a", tokA) }()
	close(release)
	for i, ch := range []chan bool{first, second} {
		select {
		case ok := <-ch:
			if !ok {
				t.Errorf("caller %d rejected a valid token", i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("caller %d never returned", i)
		}
	}
	if got := f.count("a"); got != 1 {
		t.Errorf("Gets = %d, want 1 (cold waiters share the in-flight read)", got)
	}
}

// The relay trims its token file, so a stored token with a trailing newline
// (a Secret made with `echo`) must accept the trimmed token. A stored token
// that is only whitespace is empty and accepts nothing.
func TestTokenStoreTrimsStoredToken(t *testing.T) {
	ctx := context.Background()
	s, f, _ := newTestStore()
	f.set("a", tokA+"\n")
	f.set("blank", " \n\t")
	if !s.Valid(ctx, "a", tokA) {
		t.Error("stored token with a trailing newline rejected the trimmed token")
	}
	if s.Valid(ctx, "blank", " ") || s.Valid(ctx, "blank", " \n\t") {
		t.Error("a whitespace-only stored token accepted a presented value")
	}
}

// A non-NotFound Secret read error is logged at Warn once per namespace per
// transition: repeated identical errors log once, a changed error logs again,
// and the same error after a success logs again. Token values never appear.
func TestTokenStoreWarnsOncePerErrorTransition(t *testing.T) {
	ctx := context.Background()
	core, logs := observer.New(zap.DebugLevel)
	f := newFakeSecrets()
	clk := &fakeClock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	s := NewTokenStore(f, testTTL, testGrace, clk.now, WithLogger(zap.New(core).Sugar()))
	f.set("a", tokA)

	warns := func() int { return logs.FilterLevelExact(zap.WarnLevel).Len() }
	try := func(err error) {
		t.Helper()
		if err != nil {
			f.failNext("a", err)
		}
		s.Valid(ctx, "a", tokA)
		clk.advance(testTTL + time.Second)
	}

	boom := errors.New("connection refused")
	try(boom)
	try(boom)
	try(boom)
	if got := warns(); got != 1 {
		t.Fatalf("after three identical errors: %d Warn logs, want 1", got)
	}
	try(errors.New("forbidden"))
	if got := warns(); got != 2 {
		t.Fatalf("after a changed error: %d Warn logs, want 2", got)
	}
	try(nil) // success resets
	try(boom)
	if got := warns(); got != 3 {
		t.Fatalf("after error, success, error: %d Warn logs, want 3", got)
	}
	f.mu.Lock()
	delete(f.tokens, "a")
	f.mu.Unlock()
	try(nil) // NotFound is not a warning
	if got := warns(); got != 3 {
		t.Errorf("NotFound logged a Warn: %d, want 3", got)
	}
	for _, e := range logs.All() {
		if strings.Contains(fmt.Sprint(e.Message, e.ContextMap()), tokA) {
			t.Errorf("log entry contains the token: %v", e)
		}
	}
}

// A Secret without the controller's managed-by label holds no token for the
// store (#1957): a planted token is rejected from the first read, and a
// Secret that loses the label revokes the current and previous tokens at the
// next refresh. Adding the label back restores it.
func TestTokenStoreIgnoresUnmanagedSecret(t *testing.T) {
	ctx := context.Background()
	s, f, clk := newTestStore()

	f.set("planted", tokA)
	f.setUnmanaged("planted", true)
	if s.Valid(ctx, "planted", tokA) {
		t.Error("a token from an unlabelled Secret was accepted")
	}

	f.set("a", tokA)
	if !s.Valid(ctx, "a", tokA) {
		t.Fatal("token from the controller's Secret rejected")
	}
	f.set("a", tokB)
	clk.advance(testTTL)
	if !s.Valid(ctx, "a", tokB) || !s.Valid(ctx, "a", tokA) {
		t.Fatal("rotation should accept the new token and the old one within grace")
	}

	f.setUnmanaged("a", true)
	clk.advance(testTTL)
	if s.Valid(ctx, "a", tokB) {
		t.Error("the current token survived the Secret losing its label")
	}
	if s.Valid(ctx, "a", tokA) {
		t.Error("the grace-window token survived the Secret losing its label")
	}

	f.setUnmanaged("a", false)
	clk.advance(testTTL)
	if !s.Valid(ctx, "a", tokB) {
		t.Error("token rejected after the label came back")
	}
}

// An ignored unmanaged Secret is logged at Warn once per change, naming the
// Secret and never its token.
func TestTokenStoreWarnsOncePerUnmanagedTransition(t *testing.T) {
	ctx := context.Background()
	core, logs := observer.New(zap.DebugLevel)
	f := newFakeSecrets()
	clk := &fakeClock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	s := NewTokenStore(f, testTTL, testGrace, clk.now, WithLogger(zap.New(core).Sugar()))
	f.set("a", tokA)
	f.setUnmanaged("a", true)

	warns := func() int { return logs.FilterLevelExact(zap.WarnLevel).Len() }
	refresh := func() {
		s.Valid(ctx, "a", tokA)
		clk.advance(testTTL + time.Second)
	}

	refresh()
	refresh()
	refresh()
	if got := warns(); got != 1 {
		t.Fatalf("after three reads of an unmanaged Secret: %d Warn logs, want 1", got)
	}
	f.setUnmanaged("a", false)
	refresh()
	f.setUnmanaged("a", true)
	refresh()
	if got := warns(); got != 2 {
		t.Fatalf("after unmanaged, managed, unmanaged: %d Warn logs, want 2", got)
	}
	for _, e := range logs.FilterLevelExact(zap.WarnLevel).All() {
		fields := e.ContextMap()
		if fields["secret"] != inferencev1alpha1.MetalRelaySecretName || fields["namespace"] != "a" {
			t.Errorf("Warn does not name the Secret and namespace: %v", fields)
		}
		if !strings.Contains(e.Message, "deleted") {
			t.Errorf("Warn does not say how to fix it: %q", e.Message)
		}
	}
	for _, e := range logs.All() {
		if strings.Contains(fmt.Sprint(e.Message, e.ContextMap()), tokA) {
			t.Errorf("log entry contains the token: %v", e)
		}
	}
}
