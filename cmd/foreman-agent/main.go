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
// role on a verifier-tagged Linux node.
//
// Cross-platform: builds on darwin (real Metal capability) and linux/amd64
// (stub capability for now; M4 fills it in).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/changepolicy"
	"github.com/defilantech/llmkube/pkg/foreman/agent/codehost"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubpr"
	"github.com/defilantech/llmkube/pkg/foreman/agent/mcp"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
	foremantools "github.com/defilantech/llmkube/pkg/foreman/agent/tools"
	"github.com/defilantech/llmkube/pkg/foreman/agent/worktracker"
	"github.com/defilantech/llmkube/pkg/selfupdate"
)

var (
	// Version information injected at build time via -ldflags.
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// githubCodeHost builds the GitHub-backed CodeHost seam (#1158), wrapping the
// githubpr client with the same token source (env / file) as the git auth so
// PR creation and head-commit reads stay authenticated.
func githubCodeHost() codehost.CodeHost {
	token, _ := repo.TokenFromEnvOrFile()
	return &codehost.GitHubCodeHost{Ensurer: githubpr.NewClient(), Token: token}
}

// githubWorkItems builds the GitHub-backed WorkItems seam (#1158), wrapping the
// githubissue client with the same token source as the git auth so issue-body
// fetches stay authenticated.
func githubWorkItems() worktracker.WorkItems {
	token, _ := repo.TokenFromEnvOrFile()
	return &worktracker.GitHubWorkItems{Fetcher: githubissue.NewClient(), Token: token}
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(foremanv1alpha1.AddToScheme(scheme))
	utilruntime.Must(inferencev1alpha1.AddToScheme(scheme))
}

func main() {
	// Subcommand dispatch. The default (no subcommand) runs the long-
	// lived node daemon (FleetNode registrar + AgenticTask watcher). The
	// "run-task" subcommand runs ONE AgenticTask to completion and exits;
	// it is the body of the ephemeral coder Job (#620). We branch on
	// os.Args[1] before flag.Parse because the flag package has no native
	// subcommand support and the two modes carry different flag sets.
	if len(os.Args) > 1 && os.Args[1] == "run-task" {
		runTaskCommand(os.Args[2:])
		return
	}

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
		nodeLabelsFlag   string
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
		agentMode                string
		gitRemoteURL             string
		inferenceURLOverride     string
		inferenceURLHostOverride string

		// M4 gate-tool flags
		foremanNamespace  string
		commitAuthorName  string
		commitAuthorEmail string
		keepWorkspace     bool

		// #620 coder-Job git Secret selection
		coderGitSecret    string
		coderGitSecretKey string

		// PR4 self-update flags
		selfUpdateEnabled bool
		installRoot       string
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
	flag.StringVar(&nodeLabelsFlag, "node-labels", "",
		"Comma-separated key=value labels stamped on this node's FleetNode "+
			"metadata on every (re)registration (e.g. coder-pool=amd). Survives "+
			"pod restarts; used for capability-routing nodeSelectors.")
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
		"Replace the resolved InferenceService URL entirely (e.g. http://localhost:8080/v1). "+
			"Use for tests and stub OAI servers; for off-cluster, same-host installs prefer "+
			"--inference-base-url-host-override so the live port flows through on every "+
			"llama-server respawn (#540).")
	flag.StringVar(&inferenceURLHostOverride, "inference-base-url-host-override", "",
		"Rewrite the host of the resolved InferenceService URL to this value (e.g. 127.0.0.1) "+
			"and substitute the live port from the v1 Endpoints object the metal-agent "+
			"maintains for the InferenceService. Use for off-cluster, same-host installs "+
			"(foreman-agent on the same Mac as the metal-agent) where cluster DNS does not "+
			"resolve but the local llama-server's port rolls on every respawn. The "+
			"controller-runtime cache re-reads the current port on each task dispatch, so "+
			"this stays current without restart.")
	flag.StringVar(&commitAuthorName, "commit-author-name", "Foreman Bot",
		"git author + committer name for branches produced by the native loop.")
	flag.StringVar(&commitAuthorEmail, "commit-author-email", "",
		"git author + committer email. Required when --agent-mode=native.")
	flag.BoolVar(&keepWorkspace, "keep-workspace", false,
		"Preserve the per-task clone workspace after the run. Useful for debugging; default removes it.")
	flag.StringVar(&foremanNamespace, "foreman-namespace", "foreman-system",
		"Namespace the M4 run_gate_job tool submits gate Jobs into. Defaults to foreman-system.")
	flag.StringVar(&coderGitSecret, "coder-git-secret", "foreman-git-credentials",
		"Name of the Secret the coder Job projects as GITHUB_TOKEN for the clone + push (#620). "+
			"Point this at an existing git Secret (e.g. foreman-github) to reuse it instead of "+
			"creating foreman-git-credentials.")
	flag.StringVar(&coderGitSecretKey, "coder-git-secret-key", "token",
		"Key within --coder-git-secret that holds the token (e.g. GITHUB_TOKEN when reusing an "+
			"existing git Secret).")

	flag.BoolVar(&selfUpdateEnabled, "self-update", true,
		"Enable self-update from FleetNode.status.updateRequest. Only engages when running from the "+
			"managed install root (~/Library/Application Support/llmkube/foreman-agent on macOS). "+
			"Set to false to disable entirely (e.g. for tests or manual installs outside the managed layout).")
	flag.StringVar(&installRoot, "install-root", "",
		"Override the managed install root path. Defaults to the platform-specific location "+
			"(~/Library/Application Support/llmkube/foreman-agent on macOS, "+
			"~/.local/share/llmkube/foreman-agent on Linux). "+
			"Only used when --self-update=true.")

	showVersion := flag.Bool("version", false, "Print version information and exit.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if *showVersion {
		fmt.Printf("foreman-agent version %s\n", Version)
		fmt.Printf("  git commit: %s\n", GitCommit)
		fmt.Printf("  build date: %s\n", BuildDate)
		os.Exit(0)
	}

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
		// M4: relaxed from os.Exit to warnings. Deterministic Agents
		// (e.g. the M4 gate) never clone or push from the
		// foreman-agent Pod, so a gate-only install legitimately has
		// no git remote URL and no commit author. When a coder task
		// actually arrives that needs these, the native executor
		// fails that task with a clean reason
		// (GitRemoteURLNotConfigured / commit identity invalid) and
		// leaves other tasks unaffected. The flags stay required at
		// the per-task level, not at startup.
		if gitRemoteURL == "" {
			setupLog.Info("--git-remote-url is unset; coder-role tasks clone and push " +
				"each task's own payload.repo (#915). Tasks with no payload.repo fail " +
				"with GitRemoteURLNotConfigured; deterministic Agents work fine without it.")
		}
		if commitAuthorEmail == "" {
			setupLog.Info("--commit-author-email is unset; coder-role tasks that need to commit will fail. " +
				"Deterministic Agents work fine without it.")
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

	// Build the self-updater. This is gated on --self-update and on whether
	// the binary is running from the managed install root. Dev builds and
	// direct invocations outside the managed layout skip the update path
	// entirely so `go run` and test environments are never affected.
	var updater foremanagent.UpdateApplier
	if selfUpdateEnabled {
		root := installRoot
		if root == "" {
			var err error
			root, err = selfupdate.ResolveInstallRoot("foreman-agent")
			if err != nil {
				setupLog.Error(err, "failed to resolve self-update install root; self-update disabled")
			}
		}
		if root != "" {
			if selfupdate.RunningUnderManagedRoot(root) {
				u := &selfupdate.Updater{
					CurrentVersion: Version,
					OS:             goruntime.GOOS,
					Arch:           goruntime.GOARCH,
					InstallRoot:    root,
					BinaryName:     "foreman-agent",
					Verifier:       &selfupdate.SHA256Verifier{},
					HTTPClient:     &http.Client{Timeout: 10 * time.Minute},
					Log:            ctrl.Log.WithName("selfupdate"),
				}
				updater = foremanagent.UpdateApplierFunc(func(version, url, sha256 string) (bool, error) {
					res, err := u.MaybeApply(selfupdate.Target{
						Version: version,
						URL:     url,
						SHA256:  sha256,
					})
					return res.Restarting, err
				})
				setupLog.Info("self-update enabled", "installRoot", root, "currentVersion", Version)
			} else {
				setupLog.Info("self-update disabled: not running from managed install root "+
					"(dev build or direct invocation); set --self-update=false to silence this",
					"installRoot", root)
			}
		}
	} else {
		setupLog.Info("self-update disabled via --self-update=false")
	}

	reg := &foremanagent.Registrar{
		Client:   kc,
		NodeName: fleetNodeName,
		Spec:     spec,
		Labels:   parseKeyValueCSV(nodeLabelsFlag),
		Provider: provider,
		Interval: heartbeat,
		Version:  Version,
		Kind:     "foreman-agent",
		OS:       goruntime.GOOS,
		Arch:     goruntime.GOARCH,
		Updater:  updater,
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
			Client:                       kc,
			WorkspaceRoot:                workspaceDir,
			GitRemoteURL:                 gitRemoteURL,
			InferenceBaseURLOverride:     inferenceURLOverride,
			InferenceBaseURLHostOverride: inferenceURLHostOverride,
			CommitAuthor: repo.Identity{
				Name: commitAuthorName, Email: commitAuthorEmail,
			},
			CommitCommitter: repo.Identity{
				Name: commitAuthorName, Email: commitAuthorEmail,
			},
			KeepWorkspace:   keepWorkspace,
			RegistryFactory: makeRegistryFactory(kc, kcs, foremanNamespace),
			CodeHost:        githubCodeHost(),
			WorkItems:       githubWorkItems(),
			ChangePolicy:    changepolicy.NewDefaultPolicy(),
			// CoderJobSubmitter routes Job-mode Agents to an ephemeral
			// per-task Job (#620). Wired ONLY on the watcher executor: the
			// run-task path (which the Job itself runs) builds its executor
			// inside foremanagent.RunTask without a submitter, so it runs
			// the loop in-process and cannot recurse into another Job.
			CoderJobSubmitter: &foremantools.RunCoderJob{
				Client: kc,
				Cfg: foremantools.RunCoderJobConfig{
					Namespace:               foremanNamespace,
					GitCredentialsSecret:    coderGitSecret,
					GitCredentialsSecretKey: coderGitSecretKey,
					// Propagate the watcher's git remote + commit identity
					// into the coder Job's run-task invocation so the in-pod
					// clone + push can authenticate and commit; without these
					// run-task fails with GitRemoteNotConfigured (#620).
					GitRemoteURL:      gitRemoteURL,
					CommitAuthorName:  commitAuthorName,
					CommitAuthorEmail: commitAuthorEmail,
					LogTailFn:         makePodLogTailFn(kcs),
				},
			},
			// EnvtestJobRunner verifies envtest-backed packages post-push in a
			// clean-room Job (#859).
			EnvtestJobRunner: makeEnvtestJobRunner(kc, foremanNamespace, makePodLogTailFn(kcs)),
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
		if errors.Is(err, foremanagent.ErrSelfUpdateRestart) {
			// Self-update applied: exit cleanly so launchd/systemd
			// restarts the process onto the new binary. The macOS plist
			// uses KeepAlive=true so launchd relaunches on any exit;
			// the systemd unit uses Restart=always (same effect).
			setupLog.Info("foreman-agent exiting for self-update restart")
			os.Exit(0)
		}
		setupLog.Error(err, "foreman-agent exited with error")
		os.Exit(1)
	}
	setupLog.Info("foreman-agent stopped cleanly")
}

// runTaskCommand implements `foreman-agent run-task`: run exactly ONE
// AgenticTask to completion and exit. It is the entrypoint the coder Job
// (#620) runs. The Job's ServiceAccount provides in-cluster credentials;
// off-cluster invocations fall back to the standard kubeconfig discovery
// chain (loadKubeconfig). The process exits non-zero on a system /
// execution error so the Job reflects failure; a NO-GO / INCOMPLETE
// verdict is a successful run with a data-shaped outcome and exits 0.
func runTaskCommand(args []string) {
	fs := flag.NewFlagSet("run-task", flag.ExitOnError)
	var (
		taskName                 string
		namespace                string
		workspaceDir             string
		kubeContext              string
		gitRemoteURL             string
		inferenceURLOverride     string
		inferenceURLHostOverride string
		commitAuthorName         string
		commitAuthorEmail        string
		foremanNamespace         string
		keepWorkspace            bool
	)
	fs.StringVar(&taskName, "task", "",
		"Name of the AgenticTask to run. Required.")
	fs.StringVar(&namespace, "namespace", "default",
		"Namespace of the AgenticTask to run.")
	fs.StringVar(&workspaceDir, "workspace-dir", "",
		"Working directory for the per-task clone. Defaults to $HOME/foreman-workspaces.")
	fs.StringVar(&kubeContext, "kube-context", "",
		"kubeconfig context override. Ignored in-cluster.")
	fs.StringVar(&gitRemoteURL, "git-remote-url", "",
		"git URL to clone from and push the result branch to. Required for coder tasks.")
	fs.StringVar(&inferenceURLOverride, "inference-base-url-override", "",
		"Replace the resolved InferenceService URL entirely (e.g. http://localhost:8080/v1).")
	fs.StringVar(&inferenceURLHostOverride, "inference-base-url-host-override", "",
		"Rewrite the host of the resolved InferenceService URL and substitute the live port "+
			"from the v1 Endpoints object (off-cluster same-host installs).")
	fs.StringVar(&commitAuthorName, "commit-author-name", "Foreman Bot",
		"git author + committer name for the produced branch.")
	fs.StringVar(&commitAuthorEmail, "commit-author-email", "",
		"git author + committer email. Required for coder tasks that commit.")
	fs.StringVar(&foremanNamespace, "foreman-namespace", "foreman-system",
		"Namespace deterministic tools (e.g. run_gate_job) submit Jobs into.")
	fs.BoolVar(&keepWorkspace, "keep-workspace", false,
		"Preserve the per-task clone workspace after the run.")

	opts := zap.Options{Development: true}
	opts.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		setupLog.Error(err, "parse run-task flags")
		os.Exit(2)
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if taskName == "" {
		setupLog.Error(nil, "--task is required")
		os.Exit(2)
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
	kcs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "failed to construct kubernetes clientset")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	res, err := foremanagent.RunTask(ctx, foremanagent.RunTaskConfig{
		Client:                       kc,
		Task:                         types.NamespacedName{Namespace: namespace, Name: taskName},
		WorkspaceDir:                 workspaceDir,
		GitRemoteURL:                 gitRemoteURL,
		InferenceBaseURLOverride:     inferenceURLOverride,
		InferenceBaseURLHostOverride: inferenceURLHostOverride,
		CommitAuthor:                 repo.Identity{Name: commitAuthorName, Email: commitAuthorEmail},
		CommitCommitter:              repo.Identity{Name: commitAuthorName, Email: commitAuthorEmail},
		KeepWorkspace:                keepWorkspace,
		RegistryFactory:              makeRegistryFactory(kc, kcs, foremanNamespace),
		CodeHost:                     githubCodeHost(),
		WorkItems:                    githubWorkItems(),
		ChangePolicy:                 changepolicy.NewDefaultPolicy(),
	})
	if err != nil {
		// System / execution failure: the result line + ERROR sentinel
		// were already emitted by RunTask on its stdout. Exit non-zero so
		// the Job reflects failure.
		setupLog.Error(err, "run-task failed", "task", taskName, "namespace", namespace)
		os.Exit(1)
	}
	setupLog.Info("run-task completed",
		"task", taskName, "namespace", namespace,
		"verdict", res.Verdict, "branch", res.Branch, "commitSHA", res.CommitSHA)
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

// parseKeyValueCSV parses a comma-separated list of key=value pairs (e.g.
// "coder-pool=amd,zone=lab") into a map. Empty/whitespace keys and malformed
// segments (no "=") are skipped; empty input returns nil. A bare trailing
// comma is tolerated.
func parseKeyValueCSV(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
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

// assembleAgentRegistry builds an agent's tool registry: native tools
// filtered by the agent's spec.tools whitelist, then MCP tools appended.
// MCP tools bypass the whitelist by design (they are already gated by their
// per-server allowedTools). MCP-add failures are logged and skipped
// (best-effort), never fatal.
//
// Ordering matters: Filter is an allow-list intersection, and MCP tools
// are named dynamically (mcp/<server>/<tool>), so no real Agent's
// spec.tools whitelist ever names one. Filtering the MCP tools alongside
// the native ones (the pre-fix behavior) silently dropped every MCP tool
// for every agent -- the feature was inert. Filtering native first, then
// adding MCP tools to the already-filtered registry, is what makes the
// bypass actually work.
func assembleAgentRegistry(
	log logr.Logger, native []foremantools.Tool, whitelist []string, mcpTools []foremantools.Tool,
) (foremanagent.ToolRegistry, error) {
	r, err := foremantools.New(native...)
	if err != nil {
		return nil, fmt.Errorf("default registry: %w", err)
	}
	if len(whitelist) > 0 {
		r, err = r.Filter(whitelist)
		if err != nil {
			return nil, fmt.Errorf("agent tool whitelist: %w", err)
		}
	}
	if len(mcpTools) > 0 {
		if err := r.Add(mcpTools...); err != nil {
			log.Error(err, "adding MCP tools to registry; continuing without the duplicates")
		}
	}
	return r, nil
}

// makeRegistryFactory returns a RegistryFactory closure that captures
// the controller-runtime client + clientset + foreman namespace. The
// returned closure builds every tool the v0.1 surface exposes
// (read_file, write_file, str_replace, grep, bash, submit_result, and
// the M4 deterministic run_gate_job), filters that native set down to
// the Agent.spec.tools whitelist, then appends the agent's MCP tools
// (which bypass the whitelist -- see assembleAgentRegistry) when MCP is
// opted in. Failing the whitelist filter (a typo in the Agent CR's tool
// list) returns an error so the executor surfaces a clean
// ToolRegistryBuildFailed outcome rather than launching the loop with
// the wrong tools.
//
// The closure accepts ctx + workloadMCPEnabled (the effective
// Workload.Spec.MCPEnabled benchmark opt-out, threaded from the
// executor via mcpEnabledForTask). MCP tools are resolved only when
// ag.Spec.MCP is set and enabled AND workloadMCPEnabled is true: either
// gate being closed runs the loop with native tools only, exactly like
// before MCP existed.
func makeRegistryFactory(
	kc client.Client, kcs kubernetes.Interface, foremanNamespace string,
) func(
	ctx context.Context, workspace string, ag *foremanv1alpha1.Agent, workloadMCPEnabled bool,
) (foremanagent.ToolRegistry, error) {
	logTail := makePodLogTailFn(kcs)
	return func(
		ctx context.Context, workspace string, ag *foremanv1alpha1.Agent, workloadMCPEnabled bool,
	) (foremanagent.ToolRegistry, error) {
		bashTimeout := time.Duration(ag.Spec.BashTimeoutSeconds) * time.Second
		if bashTimeout <= 0 {
			bashTimeout = 30 * time.Second
		}
		native := []foremantools.Tool{
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
			// fetch_issue: read-only GitHub issue surface for the
			// reviewer. The same token the foreman-agent already
			// loads at startup (via repo.TokenFromEnvOrFile) reaches
			// GitHub through one bounded Go-side call instead of
			// being inherited by every bash subprocess via $GH_TOKEN.
			// Closes #580.
			&foremantools.FetchIssueTool{
				Fetcher: githubissue.NewClient(),
				Token:   repo.TokenFromEnvOrFile,
			},
			// run_integrate: deterministic tool for a sliced Workload's
			// integrate step. Unions the disjoint slice branches onto the
			// current base and pushes the integration branch (#1033).
			&foremantools.RunIntegrateTool{
				Workspace: workspace,
				Token:     repo.TokenFromEnvOrFile,
			},
			// run_reconcile: deterministic tool for a sliced Workload's
			// reconcile step. Checks the integrated union against the pinned
			// shared identifiers for cross-slice interface drift (#1033).
			&foremantools.RunReconcileTool{
				Workspace: workspace,
				Token:     repo.TokenFromEnvOrFile,
			},
		}

		var mcpTools []foremantools.Tool
		// Gate-decision log (fires for EVERY run, both kinds) so we can see
		// exactly which input decides whether MCP registers. logf.FromContext
		// carries the executor's task/ns values, so this line correlates to the
		// AgenticTask. Diagnostic for the freeform-works / issue-fix-silent gap.
		logf.FromContext(ctx).Info("mcp registry gate",
			"agent", ag.Name,
			"mcpConfigPresent", ag.Spec.MCP != nil,
			"mcpEnabled", ag.Spec.MCP != nil && ag.Spec.MCP.Enabled,
			"workloadMCPEnabled", workloadMCPEnabled)
		if ag.Spec.MCP != nil && ag.Spec.MCP.Enabled && workloadMCPEnabled {
			log := logf.FromContext(ctx)
			// MCP header secrets resolve from the AGENT's own namespace,
			// like provider auth (apiKeySecretRef) does — not foremanNamespace
			// (the operator's namespace, which defaults to foreman-system). The
			// Agent CR and its secrets live together (e.g. default/gateway-token),
			// so an mcp-context7 secret sits beside the Agent, not by the operator.
			secretNS := ag.Namespace
			resolve := func(ref *corev1.SecretKeySelector) (string, error) {
				var s corev1.Secret
				if err := kc.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: secretNS}, &s); err != nil {
					return "", err
				}
				b, ok := s.Data[ref.Key]
				if !ok || len(b) == 0 {
					return "", fmt.Errorf("secret %s/%s has no key %q", secretNS, ref.Name, ref.Key)
				}
				return strings.TrimSpace(string(b)), nil
			}
			servers, opts := mcp.BuildServers(ag.Spec.MCP, resolve, log)
			all, closer := mcp.Register(ctx, log, nil, servers, opts, true)
			log.Info("mcp registry result",
				"serversConfigured", len(servers), "toolsAdded", len(all))
			// Tie MCP session teardown to the run's context: the loop
			// never calls closer directly, so the sessions opened here
			// must close themselves when the run ends (success,
			// failure, or ctx cancellation) rather than leak.
			go func() {
				<-ctx.Done()
				_ = closer()
			}()
			mcpTools = all
		}

		return assembleAgentRegistry(logf.FromContext(ctx), native, ag.Spec.Tools, mcpTools)
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

// mapGateVerdict maps a RunGateJobTool verdict onto (pass, ran). Only
// GATE-PASS passes; GATE-ERROR and an empty/unknown verdict mean the Job
// could not be judged (ran=false -> could-not-verify), so a GO stands.
func mapGateVerdict(verdict string) (pass bool, ran bool) {
	switch verdict {
	case foremantools.VerdictGatePass:
		return true, true
	case foremantools.VerdictGateFail:
		return false, true
	default: // VerdictGateError, "", unknown
		return false, false
	}
}

// makeEnvtestJobRunner returns an EnvtestJobRunner that submits a clean-room
// gate Job running `make test` (envtest) for repository@branch, reusing
// RunGateJobTool scoped to the `test` make target (the fast tier already ran
// fmt/vet/lint/build).
func makeEnvtestJobRunner(
	kc client.Client, foremanNamespace string,
	logTailFn func(ctx context.Context, namespace, jobName string) string,
) foremanagent.EnvtestJobRunner {
	return &envtestJobRunnerImpl{
		tool: &foremantools.RunGateJobTool{
			Client: kc,
			Cfg: foremantools.RunGateJobToolConfig{
				Namespace:    foremanNamespace,
				LogTailFn:    logTailFn,
				PollInterval: 5 * time.Second,
				PollTimeout:  10 * time.Minute,
			},
		},
	}
}

type envtestJobRunnerImpl struct {
	tool *foremantools.RunGateJobTool
}

func (e *envtestJobRunnerImpl) Run(
	ctx context.Context, taskNamespace, taskName, repository, branch, cloneURL string,
) (pass bool, ran bool, feedback string) {
	args, err := json.Marshal(map[string]any{
		"repo":     repository,
		"branch":   branch,
		"checks":   []string{"test"},
		"cloneURL": cloneURL,
		// taskRef stamps the gate Job + pod with the originating AgenticTask
		// identity (foreman.llmkube.dev/task-{namespace,name} labels) so a
		// gate Job/pod can be traced back to its task (#893). Without it the
		// renderer falls back to "unknown".
		"taskRef": map[string]string{
			"namespace": taskNamespace,
			"name":      taskName,
		},
	})
	if err != nil {
		return false, false, "envtest gate: marshal args: " + err.Error()
	}
	result, err := e.tool.Execute(ctx, args)
	if err != nil || result == nil {
		msg := "nil result"
		if err != nil {
			msg = err.Error()
		}
		return false, false, "envtest gate: " + msg
	}
	pass, ran = mapGateVerdict(result.Verdict)
	if pass {
		return true, true, ""
	}
	fb := result.Summary
	if lt, ok := result.Extra["logTail"].(string); ok && lt != "" {
		fb += "\n" + lt
	}
	return pass, ran, fb
}
