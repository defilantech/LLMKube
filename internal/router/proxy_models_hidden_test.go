/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProxyModelsSkipsHiddenBackend pins the /v1/models half of the hidden
// contract: a backend the controller marked hidden is left out of model
// discovery.
func TestProxyModelsSkipsHiddenBackend(t *testing.T) {
	h := newProxyHarness(t)
	h.cfg.Backends[1].Hidden = true

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models = %d, want 200", rec.Code)
	}
	var got struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	if len(got.Data) != 1 || got.Data[0].ID != "local-qwen" {
		t.Fatalf("/v1/models = %+v, want only the visible backend local-qwen", got.Data)
	}
}

// TestProxyDispatchesToHiddenBackend pins the other half: hidden removes a
// backend from discovery, not from dispatch, so a rule or BackendNameMatch can
// still reach it.
func TestProxyDispatchesToHiddenBackend(t *testing.T) {
	h := newProxyHarness(t)
	h.cfg.Backends[1].Hidden = true
	h.cfg.DefaultRouteStrategy = DefaultRouteStrategyBackendNameMatch

	resp := h.post(t, map[string]any{
		"model":    "cloud-opus",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dispatch to hidden backend = %d, want 200", resp.StatusCode)
	}
	if got := h.cloudBack.calls.Load(); got != 1 {
		t.Errorf("hidden backend received %d requests, want 1", got)
	}
	if got := h.localBack.calls.Load(); got != 0 {
		t.Errorf("default-route backend received %d requests, want 0: the named hidden backend must serve", got)
	}
}
