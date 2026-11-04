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
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// errAPIDown is what every API call returns while the test's API server is
// down: the dial failure a Mac sees when it cannot reach the API server.
var errAPIDown = errors.New("dial tcp 192.0.2.1:6443: connect: connection refused")

// apiOutage switches a fake client's API server off and on, and counts the
// InferenceService lists and relay Secret reads it refused while off.
type apiOutage struct {
	down          atomic.Bool
	servedLists   atomic.Int32
	refusedLists  atomic.Int32
	refusedTokens atomic.Int32
}

func (o *apiOutage) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if o.down.Load() {
				if _, ok := obj.(*corev1.Secret); ok && key.Name == inferencev1alpha1.MetalRelaySecretName {
					o.refusedTokens.Add(1)
				}
				return errAPIDown
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if o.down.Load() {
				if _, ok := list.(*inferencev1alpha1.InferenceServiceList); ok {
					o.refusedLists.Add(1)
				}
				return errAPIDown
			}
			if _, ok := list.(*inferencev1alpha1.InferenceServiceList); ok {
				o.servedLists.Add(1)
			}
			return c.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if o.down.Load() {
				return errAPIDown
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if o.down.Load() {
				return errAPIDown
			}
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			if o.down.Load() {
				return errAPIDown
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if o.down.Load() {
				return errAPIDown
			}
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if o.down.Load() {
				return errAPIDown
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if o.down.Load() {
				return errAPIDown
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}
}

// TestStart_APIOutageKeepsServingAndResumes runs the whole agent in relay mode
// through an API server outage longer than the watcher's failure threshold
// (#2079). The agent must not return from Start (main exits the process when
// it does, which drops the in-memory relay tokens and fails every relay request
// with 401 until the API returns). Through the outage it keeps its engine and
// keeps accepting the last-known relay token, also after the token's cache
// time has passed and a Secret read has failed. When the API returns it
// reconciles again (a service created afterwards is handled) and heartbeats
// the engine's endpoint promptly, without waiting for the 30 s heartbeat tick.
func TestStart_APIOutageKeepsServingAndResumes(t *testing.T) {
	const (
		relayToken = "relay-token-1"
		threshold  = 3
	)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer engine.Close()
	enginePort := engine.Listener.Addr().(*net.TCPAddr).Port

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ingressPort := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	isvc := refusalISVC("llama-a")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: inferencev1alpha1.MetalRelaySecretName, Namespace: "default",
			Labels: map[string]string{inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByController},
		},
		Data: map[string][]byte{inferencev1alpha1.MetalRelaySecretKey: []byte(relayToken)},
	}
	var outage apiOutage
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{}).
		WithObjects(refusalModel(), isvc.DeepCopy(), secret).
		WithInterceptorFuncs(outage.funcs()).
		Build()

	var resets atomic.Int32
	core, logs := observer.New(zap.InfoLevel)
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient:       c,
		EventRecorder:   record.NewFakeRecorder(64),
		Namespace:       "default",
		ModelStorePath:  t.TempDir(),
		StateDir:        t.TempDir(),
		IngressPort:     ingressPort,
		HostIP:          "10.0.0.5",
		MemoryCheckMode: MemoryCheckModeWarn,
		// No engine binary exists: nothing in this test may start an engine.
		LlamaServerBin:      filepath.Join(t.TempDir(), "no-llama-server"),
		MaxWatchFailures:    threshold,
		ResetAPIConnections: func() { resets.Add(1) },
		Logger:              zap.New(core).Sugar(),
	})
	a.tuneWatcher = func(w *InferenceServiceWatcher) {
		w.SetPollInterval(20 * time.Millisecond)
		w.maxPollBackoff = 100 * time.Millisecond
	}
	// The engine this agent serves, as adoption would have stored it. PID 0
	// is never signalled (stopChild refuses it), and the matching spec hash
	// makes the watcher's first list a no-op for it.
	key := types.NamespacedName{Namespace: "default", Name: "llama-a"}.String()
	a.processes[key] = &ManagedProcess{
		Name: "llama-a", Namespace: "default", Port: enginePort, Healthy: true,
		Runtime: runtimeLlamaServer, SpecHash: computeSpecHash(isvc), StartedAt: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- a.Start(ctx) }()

	relay := func() (int, error) {
		tr := &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // the test checks the token path, not the pin
			MinVersion:         tls.VersionTLS13,
		}}
		defer tr.CloseIdleConnections()
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://127.0.0.1:%d/v1/models", ingressPort), nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set(inferencev1alpha1.HeaderRelayTarget, key)
		req.Header.Set(inferencev1alpha1.HeaderRelayToken, relayToken)
		resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	// waitFor polls cond until it holds, failing the test if Start returns
	// first or the deadline passes.
	waitFor := func(what string, within time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(within)
		for !cond() {
			select {
			case err := <-startErr:
				t.Fatalf("the agent exited (Start returned %v) while waiting for %s", err, what)
			case <-time.After(20 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out after %v waiting for %s", within, what)
			}
		}
	}

	// API up: the relay request is authorized with the token read from the
	// Secret and reaches the engine. The third List starts only after the
	// first poll finished, so the watcher has recorded llama-a as seen and an
	// outage misread as a deletion would stop it.
	waitFor("the first relay request to succeed", 5*time.Second, func() bool {
		code, err := relay()
		return err == nil && code == http.StatusOK
	})
	tokenRead := time.Now()
	waitFor("the watcher's first poll", 5*time.Second, func() bool { return outage.servedLists.Load() >= 3 })

	// API down for longer than the failure threshold and longer than the
	// token store's cache time, so a token refresh has to fail too.
	outage.down.Store(true)
	waitFor("the outage to outlast the failure threshold and the token cache", 15*time.Second, func() bool {
		return outage.refusedLists.Load() >= 2*threshold && time.Since(tokenRead) > ingressTokenTTL+250*time.Millisecond
	})
	code, err := relay()
	if err != nil || code != http.StatusOK {
		t.Fatalf("relay request during the outage = %d, %v; want 200 with the last-known token", code, err)
	}
	if outage.refusedTokens.Load() == 0 {
		t.Fatal("the token store never tried to re-read the Secret during the outage; the check above proved nothing")
	}
	if resets.Load() == 0 {
		t.Error("the agent never reset its API connections while the API was unreachable")
	}
	a.mu.RLock()
	_, kept := a.processes[key]
	a.mu.RUnlock()
	if !kept {
		t.Fatal("the agent dropped its running engine during the API outage")
	}

	// API back: a service created now is reconciled (replicas 0 is marked
	// Stopped without starting anything), and the running engine's endpoint
	// is heartbeated well before the 30 s heartbeat tick.
	outage.down.Store(false)
	stopped := refusalISVC("llama-b")
	stopped.Spec.Replicas = ptr.To[int32](0)
	if err := c.Create(context.Background(), stopped); err != nil {
		t.Fatalf("create llama-b: %v", err)
	}
	waitFor("the watcher to reconcile a service created after the outage", 5*time.Second, func() bool {
		got := &inferencev1alpha1.InferenceService{}
		err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "llama-b"}, got)
		return err == nil && got.Status.Phase == inferencev1alpha1.PhaseStopped
	})
	waitFor("a heartbeat of the running engine's endpoint", 5*time.Second, func() bool {
		slice := &discoveryv1.EndpointSlice{}
		err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "llama-a-agent"}, slice)
		return err == nil && slice.Annotations[inferencev1alpha1.AnnotationAgentHeartbeat] != "" &&
			len(slice.Endpoints) == 1 && ptr.Deref(slice.Endpoints[0].Conditions.Ready, false)
	})
	if code, err := relay(); err != nil || code != http.StatusOK {
		t.Fatalf("relay request after the outage = %d, %v; want 200", code, err)
	}
	for _, msg := range []string{"Kubernetes API unreachable", "Kubernetes API reachable again"} {
		if n := logs.FilterMessageSnippet(msg).Len(); n != 1 {
			t.Errorf("logged %q %d times over one outage, want once", msg, n)
		}
	}

	cancel()
	select {
	case err := <-startErr:
		if err != nil {
			t.Errorf("Start returned %v on shutdown, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the context was cancelled")
	}
}
