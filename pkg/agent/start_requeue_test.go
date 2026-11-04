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

package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These tests drive the real watcher poll loop (Watch), not a second direct
// call to the handler: a start that fails with a retryable error and writes
// nothing to the InferenceService leaves its resourceVersion unchanged, and
// the poll delivers only services whose resourceVersion moved. Each one
// creates the InferenceService while the agent runs (after the watcher's
// first poll: the startup list does not record what it delivered, so that
// poll delivers every existing service a second time, which would hide the
// bug for a service that was already there), fails its start once,
// lets the cause clear without touching the InferenceService, and requires
// the start to happen anyway.

// reconcileCtxKey marks the context the test hands the event handler, so an
// interceptor can fail a call the start path makes without failing the same
// call from the watcher's own lookups.
type reconcileCtxKey struct{}

// watchLoopTimeout bounds how long a test waits for the retried start. The
// poll runs every 20ms, so a working retry lands well inside it.
const watchLoopTimeout = 3 * time.Second

// countLists adds to funcs a List hook that counts InferenceService lists,
// so a test can tell when the watcher has run its startup list and its first
// poll.
func countLists(funcs *interceptor.Funcs) (*interceptor.Funcs, *atomic.Int32) {
	var lists atomic.Int32
	if funcs == nil {
		funcs = &interceptor.Funcs{}
	}
	funcs.List = func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*inferencev1alpha1.InferenceServiceList); ok {
			lists.Add(1)
		}
		return cl.List(ctx, list, opts...)
	}
	return funcs, &lists
}

// driveWatchLoop runs a's InferenceServiceWatcher poll loop over a's client,
// creates isvc once the watcher has listed twice (startup list and first
// poll), and hands each event to a.handleEvent on the test goroutine, as the
// agent's event loop does, until done reports true or watchLoopTimeout
// passes. afterEvent runs after each handled event with its index and error.
func driveWatchLoop(
	t *testing.T, a *MetalAgent, lists *atomic.Int32, isvc *inferencev1alpha1.InferenceService,
	afterEvent func(n int, err error), done func() bool,
) bool {
	t.Helper()
	w := NewInferenceServiceWatcher(a.config.K8sClient, "default", newNopLogger())
	w.SetPollInterval(20 * time.Millisecond)
	a.watcher = w

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan InferenceServiceEvent, 16)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		w.Watch(ctx, events)
	}()
	defer func() {
		cancel()
		for {
			select {
			case <-events:
			case <-stopped:
				return
			}
		}
	}()

	for lists.Load() < 2 {
		select {
		case <-stopped:
			t.Fatal("the watcher stopped before its first poll")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := a.config.K8sClient.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create InferenceService: %v", err)
	}

	handlerCtx := context.WithValue(context.Background(), reconcileCtxKey{}, true)
	deadline := time.After(watchLoopTimeout)
	for n := 0; !done(); {
		select {
		case ev := <-events:
			err := a.handleEvent(handlerCtx, ev)
			if afterEvent != nil {
				afterEvent(n, err)
			}
			n++
		case <-deadline:
			return false
		}
	}
	return true
}

// failOnceInStartPath returns interceptor funcs that fail the first Get of an
// object matching match made from the handler's context with err, and pass
// every other call through.
func failOnceInStartPath(match func(client.Object) bool, err error) (*interceptor.Funcs, *atomic.Int32) {
	var failed atomic.Int32
	return &interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if ctx.Value(reconcileCtxKey{}) != nil && match(obj) && failed.CompareAndSwap(0, 1) {
				return err
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, &failed
}

// A transient failure reading the sourceSecretRef Secret in the pre-flight
// check (#2320) is retried by the next poll and the service starts, although
// nothing changed the InferenceService.
func TestWatchLoop_StartRetriedAfterTransientSourceSecretRead(t *testing.T) {
	model := refusalModel()
	model.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: "creds"}
	model.Status.Size = "1.0 GiB"
	isvc := refusalISVC("svc")
	counter := &secretGetCounter{}
	unavailable := error(apierrors.NewServiceUnavailable("etcd leader changed"))
	counter.fail.Store(&unavailable)
	funcs, lists := countLists(counter.funcs())
	a, ex, _ := refusalFixtureWithInterceptor(t, MetalAgentConfig{}, funcs,
		model, hfCredentialSecret(modelSourceCredentialsLabels()))

	var firstErr error
	started := driveWatchLoop(t, a, lists, isvc, func(n int, err error) {
		if n == 0 {
			firstErr = err
			counter.fail.Store(nil)
		}
	}, func() bool { return ex.starts == 1 })
	if firstErr == nil {
		t.Fatal("the first start succeeded; the test needs it to hit the Secret read error")
	}
	if !started {
		t.Fatalf("after %v the service had not started (first error: %v); "+
			"a transient Secret read error is never retried by the poll", watchLoopTimeout, firstErr)
	}
}

// A transient failure reading the Secret during the download (the s3
// credentials read in the executor) is retried by the next poll: the
// download reaches the origin once the API server recovers.
func TestWatchLoop_DownloadRetriedAfterTransientSourceSecretRead(t *testing.T) {
	srv, hits := signedS3Server(t)
	model := refusalModel()
	model.Spec.Source = s3ObjectSource
	model.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: "creds"}
	model.Status.Size = "1.0 GiB"
	isvc := refusalISVC("svc")
	// Only the full Secret read fails: the pre-flight check reads metadata.
	funcs, failed := failOnceInStartPath(func(obj client.Object) bool {
		_, ok := obj.(*corev1.Secret)
		return ok
	}, apierrors.NewServiceUnavailable("etcd leader changed"))
	funcs, lists := countLists(funcs)
	a, _ := realExecutorFixture(t, MetalAgentConfig{}, funcs, model,
		s3CredentialSecret("default", "creds", srv.URL, modelSourceCredentialsLabels()))

	var firstErr error
	downloaded := driveWatchLoop(t, a, lists, isvc, func(n int, err error) {
		if n == 0 {
			firstErr = err
		}
	}, func() bool { return hits.Load() > 0 })
	if failed.Load() != 1 || firstErr == nil {
		t.Fatalf("the download's Secret read did not fail first (failed=%d, first error %v)", failed.Load(), firstErr)
	}
	if !downloaded {
		t.Fatalf("after %v the origin had received no request (first error: %v); "+
			"a transient Secret read error during the download is never retried by the poll",
			watchLoopTimeout, firstErr)
	}
}

// A transient failure getting the Model, and one checking the endpoint name,
// in the start path are retried by the next poll and the service starts.
func TestWatchLoop_StartRetriedAfterTransientLookup(t *testing.T) {
	cases := []struct {
		name  string
		match func(client.Object) bool
	}{
		{"Model get", func(obj client.Object) bool {
			_, ok := obj.(*inferencev1alpha1.Model)
			return ok
		}},
		{"endpoint name check", func(obj client.Object) bool {
			switch obj.(type) {
			case *corev1.Service, *discoveryv1.EndpointSlice:
				return true
			}
			return false
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := refusalModel()
			model.Status.Size = "1.0 GiB"
			funcs, failed := failOnceInStartPath(tc.match, apierrors.NewServiceUnavailable("apiserver restarting"))
			funcs, lists := countLists(funcs)
			a, ex, _ := refusalFixtureWithInterceptor(t, MetalAgentConfig{}, funcs, model)

			var firstErr error
			started := driveWatchLoop(t, a, lists, refusalISVC("svc"), func(n int, err error) {
				if n == 0 {
					firstErr = err
				}
			}, func() bool { return ex.starts == 1 })
			if failed.Load() != 1 || firstErr == nil {
				t.Fatalf("the start path's %s did not fail first (failed=%d, first error %v)",
					tc.name, failed.Load(), firstErr)
			}
			if !started {
				t.Fatalf("after %v the service had not started (first error: %v); "+
					"a transient %s error is never retried by the poll", watchLoopTimeout, firstErr, tc.name)
			}
		})
	}
}
