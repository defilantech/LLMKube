/*
Copyright 2025.

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
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// ProcessHealthChecker checks whether a llama-server process is healthy.
type ProcessHealthChecker interface {
	Check(ctx context.Context, port int) (bool, error)
}

// DefaultProcessHealthChecker probes a llama-server's /health endpoint.
type DefaultProcessHealthChecker struct {
	client http.Client
}

// NewDefaultProcessHealthChecker creates a health checker with the given timeout.
func NewDefaultProcessHealthChecker(timeout time.Duration) *DefaultProcessHealthChecker {
	return &DefaultProcessHealthChecker{
		client: http.Client{Timeout: timeout},
	}
}

// Check sends a GET request to the llama-server /health endpoint.
func (c *DefaultProcessHealthChecker) Check(ctx context.Context, port int) (bool, error) {
	url := fmt.Sprintf("http://localhost:%d/health", port)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() {
		// Drain body to allow TCP connection reuse, capped at 64KB.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	return resp.StatusCode == http.StatusOK, nil
}

// HealthMonitor continuously polls managed processes for health.
type HealthMonitor struct {
	agent    *MetalAgent
	checker  ProcessHealthChecker
	interval time.Duration
	logger   *zap.SugaredLogger
}

// NewHealthMonitor creates a new health monitor.
func NewHealthMonitor(
	agent *MetalAgent, checker ProcessHealthChecker,
	interval time.Duration, logger *zap.SugaredLogger,
) *HealthMonitor {
	return &HealthMonitor{
		agent:    agent,
		checker:  checker,
		interval: interval,
		logger:   logger,
	}
}

// processSnapshot is a read-only copy of a managed process for health checking.
type processSnapshot struct {
	Key       string
	Name      string
	Namespace string
	Port      int
	Healthy   bool
}

// Run starts the health monitor loop. It blocks until ctx is cancelled.
func (m *HealthMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAll(ctx)
		}
	}
}

// checkAll performs one health check cycle across all managed processes.
func (m *HealthMonitor) checkAll(ctx context.Context) {
	// Snapshot processes under RLock
	m.agent.mu.RLock()
	snapshots := make([]processSnapshot, 0, len(m.agent.processes))
	for key, p := range m.agent.processes {
		snapshots = append(snapshots, processSnapshot{
			Key:       key,
			Name:      p.Name,
			Namespace: p.Namespace,
			Port:      p.Port,
			Healthy:   p.Healthy,
		})
	}
	m.agent.mu.RUnlock()

	// Update managed process count
	managedProcesses.Set(float64(len(snapshots)))

	for _, snap := range snapshots {
		if ctx.Err() != nil {
			return
		}

		start := time.Now()
		healthy, err := m.checker.Check(ctx, snap.Port)
		duration := time.Since(start).Seconds()
		healthCheckDuration.WithLabelValues(snap.Name, snap.Namespace).Observe(duration)

		if err != nil {
			healthy = false
			m.logger.Warnw("health check error", "name", snap.Name, "namespace", snap.Namespace, "error", err)
		}

		if snap.Healthy && !healthy {
			// healthy → unhealthy transition
			m.logger.Warnw("process became unhealthy",
				"name", snap.Name, "namespace", snap.Namespace)

			m.agent.mu.Lock()
			if p, ok := m.agent.processes[snap.Key]; ok {
				p.Healthy = false
			}
			m.agent.mu.Unlock()

			processHealthy.WithLabelValues(snap.Name, snap.Namespace).Set(0)
			m.agent.scheduleRestart(ctx, snap.Name, snap.Namespace)
		} else if !snap.Healthy && healthy {
			// unhealthy → healthy transition
			m.logger.Infow("process recovered",
				"name", snap.Name, "namespace", snap.Namespace)

			m.agent.mu.Lock()
			if p, ok := m.agent.processes[snap.Key]; ok {
				p.Healthy = true
			}
			m.agent.mu.Unlock()

			processHealthy.WithLabelValues(snap.Name, snap.Namespace).Set(1)
		}
	}
}

// HealthServer serves /healthz, /readyz, and /metrics for the Metal agent.
type HealthServer struct {
	agent  *MetalAgent
	port   int
	logger *zap.SugaredLogger
}

// NewHealthServer creates a new health server.
func NewHealthServer(agent *MetalAgent, port int, logger *zap.SugaredLogger) *HealthServer {
	return &HealthServer{
		agent:  agent,
		port:   port,
		logger: logger,
	}
}

// Handler returns the HTTP handler for the health server (useful for testing).
func (s *HealthServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /metrics", promhttp.HandlerFor(AgentRegistry, promhttp.HandlerOpts{}))
	return mux
}

// Run starts the HTTP server. It blocks until ctx is cancelled.
func (s *HealthServer) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", s.port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 15, // 32KB
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			s.logger.Warnw("health server shutdown error", "error", err)
		}
	}()

	s.logger.Infow("starting health server", "port", s.port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("health server failed: %w", err)
	}
	return nil
}

// handleHealthz is a liveness probe — always returns 200.
func (s *HealthServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz is a readiness probe — returns 200 if no processes or at
// least one healthy process, 503 if all processes are unhealthy.
func (s *HealthServer) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	s.agent.mu.RLock()
	defer s.agent.mu.RUnlock()

	if len(s.agent.processes) == 0 {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
		return
	}

	for _, p := range s.agent.processes {
		if p.Healthy {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
			return
		}
	}

	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("not ready"))
}
