/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// defaultRouterProxyImage is the image used when neither the controller
// flag nor the ModelRouter.spec.proxy.image override sets one. Real
// deployments override this in production; the default is a development
// tag for local kind clusters.
const defaultRouterProxyImage = "ghcr.io/defilantech/llmkube-router-proxy:dev"

// reconcileRouterDeployment creates or updates the Deployment that runs
// the router-proxy. The Deployment is owner-referenced to the
// ModelRouter; deleting the parent garbage-collects the pods.
func (r *ModelRouterReconciler) reconcileRouterDeployment(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	configHash string,
) error {
	desired := r.newRouterDeployment(mr, configHash)
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

	// PodTemplate selector is immutable; copy mutable surface while
	// preserving externally-set annotations and labels (sidecar
	// injectors, `kubectl rollout restart`'s restartedAt, GitOps sync
	// labels). Operator-owned keys win on collision; foreign keys
	// pass through. Fixes #456.
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.RevisionHistoryLimit = desired.Spec.RevisionHistoryLimit
	existing.Spec.Template.Spec = desired.Spec.Template.Spec
	existing.Spec.Template.Labels = mergePreservingExternal(
		existing.Spec.Template.Labels,
		desired.Spec.Template.Labels,
	)
	existing.Spec.Template.Annotations = mergePreservingExternal(
		existing.Spec.Template.Annotations,
		desired.Spec.Template.Annotations,
	)
	existing.Labels = mergePreservingExternal(existing.Labels, desired.Labels)
	if err := setControllerReferenceUnblocked(mr, existing, r.Scheme); err != nil {
		return fmt.Errorf("set owner ref on existing router Deployment: %w", err)
	}
	return r.Update(ctx, existing)
}

// newRouterDeployment is the in-memory blueprint of the proxy
// Deployment. Pulls overrides from spec.proxy if set, otherwise falls
// back to sensible defaults that work on most kind / vanilla K8s
// clusters and don't violate the restricted PodSecurityStandard.
func (r *ModelRouterReconciler) newRouterDeployment(
	mr *inferencev1alpha1.ModelRouter,
	configHash string,
) *appsv1.Deployment {
	replicas := int32(1)
	image := r.routerProxyImage()
	var resources corev1.ResourceRequirements
	var imagePullSecrets []corev1.LocalObjectReference
	var revisionHistoryLimit *int32

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
	}
	if resources.Requests == nil && resources.Limits == nil {
		resources = defaultRouterProxyResources()
	}

	envFrom := credentialsEnvFrom(mr)

	podLabels := routerProxySelectorLabels(mr)
	podAnnotations := map[string]string{
		// Re-roll pods automatically when the ConfigMap content
		// changes. controller-runtime's CreateOrUpdate doesn't trigger
		// a rollout on its own; the hash annotation does.
		routerProxyConfigHashAnnotation: configHash,
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routerProxyResourceName(mr.Name),
			Namespace: mr.Namespace,
			Labels:    routerProxyLabels(mr),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:             &replicas,
			RevisionHistoryLimit: revisionHistoryLimit,
			Selector:             &metav1.LabelSelector{MatchLabels: podLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					SecurityContext:  routerProxyPodSecurityContext(),
					ImagePullSecrets: imagePullSecrets,
					Containers: []corev1.Container{
						{
							Name:            routerProxyContainerName,
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            routerProxyArgs(mr),
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: routerProxyPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
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

func defaultRouterProxyResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
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
func routerProxyArgs(mr *inferencev1alpha1.ModelRouter) []string {
	args := []string{
		"--config", routerProxyConfigMountPath + "/" + routerProxyConfigKey,
		"--listen", fmt.Sprintf(":%d", routerProxyPort),
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
	return args
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
