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

package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

// traceCollector is an OTLP/gRPC trace collector that keeps every span it is
// sent, standing in for Tempo or an OpenTelemetry Collector.
type traceCollector struct {
	coltracepb.UnimplementedTraceServiceServer

	mu    sync.Mutex
	spans []collectedSpan
}

type collectedSpan struct {
	name     string
	service  string
	spanID   []byte
	parentID []byte
}

func (c *traceCollector) Export(
	_ context.Context,
	req *coltracepb.ExportTraceServiceRequest,
) (*coltracepb.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rs := range req.GetResourceSpans() {
		service := ""
		for _, kv := range rs.GetResource().GetAttributes() {
			if kv.GetKey() == "service.name" {
				service = kv.GetValue().GetStringValue()
			}
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				c.spans = append(c.spans, collectedSpan{
					name:     s.GetName(),
					service:  service,
					spanID:   s.GetSpanId(),
					parentID: s.GetParentSpanId(),
				})
			}
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func (c *traceCollector) collected() []collectedSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]collectedSpan(nil), c.spans...)
}

// TestRouterProxyExportsTraces runs the real router-proxy binary with
// OTEL_EXPORTER_OTLP_ENDPOINT pointed at a loopback OTLP/gRPC collector, sends
// one chat completion, and requires the proxy's request spans to reach the
// collector under the proxy's service name.
//
// The proxy has always started model_router.dispatch and backend.request
// spans, but the binary installed no TracerProvider, so they went to the
// global no-op provider and a user who set the endpoint saw nothing in Tempo.
func TestRouterProxyExportsTraces(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("signal handling is POSIX-specific")
	}

	collectorLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for collector: %v", err)
	}
	collector := &traceCollector{}
	grpcSrv := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(grpcSrv, collector)
	go func() { _ = grpcSrv.Serve(collectorLn) }()
	t.Cleanup(grpcSrv.Stop)

	dir := t.TempDir()
	binPath := filepath.Join(dir, "router-proxy")
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	build := exec.Command("go", "build", "-o", binPath, "./cmd/router-proxy")
	build.Dir = repoRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"traced"}}]}`)
	}))
	defer upstream.Close()

	configPath := filepath.Join(dir, "config.json")
	cfgBytes, _ := json.Marshal(map[string]any{
		"backends": []map[string]any{
			{"name": "local", "tier": "local", "address": upstream.URL},
		},
		"defaultRoute": "local",
		"policy": map[string]any{
			"classification": map[string]any{"mode": "header-only"},
			"auditLog":       map[string]any{"sink": "stdout"},
		},
	})
	if err := os.WriteFile(configPath, cfgBytes, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	listen := fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath,
		"--config", configPath,
		"--listen", listen,
		// Disabled so the test never binds the default metrics port.
		"--metrics-bind-address", "0",
		"--log-format", "text",
	)
	// The plain http scheme is the OTel convention for an insecure gRPC
	// connection; the collector above serves plaintext.
	cmd.Env = append(environWithoutOTel(),
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://"+collectorLn.Addr().String())
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start router-proxy: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if t.Failed() {
			t.Logf("router-proxy output:\n%s", output.String())
		}
	})

	if err := waitForReady("http://"+listen+"/health", 10*time.Second); err != nil {
		t.Fatalf("router-proxy never became ready: %v", err)
	}
	resp, err := http.Post("http://"+listen+"/v1/chat/completions",
		"application/json", strings.NewReader(`{"model":"any"}`))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The proxy flushes its spans on SIGTERM, so once it has exited cleanly
	// every span it ended has been delivered: no polling window is needed.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("router-proxy exited with error after SIGTERM: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("router-proxy did not shut down within 15s of SIGTERM")
	}

	spans := collector.collected()
	var dispatch, backend *collectedSpan
	for i := range spans {
		switch spans[i].name {
		case "model_router.dispatch":
			dispatch = &spans[i]
		case "backend.request":
			backend = &spans[i]
		}
	}
	if dispatch == nil {
		t.Fatalf("collector received no model_router.dispatch span; got %d spans: %+v", len(spans), spans)
	}
	if dispatch.service != "llmkube-router-proxy" {
		t.Errorf("dispatch span service.name = %q, want llmkube-router-proxy", dispatch.service)
	}
	if backend == nil {
		t.Fatalf("collector received no backend.request span; got %+v", spans)
	}
	if !bytes.Equal(backend.parentID, dispatch.spanID) {
		t.Errorf("backend.request parent = %x, want the dispatch span %x", backend.parentID, dispatch.spanID)
	}
}

// environWithoutOTel returns the test process environment minus any OTEL_*
// variable, so a developer's shell settings cannot steer the binary under test.
func environWithoutOTel() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "OTEL_") {
			env = append(env, kv)
		}
	}
	return env
}
