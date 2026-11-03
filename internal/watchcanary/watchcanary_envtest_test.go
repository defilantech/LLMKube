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

package watchcanary

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/defilantech/llmkube/internal/metrics"
)

// TestCanaryObservesOwnWrites runs the full loop against a real API server:
// the canary writes its lease, the manager's own informer delivers the event
// back, the up gauge flips and the liveness check stays green. A canary whose
// event handler is missing (the falsification for this test) writes happily
// and never observes, which this test detects through Observed() and up.
func TestCanaryObservesOwnWrites(t *testing.T) {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	testEnv := &envtest.Environment{
		BinaryAssetsDirectory: firstEnvtestBinaryDir(),
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("stop envtest: %v", err)
		}
	})

	scheme := clientgoscheme.Scheme
	utilruntime.Must(corev1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		// The envtest namespace the test runs against; CacheScope is what
		// production passes for the operator's own namespace.
		Cache:   CacheScope("default"),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	c, err := New(context.Background(), Options{
		Namespace: "default",
		Interval:  200 * time.Millisecond,
		FailAfter: 5 * time.Second,
		Reader:    mgr.GetAPIReader(),
		Writer:    mgr.GetClient(),
		Cache:     mgr.GetCache(),
	})
	if err != nil {
		t.Fatalf("new canary: %v", err)
	}
	if !c.NeedLeaderElection() {
		t.Fatal("expected the canary to need leader election")
	}
	if err := mgr.Add(c); err != nil {
		t.Fatalf("add canary: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	select {
	case <-mgr.Elected():
	case err := <-done:
		t.Fatalf("manager exited during startup: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("manager did not start")
	}

	waitFor(t, "the write loop to start", c.Started)

	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("new direct client: %v", err)
	}

	// The lease exists and carries the canary label: the write path works.
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: "default", Name: LeaseName}
	waitFor(t, "the canary lease to be created", func() bool {
		return direct.Get(context.Background(), key, lease) == nil
	})
	if lease.Labels[LabelKey] != LabelValue {
		t.Fatalf("expected the canary label on the lease, got %v", lease.Labels)
	}

	// The informer delivered the write back: observation recorded, up set,
	// and the liveness check green.
	waitFor(t, "the canary to observe its own write", func() bool {
		_, ok := c.Observed()
		return ok
	})
	waitFor(t, "the up gauge to flip", func() bool {
		return testutil.ToFloat64(metrics.WatchCanaryUp) == 1
	})
	if err := c.LivenessCheck()(nil); err != nil {
		t.Fatalf("expected a healthy canary to pass the liveness check, got %v", err)
	}

	// Renewal continues: renewTime must advance across at least two more
	// write intervals, which only happens if the write loop keeps updating
	// the object (a no-op write bumps nothing and produces no event).
	firstRenew := lease.Spec.RenewTime
	time.Sleep(3 * c.opts.Interval)
	waitFor(t, "renewTime to advance", func() bool {
		latest := &coordinationv1.Lease{}
		if err := direct.Get(context.Background(), key, latest); err != nil {
			return false
		}
		return latest.Spec.RenewTime.After(firstRenew.Time)
	})

	if got := testutil.ToFloat64(metrics.WatchCanaryWritesTotal.WithLabelValues("ok")); got < 2 {
		t.Errorf("expected at least 2 successful writes, got %f", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("manager exited with an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("manager did not stop")
	}
}

// TestCanaryRecoversFromExternalLeaseDeletion: the lease is deleted out from
// under the canary; the write loop recreates it and observation resumes.
// This is the lifecycle case a human (or a namespace cleaner) deleting the
// lease must not be able to wedge.
func TestCanaryRecoversFromExternalLeaseDeletion(t *testing.T) {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	testEnv := &envtest.Environment{BinaryAssetsDirectory: firstEnvtestBinaryDir()}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("stop envtest: %v", err)
		}
	})

	scheme := clientgoscheme.Scheme
	utilruntime.Must(corev1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Cache:   CacheScope("default"),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	c, err := New(context.Background(), Options{
		Namespace: "default",
		Interval:  200 * time.Millisecond,
		FailAfter: 5 * time.Second,
		Reader:    mgr.GetAPIReader(),
		Writer:    mgr.GetClient(),
		Cache:     mgr.GetCache(),
	})
	if err != nil {
		t.Fatalf("new canary: %v", err)
	}
	if err := mgr.Add(c); err != nil {
		t.Fatalf("add canary: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	select {
	case <-mgr.Elected():
	case err := <-done:
		t.Fatalf("manager exited during startup: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("manager did not start")
	}

	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("new direct client: %v", err)
	}
	key := types.NamespacedName{Namespace: "default", Name: LeaseName}

	waitFor(t, "the canary to observe its own write", func() bool {
		_, ok := c.Observed()
		return ok
	})

	lease := &coordinationv1.Lease{}
	if err := direct.Get(context.Background(), key, lease); err != nil {
		t.Fatalf("get canary lease: %v", err)
	}
	if err := direct.Delete(context.Background(), lease); err != nil {
		t.Fatalf("delete canary lease: %v", err)
	}

	// Recreated within a couple of intervals, and observed again: the
	// recreated lease's Add event refreshes lastObserved past the deletion.
	waitFor(t, "the canary lease to be recreated", func() bool {
		return direct.Get(context.Background(), key, &coordinationv1.Lease{}) == nil
	})
	if err := c.LivenessCheck()(nil); err != nil {
		t.Fatalf("expected the liveness check to pass after recreation, got %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("manager exited with an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("manager did not stop")
	}
}

// waitFor polls cond until it holds. Everything here waits on events the
// manager delivers within a couple of intervals, so one generous budget
// covers every case.
func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	const budget = 10 * time.Second
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// firstEnvtestBinaryDir mirrors the controller suite's helper: locate the
// envtest assets under bin/k8s when KUBEBUILDER_ASSETS is not already set.
func firstEnvtestBinaryDir() string {
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		return ""
	}
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
