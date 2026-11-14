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
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// modelEngine is a fake engine for the routing tests: it lists modelIDs at
// GET /v1/models, the way llama-server lists its --alias, and records every
// other request it receives with the body it was sent.
type modelEngine struct {
	port     int
	hits     atomic.Int32
	lastBody atomic.Value // string
}

func newModelEngine(t *testing.T, modelIDs ...string) *modelEngine {
	t.Helper()
	e := &modelEngine{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			data := make([]map[string]string, 0, len(modelIDs))
			for _, id := range modelIDs {
				data = append(data, map[string]string{"id": id, "object": "model"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
			return
		}
		body, _ := io.ReadAll(r.Body)
		e.lastBody.Store(string(body))
		e.hits.Add(1)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	e.port, _ = strconv.Atoi(portStr)
	return e
}

func (e *modelEngine) body() string {
	s, _ := e.lastBody.Load().(string)
	return s
}

// proxyRequest sends one request through p as a host-side client would.
func proxyRequest(p *ClientProxy, method, path, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Host = "127.0.0.1:" + strconv.Itoa(testClientProxyPort)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestClientProxy_SeveralEngines_RoutesByInferenceServiceName is #2412: with
// two engines running, a request that names one of them must reach that one,
// every time. Before the fix the proxy forwarded to the first healthy engine
// in map iteration order, so 20 requests all landing on the named engine had
// a 2^-20 chance.
func TestClientProxy_SeveralEngines_RoutesByInferenceServiceName(t *testing.T) {
	alpha := newModelEngine(t, "alpha-model")
	beta := newModelEngine(t, "beta-model")
	a := &MetalAgent{processes: map[string]*ManagedProcess{
		"default/alpha": {Name: "alpha", Namespace: "default", Port: alpha.port, Healthy: true, Runtime: runtimeLlamaServer},
		"default/beta":  {Name: "beta", Namespace: "default", Port: beta.port, Healthy: true, Runtime: runtimeLlamaServer},
	}}
	p := NewClientProxy(a, testClientProxyPort, zap.NewNop().Sugar())

	const n = 20
	for i := range n {
		body := fmt.Sprintf(`{"model":"beta","messages":[{"role":"user","content":"hi %d"}]}`, i)
		rec := proxyRequest(p, http.MethodPost, "/v1/chat/completions", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, body %q", i, rec.Code, rec.Body.String())
		}
	}
	if got := beta.hits.Load(); got != n {
		t.Errorf("beta (named) engine got %d of %d requests; alpha got %d", got, n, alpha.hits.Load())
	}
	if !strings.Contains(beta.body(), `"content":"hi 19"`) {
		t.Errorf("the named engine did not receive the request body intact: %q", beta.body())
	}
}

// agentProxy builds a proxy over a MetalAgent running the given processes.
func agentProxy(procs ...*ManagedProcess) *ClientProxy {
	a := &MetalAgent{processes: map[string]*ManagedProcess{}}
	for _, p := range procs {
		a.processes[p.Namespace+"/"+p.Name] = p
	}
	return NewClientProxy(a, testClientProxyPort, zap.NewNop().Sugar())
}

func llamaProc(ns, name, served string, port int, healthy bool) *ManagedProcess {
	return &ManagedProcess{Name: name, Namespace: ns, Port: port, Healthy: healthy,
		Runtime: runtimeLlamaServer, ServedModel: served}
}

const chatBody = `{"model":%q,"messages":[{"role":"user","content":"hi"}]}`

// TestClientProxy_SeveralEngines_ServedNameWinsOverInferenceServiceName pins
// the match order: the served model name, which GET /v1/models lists and the
// docs tell clients to send, decides before an InferenceService name that
// happens to be equal to it.
func TestClientProxy_SeveralEngines_ServedNameWinsOverInferenceServiceName(t *testing.T) {
	chat := newModelEngine(t, "qwen3-8b")
	other := newModelEngine(t, "gemma")
	p := agentProxy(
		llamaProc("default", "chat", "qwen3-8b", chat.port, true),
		llamaProc("default", "qwen3-8b", "gemma", other.port, true),
	)
	rec := proxyRequest(p, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(chatBody, "qwen3-8b"))
	if rec.Code != http.StatusOK || chat.hits.Load() != 1 || other.hits.Load() != 0 {
		t.Fatalf("status %d, chat hits %d, other hits %d; want 200 on the engine serving qwen3-8b",
			rec.Code, chat.hits.Load(), other.hits.Load())
	}
}

// TestClientProxy_SeveralEngines_Refusals covers the requests the proxy
// answers itself when several engines run: no engine may be reached.
func TestClientProxy_SeveralEngines_Refusals(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantIn     string
	}{
		{"unknown model", http.MethodPost, "/v1/chat/completions", fmt.Sprintf(chatBody, "llama-70b"),
			http.StatusNotFound, `model \"llama-70b\" is not served by this agent; it serves alpha-model, beta-model`},
		{"no model field", http.MethodPost, "/v1/chat/completions", `{"messages":[]}`,
			http.StatusBadRequest, "name one in the request's model field (alpha-model, beta-model)"},
		{"body is not JSON", http.MethodPost, "/v1/chat/completions", `model=alpha-model`,
			http.StatusBadRequest, "name one in the request's model field"},
		{"model is not a string", http.MethodPost, "/v1/chat/completions", `{"model":7}`,
			http.StatusBadRequest, "name one in the request's model field"},
		{"GET without a model", http.MethodGet, "/health", "",
			http.StatusBadRequest, "name one in the request's model field"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			alpha := newModelEngine(t, "alpha-model")
			beta := newModelEngine(t, "beta-model")
			p := agentProxy(
				llamaProc("default", "alpha", "alpha-model", alpha.port, true),
				llamaProc("default", "beta", "beta-model", beta.port, true),
			)
			rec := proxyRequest(p, tc.method, tc.path, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantIn) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tc.wantIn)
			}
			if h := alpha.hits.Load() + beta.hits.Load(); h != 0 {
				t.Errorf("engines received %d requests, want 0", h)
			}
		})
	}
}

// TestClientProxy_SeveralEngines_FindsModelInEngineListing covers a model ID
// the agent does not know (an --alias in extraArgs, an adopted engine, an
// mlx-server ID): the proxy finds it in the engine's own GET /v1/models.
func TestClientProxy_SeveralEngines_FindsModelInEngineListing(t *testing.T) {
	alpha := newModelEngine(t, "alpha-model")
	adopted := newModelEngine(t, "my-alias")
	p := agentProxy(
		llamaProc("default", "alpha", "alpha-model", alpha.port, true),
		llamaProc("default", "adopted", "", adopted.port, true),
	)
	rec := proxyRequest(p, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(chatBody, "my-alias"))
	if rec.Code != http.StatusOK || adopted.hits.Load() != 1 || alpha.hits.Load() != 0 {
		t.Fatalf("status %d, adopted hits %d, alpha hits %d; want the engine that lists my-alias",
			rec.Code, adopted.hits.Load(), alpha.hits.Load())
	}
}

// TestClientProxy_SeveralEngines_SameNameInTwoNamespaces covers an
// InferenceService name used in two namespaces: the bare name is ambiguous
// (409, naming both), namespace/name picks one, and when only one of them is
// healthy the bare name goes to that one.
func TestClientProxy_SeveralEngines_SameNameInTwoNamespaces(t *testing.T) {
	a := newModelEngine(t, "x")
	b := newModelEngine(t, "y")
	p := agentProxy(llamaProc("team-a", "chat", "x", a.port, true), llamaProc("team-b", "chat", "y", b.port, true))

	rec := proxyRequest(p, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(chatBody, "chat"))
	const want409 = `(team-a/chat, team-b/chat); send the namespace/name of one, for example \"team-a/chat\"`
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want409) {
		t.Fatalf("bare name: status %d body %q, want 409 naming team-a/chat, team-b/chat",
			rec.Code, rec.Body.String())
	}
	rec = proxyRequest(p, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(chatBody, "team-b/chat"))
	if rec.Code != http.StatusOK || b.hits.Load() != 1 || a.hits.Load() != 0 {
		t.Fatalf("namespace/name: status %d, a hits %d, b hits %d; want team-b's engine",
			rec.Code, a.hits.Load(), b.hits.Load())
	}

	p = agentProxy(llamaProc("team-a", "chat", "x", a.port, false), llamaProc("team-b", "chat", "y", b.port, true))
	rec = proxyRequest(p, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(chatBody, "chat"))
	if rec.Code != http.StatusOK || b.hits.Load() != 2 || a.hits.Load() != 0 {
		t.Fatalf("one healthy: status %d, a hits %d, b hits %d; want the healthy engine",
			rec.Code, a.hits.Load(), b.hits.Load())
	}
}

// TestClientProxy_SeveralEngines_SharedDaemon covers InferenceServices on one
// shared daemon (same port): a model the daemon lists is not ambiguous, since
// every match is the same daemon, which routes by model itself.
func TestClientProxy_SeveralEngines_SharedDaemon(t *testing.T) {
	daemon := newModelEngine(t, "qwen3:8b", "gemma3:4b", "llama3:8b")
	p := agentProxy(
		&ManagedProcess{Name: "qwen", Namespace: "default", Port: daemon.port, Healthy: true,
			Runtime: runtimeLlamaServer, ModelID: "qwen3:8b"},
		&ManagedProcess{Name: "gemma", Namespace: "default", Port: daemon.port, Healthy: true,
			Runtime: runtimeLlamaServer, ModelID: "gemma3:4b"},
	)
	rec := proxyRequest(p, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(chatBody, "llama3:8b"))
	if rec.Code != http.StatusOK || daemon.hits.Load() != 1 {
		t.Fatalf("status %d (body %q), daemon hits %d; want the shared daemon",
			rec.Code, rec.Body.String(), daemon.hits.Load())
	}
}

// TestClientProxy_SeveralEngines_MergesModelList checks that GET /v1/models
// lists every engine's models, and that GET /v1/models/{id} reaches the
// engine serving id.
func TestClientProxy_SeveralEngines_MergesModelList(t *testing.T) {
	alpha := newModelEngine(t, "alpha-model")
	beta := newModelEngine(t, "beta-model")
	p := agentProxy(
		llamaProc("default", "alpha", "alpha-model", alpha.port, true),
		llamaProc("default", "beta", "beta-model", beta.port, true),
	)
	rec := proxyRequest(p, http.MethodGet, "/v1/models", "")
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status %d body %q err %v", rec.Code, rec.Body.String(), err)
	}
	if list.Object != "list" || len(list.Data) != 2 ||
		list.Data[0].ID != "alpha-model" || list.Data[1].ID != "beta-model" {
		t.Errorf("merged list = %+v, want alpha-model then beta-model", list)
	}

	rec = proxyRequest(p, http.MethodGet, "/v1/models/beta-model", "")
	if rec.Code != http.StatusOK || beta.hits.Load() != 1 || alpha.hits.Load() != 0 {
		t.Errorf("GET /v1/models/beta-model: status %d, beta hits %d, alpha hits %d",
			rec.Code, beta.hits.Load(), alpha.hits.Load())
	}
}

// TestClientProxy_SeveralEngines_RefusesPathTheChosenRuntimeDoesNotServe
// checks the allowlist is applied to the engine the model picks, not just to
// any engine: /api/chat is an Ollama path, so naming the llama-server engine
// there is refused.
func TestClientProxy_SeveralEngines_RefusesPathTheChosenRuntimeDoesNotServe(t *testing.T) {
	llama := newModelEngine(t, "llama-model")
	ollama := newModelEngine(t, "qwen3:8b")
	p := agentProxy(
		llamaProc("default", "llama", "llama-model", llama.port, true),
		&ManagedProcess{Name: "ollama", Namespace: "default", Port: ollama.port, Healthy: true,
			Runtime: inferencev1alpha1.RuntimeOllama, ModelID: "qwen3:8b"},
	)
	rec := proxyRequest(p, http.MethodPost, "/api/chat", fmt.Sprintf(chatBody, "llama-model"))
	if rec.Code != http.StatusForbidden || llama.hits.Load() != 0 {
		t.Fatalf("status %d, llama hits %d; want 403 and no request to llama-server", rec.Code, llama.hits.Load())
	}
	rec = proxyRequest(p, http.MethodPost, "/api/chat", fmt.Sprintf(chatBody, "qwen3:8b"))
	if rec.Code != http.StatusOK || ollama.hits.Load() != 1 {
		t.Fatalf("status %d, ollama hits %d; want the Ollama engine", rec.Code, ollama.hits.Load())
	}
}

// TestClientProxy_SeveralEngines_RefusesOversizedBody checks the bound on
// the body the proxy reads to find the model.
func TestClientProxy_SeveralEngines_RefusesOversizedBody(t *testing.T) {
	alpha := newModelEngine(t, "alpha-model")
	beta := newModelEngine(t, "beta-model")
	p := agentProxy(
		llamaProc("default", "alpha", "alpha-model", alpha.port, true),
		llamaProc("default", "beta", "beta-model", beta.port, true),
	)
	body := `{"model":"alpha-model","prompt":"` + strings.Repeat("a", maxProxyBodyBytes) + `"}`
	rec := proxyRequest(p, http.MethodPost, "/v1/completions", body)
	if rec.Code != http.StatusRequestEntityTooLarge || alpha.hits.Load() != 0 {
		t.Fatalf("status %d, alpha hits %d; want 413 and no request to the engine", rec.Code, alpha.hits.Load())
	}
}

// TestClientProxy_OneEngine_IgnoresModel keeps the single-engine behavior:
// llama-server ignores the model field, so a client that sends any name
// still reaches the one engine running.
func TestClientProxy_OneEngine_IgnoresModel(t *testing.T) {
	only := newModelEngine(t, "only-model")
	p := agentProxy(llamaProc("default", "only", "only-model", only.port, true))
	rec := proxyRequest(p, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(chatBody, "gpt-4o"))
	if rec.Code != http.StatusOK || only.hits.Load() != 1 {
		t.Fatalf("status %d, hits %d; want the single engine whatever model the request names", rec.Code, only.hits.Load())
	}
	rec = proxyRequest(p, http.MethodGet, "/health", "")
	if rec.Code != http.StatusOK || only.hits.Load() != 2 {
		t.Fatalf("GET /health: status %d, hits %d; want the single engine", rec.Code, only.hits.Load())
	}
}

// TestReconcileProcess_RecordsServedModel checks that a spawn stamps the
// served model name the proxy matches on: spec.modelRef, the --alias the
// executor passes.
func TestReconcileProcess_RecordsServedModel(t *testing.T) {
	f := newRestartFixture(t, "served-name")
	changed := f.updateISVC(t, func(isvc *inferencev1alpha1.InferenceService) {
		isvc.Spec.ContextSize = ptr.To(int32(8192))
	})
	if err := f.agent.ensureProcess(context.Background(), changed); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	if p := f.tracked(); p == nil || f.exec.starts() != 1 || p.ServedModel != "served-name" {
		t.Fatalf("tracked process = %+v after %d starts, want ServedModel %q", p, f.exec.starts(), "served-name")
	}
}

// TestEngineServedName mirrors each executor: the proxy may match a request
// on the agent's served name only where the engine answers to it.
func TestEngineServedName(t *testing.T) {
	cfg := func(extra ...string) ExecutorConfig {
		return ExecutorConfig{Name: "svc", ServedModelName: "qwen3", ExtraArgs: extra}
	}
	tests := []struct {
		runtime string
		cfg     ExecutorConfig
		want    string
	}{
		{runtimeLlamaServer, cfg(), "qwen3"},
		{runtimeLlamaServer, cfg("--alias", "custom"), ""},
		{runtimeLlamaCPP, cfg("--alias=custom"), ""},
		{inferencev1alpha1.RuntimeOMLX, cfg(), "qwen3"},
		{inferencev1alpha1.RuntimeTensorFold, cfg(), "qwen3"},
		{inferencev1alpha1.RuntimeTensorFold, ExecutorConfig{Name: "svc"}, "svc"},
		{inferencev1alpha1.RuntimeMLXServer, cfg(), ""},
		{inferencev1alpha1.RuntimeVLLMSwift, cfg(), ""},
		{inferencev1alpha1.RuntimeOllama, cfg(), ""},
	}
	for _, tc := range tests {
		if got := engineServedName(tc.runtime, tc.cfg); got != tc.want {
			t.Errorf("engineServedName(%s, extraArgs %v) = %q, want %q", tc.runtime, tc.cfg.ExtraArgs, got, tc.want)
		}
	}
}

// TestReconcileProcess_NoServedModelWhenExtraArgsSetAlias: an --alias in
// extraArgs replaces the agent's, so the stamp must not claim modelRef. With
// it, a second engine that really serves modelRef would make every request
// for it a 409.
func TestReconcileProcess_NoServedModelWhenExtraArgsSetAlias(t *testing.T) {
	f := newRestartFixture(t, "aliased")
	// The extraArgs policy refuses --alias unless the operator relaxes it.
	f.agent.config.AllowUnsafeExtraArgs = true
	changed := f.updateISVC(t, func(isvc *inferencev1alpha1.InferenceService) {
		isvc.Spec.ExtraArgs = []string{"--alias", "custom"}
	})
	if err := f.agent.ensureProcess(context.Background(), changed); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	if p := f.tracked(); p == nil || f.exec.starts() != 1 || p.ServedModel != "" {
		t.Fatalf("tracked process = %+v after %d starts, want ServedModel empty", p, f.exec.starts())
	}
}

// TestClientProxy_SeveralEngines_SharedDaemonListedOnce: two
// InferenceServices on one shared daemon must not list its models twice.
func TestClientProxy_SeveralEngines_SharedDaemonListedOnce(t *testing.T) {
	daemon := newModelEngine(t, "qwen3:8b", "gemma3:4b")
	p := agentProxy(
		&ManagedProcess{Name: "qwen", Namespace: "default", Port: daemon.port, Healthy: true,
			Runtime: runtimeLlamaServer, ModelID: "qwen3:8b"},
		&ManagedProcess{Name: "gemma", Namespace: "default", Port: daemon.port, Healthy: true,
			Runtime: runtimeLlamaServer, ModelID: "gemma3:4b"},
	)
	rec := proxyRequest(p, http.MethodGet, "/v1/models", "")
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Data) != 2 {
		t.Fatalf("status %d, merged list %s; want the daemon's two models once", rec.Code, rec.Body.String())
	}
}

// TestClientProxy_SeveralEngines_ListsInParallel: a hung engine costs the
// listing one timeout, not one per engine.
func TestClientProxy_SeveralEngines_ListsInParallel(t *testing.T) {
	old := modelListTimeout
	modelListTimeout = 250 * time.Millisecond
	t.Cleanup(func() { modelListTimeout = old })

	procs := make([]*ManagedProcess, 0, 3)
	for i := range 3 {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}))
		t.Cleanup(srv.Close)
		_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
		port, _ := strconv.Atoi(portStr)
		procs = append(procs, llamaProc("default", fmt.Sprintf("hung-%d", i), fmt.Sprintf("m%d", i), port, true))
	}
	p := agentProxy(procs...)

	start := time.Now()
	rec := proxyRequest(p, http.MethodGet, "/v1/models", "")
	elapsed := time.Since(start)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 with every engine hung", rec.Code)
	}
	if elapsed > 600*time.Millisecond {
		t.Errorf("listing three hung engines took %v, want about one 250ms timeout (serial would be 750ms)", elapsed)
	}
}
