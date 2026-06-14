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

// Command foreman-operator is the control plane for the Foreman agentic
// workload subsystem. It runs alongside (not inside) the LLMKube core
// operator and reconciles the foreman.llmkube.dev API group: Workload,
// AgenticTask, FleetNode. Installing LLMKube alone does not install or
// require this binary.
package main

import (
	"flag"
	"os"
	"path/filepath"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC) so
	// exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	webhookserver "sigs.k8s.io/controller-runtime/pkg/webhook"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremancontroller "github.com/defilantech/llmkube/internal/foreman/controller"
	foremanwebhook "github.com/defilantech/llmkube/internal/foreman/webhook"
)

// defaultWebhookCertDir is the controller-runtime default serving-cert
// mount path. The chart mounts the self-signed serving Secret here so the
// webhook server finds tls.crt / tls.key without extra flags.
const defaultWebhookCertDir = "/tmp/k8s-webhook-server/serving-certs"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(foremanv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var allowCloudProviders bool
	var enableWebhooks bool
	var webhookCertDir string
	var webhookPort int
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8081",
		"The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8082",
		"The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&allowCloudProviders, "allow-cloud-providers", true,
		"Cluster-wide sovereignty kill switch for cloud-proxy reviewer Agents. "+
			"True (default) lets reviewer Agents whose spec.provider is non-local "+
			"dispatch (subject to per-Workload spec.allowCloudReviewers). False "+
			"causes the WorkloadReconciler to drop any step whose Agent has a "+
			"non-local provider and surface a CloudReviewersSuppressed condition. "+
			"Set false for air-gapped or compliance-restricted clusters.")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true,
		"Serve the Agent + AgenticTask validating admission webhooks. "+
			"True (default) starts the webhook server when serving certs are "+
			"present in --webhook-cert-dir; if no certs are found the server "+
			"is skipped so the operator still runs (useful for local/dev runs "+
			"without a cert). Set false to disable the webhooks entirely.")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", defaultWebhookCertDir,
		"Directory holding the webhook server's tls.crt + tls.key. The Helm "+
			"chart mounts the self-signed serving Secret here.")
	flag.IntVar(&webhookPort, "webhook-port", 9443,
		"Port the webhook server listens on.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Serve the webhooks only when enabled AND the serving cert is
	// actually mounted. This keeps a no-cert dev run (or a chart install
	// with webhook.enabled=false, which omits the cert Secret + volume)
	// from crashing the operator on a missing tls.crt.
	serveWebhooks := enableWebhooks && webhookCertsPresent(webhookCertDir)
	if enableWebhooks && !serveWebhooks {
		setupLog.Info("webhooks enabled but no serving cert found; skipping webhook server",
			"certDir", webhookCertDir)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "foreman-operator.llmkube.dev",
		WebhookServer: webhookserver.NewServer(webhookserver.Options{
			Port:    webhookPort,
			CertDir: webhookCertDir,
		}),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&foremancontroller.WorkloadReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		AllowCloudProviders: allowCloudProviders,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Workload")
		os.Exit(1)
	}

	if err := (&foremancontroller.AgenticTaskReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgenticTask")
		os.Exit(1)
	}

	if err := (&foremancontroller.FleetNodeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "FleetNode")
		os.Exit(1)
	}

	if err := (&foremancontroller.AgentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Agent")
		os.Exit(1)
	}

	if err := (&foremancontroller.AgentReleaseReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentRelease")
		os.Exit(1)
	}

	if serveWebhooks {
		if err := foremanwebhook.SetupAgentWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Agent")
			os.Exit(1)
		}
		if err := foremanwebhook.SetupAgenticTaskWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "AgenticTask")
			os.Exit(1)
		}
		setupLog.Info("validating webhooks enabled", "certDir", webhookCertDir, "port", webhookPort)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting foreman-operator")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// webhookCertsPresent reports whether both tls.crt and tls.key exist in
// certDir. The webhook server crashes the manager if it is registered but
// the cert is missing, so the operator only wires the webhooks when the
// serving Secret has actually been mounted (chart install with
// webhook.enabled=true). A dev run without certs degrades to "no
// webhooks" rather than a crash loop.
func webhookCertsPresent(certDir string) bool {
	for _, name := range []string{"tls.crt", "tls.key"} {
		if _, err := os.Stat(filepath.Join(certDir, name)); err != nil {
			return false
		}
	}
	return true
}
