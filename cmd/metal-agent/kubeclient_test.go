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

package main

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	restclient "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// fakeAPIServer answers the discovery the controller-runtime client needs for
// a core Secret and a GET of that Secret, and records each connection's last
// state.
func fakeAPIServer(t *testing.T) (*httptest.Server, func() map[net.Conn]http.ConnState) {
	t.Helper()
	bodies := map[string]string{
		"/api":  `{"kind":"APIVersions","versions":["v1"],"serverAddressByClientCIDRs":[]}`,
		"/apis": `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`,
		"/api/v1": `{"kind":"APIResourceList","groupVersion":"v1","resources":[` +
			`{"name":"secrets","singularName":"secret","namespaced":true,"kind":"Secret","verbs":["get"]}]}`,
		"/api/v1/namespaces/default/secrets/s": `{"kind":"Secret","apiVersion":"v1",` +
			`"metadata":{"name":"s","namespace":"default"}}`,
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	var mu sync.Mutex
	states := map[net.Conn]http.ConnState{}
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		states[c] = s
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, func() map[net.Conn]http.ConnState {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[net.Conn]http.ConnState, len(states))
		for c, s := range states {
			out[c] = s
		}
		return out
	}
}

// TestNewKubeClient_ResetClosesIdleAPIConnections: the reset func newKubeClient
// returns closes the connections the agent's client left idle, so the request
// after it dials a new one. This is what the agent runs before each retry
// while the API is unreachable, in place of exiting for a fresh process.
func TestNewKubeClient_ResetClosesIdleAPIConnections(t *testing.T) {
	srv, states := fakeAPIServer(t)
	cfg := &restclient.Config{
		Host: srv.URL,
		TLSClientConfig: restclient.TLSClientConfig{
			CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}),
		},
	}
	c, reset, err := newKubeClient(cfg, scheme.Scheme)
	if err != nil {
		t.Fatalf("newKubeClient: %v", err)
	}
	key := client.ObjectKey{Namespace: "default", Name: "s"}
	if err := c.Get(context.Background(), key, &corev1.Secret{}); err != nil {
		t.Fatalf("Get through the client: %v", err)
	}

	allIn := func(want http.ConnState) bool {
		s := states()
		if len(s) == 0 {
			return false
		}
		for _, st := range s {
			if st != want {
				return false
			}
		}
		return true
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s; connection states %v", what, states())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("the client's connections to go idle", func() bool { return allIn(http.StateIdle) })
	// Idle connections stay open on their own: only the reset closes them.
	time.Sleep(50 * time.Millisecond)
	if !allIn(http.StateIdle) {
		t.Fatalf("connections changed state before the reset: %v", states())
	}

	reset()
	waitFor("the reset to close every idle connection", func() bool { return allIn(http.StateClosed) })
}
