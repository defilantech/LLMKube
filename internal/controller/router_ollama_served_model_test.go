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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
	metalagent "github.com/defilantech/llmkube/pkg/agent"
)

// fakeOllamaDaemon answers the routes the agent's OllamaExecutor drives and
// the OpenAI chat route the router proxy forwards to. It serves only the tag
// the executor pulled, the way a real daemon answers an unpulled name. With
// hailoModels set it also answers GET /hailo/v1/list, as hailo-ollama does.
type fakeOllamaDaemon struct {
	srv *httptest.Server

	hailoModels []string

	mu   sync.Mutex
	tag  string
	seen []string
}

func newFakeOllamaDaemon(t *testing.T, hailoModels ...string) *fakeOllamaDaemon {
	t.Helper()
	d := &fakeOllamaDaemon{hailoModels: hailoModels}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/hailo/v1/list" && len(d.hailoModels) > 0:
			_ = json.NewEncoder(w).Encode(map[string][]string{"models": d.hailoModels})
		case r.Method == http.MethodPost && r.URL.Path == "/api/pull":
			var req struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			d.mu.Lock()
			d.tag = req.Model
			d.mu.Unlock()
			_, _ = io.WriteString(w, `{"status":"success"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/generate":
			_, _ = io.WriteString(w, `{"status":"success","done":true}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/ps":
			tag := d.pulledTag()
			_, _ = fmt.Fprintf(w, `{"models":[{"name":%q,"model":%q}]}`, tag, tag)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			var req struct {
				Model string `json:"model"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)
			d.mu.Lock()
			d.seen = append(d.seen, req.Model)
			d.mu.Unlock()
			if req.Model != d.pulledTag() {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprintf(w, `{"error":{"message":"model %q not found, try pulling it first",`+
					`"type":"api_error","param":null,"code":null}}`, req.Model)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeOllamaDaemon) pulledTag() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tag
}

func (d *fakeOllamaDaemon) seenModels() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

// port returns the daemon's port, which is where OllamaExecutor looks for the
// daemon (127.0.0.1:<port>).
func (d *fakeOllamaDaemon) port(t *testing.T) int {
	t.Helper()
	u, err := url.Parse(d.srv.URL)
	if err != nil {
		t.Fatalf("parse fake daemon URL %q: %v", d.srv.URL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("fake daemon URL %q has no port: %v", d.srv.URL, err)
	}
	return port
}

// TestRouterSendsTheTagTheOllamaAgentServes is the #2586 contract end to end:
// a ModelRouter in front of an Ollama InferenceService. The tag the request
// must carry is the one the agent's Ollama executor pulled for the Model, not
// the InferenceService name. The executor gets the Model's source the way the
// agent passes it (pkg/agent/agent.go, ModelSource: model.Spec.Source), so a
// hailo:// source is served by the tag inside it, whatever the Model is named.
func TestRouterSendsTheTagTheOllamaAgentServes(t *testing.T) {
	cases := []struct {
		name, modelName, source string
		hailoModels             []string
	}{
		{name: "catalog name", modelName: "phi-4-mini", source: "hf://org/phi-4-mini"},
		{name: "another catalog name", modelName: "llama-3.1-8b", source: "hf://org/llama-3.1-8b"},
		{name: "unmapped name", modelName: "my-custom-model", source: "hf://org/my-custom-model"},
		{
			name: "hailo source", modelName: "qwen-hailo", source: "hailo://qwen2.5-instruct:1.5b",
			hailoModels: []string{"qwen2.5-instruct:1.5b"},
		},
		{
			// The source's tag wins over the catalog mapping of the Model name.
			name: "hailo source on a catalog name", modelName: "phi-4-mini", source: "HAILO://llama3.2:3b",
			hailoModels: []string{"llama3.2:3b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			modelName := tc.modelName
			daemon := newFakeOllamaDaemon(t, tc.hailoModels...)
			executor := metalagent.NewOllamaExecutor(daemon.port(t), zap.NewNop().Sugar())
			proc, err := executor.StartProcess(context.Background(), metalagent.ExecutorConfig{
				Name:        "chat-svc",
				Namespace:   testBuilderNs,
				ModelName:   modelName,
				ModelSource: tc.source,
			})
			if err != nil {
				t.Fatalf("OllamaExecutor.StartProcess(%q, %q): %v", modelName, tc.source, err)
			}
			wantModel := daemon.pulledTag()
			if wantModel == "" {
				t.Fatalf("the executor pulled no model for %q", modelName)
			}
			if proc.ModelID != wantModel {
				t.Fatalf("the executor reports ModelID %q but pulled %q", proc.ModelID, wantModel)
			}

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "chat-svc", Namespace: testBuilderNs},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime:  inferencev1alpha1.RuntimeOllama,
					ModelRef: modelName,
				},
			}
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: testBuilderNs},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   tc.source,
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
				},
			}
			mr := &inferencev1alpha1.ModelRouter{
				ObjectMeta: metav1.ObjectMeta{Name: "chat-router", Namespace: testBuilderNs},
				Spec: inferencev1alpha1.ModelRouterSpec{
					Backends: []inferencev1alpha1.RouterBackend{{
						Name:                "chat",
						InferenceServiceRef: &corev1.LocalObjectReference{Name: "chat-svc"},
						Tier:                "local",
					}},
					DefaultRoute: "chat",
				},
			}
			r := newRouterReconcilerForTest(t, mr, isvc, model)
			compiled, err := r.compileRouterConfig(context.Background(), mr)
			if err != nil {
				t.Fatalf("compileRouterConfig: %v", err)
			}
			var cfg router.Config
			if err := json.Unmarshal(compiled.JSON, &cfg); err != nil {
				t.Fatalf("unmarshal compiled JSON: %v", err)
			}
			cfg.Backends[0].Address = daemon.srv.URL
			cfg.Backends[0].Endpoints = nil
			cfg.Backends[0].EndpointSource = nil
			if err := cfg.Validate(); err != nil {
				t.Fatalf("compiled config: %v", err)
			}
			mux := http.NewServeMux()
			router.NewProxy(&cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Mount(mux)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			seen := daemon.seenModels()
			if rec.Code != http.StatusOK || len(seen) == 0 || seen[len(seen)-1] != wantModel {
				t.Fatalf("request for %q through the router: status %d, daemon saw models %q; "+
					"the agent serves %q for Model %q, want the daemon to see %q",
					"chat", rec.Code, seen, wantModel, modelName, wantModel)
			}
		})
	}
}

// TestOllamaBackendWithMissingModelKeepsTheInferenceServiceName pins the
// degrade path of localBackendServedModel for Ollama: with no Model to read,
// or a Model whose hailo:// tag the agent refuses (so no daemon serves any
// name for it), the backend keeps the InferenceService name the proxy
// already used.
func TestOllamaBackendWithMissingModelKeepsTheInferenceServiceName(t *testing.T) {
	cases := []struct {
		name     string
		modelRef string
		// source, when set, seeds a Model named modelRef with it.
		source string
	}{
		{name: "modelRef set but the Model is absent", modelRef: "phi-4-mini"},
		{name: "modelRef empty", modelRef: ""},
		{name: "hailo source without a :tag", modelRef: "qwen-hailo", source: "hailo://qwen2.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "chat-svc", Namespace: testBuilderNs},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime:  inferencev1alpha1.RuntimeOllama,
					ModelRef: tc.modelRef,
				},
			}
			mr := &inferencev1alpha1.ModelRouter{
				ObjectMeta: metav1.ObjectMeta{Name: "chat-router", Namespace: testBuilderNs},
				Spec: inferencev1alpha1.ModelRouterSpec{
					Backends: []inferencev1alpha1.RouterBackend{{
						Name:                "chat",
						InferenceServiceRef: &corev1.LocalObjectReference{Name: "chat-svc"},
						Tier:                "local",
					}},
					DefaultRoute: "chat",
				},
			}
			objs = append(objs, isvc)
			if tc.source != "" {
				objs = append(objs, &inferencev1alpha1.Model{
					ObjectMeta: metav1.ObjectMeta{Name: tc.modelRef, Namespace: testBuilderNs},
					Spec: inferencev1alpha1.ModelSpec{
						Source:   tc.source,
						Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
					},
				})
			}
			r := newRouterReconcilerForTest(t, mr, objs...)
			compiled, err := r.compileRouterConfig(context.Background(), mr)
			if err != nil {
				t.Fatalf("compileRouterConfig: %v", err)
			}
			var cfg router.Config
			if err := json.Unmarshal(compiled.JSON, &cfg); err != nil {
				t.Fatalf("unmarshal compiled JSON: %v", err)
			}
			if cfg.Backends[0].ServedModel != "" {
				t.Errorf("ServedModel = %q, want \"\"", cfg.Backends[0].ServedModel)
			}
			if cfg.Backends[0].InferenceService != "chat-svc" {
				t.Errorf("InferenceService = %q, want chat-svc", cfg.Backends[0].InferenceService)
			}
		})
	}
}
