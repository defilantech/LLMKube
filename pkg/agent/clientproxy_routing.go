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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/defilantech/llmkube/pkg/agent/ingress"
)

// maxProxyBodyBytes bounds the request body the client proxy reads to find
// the model a request names when several children run. The ModelRouter reads
// request bodies under the same bound (internal/router maxRequestBodyBytes).
const maxProxyBodyBytes = 32 << 20

// modelListTimeout bounds each child's GET /v1/models when the proxy merges
// the listings or looks a model up in them.
const modelListTimeout = 5 * time.Second

// routeByModel picks the child that serves the model r names, for a proxy
// with several children running. When it cannot pick one it writes the
// error response itself and returns false.
func (p *ClientProxy) routeByModel(w http.ResponseWriter, r *http.Request, bs []proxyBackend) (proxyBackend, bool) {
	model, err := requestedModel(w, r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			clientProxyRequests.WithLabelValues("request_too_large").Inc()
			writeClientProxyError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"request body is over %d bytes, the most this proxy reads to find the model", maxProxyBodyBytes))
			return proxyBackend{}, false
		}
		clientProxyRequests.WithLabelValues("bad_request").Inc()
		writeClientProxyError(w, http.StatusBadRequest, "could not read the request body")
		return proxyBackend{}, false
	}
	if model == "" {
		clientProxyRequests.WithLabelValues("model_required").Inc()
		writeClientProxyError(w, http.StatusBadRequest, fmt.Sprintf(
			"this agent is serving %d models; name one in the request's model field (%s)",
			len(bs), strings.Join(modelNames(bs), ", ")))
		return proxyBackend{}, false
	}

	matches := matchModel(bs, model)
	if len(matches) == 0 {
		matches = p.matchListedModel(r.Context(), bs, model)
	}
	if b, ok := pickOne(matches); ok {
		return b, true
	}
	if len(matches) == 0 {
		clientProxyRequests.WithLabelValues("model_not_found").Inc()
		writeClientProxyError(w, http.StatusNotFound, fmt.Sprintf(
			"model %q is not served by this agent; it serves %s", model, strings.Join(modelNames(bs), ", ")))
		return proxyBackend{}, false
	}
	ids := make([]string, 0, len(matches))
	for _, b := range matches {
		ids = append(ids, b.id)
	}
	clientProxyRequests.WithLabelValues("model_ambiguous").Inc()
	writeClientProxyError(w, http.StatusConflict, fmt.Sprintf(
		"model %q is served by several InferenceServices (%s); send the namespace/name of one, for example %q",
		model, strings.Join(ids, ", "), ids[0]))
	return proxyBackend{}, false
}

// requestedModel returns the model a request names: the id in a
// GET /v1/models/{id} path, or the "model" string of a JSON request body.
// It returns "" when the request names none, including a body that is not
// JSON. A body it reads is put back on r, so the child receives it unchanged.
func requestedModel(w http.ResponseWriter, r *http.Request) (string, error) {
	if r.Method == http.MethodGet {
		if id, ok := strings.CutPrefix(r.URL.EscapedPath(), "/v1/models/"); ok {
			return url.PathUnescape(id)
		}
		return "", nil
	}
	if r.Body == nil || r.Body == http.NoBody {
		return "", nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxProxyBodyBytes))
	_ = r.Body.Close()
	if err != nil {
		return "", err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	// Unmarshal leaves Model empty on any error: a body that is not JSON, or
	// whose model is not a string, names no model.
	var named struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &named)
	return named.Model, nil
}

// matchModel returns the children model names without asking them, trying in
// order: the served model name or shared-daemon model ID (what clients are
// told to send), then the InferenceService name, then its namespace/name. The
// first rule that matches any child decides.
func matchModel(bs []proxyBackend, model string) []proxyBackend {
	rules := []func(proxyBackend) bool{
		func(b proxyBackend) bool {
			return (b.servedModel != "" && b.servedModel == model) || (b.modelID != "" && b.modelID == model)
		},
		func(b proxyBackend) bool {
			_, name, _ := strings.Cut(b.id, "/")
			return name == model
		},
		func(b proxyBackend) bool { return b.id == model },
	}
	for _, rule := range rules {
		var out []proxyBackend
		for _, b := range bs {
			if rule(b) {
				out = append(out, b)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// matchListedModel returns the children whose own GET /v1/models lists model.
// It covers the IDs the agent does not know: an --alias or
// --served-model-name set in extraArgs, the IDs mlx-server and vllm-swift
// report, a model a shared daemon serves outside LLMKube, and an engine
// adopted from an agent that did not record its served name.
func (p *ClientProxy) matchListedModel(ctx context.Context, bs []proxyBackend, model string) []proxyBackend {
	var out []proxyBackend
	for _, b := range bs {
		items, err := p.listModels(ctx, b)
		if err != nil {
			continue
		}
		for _, item := range items {
			var m struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(item, &m) == nil && m.ID == model {
				out = append(out, b)
				break
			}
		}
	}
	return out
}

// pickOne returns the single child among matches: the only match; else, when
// they all share one address (InferenceServices on one shared oMLX or Ollama
// daemon, which routes by model itself), the first; else the only healthy
// one. ok is false when there is no match or several remain.
func pickOne(matches []proxyBackend) (proxyBackend, bool) {
	switch len(matches) {
	case 0:
		return proxyBackend{}, false
	case 1:
		return matches[0], true
	}
	sameAddr := true
	for _, b := range matches[1:] {
		if b.addr != matches[0].addr {
			sameAddr = false
			break
		}
	}
	if sameAddr {
		return matches[0], true
	}
	var healthy []proxyBackend
	for _, b := range matches {
		if b.healthy {
			healthy = append(healthy, b)
		}
	}
	if len(healthy) == 1 {
		return healthy[0], true
	}
	return proxyBackend{}, false
}

// modelNames lists, for error messages, the name a client should send for
// each child: its served model name, else its shared-daemon model ID, else
// its namespace/name.
func modelNames(bs []proxyBackend) []string {
	names := make([]string, 0, len(bs))
	for _, b := range bs {
		switch {
		case b.servedModel != "":
			names = append(names, b.servedModel)
		case b.modelID != "":
			names = append(names, b.modelID)
		default:
			names = append(names, b.id)
		}
	}
	return names
}

// serveMergedModels answers GET /v1/models for a proxy with several children
// running with one OpenAI list holding every child's entries, so a client
// that discovers models through the proxy sees all of them. A child that
// does not answer is left out; when none answers the response is a 502.
func (p *ClientProxy) serveMergedModels(w http.ResponseWriter, r *http.Request, bs []proxyBackend) {
	data := []json.RawMessage{}
	answered := 0
	for _, b := range bs {
		items, err := p.listModels(r.Context(), b)
		if err != nil {
			p.logger.Warnw("client proxy could not list a child's models", "inferenceService", b.id, "err", err.Error())
			continue
		}
		answered++
		data = append(data, items...)
	}
	if answered == 0 {
		clientProxyRequests.WithLabelValues("5xx").Inc()
		writeClientProxyError(w, http.StatusBadGateway, "no running inference process answered GET /v1/models")
		return
	}
	clientProxyRequests.WithLabelValues("2xx").Inc()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}{Object: "list", Data: data})
}

// listModels returns the entries of b's GET /v1/models list. A child whose
// runtime the ingress allowlist does not let list models is skipped with an
// error, as the proxy would refuse that request too.
func (p *ClientProxy) listModels(ctx context.Context, b proxyBackend) ([]json.RawMessage, error) {
	if !ingress.Allowed(b.runtime, http.MethodGet, "/v1/models") {
		return nil, fmt.Errorf("runtime %q does not serve GET /v1/models", b.runtime)
	}
	ctx, cancel := context.WithTimeout(ctx, modelListTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+b.addr+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	transport := p.transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/models returned %d", resp.StatusCode)
	}
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode GET /v1/models: %w", err)
	}
	return list.Data, nil
}
