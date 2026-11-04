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

package tracing

import (
	"context"
	"encoding/pem"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
)

// isolateOTel clears every OTEL_* variable for the test and leaves a no-op
// global TracerProvider behind it, so neither the developer's shell nor an
// earlier test decides the outcome.
func isolateOTel(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "OTEL_") {
			t.Setenv(name, "")
			_ = os.Unsetenv(name)
		}
	}
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
}

func serviceName(s sdktrace.ReadOnlySpan) string {
	for _, kv := range s.Resource().Attributes() {
		if kv.Key == semconv.ServiceNameKey {
			return kv.Value.AsString()
		}
	}
	return ""
}

func TestSetupInstallsProviderThatRecordsSpans(t *testing.T) {
	isolateOTel(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.example:4317")

	recorder := tracetest.NewSpanRecorder()
	shutdown, err := setup(context.Background(), "llmkube-router-proxy", recorder)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("global TracerProvider = %T, want *sdktrace.TracerProvider", otel.GetTracerProvider())
	}

	// The same call the router-proxy makes per request.
	_, span := otel.Tracer("model_router.dispatch").Start(context.Background(), "model_router.dispatch")
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	if got := ended[0].Name(); got != "model_router.dispatch" {
		t.Errorf("span name = %q, want model_router.dispatch", got)
	}
	if got := serviceName(ended[0]); got != "llmkube-router-proxy" {
		t.Errorf("service.name = %q, want the default llmkube-router-proxy", got)
	}
}

func TestSetupServiceNameFromEnvironment(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "OTEL_SERVICE_NAME overrides the default",
			env:  map[string]string{"OTEL_SERVICE_NAME": "edge-router"},
			want: "edge-router",
		},
		{
			name: "service.name in OTEL_RESOURCE_ATTRIBUTES overrides the default",
			env:  map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "service.name=from-attrs,team=ml"},
			want: "from-attrs",
		},
		{
			name: "OTEL_SERVICE_NAME wins over OTEL_RESOURCE_ATTRIBUTES",
			env: map[string]string{
				"OTEL_SERVICE_NAME":        "edge-router",
				"OTEL_RESOURCE_ATTRIBUTES": "service.name=from-attrs",
			},
			want: "edge-router",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateOTel(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.example:4317")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			recorder := tracetest.NewSpanRecorder()
			shutdown, err := setup(context.Background(), "llmkube-router-proxy", recorder)
			if err != nil {
				t.Fatalf("setup: %v", err)
			}
			defer func() { _ = shutdown(context.Background()) }()

			_, span := otel.Tracer("t").Start(context.Background(), "s")
			span.End()
			if got := serviceName(recorder.Ended()[0]); got != tc.want {
				t.Errorf("service.name = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSetupWithoutExportIsNoop pins that the no-endpoint path installs
// nothing: the global provider is the one that was there before.
func TestSetupWithoutExportIsNoop(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{name: "no endpoint"},
		{name: "blank endpoint", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "  "}},
		{name: "SDK disabled", env: map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.example:4317",
			"OTEL_SDK_DISABLED":           "true",
		}},
		{name: "traces exporter none", env: map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.example:4317",
			"OTEL_TRACES_EXPORTER":        "none",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateOTel(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var sentinel trace.TracerProvider = noop.NewTracerProvider()
			otel.SetTracerProvider(sentinel)

			shutdown, err := Setup(context.Background(), "llmkube-router-proxy")
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			if Enabled() {
				t.Error("Enabled() = true, want false")
			}
			if got := otel.GetTracerProvider(); got != sentinel {
				t.Errorf("global TracerProvider replaced with %T; the no-export path must install nothing", got)
			}
			if err := shutdown(context.Background()); err != nil {
				t.Errorf("no-op shutdown returned %v", err)
			}
		})
	}
}

// TestSetupRefusesEndpointThatIsNotAURL pins the guard in front of the
// exporter's silent fallback to localhost:4317 for an endpoint it cannot
// parse.
func TestSetupRefusesEndpointThatIsNotAURL(t *testing.T) {
	isolateOTel(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "10.0.0.5:4317")
	var sentinel trace.TracerProvider = noop.NewTracerProvider()
	otel.SetTracerProvider(sentinel)

	_, err := Setup(context.Background(), "llmkube-router-proxy")
	if err == nil || !strings.Contains(err.Error(), "http://host:port") {
		t.Fatalf("Setup error = %v, want one naming the http://host:port form", err)
	}
	if got := otel.GetTracerProvider(); got != sentinel {
		t.Errorf("global TracerProvider replaced with %T after a refused endpoint", got)
	}
}

func TestEndpointPrefersTracesSpecificVariable(t *testing.T) {
	isolateOTel(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://generic:4317")
	if got := Endpoint(); got != "http://generic:4317" {
		t.Errorf("Endpoint() = %q, want the generic endpoint", got)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://traces:4317")
	if got := Endpoint(); got != "http://traces:4317" {
		t.Errorf("Endpoint() = %q, want the traces-specific endpoint", got)
	}
}

// spanCollector is an OTLP/gRPC trace collector that records span names.
type spanCollector struct {
	coltracepb.UnimplementedTraceServiceServer
	mu    sync.Mutex
	names []string
}

func (c *spanCollector) Export(
	_ context.Context,
	req *coltracepb.ExportTraceServiceRequest,
) (*coltracepb.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				c.names = append(c.names, s.GetName())
			}
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func (c *spanCollector) received() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.names...)
}

// startPlaintextCollector serves OTLP/gRPC without TLS and returns its
// host:port.
func startPlaintextCollector(t *testing.T, c *spanCollector) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(srv, c)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

// startTLSCollector serves OTLP/gRPC over TLS and returns its host:port and
// the path of a PEM file holding the certificate that signs it.
func startTLSCollector(t *testing.T, c *spanCollector) (string, string) {
	t.Helper()
	srv := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(srv, c)
	ts := httptest.NewUnstartedServer(srv)
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	return ts.Listener.Addr().String(), caPath
}

// TestSetupFollowsOTLPTransportConventions drives the real OTLP exporter at a
// loopback collector and requires the span to arrive. Each case only passes
// if the transport security matches the server: a hard-coded WithInsecure()
// fails the https case, and a hard-coded WithSecure() or a raw URL handed to
// WithEndpoint fails the plaintext ones.
func TestSetupFollowsOTLPTransportConventions(t *testing.T) {
	cases := []struct {
		name string
		env  func(t *testing.T, c *spanCollector) map[string]string
	}{
		{
			name: "http scheme is plaintext",
			env: func(t *testing.T, c *spanCollector) map[string]string {
				return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + startPlaintextCollector(t, c)}
			},
		},
		{
			name: "bare hostname:port with OTEL_EXPORTER_OTLP_INSECURE=true is plaintext",
			env: func(t *testing.T, c *spanCollector) map[string]string {
				_, port, _ := net.SplitHostPort(startPlaintextCollector(t, c))
				return map[string]string{
					"OTEL_EXPORTER_OTLP_ENDPOINT": "localhost:" + port,
					"OTEL_EXPORTER_OTLP_INSECURE": "true",
				}
			},
		},
		{
			name: "https scheme is TLS verified against OTEL_EXPORTER_OTLP_CERTIFICATE",
			env: func(t *testing.T, c *spanCollector) map[string]string {
				addr, caPath := startTLSCollector(t, c)
				return map[string]string{
					"OTEL_EXPORTER_OTLP_ENDPOINT":    "https://" + addr,
					"OTEL_EXPORTER_OTLP_CERTIFICATE": caPath,
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateOTel(t)
			collector := &spanCollector{}
			for k, v := range tc.env(t, collector) {
				t.Setenv(k, v)
			}

			shutdown, err := Setup(context.Background(), "llmkube-test")
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			_, span := otel.Tracer("t").Start(context.Background(), "exported-span")
			span.End()

			// Shutdown flushes the batch, so the export has completed (or
			// failed) by the time it returns.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(ctx); err != nil {
				t.Errorf("shutdown: %v", err)
			}
			if got := collector.received(); !reflect.DeepEqual(got, []string{"exported-span"}) {
				t.Errorf("collector received %v, want [exported-span]", got)
			}
		})
	}
}

func TestExporterEnv(t *testing.T) {
	t.Run("forwards only the exporter and sampler settings, in a fixed order", func(t *testing.T) {
		isolateOTel(t)
		t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.25")
		t.Setenv("OTEL_TRACES_SAMPLER", "parentbased_traceidratio")
		t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "tempo.monitoring.svc:4317")
		// None of these may reach a rendered workload.
		t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer secret")
		t.Setenv("OTEL_SERVICE_NAME", "llmkube-controller")
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=llmkube-controller")
		t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", "/etc/otel/ca.pem")

		want := []corev1.EnvVar{
			{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "tempo.monitoring.svc:4317"},
			{Name: "OTEL_EXPORTER_OTLP_INSECURE", Value: "true"},
			{Name: "OTEL_TRACES_SAMPLER", Value: "parentbased_traceidratio"},
			{Name: "OTEL_TRACES_SAMPLER_ARG", Value: "0.25"},
		}
		if got := ExporterEnv(); !reflect.DeepEqual(got, want) {
			t.Errorf("ExporterEnv() = %+v, want %+v", got, want)
		}
	})

	t.Run("nothing when the controller does not export", func(t *testing.T) {
		isolateOTel(t)
		t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
		if got := ExporterEnv(); got != nil {
			t.Errorf("ExporterEnv() without an endpoint = %+v, want nil", got)
		}
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://tempo.monitoring.svc:4317")
		t.Setenv("OTEL_SDK_DISABLED", "true")
		if got := ExporterEnv(); got != nil {
			t.Errorf("ExporterEnv() with OTEL_SDK_DISABLED=true = %+v, want nil", got)
		}
	})
}
