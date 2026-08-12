/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Command router-proxy serves the LLMKube ModelRouter HTTP data plane.
//
// The binary is intentionally small: it reads a compiled routing config
// from a file (the controller writes that file via a ConfigMap mount),
// listens for OpenAI-compatible chat completion requests, and dispatches
// them to backends according to the rules. The controller does the heavy
// lifting (validation, secrets, owner references); the proxy does only
// inference-path work.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

func main() {
	configPath := flag.String("config", "/etc/llmkube/router/config.json",
		"Path to the compiled router config file (mounted from the controller-managed ConfigMap).")
	listen := flag.String("listen", ":8080",
		"Address the HTTP server binds to (host:port).")
	metricsListen := flag.String("metrics-bind-address", ":9090",
		"Address the Prometheus metrics server binds to (host:port). Serves "+
			"the controller-runtime registry at /metrics, including the ModelPool "+
			"swap/residency/hold metrics. Empty or 0 disables the metrics "+
			"server, matching the manager's --metrics-bind-address.")
	logFormat := flag.String("log-format", "json",
		"Structured log format: json or text.")
	shutdownTimeout := flag.Duration("shutdown-timeout", 30*time.Second,
		"How long to wait for in-flight requests on SIGTERM before forcing a close.")
	quarantineDuration := flag.Duration("quarantine-duration", 15*time.Second,
		"How long a backend stays in the quarantined (skip) state after a "+
			"5xx / network error before becoming eligible for a half-open "+
			"probe. Shorter windows recover faster from transient blips; "+
			"longer windows reduce probe load on genuinely-down upstreams.")
	responseHeaderTimeout := flag.Duration("response-header-timeout", 120*time.Second,
		"Upper bound on how long the dispatcher waits for the upstream to "+
			"begin sending response headers. For non-streaming chat "+
			"completions this is effectively a max-generation-time cap. "+
			"Per-rule and per-backend timeouts in the ModelRouter CRD "+
			"can tighten this on a per-request basis but cannot extend "+
			"beyond it.")
	enableActivation := flag.Bool("enable-activation", false,
		"Enable ModelPool activation: pooled backends are made resident "+
			"(scaled up, incumbent drained) before dispatch, with sticky "+
			"anti-thrash swapping. Requires in-cluster RBAC to get/list/"+
			"watch/patch InferenceServices in the router namespace. When "+
			"disabled, pooled backends dispatch as ordinary local backends.")
	flag.Parse()

	logger := newLogger(*logFormat)
	slog.SetDefault(logger)

	cfg, err := router.LoadConfig(*configPath)
	if err != nil {
		logger.Error("load router config", "error", err, "path", *configPath)
		os.Exit(1)
	}
	logger.Info("router config loaded",
		"backends", len(cfg.Backends),
		"rules", len(cfg.Rules),
		"defaultRoute", cfg.DefaultRoute,
	)

	// baseCtx bounds ModelPool swap operations. It is cancelled on shutdown so
	// an in-flight swap unwinds cleanly, but it deliberately outlives any single
	// request: a client disconnecting mid-swap must not abort an in-progress
	// model load (the now-warm member serves the next request).
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	proxyOpts := []router.ProxyOption{
		router.WithDispatcherOptions(
			router.WithQuarantineDuration(*quarantineDuration),
			router.WithResponseHeaderTimeout(*responseHeaderTimeout),
		),
	}
	if *enableActivation {
		activator, actErr := newActivator(baseCtx, logger)
		if actErr != nil {
			logger.Error("initialize ModelPool activation", "error", actErr)
			os.Exit(1)
		}
		proxyOpts = append(proxyOpts, router.WithActivator(activator))
		logger.Info("ModelPool activation enabled")
	}

	proxy := router.NewProxy(cfg, logger, proxyOpts...)
	mux := http.NewServeMux()
	proxy.Mount(mux)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: streaming chat completions can be long-lived.
		// Per-request context handles the real deadline.
	}

	// Serve Prometheus metrics on a separate listener. The router and ModelPool
	// metrics (requests, residency, swaps, swap/hold duration, coalescing,
	// held-request depth) are registered on the controller-runtime registry
	// from internal/metrics; without this endpoint they are collected but never
	// scrapeable. A separate port keeps metrics off the inference listener and
	// lets a ServiceMonitor target it independently. Empty or 0 disables the
	// server, so the value the manager uses to turn metrics off works here too.
	var metricsSrv *http.Server
	if addr := *metricsListen; addr != "" && addr != "0" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("GET /metrics", newMetricsHandler())
		metricsSrv = &http.Server{
			Addr:              addr,
			Handler:           metricsMux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			logger.Info("starting metrics server", "address", addr)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("metrics server failed", "error", err)
			}
		}()
	}

	// Run the data-plane server in a goroutine so the main goroutine can wait
	// on SIGTERM and trigger graceful shutdown.
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("router-proxy listening", "address", *listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-stop:
		logger.Info("shutdown signal received; draining in-flight requests")
		ctx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
		defer cancel()
		if metricsSrv != nil {
			_ = metricsSrv.Shutdown(ctx)
		}
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
			os.Exit(1)
		}
		logger.Info("router-proxy stopped cleanly")
	case err := <-serverErr:
		if err != nil {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	}
}

func newLogger(format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	switch strings.ToLower(format) {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	default:
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
}

// newActivator builds the ModelPool Activator backed by an in-cluster
// Kubernetes client. It is only called when --enable-activation is set, so a
// router with no pooled backends never constructs an API client.
func newActivator(baseCtx context.Context, logger *slog.Logger) (*router.Activator, error) {
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}
	cl, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	memberCtrl := router.NewKubeMemberController(cl, 0)
	return router.NewActivator(baseCtx, memberCtrl, routerNameFromEnv(), logger), nil
}

// routerNameFromEnv resolves the router name used in metric labels. The
// controller injects ROUTER_NAME into the proxy Deployment; absent it, the
// Activator falls back to "default".
func routerNameFromEnv() string {
	return os.Getenv("ROUTER_NAME")
}

// newMetricsHandler serves controller-runtime's registry, NOT the Prometheus
// default one.
//
// Every llmkube collector, llmkube_router_* and llmkube_modelpool_* alike, is
// registered into ctrlmetrics.Registry by internal/metrics' init().
// promhttp.Handler() serves prometheus.DefaultGatherer, a different registry,
// so using it here returns HTTP 200 carrying Go runtime and process metrics and
// none of the series this endpoint exists for. That failure is silent: the
// scrape succeeds, the PodMonitor reports a healthy target, and the dashboards
// are simply empty.
//
// Extracted from main() purely so it can be asserted on; see the regression
// test alongside this file.
func newMetricsHandler() http.Handler {
	return promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{})
}
