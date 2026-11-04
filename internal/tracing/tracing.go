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

// Package tracing installs the OpenTelemetry TracerProvider for LLMKube
// binaries from the standard OTEL_* environment variables, so the controller
// manager and the router-proxy configure trace export the same way.
package tracing

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	corev1 "k8s.io/api/core/v1"
)

const (
	envEndpoint       = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envSDKDisabled    = "OTEL_SDK_DISABLED"
	envTracesExporter = "OTEL_TRACES_EXPORTER"
)

// inheritedEnv is what a workload the controller renders takes from the
// controller's own environment so its spans reach the same collector.
//
// Deliberately absent: OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES name the
// controller, not the workload; OTEL_EXPORTER_OTLP_HEADERS can carry collector
// credentials, which a plain env value on a Deployment in a tenant namespace
// would publish to anyone who can read that Deployment; the CERTIFICATE and
// CLIENT_* variables are file paths that exist only in the controller pod.
var inheritedEnv = []string{
	envEndpoint,
	envTracesEndpoint,
	"OTEL_EXPORTER_OTLP_INSECURE",
	"OTEL_EXPORTER_OTLP_TRACES_INSECURE",
	"OTEL_TRACES_SAMPLER",
	"OTEL_TRACES_SAMPLER_ARG",
}

// Endpoint returns the OTLP traces endpoint the environment selects:
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, else OTEL_EXPORTER_OTLP_ENDPOINT, else
// empty. The exporter reads the same two variables with the same precedence.
func Endpoint() string {
	if v := strings.TrimSpace(os.Getenv(envTracesEndpoint)); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(envEndpoint))
}

// Enabled reports whether the environment asks for trace export: an OTLP
// endpoint is set, and neither OTEL_SDK_DISABLED=true nor
// OTEL_TRACES_EXPORTER=none turns it off. Any other OTEL_TRACES_EXPORTER
// value (console, say) still exports over OTLP on purpose: OTLP is the only
// exporter these binaries build.
func Enabled() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(envSDKDisabled)), "true") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv(envTracesExporter)), "none") {
		return false
	}
	return Endpoint() != ""
}

// Setup installs a global TracerProvider that batches spans to the OTLP/gRPC
// endpoint the environment names, and returns the function that flushes and
// stops it. When Enabled is false it installs nothing, leaving otel's no-op
// provider in place, and the returned function does nothing.
//
// The exporter takes its whole configuration from the standard variables, so
// an http:// endpoint is plaintext, an https:// one is TLS, and a bare
// hostname:port is TLS unless OTEL_EXPORTER_OTLP_INSECURE=true. An endpoint
// that is not a URL (a bare IP:port) is an error. The service name
// is OTEL_SERVICE_NAME (or service.name in OTEL_RESOURCE_ATTRIBUTES), else
// defaultServiceName.
func Setup(ctx context.Context, defaultServiceName string) (func(context.Context) error, error) {
	return setup(ctx, defaultServiceName, nil)
}

// setup is Setup with the span processor injectable: nil means a batcher over
// the OTLP exporter, and a test passes an in-memory recorder instead.
func setup(
	ctx context.Context,
	defaultServiceName string,
	processor sdktrace.SpanProcessor,
) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if !Enabled() {
		return noop, nil
	}
	// The exporter parses the endpoint with url.Parse and, when that fails,
	// silently falls back to localhost:4317. A bare IP:port such as
	// 10.0.0.5:4317 is the common case that fails, so refuse it here, where
	// the caller logs the reason, instead of exporting nowhere. Source,
	// otlptracegrpc v1.47.0: WithURL in internal/envconfig/envconfig.go hands
	// a url.Parse error to otel's global error handler and skips the
	// variable, leaving the localhost:4317 default from
	// internal/otlpconfig/options.go.
	if _, err := url.Parse(Endpoint()); err != nil {
		return noop, fmt.Errorf("OTLP endpoint %q is not a URL; use http://host:port or https://host:port: %w",
			Endpoint(), err)
	}

	// Detectors merge in order with the later one winning, so the
	// environment overrides the default name.
	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(semconv.ServiceName(defaultServiceName)),
		resource.WithFromEnv(),
	)
	if err != nil {
		return noop, fmt.Errorf("build trace resource: %w", err)
	}

	if processor == nil {
		exporter, err := otlptracegrpc.New(ctx)
		if err != nil {
			return noop, fmt.Errorf("create OTLP trace exporter: %w", err)
		}
		processor = sdktrace.NewBatchSpanProcessor(exporter)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(processor),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// ExporterEnv returns the controller's trace export settings as env vars for a
// workload it renders, in a fixed order so the rendered pod spec is stable
// across reconciles. It returns nil when the controller's own tracing is not
// Enabled, so turning it off there turns it off for the workload too.
func ExporterEnv() []corev1.EnvVar {
	if !Enabled() {
		return nil
	}
	var env []corev1.EnvVar
	for _, name := range inheritedEnv {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			env = append(env, corev1.EnvVar{Name: name, Value: v})
		}
	}
	return env
}
