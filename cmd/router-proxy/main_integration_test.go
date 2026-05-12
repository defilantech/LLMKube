/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"syscall"
	"testing"
	"time"
)

// TestRouterProxyBinarySmoke compiles the router-proxy binary, starts it
// with a real config file and fake upstream backend, sends an HTTP
// request, and SIGTERM's the process. This is the per-PR check that the
// binary actually works end-to-end as a subprocess (something the
// in-process router unit tests can't verify).
//
// Cluster-level e2e arrives with #428, where the controller deploys the
// proxy from a ModelRouter CRD. This integration test is the
// pre-cluster gate.
func TestRouterProxyBinarySmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration smoke in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("signal handling is POSIX-specific")
	}

	dir := t.TempDir()
	binPath := filepath.Join(dir, "router-proxy")

	// Build the binary from source. Uses the same module the test runs
	// under, so any compile error in router-proxy fails this test.
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

	// Stand up a fake upstream backend that the proxy will dispatch to.
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"smoke ok"}}]}`)
	}))
	defer upstream.Close()

	// Write a minimal router config pointing at the fake upstream.
	configPath := filepath.Join(dir, "config.json")
	config := map[string]any{
		"backends": []map[string]any{
			{"name": "local", "tier": "local", "address": upstream.URL},
		},
		"defaultRoute": "local",
		"policy": map[string]any{
			"classification": map[string]any{"mode": "header-only"},
			"auditLog":       map[string]any{"sink": "stdout"},
		},
	}
	cfgBytes, _ := json.MarshalIndent(config, "", "  ")
	if err := os.WriteFile(configPath, cfgBytes, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	port := pickFreePort(t)
	listen := fmt.Sprintf("127.0.0.1:%d", port)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath,
		"--config", configPath,
		"--listen", listen,
		"--log-format", "text",
	)
	// Capture stdout/stderr in case the test fails so we get logs.
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start router-proxy: %v", err)
	}
	t.Cleanup(func() {
		// Force-kill in case the test fails before SIGTERM.
		_ = cmd.Process.Kill()
		out, _ := io.ReadAll(stdout)
		errOut, _ := io.ReadAll(stderr)
		if len(out)+len(errOut) > 0 {
			t.Logf("router-proxy stdout:\n%s", out)
			t.Logf("router-proxy stderr:\n%s", errOut)
		}
	})

	if err := waitForReady("http://"+listen+"/health", 10*time.Second); err != nil {
		t.Fatalf("router-proxy never became ready: %v", err)
	}

	// Exercise a real chat completion through the binary.
	resp, err := http.Post(
		"http://"+listen+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"any"}`),
	)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "smoke ok") {
		t.Errorf("response missing upstream payload: %s", body)
	}
	if upstreamHits != 1 {
		t.Errorf("upstream hits = %d, want 1", upstreamHits)
	}

	// Verify /v1/models also works.
	modelsResp, err := http.Get("http://" + listen + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer func() { _ = modelsResp.Body.Close() }()
	if modelsResp.StatusCode != http.StatusOK {
		t.Errorf("models status = %d, want 200", modelsResp.StatusCode)
	}

	// SIGTERM and confirm clean exit within the shutdown budget.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && !exitErr.Success() && exitErr.ExitCode() != 0 {
				t.Errorf("router-proxy exited non-zero after SIGTERM: %v", err)
			}
		}
	case <-time.After(15 * time.Second):
		t.Error("router-proxy did not shut down within 15s of SIGTERM")
	}
}

// waitForReady polls the given URL until it returns 2xx or the timeout
// expires.
func waitForReady(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:gosec // URL is constructed locally in test
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 300 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", url)
}

// pickFreePort returns an OS-assigned free TCP port. There is an
// inherent race between picking and using the port, but for a
// single-test subprocess it's reliable enough.
func pickFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// findRepoRoot walks up from this test file's location until it finds a
// go.mod, returning the directory containing it. Lets the test invoke
// `go build ./cmd/router-proxy` regardless of where `go test` is run.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found in any parent of %s", dir)
		}
		dir = parent
	}
}
