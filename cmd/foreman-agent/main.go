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

// foreman-agent is the Foreman node-side daemon. One instance runs on each
// host in the fleet. In M1 it owns a single responsibility: keep the
// FleetNode CR for this host present and current (initial upsert + 30s
// heartbeat). M2+ adds the AgenticTaskWatcher and executors.
//
// Cross-platform: builds on darwin (real Metal capability) and linux/amd64
// (stub capability for now; M4 fills it in).
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(foremanv1alpha1.AddToScheme(scheme))
}

func main() {
	// Note: --kubeconfig is auto-registered by sigs.k8s.io/controller-runtime/pkg/client/config
	// at import time; loadKubeconfig honors it via GetConfigWithContext. We
	// only add --kube-context on top.
	var (
		fleetNodeName    string
		tailscaleAddr    string
		kubeContext      string
		workspaceDir     string
		opencodeBin      string
		rolesFlag        string
		acceleratorFlag  string
		installedModels  string
		heartbeat        time.Duration
		maxCtx           int
		tokensPerSec     int
		staticTotalRAMGB int
	)

	flag.StringVar(&fleetNodeName, "fleet-node-name", "",
		"Identity of this node in Foreman. Defaults to a sanitized OS hostname.")
	flag.StringVar(&tailscaleAddr, "tailscale-addr", "",
		"Tailscale IP or MagicDNS name this node listens on (advertised on FleetNode.spec).")
	flag.StringVar(&kubeContext, "kube-context", "",
		"kubeconfig context override.")
	flag.StringVar(&workspaceDir, "workspace-dir", "",
		"Working directory for executor scratch (clones, transcripts). Required for M3+; unused in M1.")
	flag.StringVar(&opencodeBin, "opencode-bin", "",
		"Path to the opencode binary. Required for M3+; unused in M1.")
	flag.StringVar(&rolesFlag, "roles", "worker",
		"Comma-separated roles this node serves (worker, verifier).")
	flag.StringVar(&acceleratorFlag, "accelerator", "",
		"Accelerator label override. Defaults to metal on darwin; required on linux in v0.1.")
	flag.StringVar(&installedModels, "installed-models", "",
		"Comma-separated Model CR names this node has cached locally.")
	flag.DurationVar(&heartbeat, "heartbeat-interval", foremanagent.DefaultHeartbeatInterval,
		"How often to patch FleetNode.status with a fresh heartbeat.")
	flag.IntVar(&maxCtx, "max-context-tokens", 0,
		"Advertised max context window in tokens (0 = unset).")
	flag.IntVar(&tokensPerSec, "tokens-per-second", 0,
		"Advertised decode throughput in tok/s (0 = unset).")
	flag.IntVar(&staticTotalRAMGB, "total-ram-gb", 0,
		"Advertised total RAM on platforms without live memory probing (non-darwin only).")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if fleetNodeName == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			setupLog.Error(err, "--fleet-node-name is required; OS hostname unavailable")
			os.Exit(1)
		}
		fleetNodeName = sanitizeName(host)
	} else {
		// User-supplied name still needs to be a valid DNS-1123 label.
		clean := sanitizeName(fleetNodeName)
		if clean != fleetNodeName {
			setupLog.Info("fleet-node-name sanitized for DNS-1123 compliance",
				"input", fleetNodeName, "result", clean)
			fleetNodeName = clean
		}
	}

	if workspaceDir == "" && opencodeBin == "" {
		setupLog.Info("running in M1 mode: no executor wired yet; --workspace-dir / --opencode-bin become required at M3")
	}

	cfg, err := loadKubeconfig(kubeContext)
	if err != nil {
		setupLog.Error(err, "failed to load kubeconfig")
		os.Exit(1)
	}

	kc, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "failed to construct kubernetes client")
		os.Exit(1)
	}

	spec := foremanv1alpha1.FleetNodeSpec{
		NodeName:      fleetNodeName,
		TailscaleAddr: tailscaleAddr,
		Roles:         splitCSV(rolesFlag),
	}

	provider := foremanagent.NewCapability(foremanagent.CapabilityOptions{
		Accelerator:      foremanv1alpha1.FleetNodeAccelerator(acceleratorFlag),
		InstalledModels:  splitCSV(installedModels),
		MaxContextTokens: clampInt32(maxCtx),
		TokensPerSecond:  clampInt32(tokensPerSec),
		StaticTotalRAMGB: clampInt32(staticTotalRAMGB),
	})

	reg := &foremanagent.Registrar{
		Client:   kc,
		NodeName: fleetNodeName,
		Spec:     spec,
		Provider: provider,
		Interval: heartbeat,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := reg.Upsert(ctx); err != nil {
		setupLog.Error(err, "failed to upsert FleetNode")
		os.Exit(1)
	}

	cap := provider.Capability()
	setupLog.Info("foreman-agent started",
		"fleetNode", fleetNodeName,
		"tailscaleAddr", tailscaleAddr,
		"roles", spec.Roles,
		"accelerator", cap.Accelerator,
		"totalRAMGB", cap.TotalRAMGB,
		"heartbeat", heartbeat.String(),
	)

	if err := reg.Run(ctx); err != nil {
		setupLog.Error(err, "registrar exited with error")
		os.Exit(1)
	}
	setupLog.Info("foreman-agent stopped cleanly")
}

// loadKubeconfig defers to controller-runtime's standard discovery chain:
// the auto-registered --kubeconfig flag, then $KUBECONFIG, then
// in-cluster, then ~/.kube/config. An optional --kube-context selects a
// non-current context.
func loadKubeconfig(contextName string) (*rest.Config, error) {
	cfg, err := clientconfig.GetConfigWithContext(contextName)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return cfg, nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// clampInt32 narrows a user-supplied int flag to int32, treating negatives
// as 0 and saturating at math.MaxInt32 so the CRD's int32 field is always
// in range.
func clampInt32(n int) int32 {
	if n < 0 {
		return 0
	}
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n) //nolint:gosec // bounded above
}

var dns1123Bad = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	s = dns1123Bad.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "fleetnode"
	}
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}
