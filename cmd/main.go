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

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/controller"
	_ "github.com/defilantech/llmkube/internal/metrics" // Register custom Prometheus metrics
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(inferencev1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// initTracer initializes an OTLP trace exporter when OTEL_EXPORTER_OTLP_ENDPOINT is set.
func initTracer(ctx context.Context) func() {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return func() {}
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		setupLog.Error(err, "failed to create OTLP trace exporter")
		return func() {}
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("llmkube-controller"),
		)),
	)
	otel.SetTracerProvider(tp)
	setupLog.Info("OpenTelemetry tracing initialized", "endpoint", endpoint)

	return func() {
		_ = tp.Shutdown(ctx)
	}
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var webhookPort int
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var modelCachePath string
	var modelCacheSize string
	var modelCacheClass string
	var modelCacheAccessMode string
	var modelCacheMode string
	var allowedHostPathRoots string
	var allowedRemoteHosts string
	var gpuSharingSharedPoolSelector string
	var gpuSharingVRAMPerDeviceGiB int
	var runtimeImages string
	var modelRevalidateInterval time.Duration
	var caCertConfigMap string
	var initContainerImage string
	var defaultFSGroup int64
	var routerProxyImage string
	var defaultLiteLLMURL string
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&modelCachePath, "model-cache-path", "/models", "Path to the persistent model cache directory.")
	flag.StringVar(&modelCacheSize, "model-cache-size", "100Gi", "Size of the model cache PVC created in each namespace.")
	flag.StringVar(&modelCacheClass, "model-cache-storage-class", "",
		"Storage class for model cache PVCs (empty for default).")
	flag.StringVar(&modelCacheAccessMode, "model-cache-access-mode", "ReadWriteOnce",
		"Access mode for the shared model cache PVC (only used when --model-cache-mode=shared).")
	flag.StringVar(&modelCacheMode, "model-cache-mode", controller.ModelCacheModeShared,
		"Model cache provisioning mode: shared (default) uses a single cluster-wide "+
			"llmkube-model-cache PVC the operator mounts and all InferenceServices share "+
			"(cross-isvc dedup, cache list works; use an RWX class on multi-node clusters); "+
			"perService gives each InferenceService its own RWO, WaitForFirstConsumer cache PVC "+
			"that binds on the serving node (opt-in escape hatch for multi-node clusters without RWX).")
	flag.StringVar(&runtimeImages, "runtime-images", "",
		"Fleet-wide runtime image overrides as runtime=image[,runtime=image] with runtimes "+
			"llamacpp|vllm|sglang|tgi (chart value runtimeImages.*). Overrides the built-in "+
			"defaults and vendor divergences for air-gapped/mirrored registries; an explicit "+
			"InferenceService spec.image still wins.")
	flag.StringVar(&allowedHostPathRoots, "allowed-host-path-roots", "",
		"Comma-separated absolute path prefixes under which local/file:// and hostPath model "+
			"sources are permitted. Empty (default) disables all local/hostPath sources (GHSA-jw3m-8q7m-f35r).")
	flag.StringVar(&gpuSharingSharedPoolSelector, "gpu-sharing-shared-pool-selector", "",
		"Node selector (key=value[,key=value]) for the shared-GPU pool that gpuSharing mode "+
			"shared schedules onto. Empty (default) means no shared pool exists and mode shared "+
			"is rejected.")
	flag.IntVar(&gpuSharingVRAMPerDeviceGiB, "gpu-sharing-vram-per-device-gib", 0,
		"Device memory (GiB) of one whole GPU in this fleet, used to derive the VRAM footprint "+
			"of exclusive-mode InferenceServices for GPUQuota vramBytes accounting. 0 (default) "+
			"means exclusive footprints are unknown; quotas with a vramBytes cap then deny such "+
			"admissions with an actionable message.")
	flag.StringVar(&allowedRemoteHosts, "allowed-remote-hosts", "",
		"Comma-separated hostnames/CIDRs permitted as remote (http/https) Model sources even if "+
			"they resolve to private/link-local/loopback ranges. Public hosts are always allowed; "+
			"this only re-permits internal hosts blocked by the SSRF guard (GHSA-jw3m-8q7m-f35r).")
	flag.DurationVar(&modelRevalidateInterval, "model-revalidate-interval", controller.DefaultRevalidateInterval,
		"Minimum interval between upstream source revalidation checks for a Model. "+
			"Bounds the HEAD traffic the controller generates; drift is surfaced via the "+
			"SourceDrifted condition and acted on only when spec.refreshPolicy is OnChange.")
	flag.StringVar(&caCertConfigMap, "ca-cert-configmap", "",
		"Name of the ConfigMap containing a custom CA certificate to trust for model downloads.")
	flag.StringVar(&initContainerImage, "init-container-image", "docker.io/curlimages/curl:8.18.0",
		"Container image for the model downloader init container.")
	flag.Int64Var(&defaultFSGroup, "default-fsgroup", 102,
		"Default fsGroup for inference pods when Spec.PodSecurityContext is not set. "+
			"102 matches curlimages/curl curl_group GID and lets the init container "+
			"write to a freshly-provisioned PVC. Set to 0 to disable on OpenShift, "+
			"where the restricted-v2 SCC injects fsGroup from the namespace's allocated "+
			"range and rejects pods with explicit values outside that range.")
	flag.StringVar(&routerProxyImage, "router-proxy-image", "",
		"Default container image for ModelRouter-managed router-proxy pods. "+
			"Empty falls back to the controller's compiled-in default. "+
			"Per-ModelRouter spec.proxy.image overrides this.")
	flag.StringVar(&defaultLiteLLMURL, "default-litellm-url", "",
		"Cluster-wide default URL for External backends with provider=litellm "+
			"that omit url. Lets operators centralize the LiteLLM proxy "+
			"endpoint so application teams can declare external backends "+
			"without repeating the URL on every ModelRouter. Empty means "+
			"users must specify url explicitly.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	var enablePyrraSLO bool
	flag.BoolVar(&enablePyrraSLO, "enable-pyrra-slo", false,
		"Enable rendering Pyrra ServiceLevelObjective resources for InferenceServices with spec.slo. "+
			"Requires the Pyrra CRD in the cluster (https://github.com/pyrra-dev/pyrra).")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "The port the validating webhook server binds.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Parse the host-path allowlist for local model sources: split on comma,
	// trim spaces, drop empties. Empty (the default) disables all local and
	// hostPath model sources (GHSA-jw3m-8q7m-f35r).
	var allowedHostPathRootList []string
	for _, root := range strings.Split(allowedHostPathRoots, ",") {
		if root = strings.TrimSpace(root); root != "" {
			allowedHostPathRootList = append(allowedHostPathRootList, root)
		}
	}

	// Parse the remote-host allowlist for the controller-side SSRF guard the
	// same way: split on comma, trim spaces, drop empties. Empty (the default)
	// blocks every private/link-local/loopback destination (GHSA-jw3m-8q7m-f35r).
	var allowedRemoteHostList []string
	for _, host := range strings.Split(allowedRemoteHosts, ",") {
		if host = strings.TrimSpace(host); host != "" {
			allowedRemoteHostList = append(allowedRemoteHostList, host)
		}
	}

	// Parse the shared-GPU pool node selector (gpuSharing mode shared). A
	// malformed value is a startup error rather than a silent no-pool: the
	// operator refusing to start beats every shared workload being rejected
	// with a misleading "no pool configured" message.
	gpuSharingSharedPool, err := controller.ParseGPUSharingSharedPoolSelector(gpuSharingSharedPoolSelector)
	if err != nil {
		setupLog.Error(err, "invalid --gpu-sharing-shared-pool-selector")
		os.Exit(1)
	}

	// Bound the per-device VRAM figure. 1 PiB of device memory per GPU is
	// far beyond any real hardware, so anything outside [0, 2^20] is a typo,
	// not a fleet.
	if gpuSharingVRAMPerDeviceGiB < 0 || gpuSharingVRAMPerDeviceGiB > 1<<20 {
		setupLog.Error(fmt.Errorf("value %d out of range [0, %d]", gpuSharingVRAMPerDeviceGiB, 1<<20),
			"invalid --gpu-sharing-vram-per-device-gib")
		os.Exit(1)
	}

	runtimeImageOverrides, err := controller.ParseRuntimeImageOverrides(runtimeImages)
	if err != nil {
		setupLog.Error(err, "invalid --runtime-images")
		os.Exit(1)
	}

	// Initialize OpenTelemetry tracing (noop if OTEL_EXPORTER_OTLP_ENDPOINT not set)
	shutdownTracer := initTracer(context.Background())
	defer shutdownTracer()

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
		Port:    webhookPort,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production. To wire cert-manager-issued certificates,
	// enable [METRICS-WITH-CERTS] in config/default/kustomization.yaml and [PROMETHEUS-WITH-CERTS]
	// in config/prometheus/kustomization.yaml.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "882d6afe.llmkube.dev",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.ModelReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		StoragePath:          modelCachePath,
		RevalidateInterval:   modelRevalidateInterval,
		AllowedHostPathRoots: allowedHostPathRootList,
		AllowedRemoteHosts:   allowedRemoteHostList,
		InitContainerImage:   initContainerImage,
		CACertConfigMap:      caCertConfigMap,
		DefaultFSGroup:       defaultFSGroup,
		ModelCacheSize:       modelCacheSize,
		ModelCacheClass:      modelCacheClass,
		ModelCacheAccessMode: modelCacheAccessMode,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Model")
		os.Exit(1)
	}
	if err := (&controller.InferenceServiceReconciler{
		Client:                mgr.GetClient(),
		Scheme:                mgr.GetScheme(),
		Recorder:              mgr.GetEventRecorder("inferenceservice-controller"),
		ModelCachePath:        modelCachePath,
		ModelCacheSize:        modelCacheSize,
		ModelCacheClass:       modelCacheClass,
		ModelCacheAccessMode:  modelCacheAccessMode,
		ModelCacheMode:        modelCacheMode,
		CACertConfigMap:       caCertConfigMap,
		InitContainerImage:    initContainerImage,
		DefaultFSGroup:        defaultFSGroup,
		AllowedHostPathRoots:  allowedHostPathRootList,
		GPUSharingSharedPool:  gpuSharingSharedPool,
		RuntimeImageOverrides: runtimeImageOverrides,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "InferenceService")
		os.Exit(1)
	}
	if err := (&controller.LoRAAdapterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LoRAAdapter")
		os.Exit(1)
	}
	if err := (&controller.ModelRouterReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		RouterProxyImage:  routerProxyImage,
		DefaultLiteLLMURL: defaultLiteLLMURL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ModelRouter")
		os.Exit(1)
	}
	// InferenceServiceGateway reconciles Envoy AI Gateway exposure for opted-in
	// InferenceServices. It self-gates on the aigw CRDs being present (detected
	// via the RESTMapper at first reconcile), so a cluster without the gateway
	// stack installed still starts the operator cleanly and this controller
	// simply no-ops.
	if err := (&controller.InferenceServiceGatewayReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "InferenceServiceGateway")
		os.Exit(1)
	}
	// InferenceServiceSLO renders a Pyrra ServiceLevelObjective for each
	// InferenceService with spec.slo. Registered unconditionally so a
	// disabled integration still reports the IntegrationDisabled condition;
	// like the gateway controllers it self-gates on the pyrra.dev CRD, so a
	// cluster without Pyrra starts the operator cleanly and this controller
	// no-ops.
	if err := (&controller.InferenceServiceSLOReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Enabled: enablePyrraSLO,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "InferenceServiceSLO")
		os.Exit(1)
	}
	// ModelRouterGateway compiles a ModelRouter in dataPlane: Gateway mode onto a
	// pre-installed Envoy AI Gateway (Backend / AIServiceBackend / AIGatewayRoute
	// / BackendTrafficPolicy). Like the InferenceService gateway controller it
	// self-gates on the aigw CRDs being present, so a cluster without the gateway
	// stack still starts the operator cleanly and this controller no-ops.
	if err := (&controller.ModelRouterGatewayReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ModelRouterGateway")
		os.Exit(1)
	}
	// GPUQuota reconciles the status-only aggregation of GPU usage from
	// InferenceServices in the quota's scope. It never rejects anything or
	// owns external resources.
	if err := (&controller.GPUQuotaReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		VRAMPerDeviceGiB: gpuSharingVRAMPerDeviceGiB,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GPUQuota")
		os.Exit(1)
	}

	// Register the ModelRouter validating webhook ONLY when the serving cert is
	// actually present in the configured cert dir. The webhook server crashes the
	// manager if a webhook is registered but the cert is missing, so a dev run
	// without certs (or a chart install with webhook.enabled=false, which omits
	// the cert Secret + volume) degrades to "no webhook" instead of a crash loop.
	// The gate keys off the SAME path the webhook server was configured with.
	if webhookCertPath != "" && webhookCertsPresent(webhookCertPath, webhookCertName, webhookCertKey) {
		if err := controller.SetupModelRouterWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "ModelRouter")
			os.Exit(1)
		}
		if err := controller.SetupModelWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Model")
			os.Exit(1)
		}
		if err := controller.SetupInferenceServiceQuotaWebhookWithManager(mgr, controller.InferenceServiceQuotaWebhookOptions{
			VRAMPerDeviceGiB:     gpuSharingVRAMPerDeviceGiB,
			GPUSharingSharedPool: gpuSharingSharedPool,
		}); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "InferenceServiceQuota")
			os.Exit(1)
		}
		setupLog.Info("webhooks enabled", "webhooks", "ModelRouter,Model,InferenceServiceQuota", "certDir", webhookCertPath)
	} else if webhookCertPath != "" {
		setupLog.Info("webhook cert path set but no serving cert found; skipping ModelRouter webhook",
			"certDir", webhookCertPath)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// webhookCertsPresent reports whether both the serving cert and key exist in
// certDir. The webhook server crashes the manager if a webhook is registered
// but the cert is missing, so the operator only wires the webhook when the
// serving Secret has actually been mounted (a chart install with
// webhook.enabled=true). A dev run without certs degrades to "no webhook"
// rather than a crash loop. The cert/key file names are the same the webhook
// server is configured with (--webhook-cert-name / --webhook-cert-key).
func webhookCertsPresent(certDir, certName, keyName string) bool {
	for _, name := range []string{certName, keyName} {
		if _, err := os.Stat(filepath.Join(certDir, name)); err != nil {
			return false
		}
	}
	return true
}
