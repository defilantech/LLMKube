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
// host in the fleet. It is responsible for two things:
//
//  1. FleetNode registration + heartbeat (M1): keep the FleetNode CR
//     for this host present and current so the scheduler can target it.
//  2. AgenticTask watching + execution (M2+): poll the cluster for
//     AgenticTasks the scheduler has assigned to this node, claim them,
//     hand them to the configured Executor, and patch the terminal
//     status when the executor returns.
//
// v0.1 plugs in the StubExecutor (M2 placeholder); M3 swaps in the
// native agent loop, and M4 adds the gate-job executor for the verifier
// role on ShadowStack.
//
// Cross-platform: builds on darwin (real Metal capability) and linux/amd64
// (stub capability for now; M4 fills it in).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
	foremantools "github.com/defilantech/llmkube/pkg/foreman/agent/tools"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(foremanv1alpha1.AddToScheme(scheme))
	utilruntime.Must(inferencev1alpha1.AddToScheme(scheme))
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
		taskPollInterval time.Duration
		taskNamespace    string
		stubSleep        time.Duration
		maxCtx           int
		tokensPerSec     int
		staticTotalRAMGB int

		// M3 executor flags
		agentMode            string
		gitRemoteURL         string
		inferenceURLOverride string

		// M4 gate-tool flags
		foremanNamespace  string
		commitAuthorName  string
		commitAuthorEmail string
		keepWorkspace     bool
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
	flag.DurationVar(&taskPollInterval, "task-poll-interval", foremanagent.DefaultWatcherInterval,
		"How often the AgenticTask watcher polls the cluster for new assignments.")
	flag.StringVar(&taskNamespace, "task-namespace", "default",
		"Namespace the AgenticTask watcher reads from.")
	flag.DurationVar(&stubSleep, "stub-sleep", foremanagent.DefaultStubSleep,
		"How long the StubExecutor blocks per task. Only used when --executor=stub (the only v0.1 option).")
	flag.IntVar(&maxCtx, "max-context-tokens", 0,
		"Advertised max context window in tokens (0 = unset).")
	flag.IntVar(&tokensPerSec, "tokens-per-second", 0,
		"Advertised decode throughput in tok/s (0 = unset).")
	flag.IntVar(&staticTotalRAMGB, "total-ram-gb", 0,
		"Advertised total RAM on platforms without live memory probing (non-darwin only).")

	// M3 Phase F: default flipped to native. The stub executor is
	// kept around for development + tests, but production foreman-
	// agent runs the native loop. Set --agent-mode=stub explicitly
	// to fall back to the M2 placeholder.
	flag.StringVar(&agentMode, "agent-mode", "native",
		"Executor to dispatch to: native | stub. Default is native (M3 Phase F); stub is the M2 placeholder for tests.")
	flag.StringVar(&gitRemoteURL, "git-remote-url", "",
		"git URL to clone from and push branches to. v0.1 uses the fork for both. Required when --agent-mode=native.")
	flag.StringVar(&inferenceURLOverride, "inference-base-url-override", "",
		"Override the inference URL the OAI client dispatches to (e.g. http://localhost:8080/v1). "+
			"Required when foreman-agent runs outside the cluster; the in-cluster "+
			"path resolves from InferenceService.status.endpoint.")
	flag.StringVar(&commitAuthorName, "commit-author-name", "Foreman Bot",
		"git author + committer name for branches produced by the native loop.")
	flag.StringVar(&commitAuthorEmail, "commit-author-email", "",
		"git author + committer email. Required when --agent-mode=native.")
	flag.BoolVar(&keepWorkspace, "keep-workspace", false,
		"Preserve the per-task clone workspace after the run. Useful for debugging; default removes it.")
	flag.StringVar(&foremanNamespace, "foreman-namespace", "foreman-system",
		"Namespace the M4 run_gate_job tool submits gate Jobs into. Defaults to foreman-system.")

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

	if agentMode == "stub" && workspaceDir == "" && opencodeBin == "" {
		setupLog.Info(
			"stub executor active; --workspace-dir and --git-remote-url are required when --agent-mode=native",
		)
	}
	if agentMode == "native" {
		if gitRemoteURL == "" {
			setupLog.Error(nil, "--git-remote-url is required when --agent-mode=native")
			os.Exit(1)
		}
		if commitAuthorEmail == "" {
			setupLog.Error(nil, "--commit-author-email is required when --agent-mode=native")
			os.Exit(1)
		}
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

	// client-go Clientset just for pod-log access from the M4
	// run_gate_job tool. controller-runtime client does not surface
	// the pods/log subresource; this is the smallest seam that does.
	kcs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "failed to construct kubernetes clientset")
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

	// Executor: M2 stub or M3 native. Selection via --agent-mode.
	var executor foremanagent.Executor
	switch agentMode {
	case "stub":
		executor = &foremanagent.StubExecutor{SleepDuration: stubSleep}
	case "native":
		executor = &foremanagent.NativeAgentLoopExecutor{
			Client:                   kc,
			WorkspaceRoot:            workspaceDir,
			GitRemoteURL:             gitRemoteURL,
			InferenceBaseURLOverride: inferenceURLOverride,
			CommitAuthor: repo.Identity{
				Name: commitAuthorName, Email: commitAuthorEmail,
			},
			CommitCommitter: repo.Identity{
				Name: commitAuthorName, Email: commitAuthorEmail,
			},
			KeepWorkspace:   keepWorkspace,
			RegistryFactory: makeRegistryFactory(kc, kcs, foremanNamespace),
		}
	default:
		setupLog.Error(nil, "unknown --agent-mode", "value", agentMode, "valid", "stub|native")
		os.Exit(1)
	}

	watcher := &foremanagent.AgenticTaskWatcher{
		Client:    kc,
		NodeName:  fleetNodeName,
		Namespace: taskNamespace,
		Interval:  taskPollInterval,
		Executor:  executor,
	}

	cap := provider.Capability()
	setupLog.Info("foreman-agent started",
		"fleetNode", fleetNodeName,
		"tailscaleAddr", tailscaleAddr,
		"roles", spec.Roles,
		"accelerator", cap.Accelerator,
		"totalRAMGB", cap.TotalRAMGB,
		"heartbeat", heartbeat.String(),
		"taskPollInterval", taskPollInterval.String(),
		"taskNamespace", taskNamespace,
		"executor", executor.Kind(),
	)

	// Run the registrar and watcher concurrently. errgroup cancels both
	// if either returns an error; on clean shutdown via SIGTERM, both
	// return nil and we exit clean.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return reg.Run(gctx) })
	g.Go(func() error { return watcher.Run(gctx) })

	if err := g.Wait(); err != nil {
		setupLog.Error(err, "foreman-agent exited with error")
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
	if len(out) == 0 {
		// Separator-only or whitespace-only input is functionally
		// the same as empty input; collapse to nil so callers
		// (FleetNodeSpec.Roles, CapabilityOptions.InstalledModels) see
		// a single "absent" representation rather than nil vs []string{}.
		return nil
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

// makeRegistryFactory returns a RegistryFactory closure that captures
// the controller-runtime client + clientset + foreman namespace. The
// returned closure builds every tool the v0.1 surface exposes
// (read_file, write_file, str_replace, grep, bash, submit_result, and
// the M4 deterministic run_gate_job) and filters down to the
// Agent.spec.tools whitelist. Failing the whitelist filter (a typo in
// the Agent CR's tool list) returns an error so the executor surfaces
// a clean ToolRegistryBuildFailed outcome rather than launching the
// loop with the wrong tools.
func makeRegistryFactory(
	kc client.Client, kcs kubernetes.Interface, foremanNamespace string,
) func(workspace string, ag *foremanv1alpha1.Agent) (foremanagent.ToolRegistry, error) {
	logTail := makePodLogTailFn(kcs)
	return func(workspace string, ag *foremanv1alpha1.Agent) (foremanagent.ToolRegistry, error) {
		bashTimeout := time.Duration(ag.Spec.BashTimeoutSeconds) * time.Second
		if bashTimeout <= 0 {
			bashTimeout = 30 * time.Second
		}
		r, err := foremantools.New(
			&foremantools.ReadFileTool{Workspace: workspace},
			&foremantools.WriteFileTool{Workspace: workspace},
			&foremantools.StrReplaceTool{Workspace: workspace},
			&foremantools.GrepTool{Workspace: workspace},
			&foremantools.BashTool{Workspace: workspace, Timeout: bashTimeout},
			foremantools.SubmitResultTool{},
			&foremantools.RunGateJobTool{
				Client: kc,
				Cfg: foremantools.RunGateJobToolConfig{
					Namespace: foremanNamespace,
					LogTailFn: logTail,
				},
			},
		)
		if err != nil {
			return nil, fmt.Errorf("default registry: %w", err)
		}
		if len(ag.Spec.Tools) > 0 {
			filtered, err := r.Filter(ag.Spec.Tools)
			if err != nil {
				return nil, fmt.Errorf("agent tool whitelist: %w", err)
			}
			return filtered, nil
		}
		return r, nil
	}
}

// makePodLogTailFn returns a LogTailFn that fetches the last
// MaxLogTailBytes of the named Job's Pod log via the pods/log
// subresource. The Job names a single Pod (backoffLimit=0); if the
// list returns 0 or >1 we keep the empty string so Result.Extra
// stays consistent with the no-logs case.
//
// Kubernetes stamps two labels onto Job-managed Pods:
//
//   - `batch.kubernetes.io/job-name=<job>` (modern, k8s 1.27+, the
//     authoritative label going forward),
//   - `job-name=<job>` (legacy, deprecated for removal in a future
//     release).
//
// We try modern first and fall back to legacy. A cluster that has
// completed the deprecation cycle and stripped `job-name` still
// resolves through the modern selector; a pre-1.27 cluster falls
// through. The two-call cost only applies when the modern selector
// returns zero hits.
func makePodLogTailFn(kcs kubernetes.Interface) func(ctx context.Context, namespace, jobName string) string {
	return func(ctx context.Context, namespace, jobName string) string {
		for _, selector := range []string{
			"batch.kubernetes.io/job-name=" + jobName,
			"job-name=" + jobName,
		} {
			pods, err := kcs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: selector,
			})
			if err != nil || len(pods.Items) != 1 {
				continue
			}
			req := kcs.CoreV1().Pods(namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{})
			stream, err := req.Stream(ctx)
			if err != nil {
				return ""
			}
			defer func() { _ = stream.Close() }()
			buf := make([]byte, foremantools.MaxLogTailBytes)
			n, _ := io.ReadFull(stream, buf)
			return string(buf[:n])
		}
		return ""
	}
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
