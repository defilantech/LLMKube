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

	"github.com/defilantech/llmkube/internal/router"
)

func main() {
	configPath := flag.String("config", "/etc/llmkube/router/config.json",
		"Path to the compiled router config file (mounted from the controller-managed ConfigMap).")
	listen := flag.String("listen", ":8080",
		"Address the HTTP server binds to (host:port).")
	logFormat := flag.String("log-format", "json",
		"Structured log format: json or text.")
	shutdownTimeout := flag.Duration("shutdown-timeout", 30*time.Second,
		"How long to wait for in-flight requests on SIGTERM before forcing a close.")
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

	proxy := router.NewProxy(cfg, logger)
	mux := http.NewServeMux()
	proxy.Mount(mux)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: streaming chat completions can be long-lived.
		// Per-request context handles the real deadline.
	}

	// Run the server in a goroutine so the main goroutine can wait on
	// SIGTERM and trigger graceful shutdown.
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
