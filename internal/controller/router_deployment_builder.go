/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"fmt"
	"math"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// defaultRouterProxyImage is the image used when neither the controller
// flag nor the ModelRouter.spec.proxy.image override sets one. Real
// deployments override this in production; the default is a development
// tag for local kind clusters.
const defaultRouterProxyImage = "ghcr.io/defilantech/llmkube-router-proxy:dev"

// routerProxyDrainGraceMargin is added to spec.proxy.drainTimeout to get the
// pod's terminationGracePeriodSeconds. The kubelet's countdown starts at
// SIGTERM and the proxy's own shutdown is bounded by the drain (both servers
// share one context in cmd/router-proxy), so the margin covers process exit
// only; this pod has no preStop hook consuming budget.
const routerProxyDrainGraceMargin = 5 * time.Second

// reconcileRouterDeployment creates or updates the Deployment that runs
// the router-proxy. The Deployment is owner-referenced to the
// ModelRouter; deleting the parent garbage-collects the pods.
func (r *ModelRouterReconciler) reconcileRouterDeployment(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	configHash string,
	hasPools bool,
) error {
	if hasPools && mr.Spec.Proxy != nil && mr.Spec.Proxy.Replicas != nil && *mr.Spec.Proxy.Replicas > 1 {
		logf.FromContext(ctx).Info(
			"pinning router-proxy to a single replica: ModelPool activation requires one proxy; ignoring spec.proxy.replicas",
			"requestedReplicas", *mr.Spec.Proxy.Replicas)
	}
	desired := r.newRouterDeployment(mr, configHash, hasPools)
	if err := setControllerReferenceUnblocked(mr, desired, r.Scheme); err != nil {
		return fmt.Errorf("set owner ref on router Deployment: %w", err)
	}

	existing := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	switch {
	case errors.IsNotFound(err):
		return r.Create(ctx, desired)
	case err != nil:
		return err
	}
	if err := ensureRouterProxyOwned(mr, existing, "Deployment"); err != nil {
		return err
	}

	// PodTemplate selector is immutable; copy mutable surface while
	// preserving externally-set annotations and labels (sidecar
	// injectors, `kubectl rollout restart`'s restartedAt, GitOps sync
	// labels). Operator-owned keys win on collision; foreign keys
	// pass through. Fixes #456.
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.RevisionHistoryLimit = desired.Spec.RevisionHistoryLimit
	// An unset strategy is filled with the Kubernetes default by the API
	// server, so dropping the no-surge rule goes back to the default.
	existing.Spec.Strategy = desired.Spec.Strategy
	existing.Spec.Template.Spec = desired.Spec.Template.Spec
	// The merge keeps keys the desired template no longer sets, so first drop
	// the spec.proxy podLabels / podAnnotations an earlier reconcile applied
	// and the spec no longer asks for. Keys another tool added are not in the
	// recorded set and pass through.
	existing.Spec.Template.Labels = withoutUnwantedKeys(existing.Spec.Template.Labels,
		existing.Annotations[appliedPodLabelsAnnotation], desired.Spec.Template.Labels)
	existing.Spec.Template.Annotations = withoutUnwantedKeys(existing.Spec.Template.Annotations,
		existing.Annotations[appliedPodAnnotationsAnnotation], desired.Spec.Template.Annotations)
	existing.Spec.Template.Labels = mergePreservingExternal(
		existing.Spec.Template.Labels,
		desired.Spec.Template.Labels,
	)
	existing.Spec.Template.Annotations = mergePreservingExternal(
		existing.Spec.Template.Annotations,
		desired.Spec.Template.Annotations,
	)
	if !r.EmitScrapeAnnotations {
		// The merge keeps keys the desired template no longer sets, so drop
		// the scrape annotations an earlier run with the flag added: a router
		// moved to routerPodMonitor must not be scraped twice. Only values
		// the operator would set are removed.
		for k, v := range routerProxyScrapeAnnotations() {
			// A key spec.proxy.podAnnotations sets is the user's, not a leftover.
			if _, wanted := desired.Spec.Template.Annotations[k]; wanted {
				continue
			}
			if existing.Spec.Template.Annotations[k] == v {
				delete(existing.Spec.Template.Annotations, k)
			}
		}
	}
	existing.Labels = mergePreservingExternal(existing.Labels, desired.Labels)
	syncAppliedKeyRecords(&existing.Annotations, desired.Annotations)
	if err := setControllerReferenceUnblocked(mr, existing, r.Scheme); err != nil {
		return fmt.Errorf("set owner ref on existing router Deployment: %w", err)
	}
	return r.Update(ctx, existing)
}

// routerProxyScrapeAnnotations are the annotations --emit-scrape-annotations
// adds to router-proxy pods. They mirror buildPodAnnotations for inference
// pods, pointed at the listener --metrics-bind-address opens (#2263).
func routerProxyScrapeAnnotations() map[string]string {
	return map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/path":   "/metrics",
		"prometheus.io/port":   fmt.Sprintf("%d", routerProxyMetricsPort),
	}
}

// newRouterDeployment is the in-memory blueprint of the proxy
// Deployment. Pulls overrides from spec.proxy if set, otherwise falls
// back to sensible defaults that work on most kind / vanilla K8s
// clusters and don't violate the restricted PodSecurityStandard.
func (r *ModelRouterReconciler) newRouterDeployment(
	mr *inferencev1alpha1.ModelRouter,
	configHash string,
	hasPools bool,
) *appsv1.Deployment {
	replicas := int32(1)
	image := r.routerProxyImage()
	var resources corev1.ResourceRequirements
	var imagePullSecrets []corev1.LocalObjectReference
	var revisionHistoryLimit *int32 // nil: use the default
	var nodeSelector map[string]string
	var tolerations []corev1.Toleration
	var runtimeClassName *string
	var affinity *corev1.Affinity
	var topologySpread []corev1.TopologySpreadConstraint

	if mr.Spec.Proxy != nil {
		if mr.Spec.Proxy.Replicas != nil {
			replicas = *mr.Spec.Proxy.Replicas
		}
		revisionHistoryLimit = mr.Spec.Proxy.RevisionHistoryLimit
		if mr.Spec.Proxy.Image != "" {
			image = mr.Spec.Proxy.Image
		}
		if mr.Spec.Proxy.Resources != nil {
			resources = *mr.Spec.Proxy.Resources
		}
		imagePullSecrets = mr.Spec.Proxy.ImagePullSecrets
		nodeSelector = mr.Spec.Proxy.NodeSelector
		tolerations = mr.Spec.Proxy.Tolerations
		runtimeClassName = mr.Spec.Proxy.RuntimeClassName
		affinity = mr.Spec.Proxy.Affinity
		topologySpread = mr.Spec.Proxy.TopologySpreadConstraints
	}
	// A pooled router must run exactly one proxy replica: ModelPool activation
	// serializes swaps through a single in-process lock, so a second replica
	// would race it and thrash the shared GPU slot. Pin to 1 regardless of
	// spec.proxy.replicas until cross-replica swap coordination lands.
	if hasPools {
		replicas = 1
	}
	if resources.Requests == nil && resources.Limits == nil {
		resources = defaultRouterProxyResources()
	}
	strategy := routerProxyStrategy(replicas, affinity, topologySpread)

	envFrom := credentialsEnvFrom(mr)

	var userLabels, userAnnotations map[string]string
	if mr.Spec.Proxy != nil {
		userLabels = mr.Spec.Proxy.PodLabels
		userAnnotations = mr.Spec.Proxy.PodAnnotations
	}
	selectorLabels := routerProxySelectorLabels(mr)
	podLabels := mergePodLabels(selectorLabels, userLabels)
	podAnnotations := map[string]string{
		// Re-roll pods automatically when the ConfigMap content
		// changes. controller-runtime's CreateOrUpdate doesn't trigger
		// a rollout on its own; the hash annotation does.
		routerProxyConfigHashAnnotation: configHash,
	}
	if r.EmitScrapeAnnotations {
		for k, v := range routerProxyScrapeAnnotations() {
			podAnnotations[k] = v
		}
	}
	deploymentAnnotations := appliedKeyRecords(userLabels, selectorLabels, userAnnotations, podAnnotations)
	for k, v := range userAnnotations {
		if _, operatorOwned := podAnnotations[k]; !operatorOwned {
			podAnnotations[k] = v
		}
	}

	// Proxies that call the API (ModelPool activation, endpoint tracking) run
	// under a dedicated ServiceAccount bound to the Role reconcileRouterActivationRBAC
	// provisions; other routers keep the namespace default SA and no API access.
	serviceAccountName := ""
	if hasPools || routerNeedsEndpointWatch(mr) {
		serviceAccountName = routerProxyResourceName(mr.Name)
	}
	// Every proxy labels its metrics with this router's name, whatever else it does.
	env := append([]corev1.EnvVar{{Name: "ROUTER_NAME", Value: mr.Name}}, r.RouterProxyTracingEnv...)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        routerProxyResourceName(mr.Name),
			Namespace:   mr.Namespace,
			Labels:      routerProxyLabels(mr),
			Annotations: deploymentAnnotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:             &replicas,
			RevisionHistoryLimit: revisionHistoryLimitOrDefault(revisionHistoryLimit),
			Strategy:             strategy,
			Selector:             &metav1.LabelSelector{MatchLabels: selectorLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            serviceAccountName,
					SecurityContext:               routerProxyPodSecurityContext(),
					ImagePullSecrets:              imagePullSecrets,
					NodeSelector:                  nodeSelector,
					Tolerations:                   tolerations,
					RuntimeClassName:              runtimeClassName,
					Affinity:                      affinity,
					TopologySpreadConstraints:     topologySpread,
					TerminationGracePeriodSeconds: proxyDrainGraceSeconds(mr),
					Containers: []corev1.Container{
						{
							Name:            routerProxyContainerName,
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            routerProxyArgs(mr, hasPools),
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: routerProxyPort,
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "metrics",
									ContainerPort: routerProxyMetricsPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Env:     env,
							EnvFrom: envFrom,
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "router-config",
									MountPath: routerProxyConfigMountPath,
									ReadOnly:  true,
								},
							},
							Resources:       resources,
							SecurityContext: routerProxyContainerSecurityContext(),
							LivenessProbe:   routerProxyProbe(20, 10),
							ReadinessProbe:  routerProxyProbe(5, 5),
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "router-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: routerProxyResourceName(mr.Name),
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// credentialsEnvFrom builds the envFrom list that injects every cloud
// backend's credentials into the proxy pod. We use secretRef so the
// proxy reads the values via os.Getenv at request time. Optional=true
// keeps the pod schedulable even when a referenced Secret hasn't been
// created yet; the proxy reports the backend unhealthy until it is.
func credentialsEnvFrom(mr *inferencev1alpha1.ModelRouter) []corev1.EnvFromSource {
	seen := make(map[string]bool)
	out := make([]corev1.EnvFromSource, 0, len(mr.Spec.Backends))
	for i := range mr.Spec.Backends {
		b := &mr.Spec.Backends[i]
		if b.External == nil || b.External.CredentialsSecretRef == nil {
			continue
		}
		name := b.External.CredentialsSecretRef.Name
		if seen[name] {
			continue
		}
		seen[name] = true
		optional := true
		out = append(out, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
				Optional:             &optional,
			},
		})
	}
	return out
}

// defaultRouterProxyResources keeps the request small (an idle proxy sits
// near 14Mi) and gives the limit room for a few hundred long-context agent
// streams arriving together: each request is buffered while it is sent
// upstream, at roughly two to three times its body size during a model
// rewrite (#2194).
func defaultRouterProxyResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
}

// routerProxyPodSecurityContext is the minimum-privilege pod-level
// security context that satisfies the restricted PodSecurityStandard.
// Matches what the InferenceServiceReconciler does for inference pods.
func routerProxyPodSecurityContext() *corev1.PodSecurityContext {
	uid := int64(65532)
	return &corev1.PodSecurityContext{
		RunAsNonRoot: boolPtr(true),
		RunAsUser:    &uid,
		FSGroup:      &uid,
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// routerProxyContainerSecurityContext is the container-level security
// context. ReadOnlyRootFilesystem is safe here because the proxy never
// writes to disk (config is mounted ro, logs go to stdout).
func routerProxyContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		RunAsNonRoot:             boolPtr(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

func routerProxyProbe(initialDelay, periodSeconds int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt(int(routerProxyPort)),
			},
		},
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       periodSeconds,
		TimeoutSeconds:      2,
		FailureThreshold:    3,
	}
}

// routerProxyArgs builds the router-proxy container args. Required
// flags (config / listen / log-format) come first; optional flags
// driven by ModelRouter.spec.proxy.* only land when the user
// explicitly set them, so the proxy keeps its compiled-in defaults
// otherwise.
func routerProxyArgs(mr *inferencev1alpha1.ModelRouter, hasPools bool) []string {
	args := []string{
		"--config", routerProxyConfigMountPath + "/" + routerProxyConfigKey,
		"--listen", fmt.Sprintf(":%d", routerProxyPort),
		"--metrics-bind-address", fmt.Sprintf(":%d", routerProxyMetricsPort),
		"--log-format", "json",
	}
	if mr.Spec.Proxy != nil && mr.Spec.Proxy.QuarantineDuration != nil {
		args = append(args, "--quarantine-duration",
			mr.Spec.Proxy.QuarantineDuration.Duration.String())
	}
	if mr.Spec.Proxy != nil && mr.Spec.Proxy.ResponseHeaderTimeout != nil {
		args = append(args, "--response-header-timeout",
			mr.Spec.Proxy.ResponseHeaderTimeout.Duration.String())
	}
	if mr.Spec.Proxy != nil && mr.Spec.Proxy.StreamIdleTimeout != nil {
		args = append(args, "--stream-idle-timeout",
			mr.Spec.Proxy.StreamIdleTimeout.Duration.String())
	}
	if d, ok := proxyDrainTimeout(mr); ok {
		args = append(args, "--shutdown-timeout", d.String())
	}
	if hasPools {
		args = append(args, "--enable-activation")
	}
	return args
}

// proxyDrainTimeout reports spec.proxy.drainTimeout when it is set. Both the
// proxy flag and the pod grace period read this one accessor, so the drain
// window and the kubelet's budget cannot disagree.
func proxyDrainTimeout(mr *inferencev1alpha1.ModelRouter) (time.Duration, bool) {
	if mr.Spec.Proxy == nil || mr.Spec.Proxy.DrainTimeout == nil {
		return 0, false
	}
	return mr.Spec.Proxy.DrainTimeout.Duration, true
}

// proxyDrainGraceSeconds is the pod's terminationGracePeriodSeconds: the drain
// plus routerProxyDrainGraceMargin, or nil when the router sets no drain so
// the pod template is unchanged and existing proxies do not roll. Round up,
// never truncate: the kubelet's countdown starts at SIGTERM, so a short budget
// is the failure mode that cuts streams.
func proxyDrainGraceSeconds(mr *inferencev1alpha1.ModelRouter) *int64 {
	d, ok := proxyDrainTimeout(mr)
	if !ok {
		return nil
	}
	budget := d + routerProxyDrainGraceMargin
	return ptr.To(int64(math.Ceil(budget.Seconds())))
}

// routerProxyImage returns the image the reconciler will set on the
// router-proxy container. The reconciler holds the flag-derived
// default; per-ModelRouter overrides come from spec.proxy.image and
// are applied in newRouterDeployment.
func (r *ModelRouterReconciler) routerProxyImage() string {
	if r.RouterProxyImage != "" {
		return r.RouterProxyImage
	}
	return defaultRouterProxyImage
}

// routerProxyStrategy rolls a multi-replica proxy one replica at a time when its
// placement is hard (a DoNotSchedule topology spread, or required pod
// anti-affinity): with one replica per node on every eligible node, the default
// surge pod can never be placed and the rollout would hang. Otherwise the
// strategy stays unset, which is the Kubernetes default (#2198).
func routerProxyStrategy(replicas int32, affinity *corev1.Affinity, spread []corev1.TopologySpreadConstraint) appsv1.DeploymentStrategy {
	if replicas <= 1 || !hardProxyPlacement(affinity, spread) {
		return appsv1.DeploymentStrategy{}
	}
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       ptr.To(intstr.FromInt32(0)),
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
		},
	}
}

func hardProxyPlacement(affinity *corev1.Affinity, spread []corev1.TopologySpreadConstraint) bool {
	for _, c := range spread {
		if c.WhenUnsatisfiable == corev1.DoNotSchedule {
			return true
		}
	}
	return affinity != nil && affinity.PodAntiAffinity != nil &&
		len(affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0
}
