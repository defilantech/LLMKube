/*
Copyright 2026.

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
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	prommetrics "github.com/defilantech/llmkube/internal/metrics"
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
			"swap/residency/hold metrics. Empty, 0 or none disables the metrics "+
			"server, matching the manager's --metrics-bind-address. Must not "+
			"resolve to the same address as --listen.")
	logFormat := flag.String("log-format", "json",
		"Structured log format: json or text.")
	shutdownTimeout := registerShutdownTimeout(flag.CommandLine)
	quarantineDuration := flag.Duration("quarantine-duration", 15*time.Second,
		"How long a backend stays in the quarantined (skip) state after a "+
			"5xx / network error (two in a row for a pod of an endpoint "+
			"backend) before becoming eligible for a half-open "+
			"probe. Shorter windows recover faster from transient blips; "+
			"longer windows reduce probe load on genuinely-down upstreams.")
	streamIdleTimeout := flag.Duration("stream-idle-timeout", 300*time.Second,
		"How long a response body may go without a byte from the upstream, "+
			"after headers, before the proxy aborts the request and "+
			"quarantines that backend endpoint. Catches a backend that "+
			"accepted a request and then stopped producing. 0 disables.")
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
	relayMode := flag.Bool("relay", false,
		"Run as a metal-agent relay instead of a router: forward every "+
			"request on --listen to a pinned metal-agent ingress. No router "+
			"config, no ModelRouter CRD, no Kubernetes client. Set by the "+
			"controller for in-cluster relay pods.")
	relayTarget := flag.String("relay-target", "",
		"Relay mode: the InferenceService this relay serves, as "+
			"<namespace>/<name>. Falls back to RELAY_TARGET.")
	relayUpstream := flag.String("relay-upstream", "",
		"Relay mode: the metal-agent ingress URL to forward to "+
			"(https://...). Falls back to RELAY_UPSTREAM.")
	relayPin := flag.String("relay-spki-pin", "",
		"Relay mode: base64 SHA-256 SPKI pin of the ingress certificate. "+
			"Falls back to RELAY_SPKI_PIN.")
	relayTokenFile := flag.String("relay-token-file", "",
		"Relay mode: path to the per-namespace relay token file (a "+
			"projected Secret volume). Falls back to RELAY_TOKEN_FILE.")
	flag.Parse()

	logger := newLogger(*logFormat)
	slog.SetDefault(logger)

	if *relayMode {
		os.Exit(runRelay(logger, *listen, *metricsListen, *shutdownTimeout,
			relayConfigFromFlags(*relayTarget, *relayUpstream, *relayPin, *relayTokenFile)))
	}

	// Checked before anything binds. Two listeners on one address is otherwise
	// a race whose outcome depends on which one loses, so the same flags can
	// produce either a crash-loop or a running proxy with no metrics. Failing
	// here makes it one deterministic error that names the conflict.
	if !metricsDisabled(*metricsListen) {
		if err := listenConflict(*listen, *metricsListen); err != nil {
			logger.Error("invalid listener configuration", "error", err)
			os.Exit(1)
		}
	}

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
			router.WithStreamIdleTimeout(*streamIdleTimeout),
			router.WithRouterLabel(routerNameFromEnv()),
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

	// Endpoint-resolution backends: the proxy tracks their pods itself, so pod
	// churn never redeploys it. A watch that cannot start or sync leaves
	// dispatch on each backend's Service address.
	if watcher := newEndpointWatcher(cfg, proxy, logger); watcher != nil {
		go watcher.Start(baseCtx)
		logger.Info("endpoint watch started")
	}

	// LoadAware backends: scrape each pod's engine metrics once a second.
	if scraper := router.NewLoadScraper(cfg, proxy.Dispatcher(), routerNameFromEnv(), logger); scraper.HasTargets() {
		go scraper.Start(baseCtx)
		logger.Info("engine metrics scraper started")
	}

	// LoadAware backends: release prefill counts whose response never started.
	if cfg.HasLoadAware() {
		go proxy.Dispatcher().RunPrefillJanitor(baseCtx, 30*time.Second)
		logger.Info("prefill janitor started")
	}

	// Session-affine LoadAware backends: expire idle session bindings.
	if cfg.HasSessionAffinity() {
		go proxy.Dispatcher().RunSessionSweeper(baseCtx, 30*time.Second)
		logger.Info("session sweeper started")
	}

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
	// held-request depth) are registered on internal/metrics' RouterRegistry;
	// without this endpoint they are collected but never scrapeable. A
	// separate port keeps metrics off the inference listener and lets a
	// ServiceMonitor target it independently. Empty, "0" or "none"
	// disables the server; see metricsDisabled for why "0" is accepted.
	var metricsSrv *http.Server
	if addr := *metricsListen; !metricsDisabled(addr) {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("GET /metrics", newMetricsHandler())
		// The budget admin endpoint rides this listener: the operator polls it
		// to publish ModelRouter.status.budgetUtilization, and the inference
		// listener stays free of admin surface (#1851).
		proxy.MountAdmin(metricsMux)
		metricsSrv = &http.Server{
			Addr:              addr,
			Handler:           metricsMux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		// Bind synchronously rather than letting ListenAndServe do it inside
		// the goroutine. The bind is the step that fails, and doing it here
		// means the success line is logged only once the port is actually
		// held. It also lets the log name the resolved address, so a ":0"
		// request reports the port it actually got instead of ":0".
		metricsLn, lnErr := net.Listen("tcp", addr)
		if lnErr != nil {
			// Not fatal, deliberately: metrics are observability, and killing
			// the data plane because a scrape endpoint could not bind would
			// trade a monitoring gap for an outage. The conflict that used to
			// land here most often is now caught by listenConflict above.
			logger.Error("metrics server failed to bind; continuing without metrics",
				"address", addr, "error", lnErr)
			metricsSrv = nil
		} else {
			logger.Info("metrics server listening", "address", metricsLn.Addr().String())
			go func() {
				if err := metricsSrv.Serve(metricsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("metrics server failed", "error", err)
				}
			}()
		}
	} else {
		logger.Info("metrics server disabled", "metrics-bind-address", addr)
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
		// No handler runs any more, so no new shadow copy can start: cancel
		// the ones in flight and wait for them.
		proxy.Close()
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

// defaultShutdownTimeout is the drain window a proxy uses when its command
// line carries no --shutdown-timeout, which is what the controller renders
// for a ModelRouter that sets no spec.proxy.drainTimeout.
const defaultShutdownTimeout = 30 * time.Second

// registerShutdownTimeout registers --shutdown-timeout on fs and returns the
// value the flag will parse into. Extracted from main so a test can assert the
// flag's name, type and default on the real registration, which is the half of
// the controller's rendered arg this binary owns.
func registerShutdownTimeout(fs *flag.FlagSet) *time.Duration {
	return fs.Duration("shutdown-timeout", defaultShutdownTimeout,
		"How long to wait for in-flight requests on SIGTERM before forcing a close.")
}

// newEndpointWatcher builds the EndpointSlice watcher when the config has an
// EndpointSource backend, or returns nil. A client that cannot be built is
// logged, not fatal: dispatch then uses each backend's Service address.
func newEndpointWatcher(cfg *router.Config, proxy *router.Proxy, logger *slog.Logger) *router.EndpointWatcher {
	probe := router.NewEndpointWatcher(nil, cfg, proxy.Dispatcher(), routerNameFromEnv(), logger)
	if !probe.HasSources() {
		return nil
	}
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		logger.Error("endpoint watch: no Kubernetes config; dispatching to Service addresses", "error", err)
		return nil
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		logger.Error("endpoint watch: build client; dispatching to Service addresses", "error", err)
		return nil
	}
	return router.NewEndpointWatcher(cs, cfg, proxy.Dispatcher(), routerNameFromEnv(), logger)
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

// newMetricsHandler serves internal/metrics' RouterRegistry plus
// controller-runtime's rest_client_* families, NOT the Prometheus default
// registry, and NOT the rest of controller-runtime's.
//
// promhttp.Handler() serves prometheus.DefaultGatherer, which holds none of the
// llmkube collectors, so using it here returns HTTP 200 carrying Go runtime and
// process metrics and none of the series this endpoint exists for. That failure
// is silent: the scrape succeeds, the PodMonitor reports a healthy target, and
// the dashboards are simply empty.
//
// The rest of controller-runtime's registry is the manager's (certwatcher_*,
// controller_runtime_*), which the proxy never sets. Its rest_client_* families
// are the proxy's own: they count the API-server calls behind the EndpointSlice
// watch and the ModelRouter reads.
//
// Extracted from main() purely so it can be asserted on; see the regression
// tests alongside this file.
func newMetricsHandler() http.Handler {
	return promhttp.HandlerFor(prometheus.Gatherers{
		prommetrics.RouterRegistry,
		familiesWithPrefix(ctrlmetrics.Registry, "rest_client_"),
	}, promhttp.HandlerOpts{})
}

func familiesWithPrefix(g prometheus.Gatherer, prefix string) prometheus.Gatherer {
	return prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		families, err := g.Gather()
		return slices.DeleteFunc(families, func(mf *dto.MetricFamily) bool {
			return !strings.HasPrefix(mf.GetName(), prefix)
		}), err
	})
}
