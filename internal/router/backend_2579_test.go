/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestDispatchMarksRequestNotSentOnlyBeforeAnyByte pins the classification the
// router's per-backend retry depends on: only a failure before the request line
// and headers reached the connection carries ErrRequestNotSent.
func TestDispatchMarksRequestNotSentOnlyBeforeAnyByte(t *testing.T) {
	dispatch := func(t *testing.T, b Backend) error {
		t.Helper()
		cfg := &Config{Backends: []Backend{b}}
		disp := NewDispatcher(cfg)
		resp, err := disp.Dispatch(context.Background(), &cfg.Backends[0],
			http.MethodPost, "/v1/chat/completions", http.Header{}, []byte(`{"model":"x"}`))
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected a dispatch error")
		}
		return err
	}

	t.Run("connection refused", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve address: %v", err)
		}
		addr := ln.Addr().String()
		if err := ln.Close(); err != nil {
			t.Fatalf("release address: %v", err)
		}
		err = dispatch(t, Backend{Name: "drained", Tier: "local", Address: "http://" + addr})
		if !errors.Is(err, ErrRequestNotSent) {
			t.Fatalf("err = %v, want ErrRequestNotSent for a dial that wrote nothing", err)
		}
	})

	t.Run("request received then connection dropped", func(t *testing.T) {
		var calls atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			calls.Add(1)
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer is not a hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		}))
		defer srv.Close()

		err := dispatch(t, Backend{Name: "coder", Tier: "local", Address: srv.URL})
		if calls.Load() != 1 {
			t.Fatalf("upstream handler ran %d times, want 1", calls.Load())
		}
		if errors.Is(err, ErrRequestNotSent) {
			t.Fatalf("err = %v marked ErrRequestNotSent after the upstream received the request", err)
		}
	})

	t.Run("unset credential env", func(t *testing.T) {
		t.Setenv("LLMKUBE_2579_UNSET_CREDENTIAL", "")
		err := dispatch(t, Backend{
			Name:           "cloud",
			Tier:           "cloud",
			Address:        "http://127.0.0.1:1",
			CredentialsEnv: "LLMKUBE_2579_UNSET_CREDENTIAL",
		})
		if errors.Is(err, ErrRequestNotSent) {
			t.Fatalf("err = %v marked ErrRequestNotSent for a request the transport never sent", err)
		}
	})
}
